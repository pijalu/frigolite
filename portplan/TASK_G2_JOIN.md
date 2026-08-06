# TASK G2.JOIN — JOIN (INNER/LEFT/RIGHT/FULL/CROSS/NATURAL, USING, ON)

> **Phase**: G2 (query features).
> **Goal**: G2.JOIN.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1 stable (SELECT, WHERE, EXPR).
> **Current state: FAILING** — `join`, `joinA`, `joinB` fail.

## Objective
All JOIN forms match SQLite: `[INNER | LEFT [OUTER] | RIGHT [OUTER] | FULL
[OUTER] | CROSS] JOIN`, `NATURAL JOIN`, `JOIN ... USING (cols)`, `JOIN ... ON
expr`, comma joins (cross), qualified `t.col` resolution, `SELECT *` column
ordering across joins, NULL-filling for outer joins, and the USING column
deduplication. Also correct column-name resolution ambiguity errors
("column x is ambiguous").

## Scope — testgen packages
`join`, `joinA`, `joinB`, `joinC`, `joinD`, `joinE`, `joinF`, `joinH`, `joinI`.
(`joinD` is large/slow — large multi-table joins; coordinate with index work.)

## Pre-test file
`frigolite_p2_join_test.go` — `TestP2Join_*`. Cases vs oracle:
- INNER / LEFT / RIGHT / FULL / CROSS joins; NULL-filling on the deficient side.
- NATURAL JOIN (auto USING on common columns; column dedup in output).
- JOIN USING (cols) — dedup + ON-equivalent predicate; qualified vs unqualified
  access to USING columns.
- JOIN ON expr (arbitrary predicate, incl. non-equi).
- Comma join (cross) + WHERE.
- `SELECT *` column order: left table cols then right; aliases for duplicate names.
- Ambiguous column error; resolution of `t.col`.
- Three-or-more table chains; self-joins (aliases required).

## SQLite source references
- `src/select.c` — `sqlite3Select`, join enumeration, WHERE/ON flattening,
  outer-join NULL filling, USING column handling.
- `src/expr.c` — qualified name resolution.

## Steps
- [ ] **G2.JOIN.1** Pre-test suite. Commit: `G2.JOIN.1: JOIN pre-test suite`.
- [ ] **G2.JOIN.2** Triage join/joinA/joinB failures via pure-Go tests; likely
  NULL-filling on LEFT/FULL join or `SELECT *` column ordering. Fix
  `internal/exec/select.go`. Commit: `G2.JOIN.2: outer-join NULL filling + star order`.
- [ ] **G2.JOIN.3** RIGHT / FULL JOIN (NULL-fill on both sides; matched + unmatched).
  Commit: `G2.JOIN.3: RIGHT/FULL JOIN`.
- [ ] **G2.JOIN.4** NATURAL JOIN + USING: auto-detect common columns, dedup output,
  ON-equivalent predicate. Commit: `G2.JOIN.4: NATURAL + USING`.
- [ ] **G2.JOIN.5** Qualified column resolution + ambiguous-column error.
  Commit: `G2.JOIN.5: column resolution + ambiguity errors`.
- [ ] **G2.JOIN.6** Self-joins (table aliases); 3+ table chains.
  Commit: `G2.JOIN.6: self/multi joins`.
- [ ] **G2.JOIN.7** joinD large/slow joins: correctness over speed; ensure
  termination + correct results (index acceleration is G3.INDEX). Commit:
  `G2.JOIN.7: large join correctness`.
- [ ] **G2.JOIN.8** testgen join–joinI green. Commit: `G2.JOIN.8: JOIN TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 -timeout 120s ./testgen/join/ ./testgen/joinA/ ./testgen/joinB/ ./testgen/joinC/ ./testgen/joinD/ ./testgen/joinE/ ./testgen/joinF/ ./testgen/joinH/ ./testgen/joinI/ && \
go test -run 'TestP2Join' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "All JOIN forms match SQLite: INNER/LEFT/RIGHT/FULL/CROSS, NATURAL, USING (col dedup), ON expr, comma cross, qualified column resolution + ambiguity errors, SELECT * ordering, NULL-filling for outer joins, self/multi joins. join/joinA/joinB currently FAIL. See portplan/TASK_G2_JOIN.md." \
  completionCriterion "testgen join-joinI PASS and TestP2Join pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 -timeout 120s ./testgen/join/ ./testgen/joinA/ ./testgen/joinB/ ./testgen/joinC/ ./testgen/joinD/ ./testgen/joinE/ ./testgen/joinF/ ./testgen/joinH/ ./testgen/joinI/ && go test -run TestP2Join -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G2.JOIN. join/joinA/joinB FAIL. Engine: internal/exec/select.go. USING/NATURAL
dedup output columns. NULL-fill deficient side for LEFT/RIGHT/FULL.
Decisions: correctness first; joinD index acceleration belongs to G3.INDEX.
Next: pre-tests, triage NULL-filling + star order, then RIGHT/FULL + NATURAL/USING.
Risks: joinD is slow without index joins — keep timeout generous; don't optimize prematurely.
Carried limits: verifyCommand above (120s timeout).
```
