# TASK G6.TRIAGE — Sweep remaining applicable testgen packages

> **Phase**: G6 (finalization).
> **Goal**: G6.TRIAGE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1–G5 substantially complete.

## Objective
After the feature tasks (G1–G5) land, ~353 applicable packages minus those
covered leaves a **long tail**: `ticket*` regression tests, `tcl-*`, `misc*`,
`rowvalue`, `descidx`, `coveridxscan`, `skipscan`, `seekscan`, `expridx`,
`numindex`, `bloom`, `in`, `transitive`, `selectI`–`selectN`, `whereO`+, etc.
This task sweeps them in priority batches, triaging each failure (engine vs
transpiler vs N-A) and fixing the engine bugs. It is the task that drives the
overall pass-rate to target.

## Approach (batch, not all-at-once)
Run testgen packages in **batches** (e.g. 15–25 at a time), capture PASS/FAIL,
then for each FAIL apply the triage protocol. Never run all 614 at once (slow +
noisy). Use this one-liner to get a quick PASS/FAIL map for a batch:
```bash
for p in <batch>; do r=$(go test -tags testgen -count=1 -timeout 60s ./testgen/$p/ >/dev/null 2>&1 && echo PASS || echo FAIL); printf "%-20s %s\n" "$p" "$r"; done
```

## Suggested batches (order by feature value)
1. **Remaining WHERE/SELECT family:** `selectI` `selectJ` `selectK` `selectL`
   `selectM` `selectN` `whereO` `whereP` ... `in` `filter` `tkt-` ...
2. **Row value + index variants:** `rowvalue` `rowvaluevtab` `descidx`
   `coveridxscan` `skipscan` `seekscan` `expridx` `numindex` `bloom`.
3. **Ticket/regression tests:** all `ticket*` / `tkt-*` packages (these encode
   specific bug fixes — high signal, often small engine bugs).
4. **Misc SQL:** `misc*` `transitive` `cse` `gencol*` `hexlit` `id*` `indexedby`.
5. **Remaining schema:** `schema*` `savepoint*` `autovacuum` (rollback-only
   parts) `vacuum` `reindex` `collate*` leftovers.

## Per-package loop
- [ ] Run the batch; list FAIL packages.
- [ ] For each FAIL: write a **pure-Go test** isolating the feature (per
      GUIDELINES §3). Compare to `sqlite3`.
- [ ] If engine bug → fix (smallest diff); re-run the package + the relevant
      earlier-phase verify command (catch regressions).
- [ ] If transpiler bug → fix `tools/tcl2go/`, regenerate, review blast radius.
- [ ] If genuinely N-A → add to `plans/NOT_APPLICABLE.md` + harness map with a
      one-line reason and evidence.
- [ ] Commit per logical fix: `G6.TRIAGE.<batch>.<n>: <pkg> <summary>`.

## Verify command (final applicable sweep — run once at the end)
```bash
go test -tags testgen -count=1 -timeout 300s ./testgen/... 2>&1 | grep -E "^(ok|FAIL|---)" | sort | uniq -c
```
(Summary only; investigate FAIL packages individually.)

## Definition of done for this task
All ~353 applicable packages either PASS or are documented N-A with evidence.
No applicable package is FAILING. The deferred (WAL) set remains documented in
`plans/DEFERRED.md`.

## Goal create command
```
goal create \
  objective "Sweep all remaining applicable testgen packages (ticket*, misc*, rowvalue, descidx/skipscan/coveridxscan/expridx/numindex/bloom, remaining select*/where*, schema leftovers) in batches: triage each failure (engine vs transpiler vs N-A) via pure-Go test + sqlite3 oracle, fix engine bugs, document N-A with evidence. Drive overall pass rate to target. See portplan/TASK_G6_TRIAGE.md." \
  completionCriterion "All ~353 applicable packages PASS or are documented N-A with evidence; no applicable package FAILING." \
  verifyCommand "go test -tags testgen -count=1 -timeout 300s ./testgen/... 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && echo ALL_APPLICABLE_GREEN" \
  freshContext true
```

## Handover note (template)
```
State: G6.TRIAGE (long-tail sweep). Method: batch PASS/FAIL map → triage per package → engine fix / transpiler fix / N-A.
Every N-A needs evidence in plans/NOT_APPLICABLE.md + harness map.
Decisions: batches of 15-25; never all 614 at once; re-run earlier-phase verify after shared-code fixes.
Next: run batch 1 (selectI-N/whereO+/in), triage, fix, repeat through the 5 batches.
Risks: ticket* tests encode subtle bugs — fix root cause not symptom; long tail hides regressions — re-sweep earlier phases periodically.
Carried limits: verifyCommand above (final sweep); 300s timeout.
```
