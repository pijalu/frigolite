# colname-5.1 transform investigation and repair plan

## Evidence
- Exact failure: `FRIGOLITE_TEST=colname go test . -run '^TestSQLiteSuite$' -count=1` fails only colname-5.1. Query is `SELECT x.* FROM sqlite_master X LIMIT 1`; got rootpage `2`, fixture expects literal `x`.
- Authoritative `../sqlite/test/colname.test:251-255` wraps `db eval` in `lreplace ... 3 3 x`. This replaces flattened result element 3 (rootpage) with `x`; test does not assert alias-based star row selection or a SQLite rootpage value.
- Converted `testdata/colname.json` encodes a plain `query` with expectation `... x ...`, and no transform marker. Existing harness query transforms only support `tolower`; observed failure is a missing TCL postprocessing operation, not schema alias/source casing.
- `frigolite_full_column_names_test.go` is an existing untracked native regression and must remain untouched.

## Ordered work
1. Add converter mapping from literal-index Tcl `lreplace [db eval ...]` to a narrow query result-transform marker; add converter test.
2. Teach test harness to apply that explicit marker to flattened query output; update only `colname-5.1` fixture marker, no SQL or expectation changes.
3. Run native full-column star regression, colname and attach2 suites, related join checks, `git diff --check`, and `go test ./...`.

## Results
- `FRIGOLITE_TEST=colname go test . -run '^TestSQLiteSuite$' -count=1`: PASS.
- `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1`: PASS.
- `go test . -run 'TestFullColumnNamesExpandsBareAndQualifiedStars|TestJoinH' -count=1`: PASS.
- `git diff --check`: PASS.
- `go test ./...`: exceeded 300-second tool timeout; concrete limit is tool's 300s maximum. Root package passed focused suites; broader package test cannot be claimed passing.
- Converter's focused `TestDoTestCapturesLreplaceResultTransform` passes. `go test ./tools/tclconvert/tcl ./tools/tclconvert -count=1` additionally fails unrelated existing `TestSQLite3ResetAfterForcedeleteURI` (expects file delete followed by reset marker), retained untouched.
