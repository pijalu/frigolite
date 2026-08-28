# PORTPLAN — Implement All Missing SQLite Features

> **Status**: AUTHORITATIVE PLAN — the single source of truth. The legacy
> `PORTPLAN.md`, `HANDOVER.md`, `plans/`, and the old `portplan/TASK_G*.md` are
> now in `archive/` and are **no longer the source of truth**.
>
> **Mission correction**: The previous plan systematically *deferred* or marked
> *not-applicable* a large set of **real, implementable** features (FTS, virtual
> tables, window functions, JSON, rtree, the C-API, the query planner, and ~140
> engine packages skipped only because they were "slow"). **That was wrong.** The
> directive is: **implement the missing features**. The *only* legitimate
> exclusions are (a) features for which Frigolite already has an equivalent or
> better pure-Go implementation, and (b) tests that exercise the C runtime
> *internals* in ways that have no SQL-functional surface (e.g. malloc-allocator
> internals, OS-specific file-locking syscalls).
>
> **Reference**: SQLite C source `/Users/muaddib/dev/sqlite/src/` + `/Users/muaddib/dev/sqlite/ext/`;
> SQLite TCL tests `/Users/muaddib/dev/sqlite/test/`.
> **Oracle**: `/usr/bin/sqlite3`.
> **Rule**: Zero shortcuts. Functional surface must match SQLite exactly.

---

## 0. Guiding Principles (binding on every goal)

1. **Implement, don't defer.** A feature marked "DEFERRED" or "N/A" in the
   archived files is *presumed in-scope* unless a goal explicitly re-classifies
   it with evidence. The default disposition for a failing applicable package is
   *engine work*, not a skip.
2. **Every issue must be fixed, regardless of source** — engine, transpiler,
   harness, or test generation. No papering over.
3. **SOLID code.** Each `internal/` package has one responsibility; imports flow
   downward only; small focused interfaces; compile-time substitutability. The
   `frigolite_solid_test.go` gates run in every goal's verify command.
4. **Maximum portability via the Go standard library.** No CGO, no third-party
   dependencies — ever. Prefer stdlib building blocks: `time` for date/time,
   `regexp` (RE2) for REGEXP/FTS token matching, `math`/`math/rand` for numeric
   funcs, `encoding/*` for serialization, `strconv`, `unicode`, `sort`, `hash/*`,
   `sync` for concurrency, `os`/`io` for I/O. Only hand-roll what stdlib genuinely
   lacks (e.g. SQLite's B-tree file format, varints, the FTS5 segment index). If a
   feature maps to a stdlib package, use that package — do not reinvent it.
5. **Each goal is a small unit of work with its own dedicated completion test**
   (`verifyCommand`). A goal is done when its verify command exits 0, `make
   quality` passes, `go build ./...` passes, and no earlier goal regressed.
6. **Goa goals with `freshContext: true`, each carrying state only via its
   handover note.** Use **sub-tasks (todos) liberally** within each goal — Goa
   supports unlimited tasks with additional todos; decompose every goal into an
   ordered todo list.
7. **Triage rule (mandatory)**: on any testgen failure, write a **pure-Go test**
   driving the engine via `frigolite.Open/Exec/Query` first. If it fails → engine
   bug. Only if it passes while testgen fails → transpiler bug (`tools/tcl2go/`).
8. **SQLite is ground truth.** Derive expected output with `/usr/bin/sqlite3`;
   record the invocation in pre-tests / commit messages.
9. **Commit cadence**: small atomic commits prefixed `G<N>.<TASK>.<step>`; update
   the task MD + plan checkbox in the same commit. **Keep everything committed
   and pushed** so any interruption resumes only from plan files.
10. **Never weaken, skip, or hard-code a test to make it green.** If a generated
    assertion is genuinely wrong, fix the transpiler — never edit generated files.
11. **Performance is not a rationale to skip.** Slow packages (5–20s) are real
    tests; they were incorrectly skipped under a "verify-time budget". They must
    pass. The verify *budget* is a scheduling concern (run narrower), not a
    reason to skip a test.
12. **Measure progress continuously.** A status-reporting script
    (`tools/status/`, built in G0) produces a clear report of Frigolite's state:
    progress **per feature family** (CRUD, JOIN, SCHEMA, FUNCTIONS, CTE/WINDOW,
    FTS, VTAB, JSON, RTREE, C-API, PLANNER, WAL/CONCURRENCY, …) with each
    testgen package's state (**pass / fail / skipped**) and, for skips, the
    reason bucket. Every goal consults it before/after; G8 uses it as the final
    done-gate. No "is it green?" question is ever answered by guessing.

---

## 1. The Problem (evidence)

`tools/tcl2go/gen.go` contains **766 whole-file `skipTestFiles` entries** plus
per-test `skipTests`. They were grouped by the previous plan into "N/A" and
"DEFERRED" buckets. Categorization by reason text:

| Bucket | Count | True disposition |
|--------|-------|------------------|
| Engine gaps "DEFERRED" (real SQL features) | 405 | **Implement** |
| FTS3/4/5 (full-text search) | 94 | **Implement** |
| WAL / shared-memory / concurrency | 79 | **Implement** (WAL + locking) |
| Other real engine (auth, alter, misc, json, vtab modules…) | 64 | **Implement** |
| C-API (prepare/step/bind/blob/hooks…) | 54 | **Port to Go paradigm** |
| Custom VFS / OS layer | 41 | **Mostly implement** (locking, journal, IO; not syscall internals) |
| Window functions | 10 | **Implement** |
| Query planner / EXPLAIN | 8 | **Implement** |
| rtree | (in OTHER) | **Implement** |
| Session/RBU | (in OTHER) | **Implement** (later) |
| Shell / perf / platform | ~25 | Mostly genuine N/A (own CLI; benchmarks; Windows/UTF-16/ICU) |

Within the 405 "engine gap deferred": **140 packages were skipped *solely*
because they were "slow"** (5–20s each: view, index, attach, savepoint, sort,
count, select, alter, orderby, etc.) — these are real functional tests that must
be un-skipped and made green.

**Net**: the functional surface Frigolite must reach is ~614 packages minus a
small genuine-N/A remainder (~60: pure malloc internals, Windows/UTF-16/ICU
platform, raw perf benchmarks). Everything else must be implemented and green.

---

## 2. Current State (snapshot)

> **The detailed engineering design lives in `portplan/DESIGN.md`** — read it
> alongside this plan. It contains the per-area gap analysis, target data
> structures, algorithms, SQLite source pointers, and exact change locations.
> Task files (§5) reference DESIGN sections by letter (§A…§L).

- HEAD `7b7b1a22c` on `main`; tree now cleaned of legacy plan files (see §0/G0).
- 614 testgen packages; **368 PASS, 246 whole-file skipped (N-A), 0 FAIL**
  (last full sweep `tools/status/status`, 2026-08-11). Exact numbers, the
  per-family breakdown, and the full skip inventory live in **`STATUS.md`**
  (companion evidence: `portplan/NA_EVIDENCE.md`, audited by
  `tools/status -audit`).
- **FUNCTIONS-family stabilization (STAB) complete before formal G3 goals.**
  STAB-1..5 COMPLETE (see `portplan/tasks/TASK_G3_FUNCTIONS.md`):
  - **STAB-4** ✅ like (TEXT/BLOB compare in `internal/util/compare.go`),
    printf/func5 (transpiler `emitSkippedTestSideEffects` tolerance),
    round (transpiler `bodyEndsWithSetVar` 3-arg `set x [cmd]`) — all green.
  - **STAB-5** ✅ 8 FAIL clusters: existsexpr (EXPLAIN QUERY PLAN
    EXISTS-to-JOIN loops in `internal/execquery/explain_plan.go`), orderby
    (tcl2go bare `$var` expected-value substitution), vtab (verbatim
    `CREATE VIRTUAL TABLE` RawSQL storage; FTS3 column-type stripping;
    FTS hidden table-name/docid columns), vtabI (range-list proc
    transpilation), swarmvtabfault, misc, tkt (RAISE(IGNORE) in BEFORE
    triggers; IN subquery match+NULL), tkt_80e031a00 — all green.
  - **rowvalue** ✅ (part of G0 FIX-4-FAILS) — NULL row-value IN semantics:
    `(1,NULL) IN (SELECT 1,NULL)` is NULL not 1
    (`subqueryRowMatch` in `internal/execexpr/expression_inlist.go`).
- G0 FIX-4-FAILS done: `check`, `fkey`, `subquery`, `rowvalue` all green
  (guarded by every STAB verify command).
- Existing partial subsystems: `internal/fts/` (~2000 lines, FTS3 tokenizer +
  query + storage), `internal/vtab/` (basic module system + `generate_series`),
  window/JSON **parsed/stubbed only** (return NULL/no-op), CTE parsed.
- Public API: `Open/Close/Exec/Query/RegisterFunction/RegisterCollation` +
  authorizer/limit/progress hooks. No prepared-statement / step / bind / blob /
  backup / snapshot API yet (→ C-API port, G5).
- Legacy `PORTPLAN.md`, `HANDOVER.md`, `plans/`, and old `portplan/TASK_G*.md` are
  now in `archive/` (the clean tree: `PORTPLAN.md` + `portplan/{GUIDELINES.md,
  tasks/}`).

---

## 3. Phasing (execution order)

Dependency-driven. Each phase's **core** goals must be green before the next
phase's core goals start. Within a phase, goals may run in parallel where they
touch disjoint files; otherwise serialize. **Every goal runs with
`freshContext: true` and carries state only via its handover note.**

```
G0  HOUSEKEEPING + UN-SKIP        archive old files; un-skip the 140 "slow" pkgs;
│                                 fix the 4 known FAIL packages (foundation)
├─ G0.5  GOD-FILE SPLIT          behavior-preserving split of select.go (9039 ln),
│   (PREREQUISITE)               alter.go, insert.go → SOLID-sized files (DESIGN §B)
├─ G1  CRUD & QUERY ENGINE        the critical path: make the core engine solid
│   ├─ G1.CRUD   G1.EXPR   G1.WHERE   G1.ORDER   G1.SETOPS
│   ├─ G1.JOIN   G1.SUBQUERY   G1.AGG   G1.VIEW
│   └─ G2  SCHEMA & CONSTRAINTS
│       ├─ G2.ALTER  G2.INDEX  G2.TRIGGER  G2.FKEY  G2.CONSTRAINTS  G2.COLLATE
│       ├─ G2.SAVEPOINT  G2.ATTACH
│       └─ G3  FUNCTIONS & DATETIME
│           ├─ G3.STRING  G3.NUMERIC  G3.DATETIME  G3.PRINTF
│           └─ G4  ADVANCED SQL
│               ├─ G4.CTE   G4.WINDOW   G4.MATERIALIZED
│               ├─ G4.UPSERT  G4.RETURNING
│               └─ G5  C-API PARADIGM PORT
│                   ├─ G5.STMT   G5.BIND   G5.BLOB   G5.BACKUP   G5.HOOKS
│                   └─ G6  EXTENSIONS & VIRTUAL TABLES
│                       ├─ G6.JSON   G6.RTREE
│                       ├─ G6.VTAB-MODULES (csv, carray, intarray, series…)
│                       ├─ G6.FTS3/4   G6.FTS5
│                       └─ G7  PLANNER, WAL & CONCURRENCY, TRIAGE FINAL
│                           ├─ G7.PLANNER  G7.EXPLAIN  G7.ANALYZE
│                           ├─ G7.WAL   G7.LOCKING   G7.SNAPSHOT   G7.SESSION
│                           └─ G8  FINAL TRIAGE & N/A EVIDENCE
```

**Dependency rule**: a later phase must not start until its predecessor phase's
*core* goals are green. A goal that breaks an earlier goal's verify command is a
**blocker**, not a to-do — fix-forward immediately.

---

## 4. Goal Decomposition — Granularity Rule

- **One goal = one cohesive feature/area = one testgen family (or a small
  cluster) + a pure-Go pre-test + a `verifyCommand`.**
- Each goal is created as a Goa goal with `freshContext: true`, a machine-checkable
  `verifyCommand`, and an **ordered todo list** (Goa supports unlimited todos).
- A goal's todo list is the step-by-step decomposition; the agent ticks todos as
  it commits. Todos never escape the goal.
- **Small units**: prefer a goal covering 1–3 testgen packages over a mega-goal.
  Big subsystems (FTS5, WAL) get *multiple* sequential goals.

---

## 5. Task Index → see `portplan/tasks/TASK_*.md`

Each task file is **self-contained**: an agent can run it with only that file +
`portplan/GUIDELINES.md` + `archive/PORTPLAN_legacy.md` (for history) in context.
One task file per phase; each contains multiple **Goa goals** (small units), each
with its own `verifyCommand` and ordered todo list.

| Phase | Task file | Goals inside | Verify (short) | Status |
|-------|-----------|--------------|----------------|--------|
| G0 | `TASK_G0_HOUSEKEEPING.md` + `TASK_COMPLEXITY_REFACTOR.md` | HOUSEKEEPING, STATUS, UNSKIP-SLOW, FIX-4-FAILS, **COMPLEXITY+SOLID (CX-01→CX-26, SOLID-01→SOLID-15)** | archive done; status tool; un-skip slow; check/fkey/subquery/rowvalue green (**all 4 done — rowvalue NULL row-value IN fixed in STAB-5**); **repo-wide gocognit ≤15/gocyclo ≤12/file ≤1000 (CX-26 done); Engine god-object split into sub-packages (Track 2 SOLID pending)** | 🟢 |
| G0.5 | `TASK_G0_SPLIT.md` | SELECT-SPLIT, ALTER-SPLIT, INSERT-SPLIT | behavior-preserving god-file split (DESIGN §B); all tests unchanged | ⚪ |
| G1 | `TASK_G1_CRUD.md` | CRUD, EXPR-WHERE, ORDER-SETOPS, JOIN-SUBQUERY, AGG-VIEW | select/insert/update/delete/expr/where/order/join/subquery/agg/view families + pre-tests | ⚪ |
| G2 | `TASK_G2_SCHEMA.md` | ALTER, INDEX, TRIGGER, FKEY-CONSTRAINTS, COLLATE, SAVEPOINT-ATTACH | alter/index/trigger/fkey/check/collate/savepoint/attach families + pre-tests | ⚪ |
| G3 | `TASK_G3_FUNCTIONS.md` | STRING, NUMERIC, DATETIME, PRINTF | instr/substr/like/quote/round/date/printf families + pre-tests | 🟡 (STAB-1..5 COMPLETE — like/printf/func5/round + existsexpr/orderby/vtab/vtabI/swarmvtabfault/misc/tkt/tkt_80e031a00 all green; formal G3 goals not started) |
| G4 | `TASK_G4_ADVANCED.md` | CTE, WINDOW, UPSERT-RETURNING | with*/window1–9/upsert/returning + pre-tests | ⚪ |
| G5 | `TASK_G5_CAPI.md` | STMT-BIND, BLOB, BACKUP, HOOKS | Go-idiomatic Stmt/Bind/Blob/Backup/Hooks API; bind/capi/incrblob/backup/hook families + pre-tests | ⚪ |
| G6 | `TASK_G6_EXTENSIONS.md` | JSON, RTREE, VTAB-MODULES, FTS3, FTS5 | json*/jsonb/rtree/csv/carray/fts3*/fts5* + pre-tests | ⚪ |
| G7 | `TASK_G7_PLANNER_WAL.md` | PLANNER-EXPLAIN, WAL, LOCKING, SESSION-RBU | explain/analyze/wal*/lock*/shared*/busy*/snapshot/session*/rbu + pre-tests | ⚪ |
| G8 | `TASK_G8_FINAL_TRIAGE.md` | FINAL-TRIAGE, NA-EVIDENCE | ALL_APPLICABLE_GREEN; curated N/A evidence | ⚪ |

> **Progress legend**: 🟢 done · 🟡 partial · ⚪ not started. Updated in-task.

---

## 6. Definition of Done (per goal)

A goal is **done** when **all** are true:
1. Its `verifyCommand` exits 0 (specific testgen packages + pre-test).
2. `make quality` passes (strict gate: gocognit ≤15, gocyclo ≤12, staticcheck, file-size ≤1000 via `tools/quality_gate.sh`).
3. `go build ./...` passes.
4. `go test -run TestSOLID_ ./...` passes (architecture gates).
5. No new regression in any **earlier** goal's verify command.
6. Every fix was verified against `/usr/bin/sqlite3` (oracle); every
   engine-vs-transpiler decision was made via a pure-Go test **first**.
7. No genuine engine gap was re-hidden behind a `skipTestFiles`/`skipTests` entry.
   Re-classifying a package to N/A requires a **new evidence note** explaining
   why Frigolite has an equivalent-or-better implementation (the *only* allowed
   exception).
8. Commits are atomic, prefixed `G<N>.<TASK>.<step>`, and **pushed**. The task
   MD checkbox and plan status are updated in the closing commit.
9. **Regeneration check**: if `tools/tcl2go/` changed, regenerate
   (`go run ./tools/tcl2go/`), re-run the verify command **and** a
   previously-green regression sample, and fix any regressions exposed.

---

## 7. Commit & Cadence Rules

- **Small, atomic commits** — one logical fix per commit; message prefixed
  `G<N>.<TASK>.<step>: <summary>`.
- After each engine fix, re-run the goal's verify command before committing.
- After a transpiler fix, regenerate with `go run ./tools/tcl2go/`, review the
  `git diff` blast radius, and commit regenerated files separately:
  `G<N>.<TASK>.<step>: regenerate testgen after <reason>`.
- **Push** after every goal completes (and ideally after each commit) so any
  interruption can resume from the plan files alone.
- **Never** weaken/skip/hard-code a test to make it green.
- Update the task MD's checkbox in the same commit that closes a step.

---

## 8. Oracle & Triage Helpers

- **Oracle**: `/usr/bin/sqlite3` — derive expected output, error text, edge cases.
- **Triage rule (mandatory)** on any testgen failure: write a pure-Go test via
  `frigolite.Open/Exec/Query` that exercises the failing feature. Run it:
  - Fails → engine bug → fix the engine, re-run both tests.
  - Passes while testgen fails → transpiler bug → fix `tools/tcl2go/`, regenerate.
- See `portplan/GUIDELINES.md §Triage` for the full protocol.

---

## 9. Regressions & Scheduling

- Run each goal with `freshContext: true`; the handover note (max 4096 chars,
  structured State/Decisions/Next/Risks/Carried limits) is the **only** state
  crossing goals.
- Each goal's verify command is deliberately **narrow** (its packages + pre-test
  + build + SOLID + quality). Do **not** run all 614 packages per commit.
- Run a broader regression sweep at **phase boundaries** only.
- A goal that breaks an earlier phase's verify command is **blocked**, not
  deferred — fix before proceeding.
- **Scheduling note**: slow packages are run in narrow per-goal verify commands,
  never globally. Performance *optimization* (e.g. query planner, btree) is
  legitimate work *within* a goal only if a test is genuinely too slow to run at
  all (>60s) — in which case the fix is to make it fast, not to skip it.

---

## 10. Genuine N/A (the *only* allowed exclusions)

These are the narrow, evidence-backed exclusions. Every entry must justify why
Frigolite has an equivalent-or-better pure-Go implementation, OR why the test
exercises C-runtime internals with no SQL-functional surface. This list is
**curated in G8** from the surviving `skipTestFiles`; anything not justified here
**must** be implemented in G0–G7.

- **C malloc/allocator internals** (`malloc*`, `mem*`, `lookaside`, `memsubsys`,
  `pcache` as internal-data-structure tests) — Frigolite uses Go's GC; there is no
  malloc to fault-inject. (Allocator *behavior* reachable via SQL is in scope.)
- **Windows / UTF-16 / ICU platform specifics** (`win32*`, `badutf*`, `enc`, `icu`,
  `utf16`, `symlink` on non-target platforms) — OS/encoding internals.
- **Raw perf benchmarks** (`speed*`, `bigfile`, `bigmmap`, `bigsort`, `soak`,
  `tpch`) — not functional assertions.
- **Test-only C ABI harnesses** (the `src/test*.c` echo/tclvar modules' callback
  *trace* assertions) — no SQL surface; the SQL *behavior* they wrap is in scope.
- **Fuzz infrastructure plumbing** (`dbfuzz`, `fuzz` harness plumbing) — though
  any *bug* a fuzzer finds that is reachable via SQL must be fixed.

> If a package straddles N/A and in-scope (e.g. `crash*` mixes crash-sim plumbing
> with real rollback semantics), the in-scope parts **must** be implemented; only
> the C-internals-only parts may be per-test skipped with evidence.

---

## 11. How to resume from this plan (the contract)

Because every goal is committed + pushed and carries a handover note, an
interruption at any point resumes by:

1. `git pull` — read `PORTPLAN.md` status table + the active task MD.
2. `goal get` — read the current goal's objective, todos, and handover.
3. Continue from the first non-done todo; run its verify command to confirm.
4. On completion: `goal update complete` (runs verify), push, tick the next task.

No reliance on conversation history. **The plan files are the source of truth.**
