# analyzeC Unordered Index Planning Repair

## Evidence

`portplan/FULL_SUITE_AFTER_ANALYZE.*` first run showed analyzeC planner failures. Source `../sqlite/test/analyzeC.test:59-81` updates `sqlite_stat1.stat` with `unordered`; `analyzeC-2.1/2.3` require unordered index excluded from range and ORDER BY, while `2.3x` requires temporary ORDER BY b-tree. Current `internal/execquery` planner reads `sqlite_stat1` for row count and `sz=`, but does not parse `unordered`; `bestIndexForRowCount` admits every index ref and `findBestCoveringIndex` likewise selects unordered indexes.

## Steps

1. Add native regression pinning plans after marking t1a unordered, covering range-search rejection and ORDER BY index rejection.
2. Trace index eligibility/filter paths, add SQLite-compatible stat1 unordered lookup shared by seek and covering-index planning.
3. Run native regression RED then green; run exact analyzeC family.
4. Run full suite with retained evidence and take next remaining failure.