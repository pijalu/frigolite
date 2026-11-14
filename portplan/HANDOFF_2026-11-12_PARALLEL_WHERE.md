# HANDOFF 2026-11-12 — the parallel-agent where phase

Restart index for the parallel-agent units. **TODO.md remains the live
queue**; this doc carries the fleet state and the merge protocol.

## Landed

- **S22 — 73a643fe4** (feature/perf): the seek/order cost model. Gate
  VERIFIED: 33 fixed / 0 regressions vs /tmp/full_s21_fails.txt (1466 →
  1433 names, /tmp/full_s22_gate3_fails.txt). Where census 78 → 47:
  where 10, where2 4, where8 16, where9 8, whereH 5, wherelimit2 4
  (/tmp/w_s22_f.txt).

## The fleet (2026-11-12, all in dedicated worktrees — NEVER share a tree)

| Unit | Worktree | Branch | Scope | State |
|------|----------|--------|-------|-------|
| S23 JOIN | /private/tmp/wt_s23 | s23-join | join inner loops, FULLSCAN_STEP gating, per-loop ORDER BY | **S23.1+S23.2 MERGED** (510efa6b8; census 47→40 per the branch's frame); continuation agent running for where 6.15/14.x + residue (fresh agent — the pause expired the old task; /private/tmp was purged, uncommitted work lost, rebuild worktrees from branch refs) |
| S24 CHOOSE | /private/tmp/wt_s24 | s24-choose | whereH 5 + where2 12.1/16.2 — candidate choice | worktree rebuilt at 39d6bba9d; agent relaunch QUEUED (hold until the S23 merge gate finishes — ≤2 agents + gate) |
| S29 expr unit | /private/tmp/wt_expr | s29-expr-unit | TRIAGE_expr slices E3+E1+E2 (46) then S1+S2 (9) | running (fresh agent) |
| S25 LIMIT | — | s25-limit | wherelimit2 4 | **DONE + MERGED 3e4edb26b** (merge gate: 5 fixed incl. selectH-3.7 bonus, 2 flakes, 0 true regressions) |
| expr triage | — | s27-expr-triage | read-only census | **DONE + MERGED bc3f3d810** (expr truth = 3 leaves; 98-leaf substring table) |
| pager1 triage | — | s28-pager1-triage | read-only census | **DONE + MERGED 904efda02** (99 leaves, 7 buckets, slices U1→U7) |

S25 merge-gate baseline: /tmp/full_s25_merge_fails.txt (1430 names).
Flake watchlist (lessons): temptable2-4.1.1, where2-2.4 (probabilistic
random()-distinctness; 6/6 isolated green; chase only if it recurs).

Concurrency lesson: the user concurrency limit killed 4 agent runs at
peak (3 agents + gate + main). **Keep ≤3 agents while a gate runs, ≤4
without one; resume killed agents via SendMessage (their context
survives).**

## Merge protocol (order matters — file overlap is explain_plan.go/select_order_search.go)

1. ~~S25 LIMIT~~ **DONE** (merged 3e4edb26b; gate verified 5 fixed/2
   flakes/0 regressions vs /tmp/full_s22_gate3_fails.txt).
2. S23 JOIN merges next (`git merge s23-join` on feature/perf), then
   the FULL gate (-v -timeout 40m) + name-set diff vs
   /tmp/full_s25_merge_fails.txt (python sets, never comm).
3. S24 CHOOSE rebases over the S23 merge, then merges + one full gate.
4. S29 expr-unit merges last (disjoint territory; still gets a gate).
5. Each merge: append-only lessons consolidation, TODO.md census
   update, plan-file status lines.
6. S26 MISC (where 19.0 EQP autoindex names + 25.x corruption) runs in
   the MAIN tree AFTER the S23/S24 merges — its territory (explain EQP
   rendering) overlaps them.

## Baselines

- /tmp/full_s25_merge_fails.txt — the current full-suite failure name
  set (1430, TestSQLiteSuite/-prefixed) — DIFF TARGET for the next
  gates.
- /tmp/full_s22_gate3_fails.txt — the S22 gate set (1433).
- /tmp/full_s21_fails.txt — the S21 pre state (1466).
- /tmp/w_s22_f.txt — STALE (62 lines, a mid-session snapshot) — ignore.

## After the where classes close

expr 109 → pager1 101 (the triage docs guide the slices) → the WAL
residue classes (HANDOFF_2026-11-04_WAL_RESIDUE.md) → the carded
converter classes → the performance phase (F1c/F3 lanes in
GAP_CLOSURE_PLAN_2026-10-31.md).
