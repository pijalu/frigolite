# G00 Test Infrastructure Baseline

> Generated as part of G00: Test Infrastructure & Oracle.

## What changed

1. **Oracle harness** (`tools/oracle_generate.py`): Runs each test case's SQL
   against real sqlite3, capturing actual expected output. Eliminates false
   expectations from lossy TCL conversion.

2. **Error surfacing**: The old `checkQueryResult` silently returned (PASS)
   for known-unsupported features. It has been removed along with the
   deprecated `frigolite_sqlite_compat_test.go` (6657 unchecked `_ = db.Query`
   calls). The JSON harness now surfaces errors — known-unsupported features
   are explicitly `t.Skip` (tracked), all other errors are `t.Error`.

3. **flattenResult**: Uses `"NULL"` for nil values (was empty string).

4. **Converter**: Feature filtering moved to Go code
   (`harnessUnsupportedPatterns`). Converter only filters TCL-specific
   patterns (`$var`, `{}`, `tcl()`).

5. **Single harness**: Consolidated to the JSON harness
   (`frigolite_harness_test.go` + `testdata/*.json`). The 3MB
   `frigolite_sqlite_compat_test.go` is deprecated (renamed to `.deprecated`).

6. **Test data regenerated**: 696 files, 20,114 query expects updated
   with oracle-verified values from real sqlite3 3.51.0.

## Baseline counts (TestSQLiteSuite)

Run: `go test -v -count=1 -run "^TestSQLiteSuite$" -timeout 600s .`

| Metric            | Count |
|-------------------|-------|
| Sub-test PASS     | 2113  |
| Sub-test FAIL     | 2444  |
| Sub-test SKIP     | 47    |
| File PASS (all)   | 190   |
| File FAIL (any)   | 401   |
| Slow files skipped| 105   |

## Interpretation

- **PASS (2113)**: Frigolite matches real SQLite for these queries.
- **FAIL (2444)**: Real bugs or features not yet in the skip list. Each
  failure is either (a) a bug to fix in G01–G09, or (b) an unsupported
  feature that should be added to `harnessUnsupportedPatterns`.
- **SKIP (47)**: Explicitly skipped due to known-unsupported features
  (window functions, FTS, JSON, CTE, etc.).
- **Slow files (105)**: Skipped by default (set `FRIGOLITE_RUN_SLOW=1`).

The goal across G01–G10 is to reduce FAIL → PASS and SKIP → PASS as
features are implemented.
