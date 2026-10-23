# alterlegacy Fixture Lifecycle Repair

## Scope
Repair only `alterlegacy.test` lifecycle markers and fixture-file deletion routing; preserve unrelated workspace edits.

## Source contract
`../sqlite/test/alterlegacy.test` has `reset_db` then `forcedelete test.db2` before section 8; `reset_db` before section 9; `forcedelete test.db2` between 9.3 and 9.4; `reset_db` before section 10 and again before section 11. Tcl `db func trigger trigger` registers the trigger callback for section 11.

## Changes and rationale
- Keep section-8 reset marker; add ordered `delete_file test.db2` before 8.1.
- Replace misplaced named delete marker before 9.4 with an ordered harness delete-file step in its own setup testcase.
- Preserve reset markers at section boundaries; add ordered `delete_file test.db2` before 11.0.
- Resolve delete targets `test.db` and `test.db2` relative to lifecycle database temp directory; ordinary non-lifecycle paths unchanged.
- Install `trigger` callback with alterlegacy fixture setup, because section 11's Tcl proc invokes it inside a TEMP trigger.

## Verification
`python3 -m json.tool testdata/alterlegacy.json`, `git diff --check`, and `FRIGOLITE_TEST=alterlegacy go test . -run '^TestSQLiteSuite$' -count=1` pass. No earliest remaining alterlegacy harness or engine-visible failure remains.
