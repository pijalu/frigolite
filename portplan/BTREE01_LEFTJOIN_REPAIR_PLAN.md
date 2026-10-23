# btree01-2.1 WITHOUT ROWID LEFT JOIN repair

## Repair and source alignment
- Symptom: `btree01-2.1` expects `(198,99)` but receives `(198,NULL)` after growing that row's BLOB to 1000 bytes.
- SQLite `src/btree.c:sqlite3BtreeNext` traverses index b-trees with their interior cells as entries; `select_exec.go:309` already constructs a cursor with `isTable=false` for WITHOUT ROWID tables.
- Root cause: `internal/execquery/select_join.go:materializeTableJoin` called `scanRealTableJoinRows`, which hardcoded `isTable=true`. This routed index-btree interior separator cells as table rowid separators; scan stopped at 89 of 100 rows.
- Repair: pass `!TableIsWithoutRowidEntry(tableEntry)` into `scanRealTableJoinRows`; preserve storage-order record remapping and normal rowid table behavior.
- Native regression: `frigolite_btree01_leftjoin_test.go`, exact overflow-growth setup and expected all three LEFT JOIN rows.

## Verification evidence
- `go test . -run '^TestBtree01LargeOverflowWithoutRowidLeftJoin$' -count=1` — PASS.
- `FRIGOLITE_TEST=btree01 go test . -run '^TestSQLiteSuite$' -count=1` — PASS.
- `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1` — PASS; supersedes attach2 note in `ATTACH_FIXTURE_REGEN_PLAN.md`.
- `git diff --check` — PASS.
- `go test ./...` — exceeded 300s tool timeout; no output returned, not verified.
- Broadened `FRIGOLITE_TEST=join go test . -run '^TestSQLiteSuite$' -count=1` — FAIL. Reports join-26.1 nested parenthesized join; join4 duplicates; join5/join6/joinI many stale table-state/setup errors, plus several independent join semantic mismatches; joinH RIGHT JOIN output mismatches. Exact output is in continuation transcript. This is broader evidence only, not proof all these surfaced due to this repair. No claims of full-suite success.

## Next actions
1. Re-run narrow regression and `git diff --check` after any edits.
2. Triage broader join failures individually (pure Go reproducer, SQLite oracle, source-aligned plan) before unrelated changes.
3. Retry `go test ./...` in a longer-running execution environment; current tool imposed 300s timeout.
