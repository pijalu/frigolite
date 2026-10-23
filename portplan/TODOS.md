# W-PARITY — the single work unit: a green `TestSQLiteSuite`

> **One unit of work. One goal. One checklist.**
>
> **Goal**: every `TestSQLiteSuite` leaf passes, and every native test passes —
> the SQLite 3.54 behaviour for every statement in `ori/sqlite/test`, with no
> skipped or weakened assertions.
>
> **Binding roots** (`portplan/PARITY_PLAN.md` §0):
> 1. all tests pass — "green except …" is not an end state;
> 2. a TCL test may be marked obsolete **only** when a named native Go test
>    covers the same functional aspect (a substitution, never a dismissal);
> 3. no descoping — the skip lists do not grow, expectations are never
>    weakened, fixtures are never trimmed to hide a defect;
> 4. every failure is owned;
> 5. evidence, not assertion — each closed item lands in the progress log.
>
> **Companions**: `portplan/PARITY_EXECUTION.md` (the per-item detail: the
> oracle's answers, the implementation points), `portplan/PARITY_PLAN.md`
> (the goals and the exit criteria), `handover.md` (the session state),
> `.agents/lessons_learned.md` (the durable findings).

---

## 1. The state

| measurement | leaves | families | log |
|---|---|---|---|
| run 18 (2026-10-20) | 14,431 | 305 | `/tmp/frigolite-full-suite18.log` |
| run 21 (2026-10-21) | 10,120 | 329 | `/tmp/frigolite-full-suite21.log` |
| **run 23 (2026-10-21)** | **6,720** | **327** | `/tmp/frigolite-full-suite23.log` |
| run 25 (2026-10-22) | 2,025 | 135 | `/tmp/frigolite-full-suite25.log` |
| run 26 (2026-10-22) | 1,894 | 129 | `/tmp/frigolite-full-suite26.log` |
| run 27 (2026-10-22) | 1,858 | 129 | `/tmp/frigolite-full-suite27.log` |
| **run 29 (2026-10-22)** | **1,072** | **79** | `/tmp/frigolite-full-suite29.log` |

**Where the leaves are now** (run 27, per family — the biggest first):

| family | leaves | the unit of work it belongs to |
|---|---|---|
| fts3expr3 | 329 | D2 — the FTS3 test helpers (`fts3_exprtest`) |
| e_walckpt | 295 | C4 — the `testvfs` wrapper VFS |
| fts3expr | 97 | D2 — the FTS3 test helpers |
| e_reindex | 96 | C7 — the reindex rules |
| analyze5 | 86 | D1 — the missing Tcl functions (`alpha`, `mit`, …) |
| e_droptrigger | 53 | D1 — `list_all_triggers` |
| indexexpr2 | 49 | B5 — the planner (the expression indexes) |
| func | 46 | C3 — the error texts |
| badutf2 | 41 | C — the UTF-8 handling |
| bestindexF | 34 | B5 — the planner (the virtual-table constraints) |
| e_blobopen | 30 | C — the incremental blob I/O |
| insert4 | 30 | C — the INSERT semantics |
| e_expr | 29 | A10 — the expression corner cases |
| fkey2 | 24 | C2 — the foreign-key errors |
| json101 | 24 | C — the JSON functions |
| fts3defer2 | 20 | D2 — the FTS3 deferred tokens |
| e_wal | 19 | C4 — the WAL |
| fts3expr4 | 19 | D2 — the FTS3 test helpers |
| index9 | 19 | B5 — the planner |
| descidx2 | 18 | B5 — the planner (the descending indexes) |
| func2 / func4 | 35 | C3 — the error texts |

The remaining ~1,400 leaves are in the ~105 families below 15 leaves each.

**Where the leaves were** (run 23, per family):

| family | leaves | the unit of work it belongs to |
|---|---|---|
| where7 | 1,589 | B1 — the OR-optimization (the counters are exact; the plan is not) |
| fts3expr3 / fts3expr | 0 / 69 | D2 — the FTS3 expression parser (the tree's Tcl list form) |
| pager1 | 543 | C4 — the WAL/pager fault-injection lifecycle |
| fts3expr3 + fts3expr | 428 | D2 — the FTS3 test-helper functions |
| e_walckpt | 297 | C4 — the `testvfs` wrapper VFS |
| multiplex | 207 | C4 — the multiplex VFS + `multiplex_control` |
| notify1 | 123 | C4 — shared cache + `unlock_notify` |
| where / where8 / where9 | 171 | B — the planner |
| wal2 | 114 | C4 — the WAL/ATTACH lifecycle |
| rtree2 | 102 | A — the conditional steps (the remainder) |
| e_reindex | 97 | C — the reindex rules |
| wal | 69 | C4 — the collation destructor count |
| conflict / conflict2 | 129 | A — the runtime expectation model |
| e_droptrigger | 54 | D1 — `list_all_triggers` |
| collate4 | 52 | D1 — the "nosort" marker (SQLite's test-build sort counter) |
| the rest (~250 families) | ~2,000 | E — the tail, one family at a time |

**The families already closed by this unit**: `altercons2` 2,065 → 0 ·
`pager2` 1,937 → 0 · `incrvacuum_ioerr` 3,999 → 0 · `rtree2` 1,513 → 102 ·
`where7` 1,969 → 1,589.

---

## 2. The checklist

The phases are ordered by **dependency**, not by size. Each item is closed
only with evidence (§4).

### A. The fidelity layer — the measurement must be honest before anything else

*Nothing below A can be trusted while an assertion is dropped or an
expectation is baked from a value that only exists at run time.*

- [x] **A1** the harness's `query` step arm restored (173,795 steps in 768
      families were silently dropped) + a `default:` arm that fails loudly on
      an unknown step type
- [x] **A2** the `string` command family complete + no silent fallbacks (an
      unknown `string` subcommand and a failing command substitution now fail
      the conversion)
- [x] **A3** the `do_test` body-value semantics (the expectation follows the
      body's value, not the last captured statement)
- [x] **A4** the runtime-conditional step model (a `catch` is a runtime
      result; an `if`/`else` on a runtime variable captures both branches with
      a `condition` guard; the branch's conversion-time side effects are
      rolled back)
- [x] **A5** SQLite's statement-status counters (`SQLITE_STMTSTATUS_FULLSCAN_
      STEP` / `_SORT`) — engine counters, the `db status step|sort` capture,
      the `stmtStatus` fixture flag, the native pin
- [x] **A6** the runtime expectations (a `catch` publishes its error message;
      an expectation that is itself a runtime expression is evaluated at run
      time)
- [x] **A7** the runtime lists (a body's value that is a Tcl `list` over
      runtime values)
- [x] **A8** the helper's `lsort` — `count_steps_sort` sorts the result list,
      so the expected value is the **sorted** rows; the harness renders the OR
      plan's branch order. The converter marks a query's runtime value and
      records the sort (`resultTransform: lsort:<mode>`), and the harness
      sorts the rows it rendered (Tcl's `lsort` modes: ascii/integer/real/
      dictionary, with -decreasing/-unique/-index) → where7 **1,589 → 0**
- [ ] **A9** `conflict` / `conflict2` — the body's value is
      `list $r0 $r1 $r2 $r3` over runtime values; the runtime list step
      exists, but the do_test's *expectation* (`[list $t0 $t1 $t2 $t3]`) is
      lost inside the `foreach {i cmd t0 t1 t2 t3}` body → **129 leaves**
- [ ] **A10** `e_expr` — a literal `$as$bs` must not be expanded as a
      variable → **35 leaves**
- [ ] **A11** `collate4` — the "nosort"/"sort" marker is SQLite's test-build
      `sqlite_sort_count` (the same event as the SORT counter); model it as a
      runtime value → **52 leaves**

### B. The planner — the counters are exact, the plan is not

*where7 proved it: with the counters correct, **1,933 of 2,018 leaves
differed only in the counters** — the rows were right, the plan was not.*

- [x] **B1** the OR-optimization: equality, range, rowid-range and
      GLOB/LIKE-prefix branches, and the OR-term cap as the expression-depth
      limit (1000, not 32) → where7 1,963 → 1,589
- [x] **B2** the where7 leaves where the OR plan was still not applicable:
      the branch kinds SQLite serves and we did not — "col IS NULL" (an
      equality on NULL), a range whose constant is on the left ("1000000<b")
      and the GLOB prefix (the prefix stops at the first GLOB wildcard
      '*'/'?'/'[', not at the LIKE wildcards) → where7 61 → 0
- [x] **B3** the planner and the executor must agree about the plan — the
      rowid-alias ORDER BY rule keys off the planner's loop, but the executor
      may have fallen back to a table scan (where7-1.4): the rule now also
      accepts a WHERE with a top-level OR (the OR plan decides the order and
      marks its result `RowsInBranchOrder`), and an ORDER BY *ordinal*
      ("ORDER BY 1") is resolved against the result's columns → where7 1 → 0
- [ ] **B4** the covering-index preference — SQLite prefers a covering index
      over a table scan (`cost = nRow*K` vs `nRow*3.0`); the plan text is
      already rendered, but **no executor for a covering-index walk** exists
      (the `groupDistinct` flag reaches only the plan text) and the index
      *choice* must follow SQLite's record-size cost model (the row order
      changes with the index)
- [ ] **B5** the remaining planner families: `where` (120), `where8` (35),
      `where9` (16), `indexexpr2` (49), `orderby5` (14)

### C. The engine features

- [ ] **C1** ALTER TABLE — DROP/RENAME COLUMN, STRICT (alterdropcol2,
      altercol, alterlegacy, altertab, strict1) ≈ 40 leaves
- [ ] **C2** the errors that must be raised (41 families ≈ 312 leaves:
      `vtab1`, `quota`, `rtreeA`, `fkey2`, `savepoint`, `with2`, `incrblob`, …)
- [ ] **C3** the error *texts* (32 families ≈ 130 leaves: `func`,
      `rtreedoc2`, `e_wal`, `vtab3`, `without_rowid3`, `check`, `upsert1`, …)
- [ ] **C4** WAL / locking / shared cache / VFS ≈ 850 leaves:
      `e_walckpt` 297 (the `testvfs` wrapper) · `multiplex` 207 (the multiplex
      VFS + `multiplex_control`) · `notify1` 123 (shared cache +
      `unlock_notify`) · `wal2` 114 (the WAL/ATTACH lifecycle) · `wal` 69 (the
      collation destructor count) · `wal6`/`walbak`/`walvfs` 52 (the named-DB
      lifecycle) · `pager1` 543 (the `faultsim_delete_and_reopen` reset)
- [ ] **C5** the C-API result codes ≈ 45 leaves (`schema` 27, `columncount` 6,
      `close` 3, `bind2` 2, `misc4` 4)
- [ ] **C6** the recursive CTE must not materialize every row — `sort3-3` runs a
      2.2 M-row CTE with 1 KB blobs (~1.4 GB live). SQLite streams it through
      an ephemeral temp table; the fix is the same shape (the materialization
      belongs in the pager's temp store, not in a Go slice) → the corpus's
      memory ceiling
- [ ] **C7** `e_reindex` (97) — the reindex rules

### D. The harness model — the testfixture's dialect

- [x] **D1a** the Tcl-backed collation: a collation registered with `db
      collation <name> <proc>` is implemented by a Tcl proc, and the harness
      evaluates it with the converter's Tcl interpreter → collate4 51 → 1,
      collate2/collate6/altercol/collate5 → 0, collate9 7 → 3, collate1 6 → 4
- [x] **A11** the testfixture's sort marker — collate4.test's cksort helper
      appends "sort" or "nosort" depending on whether the statement sorted
      (the test build's sort counter); the converter records the marker on
      the statement and the harness appends it to the rendered rows
- [ ] **D1** the missing Tcl procs (`mit`, `swap_int32`, `rnode`,
      `force_rollback`, `myrandomblob`, `cksum`, `offrec`, `joinx cross`,
      `nondeter`, `external`, `sql_error`, `selectH_counter`, `::runsql`), the
      missing queries (`list_all_views`), the missing pragmas
      (`lock_proxy_file`), the missing collations (`text_collate`,
      `backwards_collate`, `nocase_collate`, `string compare`)
- [x] **D2a** the FTS3 expression parser — `internal/fts/exprtest.go` is a
      port of `ext/fts3/fts3_expr.c` (the node structure, the
      recursive-descent parser, the operator precedence, the rebalancing
      algorithm and the rendering), and the engine registers
      `fts3_exprtest` / `fts3_exprtest_rebalance` → **fts3expr 97 → 0**,
      **fts3expr2 → 0**
- [x] **D2b** the `fts3expr3` family (329 leaves) — the expectation is the
      *stripped* tree (`strip_phrase_data` drops the phrase data): a
      procedure that receives a runtime query value now records itself on the
      statement, and the harness applies it to the rendered rows
- [ ] **D2c** the `fts3expr` family (69 leaves) — the expectation is the
      braced parse tree ("AND {PHRASE 3 0 ab} {PHRASE 3 0 cd}") and the
      harness compares the rendered rows with the *Tcl list* form of the
      expectation, which drops the braces: the comparison must normalize
      both sides the same way
- [ ] **D3** the named-DB lifecycle ("named database db2 is not open" ≈ 40
      leaves) and the `vacuum-into` / `errmsg` / `trace2` classes

### E. The corpus

- [ ] **E1** the batch-reverted families — a stale fixture **hides** a defect;
      each reverted family must end up installed-fresh with its failures
      fixed (batch #6 revealed 6,451 leaves this way)
- [ ] **E2** the 102 fixtures the converter still skips (the statement
      budget) — raise it per file
- [ ] **E3** the long tail (≈250 families with ≤30 leaves), one family at a
      time, with the seven-step procedure (§4)

### F. Cross-cutting (every item)

- [ ] **F1** the batch procedure: verify → install → revert on regression;
      read the regression list against the *old* fixture's leaf names before
      reverting (the naming convention changes produce false regressions)
- [ ] **F2** the suite runs detached (a new session) so an interrupted tool
      call cannot kill it; never convert concurrently with the suite
- [ ] **F3** the memory policy: the soft heap limit
      (`debug.SetMemoryLimit`, 3 GB), `debug.SetGCPercent(50)` and
      `debug.FreeOSMemory()` per family — on macOS the runtime releases freed
      pages with `madvise(MADV_FREE)`, so the heap limit is the only lever
- [ ] **F4** this tracker, `PARITY_EXECUTION.md`'s progress log and
      `handover.md` stay current

---

## 3. The immediate queue — the next actions, in order

1. **A8** the helper's `lsort` → 1,534 leaves (where7) — a fixture-model
   change, not an engine one
2. **A9** the `foreach`-body expectation → 129 leaves (conflict, conflict2)
3. **A10** the `$as$bs` expansion → 29 leaves (e_expr)
4. **B2** + **B3** the last 61 where7 leaves (the OR plan's remaining
   branches; the planner/executor agreement)
5. **B4** the covering-index walk → the largest engine lever left
6. **C6** the recursive CTE → the corpus's memory ceiling
7. **C4** the WAL/locking cluster → ≈850 leaves
8. **E1** the reverted families → the hidden defects
9. **E3** the tail, one family at a time

---

## 4. The procedure — every item, the same seven steps

```
1. NAME     pick the family from the register (PARITY_EXECUTION.md §3) and
            write the item down (symptom, expected behaviour, the SQLite
            source pointer)
2. PROBE    write a pure-Go test that drives the engine directly
            (frigolite_<name>_test.go) and reproduces the behaviour
            - probe fails  -> the ENGINE is wrong  -> 3
            - probe passes -> the FIXTURE/CONVERTER is wrong -> 5
3. ENGINE   implement the rule in the responsible internal/ package,
            following SQLite's own source (../sqlite/src) — no simplification
4. PIN      the probe stays as a regression test; it must fail without the fix
5. CONVERT  regenerate the family's fixture
            (go run ./tools/tclconvert/ -testdir <dir> -outdir <dir>)
6. BATCH    install through the batch driver (verify -> install -> revert on
            regression)
7. CLOSE    the family is green (or green with a named native replacement and
            a compliant obsolete record); record the evidence in the progress log
```

**The rules that make it work**

- the oracle is SQLite, not the fixture: when the engine and the fixture
  disagree, run the same SQL through `sqlite3` and the same Tcl through
  `tclsh` before changing anything;
- no try/fail loop: if the second attempt does not work, stop and write a
  dedicated fix plan (GUIDELINES §1);
- never edit the engine while a batch or a suite run is in flight;
- a build failure is "no result", never "0 failures";
- a red set that grows after restoring an assertion is the truth appearing,
  not a regression — triage it, never re-drop the assertion.

---

## 5. Progress log

| date | item | evidence |
|---|---|---|
| 2026-10-20 | the plan of record (`PARITY_PLAN.md` §0 roots, §4b register) | `/tmp/run16-fails.txt` |
| 2026-10-20 | the run-18 baseline: 14,431 leaves / 305 families | `/tmp/frigolite-full-suite18.log` |
| 2026-10-20 | **F1 found**: the harness had no `case "query":` arm → 173,795 steps in 768 families silently dropped | `git show HEAD:frigolite_harness_test.go` vs the tree |
| 2026-10-20 | **F2 found**: the converter's `string` fallback returned its first argument → round1's 9,356 expectations were wrong | `tools/tclconvert/tcl/interp.go` |
| 2026-10-20 | **F3 found**: the `do_test` expectation was attached to the last captured statement | `tools/tclconvert/tcl/interp_sql.go` |
| 2026-10-21 | **N1**: the statement-status counters — the engine counters, the `db status` capture, the `stmtStatus` flag, the native pin | `frigolite_stmtstatus_test.go` |
| 2026-10-21 | the runtime-conditional model reaches pager2: the baked conditions, the nested conditions, the runtime assignments + `mode=memory` URIs → **pager2 1,937 → 0** | `tools/tclconvert/tcl/conditional_step_test.go` |
| 2026-10-21 | the Tcl authorizer (`db auth <proc>`) → **altercons2 2,065 → 0** | `frigolite_tcl_authorizer_test.go` |
| 2026-10-21 | the runtime expectations (the catch's message, an expression expectation) → **incrvacuum_ioerr 3,999 → 0** | `tools/tclconvert/tcl/dotest_body_value_test.go` |
| 2026-10-21 | the runtime list (a body's value that is a Tcl `list`) | `tools/tclconvert/tcl/dotest_body_value_test.go` |
| 2026-10-21 | run 23: **6,720 leaves / 327 families** (−3,400 from run 21) | `/tmp/frigolite-full-suite23.log` |
| 2026-10-21 | **B1**: the OR plan serves range, rowid-range and GLOB-prefix branches; the OR-term cap is the expression-depth limit → where7 1,963 → 1,589 | `internal/execdml/or.go` |
| 2026-10-22 | **A8**: the Tcl `lsort` over a query's rows is recorded in the fixture (`resultTransform`) and applied by the harness → where7 1,589 → 423 | `tools/tclconvert/tcl/runtime_value.go`, `frigolite_harness_transform_test.go` |
| 2026-10-22 | **B2**: the GLOB prefix (the LIKE prefix function stopped at the LIKE wildcards only), "col IS NULL", and a range with the constant on the left → where7 423 → 41 | `internal/execquery/explain.go`, `internal/execdml/or.go` |
| 2026-10-22 | **B3**: the rowid-alias ORDER BY with an ordinal, and the OR exception in the rowid-order rule → **where7 0** (the family is green) | `internal/execquery/select_order_search.go` |
| 2026-10-22 | **A9**: the conflict/conflict2 fixtures were stale (the runtime-list support had landed) → **128 leaves** | `testdata/conflict.json`, `testdata/conflict2.json` |
| 2026-10-22 | the 184 stale fixtures regenerated and installed → pager1 542, multiplex 206, notify1 122, where 120, wal2 113, rtree2 102, wal 68, memdb 41 … → 0 | `testdata/*.json` |
| 2026-10-22 | the runtime-value model: a query's value is marked, the marker never reaches a fixture, and a runtime expectation is compared at run time (`expectedExpr`) | `tools/tclconvert/tcl/runtime_value.go`, `frigolite_harness_test.go` |
| 2026-10-22 | **run 25**: 2,025 leaves / 135 families (run 23: 6,720 / 327) | `/tmp/frigolite-full-suite25.log` |
| 2026-10-22 | the Tcl-backed collation and the sort marker (D1a, A11) → collate4 51 → 1, collate2/collate6/altercol/collate5 → 0, descidx1 26 → 0, avtrans 46 → 0 | `frigolite_tcl_collation_test.go` |
| 2026-10-22 | **run 27**: 1,858 leaves / 129 families | `/tmp/frigolite-full-suite27.log` |
| 2026-10-22 | **D2a**: the FTS3 query expression parser (`fts3_exprtest`) → fts3expr 97 → 0, fts3expr2 → 0 | `internal/fts/exprtest.go` |
| 2026-10-22 | the general Tcl-backed SQL function (`db function <name> <proc>`) → func 46 → 37, analyze5 87 → 85 | `frigolite_tcl_function_test.go` |
| 2026-10-22 | **D2b**: the FTS3 expression parser became linear (the tokens are scanned once per expression, not per token) and the fixture carries the test's `strip_phrase_data` → **fts3expr3 329 → 0** | `internal/fts/exprtest.go` |
| 2026-10-22 | **run 29**: 1,072 leaves / 79 families | `/tmp/frigolite-full-suite29.log` |
| 2026-10-21 | **B2** (the unary `+` rule), **rule 1** (the OR plan sorts with the comparator), **rule 2** (the rowid-alias ORDER BY is free) | `internal/execdml/or.go`, `internal/execquery/select_order_search.go` |
