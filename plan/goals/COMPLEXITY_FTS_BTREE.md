# Complexity remediation — FTS and B-tree

## Objective
Remove non-test complexity findings in `internal/fts` and `internal/btree` while preserving on-disk formats, cursor invariants, indexing, tokenization, and query behavior.

## Scope
Tokenizers, query parsers, doclists, segment readers/writers, integrity/snippet paths, page insert/split/delete algorithms.

## Steps
Inventory; inspect SQLite algorithms/source; extract focused helpers; run package tests after each slice; run full analyzers, tests, and SOLID gates.

## Verify
`.agents/skills/golang-check/golang-check.sh && go test ./internal/fts/... ./internal/btree/... && go test ./... && go test -run TestSOLID_ ./...`
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
