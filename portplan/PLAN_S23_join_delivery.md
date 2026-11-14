# PLAN S23 — the join-loop machinery (where8 16, where9 8, where 14.x + 6.15)

Agent unit brief. Work in a DEDICATED git worktree branching from the S22
commit (the repo's go test invocations race each other's builds — never
share a tree with another agent). Update the Restart column of this file
as you land pieces.

## The diagnosis (verified 2026-11-11 with traces + the 3.54 oracle)

- **where8-3.x (16)**: `SELECT a, d FROM t1, t2 WHERE (a = 2 OR a = 3)
  AND d = 6` — the harness appends `db status step` (SQLITE_STMTSTATUS_
  FULLSCAN_STEP). Want step=0: C's plan drives t1 via the multi-index OR
  (i1 on a) and t2 via an INDEX seek, and FULLSCAN_STEP only counts
  OP_Next of TABLE cursors (wherecode.c sets OPFLAG_FULLSCAN_STEP for a
  loop with no constraint on the table — an INDEX scan never counts).
  Ours: the inner loop full-scans t2 → step=9.
- **where9-2.x (8)**: `SELECT t2.a FROM t1, t2 WHERE t1.a=80` — want
  "scan 0": C's inner t2 loop scans the COVERING index (the projection
  rides the index), never the table. Ours full-scans t2 (98).
- **where 6.15**: `SELECT t3.a, t1.x FROM t3, t1 WHERE t3.a=t1.w ORDER BY
  t3.a LIMIT 3` — nosort: the OUTER loop's index (t3a) delivers the
  ORDER BY; one term → obDone after loop 1.
- **where 14.1-14.7.2**: `SELECT x.a||'/'||y.a FROM t8 x, t8 y ORDER BY
  x.a, y.b` — per-loop delivery: loop 1 (x, WHERE_IPK rowid scan)
  consumes x.a; 14.3/14.4's second term x.b is marked off by the
  order-distinct rule (a unique leading term retires the outer table's
  remaining terms — where.c's "Mark off any other ORDER BY terms that
  reference pLoop"); 14.1/14.2's y.b needs the INNER loop to walk y's
  UNIQUE(b) index. **14.3/14.7.2 ALSO differ in row order**: C's inner
  loop plans the covering UNIQUE index (y.a rides the entry's rowid), so
  the rows arrive in b-index order per outer row — ours iterate the
  table.

## C contracts (read these first)

- where.c wherePathSatisfiesOrderBy (5024-5390): the per-loop walk — the
  equality-bound pre-pass, the index-column walk, the rowid tail, the
  order-distinct mark-off (port shape already exists single-table:
  walkIndexOrderTerms in select_order_search.go).
- wherecode.c: OPFLAG_FULLSCAN_STEP only for table loops; the inner-loop
  seek generation for each outer row.
- The FULLSCAN_STEP counter: internal/exec RecordFullScanSteps — find our
  table-scan call sites and gate them on the loop kind the way C does.

## Delivery shape

1. The executor's join inner loops must iterate via the loop
   scanLoopForQuery picks (an index seek / a covering walk), not a table
   scan, when the planner chose loopIndex — the single-table paths
   already do this; extend to the join nesting (and per-outer-row
   reseek).
2. The ORDER BY delivery gate for joins: walk the loops outer→inner
   (the C port above); when all terms are consumed, no sorter and the
   rows must EMERGE in that order (the loops must actually deliver it).
3. The counters: FULLSCAN_STEP for table loops only.

## Verify

- Baselines: /tmp/w_s22_f.txt (the 47-leaf where census), the family
  name-set diffs via python (never comm — macOS trap).
- The 3.54 oracle for every EQP/counter claim; ../sqlite is 3.51.0.
- Full gate after landing: -timeout 40m (the carded walfault-11 eats
  ~12 min), name-set diff vs /tmp/full_S15_fails.txt.
- Records: append your unit record to .agents/lessons_learned.md (NEVER
  edit other sections); TODO.md is the coordinator's — do not edit it;
  update this file's status lines instead.

## Restart / status (S23 execution log — update as pieces land)

- S23.1/S23.2 landed (f3ec10698, 7a3a28e6f). Fresh sweep 2026-11-14
  (this worktree, /tmp/s23_sweep_where.txt): census **40** leaves
  (where 10, where2 4, where8 15, where9 2, whereH 5, wherelimit2 4).
  S23.1/S23.2 took where8 16→15 (3.7, 3.13 green) and where9 8→2
  (4.x + 2.1-2.4 green; 2.6 still red). New arrivals: **where8-5.3 is a
  REGRESSION-turned-stale-fixture**: the S23.2 loop picks made the
  LEFT-JOIN-OR query SUCCEED (C-correct: oracle returns `1|`; the old
  engine errored, which silently matched the converter's mis-baked
  `expect: "1 {}"` read as rc-1+braced-message = error expectation).
  Hand-pin the fixture (query step) — engine is right.
- In-scope residue (23 leaves):
  - **sort-only (S23.4)**: where8-3.11/3.12/3.21/3.21.1/3.21.2/3.22
    (scan counters already right; ORDER BY t1.a delivered by the outer
    i1 range walk) + where-6.15 (rows right, sort 1→0).
  - **rows+sort (S23.4)**: where-14.3/14.4 (x.a unique retires x.b via
    the mark-off; inner y must walk UNIQUE(b) covering — y.a rides the
    entry rowid), where-14.7.2 (x outer via covering UNIQUE(b) walk,
    x.a rides the entry rowid; x.a||x.b retired by the mask rule).
    where-14.1/14.2 rows already right — sort only.
  - **OR→IN base seek (S23.3.1)**: where8-3.3/3.4/3.5/3.6 — scan 9→0,
    rows already right.
  - **OR-arm row source (S23.3.2)**: where8-3.8 (base t1 arms a=? i1,
    b=? i2 → counter 0, rows unchanged, sort stays 1), where8-3.9 (base
    t1 stays the PLAIN table scan — C declines the 3-arm OR for the
    rowid-ordered walk; inner t2 arms i3 d=?/i4 e=? per outer row →
    rows 9/4 9/9 → 9/9 9/4, sort 1→0 via the rowid walk),
    where9-2.5 (inner t2 arms per outer row, arm order).
  - **where9-2.6**: LEFT JOIN; base t1 WHERE has a 3-arm OR — want
    scan 0 (C: t1 via multi-index OR of the WHERE arms).
  - **S23.5 correlated subquery seek**: where8-3.14 (99→9), 3.15
    (909→9).
  - Out of scope stays: where-19.0/25.x, where2 4, whereH 5,
    wherelimit2 4 (S25 merge).
- Delivery order (plan-before-change per piece): P1 fixture hand-pin →
  P2 S23.4 join ORDER BY delivery (the wherePathSatisfiesOrderBy port +
  the inner covering-walk pick) → P3 S23.3 OR→IN + OR-arm row source →
  P4 where9-2.6 → P5 S23.5. Family sweep + TestS22_ pins after each.

### FINAL STATUS (2026-11-14, s23-join — all pieces landed)

- Commits: a85d3cd9b (P1 fixture), 31d9f980c (P2 delivery),
  18f554871 (P3a base OR), 7983f47a5 (P3b inner OR pass),
  2ad953337 (P3c 19.0 EQP), 9f3d07419 (P5 correlated seek).
- Census: 40 → 15 (where 10→3, where2 4→3, where8 15→0, where9 2→0,
  whereH 5 unchanged, wherelimit2 4 unchanged — S25 merges).
  where-19.0 folded in per the coordinator's scope addition; where2-12.1
  fixed as a side effect (the OR EQP shape). Remaining:
  where-25.1/25.2/25.5 (corruption/error class), where2-16.2/2.5/2.5b
  (candidate-choice), whereH 5 (EQP autoindex names), wherelimit2 4
  (S25 merge) — all carded out of this unit.
- Regression sweeps: the where families + the join families + the
  select/index/order/group/distinct/having/limit canaries diffed against
  the a85d3cd9b baseline in a DEDICATED baseline worktree — 169 → 169
  leaf-identical. TestS22_ pins + TestSOLID_ green after every piece.
- C contracts landed (see the lessons record): the backward
  sqlite3WhereExprAnalyze + the commuted-copy arm ordering, the CASE-1
  same-column IN rewrite, the correlated-bound seek (the orientation-aware
  seekRefSides + the operand scope), and the real-name schema resolution
  for aliased FROM terms.

### P2 concrete design (verified against probes + the 3.54 oracle, 2026-11-14)

- **Probes established**: where8-3.12's base already runs the i1 walk
  (rows in a-order with reversed inserts; the join EQP renderer just
  prints "SCAN t1"); where-14.7.2's base already runs the b walk;
  where-14.1/14.2 rows already right; where-14.3/14.4's wrong rows =
  the y materialization's table order vs C's covering autoindex walk;
  where-6.15's swap (shouldSwapCommaJoin fires: indexed side inner)
  destroys the t3 base walk's order — C keeps t3 outer BECAUSE its walk
  delivers the ORDER BY. Our unordered `SELECT a FROM t` covering-walk
  gap is pre-existing and out of scope.
- **New file internal/execquery/select_join_order.go**:
  - `joinDeliveryLoop{token, real, kind, idx, nEq, oneRow}` — one loop;
    built for the base from joinBaseScanRows's actual pick (RECORDED on
    the engine, save/restored like currentScanTable) and per join
    operand (real plain-table only; subquery/view/vtab operands are
    non-deliverable → the analysis dies at that loop unless obDone).
  - `joinOrderTerm{col, tableToken, desc, collate, nullsFirst/Last,
    usageTables, sat}` — expression terms carry usage masks; column
    terms carry their table token.
  - `joinOrderByDelivered(s)` — the wherePathSatisfiesOrderBy port:
    per loop the eq pre-pass (conjuncts usable at this level: refs ⊆
    positioned tables; X IS NULL / IS / eq with agreeing collation),
    the one-row skip, the index-column walk (reuse walkColumnStep/
    matchWalkColumn per loop: nEq skip, the rowid tail, direction per
    loop, the nullable-unconstrained void + the rowid-match restore,
    the j==0||j<nKeyCol break rule), and the order-distinct mark-off
    (mask |= loop token; terms whose usage ⊆ mask marked; constants
    marked). Full delivery only when obSat==obDone. OR-loop bases and
    non-real-table inners void the claim. LEFT/RIGHT/FULL joins void
    the claim (targets are all comma/inner).
  - `joinInnerDeliveryPick` — per real-table inner operand: remaining
    ORDER BY terms referencing it → the ORDER-BY-serving covering walk
    (first covering serving index, else first serving); else the cost
    covering walk (the statement's used columns on the operand ⊆ index
    cols ∪ rowid — C's colUsed covering rule; first covering index in
    schema order); else none (table walk = rowid order — already
    faithful).
- **Wiring**: execSelectPostScan computes the claim + picks BEFORE
  execJoins (the swap decline in runJoinNestedLoop reads it: a
  delivered claim owns the loop order — where-6.15 keeps t3 outer).
  materializeTableJoin permutes the rightMaps into the pick's index
  stored rowid order (indexStoredRowidOrder + indexOrderPermutation,
  reversed for a backward pick — 14.2's y.b DESC). The final gate: the
  emitIndexOrderedRows join branch returns true when the claim held
  (rows are already in the delivered order; no permutation, no
  sorter). Base walk directions must be forward (a backward base walk
  claim is declined — sorter owns it; carded).
- **Counters**: none of P2 touches counters (the sort marker only).
- **S23.3 amendment** (probes): 3.3/3.4/3.5/3.6 are covered by routing
  the join BASE through the existing OR-branch machinery (planOrIndex-
  Scan on the FROM-only view; every arm must find an index — 3.9's
  c-arm unindexed declines exactly like C's no-OR-loop rule). C's
  same-column-arm IN rewrite (exprAnalyzeOrTerm) only differs from
  branch order when the arm order is not value-sorted — not observable
  in the targets; carded nuance. The INNER OR-arm row source is a
  per-outer-row arm-order pass over the materialized right maps (the
  arm exprs evaluated on the combined maps, dedup by right rowid per
  outer row — C's RowSetTest), gated on every arm constraining an
  indexed right-operand column (LEFT JOIN included — where9-2.5/2.6).

### Diagnosis (verified against the 3.54 CLI oracle + fixture expects)

| leaves | got → want | the C plan (EQP 3.54) | fix |
|---|---|---|---|
| where9-4.2 | scan 98 → 0 | SEARCH t1 USING INDEX t1b (b>?) + TEMP B-TREE | S23.1 |
| where9-4.6 | scan 0 → 98 | NOT INDEXED → full table scan | S23.1 |
| where9-2.1-2.4,2.6 | scan 98 → 0 | t1 rowid=80 seek; t2 MULTI-INDEX OR (t2d d=?, t2c c=?) | S23.2 (base PK seek; inner contributes 0) |
| where9-2.5 | rows (80,2)(80,28)(80,54)(80,80) → (80,80)(80,2)(80,28)(80,54), scan 0 | t2 OR arms per outer row in arm order | S23.3 |
| where8-3.3/3.4 | scan 9 → 0 | t2 outer covering i3 (d=?); t1 inner covering i1 (a=?) — the (a=2 OR a=3) OR→IN | S23.3 (OR→IN seek) |
| where8-3.5/3.6 | scan 9 → 0 | t1 inner i1 (a IN 2,3); t2 OR arms | S23.3 |
| where8-3.7 | scan 9 → 0 | t1 inner i1 (a=2); t2 OR arms | S23.2 |
| where8-3.8 | scan 9 → 0, sort stays 1 | t1 MULTI-INDEX OR (i1 a=?, i2 b=?); t2 OR arms; TEMP B-TREE | S23.3 |
| where8-3.9 | rows (9,4)(9,9) → (9,9)(9,4); sort 1 → 0 | SCAN t1 (9 steps); t2 OR arms (i3 d=? then i4 e=?) per outer row; ORDER BY t1.rowid delivered by the table scan | S23.3 (inner arm order) + S23.4 (rowid nosort) |
| where8-3.11/3.21/3.21.1/3.21.2/3.22 | scan 9 → 0, sort 1 → 0 | SEARCH t1 USING INDEX i1 (a<?) — delivers ORDER BY a; t2 MULTI-INDEX OR | S23.2 + S23.4 |
| where8-3.12/3.13 | scan 9 (coincidence) sort 1 → 0 | SCAN t1 USING INDEX i1 — the UNCONSTRAINED index walk (p5 set: it counts 9); +a<5 not a constraint; i1 delivers ORDER BY a | S23.2 (unconstrained walk, records n-1) + S23.4 |
| where8-3.14 | scan 99 → 9 | SCAN t1; the correlated subquery seeks t2 via i4 (e=b) — ours full-scans t2 per outer row (+90) | S23.5 |
| where8-3.15 | scan 909 → 9 | SCAN t1; subquery `t2.d>inner.d` range-seeks via i3(t2.d)? per outer row | S23.5 |
| where-6.15 | sort → nosort | t3 outer via its a-index walk (delivers ORDER BY t3.a), LIMIT 3 | S23.2 + S23.4 |
| where-14.1 | sort → nosort | x outer rowid walk (x.a consumed); y inner covering UNIQUE(b) walk (y.b consumed) | S23.2/S23.3 inner walk + S23.4 |
| where-14.2 | sort → nosort | same, y's b walk REVERSED (y.b DESC) | S23.4 |
| where-14.3/14.4 | rows 1/1 1/4 4/1 4/4 → 1/4 1/1 4/4 4/1; sort → nosort | x.a unique → order-distinct mark-off retires x.b (x loop alone consumes x.a+x.b); y unconstrained → the COST pick = covering b walk (y.a rides the entry rowid) | S23.4 (mark-off) + inner covering pick |
| where-14.7.2 | rows wrong + sort | x outer covering UNIQUE(b) walk (x.b, then x.a = the rowid tail; x.a‖x.b marked off by the order-distinct mask rule); y covering b walk | S23.2/S23.4 |

C contracts pinned (wherecode.c/where.c 3.51.0, vdbe.c):
- OP_Next/OP_Prev increment aCounter[p5]; p5 = SQLITE_STMTSTATUS_
  FULLSCAN_STEP is set (a) for Case 6 (no usable index → full TABLE scan),
  always; (b) for the INDEX-loop case when `(pLoop->wsFlags &
  WHERE_CONSTRAINT)==0` — an UNCONSTRAINED index walk ALSO counts
  (where8-3.12's scan 9 = the i1 walk). Constrained eq/range/IN/OR-arm
  loops never count. OP_SeekRowid (rowid=?) never counts.
- wherePathSatisfiesOrderBy tail: "Mark off any other ORDER BY terms that
  reference pLoop": when isOrderDistinct, orderDistinctMask |= the loop's
  table; every unsatisfied ORDER BY term whose column-usage mask is a
  subset of the accumulated mask is marked off (expression terms included;
  a constant term (mTerm==0) is always marked). A unique leading term or a
  rowid match makes the loop order-distinct.
- whereexpr.c exprAnalyzeOrTerm: an OR whose arms are ALL WO_EQ on the
  SAME column of the SAME table becomes a virtual `col IN (v1..vn)` term
  (where8-3.3/3.4/3.5/3.6).

### Concrete delivery (plan-before-change, per piece; family diff after each)

**S23.1 — where9-4.x (single-table).** Files: select_index_seek.go
(shape gate + ref collection), select_exec.go (execSelectOrPlan),
rowid_seek.go, execdml/or.go.
1. NOT INDEXED: `s.From.IndexedBy` non-empty skips the OR-plan route
   (execSelectOrPlan) and the seek paths (indexSeekShapeApplies,
   selectRowidSeekRows) — 4.6 falls to the table scan → scan 98.
2. OR-declined fall-through: when the WHERE has a top-level OR conjunct
   and PlanOrIndexScan (or the IN expansion) declines, the seek paths may
   still run, planned from the NON-OR top-level conjuncts only:
   - splitAnd(s.Where) keeping conjuncts with no OR subtree →
     whereForSeek; empty → no seek.
   - indexSeekRefSet/indexSeekTerms/buildIndexSeekPlan plan against
     whereForSeek (thread a parameter; no statement mutation).
   - the residual stays the FULL s.Where (re-run per candidate —
     select_index_seek_exec.go:430), so the OR arm's rows are never lost.
   - the seek-vs-OR arbitration: orPlanApplicable(s) = PlanOrIndexScan ok
     (or the IN expansion plans) → the OR path keeps the statement; else
     the seek runs. Verify collectIndexedRefs is never fed the raw OR
     WHERE on this path.
3. Expect: where9 8→6.

**S23.2 — join BASE-scan loop picks.** New file
internal/execquery/select_join_scan.go; hook in execRealTableSelect's
scan dispatch (before e.scan.ScanTable) for len(s.Joins)>0 real tables.
1. joinTableOnlyConjuncts(s.Where, from alias/table, colDefs) → the
   FROM-only conjunct view (a conjunct is dropped when any column ref is
   another join operand's column; unqualified refs resolve by FROM-table
   column membership). This is C's per-loop constraint analysis (terms
   usable by loop 1 reference only already-positioned tables).
2. Plan the base loop from that view with the existing machinery
   (scanLoopForRowCount on a FROM-only statement copy): loopIPK oneRow →
   rowid seek; loopIndex+seek → index seek; loopNone → plain table scan
   (keep ScanTable + its recording).
3. Execution feeds execSelectJoins unchanged (rows+rowMaps+colDefs);
   residuals evaluate the FROM-only view (cross-table terms are
   re-checked by the join's full-WHERE filter).
4. Unconstrained index walk (loopIndex, seek=false — where8-3.12): walk
   the index btree (per entry: rowid + key values), fetch non-covered
   rows via the table btree (C's deferred seek), record
   RecordFullScanSteps(entries-1) + IncSearchCount; suppress the table
   scan's own recording when the loop is index-driven (thread a
   suppressFullScanCounter flag; the seek paths never record anyway).
5. Expect: where9 6→2 (2.5 needs S23.3), where8 16→7-ish.

**S23.3 — OR→IN base seek + the OR-arm row source.**
1. OR→IN (whereexpr.c exprAnalyzeOrTerm): in the FROM-only conjunct
   view, an OR conjunct whose arms are ALL eq on the same FROM-table
   column rewrites to `col IN (v1..vn)`; the seek planner gains an IN
   term (sorted values; seek per value + iterate; constrained loop → no
   fullscan steps). where8-3.3/3.4/3.5/3.6.
2. OR-arm row source (wherecode.c Case 5 per loop): for a comma/inner
   join operand (base or inner) whose constraint set contains an OR term
   with 2+ indexable arms (planOrIndexScan reusable for planning), per
   OUTER row: arm seeks with the outer values bound (RowSet dedup after
   the owning arm's residual accepts), rows in arm order. Base side:
   where8-3.8 (arms a=? i1, b=? i2 → counter 0). Inner side: where8-3.9
   (t2 arms d=?/e=? → row order (9,9),(9,4)) and where9-2.5.
3. Expect: where8 3.3-3.9 green; where9 2.5 green.

**S23.4 — JOIN ORDER BY delivery (wherePathSatisfiesOrderBy port).**
New logic in select_order_search.go (+ a join gate in the sort
decision).
1. joinOrderByDelivered(s, loops): walk loops outer→inner; per loop:
   the eq pre-pass, the index-column walk (nEq prefix skip, collation +
   direction + NULLS agreement, rowid tail incl. the IPK alias), the
   order-distinct mark-off (unique index + full key consumption, or a
   rowid match; mask rule across loops). loopNone = the table btree walk
   (rowid tail only). OR-arm loops never deliver.
2. The loops must EMERGE in the claimed order: base rows in the base
   loop's order (seek/walk = index order; plain scan = rowid order);
   inner rows per outer row in the inner loop's order — the inner pick
   for an unconstrained operand prefers the ORDER-BY-serving covering
   walk (remaining terms), else the cost covering walk (projection rides
   the index); materialized rightMaps are permuted into that index's key
   order (rowid tiebreak, reversed for DESC) — the walk is stateless so
   the permutation reproduces it exactly.
3. Wire the gate into the ORDER BY decision for join statements (skip
   the comparator sort; harness sort marker 0). where-6.15, 14.1-14.7.2.
4. Expect: where 10→4 (19.0/25.x remain, not this unit's class).

**S23.5 — correlated subquery index seek (where8-3.14/3.15).**
The nested subquery scan (per outer row) must plan its loop from the
outer-bound eq/range terms: `SELECT d FROM t2 WHERE e = b` seeks i4
(e=b binds the outer value); `t2.d > inner.d` ranges via i3. Check the
correlated-subquery execution path (select_subq / EvalExpr subquery
arm), extend its scan dispatch the same way as S23.2's base picks.
3.14's counter 99→9; 3.15's 909→9. (S21's carded seekConstValue
subquery-binding gap is a DIFFERENT shape — literal subquery on the
RIGHT side; here the correlation is a bare outer column ref.)

### Out of scope (carded / other units)
- where 19.0/25.x (EQP autoindex names, corruption), whereH 5,
  wherelimit2 4, where2 4 — different classes.
- The inner unconstrained TABLE walk's per-outer-row counter (none of
  the 30 targets has a C table-walk inner loop; ours' inner
  materialization stays unrecorded — noted for the lessons record).
- INDEXED BY name forcing (only NOT INDEXED is in scope).
