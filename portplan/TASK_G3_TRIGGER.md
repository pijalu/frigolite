# TASK G3.TRIGGER — CREATE/DROP TRIGGER, trigger firing, OLD/NEW

> **Phase**: G3 (schema & constraints).
> **Goal**: G3.TRIGGER.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.INSERT/UPDATE/DELETE; G2.VIEW (INSTEAD OF).
> **Current state: FAILING** — `trigger` fails.

## Objective
Triggers match SQLite: CREATE TRIGGER BEFORE/AFTER/INSTEAD OF on
INSERT/UPDATE/DELETE (incl. OF <cols> for UPDATE), FOR EACH ROW (only row-level
in SQLite), the `OLD`/`NEW` row contexts, `WHEN` clause, trigger body (a
statement list), DROP TRIGGER [IF EXISTS], recursive_triggers setting (default
OFF → no same-table recursion, but cross-table chaining fires), TEMP triggers,
and the chained-trigger stack (a trigger's action can fire triggers on *other*
tables). Error in a trigger body → statement rollback.

## Scope — testgen packages
`trigger`, `triggerA`, `triggerB`, `triggerC`, `triggerD`, `triggerE`,
`triggerF`, `triggerG`, `temptrigger`, `triggerupfrom`. (`triggerfault` → N/A.)

## Pre-test file
`frigolite_p3_trigger_test.go` — `TestP3Trigger_*`. Cases vs oracle:
- BEFORE/AFTER INSERT/UPDATE/DELETE; OLD/NEW access in body.
- UPDATE OF <cols> (fires only when listed column changed).
- WHEN clause gating.
- Trigger body with multiple statements; body errors → rollback.
- INSTEAD OF on a view (coordinate G2.VIEW).
- Cross-table chaining (trigger A on t1 inserts into t2 → trigger B on t2 fires).
- Same-table recursion blocked when recursive_triggers OFF (default).
- DROP TRIGGER; IF EXISTS; TEMP trigger scoping.
- `changes()` / `last_insert_rowid()` inside triggers (coordinate functions).

## SQLite source references
- `src/trigger.c` — `sqlite3CreateTrigger`, trigger list, code generation.
- `src/insert.c`, `update.c`, `delete.c` — trigger invocation points.
- `src/main.c` — `recursive_triggers` setting.

## Steps
- [ ] **G3.TRIGGER.1** Pre-test suite. Commit: `G3.TRIGGER.1: trigger pre-test suite`.
- [ ] **G3.TRIGGER.2** Triage `trigger` failure via pure-Go test. Recent work
      added a `triggerTables` stack for chaining; verify OLD/NEW + WHEN. Fix
      `internal/exec/insert.go`/`update.go`/`delete.go` + trigger dispatch.
      Commit: `G3.TRIGGER.2: OLD/NEW + WHEN + chaining`.
- [ ] **G3.TRIGGER.3** UPDATE OF <cols> selectivity. Commit: `G3.TRIGGER.3: UPDATE OF`.
- [ ] **G3.TRIGGER.4** INSTEAD OF on views (coordinate G2.VIEW). Commit:
      `G3.TRIGGER.4: INSTEAD OF triggers`.
- [ ] **G3.TRIGGER.5** recursive_triggers OFF default (no same-table recursion;
      cross-table chaining allowed). Commit: `G3.TRIGGER.5: recursion control`.
- [ ] **G3.TRIGGER.6** Trigger body errors → statement rollback. Commit:
      `G3.TRIGGER.6: trigger error rollback`.
- [ ] **G3.TRIGGER.7** triggerA–G + temptrigger + triggerupfrom green.
      Commit: `G3.TRIGGER.7: trigger TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/trigger/ ./testgen/triggerA/ ./testgen/triggerB/ ./testgen/triggerC/ ./testgen/triggerD/ ./testgen/triggerE/ ./testgen/triggerF/ ./testgen/triggerG/ ./testgen/temptrigger/ ./testgen/triggerupfrom/ && \
go test -run 'TestP3Trigger' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Triggers match SQLite: CREATE/DROP TRIGGER, BEFORE/AFTER/INSTEAD OF, INSERT/UPDATE(OF cols)/DELETE, FOR EACH ROW, OLD/NEW row contexts, WHEN clause, multi-statement body, cross-table chaining (recursive_triggers OFF default blocks same-table recursion only), TEMP scoping, body-error statement rollback. trigger currently FAILS. See portplan/TASK_G3_TRIGGER.md." \
  completionCriterion "testgen trigger, triggerA-G, temptrigger, triggerupfrom PASS and TestP3Trigger pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/trigger/ ./testgen/triggerA/ ./testgen/triggerB/ ./testgen/triggerC/ ./testgen/triggerD/ ./testgen/triggerE/ ./testgen/triggerF/ ./testgen/triggerG/ ./testgen/temptrigger/ ./testgen/triggerupfrom/ && go test -run TestP3Trigger -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G3.TRIGGER. trigger FAILS. triggerTables stack exists for chaining (insert.go). OLD/NEW
set during the triggering row scan. recursive_triggers OFF default (block same-table recursion).
Decisions: INSTEAD OF triggers done jointly with G2.VIEW.
Next: pre-tests, triage trigger (OLD/NEW/WHEN/chaining), then UPDATE OF + recursion control.
Risks: trigger ordering (multiple triggers on same event fire in creation order); body-error rollback.
Carried limits: verifyCommand above.
```
