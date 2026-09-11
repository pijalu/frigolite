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

### T3 RESULT (2026-09-08) — fts window + timeout-suspect serial re-runs

- **fts3snippet fail→pass** (transpiler): `sourceLeadingDeletes` scanned
  line-wise past loop headers, so a `forcedelete test.db` INSIDE a foreach
  body was pre-emitted before the preamble Open AND swallowed at its real
  position (genPreDeleted) — the per-iteration delete never happened and
  pass 2+ hit "table ft already exists". The leading-region scan now stops
  at foreach/for/while headers; in-loop deletes emit os.Remove at their
  real position. testgen regenerated.
- **corrupt: 900s+ hang → 31.7s honest fail.** Two stacked hotspots beyond
  T2's fixes, both in integrity_check on junk-corrupted images:
  (1) findOrphans re-walked the whole freelist chain per unreferenced page
  (O(orphans × chain)) — hoisted to a single walk (isFreelistOwnedSet)
  with per-page verdicts preserved byte-for-byte, including the
  duplicate-abort semantics that split "2nd reference" findings from
  "never used" orphans; (2) the orphan scan bounded by FilePageCount alone
  exploded on sparsely-extended files (millions of "never used" appends) —
  now min(HeaderPageCount(), FilePageCount()), which is C's
  i=2..pBt->nPage scan shape (lockBtree clamps nPage to the file).
- **fts4opt**: verified pre-existing — identical 3331 exec errors at HEAD
  (stash-run) and on this tree; deep FTS-optimize gap, stays P6.FTS-F.
- **Timeout-suspects serially re-run** (-p 1, 900s cap): avtrans 143.6s,
  fts3defer 65.5s, fts4check 107.9s, fts4merge4 473.4s, fts4unicode
  46.6s, rtree2 69.4s — all complete and fail honestly (deep parity reds
  for their P6 goals); corrupt completes in 32s. No hangs remain in the
  suspect set.
- **No-drift proofs**: testgen/autovacuum's failure (autovacuum-2.4.5) and
  the native TestP8FreelistMultitrunkInspectChain /
  TestP8IncrVacuum3OracleSequence failures reproduce IDENTICALLY at HEAD
  and at the dc1325ae9 worktree — pre-existing reds, recorded for T4
  (native freelist/autovacuum-drain parity: trunk leaf-count cap vs
  reserved bytes; incomplete drain).

Sweep re-seeded: 1219 packages, **734 pass / 234 fail / 244 skip / 7
timeout-suspect** (T2 close: 730/238). `tools/status -check` PASS.

### T4 session 1 (2026-09-08) — native freelist/autovacuum-drain reds fixed

Both native reds left from the T3 note are fixed with oracle/C grounding:

- **maxTrunkLeaves now uses usableSize** (src/btree.c:6871: `nLeaf <
  pBt->usableSize/4 - 8`): FreePage's trunk-fill cap was computed from
  pageSize, overfilling trunks by the reserved-byte margin. For the
  engine's default reserved=0 the cap is unchanged (248); databases with
  reserved bytes no longer overfill. Oracle cross-check (macOS sqlite3,
  reserved=12): its trunks fill to exactly (1024-12)/4-8 = 245.
  TestP8FreelistMultitrunkInspectChain's constant was ALSO wrong (it
  assumed reserved=8): the test now derives the cap from the db's own
  header byte 20.
- **In-transaction incremental_vacuum now actually drains** — the
  P8.INCRVACUUM.phase7 no-op divergence is retired. C runs the drain
  steps inside the open transaction, journal-protected. The pager
  gained the two missing pieces: truncatePages journals the before-image
  of every removed tail page, and rollbackFromJournalLocked restores the
  file length to the journal header's dbOrigSize (pager.c's nTrunc
  playback), including re-writing the journalled tail before-images to
  disk before the cache replay (cache-only replay would leave zeros in
  the restored tail). TestP8IncrVacuum3OracleSequence's tn8
  (BEGIN; double; incremental_vacuum=1000; double; COMMIT) now ends at
  freelist_count=0, integrity "ok", matching the oracle.

Regression net: TestP8*/TestP6*/TestP5*/TestIncrcorrupt/TestCorrupt2
native families green; testgen incrvacuum/autovacuum2/corrupt9/vacuum*/
journal2/journal3/rollback/savepoint*/without_rowid1/memdb1 unchanged
(savepoint's failure is pre-existing, same signature at HEAD).

### T4 session 2 (2026-09-08) — missing prepare-time error class (10 packages)

One shared root cause — the engine accepting constructs SQLite rejects at
prepare time — covered ten packages with nine error checks (each verified
against src/ C references):

- **COMMIT with no active transaction** → "cannot commit - no transaction
  is active" (src/vdbe.c OP_Transaction; insert4-8.10, tkt2920-1.9).
- **SQLITE_FULL rolls back the whole transaction** (vdbeaux.c:3352-3383
  isSpecialError class, same as the existing INTERRUPT handling):
  an INSERT/UPDATE/DELETE failing "database or disk is full" inside BEGIN
  cancels the txn, so the later COMMIT fails (tkt2920's scenario).
- **Wrong function arity** → C's canonical "wrong number of arguments to
  function X()" for EVERY built-in (sqlite3WrongNumArgs); the per-function
  WrongArgMsg gate is removed (func2-1.2.1 SUBSTR(), limit-12.1
  replace()). The name echoes the SQL's spelling.
- **likelihood() second argument** must be a constant in [0.0,1.0]
  (src/expr.c sqlite3ExprCodeTarget; func3, whereG).
- **Compound arm-width mismatch** now fires in scalar-subquery and
  INSERT-VALUES contexts ahead of the IN/column-count checks
  (sqlite3SelectWrongNumTermsError; in-12.6+, select4-11.16). The AST
  gained SelectStmt.ExplicitSetOp to distinguish explicit UNION links from
  comma-desugared VALUES rows; pure-VALUES mismatches use the SF_Values
  message "all VALUES must have the same number of terms" (values-2.1.x).
- **ALTER TABLE ADD COLUMN check order** now mirrors alter.c:350-400:
  PRIMARY KEY, UNIQUE, REFERENCES default, NOT NULL default, non-constant
  default; "duplicate column name: b" unquoted (alter3-2.x, alter4-2.x).
  DEFAULT NULL (NullLit) allowed; arithmetic/function defaults rejected.
- **Partial-index WHERE clause** validation names its constructs
  (index6-1.4/1.5, index7-1.2-1.6): "parameters prohibited in partial
  index WHERE clauses", "non-deterministic functions prohibited in
  partial index WHERE clauses" (incl. julianday('now')), "subqueries
  prohibited in partial index WHERE clauses" (incl. EXISTS); index key
  columns resolve against the table ("no such column: x") so failed
  CREATE INDEX statements no longer leak entries.
- **WHERE (SELECT 0,0) OR ...**: the subquery-arity validator now
  recurses through ParenExpr and treats AND/OR operands as scalar
  contexts (in-13.15).

Flips this batch: func2, insert4, tkt2920, whereG, in, select4, values,
alter4, index6, index7 (fail→pass); alter3 7→2 remaining failures (the
rest are a pre-existing temp-trigger state issue, T4 later). func3
improved; its 3 remaining assertions need the C-API destroy callback
(not expressible — NA class). Regression net: native P1/P3/P5/P6/P8
green; testgen func/limit/where families unchanged vs ledger.

### T4 session 3 (2026-09-08) — read-only connections (openv2, rdonly)

Two gaps closed, oracle/C-grounded:

- **Statement-level read-only gate** (execEntry): a connection whose main
  pager is read-only fails every writing statement — DML, DDL, VACUUM/
  ANALYZE/REINDEX — with "attempt to write a readonly database" (pager.c
  sqlite3PagerWrite → SQLITE_READONLY); reads are unaffected. This is the
  frigolite statement-gate analogue of pager.c's per-write SQLITE_READONLY.
- **Write-version gate** (openPager): a database whose file-format WRITE
  version exceeds 1 (WAL or a newer format) cannot be written by a
  journal-mode connection — the pager is marked read-only at open
  (rdonly-1.3/1.4 write version 3 into header byte 18; reads succeed,
  writes fail). WAL databases reopening with their -wal file enter WAL
  mode first and stay writable.
- **OpenReadOnly(":memory:")** now returns a read-only pager
  (OpenInMemoryReadOnly) — openv2-2.1 opens successfully and 2.2's
  CREATE TABLE fails — and schema.Init skips bootstrapping an empty
  read-only database instead of erroring (openv2-2.1 regression caught
  during the fix).

Flips: openv2 fail→pass, rdonly fail→pass. Regression net: filefmt,
journal2/3, wal2, walbak, backup/2, vacuum, corrupt9, incrvacuum/2/3, and
the native P1/P3/P5/P6/P8 families all green. savepoint2's sweep suspect
flag was load noise (passes serially in 32s).

### T4 session 4 (2026-09-08) — CHECK constraint validation and naming

check package: 7 → 4 failing assertions. Fixes, all grounded in
build.c/alter.c's prepare-time CHECK handling:

- Table-level and column-level CHECK expressions resolve their column
  references at CREATE TABLE: bare unknown columns report "no such
  column: q" (check-3.3); foreign-qualified refs report the qualified
  spelling "no such column: t2.x" (check-3.5); the table's own
  qualification (t3.x<25, check-3.7) and db/schema-qualified chains
  (main.t810.a, xyzzy.t811.b — check-8.1) resolve; rowid/oid/_rowid_
  resolve (check-9.1). Double-quoted tokens keep the DQS string fallback
  (check-2.1's "integer"). The failed 3.3 CREATE no longer leaks the
  table, un-cascading 3.4/3.6.
- CHECK expressions reject bound parameters: "parameters prohibited in
  CHECK constraints" (check-5.1/5.2).
- The named-CHECK violation reports the constraint name immediately
  before the CHECK keyword — the LAST stacked CONSTRAINT wins
  (check-2.12/2.13: "CHECK constraint failed: x_two", was x_one).

Remaining check failures (4): check-4.9 wants the verbatim multiline
CHECK text through the UPDATE path's tableCheckConstraintText extractor;
check-4.9's span extraction stops early. check-7.x (myfunc) requires the
TCL db-func fixture registration — converter gap (NA class, see func3).

### T4 queue note (2026-09-08) — triaged samples from the mismatch cluster

- filter1-3.3: bare column with a filtered aggregate — C's bare column
  takes the group's FIRST row when the FILTER excludes every row
  ([1,3,{}]); frigolite takes the last ([1,4,{}]). Fix belongs in the
  GROUP BY bare-column representative selection (select_agg.go;
  groupRows[0] is not consulted on the filtered-aggregate path).
- existsexpr: EXPLAIN QUERY PLAN tree-rendering differences
  (CORRELATED SCALAR SUBQUERY indentation) — EQP text parity, isolated.
- distinct: DISTINCT output ordering/collation (C emits A B C a b c,
  engine emits a b c A B C) — collation-aware DISTINCT sort.
- fkey1-5.2.1: FK error-message list accumulation shape.
- autoinc-12.5: AUTOINCREMENT sequence corruption should surface
  "database disk image is malformed" (error-detection gap like the T2
  class).

### T4 session 5 (2026-09-09) — filter1 bare column with filtered aggregate

`reorderRowsForMinMax`/`minMaxSourceRow` now honor the aggregate's FILTER
clause: minMaxAggregate carries the FILTER expression, filtered-out rows
cannot produce the extreme value, and when a filtered MIN/MAX has no
contributing row the bare columns take the group's FIRST row (C's
aggregate-with-FILTER semantics: no accumulator ever records, so the
output column reads the group's first row — filter1-3.3 expects
[1,3,{}],[2,6,{}], was [1,4,{}],[2,8,{}]). The unfiltered all-NULL
fallback (last row) is unchanged.

filter1: 4 → 3 failing assertions (3.3 fixed). Remaining filter1 failures
queued: 4.2 (ORDER BY an alias inside an expression — ORDER BY (h+1.0)
does not resolve the alias h), 6.1 (FILTER on a correlated scalar
subquery's aggregate), 440 (mixed FILTER shapes). minmax/minmax3/4/
select families verified unchanged; the unfiltered path is byte-identical.

### T4 session 6 (2026-09-09) — ORDER BY names resolve SELECT aliases inside expressions

`SELECT avg(c) FILTER (WHERE b!=1) AS h FROM t1 GROUP BY a ORDER BY
(h+1.0)` — names inside ORDER BY expressions resolve against the result
set (SQLite's resolveOrderGroupBy tries output aliases). The comparator's
fallback evaluation and the ORDER BY pre-evaluation pass now evaluate
against a combined row map (source columns + output column names mapped to
their result values, output shadowing source on name conflicts, matching
alias shadowing per resolver01-4.1's documented precedence).

filter1-4.2 passes; filter1 3 → 2 failing assertions (6.1: FILTER on a
correlated scalar subquery's aggregate; 440: mixed FILTER shapes — both
queued). Regression net: native P1/P3/P5/P6/P8 green; orderby1-5,
select*, minmax*, resolver01, with1/2 unchanged vs ledger.

### T4 queue addendum (2026-09-09) — filter1-6.1 correlated-aggregate FILTER diagnosis

Probe evidence for `SELECT (SELECT COUNT(a) FILTER(WHERE x) FROM t2) FROM t1`
(t1: 2 rows; t2: 1 row x=1; oracle [1,1], engine [0]):

- The engine's correlated-aggregate machinery steps the aggregate over the
  OUTER rows (aggRowMaps = outerRows), so a FILTER referencing an INNER
  column (x) evaluates against outer columns, misses, and the count is 0.
- The unfiltered correlated count (SELECT COUNT(a) FROM t2 inside an outer
  query) steps over outer rows too, yielding 2 (C: 1 per outer row — the
  subquery scans its own FROM). This predates the FILTER gap.
- C model to port: when the subquery has a FROM, the aggregate steps over
  the INNER table's rows; the FILTER evaluates on the inner row; argument
  names that miss the inner row resolve as outer constants (per-outer-row).
  The FROM-less correlated aggregate case (SELECT (SELECT max(y)) with y
  outer — window1 76.5) keeps the current outer-row stepping.

This is a scoped redesign of evalAggOverOuterRowsWithInner /
aggregateHasOnlyOuterRefs (the FILTER expression must participate in the
inner-reference scan) — queued as its own batch with the probe cases
above as the acceptance tests.

### T4 next-batch plan (2026-09-09) — scoped entry points

1. **Correlated-aggregate inner-row stepping** (filter1-6.1): the change
   lives in evalAggOverOuterRowsWithInner (select_agg.go:451) — when the
   subquery SELECT has a FROM clause, the aggregate's stepping rows must be
   the INNER rows (allRowMaps, each merged over the current outer row's
   values so outer-constant args resolve) instead of outerRows, and the
   FILTER must evaluate on the inner row. The single-evaluation caching of
   the scalar subquery result must also treat an aggregate arg that misses
   the inner columns as correlated. Acceptance cases (oracle-verified):
   (SELECT COUNT(a) FILTER(WHERE x) FROM t2) FROM t1 -> [1,1];
   (SELECT COUNT(a) FROM t2) FROM t1 -> [1,1] (currently [2], steps outer
   rows); (SELECT COUNT(x) FILTER(WHERE x) FROM t2) FROM t1 -> [1,1]
   (already green — do not regress). FROM-less correlated aggregates
   (window1 76.5) keep outer-row stepping.
2. **check-4.9 verbatim multiline text via the UPDATE path**: the UPDATE
   emitted "CHECK constraint failed: x+y==11" — the first line only —
   where the oracle carries the full verbatim CHECK body including
   newlines. Trace whether the message came from the column-level
   checkConstraintText fallback or a truncated tableCheckConstraintText
   span; the oracle target is the raw span between CHECK( and its matching
   ) with original whitespace.
3. attach2 'database is locked': cross-connection lock gate emits
   "database is locked" for the wrong attachment scenario.

### T4 check-4.9 narrowing (2026-09-09)

Probe isolation: the UPDATE path (checkTableUpdateChecks →
tableCheckConstraintText) already emits the FULL verbatim multiline text
("x+y==11\n        OR x*y==12\n        OR x/y BETWEEN 5 AND 8\n
OR -x==y+10") — matches the oracle. The truncation to "x+y==11" is in the
INSERT path (insert_constraints.go's checkText, checkConstraintText/
checkConstraintTextFromPart): it stops at the first newline inside the
CHECK body. Next action: find the line-boundary cut in the INSERT path's
text extraction (splitColumnDefs/checkParenExpr are depth-based, so the
cut is likely in an earlier line-splitting of the stored SQL or a
part-boundary), and make the INSERT path reuse tableCheckConstraintText's
full-span extraction. The failing setup inserts (1,1),(2,4),(4,6) are
correct CHECK violations — only the message text is truncated.

### T4 session 7 (2026-09-09) — check-4.9/4.10 fixed (VACUUM does not re-evaluate CHECKs)

Oracle isolation showed the 4.9 UPDATE message is already correct — the
real gap was 4.10: a row stored while ignore_check_constraints=ON must
SURVIVE VACUUM (C's VACUUM is a page/image copy and never re-evaluates
CHECK constraints; oracle-verified: VACUUM succeeds, the violating row
persists, integrity_check afterwards reports "CHECK constraint failed in
t4"). frigolite's logical rebuild re-inserted rows through the constraint
machinery and failed. Fix: copyViaBackup suppresses CHECK enforcement on
the destination engine for the copy's duration and restores the caller's
flag (matching C's page-copy semantics for both VACUUM and VACUUM INTO).

check package: 7 → 3 failing assertions; the remaining three (7.x myfunc)
need the TCL db-func fixture registration — converter/NA class. Vacuum
family + native suites green; sweep reseeded 741/234/244, -check PASS.

### T5 session (2026-09-09) — utf16align un-skipped; uri/uri2 scoped

- **utf16align un-skipped and PASSING** — the accumulated transpiler work
  (GMap/::G declarations, proc inlining, forcedelete handling) made the
  package transpile and run clean. skipTestFiles entry removed; the
  package is now a real transpiled test in the sweep.
- **uri / uri2**: un-skip attempted and triaged. uri's conversion is
  mangled (`file isdir $file` emitted as a bare identifier
  file_is_a_directory → build failure; PWD-substitution and bracket
  artifacts). uri2 requires the ENABLE_URI_00_ERROR engine behavior
  (reject "%00" in URIs with "unexpected %00 in uri", SQLITE_ERROR) plus
  sqlite3_open/errcode C-API seams the converter does not model (the
  generated code appends "]" and calls a nonexistent LastErrCode flow on
  an Open that should fail). Both restored to skipTestFiles with
  sharpened reasons pointing at the exact gaps; utf16align stays un-skipped.

### T4 session 8 (2026-09-09) — correlated-aggregate inner-row stepping landed

evalAggOverOuterRowsWithInner now steps a correlated subquery's aggregate
over the INNER rows when the subquery SELECT has a FROM clause: each step
row merges the representative outer row's values UNDER the inner row
(inner shadows outer), so the FILTER evaluates on the inner row and
argument names missing inner-side resolve as outer constants. FROM-less
correlated aggregates (window1 76.5) keep outer-row stepping.

Acceptance cases (oracle-matched, from the T-log probe):
- (SELECT COUNT(a) FILTER(WHERE x) FROM t2) FROM t1 -> [1] (was [0])
- (SELECT COUNT(a) FROM t2) FROM t1 -> [1] (was [2] — stepped outer rows)
- (SELECT COUNT(x) FILTER(WHERE x) FROM t2) FROM t1 -> [1,1] (unchanged)
- (SELECT SUM(a) FILTER(WHERE x) FROM t2) FROM t1 -> [1]

filter1-6.1 passes. Regression net: window1-5, filter1's other cases,
func4/subquery (baseline-fail, unchanged), native P1/P3/P6/P8 all green.

### T4 trigger2 triage (2026-09-09) — root cause found: tclExprWith lacks TCL int()

trigger2-1.x.1/.2 (and without_rowid4's same shape) fail because the
generated `tclExprWith("int($v)", ...)` cannot evaluate TCL's `int()`
coercion function — it falls back to string semantics, producing the
literal strings "int1".."int5" instead of the integers 1..5 from each
rlog row's idx column. The engine's multi-statement Query and trigger
execution are fine (probe: rlog rows are 7-wide with int64 idx; the two
SELECT statements concatenate correctly). Fix: teach the generated
tclExprWith helper (helpers template) to coerce with int()/wide()/
boolean() TCL functions, then regenerate. trigger2 and without_rowid4
should flip fail->pass (their remaining assertions are the same shape).

### T4 trigger2 triage continued (2026-09-09) — int() coercion landed; multi-column db-eval accumulation scoped

tclExprWith now supports TCL coercions int(X)/wide(X)/double(X)/
boolean(X) before paren resolution (resolveParens was gluing the function
name to the value: "int(1)" -> "int1"). The "int1..int4" string artifacts
are gone from trigger2.

trigger2/without_rowid4 still fail on a SECOND converter gap: the db-eval
row loop appends only the FIRST column (v := _row1[0]) per row while the
assertions need every column of rlog/clog (35-value lists). The converter
must translate `db eval {SELECT * FROM t} r { lappend r $r(c1) $r(c2) ... }`
into per-column appends — queued as the db-eval multi-column accumulation
feature (harness-determinism batch).

### FLAKY flag (2026-09-09) — rtreecheck

rtreecheck flipped pass->fail between sweeps with NO generated-code or
engine delta in its path (verified: testgen/rtreecheck is byte-identical
across the sweep states; stash-runs on both trees fail 5/5 now while the
21:32 sweep recorded pass). The 5.1/5.2 assertions do shadow-table
corruption writes (set_int32 on r3_node) inside BEGIN and read
rtreecheck('r3') inside the transaction — the pass/fail boundary is
suspected to be uncommitted-shadow-write visibility ordering. Flagged
for P6.RTREE with a determinism investigation requirement.

### Correction (2026-09-09) — check-4.9 "newline-cut" was a log artifact

Re-probing with %q formatting shows the INSERT path emits the FULL
verbatim multiline CHECK text ("x+y==11\n        OR x*y==12\n ...") —
there is no newline-cut in checkConstraintText/checkParenExpr. The
earlier truncated-looking observations were grep/log lines cutting the
multi-line error message. check-4.9 and 4.10 are both fixed by the T4.7
copyViaBackup CHECK suppression; the check package's only remaining
failures are the three myfunc fixture-seam assertions (TCL db-func
registration, converter/NA class). CLOSED — no further work needed on
check-4.9.

### T4 db-eval cell iteration (2026-09-09) — single-variable db-eval loops iterate cells

emitDBEvalForeach now emits a nested cell loop when the foreach has ONE
loop variable: [db eval SQL] returns a FLAT list of every cell of every
row (TCL execsql semantics), so `foreach v [execsql {SELECT * FROM
rlog}]` must bind v to each cell, not only column 0. Multi-variable
destructuring is unchanged. This fixed trigger2-1.x's row shape (idx
values now numeric and complete); without_rowid4 improved.

Remaining trigger2-2.x failures are the backslash-continuation mangling:
TCL list entries ending in "\" (line continuations, e.g. tbl_definitions)
are preserved as literal backslashes in the generated SQL strings, which
the engine then rejects with `unrecognized token: "\"`. The converter's
list splitter must fold backslash-newline continuations inside braced
list elements (same class as tclSplitList's handling) — queued.

### T4 backslash fold + regen fix (2026-09-09)

- foldBackslashNewline: goStringLiteral's Braced branch now applies TCL's
  brace-word line-continuation rule (backslash-newline + following
  whitespace folds to a single space), eliminating the literal '\'
  preserved in generated SQL strings (trigger2-2.x "unrecognized token").
- Found and fixed the reason trigger2's cell loop never appeared: the
  T4.10 regeneration had CRASHED partway (strings.Repeat negative count —
  the cell-loop emission incremented indent once but the tail decremented
  twice), leaving a mixed on-disk state; the committed "cell iteration"
  did not fully land. With the indent balance the regen completes and
  trigger2/without_rowid4's 1.x rows evaluate correctly.
- LESSON: when a regen command exits non-zero, the on-disk generated tree
  is PARTIAL — never commit it as if complete; re-run to completion
  first.

### T6 post-T4.11 tranches (2026-09-10/11 — T-log backfill for committed work)

Committed after the T4.11 close without T-log entries; recorded here from
commit subjects + the 2026-09-11T00:09:07Z sweep (769/207/243 of 1219,
ledger re-seeded 2026-09-11T00:26:44Z, `--check` PASS):

- **attach** (b5db6dd2c + parents): lock-registry ATTACH gate + eager
  schema read + owning-db lock keys — attach suite PASS in the sweep;
  attach-9.1 lazy-creation investigation recorded (5919cb025: needs
  connection lock tracking). **attach2 still FAIL** ('database is locked'
  cross-connection gate emits for the wrong attachment scenario — §T4
  item 3 open).
- **reindex** (ee71191e0): target capture + unknown-collation/object
  validation — reindex PASS in the sweep.
- **check/cacheflush/subjournal** (f43e74a56, e37f158d4, 394dbdab3):
  CHECK-constraint function handling (unknown-function message +
  CREATE-time validation), SAVEPOINT-placeholder collapse
  (cacheflush/subjournal PASS), cacheflush minimal-repro isolation
  matrix — check PASS in the sweep.
- **conflict** (7c7b77a0c + reverts 590817319/3dbdab6b8): per-column ON
  CONFLICT UPDATE dispositions + violated-column keying (11→3
  assertions); full-matrix-atomic partial prototype REVERTED (regressed
  ROLLBACK/ABORT subtests) — conflict PASS in the sweep.
- **bloom1/minmax/minmax2** (5ee07df44, 97524bd7f, f10801263/a4639c23f):
  unquoted TRUE/FALSE literals, sqlite_search_count N-A skips, minmax
  N-A classification — all PASS in the sweep.
- **P6.FTS-RESIDUE collateral**: intarray/tpch01 regexp-in-set-var
  (b00b5b620), aggerror/count aggregate UDFs + IN-subquery misuse
  (c4060c6e1), fts3tok1/fts4unicode (0e98e6395), fts3defer/fts3drop/
  fts4noti docid-0 reopen rebuild (8025c85d5), fts4opt %_stat hint
  rowid-alias (1bb32163a), filectrl tempfilename (baa20a9d9),
  fts3comp1/trustschema1 (f09f9c8a8) — all PASS in the sweep.
- **REGRESSION WATCH (open)**: `autovacuum` 2.4.5 FAIL and `pragma2`
  page_size=16384+cache_spill FAIL in the fresh sweep against P8-closed
  goals — triage tranches own them (see PORTPLAN §2 / §5a item 10).
- **trigger2/without_rowid4 still FAIL**: db-eval multi-column
  accumulation queued since T4 triage — next converter tranche.

### T7 baseline (2026-09-11T06:09:26Z) — serial live states, all 5 FAIL

- **trigger2**: 5 failing assertions, all 6.1/6.2 OR-conflict shape
  (clean tree: 6.1b/6.1d/6.2b/6.2d/6.2g all nil; with the partial T7.1
  working-tree fix the INSERTs flip green and the UPDATEs report bare
  "tbl" instead of "tbl.a"). The 6.1 gaps are engine (see T7.1 probe
  notes below); the 6.2 gaps are the update conflict-error column-naming
  gap (update.go uniqueConflictError column-less fallback). T7.1 owns.
- **without_rowid4**: 2 result mismatches (lines 352/376) + 5 UNIQUE-nil
  gaps (lines 494/524/550/562/586, same 6.x OR-conflict shape as
  trigger2). T7.1 owns.
- **attach2**: 4.4 expects "database is locked" got nil (db2 INSERT
  while db holds SHARED read txn on same file — CrossConnLockError
  WriteTxByOther misses read-txn SHARED); 4.11/4.12 COMMITs then fail
  with spurious "database is locked" (cascade of the same gate
  misfiring on the wrong scenario). T7.2 owns.
- **T7.2 CLOSED (2026-09-11): attach2 GREEN — two lock-gate fixes**
  - 4.4 (autocommit write vs other's read txn): CrossConnLockError now
    refuses an AUTOCOMMIT write when another connection holds SHARED on
    the file (internal/exec/locks.go). Writes inside an explicit txn
    still take RESERVED and fail later at COMMIT (4.10), matching the
    RESERVED/EXCLUSIVE split.
  - 4.11/4.12 (COMMIT upgrades wrong files): commitLockError now gates
    only DIRTY-pager files (HasDirtyPages), not every attached file —
    db2's file2-only COMMIT no longer trips on db's released main
    SHARED, and db's read-only COMMIT (no dirty pages) falls back to
    all-keys (unchanged behavior).
  - Verified: full 4.1→4.12 pure-Go sequence matches every TCL
    expectation; lock/lock2/lock3/lock4/lock6/lock7 + attach stay green
    (lock/lock5 failures pre-existing on clean tree, unchanged).
- **autovacuum 2.4.5**: rootpage list has holes at 65, 207, 412
  (got skips 65/207/412; want contiguous 65/207/412 present) — page
  allocator wrongly treats pointer-map/pending-byte pages as unusable
  for root pages. Known P8.INCRVACUUM residue. T7.3 owns.
- **pragma2-5.1**: Query("PRAGMA page_size=16384; CREATE TABLE t1(x);
  ...PRAGMA cache_spill") errors "file is not a database" in-suite
  but PASSES as a standalone probe — state carried from the pragma2-4.x
  prefix (big-table spill + COMMIT + DETACH leaves stale pager state;
  page_size change on reopen then misvalidates). T7.4 owns.
- **T7.1 CLOSED (2026-09-11): trigger2 GREEN; root cause was ENGINE, not db-eval**
  - The queued db-eval multi-column accumulation theory was WRONG:
    trigger2-1.x/2.x pass on the current tree (converter backslash-fold
    + cell-iteration fixes from T4/T6 already landed; regen diff is only
    whitespace/continuation folding).
  - Real 6.1 root cause (oracle trigger.c:1135-1150 codeTriggerProgram):
    the AFTER-trigger body step runs at depth 0 via Engine.Exec and
    re-published OuterOrConflict, so the body's INSERT OR IGNORE
    clobbered the outer INSERT OR ABORT/FAIL/ROLLBACK. Worse, the
    clobbered IGNORE then swallowed the body step's own self-conflict
    (outer row written BEFORE the AFTER trigger fires, so
    (new.a,0,0) self-conflicts). Fix: OuterOrConflict plumbing
    (exectrigger.Manager + DMLContext + Engine) with no-clobber publish
    (insert_core.go/update_split.go: only publish when none active) +
    applyOuterOrConflict override (ABORT/FAIL/ROLLBACK override an
    explicit weaker step policy; IGNORE/REPLACE never do) + OrIgnore/
    OrFail flag sync (else insertOneTuple's OrIgnore check swallows).
  - Real 6.2 root cause: the 6.2b/d/g failure is the BODY step's conflict
    (UPDATE OR IGNORE→ABORT tbl SET a=new.a=4, no WHERE: row2 6→4 dups
    row1's 4), and updateRowConflictsWithTable returned only bool, so
    the error fell back to bare "tbl". Fix: thread the conflicting
    live-row values out (updateRowConflictValues/cellConflictValues) and
    name the column from them (update_apply.go).
  - Residual risk: applyOuterOrConflict mutates the parsed trigger-body
    step in place; steps are re-parsed per fire (parseTriggerBody), so no
    cross-fire contamination. The stricter-override rule (ABORT over an
    explicit IGNORE) is verified against sqlite3 3.51.0 for the 6.x
    shapes; exotic mixed OR-step programs could differ — no TCL
    coverage beyond 6.x.
  - without_rowid4: 2.x BEFORE-UPDATE trigger path loses the 500 row +
    6.x WR OR-conflict gaps REMAIN (separate WR trigger-path gaps:
    mergeTriggerModifiedRow/rowExists rowid-keyed on synthetic RowID 0;
    parked, NOT regressed — clean-tree stash comparison shows identical
    2.x/6.x failures before/after T7.1). Next tranche owns.
- **T7.1 engine probes (oracle-matched, sqlite3 3.51.0)**:
  - trigger.c codeTriggerProgram (trigger.c:1135-1150): step WITHOUT
    explicit OR inherits firing stmt policy; step WITH explicit OR keeps
    its own — verified: cross-table OR IGNORE step under OR ABORT outer
    still skips (rc=0), firing only ABORTs when the step itself
    conflicts under ABORT.
  - trigger2-6.1 shape (AFTER INSERT ... INSERT OR IGNORE INTO same
    tbl, fresh-key INSERT OR ABORT): sqlite ERRORS tbl.a — the outer
    INSERT's row is written BEFORE the AFTER trigger fires, so the body
    step's (new.a,0,0) self-conflicts with the just-written row; under
    ABORT that conflict raises. Engine misses it because the body step
    runs at depth 0 via Engine.Exec and re-publishes OuterOrConflict
    (IGNORE clobbers ABORT); partial T7.1 fix in working tree
    (OuterOrConflict plumbing + no-clobber publish + OrIgnore/OrFail
    flag sync) flips INSERT OR FAIL/ROLLBACK green but ABORT still nil
    — the ABORT body step's insertOneTuple OrIgnore check needs the
    same flag-sync treatment. NOT committed; stash-verified pre-existing.
  - trigger2-6.2 shape (AFTER UPDATE ... UPDATE OR IGNORE, UPDATE OR
    ABORT dup): sqlite ERRORS tbl.a (verified incl. rowid parity
    1|1|2|10 / 2|6|3|4). Engine reports bare "tbl" — the update
    conflict path (update.go uniqueConflictError) falls to the
    column-less message; needs violated-column keying like conflict
    tranche 7c7b77a0c did for the other update path.
- **T7.3 CLOSED (2026-09-11): autovacuum GREEN — two root causes**
  - (1) ENGINE (internal/pager/pager.go allocateExtendLocked): the extend
    allocator treated an OVERRIDDEN pending-byte page as an ordinary usable
    page ("SQLite's own overflow chains and root lists use it" — a misreading
    of autovacuum-2.4.5, whose expected root list EXCLUDES the overridden
    page 65 exactly like ptrmap pages 207/412). btree.c:6740 skips
    PENDING_BYTE_PAGE on EVERY end-of-file increment, override or not
    (TESTCTRL_PENDING_BYTE moves WHERE the lock byte lives, not WHETHER the
    page is usable). Pre-fix, an index leaf was allocated AT page 65; the
    drain later relocated its overflow children through the stale ptrmap
    entry (Overflow1, parent=65) and failed "update parent 65: database
    disk image is malformed" (1,075 cascading errors from autovacuum-1.1.15
    on). Fix mirrors btree.c:6740-6766: unconditional pending-byte skip +
    re-check after the ptrmap-page skip; the reserved page is materialized
    zeroed so file_pages still counts it.
  - (2) TRANSPILE (tools/tcl2go/cmdexpr.go info-exists dynamic-key handler):
    the literal-vs-variable decision was made AFTER stripping the `$` sigil,
    so `unusable_page($i)` emitted a LITERAL `"i"` key lookup — the 2.4.5
    expected root list then included the unusable pages. Fix: decide on the
    RAW key (HasPrefix "$") before stripping, matching stringexpr.go's
    established pattern. Regen delta: 7 files (autovacuum + thread/notify/
    indexfault fixtures that share the template).
  - UCL: TestNativePendingBytePageNeverAllocated (frigolite_autovacuum_native_test.go)
    pins the contract: page 65 stays zeroed, drains never consult it, roots
    skip past it. Gates: build/vet/SOLID/race green; vacuum/autovacuum/
    incrvacuum/backup/memdb1/reservebytes/without_rowid1 families green
    serially; changed-file quality gate shows zero NEW violations (pager.go
    2120 lines and cmdexpr.go 1421 lines over the hard gate pre-existing,
    deferred per §5c).
  - RESIDUE (new item, owned by the next tranches): the UCL sequence
    WITHOUT the between-deletes integrity_check reads leaves one orphan
    tail page per drain from ~delete 13 on ("Page N: never used",
    freelist_count 0 — a live overflow-chain free lands one page short
    when no read interleaves). Not hit by the TCL shape (integrity_check
    runs between deletes); needs the same btree.c-parity treatment as the
    delete-path overflow free (findOverflowChain caller in the DELETE path).
- **T7.4 CLOSED (2026-09-11): pragma2 GREEN — stale genPreDeleted transpiler bug**
  - The 5.1 failure ("file is not a database" at the post-4.8 reopen) was
    NOT pager state: the TCL resets with `db close; forcedelete test.db;
    sqlite3 db test.db`, but the generated test never emitted the remove —
    5.1 then reopened the OLD 1024-page database and the page_size=16384
    setter (fresh-db-only) misfired into header validation.
  - Root cause (tools/tcl2go): the preamble hoists leading file deletes into
    the genPreDeleted registry; processFileDelete (forcedelete) consumes a
    matching entry ONCE, but processDeleteFile (delete_file — which emits its
    own os.Remove for every arg) never cleared the registry. pragma2's
    file-top `delete_file test.db` therefore left a stale entry that
    silently swallowed the mid-file 5.1 `forcedelete test.db`. Fix:
    processDeleteFile deletes its args from genPreDeleted (the body remove
    supersedes the hoisted one). Same class as the walpersist
    "forcedelete -wal -shm dropped" gap noted in the g7 diagnosis.
  - Regen delta: exactly 1 line (pragma2: os.Remove("test.db") before the
    5.1 reopen). Gates: build/vet/SOLID green; standard JSON suite
    unchanged (same 7 top-level failures as HEAD); pragma2 serial 0.43s.
- **T8 tranche (2026-09-11): nested-BEGIN + transpiler file-reset sweep —
  trans/avtrans/delete4/transitive1/triggerupfrom GREEN**
  - ENGINE (internal/exec/transaction.go execBegin): BEGIN inside an active
    transaction now errors "cannot start a transaction within a transaction"
    BEFORE any lock work (build.c sqlite3BeginTransaction's
    db->autoCommit==0 check; oracle-verified 3.51.0). Closes the g7
    nested-BEGIN class (trans-4.6, avtrans, lock3-3.x). trans-4.9's residual
    empty-msg was TRANSPILE: `catch {execsql {END; SELECT ...}}` binds the
    batch's ROWS to msg (tclsqlite.c), but bodyEndsWithExecsqlSelect only
    matched single statements — added sqlBatchEndsWithRowStmt (comment-aware
    scan; the LAST statement of the batch decides).
  - TRANSPILE (tools/tcl2go gen.go sourceLeadingDeletes): the leading-region
    scan stopped only at "\\ndo_test ", so do_execsql_test-driven files
    (delete4.test) had their WHOLE body classified as head — every mid-file
    forcedelete was hoisted into the preamble and the body occurrences
    silenced, so close/forcedelete/reopen resets reused stale databases
    ("table t1 already exists"). The scan now stops at the first
    do_execsql_test/do_catchsql_test/do_eqp_test/do_realnum_test/
    do_nullid_test marker too. genPreDeleted became a COUNT map (a path may
    legitimately be hoisted several times) consumed by processFileDelete and
    processDeleteFile alike.
  - Regen delta: 16 files. GREEN flips: trans, avtrans, delete4,
    transitive1, triggerupfrom (+ lock3 re-confirmed). Still-failing among
    the touched set (csv01, e_reindex, e_resolve, misc8, zipfile, lock,
    savepoint) all pre-existing reds with diagnosed root causes (g1/g2/g4
    reports). Gates: build/vet/SOLID green; staticcheck unchanged (10
    pre-existing findings in internal/exec + tools/tcl2go); -race native
    suite green.
- **T9 tranche (2026-09-11): parser xfullname rule-123 fix + DML alias
  plumbing + WR upsert DO UPDATE write path — upsert2/upsert3 green**
  - PARSER (internal/parse/parser_rules2.go rule123): the grammar's rule 123
    is `xfullname ::= nm DOT nm AS nm` (sql_tables.go: lhs 265, nrhs 5) but
    its handler was the joinop-flavored action reading RHS1 as a JOIN_KW —
    every schema-qualified aliased DML target ("INSERT INTO main.t1 AS
    t2(a,b)") produced a zero joinOp as the table name ("no such table:
    { false false}"). Fixed to yield "schema.table". Diagnosed via
    stack-trace + unhandled-rule tracing (the sibling fallback diagnostics
    are removed).
  - PARSER alias plumbing: the xfullname productions DROP the AS alias
    (the value is a plain "schema.table" string), so InsertStmt/UpdateStmt/
    DeleteStmt .Alias stayed empty and DO UPDATE alias references failed
    ("no such column: t2.c" — upsert3 class). Added Parser.pendingDMLAlias
    (set by rules 122/123, read-and-cleared by the DML statement rules
    152/159/164/165), replacing the RawSQL re-scan workaround path
    (fixupDMLTableAlias remains for re-parsed schema text).
  - ENGINE WR (internal/execdml/insert.go writeUpdatedRow): the upsert
    DO UPDATE write path was rowid-only — WITHOUT ROWID tables now route
    through the PK-identity delete + PK-first CellIndexLeaf re-insert
    (mirroring writeUpdateCell's WR branch). This was the source of the
    "database disk image is malformed" corruption after DO UPDATE on a WR
    table (upsert2-104 etc.).
  - ENGINE upsert alias row keys: buildUpdatedRow/upsertWhereAllows expose
    alias-qualified keys ("t2.c") alongside bare/excluded keys so SET and
    WHERE expressions resolve the target alias.
  - Status: upsert2, upsert3 GREEN (were corruption-failing);
    without_rowid3 12→3 failing assertions (remainder: WR FK ON UPDATE
    CASCADE + CHECK-on-cascade, next tranche); upsert1 2 fails (600/610 WR
    INSERT column-list→PK-first storage mapping), upsert4 2 fails (WR DO
    UPDATE unique-check shapes), upsert5 improved, conflict2/without_rowid4
    unchanged (WR PK/UNIQUE enforcement on UPDATE — next tranche).
    Oracle-verified against sqlite3 3.51.0 for the DO UPDATE WHERE/SET
    alias shapes.
- **T10 INVESTIGATION (2026-09-11, open — WR named-column insert corrupts
  the table root page; upsert1-600/610 residue)**
  - Repro: `CREATE TABLE t1(b UNIQUE, a INT PRIMARY KEY) WITHOUT ROWID;
    INSERT INTO t1(a) VALUES('1'); PRAGMA integrity_check` → "NULL value in
    t1.a". The FULL-tuple insert (`VALUES('x', 3)`) is CLEAN; only the
    named-column-list path corrupts.
  - On-disk evidence (raw page dump): the WR table root (page 2) starts with
    a raw record image `03 03 09 00 00 00 00 00` where the 0x0a index-leaf
    header should be; page 3 (sqlite_autoindex) is a properly initialized
    empty 0x0a page. I.e. a cell/record was written at offset 0 of the
    (empty) table root instead of at the cell-content offset with a proper
    header.
  - Path facts verified: mapNamedTupleValues produces the correct
    declaration-order full row [NULL,'1']; writeTableRow reorders via
    WithoutRowidStorageOrder — order computed correctly ([1,0] with
    cd.PrimaryKey=true); reads (SELECT a,b) return the logically correct
    row, so the read path compensates for the mis-written storage.
  - Suspects: NullIPKAliasForWrite (does it mis-alias the PK slot for WR
    tables?) or the empty-page fast path in the btree index-leaf insert
    (internal/btree/btree_insert.go) writing at offset 0 when cellcontent
    == pageSize. Next step: hexdump + instrument InsertCell for the
    named-column case, compare with the full-tuple case byte-for-byte.
- **T11 tranche (2026-09-11): CREATE TABLE collation validation — collate7
  green, collate3 1.2 fixed**
  - ENGINE (internal/execddl/ddl.go): runCreateTableValidations gains
    validateTableCollations — column-level COLLATE names and table-level
    PRIMARY KEY/UNIQUE column collations must resolve against the
    connection's collation registry at CREATE time (build.c
    sqlite3AddCollateType; oracle "no such collation sequence: NAME"
    verified on 3.51.0). Reuses the DDLContext.CheckCollationString seam
    the CREATE INDEX validation (P2.INDEX) already uses.
  - collate7 GREEN; collate3 1.2 green; collate3-1.1/1.1.2/1.3 (explicit
    COLLATE in SELECT/CREATE INDEX) were already green.
  - REMAINING (collate3-2.x class, open): after close+reopen WITHOUT
    re-registering a schema-referenced collation, statements that RESOLVE
    that collation (ORDER BY c1, WHERE c1=, explicit COLLATE) must fail
    with "no such collation sequence: NAME" (SQLite errors at prepare via
    sqlite3LocateCollSeq). The engine silently falls back to BINARY.
    Fixes must NOT fire for statements that don't need the collation
    (bare SELECT * FROM t must keep working — check the TCL for the exact
    success expectations). Entry points: wrapValueForRowMap
    (internal/execquery/helpers.go:121), sort-key collation resolution
    (select_expr.go:351 mapCollations), comparison dispatch.
### T12 diagnosis index (2026-09-11) — full-suite drift root-cause classes

All eight family groups diagnosed by parallel agents (reports:
/tmp/frigolite_diag/report_g{1,2,3,4,5,6,7,8}.md — ephemeral; this index is
the durable extract). Package → class mapping per group report. ENGINE
classes (fix tranches), highest impact first:

1. Rowid-alias (IPK) reads NULL under WHERE-filtered/indexed scans, SELECT *
   (g3 class 6: regexp1; g4: indexexpr1, tableopts, altercons; g1: whereA).
   Top priority — likely one read-path seam.
2. WR named-column insert corrupts table root (T10, fix in flight).
3. WR PK/UNIQUE unenforced on UPDATE + WR FK ON UPDATE CASCADE
   (conflict2, without_rowid3/4; g2).
4. Scalar subquery with aggregate-expression term mis-evaluated / FILTER
   bare-column mixups (randexpr1, filter1; g3 class 1).
5. TEXT→REAL coercion integer-only ('4.5'+0 → 0) (tkt_a8a0d2996a; g3).
6. misc1: ~950-byte schema record written as fully-local page-1 cell
   (1024B page) → undecodable sqlite_master (g2).
7. DML name/function resolution missing (SELECT validates, DML doesn't):
   update, delete_pkg, insert, insert3, triggerB, misc4/5 (g2 class B).
8. Missing prepare-time validations: function arity (limit, select1/5),
   ORDER BY/GROUP BY ordinal range (select1/3, tkt2822), join ON/USING
   guards (join ×8, tkt3935), aggregate-in-WHERE misuse (tkt1514/3508),
   compound 500-term limit (select7), nested-aggregate semantics
   (aggnested, aggorderby), resolver alias precedence (resolver01).
9. Window-frame boundary computation (windowB/E/fault; g1 class 1).
10. DELETE/UPDATE ORDER BY LIMIT grammar (wherelimit, wherelimit2).
11. Conflict clauses: UNIQUE IGNORE (null), NOT NULL REPLACE+DEFAULT
    (notnull), per-column ON CONFLICT on INSERT (conflict3).
12. FK: parent/column validation + RESTRICT-before-trigger + CASCADE
    re-CHECK regression (e_fkey, fkey2), authorizer firing (alterauth).
13. Collation: prepare-time resolution after reopen (collate3-2.x — T11),
    index-level COLLATE in UNIQUE (collate4).
14. vtab: DDL guards (vtab5), created-vtab join column resolution (vtab6,
    tkt3121), per-connection module registry (vtab_shared),
    recursive-CTE inner-join rescan (closure01).
15. Nested-txn visibility in Query context + write-during-read (misc8);
    in-scan DELETE NULL semantics (delete2, delete_pkg).
16. Storage/lexer: nan 4-byte cell over-reservation; lexer partial
    exponents/unterminated comments (tokenize); func4 affinity saturate;
    tkt_4a03edc4c8 REPLACE+FAIL ordering; tkt_fc62af4523 journal-mode
    locking; tkt_2a5629202f qualified multi-key ORDER BY;
    tkt_54844eea3f outer-ref in FROM-subquery; tkt_78e04e52ea quoted
    empty name; view/indexed errors (view, view3, indexedby).
17. Testing-pragma/test-control gaps (low prio): PRAGMA
    optimization_control (tkt_80ba201079), TESTCTRL_LOCALTIME_FAULT
    (tkt_bd484a090c), percentile/zipfile message parity.

TRANSPILE/supersession candidates (Pure-Go supersession policy; ~20 pkgs):
thread003-005, notify2, init, mutex1, pcache2, misuse, loadext,
permutations, shell1, shell6, avfs, trans2, e_droptrigger, e_dropview,
e_reindex, index2, fkey1, rowid, bigrow, update2, vtab1, bind, ptrchng,
bestindexA/D, csv01, func3, qrf01-03, tkt2565, trace, trace3, tkt3992,
tkt_f777251dc7a, func_pkg(md5/UDF parts), pcache, shortread1, sort5,
chunksize, altertab2 (harness flatten asymmetry).
- **T13 tranche (2026-09-11): aggregate-in-WHERE misuse — tkt1514/tkt3508
  green**
  - ENGINE (internal/execquery/select_agg_validate.go validateWhereExprs):
    two resolve.c-parity checks added. (1) whereDirectAggregate: a scalar
    aggregate used directly in this level's WHERE errors "misuse of
    aggregate: X()" (resolve.c clears NC_AllowAgg for the WHERE subtree);
    the walk stops at Subquery/EXISTS nodes (nested WHEREs are validated
    against their own scope), and respects the min/max dual-nature
    (2+ args = scalar, builtin.c) plus arity ordering (count(f1,f2)
    reports the arity error, not misuse — select1-3.9). (2) A WHERE
    reference to a SELECT alias whose expression is an aggregate resolves
    to that aggregate → same misuse (tkt3508 "where c > 1" with
    count(x) AS c).
  - Gates: build/vet/SOLID green; select1/where/randexpr1 failing-assertion
    sets byte-identical to HEAD (diffed, no regression); tkt1514, tkt3508
    serially green.
- **T14 tranche (2026-09-11): vtab DDL guards — vtab5 green**
  - ENGINE (internal/execddl): CREATE TRIGGER on a virtual table now errors
    "cannot create triggers on virtual tables" (build.c
    sqlite3CodeRowTriggerDirectly) and only INSTEAD OF triggers are allowed
    on views ("cannot create BEFORE trigger on view: vv"); CREATE INDEX on a
    virtual table errors "virtual tables may not be indexed" (build.c
    sqlite3CreateIndex) instead of scanning the shadow storage as a btree
    ("database disk image is malformed"). Guards detect vtabs via
    IsStoragelessVirtualTable || RootPage==0 (oracle-verified 3.51.0).
  - No regression: vtabE/vtabH/vtabK/temptrigger/indexA/tabfunc01/e_walckpt
    green; altertab/trigger1/index2 failures pre-existing (diagnosed in
    T12 index: trigger DDL validation class + transpile mis-lex).
  - g6 rtree diagnosis also complete (last group) — class map added to T12
    index (rtree constraint-pushdown, aux columns, rowid resolution,
    float32 rounding, 2nd-gen geometry API, geopoly decision needed).
- **T15 tranche (2026-09-11): lazy-decode IPK substitution — tableopts,
  whereA green**
  - ENGINE (internal/execquery/select_scan_helpers.go
    fillStructRowRemainingFromTypes): the two-phase lazy decode filled the
    INTEGER PRIMARY KEY rowid-alias from the rowid in phase 1
    (applyStructRowAffinity) but the phase-2 remaining-columns decode
    re-read the stored NULL from the record and OVERWROTE it — any filtered
    scan showing `SELECT *` (alias column in the remaining set) returned
    NULL for the rowid alias. Phase 2 now re-applies the substitution after
    decoding. The indexed-scan variant shares the seam.
  - tableopts, whereA GREEN serially; regexp1 9→5, indexexpr1/altercons
    improved (remainders are the DQS-fallback class per the T12 index).
  - No regression: the failing-assertion sets of select1/select2/select3/
    select5/where/where2/join/subquery/insert/update/null/distinct are
    byte-identical before/after (49 bodies, diffed).
  - Gates: build/vet/SOLID green; quality gate on the changed file clean.
- **T16 tranche (2026-09-11): TEXT real-prefix arithmetic promotion —
  tkt_a8a0d2996a green**
  - ENGINE (internal/execexpr/expression_eval.go addValues): vdbe.c
    numericType classifies a TEXT/BLOB operand whose leading numeric prefix
    is a REAL ("4.5") as MEM_Real, which forces the REAL add path even when
    the other operand is an integer. The engine's both-integers shortcut
    never consulted the text prefix type, so '4.5'+0 was INTEGER 0. Added
    hasRealNumericPrefix promotion (integer prefix and no-prefix operands
    keep the int path — oracle-verified: typeof('100x'+1)=integer,
    typeof(0+'abc')=integer, typeof(0+x'00')=integer, '4'+3 integer,
    '100x'+'4.5y'=104.5 on 3.51.0; 0+matchinfo(...) stays INTEGER 0).
  - tkt_a8a0d2996a GREEN. func4 unchanged (its affinity-saturation class is
    separate). func3/nan/randexpr1/misc8 failing sets identical to HEAD.
  - RESIDUE: subtract/multiply/divide paths may need the same real-prefix
    promotion for non-ticket shapes ('2.5'*2); subValues has deliberate
    int-prefix precision handling (tkt_a8a0d2996) — extend carefully with
    oracle evidence.
- **T17 tranche (2026-09-11): window PARTITION BY column resolution +
  per-row group_concat separator — windowB green**
  - ENGINE (internal/function/function_aggregate.go groupConcatAgg): the
    separator was a single field overwritten at every Step and applied at
    Final — for window frames (or any group) whose separator argument varies
    per row, EVERY junction got the LAST row's separator. func.c
    groupConcatFinalize instead prefixes each element with ITS OWN row's
    separator: seps are now stored per element. Oracle-verified on
    windowB-20.x (group_concat('-', x) OVER (... ROWS 1 PRECEDING/1
    FOLLOWING) → "-22-", "-22-333-", "-333-4444-", "-4444-").
  - ENGINE (internal/execquery/window.go windowPartitions): PARTITION BY
    expressions now validate bare column references against the FROM row
    space and error "no such column: NAME" (windowB-19.x fake_column).
    Skipped when an outer row scope exists (e.outerRow/outerRows): window
    ORDER BY and PARTITION BY may be correlated outer references
    (window1-55.x, window1-44.x) — those are not local errors.
  - windowB GREEN; full window family window1-9/B/C/D/pushd green;
    windowE/windowfault unchanged (5 assertions, the RANGE-frame boundary
    class — next tranche). Gates: build/vet/SOLID green, -race native suite
    green, quality gate clean.

- **T10 CLOSED (2026-09-11): WR named-column insert did NOT corrupt storage —
  PRAGMA integrity_check misread PK-first records as declared-order**
  - The T-log's page-dump evidence was an artifact: a proper dump (pageSize
    from the file header, cell at the header cell-pointer offset) shows page 2
    is a valid 0x0a index-leaf whose single cell is `03 03 09 00` — payload
    len 3 + record [const-1, NULL] = PK-first [a=1, b=NULL]. sqlite3 3.51.0
    on the identical statements writes the byte-identical cell
    (`03 03 09 00`) and the identical full-tuple cell (`05 03 01 0f 03 78`).
    Probed all four write paths — plain INSERT INTO t1(a), full tuple,
    INSERT OR IGNORE ... ON CONFLICT(a) DO NOTHING, plain ON CONFLICT DO
    NOTHING: every path writes identical, oracle-identical storage. The
    write pipeline (mapNamedTupleValues → ReorderToStorage([1,0]) →
    NullIPKAliasForWrite no-op for WR → EncodeRecord → CellIndexLeaf insert)
    is correct; NullIPKAliasForWrite and the btree empty-page fast path were
    exonerated.
  - Root cause: internal/exec/quickCheckTable decoded the index-leaf record
    and fed it straight to buildRowMapFromValues, mapping storage slot 0 to
    DECLARED column 0. Stored [1, NULL] therefore read as b=1, a=NULL and
    quickCheckNotNull emitted "NULL value in t1.a" for a healthy image. The
    SELECT scan path already permutes at decode (execquery
    RemapWRRecordToDeclared); the integrity scan now does the same for
    WITHOUT ROWID tables (pragma_quickcheck.go, hasWithoutRowidKeyword →
    e.selectEngine.RemapWRRecordToDeclared before the row-map build).
  - Verification (isolated git worktree pinned at 895fa61a5, fix vs pristine):
    - testgen/upsert1 2 → 0 failing assertions (upsert1-600/610 green);
      all 12 target packages re-run (upsert1/4/5, without_rowid1-7,
      conflict2/3): every other package's assertion count UNCHANGED (its
      residue is the documented WR DO-UPDATE-unique / FK-CASCADE /
      ALTER-REFERENCES tranches, not integrity_check).
    - Root harness TestSQLiteSuite: green with the fix (failure set
      identical to pristine). TestBackupConformance and
      TestNative{BtreeDivider,WalCheckpointPassive}FixtureReference fail
      IDENTICALLY with and without the fix — pre-existing mainline state
      from concurrent tranches, not this change.
    - go build ./... / go vet ./... / go test -run TestSOLID_ ./... green;
      -race TestNative|TestWR unchanged; tools/quality_gate.sh on
      pragma_quickcheck.go: findings byte-identical to HEAD's version
      (pre-existing quickCheckTables/checkFreelistCount/execQuickCheck
      complexity overages; no NEW violations).

### T13 FTS flush/crisis-merge: false-positive corruption checks fixed (2026-09-11)

Class (g5 report class 1): the FTS4 flush/crisis-merge segment accounting —
diagnosed as "merged/written segment stores wrong content vs the oracle" —
turned out to be THREE false-positive "database disk image is malformed"
emitters aborting the write path mid-flight, NOT a writer content bug. With
the emitters fixed, frigolite's flush and crisis-merge outputs are
BYTE-IDENTICAL to /usr/bin/sqlite3 3.51.0 on the previously failing shapes.

Byte evidence (probes under /tmp/probe_ftsres, oracle CLI as ground truth):

- Scenario A (fts4merge minimal): page_size=512, 16 autocommit inserts of a
  600-char single-token doc. Oracle: 16 root-only segments, end_block
  "0 607", %_segments empty. Engine before: 0 segdir rows (flush dropped
  every batch); after: 16 root-only 607B segments, root bytes and
  %_segments byte-identical to the oracle (cmp clean).
- Scenario C (fts4growth minimal): page_size=1024, 40 Genesis verses, one
  INSERT..SELECT per autocommit. Oracle: 8 L0 roots + L1 idx0 (leaves 1-2,
  4-byte root) + L1 idx1 (leaves 3-5, 9-byte root), 5 %_segments rows
  totalling 3700 bytes. Engine before: "malformed" from insert 25
  (docid 1001025 — the exact fts4growth test site), 8 L0 rows, one L1 row.
  After: completes all 40 inserts; segdir geometry, all root blobs and all
  %_segments blocks byte-identical to the oracle (cmp clean on
  group_concat(quote(block)) and per-row quote(root)).

Root causes and fixes:

1. Pager committed a stale page-1 header when a mid-cycle allocation grew
   the file (internal/pager/pager.go). Within one flushAllCtx cycle, page 1
   could flush BEFORE a higher page; growHeaderSizeLocked then raised the
   in-memory header and re-dirtied page 1, but the end-of-cycle dirty wipe
   dropped the mark — the file ended with nPage 47 on disk while page 48
   existed ("invalid page number 48" in integrity_check; page_size=512 FTS
   builds). Fix: flushOrderLocked() writes all dirty pages ascending and
   page 1 LAST (sqlite3PagerCommitPhaseOne stamps the change counter after
   the page-list write). Deterministic order also de-flakes commit images.
2. HeaderBeyondFile ran mid-write (internal/pager/external.go). lockBtree's
   nPage>nPageFile check belongs to shared-lock time, when the connection
   cannot have in-flight pages; frigolite consults it per statement and from
   mid-flush schema lookups (schema.Manager.FindTable → ValidateHeader),
   where our own overflow allocation (growHeaderSizeLocked) makes the
   in-memory header lead the not-yet-extended file → false "malformed". This
   is what silently swallowed the page_size=512 segdir row (Fix A's
   companion; the error was eaten by writeFTSShadowRowRaw's `_ =`). Fix:
   return false while the pager holds its own dirty pages.
3. ValidateFreelistForGrowth treated an all-zero trunk first-8-bytes as
   corruption (internal/exec/ddl_forward.go). A VALID empty trunk is
   next=0/k=0 — exactly 8 zero bytes (freePage2's tail block on a fresh
   freelist). After the first crisis-merge chomp the freelist drains back to
   a single empty trunk, and then EVERY INSERT..SELECT failed at statement
   start (insertSelectIntoFTS → ValidateFreelistForGrowth): fts4check's
   4617-site loop and fts4growth's 2.2/4.x loops. Fix: apply C's only
   structural bound (btree.c freePage2/allocateBtreePage: nLeaf >
   usableSize/4-2 → corrupt); a zeroed trunk (k=0) is valid, as in C.

Also re-landed the merge cont-rewrite rowid-cursor sync (lost with the prior
session's reverted instrumentation; P6.FTS-RESIDUE "segdirNextRowID sync"
item): syncSegdirRowID raises the explicit-rowid cursor after the
cont-rewrite's fresh scan so a later fresh-branch write in the same MergeFTS
call cannot reuse the rowid and silently REPLACE the live segment row
(internal/execddl/export_fts_merge.go).

UCL (portplan/UNIT_CONFORMANCE.md): two new committed scenarios +
oracle fixtures (tools/orafixture):
- fts-page512-oversized (page_size=512, 600-char single-token docs — pins
  the oversized-term root-only flush, previously dropped entirely);
- fts-genesis-rowload (page_size=1024, 40 Genesis verses via per-docid
  INSERT..SELECT — pins the crisis-merge freelist drain end-to-end).
Both PASS byte-for-byte (segdir geometry + root blobs + %_segments blocks)
under TestWriterConformance, including under -race (no data race; the only
-race failure is the pre-existing fts-x6-growth assertion below).

Verification (go test -tags testgen, -count=1):
- fts4check FAIL→PASS (216s → 158s, was 4617 failing sites at line 312).
- fts3integrity, fts4merge2, fts4merge3, fts4merge5, fts4aa, fts4intck1,
  fts4langid, fts3prefix, fts3aa/ab/ac/b, fts4noti, fts3drop: green.
- fts4opt FAIL→PASS (merge=5,2 loop no longer trips the mid-cascade
  validation; 24s).
- No regressions: fts3defer, fts4opt, fts3drop, fts4noti failure sets
  identical/empty before vs after; build/vet/SOLID green.

Remaining residue (NOT this class, next session):

- fts4growth 2.3-2.7/4.x-5.x (10 assertions) and fts4merge4 2.2.x and the
  pre-existing UCL fts-x6-growth scenario: the flush-time AUTOMERGE grind
  class — oracle does ONE level-grind per call with partial merges
  (negative end_block sizes: "5588 -3950" → "-11766" → "-15541") and L2
  reaches 6 segments where ours reaches 11; ours completes pairs instead of
  truncating at the nRem cutoff. Quota accounting verified equal (372).
  Entry: MergeFTS iteration loop + flush automerge gate
  (internal/execddl/export_fts_merge.go, export_fts_flush.go).
- fts4merge 5.9-5.11 (3 assertions; original 4.3.1 site FIXED): transpiler
  gap — the TCL `set L [expr 16*16*7+16*3+12]` then `... LIMIT $L` inside
  execsql braces is emitted as a literal `$L` string, so the engine sees an
  unbound parameter ("datatype mismatch", which matches real SQLite for an
  unbound param — the TCL layer must substitute). tcl2go substitution for
  set-vars inside do_test SQL bodies; supersession/native-port candidate.
  NOTE: the oracle CLI binary reserves 12 bytes per page (usableSize 500 at
  page_size 512), so byte-parity comparisons must target BLOB CONTENT
  (segdir roots / %_segments blocks), not page-layout offsets.
- Pre-existing integrity_check noise, separate btree defect: an in-place
  cell replacement can leave a 4-byte gap that is neither a freeblock nor
  reflected in the page's fragmented-byte counter ("Fragmentation of 4
  bytes reported as 0 on page N"). Minimal repro: page_size=512, plain
  table, INSERT OR REPLACE of a growing blob ×16. Reproduces on HEAD
  (pre-fix) — pre-existing, writer/checker convention mismatch (writer
  reserves a pageSize-4 cell tail the checker counts as unaccounted).
- **T18 tranche (2026-09-11): wherelimit GREEN, wherelimit2 5→3**
  - ENGINE (internal/parse/update_delete_limit.go, new): the
    SQLITE_ENABLE_UPDATE_DELETE_LIMIT prepare-time rule (delete.c:201 /
    update.c:212) — a DELETE/UPDATE with a top-level ORDER BY and no
    top-level LIMIT errors "ORDER BY without LIMIT on DELETE"/"on UPDATE".
    Lexical scan (strings/comments/parens aware, WITH header stripped)
    before the LALR parse, whose tables only accept ORDER BY with LIMIT.
  - ENGINE (internal/execdml/update_split_tail.go applyUpdateOrderLimit):
    LIMIT-window survivors were keyed by rowID — WITHOUT ROWID changes all
    carry the synthetic rowid 0, so every change matched the keep-set and
    LIMIT updated ALL rows (wherelimit2-2.x). Keyed by the oldValues slice
    identity instead (unique per matched row).
  - ENGINE (internal/execdml/update_apply.go validateDMLAliasQualifier,
    wired into update_split.go + delete.go): with "UPDATE t1 AS a", the
    original table name is not a valid WHERE/SET qualifier — "no such
    column: t1.x" (wherelimit-0.5.2).
  - wherelimit GREEN (7/7). wherelimit2 residue (3 assertions, exotic):
    ORDER BY/LIMIT through INSTEAD OF view triggers (5.1-5.5) and a
    CTE-aliased DELETE target with rank()OVER() in ORDER BY (5.6).
  - No regression: update/update2/without_rowid3/without_rowid4/delete4/
    trans failing sets identical to HEAD. Gates: build/vet/SOLID green.
- **T19 tranche (2026-09-11): scoped quick_check skips page-usage audit —
  strict2 green**
  - ENGINE (internal/exec/pragma_quickcheck.go): the multi-line page audit
    (Tree N page M cell K / Page N: never used) now runs only for FULL-scope
    checks. btree.c gates the page-usage loop behind !bPartial — a
    TABLE-SCOPED check (quick_check('t1')) skips it entirely. Oracle-verified
    (3.51.0) on the writable_schema shared-root image: scoped = "ok", full
    check reports "2nd reference to page 2" x2 + "Page 3/4: never used".
  - strict2 GREEN. No regression: corrupt/corruptB failing sets identical to
    HEAD (corrupt2/check/quick/intarray/pragma3 green).
  - NOTE: the earlier "delete-path overflow leak" hypothesis (T-log T7.3
    residue) is RESOLVED BY the T7.3 allocator fix — re-probe with/without
    interleaved reads is clean in both variants; no separate defect.
- **T20 tranche (2026-09-11): DML prepare-time name resolution — wherelimit
  green; update/insert/insert3/delete_pkg 9 assertions fixed, 0 regressions**
  - ENGINE (internal/execdml/dml_validate.go, new; context.go LookupFunction;
    exec/dml_forward.go): INSERT/UPDATE/DELETE now resolve every column
    reference and function name in WHERE/SET at prepare time (resolve.c
    parity) — "no such column: z", "no such function: nosuchfunc",
    "misuse of aggregate: max()". NEW./OLD. trigger pseudo-qualifiers are
    always valid and skip the column lookup (trigger rows carry the view's
    columns); subquery bodies are skipped (own scope); UPDATE...FROM skipped
    (join columns).
  - ENGINE (insert_core.go): named INSERT column lists validate against the
    table — "table t has no column named z" (build.c
    sqlite3AddColumnToList).
  - ENGINE (insert_core_tail.go): the VALUES-tuple arity error for a NAMED
    column list drops the table prefix — oracle "4 values for 2 columns"
    (insert-1.3c/d green).
  - wherelimit GREEN (7/7; T18's parse check + alias masking + this tranche).
    4-package diff vs HEAD: 21→12 failing assertions, deletions only.
  - Residues (documented classes): trigger-WHEN unknown-column validation at
    CREATE (insert3-131/143, update-1071/1083), in-scan DELETE NULL
    semantics (delete-9.x), trigger-body column refs (insert-419),
    wherelimit2 5.1-5.6 view-trigger/CTE shapes, update rowid-shift spurious
    UNIQUE (1039/1057 — pre-existing, NOT caused by this tranche; verified
    by full-set stash diff).
- **T21 investigation (2026-09-11, open — misc1 schema root-cell placement)**
  - Repro: `CREATE TABLE manycol(x0 text, ..., x99 text)` (100 cols; schema
    record 938 bytes) succeeds, then ANY statement touching the schema fails
    "database disk image is malformed" (misc1's ~39 assertions cascade).
  - Ruled out: prepareCell's overflow math is correct (938 ≤ maxLocal 989 →
    fully local is the SQLite-correct local/overflow decision; SQLite
    reconciles it by SPLITTING the root so the cell lands on a child page).
  - Working hypothesis: the page-1 (schema root) split path — leafHasRoom
    says "no room", but the resulting on-disk page-1 cell sits at
    contentStart=914 with the full 938-byte local payload (byte range
    914..1852 crossing the 1024-page end) instead of the cell moving to a
    freshly allocated child leaf. An earlier quick-dump's "payloadLen=5383"
    reading is unreliable (T10's lesson: decode cells via the page header's
    cell-pointer offsets and double-check varint boundaries before
    theorizing).
  - Next steps: byte-parity the page-1 layout against the 3.51.0 oracle for
    this exact repro (oracle page 1 = header + interior root with one child
    leaf holding the 938-byte cell), then fix the schema-root split path in
    internal/btree (insertPage/insertLeafPage/relocateRootSplit family).
- **T22 tranche (2026-09-11): trigger WHEN column resolution at fire time —
  insert3 green, update 10→4 failures**
  - ENGINE (internal/execdml/insert_trigger_exec.go triggerWhenPasses): the
    WHEN clause was evaluated with a nil row, so unknown columns silently
    evaluated NULL and every row passed. resolve.c resolves the trigger
    program against the subject table's columns when the firing statement is
    prepared — the engine now validates WHEN column references (bare and
    NEW./OLD.-qualified) against the subject table at fire time, erroring
    "no such column: NAME" (insert3-131/143, update-1071/1083 green; oracle
    verified: CREATE with WHEN nosuchcol is accepted, the firing INSERT
    errors).
  - insert3 GREEN (3→0). update 10→4 (all four remainders pre-existing:
    sqlite_master guard wording 98, rowid-shift spurious UNIQUE 1039/1057,
    869 — g2 documented classes).
  - trigger1/4/7/e_fkey counts identical with/without the change (7/1/1/24,
    all documented classes). Gates: build/vet/SOLID green; -race TestNative
    green; trigger family + temptrigger/altertab/trigger2 green or unchanged.
- **T21 CLOSED (2026-09-11): schema-root split for oversize local cells —
  misc1 46→10 failures (deletions only, zero new)**
  - ROOT CAUSE (revised vs the open-entry hypothesis — the oracle ran first
    this time): `CREATE TABLE manycol(x0 text,…,x99 text)` produces a 938-byte
    sqlite_schema record, fully local per the file-format formula (938 ≤
    maxLocal 989 at page_size=1024), but page 1's content area is only ~914
    bytes after the 100-byte file header. insertLeafPage's empty-leaf branch
    then REDUCED the cell's LocalLen to minLocal and spilled the rest to
    overflow — a cell whose local/overflow split CONTRADICTS the formula.
    Frigolite's own formula-based reader (and the sqlite3 oracle) computes
    local=938 for plen=938 and reads past the page end → "database disk image
    is malformed" on every later schema touch (misc1's ~39 malformed
    assertions were this one CREATE's cascade). SQLite NEVER shrinks local
    below the formula: it reconciles via balance_deeper (src/btree.c:9010,
    called from balance() for an overfull ROOT, src/btree.c:9115-9129).
  - ORACLE LAYOUT (3.51.0, page_size=1024, same schema; cells decoded via
    the page-header cell-pointer array): page1 type=5 interior nCell=0
    contentStart=1012 rightMost=3; page2 manycol root (empty leaf); page3
    leaf nCell=1 cell@71 plen=938 rowid=1 FULLY LOCAL end=1012. No overflow
    page. Frigolite post-fix: page1 type=5 nCell=0 rightMost=3; page3 leaf
    cell@79 plen=938 local=938 end=1020 — structurally identical (cell
    offset differs by frigolite's 4-byte sibling-chain convention);
    `sqlite3 t21.db "PRAGMA integrity_check"` = ok (pre-existing 4-byte
    fragmentation-accounting nits only), data reads green via both engines.
  - FIX (internal/btree): new btree_balance_deeper.go —
    balanceDeeperRootLeaf allocates a fresh child leaf parented to the root
    (ptrmap PTRMAP_BTREE parent=page1, btree.c:9028), moves the root's
    cells via writeLeafHalf (rebuilds the b-tree header at the child's
    content offset 0; reparentPageOverflowChains for moved chains), zeroes
    the root's b-tree content (page-1 file header preserved) into an
    interior page with rightmost=child, then inserts the pending cell into
    the child via the normal insertPage machinery (applyChildSplits'
    rightmost branch would wire any child-split dividers). Dispatch in
    insertLeafPage: a cell whose local form cannot fit the page even when
    EMPTY is only possible on a root — `parentPgno == 0 &&
    !leafCellsFit([][]byte{cellData}, coff, pageSize)` routes to
    balance_deeper; the empty non-root case is unreachable by geometry
    (fresh leaf area pageSize-10 > max cell pageSize-20) and now errors.
    The non-conformant reduce-LocalLen hack is deleted. prepareCell gained
    an idempotency guard (the child-level insert re-prepares an
    overflow-form cell without duplicating its chain).
  - VERIFY (assertion-level diff vs the pre-fix stash baseline, cells read
    via cell-pointer arrays per the T10 lesson):
    `go test -tags testgen ./testgen/{misc1,misc3,misc4,misc5,misc8,corrupt,corrupt2,intarray,check}/`
    → misc1 46→10 (36 deletions, 0 additions; remainder = documented
    residue: no-such-table/collation text, CREATE..AS error-text cascade
    643/649/655/667, 19.11/19.12), misc3/4/5/8 and corrupt 1966 failure
    lines IDENTICAL pre/post (zero new), corrupt2/intarray/check ok.
    No-regression: select1 35=35, insert 1=1, create 0=0, without_rowid1
    0=0. `go test ./internal/btree/` ok; build/vet ok; TestSOLID_ ok;
    `-race` TestNative ok; quality_gate on changed files: no NEW
    violations (insertLeafPage gocognit 42→24, gocyclo 23→16, file
    1440→1418 — improved; new functions under thresholds).
  - Residual note: misc1-19.11/19.12 ("got [{}] want [0]") and the
    CREATE-table-already-exists cascade are separate pre-existing classes,
    untouched by this tranche.

### T23 collate3-2.x (2026-09-11) — prepare-time schema-collation resolution

Gap (from T11 remainder): after close+reopen WITHOUT re-registering a
collation that a table's schema references
(CREATE TABLE collate3t1(c1 COLLATE string_compare)), statements that
RESOLVE the collation silently fell back to BINARY. SQLite errors at
prepare via sqlite3LocateCollSeq (build.c/resolve.c); the error fires even
on an empty table, so the fix must be statement-level, not per-value.

SPEC (TCL corpus + SQLite 3.53 oracle via Python sqlite3 create_collation
reopen repro, /tmp scratch): statements that must ERROR: ORDER BY <col /
ordinal resolving to a collated column>, GROUP BY <col>, SELECT DISTINCT
<col>, UNION/EXCEPT/INTERSECT (dedup) over the collated column, compound
UNION ALL + ORDER BY 1 (term inherits the result column's collation),
INSERT into a table with an index on the collated column, UPDATE SET of an
indexed column, DELETE ... WHERE (row-by-row maintains all indexes),
PRAGMA integrity_check (opens every index). Must SUCCEED: bare SELECT *,
UNION ALL without ORDER BY, UPDATE SET c2 (index on c1 — only indexes
whose key/expression/predicate columns are assigned are maintained),
bare DELETE (truncate, no index maintenance), DML on tables where the
collation appears in NO index, integrity_check on such tables.

SEAMS (statement-level, narrowest correct):
- internal/execquery/select_collate.go (new): validateSchemaCollations —
  wired at the end of validateSelectExprs (runs per SELECT level at
  prepare). Resolves ORDER BY/GROUP BY term collations (ordinal → result
  column; bare name → select-list alias, resolve.c precedence; column refs
  resolved against the FROM/join tables' declared collations with
  sqlite3ExprCollSeq-style propagation: COLLATE > first collation-bearing
  function arg/CASE branch/||), DISTINCT + deduplicating-setop
  result-column collations, and compound ORDER BY terms (the parser
  attaches the compound ORDER BY to the TAIL member — the head-level check
  walks the chain). Names keep schema-declared case for the error text
  (collateOperandName/lit values, not the uppercased selectOutputCollations
  map). Explicit COLLATE (validateCollateClause via validateSelectRowValues)
  and WHERE sides (checkWhereCollations) were already covered by earlier
  tranches.
- internal/execdml/dml_collate.go (new): validateIndexCollations —
  statement-start check that every index the statement maintains resolves
  its key collations (IndexKeyCollations: explicit COLLATE in the index SQL
  wins, else the key column's declared collation, else expression
  propagation; indexDef gains the stored SQL). Changed-column analysis for
  UPDATE (rowid assignment maintains all). validateDMLComparisonCollations
  (hooked into validateDMLExprs) resolves WHERE/SET comparison sides and
  COLLATE names — oracle: `UPDATE t SET c2=1 WHERE c1='x'` errors even with
  no index. Call sites: execInsert (nil = all indexes), execUpdate (changed
  set), execDelete via deleteTableContext (only when s.Where != nil).
- internal/exec/pragma_quickcheck.go: execQuickCheck fails with
  "no such collation sequence: NAME" via unknownIndexCollation (all schema
  indexes, optionally scoped to the pragma's table argument), reusing
  execdml.IndexKeyCollations; pragma_analyze.go untouched net (HEAD line
  count restored after the helper initially pushed it past the 1000-line
  hard gate).

VERIFY (assertion-level diffs vs a HEAD `git worktree` baseline — the
shared tree was unusable for A/B: a concurrent agent's runs contaminate
it; see lessons_learned):
- `go test -tags testgen ./testgen/collate3/...`: 22→9 failing assertions.
  All 13 engine-gap failures fixed (2.2, 2.7.1, 2.7.2, 2.8, 2.9, 2.10,
  2.11, 2.17, 3.1, 3.2, 3.4, 3.8, 3.11). The 9 residues are tcl2go
  artifacts, engine-unfixable without violating no-testgen-edits:
  collate3-4.8.2/4.8.3/4.9 (transpiler closed the db WITHOUT the reopen —
  `lindex [catch {sqlite3 db test.db}] 0` elided; execs on a closed
  connection correctly error), collate3-5.2-5.8 (the `db collation_needed
  cfact` lazy-registration callback dropped — "proc definition (not
  transpiled)" — so 'unk' is never registered and cfact_cnt stays 0; 5.0
  proves the same statement errors without the callback), 5.9 (cascade of
  5.7). Per the Pure-Go supersession policy this package is now
  engine-complete; a native port would need a RegisterCollationNeeded API
  (queued, not T23 scope).
- collate4: 1 residue, IDENTICAL set (known index-level-COLLATE UNIQUE
  class). collate7, collate1/2/5/6/8/9/A/B, enc, enc2, enc3, enc4: green.
- No-regression (failing-set diffs vs HEAD, all IDENTICAL): select1 35=35,
  where 105=105, orderby1 1=1, indexA 0=0, reindex 9=9, altertab 4=4;
  broad sweep with1/with2/distinct/distinct2/pragma/quickcheck/corrupt/
  conflict/trigger1/view/e_select/vacuum identical; full JSON harness
  (TestSQLiteSuite subtests) identical in isolated worktrees
  (TestBackupConformance + TestNative*FixtureReference fail in fresh
  worktrees on both sides — untracked fixture dirs — and pass in the main
  checkout with the tranche applied).
- Gates: go build ./..., go vet ./..., go test -run TestSOLID_ ./..., and
  `go test -race -count=1 -run TestNative .` all pass (main checkout).
  tools/quality_gate.sh on the changed files: section verdicts unchanged
  vs HEAD (pre-existing repo-wide staticcheck/complexity residues);
  pragma_analyze.go back to its exact HEAD line count; all new functions
  under the gocognit 15 / gocyclo 12 thresholds (validateSchemaCollations
  split into sort-key/result/compound-ORDER-BY validators;
  validateDMLComparisonCollations split into walk/per-node/sides helpers).
- **T25 investigation (2026-09-11, open — windowE/windowfault sum-overflow
  drift)**
  - windowE 5.2: `SELECT id, sum(x) OVER (ORDER BY id ROWS BETWEEN CURRENT
    ROW AND 2 FOLLOWING) FROM t` with x ∈ {-1, 9223372036854775807, 1, 0.5}.
    The engine raises "integer overflow" (legacy sum semantics). The
    GENERATED testgen expectation says overflow promotes the accumulator to
    REAL (want: 9223372036854775807, 9.22337203685478e+18, 1.5, 0.5) — but
    the CURRENT oracle (3.51.0) returns NEITHER: 9223372036854775807, 0.5,
    -9.22337203685478e+18, -9.22337203685478e+18 (its overflow continuation
    wraps and/or resets per func.c sumStep's ovrfl handling — reverse-
    engineer from src/func.c sumStep/finalize before touching the engine).
  - Disposition: per UCL U1 the current oracle is ground truth; the testgen
    want is an oracle-drift artifact (captured from an older SQLite). The
    engine fix must implement 3.51's exact overflow continuation
    (sumStep p->ovrfl path), NOT the generated expectation and NOT the
    legacy error. Owned by the windowE/fault residue tranche; do not
    attempt without reading src/func.c first.
