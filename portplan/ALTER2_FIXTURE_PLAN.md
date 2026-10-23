# alter2 repair plan

## Evidence
- Baseline focused run: `FRIGOLITE_TEST=alter2 go test . -run '^TestSQLiteSuite$' -count=1`; failures: unsupported file format expected but succeeds (alter2-4.2/4.4), shorthand `set_val` proc lacks metadata/body (setup_2), and trigger reports `no such column: old.b` (alter2-8.2).
- `testdata/alter2.json` already carries ordered `alter_table` markers but no operation order metadata around `set_val` or mixed header edits; preserve all unrelated workspace changes.
- Harness has short-record test and partial zero-arg callback support; `buildRowMapFromValues` maps only stored fields, leaving absent schema fields unresolved.

## Ordered implementation plan
1. Fix converter `db function` shorthand: recognize braced body callback `{set ::val}`, emit proc body/zero arity. Harness callback stores global value, and step results update corresponding assertion state. Add focused converter/harness regression.
2. Consult `../sqlite/src/prepare.c` format checks and reject schema format outside supported range at load/prepare. Add native format-above-supported tests.
3. For row map construction, populate every declared column absent from short record with NULL (while preserving IPK handling); add native short-record trigger OLD/NEW regression.
4. Inspect harness mutation execution and generated source order; make alter/header mutations durable around close/reopen. Regenerate only `testdata/alter2.json` from `../sqlite/test/alter2.test`; verify marker ordering.
5. Update `.agents/lessons_learned.md`; run native short-record regression, `git diff --check`, and exact focused alter2 suite.

## Completed repairs and evidence
- SQLite `src/prepare.c` `sqlite3InitOne` maps schema format 0 to 1 and rejects values above `SQLITE_MAX_FILE_FORMAT` (4). Added guard to schema-load preflight and native version-5 rejection test.
- `buildRowMapFromValues` now fills every declared column; short records therefore expose absent fields as NULL in OLD/NEW. UPDATE row images allocate OLD slots through declared width before applying added-column defaults, matching SELECT semantics. Native trigger regression verifies OLD.b NULL and NEW.b set.
- Converter recognizes Tcl callback command prefix `{set ::val}` as one-argument global assignment; harness captures value. `set ::val` assertions become ordered `runtime_var` markers.
- Regenerated only `testdata/alter2.json` from `../sqlite/test/alter2.test`; fixture is ordered and includes mutation markers in source order, callback registration, and `alter2-8.3` runtime assertion.
- Baseline-to-final focused failure list resolved. Final focused command passed after these changes.

## Verification
`go test . -run '^Test(Alter2ShortRecordAfterFormatUpdate|Alter2RejectsUnsupportedFileFormat|TCLSetGlobalFunctionCallback)$' -count=1`
`go test ./tools/tclconvert/... -count=1`
`git diff --check && FRIGOLITE_TEST=alter2 go test . -run '^TestSQLiteSuite$' -count=1`
