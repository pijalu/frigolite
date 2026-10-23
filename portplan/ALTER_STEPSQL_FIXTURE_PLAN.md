# alter.test `stepsql` setup capture

## Evidence
- Full exact-family failure `/tmp/alter-after-markers.log`: alter-2.2/2.3 say `no such table: <t2>` rather than expected target-name collision; later alter-1.9 and column tests also lack tables.
- Source `../sqlite/test/alter.test:193-218` first `stepsql $DB { ALTER TABLE [-t1-] RENAME to [*t1*]; ALTER TABLE T2 RENAME TO [<t2>]; }`, then queries schema rows. This is essential setup creating `<t2>` and corresponding renamed objects.
- Both checked-in `testdata/alter.json` and freshly regenerated `/tmp/tclconvert-alter-out/alter.json` omit those two statements from `alter-1.7`; regenerated output captures only later objlist SQL. Thus failure precedes DDL engine; source helper `stepsql` unsupported in converter.
- `alter-1.7-prep` registration markers now included. Pure-Go callback test verifies `failing_app_func` callback error path, but exact prep still passed only after full existing schema context; remaining exact-family failure list no longer includes prep.

## Plan
1. Add RED converter test for a `do_test` body containing `stepsql $DB {SQL statements}`; require ordered captured exec preserving schema connection/statement boundaries. Inspect `tester.tcl` definition of `stepsql` and local command routing before mapping.
2. Implement full capture semantics, including database identity (`$DB` connection pointer if meaningful) and error propagation/step ordering, rather than text substitution shortcut.
3. Regenerate only alter into temp; compare full source test order and output. Transplant source-required missing markers/steps only, preserve other established fixture-only harness records unless source disproves them.
4. Verify alter-1.7 and exact alter family; inspect next earliest failures for converter-vs-engine evidence. Run converter tests, `git diff --check`, then full suite after substantial fixes.
