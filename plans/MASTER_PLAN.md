# Frigolite — Master Plan: SQLite Test Compliance

> **Status**: RESEARCH COMPLETE — PLAN WRITTEN, NOT EXECUTED.
> **Created**: 2026-08-02.
> **Supersedes**: `plans/TDD_MASTER_V2.md` (keeps its TDD constitution §2; restructures execution).
> **Reference spec**: SQLite C source at `/Users/muaddib/dev/sqlite/src/`, TCL tests at `/Users/muaddib/dev/sqlite/test/`.
> **Goal**: All applicable SQLite tests green. Zero shortcuts. Functional surface immutable.

---

## 0. CHECKPOINT — G1.CREATE.UNBLOCK (2026-08-03)

### Objective

Fix transpiler execsql handling + implement `if()`/`quick_check` to unblock the
strict and without_rowid testgen packages; verify:

```
go test -tags testgen ./testgen/select1/ ./testgen/types/ ./testgen/strict/ ./testgen/without_rowid/ ./testgen/tableopts/ -count=1 && go test -run TestP1Create -count=1 .
```

### Status at checkpoint (mid-goal, NOT complete)

| Item | State |
|------|-------|
| `go build ./...` | ✅ passes |
| select1 / types | ✅ PASS (fresh test.db) |
| tableopts | ❌ FAIL — `tableopts_test.go:104: exec error: no such table: t1` (see below) |
| strict / without_rowid | ⏳ not yet re-run after transpiler fixes |
| `if()`/`iif()` | ✅ done (G1.CREATE.UNBLOCK.1, committed) |
| PRAGMA quick_check/integrity_check | ✅ done (uncommitted WIP) |
| ALTER TABLE ADD COLUMN STRICT | ✅ done (uncommitted WIP) |
| without_rowid engine (rowid col, UNIQUE) | 🔄 WIP (uncommitted) |

### Committed (pushed to origin/main)

- `48fba940 G1.CREATE.UNBLOCK.1: implement if()/iif() + fix keyword case in LALR parser`
- earlier: `G1.CREATE.7..10` (STRICT validation, WITHOUT ROWID PK ordering, NUMERIC affinity, select1/types green)

### Uncommitted WIP (working tree)

1. **tools/tcl2go/gen.go** — transpiler fixes:
   - `execsql` in `do_test` string bodies → `_r = tclExecSQL(db, ...)` comparison
   - `[string range $var N M]` → `tclStringRange(...)`
   - line-continuation (`\\\n`) handling in `tclUnescapeQuoted`
   - varset foreach (`foreach v [list {set a 1} {set a 2}]`) → Go struct slices
   - generation-time `tclSplitList` added (was only in generated-test template strings; build was broken without it)
   - **NEW THIS SESSION**: `db2 eval` handler now assigns `_res = db2.Exec(...)` instead of discarding the result and checking a stale `_res` (was the root cause of a false "no such column: oid" failure in tableopts 2.3)
2. **internal/btree, exec/{alter,ddl,engine,insert,pragma,select,update}, schema** — without_rowid engine WIP (rowid column, UNIQUE) + quick_check + ALTER STRICT.
3. **testgen/** — regenerated 139 files from the transpiler changes (mostly `\\\"` → `\"` unescaping + execsql/string-range rewrites). `testgen/tableopts/tableopts_test.go` now emits `_res = db2.Exec(...)`.

### Tableopts failure analysis (root cause found, fix pending)

Original TCL (`ori/sqlite/test/tableopts.test`):
- `tableopt-2.1`: `CREATE TABLE t1(a,b,c, PRIMARY KEY(a,b)) WITHOUT rowid; INSERT ...; SELECT c ...` → `{3 4}` ✅
- `tableopt-2.1.1..2.1.3`: `catchsql {SELECT rowid/_rowid_/oid, * FROM t1}` → expect `{1 {no such column: ...}}` (SQLite errors on WITHOUT ROWID)
- `tableopt-2.2`: `VACUUM; SELECT c ...` → `{3 4}` ✅
- `tableopt-2.3`: `sqlite3 db2 test.db; db2 eval {SELECT c FROM t1 ...}` → `{3 4}` ❌

Root cause: tester.tcl's `reset_db` opens the main `db` on the **file** `./test.db`
(`forcedelete test.db; sqlite3 db ./test.db`), but the transpiler converts the main
open to **in-memory** (`frigolite.Open("")`). t1 lives in-memory only; db2 opens the
real `test.db` file and cannot see it → `no such table: t1`.

The stale `test.db` in `testgen/tableopts/` (untracked, from a prior run) masked
this: with an old file present the test happened to pass. Removing the stale file
reproduces the failure reliably. Committed HEAD passes only because the old engine
did not raise `no such column: oid` for WITHOUT ROWID (stale `_res` stayed nil).

Options to fix (choose one):
- **A.** Make main `db` open the real `test.db` file when a `sqlite3 dbN test.db`
  shares it (transpiler: track `reset_db`/file open; share state across connections).
- **B.** Transpiler: emit `db2 = db` (alias the secondary connection to the main
  in-memory db) when the file name equals the main test file.
- **C.** Keep engine in-memory, but emit the `db2 eval` against `db` when the
  connection was opened on the same logical test file.

Prefer B (smallest change; db2/db3 are secondary connections in this harness).

### Next steps (after checkpoint)

1. Fix tableopts 2.3 connection sharing (option B above) → regenerate → tableopts green.
2. Re-run strict + without_rowid; fix remaining engine gaps (rowid column, UNIQUE).
3. Run full verify command; then `go test -run TestP1Create -count=1 .`
4. Commit + push each green slice (`G1.CREATE.UNBLOCK.N: ...`).

### Housekeeping note

The `tcl2go` binary at repo root was a tracked build artifact; it is now
`git rm --cached`'d and added to `.gitignore` (per user instruction: binaries are
not checked in). Rebuild with `go build -o tcl2go ./tools/tcl2go/` if needed.

---

## 1. RESEARCH FINDINGS — Current State

### 1.1 Grammar (LALR parser, `internal/parse/`)

Frigolite ships the **full SQLite LALR(1) grammar** — the go-lemon-generated parse
tables contain **412 rules** (from `parse.y`'s 417 production rules, minus the
conditional `%ifdef` variants that are compiled out).

| Metric | Value | Source |
|--------|-------|--------|
| Total grammar rules | 412 | `GetParseTables().YYNRule` |
| Rules that FIRE during 35,653-statement corpus parse | 282 | `TestRuleInventory` |
| Rules that never fire (unreachable from corpus) | 130 | `TestRuleInventory` |
| Rules that fire but have NO handler (passthrough→nil) | 10 | `TestRuleInventory` |
| `handleRule` case statements implemented | 261 | `grep -c "case [0-9]" parser.go` |
| Cases returning `nil` (includes legit empty-rule returns) | 71 | `grep -c "return nil"` |

**Key insight**: 282 of 412 rules fire during real SQL parsing. The 130 "unfired"
rules are mostly alternative productions of the same nonterminal that happen not
to appear in the corpus — many ARE reachable but rare (e.g., window-function
frame bounds, CTE MATERIALIZED, virtual-table args). The grammar coverage test
(`TestGrammarCoverage`) correctly targets the 282 fired rules.

**The 10 passthrough rules** (fired, no handler, multi-symbol → AST corruption):
see `GRAMMAR_COMPLETENESS.md` §3 for the full list and the tests they break.

See: [`GRAMMAR_COMPLETENESS.md`](GRAMMAR_COMPLETENESS.md) for the full rule-by-rule map.

### 1.2 Test State (testgen, tcl2go transpiled tests)

The tcl2go transpiler converts SQLite TCL tests → Go `_test.go` files. All 614
packages compile (`go build -tags testgen ./testgen/...` exits 0 — Phase 0 done).

**Baseline** (from `HANDOVER_TIER1.md` + `TDD_MASTER_V2.md §0`, measured 2026-07-31):

| Metric | Value |
|--------|-------|
| testgen packages | 614 |
| PASS | ~339 |
| RUNTIME_FAIL (engine bugs) | ~275 |
| BUILD_FAIL | 0 (Phase 0 complete) |
| OOM-prone (skip in bulk runs) | fts4merge, fts4merge2 |

⚠️ **Memory leak**: full-suite runs can OOM. Always run package-by-package or in
small batches with `-timeout`. The FTS merge suites are the worst offenders.

### 1.3 Two-Parser Situation

Frigolite has TWO SQL parsers:
1. **`internal/parse/`** — go-lemon LALR(1) parser (full grammar tables). The
   engine uses this for full statement parsing (`internal/exec/engine.go:404`).
   `handleRule()` converts reductions → AST nodes.
2. **`internal/sql/parser.go`** — hand-written recursive-descent parser. Still
   exists, still modified recently. Provides AST types (`ast.go`).

This dual-parser state is a risk: fixes may be needed in both. The LALR parser is
the primary one (used by the engine). The RD parser is a fallback/legacy path.

### 1.4 Existing Work (T1.1–T1.6 done, T1.7–T1.29 pending)

Tier 1 has 58 packages. As of last handover: **33 PASS / 25 FAIL**.
Completed tasks: T1.1 (delete3 crash), T1.2 (selectG perf), T1.3 (cse bool),
T1.4 (whereG braces), T1.5 ($var-in-SQL), T1.6 (multi-row VALUES).
Pending: T1.7 (RETURNING), T1.8 (grammar/custom-fns), T1.9–T1.29 (various).

See: [`TCL_RESEARCH_TIER1.md`](TCL_RESEARCH_TIER1.md) for the detailed Tier 1 fix plan.

---

## 2. CONSTITUTION — Non-Negotiable Principles

(Carried forward from `TDD_MASTER_V2.md` §2. These govern ALL work.)

| Principle | Rule |
|-----------|------|
| **P1 — Errors visible** | Never `t.Skipf` a failure. Use `t.Errorf`. Every failure is a signal. |
| **P2 — Functional surface immutable** | Never change SQL or expected results to make a test pass. Only fix harness/transpiler/engine. |
| **P3 — Smallest fix** | No opportunistic cleanup. Fix the one root cause. |
| **P4 — Verify the real check** | Run the actual failing test after each fix. No "it should work." |
| **P5 — Commit after each GREEN** | One logical fix per commit. Format: `<GoalID>: <description>`. |
| **P6 — Pre-test before TCL tests** | Write a hand-written Go test isolating the feature BEFORE running the full SQLite test suite. This distinguishes engine bugs from transpiler bugs. |

### P6 — The Pre-Test Protocol (NEW, per user requirement #3)

Before attempting to make a SQLite TCL-transpiled test pass for a feature:

1. **Write a focused hand-written Go test** (`frigolite_<feature>_test.go`) that
   exercises the specific SQL functionality with known inputs/outputs.
2. **Run it against the frigolite engine** — if it fails, the bug is in the ENGINE.
3. **Compare against `/usr/bin/sqlite3`** (the oracle) for expected output.
4. **Fix the engine** until the hand-written test passes.
5. **THEN run the SQLite TCL-transpiled test** (`go test -tags testgen ./testgen/<pkg>/`).
   If it still fails after the engine test passes, the bug is in the TRANSPILER
   (`tools/tcl2go/gen.go`) or the test harness (`testgen/*/helpers_test.go`).

This two-stage approach cleanly separates engine bugs from transpiler bugs,
preventing wasted effort fixing the wrong layer.

---

## 3. GRAMMAR COMPLETENESS — Summary

The full grammar rule map is in [`GRAMMAR_COMPLETENESS.md`](GRAMMAR_COMPLETENESS.md).
Summary by grammar area:

| Grammar Area | Rules | Fired | Unfired | Passthrough | Status |
|---|---|---|---|---|---|
| Top-level (input/cmdlist/ecmd/cmdx) | 7 | 5 | 2 | 0 | ✅ mostly done |
| Transactions (BEGIN/COMMIT/ROLLBACK/SAVEPOINT) | 12 | 8 | 4 | 0 | ✅ mostly done |
| CREATE TABLE + column constraints | ~50 | ~35 | ~15 | 0 | ⚠️ gaps in FK, generated cols |
| CREATE INDEX | ~15 | ~10 | ~5 | 0 | ⚠️ expr-index gaps |
| CREATE VIEW / DROP | 6 | 4 | 2 | 1 (rule 14) | ⚠️ minor |
| SELECT (core) | ~30 | ~25 | ~5 | 0 | ✅ mostly done |
| SELECT (compound UNION/INTERSECT/EXCEPT) | ~8 | ~5 | ~3 | 0 | ⚠️ gaps |
| FROM / JOIN / seltablist | ~20 | ~15 | ~5 | 1 (rule 133) | ⚠️ NOT INDEXED, table-fn |
| Expressions (binary/unary/CASE/CAST) | ~60 | ~45 | ~15 | 1 (rule 231) | ⚠️ window-fn expr |
| INSERT / UPDATE / DELETE / RETURNING | ~25 | ~20 | ~5 | 0 | ✅ mostly done |
| UPSERT | 6 | 4 | 2 | 0 | ⚠️ minor |
| PRAGMA | 8 | 5 | 3 | 0 | ⚠️ table-valued pragma |
| TRIGGER (decl/cmd_list/cmd) | ~20 | ~12 | ~8 | 1 (rule 277) | ⚠️ trigger-cmd gaps |
| ALTER TABLE | ~15 | ~8 | ~7 | 0 | ⚠️ DROP/RENAME col |
| CREATE VIRTUAL TABLE | ~12 | ~8 | ~4 | 3 (rules 2,302,348) | ⚠️ passthrough |
| CTE (WITH) | ~10 | ~6 | ~4 | 0 | ⚠️ MATERIALIZED |
| Window functions (OVER/FILTER/frame) | ~25 | ~10 | ~15 | 0 | 🔴 major gaps |
| RAISE / trigger expressions | 5 | 3 | 2 | 0 | ✅ mostly done |
| ATTACH / DETACH | 6 | 3 | 3 | 0 | ⚠️ runtime not grammar |
| VACUUM / ANALYZE / REINDEX | 6 | 4 | 2 | 0 | ✅ done |

**Priority grammar gaps** (cause test failures): see GRAMMAR_COMPLETENESS.md §4.

---

## 4. TEST TAXONOMY — All 614 Packages Categorized

Full categorization in [`TEST_TAXONOMY.md`](TEST_TAXONOMY.md). Summary:

### Priority Levels

| Priority | Category | Packages | Description |
|----------|----------|----------|-------------|
| **P0** | Grammar completion | — | Fix the 10 passthrough rules + key unfired rules that block tests |
| **P1** | CRUD Core | ~58 | CREATE/INSERT/SELECT/UPDATE/DELETE + WHERE + types + expr |
| **P2** | Query Features | ~30 | JOIN, subquery, aggregate, ORDER BY, DISTINCT, UNION, VIEW |
| **P3** | Schema & Constraints | ~47 | ALTER, index, trigger, FK, UNIQUE, CHECK, collation, upsert |
| **P4** | Functions & Expressions | ~32 | string/date/printf/numeric functions, LIKE, CAST |
| **P5** | Advanced SQL | ~90 | FTS, vtab, window, CTE, ATTACH, pragma, JSON, rtree |
| **P6** | Triage | ~357 | 6a (N/A), 6b (deferred WAL), 6c (applicable misc) |

### P1 CRUD Subcategories (per user requirement #3)

Each subcategory gets **hand-written pre-tests** before running TCL tests:

| Subcategory | Pre-test file | SQLite ref | TCL packages |
|-------------|---------------|------------|--------------|
| P1a — CREATE TABLE | `frigolite_p1_create_test.go` | `src/build.c` | select1, types, strict, tableopts, without_rowid |
| P1b — INSERT | `frigolite_p1_insert_test.go` | `src/insert.c` | insert, insertfault, values, valuesfault, default_pkg |
| P1c — SELECT (basic) | `frigolite_p1_select_test.go` | `src/select.c` | select1–selectH, select2–select9 |
| P1d — WHERE / filter | `frigolite_p1_where_test.go` | `src/where.c` | where–whereN |
| P1e — UPDATE | `frigolite_p1_update_test.go` | `src/update.c` | update, returning |
| P1f — DELETE | `frigolite_p1_delete_test.go` | `src/delete.c` | delete_, delete2, delete3, delete4, delete_pkg |
| P1g — Types & affinity | `frigolite_p1_types_test.go` | `src/vdbemem.c` | affinity, cast, numcast, types, intpkey, intreal, nulls |
| P1h — Expressions | `frigolite_p1_expr_test.go` | `src/expr.c` | expr, between, coalesce, literal, istrue, cse, subtype |

---

## 5. GOAL SCHEDULE — Execution Order

Each goal runs with **`freshContext: true`** (clean context, handover note only)
to limit cost. Goals are queued; each completes before the next starts.

Goals reference their **sub-plan** for full detail. The sub-plan is the contract.

```
G0: GRAMMAR COMPLETION (passthrough rules → handlers)
  └─ subplan: subplans/G0_GRAMMAR.md
     ├── G0.1: Fix 10 passthrough rules (commit each fix)
     ├── G0.2: Implement unfired-but-reachable rules for P1 features
     └── G0.3: Grammar coverage test → 100% of fired rules have handlers

G1: P1 CRUD CORE (one goal per subcategory)
  ├── G1.CREATE  → subplans/P1_CREATE.md
  ├── G1.INSERT  → subplans/P1_INSERT.md
  ├── G1.SELECT  → subplans/P1_SELECT.md
  ├── G1.WHERE   → subplans/P1_WHERE.md
  ├── G1.UPDATE  → subplans/P1_UPDATE.md
  ├── G1.DELETE  → subplans/P1_DELETE.md
  ├── G1.TYPES   → subplans/P1_TYPES.md
  └── G1.EXPR    → subplans/P1_EXPR.md

G2: P2 QUERY FEATURES
  ├── G2.JOIN    → subplans/P2_JOIN.md
  ├── G2.SUBQUERY→ subplans/P2_SUBQUERY.md
  ├── G2.AGG     → subplans/P2_AGG.md
  ├── G2.ORDER   → subplans/P2_ORDER.md
  ├── G2.SETOP   → subplans/P2_SETOP.md   (UNION/INTERSECT/EXCEPT)
  └── G2.VIEW    → subplans/P2_VIEW.md

G3: P3 SCHEMA & CONSTRAINTS
  ├── G3.ALTER   → subplans/P3_ALTER.md
  ├── G3.INDEX   → subplans/P3_INDEX.md
  ├── G3.TRIGGER → subplans/P3_TRIGGER.md
  ├── G3.FK      → subplans/P3_FK.md       (foreign keys)
  ├── G3.CONSTR  → subplans/P3_CONSTR.md   (UNIQUE/CHECK/NOT NULL)
  └── G3.COLLATE → subplans/P3_COLLATE.md

G4: P4 FUNCTIONS & EXPRESSIONS
  ├── G4.STRING  → subplans/P4_STRING.md
  ├── G4.DATE    → subplans/P4_DATE.md
  ├── G4.NUMERIC → subplans/P4_NUMERIC.md
  ├── G4.PRINTF  → subplans/P4_PRINTF.md
  └── G4.LIKE    → subplans/P4_LIKE.md

G5: P5 ADVANCED SQL
  ├── G5.CTE     → subplans/P5_CTE.md      (WITH)
  ├── G5.WINDOW  → subplans/P5_WINDOW.md
  ├── G5.PRAGMA  → subplans/P5_PRAGMA.md
  ├── G5.FTS     → subplans/P5_FTS.md
  ├── G5.VTAB    → subplans/P5_VTAB.md
  ├── G5.ATTACH  → subplans/P5_ATTACH.md
  ├── G5.JSON    → subplans/P5_JSON.md
  └── G5.ANALYZE → subplans/P5_ANALYZE.md

G6: P6 TRIAGE
  ├── G6.NA      → subplans/P6_NOT_APPLICABLE.md  (document exclusions)
  ├── G6.DEFERRED→ subplans/P6_DEFERRED.md        (WAL/concurrency)
  └── G6.MISC    → subplans/P6_APPLICABLE.md      (remaining applicable)
```

### Goal Definition Format

Each sub-plan defines the goal with this structure:

```
Goal ID: <G-level.category>
Objective: <one sentence, verifiable>
Completion criterion: <machine-checkable end state>
Verify command: <shell command, exit 0 = pass>
Fresh context: true
Handover: <state + decisions + next steps + risks>
```

### Commit Convention

```
<G-level.category>.<step>: <description>

Example: G1.SELECT.3: fix LEFT JOIN view column resolution in ON clause
```

---

## 6. HOW TO USE THIS PLAN

### For each goal:

1. **Create the goal** using the definition from the sub-plan file.
2. The goal runs with `freshContext: true` — it reads only the sub-plan, not this
   conversation.
3. The sub-plan's handover note carries the state between goals.
4. Each step in the sub-plan ends with: commit + update the sub-plan checkbox + push.
5. When the goal completes, the verify command runs automatically.

### Step template (in every sub-plan):

```
### Step N: <title>
- **Hypothesis**: <what's broken and why>
- **Pre-test**: <hand-written test to write/extend (P6 protocol)>
- **SQLite ref**: <.c file + function>
- **Fix location**: <file(s)>
- **Verify**: <command>
- **Commit**: `<GoalID>.<N>: <description>`
- [ ] Done
```

---

## 7. MEMORY LEAK — CRITICAL CONSTRAINT

⚠️ **NEVER run `go test -tags testgen ./testgen/...` in one process.** It will OOM.

- Always run **package-by-package**: `go test -tags testgen ./testgen/<pkg>/ -count=1`
- The FTS merge suites (fts4merge, fts4merge2, fts3*) are the worst offenders.
- A memory-leak investigation goal (G0.0 or part of G5) should profile and fix this
  before bulk test runs are attempted.
- The `scripts/verify_tier.sh` script runs per-package with timeouts — use it.

---

## 8. REFERENCE PATHS

| Resource | Path |
|----------|------|
| SQLite grammar (spec) | `/Users/muaddib/dev/sqlite/src/parse.y` (2163 lines, 417 rules) |
| SQLite C source (spec) | `/Users/muaddib/dev/sqlite/src/*.c` |
| SQLite TCL tests | `/Users/muaddib/dev/sqlite/test/*.test` (1190 files) |
| go-lemon LALR parser | `internal/parse/` (parser.go, engine.go, sql_tables.go, token.go) |
| RD parser (legacy) | `internal/sql/parser.go` |
| AST types | `internal/sql/ast.go` (674 lines, all types defined) |
| Execution engine | `internal/exec/` |
| tcl2go transpiler | `tools/tcl2go/` (gen.go, main.go) |
| Generated tests | `testgen/` (614 packages) |
| sqlite3 oracle | `/usr/bin/sqlite3` |
| This plan | `plans/MASTER_PLAN.md` |
| Grammar map | `plans/GRAMMAR_COMPLETENESS.md` |
| Test taxonomy | `plans/TEST_TAXONOMY.md` |
| Sub-plans | `plans/subplans/*.md` |
| Prior Tier 1 work | `plans/TCL_RESEARCH_TIER1.md` |
| SOLID architecture test | `frigolite_solid_test.go` |

---

## 9. WHAT TO DO NEXT (execution, not this document)

1. Review this plan and the sub-plans.
2. Decide whether to resume the paused goal (T1.8 grammar) or start fresh from G0.
3. Create the first goal from the chosen sub-plan.
4. The goal system auto-queues subsequent goals.

**This document is the plan. It is NOT being executed.**
