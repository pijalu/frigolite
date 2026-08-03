# Goal Schedule — Execution Commands

> **Purpose**: Exact `goal create` commands for each sub-goal, in execution order.
> Each goal uses `freshContext: true` to limit cost.
> Goals are queued — each completes before the next starts.

---

## How to Use

1. Goals are created ONE AT A TIME (the active goal runs; the rest queue).
2. Use the `objective`, `completionCriterion`, `verifyCommand`, and `handover`
   from the referenced sub-plan file.
3. The handover note is critical — it's the ONLY context a fresh-context goal sees.
4. After a goal completes, update its sub-plan checkboxes and commit.

---

## Phase 0: Grammar (G0)

### G0.1 — Fix passthrough rules
```
Goal: G0.1 — Fix 10 passthrough grammar rules
Sub-plan: plans/subplans/G0_GRAMMAR.md (§G0.1)
Objective: Implement handlers for the 10 multi-symbol passthrough rules (277, 133,
231, 302, 349, 352, and audit 2,14,348,351) so every fired multi-symbol rule
produces a real AST node.
Completion criterion: TestGrammarCoverage passes with 0 multi-symbol passthrough
failures; trigger/where/expr TCL packages unchanged or improved.
Verify command: go test ./internal/parse/ -run TestGrammarCoverage -count=1 -v
Fresh context: true
Handover: <see G0_GRAMMAR.md handover note>
```

### G0.2 — Extend grammar corpus
```
Goal: G0.2 — Extend grammar coverage corpus
Sub-plan: plans/subplans/G0_GRAMMAR.md (§G0.2)
Objective: Extend grammarCoverageCorpus to exercise window, CTE, ALTER, constraint
statements, exposing previously-unfired rules.
Completion criterion: Corpus extended; TestGrammarCoverage runs (may show new
failures to fix in G0.3).
Verify command: go test ./internal/parse/ -run TestGrammarCoverage -count=1
Fresh context: true
```

### G0.3 — Full grammar coverage
```
Goal: G0.3 — Implement remaining reachable rules to full coverage
Sub-plan: plans/subplans/G0_GRAMMAR.md (§G0.3)
Objective: Implement handlers for all reachable grammar rules exposed by the
extended corpus — 0 multi-symbol passthrough failures.
Completion criterion: TestGrammarCoverage passes; FRIGOLITE_INVENTORY shows 0
multi-symbol passthroughs.
Verify command: go test ./internal/parse/ -run TestGrammarCoverage -count=1 && go test -tags testgen ./testgen/select1/ ./testgen/insert/ ./testgen/update/ ./testgen/delete_/ -count=1
Fresh context: true
```

---

## Phase 1: CRUD Core (G1.*)

Create each in order. The `priority: "front"` option can jump a goal ahead.

### G1.CREATE
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.CREATE)
Objective: All CREATE TABLE functionality — types, constraints, WITHOUT ROWID,
STRICT, AS SELECT, AUTOINCREMENT, generated columns.
Completion criterion: testgen select1, types, strict, without_rowid, tableopts PASS; TestP1Create PASS.
Verify: go test -tags testgen ./testgen/select1/ ./testgen/types/ ./testgen/strict/ ./testgen/without_rowid/ ./testgen/tableopts/ -count=1 && go test -run TestP1Create -count=1 .
Fresh context: true
```

### G1.INSERT
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.INSERT)
Objective: All INSERT — VALUES, multi-row, INSERT...SELECT, DEFAULT VALUES, UPSERT,
OR IGNORE/REPLACE, column affinity.
Completion criterion: testgen insert, values, valuesfault, default_pkg PASS; TestP1Insert PASS.
Verify: go test -tags testgen ./testgen/insert/ ./testgen/values/ ./testgen/valuesfault/ ./testgen/default_pkg/ -count=1 && go test -run TestP1Insert -count=1 .
Fresh context: true
```

### G1.SELECT
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.SELECT)
Objective: SELECT — projection, WHERE, DISTINCT, ORDER BY, LIMIT/OFFSET, aliases,
star expansion, float formatting, compound, view column resolution.
Completion criterion: testgen select2–selectH PASS; TestP1Select PASS.
Verify: go test -tags testgen ./testgen/select2/ ... ./testgen/selectH/ -count=1 && go test -run TestP1Select -count=1 .
Fresh context: true
```

### G1.WHERE
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.WHERE)
Objective: WHERE — all operators, NULL three-valued logic, IN/BETWEEN/LIKE/GLOB,
COLLATE, type affinity, NULLS FIRST/LAST.
Completion criterion: testgen where–whereN PASS; TestP1Where PASS.
Verify: go test -tags testgen ./testgen/where/ ... ./testgen/whereN/ -count=1 && go test -run TestP1Where -count=1 .
Fresh context: true
```

### G1.UPDATE
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.UPDATE)
Objective: UPDATE — SET, multi-column, WHERE, OR IGNORE/REPLACE, UPDATE...FROM, RETURNING.
Completion criterion: testgen update, returning PASS; TestP1Update PASS.
Verify: go test -tags testgen ./testgen/update/ ./testgen/returning/ -count=1 && go test -run TestP1Update -count=1 .
Fresh context: true
```

### G1.DELETE
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.DELETE)
Objective: DELETE — WHERE, RETURNING, subquery in WHERE, WITHOUT ROWID.
Completion criterion: testgen delete_, delete2, delete3, delete4, delete_pkg PASS; TestP1Delete PASS.
Verify: go test -tags testgen ./testgen/delete_/ ./testgen/delete2/ ./testgen/delete3/ ./testgen/delete4/ ./testgen/delete_pkg/ -count=1 && go test -run TestP1Delete -count=1 .
Fresh context: true
```

### G1.TYPES
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.TYPES)
Objective: Type affinity, CAST, NULL handling, typeof(), integer PK extremes.
Completion criterion: testgen affinity, cast, numcast, types, intpkey, intreal, nulls, null PASS; TestP1Types PASS.
Verify: go test -tags testgen ./testgen/affinity/ ./testgen/cast/ ./testgen/numcast/ ./testgen/types/ ./testgen/intpkey/ ./testgen/intreal/ ./testgen/nulls/ ./testgen/null/ -count=1 && go test -run TestP1Types -count=1 .
Fresh context: true
```

### G1.EXPR
```
Sub-plan: plans/subplans/P1_CRUD.md (§G1.EXPR)
Objective: Expression evaluation — arithmetic, comparison, logical, CASE, BETWEEN,
IN, LIKE, IS NULL, NULL propagation, bool rendering.
Completion criterion: testgen expr, between, coalesce, literal, istrue, cse, subtype PASS; TestP1Expr PASS.
Verify: go test -tags testgen ./testgen/expr/ ./testgen/between/ ./testgen/coalesce/ ./testgen/literal/ ./testgen/istrue/ ./testgen/cse/ ./testgen/subtype/ -count=1 && go test -run TestP1Expr -count=1 .
Fresh context: true
```

---

## Phases 2–6: Same Pattern

For G2.* through G6.*, use the goal definitions from the respective sub-plan files:

| Phase | Sub-plan | Goals |
|-------|----------|-------|
| P2 | plans/subplans/P2_QUERY.md | G2.JOIN, G2.SUBQUERY, G2.AGG, G2.ORDER, G2.SETOP, G2.VIEW |
| P3 | plans/subplans/P3_SCHEMA.md | G3.ALTER, G3.INDEX, G3.TRIGGER, G3.FK, G3.CONSTR, G3.COLLATE |
| P4 | plans/subplans/P4_FUNCTIONS.md | G4.STRING, G4.DATE, G4.NUMERIC, G4.PRINTF, G4.LIKE |
| P5 | plans/subplans/P5_ADVANCED.md | G5.0, G5.CTE, G5.WINDOW, G5.PRAGMA, G5.FTS, G5.VTAB, G5.ATTACH, G5.JSON |
| P6 | plans/subplans/P6_TRIAGE.md | G6.NA, G6.DEFERRED, G6.MISC |

Each goal definition is in the sub-plan — copy the `Objective`, `Completion criterion`,
`Verify command`, and `Fresh context: true` fields into a `goal create` call.

---

## Execution Order (full sequence)

```
G0.1 → G0.2 → G0.3
  → G1.CREATE → G1.INSERT → G1.SELECT → G1.WHERE
  → G1.UPDATE → G1.DELETE → G1.TYPES → G1.EXPR
  → G2.JOIN → G2.SUBQUERY → G2.AGG → G2.ORDER → G2.SETOP → G2.VIEW
  → G3.ALTER → G3.INDEX → G3.TRIGGER → G3.FK → G3.CONSTR → G3.COLLATE
  → G4.STRING → G4.DATE → G4.NUMERIC → G4.PRINTF → G4.LIKE
  → G5.0 → G5.CTE → G5.WINDOW → G5.PRAGMA → G5.FTS → G5.VTAB → G5.ATTACH → G5.JSON
  → G6.NA → G6.DEFERRED → G6.MISC
```

Total: ~40 goals. Each runs with fresh context (cost-limited). Each has its own
sub-plan with step-by-step instructions and commit points.

---

## Resuming the Paused Goal (T1.8)

There is a paused goal `lively.wolf` (T1.8: grammar implementation). Its work
overlaps with G0 (grammar completion). Options:

1. **Resume T1.8** — call `goal update status=active` to continue its specific
   scope (inventory + grammar-caused test fixes + coverage test).
2. **Replace with G0** — T1.8's completion criterion is a subset of G0.3.
   The G0 sub-plan is more comprehensive.

**Recommendation**: Resume T1.8 first (it's partially done), then proceed with
G0.2/G0.3 to extend coverage beyond T1.8's scope.
