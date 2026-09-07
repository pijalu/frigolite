# FULL-SUITE-DRIFT

> **Status**: ACTIVE (goal file created 2026-09-07 per §5b; ledger instrument
> already landed — GREEN-LEDGER ✅ 2026-09-05).
>
> **Scope**: drive the red ledger (currently 247 fail / 11 timeout-suspect of
> 1219) toward zero with per-tranche oracle-verified fixes; triage the
> standard-suite (hand-written, non-testgen) drift the ledger does not gate;
> close the corrupt9/C/F/L/N residue owned here from P8.CORRUPT; un-skip the
> P8.ENCODING stragglers (uri, uri2, utf16align) per its close note.

## Verify Command

```bash
go build ./... && go vet ./... && go test -run TestSOLID_ ./... && \
  go run ./tools/status -timeout 90s && go run ./tools/status ledger && \
  go run ./tools/status --check
```

Per-tranche verify: the tranche's named packages/tests, run with -count=1.

## DoD

1. Every tranche: named failing tests green, verified against
   `/usr/bin/sqlite3` where the contract is engine-visible.
2. `tools/status --check` PASS at every checkpoint (no unexpected flips).
3. Ledger re-seeded at tranche close with the recorded run stamp.
4. Standard-suite drift: `go test ./...` failures either fixed or classified
   (engine gap / test bug / VFS-layer N-A with evidence).

## T-log

### T0 BASELINE + TRANCHES (2026-09-07)

Seed triage established during the P8.VACUUM close:

- **Standard-suite drift (NOT ledger-gated)**: `go test ./...` root-package
  failures `TestP1InsertUpsert/{do_nothing,do_update,do_update_multi-row}`,
  `TestP3FKey/{Deferred,FKeyCheck,SelfRef}`, `TestP3Trigger/Basics`.
  Symptom (upsert): `INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO NOTHING`
  reports "UNIQUE constraint failed: t.a" — the IPK-conflict →
  upsert-arbiter path is bypassed for rowid tables (findOnConflictRow
  returns no hits, or the clause match fails).
  **Bisect (2026-09-07, T1, refined)**: PASS at 0d7924066
  (P8.INCRVACUUM.phase10 meta[3] cascade fix), FAIL from d30076e42
  ("P8.INCRVACUUM: autovacuum-9.x / 2.4.5 fixes (testgen)", 2026-09-03)
  onward. d30076e42's visible diff is pending-byte scoped
  (pendingByteOverride + AllocatePage filters + testgen preamble) and looks
  orthogonal — next step: reproduce inside d30076e42's tree with a debug
  print in `findOnConflictRow` (hits count) and `clauseMatchesHit` to see
  which side breaks, and diff `uniqueIndexColumns`/`compositeUniqueGroups`
  inputs the commit may have shifted. EARLIER bisect note (WR-WRITE
  suspicion) was wrong — da88258c2 merely inherited the failure.
- **Ledger red classes** (from tools/status ledger.json, 247 fail):
  - corrupt9/C/F/L/N (5, in-scope residue from P8.CORRUPT),
  - fts3snippet/fts4opt window (per §2 DRIFT ALERT — planner-stat + pager
    suspects; fts4opt historically perf/hang),
  - func_pkg func-1.1 ("wrong number of arguments to function length()"
    at func_test.go:175),
  - without_rowid4 (UPDATE OR ABORT UNIQUE conflict error missing),
  - the select1/insert/where long-tail class (pre-squash drift).
- **Un-skip queue**: uri, uri2, utf16align (P8.ENCODING close note);
  pendingrace-class re-checks.

### T1 RESULT (2026-09-07)

Root cause found: the rowid-alias storage convention (an INTEGER PRIMARY KEY
column is stored NULL in the record; its value is the rowid — see
NullIPKAliasForWrite) was not honored by four row-reading seams, so any
feature comparing raw record values against real key values missed IPK rows:

1. `execdml.scanAllUniqueConflicts` — substitute cell.RowID for a NULL IPK
   slot before the UNIQUE/PK comparison (upsert arbiter hits were empty).
2. `execdml.buildRowMapFromValues` — trigger OLD/NEW rows exposed NULL for
   the IPK column (update/delete trigger old.a/new.a were "-").
3. `execconstraint.fkParentRowInTable` + `fkParentRowExists` — the parent-key
   lookup missed every parent row whose key was a rowid alias (valid child
   inserts reported "FOREIGN KEY constraint failed"; deferred COMMITs failed
   on repairable transactions).
4. `execconstraint.fkCheckChildTable` — foreign_key_check treated the
   rowid-alias child key as an exempt NULL (no violations reported).

`go test .`: 16 → 8 failures (TestP1InsertUpsert, TestP3FKey ×5,
TestP3Trigger ×2 fixed; zero new failures; remaining 8 pre-existing —
WAL/rtree/oracle-fixture classes, later tranches). testgen: fkey3 flipped
fail→pass (a fix flip, ledger re-seeded); fkey1/fkey2/trigger1/trigger2 stay
at ledger baseline. fk_constraint.go is at 1008 lines (>1000 hard gate) —
pre-existing 999-line file + 9 lines of this fix; split deferred to the
file-size remediation tranche per §5c.

### Tranches (execute in order; one tranche per commit series)

- **T1 standard-suite drift**: repair the hand-written P1/P3 upsert, FK and
  trigger tests (root-cause in the WR-WRITE conflict/or-plan plumbing —
  the fix must be WR-gated, not a revert). UCL: native tests pinning upsert
  DO NOTHING/DO UPDATE on rowid + WR tables.
- **T2 corrupt residue**: corrupt9/corruptC/corruptF/corruptL/corruptN.
- **T3 fts window**: fts3snippet/fts4opt (+ perf/hang timeout suspects
  serially re-run ≥55s class).
- **T4 long-tail**: the remaining ledger red in ledger-order batches of
  ~20, re-seeding the ledger per batch.
- **T5 un-skip queue**: uri/uri2/utf16align (+ any timeout-suspect
  resolutions), then final full sweep + check + close.

## Checkpointing Protocol

Per tranche: fix → tranche verify → `tools/status ledger` + `--check` →
update this T-log → commit ("FULL-SUITE-DRIFT.Tn: …") → push.
