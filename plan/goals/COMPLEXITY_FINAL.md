# Complexity remediation — final closure

## Objective
Close every remaining non-test golang-check finding, including newly exposed complexity and file-size issues, then prove repository gates green.

## Steps
1. Run full golang-check and classify every remaining finding.
2. Fix each root cause in package order; no suppression or threshold changes.
3. Run build, vet, staticcheck, SOLID, full tests, race tests, and callback/interrupt regressions.
4. Record evidence and update status/plan files.

## Verify
`.agents/skills/golang-check/golang-check.sh && go build ./... && go vet ./... && go test -run TestSOLID_ ./... && go test ./... && go test -race -count=1 -run '^Test[^C]' ./... && go test -tags testgen -count=1 ./testgen/dbstatus ./testgen/dbstatus2 ./testgen/hook ./testgen/hook2 ./testgen/interrupt ./testgen/interrupt2`
## Mandatory per-task quality gate

Before task completion, run `tools/quality_gate.sh <changed-production-go-files>` and require success, passing only newly added or materially changed production non-test files. This gate enforces complexity and file-size limits for new/changed code and staticcheck diagnostics attributable to task changes. Findings in untouched legacy code are deferred exclusively to final plan closure; task changes must introduce zero new findings. Record command and output here.
