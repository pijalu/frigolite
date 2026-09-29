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

---

# Fix Execution — 2026-09-29 rerun

All plan items P1–P6 landed and P7's first measured slice; every fix landed
with pure-Go probe evidence, targeted testgen suites, and the quality gates.
Merged to main via the fleet worktree protocol (branches
`fleet/perf-p1-update-gate`, `fleet/perf-p24-rowid-seek`,
`fleet/perf-p5-delete-journal`, `fleet/perf-p6-cursor-lifecycle`; P3 direct).

## Results after fixes (same harness, same sizes where comparable)

| Phase | before | after | speedup | vs sqlite3 literal |
|---|---|---|---|---|
| INSERT ×50k (1 txn) | 21,281 ops/s | 109,974 ops/s | **5.2×** | 60× → **11.6×** gap |
| SELECT point `WHERE id=?` | 192 ops/s | 100,857 ops/s | **525×** | 4,426× → **8.7×** |
| SELECT scan (rows/s) | 1,450,031 | 1,363,121 | parity | 36× → 39× (standing eval-engine gap) |
| SELECT GROUP BY | 14 passes/s | 15 | parity | 8.7× → 8.2× (standing) |
| UPDATE ×20k (1 txn) | 65 ops/s | 57,004 ops/s | **877×** | 15,152× → **17.4×** |
| DELETE ×1k (1 txn) | 133 ops/s | 53,313 ops/s | **401×** | 9,295× → **23.9×** |
| INSERT autocommit (file) | 9,108 ops/s | 9,290 | parity | fsync-bound both sides |

CPU utilization dropped from 1.6–2.5× wall to **1.2–1.4×** per phase; peak
per-phase heap fell from 18–122 MB to 2–39 MB. The GC-coordination profiles
(kevent/cond_wait 60–95% of CPU) are gone from the fixed paths.

## What landed

- **P1** (`320eac0ac`, `d073d2fca`): UPDATE uniqueness checks now skip when
  no constrained column's value changed (same comparators as the scan;
  re-keyed rows keep full checks; BEFORE-trigger paths excluded by
  design). Probe: 5.72ms → 79µs per single-row UPDATE @20k (73×), 14.15ms →
  159µs @50k (89×). Bonus fix found by parity probes: WITHOUT ROWID
  table-level PKs were not enforced by UPDATE at all — now synthesized from
  WRPKIndices, oracle-exact error text. Deferred with justification: the
  unique-index probe for changed constrained columns — today's index seek
  is an exhaustive leaf walk (byte-ordered storage), so it buys no
  asymptote until the value-ordered-index tranche.
- **P2+P4** (`bef1da784`, `372edf8cf`): IPK-alias equality seek (5.35ms →
  8–20µs @50k) and rowid range seek — BETWEEN/`<`/`>`/`<=`/`>=` with
  literal bounds, alias spellings included; EXPLAIN QUERY PLAN now renders
  from the same analysis the executor runs (28-shape battery, 0 diffs vs
  the sqlite3 CLI). Along the way: SeekToRowID now maintains the cursor
  path stack (iteration after a seek previously replayed rows), the IPK
  alias NULL-in-record/fill-from-rowid contract is honored by the seek
  row-source, `reverse_unordered_selects` reverses the range walk, and the
  range loop uses the scan's lazy two-phase decode. Pre-existing gap
  documented, not touched: rowid-vs-text eval ignores whitespace (diverges
  on the SCAN path too).
- **P5** (7 commits, tip `ac6fd7e14`): pager statement journal
  (`internal/pager/pagerstmt.go`, pager.c sub-journal port) — O(1) begin,
  first-modification before-image capture at the `markDirtyLocked` choke
  point, nested-scope splicing, whole-state snapshots kept only for
  BEGIN/SAVEPOINT/memdb/FTS-index scopes. DELETE additionally plans
  absent-rowid seeks as empty candidate sets and sparse (≤64) deletes seek
  via DeleteCellByRowID. No-match DELETE 15.6ms → **8.5µs (~1000×)**;
  matching single-row DELETE 4.4ms → 167µs. The agent ran the full testgen
  corpus (1363 packages) with zero failures before push.
- **P6** (`20f7fb424`..`3512267bf`): deterministic cursor lifecycle —
  `BTree.Close()` ownership model, statement-funnel release (`Engine.Exec`
  defer with nested-Exec segment marks so enclosing scan cursors survive
  inner trigger/eval DML — the misc8-1.6 contract stays green), and the
  `saveAllCursors` fast-path; finalizer kept as safety net. Insert
  allocation fell from 94→254KB/op (quadratic) to **~10KB/op flat**; the
  insert slope is linear (105k ops/s flat at 20k/40k/60k).
- **P3** (`65746ffe6`): `rowIDExists` seeks instead of scanning (REPLACE
  3.14 → 2.59ms/op @20k; the residual is allocation churn, see P7).
- **P7 (first slice)** (`1d4317e02`): short-circuit `echoVTabSource`
  before `parseVTabSQL` for plain tables — every DML statement probed the
  schema entry and paid a formatted-error allocation for the "not a vtab"
  answer. Remaining P7 tranche, scoped with alloc profiles: btree
  split-cell repacking (`partitionSplitCells` 33% + `MakeNoZero` 19% of
  insert-phase bytes) wants page-cell pooling; index defs re-parse per
  statement (`indexDefsIn` → `FindTable`, ~1KB/stmt) wants a schema-level
  cache; a prepare/reuse API is NOT the lever (parse is µs-level and an
  AST template cache already exists) — the standing eval-engine row
  throughput (scan 36–39×, GROUP BY 8×) remains the biggest residual and
  belongs to P9.PERF.

## Verification

- Probes per fix (numbers above), all pure-Go driving Open/Exec/Query.
- Targeted suites per fix: P1 50 suites (conflict/upsert/without_rowid/
  update/fkey/trigger/altertab families); P2+P4 all where*/eqp*/rowid*/
  select*/join*/index*/limit* matches; P5 the FULL 1363-package testgen
  corpus (exit 0, zero FAIL) plus journal/savepoint/fkey/trigger/vacuum/
  wal/integ risk dirs; P6 ~80 packages including misc8, trigger*, fkey*,
  vacuum*, fts4merge{,2,3,5}, plus a -race subset of btree.
- Merged-main full non-testgen suite: every failure triaged pre-existing
  (TestSQLiteSuite legacy drift; P8IncrVacuum3 randomblob flake;
  TestRtreeStressChurn a pre-existing map-order-dependent rtree churn
  flake, 4/60 at base vs 2/60 after — follow-up filed;
  TestWindowCGroupConcatBlobUTF16 passes isolated at base and after —
  full-suite ordering artifact).
- Census + SOLID + quality gates re-run at merge completion (see
  FLEET-STATE): **census 1073 pass / 0 fail / 290 skip — identical counts
  to the pre-PERF baseline.** Method note: the default 8-worker census
  pool flags ~15 long wall-clock suites (fts4merge4, avtrans, fts3b, …)
  as contention flakes; all of them pass serially and got FASTER with the
  fixes (fts4merge4: 601s at base → 543s after P24 → 440s after P6 → 427s
  after P5), so the authoritative post-merge census runs at
  `--concurrency 2 --timeout 25m`. One real regression WAS caught by the
  census and fixed: P1's first gate skipped the row write (not just the
  uniqueness scan) in the OR FAIL path — check-6.5/6.6 — restored plus
  native guard `TestUpdateOrFailKeepsPriorRows` (`768eae135`).
