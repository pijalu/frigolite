# Named Tcl unbraced eval conversion repair

## Scope and plan
- Why: regenerated attach2 fixture loses `db2 eval BEGIN`, suppressing writer transaction state and masking lock behavior.
- Files: `tools/tclconvert/tcl/interp_cmds.go`; regression in `tools/tclconvert/tcl/interp_sql_test.go`; regenerate `testdata/attach2.json` only.
- Steps: (1) RED converter regression requiring `db2`-scoped exec marker for unbraced `BEGIN` (observed failing); (2) route named `eval` through SQL capture when braced or unbraced, preserving database identity; (3) test converter and regenerate/inspect fixture; (4) rerun exact attach2 family.
- Verification: focused converter regression/package and `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1`.
