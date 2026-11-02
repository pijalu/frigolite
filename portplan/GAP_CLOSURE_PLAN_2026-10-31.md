# GAP CLOSURE PLAN — 2026-10-31 (session plan, instantiates UNIFIED_PARITY_PLAN)

> Scope: review the current status and the gap against SQLite 3.54
> (`../sqlite`) — parity **and** performance — and close it. This document is
> the *session-executable* slice of `portplan/UNIFIED_PARITY_PLAN.md` (the
> master, still binding) with fresh evidence. Root bindings stay: PARITY_PLAN
> §0 (no descoping, every failure owned), GUIDELINES §1 (no try/fail, no
> simplify), UCL-first, engine edits and full-suite runs never overlap.

---

## 1. Status review (evidence, 2026-10-31)

### 1.1 Parity state
- **Baseline**: run 33 = **3,481 failing leaves / 309 families**
  (commit `e8faf18ae`); session totals runs 30→33: −292 leaves,
  regression-free. testgen census **1048 pass / 25 fail / 290 skipped**
  (the 25 pre-exist at `7f8441f73` — drift-triage goal queued).
- **Quality**: staticcheck 0; SOLID architecture test green; pre-commit gates
  active.
- **Work in progress (uncommitted on `feature/perf`)**: the *where8
  search-count unit* — engine side landed (Engine.searchCount mirroring
  vdbe.c increment sites, `IncSearchCount` wired through DMLContext /
  SelectContext, `OrRange.LikeGlob` + OR-plan delta with the `beyond[]`
  EOF-step model in `internal/execdml/or.go`). **The converter/harness half
  is missing**: where8's `execsql_status`/`execsql_status2` helpers render
  `[rows…] [step] [sort]` (bare) and `[rows…] [step] [sort] [search_count]`
  (search count reset *before* the probe query); the current fixture renders
  the where7-style labeled form `scan N sort M`. where8 = 35 leaves red.

### 1.2 Performance state — FRESH measurement (perfbench, this session, 50k rows, `:memory:`, median/reps)

| lane | frigolite ops/s | sqlite ops/s | gap |
|---|---|---|---|
| insert (rowid) | 913k | 1,071k | **1.17×** |
| point-select | 852k | 770k | **at parity / faster** |
| point-update | 577k | 824k | 1.43× |
| point-delete | 837k | 933k | 1.12× |
| scan (rows/s) | 22.6M | 66.1M | **2.9×** |
| count-star | 1.27G | 9.21G | **7.2×** |
| group-by | 81 | 249 | **3.1×** |
| file-insert (autocommit) | 9,012 | 4,186 | **2.2× faster** |
| insert (w/ index) | 291k | 692k | **2.4×** |
| indexed-select | 121k | 639k | **5.3×** |
| indexed-update | 133k | 539k | **4.1×** |
| indexed-delete | 124k | 668k | **5.4×** |

Read: rowid CRUD is at parity; the remaining perf gap is **(a) secondary-index
lanes (2.4–5.4×), (b) scan/aggregate read paths (2.9–7.2×)**. This matches
R14 §7's attribution; the F1 residue levers target exactly these lanes.

---

## 2. The gap, decomposed (3,481 leaves → units)

| cluster | families (leaves) | unit | track/milestone |
|---|---|---|---|
| where8 search count | where8 (35) | finish WIP: converter `execsql_status`/`status2` shapes + harness render + engine counters | **W** / M2 |
| expression indexes | indexexpr2 (50) | EQP/rows probe vs oracle, then planner fix | **R**→implement / M2 |
| named results | select1 (32) | fresh-fixture `query2` renderer mismatch | **R**→implement / M2 |
| normalize.c | normalize (47) | port ext/misc/normalize.c → `internal/normalize` + Stmt API + harness step | **N** / M2 |
| xfer optimization | insert4 (30) | INSERT…SELECT transfer opt + `sqlite3_xferopt_count` | M2 |
| reindex rules | e_reindex (97) | integrity_check index/table mismatch checks | M2 |
| small-cluster residue | memdb/func2/func4/stat/rowvalue (18–19 each), e_expr (22), func auxdata (37), fts3expr4 ICU (20) | per-family probes | **R** / M2 |
| testvfs/fault VFS | e_walckpt (296), pagerfault (59) | P-C1 testvfs wrapper VFS | M3 |
| multiplex VFS | multiplex (207→4) | **LANDED (M2.MUX, 2026-11-02)** — full port: internal/multiplex Group (byte-level chunk split, truncate-above-mark, virtual Stat, existing-file FileCount, %03d names, chunk-1 probe rederivation, pending-byte loop), pager pagerFile interface (every main-db file grouped; RAW chunk-0 stats for os.SameFile identity), multiplex_control() function + multiplex_* pragmas, converter multiplex_set/global write-through + regenerated corpus, harness close_db expect gated on collation fixtures. Residue 4 leaves: 3.1.x cross-connection schema staleness (two connections on one file — first connection does not reload the schema after the second writes; pre-existing, notify1-class), 5.4.1 runtime-catch list, setup_3 | commit `M2.MUX` |
| shared cache + unlock-notify | notify1 (123) | P-C shared-cache goal | M3 |
| WAL snapshot API | snapshot (31), wal2/wal/walbak (182) | P7.WAL-G7 residue | M3 |
| pager1 | pager1 (225) | probe malformed-class first | M3 |
| UTF-16 | like3 (33) | P-C11 UTF-16 (last) | M3 |
| planner tail | where (27), bestindex* residue (~85) | B4/B5 continuation | M2/M3 |
| testgen drift | 25 packages | drift triage | queued |
| perf indexed lanes | — | F1a (#5 WHERE-ref cache), F1d (findChildIndexForInsert decode diet) | **P** / M6-F1 |
| perf scan/aggregate | — | F1c (#8 read-side decode-free compare), F1b (#7 rowmap slice), F3 | M6-F1/F3 |

## 3. Execution map — this session (parallel tracks, disjoint file ownership)

Binding constraint: engine edits and full-suite runs never overlap; one active
unit per track; harness file (`frigolite_harness_test.go`) owned by Track W
only this session. Parallel agents on *disjoint packages*.

| track | owner | files (exclusive) | deliverable | validation |
|---|---|---|---|---|
| **W** where8 | main agent | `tools/tclconvert/tcl/interp_sql.go`, `frigolite_harness_test.go`, `internal/execdml/or.go` (finish) | converter models `execsql_status`/`execsql_status2` (bare render, search-count element + reset); harness renders 3- and 4-element forms; regenerate where8 fixture; oracle-verify deltas | `FRIGOLITE_TEST=where8` → 0; battery families byte-stable; commit |
| **P** perf F1a | agent perf | `internal/execquery/select_index_seek*.go`, new cache file | implement R14 §7 #5 (cache WHERE-ref analysis per statement shape) | go test execquery+root; harness leaf-sets identical on index/where/select; EQP diff; perfbench before/after paired |
| **N** normalize | agent norm | new `internal/normalize/` only | port ext/misc/normalize.c (717 lines) + native Go tests vs oracle (`go-sqlite3` scratch in /tmp) | `go test ./internal/normalize/...`; byte-parity probes vs oracle |
| **R** probes | agent probe | read-only + `/tmp` scratch | root-cause reports: indexexpr2, select1, e_reindex 1.2–2.x, memdb/func2/func4/stat/rowvalue, func auxdata, fts3expr4 ICU, testgen-25 drift census | each report: exact error class, oracle behavior, native repro, proposed unit card |

Serialization: after tracks land, main agent runs the family-scoped battery
(index/where/select/delete/analyze/collate/upsert/conflict/insert), then a
full-suite checkpoint run (serial, no engine edits in flight), then commits
per unit with run stamps.

## 4. Next tranches (queued after this session)

1. **T2 (M2 finish)**: indexexpr2 + select1 + insert4 xfer-opt + e_reindex +
   small-cluster fixes from R's reports (parallel by family, harness
   serialized).
2. **T3 (M3 open)**: testvfs P-C1 (unlocks e_walckpt 296 + pagerfault 59),
   multiplex VFS (207), shared-cache (123) — the three biggest feature gaps;
   WAL snapshot API; pager1 probe results; UTF-16 last.
3. **T4 (M6-F1 finish)**: F1c read-side decode-free compare (scan 2.9× →
   target ≤2×), F1b rowmap slice, F1d findChildIndexForInsert; F3
   count-star/group-by paths (7.2×/3.1×).
4. **T5**: F2 plan cache (statement-shape), F7 perf gate.
5. **T6**: Q2–Q5 re-close; testgen drift triage; M7 closeout matrix.

## 5. Progress log

| date | event | evidence |
|---|---|---|
| 2026-10-31 | plan written; fresh perfbench baseline recorded (§1.2); where8 WIP audited (engine landed, converter/harness missing); tracks W/P/N/R launched | this file |
| 2026-10-31 | **Track W tranche 1 landed** — where8 35→**18**: converter models `execsql_status`/`execsql_status2` (bare render + search-count reset via counter markers + `applyStatusStyle`); harness renders bare `N M` and bare `N M S`; vdbe-calibrated search accounting (full-scan Nexts = rows−1; sorter = −1 + successful delivery Nexts; OR-plan branch seeks unconditional, nexts = matches (beyond) else matches−1, movetos = unique rows); `ColumnValue` unwrap in the OR `beyond[]` bound check; two bounds on one column merge into one OR branch (SQLite's combined `a>? AND a<?` range — where8-1.11). Pin `TestDBStatusBareStyleAndSearchCount`; regen where8 fixture byte-identical modulo the new fields; battery byte-stable (where7 0, where 26, index 2, delete 6, analyze 1, collate4 1) | commits `M2.W8 tranche 1` |
| 2026-10-31 | where8 residue (18) = one class: our executor full-scans where SQLite seeks (1.8 `a>1 AND c LIKE`, 1.12.1 `a IN(...) OR a=5` IN-branches, the 3.x join OR terms, correlated-subquery probes) + the index-walk increments in `select_index_seek*` (queued behind Track P's F1a cache). vdbe calibration recorded: Seek* counted unconditionally (found or not), Rewind/Last/SeekRowid uncounted, OP_Sort −1 with NO recovering +1 (Rewind has its own break), SorterNext counts at next_tail | oracle EXPLAIN bytecodes, this file |
| 2026-10-31 | **Track N landed** — `internal/normalize` (normalize.c port): verbatim charClass/ctypeMap tables, CC_* dispatch, first-pass emitter + second-pass IN rewrite. Validated by differential fuzzing against the unmodified C compiled as an oracle: 320,036 inputs, zero mismatches; 250 Go vectors incl. the C's two non-idempotent rows. Layer 0 | commit `M2.NORM` |
| 2026-10-31 | **Track P landed (F1a)** — WHERE-ref analysis statement-shape memo: perfbench indexed-select 170-176k → **234-235k ops/s (+38%)**, allocs −28%; behavior identity proven (leaf-sets identical on 7 families, EQP+rows byte-identical over ~40 shapes incl. prepared loops). Key design fact: the slot-path live clone rewrites literal leaves per execution, so only STRUCTURE is memoizable — values re-read per serve | commit `PERF.R14-5` |
| 2026-10-31 | **Track W tranche 2 landed** — catch+lappend body value → runtime_list: select1 32→**14**; runtime_list joined the converter whitelist (silent-drop class recurrence); catch steps store ROWS in msg on success. Honest side effect: select2/select7/conflict2/ioerr4/select5 were aborting silently on the unresolved `$msg` — they now run to completion and surface their real residue | commit `M2.SEL1` |
| 2026-10-31 | **Run 34 checkpoint launched** (post M2.W8-1 + M2.NORM + PERF.R14-5 + M2.SEL1) | `/tmp/frigolite-full-suite34.log` |
| 2026-10-31 | **Run 34 verdict: 2,876 leaves / 276 families — net −44 vs a fresh baseline suite at `e8faf18ae` (2,920/274, same parser+convention; the run-33 record's 3,481 used a broader parse).** Improved: where8 −17, select1 −17, wal −12, resetdb −1. The 5 silently-aborting families surface their real residue (+11, previously counted 0 while the family still failed). Two regressions caught by the baseline diff and fixed in-session (`8fa10c6db`: the catch msg rows re-run re-executed stateful PRAGMAs); atof1 +1 = the known randomblob(0) flake. Lesson: the run-33-era red totals are not directly comparable across parse conventions — always diff against a same-convention baseline | `/tmp/run33-baseline-verify.log`, `/tmp/run34-fails.txt`, commit `8fa10c6db` |
| 2026-10-31 | **Track R landed** — `portplan/PROBES_2026-10-31.md`: 10 families root-caused (read-only). Headline: the BETWEEN NULL-bound short-circuit (`expression_eval_tail.go:183`) — **landed as `M2.IX2a`: indexexpr2 49→9**, oracle-verified, pin `TestNativeBetweenNullBoundKleene`. select1's fixture proven byte-identical to a fresh conversion (pure renderer unit); e_reindex-1.3 = integrity_check never opens index b-trees; fts3expr4 = stale `ifcapable icu` fixture; the 25 testgen-drift packages enumerated. All cards queued in §5.1 | commit `M2.IX2a`, PROBES report |
| 2026-10-31 | **Closure round 2 (goal: close all gaps)** — S-units landed: fts3expr4 fixture removed (19→0, capability gate), func2 bin_to_hex (17→0), func4 TCL float rendering (18→2), rowvalue3/4 drop_all_indexes (11→0), COLNAME mode matrix (agent: select1 14→5, colname 10→0, 55-family sweep clean), insert4 xferopt counter (30→0: full decision port + counter plumbing + native pin). REIDX (integrity walk) mid-validation — its entry-count check surfaced PRE-EXISTING DML index-maintenance leaks (UPDATE OR REPLACE stale entries, TEMP-table insert, WR desync); containment = fix the leak sites, not weaken the check. AUX (auxdata) implementing | commits `M2.INS4`, `M2.COLNAME`, `M2.FUNC4`, `M2.FTS3EXPR4`, `M2.FUNC2`, `M2.RV34` |

### 5.1 Cards queued by this session (with evidence; probe details in `portplan/PROBES_2026-10-31.md`)

| unit | first action | evidence |
|---|---|---|
| ~~**where8-B**~~ (partially landed) | indexexpr2-1a BETWEEN NULL-bound fix **LANDED** (`M2.IX2a`: 49→9). Residue: 1b FALSE-AND-NULL under LEFT JOIN (S-M), 1c `explain()` TVF (M), 1d collated expression-index ORDER BY (M), 1e UPDATE index-maintenance counting (S-M), 1f aggregates in correlated subquery (M) | PROBES §1 |
| **colname-6 (select1 residue, 14)** | full_column_names name modes: explicit ref → real table name (not alias), star → alias.column, subquery → `(subquery-N).expr`, span mode = original token text (`test1 . f1`); 11.8 = execsql2 dup-collapse; 6.3.1 = TCL element quoting for trailing-space names; 6.8/6.11 = query2 catch `$v` harness model. Converter verified byte-identical — pure renderer work | PROBES §2 |
| **e_reindex-1.3 (integrity_check index walk)** | `exec/pragma_quickcheck.go` never opens index b-trees — cannot produce "wrong # of entries in index X" / "row N missing". Index-btree walk + count + per-row presence (pragma.c checkTreePage semantics; Size M-L). 2.x (~95 leaves) = the ATTACH test.db2 shared-filename harness race (S) | PROBES §3 |
| **memdb (18)** | query_assign resultAll/`$var` substitution model vs `set rx [db eval …]` self-reference (S-M, harness) | PROBES §4 |
| **func2 (17)** | `bin_to_hex` wrapper dropped by the converter (S) | PROBES §4 |
| **func4 (18)** | `toreal` out-of-int64/blob-bit conversion (S, engine) + TCL float rendering (S-M, harness) | PROBES §4 |
| **stat (22)** | setup idempotency (S) + `table dbstat may not be dropped`/`no such database: mainx` errors (S) + dbstat page-level fidelity (M-L) | PROBES §4 |
| **rowvalue (21)** | setup cascades (S) + row-value IN/comparison probes (S-M) + row-value join-range EQP (M, B5) | PROBES §4 |
| **func (92)** | auxdata slots on the eval context with Stmt lifetime (P-C6, M) + the test_func.c fixture-UDF set (M) | PROBES §5 |
| **fts3expr4 (19)** | **LANDED** — fixture removed (source is `ifcapable !fts3||!icu`-gated, 8_3_names precedent) + skip-map evidence entry | commit `M2.FTS3EXPR4` |
| **func2 (17)** | **LANDED** — bin_to_hex hex-render for the family's []byte results (blob.test mechanism) → 0 | commit `M2.FUNC2` |
| **func4 (18)** | **18→2** — the harness's REAL rendering now uses the TCL shortest-round-trip rules (tclsh-pinned formatTclDouble); residue 2 = func4 2.46/2.49's runtime-conditional highPrecision probes (converter runtime_expr assignment card) | commits `M2.FUNC4` (formatTclDouble) |
| **rowvalue4 (8) + rowvalue3 (3)** | **LANDED** — drop_all_indexes fixture marker (tester.tcl parity) → 0/0 | commit `M2.RV34` |
| **rowvalue2-2.x (2, B5 OR-single-range)** | **LANDED (M2.OR1)** — the actual rule is whereexpr.c CASE 2 (whereCombineDisjuncts), not a union-range: a TWO-way OR holding a subterm pair with the same operands and compatible operators ("a>2 OR (a=2 AND b>2)") combines to a virtual "a>=2"; when an index/IPK serves it, the OR plan collapses to ONE branch whose per-branch index-key sort reproduces the single range loop's row order exactly. Non-combining controls stay multi-branch: different bound constants ("a=4 OR a>2"), different literal token (a=2.0), mixed directions ("a>2 OR a<0"). rowvalue2 2→0; every other family's leaf register byte-stable; oracle-verified 12-shape probe + native pin TestOrPlanCollapsesSingleRange | commit `M2.OR1` |
| **rowvalue2-2.x (2, B5 OR-single-range) (superseded duplicate of the row above)** | `a>2 OR (a=2 AND b>2)` over i2(a,b,c): SQLite plans ONE range `SEARCH i2 (a>?)` with the OR as residual (output = index order 44..63); our planner builds MULTI-INDEX OR (branch order 48..63,44..47) so the compare_query mismatch. where.c's single-range OR rule: when one OR term's index range covers the whole OR clause, scan that range with the full WHERE as residual instead of the multi-index loop (probe + oracle EQP in this session) | oracle probe, where.c whereLoopAddOr |
| **rowvalue5 (8)** | vtab row-value constraint unwrapping: SQLite passes `(a,b) = (9,10)` to xBestIndex as per-column EQ constraints; our TCL-vtab path gets none (classifyVtabBinaryOp cannot resolve RowValue operands), so the constraint-rendering `expr` column stays empty. Fix site: `internal/execquery/vtab_bestindex.go` classifyVtabBinaryOp — offer N per-column constraints for RowValue-vs-constant-row comparisons; the `add()` conjunct/`VtabConjunctValue` machinery needs a per-constraint value override (each split constraint binds a DIFFERENT element of the row value); whereexpr.c's TK_ROW split is the reference (B5, M) | rowvalue5.test xBestIndex rendering |
| **memdb (18)** | composition-aware set-assignments: `set X [proc]` whose value is a Tcl list over runtime components ([string length $rx] + $rx) collapses to the single query marker — needs a runtime_list-with-variable step so $X stays a full runtime ref; strlen derivation + runtime case-name expansion already landed | commits `M2.MEMDB-a` |
| **testgen drift (25 pkgs)** | enumerated in PROBES §7 (altercol, altercorrupt, … without_rowid7) — one triage pass classifies each engine-vs-testgen-only per the pure-Go-port protocol | PROBES §7 |
| **func residue 32 (func 12, func4 2, func5 1, func6 8, func7 2, func8 7)** | **LANDED (M3.FUNC1, first tranche — 20 of 34 leaves; incl. func-36.100/110 found during the sweep)**. Class A+B+func8: fpnum_compare port (test1.c:6168 — trailing-zero tolerance only when the OTHER string ends mid-token needs 15 matched digits; ≥15 matched → rest skipped) in the harness expectation path + TestFPNumCompare pin; fixture UDFs (test_destructor/test_destructor_count with statement-boundary drain, test_error NULL→empty message, test_isolation, legacy_count aggregate, testfunc1 `error "…"` bodies, nullx arity-127); func8 script-form `db func cross {joinx cross}` (converter FunctionScript + tclsqlite appended-args invocation); func-36.100/110: `db func -> ptr1` — parse.y "expr ::= expr PTR expr" (parse.y:1401) compiles `a -> b` into the plain function call "->"(a,b), so user registrations replace the json.c built-in via the ORDINARY registry — the engine's arrow dispatch was wrongly gated behind the vtab OverloadProbe contract (execexpr userArrowOperatorOverload now un-gated; json lane byte-identical). Remaining (Batch 2/3): **C. engine**: INSERT default-eval `unknown function: nosuchfunc()` (expr.c:5332) (1); counter2 DETERMINISTIC + constant factoring (func5-2.3) (1); test_frombind bitmask (6); sqlite_offset INLINEFUNC→OP_Offset + offrec UDF (8). **D. C-API harness**: stmt_bind (16.1) + stmt_step row collection (13.7). **Residue**: func-26.2/26.3 (converter dropped the 32766-arg `for` loop; native-pin covered). func4 highPrecision: NOT an engine gap — totype.c toreal(2^53+1) is NULL (iVal != double round-trip); converter recorded the wrong conditional branch | run-37 registers, sqlite src, this session's probes |

| 2026-10-31 | **Run 36 checkpoint: 2,702 leaves / 267 families — ZERO regressions vs run 35, −174/−9 cumulative this round.** The per-family `tcl_precision` rendering resolved run-35's float drift (the regressions were precision-15 families rendered shortest); run 35's other findings (resetdb +1, numindex1) also resolved. Queued next tranches with full cards: where8-B (index-walk increments + executor honors the plan), rowvalue5 vtab split, OR-single-range planner rule, REIDX residue (WR tail unification, temptable2 5), func residue 22, and the M3 feature goals (testvfs/multiplex/shared-cache/UTF-16 = ~700 leaves) | `/tmp/frigolite-full-suite36.log`, `/tmp/run36-fails.txt` |
| 2026-11-02 | **M2.OR1 landed: OR-single-range (whereCombineDisjuncts) — rowvalue2 2→0.** Oracle-revealed the exact rule (CASE 2 + CASE 1 OR→IN; CASE 1's all-eq shapes already coincide). Also: the native full_column_names test's expectations corrected to the oracle (the engine already matched; the run-36 register missed native-test failures). **Run 37 verdict: 2,897 leaves / 302 families** — self-consistent extraction, matches every recorded per-unit number (e_walckpt 272, wal2 89, multiplex 206); the vs-run-36 delta is the old record's extraction convention (run 36's checkpoint commit does not build — its numbers came from the pre-commit tree; log lost). Native residue: TestNativeThreadReaderDuringWrites = isolation-passing flake; pager1-10.x = full-suite-context flake (passes family-scoped at HEAD, +43 only in-suite — P-C1 interaction, carded for the P-C1 continuation). **Protocol change: per-family registers are the default verification; full suites only at round checkpoints; every run's register now archived under `portplan/runs/`** | `portplan/runs/full-suite37.log.gz`, `runs/run37-family-counts.txt` |
