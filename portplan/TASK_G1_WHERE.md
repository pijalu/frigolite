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
> **Session 2026-08-06 progress (committed `5faefdad`, `80f98eea`, `30a703c2`):**
> - ✅ G1.WHERE.4 (partial) — IN with NULL + composite PK: fixed the
>   `PRIMARY KEY('x' ASC,"y" ASC)` collapse engine bug and the
>   `WHERE a IS $null` transpiler bug → where4-5.2, where4-8.2, where6 PASS.
> - ✅ G1.WHERE.5 (partial) — `db function tclvar` inlined → where-10.2/10.3 PASS.
>
> **Session 2026-08-07 progress (committed `5713622c`..`ac8f9f1e`):**
> - ✅ Pre-tests: `frigolite_p1_where_test.go` (8 oracle-verified tests) PASS.
> - ✅ BETWEEN collation/affinity: evalBetween uses compareValuesWithCollate
>   (handles collatedValue wrappers, left-column collation) + NULL bounds.
> - ✅ LIKE/GLOB/REGEXP REAL text rendering via util.SQLiteValueString
>   (%.15g): 10.0 LIKE '10.0' matches (whereM PASS).
> - ✅ EXPLAIN QUERY PLAN single-table SEARCH now emits 'USING INDEX name'.
> - ✅ NOT BETWEEN parser negation (rule 220 reads the between_op token).
> - ✅ LIKE ESCAPE string extraction (rule 207 uses getString).
> - ✅ IN/NOT IN with NULL list items → NULL when unmatched (Kleene).
> - ✅ Explicit COLLATE operator overrides column collation.
> - ✅ GROUP BY aggregate output sorts by key VALUES (NULL first) — whereG PASS.
> - ✅ Scalar subquery with SELECT * FROM (subquery) arity.
> - ✅ JOIN USING-column filtering only for USING/NATURAL; TRUE/FALSE in ON.
> - ✅ PRAGMA reverse_unordered_selects (top-level scan reversal).
> - ✅ int64 arithmetic overflow → REAL; sqlite3IntFloatCompare at 2^63.
> - ✅ count_changes pragma (DML returns changed count) — where7 PASS.
> - ✅ TRUE/FALSE in WHERE column validation — where7-4.1 PASS.
> - ✅ compound SELECT ORDER BY rowid — where9-6.2.9 PASS.
> - ✅ ANALYZE nm / nm dbnm parse (rule 291) — whereJ PASS.
> - ✅ join planner resolves unqualified refs; scans fewest-index table —
>   whereE PASS.
> - ✅ tcl2go: TCL regex `\y` word-boundary → RE2 `\b` — whereF-1.x/2.x PASS.
> - ✅ **fix(btree): persist table root-page splits to sqlite_schema** —
>   where9 PASS (was count=42). This was a real data-loss bug: a table
>   b-tree split moved the root page but sqlite_schema kept the old
>   rootpage, so reopen (or tableRootPages invalidation) exposed only the
>   first leaf page's rows.
> - ✅ pragma_cache_size table-valued pragma + materialized rowids —
>   where-29.1 PASS.
> - **Now passing**: whereB, whereC, whereE, whereG, whereJ, whereK,
>   whereM, whereN (8/15 packages) + where7, where9 (subtests of `where`)
>   + all TestP1Where pre-tests.
> - **Remaining (mostly G3.INDEX / out-of-scope)**:
>   - whereD/H/I + whereA-3.x + where-19/21 ordering + whereF-3.x/4.0:
>     index-assisted WHERE scan order, OR-optimization, ORDER BY index
>     choice, PK-autoindex SEARCH → G3.INDEX optimizer.
>   - where9 count=42: CREATE INDEX b-tree page-cache aliasing corrupts the
>     table when the index grows enough to split → G3.INDEX btree bug.
>   - whereF: json_each virtual table.
>   - where-26.x: corruption detection. where-29.1: pragma_table_info.
>   - where-15.1: TEMP schema scoping.
>   - whereL-940/950: transpiler NULL-vs-empty-string ambiguity (engine
>     matches oracle; TCL renders both as `{}` so the transpiled want cannot
>     distinguish).
> - Remaining within `where`: where2 (EXPLAIN/plan-shape → G5.EXPLAIN scope),
>   where7 (engine: `want [1 2 1 2 2]` got `[1 2 1 2]`), where9 (engine:
>   missing rows / NULL rows), where-10.4 (stateful `tclvar` flip — needs a
>   per-row function callback or `CreateFunction`-style public API), and the
>   pre-existing `CREATE TEMP TABLE t1 already exists` (TEMP table scoping /
>   test-state).
> - `whereA` still fails (ordering/NULL mismatches — triage with pure-Go test).

- [ ] **G1.WHERE.1** Pre-test suite. Commit: `G1.WHERE.1: WHERE pre-test suite`.
- [ ] **G1.WHERE.2** Three-valued logic correctness in the expression evaluator
  (`internal/exec/expression.go`): every boolean op must propagate NULL per
  SQLite's Kleene logic. **This is the likely root cause of where/whereA.**
  Commit: `G1.WHERE.2: correct NULL three-valued logic`.
- [ ] **G1.WHERE.3** Comparison affinity: apply SQLite's affinity rules
  (src/expr.c `sqlite3Affinity`) so INTEGER-vs-TEXT compares numerically when
  appropriate. Commit: `G1.WHERE.3: comparison affinity`.
- [x] **G1.WHERE.4** IN with NULL in list; NOT IN semantics; IN subquery.
  (Session: fixed the where4 composite-PK + `$null` transpiler bugs; where4
  subtests now pass. IN-subquery and NOT-IN-with-NULL may still need work.)
  Commit: `G1.WHERE.4: IN/NOT IN with NULL`.
- [ ] **G1.WHERE.5** LIKE/GLOB/REGEXP: ASCII case-insensitivity, ESCAPE clause,
  pattern-too-big error. Commit: `G1.WHERE.5: LIKE/GLOB/REGEXP`.
- [ ] **G1.WHERE.6** COLLATE in predicate overrides column collation (BINARY/
  NOCASE/RTRIM). Commit: `G1.WHERE.6: COLLATE in WHERE`.
- [ ] **G1.WHERE.7** Error propagation: a bad WHERE expression raises the error
  rather than silently skipping rows (rowPassesWhere returns (bool,error)).
  Commit: `G1.WHERE.7: WHERE error propagation`.
- [ ] **G1.WHERE.8** testgen where–whereN green. Commit: `G1.WHERE.8: WHERE TCL green`.
- [ ] **G1.WHERE.9** (new, from session) Triage remaining `where` failures:
  where7 row count, where9 NULL/missing rows (pure-Go test first), the
  stateful `tclvar` flip in where-10.4 (document or add a
  `CreateFunction`-style public API), and the TEMP t1 duplicate. EXPLAIN-shape
  failures (where2) belong to G5.EXPLAIN — do not fix here.
  Commit: `G1.WHERE.9: remaining where failures`. 
- [ ] **G1.WHERE.10** (new, from session) `whereA` ordering/NULL mismatches —
  triage with a pure-Go test; likely DISTINCT/ORDER-BACK or NULL rendering.
  Commit: `G1.WHERE.10: whereA fixes`.

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
