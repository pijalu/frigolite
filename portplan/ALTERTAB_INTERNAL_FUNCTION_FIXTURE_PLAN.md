# altertab Internal Function Fixture Repair

## Evidence

- Refreshed full suite `go test ./... -count=1`: altertab/7.2 is first remaining family after altercol fixes; fails with `no such function: sqlite_rename_table`.
- SQLite source `../sqlite/test/altertab.test:243-247` enables `SQLITE_TESTCTRL_INTERNAL_FUNCTIONS` immediately before 7.2. Existing `testdata/altertab.json` lacks that step; canonical `tools/tclconvert` output includes it after 7.1 setup and disables it after 7.2.
- Exact `FRIGOLITE_TEST=altertab` fails on that missing setup.

## Plan

1. Regenerate only `testdata/altertab.json` using `go run ./tools/tclconvert -testdir ../sqlite/test -outdir testdata altertab.test`.
2. Review diff for captured internal-function toggles and unchanged source expectations; run exact altertab family.
3. If engine-visible failure remains, diagnose against source before edits; add native regression where appropriate.
4. Run full suite with captured elapsed/stdout/stderr/status; preserve `test.db2` and all unrelated edits.
