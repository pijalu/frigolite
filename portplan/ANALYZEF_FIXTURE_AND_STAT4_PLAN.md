# analyzeF failure repair plan

## Evidence / diagnosis
- Refreshed full suite: `go test ./... -count=1`, exit 1, 378s; status retained as `portplan/FULL_SUITE_AFTER_ANALYZE_PASS.status`; complete stdout kept outside the repository.
- Earliest failing family: `analyzeF`. Exact command `FRIGOLITE_TEST=analyzeF go test . -run '^TestSQLiteSuite$' -count=1 -v` fails in setup (missing `isqrt`), plus missing `func()` error and SQLite test-only UDF/size-limit error cases.
- Canonical source `../sqlite/test/analyzeF.test`: registers `isqrt`, `str`, deterministic/nondeterministic zero-arg functions, throwing `error`, `dstr`, calls `sqlite3_limit`, and uses `test_zeroblob`; fixture generation lost Tcl function registrations/callback semantics and limit setup. SQL logic includes STAT4 plan-choice assertions and error-propagation checks.

## Goal
Restore canonical engine-visible analyzeF coverage without skipping/weaking tests; implement required harness callback/limit semantics only if no existing fixture representation can preserve them, and separately diagnose any genuine engine STAT4 or function errors.

## Ordered steps
1. Inspect fixture generator support for `db func`, `sqlite3_limit`, Tcl callbacks, plus harness step schema/dispatch; identify precise unsupported constructs and SQLite's error evaluation requirements.
2. Consult SQLite sources (`where.c`, `func.c`, `test1.c`) for planner parameterization and error/limit behavior. Create native Go regression(s) for engine-visible differences (function errors / oversized results or plan selection) before engine edits.
3. Write fixture/harness sub-plan and implement minimal lossless representation of Tcl function registrations/callbacks/connection limit if the converter has a general supported path; otherwise canonical fixture regeneration must not omit these semantics.
4. Implement only required engine fixes, preserving index-planning behavior. Add/update durable lesson if finding generalizable.
5. Run focused native tests, converter tests, exact analyzeF family, then refresh full suite after all first-failure-family work; retain command, complete output, elapsed time, status. Check test.db2 SHA256 unchanged.

## Verification
- `go test ./tools/tclconvert/... -count=1`
- targeted native regression(s)
- `FRIGOLITE_TEST=analyzeF go test . -run '^TestSQLiteSuite$' -count=1`
- later full suite `go test ./... -count=1`
- `git diff --check`

## Risks
TCL `db func` callback evaluation and SQLite's optimizer-driven evaluation order are harness-specific; must reproduce observable errors without hardcoding failing SQL or weakening cases. A suite family can contain separate real feature failures; retain each until fixed.
## Progress update
- Regenerated only `testdata/analyzeF.json` from canonical `../sqlite/test/analyzeF.test`; callback registrations and length-limit markers retained in source order.
- SQLite source contract validated: `where.c:2271-2314` reaches STAT4 probe; `vdbemem.c:1523-1532` restricts folding to CONSTANT/SLOCHNG, and `1741-1743` propagates callback errors.
- Implemented deterministic metadata/registration, constant-call AST retention, STAT4 evaluation, private planner error returns into SELECT result, and function-result length enforcement.
- Added native deterministic-error and nondeterministic-skip tests. Both pass; exact analyzeF family passes.
- Full suite refreshed: `go test ./... -count=1`, exit 1, elapsed 379s; status/summary retained at `portplan/FULL_SUITE_AFTER_ANALYZEF.status/.summary`; full stdout kept outside the repository. Earliest failures now `atof1`, `atof2`, and `attach` (not analyzeF); relevant targeted native and analyzeF checks passed. `test.db2` SHA256 remains `e8e0aa3243b216a87158c6a11f101a8a93d873df7ebf3285b7df1b493bb5c7ed`.
