# Complexity remediation — tooling and CLI

## Objective
Remove non-test complexity findings in `tools/tcl2go`, `tools/tclconvert`, `cmd/frigolite`, and `frigodb` while preserving transpiler and CLI output.

## Steps
Inventory findings; split command handlers and conversion workflows by responsibility; regenerate outputs; compare builds/tests; run all analyzers and Go/SOLID gates.

## Constraints
Do not edit generated fixtures or weaken transpiler assertions.

## Verify
`.agents/skills/golang-check/golang-check.sh && go run ./tools/tcl2go/ && go test ./tools/... ./cmd/... ./frigodb/... && go test ./...`
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
