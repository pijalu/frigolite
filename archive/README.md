# Archive — Superseded Planning Artifacts

This directory holds **historical** planning files that are **no longer the
source of truth**. They are kept for reference only.

## Current source of truth

- **`PORTPLAN.md`** (repo root) — the authoritative implementation plan.
- **`portplan/GUIDELINES.md`** — agent guidelines (triage protocol, cadence).
- **`portplan/tasks/TASK_G*.md`** — the per-phase task files (G0–G8).

## What's archived here

| Path | What it was |
|------|-------------|
| `PORTPLAN_legacy.md` | The prior `PORTPLAN.md` ("Make All Applicable SQLite TCL Tests Green"). Systematically deferred/skipped real features; superseded by the current `PORTPLAN.md` ("Implement All Missing SQLite Features"). |
| `HANDOVER_legacy.md` | The prior `HANDOVER.md` session log. |
| `plans/` | The prior `plans/` tree: `MASTER_PLAN.md`, `GOAL_SCHEDULE.md`, `TEST_TAXONOMY.md`, `DEFERRED.md` (WAL + incorrectly-deferred engine gaps), `NOT_APPLICABLE.md` (incorrectly-skipped features), `subplans/`, `tasks/`, `tdd/`, `PACKAGES_*.txt`, and the pre-existing `archive`/`archive-old`. |
| `portplan-legacy/` | The prior per-task files (`TASK_G0_FOUNDATION.md` … `TASK_G6_TRIAGE.md`) and `G6_TRIAGE_STATUS.md`. |

## Why these were archived

The previous plan moved a large set of **real, implementable** features into
`DEFERRED.md` / `NOT_APPLICABLE.md` and skipped ~140 packages merely because they
were "slow". The corrected directive is: **implement the missing features**; the
only legitimate exclusion is where Frigolite has an equivalent-or-better
implementation or the test has no SQL surface (see `PORTPLAN.md §10`).
