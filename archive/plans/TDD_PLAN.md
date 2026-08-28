# Frigolite — TDD Plan

## Approach

TDD across 6 tiers: each tier handles a set of TCL test files by feature area.
Start with core SQL (CREATE/INSERT/SELECT/DELETE/UPDATE), progress to
advanced features (FTS, VACUUM, ANALYZE) and edge cases.

Each tier has numbered tasks. Within each tier, the rule is:
1. Run the generated test → see it fail (RED)
2. Analyze the first failure pattern
3. Fix the frigolite engine (smallest change)
4. Re-run → see it pass (GREEN)
5. Commit

No silent skipping. Every untranspiled TCL command produces a visible
`t.Errorf("TODO: ...")` marker in the generated Go code.

## Tiers

| Tier | Area | Files | TODOs | Plan |
|------|------|-------|-------|------|
| 1 | Core SQL (CREATE/INSERT/SELECT/DELETE/UPDATE) | ~50 | ~800 | [TIER_1_CORE_SQL.md](tdd/TIER_1_CORE_SQL.md) |
| 2 | SQL Features (JOINs, subqueries, ORDER BY, etc.) | ~80 | ~2000 | [TIER_2_SQL_FEATURES.md](tdd/TIER_2_SQL_FEATURES.md) |
| 3 | Functions (string, numeric, date, aggregate, window) | ~60 | ~2500 | [TIER_3_FUNCTIONS.md](tdd/TIER_3_FUNCTIONS.md) |
| 4 | Schema & Constraints (ALTER, FK, CHECK, etc.) | ~40 | ~500 | [TIER_4_SCHEMA.md](tdd/TIER_4_SCHEMA.md) |
| 5 | Advanced (FTS, vtab, ATTACH, ANALYZE, VACUUM) | ~100 | ~4000 | [TIER_5_ADVANCED.md](tdd/TIER_5_ADVANCED.md) |
| 6 | Edge Cases & Infrastructure | ~400 | ~4700 | [TIER_6_EDGE_CASES.md](tdd/TIER_6_EDGE_CASES.md) |
| **Total** | | **~730** | **~14,500** | |

## How To Work

```bash
# Generate all test files
go run ./tools/tcl2go/

# Run all generated tests to see current state
go test ./testgen/... -count=1

# Focus on a single test file
go test ./testgen/select1/... -v -count=1

# Check architecture
go build ./...
go test -run TestSOLID_ -count=1
```

## Protocol

1. **Pick the first file** in the current tier with the fewest TODOs
2. **Reproduce** — `go test ./testgen/<file>/... -v -count=1` — capture output
3. **Analyze** — find the root cause in the engine
4. **Fix** — smallest change that resolves the root cause
5. **Verify** — the file passes, no regressions
6. **SOLID** — `go test -run TestSOLID_ -count=1`
7. **Commit** — `P<tier>.<task>: <description>`
8. **Update** progress in the tier file's session notes
