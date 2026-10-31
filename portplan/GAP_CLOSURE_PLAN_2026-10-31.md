# GAP CLOSURE PLAN — 2026-10-31 (session plan, instantiates UNIFIED_PARITY_PLAN)

> Scope: review the current status and the gap against SQLite 3.54
> (`../sqlite`) — parity **and** performance — and close it. This document is
> the *session-executable* slice of `portplan/UNIFIED_PARITY_PLAN.md` (the
> master, still binding) with fresh evidence. Root bindings stay: PARITY_PLAN
> §0 (no descoping, every failure owned), GUIDELINES §1 (no try/fail, no
> simplify), UCL-first, engine edits and full-suite runs never overlap.

---

## 1. Status review (evidence, 2026-10-31)

### 1.1 Parity state
- **Baseline**: run 33 = **3,481 failing leaves / 309 families**
  (commit `e8faf18ae`); session totals runs 30→33: −292 leaves,
  regression-free. testgen census **1048 pass / 25 fail / 290 skipped**
  (the 25 pre-exist at `7f8441f73` — drift-triage goal queued).
- **Quality**: staticcheck 0; SOLID architecture test green; pre-commit gates
  active.
- **Work in progress (uncommitted on `feature/perf`)**: the *where8
  search-count unit* — engine side landed (Engine.searchCount mirroring
  vdbe.c increment sites, `IncSearchCount` wired through DMLContext /
  SelectContext, `OrRange.LikeGlob` + OR-plan delta with the `beyond[]`
  EOF-step model in `internal/execdml/or.go`). **The converter/harness half
  is missing**: where8's `execsql_status`/`execsql_status2` helpers render
  `[rows…] [step] [sort]` (bare) and `[rows…] [step] [sort] [search_count]`
  (search count reset *before* the probe query); the current fixture renders
  the where7-style labeled form `scan N sort M`. where8 = 35 leaves red.

### 1.2 Performance state — FRESH measurement (perfbench, this session, 50k rows, `:memory:`, median/reps)

| lane | frigolite ops/s | sqlite ops/s | gap |
|---|---|---|---|
| insert (rowid) | 913k | 1,071k | **1.17×** |
| point-select | 852k | 770k | **at parity / faster** |
| point-update | 577k | 824k | 1.43× |
| point-delete | 837k | 933k | 1.12× |
| scan (rows/s) | 22.6M | 66.1M | **2.9×** |
| count-star | 1.27G | 9.21G | **7.2×** |
| group-by | 81 | 249 | **3.1×** |
| file-insert (autocommit) | 9,012 | 4,186 | **2.2× faster** |
| insert (w/ index) | 291k | 692k | **2.4×** |
| indexed-select | 121k | 639k | **5.3×** |
| indexed-update | 133k | 539k | **4.1×** |
| indexed-delete | 124k | 668k | **5.4×** |

Read: rowid CRUD is at parity; the remaining perf gap is **(a) secondary-index
lanes (2.4–5.4×), (b) scan/aggregate read paths (2.9–7.2×)**. This matches
R14 §7's attribution; the F1 residue levers target exactly these lanes.

---

## 2. The gap, decomposed (3,481 leaves → units)

| cluster | families (leaves) | unit | track/milestone |
|---|---|---|---|
| where8 search count | where8 (35) | finish WIP: converter `execsql_status`/`status2` shapes + harness render + engine counters | **W** / M2 |
| expression indexes | indexexpr2 (50) | EQP/rows probe vs oracle, then planner fix | **R**→implement / M2 |
| named results | select1 (32) | fresh-fixture `query2` renderer mismatch | **R**→implement / M2 |
| normalize.c | normalize (47) | port ext/misc/normalize.c → `internal/normalize` + Stmt API + harness step | **N** / M2 |
| xfer optimization | insert4 (30) | INSERT…SELECT transfer opt + `sqlite3_xferopt_count` | M2 |
| reindex rules | e_reindex (97) | integrity_check index/table mismatch checks | M2 |
| small-cluster residue | memdb/func2/func4/stat/rowvalue (18–19 each), e_expr (22), func auxdata (37), fts3expr4 ICU (20) | per-family probes | **R** / M2 |
| testvfs/fault VFS | e_walckpt (296), pagerfault (59) | P-C1 testvfs wrapper VFS | M3 |
| multiplex VFS | multiplex (207) | P-C multiplex.c port | M3 |
| shared cache + unlock-notify | notify1 (123) | P-C shared-cache goal | M3 |
| WAL snapshot API | snapshot (31), wal2/wal/walbak (182) | P7.WAL-G7 residue | M3 |
| pager1 | pager1 (225) | probe malformed-class first | M3 |
| UTF-16 | like3 (33) | P-C11 UTF-16 (last) | M3 |
| planner tail | where (27), bestindex* residue (~85) | B4/B5 continuation | M2/M3 |
| testgen drift | 25 packages | drift triage | queued |
| perf indexed lanes | — | F1a (#5 WHERE-ref cache), F1d (findChildIndexForInsert decode diet) | **P** / M6-F1 |
| perf scan/aggregate | — | F1c (#8 read-side decode-free compare), F1b (#7 rowmap slice), F3 | M6-F1/F3 |

## 3. Execution map — this session (parallel tracks, disjoint file ownership)

Binding constraint: engine edits and full-suite runs never overlap; one active
unit per track; harness file (`frigolite_harness_test.go`) owned by Track W
only this session. Parallel agents on *disjoint packages*.

| track | owner | files (exclusive) | deliverable | validation |
|---|---|---|---|---|
| **W** where8 | main agent | `tools/tclconvert/tcl/interp_sql.go`, `frigolite_harness_test.go`, `internal/execdml/or.go` (finish) | converter models `execsql_status`/`execsql_status2` (bare render, search-count element + reset); harness renders 3- and 4-element forms; regenerate where8 fixture; oracle-verify deltas | `FRIGOLITE_TEST=where8` → 0; battery families byte-stable; commit |
| **P** perf F1a | agent perf | `internal/execquery/select_index_seek*.go`, new cache file | implement R14 §7 #5 (cache WHERE-ref analysis per statement shape) | go test execquery+root; harness leaf-sets identical on index/where/select; EQP diff; perfbench before/after paired |
| **N** normalize | agent norm | new `internal/normalize/` only | port ext/misc/normalize.c (717 lines) + native Go tests vs oracle (`go-sqlite3` scratch in /tmp) | `go test ./internal/normalize/...`; byte-parity probes vs oracle |
| **R** probes | agent probe | read-only + `/tmp` scratch | root-cause reports: indexexpr2, select1, e_reindex 1.2–2.x, memdb/func2/func4/stat/rowvalue, func auxdata, fts3expr4 ICU, testgen-25 drift census | each report: exact error class, oracle behavior, native repro, proposed unit card |

Serialization: after tracks land, main agent runs the family-scoped battery
(index/where/select/delete/analyze/collate/upsert/conflict/insert), then a
full-suite checkpoint run (serial, no engine edits in flight), then commits
per unit with run stamps.

## 4. Next tranches (queued after this session)

1. **T2 (M2 finish)**: indexexpr2 + select1 + insert4 xfer-opt + e_reindex +
   small-cluster fixes from R's reports (parallel by family, harness
   serialized).
2. **T3 (M3 open)**: testvfs P-C1 (unlocks e_walckpt 296 + pagerfault 59),
   multiplex VFS (207), shared-cache (123) — the three biggest feature gaps;
   WAL snapshot API; pager1 probe results; UTF-16 last.
3. **T4 (M6-F1 finish)**: F1c read-side decode-free compare (scan 2.9× →
   target ≤2×), F1b rowmap slice, F1d findChildIndexForInsert; F3
   count-star/group-by paths (7.2×/3.1×).
4. **T5**: F2 plan cache (statement-shape), F7 perf gate.
5. **T6**: Q2–Q5 re-close; testgen drift triage; M7 closeout matrix.

## 5. Progress log

| date | event | evidence |
|---|---|---|
| 2026-10-31 | plan written; fresh perfbench baseline recorded (§1.2); where8 WIP audited (engine landed, converter/harness missing); tracks W/P/N/R launched | this file |
| 2026-10-31 | **Track W tranche 1 landed** — where8 35→**18**: converter models `execsql_status`/`execsql_status2` (bare render + search-count reset via counter markers + `applyStatusStyle`); harness renders bare `N M` and bare `N M S`; vdbe-calibrated search accounting (full-scan Nexts = rows−1; sorter = −1 + successful delivery Nexts; OR-plan branch seeks unconditional, nexts = matches (beyond) else matches−1, movetos = unique rows); `ColumnValue` unwrap in the OR `beyond[]` bound check; two bounds on one column merge into one OR branch (SQLite's combined `a>? AND a<?` range — where8-1.11). Pin `TestDBStatusBareStyleAndSearchCount`; regen where8 fixture byte-identical modulo the new fields; battery byte-stable (where7 0, where 26, index 2, delete 6, analyze 1, collate4 1) | commits `M2.W8 tranche 1` |
| 2026-10-31 | where8 residue (18) = one class: our executor full-scans where SQLite seeks (1.8 `a>1 AND c LIKE`, 1.12.1 `a IN(...) OR a=5` IN-branches, the 3.x join OR terms, correlated-subquery probes) + the index-walk increments in `select_index_seek*` (queued behind Track P's F1a cache). vdbe calibration recorded: Seek* counted unconditionally (found or not), Rewind/Last/SeekRowid uncounted, OP_Sort −1 with NO recovering +1 (Rewind has its own break), SorterNext counts at next_tail | oracle EXPLAIN bytecodes, this file |
