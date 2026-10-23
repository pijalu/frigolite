# e_createtable custom SELECT-test helper conversion

## Evidence
- Latest profile `/tmp/frigolite-after-trustschema.jsonl` has 27 e_createtable leaves; early failures include 2.3.2.* missing x1–x4, while source `../sqlite/test/e_createtable.test:635-648` creates schema and 696-715 expects `do_createtable_tests 2.3.1` to create x1–x4 and then verifies NULL defaults.
- Source defines `do_createtable_tests` at `e_createtable.test:31-33` as a wrapper over tester.tcl `do_select_tests`; `tester.tcl:1103-1157` expands `{testNumber sql expected}` triples into do_execsql_test/do_test cases, executes repair before/after each case, and applies query/tclquery modes.
- `testdata/e_createtable.json` omits source setup `e_createtable-2.0` and test cases 2.1–2.3.1; converter emits malformed placeholder `"e_createtable-2.3.1"` rows later, causing downstream cascades. `/tmp/ecreate-regenerated/e_createtable.json` confirms current converter omits helper-generated cases.

## Newly reproduced earliest divergence (2026-06-18)
Initial exact reproduction showed first failure at `e_createtable-0.1.2.1`: source's `-error` extension was emitted as plain `exec`. Added RED/green regression and mapped `-error` to `catch` with catchsql-style `1 {expected}` so existing harness validates error substring. After fix, exact leaf passes; latest family run exposes Tcl-helper fixture/engine failures starting at 1.3.1, plus malformed database cascade at 4.15.1.1. Run output retained `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/goa-output-1991120642.txt`.

## Ordered plan
1. DONE: regression and implementation for `do_select_tests -error`; exact previously failing leaf passes.
2. Continue helper-generated test audit: `do_select_tests` Tcl query/error/count modes and required setup/testcase ordering.
3. Isolate earliest remaining family divergence before engine edits; retain malformed database cascade as downstream until 4.15.1.1 can be reached cleanly.

## Next divergence: Tcl-query mode
Exact family output `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/goa-output-1991120642.txt` starts failing at 1.3.1. Source `e_createtable.test:399-411` uses `-tclquery` to run `unset`, `array set X [table_list]`, and `list $X(main)...` after each CREATE. Current `commandDoSelectTests` stores both `-query` and `-tclquery` into one SQL query statement, yielding SQL syntax error at `unset`. This is conversion mode loss, not CREATE TABLE engine behavior.

Plan before edits:
1. RED test: verify `-tclquery` retained as distinct typed Tcl-query assertion, not SQL query.
2. Trace harness Tcl-expression capabilities and interpreter DB callback support; choose smallest complete bridge that evaluates source Tcl script against live connection state, preserving per-case sequencing.
3. Implement and add direct regression covering schema list output across main/temp/attached DBs.
4. Regenerate `e_createtable` only; rerun exact family and track next earliest divergence.
5. Keep malformed database cascade deferred until preceding fixture/setup issues corrected.

## Next failure: IF NOT EXISTS colliding with index
Latest full-family output `/tmp/ecreate-tclquery4.log`: early `-tclquery` failures are cleared, and NULL-default expectation failures are cleared by ordered nullvalue markers. First remaining failure: `e_createtable-1.9.1`, `CREATE TABLE IF NOT EXISTS i1(a,b)` returned success although source setup `e_createtable-1.8.0` created index `i1` on `t1`. Source `e_createtable.test:531-539` requires error when name collides with an index, even under IF NOT EXISTS. Harness preceding setup is present at fixture 1.8.0; verify engine behavior with native regression before changing engine code.

Plan: (1) add pure-Go regression with table+index and conflicting CREATE TABLE IF NOT EXISTS, observe RED; (2) inspect SQLite create-table name resolution path and local validation; (3) implement narrowly, rerun native and exact suite; (4) proceed to remaining failures (time-dependent defaults, `nextint`, malformed DB cascade).

## Next missing setup: scripted clock and function
Latest run `/tmp/ecreate-tclquery6.log`: fixture contains 3.5 and 3.7 SQL cases, but source sets `sqlite_current_time=1000000000` at lines 923 and 1005 and registers `db func nextint nextint` at 973-975; these are not represented in generated steps. Date defaults therefore use wall clock; nextint defaults fail with no-such-function.

Plan before edits:
1. RED converter tests: `set sqlite_current_time 1000000000` emits ordered clock marker; `db func nextint nextint` emits callable registration marker carrying Tcl proc body.
2. Add harness runtime handling with `function.SetNowFunc` restoration, and register zero-arg increment function implementing Tcl `incr ::nextint` semantics.
3. Regenerate `e_createtable`, run 3.5/3.7/3.8 cases and exact family; preserve error result evidence.
4. Continue remaining exact-family divergences including create-table column limit and malformed index cascade.

## Column-limit/Tcl global repair verified
`SQLITE_MAX_COLUMN=2000` now seeds converter globals; source `sqlite3_limit` calls emit ordered `set_limit` markers handled by harness. Regenerated 3.10.1 now has 2000 columns, 3.10.2 has 2001; 3.11.1–3.11.5 limit markers apply runtime values. Exact family output `/tmp/ecreate-limit.log` has no remaining 3.10/3.11 failures.

## Reset integrity corruption: focused plan

Evidence: exact-family trace `/tmp/ecreate-droptrace.log` shows `PRAGMA integrity_check` is `ok` before the `drop_all_tables` immediately preceding 4.3.0, then reports `Freelist: 2nd reference to page 25` after dropping t10 and additionally page 10 after t9. `4.2` repeatedly creates and drops t5 through its `-repair` hook; 4.1 has just created t1–t4. SQLite source `../sqlite/src/btree.c:10374` routes `sqlite3BtreeDropTable` into `btreeDropTable`, which must free an index/table tree and update root-page metadata; creation root allocation localizes to `internal/execddl/root_alloc.go:14-18` calling `BTree.AllocateRootPage`.

Plan before implementation:
1. Add narrowly scoped temporary trace at harness case boundary for `e_createtable-4.1` and `4.2`: query schema rootpages before each step/repair and integrity after repair; retain SQL step and output.
2. Identify earliest duplicate rootpage or integrity transition; reproduce that exact CREATE/DROP sequence as a pure-Go regression and observe RED.
3. Trace allocator/drop page transitions; compare root allocation/drop invariants with SQLite `btree.c` and relevant source call sites. Repair smallest root cause.
4. Remove temporary trace; run native regression and exact family, then full package and repository tests.

Root trace update: `/tmp/ecreate-resetroot.log` confirms earlier reset failure: after each successful `DROP table t10`/`t9`, both entries remain queryable in `sqlite_master`; this is not a stale result cache. A direct 12-table, 500-column-per-table reproduction first REDs: all DROP statements return nil, yet all 12 schema entries remain and `PRAGMA integrity_check` errors. Native regression retained in `frigolite_schema_split_drop_test.go`. Root cause: `DeleteCellsWhere` predicates intentionally receive local-only payloads; wide `CREATE TABLE` SQL overflows sqlite_schema records, so `storage.DecodeRecord` fails and schema deletion silently misses them. Root test REDs before any engine change. SQLite `clearCell` frees overflow only after selecting row; schema matching still needs full payload. Plan: add an overflow-aware bulk-delete variant preserving local-only semantics for existing callers; use it in schema remove/update paths; add tests for both DROP and schema rename/update; then run regression and exact family. This explains reset leftovers and subsequent root aliases. Interior table cells are not the identified cause.

Root trace update: `/tmp/ecreate-roottrace3.log` shows aliases before reset: at `e_createtable-4.2.1.2`, `sqlite_autoindex_t5_1` has rootpage 10, same as extant table t9; at `4.2.2.1`, `sqlite_autoindex_t5_1` has rootpage 7, same as extant table t5. Integrity still reports `ok`, and root values change across repeated CREATE/DROP. Reset exposes a duplicate-rootpage defect created during `4.2` root allocation/schema bookkeeping; do not treat reset's `DROP` as origin. Trace `allocateRootPage`/`initIndexRootPage` against pager metadata and freelist.

Latest family output `/tmp/ecreate-limit.log`: earliest remaining bug is `e_createtable-4.8.1`, UPDATE NULL on t1 returns `btree: unexpected page type 0x0d for index seek`; subsequent 4.9+ malformed errors are cascade. Source `e_createtable.test` around 1280-1320 uses a UNIQUE index then updates nullable indexed values. Before engine edits: write a pure-Go regression for the exact schema/data/update, record original failure and page/index state; inspect SQLite index seek/update maintenance path and local btree implementation.

## Next plan
1. RED pure-Go test for source 4.8.1 scenario; compare standard SQLite oracle behavior.
2. Localize unexpected index-seek page-type route, then source-read SQLite's index update/delete algorithm.
3. Fix minimal engine issue; rerun native, exact 4.8.1, then full family to see whether 4.9 cascade clears.
4. Resume remaining subfamilies without skips or weakened assertions.

## Ground truth
`tester.tcl:1139-1155` executes repair before the loop and after each generated test. `-query` evaluates SQL to create the table then compares schema query results; `-tclquery` runs SQL then evaluates Tcl query. Preserve this sequencing and captured expected list.
