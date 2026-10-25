# UNIFIED PARITY PLAN — SQLite feature parity · clean code · performance

> **Status**: ACTIVE (2026-10-24). The master **sequencing** plan for the three
> remaining work tracks. It coordinates — it does not replace — the existing
> plan of record:
>
> | document | role |
> |---|---|
> | `PORTPLAN.md` §5e/§5g | the binding Definition of Done + Anti-Drift Protocol (unchanged) |
> | `portplan/PARITY_PLAN.md` | Track P goals + binding roots (§0) + exit criteria |
> | `portplan/PARITY_EXECUTION.md` | per-family work register, batching procedure, stop rules |
> | `portplan/TODOS.md` | the live per-unit checklist for Track P |
> | `benchmarks/R13_RESEARCH.md`, `R14_RESEARCH.md` | Track F measurements and levers |
> | this document | the three-track consolidation: state, targets, sequencing, cross-track rules |
>
> **References**: engine source of truth `/Users/muaddib/dev/sqlite/src/` + `ext/`
> (3.51.0, `../sqlite`); harness corpus `ori/sqlite/test` (3.54-equivalent,
> 1,221 `.test` files); CLI oracle `/usr/bin/sqlite3` (3.51.0); Tcl oracle
> `/opt/homebrew/bin/tclsh`.
>
> **Corpus-vs-source rule**: engine implementation decisions follow the
> `../sqlite` source (per AGENTS.md / GUIDELINES). Where a 3.54 corpus fixture
> expects behaviour that differs from 3.51, the fixture governs the *expected
> value* and the divergence is recorded in `.agents/lessons_learned.md` with an
> oracle probe against a matching build.

---

## 1. Current state (verified 2026-10-24, branch `feature/perf`, HEAD `7f8441f73`)

`go build ./...` exits 0. Three measured surfaces:

### 1.1 Test surfaces

| surface | scope | state | stamp |
|---|---|---|---|
| **testgen** (transpiled Go tests) | 1,363 packages | **1,073 pass / 0 FAIL / 290 skip** (all skips NA_EVIDENCE-audited) | `tools/status/last_run.json` 2026-10-08 (ledger baseline 2026-09-28: 1,072/0/290) |
| **TestSQLiteSuite** (JSON harness over `testdata/*.json`, 1,224 fixtures) | the 3.54 corpus | **1,072 failing leaves / 79 families** (run 29; down from 14,431/305 at run 18, 2026-10-20) | `/tmp/frigolite-full-suite29.log`, 2026-10-22 |
| native Go tests (`frigolite_*_test.go` + internal packages) | engine contracts | green at last full run (part of `go test ./...`) | run 29 |

The JSON harness is the active parity front. Its remaining red set (run 29,
biggest first, from the run-27/29 registers — regenerate the authoritative
register per `PARITY_EXECUTION.md` §4b before starting work):

| family | leaves | unit (TODOS.md) |
|---|---|---|
| `e_walckpt` | 295 | C4 — the `testvfs` wrapper VFS |
| `e_reindex` | 96 | C7 — the reindex rules |
| `analyze5` | 85 | D1 — missing Tcl functions (`alpha`, `mit`, …) |
| `e_droptrigger` | 53 | D1 — `list_all_triggers` residue |
| `indexexpr2` | 49 | B5 — planner (expression indexes) |
| `badutf2` | 41 | C — invalid-UTF-8 text handling |
| `func` (→37) | 37 | C3 — error texts + auxdata/frombind |
| `func2`/`func4` | 35 | C3 — error texts |
| `bestindexF` | 34 | B5 — planner (vtab constraints) |
| `e_blobopen` | 30 | C — incremental blob I/O harness surface |
| `insert4` | 30 | C — INSERT semantics |
| `e_expr` | 29 | A10 — expression corners |
| `fkey2` / `json101` | 24+24 | C2 / C — FK errors / JSON functions |
| `fts3defer2`/`fts3expr4`/`e_wal`/`index9`/`descidx2` | 18–20 each | D2 / C4 / B5 |
| ~60 tail families | ≤15 each | E3 |

Closed since run 18 (evidence in `TODOS.md` §5): where7 (2,021 → 0, planner +
counters + lsort), pager2 (1,937 → 0), incrvacuum_ioerr (3,999 → 0),
altercons2 (2,065 → 0), fts3expr/fts3expr2/fts3expr3 (525 → 0), rtree2
(2,010 → 102 → regen), conflict/conflict2 (129), collate4 (51 → 1), the 184
stale-fixture wave (pager1 542, multiplex 206, notify1 122, where 120, wal2
113, rtree2 102, wal 68, memdb 41 → 0 — stale fixtures, not engine defects).

### 1.2 Performance state (R14, 2026-10-23; 50k rows, `:memory:`, literal SQL)

| lane | frigolite | sqlite 3.54 | gap | note |
|---|---|---|---|---|
| `SELECT c WHERE a=?` (rowid) | 1.10 µs | 1.20 µs | **faster** | at parity |
| `UPDATE … WHERE a=?` (rowid) | 550k ops/s | 893k | 1.6× | |
| file autocommit INSERT | — | — | **2.2× faster** | R13 |
| `SELECT a WHERE b=?` (index) | 196k ops/s | 742k | **3.8×** | |
| `INSERT` + unique index | 219k ops/s | 949k | **4.3×** | |
| `UPDATE … WHERE b=?` (index) | ~88k ops/s | 681k | **7.7×** | was 239 ops/s before PERF.R14-1 (360× fix) |
| `DELETE … WHERE b=?` (index) | 298k ops/s | 935k | **3.1×** | |
| `COUNT(*)` / `SUM` / `GROUP BY` | 8.7× / 3.0× / 3.0× | — | — | R13 baseline; COUNT partly improved by R13-L4 |
| speed family wall clock | speed1p 445 s serial | — | ~10–100× | P9.PERF backlog |
| recursive CTE memory | sort3-3: ~1.4 GB live heap | streams via ephemeral table | — | W6.9/C6 |

Landed perf commits on this branch: R13-L1..L7 (real SEARCH/USING INDEX
executor, OP_IdxDelete-parity maintenance, OP_Count from page headers,
statement-at-a-time run scripts, index interior REAL entries, rowid-op
residue), PERF.R14-1..5 (indexed-UPDATE positioned deletes 360×, UNIQUE
conflict by seek 148×, EQP-only condition text, KeyInfo memoization ×2).
Full detail: `benchmarks/R14_RESEARCH.md` §9.

### 1.3 SOLID / clean-code state (measured 2026-10-24)

The 2026-09-27 §5d closure (gocognit>15 = 0, gocyclo>12 = 0, staticcheck = 0,
zero files >1,000 lines) has **drifted** under the October parity/perf
sessions:

| gate | 2026-09-27 | now (2026-10-24) |
|---|---|---|
| staticcheck `./...` | 0 | **9** (6× U1000 unused funcs, 2× ST1005, 1× SA4006 — list in §4.1) |
| gocognit > 15 (non-test, excl. testgen) | 0 | **132** |
| gocyclo > 12 | 0 | **133** |
| production files > 1,000 lines (hard) | 0 | **11** |
| production files 500–1,000 lines (soft) | — | 222 |
| `go vet ./...` / build | clean | clean |
| SOLID architecture tests | green | green at last run |
| race | clean (last full run) | re-verify at M5 |

Top complexity offenders (gocognit): `queryEachLiveJoin` 70
(`execquery/query_each.go:16`), `markJoinSourceColumns` 69
(`execquery/select_subq_unused.go:352`), `fts5 decodeStructRec` 66
(`fts5/structure.go:121`), `joinOnValidator.validateJoins` 64
(`execquery/select_join_validate.go:426`), `fts5 loadFromShadow` 53, the
`fts3_expr.c` port trio 52/51/46 (`fts/exprtest.go`), `renameColumnInTriggerSQL`
52 (`execddl/alter_quotefix.go:378`), `stat4RangeSelectivity` 42,
`decodeSQLiteFloat` 40, `applyJoin` 36.

Files over the hard limit: `execdml/update_split.go` 1,011 ·
`execddl/alter_rename.go` 1,335 · `execddl/alter_modify.go` 1,128 ·
`execquery/select_join.go` 1,059 · `execquery/select_colnames.go` 1,074 ·
`exec/engine_core.go` 1,054 · `exec/engine.go` 1,058 · `exec/vtab_dbpage.go`
1,008 · `tclconvert/tcl/interp_sql.go` 1,167 · `tclconvert/tcl/interp_cmds.go`
1,559 · `tclconvert/tcl/interp.go` 1,660.

Production LOC ≈ 235k (excl. tools). The pre-commit quality gate
(`tools/quality_gate.sh`, `git config core.hooksPath .githooks`) enforces the
per-task ceiling on *changed* files; the drift is legacy debt accumulated by
whole-file ports (fts3_expr.c) and the converter growth — it must be re-closed
repo-wide (§4), not normalized.

---

## 2. Exit criteria — the definition of "parity complete"

The program is done when **all** of the following hold (extending
`PARITY_PLAN.md` §1 with the Q and F tracks):

**P — behaviour parity**
1. `go test -p 1 ./... -count=1` passes; `TestSQLiteSuite` reports **0 failing
   leaves**; the run stamp is cited.
2. Skip lists (`slowTestFiles`, `unsupportedTestFiles`, `harnessSkipSubtests`)
   are unchanged or *shrunk*; every remaining skip is §1-genuine-N/A or carries
   current NA_EVIDENCE + a named native substitute test.
3. Every corpus family has a fixture generated by the **current** converter and
   installed (no batch-reverted stale fixture remains — E1), and the 102
   converter-skipped fixtures are converted (E2) or carry per-file evidence.
4. `tools/status` full run: testgen 0 FAIL; ledger green; `--check` passes.

**Q — clean code**
5. `staticcheck ./...` = 0, `gocognit -over 15` = 0, `gocyclo -over 12` = 0 on
   non-test production code (testgen fixtures excluded by policy), zero
   production files > 1,000 lines, soft-cap policy per §4.4.
6. `go vet ./...`, `go test -race -count=1 ./...`, `go test -run TestSOLID_ ./...`
   all green.

**F — performance**
7. Every perfbench lane ≤ **2×** sqlite 3.54 on the canonical harness (paired,
   same-shell, median-of-3); the rowid lanes stay at parity or better; the
   speed-family wall clock ≤ 4× C; the corpus full-suite RSS peak ≤ ~2.5 GB
   after the CTE streaming fix.
8. A repeatable perf gate exists in-repo (paired bench script + recorded
   medians) so regressions are caught at commit time, not rediscovered.

**Cross-cutting**
9. PORTPLAN §5e items 5–10 (oracle-verified, plan status updated, run stamps
   cited, commits pushed) — binding as-is.

---

## 3. Track P — feature & behaviour parity (finish the JSON-harness red set)

Method unchanged: the seven-step procedure (`TODOS.md` §4 / PARITY_EXECUTION
§4.1) — NAME → PROBE (pure-Go test first) → ENGINE (follow `../sqlite/src`)
→ PIN → CONVERT → BATCH → CLOSE. The register (`PARITY_EXECUTION.md` §3,
regenerated from the latest run log) is the work list; the units below are the
sequencing wrappers. **First action of Track P: run the suite, regenerate the
register, and reconcile the stale checkboxes in `TODOS.md` (A9/A11 show log
entries but unchecked boxes).**

### P-A. Fidelity residue (harness/converter) — cheap, do first

| unit | content | evidence |
|---|---|---|
| P-A1 | Close/verify A9 (`conflict`/`conflict2` foreach-body expectations — log claims fixed by regen), A10 (`e_expr` `$as$bs` non-expansion, 29 leaves), A11 residue (collate4 → 1 leaf) | `TODOS.md` §2.A |
| P-A2 | D2c — `fts3expr` braced-tree comparison normalization (69 leaves at run 27; verify current) | `TODOS.md` §2.D |
| P-A3 | D3 — named-DB lifecycle ("named database db2 is not open", ~40 leaves) + `vacuum-into`/`errmsg`/`trace2` classes | `TODOS.md` §2.D |
| P-A4 | D1 — the remaining testfixture surface: Tcl procs (`mit`, `swap_int32`, `rnode`, `force_rollback`, `myrandomblob`, `cksum`, `offrec`, `joinx cross`, `nondeter`, `external`, `sql_error`, `selectH_counter`, `::runsql`), `list_all_views`, `lock_proxy_file` pragma, missing collations (`text_collate`, `backwards_collate`, `nocase_collate`) → `analyze5` 85, `e_droptrigger` 53 | `TODOS.md` §2.D1 |

### P-B. The planner (semantics before speed — Track F depends on this)

| unit | content | SQLite reference |
|---|---|---|
| P-B1 | **B4: covering-index walk executor.** The plan text renders `SCAN t USING COVERING INDEX`, but no executor walks the index (`groupDistinct` reaches only the plan text); index *choice* must follow SQLite's record-size cost model (`cost = nRow*K` vs `nRow*3.0`) — the row order changes with the index. This is the largest engine lever left in the red set and the same machinery Track F needs. | `src/where.c` `whereLoopAddBtree` cost model, `OP_OpenRead`/`OP_SeekGE`+`OP_IdxGT` covering loops |
| P-B2 | **B5: planner families** — `where` residue, `where8` 35, `where9` 16, `indexexpr2` 49 (expression indexes), `index9` 19, `descidx2` 18 (descending indexes), `orderby5` 14, `bestindexF` 34 (vtab constraint ordering). One EQP-diff per family; golden EQP fixtures per UCL. | `src/where.c`, `src/whereexpr.c`, `src/wherecode.c` |

### P-C. Engine features (the honest C4-class gaps — one UCL goal each)

Ordered by leaf count; each gets a `plan/goals/` file + UCL tranche
(oracle fixtures BEFORE engine edits, PORTPLAN §5b item 0).

| unit | feature | families | SQLite reference | size |
|---|---|---|---|---|
| P-C1 | **`testvfs` wrapper VFS** — the installable wrapper VFS with controllable locking/side-file WAL semantics the WAL families drive; pure-Go VFS seam (the journal-sidecar hook of P7.WAL-E is the precedent) | `e_walckpt` 295 + testvfs families | `test/testvfs.c`, `src/vfs.c` | L |
| P-C2 | **reindex rules** — `REINDEX` collation-change rescanning, per-index and named forms | `e_reindex` 96 | `src/build.c` `sqlite3Reindex`, `reindexTable`/`reindexIndex` | M |
| P-C3 | **error-raising parity** (41 families ≈ 312 leaves): the conditions SQLite errors on and we accept — `vtab1`, `quota`, `rtreeA`, `fkey2` 24, `savepoint`, `with2`, `incrblob`, … | C2 set | case-by-case `sqlite3ErrorMsg` sites | M |
| P-C4 | **error-text parity** (32 families ≈ 130 leaves): exact message strings — `func` 37, `func2`/`func4` 35, `rtreedoc2`, `e_wal` 19, `vtab3`, `without_rowid3`, `check`, `upsert1`, … | C3 set | case-by-case | M |
| P-C5 | **invalid UTF-8 text handling** — `sqlite3_exec` accepts it; C-API length reporting | `badutf2` 41 | `src/util.c` `sqlite3Utf8ToBlob`-adjacent, `test1.c` | M |
| P-C6 | **function auxdata / frombind** (`test_auxdata`/`test_frombind`, test1.c) — `sqlite3_get_auxdata`/`set_auxdata` + `sqlite3_value_frombind` on the Go function API | `func` residue | `src/vdbeaux.c` `sqlite3GetAuxData`, `vdbeapi.c` | M |
| P-C7 | **blob-I/O harness surface** — engine `OpenBlob` exists (P5.BLOB); model the `e_blobopen` fixture steps | `e_blobopen` 30 | `test/test_blob.c` | S |
| P-C8 | **INSERT semantics residue** (`insert4` 30) + the remaining C-class singles (`json101` 24) | — | case-by-case | S |
| P-C9 | **recursive CTE streaming** — the recursive queue must live in the pager temp store (ephemeral btree), not a Go slice; doubles as the memory fix (exit criterion 7) and a perf lever | `sort3-3` 1.4 GB live | `src/select.c` `generateWithRecursive`, `OP_Queue`/ sorter+ephemeral tables | M |
| P-C10 | **ALTER residue** (C1: alterdropcol2, altercol, alterlegacy, altertab, strict1 ≈ 40 leaves) + STRICT table corners | `execddl/alter_*.go` | `src/alter.c` | M |
| P-C11 | **UTF-16 databases** (W4.6, `like3`) — page-level encoding, text conversion at the record boundary; the single largest remaining format gap. Sequence last in Track P; re-validate the enc3 supersession decision when it lands | `like3` + enc families | `src/utf.c`, `src/util.c`, header bytes 56–59 | XL |

Known-closed or regen-fixed C4 members (do **not** re-plan; verify only at the
full-suite gate): multiplex, notify1, wal2, wal, pager1, rtree2, memdb.

### P-D. Corpus completion

| unit | content |
|---|---|
| P-D1 | **E1 — batch-reverted families**: every family ever reverted by a batch ends the program installed-fresh with its failures fixed (the reverted list is a work queue, not an archive). |
| P-D2 | **E2 — the 102 converter-skipped fixtures**: raise the per-file statement budget where the file is legitimately large (never globally); conversion errors keep their installed fixture until the converter handles them. |
| P-D3 | **E3 — the tail** (~60–105 families ≤ 15 leaves): alphabetical batches of ~20, one-line root cause per family in the ledger; seven-step procedure per family. |

### P-E. Ledger reconciliation

At every full-suite gate: `tools/status` re-run (testgen must stay 0 FAIL),
ledger re-seed if states changed, `⚠ live` markers refreshed, and any of the
290 audited testgen skips whose feature has since landed (e.g. via P-C units)
is re-verified and un-skipped.

---

## 4. Track Q — SOLID / clean code (re-close the gates, keep them closed)

Binding method for **every** Q unit: a refactor is only complete when it
proves *behaviour identity* with the pre-refactor base — (a) `go test ./...`
green, (b) the JSON-harness failing-leaf set identical on the affected
families, (c) for storage/comparator/planner refactors: **byte-identical
on-disk databases** (`cmp -s` + sha256 over a churn workload — the R14 §2
protocol), (d) existing pin tests green. No `nolint`, no threshold changes, no
exclusions.

### Q1. staticcheck → 0 (hours)

Fix the 9 findings (2026-10-24): 6 unused functions (`frigolite.go:242
releaseSharedMem`, `execdml/wr_order.go:280 wrCellMatchesOldKey`,
`execquery/select_colnames.go:279 fullNamesForBareStar`,
`execquery/select_join_part2.go:237 rightDefBaseNames`,
`fts/exprtest.go:86 fts3ExprParseResult`, `tclconvert/tcl/interp_cmds.go:1242
decodeSQLiteExecHex`) — delete or wire in; 2 ST1005 error-string findings and
1 SA4006 (`fts/exprtest.go:583`) in the fts3_expr port. Then staticcheck joins
the pre-commit gate permanently (it already runs in `tools/quality_gate.sh`).

### Q2. Complexity → 0 over thresholds (gocognit ≤ 15, gocyclo ≤ 12)

132/133 findings. Order: biggest-first, one package per session, split along
responsibility seams (each split lands with its own behaviour-identity proof):

1. `execquery` — `queryEachLiveJoin` 70, `markJoinSourceColumns` 69,
   `validateJoins` 64, `stat4RangeSelectivity` 42, `applyJoin` 36 (+ the rest
   of the ~50 findings in the package — the largest concentration).
2. `fts5` — `decodeStructRec` 66, `loadFromShadow` 53 (+ remainder).
3. `fts` — the `fts3_expr.c` port trio (`fts3ExprBalance` 52, `fts3ExprParse`
   51, `fts3GetNextNode` 46): keep the C-mirroring structure but extract the
   tokenizer/parse-node helpers so each function carries one phase.
4. `execddl` — `renameColumnInTriggerSQL` 52 (+ alter_* remainder).
5. The long tail (≤ 35) in `function`, `exec`, `execdml`, `vtab`, tools.

### Q3. File sizes → 0 hard violations

Split the 11 files > 1,000 lines along the same seams (mostly extracting
cohesive passes/phases: `alter_rename.go` into walk/rewrite/apply phases;
`interp*.go` into per-command-family files matching the existing
`interp_string.go` pattern; `select_join.go`/`select_colnames.go` into the
join-plan vs projection responsibilities). Do this **after** P-C lands its
engine edits in those same files (avoid churn collisions), **before** the
Track F refactor wave (F must land on clean files).

### Q4. Soft-cap policy

222 files at 500–1,000 lines: freeze — no file crosses 1,000; any file touched
by a goal must not grow past its current size by more than the removed
elsewhere (net-zero within the file where practical). Re-assess at M7.

### Q5. Architecture upkeep

`frigolite_solid_test.go` layer map stays current: any new package gets a
layer assignment before merge; imports stay downward; exported-symbol counts
monitored. The per-task gate (PORTPLAN §5c) remains binding during P and F —
Q2/Q3 are the *repo-wide* closure of the debt those tasks were allowed to
leave, exactly per §5d.

---

## 5. Track F — performance (R15 program)

Ground rule: **never optimize a semantics that P is still changing.** Track F
units that touch paths owned by open P units (P-B planner, P-C9 CTE) are
sequenced after them. Everything else can proceed in parallel with Track P.

Measured targets are against the canonical paired harness
(`benchmarks/perfbench/{frigolite,sqlite}`, median of paired runs, same shell;
`-index -reps 3` and `-shape all -n 5000`). The sqlite column is the 3.54 CGo
reference build.

### F1. Land the R14 residue levers (validated attribution, no prototypes yet)

| # | lever | entry point | expected |
|---|---|---|---|
| F1a | cache the WHERE-ref analysis (`seekableRefs`/`collectBinaryRef` closure results) per statement shape | `execquery/select_index_seek*.go` | ~30 % of indexed-SELECT allocations |
| F1b | `buildRowMapFromValues` → slice-based column lookup | `execdml/` | 16 % of INSERT objects, 11 % of bytes |
| F1c | decode-free comparison on the read side (the R14 §4.2 comparator applied to `DecodeRecord`/`DecodeCell`/`decodeValue` consumers) | `storage/`, `btree/` | ~20 % of indexed-SELECT allocations |
| F1d | `findChildIndexForInsert` decode diet (10.2 % of INSERT CPU — same decode-boxing class) | `btree/` | INSERT lane |

Stage target: indexed SELECT ≥ 280k, INSERT ≥ 300k, UPDATE ≥ 150k, DELETE ≥
450k ops/s.

### F2. The statement-shape plan cache (the structural lever)

R14 §7.1: every remaining gap is *per-statement work that does not depend on
the data* — `CREATE INDEX` text re-parse (`execdml.IndexKeyInfo` →
`parseIndexColumns` → `splitIndexCols`, incl. the double parse in
`exec/index.go:992-1037`), `KeyInfo` reconstruction, WHERE-shape analysis,
`BTree` wrapper allocation per statement. SQLite does this once per
`sqlite3_prepare`; frigolite has the asset (`Stmt.Prepare` + AST template
cache) but the Exec path bypasses it. Build the **prepared-plan cache**: plan
once per (normalized statement shape, schema fingerprint), reuse across
executions, invalidate on schema change (the R14-4 memo already keys
KeyInfo per (schema, context, index) — generalize that seam). Route cold
`Exec`/`Query` through it transparently; keep `Stmt` semantics unchanged.
This is the lever that moves all indexed lanes the remaining distance.

Stage target: every indexed lane within 2×; cold-literal parsing excluded
from the claim (documented separately).

### F3. Aggregates and count paths

`SUM`/`GROUP BY` at 3× and the COUNT residue: profile the aggregate
accumulator loop; apply F1c's decode diet to the scan feed; verify R13-L4's
page-header `count(*)` covers all shapes (partial index, WHERE).

### F4. CTE / ephemeral streaming (shared with P-C9)

After the ephemeral-table queue lands, re-measure `sort3-3` and the
recursive-CTE lanes (heap peak and wall clock); the ephemeral btree must reuse
the pager temp store with spill, not trade the heap for a slower medium
without bound.

### F5. FTS paths

Re-validate the earlier findings (lessons: `readCellByRowID` → `SeekToRowID`
landed; the 65k-row FTS4 self-copy was 0.61 s vs sqlite 107 ms at that
measurement). Profile `fts3defer*`/`fts4merge*` and the fts5 insert path;
apply the F1c decode diet to the posting/segment writers.

### F6. speed-family wall clock

speed1p 445 s serial (~10–100× C). After F1/F2, profile the speed1/speed2
mixes (bulk insert, 100k-row scans, indexed lookups, ORDER BY). Target ≤ 4×;
re-anchor the P9.PERF sub-plan verify timeouts to the achieved numbers.

### F7. The perf gate (infrastructure)

Make the R14 §8 verification loop a repo tool: `tools/perfcheck` — paired
frigolite/sqlite runs over the canonical lanes, medians, a recorded baseline
file (`benchmarks/perfcheck_baseline.json`), fail on > 10 % regression beyond
noise. Respect R13-L3's flakiness lessons: floor the sample counts, pair the
runs, compare medians. Wire into the pre-commit hook as an advisory gate
(fast lanes only) and document the full gate command in §8.

---

## 6. Sequencing — milestones and dependencies

```
M0 (now) ──► M1 fidelity+register ──► M2 planner ──► M3 engine features ──► M4 green suite
   │                                                   │
   └──► M5 clean-code closure (starts after M3's engine churn, before F's refactor wave)
                        │
                        ▼
                       M6 performance program (F1 may start at M1; F2 after M2; F4 after P-C9)
                        │
                        ▼
                       M7 closeout (full matrix, ledgers, docs, push)
```

| milestone | content | exit signal |
|---|---|---|
| **M0** | baseline: fresh full-suite run + register regen; reconcile `TODOS.md` checkboxes; Q1 (staticcheck 0) | run stamp + register cited in TODOS.md; staticcheck 0 |
| **M1** | P-A (fidelity residue) + P-D2 start | harness leaves < ~800; A/D units checked |
| **M2** | P-B (B4 covering-index executor + B5 planner families) | planner families green; EQP golden set committed |
| **M3** | P-C1..C10 (engine features, one UCL goal each; P-C11 UTF-16 last) | each family green or natively substituted |
| **M4** | P-D (E1 reverted, E2 skipped fixtures, E3 tail) | **0 failing leaves**, PARITY_PLAN §1 items 1–3 met |
| **M5** | Q2+Q3 (complexity + file-size closure), then Q4/Q5 upkeep | gates at zero; race+SOLID green |
| **M6** | F1→F2→F3/F5/F6 (F4 after P-C9), F7 gate from M1 | §2 exit criteria 7–8 |
| **M7** | closeout: full verification matrix (§8), `tools/status` re-run + ledger re-seed, PORTPLAN/PARITY_PLAN markers refreshed with run stamps, all commits pushed | §2 all items |

Interleaving rules:
- One active unit per track max; engine edits and batch/suite runs never
  overlap (binding, PARITY_EXECUTION §4.5).
- P units that change executor semantics land **before** F units touching the
  same path (the F2 plan cache must be built on M2's planner, not under it).
- Every unit closes per PORTPLAN §8 checkpointing (sub-plan updated, commit,
  push); full-suite gates at goal close and after any corpus-wide regen
  (§5g item 4).

Effort shape (from the PARITY_EXECUTION §4b estimates, updated): M1 ≈ 1–2
sessions · M2 ≈ 2–3 · M3 ≈ 6–9 (P-C1 testvfs and P-C11 UTF-16 dominate) ·
M4 ≈ 5–8 (the tail is ~20 min/family) · M5 ≈ 4–6 · M6 ≈ 5–8. A "session" is
one continuous agent sitting.

---

## 7. Cross-cutting protocol (binding)

1. **All existing roots stay binding**: PARITY_PLAN §0 (no descoping, every
   failure owned, native-substitution-only obsolescence), GUIDELINES §1
   (no try/fail, no simplification, SQLite source first), PORTPLAN §5e/§5g
   (strict DoD, anti-drift, serial confirmation, run-stamp citations).
2. **UCL-first** for every P-C engine feature (oracle fixtures + localized
   unit tests before engine edits — PORTPLAN §5f).
3. **Behaviour-identity proofs** for every Q refactor and F optimization:
   `go test ./...` + identical harness failing-leaf sets on affected families
   + byte-identical DBs for storage/planner changes + pin tests. The R14 §2
   protocol is the template.
4. **Perf claims** are always paired/median, same shell, named harness,
   against the recorded sqlite reference; the perfbench `indexed-update` phase
   order artifact (R14 §3.3) means any indexed-UPDATE claim needs a
   fresh-table run.
5. **Suite discipline**: full suite detached and alone (~55 min, soft heap
   limit 3 GB, RSS ≈ 4.4 GB until P-C9 lands); never convert concurrently;
   a build failure in a batch is "no result".
6. **Lessons**: every session updates `.agents/lessons_learned.md`
   (consolidating, not appending).

---

## 8. Verification matrix (run at every milestone close, all at M7)

```bash
# build + vet + static checks
go build ./... && go vet ./...
staticcheck ./...
gocognit -over 15 <production files>   # 0 findings
gocyclo -over 12 <production files>    # 0 findings
tools/quality_gate.sh                  # file-size hard/soft gates

# architecture + race
go test -run TestSOLID_ ./...
go test -race -count=1 ./...

# behaviour: testgen census + JSON harness + native tests
go run ./tools/status && tools/status --check-against-cache   # testgen 0 FAIL
go test -p 1 ./... -count=1 -timeout 5400s 2>&1 | tee /tmp/frigolite-full-suite<N>.log
grep -a -o "FAIL: TestSQLiteSuite/[^ ]*" /tmp/frigolite-full-suite<N>.log \
  | sed 's/ (.*//' | sort -u | wc -l    # 0

# performance (paired, medians)
go run ./benchmarks/perfbench/frigolite -index -reps 3
go run ./benchmarks/perfbench/sqlite  -index -reps 3
tools/perfcheck                        # F7 gate once it exists
```

---

## 9. Risks

| risk | mitigation |
|---|---|
| The register is stale (checkboxes vs run 29) | M0 regenerates the register before any work |
| Restored/surfaced assertions grow the red set | expected truth-revealing; triage, never re-drop (stop rule §4.3) |
| P-C11 UTF-16 is a format-level change with corpus-wide blast radius | sequenced last; UCL fixtures first; enc-family regression set pinned before edits |
| Q2/Q3 refactors collide with P-C engine edits in the same files | Q closure scheduled after M3, before M6's refactor wave; per-file ownership at any moment is single-unit |
| F2 plan cache introduces plan-staleness bugs (schema change mid-cache) | invalidation keyed on schema fingerprint (the R14-4 seam); stale-plan pin tests; byte-identical DB proofs |
| Perf-clause flakiness in gates (R13-L3) | paired medians, floored samples, advisory pre-commit gate, full gate manual |
| Suite memory (4.4 GB RSS) on constrained machines | keep the 3 GB soft limit until P-C9; run alone |
| Corpus (3.54) vs source (3.51) divergences | corpus governs expected values; divergence recorded with oracle probes (§0 rule) |

---

## 10. Progress log

| date | item | evidence |
|---|---|---|
| 2026-10-24 | plan created; state verified (build clean; census 1,073/0/290 @ 2026-10-08; harness 1,072/79 @ run 29; Q debt 9/132/133/11; R14 lanes) | this document §1 |
| 2026-10-24 | **M0.Q1 complete: staticcheck → 0** — unused symbols deleted (`fullNamesForBareStar`, `rightDefBaseNames`, `wrCellMatchesOldKey`, `fts3ExprParseResult`, `decodeSQLiteExecHex`); the fts3_exprtest usage error kept byte-exact (`fts3expr.test:409` asserts it) via a constant passed through `fmt.Errorf("%s", …)`; SA4006 dead store dropped from `fts3ExprBalance`; **behavior fix**: shared `mode=memory&cache=shared` stores now released on last Close (was a process-lifetime leak contradicting SQLite's nRef-0 semantics) + native pin `TestSharedMemoryDBReleasedOnLastClose`. Verified: internal/fts, execdml, execquery, tclconvert package tests; testgen fts3expr+fts3expr2; JSON families fts3expr/attach/tkt3793/pager2 | commit `M0.Q1`; `staticcheck ./...` = 0 |
| 2026-10-24 | **M0 register reconciled**: run-29 per-family register (1,151 leaf names / 79 families); TODOS.md §1 table replaced, A9/A11 checked (conflict/conflict2 0, collate4 2); green-at-run-29 corrections: indexexpr2/func/func2/func4/insert4/json101/fts3expr4/where/where8/where9/orderby5/index9 — B5 residue = descidx2 19, bestindexF/A/E/5/9 35+10+9+14+7, expridx1 11 | `/tmp/run29-fails.txt`, TODOS.md §1 |
| 2026-10-24 | M0 full-suite baseline **run 30 launched** (detached, post-Q1 tree) | `/tmp/frigolite-full-suite30.log` |
| 2026-10-25 | **run 30 landed: 3,773 leaves / 314 families** — the consolidation commit `7f8441f73` installed the full-corpus fixture regeneration, so the register is now the post-regen truth (stop rule: triage, never re-drop). Newly-visible feature gaps: multiplex VFS 207, pager1 225, notify1 123, rtree2 103, WAL cluster (wal2 90, wal 69, e_walckpt 296), like3 33 (UTF-16), normalize 47. One flaky native test under load (`TestNativeThreadConcurrentWritersSerialize` — passes isolated) | `/tmp/run30-fails.txt`, `/tmp/run30-register.txt` |
| 2026-10-25 | **M1a landed** — `db status` has three fixture shapes (inline helper → counter suffix; standalone body → new `db_status` step, the old code corrupted the *previous* test's rendering; if-condition → sort marker for where.test's cksort variant) + converter pins. where 119→85, where8→35 (residue = ORDER BY not satisfied by index, the B4 engine unit) | commit `M1a`; `TestDBStatusStandaloneBodyEmitsStep` |
| 2026-10-25 | **M1b landed** — `runtime_var_reset` step (empty `set` on a callback-recorded global; procs mark their `::`-globals tracked at definition) + pin. e_droptrigger 54→**0**, e_expr 28→21 | commit `M1b`; `TestTrackedVarResetEmitsStep` |
| 2026-10-25 | **M1c landed** — fts3_exprtest arity-free (C's nArg=-1; the byte-exact `Usage:` error now comes from the function body — fts3expr-5.1) and phrase tokens render the tokenizer's folded term (a demoted NEAR keyword renders lowercased — fts3expr 3.13-3.15). fts3expr 5→**1**; testgen fts3expr/fts3expr2/e_fts3 green | commit `M1c` |
| 2026-10-25 | **M1d/e landed** — analyze5's b-tests assert the real runtime comparison (indexed vs NOT INDEXED rowids) instead of the inline `t.Skip`: analyze5 86→**0** honest; win32longpath added to the win32* §1 N/A entries (19). The remaining two inline skips (badutf2's `$sql` binding + `lindex` command substitution; bestindex5's xFilter log) are logged standing W5 items | commits `M1d/e` |
| 2026-10-25 | **M1 checkpoint — run 31: 3,678 leaves / 313 families** (−95 vs run 30, regression-free: no family grew; e_droptrigger + win32longpath left the register). M1 totals: e_droptrigger 54→0, analyze5 86→0 (honest, skip removed), where 119→86 (residue = B4), e_expr 28→22, fts3expr 5→2, win32longpath §1-skipped. The multiplex/pager1/notify1/rtree2/e_reindex/WAL clusters are unchanged and own the M2/M3 cards (`TODOS.md` M2 table) | `/tmp/run31-fails.txt`, `/tmp/frigolite-full-suite31.log` |
