# HANDOVER — Frigolite Tier 1 / EXPLAIN QUERY PLAN multi-node / b-tree scan bug work

**Date**: 2026-07-31 (session handover, 22:36)
**Last commit**: `5dac6182` (feat(exec): emit multi-node EXPLAIN QUERY PLAN with join and GROUP BY nodes)
**Active goal (emerald.puma)**: Implement multi-node EXPLAIN QUERY PLAN emission (COMPLETE — see §2)
**Goal plan state**: emerald.puma COMPLETE · all 5 queued goals CANCELLED (goal list empty)
**Master plan**: `plans/TDD_MASTER_V2.md` (the complete 6-tier roadmap — summarized in §7 below)

---

## 1. Current state (verified, committed)

- **8 packages verified PASS**: types, select2, whereA, affinity, whereK, selectE (required) + analyzeC, eqp (EQP baselines)
- **whereJ now PASSES FULLY** (was 4 errors at previous handover — all fixed):
  - line 92 whereJ-1.4 EQP `GROUP BY aid` wants `B-TREE` → FIXED (5dac6182: `USE TEMP B-TREE FOR GROUP BY` node)
  - line 190/196 3.4/3.5 `EXPLAIN QUERY PLAN` with `--` line comments → parse error → FIXED (31a7f0f9: parser appends `"\n;"` so a trailing `--`/`#` comment cannot swallow the SEMI terminator)
  - line 223 whereJ-4.2 EQP multi-node plan → FIXED (5dac6182: per-table nodes `SCAN cx` / `SEARCH px p_cid0` / `SEARCH le le_id`)
- **cast: 4 errors, expr: 4 errors** (known baseline: window functions/quote-substr/pragma_table_info for cast; custom function implies_nonnull_row for expr)
- **SOLID verify passes**: `go test -run TestSOLID_ -count=1 ./...` → exit 0
- **Build clean**: `go build ./...` → OK
- No regressions in the 6 passing packages (verified)

### whereJ parse-error investigation — RESOLVED (2026-07-31)

Symptom: `EXPLAIN QUERY PLAN` (and plain SELECT) containing `--` line comments failed with
`parse error: syntax error near: ` (empty token). The tokenizer was NOT the culprit —
`internal/sql/lexer.go` correctly skips comments; the failure was in the LALR parse step.
Root cause: a trailing `--`/`#` line comment at the end of a statement swallowed the final
`;` terminator (the comment consumed up to the newline, so the token stream lost the SEMI
before EOF). **Fix** (internal/parse/parser.go): append `"\n;"` to the input so a trailing
line comment cannot eat the SEMI terminator. Committed as `31a7f0f9`.

## 2. THE BIG WIN this session: multi-node EXPLAIN QUERY PLAN emission

**Symptom**: the engine emitted ONE plan row (SEARCH/SCAN) for the whole query, but SQLite
emits one node per joined table (`SCAN cx` / `SEARCH px` / `SEARCH le`) plus temp b-tree
nodes. whereJ-4.2 (`FROM le, cx, px`) and whereJ-1.4 (`GROUP BY aid`) failed.

**What changed** (internal/exec/explain.go, commit `5dac6182`):
1. `planResult(nodes)` renders one row per plan node (`QUERY PLAN`, `|--...`, `` `--... ``).
   The harness `flatten` joins rows with spaces, so multi-node regex patterns match.
2. `explainQueryPlanSelect` collects all FROM tables (From + Joins). Single-table queries
   keep the exact previous node computation; joins go through the new `planJoin`.
3. `planJoin` emits a node per joined table:
   - driving table = the table with constant predicates and the smallest estimated row
     count (live b-tree cell count → sqlite_stat1 ANALYZE count → 1M default);
   - inner tables placed in dependency order, using an index SEARCH when a join column
     is indexed (`SEARCH px USING INDEX p_cid0 (cx_id=?)` after cx is planned);
   - otherwise SCAN.
4. `USE TEMP B-TREE FOR GROUP BY` appended when `GROUP BY` is present (fixes whereJ-1.4's
   `B-TREE` pattern).
5. `collectIndexedRefs` now skips column-to-column predicates (`constVal != nil` check): a
   join term like `px.cx_id = cx.cx_id` is not a constant ref, so cx is no longer wrongly
   reported as `SEARCH c_id (cx_id=?)` on a join predicate.

**Result**: whereJ line 223 emits `SCAN cx` / `SEARCH px p_cid0 (cx_id=?)` /
`SEARCH le le_id (le_id=?)`; line 92 emits `SEARCH tx1 ... ` + `USE TEMP B-TREE FOR GROUP BY`.

## 3. Other fixes committed this session (chronological)

| Commit | Fix |
|---|---|
| 5dac6182 | multi-node EXPLAIN QUERY PLAN (join nodes + GROUP BY B-TREE + collectIndexedRefs const-fix) |
| 31a7f0f9 | parser appends "\n;" so trailing --/# line comments don't swallow the SEMI terminator |
| 2ad059d5 | INSERT VALUES → s.Values path (THE scan bug fix) + normalizeExpectedWord (do_test expected whitespace collapse) |
| 3624dc65 | deep-copy mutable values in structRowToMap (RowMap shared-reference safety) |
| f941bde4 | unwrap ColumnValue in aggregate bare-column output (pointer → value) |
| feb5547f | execsql `$var` substitution + incr-only variable init (TCL braced execsql args re-evaluated by uplevel; vars only `incr`'d start at "0" not "") |
| af82b66d | testgen: regenerate whereK/whereJ with regex-pattern EQP comparisons |
| 4662253d | EXPLAIN QUERY PLAN flag + regex-pattern expectations (rule 1 explain ::= EXPLAIN QUERY PLAN; rule 353 sets QueryPlan; do_test regex wants `/B-TREE/`/`~/SCAN/` → regexp.MatchString) |
| a26a6b17 | CTE merge applies to any statement in multi-statement input (WITH RECURSIVE support) |

Earlier session: go-lemon lempar.go template, EXPLAIN flag, regex expectations, CREATE VIEW
queryable, JOIN ON/USING/NATURAL/RIGHT-of-subquery-view fixes, CAST semantics.

### Historical: b-tree scan bug FIXED (previous session, commit 2ad059d5)

**Symptom**: `SELECT a, count(*) FROM t1 GROUP BY a` on a table >~170 rows returned wrong
groups (rowids jumped 171 → 344; a=2 counted 70 instead of 100). This affected GROUP
BY/aggregate queries on ANY table spanning multiple b-tree pages.

**Root cause (NOT a b-tree cursor bug — a PARSER bug)**: the LALR parser parsed
`INSERT INTO t1 VALUES(0,0,0)` as `InsertStmt{Values: [], Select: <VALUES-as-SELECT>}`;
the engine ran `execInsertSelect` instead of the VALUES `insertRow` path, which mis-assigned
rowids for larger tables → GROUP BY read garbage.

**The fix** (internal/parse/parser.go rule 164):
```go
// A VALUES insert (INSERT INTO t VALUES(...),(...) ) parses as a SELECT with no FROM.
// Convert it into s.Values tuples and clear s.Select so the engine uses the VALUES
// path (insertRow); a real INSERT...SELECT keeps s.Select.
var values [][]sql.Expr
if sel != nil && sel.From.Name == "" {
    values = valuesFromSelect(sel)   // walks the UNION compound chain, each member = one tuple
    sel = nil
}
return &sql.InsertStmt{Table: table, Columns: columns, Values: values, Select: sel, CTEs: ...}
```
`valuesFromSelect` walks `sel.Union` (multi-row VALUES = UNION ALL compound) collecting each
member's `Columns[i].Expr` as a tuple.

## 4. Remaining Tier 1 failures — next targets (each is a SEPARATE feature)

1. **Window functions** (cast line 869 `COUNT(*) OVER ()`): parser produces nil column for
   OVER() — needs OVER clause parsing + window execution. Substantial.
2. **quote()/substr() on blobs** (cast line 842): `quote(X'31003200...')` blob handling.
3. **table-valued pragmas** (cast line 1055 `pragma_table_info('v1')`): parser drops the
   function args; needs table-valued pragma materialization.
4. **WITHOUT ROWID tables** (whereI): only stored in SQL string; storage still uses rowid.
5. **CTE-heavy where packages** (where/whereL/whereD/delete4): CTE now parses; residual errors
   are EXPLAIN-format / other.
6. **custom test functions** (expr `implies_nonnull_row`): not engine features.

## 5. How to verify the EQP multi-node fix is still good

```bash
# whereJ now fully passes (was: 4 errors at previous handover):
go test -tags testgen ./testgen/whereJ/ -count=1
# 6 passing packages + EQP baselines:
go test -tags testgen ./testgen/{types,select2,whereA,affinity,whereK,selectE,analyzeC,eqp}/ -count=1
# SOLID gate:
go test -run TestSOLID_ -count=1 ./...
```

## 6. Repo conventions (for the fresh context)

- Generated testgen files carry `//go:build testgen` — run them with `-tags testgen`.
  `go run ./tools/tcl2go/` regenerates all 614 packages (fast now, ~1s).
- The `tcl2go` binary at repo root is a tracked build artifact; don't commit its changes
  (restore with `git checkout -- tcl2go` if modified).
- Full suite measurement: `go test -tags testgen ./testgen/...` takes ~5-6 min (crash/shell
  packages are slow). Sample specific packages for quick iteration.
- Debug instrumentation during investigation: add `fmt.Printf("ZZ...")` lines, remove before commit.

---

## 7. THE COMPLETE PLAN — all 6 tiers (from plans/TDD_MASTER_V2.md)

> **Status**: PHASE 0 COMPLETE — Tier 1 engine work in progress.
> Total: ~607 testgen packages. **Goal ordering**:
> `Phase 0 (done) → Tier 1 → Tier 2 → Tier 3 → Tier 4 → Tier 5 → Tier 6c`
> (6a excluded as not-applicable, 6b deferred). One goal per tier, `freshContext: true`.

### Tier 1 — Core DML/DDL (70 packages) ⬅ CURRENT
> Functional impact: HIGHEST. Basic CREATE/INSERT/SELECT/UPDATE/DELETE.
> Status: 14 PASS · 32 RUNTIME_FAIL · 23 BUILD_FAIL · 1 UNKNOWN (master-plan baseline; now higher PASS)

- **1a Basic CRUD**: `select1`(PASS), `insert`(PASS), `delete_`(PASS), `update`(PASS), `null`(PASS)
- **1b Types & expr**: `affinity`, `expr`, `types`, `cast`(PASS), `between`(PASS), `coalesce`,
  `literal`, `istrue`, `numcast`, `subtype`, `strict`, `intpkey`(PASS), `intreal`(PASS), `nulls`
- **1c SELECT variants**: `select2`–`select9`, `selectA`–`selectH` (select7/8 PASS)
- **1d WHERE**: `where`–`whereN` (whereN PASS; whereJ now PASS too)
- **1e DELETE/UPDATE/values**: `delete2`(PASS), `delete3`(PASS), `delete4`, `delete_pkg`,
  `returning`, `values`, `valuesfault`, `cse`

**Likely engine bugs**: flatten() format mismatch, type affinity/coercion, expression edge
cases, NULL handling.

### Tier 2 — Query Features (35 packages)
> Functional impact: HIGH. Real-world queries.
> Status: 1 PASS · 24 RUNTIME_FAIL · 9 BUILD_FAIL · 1 UNKNOWN

**Packages**: `join`–`joinI`, `subquery`, `subselect`, `count`, `having`, `distinct`,
`distinctagg`, `aggerror`(PASS), `aggfault`, `orderby`, `orderbyA`–`B`, `limit`, `minmax`,
`sort`, `sorterref`, `starschema`, `unionall`, `exists`, `existsexpr`, `view`, `countofview`

**Likely engine bugs**: JOIN algorithms (nested loop, index-based), aggregate computation
(GROUP BY/HAVING), DISTINCT, ORDER BY with collation, UNION/INTERSECT/EXCEPT.

### Tier 3 — Schema & Constraints (47 packages)
> Functional impact: MEDIUM-HIGH.
> Status: 2 PASS · 29 RUNTIME_FAIL · 16 BUILD_FAIL

**Packages**: `alter`–`altertrig`, `conflict`, `collate`–`collateB`, `fkey`–`fkey_`,
`index`–`indexfault`, `indexedby`, `indexexpr`, `notnull`(PASS), `savepoint`–`savepointfault`,
`schema`, `tableopts`, `temptrigger`, `trans`, `transitive`, `trigger`–`triggerupfrom`,
`upsert`, `without_rowid`, `check`, `alterqf`(PASS)

**Likely engine bugs**: ALTER TABLE (RENAME, ADD/DROP COLUMN), constraint enforcement
(UNIQUE/CHECK/FK/NOT NULL), trigger firing, index creation/usage.

### Tier 4 — Functions & Expressions (32 packages)
> Functional impact: MEDIUM.
> Status: 5 PASS · 20 RUNTIME_FAIL · 7 BUILD_FAIL

**Packages**: `func2`–`func9`, `date`, `timediff`, `printf`, `instr`, `substr`, `like`,
`regexp`(PASS), `hexlit`(PASS), `blob`, `zeroblob`, `quote`, `round`, `decimal`, `percentile`,
`spellfix`, `closure`(PASS), `hidden`, `ieee`(PASS), `nan`, `atof`, `fpconv`, `unhex`,
`zeroblobfault`, `instrfault`, `timediff`

**Likely engine bugs**: date/time formatting, printf format specifiers, string function
edge cases, numeric precision (floating point).

### Tier 5 — Advanced SQL (90 packages)
> Functional impact: LOWER. Extended features.
> Status: 16 PASS · 43 RUNTIME_FAIL · 31 BUILD_FAIL

**Packages**: `fts`–`fts4merge`, `vtab`–`vtabrhs`, `window`–`windowpushd`, `with`–`withM`,
`attach`, `analyze`–`analyzer`, `vacuum`–`vacuummem`, `pragma`–`pragmafault`, `json`–`jsonb`,
`rtree`, `carray`, `intarray`, `tabfunc`, `session`, `upfrom`, `csv`, `recover_pkg`,
`stat`–`statfault`, `autoindex`, `bestindex`–`bestindexG`, `eqp`, `pushdown`, `scanstatus`,
`cost`, `filter`–`filterfault`, `fts_9fd`

**Likely engine bugs**: FTS query parsing/execution, vtab module (xBestIndex/xFilter/xColumn),
window framing, CTE/WITH materialization, ATTACH database.

### Tier 6 — Triage (333 packages)
> Functional impact: LOWEST.
> Status: 51 PASS · 176 RUNTIME_FAIL · 106 BUILD_FAIL

- **6a NOT APPLICABLE (~120 pkgs)** — document & exclude via `plans/NOT_APPLICABLE.md`:
  C API (`capi`, `capi3`, `bind`...), fault injection (`*fault`, `*malloc`), corruption
  (`corrupt`–`corruptN`), custom VFS (`avfs`, `tvfs`, `testvfs`, `vfs`, `cksumvfs`),
  shell (`shell`–`shellB`), build/config (`bitvec`, `atomic`, `ctime`, `keyword`, `memsubsys`)
- **6b DEFERRED (~80 pkgs)** — future support: WAL (`wal`–`walvfs`), concurrency
  (`thread`, `walthread`, `mutex`), shared cache (`shared`–`sharedlock`)
- **6c APPLICABLE (~133 pkgs)** — standard SQL tests, attempt after Tier 5:
  `auth`, `backup`, `descidx`, `exec`, `misc`, `pragma`, `readonly`, etc.

## 8. Constitution & workflow (abbreviated from master plan)

- **P1**: errors never ignored · **P2**: functional surface of a test is immutable
  · **P3**: smallest fix that resolves the root cause · **P4**: verify against the real check
  · **P5**: commit after each GREEN
- **TDD inner loop**: pick a failing package → classify failure (BUILD_FAIL vs RUNTIME_FAIL
  vs format mismatch vs engine gap) → smallest root-cause fix → run that package → run the
  6 passing packages for regression → commit.
- **What counts as done**: a tier is done when `go test -tags testgen ./testgen/<tier>/...`
  all pass + SOLID check passes.
- Full master plan detail: `plans/TDD_MASTER_V2.md` (§1-11: why previous plan failed,
  constitution, baseline, architecture, tier breakdowns, goal templates, workflow,
  engine bug patterns, risks).
