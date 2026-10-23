# Remaining `altertab` verifier failures — focused plan

## Evidence and scope

Baseline command `FRIGOLITE_TEST=altertab go test . -run '^TestSQLiteSuite$' -count=1` reports ten failing assertions: altertab-21.1, 21.3, 23.1–23.4, 24.2.1, 25.2, 32.0, 33.1, 33.2. Existing altertab2/3 harness skips are unrelated and must remain unchanged; no new skips or expectation edits. Isolated attempts confirmed 21.1, 23.1 and 24.2.1 fail independently, but harness subtest filtering omits preceding ordered setup; reproduce each dependent case with its setup in native Go.

## Ordered approach

1. Inspect each corresponding block in `../sqlite/test/altertab.test`, generated `testdata/altertab.json`, setup ordering and executor paths; probe SQLite CLI oracle for each exact behavior when available.
2. Add focused native `frigolite.Open/Exec/Query` regressions for engine-visible behaviors, including prerequisite schema/collation/trigger setup; run them before implementation to establish engine-vs-fixture verdict.
3. Fix minimal underlying engine behavior, grouped by causal subsystem: expression validation (21), ALTER TABLE/view and collation restoration (23, 24, 25), trigger reparse/column rename (32, 33). Inspect appropriate SQLite source (`src/alter.c`, resolver/expression implementation) before each group.
4. Run each native regression and the original targeted harness family; preserve fixture expectations and skip maps.
5. Run required verifier exactly: `git diff --check && go test ./internal/vtab ./internal/fts ./internal/execddl ./internal/execdml ./internal/exec -count=1 && FRIGOLITE_TEST=altertab go test . -run '^TestSQLiteSuite$' -count=1`.

## Files likely involved

- Tests: new focused top-level `frigolite_altertab_*_test.go` (one per coherent behavior group).
- Production: `internal/execddl/alter_rename.go`, related DDL/trigger validation code, and expression validation only where pure-Go repro confirms defect.
- Fixtures/skip policy: no intended changes. `testdata/altertab.json` and `frigolite_harness_test.go` are read-only for this work unless a demonstrated fixture-conversion bug requires correction.

## Verification / completion

Every case must pass without skipped/altered expectations. Run regressions first, package command and exact verifier last; inspect `git diff --check` and changed paths to avoid disturbing pre-existing work.
