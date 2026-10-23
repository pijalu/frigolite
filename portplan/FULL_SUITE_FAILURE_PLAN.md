# macOS Full-Suite Failure Triage Plan

## Baseline evidence
- `go test ./...` completed with exit 1 after root package `FAIL` at 366.318s; `/tmp/frigolite-final.exit` contains `1`; retained log `/tmp/frigolite-final.log` is 9.6MB.
- 361 failing leaf cases within `TestSQLiteSuite`; independent native failures: `TestP4Numeric_AbsRound`, `TestP4Numeric_NaNInf`, `TestWith1NestedNotMaterializedFromLimit`.
- Do not edit unrelated pre-existing workspace changes; no skips or assertion weakening.

## Current earliest failure: check-7 stale fixture / missing Tcl CHECK UDF
- Fresh full run `go test ./... -count=1` recorded at `portplan/CACHEFLUSH_REASSESSMENT.*` exits 1; earliest SQLite family is `check`, 7.1–7.8. Isolated exact `FRIGOLITE_TEST=check go test . -run '^TestSQLiteSuite$' -count=1` fails because stale `testdata/check.json` omits source setup `db func myfunc -deterministic myfunc` and named handle db2 behavior from `../sqlite/test/check.test:452-480`.
- Regenerated fixture in `/tmp/check-fixture/check.json` contains faithful setup registration and db2 lifecycle, but executing setup fails: harness callback adapter rejects `myfunc` body `expr $x < 10`.
- Repair: regenerated only `testdata/check.json`; converter now captures named `db2 func`/`function` registration markers, routed to that connection. Harness runtime adapter evaluates exact deterministic body `expr $x < 10` as SQLite integer boolean. Native `TestTCLExpressionFunctionAdapter` and converter `TestNamedDBFunctionRegistrationEmitsDatabaseMarker` pass; exact `FRIGOLITE_TEST=check` passes.
- Refreshed full run after check repair: `/tmp/frigolite-attach2-reassessed.stdout`, stderr `/tmp/frigolite-attach2-reassessed.stderr`, status `/tmp/frigolite-attach2-reassessed.status` (exit 1; command `go test ./... -count=1`, 378.049s root duration). `check` no longer appears among failed families. First reported harness family is `collate7`: `collate7-2.1` fails `no such collation sequence: CASELESS`; 2.2/2.4 cascade because table was not created. Exact isolated reproduction passed through diagnosis: `FRIGOLITE_TEST=collate7 go test . -run '^TestSQLiteSuite$' -count=1` exits 1 with same errors.
- Authoritative `../sqlite/test/collate7.test:21-70` requires Tcl collation registration (`caseless_cmp` uses case-insensitive string comparison), deletion callback/destructor count, close/reopen file lifecycle, then re-registration and second deletion. Current generated JSON omits all callback operations; fresh conversion preserves ordered lifecycle but still omits custom callbacks. Static harness registration would be wrong because tests assert deletion/destructor lifecycle. Scoped plan/blocker: `portplan/COLLATE7_REPAIR_PLAN.md`. No fixture or expectation changed.
- Same full run reports numerous unrelated suite failures (`printf`, `ptrchng`, `without_rowid`, many others) and native join failures; not green. Target `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1` passes after this run; `git diff --check` passes.

## First isolated fix
- Repro: `go test . -run '^(TestP4Numeric_AbsRound|TestP4Numeric_NaNInf)$' -count=1` fails because `formatSQLiteValue` in `frigolite_test.go:284-297` explicitly maps positive infinity to lowercase `inf` while tests/source SQLite expect `Inf`. `util.FormatSQLiteReal` already implements SQLite spelling.
- Change: remove special threshold branches; return `util.FormatSQLiteReal(v)` for every float64.
- Verified: `go test . -run '^(TestP4Numeric_AbsRound|TestP4Numeric_NaNInf)$' -count=1` passed after change.
- Full-suite residual independent tests `TestP4Numeric_AbsRound` and `TestP4Numeric_NaNInf` are resolved.

## Second isolated family: nested CTE FROM-term limit
- Repro: `go test . -run '^TestWith1NestedNotMaterializedFromLimit$' -count=1` fails because query returns nil error.
- Source: `../sqlite/src/build.c:4785-4808` enforces `SQLITE_MAX_SRCLIST=200` at `sqlite3SrcListEnlarge`; CTE refs attach their SELECT at `select.c:5869-5883`, and NOT MATERIALIZED enables inlining behavior. The existing Go `countStatementFromTerms` counts CTE bodies once rather than accounting for repeated inline references, underestimating this nested exponential case.
- Plan before edit: inspect parser AST/hint representation and use a scoped CTE expansion-count model to account for each reference without counting unreferenced/materialized bodies incorrectly. Add a native regression from with1-22.1, compare success/error against source SQLite, run isolated then relevant with1 suite.

## Third isolated family: with1 multiline string result
- Repro: `FRIGOLITE_TEST=with1 go test . -run '^TestSQLiteSuite$' -count=1` leaves only `with1-8.1-mandelbrot` failing in that package; engine output's first line has SQLite-required 36 leading spaces.
- Source: `../sqlite/test/with1.test:377-416` encodes multiline scalar under Tcl braces; `/tmp/sqlite-stat4` returns the same leading spaces. Current `cleanExpected` removes outer braces then `TrimSpace`s payload, stripping significant data.
- Plan: add helper regression proving braced multiline interior whitespace survives, change only braced scalar cleanup, rerun native Mandelbrot prefix test + isolated with1 suite.

## Refreshed run correction
- Two detached full-suite runs overlapped after a stale exit marker obscured the first process. Their output (`/tmp/frigolite-final.log`, exit 1 after 373.142s) is not reliable for harness failure-family counts because both mutated shared harness database files concurrently. Do not use its 358-family summary to prioritize fixes.
- It did identify one isolated stale test expectation: `TestFormatSQLiteValueInfinity` expected `inf/-inf` while `util.FormatSQLiteReal` and SQLite's `%g` oracle return `Inf/-Inf`. Updated the expectation only; focused `TestFormatSQLiteValueInfinity`, `TestP4Numeric_AbsRound`, `TestP4Numeric_NaNInf` passed. A new single-process full run is retained at `/tmp/frigolite-clean.log` with exit marker `/tmp/frigolite-clean.exit`.

## Fourth isolated family: analyze5 fixture-generation mismatch
- Clean serialized full run: `go test ./...` exited 1 after 373.212s; retained at `/tmp/frigolite-clean.log` and `/tmp/frigolite-clean.exit`. `TestSQLiteSuite` took 149.57s; analyze5 has 88 failing child cases, the largest family.
- Isolated repro: `FRIGOLITE_TEST=analyze5 go test . -run '^TestSQLiteSuite$' -count=1` exits 1 after 3.612s. Detailed output saved at `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/goa-output-1539004161.txt`. Initial `analyze5-1.0` got `[NULL]` vs `[alpha bravo charlie delta]`, followed by range queries where forced/indexed results differ.
- Source evidence: `../sqlite/test/analyze5.test:43-68` loops 1000 times, derives columns t/u/v/w/x/y/z, conditionally unsets x or w, and calls Tcl `db eval` with `$` parameters (unset means NULL). Current generated `testdata/analyze5.json:15-...` repeats `INSERT INTO t1 VALUES(0.5,NULL,'',0,0,0,0)` for the first 400 rows; those rows should vary and include non-NULL `u`. Thus STAT4 output/range mismatches are downstream symptoms of incorrect fixture data, not evidence against the native-pinned STAT4 algorithm.
- Converter inspection (`tools/tclconvert/tcl/interp_cmds.go:22-96`) shows `for`, `expr`, `unset`, and `string` handlers exist, but `switch` is absent and unknown commands silently no-op (`interp_cmds.go:99-109`). This explains the `u`/`v` columns being unset/NULL and the repeated placeholder rows.
- Added converter `switch` dispatch plus simple pattern/default execution; test-first regression `TestSwitchSelectsMatchingAndDefaultBody` failed before the implementation (all SQL params NULL), then passed after it. Regenerated only `testdata/analyze5.json`. Confirmed 89 test cases, 1000 inserts, all expectations retained, and row boundaries/data shapes match the source loop. `go test ./tools/tclconvert/tcl -count=1` and `go test . -run '^TestAnalyze(Stat4|5)' -count=1` pass; isolated `analyze5-1.0` passes.
- Important residual: isolated family still fails on 86 `*b` cases. Source `analyze5.test:186-197` runs NOT INDEXED and indexed queries, compares their Tcl lists (`$a1==$a2`), and expects `ok`. Current JSON captures two independent query steps, attaching `ok` to the second query; harness has no pairwise SQL-result-equality step, so it compares row ids against literal `ok`. These failures do not establish engine divergence. Do not change the expected `ok` or interpret as planner defects without expressing source comparison semantics. Plan a generic source-faithful paired-query assertion format before additional fixture edits.

## Fifth isolated family: stale select3 fixture ordering
- Source: `../sqlite/test/select3.test:22-35` requires creating and populating `t1` before aggregate tests. Clean full log `/tmp/frigolite-clean2.log` shows `select3-1.0` fails `CREATE TABLE t1` with `already exists`, then `select3-1.1` sees 0 rows and follow-ups miss columns. Existing `testdata/select3.json` has no `ordered:true`; it places tests 12.0–12.8 before select3-1.0, although source places those tests later. This is a causal harness fixture-order defect and produces cascading failures.
- Converter check: running only `go run ./tools/tclconvert/ -testdir ../sqlite/test -outdir /tmp/select3-generated select3.test` produces `ordered:true`, 96 source-order test groups, with `select3-1.0` first (existing fixture has 55 tests and stale order).
- Regenerated only `testdata/select3.json` from `../sqlite/test/select3.test`. Result: 96 source-order test cases (91 unique names), `ordered:true`; setup tests `select3-1.0/-1.1` first, `12.0` at position 87, `12.8` last. Existing fixture had 55 cases, no ordered flag, setup around position 10, and duplicate names. No manual reorder/expectation edits.
- Verified: `FRIGOLITE_TEST=select3 go test . -run '^TestSQLiteSuite$' -count=1` passes; `go test ./tools/tclconvert/tcl -count=1 && git diff --check` passes. This fixes the table-already-exists cascade for select3.

## Sixth isolated family: stale altercol fixture
- Post-select3 clean full run: `/tmp/frigolite-clean3.log`, exit `1`, total 376.468s, harness 149.60s. Failures reduced 2,887→2,842; families 358→357; `select3` reduced from 45 to zero failing child tests.
- `altercol` has 58 failures, beginning at `altercol-8.5.2` (`DROP name, SELECT` syntax error), then malformed schema reads and `t1 already exists`. Source `../sqlite/test/altercol.test:346-389` expects 8.5.2 to run a SELECT, then `do_rename_column_test` iterates source SQL definitions and calls `drop_all_tables_and_views`.
- Existing `testdata/altercol.json` has 291 tests and wrong `DROP name, SELECT` / `DROP FROM type` / `DROP WHERE sqlite_master` statements within `altercol-8.5.2`. Fresh converter output in `/tmp/altercol-generated/altercol.json` has 288 ordered groups and correct `CREATE TABLE t1`/trigger setup in that test; no fabricated DROP SQL. This is source/fixture conversion defect.
- Converter fixture regeneration fixes fabricated DROP SQL but `altercol` still fails because the helper proc's database query cannot execute in the converter's no-live-DB model. Source `altercol.test:30-38` shows `drop_all_tables_and_views` queries and drops only `main.sqlite_master`; no general database state needed beyond an explicit cleanup marker.
- Added converter recognition for this declared cleanup helper, emitting dedicated `__DROP_MAIN_TABLES__`; test-first marker regression failed then passed. Harness drops only main tables/views and preserves FK pragma, matching helper scope. Regenerated only altercol fixture; prior 58 failures now 2 (`altercol-14.2`, `altercol-18.0`).
- `altercol-14.2` source toggles `SQLITE_TESTCTRL_INTERNAL_FUNCTIONS`; expects SQLite internal `sqlite_rename_column`, absent from Frigolite. No approximation planned.
- `altercol-18.0` source at `altercol.test:768-779` explicitly does `reset_db; forcedelete test.db2` before attach. Added typed `delete_file` statement/path marker (test-first regression failed then passed), harness removes only specified file, and regenerated altercol fixture. Isolated family now has only `altercol-14.2` failing; `18.0` passes.
## Previous valid full-suite baseline
- Pre-select3 run: `go test ./...` exit 1 after 376.054s; `/tmp/frigolite-clean2.exit` is `1`, log `/tmp/frigolite-clean2.log`; harness 150.28s. 2,887 failing children across 358 harness families. `analyze5` reduced 88→86; no top-level native failures. Two STAT4 sample result cases were fixture artifacts.

## Clean4 results
- After analyze5, select3, and altercol fixes: `/tmp/frigolite-clean4.log`, exit `1`, full duration 359.998s, harness 145.20s. Failures 2,842→2,773; family count 357; `select3` remains 0; `altercol` falls to 2 from 58; analyze5 86 unchanged. Remaining altercol-14.2 internal function is a known unsupported SQLite test-control surface.

## Clean5 results
- After all four data/fixture fixes: `/tmp/frigolite-clean5.log`, exit `1`, total 365.979s; harness 148.03s. 2,716 child failures across 356 families. `select3=0`, `joinF=0`, `altercol=2` (internal function unsupported), `analyze5=86` (paired-query output cannot yet be expressed).

## Current broad run after conflict fixture repair
- `go test ./... -count=1` serialized run `/tmp/frigolite-current-full-suite3.log`, exit 1 after 381.897s root. `conflict`, `conflict2`, and `conflict3` no longer fail after source regeneration; first remaining family is `cost` with table-already-exists setup cascades.
- Source `../sqlite/test/cost.test` has ordered setup by numeric source order. Stale `testdata/cost.json` was unordered and placed 9.2 before 4.1, etc., reusing t1 and causing collisions. Fresh conversion `/tmp/cost-generated/cost.json` has ordered=true and 52 source-order cases. Replaced only `testdata/cost.json`; exact `FRIGOLITE_TEST=cost go test . -run '^TestSQLiteSuite$' -count=1` now passes.
- Next full-suite run required before attribution of next family. A native `TestConflict7UpdateIgnorePreservesOneNonConflictingRow` passes; join positional aliasing is fixed/pinned by `TestCollateBJoinOrderKeepsMatchedRightRow`.

## altertab-16 defensive-mode shadow-table guard
- Exact repro: isolated `/tmp/altertab-viewtrigger.log` had `altertab-16.10` INSERT into `y1_segments` succeed; SQLite source `altertab.test:537-557` registers Tcl module, enables DEFENSIVE before FTS3 creation, expects shadow writes/drops/rename rejected, then disables DEFENSIVE.
- Fixture now preserves ordered `defensive` markers at source positions, but the isolated family still fails. `internal/exec/engine_progress.go` exposes `SetDefensive`, yet setting is only consulted by schema-version pragma; FTS shadow protections are not implemented. SQLite `build.c:3455-3477` applies read-only checks to recognized shadow tables; this is an engine behavior gap, not marker ordering.
- Plan: inspect `xShadowName`/module shadow identity contract and existing schema resolution APIs; add test-first regression for FTS3 shadow INSERT and DROP with defensive mode enabled and disabled; implement shadow classification at common DML/DDL preflight following SQLite's registered module contract; run focused regression and isolated altertab. Do not add more harness state changes until engine guard path verified.

## altertab-2.2 focused plan
1. Regenerate `testdata/altertab.json` using current converter because existing fixture contains neither `register_echo_module` nor `reopen_db`, despite source lines 72-91 issuing explicit module registration then closing/reopening the file-backed DB.
2. Inspect fresh conversion output for faithful ordered setup and reopen markers; adjust harness lifecycle only if needed after fixture correction.
3. Add/run focused regression for post-reopen `no such module: echo`, then isolated altertab suite and retain output before proceeding.

### Next residual: `altertab-4.2` Tcl UDF
- Source `../sqlite/test/altertab.test:151-162` defines `squish` as collapsing every whitespace run to one space and trimming ends, then queries renamed trigger SQL. Converted JSON retains `SELECT squish(sql)` but cannot represent Tcl `db func` registration.
- Plan: add a scoped `altertab` harness UDF matching `squish` exactly; add focused native regression covering whitespace collapse/trim; run native regression and isolated altertab suite. No engine edits or expectation changes.

### Results
- Fresh source conversion generated 166 ordered test groups and correctly preserved `register_echo_module`, `close_db`, and `reopen_db` around cases 2.1/2.2; checked against `../sqlite/test/altertab.test:71-91`.
- Replaced only `testdata/altertab.json` with fresh converter output. Focused echo close/reopen native test passes. Harness now opens altertab without implicit echo registration and does not silently re-register at reopen; explicit converted marker supplies registration.
- Isolated suite `/tmp/altertab-fixed.log`: `altertab-2.2` now passes; remaining 29 children fail (down from 30). Further source-backed diagnosis required; no tests skipped/weakened.
- Source-backed Tcl UDF mapping added as `harnessCollationFixtures["altertab"]` using whitespace split/collapse semantics from `string trim [regsub -all {[[:space:]][[:space:]]*} ... { }]`. Native SQL test `TestAltertabSquishHarnessFunction` passes; fresh isolated run `/tmp/altertab-squish.log` reduces remaining failures from 29 to 28 (`altertab-4.2` passes).
- `register_tcl_module` was parsed but initially omitted by `tools/tclconvert/main.go`'s step allowlist; fixed allowlist, regenerated only altertab fixture. Harness executes marker using the existing `vtab.TclCommandModule`, with schema `CREATE TABLE t1(a, b, c)` matching source xConnect proc. Isolated `/tmp/altertab-tclmodule2.log` now passes `altertab-6.0/6.1`; residual children fall to 26. `TestAltertabTCLVtabCreateRename` validates module create/rename/schema SQL independently.
- `altertab-11.1` source at `../sqlite/test/altertab.test:337-355` defines a `trigger` Tcl UDF invoked only by a TEMP trigger on attached `aux.t1`; generated fixture omits TCL proc. Added source-scoped no-op SQL callback; native regression verifies only aux insert invokes callback with `[4,5,6]`. Isolated failures fell 25→23.
- Next `altertab-15.4`: after fixing 15.5 alias preservation, isolated output now fails `NOT NULL constraint failed: x.f2`; source has INSTEAD OF UPDATE trigger body `UPDATE x SET f1=NEW.f1`, and `ALTER TABLE x RENAME f1 TO f2` must rewrite that DML assignment to f2. Plan: native regression pins trigger SQL and update-through-view result. Added `Assignment.ColumnTok`, populate both SET grammar rules from terminal token, and extend `internal/rename` UPDATE walker to collect only assignment target ranges for matching table. Native `TestAltertabColumnRenamePreservesViewAlias` now verifies view remains `f2 AS f1`, trigger becomes `UPDATE x SET f2=NEW.f1`, and update-through-view changes all three rows. Parser/rename/execddl/exec tests pass; isolated `/tmp/altertab-viewtrigger.log` passes 15.4/15.5, residual family failures 20→19.
- `altertab-15.5`: source at `../sqlite/test/altertab.test:499-506` expects column rename to update view expression but preserve alias. Replaced broad `renameColumnInViews` string rewrite with parser token ranges; added `ColumnRef.NameTok` from lexer token positions (including quoted token ends) to support structural rewrite. Native `TestAltertabColumnRenamePreservesViewAlias` passes, parser/exec/execddl suites pass, isolated `/tmp/altertab-viewalias.log` passes 15.5 and leaves 20 failures. trigger body `SELECT y FROM t1,t2` becomes ambiguous. Native `TestAltertabColumnRenameAmbiguousTriggerReference` reproduces current success vs required `error in trigger tr1 after rename: ambiguous column name: y`. A SELECT probe before persistent schema write cannot resolve future column names from old sqlite_schema; added trigger SELECT AST/from-table column-map collision check covering supported top-level expressions/clauses and table column maps, counts renamed target under new name plus other FROM operands, rejects before mutation. Native regression passes and confirms original `t2.b` preserved on error. Parser/exec/execddl package tests pass; isolated `/tmp/altertab-columnfix2.log` passes 13.2 and leaves 21 altertab failures (next 15.4).
- New earliest engine failure `altertab-11.4`: expected TEMP trigger tbl_name stay `t1` after `ALTER TABLE main.t1 RENAME TO t2`, but got `t2`; trigger is bound to same-named `aux.t1`. Plan: add native main+aux same-name table / temp trigger regression, verify SQLite source’s schema identity behavior, then correct trigger rename eligibility without changing unrelated entries. Root coupling: `renameUpdateRelatedEntries` walked every schema and `triggerEntryRenameAllowed` used only `TblName`; passed target schema from `entryCtx.Name`, and for temp-owned triggers rewrite only when ON target resolves to that schema (unqualified target defaults to main). Native `TestAltertabTempTriggerOnAttachedTable` initially failed (`got t2, want t1`), now passes main-rename preservation. Isolated `/tmp/altertab-tempfix.log` passes cases 11.1-11.7; residual failures 23→22.

- `sqlite_rename_table` prototype exposed a concrete prerequisite gap: `internal/rename.FindRenameTokens` returns no ranges for `CREATE TABLE ddd(a)` because parser's `CreateTableStmt.NameTok` is `{Start:0, End:0}`. Initial native regression failed (`CREATE TABLE ddd(a)` unchanged instead of quoted rename); incomplete helper was removed.
- Parser diagnosis: CREATE TABLE rule received `nm` as string; `parser_rules_misc.go` discarded `sql.Token.Pos`. Test-first `TestCreateTableNameTokenOffsets` failed at `{0,0}`; changed `nm` reductions to preserve `sql.Token`, relying on existing `getString` token handling, and added token end offsets to lexer for quoted names. Native test now passes bare and quoted identifiers; full parser and rename package tests pass.
- Implemented `FnSQLiteRenameTable` using SQLite's 7-argument contract and parser-backed `FindRenameTokens`/`ApplyRenames`, registered internal test function. Native `TestFnSQLiteRenameTable` passes NULL and quoted-renaming cases; isolated `/tmp/altertab-rename-table.log` now passes altertab-7.2 and reduces remaining family failures 26→25.
- Next leaf `altertab-6.0/6.1` requires SQLite Tcl virtual-table module (`register_tcl_module`, `tcl_command` proc). Source contract at lines 212-230 is create/rename and preserved sqlite_schema SQL. Frigolite exposes `RegisterVtabModule(name, vtab.Module)`, so engine-visible behavior can be pinned natively; JSON needs an explicit harness fixture hook for source-position module registration rather than a skip. Plan: implement a small test module declaring `t1(a,b,c)`, run native CREATE/ALTER/SELECT sqlite_schema assertion first, then connect file-specific harness registration to source marker and regenerate/verify altertab.

- Selected using retained clean5 full-run evidence: `altertab` had 36 failing children; `analyze9` had 34 but substantial overlap with STAT4 test UDF setup; `collate4` had repeated `COLLATE TEXT` failures. Current log `/tmp/frigolite-final.log` is prior/stale and not used for family counts.
- Isolated reproduction: `FRIGOLITE_TEST=altertab go test . -run '^TestSQLiteSuite$' -count=1` exits 1; `/tmp/altertab-isolated.log` retained. Earliest output includes unexpected success for `ALTER TABLE fff RENAME TO ggg`, then missing Tcl helper `squish`, unsupported `USING tcl` virtual table, and cascading failures (`x1`, repeated t1 already exists).
- Triage: `squish`/`tcl` and `sqlite_rename_table` failures are SQLite Tcl test scaffolding/internal test-control surfaces; table-already-exists is likely cascade after an earlier unexpected-success assertion, but this needs exact child-case/source mapping before any fix. No edits made; this is not yet an engine regression classification.
- Next exact step: identify first failing leaf case SQL + source line from `../sqlite/test/altertab.test`, inspect SQLite `alter.c` contract for that case, and determine converter setup fidelity vs engine behavior; write focused plan and native test before change. Preserve isolated log and run a single harness process only.

- Clean3 evidence `/tmp/frigolite-clean3.log`: all 55 `joinF` tests fail with `no such table: t1`; source `../sqlite/test/joinF.test:32-43` creates t1–t4 and rows before test 1.
- Existing `testdata/joinF.json` had 54 cases, no ordered flag, and started at `joinF-1`; setup absent. Replaced only this file with source-generated ordered fixture, 55 cases including `setup_0` before joinF-1. Isolated `FRIGOLITE_TEST=joinF go test . -run '^TestSQLiteSuite$' -count=1` passes; converter tests and `git diff --check` pass.

## Tenth: per-call aggregate row ownership and regression full run
- Native `TestAggnestedPin` added aggnested-1.2 and initially failed with `1x1-4y5` vs `1x2x3-4y5`. SQLite source `select.c:6623-6650` processes aggregate calls individually; added per-call row-map dispatch to support mixed outer/inner aggregates. Native pin now passes; isolated aggnested leaves independent aggregate-validation failures 10.1, 11.2, 11.3.
- A descendant-walk-only attempt was reverted after changing output to NULL. Per-call association keeps existing global fallback for single aggregates.
- Full run `/tmp/frigolite-progress2.log` timed out at 600s (exit 1), stack spent 6m57s in `temptable2-9.2.4.3`; this is anomalous against previous 335s run. Isolated `FRIGOLITE_TEST=temptable2 go test . -run '^TestSQLiteSuite$' -count=1` completes in 3.9s but fails `temptable2-9.2.12.1` with B-tree incremental-vacuum relocation mismatch. Need capture fresh profile/serialized full-run timing and investigate whether suite timeout is contention or current code regression before further family work.

## Ninth family: analyze5 source comparison assertion
- Isolated current repro `/tmp/analyze5-current.log`: data setup/sample checks now pass; all remaining 86 failures arise because converted two `db eval` calls are represented as separate queries, with literal `ok` attached to second query. SQLite source `../sqlite/test/analyze5.test:186-197` compares result lists and returns `ok` only when equal.
- Plan: add explicit generic `compare_query` fixture step with two SQL fields (rather than weakening/removing expected `ok`); harness executes both and compares rows; unit-test comparator semantics, update analyze5 generated fixture cases, run converter and isolated analyze5 suite.
- Clean5 `/tmp/frigolite-clean5.log`: vtab1 has 38 failures; isolated `/tmp/vtab1-isolated.log` reproduces. Source `../sqlite/test/vtab1.test:56-75` asserts `echo` is unavailable initially, then explicitly calls `register_echo_module`; source `:212-213` repeats registration before a later constructor test.
- Harness's `setupDB` (`frigolite_test.go:65-70`) pre-registers echo for every connection; tclconvert silently ignores `register_echo_module`. Thus initial source contract differs, and early failed-table state cascades (vtab1-1.16/1.17). Engine already exposes `DB.RegisterEchoModule`.
- Plan before edit: test-first add Tcl registration-marker conversion test; implement marker classification/emission, make TestSQLiteSuite vtab1 connection/reset start unregistered while preserving current default for all other fixtures, execute registration marker at source position; regenerate only vtab1.json; verify early unavailable module, explicit registration behavior, isolated family. No skipping or expected-error edits.
- First isolated pass after registration markers: `FRIGOLITE_TEST=vtab1 go test . -run '^TestSQLiteSuite$' -count=1` still failed (38 leaf cases). Source order proves database reconnection is represented by the `sqlite3 db test.db` reset marker after `db close`; an extra standalone unregister marker is not a faithful lifecycle substitute. An attempted unregister-marker change increased failures to 47, so it must be reverted. Next fix reset_db handling to preserve vtab1's unregistered initial-module state.

## Session 2026-10-19: trigger family repairs + attach-time schema init
- Evidence base: `/tmp/frigolite-full-suite7.log` (exit 1, 1,811 failing leaf cases across 282 families; log has
  non-text bytes, use `grep -a`). Sorted failing snapshot `/tmp/run7-fails.txt`.
- Corpus authority confirmed as `ori/sqlite/test` (3.54-equivalent). 3.51.0 rejects temp-trigger qualified DML
  targets that 3.54 accepts; engine matches 3.54. Do not "fix" toward `../sqlite`.
- Converter repairs (each with converter regressions in `tools/tclconvert/tcl/interp_sql_test.go`): `%d`
  error-template formatting (`tclFormat`), `ifcapable EXPR BODY else ELSEBODY`, `{*}` expansion, canonical `{}`
  serialization, and `open_empty_db` (replaces the incorrect `reset_db` mapping for `sqlite3 db ""` /
  `sqlite3 db :memory:`; TCL opens a fresh empty handle and keeps `test.db` on disk).
- Harness repairs: `routeFixtureAttachPaths` no longer rewrites `':memory:'` (temptrigger 9.x needs eight
  independent in-memory DBs, not one file named `:memory:`); new `open_empty_db` step; `usesReopen` lifecycle
  scan includes `open_empty_db` (main then models TCL's file-backed `test.db`).
- Engine repairs: trigger grammar restrictions (qualified DML targets non-temp only; INDEXED BY all triggers on
  UPDATE/DELETE); `temporary trigger may not have qualified name`; `FindTrigger` exact match;
  attach-time `sqlite3Init` parity — `ValidateAttachedSchema` runs row + trigger validation before registration
  so a malformed attached schema fails/rolls back the ATTACH itself (triggerupfrom-2.4); `checkLoadedTableRefCtx`
  mirrors attach.c `fixSelectCb` name-comparison semantics; fire-time trigger validation uses the trigger's
  owning schema and exempts TEMP triggers (attach.c `bTemp`).
- Fixtures regenerated from `ori/sqlite/test`: `e_update`, `e_delete`, `e_insert`, `trigger1`, `trigger7`,
  `triggerupfrom`, `temptrigger`. Isolated families green; no new failures vs run7 for `trigger*`, `view*`,
  `fkey*`, `alter*` (verified with `comm -13`).
- Next: run8 (`/tmp/frigolite-full-suite8.log` / `.exit`); triage next earliest family.

## Session 2026-10-19 (continued): run8 triage, view joins, fixture-regen batch
- Run8 evidence: exit 1, 1,712 failing leaf cases across 272 families (102 fixed vs run7; sorted
  snapshot `/tmp/run8-fails.txt`). Only native failure was
  `TestMiscregReattachedSchemaNameRevalidatesTriggers`, whose expectation contradicted the oracle
  (attaching a file whose trigger names another database is malformed even when the object exists);
  the pin was rewritten to the oracle scenario (A = unqualified-refs trigger attaches and fires,
  B = foreign-name trigger fails the ATTACH) and passes.
- Three "new" `temptable2` names are the family's random()-driven incremental-vacuum relocation set,
  which differs run to run (run7 had the same error class with different leaves).
- Run8 top clusters: result mismatch 393; "table tN already exists" 285 leaves/155 families; expected
  error but got success 86; error mismatch 68; `pointer_change` UDF 29; transaction lifecycle ~36;
  `sqlite_rename_table` 18.
- View join repairs (`portplan/VIEW_NATURAL_JOIN_PLAN.md`): `viewStarSourceDefs` applies the
  USING/NATURAL star merge to schema-based view defs; `markJoinSourceColumns` recognizes named-view
  join sources (`sourceNames`) so the omit-unused-column optimization cannot NULL a join-constrained
  column. Oracle-verified (`PRAGMA table_info` single `x`; `count(*) FROM v1 NATURAL JOIN v1` = 2).
  `join5` fixture regenerated → family 18 → 0.
- Fixture-regen batch: the "already exists" cluster is stale fixtures missing TCL section resets
  (`db close; forcedelete test.db; sqlite3 db test.db`, `reset_db`). Fresh conversions for 144
  families generated to `/tmp/regen-all` (142 ok; `tkt2686`, `types` conversion errors). Batch
  installer/verifier `/tmp/regen-batch.sh` → `/tmp/regen-batch.log` with per-family logs and
  backups; regressions (old-fixture test starts failing) trigger revert. Next: review results and
  re-run the full suite.


## Session 2026-10-19 (continued 2): batch verdict, converter round 2/3, full refresh
- Batch 1 verdict (144 families): re-verified in Python (locale-safe `comm` had warned): **0
  regressions**; 654 previously-failing leaves fixed, 3,105 surfaced by the more faithful fixtures.
- The big surfaced clusters were mostly converter artifacts, not engine gaps. Converter round 2
  (`tools/tclconvert/tcl/`): `subst` option abbreviations (`-novar`/`-nocommand`); bracketed SQL
  words go through `bindSQLParams` like braced SQL; shared `parseTclIndex` for `string range/index`
  + `lrange`/`lindex`/`lreplace` (`end`, `end±N`, `N±M`, negatives — `Atoi("end")` was silently
  returning 0/empty); `switch` body fallthrough (`== - IS {...}`); multi-pair `foreach`
  (`foreach c $cList v $vList`, longest-list semantics with empty fill).
  Result: `rowvalue2` 2,430 (intermediate) → 222 → **0**; `distinct`, `stmt` green.
- Converter round 3: `sqlite3_create_function DB` marker (`register_test_funcs`) + harness
  `pointer_change`/`x_coalesce` ports (`frigolite_harness_testfuncs_test.go`). `ptrchng` 29 → 0;
  `bind`, `fpconv1` green. `groupToTestCase` whitelist now keeps the marker.
- Full-corpus re-conversion to `/tmp/regen-full`: 913 JSONs (30 skipped, 16 errors). **732 of 913
  fixtures differ** from installed. Batch 2 installer/verifier running
  (`/tmp/regen-full-batch.sh` → `/tmp/regen-full-batch.log`, backups `/tmp/regen-full-backup`,
  per-family logs `/tmp/rfb-out-<fam>.log`, reverted list `/tmp/regen-full-reverted.txt`); each
  family is reverted when a leaf that existed in the old fixture starts failing.
- Still-open clusters after 3,105 → post-round-2/3 reclassification: WAL checkpointing, transaction
  lifecycle, `sqlite_rename_table` (18), prepared-statement harness gaps (`normalize` 49 + 56 ×
  "exactly one SQL statement"), stmt-status counters (`where7` ~2000, `where8`), plus whatever the
  full refresh surfaces.
