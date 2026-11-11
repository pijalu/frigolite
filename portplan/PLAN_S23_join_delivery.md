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
