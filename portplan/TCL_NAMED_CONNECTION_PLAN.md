# Tcl named-connection routing for vtab1

## Evidence
`vtab1` now falls from 76 to 18 failures after restricting deleted-file `sqlite3` reset markers to primary `db`; source `vtab1.test:756-783` still has a malformed `setup_6` group. Tcl opens `db2` on `test2.db`, executes the same SQL separately against `db` and `db2`, then compares rowsets. Current converter discards DB argument and emits both copies as primary SQL; primary duplicate-key error follows. `TestSQLite3AttachedConnectionDoesNotResetMain` pins the converter bug and passes after fix.

## Ordered plan before edits
1. Add failing converter tests that `execsql {INSERT...} db2` and `sqlite3 db2 test2.db` retain database identity, plus `register_echo_module` target identity where supported. Preserve existing primary-connection marker tests.
2. Extend captured statement/JSON step contract with optional connection name. Emit explicit open/close markers for named DB connections; ensure `db2` open doesn't mutate main lifecycle. Keep file paths scoped to harness temp directory.
3. In harness, manage named `*DB` handles, execute marked SQL on intended handle, and close them on reset/test cleanup. Register test modules on the selected connection only. No changes to default behavior for fixtures without connection metadata.
4. Reconvert only vtab1 from source and inspect source-order setup; add native engine parity test for two independent echo-backed DB connections and row equality.
5. Run converter unit tests, native regression, isolated vtab1; confirm setup_6 becomes green and quantify remaining independent failures before next family plan.
