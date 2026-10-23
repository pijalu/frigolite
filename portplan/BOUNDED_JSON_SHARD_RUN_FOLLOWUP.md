# Bounded SQLite JSON shard run follow-up

## Status

The four-shard run at `/tmp/frigolite-bounded-shards-20261023-210357` did not complete within the approved 10-minute ceiling. It was still processing shard 2's `walfault` family after 10 minutes; that workload was terminated. Do not claim corpus coverage.

Captured evidence from logs:

| Shard | Observed families started | Completed family outcomes | Failures | Outcome |
|---|---:|---:|---:|---|
| 0 | 206 | 206 | 74 | failed; `FAIL` log footer |
| 1 | 209 | 209 | 74 | failed; `FAIL` log footer |
| 2 | 191 | 0 visible completed family outcomes | 0 | incomplete; terminated while executing `walfault` |
| 3 | 209 | 209 | 85 | failed; `FAIL` log footer |

The process driver used in this attempt ran under zsh despite Bash array syntax; consequently shard status sidecars were not produced. Preserve existing logs as evidence; rerun only with the corrected documented Bash runner, which waits every child and writes each collected exit status.

## Updated allocator and rerun evidence

Runtime-aware deterministic greedy allocation landed in `frigolite_harness_test.go`.
The runtime sample is deliberately partial: 15 complete family-level timings seeded
from shard 0/1/3 logs (largest: `rtreeH` 79.18s, `trans2` 51.97s, `trans` 37.05s,
`avtrans` 32.21s); shard 2 had no completed-family summaries. Unmeasured families,
including its interrupted work, retain the fixture-byte + 128-bytes/case fallback.
Existing pre-assignment eligibility filtering and basename grouping remain intact.

Focused verification passed: `go test . -run '^TestHarnessShard(Config|Assignments)$' -count=1`.
The Bash runner was tested with three fake children (statuses `0, 7, 0`); runner
returned 1 and persisted every status. An attempted new four-shard run was started
through that runner, but the command hit the tool's 300-second limit before the
approved 600-second bound. Logs remain at `/tmp/frigolite-shards.DDyvoN/`; observed
started-subtest counts were shard 0: 102,960; shard 1: 7,786; shard 2: 81,982;
shard 3: 792. No family summaries or status sidecars were recorded before timeout,
so this run establishes no completion coverage. Actual PIDs were no longer running
when checked. The 10-minute run remains unverified/infeasible in this execution.

Concrete follow-up: rerun using the Bash runner in an environment that permits a
single uninterrupted 600-second command, then preserve all `.status` files and
family summaries. Current runtime seed covers only 15 / 624 complete observations;
extend timing evidence before relying on expected balance. No claim of corpus
completion or all-family success.

Earlier narrow fixture run `FRIGOLITE_TEST=fts3expr ...` failed in existing
`fts3expr3` cases (`fts3_exprtest: the expression must be text`), demonstrating
current corpus is not entirely green independently of shard allocation.
