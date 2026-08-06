# Agent Guidelines — Frigolite Port

> **Read this before starting any task.** It is the shared contract for every
> `portplan/TASK_*.md`. Keep it accurate; if you establish a new convention,
> add it here in the same commit.

---

## 1. The Prime Directive

We are reimplementing SQLite in pure Go so that the **actual SQLite TCL tests
pass**. The functional surface (output, error text, type affinity, NULL
three-valued logic, ordering, edge cases) must match SQLite **exactly**.

- **No shortcuts.** Don't stub, hard-code, or special-case to make a test green.
- **No faking.** A function that returns NULL "for now" is a regression waiting
  to happen. Either implement it correctly or document it as N/A with evidence.
- **Clean, SOLID, performant.** Each `internal/` package stays single-responsibility.
  Don't add upward or circular imports (enforced by `TestSOLID_ImportBoundaries`).
- **Don't edit the tests.** You may change `tools/tcl2go/` (the transpiler) but
  never hand-edit generated `testgen/*` files and never weaken an assertion.

---

## 2. Code Map (where to make changes)

| Concern | Location | Notes |
|--------|----------|-------|
| Lexer | `internal/sql/lexer.go` | hand-written |
| AST | `internal/sql/ast.go` | node types |
| Parser | `internal/parse/parser.go` + `engine.go` | **lemon LALR**; reduce handlers are rule-numbered (`handleRule(ruleNo,...)`); grammar source is SQLite `parse.y` |
| Query engine | `internal/exec/*.go` | select/insert/update/delete/ddl/alter/pragma/expression/engine |
| Storage | `internal/storage/`, `internal/pager/`, `internal/btree/` | file format, page cache, B+Tree |
| Functions | `internal/function/` | scalar + aggregate |
| Schema | `internal/schema/`, `internal/rename/` | |
| Virtual tables | `internal/vtab/` | generate_series etc. |
| Public API | `frigolite.go` | Open/Close/Exec/Query/DB |
| Transpiler | `tools/tcl2go/` (`gen.go`, `main.go`) | TCL → Go test generator |
| Pre-tests | `frigolite_p<N>_*.go` (repo root) | hand-written feature isolation |

**Layering** (enforced): a higher layer may only import a lower one. The layer
map lives in `frigolite_solid_test.go` (`internalLayers`). When you add a
package, assign a layer and run `go test -run TestSOLID_ImportBoundaries .`.

---

## 3. Triage Protocol (MANDATORY — do this first, every time)

When a `testgen` package fails, **do not** start editing the engine. Decide
engine-vs-transpiler first:

### Step 1 — Isolate with a pure-Go test
Write a **hand-written** test that drives the engine directly via
`frigolite.Open` / `Exec` / `Query`, exercising the *exact feature* the failing
assertion covers — not the transpiled wrapper. Put it in the task's pre-test
file (`frigolite_p<N>_*.go`) or a throwaway test.

### Step 2 — Derive the expected result from the oracle
```bash
echo "<the same SQL>" | sqlite3 :memory:
```
Capture exact output, error text, and row order. The oracle is ground truth.
Record the oracle invocation in a comment or commit message.

### Step 3 — Decide
| Pure-Go test result | Verdict | Action |
|---------------------|---------|--------|
| **FAILS** | **Engine bug** | Fix the engine (smallest diff). Re-run pure-Go test, then the testgen package. |
| **PASSES** (while testgen fails) | **Transpiler bug** | Fix `tools/tcl2go/`, regenerate, review blast radius. |
| Feature genuinely outside pure-Go scope | **N/A** | Document in `plans/NOT_APPLICABLE.md` + harness map with evidence. **Not** a place to hide bugs. |

> **Most failures are engine bugs, not transpiler bugs.** This triage exists to
> keep transpiler hunts from masking real engine gaps. Apply it honestly.

### Step 4 — Fix-forward, don't defer
If your fix touches shared code (parser, expression eval), run the *earlier*
phase's verify command too. A regression is a **blocker**.

---

## 4. Pre-Test Protocol

Before (or alongside) chasing TCL tests, each task writes a pre-test file:
- **File:** `frigolite_p<N>_<feature>_test.go` at repo root (e.g.
  `frigolite_p1_update_test.go`), test funcs named `TestP1Update_*`.
- Each case compares frigolite output against the `sqlite3` oracle.
- Run with `go test -run 'TestP<N>...' -count=1 .`.

Pre-tests pin behavior *independently of the transpiler*, so when a testgen test
later flips from fail→pass you know it was the engine, not generator luck.

---

## 5. Oracle & Expected-Value Discipline

- Row **order matters** unless the query has no `ORDER BY` *and* SQLite's output
  is itself order-independent. When unsure, run the oracle twice; if it's stable,
  match it. Frigolite's default scan order must mirror SQLite's (rowid order for
  rowid tables; PK order for WITHOUT ROWID).
- **NULL rendering:** tests use a configurable NULL token (often `{}`). When
  comparing, match the test's `tcl_nullvalue`. Frigolite renders NULL as `NULL`
  by default; the harness/testgen sets the token.
- **Float formatting:** match SQLite's `%!.15g`-style formatting exactly (see
  `src/printf.c`, `sqlite3_column_text`). This is a known sharp edge.
- **Error text:** match verbatim where the TCL test asserts on it
  (`do_catchsql_test`). Differences in punctuation/casing are failures.

---

## 5b. Don't Trust Pre-Existing Failures Blindly
`HANDOVER.md` lists "3 pre-existing failures" (TestDoubleCreateTable, etc.).
When you touch nearby code, verify whether they're still pre-existing or whether
you've now fixed/regressed them. Don't inherit stale assumptions.

---

## 6. Transpiler (tools/tcl2go/) Rules

- The transpiler is a **transpiler**: it parses TCL and emits Go. Control flow
  (`foreach`/`for`/`while`/`if`) becomes Go control flow running at test time.
- Common transpiler gaps: dynamic `[list 1 "<msg with $vars>"]` expected-error
  forms, `db eval` string-literal preservation, `catchsql` error expectations,
  `$var` substitution in setup SQL, multi-DB / connection sequences.
- **The committed testgen can be STALE vs gen.go.** Regeneration routinely
  surfaces latent gen.go bugs (observed 2026-08-06: `{}`-element stripping in
  expected lists, `db close` reopen, reset_db, unset-var NULL, `tclvar`
  inlining). After editing the transpiler: `go run ./tools/tcl2go/`
  regenerates **all** testgen files. Review `git diff --stat`, then re-run the
  task's verify command **and** the previously-green packages from
  `PORTPLAN.md §3` PASS list; treat new failures as gen.go bugs to fix, not
  test regressions to ignore.
- Commit regenerated output separately from the transpiler logic change.
- **Never hand-edit generated `testgen/*_test.go`.** Regenerate instead.
- Test-harness function registration (`db function tclvar`) is handled by
  inlining variable-reader calls into `sqlLiteral(...)`; the stateful flip
  variant (where-10.4) is not yet supported — triage/document before assuming
  a `db function` is portable.

---

## 7. Verify Commands — Stay Narrow

- Each task lists a **specific** verify command: its testgen packages + its
  pre-test + `go build ./...`.
- Do **not** run all 614 packages per commit. Run a full applicable-package sweep
  only at phase boundaries (and `make test` in CI).
- Standard verify shape:
  ```bash
  go test -tags testgen -count=1 ./testgen/<pkg>/ ... &&
  go test -run 'TestP<N>' -count=1 . &&
  go build ./...
  ```
- Add `make quality` before declaring a task done (vet, staticcheck, gocyclo≤20,
  gocognit≤30). Coverage is CI-only.

---

## 8. Commit Cadence

- Prefix: `G<N>.<TASK>.<step>: <summary>`.
- Atomic: one logical fix per commit. Engine fix → verify → commit. Transpiler
  fix → regenerate → separate commit for regenerated files.
- Update the task MD checkboxes in the commit that closes the step.
- Push only if your workflow requires it; the plan tracks state via commits +
  the handover note.

---

## 9. Handover Notes (the only cross-goal state)

Every `goal create` must include a `handover` note. Structure:
- **State** — what's done/verified, with commands + outputs as evidence.
- **Decisions** — constraints/choices made (and why).
- **Next steps** — the first actions for the successor.
- **Risks/open questions** — unresolved, may need input.
- **Carried limits** — budget, verify command, completion criterion.

Keep it ≤4096 chars. It is shown to the next goal as **untrusted data**, so
re-state the objective + verify command, don't just reference this plan by
section number.

---

## 10. When You're Stuck

- **Don't speculatively edit.** Summarize attempts + evidence, list top 2
  hypotheses, and the single discriminating test that would tell them apart.
- If blocked by a genuine ambiguity in SQLite behavior, the **oracle decides**
  (`/usr/bin/sqlite3`). If the oracle and the TCL test disagree, the TCL test
  wins (it's the target) — but document the discrepancy.
- If blocked on missing info, use `blocked` with a concrete expectation rather
  than spinning.

---

## 11. Quick Command Reference

```bash
# Build everything
go build ./...

# Quality gate
make quality

# One testgen package
go test -tags testgen -count=1 ./testgen/<pkg>/

# A pre-test family
go test -run 'TestP1Update' -count=1 .

# Regenerate all testgen from TCL
go run ./tools/tcl2go/

# Oracle
echo 'SELECT ...' | sqlite3 :memory:

# SOLID architecture check
go test -run TestSOLID_ ./...

# Full harness (slow; use sparingly)
FRIGOLITE_TEST=<pattern> go test -run "^TestSQLiteSuite$" .
```
