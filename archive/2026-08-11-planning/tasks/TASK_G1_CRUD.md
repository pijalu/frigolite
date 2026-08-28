# TASK G1 — CRUD & Query Engine (THE critical path)

> **Phase**: G1 (CRUD & QUERY ENGINE — core goals must be green before G2)
> **Goal IDs**: G1.CRUD, G1.EXPR-WHERE, G1.ORDER-SETOPS, G1.JOIN-SUBQUERY,
> G1.AGG-VIEW
> **Read first**: `PORTPLAN.md` §0, **`portplan/DESIGN.md` §A (execution model),
> §B (god-file split), §C (CRUD gap analysis)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started (some families partially green from prior work)

---

## Objective

Make the CRUD and core query foundation rock-solid. After G1, every
SELECT/INSERT/UPDATE/DELETE/expr/where/order/limit/join/subquery/aggregate/view
family is green. These are the families the 140 un-skipped "slow" packages
expand on, so G1 *absorbs* the engine gaps those packages expose.

**Triage rule (mandatory)**: for each failing package, write a pure-Go pre-test
(`frigolite_p1_*.go`) reproducing the feature, compare against `sqlite3`, decide
engine-vs-transpiler, fix the smallest diff. Most failures are engine bugs.

Each goal below is one Goa goal (`freshContext: true`) with its own verify
command and an ordered todo list. Goals within G1 serialize when they touch the
same engine file (`internal/exec/select.go`, `expression.go`); otherwise they may
run in parallel.

---

## Goal G1.CRUD — SELECT / INSERT / UPDATE / DELETE core

**Scope** (testgen families): `select1`–`selectH`, `insert`, `insert2`–`insert5`,
`values`, `default`, `update`, `delete`, `delete2`–`delete4`, `types`, `without_rowid`,
`tableopts`.

**Key engine areas** (`internal/exec/`): `select.go`, `insert.go`, `update.go`,
`delete.go`, `ddl.go`; affinity in `internal/util/`; rowid/INTEGER PK aliasing.
Reference SQLite `src/select.c`, `src/insert.c`, `src/update.c`, `src/delete.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/select1/ ./testgen/select2/ ./testgen/select3/ ./testgen/select4/ \
  ./testgen/select5/ ./testgen/select6/ ./testgen/select7/ ./testgen/select8/ \
  ./testgen/select9/ ./testgen/selectA/ ./testgen/selectB/ ./testgen/selectC/ \
  ./testgen/selectD/ ./testgen/selectE/ ./testgen/selectF/ ./testgen/selectG/ \
  ./testgen/selectH/ ./testgen/insert/ ./testgen/update/ ./testgen/delete/ \
  ./testgen/types/ ./testgen/without_rowid/ ./testgen/values/ ./testgen/default/ \
  ./testgen/count/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP1CRUD' -count=1 . && go build ./... && make quality
```

**Todos** (decompose per the status tool's fail list for these families):
1. Run `tools/status` → capture the per-package fail set for the CRUD families.
2. `select*`: fix SELECT-list, FROM-less SELECT, DISTINCT, LIMIT/OFFSET gaps.
3. `insert`: multi-row VALUES, DEFAULT VALUES, INSERT…SELECT, column lists,
   INTEGER PK rowid aliasing, type affinity on write.
4. `update`: WHERE eval error propagation, SET expression eval, multi-col SET.
5. `delete`: WHERE, ORDER BY+LIMIT (already partial), cascade interactions.
6. `types`/`without_rowid`/`values`/`default`: affinity & WITHOUT ROWID PK order.
7. Per fix: pure-Go pre-test + oracle → engine fix → re-run verify → commit.
8. Re-run `tools/status` for these families → all green; commit + push.

---

## Goal G1.EXPR-WHERE — Expressions & WHERE

**Scope**: `expr`, `expr1`, `expr2`, `between`, `coalesce`, `istrue`, `literal`,
`where`, `where2`–`whereN`, `in`, `in2`–`in6`, `case`, `cast`, `collate` (expr-level).

**Key engine areas**: `internal/exec/expression.go`; operator precedence; NULL
three-valued logic; `IN`/`BETWEEN`/`IS`/`CASE`/`CAST`; affinity in comparisons.
Reference SQLite `src/expr.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/expr/ ./testgen/where/ ./testgen/where4/ ./testgen/where6/ \
  ./testgen/where7/ ./testgen/where9/ ./testgen/between/ ./testgen/coalesce/ \
  ./testgen/istrue/ ./testgen/cast/ ./testgen/in/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP1Expr' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set for expr/where families.
2. NULL semantics: `AND`/`OR`/`NOT` (Kleene), `IS NULL`/`IS NOT NULL`, `IS TRUE/FALSE`.
3. `IN` (list/subquery), `BETWEEN`, `CASE`/`COALESCE`/`IFNULL`/`NULLIF`.
4. `CAST` type conversions (all target types incl. NUMERIC/REAL/TEXT/BLOB/INT).
5. Comparison affinity + COLLATE in expressions.
6. Subquery-valued LIMIT (carry over G0.FIX-4-FAILS `subquery` fix if shared code).
7. Per fix: pre-test + oracle → fix → verify → commit; final `tools/status` check.

---

## Goal G1.ORDER-SETOPS — ORDER BY, LIMIT, UNION/INTERSECT/EXCEPT, sorting

**Scope**: `orderby1`–`orderby9`, `sort`, `sort2`–`sort5`, `limit`, `minmax`,
`unionall`, compound selects (`select*` compound cases), `collate` (sort order).

**Key engine areas**: `internal/exec/select.go` (ORDER BY, set ops, DISTINCT);
sort stability & collation; NULLS ordering. Reference SQLite `src/select.c`
`computeLimitRegisters`, `multiSelect`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/orderby1/ ./testgen/orderby2/ ./testgen/orderby3/ ./testgen/orderby4/ \
  ./testgen/orderby5/ ./testgen/orderby6/ ./testgen/orderby7/ ./testgen/orderby8/ \
  ./testgen/orderby9/ ./testgen/sort/ ./testgen/sort2/ ./testgen/sort3/ \
  ./testgen/sort5/ ./testgen/limit/ ./testgen/minmax/ ./testgen/unionall/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP1Order' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set; triage each.
2. ORDER BY: expression terms, position numbers, NULLS FIRST/LAST, COLLATE.
3. Sort stability; DISTINCT ordering; LIMIT/OFFSET edge cases (0, -1, subquery).
4. UNION/UNION ALL/INTERSECT/EXCEPT: column-count check, dedup, ordering, NULLs.
5. Float formatting in sort output (`%!.15g`) — match SQLite exactly.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G1.JOIN-SUBQUERY — JOINs, subqueries, row values

**Scope**: `join`, `joinA`–`joinI`, `subquery`, `subselect`, `subquery2`,
`exists`, `rowvalue`, `rowvalue2`–`rowvalue9`, `rowvalueA`.

**Key engine areas**: `internal/exec/select.go` (join, subquery-in-FROM),
`internal/exec/expression.go` (scalar/correlated subquery, EXISTS, row-value).
Reference SQLite `src/where*.c`, `src/expr.c` (row-value).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 200s \
  ./testgen/join/ ./testgen/joinA/ ./testgen/joinB/ ./testgen/joinC/ \
  ./testgen/joinD/ ./testgen/joinE/ ./testgen/joinF/ ./testgen/joinG/ \
  ./testgen/joinH/ ./testgen/joinI/ ./testgen/subquery/ ./testgen/subselect/ \
  ./testgen/exists/ ./testgen/rowvalue/ ./testgen/rowvalueA/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP1Join' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. INNER/LEFT/RIGHT/FULL/CROSS/NATURAL/USING joins; column resolution & qualifiers.
3. Subquery-in-FROM (derived tables); correlated subqueries; scalar subquery arity.
4. EXISTS; row-value comparison/BETWEEN/IN (carry rowvalue fixes from G0).
5. RIGHT JOIN NULL-padding (carry from prior work); empty-table short-circuit.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G1.AGG-VIEW — Aggregates, GROUP BY/HAVING, DISTINCT-agg, VIEWs

**Scope**: `count`, `countofview`, `having`, `distinct`, `distinctagg`,
`aggorderby`, `aggnested`, `aggerror`, `view`, `view2`, `view3`, `selectB` (group).

**Key engine areas**: `internal/exec/select.go` (GROUP BY/HAVING, DISTINCT),
`internal/function/` (aggregates). Reference SQLite `src/select.c` (grouping),
`src/func.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/count/ ./testgen/countofview/ ./testgen/having/ ./testgen/distinct/ \
  ./testgen/distinctagg/ ./testgen/aggorderby/ ./testgen/aggnested/ \
  ./testgen/aggerror/ ./testgen/view/ ./testgen/view2/ ./testgen/view3/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP1Agg' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. GROUP BY partition + per-group aggregates; non-aggregate columns in SELECT.
3. HAVING (aggregate + row-level); DISTINCT vs DISTINCT-aggregate (`COUNT(DISTINCT)`).
4. Aggregate ORDER BY; nested-aggregate misuse detection; sum/avg/total overflow.
5. VIEW expansion (parse, store, re-execute); updatable-where-applicable; view rename.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All five goals' verify commands pass; the 5 pre-test families (`TestP1CRUD`,
  `TestP1Expr`, `TestP1Order`, `TestP1Join`, `TestP1Agg`) pass; `make quality` +
  SOLID pass; no G0 regression (`tools/status` shows G0 families still green).
- `PORTPLAN.md` §5 status rows → 🟢.
