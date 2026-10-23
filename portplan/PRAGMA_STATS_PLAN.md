# pragma_stats compatibility plan

## Contract
`autoanalyze1.test` is debug-build-only. Its test-only `pragma_stats` TVF exposes per-table and per-index state using `tbl`, nullable `idx`, and integer `flgs`; tests pin `TF_HasStat1=0x10` and `TF_StatsUsed=0x100`. Expected behavior: before ANALYZE both index and table stat1 flags clear; unique PK/unique index accesses do not set table StatsUsed; non-unique index access/prefix of unique index does; rowid lookup short-circuits other constraint; connection reopen clears StatsUsed.

## Ordered work
1. Trace Engine state/lifecycle, table stat1 loading, planner access path selection and exact SQLite test hooks/source.
2. Add per-connection stats state and table-valued pragma rows using live schema/index/stat1 facts; mark stats consumed only for applicable chosen plans, not every predicate.
3. Add native tests for columns/flags and index/rowid cases; source-faithful test lifecycle if harness permits.
4. Run focused native and exact autoanalyze1 command; retain full-suite run status, then capture fresh full-suite result and identify next independent family.
5. Update lessons and feature status/plan evidence.

## Progress evidence
- Implemented `pragma_stats` TVF with `(tbl, idx, flgs)`, stat1-derived `TF_HasStat1`, and connection-local StatsUsed state. Added chosen non-unique-index plan recording, unique full-key exemption, and exact fixture reopen markers for source's `sqlite3 db test.db` boundaries (300/310/320/400/500).
- Focused native coverage in `frigolite_pragma_stats_test.go`; `FRIGOLITE_TEST=autoanalyze1 go test . -run '^TestSQLiteSuite$' -count=1` passes. `go test ./internal/exec ./internal/execquery -count=1`, `go test -run 'TestSOLID_' ./...`, and `git diff --check` pass.
- Fresh serialized full run completed exit 1 in 382.160s; JSON at `/tmp/frigolite-pragmastats-full2.json`, stderr empty, 1,737 failing SQLite leaf events across 323 families. First independent family is `autoinc`: `autoinc-8.1` missing `t1`; `12.3/12.4` malformed sqlite_sequence schema errors differ. Requires separate autoincrement/schema-compat work; stopping here as requested.

`go test ./...` subsets touching exec and native tests; `FRIGOLITE_TEST=autoanalyze1 go test . -run '^TestSQLiteSuite$' -count=1`; then serialized `go test -json ./... -count=1` and record first independent failure. Never edit prior workspace state unrelated to this goal.
