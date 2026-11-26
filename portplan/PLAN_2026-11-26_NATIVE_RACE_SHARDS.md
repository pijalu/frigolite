# CI native race step sharding (2026-11-26)

## Problem

Push run `45828442878` stayed in `Race tests (non-harness)` past 36m.
The step runs one `go test -race` over every package; locally the root package
alone takes ~21m under -race, so a slower CI runner exceeds the 30m go-test
timeout. No step conclusion was available because GitHub keeps job logs only
after completion.

## Fix

Replace the single race command with `tools/run_native_race_shards.sh`:
six deterministic `go test -timeout 30m -race -count=1 -run <regex> ./...`
invocations covering every `Test[A-Z]*` function exactly once:

- `^Test[A-F]`
- `^Test[G-M]`
- `^Test[N-O]`
- `^TestP`
- `^Test[Q-T]`
- `^Test[U-Z]`

`FRIGOLITE_TEST=__none__` keeps the JSON harness out of these shards; the
harness has its own 12-shard race step.

The earlier draft used `^Test[A-BD-F]`, which silently dropped every `TestC*`
test. The corrected regex includes C; the partition check below is across
`go test -list '^Test' ./...`, not just the root package.

## Verification

- Partition check over all package test lists: 2,001 `Test*` functions,
  unmatched_or_duplicate = 0; shard counts 530/179/170/525/416/181.
- `bash -n tools/run_native_race_shards.sh`.
- Full non-harness race already passed locally after the WAL fix. The newly
  included C group was probed with `FRIGOLITE_TEST=__none__ go test
  -timeout 10m -race -count=1 -run '^TestC' ./...`: exit 0 both with the normal
  PATH and with sqlite3 removed from PATH. No `TestCompat*` functions exist;
  C tests that need the oracle use `exec.LookPath` and skip when it is absent.
- A local `-race -count=1` execution of each shard is required before relying
  on CI as the sole gate.
- Push and confirm CI/CD quality-and-test reaches completion.
