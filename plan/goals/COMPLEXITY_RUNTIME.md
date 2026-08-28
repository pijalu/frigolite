# Complexity remediation — parser, schema, runtime, functions, public API

## Objective
Remove non-test complexity findings in parser/schema/storage/SQL/value/function and root public API/backup/blob code, preserving grammar, file compatibility, and API behavior.

## Scope
`internal/parse`, `schema`, `storage`, `sql`, `value`, `function`, root `frigolite*.go`, `stmt.go`.

## Steps
Inventory; consult SQLite source for parser/file-format semantics; extract focused helpers; run parser/storage/function/API tests; run all gates.

## Verify
`.agents/skills/golang-check/golang-check.sh && go test ./internal/parse/... ./internal/schema/... ./internal/storage/... ./internal/sql/... ./internal/value/... ./internal/function/... ./... && go test -run TestSOLID_ ./...`
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
