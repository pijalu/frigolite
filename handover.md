# Full-suite repair handover

## Objective
Continue repairing broad test failures while preserving existing workspace edits. Do not skip or weaken tests. Finish only after `git diff --check` and `go test ./...` pass, or document exact unresolved failures and evidence.

## ⭐ RESUME HERE (2026-10-19, session 3 — "e_expr parity push")
The `e_expr` family went from **16,657 failing leaves → 29** (installed fixture: `testdata/e_expr.json`,
regenerated from `ori/sqlite/test/e_expr.test`). The wins came from converter fidelity plus a handful of
engine fixes — nothing was skipped or weakened. The converter changed a lot since the last full-corpus
regeneration, so **a full re-conversion and delta-verification is still required** (see "Next actions").

### Session-3 outcome (e_expr: 16,657 → 74)
| step | failures |
|---|---|
| start (run10 fixture) | 16,657 |
| + `db one`/`execsql` runtime placeholders, baked deferred exprs, `string match`, `SQLITE_MAX_VARIABLE_NUMBER` | 297 |
| + atomic expr substitution (values are operands, not source) | 139 |
| + `too many SQL variables` / prepare-error parity | 134 |
| + harness: no aborted prepare/finalize (the "database is locked" cascade) | 102 |
| + dynamic-flag reset on plain `set`, `emptytext` accepts NULL | 79 |
| + `int` vs `int64` boxing in the `% -1` fast path | 74 |
| + operator overrides (a user `like`/`glob`/`regexp` replaces the built-in) | 53 |
| + `var`/`x`/`ceval` Tcl procs in the harness + the registration scope | 33 |
| + `COLLATE reverse` comparator in the harness | 29 |

Remaining 29 (all genuine engine/harness gaps, no skips): 16 result mismatches, 10 runtime-variable
mismatches (the harness's recorded argument lists are never reset between tests — the TCL source resets
them with `set ::matchargs [list]`, which the converter does not capture as a step), 1 `expected error but
got success` (e_expr-11.7.x: the converter inlines the *value* of a named SQL parameter, so the parameter
count — and with it the "too many SQL variables" error — is lost), 1 C-API column-type capture
(`sqlite3_column_type` is not modelled), 1 emptytext/NULL case.


### Converter changes landed in this session (`tools/tclconvert/tcl/`)
| Change | File | Why (e_expr symptom) |
|---|---|---|
| `exprAtom` now renders every substituted value as an *atomic operand*: numeric → bare literal (int stays int), everything else → quoted string with `\`/`"` escapes | `interp.go` | Tcl substitutes values, it does not re-read them as expr source. The e_expr literals list keeps its quotes (`'abc'`), so the old bare insertion made the expression unparsable and the `if` took the wrong branch. |
| expr string literals now process backslash escapes | `expr.go` (`parseStringLit`) | needed by the quoting above |
| `string match` (Tcl glob: `*`, `?`, `[class]`, `\` escapes, **no** class negation), `string is` (integer/double/boolean/alpha/alnum/digit/space/list/…), `string replace` (Tcl clamping rules) | `interp_string.go` | `string match` was **missing** (83 corpus uses): e_expr-12.3.x never substituted `EXPR`/`EXPR2`, so 75 leaves ran `SELECT + EXPR FROM tblname` |
| `SQLITE_MAX_VARIABLE_NUMBER=32766` added to the interpreter's globals | `interp.go` (`NewInterp`) | e_expr-11.1.x/11.3.x: `[expr $SQLITE_MAX_VARIABLE_NUMBER+2]` evaluated to `+2` (unset var) → `?2` instead of `?32768`, and the expected error text was wrong |
| `set X [execsql/db one/db eval …]` → `query_assign` keeps a **`$name` placeholder** (the value only exists at runtime) | `interp_cmds.go` (`commandSet`) | e_expr-1.cat.cat.*: the expectation was the empty capture-time value (`want [NULL]`) instead of the runtime result |
| `set X [execsql …]` retag now also accepts a captured **`query`** step (not only `exec`) and marks `ResultAll` (Tcl `execsql` yields the whole row as a list; `db one` yields one value) | `interp_cmds.go` | e_expr-8.2.* (218 leaves): `set eq [execsql "SELECT …"]` was not converted, so the expectation was empty |
| Deferred `runtime_expr` bodies now **bake conversion-time-known variables** into the text (numeric → bare, else quoted), leaving runtime vars as `$name` | `interp_sql.go` (`bakeKnownExprVars`, `exprVarNameAt`, `exprDeferAtom`) | e_expr-7.*: 4,056 leaves failed with `runtime variable "op" is not set` |
| `do_test` bodies ending in `set <name>` emit a `runtime_var` step for bare names the harness owns (query_assign target, or a global written by a registered callback) | `interp_sql.go` + `interp_collation.go` (`trackCallbackVars`) | e_expr-15.1.2/17.3.2/18.2.2/19.2.4 were **absent** from the fixture (silently dropped) |
| `faultsim_delete_and_reopen` / `db_delete_and_reopen` → `reset_db` marker | `interp_cmds.go` | pager1/pager2/wal* leak the database between sections (the TCL helper closes, deletes `test.db*` and reopens) |
| TCL `strftime` command, `expr` integer typing (`+ - * / % & | ^ << >> ~`, `abs`, `min`, `max`), `Tcl_PrintDouble` float formatting | `strftime.go`, `expr.go` | earlier in this session: date4 24,859 → 0; atof1 19,999 → 0 |

New pins: `tools/tclconvert/tcl/string_match_test.go` (tclsh-verified `string match`/`is`/`replace` +
`SQLITE_MAX_VARIABLE_NUMBER`).

### Engine changes landed in this session
- `internal/execexpr/arith.go` — `bitwiseAnd` now coerces like `sqlite3VdbeIntValue` (text/blob parsed as
  integers, reals truncated, NULL propagates) instead of failing with "cannot bitwise-AND non-integer
  values". Oracle: `SELECT '2245' & 66` → 64, `SELECT 2.5 & 3` → 2, `X'414243' & 3` → 0 (vdbe.c OP_BitAnd).
- `internal/execexpr/fastpath.go` — the `x % -1` wrap case returned an untyped `0`, which boxes as Go
  `int`; every downstream `int64` assertion then failed (`SELECT 0 - (1 % -1)` → "cannot subtract
  non-numeric values"). Now `int64(0)`.
- `internal/exec/params.go` + `internal/exec/engine_core.go` — the second SQLite parameter error:
  "too many SQL variables" when the *assigned* variable index passes SQLITE_LIMIT_VARIABLE_NUMBER
  (expr.c `sqlite3ExprAssignVarNumber`). A failed prepare now returns **no** statements, so the Query
  path reports the error too (it silently executed the statement before).
- `internal/execexpr/arith.go` (earlier) — `shiftOp`: negative shift counts no longer overflow
  `MinInt64` (SQLite flips the direction, Tcl errors).
- `internal/execexpr/expression_rowvalue.go` + `expression_eval.go` — **operator overrides**: SQLite
  compiles `X LIKE Y` into the function call `like(Y,X)` and resolves the name through the function table,
  so an application function named like/glob/regexp REPLACES the built-in operator (expr.c
  `sqlite3ExprCodeTarget`, TK_LIKE_KW). The engine now calls the user function (pattern first) and
  applies SQL NOT to the negated forms. Pinned by `TestSQLiteOperatorOverrideUsesUserFunction` /
  `...WithoutUserFunction`.
- `internal/execexpr/fastpath.go` — the `x % -1` wrap case returned an untyped `0` (Go `int`); every
  downstream `int64` assertion failed. Now `int64(0)`.
- `internal/exec/params.go` + `internal/exec/engine_core.go` — the second SQLite parameter error
  ("too many SQL variables"); a failed prepare now returns **no** statements, so the Query path reports it.
- `internal/execquery/select_agg_validate_part2.go` — CURRENT_TIME/CURRENT_DATE/CURRENT_TIMESTAMP stay
  keywords when the statement has a FROM clause (they are not column names).
- `internal/sql/lexer.go` — a ':'/'@' parameter name may contain '$' (SQLite's IdChar): ":_$_" and
  "@_$_" are valid names.
- Pinned by `frigolite_expr_parity_pin_test.go` (keyword columns, parameter names, parameter-limit
  errors, bitwise coercion) and `frigolite_operator_override_pin_test.go`.

### Harness changes landed in this session (`frigolite_harness_test.go`)
- `registerTCLIdentityFunction`: new cases for the e_expr Tcl procs —
  `expr {$a==$b}` (matchfunc/regexfunc → the `MATCH`/`REGEXP` operators) and
  `lappend ::X $args; return 1` (likefunc/globfunc/regexpfunc/glob overrides, which also record their
  arguments into a runtime list). Helpers `tclFuncEquality`, `tclFuncArgString`, `tclFuncArgNumber`.
- `query_assign` honours the new `resultAll` flag (whole row as a Tcl list).
- `expandTCLRuntimeVars` splits a `$name` reference at an inner `$` when the full name is not a runtime
  variable, so two adjacent placeholders (`$as$bs` from `[list "${as}${bs}"]`) both expand.
- `prepare_stmt`/`stmt_step`/`stmt_reset`/`stmt_finalize`/`runtime_assert` failures no longer abort the
  rest of the test case (`t.Errorf` + `continue`): aborting skipped the matching `stmt_finalize`, whose
  never-released prepared-read lock then blocked every later write with "database is locked".
- `emptytext` accepts a NULL result when the connection's null value is the empty string (Tcl renders
  NULL as the null value; `{{}}` is one empty element).
- `register_function` steps now carry the Tcl scope (`callbackVars`); the harness seeds the runtime
  variables from it, so a function that reads a global (e_expr's `var` reads `::$nm`) sees it.
- New Tcl proc support: `var` (reads a global, records the name in `::varlist`), `x` (counts calls,
  returns `::x`), `ceval` (counts calls, returns its argument).
- The `COLLATE reverse` comparator (a helper proc that reverses both operands) is now modelled.

### e_expr status: 74 remaining failures (all pre-existing engine/harness gaps, none are skips)
| count | symptom | assessment |
|---|---|---|
| 22 | result mismatch | needs case-by-case triage (see `/tmp/eexpr-new9.log`) |
| 13 | runtime variable mismatch | harness runtime-var bookkeeping |
| 12 | `error parsing regexp: invalid UTF-8` | engine regexp over blobs |
| 10 + 1 | `no such function: var` | Tcl proc `var` (`lappend ::varlist $nm; return [set "::$nm"]`) not modelled |
| 6 + 1 | `no such function: x` | Tcl proc `x` (e_expr-12.x literal-value table) not modelled |
| 2 + 1 | `ceval` | Tcl proc `ceval` (`incr ::evalcount; return $x`) not modelled |
| 2 | `unknown prepared statement "stmt_N"` | prepared-statement bookkeeping |
| 1 | `prepare: near "$_": syntax error` | parameter-name edge case |
| 1 | `expected error but got success` | parameter-number error case |

### Full-corpus regeneration + verify/install batches (session 3)
- Full re-conversion with the final converter: `/tmp/regen8` (1,118 files, 77 skipped, 24 conversion
  errors, EXIT=0 — the same pathological files as before, whose installed fixtures stay).
- Batch #1 (`/tmp/regen6` → the installed tree, 245 changed families): **195 kept, 49 reverted, 1 new
  family**, **1,032 failing leaves fixed**, 544 newly surfaced. Highlights: `date5` 341→0,
  `skipscan5` 29→0, `like3` 16→0, `rtreeA` 20 fixed, `literal` 9 fixed, `walfault` 6, `where` 5,
  `walmode` 5, `unhex` 5. Verdicts: `/tmp/regen6-batch.jsonl`, log `/tmp/regen6-batch.log`,
  reverted list `/tmp/regen6-reverted.txt`, backups `/tmp/regen6-backup`.
- Batch #2 (`/tmp/regen8` → the installed tree, 141 changed families): **92 kept, 49 reverted**,
  **789 failing leaves fixed**, 508 newly surfaced. Highlights: `orderby8` **199 → 0** (the
  `db eval {SQL} { lappend res $x }` callback shape now captures the first column of every row),
  `e_droptrigger` **66 → 0**, `indexexpr2` 52 → 50, `selectH` 6 → 2, `quote` 4 fixed.
  Verdicts: `/tmp/regen8-batch.jsonl`, log `/tmp/regen8-batch.log`, backups `/tmp/regen8-backup`.
- The batch rule (locale-safe Python set logic): install the fresh fixture, re-run, and **revert** when a
  leaf that exists in the installed fixture and passed before starts failing. `old_fails` = failures with
  the installed fixture, `fixed` = leaves that stopped failing, `new` = *newly* failing leaves.
- The 49 reverts in batch #1 are families where the fresh fixture surfaced *new* leaves that the stale
  fixture did not have (e.g. `pager1` 458 fixed but 43 regressions, `json501` 62 new, `e_reindex` 94 new).
  Each needs its own triage: usually a converter expectation that is still computed at conversion time.

### Full-suite run 11 (2026-10-19 17:04)
- `go test -p 1 ./... -count=1` → `/tmp/frigolite-full-suite11.log` (8.8 MB), the root package FAILs.
  **55 min wall / 33 min CPU, peak RSS ≈ 7.4 GB** (the GC reclaims between families: the RSS oscillates
  between ~2 GB and ~7.4 GB — no unbounded growth).
- **8,395 failing leaves** in `/tmp/run11-fails.txt` (≈8,390 after the three re-verification reverts
  below: `skipscan5` 36→32, `snapshot2` 5→3, `temptable2` 4→5). The count is *not* comparable with run8 (1,712):
  the fixtures are now faithful, so most families contain far more tests than the stale ones.
  Top families: `where7` 2,001 (SQLite-style stmt-status counters — feature gap), `pager2` 1,971,
  `pager1` 776, `trans2` 234, `multiplex` 209, `wal3` 160, `wal2` 126, `notify1` 124 (shared-cache +
  unlock-notify — feature gap), `wal` 113, `e_walckpt` 103, `sharedB` 102, `rtree1` 85,
  `e_droptrigger` 83, `indexexpr2` 51, `func` 46, `badutf2` 42.
- **The batch tail ran while the engine was mid-edit and hit a build failure**: 13 families were
  installed without a real verification run. They were re-verified against the pre-batch backups
  (`/tmp/regen6-backup`): `skipscan5` (6 regressions → reverted), `snapshot2` (1 → reverted),
  `temptable2` (2 → reverted); the other ten had no regressions (`snapshot_up` even improved 9 → 3).
  **Never edit the engine while a batch is running, and check the batch logs for "build failed".**

### Family spot checks after the batches (final state)
`e_expr` 27 (was 16,657) · `orderby8` **0** (was 199) · `date5` **0** (was 341) · `e_droptrigger` 82
(unchanged — a *harness* gap: the Tcl procs `r`/`list_all_triggers` are not modelled) ·
`skipscan5` 32 (the skip-scan EQP detail renders `b=?` instead of `b>? AND b<?` — an engine gap).

### Session-3, part 2 (after run 11)
- **Converter**: implemented Tcl's `eval` (the arguments are concatenated and evaluated as a script) and
  the testfixture's `execsql_intout`. Without `eval`, trigger2's `eval $test_varset` never ran, so the
  generated `CREATE TRIGGER` statements had no event and the shadow tables survived → 30 leaves in
  trigger2 (now 0) and the same pattern in without_rowid4 and ~45 other files.
- **Engine**: the rtree module now refuses to create a virtual table when a shadow-table name is taken,
  with SQLite's message `table "t1_rowid" already exists` (rtree.c xCreate).
- **Harness**: a `close_db` step on an already-closed database is a no-op, not an error.
- Family effects measured in isolation: `trigger2` 30 → 0 (fresh fixture), `rtree1` 85 → 71.
- The remaining big families are **feature gaps**, not bugs: `where7` (2,001 — SQLite's
  `SQLITE_STMTSTATUS_FULLSCAN_STEP`/`SORT` counters, needed by the `count_steps_sort` helper),
  `notify1` (124 — shared cache + unlock-notify), `multiplex` (209 — the multiplex VFS and its
  `multiplex_control` helper), `trans2` (234 — the Tcl-side MD5 of the scrambled data), `pager2` (1,971 —
  a runtime-dependent TCL `for` loop the converter cannot unroll).

### Batch #3 (the `eval` / `execsql_intout` / rtree-shadow fixes) — `/tmp/regen9`
- Full re-conversion `/tmp/regen9` (1,118 files ✓) and a verify-install batch over the 74 changed
  families: **24 kept, 50 reverted**, **432 leaves fixed**, 510 newly surfaced.
  Verdicts `/tmp/regen9-batch.jsonl`, log `/tmp/regen9-batch.log`, backups `/tmp/regen9-backup`.
- The 50 reverts are the same families as before (the fresh fixture surfaces leaves the stale one did
  not have); they need their own triage, they are not regressions of the engine.
- Family gains measured after batch #3: `rtree1` 63 → 26, `without_rowid4` 32 → **0**,
  `e_walckpt` 5 → **0**, `sort2` 3 → **0**, `fts3defer` 8 → 3, `trigger2` 30 → **0** (installed
  manually from `/tmp/one-out12`), `e_droptrigger` 82 → 61, `indexexpr2` 51 → 49, `e_expr` 16,657 → 27,
  `orderby8` 199 → 0, `date5` 341 → 0.

### Execution status (2026-10-20, night) — latest
- **Harness**: `list_all_triggers` (every schema, prefixed) → e_droptrigger 62 → 53.
- **Harness**: the `query_assign` step now compares its expectation (against the rendered rows) — it used
  to drop the assertion entirely, which had made where7's 2,001 leaves vacuous.
- A full-suite run with the query_assign check in place is at `/tmp/frigolite-full-suite17.log`.

### Execution status (2026-10-20, night) — run 16 baseline
- **Run 16**: **2,804 failing leaves / 286 families** (`/tmp/run16-fails.txt`) — down from run 11's
  8,395. The bulk of the drop is `pager2` (1,971 → 1, the `runtime_loop` step) and `where7`
  (2,001 → 20, the `query_assign` expectation check that had silently dropped those assertions).
- **Honesty note**: `where7`'s remaining 20 leaves need SQLite's `SQLITE_STMTSTATUS_FULLSCAN_STEP`/
  `_SORT` counters (W4.1) — the fixture expects "scan N sort M" after the result.
- Top families now: `e_walckpt` 267 · `pager1` 213 · `btree01` 212 · `notify1` 123 · `multiplex` 109 ·
  `wal2` 100 · `temptable2` 76 · `e_droptrigger` 62 · `rtree1` 55 · `badutf2` 42.

### Execution status (2026-10-20, night) — W1.2 landed, batch #6 done
- Batch #6 (`/tmp/regen12`, 51 changed families): **10 kept / 41 reverted**, 143 leaves fixed,
  `delete` +8. The 41 reverts are the standing W5 triage set — each needs one family-sized
  investigation (pure-Go test → root cause → fix → re-batch), not a batch-level decision.
- The final full-suite run (16) is in flight: `/tmp/frigolite-full-suite16.log`.

### Execution status (2026-10-20, night) — W1.2 landed
- **W1.2 done**: a Tcl `for` loop whose bound comes from a query result is now captured as a
  `runtime_loop` fixture step (the loop variable, the start/condition expressions, the body). The harness
  evaluates the bounds with the runtime variables, binds the loop variable per iteration and runs the
  body as ordinary statements. **pager2 1,971 → 1** (the last leaf needs the "database table is locked"
  semantics — W2). New converter fields `Stmt.Loop*` / `TestStep.Loop*`; the harness's `runtime_loop`
  step type.
- A full re-conversion with this converter is running (`/tmp/regen12`); batch it with
  `/tmp/regen3-batch.py` afterwards — the `runtime_loop` step is new, so every family whose fixture
  changes must be re-verified.

### Execution status (2026-10-20, evening) — run 15 baseline
- **Run 15**: **7,644 failing leaves / 304 families** (`/tmp/run15-fails.txt`) — 751 below run 11's
  8,395. The WAL families gained the most in this pass (wal3 −149, wal −34) from the fresh fixtures
  batch #5 kept; pager1 −27, rtreedoc −4, journal2 −3.
- Remaining top families: `where7` 2,001 (stmt-status counters, W4.1) · `pager2` 1,971 (the runtime
  TCL loop, W1.2) · `pager1` 543 · `e_walckpt` 285 (the `testvfs` VFS, W4.10) · `multiplex` 207 (W4.3) ·
  `notify1` 123 (W4.2) · `wal2` 114 · `sharedB` 102 · `e_droptrigger` 62 · `indexexpr2` 50.

### Execution status (2026-10-20) — later items
- **Engine**: the skip-scan EQP recorded every comparison as "=" (a range rendered `b=?`); the real
  operator is now recorded and two bounds on one column render `b>? AND b<?` → `skipscan5` 36 → 30.
- **Harness**: `a_string`/`blob`/`contained_in`/`int2hex` procs → `pagerfault` 30 → 29.
- No regressions in `where7` (2,000), `indexexpr2` (49), `rtreeA` (9), `where8` (35).

### Execution status (2026-10-20) — run 14 baseline
- **Run 14** (after the panic fix): **7,878 failing leaves / 309 families** (`/tmp/run14-fails.txt`),
  517 fewer than run 11's 8,395. `e_walckpt` +182 is the panic fix (the family now *reports* instead of
  aborting the suite). Run 15 is in flight (`/tmp/frigolite-full-suite15.log`).
- **Harness procs added** (the "unsupported TCL function proc" cluster): `a_string` (55 leaves),
  `blob` (10), `contained_in` (2), `int2hex` (2) — `pagerfault` 30 → 29. The rest of that cluster
  (`mit`, `swap_int32`, `rnode`, `sql_error`) needs the binary-scan / fault-injection machinery
  (W4.8) or a dedicated implementation.
- **Next execution order** (from `portplan/PARITY_PLAN.md`): W1.2 (the runtime-loop fixture step →
  `pager2`, 1,971 leaves) → W5 (triage the 49 reverted families) → W2 (per-family engine gaps) →
  W3 (harness model) → W4 (feature gaps) → W6 (long tail).

### Execution status (2026-10-20, after the plan was written) — continued
- **Engine**: SQLite's JSON parser keeps a raw control character inside a string and the serializer
  escapes it (`{"label":"abc\u0001xyz"}`); the engine dropped it → **json501 62 → 0** (batch #5 kept
  the family, `/tmp/regen11-batch.jsonl`).
- Batch #5 (re-verification of the 58 families with the JSON fix): 9 kept / 49 reverted, 356 leaves
  fixed. The reverts are the same families as before (their fresh fixtures surface leaves the stale
  ones never had) — they need the W5 triage pass, one family at a time.
- The batch's regression rule compares *normalized* leaf names, so a family whose OLD fixture used a
  different naming convention reports false regressions: read the regression list against the old
  fixture's actual names before reverting.

### Execution status (2026-10-20, after the plan was written)
- **W1.5 done**: the converter's `lsort` was a stub; `lsort` (with -ascii/-integer/-real/-dictionary/
  -index/-decreasing), `md5` and `random_uuid` are implemented and pinned against tclsh
  (`tools/tclconvert/tcl/interp_sort_test.go`) → **trans2 234 → 0**.
- **Batch #4** (`/tmp/regen10`, 58 changed families): 24 kept / 34 reverted, `whereC` 30 → 10,
  `temptable2` 5 → 1. Verdicts `/tmp/regen10-batch.jsonl`, backups `/tmp/regen10-backup`.
- **Harness robustness**: a failed database open no longer leaves a nil handle (run 12 aborted with a
  panic in `e_walckpt/setup_4`); every open site now falls back to an empty database.
- A fresh full-suite baseline is running in the background: `/tmp/frigolite-full-suite14.log`
  (`/tmp/frigolite-full-suite14.exit`); the plan's inventory table still cites run 11 (8,395 leaves).

### Current focus: Phase A of `portplan/PARITY_EXECUTION.md` (2026-10-20)

The detailed plan is `portplan/PARITY_EXECUTION.md` (W0-W8, the register, the
procedure). Three blocking findings gate everything else:

| id | finding | impact |
|---|---|---|
| **F1** | the harness's step switch lost its `case "query":` arm (uncommitted rename `query` -> `query2`) | 173,795 steps across 768 families silently dropped (not executed, not asserted); `btree01`'s `t1` never created |
| **F2** | `cmdString` answers an unknown `string` subcommand with its first argument | `string trimright` returns the input -> `round1`'s 9,356 expectations are wrong |
| **F3** | `cmdDoTest` attaches the `do_test` expectation to the last captured statement | the rows are compared with a scalar -> `rtree2` 2,010, `where`/`conflict`/`collate4`/... ~1,500 |

Order: **W0** (guards: a `default:` arm in the harness switch, no silent
fallback in the converter) -> **W1** (restore `case "query"`) -> **W2**
(`string trimleft`/`trimright` + the rest) -> **W3** (the `do_test` body
value) -> **W4** (one statement per step) -> **W5** (regenerate + batch the
whole corpus) -> then Phase B (W6 engine parity) and Phase C (W8's 129-family
tail).

Note: after W1 the red set is expected to *grow* (the restored assertions now
run). That is the truth being revealed — it must not be "fixed" by dropping
the step again.

**Baseline after W0-W3 (run 21, 2026-10-21 08:00)**: **10,120 leaves / 329
families** (`/tmp/frigolite-full-suite21.log`, `/tmp/run21-fails.txt`) — 4,281
leaves fewer than run 18. The restored assertions moved ~4,000 leaves from
*vacuous* to *red* (where7 20 → 2,021 — the stmt-status counters; pager2 1 →
1,938 — the runtime loop's inner statements; pager1 212 → 543) and three
families are new coverage (fts3expr, fts3expr3, e_reindex = 525 leaves).
That is the truth being revealed, not a regression.

**Memory**: the corpus-wide RSS is now bounded (a 3 GB soft heap limit +
`debug.SetGCPercent(50)` + `FreeOSMemory` per family: the sort families' peak
fell 2.77 → 1.52 GB). On macOS `FreeOSMemory` cannot shrink the RSS (the
runtime uses `madvise(MADV_FREE)`), so the heap limit is the only lever until
**W6.9** (the recursive CTE materializes 2.2 M rows of 1 KB blobs in
`sort3-3` → ~1.4 GB live) is fixed.

**Landed (2026-10-20 evening)**

| item | result |
|---|---|
| W0 (guards) | the harness's step switch has a `default:` arm (an unknown step type now fails loudly); the audit proved `query` was the only unhandled type — 173,795 steps in 768 families |
| W1 (`case "query"`) | restored: `btree01` 211 -> **0**, `temptable2` 75 -> **3**, `date4`/`json106`/`corruptI` now really assert 24,859/40,000/50,004 statements |
| W2 (`string` family) | `trimleft`/`trimright`/`last`/`cat`/`reverse`/`wordstart`/`wordend` implemented + oracle pins; the unknown-subcommand fallback now raises `UnsupportedCommandError` (fatal for the file); a failing command substitution aborts the conversion |
| batch #6 (`/tmp/regen20`, 60 families) | 14 kept · 44 reverted · 2 new-family; **round1 9,356 -> 0** |

**W3 landed (2026-10-20 evening)**: the do_test expectation now follows the
*body's value* (`set rc` -> a runtime_var step), `set X [expr {...}]` becomes a
runtime expression that stores its value in X, and the variable holds every row
(the new `resultRows` fixture field — Tcl's execsql/db eval yield a list of
rows). `rtree2` 2,010 -> **1,513** with the assertion now real; the pin test
is `tools/tclconvert/tcl/dotest_body_value_test.go`.

**W3.5 — the remaining rtree2 gap (C1)**: the fixture model has no
*conditional* step. The TCL `set rc [catch {db eval "INSERT ..."}]` followed by
`if {$rc} {...} else {...}` runs the second insert only when the first
succeeded; the converter evaluates `catch` at conversion time (where the
capture cannot fail) and always takes the `else` branch -> the shadow table
gets rows the rtree does not -> the comparison is false. The fix is a
`condition` field on the fixture step (the harness evaluates it with
`evalHarnessTclExpr`) plus a `variable` on the `catch` step. It also fixes the
same class in `pagerfault` and `temptable2`.

**Batches**: #6 (`/tmp/regen20`, 60 families): 14 kept / 44 reverted / 2 new —
**round1 9,356 -> 0**. #7 (`/tmp/regen21`, 57 families, fixed harness): 12 kept
/ 45 reverted / 459 leaves fixed in the kept families.

**Next**: the single work unit `portplan/TODOS.md` (**W-PARITY** — one goal: a
green `TestSQLiteSuite`) holds the whole remaining checklist, grouped into
the fidelity layer (A), the planner (B), the engine features (C), the harness
model (D), the corpus (E) and the cross-cutting items (F), with the ordered
queue in §3 and the seven-step procedure in §4.

**The state (run 27, 2026-10-22)**: 1,858 failing leaves in 129 families
(run 23: 6,720 in 327). where7, pager1, multiplex, notify1, where, wal2,
rtree2, wal, conflict, conflict2, memdb, rtree1, vtab1, quota, schema,
pragma, stat, snapshot, pagerfault, collate2, collate6, altercol, descidx1,
avtrans … are green.

**The queue**: D2 the FTS3 test helpers (445 leaves) · C4 the `testvfs`/
WAL cluster (295 + ~150) · C7 the reindex rules (96) · D1 the missing Tcl
functions and queries (analyze5 86, e_droptrigger 53) · B5 the planner
(indexexpr2 49, bestindexF 34, index9 19, descidx2 18) · C3 the error texts
(func 46, func2, func4) · A10 e_expr (29) · then the ~105 families below 15
leaves each.

**The procedure**: the seven steps in TODOS.md §4 — name, probe (a pure-Go
test), engine, pin, convert, batch (verify → install → revert on
regression), close. The oracle is SQLite 3.54 (`sqlite3` CLI, `tclsh`), not
the fixture.

**The complete work set is tracked in `portplan/TODOS.md`** (N1–N7 + the
planner-parity items P.1–P.4 + the cross-cutting items). Landed so far:
**N1** (SQLite's statement-status counters — engine counters, the
`db status step|sort` capture, the `stmtStatus` fixture flag, the native pin
`frigolite_stmtstatus_test.go`).

**The key finding of N1**: with the counters correct, **1,933 of where7's
2,018 leaves differ only in the counters** — the rows are right, the *plan* is
not. where7 is SQLite's OR-optimization test, so the counters were the
*diagnostic*, not the defect. New planner items P.1–P.4 are in the TODOS.

**pager2 (1,937) re-diagnosed**: its expectations are *correct* — the failure
is the engine's, because the converter drops the TCL's
`if {[info exists sp_$x]} {execsql "ROLLBACK TO sp_$x"}` branch (the
condition is only knowable at run time), so the rows the rollback would have
removed are still there ("got 100 ok, want 0 ok"). N2.1 therefore folds into
N2.3 (the conditional step) — the biggest remaining lever (≈4,000 leaves:
pager2 1,937 + rtree2 1,513 + the F3 cluster ≈525).

**Memory (2026-10-21)**: the suite's RSS was observed at >20 GB. Root cause
found and documented: `sort3-3` runs a **2.2M-row recursive CTE with 1 KB
blobs**, and `internal/execquery/select_cte_exec.go:iterateRecursiveCTE`
materializes every row in the heap (1.4 GB live, `GODEBUG=gctrace=1`
evidence: `1955->1975->1410 MB` then `2752->2753->95 MB`). SQLite streams the
recursive result through an *ephemeral temp table*; the faithful fix is the
work item **W6.9** in `portplan/PARITY_EXECUTION.md`.

Interim mitigation **landed** in the harness: `debug.SetMemoryLimit` (default
6 GB, `FRIGOLITE_MEM_LIMIT` in MiB, `0` disables it) + `debug.FreeOSMemory()`
after every family, so a family's freed heap returns to the OS immediately
instead of accumulating across the corpus. The sort families' peak dropped
from 2.77 GB to ~1.96 GB; the cumulative growth across the corpus is gone.

**Caution**: run the full suite *alone* — run 19 was killed (`signal: killed`)
because a converter run executed concurrently pushed the machine over its
memory limit. The suite is launched detached so that an interrupted tool call
cannot kill it:

```bash
python3 -c "import os,subprocess; os.setsid(); subprocess.Popen(['sh','-c','cd /Users/muaddib/dev/frigolite && (go test -p 1 ./... -count=1 -timeout 5400s > /tmp/frigolite-full-suite21.log 2>&1; echo EXIT=\$? > /tmp/frigolite-full-suite21.exit)'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL, start_new_session=True)"
```

### Binding roots for the remaining work (2026-10-20)
1. **All tests pass** — the end state is a green suite, not "green except …".
2. **A TCL test may be marked obsolete only when a native test covers the same functional aspect**;
   the obsolete record must name that native test (a substitution, never a dismissal).
3. **No descoping of functional coverage** — the skip lists do not grow, expectations are never
   weakened, fixtures are never trimmed to hide a defect, and no failure is reclassified as
   "not our problem" (GUIDELINES §1d).
4. **Every failure is owned** — each failing leaf is classified (C1–C5) and attached to a work item whose
   exit criterion is a green test (or a native replacement + a compliant obsolete record).
5. **Evidence, not assertion** — every item closes with a run log in the plan's progress log.

### Master plan: `portplan/PARITY_PLAN.md` (added 2026-10-20)
The complete parity plan now lives in **`portplan/PARITY_PLAN.md`**: the exit criteria
(0 failing leaves, unchanged skip lists, every family fixture regenerated and batch-verified),
the run-11 inventory (8,395 leaves / 315 families), the four root-cause classes
(converter / engine / harness protocol / feature gap), six ordered work streams
(W1 converter completeness, W2 engine semantics, W3 harness model, W4 feature gaps,
W5 fixture hygiene for the reverted families, W6 the long tail), the binding execution
procedure (triage → plan → convert → batch-verify → pin → close) and the risks.
Run 12 (in flight) re-measures the baseline after batches #2/#3 and the engine/harness fixes.

### Next actions (ordered)
1. The full suite has been run (run 11, see above). The next lever is the biggest remaining families:
   `where7` (2,001 leaves — the SQLite stmt-status counters FULLSCAN_STEP/SORT), `pager2`/`pager1`
   (the `faultsim_delete_and_reopen` reset changes the fixture's grouping; the fresh fixtures were
   reverted because of regressions), `trans2`/`multiplex` (transaction lifecycle), the WAL families
   (`wal`/`wal2`/`wal3`/`e_walckpt`/`sharedB` — P7.WAL work) and `notify1` (shared cache +
   unlock-notify — a feature gap, not a bug).
2. The converter does **not** create its `-outdir`: a missing directory silently produces
   "write error: … no such file or directory" for every file while the process keeps running.
2. Triage the 74 remaining e_expr leaves (table above): the `var`/`x`/`ceval` Tcl procs (harness function
   support), the regexp-over-invalid-UTF-8 errors, and the 22 result mismatches.
3. Implement the SQLite operator-override semantics: a user-defined `like()`/`glob()`/`regexp()`/`match()`
   function **replaces** the built-in operator (SQLite rewrites `X LIKE Y` into `like(Y,X)`; expr.c
   `sqlite3ExprCodeTarget`/`TK_LIKE_KW`). The engine currently intercepts LIKE/GLOB/REGEXP by name
   (`scalarSpecialDispatch` in `internal/execexpr/expression_eval.go`) and `probeOperatorOverload`
   (`expression_rowvalue.go`) only *probes* the registry and discards the result. Needed for e_expr
   15.1.x/17.3.x/18.2.x and for `func`/`like` families.
4. Then re-run the full suite (run11) with `go test -p 1 ./...` and a unique log, and compare with
   `/tmp/run8-fails.txt` (1,712 leaves) — remember run10 aborted early at pager1 (panic, since fixed).
5. Keep `handover.md` and `.agents/lessons_learned.md` current after every change.

## Current verified state (2026-10-19)
- All work below is in the workspace, uncommitted.
- Isolated families verified green with `FRIGOLITE_TEST=<f> go test . -run '^TestSQLiteSuite$' -count=1`:
  `e_update`, `e_delete`, `e_insert`, `trigger1`, `trigger7`, `triggerupfrom` (incl. 2.4),
  `temptrigger` (65 cases, incl. section 9 multi-`:memory:` attaches), `attach`;
  converter rounds 2/3: `rowvalue2` (4030 cases), `distinct`, `stmt`, `ptrchng`, `bind`, `fpconv1`.
- Regression comparison vs the run7 failure set (`comm -13 /tmp/run7-fails.txt <family-now>` empty) for
  `trigger*`, `view*`, `fkey*`, `alter*` — no previously-passing case broke.
- `go test ./tools/tclconvert/... -count=1` green; `git diff --check` clean; `go test . -run 'TestSOLID_' -count=1` green.
- Native pins added: `TestSQLiteMalformedTriggerOnAttachPin`, `TestSQLiteTempTriggerQualifiedNamePin`,
  `TestFixtureAttachPathKeepsMemoryDatabasesInMemory` (+ earlier temp-trigger qualified-DML pins in
  `frigolite_trigger_ddl_pin_test.go`).
- Fixture regeneration workflow: `mkdir -p /tmp/<dir> && go run ./tools/tclconvert/ -testdir ori/sqlite/test -outdir /tmp/<dir> <name>.test`,
  inspect the diff against the installed JSON, then install. Corpus authority is `ori/sqlite/test`
  (3.54-equivalent), NOT the pinned `../sqlite` (3.51.0) — see `portplan/SESSION_COMPARE_DATE_PLAN.md` for the
  oracle evidence on temp-trigger qualified DML targets.

## Changes landed this session
### View NATURAL/USING join fixes (run8 triage)
- `internal/exec/pragma_table_views.go` — `viewStarSourceDefs`: schema-based view column
  defs now apply the USING/NATURAL star merge (drop the right operand's first copy of each
  merged column, mirroring `select_join.go` `filterUsingColumns`/`naturalJoinCommonCols`).
  Before: `CREATE VIEW v1 AS SELECT * FROM t1 NATURAL JOIN t1` reported columns `x` and
  `x:1` (oracle: `x`), and natural joins of such views matched on the phantom NULL copy
  → empty results (`join5-10.1`).
- `internal/execquery/select_subq_unused.go` + `select_cte.go` — `markJoinSourceColumns`
  now recognizes a named-view source (`sourceNames` = view name) in addition to derived
  subqueries, so the omit-unused-column optimization cannot NULL a column a NATURAL/USING
  join condition reads (`SELECT count(*) FROM v1 NATURAL JOIN v1` returned 0; oracle 2).
- Plan + oracle evidence: `portplan/VIEW_NATURAL_JOIN_PLAN.md`.
- `join5` fixture regenerated from `ori/sqlite/test` (old fixture was missing tests
  1.1–2.12): family 18 → 0 failures.
### Fixture regeneration batch (DONE)
- Run8 failure signature "table tN already exists" (177 messages, 144 fixture families)
  is stale-fixture fallout: the installed fixtures lack the TCL lifecycle resets
  (`db close; forcedelete test.db; sqlite3 db test.db`, `reset_db`) that separate
  sections, so later sections re-CREATE tables in leaked state.
- Fresh conversions for all 144 families generated into `/tmp/regen-all` (142 ok;
  `tkt2686`, `types` conversion errors — old fixtures kept). Batch installer/verifier
  finished: `/tmp/regen-batch.log`, backups `/tmp/regen-backup`, per-family logs
  `/tmp/regen-out-<fam>.log`. Re-verified in Python (locale-safe): **0 families with
  regressions** (no test present in an old fixture fails after install).
- Net effect over the 144 families: 654 previously-failing leaves fixed, 3,105 leaves
  surfaced by the (much) more faithful fixtures. Biggest surfaced families were
  converter artifacts, not engine gaps — see the second converter round below; e.g.
  `rowvalue2` went from 222 surfaced failures to 0 once the converter emitted the
  real TCL semantics. Genuine surfaced clusters remain (WAL checkpointing,
  transaction lifecycle, stmt-status counters in `where7`/`where8`).
### Converter round 2 (`tools/tclconvert/tcl/`) — TCL fidelity fixes
After installing the faithful fixtures, three TCL constructs the converter silently
mis-evaluated were found and fixed (each oracle-checked against tclsh where relevant):
- `subst` option abbreviations (`subst -novar` = `-novariables`, also `-nocommand`):
  `matchSubstOption` accepts unique prefixes; previously the option error silently
  emptied the substituted text (rowvalue2/distinct).
- Bracketed SQL words (`do_execsql_test NAME [subst -novariables {...}] ...`) now run
  through `bindSQLParams` like braced SQL, so `$var`/`${var}` references the TCL
  interface would bind at runtime are inlined as SQL literals.
- `string range ... end` / `string index ... end`: new `parseTclIndex` handles
  `end`, `end±N`, `N±M`, and negative indices (previously `Atoi("end")` failed →
  empty slice, corrupting generated INSERT data in rowvalue2).
- `switch` body fallthrough (`== - IS { ... }`): a body of `-` defers to the next
  pattern's body (`make_expr2` in rowvalue2 emitted empty control queries before).
- Multi-pair `foreach` (`foreach c $cList v $vList { ... }`): new `foreachPair` /
  `parseForeachPairs` implement TCL semantics — iterate over the longest list,
  fill shorter lists with empty strings (previously only the first pair bound).
- `lrange`/`lindex`/`lreplace` now use `parseTclIndex` too: `lrange $l 0 end-1`,
  `lindex $l end` were returning element 0 (`Atoi("end")` → 0), which made
  `make_expr3` build wrong prefix expressions.
- Result: `rowvalue2` regenerated → **0 failing leaves** (was 222 before round 2,
  2,430 at the intermediate stage); `distinct`, `stmt` re-verified green.
- Full-corpus re-conversion (959 files) launched into `/tmp/regen-full` to find any
  other fixture affected by the round-2 semantics (`/tmp/regen-full.log`): 913 JSONs
  produced, 30 skipped (no tests captured), 16 conversion errors; **732 of 913 fixtures
  differ** from the installed ones. Batch installer/verifier (#1) was shell/`comm`-based
  and unreliable (locale sort warnings → a few missed regressions); it was replaced by
  `/tmp/rfb2-driver.py` (pure-Python set logic, per-family `-timeout 150s`, one retry on
  timeout, revert-on-regression, verdicts in `/tmp/rfb2-log.jsonl`). Final state:
  - batch #1 processed 134 families (backups `/tmp/regen-full-backup`, logs
    `/tmp/rfb-out-<fam>.log`); **15 reverted** (`/tmp/regen-full-reverted.txt`:
    aggnested, alter2, altercons2, alterlegacy, analyze3, analyze5, analyze9, attach2,
    autoinc, autoindex1, bestindex1, bestindexF, bind2, btreefault, collate1) — all
    confirmed by an independent Python re-check.
  - `badutf` handled manually: no failures with the fresh fixture (the "hang" did not
    reproduce; it was machine memory pressure, see the size-budget fix below), kept.
  - driver #2 finished the remaining **589** families (`/tmp/rfb2.log`,
    `/tmp/rfb2-log.jsonl`): **550 kept, 39 reverted, 0 timeouts**, +300 failing leaves
    fixed, +1,108 surfaced. Reverted by driver #2: delete, e_fkey, e_reindex, e_walhook,
    filter2, func, func3, func4, func7, index6, insert4, join, json102, json501, like,
    main, memsubsys1, quote, reindex, rowid, schema2, select1, select2, select4,
    snapshot2, snapshot4, snapshot_up, sort3, subquery, temptable2, trigger6, vacuum2,
    view, view3, vtab7, vtab_alter, vtab_shared, wal9, wherelimit2.
  - The regen-full round predates converter round 4 (format verbs, escaped parameters)
    and the fixture size budget; those fixes are re-verified by the `/tmp/regen-full3`
    pass below.
  - `ptrchng`, `bind`, `fpconv1` are excluded — they were regenerated after the round-3
    marker change (below) and are already green.
### Converter round 3 (`tools/tclconvert/`) — testfixture functions
- `split string ?splitChars?` implemented (`commandSplit`, Tcl semantics verified against
  tclsh: any separator char, empties preserved, empty separator = characters). Missing it
  made `lindex [split $sql =] 0` (pragma4) yield an empty prepare SQL.
- `sqlite3_prepare_v3` registered as an alias of `commandPrepareStmt` (fts4content,
  normalize, without_rowid7 used it; the prepare steps were previously dropped, leaving
  `stmt_finalize` with an unknown handle).
- `sqlite3_create_function DB` (test1.c `test_create_function`) was an unknown command
  and silently ignored, so `ptrchng.test` (29 failing leaves) had no `pointer_change`
  registration. New `commandSQLiteCreateFunction` emits a `register_test_funcs` marker
  (carrying the connection name); `groupToTestCase` now keeps that marker step type
  (unknown empty-SQL step types are dropped by its whitelist — see lessons).
- Harness `registerTest1Functions` (`frigolite_harness_testfuncs_test.go`) installs the
  Go equivalents: `pointer_change` (ptrChngFunction, test1.c:914) and `x_coalesce`
  (t1_ifnullFunc, test1.c:731, returns the first non-NULL argument *as text*). The
  pointer model mirrors Mem.z identity: text16/text re-encode (pointer changes),
  blob returns the current buffer unchanged (ptrchng-2.5/5.3 expect 0).
- Result: `ptrchng` 29 → 0, `bind` green, `fpconv1` green (all three fixtures also
  regenerated to include the marker and to drop the stale `ifcapable utf16` cases).
### Converter round 4 (`tools/tclconvert/`) — format verbs & escaped parameters
- `tclFormat` rewritten: C/Tcl parse the argument according to the conversion —
  floating point for `a/A/e/E/f/F/g/G` (Go previously rendered `format %.32e -Inf`
  as `%!e(string=-Inf)` in atof1), `%g`/`%G` default to 6 significant digits, `%i`
  maps to `%d`, `%u` takes the unsigned 32-bit interpretation, `%c` takes a char
  code, C length modifiers (`%lld`, `%llx`) are decoupled from Go's fmt.
  Pinned by `TestFormatCommandMatchesTcl` (tclsh-verified).
- `db eval "...\$x"` / `execsql "...\$x"`: escaped parameter references survived
  TCL substitution and reached the fixture as `$x` SQL parameters — nothing bound
  them, so atof1 executed 19,999 queries with an unbound parameter. `dbEval` and
  `cmdSQL` now run the evaluated word through `bindSQLParams`, which inlines the
  current TCL value as an SQL literal unless the variable is a *dynamic* variable
  (runtime query result), in which case the `$name` reference is kept for the
  harness's `expandTCLRuntimeVars` (fixes `TestExecSQLResultAssignmentIsCapturedAsRuntimeStep`).
  Pinned by `TestDBEvalBindsEscapedParamsInQuotedSQL` / `...NumericEscapedParams`.
- `expr rand()`/`srand()` now reproduce TCL's Park–Miller LCG (IA=16807, IM=2³¹−1,
  Schrage; result multiplied by the double constant `1.0/RAND_IM`) as implemented by
  `TclRand`/`TclSrand` in `tclBasic.c`: `srand(seed)` masks the seed to 31 bits (with
  the `^123459876` rule for 0/2³¹−1) **and consumes one draw**, so the first `rand()`
  after `srand(1)` is 16807²/IM = 0.13153778814316625. The old stub returned the
  constant `math.Float64frombits(12345)`, so atof1's 20,000 loop iterations all
  collapsed to the same `-Inf` value and the fixture became
  `SELECT -Inf='-Inf'` ("no such column: Inf") ×19,999. Pinned by
  `TestRandMatchesTcl` (exact bit patterns) and `TestRandMatchesTclAtof1Loop`
  (the atof1 loop shape; ±1 ulp allowed because Go's `math.Pow` is not bit-identical
  to the platform libm `pow` TCL calls).
- Result: regenerated `atof1.json` (40,006 distinct tests, no Inf/NaN artifacts,
  `SELECT <32-digit xf>=<x>` / `CAST(quote(x) AS real)` round-trips) — **atof1
  19,999 → 0 failures**, installed.
- `do_catchsql_test { SQL }` had the same parameter-binding gap as `db eval`
  (round 4): the SQL word is braced, so TCL does not substitute `$i` even though
  testfixture's `catchsql` binds it from the caller's scope. Every iteration of
  e.g. `rtree8-2.1.3.$i { SELECT * FROM t1 WHERE id = $i }` executed the same
  unbound SQL. `cmdDoCatchSQL` now calls `bindSQLParams` like `cmdSQL` does;
  pinned by `TestDoCatchsqlBindsTCLVariables`. Re-converting the 256 files that
  contain `do_catchsql_test` changes 12 fixtures: bestindexC, e_expr,
  fts3corrupt5, fts4content, fts4merge, indexexpr2, misc1, rtree8, tabfunc01,
  tclsqlite, triggerE, zipfile — they get a targeted second verify-install pass
  after the main batch (env-parameterized `/tmp/regen3-batch.py`).
### Fixture size budget — the multi-GB conversion/test blow-up
- Root cause: `for {set iError 1} {$bStop == 0} {incr iError}`-style loops have
  runtime-dependent termination; the converter unrolled them to the 50k loop cap
  times their nested `foreach` combinations. The regen-full installs contained
  fixtures like `backup_ioerr.json` = **1.1 GB / 1.8M tests / 6.6M statements**
  (old fixture: 735 bytes), `savepoint6` 288 MB, `backup` 222 MB, `ioerr2` 202 MB.
  A full `go test ./...` loaded and executed them → >20 GB RAM and multi-minute
  runs; `backup_ioerr`/`savepoint6`/`e_walauto` are not in the skip lists
  (`backup`/`ioerr2`/`fuzzer2`/`interrupt`/`crash`/`crash4`/`fts4merge4` are
  `unsupportedTestFiles`, so they were dead weight in the tree).
- Second leak in the same code path: `runInterpreter` used `select`/`time.After`
  for its 30-second timeout, abandoning a goroutine that kept running the
  interpreter and appending to `i.stmts` forever — one runaway loop per timed-out
  file accumulated for the rest of the (multi-file, single-process) conversion.
- Fix (`tools/tclconvert/`): new per-interpreter statement budget (150,000) and
  deadline (30 s) enforced cooperatively by `checkRunaway` before every command
  (sticky `ErrStatementBudget`/`ErrConversionDeadline` errors so even `catch`
  cannot swallow them); `runInterpreter` is now synchronous with `recover` and
  sets both. Oversized files become **conversion errors**: no JSON is written and
  the installed fixture stays in place. `backup_ioerr`/`savepoint6`/`e_walauto`
  now abort in ~2 s with 367 MB peak RSS (was multi-GB). Pinned by
  `TestStatementBudgetStopsRunawayLoops` (incl. the `catch` variant),
  `TestConversionDeadlineStopsExecution`, `TestStatementBudgetDoesNotAffectSmallFiles`.
- The 10 already-installed oversized fixtures were restored from
  `/tmp/regen-full-backup`: backup, backup_ioerr, crash, crash4, e_walauto,
  fts4merge4, fuzzer2, interrupt, ioerr2, savepoint6 (≈1.5 GB of testdata reclaimed).
- Full-corpus re-conversion with the fixed converter ran into `/tmp/regen-full3`
  (`/tmp/regen-full3.log`): **1,118/1,219 files converted (77 skipped, 24 errors,
  EXIT=0)**, no fixture over the statement budget; the only files it refuses that
  earlier rounds converted are the pathological ones above (checked set difference
  vs `/tmp/regen-full2` and `/tmp/regen-full`), which keeps their restored small
  fixtures. Re-conversion of the 50 `srand`/`rand()` files (e.g. round1, rtree2,
  speed1p, soak) was folded in after the PRNG fix.
- Diff vs the installed tree: **308 changed families** = 101 existing + **207 new
  families that had no fixture at all** (none of them are in the skip lists, so all
  207 are real new coverage; 39 MB total, largest 100k steps `fuzz_malloc`, 50k
  `soak`, 44k `e_expr`). Excluded from install: `fts4merge4` (161 MB, unsupported),
  `speed3` (20 MB, unsupported) and `round1` (100k tests, not currently installed —
  evaluate separately).
- Verify-install batch `/tmp/regen3-batch.py` → `/tmp/regen3-batch.jsonl`,
  `/tmp/regen3-reverted.txt`, `/tmp/regen3-timeouts.txt`: one run of the installed
  fixture (baseline), install fresh, run again; revert when a previously-present
  leaf regresses; new families get verdict `new-family`; a new fixture that times
  out twice is removed. First 9 families processed before a script crash on a
  missing installed fixture (`attachmalloc`, now handled); re-running from
  `attachmalloc` after the PRNG fix with the extended changed list.
### Converter (`tools/tclconvert/`)
- `%d` error-template formatting via `tclFormat`; `ifcapable EXPR BODY else ELSEBODY`; `{*}` expansion;
  canonical `{}` serialization.
- `sqlite3 db ""` / `sqlite3 db :memory:` now emit `open_empty_db` instead of `reset_db`: TCL opens a fresh
  EMPTY main handle and leaves on-disk `test.db` intact (`triggerupfrom.test:85-92`). `reset_db` keeps its
  delete+reopen semantics.
### Harness (`frigolite_harness_test.go`)
- `routeFixtureAttachPaths` keeps `':memory:'` in memory (no fixture-dir rewrite). Before the fix every
  `ATTACH ':memory:'` attached one real file named `:memory:`, so `db0`..`db7` shared a pager
  (`temptrigger` section 9).
- New `open_empty_db` step: close current db if open, `Open(":memory:")`, re-register echo unless
  vtab1/altertab.
- `usesReopen` scan treats `open_empty_db` as a lifecycle signal (main models TCL's `test.db` file, so
  earlier writes reach the file and later ATTACHes find it).
### Engine
- `internal/execddl/ddl_trigger.go` — `validateTriggerGrammarRestrictions`: qualified INSERT/UPDATE/DELETE
  targets inside trigger bodies are rejected only for non-temp triggers; `INDEXED BY`/`NOT INDEXED`
  rejected in every trigger body on UPDATE/DELETE.
- `internal/execddl/ddl_trigger_validate.go` — `temporary trigger may not have qualified name` (checked
  before schema resolution); `schema.Manager.FindTrigger` exact-name matching.
- `internal/exec/schema_validate.go` — `ValidateAttachedSchema`: attach-time `sqlite3Init` parity (row
  checks + trigger load validation BEFORE registration). A malformed attached schema now fails the ATTACH
  itself and rolls back (oracle-verified), instead of surfacing on the next statement.
- `internal/execdml/insert_conflict_tail.go` — `ValidateAttachedSchemaTriggers`.
- `internal/execdml/insert_select.go` — `checkLoadedTableRefCtx` mirrors attach.c `fixSelectCb`: an explicit
  database name other than the trigger's own schema is malformed even when that database exists (compare
  names first); schema-qualified refs must still resolve.
- `internal/execdml/insert_core_tail.go` — fire-time validation uses the trigger's OWNING schema and exempts
  TEMP triggers (attach.c `bTemp`).
- `internal/execddl/ddl_attach.go` / `ddl_core.go` — ATTACH validation hook wiring.
### Fixtures regenerated from `ori/sqlite/test`
`e_update`, `e_delete`, `e_insert`, `trigger1`, `trigger7`, `triggerupfrom`, `temptrigger` (all stale or
hand-trimmed before).

## Latest full-suite evidence
- Run7: `go test ./... -count=1` → `/tmp/frigolite-full-suite7.log` (9.5 MB), exit marker
  `/tmp/frigolite-full-suite7.exit` = 1. 1,811 failing leaf cases across 282 harness families. The log
  contains non-text bytes: use `grep -a`.
- Sorted failing-set snapshot: `/tmp/run7-fails.txt` (`grep -a -o "FAIL: TestSQLiteSuite/[^ ]*" ... | sed 's/ (.*//' | sort -u`).
- Run8 (after the trigger/attach work): `/tmp/frigolite-full-suite8.log`, exit marker
  `/tmp/frigolite-full-suite8.exit` = 1. 1,712 failing leaf cases across 272 families (102 fixed vs
  run7, no real regressions): sorted snapshot `/tmp/run8-fails.txt`. The only native failure was
  `TestMiscregReattachedSchemaNameRevalidatesTriggers`, whose old expectation contradicted SQLite
  (oracle rejects attaching a file whose trigger body names another database even when the object
  exists); the test was rewritten to the oracle scenario and passes. Three "new" `temptable2` leaf
  names are the family's random()-driven incremental-vacuum relocations (same error class in run7).
- Run8 triage: top error clusters are result mismatches (393), "table tN already exists" (285 leaves,
  155 families — stale fixtures, see batch above), "expected error but got success" (86), error
  mismatch (68), and per-feature gaps (`pointer_change` UDF 29, `sqlite_rename_table` 18, transaction
  lifecycle ~36).

## Immediate next actions
0. **Start at "⭐ RESUME HERE" above** (session 3 state: e_expr 16,657 → 139, converter changed
   substantially, full re-conversion + delta-verify still pending).
1. Let `/tmp/regen3-batch.py` finish (302 families: 101 existing + 207 new; verdicts in
   `/tmp/regen3-batch.jsonl`). Handle `round1` separately (100k tests / 23 MB, not
   installed yet — run it isolated, check time+RSS, then decide). Review reverted and
   timeout families; the new families' failures are surfaced engine gaps, not
   regressions.
2. Then re-run the full suite serially with a unique log (run9; compare against
   `/tmp/run8-fails.txt`). Memory: the >20 GB runs came from the oversized fixtures now
   restored (size budget above); still prefer `go test -p 1 ./...` (or GOMEMLIMIT) and a
   unique log, and check RSS during the root-package run. Expect the failure count to
   *rise* vs run8 because 207 previously-absent families now run; use the batch JSONL to
   separate newly surfaced leaves from true regressions.
3. `sqlite_rename_table` (fkey2/without_rowid3, 18 leaves): the engine already registers
   `FnSQLiteRenameTable` as an `InternalOnly` function and the fresh fixtures contain the
   `sqlite3_test_control SQLITE_TESTCTRL_INTERNAL_FUNCTIONS db` toggles (`internalFunctions`
   step → `execDB.ToggleInternalFunctions()`); re-check after the refresh before coding.
4. Next root causes: transaction lifecycle ("cannot start a transaction within a
   transaction", "cannot commit - no transaction is active" — trans/trans2/triggerC/
   snapshot3), `sqlite3_normalized_sql`/`sqlite3_normalize` SQL-normalization feature
   (normalize family, feature-sized), stmt-status counters (`where7` ~2000 / `where8` 34 —
   require a SQLite-style FULLSCAN_STEP/SORT counter + multi-index OR planner; document
   until feasible), then result-mismatch families by size.
5. Write a scoped plan before each nontrivial fix; use SQLite as oracle; add native pins; never alter skip
   lists or expected outcomes.
6. After each fix: isolated family, converter tests (`go test ./tools/tclconvert/... -count=1`),
   `git diff --check`; update this file and `.agents/lessons_learned.md`.
7. `TestSQLite3ResetAfterForcedeleteURI` (converter) is currently green — it was resolved during the
   converter repairs; do not re-open unless it fails.

## Workspace cautions
- Workspace has many unrelated modified/untracked files. Never reset/clean broadly; inspect scoped diffs
  only. No commits have been made.
- Never regenerate fixtures with a converter that lacks the statement budget: runtime-terminated TCL
  loops produced 0.2–1.1 GB JSON fixtures (backup_ioerr, savepoint6, backup, ioerr2, …), which made a
  full test run exceed 20 GB RAM. Oversized files must stay conversion errors (old fixture kept).
- The full suite now takes **~55 min** (run 11: 8,395 failing leaves, peak RSS ≈ 7.4 GB) because the
  fixtures are faithful and contain far more tests than the stale ones; run it in the background with a
  unique log/exit path and compare against `/tmp/run11-fails.txt` (not run8's 1,712 — the test sets
  differ).
- Project rules: plan before every nontrivial change; durable findings go to `.agents/lessons_learned.md`.
