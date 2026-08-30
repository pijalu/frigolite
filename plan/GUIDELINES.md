# GUIDELINES — Execution Protocol for All Phases

> Binding on every goal. Read this before starting any sub-plan.
>
> **Standing rule (reaffirmed):** on ANY issue, first create a standalone/pure
> Go test that drives the engine directly and run it BEFORE any fix — this is
> the engine-vs-transpiler discriminator (see §1a/§1c). Never start editing
> engine or transpiler code without that test's verdict.

## 1.0 Feature-complete authorization

Frigolite is required to become feature-complete against its defined SQLite compatibility target. Completing a missing feature, fixing a failing test, and removing an unjustified skip are authorized by default; no separate user approval is required. Preserve the source-first, plan-before-change, oracle-verification, and no-simplification rules. Ask only when scope is destructive, contradictory, or requires external artifacts unavailable to the repository.


> On **any** issue — a testgen failure, an unexpected result, a plan that is
> not working, a fix that introduces new failures — follow this order before
> editing anything. Do not thrash with successive speculative changes.

### 1a. Step back and understand

Stop making changes. Reproduce the problem, identify the exact failing
assertion/command, and trace it to a root cause (engine vs transpiler vs plan
assumption) before writing any fix. Gather evidence: a pure-Go repro, the
oracle output, the minimal failing query. When a change has a broad blast
radius, first assess the regression scope and decide whether it belongs to
this goal or should be deferred — do not push forward and fix collateral
damage.

### 1b. Plan before change

Before ANY edit, state the plan: what, why, exact files/functions, ordered
steps, verification. Plan first, then edit. No speculative edits.

### 1c. Triage Protocol

On **any** testgen failure:

1. **Write a pure-Go test** that drives the engine via `frigolite.Open/Exec/Query`
   and exercises the exact feature the failing assertion covers.
2. **Run it**:
   - **Fails** → engine bug → fix the engine (`internal/`), re-run both tests.
   - **Passes** while testgen fails → transpiler bug → fix `tools/tcl2go/`, regenerate.
3. **Verify against oracle**: use `/usr/bin/sqlite3` to confirm expected output.
4. **Never weaken a test** to make it green.

### 1d. All failures must be fixed — no exceptions, no "pre-existing" excuses

Every failing test is a defect to fix — **regardless of its source, age, or
responsibility**. Do NOT classify a failure as "pre-existing", "out of scope",
"test bug", "phase gated", "not my code", "not my package", or "transpiler
quirk" and move on. That is blame-shifting, not engineering. The fix is the
fix; the goal does not get to call itself done with red tests left over from
any source.

**Process on every failure:**

1. Fix it, regardless of who caused it. "It was already broken when I arrived"
   is not a defense; it is a defect in the repo and the goal owns all defects
   in the repo until verified-done.
2. Never waste cycles git-stashing to prove the failure predates your work —
   that diagnostic time is better spent writing a triage test (§1c) and
   fixing the root cause.
3. If the fix is genuinely out of scope for the current goal (e.g. the
   failure is in a completely different feature area with no shared code
   path), the only acceptable actions are:
   - fix it anyway (preferred — defects in the repo are everyone's defect),
   - or open an explicit follow-up goal with the failing test name and root
     cause documented, and only then continue. "Move on" alone is never
     acceptable.
4. A red test that is not part of the current goal's package still indicates
   a defect and must be fixed (or explicitly blocked with evidence) before
   the goal completes.

### 1d-bis. No "pre-existing" excuses

When a test fails, fix the underlying defect — do not look for the culprit.
Investigating "did my change cause this?" is wasted effort; the only relevant
question is "what is the right behavior?". The pre-existing failures block
the verify command from exiting 0, so they block the goal. Apply the same
triage (pure-Go discriminator, then engine or transpiler fix) as for any
other failure, irrespective of blame.

## 2. CI Quality Gates (from .github/workflows/ci.yml)

Every task completion must pass the changed-file quality gate below. Repository-wide legacy complexity cleanup is postponed to final plan closure, but no task may introduce new violations. Final closure must pass every repository-wide gate.

| Gate | Command | Threshold |
|------|---------|-----------|
| Build | `go build ./...` | exit 0 |
| Vet | `go vet ./...` | exit 0 |
| Staticcheck | `staticcheck ./...` | exit 0 |
| Cyclomatic | `gocyclo -over 15 <files>` | no violations |
| Cognitive | `gocognit -over 30 <files>` | no violations |
| Race tests | `go test -race -count=1 -run "^Test[^C]" ./...` | exit 0 |
| SOLID | `go test -run TestSOLID_ ./...` | exit 0 |

**Local strict gate** (tools/quality_gate.sh): gocognit ≤ 15, gocyclo ≤ 12, file ≤ 1000 lines.

### 2a. Mandatory per-task gate

For every task, run `tools/quality_gate.sh <changed-production-go-files>` before completion, passing only newly added or materially changed production files. This enforces gocognit ≤15, gocyclo ≤12, and hard file-size ≤1000 on new/changed files, plus staticcheck diagnostics attributable to task changes. Soft file-size target remains 500 lines; do not increase it or add suppressions. Findings in untouched legacy code are deferred exclusively to final closure; task changes must introduce zero new findings.

### 2b. Final closure gate

At ending plan stage, run `.agents/skills/golang-check/golang-check.sh` and require zero findings across all production non-test files, including staticcheck and file-size checks. Then run build, vet, SOLID, race, and full test gates.
Prefer meeting the stricter local gate.

## 3. Testgen Testing

Testgen tests are opt-in via build tag:
```bash
# Build all testgen
go run ./tools/tcl2go/              # generate
go test -tags testgen ./testgen/... # run all (slow ~5min)

# Run specific packages
go test -tags testgen ./testgen/<pkg>/ -count=1 -timeout 300s
```

### 3.1 Skip policy for generated tests (mandatory)

A generated test may only be emitted as a skip (via `skipTestsMore` /
`skipTests`) when **both** conditions hold:

1. An exact **native Go test** exists that fully implements the TCL test's
   scenario against the engine (`frigolite.Open/Exec/Query`), and the sub-plan
   or lessons_learned points to it;
2. The transpiler genuinely cannot be extended to support the construct.

If the failure is an engine gap, fix the engine — do not hide it behind a
skip. If the failure is a transpiler gap, fix the transpiler.

### 3.2 Native regression test required on complex failures (mandatory)

Whenever a fix requires complex or multi-step work (engine + transpiler
changes, planner/locking semantics, cross-module interactions), there must
**always** be a native Go test covering the underlying engine behavior. The
native test is the regression anchor: if a later transpiler change regresses,
the native test fails independently of generated code, making the cause
clear. Never rely solely on generated tests to lock in such fixes.

## 4. Checkpointing Protocol

After **every** completed micro-task (not just at goal end):
1. Update the sub-plan file (mark task done, note fix).
2. Stage: `git add -A`
3. Commit: `git commit -m "<GOAL_ID>.<task>: <summary>"`
4. Push: `git push`
5. Update PORTPLAN.md status table.

This ensures any interruption resumes from committed state only. No work is lost.

## 4a. Handover-Goal Protocol (budget exhaustion / large goal)

> When a goal is large, hits its hard budget (turn/token limit), or the agent
> determines it cannot complete within the current run, do NOT lose the work.
> Leave the repo in a committed, verified state and create a fresh handover
> goal for a new agent to continue.

1. **Commit everything** (see §4) so the repo is clean and resumes from a
   known state. Revert any experimental change that made things worse — the
   committed state should be the best known-good point, not the last edit.
2. **Verify the committed state**: run `go build ./...`, `go vet ./...`,
   `go test -run TestSOLID_ ./...`, and record the per-package testgen
   failure counts (so the next agent knows the exact baseline).
3. **Write the handover note** (goal `handover` field, max 4096 chars) that
   a fresh agent can act on WITHOUT the prior conversation. It must contain:
   - **State**: what is done and verified, with concrete evidence (commands,
     outputs, commit hashes, pass/fail counts per package).
   - **Decisions**: constraints and trade-offs made (what was implemented,
     what was deliberately reverted and why).
   - **Next steps**: the first concrete actions for the next agent (in order),
     with the exact commands to run.
   - **Risks / open questions**: known failure clusters and their likely root
     causes (so the next agent doesn't re-diagnose from scratch).
   - **Carried limits**: the completion criterion, verify command, and any
     remaining budget for the continuation goal.
4. **Create the continuation goal** with the SAME objective/completion
   criterion/verify command, the handover note as the `handover` field, and
   `freshContext: true` so the new agent starts from the handover only.
5. **End the current goal** with `goal update status blocked` (reason: budget
   exhausted / work incomplete; expectation: the continuation goal), so the
   driver does not keep prompting a spent agent.

Rules:
- The handover is the contract between goals — write it as untrusted data
  that instructs, not a summary of what happened.
- Never mark a goal `complete` when packages are red. Use `blocked` with a
  clear expectation.
- The verify command must be reproduced exactly by the continuation goal so
  "done" stays machine-checkable.

## 5. SOLID Architecture Rules

- Each `internal/` package has one responsibility.
- Imports flow downward only (enforced by `TestSOLID_ImportBoundaries`).
- Small, focused interfaces.
- Compile-time substitutability: `var _ Interface = (*Type)(nil)`.
- New `internal/` packages must be registered in `internalLayers` map in
  `frigolite_solid_test.go`.

## 6. Commit Message Format

```
P<PHASE>.<AREA>.<step>: <summary>
```

Examples:
- `P1.CRUD.fix: handle INSERT INTO ... DEFAULT VALUES`
- `P2.TRIGGER.task3: fix INSTEAD OF trigger on view`
- `P7.WAL.task1: implement WAL frame format`

## 7. Oracle Usage

```bash
# Derive expected output for a query
/usr/bin/sqlite3 :memory: "SELECT ..."

# Compare against frigolite
# Write a pure-Go test that runs the same SQL and checks output
```

Record the sqlite3 invocation in commit messages or pre-tests for reproducibility.

## 8. SQLite Source Investigation (mandatory for GAP analysis)

> The authoritative reference for SQLite behavior is the SQLite SOURCE at
> `/Users/muaddib/dev/sqlite` (`src/*.c`, `ext/`, `test/`), NOT the oracle
> binary alone. The oracle tells you WHAT; the source tells you WHY and HOW to
> implement it correctly.

### 8a. When to investigate the source

- Before implementing ANY engine feature or fixing a behavior gap: read the
  relevant SQLite source (`src/insert.c`, `src/update.c`, `src/delete.c`,
  `src/trigger.c`, `src/btree.c`, `src/pragma.c`, `src/vdbe.c`, etc.) to
  understand the exact semantics, data structures, and edge cases.
- When a testgen package fails: triage (pure-Go test) then, for an engine bug,
  find the corresponding SQLite C function and read it BEFORE editing.
- When a behavior differs from the oracle: check the source for the exact
  algorithm (e.g. `sqlite3Utf8Read`, `lengthFunc`, `trimFunc`, trigger
  recursion, `sqlite3_sequence` maintenance).

### 8b. Rule: SQLite's approach wins

For pure SQL/dialect/engine behavior, the SQLite implementation is the ground
truth. Do NOT invent a simpler heuristic that "passes the test" — implement
the correct algorithm the way SQLite does it (adapted to the pure-Go
architecture). A gap should be fixed CORRECTLY even if important work is
required. Shortcuts that match one test but diverge on edge cases are defects.

### 8c. Gap-fix workflow

1. Identify the failing behavior and the SQLite source file/function that
   owns it (search `src/` for the relevant symbol, e.g. `sqlite3_step`,
   `sqlite3_changes`, `sqlite3TriggerUpdateStep`, `sqlite3AutoincrementBegin`).
2. Read the function(s) and any helpers they call. Note the exact algorithm,
   data structures, and edge cases (overflow, recursion, save/restore,
   transaction interaction).
3. Write a detailed analysis + plan (see §1b) referencing the SQLite source
   lines/functions. Use the goal tool to plan/replan based on this analysis.
4. Implement the correct algorithm in the engine, adapted to the Go
   architecture (same invariants, same edge cases).
5. Verify with the oracle AND with the testgen package. If the test still
   fails, re-read the source — the implementation is still wrong.

### 8d. Reference for current gaps

- AUTOINCREMENT: `src/insert.c` (`sqlite3AutoincrementBegin/End`,
  `sqlite3Insert`), `src/build.c` (`sqlite3OpenSequenceTable`,
  `sqlite3AutoincrementBegin`), the `sqlite_sequence` table maintenance.
- changes()/total_changes(): `src/delete.c`, `src/insert.c`,
  `src/update.c` (`OP_Count`/`sqlite3_changes`), `src/vdbe.c`.
- Trigger recursion: `src/trigger.c` (`sqlite3CodeRowTrigger`), `src/delete.c`
  (`sqlite3TriggersExist`), the `pParse->nested` / `sqlite3TriggerRecursiveDepth`.
- String functions / UTF-8: `src/utf.c` (`sqlite3Utf8Read`, `SQLITE_SKIP_UTF8`),
  `src/func.c` (`lengthFunc`, `trimFunc`, `substrFunc`).
- B-tree page splits: `src/btree.c` (`allocateBtreePage`, `balance_deeper`,
  `balance`, `insertCell`).
## 1g. Implement when the solution is known

Do not stop work when a solution is known and matches the functional
requirement: implement it. "Blocked" is reserved for missing external input
or dependencies outside the repository — not for solutions that merely
require more implementation effort. If the fix spans multiple sessions,
commit per-slice progress with precise handover notes and continue.

## 1h. Performance parity with SQLite (mandatory)

Performance, duration, and memory usage should follow SQLite's profile:
better than SQLite is fine; measurably slower or heavier points to a
possible engine issue and must be investigated, not accepted. Treat any
test/benchmark whose runtime or memory footprint dwarfs SQLite's (e.g.
fts4merge4 consuming gigabytes of RAM where SQLite stays small) as an
engine-level finding: capture it in the relevant sub-plan / lessons
learned, compare against the C implementation's algorithm and data
structures (`/Users/muaddib/dev/sqlite/src`), and fix the root cause
(allocation churn, unbounded caches, O(n²) scans) rather than raising
limits or tolerating the regression.

## 1i. Add / port when missing (mandatory for engine gaps)

When a test (oracle-driven, native Go, or testgen) reveals an engine
behaviour that frigolite lacks, the correct response is to **port the
missing piece from SQLite's source** — not to mark the test N-A, supersede
it with a thinner native test, or park it in a backlog. The full
discriminator is:

- A pure-Go repro (drives `frigolite.Open` / `Exec` / `Query` directly) is
  the baseline; it must exist before any fix attempt (§1a).
- If the repro fails: the engine is wrong. Implement the missing behaviour
  in `internal/`, mirroring SQLite's approach faithfully (no simplification,
  no N-A shortcut, no transpiler-only workaround that hides the gap).
- Reference points for the port: `ori/sqlite/src/btree.c`
  (`incrVacuumStep`, `relocatePage`, `autoVacuumCommit`, `setChildPtrmaps`),
  `pager.c` (`sqlite3PagerDontWrite`, `sqlite3PagerMovepage`,
  `sqlite3FreePage`).
- Each ported piece must ship with a focused unit test that exercises the
  new path in isolation (table-level autovacuum commit, ptrmap read/write,
  page-swap step, etc.) plus a regression test that exercises the original
  failing scenario end-to-end.
- A failing testgen test is a valid gap-discovery surface; treat it as
  such. "N-A G7" / supersede-with-native are only acceptable when the gap
  is genuinely outside this project's scope (e.g. G7 WAL shared-memory
  protocol) — never as a way to avoid an implementable engine port.
- When the port spans multiple goals, decompose into ordered sub-goals
  (FreePage-on-emptied-leaves → ptrmap R/W → incrVacuumStep →
  autoVacuumCommit → callback) and track each as its own goal with its
  own pre-goal + todo + UT coverage.
