# analyzeE Empty-Text BETWEEN Regression

## Evidence

After STAT4 selectivity change, exact `FRIGOLITE_TEST=analyzeE go test . -run '^TestSQLiteSuite$' -count=1` passes broad/selective planner cases 3.x/4.x and now first fails at `analyzeE-5.0`. Canonical `../sqlite/test/analyzeE.test:245-252` resets DB, sets UTF-16 encoding, inserts empty string into indexed TEXT column, analyzes, then expects `SELECT * FROM t0 WHERE t0.c1 BETWEEN '' AND (ABS(''))` to return the empty-string row (Tcl expectation `{}`). Current engine returns no rows. This is execution/coercion, separate from STAT4 planning.

## Steps

1. Add native regression for exact SQL and compare intermediate `ABS('')`, `typeof`, and BETWEEN predicate results.
2. SQLite CLI confirms the result row is an empty string: `SELECT typeof(ABS('')), quote(ABS('')), c1 BETWEEN '' AND ABS('') FROM t0` returns `real|0.0|1`; native forced-scan and index plans both return the row. Engine predicate is correct; harness mismatch comes from `cleanExpected` collapsing outer `{{}}` into `{}` and treating that as NULL.
3. Add converter marker `emptytext` only for the exact Tcl one-empty-string expectation; harness asserts one row/one cell equal to `""`. Add converter regression, regenerate only analyzeE and inspect marker/SQL/expectation.
4. Run exact analyzeE; continue remaining source-faithful failures.
3. Implement source-faithful smallest correction, ensure index and full scan parity.
4. Run native test and exact analyzeE; then continue current full-suite failure inventory.