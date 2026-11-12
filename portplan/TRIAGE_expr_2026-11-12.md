# EXPR family census triage — 2026-11-12 (worktree wt_triage, branch s27-expr-triage, base feature/perf @ 965d510db)

Run: `FRIGOLITE_TEST=expr go test -run "^TestSQLiteSuite$" -count=1 -timeout 30m .`
in this worktree → /tmp/expr_triage_run.log. **COMPLETED**: `FAIL github.com/pijalu/frigolite 13.891s`
(the exit-FAIL is the expected residue), no panic/timeout.

## Substring sweep scope (the mandatory caveat)

FRIGOLITE_TEST is substring-based, so "expr" swept **17** fixture families
(e_expr, existsexpr, existsexpr2, expr, expr2, exprfault, exprfault2, expridx1,
expridx2, fts3expr, fts3expr2, fts3expr3, fts3expr5, indexexpr1-3, randexpr1).
Two are map-excluded (indexexpr1 = slowTestFiles, exprfault2 = unsupportedTestFiles).
Failing leaves appear in **7** families; expr2/exprfault/expridx2/fts3expr2/
fts3expr5/existsexpr*/randexpr1 pass. **The expr-family truth = the
TestSQLiteSuite/expr/ paths: only 3 leaves** — the census row "expr | 4 | 109"
is stale: 109 was the S5-era substring-group number and the S12–S21 slices
(e.g. whereD/searchcount, duplicate-fixture deletion) shrank it. Today's
substring-group residue = **98 leaf failures** (106 FAIL paths = 98 leaves +
8 parent lines), of which 52 are one fixture counted twice (see the
stale-fixture signature below).

Per-family leaf counts (unique leaves; fts3expr3 runs twice):

| family | leaves | notes |
|---|---|---|
| e_expr | 21 | |
| fts3expr3 | 26 ×2 = 52 | .json AND .json.gz both in testdata/ |
| expridx1 | 10 | source file gone from the 3.51.0 tree |
| indexexpr2 | 9 | |
| expr | 3 | the family truth |
| indexexpr3 | 2 | |
| fts3expr | 1 | |

## Stale-fixture signatures noticed during triage

1. **fts3expr3 runs twice**: `testdata/fts3expr3.json` and
   `testdata/fts3expr3.json.gz` BOTH exist — the same source converted twice,
   the .gz run renamed `fts3expr3#01` by t.Run dedup. The S14/S15
   duplicate-generation class; regenerate/delete the .gz copy first.
2. **expridx1's source does not exist in ../sqlite (3.51.0, fb2c931a)**:
   `test/expridx1.test` was removed upstream (merged into indexexpr1.test);
   its 4.3/4.6 expectations ("index z1b stores an imprecise floating-point
   value for row N") contain a diagnostic that **does not exist anywhere in
   the 3.51.0 tree** (`grep -r imprecise src/ ext/` = empty) — the fixture's
   oracle came from an older sqlite. Those two leaves are unfalsifiable
   against the current oracle → stale-oracle fossil.
3. **e_expr-5.1/5.2 expect is a frozen TCL substitution**: `"expect":
   "$as$bs"` — the source computed `set as [db one ...]` / `${as}${bs}` at
   runtime; the converter froze the unexpanded variable text.
4. **fts3expr3 andor wants are raw templates**: every `3.N#01` expect is the
   *unsubstituted* `balanced_andor_tree` template ("AND xxx xxx") because the
   converter's regsub drops the `-all` flag (class E3 below).

## Per-class table

Legend — attribution: **E** = engine gap, **C** = converter/tclconvert gap,
**F** = stale fixture (regen or N-A). Leaves are fixture names inside the
family; fts3expr3 leaves count ×2 while both fixture files exist.

### A. e_expr — 21 leaves, 9 classes

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| A1 | 8 | 21.1.1-21.1.4, 21.2.3, 21.3.1, 21.3.2, 22.1.1 | got [B] / varlist ["a b"] vs want {}/[a b c]/[C] | Per-case runtime-var init from `foreach {a b c} {0 0 0} break` dropped by the converter → the harness `var()` UDF reads stale globals; engine lazy-eval is likely correct (got shapes fit a shifted value set) | C | tools/tclconvert: emit `runtime_var` set steps for the `foreach {v…} {…} break` idiom; regen e_expr.json. Then re-check the varlist orders (21.1.2 wants "a b c" = all WHENs evaluated when all false — matches engine laziness) |
| A2 | 1 | 26.1.6 | evalcount got 8, want 5 | CASE-WITHOUT-BASE evaluates WHEN terms non-lazily: per row SQLite stops at the first true WHEN (1+2+2 = 5 ceval calls over 3 rows); ours re-evaluates | E | R-30638-59954; src/expr.c TK_CASE codegen (OP_Once per arm). Unit: internal/execexpr/expression.go:444 `evalCaseExpr` — evaluate each WHEN exactly once per row, stop at first true, never re-evaluate the chosen arm's guard |
| A3 | 2 | 27.3.2, 27.3.3 | got text vs want blob | CAST must derive its target affinity from the FULL substring rules (`shobblob_x`→blob, `abbLOb10`→blob), not exact names; oracle 3.54: `CAST('def' AS shobblob_x)`=blob, `CAST('x' AS shint_x)`=integer, `CAST('x' AS clob)`=text | E | src/build.c:1650 `sqlite3AffinityType` (CHAR/CLOB→TEXT, BLOB→BLOB, REAL/FLOA/DOUB→REAL, INT-suffix→INTEGER, else NUMERIC) + src/expr.c TK_CAST affinity (expr.c:~5131 `sqlite3ExprAffinity` case TK_CAST). Unit: internal/execexpr/expression_eval_tail.go:444 `evalCastExpr` — route the type name through an sqlite3AffinityType port |
| A4 | 1 | 28.1.2 | got [text g h i ] vs want [text g] | blob→text CAST with embedded NULs: oracle 3.54 gives typeof=text, **length=1**, quote='g' — the text value stops at the first NUL; ours substitutes/blanks through | E | sqlite3VdbeMemCast (vdbemem.c, SQLITE_AFF_TEXT arm) — truncate the text at the first NUL byte. Unit: evalCastExpr blob→text path |
| A5 | 2 | 32.1.8, 32.1.9 | got integer 9.22337e+15 vs want real | CAST float-syntax text to NUMERIC must stay REAL (only integer-literal text becomes INTEGER); oracle 3.54: typeof(CAST('9.223372036e15' AS NUMERIC)) = **real** | E | src/vdbemem.c sqlite3VdbeMemNumerify: Atoi64 the TEXT first; non-integer-syntax text → REAL, no lossless-int collapse. Unit: evalCastExpr NUMERIC arm |
| A6 | 3 | 37.4, 37.5, 37.10 | got [false true] vs [false false]; 37.10 got [false true] vs [true true] | Two truthiness bugs: (a) CASE WHEN text must use the numeric-PREFIX rule ('1english'→1→true — oracle-confirmed); (b) iif() must share CASE's coercion exactly ('engligh'/'0'→false — oracle-confirmed f/f/f/t) | E | src/expr.c sqlite3ExprTruthiness-ish path (CASE codegen) + src/func.c iifFunc. Units: evalCaseExpr WHEN-truth conversion via value.applyNumericAffinity (internal/value/compare.go:21); iif in internal/function/function.go must call the same helper |
| A7 | 1 | 11.7.1 | expected error "too many SQL variables", got success | Parameter auto-numbering must enforce SQLITE_MAX_VARIABLE_NUMBER (default **32766**, src/sqliteLimit.h:147): `SELECT ?32766, 22` pushes the next param to 32767 → prepare error | E | src/resolve.c resolveExprStep (parameter numbering) + sqlite3Limit.h. Unit: internal/parse or resolve slot assignment — error when the assigned index exceeds the limit |
| A8 | 2 | 5.1, 5.2 | got helloworld12345 (correct) vs want "$as$bs" | Frozen unexpanded expect (signature 3): converter must evaluate `${as}${bs}` through runtime vars | C | tools/tclconvert: expand expect strings against runtime vars (or emit a runtime-computed expect step); regen |
| A9 | 1 | 32.2.8 | got integer 9000000000000000000 vs want real 9.0e+18; plus 9.22337203685478e+18 vs 9.2233720368547758e+18 | (a) same class as A5 (float-syntax input must classify REAL); (b) the want renders reals through the TCL oracle pipeline (17-digit %.17g-style) while our got renders %.15e — the harness compare layer must render engine reals the TCL way | E+C | same as A5 + the harness real-rendering layer (the rtreeA trace-float precedent) |

### B. expr — 3 leaves (the family truth), 1 class

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| B1 | 3 | 16.100, 16.101, 16.102 | "no such function: implies_nonnull_row" | `implies_nonnull_row` is one of the FOUR test-only INLINE functions (implies_nonnull_row, expr_compare, expr_implies_expr, affinity) gated by `sqlite3_test_control(SQLITE_TESTCTRL_INTERNAL_FUNCTIONS, db)` — the fixture's step (expr.test:1069) was not converted and the engine has neither the gate nor the functions | E+C | src/func.c:3270-3274 (aBuiltinFunc TEST_FUNC block), src/expr.c:4605-4650 (INLINEFUNC codegen: implies_nonnull_row(A,B) = compile-time constant: B is TK_COLUMN → OP_Integer(sqlite3ExprImpliesNonNullRow(A, B.iTable, 1)) else OP_Null; src/expr.c:6980 sqlite3ExprImpliesNonNullRow), src/test1.c:8002 (the test-control). Units: engine — internal test-control flag + the four names at resolve with the gate + expr.c-style codegen (ExprImpliesNonNullRow port); converter — emit a test_control step for `sqlite3_test_control SQLITE_TESTCTRL_INTERNAL_FUNCTIONS db` |

### C. expridx1 — 10 leaves, 2 classes

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| C1 | 8 | 1.1.1b, 1.1.1c, 1.2.1, 1.2.2, 1.3.1, 1.3.2, 2.3, 3.3 | integrity_check got [ok] vs "row 3 missing from index i1"; discrepancy-count queries got []/[0] vs [20]/[500] | The x1 WITHOUT ROWID tables are **imposters** (TESTCTRL_IMPOSTER) over the expression-index b-trees; their UPDATEs corrupt the index directly. The converter flattened them into plain tables, so nothing is ever corrupt. After a faithful imposter mechanism, integrity_check must also validate expression-index entries per row ("row N missing from index") | F+E (carded) | src/main.c SQLITE_TESTCTRL_IMPOSTER; integrity_check index validation src/pragma.c:2055-2090 (" missing from index " at 2074) + sqlite3GenerateIndexKey with pParse->iSelfTab (expression keys evaluated per row). Units: (1) engine+harness imposter test-control; (2) integrity_check index-entry existence check incl. expression indexes |
| C2 | 2 | 4.3, 4.6 | got [ok] vs "index z1b stores an imprecise floating-point value for row N" | The imprecise-fp integrity_check diagnostic **does not exist in 3.51.0** — the expectation is a fossil of the pre-rename tree (signature 2). Even a perfect imposter port cannot reproduce it from the current oracle | F | N-A — mark the two leaves unsupported in the harness (or regenerate from an older sqlite checkout if the census ever wants them) |

### D. fts3expr — 1 leaf, 1 class

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| D1 | 1 | 6.1 | "table t1 already exists" on CREATE VIRTUAL TABLE t1 USING fts3(a) | Section 4.1 created t1(a,b,c); upstream the malloc tests between 4.x and 6.x reset the database (do_malloc_test → faultsim machinery, `faultsim_delete_and_reopen` test/malloc_common.tcl:177) so 6.1's CREATE succeeds. The converter dropped that reset (oracle 3.54 confirms a duplicate create errors) | C | tools/tclconvert: do_malloc_test must translate to (at minimum) a db-reopen/reset step pair; regen fts3expr.json |

### E. fts3expr3 — 26 leaves (×2 files = 52 subtests), 3 classes

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| E1 | 2 (×2=4) | 1.1, 2.1 | "fts3_exprtest: the expression must be text" vs want "P 1" | The C never type-checks argv[1]: `sqlite3_value_text(argv[1])` coerces (integer 1 → "1" → phrase "P 1"); our internal/fts/exprtest.go:150 rejects non-text | E | ext/fts3/fts3_expr.c:1233-1245 (zExpr = value_text). Unit: ExprTestFunc — drop the type check, coerce via the engine's value-text |
| E2 | 5 (×2=10) | 3.65#01-3.69#01, 4.1.1, 4.2 | got balanced tree vs want [1 {Error parsing expression}] (4.1.2 wants [0] = 4000-term OK) | The rebalance parse must fail past the depth limit: SQLITE_FTS3_MAX_EXPR_DEPTH = **12** (ext/fts3/fts3Int.h:65-66); ours uses fts3MaxExprDepth = **100** (internal/fts/exprtest.go) so nothing errors. Order per C: balance first, then depth-check the BALANCED tree (fts3_expr.c:1059-1066) — our order already matches; 4.1.2 (4000-term AND → balanced depth 12 OK) pins the constant | E | fts3ExprBalance (fts3_expr.c:811) + fts3ExprCheckDepth (:785) + sqlite3Fts3ExprParse tail (:1059-1066). Unit: set the constant to 12; verify the balance algorithm byte-matches C against the regenerated E3 wants |
| E3 | 16 (×2=32) | 3.2#01, 3.6#01, 3.10#01 … 3.62#01 | got balanced andor tree vs want "AND xxx xxx" (raw template) | The converter's cmdRegsub takes args[0] as the pattern, so `regsub -all xxx $tree $node tree` matches literal "-all" → no substitution → the frozen expect is the unsubstituted `balanced_andor_tree` template | C | tools/tclconvert/tcl/interp.go:1923-1941 cmdRegsub: skip `-all`/-nocase option args, use ReplaceAllString for -all, watch the TCL-ARE vs Go-RE pattern dialect. Regen fts3expr3.json, then verify our engine's got against the corrected wants (the got shapes already look balanced-correct) |

### F. indexexpr2 — 9 leaves, 5 classes

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| F1 | 1 | 1.2 | got {1 0} (table order) vs want {0 1} | WHERE (b \|\| 'x')>'onex' must drive index i1 (b \|\| 'x') — index scan order (threex, twox); oracle 3.54 returns 0 then 1 | E | src/where.c expression-index range plans. Unit: internal/execquery/planner — expression-index candidate for range constraints |
| F2 | 1 | 3.4.5eqp | got "SCAN t4 …USE TEMP B-TREE FOR ORDER BY" vs /SCAN t4 USING INDEX i4/ | ORDER BY Substr(a,-2) COLLATE nocase must be satisfied by i4 ON t4(Substr(a,-2) COLLATE nocase) (indexexpr2.test:150-157) — no sorter | E | src/where.c orderByCancellation for expression indexes (collation-match rules). Unit: planner ORDER-BY mark-off over expression indexes |
| F3 | 2 | 4.120, 4.130 | refcnt counter got 3 vs want 2; got 3 vs want 0 | UPDATE index maintenance: the index expression is evaluated exactly TWICE when an indexed column changes (delete-old + insert-new) and ZERO times when none of its columns change; ours evaluates 3× unconditionally | E | indexexpr2.test:160-197 comments; src/vdbe.c OP_IdxDelete/OP_NewRowid flow — SQLite evaluates the expr per index op. Units: internal/execdml/update.go:456 `maintainedUpdateIndexes` (skip unchanged-column indexes) + :384 `collectUpdateIndexDeleteTarget` / the insert path (exactly one eval per side) |
| F4 | 4 | 4.200, 4.210, 4.220, 4.900 | "no such table: explain" | The `explain` table-valued function (ext/misc/explain.c — eponymous vtab over the VDBE listing) loaded via `load_static_extension db explain`; the converter dropped that step and the engine has neither the module nor the static-extension step | E+C (carded) | ext/misc/explain.c (explainVtab). Units: engine — the explain TVF module (needs the regular-EXPLAIN listing of G); converter — emit the load_static_extension step. 4.900 pins the no-argument/no-rowid-constraint protocol ({0 {}}) |
| F5 | 1 | 11.0 | "misuse of aggregate: sum()" vs {44 -44} | Generated column b AS (-a) used in both loops + scalar aggregate subquery (SELECT sum(-a0.a=b) FROM t3 GROUP BY b) inside WHERE of an outer GROUP BY — our aggregate validation false-positives (forum 409ebc7368: EP_Collate/TK_AGG_COLUMN resolution) | E | forum 409ebc7368 / src/expr.c aggregate resolution. Unit: internal/execquery/select_agg_validate_part2.go:113 + part3.go:238/246 — allow the aggregate-in-scalar-subquery shape |

### G. indexexpr3 — 2 leaves, 1 class

| id | n | leaves | got vs want | root cause | attr | C pointer / unit |
|---|---|---|---|---|---|---|
| G1 | 2 | 1.5, 1.6 | first column got 0 vs want 2 | The do_hasfunction_test helper counts `opcode=="Function"` rows in EXPLAIN; our regular EXPLAIN is a 2-row STUB (Init/Return — internal/execquery/explain.go:11-24) so the count is always 0. 1.1-1.4 pass vacuously today (they expect 0 = the function resolved into the index) — a faithful listing must model expression-index substitution or those regress | E (carded) | src/vdbe.c OP_Function/OP_PureFunc + the index-resolution of expr.c. Unit: regular EXPLAIN must emit a faithful VDBE listing (at minimum: per-function-call rows named "Function" for non-index-resolved calls; index-resolved ones stay OP_Column). Shares the VDBE-listing surface with F4 |

## Recommended unit order (smallest-first; bang noted)

1. **S1 (3 leaves, pure engine, oracle-confirmed)** — A4 (blob→text NUL) + A5 (CAST-NUMERIC keeps REAL). evalCastExpr, expression_eval_tail.go:444.
2. **S2 (6 leaves, pure engine, oracle-confirmed)** — A3 (affinity-name CAST) + A6 (truthiness prefix rule + iif==CASE) + A7 (variable-number limit 32766).
3. **S3 (1 leaf)** — A2 CASE evaluation count (evalCaseExpr).
4. **S4 (14 subtests, engine, small)** — E1 + E2: fts3 exprtest arg coercion + depth constant 12. Internal/fts/exprtest.go only.
5. **S5 (converter batch: 11 leaves) + fixture regens** — A8 (expect expansion), A1 (foreach var-init), D1 (malloc-test db reset). Regen e_expr.json + fts3expr.json; engine re-verified after regen.
6. **S6 (32 subtests via regen)** — E3 cmdRegsub `-all`; regen fts3expr3.json (drop the .gz duplicate first), verify engine balance output against the corrected wants + oracle.
7. **S7 (3 leaves)** — B1 the TESTCTRL_INTERNAL_FUNCTIONS internal-function set (engine gate + 4 inline funcs; converter test-control step). Restores the whole expr family truth.
8. **S8 (3 leaves)** — F3 UPDATE index-maintenance eval counts + F5 aggregate false positive.
9. **S9 (2 leaves)** — F1/F2 expression-index planner (range scans + ORDER BY satisfaction).
10. **S10 (6 leaves, largest)** — F4 + G1: the explain TVF + a faithful regular-EXPLAIN listing (shared VDBE-listing surface; carded).
11. **S11 (8 + 2, carded/stale)** — C1 imposter mechanism + integrity_check expression-index validation; C2 N-A the two imprecise-fp fossils.

Bang view: S1-S6 + S7 close 66 of the 98 subtests with no planner or EXPLAIN
work; S10/S11 are the only large surfaces.

## Oracle session (tagged)

/usr/bin/sqlite3 — **3.54.0 2026-04-09** (8fa8248e3032) — confirmed:
- `CAST('def' AS shobblob_x)`→blob, `CAST('ghi' AS abbLOb10)`→blob,
  `CAST('x' AS shint_x)`→integer, `CAST('x' AS clob)`→text (A3);
- `typeof(CAST('9.223372036e15' AS NUMERIC))`=real; printf('%.5e')=9.22337e+15 (A5);
- `CAST(X'670068006900' AS text)`: typeof=text, length=1, quote='g' (A4);
- CASE/iif truthiness: f/f/f/f/t/t ('1english'→t) (A6);
- `SELECT ?32766, 22` accepted by the 3.54 CLI build (its compile-time limit
  differs) — the contract comes from src/sqliteLimit.h (32766) + the fixture
  baked at conversion time (A7);
- duplicate `CREATE VIRTUAL TABLE t1 USING fts3(a)` errors (D1);
- `(b||'x')>'onex'` index scan order 0,1 (F1).

Sweep artifacts: /tmp/expr_triage_run.log (raw), /tmp/expr_fails_all.txt (98
leaf paths + 8 parents), /tmp/expr_cases.tsv (leaf/got/want/SQL digest).
