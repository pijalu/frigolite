# TCL SQL parameter binding preservation

## Evidence
- `TestExecSQLBindsOnlySQLiteParameterTokens` RED: `execsql {SELECT '$v1', $v1}` turns SQL into `SELECT ''value'', 'value'`; named binding currently replaces inside string literals and corrupts quoting.
- `regexp1` SQL patterns with `\u0024` were captured as literal `$` then treated as an unset Tcl SQLite parameter, yielding NULL; this caused remaining unicode regexp errors.
- SQLite's binder substitutes parameters only in SQL lexical code, never quoted strings/comments.

## Work completed
1. `bindSQLParams` now tracks SQL quoted strings/identifiers and line/block comments, binding only SQL code.
2. Test-first regression pinned literal preservation plus parameter substitution; red result showed prior output `SELECT ''value'', 'value'`. Additional test confirms Tcl form-feed decoding and regexp pattern retention.
3. Regenerated only regexp1; `FRIGOLITE_TEST=regexp1 go test . -run '^TestSQLiteSuite$/^regexp1$' -count=1` now passes. No expectations changed.
