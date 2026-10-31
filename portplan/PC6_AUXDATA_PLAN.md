# PC6 — sqlite3_get_auxdata / sqlite3_set_auxdata (P-C6)

Unit: per-statement, per-call-site, per-argument auxdata slots plus the
context-bearing UDF registration surface the harness needs for the
test_func.c fixture functions (test_auxdata, test_destructor_count,
test_isolation, test_frombind).

## Observable contract (from the C)

- Storage: `Vdbe.pAuxData` — a list of `AuxData{iAuxOp, iAuxArg, pAux,
  xDeleteAux}` (vdbeInt.h:374). Key: (VDBE instruction address of the
  OP_Function site, argument index). `iAuxArg < 0` = statement-global cache
  shared by all function sites (undocumented; vdbeapi.c:1151/1182).
- `sqlite3_get_auxdata(ctx, i)`: first entry with
  `iAuxArg==i && (iAuxOp==ctx->iOp || i<0)`, else NULL (vdbeapi.c:1163).
- `sqlite3_set_auxdata(ctx, i, p, del)`: find-or-create the (iOp, i) entry;
  replacing runs the old destructor; CREATING a slot sets `pCtx->isError=-1`
  (a soft flag, vdbeapi.c:1213).
- Post-call (vdbe.c OP_Function ~8824): if `pCtx->isError != 0` (soft flag OR
  real error), `sqlite3VdbeDeleteAuxData(db, &p->pAuxData, iOp, pOp->p1)`
  deletes every slot at this site whose argument was NOT compile-time
  constant (`iAuxArg>31 || !(P1 & bit(iAuxArg))`, vdbeaux.c:3702). Then
  isError resets; a real error propagates after the deletion. Net effect:
  constant-argument slots persist across rows of one statement execution;
  non-constant slots are fresh every call. Within a single invocation
  get-after-set sees the value (json.c parse cache relies on it).
- Teardown: `closeAllCursors` (statement halt/reset) destroys ALL auxdata
  with destructor callbacks (vdbeaux.c:2852) — a re-executed statement starts
  with empty slots (func.test 13.8.1-3).
- P1 constant mask (expr.c TK_FUNCTION ~5345): bit i set iff
  `sqlite3ExprIsConstant(arg_i)` (eCode=1 walker, expr.c:2534): literals
  (int/float/string/blob/NULL) constant; TK_VARIABLE (bound parameter)
  constant (func.test 13.7/13.8.4); TK_COLUMN/DQS identifiers non-constant;
  subqueries non-constant (SelectWalkFail); operators constant iff all
  operands constant; nested TK_FUNCTION constant iff the FuncDef carries
  SQLITE_FUNC_CONSTANT (deterministic) with constant args
  (exprNodeIsConstantFunction). A fully-constant non-deterministic call is
  never factored out (okConstFactor requires the CONST flag), so
  `SELECT test_auxdata('hello world') FROM t4` calls per row → {0 1}.
- test_auxdata observable (test_func.c:185): per argument: get non-NULL →
  '1' (asserts the stored text equals the current argument text), else '0';
  then set a copy of the argument text. NULL arguments are skipped entirely.

func.test expectations to mirror: 13.1 `0`; 13.3 `0 1`; 13.4 `{{0 0} {1 1}}`
(two constants); 13.5/13.6 `{{0 0} {1 0}}` (const + column / const-expr +
column); 13.7 `{{0 0} {1 0}}` (bound param + column); 13.8.1-3 each fresh
execution restarts `0 1`.

## Design

- `function.FuncContext` (new file internal/function/funcctx.go): the
  sqlite3_context stand-in exposing `GetAuxData(arg)` / `SetAuxData(arg, v,
  destroy)`; `AuxStore` port interface implemented by the evaluator (DIP —
  function stays below execexpr). Tracks the C isError=-1 soft flag as
  `AuxSlotCreated()`.
- `function.Func.ContextFn func(*FuncContext, []interface{}) (interface{},
  error)` + `Registry.RegisterContext` (user-set registration; shadows
  builtins like Register).
- `execexpr.Evaluator.siteAux map[auxKey]auxEntry` (new file
  internal/execexpr/auxdata.go): `auxKey{site *sql.FuncCall node, arg int}`;
  `site==nil` key = the negative-arg statement-global cache. Statement-
  scoped exactly like stmtAux/collMemo (the Evaluator is engine-scoped;
  node addresses may be reused by later statements, so the map is cleared
  at the statement boundary — types.go comment).
- Dispatch (expression_eval.go evalScalarFuncCall): when `fn.ContextFn !=
  nil`, build `NewFuncContext(ev, f)`, call, then if the call errored OR a
  new slot was created run the C post-call release:
  `releaseNonConstSiteAux(f, mask)` (mask = per-arg sqlite3ExprIsConstant;
  args >31 always released, mirroring the 32-bit P1).
- `ResetStatementAux` fires every stored destructor, then drops the map —
  statement-start clearing is observationally equivalent to C's halt-time
  clearing because Exec runs statements to completion synchronously (same
  pattern as stmtAux/collMemo).
- Known documented deviations (consistent with the existing evaluator
  model): nested-function constness uses only the registry's
  `Deterministic` flag (builtin FuncDefs do not carry a CONST flag today);
  auxdata is shared with trigger-nested statements (no VdbeFrame
  save/restore); aggregates do not receive a FuncContext.

## Files

1. NEW internal/function/funcctx.go — FuncContext + AuxStore.
2. internal/function/function.go — Func.ContextFn, Registry.RegisterContext.
3. NEW internal/execexpr/auxdata.go — storage, release rule, const mask.
4. internal/execexpr/types.go — siteAux field, ResetStatementAux teardown.
5. internal/execexpr/expression_eval.go — ContextFn dispatch branch.
6. NEW internal/execexpr/auxdata_pin_test.go (package execexpr_test —
   imports exec/pager/parse; no cycle) — engine Exec-path pin of the 13.x
   contract with a test_auxdata-equivalent UDF.

## Verification

- go build ./... ; go vet ./internal/execexpr/ ./internal/function/ — OK
- go test ./internal/execexpr/... ./internal/function/... -count=1 — OK
  (9 new pin tests green; pre-existing function-package tests green)
- go test -run TestSOLID_ . -count=1 — OK
- staticcheck ./internal/execexpr/ ./internal/function/ — clean
- tools/quality_gate.sh on the changed non-test files — OK after
  decomposing exprIsConstant into constFuncCall + constOperandExprs/
  constBinaryOperands/constUnaryOperand/constVariadicOperands
  (gocognit 35/gocyclo 41 → within limits; the evalFuncCall gocyclo 13
  warning is pre-existing at HEAD, not this unit's dispatch).
- Family leaf diff vs baseline (2026-10-31, isolated working dir +
  symlinked testdata): func family (func+func2..9) 63 leaves IDENTICAL,
  fts 95 leaves IDENTICAL, misc 17 leaves IDENTICAL; tkt re-run in
  progress at plan time (baseline 0 failures). The 13.x/25.1/32.x leaves
  stay red until the harness owner registers the fixture UDFs via
  db.engine.Functions().RegisterContext — the engine surface is in place
  and pinned natively (internal/execexpr/auxdata_pin_test.go).
