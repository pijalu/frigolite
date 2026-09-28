# Performance Report — frigolite vs SQLite 3.54 (CRUD microbenchmarks)

**Date:** 2026-09-28 · **Platform:** macOS arm64 (darwin 27), Go 1.24, system
libsqlite3 3.54 (Apple SDK build) · **Mode:** in-memory unless noted ·
**Verdict:** frigolite is functionally correct but 8×–15,000× slower on CRUD
hot paths. Six concrete bottlenecks were isolated, each with profile or probe
evidence, and five of them have small, well-scoped fixes planned (§4).

## 1. Results

Ops-per-second, single connection, same statement sequence per phase on both
engines. sqlite3 runs two modes: **prepared** (idiomatic prepare+bind+reset)
and **literal** (prepare-per-call with inlined literals — the only mode
frigolite's `Open/Exec/Query` API allows, so it is the apples-to-apples
baseline; parse cost in sqlite3 is ~0.5–2µs/statement).

| Phase | frigolite ops/s | sqlite3 literal | ratio | sqlite3 prepared | ratio |
|---|---|---|---|---|---|
| INSERT ×50k (1 txn) | 21,281 | 1,276,962 | **60× slower** | 2,508,422 | 118× |
| SELECT point `WHERE id=?` | 192 | 849,777 | **4,426× slower** | 2,544,999 | 13,255× |
| SELECT scan (rows/s) | 1,450,031 | 52,027,746 | **36× slower** | 53,239,356 | 37× |
| SELECT GROUP BY (passes) | 14 | 122 | **8.7× slower** | 119 | 8.5× |
| UPDATE ×20k (1 txn) | 65 | 984,913 | **15,152× slower** | 3,659,530 | 56,300× |
| DELETE ×1k (1 txn) | 133 | 1,236,389 | **9,295× slower** | 3,813,964 | 28,676× |
| INSERT autocommit (file) | 9,108 | — | — | 5,141 | **1.8× faster** (caveat, §5) |

Per-statement latency (frigolite, 20k–50k-row tables): point select 5.2 ms,
matching UPDATE 5.2 ms (@20k) / 15.3 ms (@50k) — i.e. **proportional to table
size** — DELETE 9.4 ms (@20k, constant, even when the WHERE matches nothing),
plain INSERT 19–47 µs.

### CPU and memory (per side, whole run)

| Metric | frigolite | sqlite3 (literal) |
|---|---|---|
| CPU utilization (user+sys / wall) | **1.6–2.5×** (multi-core GC churn) | 0.97–1.0× (single-thread) |
| Peak RSS (phases) | 58 MB → 122 MB (grows with ops) | 6.5 MB → 20 MB |
| Cumulative heap allocated, 50k inserts | **10.8 GB with GC off (216 KB/stmt)** | ~O(few KB/stmt) |
| GC-off vs GC-on insert wall (50k) | 1.79 s vs 2.19 s wall — but 1.79 s vs **4.80 s CPU** | n/a |

CPU profiles of every slow phase are dominated by Go runtime GC coordination
(`runtime.kevent` 26–57%, `pthread_cond_wait/signal`, `madvise` 10–95%), not
engine logic: the per-statement allocation volume is so large that repeated
statements run GC-assist nearly continuously. Under `GOGC=off` the insert
phase is ~19% faster wall-clock but allocates 216 KB per INSERT statement
(94% of it from `btree.saveAllCursors`, §3.5).

## 2. Method

- frigolite: scratch program under `/tmp/perf/frigo` (module `replace` to the
  repo), drives `frigolite.Open(":memory:")` / `Exec` / `Query` directly.
  No prepared-statement or bind API exists in the public surface, so SQL text
  is re-parsed per statement with integer literals inlined.
- sqlite3: scratch cgo program under `/tmp/perf/ssql` linking the system
  `libsqlite3`, identical phases, both modes above.
- Phases: txn-insert → point-selects → aggregate range scans → GROUP BY →
  txn-updates by rowid → txn-deletes by rowid → file-backed autocommit
  inserts (fresh db file per side).
- Timing: `time.Now` per phase; CPU via `getrusage(RUSAGE_SELF)` deltas
  (same process, both sides); peak RSS via `ru.Maxrss`; Go heap via
  `runtime.ReadMemStats`; profiles via `runtime/pprof` (CPU + allocs).
- Statements verified semantically on both sides (row counts asserted; same
  7919-stride point keys, same 7-modulo payload values).
- Sizes: sqlite3 phases 30k–3M ops; frigolite phases 1k–50k ops sized for the
  time budget (some frigolite phases would otherwise run for hours, see §3.1).
  Comparisons use ops/s ratios, never totals.

## 3. Bottlenecks (evidence → root cause)

### 3.1 UPDATE: uniqueness conflict check full-scans the table per updated row
**Impact:** the worst measured path — 5.2 ms/statement @20k rows, 15.3 ms
@50k (cost ∝ table size). Any multi-row UPDATE via the public API takes
minutes. **Evidence:** probe — matching autocommit UPDATE = 5.8 ms/op at
20k rows but **0.065 ms** when the WHERE matches nothing; cost scales
linearly with row count. CPU profile: 1.1 ms/op inside
`DMLExecutor.checkLiveTableConflictsWR`, rest GC churn.
**Root cause:** `internal/execdml/update_check.go` — `checkUpdateConflicts`
gates on "table *has* unique/PK columns", then
`checkLiveTableConflictsWR` (update_check.go:102) walks the **entire table
b-tree for every updated row** to detect UNIQUE conflicts. It runs even when
the UPDATE changes no constrained column (`SET c=c+1` on an INTEGER PRIMARY
KEY table) and the rowid is unchanged. SQLite skips constraint checks for
constrained columns whose values did not change (`sqlite3GenerateConstraintChecks`
aUpdateFlag/chngRowid logic) and checks the rest via index lookups (O(log n)).

### 3.2 SELECT: rowid-alias equality never takes the seek fast path
**Impact:** 5.2 ms vs 0.016 ms per point lookup at 50k rows (300×); the
single most common OLTP shape. **Evidence:** probe —
`WHERE rowid=25000` = 0.01 ms; `WHERE id=25000` (same table,
`id INTEGER PRIMARY KEY`) = 5.07 ms — while **EXPLAIN QUERY PLAN reports
`SEARCH t USING INTEGER PRIMARY KEY (rowid=?)` for both**: the plan is right,
the executor ignores it (plan/execution mismatch).
**Root cause:** `internal/execquery/rowid_seek.go` — `selectRowidSeekConst`
matches only `IsRowIDName(ref.Name)` (literal `rowid/_rowid_/oid`). The IPK
rowid-alias resolution that DML already has (`isIPKRowidAliasCol`,
internal/execdml/insert.go:279 — `PrimaryKey && !PKDesc && type INTEGER`) was
never carried over to the SELECT gate, so every `WHERE <ipk>=const` falls
back to a full scan with filter.

### 3.3 SELECT: rowid range constraints are never sought
**Impact:** `WHERE rowid BETWEEN a AND b` (and `<`, `>`) scans the whole
table — 3.5 ms for an 11-row answer at 20k rows; any range/keyset pagination
query degrades to O(table). **Evidence:** `EXPLAIN QUERY PLAN` = `SCAN t`
for rowid BETWEEN (sqlite3: `SEARCH ... (rowid>? AND rowid<?)`);
probe timings match scan cost. **Root cause:** `selectRowidSeekRows` handles
only equality (`selectRowidSeekConst`); no range-seek path exists in the
scan engine (btree cursor `SeekToRowID` + forward iteration would serve it).

### 3.4 DELETE: per-statement whole-database snapshot
**Impact:** 9.4 ms per DELETE statement at 20k rows **regardless of matches**
(no-match DELETE measured 9.42 ms) — O(database size) per statement; DELETE
loops over N statements are quadratic in total.
**Root cause:** `internal/execdml/delete.go` `execDeleteBulk` takes
`dbCtx.Pager.Snapshot()` per statement (FK-failure rollback protection), and
`internal/pager/pagersnapshot.go:11` **deep-copies every cached page**
byte-for-byte. The comment records that warming was already removed once for
quadratic behavior, but the copy-per-statement remains. SQLite rolls a
statement back from the rollback journal's before-images — O(modified pages),
O(1) when the statement modifies nothing.

### 3.5 btree cursor registry: quadratic growth, finalizer-cleaned
**Impact:** dominates INSERT at scale (measured O(n²): 20k→40k inserts =
3.0× time, 40k→60k = 2.0× under GOGC=off; 191 KB allocated per INSERT at
50k rows, 9.56 GB of 10.16 GB total = 94% from `saveAllCursors`) and adds
`SetFinalizer` churn to every statement.
**Root cause:** `internal/btree/btree_cursor_save.go` — a per-(pager, root)
cursor list appended by every `OpenCursor` and pruned **only by finalizer**
(no explicit cursor close exists in the engine). `saveAllCursors` runs at
every mutation entry point and allocates `make([]*Cursor, 0, len(list))`
over the ever-growing stale list; every *positioned* stale cursor also gets
`saveCursorPosition` (key copy + page-cache clear). SQLite closes cursors
deterministically at statement end (`sqlite3Vdbe` frame cleanup) and its
`saveAllCursors` therefore sees O(open-cursors), not O(cursors-ever).

### 3.6 INSERT conflict paths: linear rowIDExists
**Impact:** REPLACE / rowid-conflict INSERTs degrade to O(table) per row.
**Root cause:** `internal/execdml/insert.go:300` `rowIDExists` walks the
b-tree cell by cell; `cursor.SeekToRowID(rowid)` answers in O(log n). Same
fix class as 3.1 (index-style lookup instead of scan).

### 3.7 Systemic: per-statement allocation volume and GC pressure
**Impact:** every repeated small statement is GC-bound: CPU samples are
60–95% runtime coordination (`kevent`, cond vars, `madvise`); GC roughly
doubles CPU time vs GOGC=off; peak RSS 5–8× sqlite3's. Parse+AST+exec
allocation happens per statement because the public API has no
prepare/reuse path. The eval-engine scan throughput (1.45M rows/s vs 53M) is
the standing P9.PERF gap (improved −24..−29% in the speed1p round, still 36×).

## 4. Fix plan (priority = impact ÷ effort; each lands with bench + suite proof)

| # | Fix | Scope | Expected effect |
|---|-----|-------|-----------------|
| P1 | **UPDATE change-detection gate**: skip uniqueness checks when no UNIQUE/PK/rowid column's value changed (mirror sqlite3GenerateConstraintChecks aUpdateFlag/chngRowid); index-probe (not scan) when they did | `update_check.go` gate + `checkLiveTableConflictsWR` | UPDATE 15,000× → <10× gap; unblocks txn update workloads |
| P2 | **IPK-alias rowid seek** in SELECT: resolve `id` → rowid in `selectRowidSeekConst` + `selectRowidSeekGate` (replicate `isIPKRowidAliasCol` predicate in `execquery` — layering forbids importing `execdml`); keeps EQP/exec consistent | `execquery/rowid_seek.go` + small predicate | point SELECT 300× → ~5× |
| P3 | **rowIDExists → SeekToRowID** | `execdml/insert.go:300` (3-line loop → seek) | REPLACE/conflict INSERT O(log n) |
| P4 | **rowid range seek**: BETWEEN/`<`/`>`/IN on rowid (and IPK alias after P2) seek + iterate; EQP SEARCH text parity | scan engine seek path | range queries O(log n + k) |
| P5 | **DELETE rollback via before-image journaling** (or lazily capture Snapshot at first page-dirty): O(modified pages) instead of O(database) | `pager` + `delete.go` snapshot site | DELETE 9.4 ms → ~0.1 ms per statement |
| P6 | **Cursor lifecycle**: explicit statement-end cursor release (engine defer, mirrors sqlite3Vdbe cleanup) + `saveAllCursors` fast-path (no alloc/scan when no positioned foreign cursor); retire finalizer-based pruning | `btree_cursor_save.go` + exec statement boundaries | kills O(n²) insert/update growth + finalizer churn |
| P7 | **Alloc diet + prepare/reuse API** (standing): statement prepare-handle, lexer/AST reuse, boxed-value reduction; eval-engine row throughput per P9.PERF | exec/parse hot paths | narrows the residual 2–10× |

Verification per fix: rerun this benchmark (harness scripts + probes under
`/tmp/perf`, method in §2), then the targeted suites (conflict3, upsert4,
altertab3, where/join/select families, eqp tests for P2/P4 plan text) and the
full census. P1/P2/P3 are small and independent — natural one-session fleet
tasks; P5/P6 are engine-level (worktree branches, merge through fleet
protocol).

## 5. Caveats

- The file-autocommit result (frigolite 1.8× faster) is almost certainly
  **fewer fsyncs, not speed** — verify journal/sync semantics against
  sqlite3's `PRAGMA synchronous=FULL` default before treating it as a win.
- sqlite3-side literal mode still re-compiles SQL per call; its compiler is
  ~0.5–2 µs/statement, so the residual frigolite-vs-literal gap after P1–P6
  isolates true execution cost.
- Apple's libsqlite3 3.54 build was used, not a from-source build; ratios at
  this magnitude are insensitive to that delta.
- Single connection, no WAL, no concurrent readers — CRUD microbench scope.
