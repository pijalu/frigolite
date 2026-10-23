# STAT4 Sample Generation Plan

## Objective
Port SQLite 3.51.0 `analyze.c` STAT4 candidate accumulator faithfully. Diagnose exact 24-row interleaved multi-column mismatch, ensure persisted statistics belong to finalized selected candidate, and validate index cursor order and source-built SQLite oracle.

## Evidence so far
- SQLite source: `../sqlite/src/analyze.c` lines 511-665 (`sampleIsBetterPost`, `sampleIsBetter`, `sampleInsert`, `samplePushPrevious`), 680-778 (`statPush`), 884+ (final sample retrieval).
- Native baseline run: `go test . -run '^(TestAnalyzeStat4StoresIndexSampleRecords|TestAnalyzeStat4DuplicateMultiColumnSamples|TestAnalyze5Stat4SampleContracts)$' -count=1` passed.
- Discriminator failure: `go test . -run '^TestAnalyzeStat4InterleavedMultiColumnOracleSamples$' -count=1` failed. Candidate records/stat tuples diverge beginning sample 7; some rows' selected sample BLOB is from different index position than tuple's synthesized prefix counters.
- `stat4.go` now ports source-shaped running current counters and `aBest` snapshots; selected samples retain complete `anEq/anLt/anDLt`, encoded record, column reason, periodic flag, and hash. Candidate copies isolate zeroed `anEq` arrays from current/best state.
- Root mismatch: prior Go insertion logic rejected a periodic candidate unless it outranked the current minimum. SQLite calls `sampleInsert` unconditionally for periodic samples and evicts the least desirable non-periodic sample when full; only non-periodic samples are prefiltered in `samplePushPrevious`.
- Minimized source-built oracle fixture saved as `testdata/stat4_interleaved.sql`; oracle run `/tmp/sqlite-stat4 :memory: < /tmp/stat4_interleaved.sql` emits the exact 24 expected rows pinned by native test. Deterministic insert ordering uses `ORDER BY a.x,b.y`.
- Native forced-index `ORDER BY a,b,rowid` assertion verifies key-plus-rowid suffix order; full sample-record output agrees with oracle. Source-built SQLite source-id is `2025-11-04 ... alt1`; compile options include `ENABLE_STAT4` and `THREADSAFE=1`.
- Recreated analyze5-1.0 data deterministically from `../sqlite/test/analyze5.test:43-69`; `/tmp/sqlite-stat4` output confirms `{t1t 8, t1u 8, t1v 8, t1w 8, t1x 8, t1y 9, t1z 8}` and t1y final tuple `26 1|974 975|1 975|0309011B`. Native `TestAnalyze5Stat4SampleContracts` asserts same STAT4 row counts, labels, and t1y stats/sample record.
## Ordered implementation plan
1. Add deterministic minimized SQL fixture and pin complete source-built SQLite 3.51.0 STAT4 output, including each encoded sample record and index rowid suffix. ✅
2. Port `analyze.c` accumulator state (`current`, `aBest`, periodic samples, prefix transitions, final candidate counters) and fix observed periodic admission/eviction discrepancy. ✅
3. Native verification: exact interleaved 24 samples, duplicate-index samples, analyze5 STAT4 contract, SQL-visible key-plus-rowid order. ✅
4. Remove temporary diagnostics; record durable SQLite source invariant. ✅
5. Run focused tests, SOLID, `git diff --check`, and recorded verify command. ✅ Focused four-test suite, SOLID, recorded verify command, and diff check all passed. Source oracle `/tmp/sqlite-stat4 --version` reports SQLite 3.51.0; minimized fixture emits exactly 24 rows. Changed-file quality gate complexity/file-size checks pass; staticcheck reports unrelated `internal/execquery/select_join_part2.go:237` U1000 (`rightDefBaseNames`).

## Files in scope
- `internal/exec/pragma_analyze.go`
- `internal/exec/stat4.go`
- `internal/exec/stat4_format.go`
- `frigolite_analyze_stat4_sample_test.go`
- `testdata/stat4_interleaved.sql`
- `portplan/STAT4_IMPLEMENTATION_PLAN.md`
- `.agents/lessons_learned.md`
