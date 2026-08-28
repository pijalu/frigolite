# Complexity remediation — ExecDDL

## Objective
Remove all non-test `gocognit >15` and `gocyclo >12` findings in `internal/execddl` by focused helper extraction, preserving SQLite/FTS behavior.

## Scope
FTS merge/flush/integrity/rebuild, virtual-table paths, trigger validation, ALTER/dependency/schema operations. Highest baseline findings: `MergeFTS` cognitive 212/cyclomatic 88.

## Steps
1. Inventory current findings and callers.
2. Trace FTS invariants and relevant SQLite source before each behavior-sensitive edit.
3. Extract cohesive helpers with narrow inputs/outputs; retain error and transaction ordering.
4. Run `go test ./internal/execddl` and targeted integration tests after each slice.
5. Run complexity checks, `go test ./...`, and SOLID checks.

## Verify
`.agents/skills/golang-check/golang-check.sh && go test ./internal/execddl/... && go test ./... && go test -run TestSOLID_ ./...`

## Constraints
No `nolint`, threshold changes, generated edits, semantic simplification, or speculative edits.
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
