# PLAN S35abc — the three smallest WAL-residue slices (2026-11-16)

## STATUS (2026-11-17, live)

- **S35a DONE + VERIFIED** (commits bc2ef93bf, S35a-docs): the pre-scan
  flip landed wholesale — the honest sweep found the flip touches 39 fixture
  families (20 RUN), and the churn IS the win: 27 GONEs (wal9-1.1/1.7,
  walcrash2-1.3 targets + 24 same-class "no such table" harness artifacts),
  1 NEW (pager4-1.6) oracle-adjudicated as a fixture-expect defect (converter
  drops `file rename`) and resolved by the rename_file harness step + 3
  hand-pins (pager4 5→0, engine's os.SameFile DBMOVED identity already
  C-exact).
  **Sweep verdict (S35a harness + pager4 pin at HEAD, vs the merge-gate
  baseline, python raw byte scan leaf-only):** the whole wal* group (23
  families incl. wal/wal2/wal3/wal4/wal6/walvfs/walmode/walfault/walckptnoop/
  walfault2/e_wal*/journal2/subjournal) is BYTE-IDENTICAL except the two
  targets; pager1/alter/pragma/pragma2/shared/sharedA/journal/journal2/
  notify1/interrupt/interrupt2 identical; savepoint {9.1,9.2,9.3} GONE = the
  S37 record's known fixes (the baseline predates S37);
  vacuum-into-500 GONE = pre-existing flake flap (the baseline has it
  failing; 3 reruns fail it again; no S35a mechanism touches vacuum-into).
- S35b/S35c: pending.

Source: `portplan/TRIAGE_wal_residue_2026-11-16.md` §3 (S35a/S35b/S35c).
Pre-fix failing name sets (the verified gate, 2c9af99fa): wal9 {1.1, 1.7},
walcrash2 {1.3}, walmode {4.9}, wal {12.1, 23.4}, walvfs {1.2, 3.1, 3.2,
3.2.2, 4.2, 6.2, 9.1}, wal4 {1.3}, wal3 {3.3, 6.2.3, 6.2.5, 7.1.4, 7.2.2},
subjournal {2.1}, wal6 {4.4.2, 5.2}. Targets: 10 leaves (wal-23.4 and the
rest of walvfs are other units). Verify by failure-NAME-SET diff (python raw
byte scan, leaf-only) per family, family context only.

## Oracle probes already run (3.51.0 testfixture, DYLD_LIBRARY_PATH=/tmp/tclshim)

- **P4-CORRECTION (walvfs-1.2)**: the triage's "padToSectorBoundary fires
  under synchronous=normal" is WRONG. The extra frames are REAL page-1
  frames from C's **pagerWriteLargeSector** (pager.c:6131): `tvfs devchar
  sequential` drops IOCAP_POWERSAFE_OVERWRITE → setSectorSize
  (pager.c:2727-2735) keeps the real sector size (4096, os.h:53 /
  os_unix.c:4367) instead of 512 → at page_size 1024, sqlite3PagerWrite's
  dispatch (pager.c:6106) routes EVERY write-intent to
  pagerWriteLargeSector, which journals (makes writable) every page in the
  sector. Frame dump (p4.tcl): each 1-row INSERT at page 1024 commits
  frames `1/0 2/2` (pgno 1 uncommitted-marked + the leaf with the commit
  mark). The padding gate in wal.c:4173 (`WAL_SYNC_FLAGS(sync_flags)!=0`)
  stays commit-bits based (non-zero only at synchronous=FULL — the
  fixture's 1.3 sync_count=0 at NORMAL proves no commit sync at NORMAL).
- **wal3-3.3 (sync-flag rendering)**: C's sqlite3PagerSetFlags
  (pager.c:3636-3650): syncFlags = NORMAL (0x02) unless noSync or
  PAGER_FULLFSYNC; walSyncFlags = syncFlags<<2 | (fullSync ? syncFlags :
  0). So at synchronous=FULL without fullfsync the COMMIT sync bits are
  NORMAL — every xSync renders "normal" (the fixture want is `normal ×4`).
  The comment at pager.c:605-616 states the bit ASSIGNMENT swapped (it is
  stale); wal.h:25-26 is authoritative (WAL_SYNC_FLAGS = low bits = commit).
  Oracle p2.tcl: XSYNC wal flags=normal at synchronous=FULL.
- **wal6-4.4.2**: the fixture's txn runs on db (NOT db2) — wal6.test:200-206
  (`execsql { BEGIN; INSERT ... }` has no db2 arg). C's reopen adopts the
  live -shm (walIndexTryHdr, wal.c:2580-2625: isInit + checksums only, NO
  file re-check on the real-shm path — the salt check exists only in the
  heap-memory path wal.c:2851 and in RECOVERY). The frame read is RAW
  (sqlite3WalReadFrame, wal.c:3635-3645 — no frame validation); the zeroed
  page image fails the btree page-init → "database disk image is
  malformed". t1 works because the reader skips checkpointed frames
  (minFrame = nBackfill+1, walFindFrame wal.c:3520-3556). Oracle p11.tcl:
  wal=2128 after COMMIT, T1={1 2}, T2={1 {database disk image is
  malformed}}.
- **wal4-1.3**: the snapshot holds wal+shm with the main MISSING. C's open
  creates a 0-byte main (B main-exists=1 size=0), the FIRST statement's
  pagerOpenWalIfPresent (pager.c:3337-3361: pagerPagecount nPage==0 →
  sqlite3OsDelete(zWal)) deletes the -wal (the -shm is kept), journal_mode
  falls back to delete (C: jm=delete after a later CREATE; main stays 0
  bytes — NO header written at open), SELECT → {1 {no such table: t1}}.
  **Header-bootstrap answer (oracle p13.tcl)**: C writes page 1 to the main
  file at `PRAGMA journal_mode=wal` via BtreeSetVersion's rollback-mode
  flush WITHOUT an explicit page_size pragma (main=1024, hdr18=2) — our
  engine already does this (writeWALVersionBytesLocked); P5 stays the
  regression guard.

---

## S35a — lifecycle-for-named-db harness rule (3 leaves; ~15 LOC, zero engine risk)

**File: frigolite_harness_test.go (pre-scan loop ~3467-3497).**

The pre-scan sets `usesReopen` for reopen_db/alter_table/header_format/
copy_file/load_hex_file/open_empty_db steps. The wal9/walcrash2 fixtures'
ONLY file trigger is `open_named_db` with SQL `test.db` (db2), so main runs
on `:memory:` (setupDB, frigolite_test.go:57) while db2 opens
fixtureDBDir/test.db — the table created on main is invisible to db2
("no such table: t"). The main-loop open_named_db handler (frigolite_harness_test.go:3774)
already routes `normalized == filepath.Base(lifecycleDBPath)` to
lifecycleDBPath, so the only gap is the pre-scan.

Change: in the pre-scan loop, for `step.Type == "open_named_db"`, set
usesReopen = true when the target resolves to the lifecycle filename,
mirroring the runtime normalization: `sql := strings.TrimSpace(step.SQL)`
non-empty, `normalizeFixtureDBName(step.SQL) == "test.db"`, not
":memory:", and not a shared-memory URI (the sharedMemURI checks from the
runtime handler: `sharedMemURI(step.SQL)`'s shared/name forms stay
path-verbatim).

- C contract: tester.tcl reopen_db opens the lifecycle test.db; wal9.test:23
  `sqlite3 db2 test.db` shares that file with main.
- Verify: `FRIGOLITE_TEST=wal9` and `FRIGOLITE_TEST=walcrash2` → 0 failures.
- Churn guard (two-direction name-set diffs): journal2, wal3, wal2,
  e_walckpt, attach (sample: the open_named_db families must keep their
  failure sets byte-identical).

## S35b — the small engine + converter pins (4 leaves; ~120 LOC)

### S35b.1 — wal3-3.3: the WAL sync-flag levels (~5 LOC engine)

**File: internal/pager/vfshooks.go:38-41.**

`WALSyncFull = 0x0F` renders the commit sync bits as SQLITE_SYNC_FULL (0x03
→ "full"). C's derivation (pager.c:3644-3647) puts syncFlags (NORMAL, 0x02)
into the commit bits at synchronous>=FULL absent PAGER_FULLFSYNC: the
constant must be `0x0A` (NORMAL<<2 | NORMAL). Consequences:

- vfsSync's zFlags rendering (bits&0x01 → "full") now renders "normal" for
  every synchronous level without fullfsync — wal3-3.3's want.
- walCommitSyncBits(0x0A) = 0x02 ≠ 0 — the walvfs-5.3 padding gate and the
  commit-sync event (wal.go:573) keep firing at synchronous=FULL
  (wal3-3.2/3.3's counts unchanged: the sync EVENTS count is identical, only
  the flag STRING changes).
- `PRAGMA fullfsync` / `checkpoint_fullfsync` are unmodeled (the engine has
  no PAGER_FULLFSYNC bit) — out of scope; the default-off path is the
  fixture surface.

C pins: pager.c:3636-3650, wal.h:25-26, wal3.test:195-225 (the 3.x table).
Verify: wal3 (3.3 → 0; 3.1/3.2 stay green), walvfs (5.3 stays green), wal.

### S35b.2 — walmode-4.9: propagate the WAL→rollback BUSY (~2 LOC)

**File: internal/exec/pragma_state.go:176-180.**

`setJournalMode` swallows the leaveWALModeLocked "database is locked" error
and echoes the current mode (the swallow landed in b72123592 "P-C1 S7e
piece 2" citing a nonexistent wal-25.x contended DELETE — wal.test 25.x is
the ATTACH journal-mode sweep, no contention). C's OP_JournalMode
(vdbe.c:8081-8096): `rc = sqlite3PagerCloseWal(...)` → `if( rc ) eNew =
eOld` → `if( rc ) goto abort_due_to_error` — the statement FAILS with
SQLITE_BUSY's "database is locked" (sqlite3ErrStr). The BUSY arises because
C's WAL connections hold the POSIX SHARED db lock for their WAL lifetime
(pager_unlock's WAL branch, pager.c:~6900, never unlocks) so PagerCloseWal's
pagerExclusiveLock (pager.c:7682) fails beside any second WAL connection
(oracle p10/p10b: at the 4.9 boundary no POSIX lock is observable from
python — the engine's in-process wal-index RefCount gate is the
single-process equivalent, already implemented in leaveWALModeLocked).

Change: delete the "database is locked" swallow branch — the error
propagates (`return &execpragma.Result{Error: err}`).

- Guard: the in-memory swallow directly above stays (memdb1.test 420).
- Verify: walmode (4.9 → 0; 4.10/4.11/4.12 stay green), wal (the
  leaveWALMode family), jrnlmode/jrnlmode2/jrnlmode3.

### S35b.3 — wal-12.1: the file-size hand-pin (~30 LOC harness + JSON pin)

**Files: frigolite_harness_test.go (new step case), testdata/wal.json.**

The do_test body is
`list [expr [file size test.db]/1024] [file size test.db-wal]`
(wal.test:779-796); the converter stamped expect "1 {}" on the exec step
(the migrated-value defect). Harness pin:

1. New step type `file_size`: SQL = the fixture-relative path, Variable =
   the decimal divisor ("" = 1), Expect = the compared value. The case
   resolves the path against fixtureDBDir (file_attr's pattern), os.Stat,
   renders size/divisor as the actual, matchExpectation against Expect.
   (The converter never emits it — a hand-pin-only type like the S30/S32
   pins; the unknown-type arm fails loudly so the type must exist.)
2. testdata/wal.json wal-12.1: keep the exec step (drop the bogus expect),
   append `{"type":"file_size","sql":"test.db","variable":"1024","expect":"1"}`
   and `{"type":"file_size","sql":"test.db-wal","expect":"5272"}`.
   5272 = wal_file_size 5 1024 = 32 + 5*1048 (the P3/P5 oracle pin: 5
   frames). The JSON edit must reproduce Go's MarshalIndent byte-for-byte
   for the untouched regions (the S32 lesson).

C pins: wal.test:779-796; tester.tcl's file size semantics; the harness's
wal_file_size helper (interp_cmds.go:422) evaluated 5 frames at conversion.
Verify: `FRIGOLITE_TEST=wal` (wal-12.1 → 0; wal-23.4 stays — other unit;
the rest identical).

### S35b.4 — subjournal-2.1: the backup step pin (~8 LOC JSON)

**File: testdata/subjournal.json.**

The source body (subjournal.test:43-49):
`forcedelete test.db2` (a no-op — the file is test2.db and never exists),
`sqlite3 db2 test2.db`, `sqlite3_backup B db2 main db main`,
`set nPage [db one {PRAGMA page_count}]`, `B step [expr $nPage-10]` —
the body value is the backup step's rc (SQLITE_OK: a budget 10 pages short
of the source cannot cover it — backup.c's sqlite3_backup_step returns
SQLITE_OK until DONE). The converter dropped the backup step and stamped
"SQLITE_OK" onto query_assign (got [229] vs want SQLITE_OK).

Pin (all machinery exists — no harness change):
1. query_assign `PRAGMA page_count` → var nPage: DROP the migrated expect
   (the assignment stays).
2. NEW runtime_expr step: SQL `$nPage-10`, Variable `pages` (the harness's
   evalHarnessTclExpr evaluates at runtime against runtimeVars).
3. NEW backup_db step: SQL `$pages` (the main loop's expandTCLRuntimeVarsSQL
   at :3714 substitutes the numeric value bare before the case runs),
   database `db2`, destDb `main`, srcConn `db`, srcDb `main`,
   expect `SQLITE_OK` (the harness renders srcRC alone for a single-code
   expect — the S7g lesson).

C pins: subjournal.test:43-49; test_backup.c's step contract; backup.c:599
(finish/step rc). Verify: `FRIGOLITE_TEST=subjournal` (2.1 → 0; the family's
other leaves byte-identical).

## S35c — the WAL frame/recovery rules (3 leaves; ~120 LOC engine)

### S35c.1 — walvfs-1.2: the large-sector write rule (~60 LOC engine)

**Files: internal/pager/pager.go (field), pageropen.go (init), vfshooks.go
(SetDeviceCharacteristics), pagerstmt.go (PrepareWrite).**

Oracle-pinned mechanism (P4-CORRECTION above; the triage's padding-gate
hypothesis dropped — the S10 padding gate is already sync-bits based and
C-exact):

1. `Pager.sectorSize uint32`:
   - construction (pageropen.go:84/443): 512 (PSO in the default
     deviceCharacteristics → setSectorSize's PSO rule, pager.c:2727-2735).
   - SetDeviceCharacteristics(bits) (vfshooks.go:184): recompute — PSO
     present → 512; absent → 4096 (SQLITE_DEFAULT_SECTOR_SIZE, os.h:53;
     the Darwin unix VFS's value, os_unix.c:4367). (C recomputes at open +
     playback only; the harness arms devchar before any I/O, so the
     recomputed value stabilizes pre-I/O exactly like C's open-time value.)
2. PrepareWrite (pagerstmt.go:519) — C's sqlite3PagerWrite dispatch
   (pager.c:6106: `sectorSize > pageSize` → pagerWriteLargeSector):
   - when `p.sectorSize > p.pageSize` AND the pager is file-backed:
     port pagerWriteLargeSector (pager.c:6131-6190): nPagePerSector =
     sectorSize/pageSize; pg1 = ((pgno-1) & ^(nPagePerSector-1)) + 1;
     nPageCount = the db file's page count (C's pPager->dbSize — the
     file-size-derived count, the S34 lesson); nPage = (pgno > nPageCount)
     ? pgno-pg1+1 : (pg1+nPagePerSector-1 > nPageCount ? nPageCount+1-pg1 :
     nPagePerSector); for each pg in [pg1, pg1+nPage) with pg != pgno:
     ReadPage(pg) + markDirtyLocked(pg) (C's sqlite3PagerGet + pager_write;
     the fetched page joins the dirty set so the commit appends its frame).
     The target page keeps the existing stmt-journal capture.
   - Memory pagers never trigger (PSO always present → 512 < any pageSize).
3. The commit path needs no change: the WAL commit sorts the dirty set
   ascending (wal.go commit()) and appends a frame per dirty cached page —
   the fetched page-1 frames appear first, exactly C's pDirty order.
4. C's rider (pagerWalFrames:3216 → pager_write_changecounter, pager.c:3082):
   when page 1 is in the commit list, bump bytes 24 (+1 from
   dbFileVers), mirror at 92 (VersionValidFor), stamp
   sqliteVersionNumber at 96 — into the page-1 IMAGE only. Port into
   commitLocked before the frames loop (read p.knownFileVers' counter,
   +1, write the image bytes; no pager-header mutation — C's dbFileVers
   stays stale across consecutive page-1 WAL commits, so consecutive
   commits stamp the SAME value, matching C).

C pins: pager.c:6106-6190 (sqlite3PagerWrite dispatch + pagerWriteLargeSector),
pager.c:2727-2735 (setSectorSize), os.h:53, os_unix.c:4347-4367,
pager.c:3082-3097 (pager_write_changecounter), pager.c:3216, wal.c:4173
(the padding gate — unchanged). Verify: walvfs (1.2 → 0; 1.1's sync_count=1,
1.3's 0, 5.3's {24 100 100 100} stay green — P4's oracle numbers), wal6,
wal4, plus the devchar-exposed families pager1/pager2/journal2/nolock/ioerr6
(name-set identity vs pristine).

### S35c.2 — wal6-4.4.2: trust the parsed wal-index header (~25 LOC engine)

**File: internal/pager/wal.go (walInitHeader, walFileHeaderSaltsMatchLocked).**

C's walIndexTryHdr (wal.c:2580-2625) trusts a parsed shared header
(isInit + header checksums) with NO file re-check on the real-shm path;
the file salts are consulted only in RECOVERY (walIndexRecover, from the
FILE's own header) and in the heap-memory path (wal.c:2851). Our
walInitHeader's adopt path additionally runs walFileHeaderSaltsMatchLocked
(salts + capacity vs the -wal file) and rebuilds on mismatch — a
zeroed/garbage -wal header voids the log at every reopen, so the zeroed
frame-2 read never happens and t2 scans empty (the probe: t2=[]).

Change: delete the stale-gate block in walInitHeader (the
`stale = w.hdr.MxFrame > 0 && !w.walFileHeaderSaltsMatchLocked()` branch
and walFileHeaderSaltsMatchLocked with it). The adopt path then proceeds:
the zeroed frame read (readPageWALLocked already reads frames raw — C's
sqlite3WalReadFrame) delivers the zeroed page image; the btree's
validatePageHeader rejects page type 0x00 ("storage: unknown page type").

The user-visible message must be "database disk image is malformed"
(normalizeCorruptionError maps "unknown page type" — but its only caller is
execAfterWrite, the WRITE path). Verify the SELECT path's error text in the
native probe; if raw, extend the normalization to the query result path
(same strings, fts3corrupt4-27.2 precedent).

- Guard: the gate's stated purpose (a snapshot pair pairing a wal-index
  with a restarted WAL) — the harness's faultsim pairs copy the whole
  test.db* glob (consistent sets); the sweeps adjudicate (wal, wal6,
  walvfs, walfault, pagerfault, walcrash).
- C pins: wal.c:2580-2625 (walIndexTryHdr), wal.c:2851 (the heap-memory
  salt check — the ONLY file check), wal.c:3635-3645
  (sqlite3WalReadFrame, raw), wal6.test:200-236, walIndexRecover's header
  validation (already ported, walrecover.go — the recovery path is
  untouched).

### S35c.3 — wal4-1.3: the missing-main + WAL delete rule (~8 LOC engine)

**File: internal/pager/pageropen.go (attachWalAtOpen's garbage rule, ~349).**

The rule deletes the -wal/-shm beside a 0-byte main, but a MISSING main
(the wal4 snapshot: sv_test.db deleted, sv_test.db-wal/-shm restored)
skips the rule (`os.Stat` error) → openWal attaches → the old schema
resurrects (got success for `SELECT * FROM t1`).

Change: treat a missing main like a 0-byte main — the gate condition gains
`|| os.IsNotExist(err)` (C's pagerOpenWalIfPresent: the open created the
0-byte file, pagerPagecount's xFileSize reports 0 → sqlite3OsDelete(zWal),
pager.c:3348-3351). The `!walIndexLiveEntry` guard stays (deleting a held
log orphans the holder). Keep deleting the -shm with the -wal (C leaves the
shm file; no fixture observes it, and a stale-shm adopt is the worse
hazard).

- The journal mode after the delete stays the open-time default (delete) —
  C's D step (jm=delete) — no code needed (the attach never happened).
- The main stays 0 bytes (schema.Init's materialization is memory-only —
  verify with the family run: wal4-2's "Db file grew" assertion).
- C pins: pager.c:3337-3361 (pagerOpenWalIfPresent), pager.c:841-856
  (pagerPagecount's file-size nPage), the p13 oracle probe. Verify: wal4
  (1.3 → 0; wal4-2's faultsim leaves stay green), wal (12.1/14/P5 pins),
  wal9, wal6.

## Execution order

1. S35a (harness) → verify wal9/walcrash2 + the open_named_db sample sweep →
   commit.
2. S35b.1 (sync flags) → verify wal3/walvfs → S35b.2 (BUSY propagate) →
   verify walmode/wal → S35b.3+S35b.4 (JSON pins + file_size step) → verify
   wal/subjournal → commit S35b as one unit (or split: engine pins vs
   hand-pins, two commits).
3. S35c.1 (large-sector) → verify walvfs/wal6/wal4 + devchar families →
   S35c.2 (stale-shm gate) → verify wal6 + sweeps → S35c.3 (missing-main) →
   verify wal4 → commit per verified step.
4. Update .agents/lessons_learned.md after each key achievement (the P4
   correction, the sync-flag levels, the real-shm trust rule); update the
   PLAN STATUS section as slices land.

## Verification commands

```
FRIGOLITE_TEST=<fam> go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
# name-set diff: python raw byte scan re.findall(rb'--- FAIL: TestSQLiteSuite/([^\s(]+)') on -v logs, leaf-only
go test -run 'TestSOLID_' ./...   # after engine changes
go vet . && gofmt -l .
```
