# PLAN S24 — the candidate-choice class (whereH 5, where2 12.1/16.2)

Agent unit brief. Dedicated git worktree off the current feature/perf
head (the repo's go test invocations race builds — never share a tree).
Rebase over S23's merge if it landed first; the file sets only overlap at
explain_plan.go.

## whereH (5) — the "proper superset" candidate choice

`SELECT d FROM t1 WHERE a=? AND b=? AND c>=? ORDER BY c` with indexes
t1bc(b,c) and t1abc(a,b,c): C plans **t1abc** — the index whose prefix
consumes MORE equality terms (a=?, b=? then the c range on column 3
delivers ORDER BY c). Ours plans t1bc (b=? + the c range) — our
bestIndexForRowCount/seekableRefs path never elevated t1abc.

C's rule (whereLoopAddBtree + wherePathSolver): every candidate index
yields a loop priced by its (nEq equality prefix + one range); the
ORDER-BY-delivering loops save the sorter; the cheapest loop wins. The
2-equality prefix undercuts the 1-equality one on BOTH the row estimate
and the sort saving — the H family pins exactly this superset choice.
Read whereH.test's header comment: "one candidate index covers a proper
superset of the WHERE clause terms of another".

Suspects: collectIndexedRefs assigns each term ONE best index — a=? may
never join the t1abc candidate group (check seekableRefOrder's per-index
prefix grouping: the a=? ref must join t1abc's group and extend its
prefix to [a,b] + the c range).

## where2-12.1 / 16.2 — the EQP join-order/shape pins

- 12.1: `CREATE TABLE t12(x INTEGER PRIMARY KEY, y INT, z CHAR(100));
  CREATE INDEX t12y ON t12(y); EXPLAIN QUERY PLAN SELECT ...` — want the
  plan to render SEARCH b twice (read the source:
  ../sqlite/test/where2.test "Ticket #1807" region and the fixture's
  want pattern) — ours renders an IPK seek where C renders index seeks.
- 16.2: `SELECT * FROM t1, t2 CROSS JOIN t3, t4 WHERE ...` — the join
  ORDER pin: C keeps t4 OUT of the first two positions (the
  "want regex NOT to match: .* t4 .* t[12] .*"). Ours puts t4 first.
  CROSS JOIN constrains the order (t1,t2 before t3; t4 unconstrained —
  C's planner picks by cost; ours differs). Read where2-16.x's source
  block for the exact query and where.c's CROSS JOIN handling
  (JT_CROSS + the reorder constraints).

## Verify

- The 3.54 oracle EQP for every shape (CREATE the exact schema, ANALYZE
  off unless the source does).
- Family name-set diffs (python, never comm); the where2/whereH families
  must be strictly better, zero new anywhere you touch.
- Records: append your unit record to .agents/lessons_learned.md; do not
  edit TODO.md; keep this file's status lines current.

## STATUS (S24 execution) — COMPLETE

- Result: whereH 5 -> 0, where2-16.2 fixed (12.1 carded below);
  where* name-set diff vs the base commit: exactly those 6 fixed, ZERO
  new; orderby/index/analyze/intpkey/select/join/limit/like all 0 new /
  0 fixed; TestS22_ + TestSOLID_ green. Commit on branch s24-choose.
- Files touched: internal/execquery/select_order_loopcost.go (NEW),
  internal/execquery/explain_join_order.go (NEW — the join-order EQP
  block moved out of explain_plan.go for the size budget),
  internal/execquery/select_order_search.go (the elevation hook + the
  firstOrderServingSwap split + the rowid-emission block moved to
  select_order_emit.go), internal/execquery/select_order_emit.go,
  internal/execquery/explain_plan.go, this file,
  .agents/lessons_learned.md.
- Baselines (own worktree, 39d6bba9d): whereH = {2.1, 3.1, 4.1, 5.1, 6.1}
  (1.1/7.1/8.1 pass by creation-order luck — the first-created index wins
  the per-term assignment); where2 = {2.5, 2.5b, 12.1, 16.2} (2.4 flake
  did not fire).
- DIAGNOSIS whereH (debug-shell wheretrace, /tmp/s24oracle/sqlite3dbg,
  3.51 + probes vs /usr/bin/sqlite3 3.54): C decides the superset choice
  in whereLoopInsert's subset-cost adjustment
  (whereLoopCheaperProperSubset, where.c:2649-2720): a loop whose
  constraint set is a proper SUPERSET of an existing loop's is forced
  strictly cheaper (rRun=min, nOut=min(existing-1)); a proper subset is
  forced strictly costlier. Verified load-bearing: whereH-1's t1abc loop
  ends at (rRun 50, nOut 11) vs t1bc (51, 12) — raw ladder arithmetic
  TIES at nOut 12 and the subset rule breaks it. Probe order = the index
  chain = REVERSE creation (each CREATE INDEX prepends).
- C pricing chain (no stat1): default ladder build.c sqlite3DefaultRowEst
  aVal={33,32,30,28,26,23...}, a[0]=max(nRowLogEst,99), nRowLogEst=200
  (stat1 absent); nOut(nEq eq terms) = ladder[nEq]; one range bound:
  whereRangeScanEst −20 per bound (truthProb defaults to 1,
  whereexpr.c:83), −20 more for both bounds, nOut -= #bounds, floor 10;
  whereLoopOutputAdjust (gated: single table AND ORDER BY): −1 per
  unconsumed single-table term, iReduce cap = 10 (unused `col = int in
  [-1,1]`) / 20 (else) against ladder[0]; rRun = LogEstAdd(estLog(ladder[0]),
  nOut+1+(15*szIdxRow)/szTabRow) [+ LogEstAdd(..., nOut+16) when not
  covering]; szIdxRow/szTabRow are LogEst(w*4) (build.c:2229/2244).
  Solver pick: rRun, then nOut; ties keep the incumbent.
- DIAGNOSIS where2-16.2 (probed 3.54 CLI + confirmed against fetched
  3.54 where.c:4960-5017): the mPrereq rule CHANGED after 3.51 —
  `hasRightCrossJoin` (set by JT_CROSS or JT_LTORJ) suppresses the
  `mPrereq = 0` reset for ALL later FROM terms, so a comma-joined term
  after a CROSS edge inherits the CROSS term's prereq (probes A/J/K/L:
  `t1, t2 CROSS JOIN t3, t4` gives t3 AND t4 prereq {t1,t2}; LEFT edges
  still reset — probe H/M). Q1's order t1,t2,t4,t3 and Q2's t2,t1,t3,t4
  both follow from prereq gating + cost/tie order. Our planJoin has NO
  prereq machinery: driver=t4 (const pred) in Q1, order t2,t3,t4,t1 in
  Q2.
- CARD where2-12.1: the contract is C's multi-index-OR loop as table b's
  JOIN loop (whereLoopAddOr inside a join: OR arms b.x=rowid-seek +
  b.y=t12y-seek, prereq 0, then a searched inner via t12y(y=b.x)) —
  needs OR loops modeled as join-loop candidates + the MULTI-INDEX OR
  EQP block. Overlaps S23's active join machinery (select_join_scan.go)
  and the S21 OR-plan executor; NOT attempted this unit — carded with
  this diagnosis for a dedicated unit after the S23 merge settles.
- PLAN (concrete):
  1. NEW internal/execquery/select_order_loopcost.go —
     orderLoopPrice(t, s, token, conjuncts): prices one secondary-index
     seek loop (eq prefix via indexEqPrefixLen's primitives, optional
     range at position nEq, ladder from stat1Tokens/defaults, output
     adjust with the iReduce cap, rRun with szIdxRow/szTabRow via
     wherecost.go helpers, covering via indexCoversQuery) returning
     (rRun, nOut, consumed-conjunct-set); properLoopSubset(x, y) porting
     whereLoopCheaperProperSubset case 2 (term-set containment, the (2e)
     covering guard; case 1 subsumed by term sets); adjustLoopSubset
     running whereLoopAdjustCost + whereLoopFindLesser semantics over a
     probe-ordered list (reverse creation = GetEntries reversed).
  2. select_order_search.go preferOrderByServingIndex: the
     chosen-delivers path (currently an early return) first runs the
     elevation: price the chosen + every ordered-index candidate
     (nEq>=1), apply the subset adjustment in C's probe order, and swap
     to the winning delivering loop when it strictly undercuts the
     chosen on (rRun, nOut). The chosen-not-delivering path keeps its
     existing tranche rule untouched. Both EQP and the seek executor
     flow through scanLoopForRowCount, so plan and execution stay
     coherent.
  3. explain_plan.go planJoin: compute per-FROM-term prereq masks with
     the 3.54 rule (CROSS |= mPrior + latches hasRightCrossJoin; OUTER
     |= mPrior; reset only while no CROSS seen); joinDriver skips
     prereq!=0 tables (both the const-pred and leastJoinRefs paths);
     nextJoinTable skips candidates whose prereq is not covered by the
     planned set (planJoinRemaining tracks planned indexes).
  4. Verify: whereH/where2 sweeps (name-set diffs vs the baselines
     above), TestS22_ pins, the where*/orderby*/index*/analyze*/
     bestindex*/intpkey neighborhood, oracle EQP re-checks.
- Files touched (for the coordinator's merge with S23):
  internal/execquery/select_order_loopcost.go (NEW),
  internal/execquery/select_order_search.go,
  internal/execquery/explain_plan.go, portplan/PLAN_S24_candidate_choice.md,
  .agents/lessons_learned.md.
