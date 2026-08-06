# TASK G1.INSERT — INSERT / UPSERT / VALUES

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.INSERT.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.CREATE (table structure).

## Objective
All INSERT forms match SQLite: VALUES (single + multi-row), column lists (subset
+ reordered), INSERT...SELECT, DEFAULT VALUES, UPSERT (`ON CONFLICT`), conflict
resolution (`OR IGNORE`/`OR REPLACE`/`REPLACE INTO`), RETURNING, default-value
evaluation, type affinity coercion, NULL/NOT NULL/UNIQUE/CHECK enforcement, and
exact error text on constraint violations.

## Scope — testgen packages
`insert`, `values`, `valuesfault`, `default_pkg` (note: `valuesfault` may
contain fault-injection cases — triage; keep the applicable ones, document the
rest in NOT_APPLICABLE.md).

## Pre-test file
`frigolite_p1_insert_test.go` — `TestP1Insert_*`. Cases vs oracle:
- Single-row, all columns; column-list subset; reordered column list.
- Multi-row VALUES; VALUES with expressions (`1+1`, `NULL`, `'a'||'b'`).
- INSERT...SELECT (incl. self-insert guard).
- DEFAULT VALUES; DEFAULT expr evaluation.
- OR IGNORE / OR REPLACE / REPLACE INTO on UNIQUE/PK conflict.
- UPSERT: `ON CONFLICT(a) DO NOTHING`; `DO UPDATE SET b=excluded.b WHERE ...`;
  `DO UPDATE SET` referencing `excluded.*` and the target row.
- RETURNING `*` and explicit columns (coordinate with G1.UPDATE/G1.DELETE which
  share the RETURNING code path).
- Affinity coercion: insert `'5'` into INTEGER; `3.0` into TEXT; etc.
- NOT NULL / UNIQUE / CHECK / FK violation → exact error text + statement rollback.
- Wrong column count → exact error.

## SQLite source references
- `src/insert.c` — `sqlite3Insert`, upsert, default-value eval, xfer optimization.
- `src/upsert.c` — upsert resolution.
- `src/update.c` — RETURNING shared machinery.

## Steps
- [ ] **G1.INSERT.1** Pre-test suite. Commit: `G1.INSERT.1: INSERT pre-test suite`.
- [ ] **G1.INSERT.2** Expression evaluation in VALUES (currently flagged in
  HANDOVER as ~8 failures). Fix `internal/exec/insert.go` so value expressions
  evaluate in the row context. Commit: `G1.INSERT.2: VALUES expression eval`.
- [ ] **G1.INSERT.3** UPSERT `DO UPDATE SET` with `excluded.*` + WHERE guard;
  multi-column conflict target. Commit: `G1.INSERT.3: full UPSERT semantics`.
- [ ] **G1.INSERT.4** Conflict-resolution (`OR IGNORE/REPLACE`, `REPLACE INTO`):
  delete-then-insert for REPLACE; exact behavior for WITHOUT ROWID. Commit:
  `G1.INSERT.4: conflict resolution`.
- [ ] **G1.INSERT.5** Default-value evaluation (literal + expr; CURRENT_TIME
  family if a column default). Commit: `G1.INSERT.5: default value eval`.
- [ ] **G1.INSERT.6** Statement-level rollback on any constraint error (snapshot
  pager, restore on error) — coordinate with G1.CONSTRAINTS.
  Commit: `G1.INSERT.6: statement rollback on insert errors`.
- [ ] **G1.INSERT.7** RETURNING (shared path; coordinate with UPDATE/DELETE).
  Commit: `G1.INSERT.7: INSERT RETURNING`.
- [ ] **G1.INSERT.8** testgen packages green (triage transpiler gaps first).
  Commit: `G1.INSERT.8: INSERT TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/insert/ ./testgen/values/ ./testgen/default_pkg/ && \
go test -run 'TestP1Insert' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "All INSERT forms match SQLite: VALUES (single/multi-row), column lists, INSERT...SELECT, DEFAULT VALUES, UPSERT (ON CONFLICT), OR IGNORE/REPLACE, RETURNING, affinity coercion, constraint errors with statement rollback. See portplan/TASK_G1_INSERT.md." \
  completionCriterion "testgen insert, values, default_pkg PASS and TestP1Insert pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/insert/ ./testgen/values/ ./testgen/default_pkg/ && go test -run TestP1Insert -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.INSERT. [done list + outputs]. Statement rollback snapshots all pagers
(internal/exec/engine.go). RETURNING code path shared with UPDATE/DELETE.
Decisions: REPLACE = delete+insert; upsert excluded.* resolution in insert.go.
Next: pre-tests, then fix VALUES expr eval + UPSERT, then testgen.
Risks: WITHOUT ROWID conflict semantics; FK interaction.
Carried limits: verifyCommand above.
```
