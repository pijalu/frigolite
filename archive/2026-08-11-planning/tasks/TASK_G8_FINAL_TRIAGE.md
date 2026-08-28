# TASK G8 — Final Triage & Genuine N/A Evidence

> **Phase**: G8 (FINAL — depends on G7 complete)
> **Goal IDs**: G8.FINAL-TRIAGE, G8.NA-EVIDENCE
> **Read first**: `PORTPLAN.md` §0, §10 (genuine N/A), **`portplan/DESIGN.md`
> (all sections — final integration)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started

---

## Objective

Drive the whole testgen tree to **zero FAIL** across every applicable package,
and justify each *remaining* `skipTestFiles`/`skipTests` entry with evidence that
it is genuinely N/A (C-runtime internals with no SQL surface) — *not* a hidden
engine gap. This is the final done-gate for the entire plan.

The `tools/status` report (built in G0) is the instrument: per-family pass/fail/
skipped, trending to 100% pass on applicable packages.

---

## Goal G8.FINAL-TRIAGE — Sweep remaining applicable failures to green

**Scope**: every testgen package still failing after G0–G7. Run `tools/status`
across all 614 packages and drive each non-green applicable package to pass.

**Verify command** (the global done-gate):
```bash
go test -tags testgen -count=1 -timeout 600s ./testgen/... 2>&1 | \
  grep -cE '^FAIL' | grep -q '^0$' && echo ALL_APPLICABLE_GREEN
```

**Todos**:
1. Run `tools/status --concurrency 8` → capture the full per-family report; this
   is the closing backlog.
2. For each remaining FAIL family: apply the triage rule (pure-Go pre-test +
   oracle); fix engine/transpiler; never re-skip without evidence.
3. Drive each family to green; re-run `tools/status` after each fix to confirm no
   regression in other families.
4. Reconcile the per-test `skipTests` entries: remove any that are now satisfied;
   keep only evidence-backed N-A entries.
5. Final full run → `ALL_APPLICABLE_GREEN`.

---

## Goal G8.NA-EVIDENCE — Curate the genuine N/A set

**Scope**: the surviving `skipTestFiles`/`skipTests` entries. Each must be one of
the `PORTPLAN.md §10` categories (C malloc internals; Windows/UTF-16/ICU platform;
raw perf benchmarks; test-only C-ABI trace assertions; fuzz plumbing) **with a
one-line evidence note**. Anything that does not fit → it's an engine gap:
implement it (loop back to the relevant G1–G7 goal).

**Deliverable**: a curated `portplan/NA_EVIDENCE.md` listing every surviving skip
with: package, test (if per-test skip), category, and the *reason Frigolite has an
equivalent-or-better implementation or the test has no SQL surface*. This replaces
the legacy `plans/NOT_APPLICABLE.md` (archived in G0).

**Verify command**:
```bash
# Every skipTestFiles entry must appear in NA_EVIDENCE.md with a category;
# tools/status --audit enforces this.
./tools/status --audit && test -f portplan/NA_EVIDENCE.md
```

**Todos**:
1. Enumerate surviving skips from `gen.go` + `tools/status`.
2. For each, classify per §10; write the evidence line.
3. For anything mis-classified as N/A that is actually an engine gap: open a
   follow-up (re-queue the relevant G1–G7 goal) — **do not leave it skipped**.
4. Add `--audit` to `tools/status`: fails if any skip lacks an evidence entry.
5. Commit `G8.NA-EVIDENCE: curated genuine N/A set`; push.

---

## Definition of Done (this task, and the whole plan)
- `ALL_APPLICABLE_GREEN`: the global verify command reports zero FAIL across all
  applicable testgen packages.
- `portplan/NA_EVIDENCE.md` exists and `tools/status --audit` passes: every skip
  is justified as genuinely N/A (equivalent-or-better implementation or no SQL
  surface), with evidence.
- `make quality` + SOLID gates pass; `go build ./...` passes.
- The full feature surface (CRUD, query, schema, functions, advanced SQL, C-API
  paradigm, extensions/vtabs/FTS, planner, WAL/concurrency, session/RBU) is
  implemented and green.
- `PORTPLAN.md` §5 → all rows 🟢.
