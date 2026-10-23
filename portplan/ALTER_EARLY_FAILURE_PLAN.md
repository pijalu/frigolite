# alter.test earliest failure: application-defined function setup

## Evidence

- Full suite after converter mapping and fixture repairs (`/tmp/full-go-test-final3.log`) first fails `alter/alter-1.7-prep`; isolated `FRIGOLITE_TEST=alter go test . -run '^TestSQLiteSuite$/^alter$' -count=1` reproduces.
- Source `../sqlite/test/alter.test:176-188` defines `failing_app_func` then `db func substr/like/sqlite_rename_* failing_app_func` inside `do_test alter-1.7-prep`; only afterward checks `catchsql {SELECT substr(name,1,3) FROM sqlite_master}` expects `1 {bad function}`.
- Current `testdata/alter.json` step 1.7-prep contains catch query only and omits all four function registrations; expected error therefore cannot occur. Not evidence of ALTER engine bug.

## Plan
1. Convert source `alter.test` into `/tmp` output directory; inspect exact step and whether function markers are emitted, without replacing entire dirty fixture.
2. If converter output retains markers, transplant only ordered markers into this case and ensure callback proc bodies/arity represented; if not, write RED converter test for `do_test` containing db func setup and fix capture order.
3. Preserve five source-generated registration markers before catch step. Extend harness TCL UDF adapter to implement exact source proc `proc failing_app_func {args} {error "bad function"}` as a variadic callback returning that SQLite error; keep existing identity/counter registrations unchanged. Add focused native callback/harness coverage when feasible.
4. Run converter unit test and exact alter leaf/family; evaluate subsequent failures against source before engine changes.
4. Continue next earliest family failure only after this fixture setup is restored; run full suite when a meaningful set is fixed.
