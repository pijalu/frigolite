# Full-suite timeout triage

## Evidence
- Fresh serialized run `/tmp/frigolite-after-func-wrapper.jsonl` exited 1 at the configured `go test -timeout 30m`; root package elapsed 1801.272s, output states `temptable2-9.1.11.5` had run 26m46s.
- Timeout goroutine stack is captured in JSON around lines 323500+: `execInsertSelect` → `insertSelectGrowthGuard` (`internal/execdml/insert_conflict_scan.go:389`) → `EstimateFreeSpace` → `freeSpaceWalk` (`internal/schema/schema_tail.go:292`, current page 0x442).
- Test SQL (`testdata/temptable2.json:876-881`) inserts 500 rows of two 100-byte random blobs after delete inside transaction. Previous full profile `/tmp/frigolite-post-repair-profile-final.jsonl` passed this same leaf in 0.01s. Thus likely workload-dependent scan cost/state, not deterministic SQL semantic hang.
- Root cause confirmed: `freeSpaceWalk` accepted a `seen` map but only initial roots were recorded; recursive child pages were never marked or skipped. Added early skip+mark when popping each page, preventing cyclic/shared child references from causing unbounded repeated reads. A synthesized self-cycle regression RED-failed at 100ms before the fix and passes after it.
- Post-fix isolated full `temptable2` family terminates in 3.43s (previous full run stalled >26m at 9.1.11.5). Fresh full serialized profile `/tmp/frigolite-after-freewalk.jsonl`: root elapsed 351.084s, exit 1; 1,996 failing leaves across 348 fixture families. Timeout is resolved. Remaining temptable2 leaves are `4.1.1` page-count mismatch and `9.1.12.1` / `9.1.15.1` incremental-vacuum errors; prior 26m INSERT timeout absent. `func-4.39` also remains passing.

## Ordered plan
1. Audit source/setup and current failed timeout stack; inspect reachability/page-count state, and reproduce the same ordered `temptable2` file without unrelated suite concurrency.
2. Identify whether repeated schema free-space scans or page/root reachability complexity dominates; use profiling/temporary instrumentation only after baseline reproduction.
3. Design the smallest correct fix preserving corruption-growth guard semantics (source contract: corrupt table roots must only be validated when write grows database); add regression covering the expensive workload plus corrupted-root control.
4. Run exact fixture family and focused guard tests, then fresh serialized full profile with elapsed time/failure counts.

## Prohibited
Do not weaken/delete the guard or bypass corruption validation based on table name or fixture. No overlapping suite runs; retain all evidence.
