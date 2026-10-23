# aggnested 10.1 fixture mismatch

## Evidence
- Full suite `/tmp/full-go-test-final2.log` reports `aggnested-10.1` expected error but got success.
- Authoritative `../sqlite/test/aggnested.test:479-485` is `do_execsql_test 10.1` and expects rows `{1 2}` for exact SQL; fixture `testdata/aggnested.json:441-447` incorrectly encodes a `catch` expecting `misuse of aggregate: sum()`.
- Regenerated source to `/tmp/tclconvert-aggnested-out/aggnested.json` using `go run ./tools/tclconvert -testdir /tmp/tclconvert-aggnested-src -outdir /tmp/tclconvert-aggnested-out`; output correctly emits query expectation `1 2`. Regeneration also changes unrelated setup/reset and omits fixture-only 11.1-11.3, so do not copy whole generated fixture.

## Plan
1. Edit only aggnested-10.1 step to match source command type `query` and expectation `1 2`; preserve surrounding fixture-only coverage/setup.
2. Run exact `FRIGOLITE_TEST=aggnested go test . -run '^TestSQLiteSuite$/^aggnested$' -count=1`.
3. Inspect newly exposed earliest aggnested failure against source and regenerate only source-proven fixture errors; otherwise add native engine regression and read corresponding SQLite source.
4. Record full-suite source-faithful fixture lesson and rerun full suite only after next measurable repair slice.

Follow-on fixture-only cases: after fixing 10.1, exact aggnested leaves 11.2 and 11.3 fail because they expect `misuse of aggregate: SUM()`. Neither case exists in authoritative `aggnested.test`, which ends after 10.1. SQLite CLI probe of 11.2 SQL returns successful empty result (t2 is empty); 11.3 differs only by `WHERE true` and likewise succeeds empty. Keep both fixture cases but change type from `catch` to `query` with empty expected output, preserving assertion that SQL prepares/executes and yields no rows. Verify against SQLite CLI and exact family.
