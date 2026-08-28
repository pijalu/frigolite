# TASK G2.SUBQUERY — Subqueries (scalar, IN, EXISTS, derived tables)

> **Phase**: G2 (query features).
> **Goal**: G2.SUBQUERY.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1 stable; G2.JOIN.
> **Current state: FAILING** — `subquery` fails.

## Objective
Subqueries in all positions match SQLite: scalar subquery in SELECT/WHERE,
`(SELECT ...)` as a value; `x IN (SELECT ...)` / `NOT IN`; `EXISTS (SELECT ...)`
/ `NOT EXISTS`; correlated subqueries (referencing outer query columns); derived
tables / subqueries in FROM (`FROM (SELECT ...) AS t`); row-value subqueries;
and the "subquery returns N columns - expected 1" type errors. Also the
flattening-independent correctness (we don't need the optimizer, just correct
results).

## Scope — testgen packages
`subquery`, `subselect`, `exists`, `existsexpr`.

## Pre-test file
`frigolite_p2_subquery_test.go` — `TestP2Subquery_*`. Cases vs oracle:
- Scalar subquery in SELECT list and in WHERE (`WHERE x = (SELECT max(y)...)`).
- `IN (SELECT ...)`, `NOT IN (SELECT ...)` with NULL handling.
- `EXISTS`/`NOT EXISTS`.
- Correlated subquery (inner references outer column).
- Derived table in FROM with alias + column list.
- Row-value: `(a,b) IN (SELECT ...)` (if supported; coordinate with rowvalue).
- Error: subquery returning >1 column in scalar context; >1 row in scalar context.

## SQLite source references
- `src/select.c` — subquery materialization, scalar subquery, EXISTS.
- `src/expr.c` — `sqlite3FindInIndex`, IN-subquery handling.

## Steps
- [ ] **G2.SUBQUERY.1** Pre-test suite. Commit: `G2.SUBQUERY.1: subquery pre-test suite`.
- [ ] **G2.SUBQUERY.2** Triage `subquery` failure via pure-Go test. Likely:
  correlated-variable binding, or scalar subquery returning a row set instead of
  one value. Fix `internal/exec/select.go`/`expression.go`. Commit:
  `G2.SUBQUERY.2: scalar + correlated subqueries`.
- [ ] **G2.SUBQUERY.3** IN/NOT IN subquery with NULL semantics. Commit:
  `G2.SUBQUERY.3: IN-subquery`.
- [ ] **G2.SUBQUERY.4** EXISTS/NOT EXISTS. Commit: `G2.SUBQUERY.4: EXISTS`.
- [ ] **G2.SUBQUERY.5** Derived table in FROM (alias + column list + star).
  Commit: `G2.SUBQUERY.5: derived tables`.
- [ ] **G2.SUBQUERY.6** Column-count / row-count error text for scalar contexts.
  Commit: `G2.SUBQUERY.6: scalar subquery arity errors`.
- [ ] **G2.SUBQUERY.7** testgen subquery/subselect/exists/existsexpr green.
  Commit: `G2.SUBQUERY.7: subquery TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/subquery/ ./testgen/subselect/ ./testgen/exists/ ./testgen/existsexpr/ && \
go test -run 'TestP2Subquery' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Subqueries in all positions match SQLite: scalar (SELECT/WHERE), IN/NOT IN (SELECT), EXISTS/NOT EXISTS, correlated subqueries, derived tables in FROM (alias+cols), row-value subqueries, scalar arity errors. subquery currently FAILS. See portplan/TASK_G2_SUBQUERY.md." \
  completionCriterion "testgen subquery, subselect, exists, existsexpr PASS and TestP2Subquery pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/subquery/ ./testgen/subselect/ ./testgen/exists/ ./testgen/existsexpr/ && go test -run TestP2Subquery -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G2.SUBQUERY. subquery FAILS. Engine: internal/exec/select.go (subquery materialization)
+ expression.go (scalar/IN/EXISTS eval). Correlated binding = outer-row column lookup.
Decisions: correctness over optimizer; no query flattening required, just right results.
Next: pre-tests, triage scalar/correlated, then IN/EXISTS, then derived tables.
Risks: deeply nested correlation may expose expression-evaluator scope bugs.
Carried limits: verifyCommand above.
```
