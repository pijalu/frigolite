# Hook/transaction SQL-observable semantics — oracle transcript (UCL U1/U2)

`script.sql` is a scenario exercising the SQL-observable behaviors the
dbstatus/hook/hook2/interrupt testgen packages assert: changes() /
total_changes() counters across INSERT/UPDATE/DELETE, ROLLBACK undoing an
open transaction, and trigger RAISE(ABORT/ROLLBACK) error delivery
("delete blocked", "insert blocked").

`expected_output.txt` is the verbatim `/usr/bin/sqlite3` (3.51.0) output for
the script, including the "Runtime error near line N:" diagnostics. The
C-API-only parts of the hook seam (Tcl_Eval-based progress/hook callback
delivery, tclsqlite.c DbProgressHandler L689-699, SQLITE_TEST interrupt
countdown vdbe.c L963-969) are classified in portplan/NA_EVIDENCE.md;
frigolite preserves them through equivalent Go surfaces
(SetCommitHook/SetRollbackHook/SetUpdateHook/SetPreupdateHook/
SetProgressHandler/SetInterruptCount).

Regenerate:

    /usr/bin/sqlite3 :memory: < testdata/hookconformance/script.sql \
      > testdata/hookconformance/expected_output.txt

Consumed by TestHookScriptConformance (frigolite root package).
