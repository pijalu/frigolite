# Plan — S52 slice 1: expridx1 imposter fixture + engine diagnostics (2026-11-24)

Goal: close the 10 `expridx1` leaves. Baseline: `049a2a068` → current HEAD
`7d18a7ab5`, focused log `/tmp/expridx1_t11_base.log` (10 leaves + parent).

## Findings

The test source is `../frigolite/ori/sqlite/test/expridx1.test` (not the
`../sqlite` tree). Each group calls
`sqlite3_test_control(SQLITE_TESTCTRL_IMPOSTER, db, main, 1, $root)` and then
creates a WITHOUT ROWID table `x1`; in C, `x1`'s schema rootpage is the index
root, so DML on `x1` deliberately distorts the index b-tree. The converter
currently drops the test-control call, so `x1` is a normal table and the
distortion never happens: integrity_check says `ok` and NOT-INDEXED probes
find no missing rows.

A direct engine experiment reproduces the imposter contract with existing
SQL: create `x1` normally, then under `PRAGMA writable_schema=ON` set
`sqlite_schema.rootpage=$root` for `x1`, then update `x1`. The index entry
disappears and `PRAGMA integrity_check` already reports
`row 3 missing from index i1` (verified before any code change). So the first
slice is fixture/converter fidelity, not an engine rewrite.

## Changes

1. Converter (`tools/tclconvert/tcl/interp_cmds.go`,
   `interp_sql.go`): recognize
   `sqlite3_test_control SQLITE_TESTCTRL_IMPOSTER db main 1 $root`, remember
   the root expression and table name from the next `CREATE TABLE`, and emit
   a rootpage-rewrite exec step after that CREATE:
   `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET rootpage=$root WHERE
   name='x1'; PRAGMA writable_schema=OFF;`.
   Add a converter unit test pinning the emitted shape.
2. Fixture (`testdata/expridx1.json`): add the same rootpage rewrite step to
   the four imposter CREATE TABLE steps (setup_0..setup_3). Do not regenerate
   the whole file — the generator currently differs in unrelated scaffolding
   (setup_0 source directive and `resultAll`), which would pollute the
   durable baseline.
3. Re-run focused `expridx1`. Classify the remaining leaves:
   - missing-row diagnostics / NOT-INDEXED existence probes should go green;
   - imprecise-float diagnostics (4.3, 4.6) may still need engine support in
     `internal/exec/pragma_integrity_index.go` (compare recomputed expression
     value against the stored index key under C's `sqlite3ExprAffinity`/
     floating-point precision rule) — implement only if the focused run
     shows it is still missing.

## Progress (2026-11-24, final slice state)

All 10 expridx1 leaves now pass the focused harness gate
(`/tmp/expridx1_t11_wr.log`: no failures). The worktree fixes, in dependency
order:

- Fixture/converter imposter rootpage rewrite; converter unit test
  `TestSQLite3ImposterControlEmitsRootpageRewrite` passes (full converter
  package still has the same 7 pre-existing failures as HEAD, verified in a
  clean `/tmp/frigolite_head` worktree).
- De-wrap CREATE INDEX expression keys in `execdml.ParseIndexColumns`, so
  `(b+0.0)` and the WHERE term `b+0.0` agree in the executor seek plan.
- Correlated expression bounds evaluate against the outer row in both the
  planner (`findExprAndConstExpr`) and executor (`indexSeekExpressionTerm`);
  an outer `o.c` is no longer misread as the table column side.
- WITHOUT ROWID secondary index seeks use key columns plus the PK tail as a
  covering loop (`indexSeekColumns`), with unary `+col` keys treated as
  covering `col`. Non-covering WR seeks fall back to the regular scan.
- OP_IdxDelete's stale-key fallback removes the index entry by its rowid / PK
  tail when the recomputed key is absent (`DeleteIndexEntryByTail`), matching
  C's cursor-positioned delete on imposter-distorted keys. The earlier count
  suppression was removed once this root fix landed.
- integrity_check reports 1–2 ULP key drift as an imprecise floating-point
  value; ULPs are counted in the drift's direction (the step below a power of
  two is half the step above it), so exactly 3 ULPs remains "row missing".
- Pure-Go pins `TestExpridx1EnginePins` cover the imposter rewrite,
  correlated expression seek, WR covering seek, and imprecise diagnostics.

Neighbour gates: `e_reindex`, `indexexpr2`, `without_rowid1`, `delete4`
clean; focused `collate1`, `conflict`, `pager1`, `pagerfault`, `vacuum-into`
raw-byte fail sets identical to clean HEAD. Final full gate
`/tmp/t11_full4.log` (1150.308s): 1339 leaves; raw-byte diff vs the durable
`/tmp/t10_full2.log` (1351): **NEW=0**, GONE=12 (all 11 `expridx1` leaves
plus the `temptable2-4.1.1` randomblob flake). vs clean HEAD the only NEW is
`vacuum-into-500`; focused runs give current and HEAD the identical 10-leaf
set, and the durable baseline already fails it — a full-gate order flake.
`/tmp/t11_full4.log` is leaf-identical to the pre-refactor
`/tmp/t11_full3.log`.

## Progress (2026-11-24, mid-slice)

Fixture rootpage rewrite + imposter integrity skip landed in the worktree.
Focused log `/tmp/expridx1_t11_seek.log`: 11 leaf failures remain
(`1.1.3`, `1.2.1`, `1.2.4`, `1.3.1`, `1.3.4`, `2.3`, `4.3`–`4.6`).
Two more root causes are now pinned:

- `execdml.ParseIndexColumns` kept CREATE INDEX's mandatory outer parentheses
  (`(b+0.0)`), while WHERE term rendering gives `b+0.0`. `indexSeekPlanStep`
  therefore declined the expression seek even though the planner chose `i1`.
  Fixed by dewrapping balanced outer parentheses in `indexKeyColName`;
  pure-Go scratch now returns `20` for the correlated 1.1.1c probe and `[]`
  for a stale-key seek. EQP still prints SCAN for that correlated subquery
  because `refsForBestIndex` does not render expression refs; not a harness
  assertion in this test.
- Deleting a t1 row whose index key was distorted by the imposter leaves the
  stale entry (our OP_IdxDelete equivalent requires exact key equality),
  so integrity_check emits `wrong # of entries in index ...` where C is
  silent. The count comparison is now suppressed only for an index whose
  root is also a table root (engine-visible imposter mark), but that hides a
  real maintenance gap: the Oracle shows C removes the distorted entry.
  Next: add a btree locator-tail delete fallback used only when the exact
  OP_IdxDelete target is absent, then drop the count suppression again.
- 4.3/4.6 need the newer imprecise-float diagnostic: on a missed presence
  probe, inspect the neighbouring index entry with the same rowid/PK tail;
  a numeric key within two float64 ULPs reports `index <name> stores an
  imprecise floating-point value for row <ordinal>`, otherwise the existing
  missing-row message stays. Three ULPs is missing.
- 2.3 needs expression-index seek on a WITHOUT ROWID table (`+c` key over
  PK(a,b)); `selectIndexSeekPlanFor` currently excludes every WITHOUT ROWID
  table. Extend it with a covered secondary-index path whose tail supplies
  the PK columns, mirroring C's covering index loop.

## Verification

- Focused `FRIGOLITE_TEST=expridx1` green (`/tmp/t11_refactor_expridx1.log`);
  pins and converter imposter unit green.
- `go test ./tools/tclconvert/... -run TestSQLite3` green for the new unit;
  the 7 remaining package failures reproduce at clean HEAD.
- Neighbour families `e_reindex`, `indexexpr2`, `without_rowid1`, `delete4`,
  `collate1`, `conflict`, `pager1`, `pagerfault`, `vacuum-into`: raw-byte
  leaf sets equal to clean HEAD.
- Final full gate `/tmp/t11_full4.log` (1150.308s, 1339 leaves): raw-byte
  diff vs `/tmp/t10_full2.log` (1351) = NEW=0, GONE=12 (11 expridx1 leaves
  + documented `temptable2-4.1.1` flake); vs `/tmp/s50_full3.log` (1383) =
  NEW=0, GONE=44; vs clean HEAD the only NEW is the documented
  `vacuum-into-500` order flake (focused sets identical).
- Complexity: no new gocognit/gocyclo findings over the clean-HEAD baseline
  in touched files (new helpers extracted); staticcheck and hard file-size
  findings are pre-existing. Code commit: `0fec78348`.
