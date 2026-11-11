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
| S23 JOIN | /private/tmp/wt_s23 | s23-join | where8 16, where9 8, where 6.15+14.x — the join inner loops (index-driven/covering), FULLSCAN_STEP table-cursor gating, per-loop ORDER BY delivery | running |
| S25 LIMIT | /private/tmp/wt_limit | s25-limit | wherelimit2 4 — DELETE/UPDATE … ORDER BY LIMIT, INSTEAD OF trigger + log UDF runtime vars | resumed, near done |
| expr triage | /private/tmp/wt_triage | s27-expr-triage | read-only census doc portplan/TRIAGE_expr_2026-11-12.md | resumed |
| pager1 triage | /private/tmp/wt_triage2 | s28-pager1-triage | read-only census doc portplan/TRIAGE_pager1_2026-11-12.md | resumed |
| S24 CHOOSE | (queued) | s24-choose | whereH 5 + where2 12.1/16.2 — brief: portplan/PLAN_S24_candidate_choice.md | waiting for a free concurrency slot |

Concurrency lesson: launching a 5th concurrent agent while a full gate
runs trips the user concurrency limit (it killed all three first-wave
agents once already). **Keep ≤4 agents; hold S24 until S25 lands.**

## Merge protocol (order matters — file overlap is explain_plan.go/select_order_search.go)

1. S23 JOIN merges first (`git merge s23-join` on feature/perf), then
   the FULL gate (go test -run ^TestSQLiteSuite$ -count=1 -v -timeout
   40m) + name-set diff vs /tmp/full_s22_gate3_fails.txt (python sets,
   never comm).
2. S24 CHOOSE rebases over the S23 merge, then merges + one full gate.
3. S25 LIMIT rebases over both, merges + one full gate.
4. Each merge: append-only lessons consolidation, TODO.md census
   update, plan-file status lines.
5. S26 MISC (where 19.0 EQP autoindex names + 25.x corruption) runs in
   the MAIN tree AFTER the merges — its territory (explain EQP
   rendering) overlaps S23/S24.

## Baselines

- /tmp/full_s22_gate3_fails.txt — the current full-suite failure name
  set (1433, TestSQLiteSuite/-prefixed).
- /tmp/w_s22_f.txt — the where-census leaf set (47).
- /tmp/full_s21_fails.txt — the S21 pre state (1466).

## After the where classes close

expr 109 → pager1 101 (the triage docs guide the slices) → the WAL
residue classes (HANDOFF_2026-11-04_WAL_RESIDUE.md) → the carded
converter classes → the performance phase (F1c/F3 lanes in
GAP_CLOSURE_PLAN_2026-10-31.md).
