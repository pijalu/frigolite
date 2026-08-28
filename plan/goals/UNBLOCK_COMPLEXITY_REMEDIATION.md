# Legacy complexity remediation

## Objective
Remediate all legacy production cyclomatic and cognitive complexity findings while preserving behavior.

## Scope
- Production non-test Go files only.
- Exclude `*_test.go`, generated `testgen/`, vendored code, and `third_party/` from complexity and line-size checks.
- Fix root causes by extracting focused helpers; no threshold changes, generated-file edits, or lint suppressions.

## Ordered work
1. Inventory current `gocyclo -over 12` and `gocognit -over 15` findings.
2. Group by package and refactor one coherent package slice at a time.
3. Run affected package tests after each slice.
4. Run build, vet, staticcheck, and both complexity checks.
5. Run race, SOLID, callback suites, and regression tests.
6. Update PORTPLAN and remediation evidence.

## Completion
All production gocyclo/gocognit findings resolved; build, vet, staticcheck, hard file-size, race, SOLID, callback, and regression gates pass.

## Verify
`bash .agents/skills/golang-check/golang-check.sh && go build ./... && go vet ./... && go test -run TestSOLID_ ./...`

## Baseline inventory (reviewed 2026-08-21)

Command: `.agents/skills/golang-check/golang-check.sh`

- `gocognit -over 15`: 256 production-function findings. Package counts: `execquery` 57, `execddl` 44, `fts` 39, `tools/tcl2go` 38, `execdml` 24, `exec` 14, `parse` 7, `function` 7, `frigolite` 6, `execexpr` 6, `btree` 6, `schema` 3, `execconstraint` 3, `sql` 1.
- `gocyclo -over 12`: 214 production-function findings. Package counts: `execquery` 44, `tools/tcl2go` 43, `execddl` 35, `fts` 25, `execdml` 17, `exec` 12, `frigolite` 8, `execexpr` 7, `function` 7, `btree` 6, `parse` 4, `schema` 3, `sql` 1, `storage` 1.
- `staticcheck`: no findings in baseline.
- File-size checker: 132 soft-limit findings, zero hard-limit findings. Soft-limit cleanup follows complexity batches and must not alter generated fixtures or excluded third-party code.

## Remediation batches and exact order

1. **ExecDDL** — `internal/execddl`: split FTS merge/flush/integrity/rebuild paths, virtual-table and trigger validation, ALTER/dependency and schema operations. Highest cognitive finding: `MergeFTS` 212; highest cyclomatic: `MergeFTS` 88. Preserve FTS shadow-table and transaction invariants.
2. **ExecQuery** — `internal/execquery`: split window frame/value/range logic, aggregate and expression walkers, SELECT/CTE orchestration, join planning/validation, and result finalization. Keep NULL, collation, ordering, and window-frame semantics unchanged.
3. **FTS/B-tree** — `internal/fts`, `internal/btree`: split tokenization/query parsing, doclist/segment readers and writers, integrity/snippet paths, and page insert/split/delete algorithms. Preserve on-disk formats and cursor invariants.
4. **DML/engine/expression/FK** — `internal/execdml`, `internal/exec`, `internal/execexpr`, `internal/execconstraint`: split insert conflict/RETURNING/FTS paths, update/delete/transaction dispatch, expression dispatch, and FK validation/actions. Preserve rollback, trigger, deferred-check, and error behavior.
5. **Parser/schema/runtime/functions/public API** — `internal/parse`, `internal/schema`, `internal/storage`, `internal/sql`, `internal/value`, `internal/function`, root `frigolite*.go`, `stmt.go`: extract parser token/rewrite helpers, schema walkers, page parsing, lexer branches, numeric/string/extension helpers, and backup/blob/API workflows. Preserve grammar and SQLite file compatibility.
6. **Tooling** — `tools/tcl2go`, `tools/tclconvert`, `cmd/frigolite`, `frigodb`: split transpiler command handlers and remaining production CLI/tool workflows. Regenerate output and compare generated build/tests; no generated fixture edits.
7. **Final closure** — rerun all analyzers and tests, address newly exposed findings and soft/hard file-size issues, then run build, vet, staticcheck, SOLID, full tests, race tests, and targeted callback/interrupt suites.

Each batch requires: inspect function and callers; write focused helper extraction plan; edit only after plan; run affected package tests; run complexity check; run `go test ./...` and SOLID before advancing. No `nolint`, threshold changes, behavior weakening, or speculative try/fail edits.

# Final gate closure

## Objective
After complexity remediation, run all repository gates and document results.

## Verify
`bash .agents/skills/golang-check/golang-check.sh && go test ./... && go test -race -count=1 -run '^Test[^C]' ./... && go test -tags testgen -count=1 ./testgen/dbstatus ./testgen/dbstatus2 ./testgen/hook ./testgen/hook2 ./testgen/interrupt ./testgen/interrupt2`
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
