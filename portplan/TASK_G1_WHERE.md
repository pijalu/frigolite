# TASK G1.WHERE — WHERE clauses, operators, NULL three-valued logic

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.WHERE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.EXPR (shared expression evaluator).

## Objective
WHERE filtering matches SQLite exactly across all operators and the NULL
three-valued logic. This is currently **FAILING** (where, whereA fail) — a top
priority. Covers: comparison ops, AND/OR/NOT, IS NULL / IS NOT NULL, IS /
IS NOT, BETWEEN, IN (list + subquery), LIKE/GLOB/REGEXP, three-valued logic,
COLLATE in predicates, type affinity in comparisons, NULL ordering in indexes.

## Scope — testgen packages
`where`, `whereA`, `whereB`, `whereC`, `whereD`, `whereE`, `whereF`, `whereG`,
`whereH`, `whereI`, `whereJ`, `whereK`, `whereL`, `whereM`, `whereN`.
(`where` and `whereA` are the current red signals — start there.)

## Pre-test file
`frigolite_p1_where_test.go` — `TestP1Where_*`. Cases vs oracle:
- Comparison ops `= <> != < <= > >=` with numeric, text, blob, mixed affinity.
- NULL three-valued logic: `NULL = NULL`→NULL (false in WHERE), `NULL <> 1`→NULL,
  `NOT NULL`→NULL, `NULL OR TRUE`→TRUE, `NULL AND FALSE`→FALSE.
- `IS NULL`, `IS NOT NULL`, `IS`, `IS NOT` (incl. `IS ''` vs `IS NULL`).
- BETWEEN (NOT BETWEEN), symmetric NULL handling.
- IN (scalar list), IN (subquery), NOT IN with NULL in list → empty.
- LIKE (`%`, `_`, case-insensitive for ASCII, ESCAPE), GLOB, REGEXP.
- COLLATE in WHERE (BINARY/NOCASE/RTRIM) overriding column collation.
- Affinity: comparing INTEGER column to TEXT literal coercion rules.
- Short-circuit and parenthesization; `x AND y OR z` precedence.

## SQLite source references
- `src/where*.c` — where-clause planning + code generation.
- `src/expr.c` — `sqlite3ExprIfTrue/IfFalse`, `sqlite3ExprCompare`, affinity.
- `src/vdbe.c` — comparison opcodes + NULL jump logic.

## Steps
- [ ] **G1.WHERE.1** Pre-test suite. Commit: `G1.WHERE.1: WHERE pre-test suite`.
- [ ] **G1.WHERE.2** Three-valued logic correctness in the expression evaluator
  (`internal/exec/expression.go`): every boolean op must propagate NULL per
  SQLite's Kleene logic. **This is the likely root cause of where/whereA.**
  Commit: `G1.WHERE.2: correct NULL three-valued logic`.
- [ ] **G1.WHERE.3** Comparison affinity: apply SQLite's affinity rules
  (src/expr.c `sqlite3Affinity`) so INTEGER-vs-TEXT compares numerically when
  appropriate. Commit: `G1.WHERE.3: comparison affinity`.
- [ ] **G1.WHERE.4** IN with NULL in list; NOT IN semantics; IN subquery.
  Commit: `G1.WHERE.4: IN/NOT IN with NULL`.
- [ ] **G1.WHERE.5** LIKE/GLOB/REGEXP: ASCII case-insensitivity, ESCAPE clause,
  pattern-too-big error. Commit: `G1.WHERE.5: LIKE/GLOB/REGEXP`.
- [ ] **G1.WHERE.6** COLLATE in predicate overrides column collation (BINARY/
  NOCASE/RTRIM). Commit: `G1.WHERE.6: COLLATE in WHERE`.
- [ ] **G1.WHERE.7** Error propagation: a bad WHERE expression raises the error
  rather than silently skipping rows (rowPassesWhere returns (bool,error)).
  Commit: `G1.WHERE.7: WHERE error propagation`.
- [ ] **G1.WHERE.8** testgen where–whereN green. Commit: `G1.WHERE.8: WHERE TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/where/ ./testgen/whereA/ ./testgen/whereB/ ./testgen/whereC/ ./testgen/whereD/ ./testgen/whereE/ ./testgen/whereF/ ./testgen/whereG/ ./testgen/whereH/ ./testgen/whereI/ ./testgen/whereJ/ ./testgen/whereK/ ./testgen/whereL/ ./testgen/whereM/ ./testgen/whereN/ && \
go test -run 'TestP1Where' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "WHERE filtering matches SQLite: all comparison ops, AND/OR/NOT, NULL three-valued (Kleene) logic, IS [NOT] NULL, BETWEEN, IN (list+subquery), LIKE/GLOB/REGEXP, COLLATE, comparison affinity, error propagation. where & whereA currently FAIL. See portplan/TASK_G1_WHERE.md." \
  completionCriterion "testgen where-whereN PASS and TestP1Where pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/where/ ./testgen/whereA/ ./testgen/whereB/ ./testgen/whereC/ ./testgen/whereD/ ./testgen/whereE/ ./testgen/whereF/ ./testgen/whereG/ ./testgen/whereH/ ./testgen/whereI/ ./testgen/whereJ/ ./testgen/whereK/ ./testgen/whereL/ ./testgen/whereM/ ./testgen/whereN/ && go test -run TestP1Where -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.WHERE. [done + outputs]. Root cause of where/whereA was NULL three-valued
logic (internal/exec/expression.go). rowPassesWhere returns (bool,error) — propagate.
Decisions: affinity from src/expr.c sqlite3Affinity; COLLATE overrides column collation.
Next: pre-tests, then Kleene logic fix, then affinity, then where–whereN.
Risks: index-assisted WHERE (whereD/etc.) may need G3.INDEX cooperation.
Carried limits: verifyCommand above.
```
