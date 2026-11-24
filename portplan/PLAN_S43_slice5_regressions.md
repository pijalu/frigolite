# PLAN — S43 slice 5 gate regressions (2026-11-22)

## State

The uncommitted S43 slice-5 unit closes bestindexE 6→0. Its full gate
(`/tmp/s43_slice6_full.log`, 1193.65s, completed, raw-byte failure-name scan)
has 1174 leaves: GONE vs `portplan/baselines/s35g_gate_fails.txt` includes
all six bestindexE leaves; NEW vs the pre-slice gate5 are:

- `collate5/collate5-5.1`
- `join2/join2-7.0`
- `subquery2/subquery2-3.0`
- `with2/with2-1.15.2`
- `temptable2/temptable2-4.1.1` (documented randomblob flake, not engine)

The first four are real regressions from `internal/execquery/select_subq_pushdown.go`.
Do not commit until all four are green with bestindexE still gone.

## Reproductions (full-gate assertions)

- collate5: `SELECT * FROM (SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2) WHERE b='BbB'`
  — got `[]`, want `[1 bbb]`. `t1.b` is `COLLATE nocase`; `t2.d` is BINARY.
- join2: `SELECT * FROM test` where `test` is `t1 LEFT JOIN (SELECT * FROM t2,t3) ... WHERE c IS NULL`
  — got all three t1 rows, want only the two unmatched ones.
- subquery2: `SELECT id FROM (SELECT id,data FROM (SELECT * FROM t1 UNION ALL SELECT * FROM t2) WHERE id=10 ORDER BY data)`
  — got `[]`, want `[10 10]`.
- with2: recursive CTE whose recursive term uses `FROM (SELECT * FROM main.t4)` —
  got `[4]`, want `[4 5]`.

## C contracts (../sqlite 3.51.0)

1. `sqlite3ExprIsSingleTableConstraint` (expr.c:2747-2778) returns 0 when
   `pSrc->fg.jointype & JT_LTORJ` (rule 3). `pushDownWhereTerms`
   (select.c:5275-5277) also rejects a right operand of LEFT/RIGHT JOIN.
   A WHERE filter on the nullable side of a LEFT JOIN must not be pushed
   into that operand.
2. `substExpr` (select.c:3983-3995) wraps an substituted expression with
   the output column's collation from `pCList` (the LEFTMOST compound arm's
   expression list) when its natural collation differs. Our
   `substituteOutputRefs` uses the arm's own expression without that
   wrapper, changing `b='BbB'` (NOCASE) into `d='BbB'` (BINARY).
3. C's push-down substitutes into a RESOLVED `pEList`. Our AST still carries
   `SELECT *` as a `ColumnRef{Name:"*"}` in the compound arms; substituting
   that node produces no usable predicate. Resolve star members to a
   per-position `ColumnRef` against the member's FROM source.
4. `pushDownWhereTerms` rejects recursive CTEs (select.c:5272-5274,
   SF_Recursive), materialized CTEs (restriction 7, select.c:8092-8094),
   and VALUES subqueries (restriction 11, select.c:5272's SF_MultiPart /
   SF_Values class). Skip push-down when the target subquery is a VALUES
   chain or reads a CTE.

## Ordered changes

1. Add a focused reproduction run for the four families; record got/want.
2. Extend `pushDownWhereTermsIntoSub` guards:
   - reject a FROM operand on the nullable side of a LEFT/RIGHT JOIN;
   - reject VALUES-chain members and members whose FROM table resolves to
     a CTE in the current scope (normalize `main.t4` to `t4`).
3. Make substitution faithful:
   - pass the head member's resolved output collations into
     `substituteOutputRefs`;
   - wrap a replacement with `COLLATE <headColl>` when its natural
     collation differs (use the member's own resolved output collation);
   - expand a bare `SELECT *` member's replacement expression to a
     qualified `ColumnRef` for the requested output position.
4. Verify: focused family gates (collate5, join2, subquery2, with2,
   bestindex, where, select, vtab) by failure-name set; static checks;
   full suite gate with raw-byte name diff against the s35g baseline and
   gate6; commit only with no REAL NEW and bestindexE still 0.

## Resolution

The four root causes above were fixed in
`internal/execquery/select_subq_pushdown.go`: the outer-join operand veto,
the leftmost-arm collation wrapper on substituted expressions, star-member
expansion through the resolved FROM source, and the CTE/VALUES guards.
Focused family gates are baseline-identical (collate5/join2/subquery2 = 0
fails; with2 = its six baseline leaves; bestindexE = 0). The FULL gate
(`/tmp/s43_slice7_full.log`, 1181.1s) has **1170 leaves**, with the four
regressions GONE and NEW=0 versus the pre-slice gate5; bestindexE stays 0.

## Carded / out of scope

- `temptable2-4.1.1` randomblob dither; `resetdb-300` load flap.
- bestindexF `1.4.1` and the 2.8/2.9/2.10 `.0` converter-placement trio
  remain the next functional slice after this regression fix.
