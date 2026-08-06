# TASK G1.EXPR — Expressions (arithmetic, logical, CASE, IN, scalar funcs)

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.EXPR.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.TYPES.
> **Current state: PARTIAL** — `between` PASSes; `expr`, `coalesce` FAIL.

## Objective
The shared expression evaluator (`internal/exec/expression.go`) handles every
SQL expression form correctly: arithmetic (`+ - * / %`), concatenation (`||`),
comparison + logical (delegating NULL handling to G1.WHERE), `BETWEEN`, `IN`,
`CASE WHEN/...THEN/ELSE/END`, `CAST`, `EXISTS`, scalar function calls, literals
(integer/real/text/blob/hex/NULL), `||` with NULL, operator precedence, and the
three-valued logic throughout. This is the foundation for WHERE, SELECT
projection, UPDATE SET, and CHECK constraints.

## Scope — testgen packages
`expr`, `between`, `coalesce`, `istrue`, `literal`, `cse`, `subtype`.

## Pre-test file
`frigolite_p1_expr_test.go` — `TestP1Expr_*`. Cases vs oracle:
- Arithmetic with affinity (INTEGER/REAL division: `5/2`→2 int; `5.0/2`→2.5).
- `%` modulo; `||` concat incl. NULL propagation (`'a'||NULL`→NULL).
- Operator precedence: `||`, `* / %`, `+ -`, comparisons, `NOT`, `AND`, `OR`.
- CASE: searched (`CASE WHEN ...`), simple (`CASE x WHEN ...`), NULL in WHEN.
- IN (list), NOT IN with NULL; BETWEEN; EXISTS.
- Literals: integer, real, single-quoted text, double-quoted (DQS), x'..' blob,
  0x.. hex, NULL.
- IS / IS NOT (incl. `IS NULL`, `IS NOT DISTINCT FROM`).
- Type errors: `SELECT 'a'+1` (SQLite → 1; affinity), etc.

## SQLite source references
- `src/expr.c` — `sqlite3ExprCode`, operator precedence, affinity.
- `parse.y` — expression grammar + precedence (the `expr` non-terminal).
- `src/vdbe.c` — arithmetic opcodes, Concat, Cast.

## Steps
- [ ] **G1.EXPR.1** Pre-test suite. Commit: `G1.EXPR.1: expr pre-test suite`.
- [ ] **G1.EXPR.2** Operator precedence correctness (verify against parse.y
  precedence levels). Commit: `G1.EXPR.2: operator precedence`.
- [ ] **G1.EXPR.3** Arithmetic affinity: int vs real division, modulo, type of result.
  Commit: `G1.EXPR.3: arithmetic affinity`.
- [ ] **G1.EXPR.4** `||` concat with NULL propagation. Commit: `G1.EXPR.4: concat`.
- [ ] **G1.EXPR.5** CASE (searched + simple) with NULL in WHEN/THEN/ELSE.
  Commit: `G1.EXPR.5: CASE expressions`.
- [ ] **G1.EXPR.6** Triage `coalesce` failure (likely IS/NULL handling or arg eval).
  Commit: `G1.EXPR.6: COALESCE/IFNULL`.
- [ ] **G1.EXPR.7** Triage `expr` testgen failure via pure-Go test; fix root cause.
  Commit: `G1.EXPR.7: expr TCL fixes`.
- [ ] **G1.EXPR.8** testgen expr/between/coalesce/istrue/literal green.
  Commit: `G1.EXPR.8: expr TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/expr/ ./testgen/between/ ./testgen/coalesce/ ./testgen/istrue/ ./testgen/literal/ ./testgen/cse/ ./testgen/subtype/ && \
go test -run 'TestP1Expr' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Shared expression evaluator handles all forms: arithmetic with affinity, || concat with NULL, comparison/logical with 3-valued logic, BETWEEN, IN, CASE (searched+simple), CAST, EXISTS, literals (int/real/text/blob/hex/NULL/DQS), operator precedence. expr & coalesce currently FAIL. See portplan/TASK_G1_EXPR.md." \
  completionCriterion "testgen expr, between, coalesce, istrue, literal, cse, subtype PASS and TestP1Expr pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/expr/ ./testgen/between/ ./testgen/coalesce/ ./testgen/istrue/ ./testgen/literal/ ./testgen/cse/ ./testgen/subtype/ && go test -run TestP1Expr -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.EXPR. between PASSes; expr/coalesce FAIL. Evaluator: internal/exec/expression.go.
This is the foundation for WHERE/SELECT/UPDATE/CHECK — keep it correct and 3-valued.
Decisions: int/real division follows affinity (5/2=2); || propagates NULL.
Next: pre-tests, then precedence + arithmetic affinity, then CASE, then triage expr/coalesce.
Risks: changes here ripple into every query task — re-run G1.WHERE + G1.SELECT verify after.
Carried limits: verifyCommand above.
```
