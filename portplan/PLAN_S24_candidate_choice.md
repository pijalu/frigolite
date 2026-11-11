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
