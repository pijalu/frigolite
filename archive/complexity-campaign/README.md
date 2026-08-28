# Archived: Complexity Campaign (Phase 1)

These files are superseded by `portplan/tasks/TASK_COMPLEXITY_REFACTOR.md`
(the consolidated master plan). They are kept for historical context.

## Files

- **PLAN_QUALITY_GATE_RESTRUCTURE.md** — one-time plan for the quality-gate
  infra fix (scripted gate, pre-commit hook change, plan-before-change
  guideline). **DONE** (committed e783d997d). All decisions folded into the
  current plan.

- **GOAL-CHECKPOINT-G0-FIX-4-FAILS.md** — checkpoint for the monolithic
  complexity goal. Contains valuable gotchas (handleRule case-splitting,
  evalColumnRef qualified ref, execInsertRow tuple expr, regression net).
  Superseded by the micro-task approach.

- **TASK_G0_SPLIT.md** — the G0.5 god-file split task. Superseded: god-file
  splits are now integrated into the CX-NN micro-tasks (Track 1 of the
  consolidated plan), which split files by concern while reducing complexity.

## Why superseded

The monolithic approach (one goal per whole file) caused context-explosion
loops. The new plan uses micro-task splitting (5-15 functions per goal) with
an anti-loop protocol (skip-and-note) and mandatory checkpoints.
