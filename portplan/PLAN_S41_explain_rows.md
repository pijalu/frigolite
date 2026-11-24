# PLAN S41 — the EXPLAIN-row substrate (agent card, 2026-11-16)

Base: `2c9af99fa` (branch `s41-explain-rows`, worktree `/private/tmp/wt_s41`).
Bucket A of `TRIAGE_index_callback_2026-11-16.md`: ~45 leaves through ONE
engine surface (a plan-derived synthetic opcode stream) + two consuming
surfaces (the `explain()` TVF, the harness per-row/callback chain) + one
shared harness gap (runtime_expr command substitution).

Pre-fix failing name sets (python raw byte scan over
`/tmp/full_s33s34_merge_gate.log`, saved at `/tmp/s41_prefix_leaves.json`):

| family | leaves | names |
|---|---:|---|
| index9 | 19 | index9-1.1..1.5, 2.1..2.4, 3.1..3.5, 4.1..4.5 |
| indexexpr2 | 9 | 1.2, 3.4.5eqp, 4.120, 4.130, **4.200, 4.210, 4.220, 4.900**, 11.0 |
| where2 | 2 | **2.5, 2.5b** |
| bestindexF | 34 | **setup_4..13 (10) + 2.N.0 (10)** = this card; 2.N.1/2.N.2 + 1.4.1 = Bucket B (S43) |
| vacuum2 | 4 | **3.1, 3.3, 3.13** (runtime_expr command substitution) + 5.4 (out of scope) |

## Root contract

Plain `EXPLAIN <stmt>` renders a faithful opcode row stream (columns
`addr, opcode, p1, p2, p3, p4, p5, comment` — vdbe.c builds it; OP_OpenRead
is vdbe.c:4384). Our engine renders a 2-row stub
(`internal/execquery/explain.go:11 execExplain`: Init/Return with
`fmt.Sprintf("%T", ...)` leaking `*sql.SelectStmt`). Every leaf above dies on
that stub.

**The honest equivalent** for a tree-walking executor is the plan-node stream
the EQP renderer already derives (`execExplainQueryPlan` →
`planSingleTableNodesLoop`/`planJoin`/`planSortNodes`/`planSubqueryNodes`/
`planCompound`), rendered row-per-decision: each EQP node becomes the opcode
rows of the decision it represents (SCAN → OpenRead+Rewind+Next; SEARCH →
OpenRead+Seek; sorter node → SorterOpen (+Function rows for non-column ORDER
BY terms); DISTINCT node → OpenEphemeral+Found+IdxInsert; vtab node → VOpen+
VFilter). ONE derivation shared by EQP and the stream: the stream parses the
same plan-node detail strings the EQP renders (grammar below), so the two
surfaces cannot drift.

## Oracle pins (captured BEFORE implementation)

Oracle = `/usr/bin/sqlite3` 3.54.0 for plain-table streams +
`DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture` (3.51.0, the fixture
era) for the fixture contracts. Probes under `/tmp/s41/oracle/`.

**where2-2.5** `EXPLAIN SELECT * FROM x1, x2 WHERE x=1 ORDER BY random()`:
`1 SorterOpen 2 5 0 k(1,B) 0`, `10 Function 0 0 2 random(0) 0 r[2]=func()`
→ the flattened stream contains `" SorterOpen "` (opcode column) and
`" random"` (p4). **where2-2.6** (`ORDER BY abs(5)`): C optimizes the constant
ORDER BY out — NO SorterOpen, no abs anywhere (2.6/2.6b are green today via
the stub; the stream must keep them green → the constant-ORDER-BY
optimization must reach `planSortNodes`, where C's select.c removes constant
terms before the sorter is created).

**index9** (sqluses proc, `test/index9.test:20-30`):
`db eval "EXPLAIN $sql" a { if {$a(opcode)=="OpenRead"} { set ::T($a(p2)) 1 } }`
then `SELECT name FROM sqlite_master WHERE rootpage IN ($in) ORDER BY 1`.
Oracle streams (schema t1(x,y) + the four partial indexes; empty table):

| case | OpenRead p2 set | expect |
|---|---|---|
| 1.1 `x=? AND y=45` | {2=t1, 3=t1x} | t1 t1x |
| 1.2/1.3/1.4/1.5 | {2} | t1 |
| 2.1 `y=45 ORDER BY x` | {2,3} (t1x walk serves ORDER BY; no sorter) | t1 |
| 2.2 `y=-20111000111` | {2,3=t1x2} | t1 t1x2 |
| 2.3/2.4 | {2} (+sorter) | t1 |
| 3.1 `y=9223372036854775807` | {2,3=t1x3} | t1 t1x3 |
| 3.2 `y=9.223372036854776e+18` (REAL) | {2} | t1 |
| 4.1 `y=-9223372036854775808` (INT64 min) | {2,3=t1x4} | t1 t1x4 |
| 4.4 `-9223372036854775808=y` | {2,3=t1x4} | t1 t1x4 |

**The $y binding contract (testfixture 3.51.0 probes, tf_i9*.tcl)**: the
C's `db eval "EXPLAIN $sql"` passes `$y` through as a **bound parameter**
(tclsqlite.c dbPrepareAndBind binds named `$name` parameters from TCL scope
with the object type); partial-index usability (`whereUsablePartialIndex`,
where.c:3604) compares the CURRENTLY BOUND VALUE against the partial WHERE's
constant via `exprCompareVariable` (expr.c:6456) — value-equal implies,
QPSG (`sqlite3_db_config db QPSG 1`) disables it (expr.c:6484 "If the
SQLITE_EnableQPSG flag is set ... always returns false"). TEXT "45" (1.5's
`string range` result) never implies INTEGER 45; REAL 9.22e18 never implies
INT64 max; INT64 min implies INT64 min.

**Fixture SQL errors to hand-pin (converter-era)**: the fixture baked the
bound values as literals — 4.1/4.4 carry `y=-9.223372036854776e+18` (the
TCL-8-style REAL rendering of `[expr -9223372036854775808]`) but expect
`t1 t1x4`, which only the INTEGER literal `-9223372036854775808` produces
(TCL 9 testfixture probe: `[expr -9223372036854775808]` is wide=1,
`[expr 9223372036854775808]` is bignum, `[expr -9223372036854775809]` is
bignum). Hand-pins: restore the C's per-case `set y` values as
runtime_var_set steps + keep `$y` in the SQL; QPSG cases (3.4/3.5/4.5) keep
`$y` unexpanded (y absent from runtimeVars → the named-parameter text fails
the implication — the C-observable of QPSG without modeling QPSG); 1.5's
TEXT "45" hand-pins as `y='45'`; 1.4's unset y via runtime_var_reset.
`IN ($in)` is TCL-level substitution (tclsqlite binds only refs that SURVIVE
TCL substitution; the double-quoted arg substitutes `$in` into the TEXT) →
the harness expansion for that reference must stay RAW (`IN (2,3)`, not
`IN ('2,3')`) → new fixture flag `rawVarExpansion` honored by
expandTCLRuntimeVarsSQL.

**indexexpr2-4.2xx** (`ext/misc/explain.c` TVF): `UPDATE t2 SET b=b+1` opens
t2(2)+t2abc(3); `SET c=c+1` adds t2cd(4); `SET c=c+1, f=NULL` adds t2def(5)
→ the `Open%` decision is update.c's compile-time index-maintenance set
(indexColumnIsBeingUpdated: an index is opened iff its columns intersect the
SET column list). **4.900**: `SELECT * FROM explain WHERE rowid = 0` → `0 {}`
— no usable `sql=` constraint → explainFilter gets no TEXT argv → rc=DONE →
zero rows (explain.c:192-214).

**bestindexF** (`uses_idxinsert` proc, bestindex8.test:103 shape): 
`set vm [db eval "EXPLAIN $sql"]` (a FLAT list of all rows' columns — the
fixture's query_assign captures only the first row today, a second harness
gap) then `lsearch $vm IdxInsert`. `do_vtabsorter_test` also reads
`lindex $vm [ii+4]` = the VFilter row's **p4** (the idxStr) and ERRORS on any
SorterSort row. Fixture wants idx=1 ONLY for
`SELECT DISTINCT a, count(*) FROM t1 GROUP BY a`; idx=0 for the other nine
shapes (the tcl module consumes the distinct/orderby handshake; the plain-
table oracle answers [0,1,0,1,0,0,0,0,0,1] — the module handshake changes
2.3/2.5). The `::res` leaves (2.N.1) and the query rows (2.N.2) are Bucket B
(S43), not this card.

**vacuum2-3.x**: `[file size test.db]/$pageSize` in runtime_expr — command
substitution + the `file size` command (resolving the lifecycle test.db).

## Stage 1 — the EXPLAIN row stream (engine)

New file `internal/execquery/explain_rows.go` (explain.go is at the 1000-line
hard-cap boundary). `execExplain` (explain.go:11) delegates to the new
builder.

1. **Program frame**: row 0 `Init 0 <halt-addr> 0 "" 0 "Start at <halt>"`;
   body rows; `Halt 0 0 0 "" 0 ""`; `Transaction 0 0 <n> 0 1
   usesStmtJournal=0`; `Goto 0 1 0 "" 0 ""`. Addresses sequential from 0.
2. **Plan-node → rows** (parse the EQP detail grammar):
   - `SCAN <t>` → `OpenRead 0 <root(t)> 0 <nCol> 0 root=.. iDb=0; t` +
     `Rewind 0 <end> 0 "" 0` + `Next 0 <entry> 0 "" 1`.
   - `SCAN <t> USING [COVERING] INDEX <i>` → table OpenRead + index OpenRead
     (p4 `k(<n>,<colls>)`) + Rewind on the index cursor + Next.
   - `SEARCH <t> USING INDEX <i> (...)` → OpenRead(s) + `SeekGE` (+`IdxGT`)
     per the condition text; `SEARCH <t> USING INTEGER PRIMARY KEY (...)` →
     OpenRead table + `SeekRowid`.
   - `USE TEMP B-TREE FOR ORDER BY` → `SorterOpen 2 5 0 k(1,B) 0` + per
     non-column ORDER BY term a `Function 0 0 2 <fn>(0) 0 r[..]=func()` row
     (where2-2.5's ` random`), + SorterInsert/SorterSort/SorterData/
     SorterNext rows. `FOR GROUP BY` → same family. `FOR DISTINCT` →
     `OpenEphemeral 1 0 0 k(1,B) 8` + `Found 1 <x> 1 1 0 key=r[1]` +
     `MakeRecord` + `IdxInsert 1 2 1 1 16 key=r[2]` (C's UNORDERED dedup).
   - `SCAN <name> VIRTUAL TABLE INDEX <n>:<str>` → `VOpen` + `VFilter` (p4 =
     idxStr) — bestindexF's `::res` reads VFilter p4.
   - `CO-ROUTINE <a>` / `COMPOUND QUERY` / subquery nodes → nested bodies in
     node order.
3. **Constant-ORDER-BY optimization** (`explain.go planSortNodes` + the
   stream): skip ORDER BY terms that are constant expressions
   (`sqlite3ExprIsConstant` parity: literals, functions of constants); when
   ALL terms are constant → no sorter node at all. C: select.c removes
   constant ORDER BY terms before where.c sees them (where2-2.6 vs 2.5;
   vtabExplainPlan's nOrderBy walk skips constants the same way —
   where.c:1490-1493). This CHANGES EQP output for those queries (toward C)
   — sweep where/select EQP families.
4. **Partial-index subsumption** (where.c wherePartIdxExpr:3819 +
   whereUsablePartialIndex:3604): a partial index's `col=const` EQ conjunct
   implies the query's matching term — the subsumed column is NOT a residual
   WHERE constraint and the walk serves as the scan vehicle even when the
   query has an ORDER BY the index delivers (index9-2.1/2.2) and for
   WHERE-only queries (o7.db probe: `SCAN t1 USING INDEX t1x2`). Port:
   `partialIndexImplied`'s term matcher becomes the subsumption set; the
   order-serving path (`whereHasNonIndexConstraint`) and the walk candidate
   (scanLoopForQuery's loopNone fallback) consult it. Value-equality rule:
   text-normalized comparison of the NORMALIZED terms (INTEGER literals
   match their own text; TEXT-vs-INTEGER never matches — the quoted literal
   keeps its quotes; REAL-vs-INT never matches).
5. **DML streams** (UPDATE/INSERT/DELETE, from `explainDMLPlan` + the
   executor's maintenance decision): Init/Null/OpenWrite table (p4 nCol)/
   Rewind/Once/OpenWrite per maintained index/IdxDelete+IdxInsert body rows/
   Halt/Transaction/Goto. The maintained-index set = the compile-time
   SET-column ∩ index-columns decision (update.c indexColumnIsBeingUpdated) —
   indexexpr2-4.200/4.210/4.220.

Validate each stage against the pinned streams BEFORE moving on.

## Stage 2 — the explain() TVF (engine)

Eponymous vtab module (ext/misc/explain.c):
- Schema `CREATE TABLE x(addr,opcode,p1,p2,p3,p4,p5,comment,sql HIDDEN)`
  (explain.c:93).
- xFilter (explain.c:192-214): the hidden `sql` arg wraps as `EXPLAIN %s`,
  prepares, steps; non-TEXT/absent argv → rc=DONE → zero rows (4.900).
- xBestIndex (explain.c:225): a usable EQ on `sql` → argvIndex=1, omit=1,
  cost 10.0, idxNum=1; unusable constraints on `sql` → SQLITE_CONSTRAINT.
- xRowid = addr (explain.c:169).
- Registered as an eponymous module named `explain` — queryable as
  `FROM explain('...')` and `FROM explain` (4.900). Same stream as Stage 1.
- The fixture's `load_static_extension db explain` must not fail (the
  converter/harness step already tolerated or no-ops it — verify).

## Stage 3 — the harness: explain_rows step + runtime_expr command substitution

1. **runtime_expr command substitution** (the shared gap; closes vacuum2-3.x
   and the bestindexF/vacuum2 fixtures' `[...]` forms): in
   `tclExprParser.parsePrimary`'s `[` case (harness_expr_test.go:389), parse
   `[cmd arg...]` with Tcl semantics (command substitution first, $vars in
   args), evaluate through a new harness command layer with GENERAL Tcl
   semantics: `lsearch`, `join`, `array names`, `file size`, `expr` (nested
   evalHarnessTclExpr). Unknown command → ERROR (loud, the subst lesson).
2. **`explain_rows` step type** (index9's per-row callback): JSON
   `{type:"explain_rows", sql, variable:"a", body}`. Harness arm: run the
   EXPLAIN query; build one `tcl.NewInterp()` (the converter interpreter the
   UDF callbacks already use); push present runtimeVars (S25 push-before-
   invoke); per row set the array elements `a(addr)...a(comment)`; Execute
   the body; pull written globals (NotifyScriptVarTargets + the array
   elements) back into runtimeVars. Body = arbitrary Tcl (`if {$a(opcode)==
   "OpenRead"} { set ::T($a(p2)) 1 }`).
3. **`array names` in the command layer**: reads the pulled `T(...)` runtime
   keys (the flat-key array model the tcl.Interp uses).
4. **rawVarExpansion** fixture flag: expandTCLRuntimeVarsSQL substitutes the
   value verbatim (the TCL double-quoted substitution semantics — `IN (2,3)`).
5. **Converter**: teach tclconvert the `db eval "EXPLAIN $sql" <arr> {body}`
   callback shape → explain_rows (with the `groupToTestCase` whitelist,
   tools/tclconvert/main.go:481) + the `set in [join [array names ::T] ,]`
   body value → runtime_expr with command substitution. index9.json is
   hand-pinned FIRST (the C source's exact step sequence + the $y restores)
   — a regen would re-emit the baked-value defect (S31 lesson).

## Stage 4 — where2 + bestindexF on top of the stream

- where2-2.5/2.5b: plain regex wants (zero harness work) — the stream's
  SorterOpen + random Function rows must appear; 2.6/2.6b must keep NOT
  matching (constant ORDER BY optimized out).
- bestindexF: query_assign(EXPLAIN) must capture the FLAT all-rows rendering
  (`db eval` semantics — the fixture's resultAll renders only the first row;
  add `resultFlat` + hand-pin the 10 setup steps) + the Stage-3 lsearch
  support closes setup_4..13. The 2.N.0 (idx) leaves need the vtab
  distinct/orderby plan handshake (IdxInsert presence per the module's
  consumed decisions) — attempt after 4a; if the handshake proves deeper
  than the remaining budget, record the residual for S43 honestly.

## Verification

Per stage (family-context runs, one each, bounded; NEVER the full suite):
`FRIGOLITE_TEST=index9`, `=indexexpr2`, `=where2`, `=bestindexF`,
`=vacuum2` — name-set diffs two directions vs `/tmp/s41_prefix_leaves.json`.
EXPLAIN-adjacent sweeps (the stream changes EXPLAIN output everywhere):
`FRIGOLITE_TEST=explain` (e_explain etc. if present), plus where/select EQP
families (`=where `, `=select`) because Stage 1 touches planSortNodes and
the partial-index eligibility — EQP assertions must stay UNCHANGED except
the constant-ORDER-BY class. `go test -c -o /dev/null .` + `go test -run
TestSOLID_ ./...` + TestS22_ after every engine stage. Quality gate
(gocognit ≤15, gocyclo ≤12) on every changed non-test file.

- **Stage 3 COMPLETE (2026-11-17)** — the harness surfaces:
  (1) the runtime command layer (frigolite_harness_expr_test.go):
  `[cmd ...]` substitution in runtime expressions with lsearch (Tcl's
  default -glob via the tcl package's StringMatch), lindex, join,
  `array names` over the flat-key runtime model, `file size` via the
  fixture lifecycle resolver, nested `expr` — unknown commands error
  loudly; runtime_expr routes command heads through the layer
  (evalHarnessRuntimeExpr). (2) the `explain_rows` step: the per-row Tcl
  callback over the EXPLAIN row stream (S25 push/pull over a fresh
  interpreter, `array unset ::T` reset at walk start, array-element pulls
  via the new tcl.Interp.ArrayKeys). (3) rawVarExpansion (verbatim $var
  substitution for the `IN ($in)` form) + runtime_var_unset. (4)
  query_assign: resultFlat (db eval's flat all-rows list) + plain-EXPLAIN
  captures (EXPLAIN QUERY PLAN keeps the converter-era exec-drop). (5) the
  converter: explain_rows/dbEval capture, set-[cmd] runtime_expr capture,
  unset→runtime_var_unset, whitelist — ALL gated on the new
  conversionMode flag (a runtime callback interpreter must EVALUATE
  `set ::box_geom [lindex $args 2]`, not record it — the rtgeom
  regression). Hand-pins: index9.json (the C's per-case $y restores + the
  sqluses sequence — 19 leaves GREEN) + bestindexF setup_4..13 resultFlat.
- **Stage 3 scoreboard**: index9 19→0, indexexpr2 9→5 (Bucket C residue),
  vacuum2 4→2 (3.3 residual: the auto_vacuum=FULL page-count class — the
  substitution gap itself closed; 5.4 carded), bestindexF 34→17 (setup_4..13
  + 2.2.0/2.6.0-2.11.0; the 2.3.0/2.4.0/2.5.0 DISTINCT-over-vtab handshake +
  the .1/.2 xBestIndex-globals + 1.4.1 = Bucket B/S43), where2 2→0 (Stage 1).
  Sweeps two-direction clean: where/select/index/vacuum universes
  NEW=0, date/notify1/where8/with1/rtree2/rtreedoc/tabfunc01/carray/dbstat/
  dbpage/keyword1 = baseline.

- **Stage 4 + the full gate (2026-11-17)** — where2 closed in Stage 1;
  bestindexF's 2.3.0/2.4.0/2.5.0 (the vtab DISTINCT/orderby handshake:
  where.c's eDistinct 1/2/3 → the module's idxStr-DISTINCT consumption →
  the ephemeral suppression) + the .1/.2/1.4.1 xBestIndex-globals = S43
  (Bucket B). The full-suite gate (2288s, no stuck tests) vs the 1,317-leaf
  baseline: **1281 failing, 61 GONE, 25 NEW — all 25 exposed vacuous
  passes**: the plain-EXPLAIN query_assign captures that the old exec path
  silently dropped. distinctagg's 18 C-table-backed hand-pins (the
  lsearch-OpenEphemeral pattern) fixed 13; the 5 residual
  (distinctagg-3.3.1/3.8.1/4.2.1/4.3.1/4.5.1) need the DISTINCT-aggregate
  ordered-input optimization (C skips the accumulator ephemeral only when
  the DISTINCT argument arrives sorted — select.c's ordered-input rule);
  trigger7-2.1..2.6 (6) need trigger SUB-PROGRAM rows in the stream (C's
  EXPLAIN UPDATE renders the matching triggers' compiled bodies — the
  string-match on the trigger SQL text); misc3-6.11-utf8 (1) needs
  expression-constant load rows (C's OP_Integer p1 / OP_Real p4 rendering
  of the SELECT-list constants). 12 exposed-failing leaves = the S41-2
  residual; the stream surfaces are honest, not vacuous.

## Carded / out of scope

- indexexpr2 1.2, 3.4.5eqp, 4.120, 4.130, 11.0 (Bucket C).
- bestindexF 2.N.1/2.N.2 + 1.4.1 (Bucket B — the xBestIndex global round
  trip + distinct/orderby EXECUTION; S43).
- vacuum2-5.4 (the query+catch mixed expectation shape).
- QPSG modeling (the harness hand-pins express the observable; a real QPSG
  planner flag is a separate engine unit if other fixtures need it).

## STATUS

- **Stage 1 COMPLETE (2026-11-17)** — `internal/execquery/explain_rows.go`:
  the synthetic opcode row stream. All DML (INSERT/UPDATE/DELETE) renders
  write-cursor streams through `emitDMLRows` (the WIP checkpoint's dispatch
  routed UPDATE through the read-scan path — fixed); the maintained-index
  set is the faithful update.c port (chngKey / indexWhereClauseMightChange /
  indexColumnIsBeingUpdated incl. expression-index column-reference walk at
  the tokenizer level — `exprReferencesChangedCols`). `explain.go`'s
  `execExplain` delegates to the stream; EQP and the stream share ONE plan
  derivation (`explainQueryPlanSelectNodes` / `explainDMLPlanNodes`).
- **Oracle pins ALL OK** (`tmp_probe` validation vs testfixture 3.51.0
  captures in /tmp/s41/oracle/ — tf_i9full.tcl et al.): index9's 15
  hand-pin literal forms (OpenRead p2 sets + SorterOpen presence),
  where2-2.5/2.5b/2.6 (SorterOpen + random p4), bestindexF plain shapes
  (IdxInsert presence), indexexpr2-4.200/4.210/4.220 (UPDATE maintained
  sets t2,t2abc / +t2cd / +t2def).
- **Constant-ORDER-BY optimization landed** (`allOrderTermsConstant` +
  `exprIsConstantExpr` in explain_rows.go, consulted by
  `orderByConsumedByLoop`): where2-2.6/2.6b keep NOT matching; integer
  ordinals (ORDER BY 1) are POSITIONAL in this engine's pipeline (C
  resolves them earlier) and never count as constants — EQP text and row
  order for the ordinal class are unchanged (base-probe verified).
- **where2-2.5/2.5b already closed** by the stream (plain regex wants
  SorterOpen + random p4 — no harness work needed; the plan's Stage 4
  target fell in Stage 1).
- Sweeps (python raw byte scan, leaf-only, two-direction vs
  /tmp/s41_baseline_universe.json built from the verified merge gate):
  where 4,290 leaves 0 fails (where2 2 GONE = the targets);
  select 9,036 leaves NEW=0 GONE=0; index universe (bestindex*/index*/
  autoindex*/e_reindex) NEW=0 GONE=0; vacuum NEW=0 (vacuum-into-500 GONE =
  the carded watchlist dither). No `explain`-named families exist in the
  fixture set — EXPLAIN assertions ride the where/select/index families.
- **Stage 2 COMPLETE (2026-11-17)** — `internal/vtab/explain.go` +
  `internal/exec/vtab_explain.go` + registration: the explain() TVF port of
  ext/misc/explain.c (EponymousOnly module, ValueModule for typed args —
  non-TEXT argv → zero rows, HiddenConstraintSetter for the sql= WHERE form,
  rowid = addr). **indexexpr2-4.200/4.210/4.220/4.900 green** (the family is
  9 → 5; the residue is Bucket C).
- **Eponymous shadowing rule added**: a REAL schema table/view named like a
  module wins over the eponymous FROM fallback (sqlite3FindTable resolves
  before the eponymous registry) — keyword1-explain.1's
  `CREATE TABLE explain(explain explain)` had been shadowed by the new TVF.
  Guard added to all four probes (execquery eponymousModuleResolvable/
  eponymousModuleColumnNames, exec tryMaterializeEponymousVtab/
  eponymousVtabColDefs). Sweeps: tabfunc01 0, with2 =baseline 6, join8
  =baseline 2, carray 0, dbstat-substring =baseline, dbpage 0, keyword1
  green, indexexpr NEW=0 GONE=4.

## 2026-11-24 — S44 slice 1: distinctagg residue closed (5→0)

`internal/execquery/explain_rows.go` emits the aggregate-DISTINCT
`OpenEphemeral` opcode rows C's `resetAccumulator` adds for each
`iDistinct>=0` aggregate. The decision mirrors `WHERE_DISTINCTBY`:
a rowid argument, a WHERE-pinned constant, or a plain column after the
index's equality + GROUP BY prefix needs no dedup b-tree; GROUP BY is an
ORDER prefix, not an equality binding (distinctagg-4.2.1 caught that
mistake). `frigolite_distinctagg_pin_test.go` pins the engine-visible
EXPLAIN contract. Full gate `/tmp/s44_full.log`: 1164 leaves, distinctagg
5→0, NEW=0 except the documented temptable2 flake. See
`portplan/PLAN_S44_distinctagg_explain.md`.

## 2026-11-24 — S45 slice 1: trigger7-2.x closed (6→0)

`internal/execquery/explain_trigger_rows.go` renders firing UPDATE OF
trigger sub-programs into the synthetic EXPLAIN stream; the converter now
emits a runtime `string match` assertion for a trailing do_test command
that matches a captured `db eval` value, and the harness command layer
dispatches `string match` through the converter's Tcl matcher. Full gate
`/tmp/s45_full.log`: 1158 leaves, trigger7 6→0, NEW=0. See
`portplan/PLAN_S45_trigger7_explain.md`.
