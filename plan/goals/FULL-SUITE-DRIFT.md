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

### T1.2 RESULT (2026-09-07/08) — the other 6 standard-suite failures

All six were ORDER-DEPENDENT casualties of one hygiene bug: hand-written
tests calling `os.Chdir(t.TempDir())` (or MkdirTemp+Chdir) without
restoring — t.TempDir DELETES the directory at test cleanup, leaving the
process CWD dangling for every later test (file creations fail ENOENT;
fixture-relative paths resolve into deleted dirs). Converted all 21 sites
across 8 files to `t.Chdir` (Go 1.24+, auto-restores; go.mod `go`
directive bumped 1.22.0 → 1.24.0 — toolchain is 1.27).

Two real fixes alongside:
- `execdml.scanForConflict` substitutes cell.RowID for the NULL IPK slot
  (T1 root cause, second seam): a table whose IPK is one of SEVERAL unique
  columns skips the rowid-seek fast path, so INSERT OR IGNORE with an
  explicit duplicate IPK silently REPLACED the row (TestP1InsertOrIgnore:
  expected count=1 sum=1, got 2/3).
- TestP6_VacuumReindex removes a stale `vacuum_out.db` target before
  VACUUM INTO (the target-must-not-exist contract is correct; the test
  lacked cleanup).

After: `go test .` reports ZERO failing tests; the earlier
TestNativeBtreeDividerFixtureReference / TestNativeWalCheckpointPassive
FixtureReference / TestWALConformanceReadParity / TestRtreeStressChurn /
TestNativeRtreeCircleMatch / TestP8FreelistMultitrunk / TestP8IncrVacuum3
failures all disappear with the CWD fix.

NOTE (perf, P9.PERF scope): the root TestSQLiteSuite binary runs ~all 1219
JSON files in one process and sits right at the default 10-minute test
timeout (savepoint4 alone = 131s standalone, identical at HEAD db031d58c
and this tree — no regression from these fixes). Use `-timeout 1500s` for
full-suite verification until the perf tranche lands.

### T2 CENSUS (2026-09-08)

corrupt9/corruptC/corruptF/corruptL/corruptN triaged (per-package -timeout
120s runs):

- **corrupt9** (3 asserts): the corruption step is a TCL proc
  `corrupt_freelist test.db N` (hexio overwrites freelist trunk leaf
  entries with duplicates of the first entry) — emitted as
  "unsupported command, not transpiled", so the db is never actually
  corrupt and REINDEX legitimately succeeds. Needs (a) a tcl2go helper
  `tclCorruptFreelist(file, n)` + call recognition mirroring the proc
  (header offset 32/36 → trunk offset → overwrite leaves), and (b) an
  engine check: allocating/popping from a freelist with duplicate entries
  (or REINDEX writing through one) reports "database disk image is
  malformed" (btree.c freeList checks).
- **corruptC / corruptN** (build failed): generated code references
  `GMap[...]` / `issoak` / `perm` / `presql` without declaring them — a
  tcl2go array-map collection gap: `set ::GMap(key) val` style writes (or
  `global GMap` declarations) are not registered in `arrayKeys`/
  `arrayMapVars` for these files, so the preamble omits the map vars and
  the helper vars.
- **corruptF** (1.2 file size 0 != 6144): the test's setup proc
  `create_test_db` is "unsupported command, not transpiled" — test.db is
  never created. Needs a shape detector for the proc (it wraps a fixed
  execsql script) or inlining of user procs at call sites.
- **corruptL**: FAIL at 102s of a 120s cap — timeout-class; serial run
  with -timeout 600s required before triaging assertions.

Common theme: these corrupt files drive corruption via TCL-side file
manipulation procs; the transpiler needs a small library of file-corruption
helpers (tclCorruptFreelist first) plus proc-call inlining for test-local
procs that only wrap execsql/hexio sequences.

### T2 RESULT (2026-09-08)

Transpiler (tools/tcl2go):

- **::G harness array** — the TCL runner's `::G` options array (-soak/-perm/
  etc.) is never populated in the Go harness, so `info exists ::G(k)` is
  always false and a read yields "". cmdexpr.go's "info" handler special-
  cases `G`/`G(...)` to emit `"0"`, and stringexpr.go's arrayLookupExpr
  returns `""` for `::G` reads. This unblocked corruptC/corruptN's build
  (GMap/issoak/perm/presql declarations resolved through the same path).
- **proc-body inlining inside do_test** — runDoTestBody's sub-transpiler now
  copies `inlineProcs`/`inlineProcParams` from the outer transpiler, so
  zero-arg test-local procs (corruptF's `create_test_db`) inline into
  do_test bodies (processcommand.go's proc-call path).
- **tclCorruptFreelist helper** — helpers_template_part1_tail.go gains
  `tclCorruptFreelist(file, n)` mirroring corrupt9.test's `corrupt_freelist`
  proc (reads header freelist fields with manual byte arithmetic —
  encoding/binary is not in detectImports' list; overwrites trunk leaf
  slots 2..n+1 with a copy of the first leaf), and processcommand.go
  recognizes `corrupt_freelist FILE N`.

Engine (corrupt9 contract, oracle-verified with /usr/bin/sqlite3):

- **DROP INDEX now frees the index's pages** (ddl_drop.go):
  sqlite3DropIndex emits OP_Destroy/btreeDropTable; execDropIndex now calls
  dropBtreeRoot + refreshLargestRootPage after removing the schema entry.
  Without it the freelist never grows on DROP INDEX (corrupt9-1.1 needs
  free pages for the corruption step to have leaves).
- **Freelist-pop "page already in use" detection** (pager.go
  AllocatePageForTree + btree.go allocPage): btreeGetUnusedPage
  (src/btree.c:2449) reports SQLITE_CORRUPT when the popped page's pager
  refcount is >1, i.e. the page is still held by the allocating tree —
  corrupt9's duplicated leaf IS the root the build just consumed (leaves[0]
  pops first as the CREATE INDEX root). The faithful in-engine signal: a
  freelist pop returning the calling BTree's own root is always corruption
  (a live root is never on the freelist); AllocatePageForTree(liveRoot)
  returns "database disk image is malformed" in that case.
- **Record-header bound** (op_column_corrupt parity, vdbe.c OP_Column):
  parseRecordSerialTypes / storage.ParseRecordHeader / storage.DecodeRecord
  now require the record header to lie within the record's own bytes;
  otherwise "database disk image is malformed". Without it a junk-corrupted
  record header (corrupt-2.x appends 256 junk bytes at every 256-byte
  offset of the file) spins the serial-type loop appending billions of
  entries — measured 9-12 GB RSS in ~2s, the cause of the corrupt and
  corruptL baseline "timeout" states.
- **Overflow-chain geometric bound** (btree_insert.go readOverflow): the
  chain lives in the file, so a cell payload can never exceed
  numPages×(usable-4); a corrupt payload length promising more reports
  "database disk image is malformed" before any allocation (the second
  half of the same memory bomb: garbage plen allocated a GB-scale buffer
  that then fed the header spin).

Results (vs ledger baseline 2026-09-08):

- pass flips: corrupt4 (was GMap build fail), corrupt9.
- hang→clean: corrupt and corruptL now complete fast — corrupt FAILs on
  result mismatches in 90s (was: timeout-suspect + memory bomb), corruptL
  completes in 17s at -timeout 600s serial (was: timeout-suspect).
  Both are deep corruption-parity reds (P8.CORRUPT long tail), now
  actionable.
- compile-fail→run: corruptC/corruptN now build and run to deep
  multi-assertion corruption contracts (each assertion is a separate btree
  gap — P8.CORRUPT scope).
- unchanged reds: corruptB (same 3.1.1 autovacuum root-relocation failure
  as baseline; the ledger tail text was the SQL echo of the same error),
  corruptF (1.2 file-size split parity + late assertion), fts3corrupt
  family (P6.FTS-B).
- memory safety (user-reported system-memory risk): the unbounded
  allocation is fixed; a 1400-iteration corrupt-image Open/Exec/Close
  probe retains ~0 MB (heap 0MB / sys 35MB); the corrupt testgen binary
  peaks ~1.3 GB RSS over its full 1344-offset run (bounded, harness-side).

Tranche verify: corruption family packages re-run individually (no
pass→fail flips), internal/... all green except the pre-existing
internal/fts TestWriterConformance (fails identically at HEAD 395738c7f),
root suite + SOLID in this commit's verification.

### T2 addendum — freelist-membership memoization (2026-09-08)

The corrupt-engine work (correctly freeing pages on DROP INDEX, bigger
legitimate freelists) pushed an EXISTING quadratic hotspot over the
suite-timeout edge: `TestSQLiteSuite/temptable2/4.1.2`
(BEGIN; UPDATE t1 SET b=randomblob(100); ROLLBACK; on a temp table seeded
with 100k rows by section 1) spends minutes inside `DeleteCellsWhere →
maybeRebalanceAfterDelete → findParentByWalk → IsPageOnFreelist`, each
call re-walking the on-disk freelist chain (`chainContainsLocked`) —
O(pages × chain) per walk-family. Verified pre-existing at HEAD
395738c7f by stash-run (identical stall, 5-min -timeout kill).

Fix: `Pager.freeSet` memoizes the chain membership (trunks + leaves) in
ONE walk, rebuilt whenever any chain mutator nils it
(`invalidateFreelistSetLocked` from FreePage / allocateFreelistLocked /
allocateFreelistNearLocked / freelistPagesAboveLocked / writeFreelistTrunkLocked /
ZeroFreelistChain). IsPageOnFreelist (non-autovacuum) answers from the
memo; rebuild is cycle- and count-bounded (≤ n entries) so corrupt chains
cannot spin it. 4.1.2: minutes → 0.00s; the whole file: 300s-timeout → 42s.

Note: temptable2's "table t1 already exists" cascade at 3.1.1+ is
pre-existing at HEAD (stash-verified, identical messages) — the JSON
harness runs testdata/temptable2.json unskipped, while the ledger's
'temptable2: skipped' entry refers to the testgen no-op package. The
in-suite behavior of that file is re-verified in this commit's root-suite
run; its assertion drift is FULL-SUITE-DRIFT T4 scope either way.

### T2 addendum 2 — root JSON suite is NOT a deterministic gate (2026-09-08)

Tonight's full `go test . -timeout 1500s` runs could not reproduce the
T1.2 "zero failing tests" state, and the investigation shows the root
JSON-harness suite is intrinsically non-reproducible; do NOT use it as a
flip gate until T4 fixes these three classes:

1. **Converter-dropped fixture functions.** alias.test line 42 registers
   `db function sequence` — the JSON has no step type for it, so
   `alias/setup_0` fails "no such function: sequence" deterministically,
   in any run, at any commit (HEAD-stash verified). Files exercising
   `db function`/`db func` were never individually green. Same class:
   tkt_d635236375 (the converter lost the TCL `db close / file delete /
   sqlite3 db` between 1.0 and 1.1, so 1.1's re-CREATE batch cannot pass;
   "UNIQUE constraint failed: t1.id1" reproduces identically at HEAD).
2. **Parallel-interleaving dependence.** Subtests run t.Parallel() across
   files sharing one process; combined with class 1, whether a file's
   setup sees state from a benefactor file depends on scheduling. The
   failure SET (400 files tonight) is not stable across runs of the same
   tree.
3. **The pre-memoization O(n²)** (freelist-walk per IsPageOnFreelist in
   bulk delete/rebalance) made suite completion time unstable: the
   dc1325ae9 worktree re-run tonight TIMED OUT at 25 minutes inside
   temptable2/4.1.2 — the "fully green" baseline commit does not
   reproduce green tonight even before this tranche's engine changes.
   The memoization in this tranche removes that instability source.

T2 verification therefore rests on the deterministic surfaces: the
testgen packages (corruption family + flips, ledger-governed),
internal/... unit packages, SOLID, and the per-file -v comparisons
against HEAD-stash for every suspicious file (tkt_d635236375, alias,
temptable2 — all identical at HEAD). Harness determinism (per-file engine
globals, `db function` fixture registration in the converter) is queued
as T4 scope.

### T2 CLOSE (2026-09-08)

Final state: sweep re-seeded — 1219 packages, **730 pass / 238 fail /
244 skip / 7 timeout-suspect** (baseline was 724/251/244 at the T2
census; net +6 pass). `tools/status -check` PASS (no unexpected flips).
The 7 duration-suspects (≥55s class) carry to T3's serial re-runs;
corrupt (90s) and corruptL (17.6s) have already been serially verified
this tranche and are honest fails, not hangs.

Flips this tranche: corrupt4 (compile-fail → pass), corrupt9 (fail →
pass), corrupt + corruptL (timeout-suspect → fail, evidence recorded).
No pass→fail flips anywhere.

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
