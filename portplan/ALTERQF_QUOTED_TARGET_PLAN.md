# alterqf Quoted Target DQS Repair Plan

## Scope
Implement SQLite `ALTER TABLE ... RENAME COLUMN` quoted-target behavior, preserving unrelated repository edits and `test.db2`.

## Evidence
`FRIGOLITE_TEST=alterqf go test . -run '^TestSQLiteSuite$' -count=1` fails 1.1–1.11 because foreach `$before` setup is absent from JSON; 2.1 independently fails: `TO 'four'` results in unquoted/broken schema and no DQS conversion. SQLite `src/alter.c` stores semantic name then calls `renameEditSql` with `bQuote`; schema token ranges receive quoted replacement selectively. Existing Go parser loses this quote mode and helpers apply plain strings.

## Harness Fixture Repair Plan

Existing `testdata/alterqf.json` omits Tcl `foreach` SQL arguments and the `reset_db` marker before section 2. A temporary test using `tools/tclconvert` output from authoritative `../sqlite/test/alterqf.test` passed the exact family. Regenerate only this fixture from that source, inspect the diff to confirm it repairs setup/order (no expected-output weakening), then run the exact family and full suite. Preserve all other workspace edits and `test.db2`.

## Changes
1. Add native RED test for `TO 'four'` with table CHECK, expression index/predicate, TEMP trigger; assert exact sqlite_schema and sqlite_temp_schema SQL.
2. Preserve target quote mode on ALTER AST while keeping semantic name unchanged.
3. Pass quoted replacement only into AST-selected identifier rename ranges; update table definition token rewrite without replacing every occurrence as raw text. Normalize DQS against old table columns via schema parsing before rename as SQLite does.
4. Run native regression and exact alterqf suite; then full suite with complete evidence.

## Verification
Native focused test, `FRIGOLITE_TEST=alterqf go test . -run '^TestSQLiteSuite$' -count=1`, diff check, full suite retained with elapsed time.
