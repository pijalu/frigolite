# e_createtable reset repair — complete execution plan

## Objective and boundaries

Repair source-faithful `e_createtable` fixture execution and remaining engine failures without changing expected results, skipping tests, or disturbing unrelated dirty-worktree edits. Then continue the active full-suite goal only after this family is stable. Keep evidence in this plan and durable lessons in `.agents/lessons_learned.md`.

## Verified actuals

1. Converter work already in workspace preserves `do_select_tests -error`, Tcl-query assertions, list serialization, ordered nullvalue/current-time/UDF/limit markers. Column-limit cases 3.10/3.11 no longer appear among failures in `/tmp/ecreate-limit.log`.
2. Repro command: `FRIGOLITE_TEST=e_createtable go test . -run '^TestSQLiteSuite$/^e_createtable$' -count=1`.
3. Before reset repair, `/tmp/ecreate-droptrace.log` showed valid integrity before reset before 4.3.0, then duplicate freelist pages after DROP. `/tmp/ecreate-resetroot.log` showed `DROP t10/t9` return nil but rows remain visible in `sqlite_master`.
4. Cause isolated: long CREATE SQL makes schema records overflow. Schema matching calls `DeleteCellsWhere`; that API intentionally passes local payload only. `storage.DecodeRecord` fails for overflow-backed sqlite_schema rows; DROP silently fails to delete schema records. This is not an interior-cell deletion or root allocator defect.
5. Native RED: new `frigolite_schema_split_drop_test.go`, creating twelve tables with 500 columns each then dropping all, failed with all rows left and integrity query failure. After introducing `DeleteCellsWhereFullPayload` and switching schema remove/update/root-update paths, `go test . -run '^TestDropTablesFromSplitSchemaBTree$' -count=1 -v` passed.
6. Focused e_createtable command was rerun after the fix, output retained at `/tmp/ecreate-fullpayload.log`; command output was truncated by tool and exact exit / full failure list still must be extracted. Temporary harness tracing remains in `frigolite_harness_test.go`; partial cleanup has started and currently removed trace variable declaration only, leaving references, so workspace currently does not compile until instrumentation cleanup is completed. Temporary probe `frigolite_tmp_drop_probe_test.go` remains and must be deleted using file-edit tool.
7. One residual named failure seen in truncated family output: `e_createtable-5.5.3`; must establish earliest residual failure from full retained log, not infer from tail. Existing worktree is extensively dirty; preserve all unrelated changes.

## Follow-up focused plan: duplicate primary key (first remaining family failure)

Exact clean family output `/tmp/ecreatable-clean.log`: only two leaves fail; earliest is `e_createtable-4.1.2.1`, SQL `CREATE TABLE t5(a INTEGER PRIMARY KEY, b, c, PRIMARY KEY(a))` returns success. Source `../sqlite/test/e_createtable.test:1128-1136` requires all six duplicate-PK declarations to fail with `table "t5" has more than one primary key`. Current `validateTableKeyConstraints` in `internal/execddl/ddl_create_validate.go:492-538` already counts non-promoted column PK and table PK but discounts `PKPromoted`; inspect parser output and promotion semantics. Second remaining leaf `5.5.3` is a Tcl helper mismatch and waits until first failure is fixed.

Plan before edits:
1. Add pure-Go RED test using the exact SQL and assert SQLite error substring; run alone.
2. Inspect parser AST (`PrimaryKey`, `PKPromoted`, table constraint columns) for exact statement plus adjacent legal INTEGER PK and exact duplicate PK declarations. Compare against SQLite parser/build behavior in `../sqlite/src/build.c` (`sqlite3AddPrimaryKey`) and parse.y actions.
3. Fix PK declaration counting at correct source of truth, preserving legal duplicate key-list/index dedup semantics and WITHOUT ROWID behavior.
4. Run native regression, neighboring e_createtable 4.1.2 leaves, and exact family; update failures from actual output.

Execution evidence: parser scratch AST for the exact failing SQL had column `a` with `PrimaryKey=true, PKPromoted=true`, plus one table-level PRIMARY KEY constraint. `promoteColumn` unconditionally set the promotion flag, erasing declaration provenance; `countPrimaryKeyDeclarations` then counted only table-level PK. Changed promotion to return without changing the column when it is already a declared PK. RED native test passed; `go test ./internal/parse -count=1` passed. Clean exact-family rerun `/tmp/ecreatable-after-pk.log` now has only `e_createtable-5.5.3` failing. Scratch parser test moved out of repo.

Next residual source case: source `../sqlite/test/e_createtable.test:1819-1828` maps helper case 5.5.3 to `CREATE TABLE t(x INTEGER, y, z, PRIMARY KEY(x DESC))`, expecting rowid alias `1`. Parser promotion previously skipped all DESC table-level PKs; SQLite `build.c:1867-1871` gets table constraint sort order separately, while only column-level `INTEGER PRIMARY KEY DESC` is exception. RED native test proved current result `0`; SQLite CLI oracle returned `1` for table-level DESC, `0` for column-level DESC. Removed DESC skip in table-level `promoteConstraints`; native duplicate-PK + table-DESC tests and `go test ./internal/parse -count=1` pass. Exact family now passes: `FRIGOLITE_TEST=e_createtable go test . -run '^TestSQLiteSuite$/^e_createtable$' -count=1`, output `/tmp/ecreatable-after-desc.log`, exit 0.

Full-suite first attempt (`go test ./...`, 364s) exposed full source of converter failures: `do_test` absent from `commandHandlers` map despite `commandDoTest` implementation. Added dispatch mapping; all six converter failures clear. Verified exact family, `go test ./tools/tclconvert/... -count=1`, focused schema/PK tests, B-tree/schema/DDL packages, and `git diff --check`. Required full-suite rerun pending.

## Ordered execution

### A. Restore buildable harness / keep regression
1. Remove only temporary debug instrumentation from `frigolite_harness_test.go`: root/integrity logs around all section 4 cases, reset-only trace flag/branch, per-DROP diagnostics, and special reset log. Restore ordinary `dropAllHarnessTables` behavior and test-loop behavior.
2. Remove `frigolite_tmp_drop_probe_test.go`; retain `frigolite_schema_split_drop_test.go` as regression. Format changed Go files.
3. Run `git diff --check` and `go test . -run '^TestDropTablesFromSplitSchemaBTree$' -count=1`; inspect actual output.

### B. Audit API semantic and call-site scope
1. Review `DeleteCellsWhereFullPayload` implementation, especially when `Overflow != 0`, error propagation, preservation of original local payload for `finishLeafDelete`, and no overflow reads in ordinary path.
2. Search all schema predicates decoding payload; switch any payload-dependent schema mutation still using local-only API. Keep rowid-only DML/FK predicates on existing local-only API.
3. Add/retain tests for schema remove and entry update/root update when SQL field overflows; use focused package tests.
4. Compare behavior with SQLite source `src/btree.c` clear-cell/cursor semantics; full-payload matching must not alter row ordering or schema rowids.

### C. Verify targeted compatibility family
1. Run the exact e_createtable family command; retain complete output and exit code in `/tmp/ecreate-final.log`.
2. Extract all failed leaves and identify earliest source-order divergence. Compare that case against `../sqlite/test/e_createtable.test` and `tester.tcl`; write a new focused sub-plan before any further behavior edits.
3. Repair one root cause at a time with RED regression, actual SQLite behavior/source check, minimal fix, and original failing command rerun. Do not skip or weaken cases.
4. Regenerate fixture only if source-vs-JSON evidence identifies converter mismatch; otherwise keep work in engine/harness layer.
5. Update lessons with durable finding: schema predicates on fields stored in overflow records require explicit full-payload matching, while generic bulk DML retains local-only optimization.

### D. Continue full-suite objective
1. Consult `tools/status` before/after next goal slice (invoke with interpreter if not executable).
2. Run applicable focused package tests and `go test -run TestSOLID_ ./...` after family stabilization.
3. Run `git diff --check`; inspect diff for accidental debug code and preserve unrelated dirty edits.
4. Run required `go test ./...`, retain full output and elapsed duration. On failure, identify earliest independently reproducible root issue; plan and fix without broad cleanup or test suppression. Repeat until exact completion criterion passes.
5. Only then mark active goal complete, citing the actual full-suite command, exit 0, elapsed duration, focused checks, and retained evidence path.

## Risks and guardrails

- Full payload reassembly can be expensive; activate only for schema operations whose predicates inspect record payloads. Never route rowid-only bulk deletes through it.
- `readOverflow` must not mutate stored `decoded` cells used to free original overflow chain.
- The reported `e_createtable-5.5.3` is not yet root-caused; do not chase it until clean exact-family output confirms it is earliest.
- Do not attribute remaining failures to root allocation, index UPDATE, parser, or fixture until original focused command establishes evidence.
- Do not use destructive git operations; never revert existing workspace edits.
