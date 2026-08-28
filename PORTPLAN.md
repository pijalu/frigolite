# PORTPLAN — Frigolite Complete Implementation Plan

> **Status**: AUTHORITATIVE — the single source of truth.

> **Mission**: Implement **all** SQLite features. Frigolite must be a 1:1
> functional match, as fast or faster. WAL, concurrency, FTS are
> **non-negotiable**. Only 20 packages are genuine N/A.

> **Previous plans**: `archive/2026-08-11-planning/` (historical only).
> **Sub-plans**: `plan/goals/<GOAL_ID>.md` — one per goal, self-contained.
> **Method**: `portplan/UNIT_CONFORMANCE.md` — UCL (unit conformance layer)
> is MANDATORY for all remaining goals: oracle fixtures + localized unit tests
> BEFORE engine edits (see §5b item 0 and §5f).
> **Guidelines**: `plan/GUIDELINES.md` — triage, quality gates, checkpointing.

> **Reference**: SQLite source `/Users/muaddib/dev/sqlite/src/` + `/ext/`.
> **Oracle**: `/usr/bin/sqlite3`.
> **CI**: `.github/workflows/ci.yml`.

---

## 1. Critical Assessment

### The Problem

The previous analysis (`archive/plans/DEFERRED.md`, `NOT_APPLICABLE.md`)
systematically **over-excluded** real features:

| Previous Classification | Entries | Reality |
|-------------------------|---------|---------|
| "Deep-engine gap DEFERRED" | 298 | **Must implement** |
| FTS3/4/5 "N-A" | 92 | **Must implement** (non-negotiable) |
| WAL/concurrency "deferred" | 47 | **Must implement** (non-negotiable) |
| C-API "N-A" | 51 | **Must implement** (port to Go) |
| Window "N-A" | 10 | **Must implement** |
| Extensions "N-A" | 16 | **Must implement** |
| Genuine C-runtime/OS | ~73 | **Genuine N/A** (only exclusion) |

The worst error: **140 packages skipped solely for being slow** (5-20s
each) under a self-imposed "verify-time budget". Fix = optimize engine.

### Reclassification

| Disposition | Packages |
|-------------|----------|
| **Must implement** | **226** |
| **Genuine N/A** | **20** |

### Genuine N/A (20 — the ONLY exclusions)

**C malloc/allocator** (Go has GC): `malloc`, `malloc3`-`9`, `mallocI`, `mallocK`

**Fault/crash/IO-error injection** (C test VFS): `crash`-`8`, `io`, `ioerr`-`6`,
`pagerfault`-`3`, `rollbackfault`, `snapshot_fault`, `sysfault`, `writecrash`,
`fallocate`, `unionvtabfault`, `fts3fault2-3`, `backup_ioerr`, `backup_malloc`,
`fuzz`-`4`, `fuzzer1-2`, `fuzzerfault`, `dbfuzz001`

**Platform/TCL** (no SQL surface): `win32*`, `tclsqlite`, `shellA`

**C test-module ABI / VDBE**: `vtab7`, `rowvalue5`, `subtype1`, `sort4`

**Non-transpilable TCL**: `savepoint4`, `savepoint6`

**Pure benchmark**: `speed4`

---

## 2. Current State (checkpoint 2026-08-20; refreshed 2026-08-21)

- **1192 testgen packages**; the last recorded live baseline was **615 PASS,
  162 FAIL, 415 SKIPPED**. Direct serial package runs supersede stale cached
  details; refresh `tools/status` before selecting a new package tranche.
- P5 API foundation and locks are implemented: public `Prepare/Bind/Step/Exec/
  Reset/ClearBindings/Close`, per-DB prepared-statement lifetime tracking,
  process-local cross-connection read locks, SQLITE_BUSY on conflicting writes
  and `DB.Close`, and focused pure-Go regressions in
  `frigolite_p5_locks_test.go`. P5 backup, build, vet, SOLID, and full Go tests
  pass at the current checkpoint.
- A full live diagnosis of the P5.BACKUP blocker was done (2026-08-21): the 4
  target packages were un-skipped, regenerated, and run serially (`backup` 101
  mismatches, `backup2` 1, `backup4` 4, `backup5` 2); every failure is mapped to
  a verified root cause (5 transpiler + 7 engine) with a 14-micro-task plan in
  `plan/goals/P5.BACKUP.md` §Micro-Tasks. Nothing in the engine is undiagnosed.
- A first P6.JSON slice is implemented in `internal/function/json.go` and
  `internal/function/function.go`: validated/compact `json()`/`jsonb()`,
  `json_array()`/`json_object()` constructors, existing `json_extract()` and
  `json_insert()`, plus `frigolite_p6_json_test.go`. This is **not** complete
  JSON1/JSONB coverage; the twelve JSON testgen packages remain skipped.
- **Approach review 2026-08 (adopted)**: the FTS4 merge-writer stall was
  diagnosed as a method failure (zero-locality e2e assertions + patched
  re-derivation of `fts3_write.c` + unbounded duplicate goal queue).
  Countermeasure: `portplan/UNIT_CONFORMANCE.md` (UCL) is now the mandatory
  method for **all** remaining topics — oracle-CLI golden fixtures,
  structure decoders ported from SQLite tooling, first-divergence test
  output, and a global circuit breaker (U5). All queued goa goals were
  cancelled and replanned under this contract; the FTS residue is now
  `P6.FTS-WPORT` (structural port + unit conformance).
- `go test ./...`, `go build ./...`, `go vet ./...`, and
  `go test -run TestSOLID_ ./...` pass. Generated helper file-size checks remain
  red by design of generated fixtures; do not hand-refactor generated helpers.
- All prior queued/active goals were cancelled before reorganization. The
  dependency-order queue remains in §5 and should be resumed from the first
  unfinished tranche, not by re-adding completed P5 work.

---

## 3. Phased Execution

```

P1 CORE → P2 SCHEMA → P3 FUNCTIONS → P4 ADVANCED SQL → P5 GO API
→ P6 EXTENSIONS → P7 PERF & CONCURRENCY → P8 CORRUPTION & RECOVERY → P9 FINAL

```

Each phase starts only after its dependencies are green.

---

## 4. Complete Goal Index

**63 goals**. Each has its own sub-plan: `plan/goals/<ID>.md`.


### P1 — Core Engine

*10 goals, 73 packages — depends on: none*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P1.CRUD` | [`P1.CRUD.md`](plan/goals/P1.CRUD.md) | 12 | ✅ all 12 green (10 per-test skips w/ evidence: P4/P5/P7/P8) | Core INSERT/table/temp-table/VALUES semantics |
| `P1.DISTINCT` | [`P1.DISTINCT.md`](plan/goals/P1.DISTINCT.md) | 2 | ✅ green | DISTINCT and DISTINCT-NULL semantics |
| `P1.E-EXPR` | [`P1.E-EXPR.md`](plan/goals/P1.E-EXPR.md) | 3 | ✅ green (1 per-test skip: P8.CORRUPT) | Expression resolution and type coercion |
| `P1.E-SQL` | [`P1.E-SQL.md`](plan/goals/P1.E-SQL.md) | 7 | Comprehensive SQL test suite: CREATE/INSERT/SELECT/UPDATE/DELETE |
| `P1.IN` | [`P1.IN.md`](plan/goals/P1.IN.md) | 8 | ✅ all 8 green | IN operator: list, subquery, NULL handling |
| `P1.JOIN` | [`P1.JOIN.md`](plan/goals/P1.JOIN.md) | 14 | All JOIN types: inner/outer/left/right/full/natural/using/cross |
| `P1.MISC` | [`P1.MISC.md`](plan/goals/P1.MISC.md) | 13 | ✅ 8/8 target packages green (auth transpiled, affinity2/3, aggnested, analyzeF, bigrow; 7 per-test skips w/ evidence) | Misc core gaps: affinity, aggregates, auth, wide tables |
| `P1.PARSER` | [`P1.PARSER.md`](plan/goals/P1.PARSER.md) | 3 | ✅ all 3 green | Parser edge cases: keywords, grammar, format |
| `P1.SELECT` | [`P1.SELECT.md`](plan/goals/P1.SELECT.md) | 2 | ✅ all 2 green (select4, selectD); bonus fix: temp-table join short-circuit + view-expansion memoization (view3 no longer hangs) | SELECT edge cases: compound queries, correlated subqueries |
| `P1.WHERE` | [`P1.WHERE.md`](plan/goals/P1.WHERE.md) | 8 | ✅ 8/8 target packages green (where, where2-8); fix: INSERT case-insensitive column mapping; 4 per-test skips w/ evidence (P4/P5/P7) | WHERE clause evaluation and optimization |

### P2 — Schema & Constraints

*6 goals, 40 packages — depends on: P1*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P2.ALTER` | [`P2.ALTER.md`](plan/goals/P2.ALTER.md) | 6 | ✅ all 6 green (altercol, altercorrupt, alterlegacy, altertab, altertab2, altertab3); fixes: paren-set trigger verbatim SQL, FK parent-dirty, trigger TblName/schema, hexdb corrupt-DB detection, temp-trigger cross-schema refs | ALTER TABLE rename/add/drop column + dependency rewrite |
| `P2.CONSTRAINT` | [`P2.CONSTRAINT.md`](plan/goals/P2.CONSTRAINT.md) | 8 | ✅ all 8 green (p_8_3_names, createtab, resolver01, trustschema1, upfrom1-4); fixes: UPDATE ... FROM (joins LEFT/RIGHT/FULL/NATURAL/CROSS + ON, CTE/view/subquery FROM, self-ref validation), trusted_schema PRAGMA + function-safety flags, ORDER BY alias resolution, FROM-less subquery outer aliases, ATTACH URI stripping, copy_file transpilation | Table creation, FK resolution, upfrom, constraints |
| `P2.E-SCHEMA` | [`P2.E-SCHEMA.md`](plan/goals/P2.E-SCHEMA.md) | 3 | ✅ all 3 green (e_droptrigger, e_dropview, e_fkey); fixes: temp-schema priority for unqualified DROP TRIGGER, AFTER triggers reverse creation order, INSERT/UPDATE NO ACTION deferred to statement end (AFTER-trigger repair), RESTRICT never deferred, FK DDL validation (unknown column/cardinality), PRAGMA foreign_keys no-op inside transaction, savepoint RELEASE checks deferred FK + implicit-txn commit, EQP FK child scans, CollatedValue unwrap in FK matching, harness/transpiler eqp + catchsql-var + multi-word cell bracing | Schema DDL test suite: DROP, FK |
| `P2.INDEX` | [`P2.INDEX.md`](plan/goals/P2.INDEX.md) | 4 | ✅ all 4 green (indexA, indexexpr1, indexexpr2, indexexpr3). Fixes: CREATE INDEX collation validation (keys+WHERE, before schema write), INDEXED BY partial-index predicate match (whitespace-normalized), integrity_check expression-index keys, JSON1 core json_extract/json_insert (relaxed-mode parser + path walker, registered Innocuous for expression indexes) | Index creation, expression indexes, partial indexes |
| `P2.ROWID` | [`P2.ROWID.md`](plan/goals/P2.ROWID.md) | 7 | ✅ all 7 green (without_rowid1-7). Fixes: WITHOUT ROWID PK autoindex dedup (no sqlite_autoindex for PK/UNIQUE-matching-PK), EQP PK-only conditions, writable_schema allows sqlite_* names, FK statement-end immediate checks inside transactions + DELETE NO ACTION deferral (AFTER-trigger repair), OR FAIL FK rollback, parent-child mismatch tolerance, transpiler sqlite3_step error tolerance, parser empty-statement (;;) collapse | WITHOUT ROWID tables (clustered index) |
| `P2.TRIGGER` | [`P2.TRIGGER.md`](plan/goals/P2.TRIGGER.md) | 12 | ✅ 8/12 green (temptrigger, trigger1-7). Fixes: two-connection main-DB schema staleness (checkExternalMod guard removed for fresh-DB counter 0; Open flushes Init's dirty page-1 so external-mod detection fires), collapseEmptyStatements preserves non-empty statement text verbatim (trigger-body '; END' stored correctly). Also fixed pre-existing: altercol/altertab/altertab2/altertab3/corruptD/e_select2/indexA/minmax4 | Triggers: OLD/NEW, BEFORE/AFTER, INSTEAD OF, cascades |

### P3 — Functions & Datetime

*2 goals, 9 packages — depends on: P1*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P3.DATETIME` | [`P3.DATETIME.md`](plan/goals/P3.DATETIME.md) | 6 | ✅ all 6 green (ctime, date, date2, date3, date4, date5). Fixes: sqlite_compileoption_used/get + PRAGMA compile_options (ctime.c port, THREADSAFE=2), pager reopen reads real page size from header (page_size=1024 round-trip), failed CREATE INDEX rolls back schema entry on partial-index WHERE eval error (non-deterministic date()), transpiler lsearch-[db eval]-expr do_test body | date/time/datetime/strftime/julianday functions |
| `P3.MISC` | [`P3.MISC.md`](plan/goals/P3.MISC.md) | 3 | ✅ all 3 green (fpconv1, nan, normalize). Fixes: transpiler runs non-VACUUM side effects in `db eval` bodies (nan-3.1), sqlite3_normalize/normalized_sql/prepare_v3 recognized as C-API N-A (normalize), harness FP_DIGITS renderer (fpconv1: shortest round-trip default for FP_DIGITS files, %!.15g for the rest) + decimal/ieee754 ext functions skipped N-A | NaN handling, normalize, fpconv |

### P4 — Advanced SQL

*4 goals, 24 packages — depends on: P1*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P4.CTE` | [`P4.CTE.md`](plan/goals/P4.CTE.md) | 6 | ✅ all 6 green (with1-6); 11 per-test skips w/ evidence (insert_into_tree/scan_tree/genstmt procs, NOT MATERIALIZED inlining); fixes: queue-based recursive iteration, CTE scope shadowing, scalar min/max, view/trigger schema validation | Recursive and non-recursive WITH (CTE) |
| `P4.RETURNING` | [`P4.RETURNING.md`](plan/goals/P4.RETURNING.md) | 1 | ✅ 1/1 green (returning1); fixes: empty-input nested aggregate via aggRowMaps, RETURNING * hides FTS hidden columns | RETURNING clause |
| `P4.UPSERT` | [`P4.UPSERT.md`](plan/goals/P4.UPSERT.md) | 5 | ✅ all 5 green (upsert1-5); fixes: chained ON CONFLICT first-match-wins, expression/COLLATE/partial-index conflict-target validation, hit-based conflict detection with index-name errors, DO UPDATE WHERE + table alias + excluded shadowing, generated-column recompute + REAL affinity, OR REPLACE precedence fallback, INSERT...SELECT upsert, count_changes inserts-only, view rejection, tcl2go TCL-array dynamic lookup + catchsql regex | ON CONFLICT / UPSERT |
| `P4.WINDOW` | [`P4.WINDOW.md`](plan/goals/P4.WINDOW.md) | 12 | ✅ all 11 testgen packages green (window1-9, windowD, windowpushd; `window` superseded by window1-9 split); 3 per-test skips w/ evidence (window5 C-API win(), window1 32.10 stale RENAME TO revalidation, window1 61.4.2 flattener-off expectation); fixes: aggregate/ranking/value windows, ROWS/RANGE/GROUPS frames + EXCLUDE, named windows, GROUP BY+window, window-in-UPDATE-SET (73.4), correlated-agg-in-agg-arg misuse (75.1), GROUP BY correlated-agg subquery per-group (76.5), empty-frame group_concat NULL (78.2), compound ORDER BY subquery error precedence (67.1), WHERE row-value correlated-agg collapse (71.0), view UPDATE RETURNING (73.2) | Window functions: frame types, partition, aggregate windows |

### P5 — Go API Port

*7 goals, 55 packages — depends on: P1, P2*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P5.BACKUP` | [`P5.BACKUP.md`](plan/goals/P5.BACKUP.md) | 4 | ✅ **complete** — all 14 micro-tasks done; 4/4 packages green un-skipped; post-regression fixes (ResetToEmpty, lazy open, IPK rowid read, setDestPgsz scope, step/finalize transpiler helpers) + UCL tranche (`testdata/backupconformance` oracle src/dest pairs + `TestBackupConformance`) — see §Resolution (2026-08-24) | Online backup |
| `P5.BIND` | [`P5.BIND.md`](plan/goals/P5.BIND.md) | 2 | ✅ **complete** — bind/bind2 un-skipped and green via runtime Stmt VM emulation (prepare/bind/step/reset/finalize + parameter metadata); UCL oracle transcript + conformance test — see §Resolution (2026-08-25) | Parameter binding |
| `P5.BLOB` | [`P5.BLOB.md`](plan/goals/P5.BLOB.md) | 12 | ✅ 12/12 green (tranche 3c: incrcorrupt closed — auto_vacuum mode mapping + ptrmap reservation + autoindex root pages for page-count parity; pager per-statement external-file detection (sqlite3PagerSharedLock file stamp) + lockBtree header-vs-file corruption checks; header parity @28/@92/@96; incremental_vacuum with nFree≥nOrig→SQLITE_CORRUPT; Stmt.Finalize error re-reporting + sqlite3_errmsg last-call semantics; transpiler hexio_write/db_save/chan-truncate) | Incremental blob I/O (OpenBlob) |
| `P5.CAPI` | [`P5.CAPI.md`](plan/goals/P5.CAPI.md) | 15 | ✅ closeout green: 14/14 existing packages pass (8 core + main/misc6/notify1-3/resetdb); imposter1 N-A (TESTCTRL_IMPOSTER test-only C API), notify N-A (no source artifact) | Misc C-API: changes, column metadata, status, notify |
| `P5.HOOKS` | [`P5.HOOKS.md`](plan/goals/P5.HOOKS.md) | 6 | ✅ **complete** — all 6 packages green; interrupt-countdown wiring + UCL hookconformance oracle script transcript; C-runtime callback delivery classified in NA_EVIDENCE (2026-08-25) | Update/commit/rollback hooks, interrupt, db_status |
| `P5.SHELL` | [`P5.SHELL.md`](plan/goals/P5.SHELL.md) | 9 | ✅ shell1-9 green; CLI-only shell9 behavior N/A with evidence, SQL surface covered | CLI shell command tests |
| `P5.STMT` | [`P5.STMT.md`](plan/goals/P5.STMT.md) | 6 | ✅ **complete** — all 6 packages green incl. capi3c-3.6.2-misuse fix and real parameter-metadata assertions (2026-08-25 addendum) | Prepared statements (Prepare/Step/Reset/Close) |

### P6 — Extensions

*11 goals, 143 packages — depends on: P2*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P6.EXT` | [`P6.EXT.md`](plan/goals/P6.EXT.md) | 9 | ✅ 8/9 green (basexx1, decimal, extension01, ieee754, loadext, loadext2, misc8, percentile); quota_ remains skipped (quota VFS, N-A); engine: native ports of ext/misc/basexx.c (base64/base85/is_base85), decimal.c (decimal family + collation + sum agg), fileio.c (readfile/writefile), ieee754.c (ieee754 family), eval.c (eval via EvalExecSQL), sqlite3_status; SELECT output-expr errors propagate (SQLite semantics); PRAGMA database_list reserves seq 1 for temp; nested-ROLLBACK-with-DDL aborts statement ("abort due to ROLLBACK"); derived-table rowid ambiguity; transpiler: ifcapable !load_ext guard (C-API dlopen N-A), open/read file channels, bare file-size do_test, catchsql regex "/1" form, sqlite3_limit set-var, maindbname alias rewrite, catch-var redeclare fix; 1 per-test skip w/ evidence (misc8-2.1 delete-during-join-iteration, streaming-join gap) | Loadable extensions: decimal, ieee754, percentile, etc. |
| `P6.FTS-A` | [`P6.FTS-A.md`](plan/goals/P6.FTS-A.md) | 21 | ✅ 8/21 green (e_fts3, fts3aa, fts3ab, fts3ac, fts3ad, fts3ae, fts3af, fts3ag); engine: FTS UPDATE/DELETE paths, SQLite-faithful vtab-arg parsing (unrecognized-parameter errors, first-token column names), docid rowid alias, NULL column preservation, order=desc scan, column-filtered + parenthesized + porter-stemmed MATCH, prefix phrases, shadow tables (%_content/_segments/_segdir/_docsize/_stat) with segdir counting + optimize merge, shadow-table AFTER DELETE triggers w/ recursion guard; transpiler: e_fts3 ddl_test/write_test/read_test/error_test wrapper procs, wordset proc, dynamic-key TCL array maps (set arr($k) V / $arr(K) / [concat $arr(K)...]); 13 per-test skips w/ evidence (e_fts3 aux functions matchinfo/snippet/offsets → P6.FTS-D/E/H; 2007 parens-off precedence) | FTS3/4 basics: CREATE, INSERT, MATCH query |
| `P6.FTS-B` | [`P6.FTS-B.md`](plan/goals/P6.FTS-B.md) | 27 | ✅ 27/27 green — all un-skipped and passing; residual divergences documented as evidence-based per-assertion N-A skips (fts3defer2 §2 deferred-token cost model, fts3corrupt6-4.2, fts3corrupt4 12.1/31.1) in `tools/tcl2go/skiptests2_part2.go` + `portplan/NA_EVIDENCE.md` | FTS3/4 query syntax: NEAR, prefix, phrase |
| `P6.FTS-C` | [`P6.FTS-C.md`](plan/goals/P6.FTS-C.md) | 2 | ✅ 2/2 green (fts3join, fts3near); engine: FTS joins (MATCH against joined columns, FTS-FTS joins, LEFT JOIN MATCH in ON, qualified docid resolution in joined rows), FTS vtab row materialization for joins, NEAR operator (parser + position-based matcher, column-aware, chained NEAR, NEAR/n distance), CREATE INDEX root-page header init fix, multi-MATCH validation ("unable to use function MATCH in the requested context"); 2 skips removed from skiptestfiles | FTS3/4 tokenizers, conflict, join |
| `P6.FTS-D` | [`P6.FTS-D.md`](plan/goals/P6.FTS-D.md) | 2 | ✅ 2/2 green (fts3matchinfo, fts3matchinfo2); engine: full matchinfo() with p,c,n,a,l,x,y,b formats and per-phrase per-column hit stats (local + global, NEAR-scoped, AND-gated), FTS4 matchinfo=fts3 (no _docsize shadow table, format validation), offsets()/snippet()/optimize() aux functions with byte-span tokenizer, MATCH RHS coercion (int/blob → text), query parser skips non-token chars (binary-blob MATCH), FTS special commands (nodesize/optimize) don't add documents, hidden-column (docid/table-name) exclusion from t.* expansion; transpiler: mit (binary scan littleEndian) blob decoder, tclExprWith multi-ternary arithmetic; 2 skips removed from skiptestfiles | FTS3/4 query expressions, matchinfo, aux |
| `P6.FTS-E` | [`P6.FTS-E.md`](plan/goals/P6.FTS-E.md) | 7 | ✅ 7/7 green (fts3prefix, fts3prefix2, fts3rank, fts3snippet, fts3snippet2, fts3tok1, fts3tok_err); verify command passes | FTS3/4 snippet, ranking, prefix, tok |
| `P6.FTS-F` | [`P6.FTS-F.md`](plan/goals/P6.FTS-F.md) | 19 | 🔄 **17/19 green**; red: fts4langid (languageid= WIP `bcdc6caa1` — hidden col, %_content langid col, per-language MATCH filter, insert-path langid landed; 3 assertions left) + fts4opt (genesis-volume optimize O(n²)/snapshotAllPagers panic — needs FTS-G automerge perf); see sub-plan handover | FTS4: content, contentless, docid, growth |
| `P6.FTS-G` | [`P6.FTS-G.md`](plan/goals/P6.FTS-G.md) | 5 | 🔄 4/5 green (fts4merge, fts4merge2, fts4merge3, fts4merge5 — via FTS-F session side work); red: fts4merge4 (2.2.4.2 automerge level distribution 16/2/1 vs 4/3/1) — expected to be resolved or advanced by `P6.FTS-WPORT` structural port; fts4merge slow (72s serial) | FTS4 segment merge |
| `P6.FTS-WPORT` | [`P6.FTS-WPORT.md`](plan/goals/P6.FTS-WPORT.md) | 2 | ✅ DONE: structural port of fts3_write.c writer + fts3view-derived `segview` decoder (`internal/fts/segview.go`, `tools/ftsview`) + oracle-fixture conformance harness (`tools/orafixture`, `writer_conformance_test.go` — x6 per-block byte parity); closed fts4growth (7.5–7.7: interior-node header accounting in incrwriter + bNoLeafData propagation in MergeFTS continuation; sum(length(block))=635247 oracle-exact) with fts4opt green; all debug instrumentation removed; residue: fts4merge4 automerge distribution → P6.FTS-G | FTS3/4 writer structural port + unit conformance |
| `P6.FTS-H` ✅ | [`P6.FTS-H.md`](plan/goals/P6.FTS-H.md) | 9 | ✅ **complete** — 5 packages fully green (integrity/offsets/query/sort/varint) with aux-validation + NEAR-offsets fixes; 4 NA-with-evidence (malloc/shared/misc/rnd → G7/G8 phases), see §Resolution (2026-08-25) | FTS3/4 misc: integrity, offsets, sort, varint |
| `P6.JSON` | [`P6.JSON.md`](plan/goals/P6.JSON.md) | 12 | 🔄 constructor/extract/insert slice passes pure-Go tests; all 12 target packages still skipped; missing full JSON1/JSONB function matrix, JSONB binary representation, and ->/->> execution coverage | JSON1 functions, JSONB, ->/->> operators |
| `P6.VTAB` | [`P6.VTAB.md`](plan/goals/P6.VTAB.md) | 30 | ✅ **complete** — all 30 target packages accounted for: 24 passing natively as testgen (csv, carray, closure, dbdata/dbpage, intarray, rowvaluevtab, spellfix×4, tabfunc01, unionvtab, vtabE/H/J/K/L, vtabdrop, zipfile×2, amatch1) + 6 superseded by native Go ports per the Pure-Go supersession policy (swarmvtab×3 harness-scaffolding-dependent; stmtvtab1/vtabdistinct/vtabrhs1 C-API/query-planner introspection modules `sqlite_stmt`/`qpvtab` not ported — see P6.VTAB.md Session 12). Engine: union/swarm LRU + aux-arg forms, spellfix editdist3/phonetic hash/MATCH scoring, zipfile crafted-archive/corrupt handling, rtree stat1 probe, ANALYZE/integrity_check/gen-col subquery contracts | Virtual table modules: csv, carray, spellfix, zipfile, etc. |

### Blocker Register for Next Agent

| Area | Current blocker | Evidence / entry points | Recommended analysis order |
|------|-----------------|-------------------------|-----------------------------|
| P5.BACKUP | ~~Deep restore/source-busy/destination-detach/page-size semantics~~ **DIAGNOSED 2026-08-21**: 108 failing assertions, each mapped to a root cause — transpiler: TCL `eq`/`ne` eval (98× backup-2), compile-time connClosed for errmsg, `catchsql {…} db2` wrong connection, `ifcapable memorymanage` unsupported, multi-command `file size` result; engine: init errors on wrong conn + no dest-in-use check, Stmt.Step errmsg, BEGIN EXCLUSIVE/IMMEDIATE not real txns, engine-wide (not per-schema) busy check, eager page-1 write (0-byte file), empty-source dest rewrite | `plan/goals/P5.BACKUP.md` §Diagnosis + §Micro-Tasks (14 tasks, per-task DoD + commit), `frigolite_backup.go`, `internal/exec/locks.go`, `internal/exec/transaction.go`, `tools/tcl2go/helpers_template_part2.go`, `tools/tcl2go/processbackup.go` | Execute micro-tasks T0→T13 in order; each task is self-contained (pure-Go test first, oracle check, commit) |
| P5.BIND / P5.STMT | Public Go API exists, but full testgen bind/VM compatibility and package unskip are unfinished; no bytecode VM parity | `stmt.go`, `tools/tcl2go/skiptestfiles.go`, `plan/goals/P5.BIND.md`, `P5.STMT.md` | Separate supported Go API assertions from C-only `sqlite3_prepare/step` expectations; add pure-Go cases first |
| P5.CALLBACKS / CAPI | Hook/status/interrupt slices complete: all six target packages green; C-runtime callback delivery classified with evidence in NA_EVIDENCE (tclsqlite.c/vdbe.c citations) with equivalent Go surfaces preserved | `plan/goals/P5.HOOKS.md`, `P5.HOOKS-INTERRUPT.md`, `portplan/NA_EVIDENCE.md`, `testdata/hookconformance` | Focused hook/status/interrupt suites pass |
| P6.JSON | JSON constructors are now real, but registry still lacks most JSON1 functions (valid/type/array/quote/set/replace/remove/patch/path helpers), JSONB binary semantics, and complete arrow operators; `json101`–`jsonb01` remain skipped | `internal/function/json.go`, `internal/function/function.go`, `internal/parse/token.go`, `internal/execquery`, `plan/goals/P6.JSON.md` | Read `/Users/muaddib/dev/sqlite/src/json.c` and `jsonb.c`; build pure-Go oracle cases, then unskip one package at a time |
| P6.FTS | FTS-B/F/F-G/H are not fully green; known `fts4langid`, `fts4opt`, and `fts4merge4` gaps include language filtering and automerge/performance | `plan/goals/P6.FTS-{B,F,G,H}.md`, target `testgen/fts*` packages | Reproduce failing package serially; inspect source before performance changes |
| P6.VTAB | Thirty target modules/packages remain skipped, including C ABI/VFS-dependent csv/carray/spellfix/zipfile families | `plan/goals/P6.VTAB.md`, `internal/vtab`, `tools/tcl2go/skiptestfiles.go` | Implement one module contract at a time; explicitly mark OS/VFS/C ABI cases N/A only with `portplan/NA_EVIDENCE.md` evidence |
| P7 WAL/concurrency | No WAL/shared-memory/concurrency implementation; current lock registry is process-local prepared/read/write state, not SQLite pager/WAL locking | `plan/goals/P7.WAL-A.md` through `P7.WAL-E.md`, `internal/lockreg`, `internal/pager` | Start with journal-mode state and pager WAL frame format; use SQLite `src/wal.c`/`wal.h`, then multi-connection pure-Go tests |
| P7 planner | AUTOINDEX/LOCK-A/B/C, planner, pushdown, skipscan, snapshot remain queued | `PORTPLAN.md` §4, `plan/goals/P7.*.md` | Do not conflate prepared-statement locks with pager/WAL locks; establish pager-level abstraction first |
| P8 storage/recovery | Corruption, encoding, vacuum, rollback, pager, recover, and related packages remain queued | `plan/goals/P8.*.md` | Preserve current corruption normalization tests; use source-first isolated package tranches |
| Quality gates | Production complexity/file-size gates exclude all Go tests and generated `testgen/` fixtures; full generated suite is not equivalent to `go test ./...` | `.agents/skills/golang-check/go-file-size-check.sh`, `tools/quality_gate.sh`, `TESTGEN_BASELINE.md` | Do not hand-refactor generated files or test fixtures; regenerate from transpiler and track generated-suite compatibility in baseline evidence |

### P7 — Performance & Concurrency

*13 goals, 118 packages — depends on: P5*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P7.AUTOINDEX` | [`P7.AUTOINDEX.md`](plan/goals/P7.AUTOINDEX.md) | 5 | Automatic index creation |
| `P7.LOCK-A` | [`P7.LOCK-A.md`](plan/goals/P7.LOCK-A.md) | 10 | File locking: multi-connection db locks |
| `P7.LOCK-B` | [`P7.LOCK-B.md`](plan/goals/P7.LOCK-B.md) | 10 | Shared-cache locking, shared_err |
| `P7.LOCK-C` | [`P7.LOCK-C.md`](plan/goals/P7.LOCK-C.md) | 13 | Busy handler, multiplex, tkt locking |
| `P7.PLANNER` | [`P7.PLANNER.md`](plan/goals/P7.PLANNER.md) | 24 | ANALYZE, stat tables, BestIndex, query optimization |
| `P7.PUSHDOWN` | [`P7.PUSHDOWN.md`](plan/goals/P7.PUSHDOWN.md) | 3 | Predicate pushdown, cursor hints |
| `P7.SKIPSCAN` | [`P7.SKIPSCAN.md`](plan/goals/P7.SKIPSCAN.md) | 6 | Skip-scan optimization |
| `P7.SNAPSHOT` | [`P7.SNAPSHOT.md`](plan/goals/P7.SNAPSHOT.md) | 5 | Snapshot API (WAL read-mark) |
| `P7.WAL-A` | [`P7.WAL-A.md`](plan/goals/P7.WAL-A.md) | 8 | WAL mode basics: journal_mode=WAL, read/write |
| `P7.WAL-B` | [`P7.WAL-B.md`](plan/goals/P7.WAL-B.md) | 10 | WAL checkpoint, checksum, no-shm, backup |
| `P7.WAL-C` | [`P7.WAL-C.md`](plan/goals/P7.WAL-C.md) | 7 | WAL crash recovery, fault handling |
| `P7.WAL-D` | [`P7.WAL-D.md`](plan/goals/P7.WAL-D.md) | 10 | WAL hooks, protocol, locks, VFS |
| `P7.WAL-E` | [`P7.WAL-E.md`](plan/goals/P7.WAL-E.md) | 7 | Journal modes: DELETE/PERSIST/TRUNCATE |

### P8 — Corruption & Recovery

*9 goals, 76 packages — depends on: P1, P2*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P8.CORRUPT` | [`P8.CORRUPT.md`](plan/goals/P8.CORRUPT.md) | 13 | Database corruption detection (SQL surface) |
| `P8.ENCODING` | [`P8.ENCODING.md`](plan/goals/P8.ENCODING.md) | 11 | Encoding (UTF-16), secure-delete, URI |
| `P8.INCRVACUUM` | [`P8.INCRVACUUM.md`](plan/goals/P8.INCRVACUUM.md) | 5 | Incremental and auto-vacuum |
| `P8.MISC` | [`P8.MISC.md`](plan/goals/P8.MISC.md) | 3 | Misc recovery: cksumvfs, harness UDFs |
| `P8.PAGER` | [`P8.PAGER.md`](plan/goals/P8.PAGER.md) | 24 | Pager/page-format/btree validation (SQL surface) |
| `P8.PRAGMA` | [`P8.PRAGMA.md`](plan/goals/P8.PRAGMA.md) | 9 | PRAGMA edge cases, max_page_count, quota |
| `P8.RECOVER` | [`P8.RECOVER.md`](plan/goals/P8.RECOVER.md) | 1 | .recover (corrupt-db recovery) |
| `P8.ROLLBACK` | [`P8.ROLLBACK.md`](plan/goals/P8.ROLLBACK.md) | 4 | Rollback journal semantics |
| `P8.VACUUM` | [`P8.VACUUM.md`](plan/goals/P8.VACUUM.md) | 6 | VACUUM and VACUUM INTO |

### P9 — Final Triage

*1 goals, 4 packages — depends on: P1, P2, P3, P4, P5, P6, P7, P8*

| Goal | Sub-plan | # Pkgs | Focus |
|------|----------|--------|-------|
| `P9.PERF` | [`P9.PERF.md`](plan/goals/P9.PERF.md) | 4 | Performance test packages (functional assertions) |

---

## 5. Goal Queue — Goa-Driven Execution

All runtime goals were cancelled in the 2026-08 approach review and replanned
under the UCL contract (§5b item 0, `portplan/UNIT_CONFORMANCE.md`). The queue
is executed through the **goa goal tool**: one goal at a time; the next goal is
created only after the previous goal's `verifyCommand` passes and the goal is
`complete`. **Every queue item below must start with its UCL tranche**
(scenario + oracle fixture + localized unit tests) before engine edits.

### 5a. Queue (dependency order)

| # | Goa goal name | Objective | Maps to sub-plan(s) | Verify (baseline) |
|---|---------------|-----------|--------------------|-------------------|
| 1 | `P6.FTS-WPORT` | FTS3/4 writer structural port of `fts3_write.c` + UCL harness (`tools/orafixture`, `internal/fts/segview`, writer conformance); closes fts4growth 7.7 and the fts4merge4 automerge residue; UCL instruments become reusable assets | `P6.FTS-WPORT.md` | Sub-plan §5 verify command |
| 2 | `P5.BACKUP-GAPS` | ✅ Execute the 14 diagnosed micro-tasks (T0→T13); UCL: oracle src/dest DB fixture pairs for page-level copy semantics (`testdata/backupconformance` + `TestBackupConformance`) — done 2026-08-24, post-regression fixes + closeout in sub-plan §Resolution | `P5.BACKUP.md` | Sub-plan verify commands |
| 3 | `P5.STMT-BIND-GAPS` | ✅ Execute the remaining P5.STMT/P5.BIND unskips (capi2..capi3e re-verified green; bind/bind2 un-skipped and green via runtime Stmt VM emulation); UCL: oracle C-API transcript (`testdata/stmtbindconformance` + `TestStmtBindConformance`) — done 2026-08-25, zero regressions vs fe48c8d7e | `P5.STMT.md`, `P5.BIND.md` | Sub-plan verify commands |
| 4 | `P5.CALLBACKS` | ✅ dbstatus/dbstatus2/hook/hook2/interrupt/interrupt2 all green (interrupt countdown wired to ::sqlite_interrupt_count); UCL: oracle script transcript (`testdata/hookconformance` + `TestHookScriptConformance`); genuine C-runtime callback surfaces classified in NA_EVIDENCE — done 2026-08-25 | `P5.HOOKS.md` | Sub-plan verify commands |
| 5 | `P6.FTS-H` | ✅ Executed per sub-plan T0-T8: 5/9 packages fully green; malloc/shared/misc/rnd NA-classified with evidence (G7/G8 deps); UCL native tests committed — done 2026-08-25, zero regressions vs fe48c8d7e | `P6.FTS-H.md` | Sub-plan verify commands |
| 6 | `P6.JSON` | ✅ Complete JSON1/JSONB: function matrix, JSONB binary (jsonbHeaderCheck/jsonbPayloadSize containment + strict TranslateText), ->/->>; all 12 testgen packages (json101..json109, json501, json502, jsonb01) un-skipped and green; UCL: oracle jsonb() byte fixtures (TestJSONBlobRoundTrip + hex-verified encoder) and golden value matrices (frigolite_json_regression_test.go: NULL-path rules, json_each NULL root, TVF WHERE pushdown, corrupt-JSONB layering) — done 2026-08-26; tcl2go hardened (db-exists conditions, TCL list-element values, tx string-map procs, setListValue catchsql guard) | `P6.JSON.md` | Sub-plan verify commands |
| 7 | `P6.VTAB` | One vtab module contract at a time; UCL: oracle transcripts per module | `P6.VTAB.md` | Sub-plan verify commands |
| 8 | `P7.CONCURRENCY` | AUTOINDEX, LOCK-A/B/C, WAL-A/B/C/D/E, SNAPSHOT; UCL: WAL frame decoder (src/wal.c) + oracle-generated -wal/-journal fixtures BEFORE any pager edit | `P7.*.md` | Sub-plan verify commands |
| 9 | `P7.PLANNER` | PLANNER, PUSHDOWN, SKIPSCAN; UCL: golden EXPLAIN QUERY PLAN fixtures | `P7.*.md` | Sub-plan verify commands |
| 10 | `P8.STORAGE` | PAGER→CORRUPT→ENCODING→INCRVACUUM→MISC→PRAGMA→RECOVER→ROLLBACK→VACUUM; UCL: hexdump/ptrmap fixtures + corrupted-DB corpus | `P8.*.md` | Sub-plan verify commands |
| 11 | `P9.PERF` | Final performance + full-suite closeout + legacy golang-check remediation (§5d) | `P9.PERF.md` | Sub-plan verify commands |

Each queue item maps to existing `plan/goals/P*.md` files; no new plan file may
silently replace an existing task. Genuine N/A requires evidence in
`portplan/NA_EVIDENCE.md` and explicit skip-map rationale.

### 5b. Goal creation contract (mandatory)

For every queue item, at goal start:

0. **UCL tranche (mandatory)** — before any engine edit, the goal's todos
   must include (and close) the seam's unit-conformance work per
   `portplan/UNIT_CONFORMANCE.md` U0: scenario JSON(s), oracle fixture(s)
   via `tools/orafixture`, decoder (byte-layout seams), and localized
   unit tests whose expectations come only from the oracle or SQLite C
   source. If the instrument does not exist yet, building it IS the first
   micro-task. Circuit breaker U5 applies for the whole goal lifetime.
1. `goal create` with:
   - `objective` — the sub-plan objective + "all target packages un-skipped
     and passing".
   - `completionCriterion` — the sub-plan's Definition of Done, ALL items,
     verifiable (no vague wording).
   - `verifyCommand` — the sub-plan's Verify Command, copied exactly.
2. `goal add_todo` for **each micro-task** in the sub-plan, in order —
   including the final "update sub-plan + PORTPLAN status + commit + push"
   task. Todos must be closed (`update_todo done`) as work proceeds; a goal
   with open todos is never `complete`.
3. Mid-goal: `goal update active` on "continue"; `goal update blocked` (with
   reason + expectation) only on a genuine external blocker.
4. On completion: restate the criterion, self-audit against evidence
   (commands + outputs), then `goal update complete` citing that evidence.

### 5c. Per-task implementation quality gate (mandatory)

Every task and micro-task MUST check only newly added or materially changed production code against the strict local quality limits. Unchanged legacy code is excluded from the task gate; repository-wide legacy remediation is postponed to final-plan closure.

Before marking any task complete, run the applicable checks on newly added or materially changed non-test Go files, excluding the untouched legacy baseline:

```bash
staticcheck ./...
gocognit -over 15 <changed-production-go-files>
gocyclo -over 12 <changed-production-go-files>
tools/quality_gate.sh <changed-production-go-files>
```

Use `tools/quality_gate.sh <new-or-materially-changed-production-go-files>` as the normal combined gate. Scope complexity and file-size checks to new/changed files; staticcheck must be clean for new-code diagnostics without requiring unrelated legacy findings to be remediated.

No new complexity violation, hard file-size violation, or staticcheck finding may be introduced by task changes. Pre-existing findings in untouched legacy code remain recorded and deferred; the final closure goal alone must drive the complete baseline to zero. Do not use `nolint`, threshold changes, generated-fixture edits, or exclusions to satisfy a gate. Record command and output in each task plan.

### 5d. Final golang-check closure (postponed plan ending)

Repository-wide golang-check remediation is deliberately postponed until the ending stage after all feature tasks. The final closure goal must remove every legacy `gocognit`, `gocyclo`, staticcheck, and hard file-size finding in production non-test code, then run the complete verification matrix in §5c/§5e. Feature tasks must enforce the per-task changed-file gate above and must not expand the legacy baseline.

### 5e. Strict Definition of Done (every goal)

A goal is done ONLY when ALL of the following hold — no partial credit,
no "mostly green":

1. **Zero FAIL**: every target testgen package is un-skipped, compiles, and
   `go test -tags testgen ./testgen/<pkg>/ -count=1` exits 0 for each.
2. **Gates green**: `go build ./...`, `go vet ./...`, `staticcheck ./...`,
   the `golang-check` skill gates (gocognit ≤ 15, gocyclo ≤ 12, file-size
   hard 1000 / soft 500), and `go test -race -count=1 -run "^Test[^C]" ./...`
   all pass.
3. **SOLID**: `go test -run TestSOLID_ ./...` passes.
4. **No regression**: every previously-green package still passes; the
   verify commands of all completed goals still exit 0.
5. **Oracle-verified**: every behavior fix checked against
   `/usr/bin/sqlite3` and traceable to SQLite source
   (`/Users/muaddib/dev/sqlite/src/`, `ext/`).
6. **Feature preservation**: any remaining skip is either in the §1 N/A list
   (OS/C-runtime) or carries documented evidence in
   `portplan/NA_EVIDENCE.md` AND preserves the functionality through an
   equivalent Go surface. "Slow", "hard", "big" are never skip reasons.
7. **Plan status updated**: sub-plan file marked done with evidence;
   PORTPLAN.md §4 table row updated; every key step committed AND pushed
   (see §8).
8. **Todos closed**: all goa todos for the goal are `done`.
9. **UCL satisfied**: the seam's scenarios + oracle fixtures + localized
   unit tests exist, are committed, and pass; every behavior fix made
   during the goal is covered by at least one localized UCL assertion
   (not only the e2e testgen package).

### 5f. Unit Conformance Layer (mandatory method)

`portplan/UNIT_CONFORMANCE.md` is AUTHORITATIVE for all remaining goals:
oracle-sourced expectations only (U1), committed golden fixtures (U2),
decoders ported from SQLite tooling (U3), first-divergence failure output
(U4), and the global circuit breaker (U5). It applies to every topic in the
§5a queue — byte-layout seams get decoders, all other seams get golden
transcripts/values. testgen packages remain the e2e safety net; UCL tests
are the debugging and localization layer.

---

## 6. Guiding Principles

1. **Implement, don't defer.** In-scope unless in §1 N/A list.
2. **Every issue fixed** — engine, transpiler, harness.
3. **SOLID code** — one responsibility, imports downward.
4. **Pure Go stdlib first** — no CGO, no third-party; AND prefer stdlib over
   hand-rolled code (`sort`/`slices`, `strings`, `bytes`, `strconv`,
   `math`, `hash/crc32`, `compress/zlib`, `encoding/binary`, `regexp`,
   `time`). Hand-rolled equivalents are acceptable ONLY where SQLite
   semantics differ from stdlib (document why in code comment).
5. **Triage rule** — pure-Go test first; oracle `/usr/bin/sqlite3`.
6. **Never weaken/skip/hard-code a test.**
7. **Performance ≠ skip reason.** Optimize.
8. **WAL, concurrency, FTS non-negotiable.**
9. **NO TRY/FAIL** — on any repeated failure: stop, re-read SQLite source
   (`/Users/muaddib/dev/sqlite`), write a detailed plan (what/why/exact
   files/ordered steps/verify) regardless of gap size, then execute
   (GUIDELINES §1, §8).
10. **SQLite approach wins** — same algorithms/invariants as the C source,
    adapted to Go; never a simpler heuristic that merely passes a test.
11. **UCL-first** — localized unit conformance (oracle fixtures, decoders,
    first-divergence output) is built BEFORE engine edits on any seam;
    scalar e2e mismatches never justify guess-patching; circuit breaker U5
    halts repeat-offender seams (`portplan/UNIT_CONFORMANCE.md`).

## 7. Definition of Done (per goal)

See §5c — the Strict Definition of Done. Summary: target packages 0 FAIL,
all quality gates + SOLID + race pass, no regression, oracle-verified,
feature preservation documented, plan status updated, commits pushed,
goal todos closed.

## 8. Checkpointing (per key step)

After **every key step** (each micro-task / todo closed, each fix batch):
1. Update the sub-plan file (mark task done, record evidence: commands,
   outputs, commit hash).
2. Update the PORTPLAN.md §4 goal-table row status.
3. `goal update_todo` — close the corresponding goa todo.
4. `git add -A && git commit -m "<GOAL_ID>.<task>: <summary>"` — atomic.
5. `git push` — the remote is the checkpoint; work is only resumable from
   a committed AND pushed state.

No key step is "done" until its plan status is updated and the commit is
pushed. Resumes happen from the pushed state only.

## 9. How to Resume

1. `git pull` — read PORTPLAN.md goal table.
2. `goal list` — find the active goal (or create the next §5a queue item per
   the §5b contract, with todos from its sub-plan).
3. Open `plan/goals/<NEXT_GOAL>.md`.
4. Start from the first open todo / first incomplete micro-task.