# Sub-Plan: P2 — Query Features (6 sub-goals)

> **Prerequisite**: P1 CRUD complete.
> **Packages**: 30 (join*, subquery, subselect, count, having, distinct*, agg*,
> orderby*, limit, minmax, sort, sorterref, starschema, unionall, exists*,
> view, countofview)

---

## G2.JOIN — JOIN Operations

### Goal
```
Objective: All JOIN types work — INNER, LEFT, RIGHT, FULL, CROSS, NATURAL,
USING, ON, comma-join, parenthesized joins.
Completion criterion: testgen join–joinI all PASS; pre-tests PASS.
Verify: go test -tags testgen ./testgen/join/ ./testgen/joinA/ ./testgen/joinB/ ./testgen/joinC/ ./testgen/joinD/ ./testgen/joinE/ ./testgen/joinF/ ./testgen/joinH/ ./testgen/joinI/ -count=1 && go test -run TestP2Join -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p2_join_test.go`
- INNER JOIN with ON
- LEFT JOIN with ON (NULL fill for no-match)
- RIGHT JOIN (SQLite 3.39+)
- FULL JOIN (SQLite 3.39+)
- CROSS JOIN
- NATURAL JOIN (auto USING on common columns)
- JOIN ... USING (col1, col2)
- Multiple joins (A JOIN B JOIN C)
- Comma-join (FROM A, B WHERE A.x = B.y)
- Parenthesized join: FROM (A JOIN B USING(x)) JOIN C
- Self-join with aliases
- JOIN with subquery in FROM

### Steps
1. **Write pre-test**. Commit: `G2.JOIN.1: add JOIN pre-test suite`
2. **Fix RIGHT/FULL JOIN** — grammar rules 55–59 (joinop variants) + execution.
   SQLite ref: `src/select.c` (sqlite3Select).
   Commit: `G2.JOIN.2: implement RIGHT/FULL OUTER JOIN`
3. **Fix NATURAL JOIN** — auto-detect common columns for USING.
   Commit: `G2.JOIN.3: implement NATURAL JOIN column merging`
4. **Fix parenthesized join parsing** — `(A JOIN B) JOIN C` as derived table.
   Note: RD parser has parseParenTableRef (check if LALR path handles this).
   Commit: `G2.JOIN.4: fix parenthesized join as subquery`
5. **Run TCL tests**. Commit: `G2.JOIN.N: join TCL tests green`

---

## G2.SUBQUERY — Subqueries

### Goal
```
Objective: All subquery types — scalar subqueries, IN (subquery), EXISTS,
correlated subqueries, derived tables (FROM subquery).
Completion criterion: testgen subquery, subselect, exists, existsexpr PASS.
Verify: go test -tags testgen ./testgen/subquery/ ./testgen/subselect/ ./testgen/exists/ ./testgen/existsexpr/ -count=1 && go test -run TestP2Subquery -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p2_subquery_test.go`
- Scalar subquery: `SELECT (SELECT count(*) FROM t2) FROM t1`
- IN subquery: `SELECT * FROM t1 WHERE a IN (SELECT b FROM t2)`
- NOT IN subquery
- EXISTS: `SELECT * FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.x=t1.x)`
- Correlated subquery (references outer table)
- Derived table: `SELECT * FROM (SELECT a,b FROM t1) AS sub`
- Derived table with column aliases: `(SELECT ...) AS sub(x,y)`

### Steps
1. **Write pre-test**. Commit: `G2.SUBQUERY.1: add subquery pre-test suite`
2. **Fix correlated subquery column resolution** — outer table refs in inner query.
   Commit: `G2.SUBQUERY.2: fix correlated subquery column resolution`
3. **Fix derived table column naming** — subquery column names and aliases.
   Commit: `G2.SUBQUERY.3: fix derived table column naming`
4. **Run TCL tests**. Commit: `G2.SUBQUERY.N: subquery TCL tests green`

---

## G2.AGG — Aggregates / GROUP BY / HAVING

### Goal
```
Objective: All aggregate functionality — GROUP BY, HAVING, COUNT/SUM/AVG/MIN/MAX/
GROUP_CONCAT, DISTINCT in aggregate, aggregate with no GROUP BY.
Completion criterion: testgen count, having, distinct, distinctagg, aggerror PASS.
Verify: go test -tags testgen ./testgen/count/ ./testgen/having/ ./testgen/distinct/ ./testgen/distinctagg/ ./testgen/aggerror/ -count=1 && go test -run TestP2Agg -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p2_agg_test.go`
- COUNT(*), COUNT(col), COUNT(DISTINCT col)
- SUM, SUM(DISTINCT), TOTAL
- AVG, MIN, MAX
- GROUP_CONCAT (with and without separator)
- GROUP BY single column, multiple columns
- HAVING (with and without aggregate)
- Aggregate with no GROUP BY (whole-table aggregate)
- Mixed aggregate and non-aggregate columns
- NULL handling in aggregates (COUNT ignores NULL, SUM treats as 0)

### Steps
1. **Write pre-test**. Commit: `G2.AGG.1: add aggregate pre-test suite`
2. **Fix GROUP_CONCAT** — with separator, ordering.
   Commit: `G2.AGG.2: fix GROUP_CONCAT with separator`
3. **Fix aggregate DISTINCT** — COUNT(DISTINCT col).
   Commit: `G2.AGG.3: implement aggregate DISTINCT`
4. **Run TCL tests**. Commit: `G2.AGG.N: aggregate TCL tests green`

---

## G2.ORDER — ORDER BY / LIMIT / sort

### Goal
```
Objective: ORDER BY (ASC/DESC, multiple cols, by alias/position/expression),
LIMIT/OFFSET, collation-aware sorting.
Completion criterion: testgen orderby, orderbyA, orderbyB, limit, minmax, sort, sorterref PASS.
Verify: go test -tags testgen ./testgen/orderby/ ./testgen/orderbyA/ ./testgen/orderbyB/ ./testgen/limit/ ./testgen/minmax/ ./testgen/sort/ ./testgen/sorterref/ -count=1 && go test -run TestP2Order -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p2_order_test.go`. Commit: `G2.ORDER.1: add ORDER BY pre-test`
2. **Fix ORDER BY with COLLATE** — sort with NOCASE/BINARY/RTRIM.
   Commit: `G2.ORDER.2: fix ORDER BY with COLLATE`
3. **Fix compound SELECT ORDER BY** — ORDER BY after UNION.
   Commit: `G2.ORDER.3: fix compound SELECT ORDER BY/LIMIT`
4. **Run TCL tests**. Commit: `G2.ORDER.N: ORDER BY TCL tests green`

---

## G2.SETOP — Set Operations (UNION/INTERSECT/EXCEPT)

### Goal
```
Objective: UNION, UNION ALL, INTERSECT, EXCEPT — compound SELECT with column
affinity, ORDER BY/LIMIT on compound.
Completion criterion: testgen unionall PASS.
Verify: go test -tags testgen ./testgen/unionall/ -count=1 && go test -run TestP2SetOp -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p2_setop_test.go` — UNION/UNION ALL/INTERSECT/EXCEPT.
   Commit: `G2.SETOP.1: add set operations pre-test`
2. **Fix compound SELECT column affinity** — types from first SELECT member.
   Commit: `G2.SETOP.2: fix compound SELECT column affinity`
3. **Fix INTERSECT/EXCEPT** — set difference operations.
   Commit: `G2.SETOP.3: implement INTERSECT/EXCEPT`
4. **Run TCL tests**. Commit: `G2.SETOP.N: set operations TCL tests green`

---

## G2.VIEW — Views

### Goal
```
Objective: CREATE VIEW, DROP VIEW, querying views, view-over-compound, view column aliases.
Completion criterion: testgen view, countofview PASS.
Verify: go test -tags testgen ./testgen/view/ ./testgen/countofview/ -count=1 && go test -run TestP2View -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p2_view_test.go` — CREATE/DROP VIEW, view query expansion.
   Commit: `G2.VIEW.1: add view pre-test`
2. **Fix view-over-compound** — `CREATE VIEW v AS SELECT ... UNION SELECT ...`.
   Commit: `G2.VIEW.2: fix view-over-compound expansion`
3. **Fix view column aliases** — `CREATE VIEW v(a,b) AS SELECT ...`.
   Commit: `G2.VIEW.3: fix view column aliases`
4. **Run TCL tests**. Commit: `G2.VIEW.N: view TCL tests green`
