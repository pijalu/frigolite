# Complexity remediation — ExecQuery

## Objective
Remove all non-test complexity findings in `internal/execquery` via focused SELECT, join, aggregate, CTE, and window helpers without changing SQL semantics.

## Steps
1. Inventory findings and call graph.
2. Review NULL, collation, ordering, frame-bound, aggregate, and CTE invariants.
3. Extract cohesive helpers; preserve evaluation order and errors.
4. Run package and focused query/window tests after each slice.
5. Run full complexity, Go, and SOLID gates.

## Verify
`.agents/skills/golang-check/golang-check.sh && go test ./internal/execquery/... && go test ./... && go test -run TestSOLID_ ./...`

## Constraints
No lint suppression, threshold changes, generated edits, or behavior weakening.
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
