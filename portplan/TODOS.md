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
| run 30 (2026-10-24) | 3,773 | 314 | `/tmp/frigolite-full-suite30.log` — the consolidation commit `7f8441f73` installed the full-corpus fixture regeneration, so the red set is the *post-regen truth* (the stop rule applies: never re-drop). Newly-visible: multiplex 207 (the multiplex VFS feature gap), pager1 225, notify1 123, where 119, rtree2 103, wal2 90, wal 69, pagerfault 59, indexexpr2 50, normalize 47, like3 33 (UTF-16), select1 32, snapshot 31, win32longpath 19 (§1 N/A, entry added). `TestNativeThreadConcurrentWritersSerialize` failed under load only (passes isolated — flaky, watch it). |
| run 31 (2026-10-25) | **3,678** | **313** | `/tmp/frigolite-full-suite31.log` — the M1 checkpoint: −95 leaves, regression-free (no family grew; e_droptrigger and win32longpath left the register). M1 verified per family: e_droptrigger 0, analyze5 0, e_expr 28→22, where 119→86, fts3expr 5→2. The big feature-gap families unchanged (multiplex 207, pager1 225, notify1 123, rtree2 103, e_reindex 97) — they are the M2/M3 cards below. `TestNativeThreadConcurrentWritersSerialize` did not fail in this run (run-30's failure was load-flakiness; watch it). |
| run 34 (2026-10-31) | **2,876** | **276** | `/tmp/frigolite-full-suite34.log` — the M2.W8/N/P/SEL1 checkpoint. Verified against a FRESH baseline suite run at the pre-session commit `e8faf18ae` in a worktree (2,920 leaves / 274 fams, same parser+convention — the run-33 record's 3,481 used a broader parse): net **−44 leaves**, improved where8 −17, select1 −17, wal −12 (the catch/msg/query2 shapes), resetdb −1; the 5 silently-aborting families (select2/select7/conflict2/ioerr4/select5) now report their real residue (11 leaves, previously undercounted at 0). Two regressions found and FIXED in-session (`8fa10c6db`): the catch msg rows re-run re-executed stateful PRAGMAs (incrvacuum_ioerr 4.1.2/4.2.2). atof1 +1 = the known randomblob(0) byte flake (passes on re-run). PERF.R14-5 landed: perfbench indexed-select 170-176k → 234-235k ops/s (+38%), leaf-sets byte-identical | `/tmp/run33-baseline-verify.log`, `/tmp/run34-fails.txt` |

**M1 progress (2026-10-25, post run 30)** — verified per family:

| unit | family | before | after | the fix |
|---|---|---|---|---|
| M0.Q1 | (quality) | staticcheck 9 | **0** | unused symbols deleted; fts3_exprtest usage text kept byte-exact via a constant; shared `mode=memory&cache=shared` stores released on last Close (leak fix + `TestSharedMemoryDBReleasedOnLastClose`) |
| M1a | where | 119 | **85** | `db status` had three shapes: inline helper (counter suffix), standalone body (new `db_status` step — the old code flagged the *previous* test's statement), if-condition (sort marker, where.test's cksort variant). Residue = ORDER BY not satisfied by index → the B4 engine unit |
| M1a | where8 | 36 | 35→B4 | same fixture regen; residue is the same engine class |
| M1b | e_droptrigger | 54 | **0** | a plain empty `set` on a callback-recorded global emits `runtime_var_reset` (`::triggers_fired` history accumulated across tests); proc bodies mark their `::`-globals tracked at definition |
| M1b | e_expr | 28 | **21** | same reset (`::matchargs`); the 16 result-mismatch corners remain |
| M1c | fts3expr | 5 (run 30) | **1** | fts3_exprtest is arity-free (nArg=-1 in C; the `Usage:` error comes from the body) and phrase tokens render the tokenizer's folded term (a demoted NEAR keyword renders lowercased); residue = one fixture-lifecycle artifact |
| M1d | analyze5 | 86 | **0** | the b-tests now assert the real runtime comparison (indexed vs NOT INDEXED rowids) instead of the inline `t.Skip` |
| M1e | win32longpath | 19 | skipped (§1) | win32* platform N/A documented in the skip map |

**Standing items discovered**: the three inline `t.Skip` guards from `7f8441f73` (badutf2 `$sql` binding + `lindex` command substitution in runtime exprs; bestindex5's xFilter callback log) still hide ~32 leaves behind skips — both need the converter model, logged as W5 work.

**M2 work cards (run-30 error classes, verified 2026-10-25)**:

| family | leaves | dominant class (count) | first action |
|---|---|---|---|
| e_walckpt | 296 | cross-connection "database is locked" on open (73) + downstream runtime vars (63) | P-C1 testvfs/shm-lock goal |
| pager1 | 225 | result mismatch (54) + "database disk image is malformed" (31) | probe the malformed class against the oracle first |
| multiplex | 207 | `no such function: multiplex_control` | P-C multiplex VFS goal (ext/misc/multiplex.c) |
| notify1 | 123 | shared cache + `sqlite3_unlock_notify` | P-C shared-cache/unlock-notify goal |
| rtree2 | 102 | all `rtree_i32`: runtime `expr {$t1 eq $t2}` = 0 (WHERE-constraint divergence vs mirror) | native probe replay: rtree_i32 vs mirror over the OPS mixes |
| e_reindex | 97 | integrity_check must report index/table mismatch (1.3, 26) · "no such table" lifecycle (2.x, 52) · FTS shadow `ta_segdir` (19) | read e_reindex.test 1.2-2.x setup; checkIndex vs our integrity_check |
| wal2/wal/walbak | 90/69/23 | WAL lifecycle ("no such table: t1", "table t1 already exists") | WAL fixture lifecycle model |
| pagerfault | 59 | fault-injection VFS (P-C1 family) | injectable VFS goal |
| indexexpr2 | 50 | result mismatch (41) | expression-index EQP/rows probe |
| normalize | 47 | `unknown prepared statement "stmt_N"` | TWO root causes verified: (1) converter — the catch-retag (interp_cmds.go ~401) renames the PREPARE step's handle to the catch variable (`set code [catch {set STMT [sqlite3_prepare_v3 ...]} res]` → prepare var becomes `code`) while `sqlite3_finalize $STMT` still references stmt_N; prepare steps must keep their canonical handle. (2) engine/harness — `sqlite3_normalized_sql` (the family's actual assertion) is unmodelled: port ext/misc/normalize.c (717 lines, self-contained tokenizer normalizer) as an engine/Stmt API + harness step, and model the body value `list $code $res` |
| where8 | 35→**18** (M2.W8 tranche 1, 2026-10-31) | was `[I IX scan 0 sort 0]` want `[I IX 0 0 6]` | LANDED (tranche 1): converter `execsql_status`/`execsql_status2` shapes (bare render + search-count element + reset via counter markers + `applyStatusStyle`), harness bare `N M[ S]` render, engine searchCount with vdbe-calibrated accounting (oracle EXPLAIN bytecodes): Seek* counted unconditionally, Rewind/Last/SeekRowid uncounted, full-scan Nexts = rows−1, OP_Sort −1 with NO recovering +1 (Rewind breaks, never reaches next_tail), SorterNext counts; OR-plan seeks/nexts/movetos modeled in or.go; ColumnValue unwrap in beyond[]; combined two-bound ranges. Residue (18) = ONE class: the executor full-scans where SQLite seeks — 1.8 `a>1 AND c LIKE` index range, 1.12.1 `a IN(...) OR a=5` IN-branches (per value: 1 seek + 1 moveto + 1 idx-Next, plus n−1 ephemeral Nexts), the 3.x join OR terms, correlated-subquery probes — plus the index-walk increments in select_index_seek* (queued behind the F1a cache track). Pin: TestDBStatusBareStyleAndSearchCount |

**M2/B4 tranche-1 result (2026-10-25)**: where 86→26 (rowid-led delivery + ORDER-BY-aware index choice + single-quote index-name dequote + 0/1-row delivery); validation battery byte-stable vs run 32; oracle-verified pin `frigolite_orderby_index_pin_test.go`. **Tranche 2a**: descidx2 18→0 (stale-ASSERTION skips keep side effects — the whole-case skip starved section 3's data); descidx1/3/4 green. where8 35 = the search-count unit above.
| like3 | 33 | "incomplete input" — UTF-16 DB encoding | P-C11 UTF-16 goal |
| select1 | 32 | named result mismatch (28) | fresh fixture vs query2 renderer |
| snapshot | 31 | "database disk image is malformed" (20) | WAL snapshot API paths |
| insert4 | 30 | runtime var `sqlite3_xferopt_count` got "" want 0/1 (30) | engine goal: the INSERT…SELECT transfer optimization + its test counter (insert.c xferOptimization; the count asserts when the fast path fires vs the row-by-row path CHECK constraints force) |
| fts3expr4 | 20 | "unknown tokenizer: icu" (11) + runtime "res" (7) | classify ICU (extension, N/A evidence?) + fix capture |
| func | 37 | `no such function: test_auxdata` (6) + error texts | P-C6 auxdata goal |
| memdb/func2/func4/stat/rowvalue | 18-19 each | result/query_assign mismatches | per-family probes |

**Where the leaves are now** (run 29 register — per-family unique failing
leaf names, regenerated 2026-10-24 from the run-29 log; the leaf-grep count
is 1,151, the summary's own count 1,072 — the register below is the work
list):

| family | leaves | the unit of work it belongs to |
|---|---|---|
| e_walckpt | 296 | C4 — the `testvfs` wrapper VFS |
| e_reindex | 97 | C7 — the reindex rules |
| analyze5 | 86 | D1 — the missing Tcl functions (`alpha`, `mit`, …) |
| fts3expr | 70 | D2c — the braced parse-tree comparison normalization |
| e_droptrigger | 54 | D1 — `list_all_triggers` residue |
| badutf2 | 42 | C — the UTF-8 handling |
| bestindexF | 35 | B5 — the planner (the virtual-table constraints) |
| e_blobopen | 31 | C — the incremental blob I/O harness surface |
| e_expr | 30 | A10 — the expression corner cases |
| fkey2 | 25 | C2 — the foreign-key errors |
| fts3defer2 / e_wal | 20 + 20 | D2 / C4 |
| descidx2 | 19 | B5 — the planner (descending indexes) |
| e_dropview / bestindex5 / attach3 | 15 + 14 + 14 | C / B5 / C5 |
| expridx1 / exclusive / e_walhook | 11 each | B5 / C4 / C4 |
| e_uri / e_select / blob / bestindexA / alterdropcol2 | 10 each | C5 / C / C / B5 / C1 |
| bestindexE / e_fts3 / e_blobwrite / analyze3 | 9 + 8 + 8 + 8 | B5 / C / C / C |
| errmsg / delete / columncount / bestindex9 | 7 each | C3 / C / C5 / B5 |
| eval / e_vacuum / e_changes | 6 + 6 + 5 | C / C / C |
| fts3defer / fkey4 / extension01 / expr / e_blobclose / collate4 | 4 + 4 + 4 + 4 + 4 + 2 | tail |
| the remaining ~35 families | ≤3 each (~70 leaves) | E3 — the tail |

Confirmed green at run 29 (previously listed red): `indexexpr2`, `func`,
`func2`, `func4`, `insert4`, `json101`, `fts3expr4`, `conflict`/`conflict2`,
`where`/`where8`/`where9`, `orderby5`, `index9` — the B5/C3 residue is now
`descidx2`, `bestindexF`/`bestindex5`/`bestindexA`/`bestindexE`/`bestindex9`,
`expridx1` and the planner's covering-index unit (B4) behind them.

**Where the leaves were** (run 27, per family):

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
- [x] **A9** `conflict` / `conflict2` — the runtime-list support landed and
      the stale fixtures were regenerated → both families **0 at run 29**
      (was 129 leaves)
- [ ] **A10** `e_expr` — a literal `$as$bs` must not be expanded as a
      variable → **35 leaves**
- [x] **A11** `collate4` — the "nosort"/"sort" marker is SQLite's test-build
      `sqlite_sort_count` (modelled as a runtime value, D1a) → **2 leaves at
      run 29** (was 52); the residue is tail triage

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
- [ ] **D2c** the `fts3expr` family (**70 leaves at run 29**) — the
      expectation is the braced parse tree ("AND {PHRASE 3 0 ab} {PHRASE 3 0
      cd}") and the harness compares the rendered rows with the *Tcl list*
      form of the expectation, which drops the braces: the comparison must
      normalize both sides the same way
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
