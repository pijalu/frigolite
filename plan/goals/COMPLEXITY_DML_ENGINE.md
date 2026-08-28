# Complexity remediation — DML, engine, expressions, FK

## Objective
Remove non-test complexity findings in `internal/execdml`, `internal/exec`, `internal/execexpr`, and `internal/execconstraint` while preserving mutation, transaction, trigger, rollback, expression, and deferred-FK behavior.

## Steps
Inventory and call graph; inspect behavior invariants; extract helpers by responsibility; test affected packages and regression paths after each slice; run all gates.

## Verify
`.agents/skills/golang-check/golang-check.sh && go test ./internal/execdml/... ./internal/exec/... ./internal/execexpr/... ./internal/execconstraint/... && go test ./... && go test -run TestSOLID_ ./...`
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
