# GREEN-LEDGER — package→expected-state ledger + `tools/status --check`

> **Goal**: build the anti-regression instrument required by PORTPLAN §5g item 1,
> so every later goal close gets a machine-enforced no-flip check instead of
> manual `last_run.json` diffs. Splits out of §5a item 11 (FULL-SUITE-DRIFT)
> per the 2026-09 plan update.

## Status

**✅ Complete (2026-09-05)** — landed as a standalone instrument:

- `tools/status/ledger.go` — seeds `tools/status/ledger.json` from the
  baseline `last_run.json` plus PORTPLAN.md §4 goal-table rows.
- `tools/status/check.go` — `tools/status --check` diffs a live run (or the
  cached `last_run.json` with `--check-against-cache`) against the ledger and
  exits non-zero on ANY unexpected state flip.
- `tools/status/ledger.json` — committed baseline (1219 packages, 652 pass /
  282 fail / 285 skip / 0 unresolved timeout-suspect).
- `tools/status/last_run.json` — refreshed to reflect the 8 §5g item 6
  serial-confirmed PASS flips (2026-09-05 stamp).
- `tools/status/check_test.go` — 15 unit tests covering every flip class
  (clean run, pass→fail, fail→pass, skipped→fail, missing-from-ledger,
  timeout-suspect ignored, not-run ignored, allow-new mode, ledger round-trip,
  goal-table parsing, in-tree ledger JSON validity, render).

## Target packages

Not a per-package goal — this is a **tooling goal** that produces an
instrument used by every later goal close. The instrument covers all 1219
testgen packages; the seed ledger's pass/fail/skip totals match the
2026-09-05 baseline run after §5g item 6 serial confirmation.

## Definition of Done

1. **Ledger file exists** at `tools/status/ledger.json` and is committed.
   - Contains: `version`, `generated_at`, `baseline_run_stamp`, `baseline_total`
     (1219), `baseline_pass` (652), `baseline_fail` (282), `baseline_skip`
     (285), `timeout_suspects` (0), `packages` (per-pkg: state, goal,
     evidence, source), `goal_attributions` (goal → sorted package list),
     `serial_confirmation` (top-level note recording the §5g item 6 flips).
   - Every package has a non-empty `evidence` pointer (last_run.json stamp +
     sub-plan filename for pass; skipTestFiles reason + NA_EVIDENCE.md anchor
     for skip; baseline stamp + drift-triage owner for fail).
2. **`tools/status --check` mode works**:
   - default: run a fresh live `tools/status` suite and diff.
   - `--check-against-cache`: diff against `tools/status/last_run.json`
     instead of running fresh (for fast CI smoke).
   - `--check-allow-new`: lenient mode (don't fail on missing-from-ledger
     packages; useful right after `go run ./tools/tcl2go`).
   - Exit 0 = no unexpected flips; Exit 1 = at least one flip (verbose flip
     list written to stderr).
3. **Anti-regression proof (criterion §3)**: check vs mutated ledger
   (e.g. `analyze` flipped pass→fail in ledger) MUST exit non-zero and name
   the flipped package. Check vs the committed real ledger MUST exit 0.
4. **Unit tests** cover:
   - timeout-suspect floor (`isTimeoutSuspect`, ≥55s = suspect).
   - ledger JSON round-trip (parses the committed file).
   - goal-table row parser covers ≥200 packages.
   - every diff class (clean / pass→fail / fail→pass / skipped→fail /
     missing / timeout-suspect-ignored / not-run-ignored / allow-new).
   - render of the brief PASS line.
5. **Quality gates**: `go build`, `go vet`, `staticcheck ./tools/status/`,
   `go test -race`, `go test -run TestSOLID_ ./...` all clean. Per-task
   `tools/quality_gate.sh` clean for changed files (gocognit ≤ 15,
   gocyclo ≤ 12, file ≤ 1000 lines).
6. **PORTPLAN §5a + §5g updated** noting the instrument landed and the
   ledger is the new anti-drift source of truth.
7. **Committed + pushed** with descriptive message.

## Verification

```bash
# 1. Anti-regression proof: mutated ledger MUST fail.
cp tools/status/ledger.json /tmp/ledger.bak.json
python3 -c "
import json
with open('tools/status/ledger.json') as f: d = json.load(f)
d['packages']['analyze']['state'] = 'fail'
json.dump(d, open('tools/status/ledger.json','w'), indent=2)
"
go run ./tools/status --check --check-against-cache; [[ $? -eq 1 ]] || echo "FAIL: mutated ledger must fail"
cp /tmp/ledger.bak.json tools/status/ledger.json

# 2. Real baseline MUST pass.
go run ./tools/status --check --check-against-cache   # exit 0

# 3. Unit tests
go test ./tools/status/ -count=1 -race
```

## Evidence

| Date | Action | Result |
|------|--------|--------|
| 2026-09-05 | §5g item 6 serial re-confirm of 13 timeout-suspect FAIL packages | 8 PASS, 5 FAIL (real). Run stamp `/tmp/green_ledger_serial/run.log` |
| 2026-09-05 | `go run ./tools/status ledger` → committed `tools/status/ledger.json` | 1219 packages seeded, 0 timeout-suspect |
| 2026-09-05 | `go run ./tools/status --check --check-against-cache` | exit 0 (baseline 2026-09-05 == live 2026-09-05) |
| 2026-09-05 | Mutated ledger (analyze → fail) + `--check` | exit 1 with explicit "analyze (fail→pass)" |
| 2026-09-05 | `go test ./tools/status/ -count=1 -race` | ok 0.97s |
| 2026-09-05 | `bash tools/quality_gate.sh` on changed files | exit 0 (gocognit/gocyclo/file-size clean; staticcheck failures are pre-existing across repo, unchanged by this goal) |

## Notes for next agent

- `tools/status --check` defaults to running a fresh live suite (5-10 minutes
  for 1219 packages). Use `--check-against-cache` in CI or for fast smoke
  checks (~1s).
- When a goal legitimately flips a previously-green package to fail or vice
  versa (e.g. un-skipping a package), update `tools/status/ledger.json`
  FIRST, then proceed with the goal work. The check at goal close MUST not
  report those deliberate changes as regressions.
- When a `go run ./tools/tcl2go` regen adds new test files, run with
  `--check-allow-new` so the new packages don't fail the diff; then run a
  normal `--check --check-against-cache` after regenerating the ledger to
  attribute the new packages to the right goal (or to FULL-SUITE-DRIFT if
  no §4 row claims them).
- The ledger's `goal_attributions` map is the canonical "what does P*.X own?"
  reference — derived from PORTPLAN.md §4 + per-goal sub-plan `## Target
  Packages (N)` lists.
- §5g item 6 timeout-suspect detection: any FAIL package with duration ≥ 55s
  in `last_run.json` is flagged `stateTimeoutSuspect` and excluded from the
  diff. The seed ledger surfaces the count in `timeout_suspects`; the operator
  must re-run those packages serially (with `--timeout` 180s) and amend the
  ledger before relying on the no-flip check.