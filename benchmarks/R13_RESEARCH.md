# R13 RESEARCH — indexed access paths: the remaining parity blocker (2026-10-08)

Base: frigolite `main` @ `ae17fd019` (PERF.PARITY8 close, R12 merged) vs sqlite3
**3.54.0** (Apple SDK `libsqlite3`, CGo scratch harness). Literal mode on both
sides (one freshly built SQL string per operation, no prepare/bind on the
frigolite side — the only mode the public `Open/Exec/Query` API allows), single
connection, in-memory unless noted, 50 000-row table unless noted. Read-only
research: **no engine code changed** (one prototype was measured and reverted,
§2d). Raw harness sources: `/tmp/perf/{frigo,ssql,v2frigo,v2ssql,idxops,
idxcssql,idxscale,orderemu,scanshape,ssqlshape,dmlprof,giantprof,diag}`.

---

## 0. Review of the current benchmark (finding before finding)

**The "canonical harness" that produced every `PERF.PARITY*` ratio is not in the
repository.** `FLEET-STATE.md:637` and `:757` place it in `/tmp/perf`
(`frigo/`, `ssql/`, `probe*/`); `/tmp` no longer holds it. The ledger numbers
(insert 1.45x, point 1.14x, scan 1.04x, update 1.58x, delete 1.65x) are
therefore **not reproducible or regression-checkable from the repo** — the same
class of gap the R11 note flagged for the stale `go.mod replace` window.
**This round commits the reconstruction** as
`benchmarks/perfbench/{frigolite,sqlite}` (same flags on both sides; `-index`
adds the secondary-index shapes of §1). Every number below is reproduced by:

```sh
cd benchmarks/perfbench/frigolite && go run . -reps 3 [-index]
cd benchmarks/perfbench/sqlite   && go run . -reps 3 [-index]   # libsqlite3 + CGo
```

A reconstruction of that harness (per-statement `Exec`/`Query` loops, op shapes
per `PERF_REPORT_2026-09-28.md` §2 and R11) reproduces the ledger's order of
magnitude and confirms it for the shapes it measures:

| phase | frigolite ops/s | sqlite literal ops/s | ratio | ledger claim |
|---|---|---|---|---|
| INSERT ×50k, 1 txn | 989 473 | 1 104 032 | **1.12x** | 1.45x |
| point SELECT `WHERE a=?` | 921 203 | 805 152 | **0.87x (faster)** | 1.14x |
| point UPDATE `WHERE a=?` | 515 066 | 871 104 | **1.69x** | 1.58x |
| point DELETE `WHERE a=?` | 973 019 | 987 065 | **1.01x** | 1.65x |
| file autocommit INSERT | 11 303 | 5 208 | **0.46x (faster)** | ~1.14x faster |
| scan `SELECT sum(b) FROM t` | 23 107 429 rows/s | 68 375 872 rows/s | **2.96x** | 1.04x (different shape) |
| GROUP BY `b%7` | 89 passes/s | 255 passes/s | **2.87x** | faster (different shape) |

So: rowid-keyed CRUD is genuinely close to parity (point/delete/file at or
better than parity, insert 1.1x, update 1.7x), and the ledger's scan/group
parity is **shape-specific** — it holds for the aggregate lane it was measured
on, not for expression scans or group-by (§3).

### 0b. A second harness-level gap: batch (multi-statement) scripts

The same statements cost **~2.4x more per statement** when sent as one
multi-statement string (the `speed1` shape used by `BenchmarkPerfInsert1`)
than as one call per statement:

| form (20 000 × `SELECT c FROM t WHERE a=?`) | ops/s |
|---|---|
| one multi-statement `Exec` | 281 341 – 295 878 |
| one `Exec`/`Query` per statement | 688 983 – 921 203 |
| sqlite, multi-statement `Exec` | 954 821 |
| sqlite, one call per statement | 805 152 |

CPU profile of the batch form: 30 of 70 ms inside `exec.(*Engine).PrepareExec`
— the whole script is parsed up-front and the per-statement template cache is
not on that path (R11's "template-caches" win does not apply to a batch text).
sqlite pays nothing comparable. Any workload that submits scripts (migrations,
`speed1`-shaped loops, the JSON harness itself) sees this 2x.

---

## 1. Headline: every secondary-index access path is O(table)

50 000-row table `t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT)` + `CREATE INDEX
i1 ON t(b)`; per-op cost, same shapes on both engines:

| operation (with `i1`) | frigolite | sqlite 3.54 | gap |
|---|---|---|---|
| SELECT `WHERE b=?` | 7.37 ms | 2.4 µs | **3070x** |
| UPDATE `SET c=? WHERE b=?` | 4.38 ms | 2.9 µs | **1510x** |
| DELETE `WHERE b=?` | 2.90 ms | 2.6 µs | **1115x** |
| UPDATE `SET b=? WHERE a=?` (index maintained) | 58 µs | 3.2 µs | **18x** |
| DELETE `WHERE a=?` (index maintained) | 2.10 ms | 1.8 µs | **1170x** |
| SELECT `WHERE a=?` (rowid control) | 5 µs | 1.4 µs | 3.5x |
| rowid point-DELETE loop, no index → with index | 897k ops/s → **485 ops/s** | 935k → 460k | 1850x vs 2x |
| DELETE k rows in one statement (k=1 / 10 / 100) | 2.6 ms / 75 ms / 665 ms | 4 µs / 22 µs / 133 µs | 660x / 3400x / **5000x** |

Scaling (`idxscale` probe, per-op): indexed lookup 282 µs @1k rows → 764 µs
@5k → 2.88 ms @20k → 7.40 ms @50k: **linear in table size** (≈145 ns/row), and
a *miss* costs the same as a hit. The rowid control stays flat at 3–5 µs
across the same sizes. An index therefore makes frigolite slower than the
scan it replaces (`WHERE b=?` 7.37 ms with `i1` vs 5.85 ms without), and
merely *having* an index turns a rowid DELETE loop from 897 000 ops/s into
485 ops/s (index maintenance walks the whole index per deleted row) where
sqlite pays 2x.

**Interpretation:** the PERF.PARITY ledger measures rowid-only shapes on an
index-free table. Real workloads (any secondary index, any `WHERE <indexed
col> = ...`) are 1000–5000x off parity and degrade with table growth. This is a
bigger parity blocker than every remaining item in R11 §5, and it is invisible
to the current benchmark.

---

## 2. Root causes (file:line, each measured)

### a. SELECT has no index-access execution path

`SELECT a FROM t WHERE b=100` — the planner reports `SEARCH t USING INDEX i1
(b=?)` (`EXPLAIN QUERY PLAN`, `explain_index.go:245`) but execution never
seeks. CPU profile of 200 such queries (`/tmp/perf/idxprof/cpu.prof`, 50k rows):

```
SelectEngine.execSelect
  execSelectScanPhase -> TableScanner.ScanTable -> scanTableRowsWithSQL   56%
      -> scanState.runScan -> Cursor.ScanTableLeaves -> scanTableLeafPage 42%
  indexScanOrderIndex (40%) -> tableRowCount -> Cursor.ScanTableLeaves    41%
```

i.e. a **full table scan plus a second full walk** to emulate SQLite's
index-order emission (`select_scan_order.go:20` → `explain.go:169
tableRowCount` → `sortScanRowsIndexOrder`, `select_scan.go:460`). The
cursor-side primitives for a real seek already exist and are value-ordered
(`Cursor.SeekToKey`, `btree.go:643`), but the query executor does not use them
for indexed predicates: `indexScanOrderIndex` is the only index awareness in
`select_scan.go` and it only fixes row *order*.

Isolated cost of the emulation (`orderemu` probe, 50k rows, per pass):
`SELECT b FROM t` (plain scan) 2.12 ms · `WHERE a<25000` (rowid range) 3.63 ms
· `WHERE b<25000` (indexed column) **10.29 ms** · `WHERE b=100` 7.34 ms.

### b. DML with a non-rowid WHERE runs the bulk table pipeline

Profile of `UPDATE t SET c=? WHERE b=?` (`/tmp/perf/dmlprof/upd.prof`):
`DMLExecutor.Update -> applyUpdateChanges -> BTree.DeleteCellsWhere ->
deletePass -> deleteAllMatchingFromLeaf -> decodeAllLeafCells` — a whole-table
sweep with page rewrite, per statement. Measured 4.38 ms/op (index) and
18.8 ms/op (no index) against sqlite's 2.9 µs. Same class of gap for
`DELETE ... WHERE b=?` (2.90 ms vs 2.6 µs).

### c. Index maintenance deletes by walking every index leaf

`execdml/delete_index.go:102/:123` → `btree.DeleteIndexEntry(s)`
(`btree_delete_leaf.go:32/:48`) collects **all** index leaf pages and matches
payloads leaf-by-leaf. For DELETE the maintenance runs per deleted row
(`deleteRowFromIndexes`), so cost is O(k × index pages): measured 2.6 ms
(k=1) → 665 ms (k=100) for one statement; no-index control 431 ms. sqlite
positions one index entry with `sqlite3BtreeMovetoUnpacked` and drops it
(OP_IdxDelete): 133 µs for the same 100 rows.

### d. Index candidate resolution still uses the pre-T31 leaf walk

`execdml/seek.go:302` resolves `col = <const>` candidates through
`btree.IndexKeyRowIDs`, whose body is an exhaustive `walkIndexLeaves`
(`btree_indexseek.go:37`). The premise documented at `btree_indexseek.go:3`
("trees are stored in RAW PAYLOAD BYTE order … value-equal entries are not
byte-contiguous") is **stale**: index trees have been ordered by the KeyInfo
comparator since `FULL-SUITE-DRIFT.T31-idxcoll` (2026-09-23, `b929f68f1`,
later than `e0390382d` which introduced the walk). `SetIndexKeyInfo`
(`btree_keyinfo.go:109`) installs `RecordPayloadCompare` and insert position,
interior routing, splits and `Cursor.SeekToKey` all order through it; a
frigolite-written index is read correctly by `sqlite3` (`PRAGMA
integrity_check` ok, indexed lookups correct) — i.e. the tree really is in
value order and the walk is a leftover.

Prototype (measured, then reverted): a lower-bound descent on the probe
comparator (`IndexRecordCompare` as the position comparator) + forward run of
equal entries, in `IndexKeyRowIDs`, with the walk as fallback and a
BINARY/ASC-only gate. It builds and is behaviour-preserving on the probes it
was exercised against, but yielded only **10–15%** on the shapes above (`upd-by-b`
4.38→3.81 ms, `del-by-b` 2.90→2.43 ms) because (b) and (c) dominate: the
descent is necessary but not sufficient, and it was not kept without a full
correctness validation.

---

## 3. Scan-shape matrix (frigolite vs sqlite, 50k rows, in-memory)

| shape | frigolite rows/s | sqlite rows/s | ratio |
|---|---|---|---|
| `SELECT count(*) FROM t` | 35 239 601 (1.42 ms) | 4 601 212 880 (11 µs) | **129x** |
| `SELECT sum(b) FROM t` | 23 271 915 | 62 567 784 | 2.7x |
| `SELECT b FROM t` | 22 574 255 | 26 065 878 | **1.15x** |
| `SELECT a, b, c FROM t` | 9 805 155 | 21 852 431 | 2.2x |
| `SELECT b%7, count(*) … GROUP BY 1` | 4 474 276 | 12 132 131 | 2.7x |
| `SELECT a FROM t WHERE b<25000` | 7 711 740 | 48 850 986 | **6.3x** |
| `SELECT count(*) … WHERE c LIKE '%7%'` | 3 855 212 | 33 150 269 | **8.6x** |

* `count(*)`: sqlite's `OP_Count` reads per-page `nCell` counts (~11 µs for
  50k rows); frigolite walks every cell (1.42 ms). Isolated 129x for the single
  most common aggregate.
* Narrow projection is at parity (1.15x); full-row projection and
  expression/aggregate scans are 2–3x.
* **Filtered scans are 6–9x**: the per-row filter/expression evaluation
  (`decodeAndFilterRow` → expression lane) is the residual, not the b-tree walk.

So "scan AT PARITY (1.04x)" describes the aggregate lane that the ledger
measured; it does not generalize to filtered scans, group-by or `count(*)`.

---

## 4. Ranked plan to reach parity

Ordered by (impact ÷ effort); each item needs the §5 protocol.

| # | Lever | Sites | Expected | Effort / risk |
|---|---|---|---|---|
| **L1** | Give the query executor a real index access path: seek (`SeekToKey`/lower-bound descent) + range iterate + rowid join; delete the `indexScanOrderIndex`/`tableRowCount`/`sortScanRowsIndexOrder` order emulation (output then arrives in index order natively) | `execquery/select_scan.go:460`, `select_scan_order.go:20`, `explain.go:169`, `bestindex.go`, `select_order_search.go` | indexed SELECT 7.4 ms → ~5–10 µs (≥500x); `WHERE <indexed col> <` 10.3 ms → ~sub-ms | High impact, medium-high effort (planner + executor + EQP text must stay identical — it already claims SEARCH) |
| **L2** | Index/rowid-driven row lookup for DML instead of the bulk sweep: plan `WHERE <indexed col> = ?` (and ranges) to candidate rowids, then point-update/point-delete per row (sqlite `where.c` + OP_Seek/OP_Update/OP_Delete loop) | `execdml/update.go` (`runPlainUpdate`/`applyUpdateChanges`), `execdml/delete.go`, `execdml/seek.go:302` | upd-by-b 4.4 ms → ~10 µs; del-by-b 2.9 ms → ~10 µs; non-indexed `WHERE b=?` 18.8 ms → O(matches) | Medium-high |
| **L3** | Index maintenance by seek: replace `DeleteIndexEntries`' all-leaf walk with a cursor seek to the single entry + `dropCell` (sqlite OP_IdxDelete); keep the batched walk only for rebuild/reindex | `btree_delete_leaf.go:32/:48`, `execdml/delete_index.go:102/:123`, `execdml/update.go:330` | del-by-a 2.10 ms → ~10 µs (**1170x → ~1x**); k-row statement 665 ms → ~k×5 µs (5000x → ~1x) | Medium: needs the cursor-path + rebalance path validated against the delete suite |
| **L4** | `OP_Count` parity for `count(*)`: aggregate page `nCell` counts (interior descent + leaf headers) instead of decoding cells | `execquery` aggregate lane / `btree` count helper | 1.42 ms → ~10 µs (**129x**) | Low |
| **L5** | Batch-script exec: per-statement template-cache path inside `PrepareExec` (or split-then-prepare) so a multi-statement text costs the same per statement as separate calls | `exec/engine` prepare path, `frigolite_exec.go:238` | 2.4x → ~1.0x on `speed1`-shaped scripts | Low-medium |
| **L6** | Rowid-op residue (R11 levers #2 journal re-read, #9 exec funnel preamble, #10 decode boxing): update 1.69x, insert 1.12x | R11 §5 items 1/2/7/9/10 minus those landed in R12 | update → ~1.2x | Medium; measure with the paired protocol |
| **L7** | Keep the committed harness (`benchmarks/perfbench`, this round) exercised per release so parity claims stay reproducible and CI-checkable | `benchmarks/perfbench/*` | — | Low; prerequisite for the rest |

Ordering rationale: L1–L3 turn 1000–5000x paths into ~1x and are all in the
same "missing access path" defect class; L4/L5 are cheap independent wins;
L6 is the tail of the current ledger; L7 makes the whole thing measurable.

---

## 5. Verification protocol per lever

1. **Real check first**: paired bench, one build per side, same op script,
   median of ≥3 reps; index shapes (§1) and the canonical shapes (§0) both.
   Engine setups that touch stateful paths run on a fresh file db too.
2. **Correctness**: `go test ./internal/btree/... ./internal/execdml/...`
   plus the JSON harness index/DML families (`FRIGOLITE_TEST=index…`), the
   dml/where/conflict/upsert pins, and the census + `tools/status audit`.
3. **Oracle**: plan text, row sets and error texts against `sqlite3` CLI for
   each new seek/range shape (value vs byte order is exactly where the last
   ordering tranche was wrong).
4. **Gates**: `tools/quality_gate.sh`, `go test -run TestSOLID_ ./...`.
5. A perf change with no before-number and no after-number is not admissible;
   reverted prototypes document themselves in this file.

---

## 6. Caveats

* The reconstruction is not byte-identical to the lost canonical harness:
  scan (`sum(b)` vs the ledger's aggregate-lane shape) and group shapes differ,
  hence the 2.96x/2.87x vs 1.04x/faster discrepancies in §0. Rowid CRUD ratios
  reproduce within ~0.35x.
* Shared machine: all figures are medians of 3 reps of the same binary; ratios
  are order-of-magnitude stable, not ±5% precise.
* The prototype in §2d is **not** validated engine code; it exists only as a
  measured indication that the descent alone does not fix the gap.
* All measurements are in-memory, single connection, no WAL. File-backed
  numbers amplify the commit-path levers (R11 #2) and are not re-measured here.
