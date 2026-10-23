# cursorhint-6 correlated HAVING regression

## Evidence and source behavior
- Isolated `FRIGOLITE_TEST=cursorhint go test . -run '^TestSQLiteSuite$' -count=1` after fixture regeneration fails only cursorhint-6.0: query returns no rows, expects `abc 2 uvw 2`.
- Pure-Go native test `TestCursorhint6CorrelatedHavingKeepsGroupRows` reproduces nil rows after schema + two inserts.
- SQLite source `../sqlite/test/cursorhint.test:174-192` groups rows from `t6 LEFT JOIN v1 ON true`; HAVING correlated EXISTS-like scalar subquery `SELECT true FROM t6 AS aa LEFT JOIN t6 AS bb ON length(v1.a)>5` is true for each group; SQLite oracle expects both groups (Python sqlite3 confirms).

## Ordered work
1. Trace correlated outer column resolution and scalar-subquery truth / HAVING aggregate pipeline; identify earliest divergence against SQLite `select.c` and `where.c`.
2. Add one scoped assertion/log-free fix to correlated aggregate evaluation or planner; keep the native RED test.
3. Run native test, full cursorhint and cursorhint2, attach2, `git diff --check`, then fresh full suite.
