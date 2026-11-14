# PLAN_S30 — unit S30, pager1 slices U1-U4 (converter/harness-only)

ADDENDUM (implementation reality, 2026-11-14): two engine deltas beyond the
original converter/harness scope proved REQUIRED by the C contracts; both are
surgical, C-source-pinned, and verified by family sweeps:

- U1-engine (7.1.6.1): the heap-memory WAL locking_mode refusal —
  pager.c sqlite3PagerLockingMode refuses a locking-mode change while
  sqlite3WalHeapMemory holds (wal.c sqlite3WalOpen's bNoShm ← pager.c:7596
  pPager->exclusiveMode; pragma.c:687 reports the PAGER's mode for the SET
  form, db->dfltLockMode for the bare QUERY form). Files: internal/pager/
  {pager.go wallocks.go wal.go} (walExclusiveLatched → walWriter.heapMemory →
  Pager.WALHeapMemory), internal/exec/pragma_state.go (the refusal gate + the
  set-vs-query result split).
- empty-root cursor rule (4.8.x cascade): btree.c:4693 sqlite3BtreeCursor —
  a cursor over the schema root (iTable<=1) on a ZERO-PAGE database becomes
  an empty cursor (iTable=0): a readonly connection cannot bootstrap page 1,
  and C's empty-file schema read yields no rows, never "malformed". Files:
  internal/btree/btree.go (OpenCursor/OpenCursorAtRoot).

U3 outcome reality: 4.5.6/4.7.2/4.8.1/4.8.3's remaining failures are the
triage's B4/B5 dependencies (no on-disk journal substrate = U6; no testvfs
xDelete→faultsim_save snapshot = U7 — the restored state wipes test.db, so
4.8.1's readonly EXPECT reads a missing file). Their fixtures now pin the
C-faithful steps (restore + chmod + readonly open + capi_db_readonly); they
go green when U6/U7 land. 4.8.2 (sqlite3_db_readonly db xyz → -1) is a NEW
leaf, green.

## Verified totals (pager1 failure-NAME-SET diff vs 62b1f8790 baseline)
- fixed 10: pager1-7.1..1, pager1-7.1.0.1, pager1-7.1._PRAGMA_locking_mode_
  =_NORMAL_.1 (the 3 garbage names; their replacements 7.1.6.1-10.1 all pass
  = 5 new green leaves), pager1-39.1, pager1-39.2, pager1-37.1, pager1-37.1.2,
  pager1-43.1, pager1-43.2, pager1-43.3.
- still failing (diagnosed): 4.5.6, 4.7.2, 4.8.1, 4.8.3 (U6/U7).
- new green leaves: pager1-7.1.{6,7,8,9,10}.1 + pager1.4.8.2.
- regressions: NONE (pager/pager1/pager2-4/pagerfault + jrnlmode*/lock* +
  tkt2820/analyze3/blob/misc6 sweeps; wal family sweep pending).

---

Baseline (worktree /private/tmp/wt_pager @ 62b1f8790, s30-pager1):
99 failing leaves; my 14 targets all present:
U1: pager1-7.1..1, pager1-7.1.0.1, pager1-7.1._PRAGMA_locking_mode_=_NORMAL_.1
U2: pager1-39.1, pager1-39.2
U3: pager1.4.5.6, pager1.4.7.2, pager1.4.8.1, pager1.4.8.3
U4: pager1-37.1, pager1-37.1.2, pager1-43.1, pager1-43.2, pager1-43.3

Regen discipline: full pager1 regen DRIFTS (drops pager1-16.1.* x25; adds
setup_249..308 x60) — HAND-PIN all fixture edits; converter fixes verified
by regen diff (name-set + targeted case comparison), NEVER installed
wholesale.

## U1 — subst-foreach column shift (3 leaves)

Diagnosis (reproduced minimally in zz_dbg30_test.go, deleted after):
- pager1.test:1229 `foreach {tn sql res js ws} [subst {...}]` — wal_file_size
  (wal_common.tcl:16, sourced at pager1.test:17) is NOT defined at conversion
  time (`source` is a converter noop). dispatchCommand's unknown-command tail
  returns nil silently → substituteCmd leaves the bracket value "" → the
  list loses one element per `[wal_file_size ...]` (rows 5,6,7) → every
  binding from row 5's ws column shifts; rows 6-10 collapse into the 3
  garbage cases.
- With the proc defined, the same loop binds tn 1..10 correctly (repro).

Fix:
1. tools/tclconvert/tcl/interp_cmds.go: `wal_file_size nFrame pgsz` command →
   32 + (pgsz+24)*nFrame (wal_common.tcl:16-18 verbatim); register in
   commandHandlers.
2. tools/tclconvert/tcl/interp_cmds.go commandSubst: propagate i.substErr
   after substituteOptions (the substituteCmd policy comment: a failing
   command substitution must abort, never bake a wrong value). Prevents the
   silent-list-shift class for future regens.
3. Fixture hand-pin: drop the 3 garbage cases; add pager1-7.1.{6,7,8,9,10}.1
   with the source rows' SQL/expect/type (validated against the fixed
   converter's regen of the 7.1 section). NO .2 cases (never emitted by the
   converter; they need the journal-size substrate B4/U6).

Verify: regen pager1 → 7.1.6.1..10.1 correct; sweep diff = exactly the 3
garbage names removed.

## U2 — __STEP_ column-text capture (2 leaves)

Diagnosis: do_test body `sqlite3_step $::stmt; sqlite3_column_text $::stmt 0`
— sqlite3_column_text is unknown (silently ignored) so the body value is the
step marker __STEP_stmt_1__; the harness expands it to the STATUS code
(SQLITE_ROW), expect is the column VALUE (one/two).
All LIVE __STEP_ users (analyze3, blob, misc6, tkt2820) expect status names;
the value-expect families (trace3, capi2) are skip-listed — status carriers
stay untouched. Engine probe: the prepared stmt survives CREATE TABLE and
step 2 returns 'two' (C contract) — already correct.

Fix:
1. Converter: commandColumnText (`sqlite3_column_text $STMT N`) sets
   vars[""] = `__COL_<handle>_<N>__`; register handler. No step emitted (the
   value is read at assert time from the statement's current row).
2. Harness: stmt_step (main + faultsim handlers) records the formatted
   current row per handle (stmtRow map); runtime_assert expands
   __COL_<h>_<i>__ from it.
3. Fixture hand-pin: 39.1/39.2 runtime_assert sql __STEP_stmt_1__ →
   __COL_stmt_1_0__ (expects unchanged).

Verify: 39.1/39.2 green; tkt2820/analyze3/blob/misc6 sweeps unchanged.

## U3 — readonly/permission surfaces (4 leaves)

Diagnosis:
- 4.8.1/4.8.3: file attributes + sqlite3_db_readonly unknown → chmod steps
  dropped; the db_readonly return folded onto the query expect. Engine READY
  (probe: Open(0400 file) falls back read-only, writes fail SQLITE_READONLY,
  OpenReadOnly works, pager.ReadOnly()/engine.PagerReadOnly() exist).
- 4.5.6: `sqlite3 db test.db -readonly 1` flag dropped (commandSQLite3 reads
  only -vfs). C fails the SELECT because a readonly connection cannot roll
  back the hot journal restored by the snapshot; our snapshot machinery
  (testvfs xDelete hook, B5/U7) and journal playback (B4/U6) are absent —
  leaf stays red; pin the CORRECT steps (readonly reopen) so it goes green
  when U6/U7 land.
- 4.7.2: needs the tv-filter-triggered faultsim_save (journal in the
  snapshot) + EACCES journal open → CANTOPEN. Same U6/U7 dependency; pin
  faultsim_restore_reopen + file_attr steps before the catch.

Fix:
1. Converter commandFile: `file attributes PATH -permissions X` /
   `-readonly N` → file_attr step (mode bits; TCL's -readonly 1 = owner
   write off, 0 = restore rw; the fixture's catch{} makes ENOENT a no-op).
2. Converter commandSQLite3: parse `-readonly <01>` → Readonly flag on the
   emitted reopen_db step.
3. Converter: `sqlite3_db_readonly db NAME` → capi_db_readonly step
   (Database, SQL=NAME).
4. Harness: file_attr (os.Chmod; missing file = no-op), reopen_db Readonly →
   frigolite.OpenReadOnly, capi_db_readonly (main → PagerReadOnly ? 1 : 0;
   unknown name → -1; attached named dbs → their own pager).
5. Fixture hand-pins: setup before 4.5.6 reopen_db +readonly; 4.7.2 pre-steps
   (restore + chmod 0400 journal); 4.8.1 = [file_attr 0400, reopen, query
   no-expect, capi_db_readonly=1]; 4.8.3 = [close_db, file_attr 0666,
   reopen, query no-expect, capi_db_readonly=0]; 4.8.2 new
   [capi_db_readonly xyz = -1] (pager1.test:981).

Engine changes: NONE (probe-confirmed).

Verify: 4.8.1/4.8.3 green, 4.8.2 green (new leaf); 4.5.6/4.7.2 remain
failing under the same names (diagnosed U6/U7 dependency — carded).

## U4 — URI collapse + sqlite3_db_status CACHE_MISS (5 leaves)

Diagnosis: commandSQLite3 strips file: URIs to the path (`file:?mode=memory&cache=shared`
→ Clean("") = "." → reopen_db "." → harness "invalid fixture database path '.'").
The ENGINE already routes shared-memory URIs (frigolite.Open sharedMemURI →
openSharedMem). 43.x: sqlite3_db_status unknown → the do_test expected (0 2 0)
folded onto the preceding query step.
C contract (pager.c sqlite3PagerCacheStat :6875, MISS increment :5615 in
sqlite3PagerAcquire before readDbPage; HIT :5568): MISS = a cache-absent
acquire whose content is read from the db file/WAL; noContent (zero-filled)
acquires and open-time direct header reads do NOT count; reset zeroes after
reading. 43.1 = 2 (page 1 schema + t1 root), 43.2 = 3 (cumulative), 43.3 = 1
(post-reset).
Probe: pager.Open does NOT route the open-time page-1 read through the page
cache (Stats the file; header read direct) — counting in ReadPage's slow
path alone reproduces the C totals; open/external/freelist-direct
readPageLocked callers (C: direct header I/O) stay uncounted.

Fix:
1. Converter commandSQLite3: emit `file:` URIs INTACT (drop the strip+Clean);
   keep non-URI paths unchanged.
2. Harness reopen_db: a step whose SQL starts with "file:" opens via
   frigolite.Open verbatim (no fixtureDBDir join, no "." guard).
3. Converter: `sqlite3_db_status db CACHE_MISS N` → capi_db_status step
   (Database, SQL=CACHE_MISS, new StatusReset bool field in tcl.Stmt +
   TestStep json "statusReset").
4. Engine internal/pager: cacheStat [2]uint64 {hit, miss}; miss++ in
   ReadPage's slow path (file/wal-backed), hit++ in the fast path;
   exported CacheStat(reset bool) (hit, miss uint64).
5. Harness capi_db_status handler: render "0 <miss> 0", compare expect,
   apply reset after read.
6. Fixture hand-pins: 37.1 reopen_db sql → "file:?mode=memory&cache=shared";
   43.1 query loses the folded expect + capi_db_status "0 2 0"; 43.2 adds
   capi_db_status reset=1 "0 3 0"; 43.3 capi_db_status "0 1 0" (queries lose
   the folded expects). 37.2-37.4 untouched (passing).

Verify: 37.1/37.1.2/43.1-3 green; 37.2-37.4 unchanged-passing; pager sweeps
(pager, pagerfault, pager2-4) unchanged; Engine-pager counter has no other
readers.

## Sweep + commit protocol

- Baselines recorded BEFORE edits: /tmp/s30_before.txt (pager1), plus
  pager/pagerfault/pager2-4 + tkt2820/analyze3/blob/misc6/trace3/capi2 for
  the touched surfaces.
- After each slice: targeted leaf run + family sweep; failure-NAME-SET diff
  (python sets, never comm).
- Final: go vet + go build; commit on s30-pager1 (pre-commit hook gates
  gocognit<=15, gocyclo<=12, staticcheck).
- Lessons entry appended to .agents/lessons_learned.md.
- zz_dbg30_test.go (repro) deleted before commit.
