# TASK G2.VIEW — CREATE/DROP VIEW, view resolution & expansion

> **Phase**: G2 (query features).
> **Goal**: G2.VIEW.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.SELECT; G2.SUBQUERY.
> **Current state: FAILING** — `view` fails.

## Objective
Views match SQLite: CREATE VIEW (with optional column list), DROP VIEW [IF
EXISTS], view expansion (a SELECT against a view rewrites to the view's query
with the right column mapping), column-name resolution through views, views on
joins/aggregates, nested views, INSTEAD OF triggers on views (coordinate with
G3.TRIGGER), read-only enforcement (no INSERT/UPDATE/DELETE on a non-trigger
view), and `sqlite_master` storing the view definition. TEMP views scoping.

## Scope — testgen packages
`view`, `countofview`.

## Pre-test file
`frigolite_p2_view_test.go` — `TestP2View_*`. Cases vs oracle:
- CREATE VIEW; SELECT * FROM view; column list on CREATE VIEW.
- View over join/aggregate/subquery; nested views.
- Column-name resolution through views; ambiguity.
- Read-only: INSERT/UPDATE/DELETE on a plain view → error.
- DROP VIEW; DROP VIEW IF EXISTS; error if dependent objects exist.
- TEMP view scoping vs main.
- `sqlite_master` view definition text.

## SQLite source references
- `src/build.c` — `sqlite3CreateView`, `sqlite3ViewGetColumnNames`.
- `src/select.c` — view expansion (`sqlite3Select` with view subquery).
- `src/trigger.c` — INSTEAD OF triggers on views.

## Steps
- [ ] **G2.VIEW.1** Pre-test suite. Commit: `G2.VIEW.1: view pre-test suite`.
- [ ] **G2.VIEW.2** Triage `view` failure via pure-Go test. Likely column
  mapping during expansion or view-over-join star resolution. Fix
  `internal/exec/select.go` + `internal/schema/`. Commit: `G2.VIEW.2: view expansion + column mapping`.
- [ ] **G2.VIEW.3** CREATE VIEW column list; nested views. Commit:
  `G2.VIEW.3: view column lists + nesting`.
- [ ] **G2.VIEW.4** Read-only enforcement + error text. Commit:
  `G2.VIEW.4: view read-only`.
- [ ] **G2.VIEW.5** DROP VIEW + dependency errors; TEMP scoping. Commit:
  `G2.VIEW.5: DROP VIEW + scoping`.
- [ ] **G2.VIEW.6** testgen view + countofview green. Commit: `G2.VIEW.6: view TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/view/ ./testgen/countofview/ && \
go test -run 'TestP2View' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Views match SQLite: CREATE VIEW (incl column list), DROP VIEW [IF EXISTS], view expansion with column mapping, name resolution through views, views over joins/aggregates, nested views, read-only enforcement, INSTEAD OF triggers (coordinate G3.TRIGGER), TEMP scoping. view currently FAILS. See portplan/TASK_G2_VIEW.md." \
  completionCriterion "testgen view, countofview PASS and TestP2View pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/view/ ./testgen/countofview/ && go test -run TestP2View -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G2.VIEW. view FAILS. Expansion in internal/exec/select.go; schema in internal/schema.
Column mapping = view columns → underlying SELECT output columns.
Decisions: INSTEAD OF triggers covered jointly with G3.TRIGGER.
Next: pre-tests, triage expansion/column-mapping, then nesting + read-only.
Risks: views over views over aggregates stress column resolution.
Carried limits: verifyCommand above.
```
