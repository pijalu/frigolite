# alias.test early return fixture repair

## Evidence
- Full suite output `/tmp/full-go-test-final2.log`: `alias/setup_0` fails on missing `sequence()` after running setup SQL.
- Source `../sqlite/test/alias.test:21-23` executes an unconditional top-level `return` before the namespace, procedure, UDF registration, or `do_test` at lines 25-140. No test setup after the return is source-executed.
- Current `testdata/alias.json` incorrectly contains all later SQL as one `setup_0`; converter does not honor top-level return across file execution. This is not an engine UDF gap.

## Plan
1. RED converter test: source `return; do_execsql_test ...` captures no SQL after return.
2. Inspect `Execute`/script execution control flow; make top-level Tcl `return` terminate remaining commands while nested proc returns remain scoped.
3. Regenerate `alias.test` alone into temp directory and verify output has no post-return tests; update only `testdata/alias.json` preserving established file conventions.
4. Run converter unit tests and exact alias family. Check next full-suite source-vs-fixture divergence.

Execution: test with top-level `return\n\ndo_execsql_test ...` passed; `runInterpreter` source output to temp dir reports `skip alias: no tests captured (0 stmts)`. Therefore no Execute implementation change was needed; converter already honors return, and `testdata/alias.json` alone was stale. Replaced fixture with an ordered file containing zero tests; exact alias family now passes. This preserves source control flow; no engine UDF or test suppression.
