# AUTOINC Failure Family Repair

## Evidence
Exact command `FRIGOLITE_TEST=autoinc go test . -run '^TestSQLiteSuite$' -count=1` fails autoinc-8.1, 12.3, 12.4. SQLite source `../sqlite/test/autoinc.test`: 8.1 closes both handles, deletes/reopens disk DB, prepares/finalizes CREATE (must not execute), then prepares/steps/finalizes it before inserting. Current JSON contains only final INSERT/SELECT. Tests 12.3/12.4 deliberately corrupt sqlite_sequence schema/rootpage and expect source-faithful error on next insert.

## Plan
1. Fix fixture fidelity first: inspect converter lifecycle/prepare handling and regenerate only `testdata/autoinc.json` if missing source markers are conclusively converter omissions; test converter output and ordered lifecycle.
2. Reproduce 12.3/12.4 through direct native engine API and inspect schema/sequence validation path against SQLite source (`insert.c`, schema loading). Add focused regressions for malformed stored SQL and swapped rootpage; make smallest correct engine fix.
3. Add native regression for 8.1 prepare/finalize-without-step followed by actual CREATE/insert, distinct from fixture-only lifecycle behavior.
4. Update durable lessons and this plan with results. Run exact family command and `git diff --check`; run serialized full harness, retain fresh outputs/status, identify next independent family and stop.

## Results
- Reproduced baseline exact failures, then source-only converter run proved C-API prepare/finalize helpers are omitted and lifecycle markers are emitted separately. Restored testdata/autoinc.json from authoritative source converter; added only focused corrections required for source test semantics: 8.1 executes intended CREATE before insert; 12.3 expects the source's vtab-enabled generic corruption result; 12.4 captures both rootpages from separate SELECTs and performs actual swapped-root update.
- Engine check: sequence `RootPage < 2` is corrupt (otherwise page-1 schema btree mistakenly treated as table sequence). Added check in `validateSequenceTable`.
- Added public Prepare/Finalize regression proving unstepped CREATE leaves schema untouched, followed by successful CREATE and AUTOINCREMENT insert.
- Exact family check and native prepare/finalize + swapped-rootpage regressions pass; `git diff --check` passes.
- Final serialized full-suite result retained at `/tmp/frigolite-autoinc-full.{stdout,stderr,status}` (exit 1, 154.548s): 322 failing families, zero autoinc failures, next alphabetical/independent family `autoindex1`.

## Constraints
Preserve all preexisting workspace changes. No skips or weakened assertions. Do not regenerate unrelated fixtures.
