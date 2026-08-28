# P6.VTAB zipfile/zipfile2 takeover status — COMPLETED

## Outcome (session close)
Required verify passes on clean tree at commits 12ac4c208 + 6567be569:
zipfile and zipfile2 generated suites fully green; execdml / vtab / tcl2go
packages green; full `go test ./...` green; SOLID checks pass.

Remaining next VTAB module can start fresh from this state. See
`.agents/lessons_learned.md` § P6.VTAB zipfile/zipfile2 session for the
engine/transpiler discoveries (ValueModule routing rule, zipfileMtime
Julian port, strict CD/LFH handling, zeroblob ceilings, aggregate pending
error threading, tcl2go emitters: string first/findall/binary hex/db one/
make_corrupt_file, sqlLiteral X'hex' binary binding).

Historical context below is superseded but retained for reference.

## Objective
Continue P6.VTAB execution for zipfile/zipfile2. Preserve conflict-aware virtual-table routing and binary-literal handling. Reproduce every remaining generated failure with native Go tests + SQLite oracle, fix engine/transpiler faithfully, validate, then continue remaining VTAB modules.

## Completion criterion / required verify
Current zipfile/zipfile2 failures reduced or resolved, focused tests and oracle evidence documented, no internal regressions, committed workspace ready for next VTAB module.

Required command:
```sh
go test ./internal/execdml ./internal/vtab ./tools/tcl2go && go test -tags testgen ./testgen/zipfile/ ./testgen/zipfile2/ -count=1 -timeout 300s
```

Goal was auto-blocked after three verification failures. Do not claim completion until required command passes or criterion/verify is explicitly revised.

## Repository state
- Working tree clean at handover.
- Latest commits:
  - `0880229af` — created-vtab join materialization uses module column schema and alias-qualified row maps; preserves conflict-aware routing.
  - `30ee8ea54` — generated scalar variable expressions for channel writes.
  - `7f0a20c95` — malformed zip blob reports archive corruption.
  - `83e202e48` — control-byte zip arguments classified as archive blobs.
  - `e60b2242e` — `ZipfileModule` implements `ValueModule` `ConnectWithValues`/`CreateWithValues`, preserving binary BLOB arguments.
- Full `go test ./...` passes after latest commits.
- Focused package tests pass: `./internal/execdml`, `./internal/vtab`, `./tools/tcl2go`.
- `git diff --check` clean.
- Quality script reports many pre-existing complexity/staticcheck/file-size findings; pre-commit build + SOLID checks pass. Do not attempt unrelated cleanup.

## Current known generated failures
Latest required verify still fails:
1. `testgen/zipfile` transaction/expected behavior failure (latest output reported failure around INSERT transaction sequence; rerun to get exact line and first failure).
2. zipfile remaining parity cases (historically cases around aggregate/nested archive and crafted archive):
   - nested `rt(zipfile(...))` path previously failed with `cannot open file: `; native direct repro now succeeds when function returns []byte.
   - crafted archive case expected `zip archive is corrupt`, previously got `cannot open file: `; `ValueModule` and control-byte blob classification were added, but generated SQL argument path still needs verification.
   - large `zeroblob(1e9/1.2e9)` expects `out of memory`, engine currently reports `string or blob too big` due eager blob allocation.
   - crafted timestamp/archive expected `A 33188 312768000 ...`, engine previously produced malformed payload/name output; local-header UT timestamp parsing added but generated case still needs rerun.
3. `testgen/zipfile2` previously failed to compile at `zipfile2_test.go:416`: generated `tclChannelAppend(test.zip, ...)` (invalid Go selector). Running `go run ./tools/tcl2go/` regenerates it as `tclChannelAppend("test.zip", ...)`, but generated files are normally restored/reverted and the required verify does not run generator. Fix must be in transpiler and/or repository generation policy; verify exact tracked behavior before editing generated tests.

## Important native discoveries
- Native repro for created zip table alias:
  `SELECT a.name,a.data FROM zz a` and self-join now return expected rows after module-schema row-map fix.
- Native nested blob repro with `rt` returning its []byte argument succeeds and returns `a.txt`, mtime, data.
- `zipfile` is aggregate for multi-row source; `ZipAgg` implementation already landed earlier.
- `zipfile` argument type must remain []byte; stringifying binary BLOB produces corrupted UTF-8 text and path-vs-blob misclassification.
- SQLite oracle is ground truth. Use `/usr/bin/sqlite3` where available and compare exact result/error.
- Generated suite can mutate fixture files; run in temp dirs only.

## Relevant code
- `internal/vtab/zipfile.go`: module, parser, aggregate, serialization, `ValueModule` methods, `looksLikeFilePath`, `zipParseEntries`, `zipUTTimestamp`.
- `internal/execdml/vtab_update.go`: optional `ConflictAwareUpdater` routing for INSERT/UPDATE conflict policies.
- `internal/execquery/select_join.go`: created-vtab join materialization through `MaterializeCreatedVTab`; must retain this path.
- `internal/execquery/select.go`: created-vtab single-table path guarded with `len(s.Joins)==0`; necessary to avoid bypassing join executor.
- `internal/execquery/select_materialize.go`: alias qualification for materialized sources.
- `tools/tcl2go/processset.go`: channel destination expression handling and invalid variable guard.
- `tools/tcl2go/processvars.go`: `varValueExpr` now resolves simple `$scalar` references to Go identifiers; committed `30ee8ea54`.
- `testgen/zipfile2/zipfile2_test.go`: generated invalid channel selector in committed baseline; do not manually edit generated output unless plan explicitly says generated fixture update.

## Ordered next steps
1. Run required verify once; capture first failing test only. If zipfile2 compile failure appears, run generator, inspect diff, trace generator source, then decide whether generated files are expected committed artifacts.
2. Reproduce first failing generated SQL in a small native Go test under `/tmp` or a temporary test file (not generated wrapper). Compare SQLite oracle.
3. Fix earliest engine/transpiler root cause; add focused native regression test where practical.
4. Rerun focused packages, then generated package(s), restoring generated files if generator output is not meant to be committed.
5. Address remaining failures in order: transaction INSERT semantics; crafted archive error classification; timestamp/local-header parsing; OOM mapping/lazy zeroblob only if still red.
6. Run full `go test ./...`, required verify, and quality checks. Update `.agents/lessons_learned.md` with generalizable discoveries. Commit each coherent fix.
7. Only after verify passes, mark goal complete; otherwise keep active or report concrete blocker.

## Cautions
- No try/fail loop. Bisect first failing checkpoint and consult SQLite source/oracle.
- Do not weaken functionality or change expected tests to make them pass.
- Do not remove current conflict routing or binary value changes.
- Generated test files were repeatedly regenerated during debugging; always `git status` and restore unintended generated diffs before commit.
