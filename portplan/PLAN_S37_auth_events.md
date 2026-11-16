# PLAN — S37: authorizer / update-hook event collection (bucket F, 10 leaves, 3 families)

Base: 2c9af99fa (feature/perf), worktree /private/tmp/wt_s37, branch s37-auth-events.
Triage: portplan/TRIAGE_index_callback_2026-11-16.md §7 bucket F.

## Targets (the slice)

- savepoint-9.1 / 9.2 / 9.3 — authorizer records SQLITE_SAVEPOINT events.
- without_rowid3-18.2 / 18.3 / 18.4 / 18.5 / 18.7 — authorizer records FK-driven
  SQLITE_INSERT/UPDATE + SQLITE_READ events.
- tkt-f7b4edec-1 / 3 — update-hook records {INSERT main t1 <rowid>}.

## Pre-fix failing sets (verified gate /tmp/full_s33s34_merge_gate.log,
python raw byte scan; saved /tmp/s37_prefix_fails.json)

| family | leaves | of which mine |
|---|---:|---|
| savepoint | 8 | 9.1/9.2/9.3 (5.x = bucket K, untouched) |
| without_rowid3 | 21 | 18.2/3/4/5/7 (17.1.x/19.x = bucket J; 18.8/18.11 IGNORE-FK = residual) |
| tkt-f7b4edec | 2 | both |
| fkey2 | 22 | 18.2/3/4/5/7 bonus (same shape as without_rowid3 §18; 18.8/18.11 residual; rest out) |
| fkey7 1 / schema 20 / schema2 5 / savepoint6 1 / savepoint7 3 | | regressions to watch only |
| alterauth, alterauth2, altercons2, analyze3, fts4aa, tclsqlite | 0 | green baselines (auth_proc users) |

auth/auth2/auth3/hook are `unsupportedTestFiles` (harness:1440-1442, :1825) —
no harness regression surface there.

## Diagnosis (verified against fixtures + engine + C source)

1. **Harness gap (S25 shape)**: `harnessTclAuthorizer`
   (frigolite_tcl_authorizer_test.go) runs the fixture proc in a PRIVATE
   tcl.Interp; the body's `lappend ::authdata ...` never reaches
   `runtimeVars`, so the runtime_var step reads "". Fix: the S25 push/pull
   round trip (`tcl.NotifyScriptVarTargets` over the proc bodies; push
   present runtime values before each invoke, pull written values after).
2. **Stale fixtures**: the section-level/in-body `set ::authdata [list]` /
   `set authargs {}` resets were dropped by the OLD converter — the current
   converter emits them (regen verified: savepoint-9.x reset steps; setup_43
   tail / setup_44 / setup_45 / 18.5 / 18.7 for without_rowid3). Hand-pin the
   reset steps; do NOT wholesale-install the drifted regens (S30/S32 lesson).
3. **Engine gap**: zero `SQLITE_READ` call sites in the engine; UPDATE auth
   fires once with arg2="" instead of per changed column. C contract below.
4. **Converter gap**: `db1 update_hook update_hook` is silently dropped
   (tclconvert has no named-handle update_hook branch; tcl2go DOES —
   tools/tcl2go/processdb_part2.go:406). The engine surface EXISTS:
   Engine.SetUpdateHook + FirePreupdate→updateHook (internal/exec/preupdate.go:92).
5. savepoint-9.4-9.6 pass vacuously today (the catch arm only asserts
   res.Error != nil for that expect shape, harness:5370-5393) — adding
   collection cannot break them.

## C contract (citations, ../sqlite @3.51.0)

- auth.c:104-125 sqlite3AuthReadCol → xAuth(SQLITE_READ, zTab, zCol, zDb,
  pParse->zAuthContext); auth.c:136+ sqlite3AuthRead for TK_COLUMN (resolve)
  and TK_TRIGGER (OLD./NEW.).
- resolve.c:842 — sqlite3AuthRead per TK_COLUMN at name resolution.
- fkey.c:888 sqlite3FkCheck — **first loop = pTab as child** (pTab->u.tab.pFKey):
  skip `aChange && !self-ref && !fkChildIsModified`; per FK column
  **fkey.c:977-990 fires sqlite3AuthReadCol(parent table, parent col, iDb)**
  (before fkLookupParent / fkScanChildren). **Second loop = pTab as parent**
  (sqlite3FkReferences): skip `aChange && !fkParentIsModified`; skip
  `!deferred && !DeferFKs && !pToplevel && !isMultiWrite` (fkey.c:1025-1036 —
  "inserting a single row into a parent cannot cause or fix an immediate
  violation"); then fkScanChildren (fkey.c:547) resolves the scan WHERE
  `parent.key = child.key` → the CHILD-side TK_COLUMN fires SQLITE_READ(child
  table, child col) via resolve.c (the parent side is TK_REGISTER — no auth).
- FkCheck call sites (per-statement codegen): insert.c:1573
  FkCheck(0,regIns); update.c:1049 FkCheck(regOld,0) + update.c:1093
  FkCheck(0,regNew) (two passes); delete.c:832 FkCheck(iOld,0).
- Statement auth: insert.c:969 SQLITE_INSERT(table); update.c:508
  SQLITE_UPDATE(table, column|ROWID) **per changed column**; delete.c:393
  SQLITE_DELETE(table). All arg3=zDb, arg4=zAuthContext ("").
- update.c:1107 sqlite3FkActions → fkey.c:1416-1440: per child FK with
  parent-key modified, fkActionTrigger (fkey.c:1215-1407) builds the action
  trigger: step WHERE `old.<pk> = <ck>`; WHEN `NOT(old.<pk> IS new.<pk>)`
  (UPDATE only, fkey.c:1279-1291); SET `<ck> = new.<pk>` (CASCADE+UPDATE) /
  NULL (SET NULL) / default expr (SET DEFAULT); op = TK_UPDATE / TK_DELETE.
  pTrigger->zName = NULL (anonymous, malloc-zero) → arg4 = {}.
  codeRowTrigger (trigger.c:1272-1348): resolve WHEN first (2 parent-col
  reads), then the step: SET resolve → per-column stmt auth (update.c:508) →
  WHERE resolve (parent col + child col) → FkCheck ×2 (UPDATE) / ×1 (DELETE)
  first-loop parent-key reads → FkActions (recursion).
- getRowTrigger cache (trigger.c:1360): the trigger compiles ONCE per
  statement — the action-trigger events fire once per statement, not per row.
- 18.5 event stream verified against this model (11 events, exact order):
  stmt UPDATE nought b → scan(regOld) READ cross e → scan(regNew) READ cross e
  → trigger WHEN(2×nought b) → trigger SET(1×nought b) → trigger stmt UPDATE
  cross e → trigger WHERE(nought b, cross e) → trigger FkChecks(2×nought b).
- update.c:758/1319 set isMultiWrite for any non-single-pass UPDATE; the
  second-loop skip is insert-specific (its own comment) → the helper treats
  UPDATE/DELETE as multiWrite (never skips the scans), INSERT skips immediate
  scans on single-row parents.

## Changes

### E1. internal/execconstraint/auth_events.go (new)
`AuthFKEvents(kind, entry, ownCtx, changedCols []string, singleRowInsert bool)`
— walks the FK structure firing the compile-time events in C order:
- gate: `ForeignKeys() && HasAuthorizer()` (C: insert.c:1572 FK gate; auth.c xAuth).
- child side (first loop): fkResolveParentForCheck per FK; on success
  AuthorizeResult(ActionRead, parentTable, parentCol, dbName) per column —
  ×1 for INSERT/DELETE, ×2 for UPDATE (two FkCheck passes). UPDATE skip when
  the child key is not modified (fkChildIsModified) and not self-referential.
- parent side (second loop): per child FK referencing entry (walk all
  databases like validateChildrenParentKeys); skip UPDATE when the parent key
  is not modified; skip immediate on single-row INSERT; AuthorizeResult(
  ActionRead, childTable, childCol, dbName) per column per scan — ×1
  (INSERT/DELETE), ×2 (UPDATE).
- FkActions: per child FK with parent-key modified and action != NONE: fire
  the action-trigger compile events per action/kind (model above), recursing
  into the child UPDATE/DELETE's own first-loop reads (+ its second-loop
  scans + actions, depth-guarded).
- ConstraintContext gains `AuthorizeResult(auth.Action, ...)` and
  `HasAuthorizer() bool` (Engine implements both; dml_context.go:120-129).

### E2. UPDATE statement auth shape (internal/execdml/update_split.go)
Replace the single `Authorize(ActionUpdate, s.Table, "", "", "")`
(update_split.go:57) with the per-column loop in SET order after the target
and SET columns are known (update.c:508): arg2 = column name, "ROWID" when
the rowid changes; then fire AuthFKEvents("UPDATE", ...).

### E3. INSERT/DELETE anchors
- insert_exec.go prepareInsertStmt (after Authorize(ActionInsert)):
  AuthFKEvents("INSERT", ..., singleRowInsert) with the same singleRowInsert
  rule as validateDMLFKPrepare (engine_core_tail.go:281-284).
- delete.go (after Authorize(ActionDelete)): AuthFKEvents("DELETE", ...).

### H1. Harness authorizer sync (frigolite_tcl_authorizer_test.go)
newHarnessTclAuthorizer(step, runtimeVars): collect targets from
NotifyScriptVarTargets over each callbackProc body; Authorize: push present
runtime values → InvokeProcedure → pull targets back (name and ::name forms).
auth_proc handler passes runtimeVars.

### C1. Converter update_hook (tools/tclconvert/tcl/interp_cmds.go)
Named-handle branch += "update_hook": register (`db1 update_hook proc` → step
{type update_hook, Database, CallbackProcs/FunctionProc/FunctionBody, seed
trackedVars/dynamicVars via NotifyScriptVarTargets}), clear (`update_hook ""`),
bare query → UnsupportedCommandError (fail loudly). main.go:481 whitelist +=
"update_hook" (empty-SQL step type).

### H2. Harness update_hook handler (frigolite_harness_test.go)
`case "update_hook":` — resolve named/main DB; build the tcl interp
(procs + callback vars); db.SetUpdateHook(func(op, db, table, rowid)):
push targets → InvokeProcedure(proc, op, db, table, rowid) → pull targets.

### F1. Fixture hand-pins
- savepoint.json: prepend {"runtime_var_reset","authdata"} to 9.1/9.2/9.3
  (and 9.4/9.5/9.6 for C faithfulness; their compare ignores the prefix).
- without_rowid3.json: setup_43 append reset; insert setup_44=[reset] between
  18.2/18.3 and setup_45=[reset] between 18.3/18.4; 18.5 reset before the
  UPDATE exec; 18.7 reset before the INSERT INTO two exec.
- tkt-f7b4edec.json: regenerate (name-set must stay 7/7; the update_hook step
  + resets arrive by conversion; diff-verify).

## Residuals (carded, NOT this slice)
- 18.8/18.11 (+fkey2-18.8/18.11): SQLITE_IGNORE on FK parent-key reads must
  NULL the parent values (fkey.c bIgnore → fkLookupParent) — auth-RESULT
  semantics, not collection.
- RESTRICT action triggers fire a synthetic SELECT whose SQLITE_SELECT/READ
  events our select path does not emit (auth-7.x class, excluded families).
- General SQLITE_READ per SELECT column (resolve.c:842 over all expressions)
  — only needed by the excluded auth/auth2 families.

## Verification

```bash
# baseline leaf sets: /tmp/s37_prefix_fails.json (from the verified gate)
FRIGOLITE_TEST=savepoint     go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v > /tmp/s37_after_savepoint.log 2>&1
FRIGOLITE_TEST=without_rowid3 go test ... -timeout 20m -v > /tmp/s37_after_without_rowid3.log
FRIGOLITE_TEST=tkt-f7b4edec  go test ... -timeout 20m -v > /tmp/s37_after_tktf7b4.log
FRIGOLITE_TEST=fkey2         go test ... -timeout 20m -v > /tmp/s37_after_fkey2.log
# auth-adjacent regression sweep (auth_proc users + substring neighbors)
FRIGOLITE_TEST=auth          (runs auth/auth2/auth3 — unsupported, inert; sanity only)
FRIGOLITE_TEST=alterauth / fkey7 / fts4aa / altercons2 / analyze3 / schema / tclsqlite
# two-direction name-set diffs vs /tmp/s37_prefix_fails.json (python, -I)
go vet . && go test -run TestSOLID_ ./... && go test ./internal/auth/...
```

## Status

- [x] Investigation + pre-fix baselines recorded
- [x] Plan committed
- [ ] E1-E3 engine auth events
- [ ] H1 harness authorizer sync
- [ ] C1+H2 update_hook surface + fixture regen
- [ ] F1 fixture hand-pins
- [ ] Verification sweeps + two-direction diffs
- [ ] Lessons updated
