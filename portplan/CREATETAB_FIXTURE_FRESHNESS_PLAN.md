# createtab fixture freshness triage

## Evidence
- Serialized full run `/tmp/frigolite-current-full-suite4.log` no longer fails cost; first family is stale createtab-4.1 expecting an obsolete schema-object-limit error.
- Current source `../sqlite/test/createtab.test` tests C-API live-cursor behavior; no createtab-4.1 or trigger `r1` exists.
- Regenerated only `testdata/createtab.json` from current source into `/tmp/createtab-generated`; ordered source lifecycle restored. New exact suite now exposes source `sqlite3_prepare db ...` as stmt_step with empty handle because converter only registers `sqlite3_prepare_v2`.

## Ordered steps
1. Add test-first converter regression for legacy `sqlite3_prepare`; prove RED.
2. Register legacy name to same prepared-statement handler; focused regression passes. Full converter package still fails unrelated existing `TestSQLite3ResetAfterForcedeleteURI`.
3. Regenerate only createtab fixture again from source. C-API lifecycle now converts correctly; only createtab-3.1 remains and expects `too many columns in index` for `UNIQUE(x[string repeat ,x 100000])`.
4. SQLite Python oracle returns `too many columns in index`; native SQL was RED and accepted 100001-term UNIQUE constraint. Added CREATE TABLE validation against parsed PK/UNIQUE column count and runtime `SQLITE_LIMIT_COLUMN`; native regression now passes.
5. Exact createtab family, focused legacy-prepare converter test, attach2 and `git diff --check` pass. Full `go test ./... -count=1` currently running; output `/tmp/frigolite-current-full-suite5.log`.
