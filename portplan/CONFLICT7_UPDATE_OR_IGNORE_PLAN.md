# conflict-7 fixture setup fidelity plan

## Evidence
- Clean `go test ./... -count=1` latest `/tmp/frigolite-current-full-suite2.log` exits 1. Isolated conflict family reports 7.3–7.7 data mismatches, 9/13 transaction cascades, plus many later setup cascades.
- Pure-Go regression of UPDATE OR IGNORE over 50 unique rows passes, ruling out engine bug for 7.3/7.4.
- Authoritative `../sqlite/test/conflict.test:341-354` test-7.1 creates table then loops 50 times inserting `(i,i+1)`. Stale JSON omitted every loop insert; later conflict data and unrelated transaction tests cascaded.
- Fresh conversion into `/tmp/conflict-generated` emits 50 ordered insert steps and retains all expected values. Regenerated only `conflict`, `conflict2`, and `conflict3` fixtures from current Tcl sources; those two sibling fixtures also encoded stale test order, causing setup collisions.

## Verified
- `FRIGOLITE_TEST=conflict go test . -run '^TestSQLiteSuite$' -count=1`: PASS.
- Equivalent `conflict2` and `conflict3` commands: PASS.
- Native 50-row `TestConflict7UpdateIgnorePreservesOneNonConflictingRow`: PASS.
- `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1`: PASS.
- `git diff --check`: PASS.
- Next action: full serialized `go test ./... -count=1`; previous full log predates fixture regeneration and cannot be used for new failure attribution.
