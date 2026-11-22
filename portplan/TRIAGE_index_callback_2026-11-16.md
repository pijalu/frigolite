# TRIAGE — index/EXPLAIN-callback residue (S36, read-only triage, 2026-11-16)

Base: `ed154d87f` (feature/perf). Source of truth:
`/tmp/full_s33s34_merge_gate.log` (verified S33/S34 merge gate, 1672.15s,
353,635 `=== RUN` lines, no panic; 224k NUL bytes of faultsim corruption —
extracted with the **python raw byte scan ONLY**
(`re.findall(rb'--- FAIL: TestSQLiteSuite/([^\s(]+)')`; the leaf-only filter
keeps names containing `/`). Result: **1,317 distinct failing leaves across
231 families**. Family-level FAIL lines (231) are the parent markers — every
family with leaf fails also has one; there are **zero zero-leaf (aborted)
families** in this gate.

Verification: the core families were re-run in the throwaway worktree
`/private/tmp/wt_s36triage` (branch `s36-index-triage` from ed154d87f) —
leaf-name SETS are **IDENTICAL gate↔worktree** for every index-cluster,
where2, vacuum, without_rowid, savepoint, e_expr family. Worktree-only
deltas: `temptable2-4.1.1` (the carded timing flake) and `vacuum-into-500`
(gate-only, file-lifecycle dither). The `alter` substring sweep matched only
`vtab_alter` (3 leaves, present in the gate too) — **the alter* DDL families
are at 0** (S31 merge closed alter2's 13).

Old-census corrections (census numbers are stale):
- "index family ~101" → the index cluster (bestindex1/6/E/F, index3/6/7/9,
  indexexpr2/3, indexedby, expridx1) is **97 leaves**; with the planner
  neighbors (pushdown, coveridxscan, skipscan1/5) it is **134**.
- bestindexF 35→34, index9 20→19, indexexpr2 10→9, bestindexE 9→8,
  indexexpr2/3 "11"→9+2, expridx1 stays 10 (fossil — no C source in the
  3.51.0 tree; fixture `file: expridx1.test` is a fossil conversion).
- rtree "~126" → **88**; tkt "~33" → **18**; vacuum "24" → **20**;
  without_rowid "27" → **25**; savepoint "15" → **12**; temptable "7" →
  **5** (plus the carded flake class); fts3* "~53" → **67**
  (incl. fts4umlaut 14 + e_fts3 7); alter "20" → **0**.

---

## 1. Bucket map

| # | Bucket | Leaves | Families |
|---|--------|-------:|----------|
| A | EXPLAIN-row callback substrate (the carded class) | ~45 | index9, indexexpr2(4), where2, bestindexF(EXPLAIN chain) |
| B | register_tcl_module completion (surface exists, incomplete) | ~34 | bestindexF(rest), bestindexE, bestindex6, bestindex1 |
| C | Expression-index value-representation & maintenance | 16 | expridx1, indexexpr2(rest), indexexpr3 |
| D | Planner/EQP residue | 45 | skipscan5, skipscan1, index3, index6, index7, indexedby, pushdown, coveridxscan |
| E | fts3 residue | 67 | fts3defer/2/3, fts3first, fts3malloc, fts3query, fts3cov, fts3auto, fts4umlaut, e_fts3 |
| F | Authorizer event collection | 10 | savepoint(3), without_rowid3(5), tkt-f7b4edec(2) |
| G | tkt residue | 18 | 13 tkt families |
| H | rtree residue | 88 | rtree1, rtreeA, rtreeJ, rtreecheck, rtreedoc/3, rtreefuzz001, rtreecirc |
| I | vacuum residue | 20 | vacuum-into, vacuum2/3, e_vacuum |
| J | without_rowid rest | 15 | without_rowid3(C-API/rename/FK), without_rowid7 |
| K | savepoint/temptable rest | 14 | savepoint(5.x,7), savepoint6/7, temptable(6.x), temptable2(2, carded temp-spill) |
| L | e_expr/expr residual | 5 | e_expr(2, carded A2/A9), expr(3, implies_nonnull_row) |

All other gate families (wal/wal2/walckptnoop/normalize/json101/... = the
remaining ~1,055 leaves) are out of scope for this card; they retain their
census attribution.

---

## 2. Bucket A — the EXPLAIN-row callback substrate (the carded class)

**Root contract**: plain `EXPLAIN <stmt>` must render a faithful opcode row
stream (addr, opcode, p1..p5, comment — vdbe.c builds it; OP_OpenRead is
vdbe.c:4384). Our engine renders a **2-row stub**
(`internal/execquery/explain.go:11 execExplain` — `Init`/`Return` with
`fmt.Sprintf("%T", s.Statement)` leaking `*sql.SelectStmt` into the comment
column). Every leaf below dies on that stub.

**The unification**: ONE engine surface (a plan-derived synthetic opcode
stream) closes ~45 leaves through three consuming surfaces:

1. **`explain()` TVF** — indexexpr2-4.200/4.210/4.220/4.900.
   C contract: `ext/misc/explain.c` — eponymous vtab, schema
   `CREATE TABLE x(addr,opcode,p1,p2,p3,p4,p5,comment,sql HIDDEN)`
   (explain.c:93), xFilter wraps the hidden sql with `EXPLAIN %s` and walks
   the program (explain.c:192-214). `load_static_extension db explain`
   enables it (indexexpr2.test:204). We error "no such table: explain".
   ~250 LOC engine (a vtab module over the same stream).
2. **Per-row EXPLAIN callback with globals** — index9 (19 leaves, the whole
   family). C contract (`test/index9.test:20-30`, the `sqluses` proc):
   `db eval "EXPLAIN $sql" a { if {$a(opcode)=="OpenRead"} { set ::T($a(p2)) 1 } }`
   then `SELECT name FROM sqlite_master WHERE rootpage IN ($in) ORDER BY 1`
   — the fixture's `IN ()` for ALL 19 cases is the converter having dropped
   the runtime-populated `$in` (the callback never ran). Needs: a converter
   `explain_rows` step (per-row vars addr/opcode/p1..p5 + a callback body
   that writes runtime globals — the `query_first_column`/callback-interp
   pattern) + a harness executor arm + faithful OpenRead p2 rows.
   Sub-quota: the unknown-step-type whitelist in
   `tools/tclconvert/main.go:481` (`groupToTestCase`) must list the new
   step type or it is silently dropped (the runtime_list lesson).
3. **Plain EXPLAIN content** — where2-2.5/2.5b (regex wants `/ random/` and
   `/ SorterOpen /` over the flattened EXPLAIN output; the fixture steps are
   plain `query` steps — no harness work needed, pure engine fidelity).

**bestindexF's EXPLAIN chain** (setup_4..13 + 2.x.0 leaves, ~20): the
fixture's `uses_idxinsert` proc does
`lsearch [db eval "explain $sql"] IdxInsert` (bestindex8.test:103 shape —
bestindexF.json's `file: bestindexF.test` is a fossil of the same class).
Two layers: (a) engine EXPLAIN fidelity (IdxInsert/SorterOpen/Function rows),
(b) the harness chain `query_assign(EXPLAIN, resultAll) → runtime_expr
[lsearch $vm IdxInsert]` — the runtime_expr arm does not support command
substitution (vacuum2-3.1's signature "command substitution is not supported
in runtime expr" is the same gap). The got `[""]` for `idx` = the runtime_expr
never stamped the variable.

Verdict: **ENGINE GAP (substrate) + CONVERTER GAP (explain_rows step) +
HARNESS GAP (runtime_expr command substitution)**. No C-source oracle risk:
the stub is already known-wrong; the oracle (`../sqlite/testfixture`,
DYLD_LIBRARY_PATH=/tmp/tclshim) can render every EXPLAIN row stream needed
for pinning.

Risk: LOW for greenness (no currently-green leaf depends on EXPLAIN rows
except through these same families). Effort: substrate ~500-700 LOC (the
tree-walking executor has no opcode stream — like the vmstep card, this is a
SYNTHETIC stream derived from the resolved plan, not an instrumentation of
execution); TVF ~250; explain_rows step ~150.

---

## 3. Bucket B — register_tcl_module completion (~34 leaves)

The surface EXISTS: harness handler
`frigolite_harness_test.go:4361` (`newHarnessTclCallbackRuntime` +
`execDB.RegisterVtabModule("tcl", vtab.NewTclCommandModuleWithRuntime(runtime))`),
engine `internal/vtab/tclcmd.go` + `tclcmd_callback.go` (orderby handling at
tclcmd_callback.go:99). C contract: `src/test_bestindex.c:845`
(`register_tcl_module`, the `tcl` vtab module; xBestIndex returns
`cost/rows/idxnum/orderby/distinct/use/omit` lists, xFilter returns
`[list sql ...]`).

Residue decomposes:
- **bestindexF 2.x.1 (10 `res` leaves)** — xBestIndex writes
  `::vtab_orderby`/`::vtab_distinct` globals consumed later by the fixture.
  The callback runtime must PUSH/PULL written globals around every callback
  (exactly the S25 UDF-global round trip:
  `tcl.NotifyScriptVarTargets` over the proc bodies, push present runtime
  values before each call, pull written values after).
- **bestindexF 2.x.2 (10) + 1.4.1 (1)** — engine: the DISTINCT-consumed and
  ORDER-BY-consumed vtab decisions must drive row multiplicity/order
  (want `[3 4 1 2]` for `SELECT DISTINCT a FROM t1` — the module's
  `orderby 1` consumed decision + distinct flag; vtab.c sqlite3VtabInSync
  region + where.c distinct handling).
- **bestindexE (8)** — 1.1.2/1.2.2/2.1.2/2.2.2 are xBestIndex-global leaves
  (want `[{x1: a=?}]` etc. — the same push/pull); 3.1.0-3.2.3 exercise a
  tcl-vtab named `tcl` end-to-end (read via `SELECT rowid, * FROM tcl`,
  write via `INSERT ... RETURNING *`) — got "invalid command name """
  (the module registration was dropped for this setup; converter may need
  the register_tcl_module step for that section, or the callback runtime
  must serve named reads).
- **bestindex6-1.2/1.3 (2)** — the module's WHERE list uses `%N%`
  placeholders (`"$col = %$iCons%"`, bestindex6.test:50) which test_bestindex.c
  substitutes with argv values; our engine executed the raw SQL with `%`
  intact → "near %: syntax error". The TclCommandModule must substitute
  %N% in the xFilter SQL.
- **bestindex1-3.4/3.5 (2)** — vtab IN-constraint: `a.ColumnA IN ('ValueA',
  'ValueB')` must run xFilter once per value (got 1 row-set, want 4 rows ×2
  sets). C: wherecode.c:1598-1610 (OP_VFilter + the OP_Integer/OP_VNext
  loop) + vtab.c sqlite3VtabIn. Engine-side loop over IN values.
- **bestindex1-4.1 (1)** — the skip-map entry
  (`frigolite_harness_test.go:1157`) did not fire (t.Errorf instead of
  Skip) — 1-line harness fix.

Verdict: **HARNESS GAP (global round trip, %N%, skip) + ENGINE GAP
(distinct/orderby consumption, IN-loop)**.

---

## 4. Bucket C — expression-index value representation (16)

- **expridx1 (10, fossil fixture)** — index on `(a, b+0.0)`;
  `UPDATE x1 SET b=26.0` then `PRAGMA integrity_check` must REPORT
  `row 3 missing from index i1` (C's stored vs recomputed representation
  mismatch), and the NOT-INDEXED probes want the rows C hides. Our engine
  reports `ok` — the update path and the integrity recompute use the same
  folded representation. Contract: build.c expression-index key storage;
  update.c `indexColumnIsBeingUpdated` (update.c:96 — false-negatives
  corrupt the index); pragma.c integrity recomputation. **Probe with the
  oracle first** — the fossil makes the fixture's baked wants authoritative
  only for the 3.51-era C.
- **indexexpr2-4.120/4.130 (2)** — `refcnt()` UDF invocation counts:
  `UPDATE t1 SET b=b+1` must invoke the index function exactly TWICE
  (old-entry delete + new-entry insert; got 3), and `SET d=d+1` (no index
  column changed) must invoke it ZERO times (got 3). Contract:
  update.c:88-120 `indexColumnIsBeingUpdated` — skip unchanged indexes.
- **indexexpr2-1.2 (1)** — expression-index scan order under
  `COLLATE nocase` comparison (got [1 0] want [0 1]).
- **indexexpr2-11.0 (1)** — `sum()` in a correlated scalar subquery with
  GROUP BY (got "misuse of aggregate").
- **indexexpr3-1.5/1.6 (2)** — json_extract inside an index expression
  (got `[0 {...}]` want `[2 {...}]` — the WHERE a=2 row's first column
  wrong when the index expression carries json functions).

Verdict: **ENGINE GAP**; the 4.120/4.130 pair is the cleanest first cut.

---

## 5. Bucket D — planner/EQP residue (45)

| family | leaves | shape |
|---|-------:|-------|
| skipscan5 | 26 | skip-scan boundaries: C plans SCAN where we plan `SEARCH ... (ANY(a) AND b>?)` (1.3.x, 3.3.x) and we miss `ANY(a) AND ANY(b) AND c>?` skip-scan on 3-col indexes (2.1.x-2.3.x). Plus 2.1.2/2.2.2/2.3.2+2.1.3/2.2.3/2.3.3 "no such collation sequence: test_collate" (the fixture registers a collation the converter dropped — stale/cascade sub-bucket ×6). |
| pushdown | 6 | MySQL push-down evaluation ORDER: index-covered terms (`f(c)`) evaluate before table-fetch terms (`f(b)`), short-circuit kills the rest (want `[c2]` vs got `[b2 c2]`). test/pushdown.test:33-50. |
| coveridxscan | 3 | covering-index scan row ORDER (`SELECT a FROM t1` must deliver index order; got [5 4 3] want [3 4 5]). |
| index6 | 4 | partial-index eligibility (`b<>99` partial index must serve `WHERE b<>99`) ×2 + ANALYZE stat for a schema-qualified-column index (`xyzzy.t3.b`) ×1 + join EQP ORDER BY delivery ×1. |
| index7 | 4 | EQP: NOT LIKE + covering index (`bad1`) ×1; stat1 for qualified-column partial index ×1; "no such table: out" cascade ×1; partial index after ANALYZE ×1. |
| indexedby | 2 | INDEXED BY inside a VIEW must propagate to EQP (`SEARCH t1 USING INDEX i1` through `v2`). |
| index3 | 1 | `b='x' COLLATE nocase` must serve the nocase index (the S32 seek-collation gate class at the EQP level). |
| skipscan1 | 2 | OR-arm skip-scan EQP (`ANY`) + the t9a subquery plan. |

Verdict: **ENGINE GAP** (planner cost model + partial-index eligibility +
index-collation delivery), plus a stale-fixture sub-bucket (skipscan5's
test_collate ×6 — regenerate first).

---

## 6. Bucket E — fts3 residue (67)

| sub-bucket | leaves | shape |
|---|-------:|-------|
| binary-format converter card | ~17 | fts3defer2 11 + fts3first 6 (via 1.3.x#01) + fts3query-3.3: "unsupported binary form: scan ..." — the carded binary-format class (S12 record: 43 source files). CONVERTER CARD. |
| fts3defer2 MATCH results | 8 | `MATCH 'f (e a)'`/NEAR/phrase returns [] (1.2.x, 2.2.3.x, 2.4.3) — deferred-token MATCH. ENGINE. |
| fts4umlaut | 14 | `DELETE FROM t1/t2` reports changes 0, want 1 — FTS4 delete with the umlaut/unicode61 fold. ENGINE. |
| fts3query | 7 | rowid-range on FTS: `WHERE rowid BETWEEN 5 AND 10` returns ALL rows (7.2.1.x/7.2.6.x/7.3..x); `INSERT ft4(rowid) VALUES(-9.22e18)` UNIQUE fail (7.2). ENGINE. |
| matchinfo validation | 3 | `matchinfo()` arg-count + `matchinfo(content)` first-arg errors (5.2.1/5.3.1/5.4.1). ENGINE. |
| stale/cascade | 12 | e_fts3 setups ("no such table: docs"/ta_segdir ×5 + optimize t4 ×2), fts3cov-11.1 ("table xx already exists"), fts3_malloc ×4. REGEN candidates (stale-fixture signature). |
| misc | 5 | fts3defer icu tokenizer + cascade (3), fts3defer3-1.7 (expected error, MATCH on a corrupt table), fts3auto-setup_2 (`INSERT INTO t1 () VALUES(...)` — empty column list parse). |

---

## 7. Buckets F/G — authorizer events + tkt

**F. Authorizer event collection (10)**: savepoint-9.1/9.2/9.3,
without_rowid3-18.2/18.3/18.4/18.5/18.7, tkt-f7b4edec-1/3 — all
`runtime variable mismatch got [""] want [SQLITE_INSERT ... SQLITE_READ ...]`.
The harness HAS `auth_proc` (`db auth <proc>`, harness:5905) and the engine
has `internal/auth`; the residue is event COLLECTION: the authorizer proc
must append the event tuples to a fixture global and that global must round
trip to runtime_vars (the same S25 push/pull pattern as Bucket B).
**HARNESS GAP; ~100-150 LOC; one surface closes 10 leaves across 3 families.**
C contract: auth.c:104/136 (sqlite3AuthReadCol/AuthRead), SQLITE_SAVEPOINT
actions for savepoint-9.x.

**G. tkt (18)**:
- schema-qualified DML names: tkt-d82e3f3721-2.1/2.2 + tkt3838-1.2
  ("no such table: main.t3" / "main.T1") — ENGINE (one resolver fix class, 3 leaves).
- tkt-5ee237-1.1#01 — readonly-write rejection (got success).
- tkt-7a31705a7e6-1.1 — "no such column: t1.a" (qualified column in a
  joined scope). ENGINE.
- tkt-80ba2-150 — runtime var x1 unset (converter).
- tkt-b72787b1.1 — CASE + UDF result.
- tkt-c694113d5.100 — NOT EXISTS probe (want contains "CREATE INDEX" —
  the fixture body composes strings; converter).
- tkt-cbd05-1.3/2.3 — "no such function: test_extract" (fixture UDF not
  registered — harness testfuncs).
- tkt-f7772-1.2/2.2 — tx nesting ("cannot start a transaction within a
  transaction"). ENGINE/harness tx-state.
- tkt-bd484a090c-2.1/2.2 — datetime('localtime'/'utc') expected-error
  timing. ENGINE.
- tkt-26ff0c2d1e-3 — C-API step: want column value [555], got SQLITE_ROW
  (the C-API step surface must read the column after the step — same
  family as without_rowid3-17.1.x). HARNESS.
- tkt2686-setup_1 — "table filler already exists" cascade (stale/regen).

---

## 8. Buckets H-K — rtree / vacuum / without_rowid / savepoint+temptable

**H. rtree (88)**:
- rtree1 setup_21..43 (**23 leaves, ONE root cause**): "cannot start a
  transaction within a transaction" on `BEGIN; INSERT INTO t1 VALUES(4, 4,
  5, 6, 7);` — a prior setup's tx never closed (tx-state leak in the exec
  chain before these setups). Fix the leak → 23 leaves fall.
- rtree1-12.3.* (6): ON CONFLICT clauses on rtree shadow INSERT — C enforces
  constraint actions per shadow-table write. ENGINE.
- rtreeJ (11): tx nesting (3) + UNIQUE constraint on shadow tables (3) +
  row mismatches after failed inserts (4). ENGINE.
- rtreeA (15): node-blob reads via `SELECT data FROM t1_node` (got `[lob]` —
  the blob renders literally; harness blob rendering or the node payload is
  wrong) + integrity enforcement (2.1.x expected errors). MIXED.
- rtreefuzz001 (8): corrupt-db classification — got "no such table" want
  "database disk image is malformed"/"rtree constraint failed". ENGINE.
- rtreecheck (5): binary-format crafting (2) + rtreecheck message bodies (3).
- rtreedoc (15): binary-format blobs + "database table is locked" + geopoly
  shapes + page counters. MIXED.
- rtreecirc (2 cascade) + rtreedoc3-setup_0 ("no such function: qbox" —
  dropped proc).

**I. vacuum (20)**:
- vacuum-into (9): "output file already exists" ×6 (C deletes/overwrites the
  target or the error lands at a different point), "invalid command name
  target" ×1 (dropped proc), cascade ×2. ENGINE+CONVERTER.
- e_vacuum (5): freelist/page accounting after auto_vacuum (`want [-7.0]`
  = the fixture's computed delta; our page counts differ). ENGINE.
- vacuum2 (4): 3.x = "command substitution is not supported in runtime
  expr" ×3 (the same runtime_expr gap as Bucket A!) + 5.4 blob. HARNESS.
- vacuum3 (2): blob query_assign + cascade.

**J. without_rowid rest (15)**:
- without_rowid7 (4): `PRAGMA index_info/xinfo` on a WR table's implicit PK
  index must report the PK columns (want `[0 0 a 1 0 a]`; we report one row).
  pragma.c:1387-1410 iterates Index.aiColumn. ENGINE.
- without_rowid3-17.1.x/19.x (7): C-API step codes (SQLITE_CONSTRAINT vs
  MISUSE/ERROR/DONE/OK) — constraint timing on WR writes. HARNESS (step
  codes) + ENGINE (constraint timing).
- without_rowid3-14.2.x (6): `sqlite_rename_table` legacy quoting
  (want `REFERENCES "t3"`, got t2/unquoted). ENGINE (rename package).
- without_rowid3-10.1.4 (1): FK mismatch message shape.
- without_rowid3-3.2.1 (1): (probe at unit time).

**K. savepoint/temptable rest (14)**:
- savepoint-5.1.1/5.2/5.3.4/5.4.3/5.4.4 (5): SAVEPOINT/RELEASE error
  timing for unnamed/savepoint-less states (C errors where we succeed and
  vice versa). ENGINE.
- savepoint7 (3): multi-statement exec with RELEASE mid-step (got [] —
  the release discarded the row set). HARNESS/engine statement boundary.
- savepoint6-1.2 (1): `PRAGMA incremental_vacuum` return.
- temptable-6.3/6.6/6.8 (3): temp-table visibility/error timing across
  named handles. ENGINE.
- temptable2-4.1.4/5.1.2 (2): **the carded temp-spill pager unit**
  (`PRAGMA temp.page_count` want 1, got 350) — carded, do not re-diagnose.

**L. e_expr/expr (5)**: e_expr-26.1.6 (carded A2) + e_expr-32.2.8 (carded
A9 real-rendering layer) + expr-16.100/101/102 (`implies_nonnull_row` —
the test-only probe function of the LEFT-JOIN strength-reduction
optimization; register it in the harness testfuncs family and implement
its contract). HARNESS+ENGINE.

---

## 9. Slice order (smallest-first) + LOC estimates

| slice | closes | effort | risk | verify |
|---|---|---:|---|---|
| S37 authorizer event collection | 10 (F) | 100-150 LOC harness (S25 push/pull pattern on the auth proc) | LOW | `FRIGOLITE_TEST=savepoint` + `without_rowid` + `tkt-f7b4` sweeps; name-set diffs |
| S38 WR pragma index_info | 4 (J) | ~60 LOC | LOW | `FRIGOLITE_TEST=without_rowid7` |
| S39 tkt schema-qualified names | 3-4 (G) | ~40 LOC resolver | LOW | `FRIGOLITE_TEST=tkt-d82e3` + `tkt3838` |
| S40 implies_nonnull_row + harness UDFs | 3 (L) + test_extract(2) | ~80 LOC | LOW | `FRIGOLITE_TEST=expr` + `tkt-cbd05` |
| S41 EXPLAIN substrate P1 (synthetic stream + `explain()` TVF) | 6 direct (where2 2 + indexexpr2 4) + unblocks S42 | 500-700 engine + 250 TVF | MED | `FRIGOLITE_TEST=where2` + `indexexpr2`; oracle-pin the stream per statement shape |
| S42 explain_rows converter step + index9 | 19 (A) + bestindexF EXPLAIN chain ~20 | 150-200 conv+harness | MED | `FRIGOLITE_TEST=index9` + `bestindexF`; whitelist the step in tclconvert main.go:481 |
| S43 tcl-module completion | ~34 (B) | 200-300 (globals round trip, %N%, IN-loop, distinct/orderby) | MED | `FRIGOLITE_TEST=bestindex` sweep |
| S44 expridx1 representation | 10 (C) | ~150-250 + oracle probe first | MED-HIGH (subtle REAL folding) | `FRIGOLITE_TEST=expridx1` |
| S45 pushdown order | 6 (D) | ~100-200 | MED | `FRIGOLITE_TEST=pushdown` |
| S46 skipscan5 cost model | 20 (D) + 6 stale-regen | ~300-400 | MED-HIGH | `FRIGOLITE_TEST=skipscan` |

Queue additions outside the slices: (1) fts3 stale-regen slice
(e_fts3/fts3cov/fts3_malloc, 12 leaves, pure regen); (2) rtree tx-leak
root-cause (23 leaves for one fix + rtreeJ); (3) VACUUM INTO overwrite
(9); (4) C-API step-code surface (without_rowid3 17.x/19.x +
tkt-26ff0c2d1e, ~8); (5) binary-format card unchanged; (6) temp-spill
pager card unchanged.

**Single-surface unification wins, scored:**
1. EXPLAIN-row substrate: **~45 leaves** through one engine stream + two
   consuming surfaces (TVF, callback step) + one shared harness gap
   (runtime_expr command substitution, which also closes vacuum2-3.x).
2. tcl-module completion: **~34 leaves** — the surface already exists
   (harness:4361, internal/vtab/tclcmd*.go); what's missing is scoped
   plumbing, not a new architecture.
3. Authorizer event collection: **10 leaves** across 3 families on a
   surface that already half-exists.

## 10. Flake watchlist (unchanged)

temptable2-9.1.x/9.2.x + 4.1.1 (stress dithers), where2-2.4, atof1-2.40,
vacuum-into-500 (file-lifecycle dither, gate-only here). Do not chase
single occurrences.

## 11. Verification commands

```bash
# full gate (the only complete net; sequential, never concurrent)
cd /Users/muaddib/dev/frigolite && go test -run '^TestSQLiteSuite$' -count=1 \
  -timeout 60m -v > /tmp/full_gate_Sxx.log 2>&1
python3 -I -c "import re;d=open('/tmp/full_gate_Sxx.log','rb').read();\
print(len([n for n in re.findall(rb'--- FAIL: TestSQLiteSuite/([^\s(]+)',d) if b'/' in n]))"

# per-family (substring match; run once)
FRIGOLITE_TEST=index9  go test -run '^TestSQLiteSuite$' -count=1 -timeout 30m -v
FRIGOLITE_TEST=bestindex go test -run '^TestSQLiteSuite$' -count=1 -timeout 30m -v
FRIGOLITE_TEST=expridx1 go test -run '^TestSQLiteSuite$' -count=1 -timeout 30m -v

# oracle for EXPLAIN streams (3.51.0)
DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture
```

Worktree evidence: `/private/tmp/wt_s36triage` (branch `s36-index-triage`),
per-family logs `/tmp/s36_run_<fam>.log`, leaf sets
`/tmp/s36_famleaves.json`, detail blocks
`/tmp/s36_details_index.json` + `/tmp/s36_details_coarse.json`.

---

## 10. S43 RESTART STATE (2026-11-22): the bucket-B residue measured fresh, the first forensic thread recorded

**The fresh sweep** (FRIGOLITE_TEST=bestindex, python raw-scan vs the s35g
baseline): 29 fails, NEW=0 — bestindex1 2, bestindex6 2, bestindexE 8,
bestindexF 17. index9 = 0 (S42's explain_rows work landed earlier — the
step exists in the converter main.go whitelist AND the harness :6218).

**bestindex6-1.2/1.3's forensic capture** (a temporary TCLFILTER print in
FilterPlan, reverted): the xFilter returns
`SELECT rowid, * FROM t1 WHERE 1 AND id = %1%` with idxStr `1 AND id =
%1%` and argv `[1]` — the `%1%` placeholder SURVIVES because the proc's
map loop (`for {set iCons 0} {$iCons < [llength $aa]}`) indexes the argv
by the CLIST position while our argv holds ONE value (the compact
consumed form). C passes this case — its xFilter argv must be
**index-aligned by argvIndex with NULL gaps** (wherecode.c allocates the
register block to max(argvIndex); the unconsumed slots stay NULL), so
`aa` has a slot per clist position up to the max. The fix lever:
**buildVtabFilterArgv must pad the argv to max(ArgvIndex) with nil gaps**
— our argv is ALREADY index-aligned by ArgvIndex (make([]interface{}, n))
so the length IS max(argvIndex)=1 here — the real gap is that C's clist
for this query yields TWO slots (the proc consumed iCons=1, meaning c0
was skipped) while ours yields one — **the offered-constraint list shape
(the ON term + the WHERE's IS NULL as c0/c1 and their usable flags) must
be cross-checked against C's bestindex6.test offering order.** Note the
returned SQL targets `t1` while the case's query names vt1/vt2 — the
fixture's vtab-naming (xConnect's $src) needs the same cross-check.

**The next slice order** (the triage's bucket B, updated):
1. bestindex6-1.2/1.3 (2): the argv-alignment + the offering-order
   cross-check above.
2. bestindex1-4.1 (1): the skip-map entry's t.Errorf → Skip (1 line).
3. bestindex1-3.4/3.5 (2): the vtab IN-constraint loop
   (wherecode.c:1598-1610, OP_VFilter + OP_VNext per value).
4. The xBestIndex-global push/pull (bestindexF 2.x.1 ×10 + bestindexE
   1.1.2/1.2.2/2.1.2/2.2.2 ×4): the S25 NotifyScriptVarTargets round
   trip around every callback.
5. The distinct/orderby consumption (bestindexF 2.x.2 ×10 + 1.4.1) and
   bestindexE 3.x's named-read module registration.

---

## 11. S43 SLICE 1 DELIVERED (2026-11-22): bestindex6-1.2/1.3 GONE — the left-join null-match exclusion

**The oracle cross-check settled both open questions from section 10**:
1. The returned SQL's `FROM t1` is CORRECT — the fixture's
   `CREATE VIRTUAL TABLE vt1 USING tcl(vtab_command t1)` passes the
   SOURCE table as the module arg, so xConnect's `$src` and xFilter's
   `FROM $src` legitimately target t1 (the "naming mystery" dissolved).
2. **C's xFilter argv is COMPACT** (aa len=1 for one consumed
   constraint) — section 10's index-aligned-with-NULL-gaps hypothesis was
   WRONG. The real divergence: **C's clist holds only the ON constraint**
   (c0: op=eq col=0 usable=1) while ours offered TWO (the WHERE's
   `vt1.value IS NULL`/`IS $xxx` as c0, shifting the proc's %iCons%
   placeholder to %1% past our one-slot argv). The WHERE's IS-NULL/IS
   forms on the scan's own table are the LEFT JOIN's null-row match tests
   and never reach C's clist (where.c:1453's TERM_VNULL skip — the
   oracle measured both 1.2's IS NULL and 1.3's IS $xxx excluded).

**The fix** (internal/execquery): VtabScanOptions.LeftJoinInner (set at
select_join.go's vtab site from join.JoinType) + BuildVtabIndexInfoWithInstance's
exclusion of the null-match conjuncts (vtabLeftJoinNullMatch: <own column>
IS NULL, or <own column> IS a NULL literal / unbound parameter). The term
stays in the residual WHERE so the row matching is unchanged.

**The gates**: bestindex6 2→0; the bestindex universe 29→27 (NEW=0);
tabfunc/series/wholenumber/generate/index9 all green. The FULL-suite gate
runs as this section's commit gate.

**The updated slice order**: the next cuts are bestindex1-4.1 (the
skip-map entry, 1 line), the vtab IN-constraint loop
(bestindex1-3.4/3.5, wherecode.c:1598-1610), the xBestIndex-global
push/pull (bestindexF 2.x.1 ×10 + bestindexE ×4 — the S25
NotifyScriptVarTargets pattern), distinct/orderby consumption
(bestindexF 2.x.2 ×10 + 1.4.1), bestindexE 3.x's named reads.

---

## 12. S43 slice 3 (the IN-loop) DEFERRED (2026-11-22): the trace shows a join-flow outer-iteration collapse

bestindex1-3.4/3.5's live trace (a temporary TCLFILTER print, reverted):
the a-side's module filter runs ONCE (`SELECT rowid, * FROM t1`, argv [],
4 rows — the statement WHERE's FlagA eq is NOT in its idxStr: the join's
left operand gets no statement-WHERE pushdown), the b-side's seek runs
TWICE with argv `[1]` BOTH times (`WHERE PrimaryKey = %0%`, 1 row each)
— the b-side's argv is stuck at the FIRST a-row's PrimaryKey and the
outer iteration collapses (got 1 joined row, want 4). The IN constraint
itself is NOT consumed by the module (the proc only consumes eq), so the
residual IN is correct — the failure is the JOIN's outer-loop iteration
feeding stale outer values to the inner vtab's seek (the
vtabRowidEquiOuters/seek path's outer maps), not the IN handling.
Slice order update: the IN-loop leaves stay deferred behind the join-flow
trace; the independent cuts proceed (the globals push/pull, distinct/
orderby).

---

## 13. S43 slice 1's ENGINE surface delivered (2026-11-22): four root-cause fixes across the vtab callback stack

The bestindex6 fix exposed THREE deeper shared-machinery bugs, all fixed
in this slice (the leaf-level wins measured on the stale fixtures:
bestindexE 8→7, bestindex6 2→0, bestindexF/bestindex1 unchanged with
NEW=0; the FULL gate = this section's commit gate):

1. **The left-join null-match exclusion** (section 11): the TERM_VNULL
   parity via VtabScanOptions.LeftJoinInner + vtabLeftJoinNullMatch.
2. **The braced-word module args** (internal/vtab/tclcmd_callback.go's
   moduleCommandWords): tclWords split braced spans — `USING
   tcl('vtab_command x1 {a b c}')` delivered lCol as three words, so the
   proc's params misbound ($method = a list element "b", the xBestIndex
   case never matched, the schema fell back empty). The module args are
   TCL word-segmented with braced spans as single words.
3. **The xBestIndex-global round trip** (the S25 pattern in
   frigolite_tcl_vtab_callback_test.go's harnessTclCallbackRuntime): the
   runtime now takes the harness's runtimeVars map, seeds the tracked
   targets (NotifyScriptVarTargets over each callback proc's body), and
   pushes the present values in / pulls the written values out around
   EVERY callback invocation — bestindexE-1.1.2's ::xBestIndex now lands
   in the runtime set (the rtgeomBoxFixture.syncVars precedent).
4. **The $-name scanner's single-colon bug** (tools/tclconvert/tcl/
   interp.go's readVarName): "$zName: [join ...]" read the name as
   "zName:" (the colon consumed) so every substitution after a name
   followed by a colon returned "". TCL's rule: only '::' continues; a
   single colon terminates. This affected EVERY quoted substitution of
   the form "$name: ..." across the whole interp.

**The regen card**: regenerating the corpus (go run ./tools/tclconvert/)
now emits a corpus-wide fixture migration (1145 files: the callbackVars'
runtime-var markers like "G(cols)": "$G(cols)", the runtime_var_reset
steps for the callback-written globals, the .gz variants) — a
project-wide fixture/converter alignment task, CARDED separately from
S43. The blanket regen was reverted; the bestindex families keep the
stale fixtures until that card runs (bestindexE's 1.2.2/2.1.2/2.2.2
accumulation leaves need the regen's runtime_var_reset steps).

**The slice's FULL gate** (/tmp/s43_gate2.log, 1122.9s): the wins = bestindex6-1.2/1.3, bestindexE-1.1.2 (+ temptable2-4.1.1/where2-2.4's benign draws); the 67 NEWs = ALL avtrans — **the draw-dependent flake class**: the fixture's case names carry `$::sig` (the DB checksum) and EVERY sig matched the previous gates exactly (the DB state deterministic), but the t3 workload's `INSERT ... SELECT randstr(10,400)` / `random()%10` are UNSEEDED — this gate's draw hit a "database disk image is malformed" in setup_17 that cascades through the 9.x cases; isolated runs pass 6/6. Same class as the resetdb load flap (TODO watchlist); the malformation itself = a real AV-machinery residue bug to chase in its own unit, not a btree-density or vtab matter. The slice's identity = the three GONEs with zero non-avtrans NEWs.

---

## 14. S43 slice 2 DELIVERED (2026-11-22): the selective fixture regen + the seeding machinery — 11 leaves gone

**The corpus regen card ran with the selective bestindex keep-set** (go
run ./tools/tclconvert/; the non-bestindex fixtures reverted; the stray
.gz/untracked regen artifacts removed): the kept 16 bestindex fixtures
landed the runtime_var_reset steps AND the runtime-var markers
("testdir": "$testdir", "G(cols)": "$G(cols)").

**Four supporting fixes were required**:
1. **The converter's file-level array-set seeding** (interp.go's
   set-handler + commandRegisterTCLModule's flush): the file-level
   array-element sets (`set G(collist) [list PrimaryKey flagA columnA]`,
   bestindex1.test:173) are DEFERRED and flushed at the next
   register_tcl_module whose registered procs READ them
   (ScriptVarReads: the `global` decls + the $-references) — the flush
   emits the runtime_expr seed steps (the quoted-literal round trip)
   BEFORE the register snapshot. bestindex1's seeds land in setup_5
   (the G sets), setup_6 carries the register — C-exact ordering. The
   emission's scope = the file-level array sets ONLY (the naive
   immediate emission fired 37,500 times across the corpus — the
   procs' per-case array sets).
2. **The harness's register seeding** (the register_collation pattern):
   the callbackVars' plain values seed runtimeVars before the runtime
   construction.
3. **The runtime's marker resolution**: the callbackVars' "$name" values
   resolve against runtimeVars via evalHarnessRuntimeExpr.
4. **The SQL-safe rendering's bare lists** (sqlSafeRuntimeValue): the
   comma-separated bare-token lists land VERBATIM ("PrimaryKey,flagA,
   columnA" must stay an identifier list, not one quoted name) — C
   builds SQL by string interpolation.

**The gates**: bestindex6 0, bestindexE 6 (NEW=0; 1.1.2 + 1.2.2 green),
bestindex1 2 (the carded 3.4/3.5), bestindexF 15 (NEW=1: 2.11.0), the
select/where/where2 families = the baseline. The FULL gate
(/tmp/s43_gate3.log, 1142.7s): 1195 fails, GONE = 11 (bestindex6 ×2,
bestindexE-1.1.2/1.2.2, bestindexF-1.4.1 + 2.2.1/2.3.0/2.4.0/2.5.0/
2.10.1/2.10.2 class), the avtrans draw CLEAN this run, the flap class
= 12, **REAL NEW = 1: bestindexF-2.11.0** — the first leaf of the
DISTINCT+GROUP-BY ORDER-BY-consumed trio (2.11.0/1/2: the module's
ORDER-BY-consumed decision for `SELECT DISTINCT a, count(*) FROM t1
GROUP BY a` must drive IdxInsert/the idxStr/the delivery order — the
where.c ORDER BY consumption for the aggregate plans) = the NEXT
engine slice.

**The bestindex residue**: 24 leaves (E: 2.1.2/2.2.2 + 3.x ×4; F: 14
incl. the 2.11 trio; 1: 3.4/3.5).

---

## 15. S43 slice 3 DELIVERED (2026-11-22): the WHERE_GROUPBY consumption — the 2.11 trio green

**The C contract decoded** (bestindexF's 2.11 module + where.c): for
`SELECT DISTINCT a, count(*) FROM t1 GROUP BY a` on a vtab, the planner
offers the GROUP BY's terms as the ordering candidates (select.c:8615's
WhereBegin passes pGroupBy with WHERE_GROUPBY), the module consumes
(orderby 1, the idxStr = {"" ORDER BY ((a+2)%5)}), and the aggregate
runs STREAMING over the consumed order — no sorter, the rows delivered
in the scan order (select.c:8720's GroupByOrder cancellation).

**Three engine changes**:
1. **The WHERE_GROUPBY parity** (vtabOrderByTerms): the GROUP BY's terms
   are the ordering candidates offered to xBestIndex for aggregate scans
   (where.c:1519-1521's eDistinct = 1 when the GROUP BY's terms are the
   offered ordering). vtabDistinctHint's precedence fixed: the GROUP BY
   → 1, beating the plain DISTINCT's 2 (both → the GROUP BY wins per
   where.c:1521's branch).
2. **The consumed-order signal** (VtabScanOptions.GroupByOrderConsumed +
   the engine's vtabGroupByOrderConsumed): readVtabWithBestIndexPlan
   fills the pointer when ii.OrderByConsumed; the single-vtab
   materialization site propagates it to the statement's execution.
3. **The streaming aggregate** (select_agg.go's group phase): the group
   phase PRESERVES the scan order (partitionByGroupKey's first-seen
   keyOrder) instead of re-sorting the keys when the ORDER BY was
   consumed — select.c:8720's GroupByOrder cancellation parity.

**The gates**: the 2.11 trio GREEN (2.11.0's IdxInsert via the slice-2
flat-vm fix; 2.11.1's idxStr via the hint precedence; 2.11.2's delivery
via the streaming aggregate); bestindexF 17 → **11**; bestindexE 6,
bestindex1 2, bestindex6 0, where/where2/select/the generator families =
the baseline. The FULL gate = this section's commit gate.
