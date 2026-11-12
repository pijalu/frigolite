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

- Baseline RESOLUTION: /tmp/w_s22_f.txt (62 lines) is a STALE mid-S22
  snapshot. The authoritative census = the verified full gate at
  73a643fe4: where-family 47 leaves (where 10, where2 4, where8 16,
  where9 8, whereH 5, wherelimit2 4). The worktree's own sweep
  reproduces exactly 47 — /tmp/s23_base_leaves.txt is the working
  baseline (leaf-only lines; the raw sweep log also has 6 family-level
  FAIL lines that must be filtered).
- Failure table (got vs want, from the sweep log) and the C contracts
  are in the diagnosis below. Scope = 30 leaves: where8 16, where9 8,
  where 6.15+14.x (6).

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
