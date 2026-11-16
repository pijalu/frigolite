# PLAN_S31 — pager1 engine slices U5-U7 (branch s31-pager-engine @ dd045611d)

Baselines (own sweeps, failure NAME sets):
- pager1 = 89 leaves (/tmp/s31_base_pager1_clean.txt)
- pager family = 96 (89 pager1 + 5 pager4 pre-existing), pager2 = 0, pagerfault = 0

C source: /Users/muaddib/dev/sqlite (3.51.0); oracle /usr/bin/sqlite3 3.54.
Verified facts this unit rests on (probed / read this session):
- frigolite journal substrate EXISTS (journal.go: eager journal at first write,
  before-images captured at WritePage time, C-format header/records, DELETE/
  TRUNCATE/PERSIST finalize, hot-journal recovery at Open with DIFFERENT rules
  than C).
- Commit ORDER defect: flushFilePagesLocked finalizes (deletes) the journal
  BEFORE the db-file xSync. C: syncJournal → write pages → sync db →
  end_transaction deletes the journal. This kills the 4.3.x (save-on-db-sync)
  and 4.5.x (save-on-journal-xDelete) snapshot contracts.
- readHotJournal rules differ from C: discards on PageSize mismatch (C adopts
  the journal's page size), discards on DBPageCount mismatch (C: hot iff first
  byte nonzero + no reserved lock + nPage>0), no header field validation
  (sector-size @20 >= 32 pow2 <= 65536; page-size @24 >= 512 pow2 <= 65536 or 0
  = compat), no record checksum verification (pager_cksum sparse sampler:
  cksumInit + sum(aData[i], i=pageSize-200 … >0, step -200)), no pgno==0/SJ →
  DONE, no pgno>dbSize skip, no first-header truncate-to-mxPg + mxPgno-upward.
- Engine gaps verified by probes: (a) explicit-rowid INSERT (VALUES or SELECT)
  never checks rowid-uniqueness on a constraint-free table (14.1.4/6);
  (b) INSERT..SELECT with a same-table scalar subquery inserts 1 row instead
  of N (the subquery's scan disturbs the outer cursor — 11.5 count 6 vs 32);
  (c) PRAGMA page_count excludes the auto_vacuum ptrmap page(s) (44.1: 30 vs
  31); (d) ROLLBACK restores neither dbSize nor mxPgno from the journal header
  (44.3: [30 16] vs [31 31]); (e) harness reopen_db probes sqlite_master and
  falls back to a FRESH db on a lazy-init error (38.1#01: the engine's
  NOTADB error state is already sticky — probed).
- C vdbeCommit super-journal gate (vdbeaux.c:2945-3140): nTrans counts WRITE
  txns with safety_level!=OFF AND aMJNeeded[journalmode] (DELETE/PERSIST/
  TRUNCATE=1, OFF/MEMORY/WAL=0) AND !memdb; MJ iff nTrans>1 AND main file name
  non-empty. Name zMainFile+"-mj%06X9%02X". Sequence: write each write-txn
  journalname (NUL-terminated) → sync MJ → per-db CommitPhaseOne(zSuper) (each
  journal gets [sjPgno][name][len][cksum][magic] appended + journal sync) →
  close+DELETE MJ (the commit point) → PhaseTwo deletes each journal.
- pager_playback(isHot): readSuperJournal from the journal tail; named MJ
  missing → NOT hot (skip playback); readJournalHdr validates magic + nRec +
  cksumInit + dbSize + sector/page-size ranges (invalid → DONE = stop); nRec
  0xffffffff → (szJ-HDR_SZ)/PG_SZ; nRec==0 && !isHot && last hdr → from size;
  FIRST header → truncate db to mxPg, dbSize=mxPg, mxPgno adjusted UPWARD;
  per record: pgno==0 || pgno==SJ → DONE; pgno>dbSize → skip; cksum mismatch →
  DONE (keep played records); after: setPagesize back, end_transaction deletes
  the journal; delsuper when a named MJ exists and no surviving journal points
  back at it (pager_delsuper).

## Slice order (land separately, commit after each)

### S31.1 — U5a auto_vacuum page_count/max_page_count (44.1/44.3)
1. Engine: PRAGMA page_count (and btree dbSize accounting) must COUNT ptrmap
   pages — find where nPage is reported (execpragma page_count → btree) and
   make the reported count the FILE page count including ptrmap pages
   (btree.c lockBtree's nPage includes them; ptrmap pages are never usable
   data but ARE db pages). Validate with the 44.1 fixture shape + oracle.
2. Engine: ROLLBACK restores dbSize AND mxPgno from the journal header's
   original-db-size field (pager.c: pager_rollback → dbSize=mxPg; mxPgno
   adjusted UPWARD only: `if mxPgno<mxPg { mxPgno = mxPg }`). Our
   rollbackFromJournalLocked restores numPages already (restoreJournalTruncationLocked)
   — add the mxPgno restore + make sure 44.3's max_page_count=2 (refused:
   max<current → stays 16; pagerSetMaxPages clamps to dbSize) rolls back to 31.
   Check PRAGMA max_page_count=2 refusal semantics first with the oracle.
3. Fixture: none (44.1/44.2/44.3 already carry the right steps).
Verify: pager1-44.1/44.2/44.3 green; jrnlmode*/wal* sweeps unchanged.

### S31.2 — U5b NOTADB reopen lifecycle (38.1#01)
Harness only: reopen_db's sqlite_master probe failure must KEEP the opened
connection (C: open is lazy, the error state is sticky) instead of newDB
fallback. Keep the fresh-db fallback ONLY when Open() itself failed.
Verify: pager1-38.1#01 green; corrupt2 / misc e_* sweeps unchanged.

### S31.3 — U5c journal_mode=OFF rowid-UNIQUE (14.1.4/14.1.6)
Engine: explicit-rowid INSERT conflict detection. The rowid-exists check must
run when the rowid is EXPLICIT even on a constraint-free table:
- extend the per-row constraint gate (checkConstraints callers: the VALUES
  path resolveInsertRowConstraints and the SELECT path insertSelectOneRow)
  with `if hasExplicitRowID && !withoutRowid && rowIDExists → rowIDConflictError`.
- OR-matrix: OR IGNORE skips (existing routing on the unique error), OR
  REPLACE deletes the conflicting row at that rowid (replaceDeleteConflicts
  with the explicit rowid — verify the routing covers the new error; the
  table-level ON CONFLICT REPLACE branch matches by error text — confirm the
  rowid error text routes).
- Statement-failure behavior with journal_mode=OFF: the failing statement's
  partial work stays dirty (no stmt journal), txn continues, COMMIT commits
  (C: stmt1's row survives; 14.1.6 = {1 2 2 2}). Our engine rolls statements
  back via in-memory savepoints — for a failed INSERT..SELECT row the already-
  inserted rows of the SAME statement stay (C keeps partial statement work
  with journal OFF? NO — C's stmt2 inserted nothing before the conflict; keep
  engine behavior = insert rows until the conflict error, they stay dirty).
  Verify with the oracle what t1 holds after 14.1.4/5: expect {1 2 2 2}.
Verify: 14.1.4/14.1.6 green; the e_insert/dml sweeps unchanged.

### S31.4 — U5d pending byte (42.1)
1. Engine: a process-global pending-byte control (sqlite3_test_control
   PENDING_BYTE): internal/pager var + accessor; Pager.lckPgno =
   pendingByte/pageSize + 1 recomputed on pageSize change (pager.c:3810);
   page READ of lckPgno → SQLITE_CORRUPT "database disk image is malformed"
   (pager.c getPageNormal: pgno==PAGER_SJ_PGNO → CORRUPT before any read).
2. Harness: new step type `testctl_pending_byte` (value from step.SQL, return
   the OLD value into step.Variable when set). Hand-pin into pager1-42.1
   between close_db and reopen_db.
Verify: 42.1 green; auto_vacuum/integrity sweeps unchanged (pending byte
default 0x40000000 = beyond any test file).

### S31.5 — U5e WAL-shm gate (21.1/21.2)
1. Harness: register_testvfs fixture flags -noshm 1 / -iversion 1 (the
   converter already parses testvfs options? hand-pin the register_testvfs
   step carrying the flag in SQL, e.g. "default" + a new `noShm` field or
   encode as SQL "noshm"). Fixture's tv is NOT -default in 21.x — it is
   attached to db2's open (open_named_db with -vfs tv): route the hook onto
   the named connection (already exists) + set pager.shmAvailable=false.
2. Engine: Pager.SetShmAvailable(false) → opening a WAL-mode db fails at the
   first use with SQLITE_CANTOPEN "unable to open database file"
   (pager.c pagerOpenWal: no xShm methods → CANTOPEN).
Verify: 21.1/21.2 green; wal*/e_walckpt sweeps unchanged (default = available).

### S31.6 — U5f temp named connection + IOERR injection (8.2.2, 11.2/11.5)
1. Harness: open_named_db with an EMPTY path opens a private fresh temp
   connection (db2 sees no tables). Check the current open_named_db handler.
2. Harness: the tvfs xWrite/xTruncate filter + lockout proc → the hook already
   returns VFSHookError for xWrite/xSync; add xTruncate to the result-code
   ops; the pager must EMIT xWrite/xTruncate events for db-file page writes
   and truncates through vfsCall (mirror C's sqlite3OsWrite/Truncate sites:
   flushPage write, truncatePages/shrinkDatabaseFile).
3. Engine: COMMIT with a failed write → disk I/O error; the journal remains
   (hot) — 11.3 then opens db2 and plays it back (needs S31.7's playback).
   11.5's count growth bug: same-table subquery scan disturbs the outer
   cursor (INSERT..SELECT adds 1 row per statement instead of N) — diagnose
   in execquery (subquery cursor isolation), fix, re-verify count=32.
Verify: 8.2.2, 11.2, 11.5 green (11.5 may need only the subquery fix).

### S31.7 — U6 hot-journal substrate (B4: 4.3.2-10, setup_11/12, 4.5.3-5, +4.5.6/4.7.2/4.8.x with U7)
1. Engine commit reorder (pagercommit.go flushFilePagesLocked): journal open →
   [C syncJournal: write nRec into header? keep 0xffffffff + xSync EVENT on
   the journal path via vfsCall] → page writes → db xSync event → journal
   finalize (delete/truncate/persist). Single-db and multi-db paths both.
2. Engine: journal events through vfsCall: xOpen/xClose/xDelete on the
   journal sidecar + xSync (the fixture hook must see journal ops for the
   faultsim save-on-xDelete contract). Keep journalFileOpHook firing too.
3. Engine: replace readHotJournal/recoverHotJournal with C's exact algorithm
   (new file pagerplayback.go):
   - hasHotJournal: journal exists && no other writer (lockreg) && dbSize>0
     (dbSize==0 → delete journal, not hot) && first byte != 0.
   - playback: readSuperJournal from the tail ([sjPgno4][name][len4][cksum4][magic8],
     cksum = sum of name bytes as signed chars; named MJ must EXIST else not
     hot); header walk per readJournalHdr (magic, nRec, cksumInit, dbSize,
     sector 32..65536 pow2, page 512..65536 pow2 or 0→compat; invalid → DONE);
     first header → truncate to mxPg + dbSize=mxPg + mxPgno upward; records:
     pgno==0/SJ→DONE, pgno>dbSize→skip, pager_cksum mismatch→DONE; play into
     cache (cache empty at open: isHot → reset first); restore page-1
     nReserve (byte 20); after: restore pageSize, delete journal, delsuper.
   - ADOPT the journal's page size during playback (temp override), C
     sqlite3PagerSetPagesize.
4. Fixture hand-pins (testdata/pager1.json — name-set equality):
   - 4.3.1: add register_testvfs tstvfs default + filter xSync + script
     xSyncCallback (+proc body: non-journal tail → faultsim_save) + the exec
     + close_db (keep the case name pager1.4.3.1; the regen's shape shows the
     expected steps).
   - 4.3.2-10: prefix faultsim_restore_reopen (the regen shape has it).
   - 4.5.x setup: the tv sectorsize 512 + script copy_on_journal_delete +
     filter xDelete + proc → new setup case (or extend the pre-4.5.1 setup)
     + 4.5.1 steps get the tv filter xDelete active during the txn.
   - setup_11/setup_12 hexio_write offsets must find the journal — they run
     AFTER 4.5.3's restore (fixture order verified).
   - 31.1 (pre-3.5.8 compat: page-size field 0) — check its current state.
Verify: the 13 B4 leaves green; journal1/journal2/journal3/jrnlmode*/pagerfault
sweeps unchanged (the recovery rule change is the risky part — full journal*
family diff before commit).

### S31.8 — U7a harness faultsim/restore half (target ~27 leaves)
1. Harness faultsim_restore_reopen: close ALL named connections (namedDBs)
   and clear attach state + close the MAIN db BEFORE the file restore (C
   tester.tcl db_restore_and_reopen: db close → restore → reopen). The
   existing path closes only main → "database aux is already in use" (.4).
   Preserve each named connection's filename + vfs for reopen? C's
   db_restore_and_reopen closes and reopens ONLY the main db — ATTACH is NOT
   re-added (the fixture re-ATTACHes explicitly). So: close namedDBs + main,
   restore files, reopen main only.
2. Harness: `file_exists` step (target from step.SQL/Variable, expect 0/1)
   for 4.6.3/4.6.5/4.6.10/4.6.11/4.6.13/4.6.15; `glob_files` step for 4.6.9
   (sorted basename list compare).
3. Fixture hand-pins: 4.4.x .4/.5/.6 need faultsim_restore_reopen steps
   (currently MISSING — the converter dropped them: regen shows .4 with just
   exec ATTACH). 4.6.1 setup gets the tv script/filter rebind + .2-.15 get
   restore + file_exists steps per the source.
Verify: the restore-cascade leaves green (4.4.1.{n,f,t1,p1}.4/5/6,
4.4.2.{...}.4/5/6, 4.6.2/4/12/14 + the file-exists leaves); pagerfault sweep
must stay green (it uses the same machinery).

### S31.9 — U7b super-journal engine half (the 22 .1b leaves + 4.4.2.*.8)
1. Engine (exec/transaction.go commitFlushAllPagers → new superJornalCommit):
   compute C's nTrans gate (write txns: sync level != OFF, aMJNeeded[mode],
   !memdb, main file non-empty) — the sync LEVEL lives in settings.synchronousLevels.
2. Gate true: (a) build the MJ name zMainFile+"-mj%06X9%02X" (random via
   crypto/rand; '9' antepenultimate); (b) create O_EXCL; (c) write each
   write-txn db's journalname+NUL; (d) sync (xSync event); (e) per-db
   CommitPhaseOne WITH zSuper: the pager's journal gets the MJ pointer
   appended (writeSuperJournal format) BEFORE its sync, then pages + db sync
   (NO journal finalize); (f) close + DELETE the MJ (xDelete event — the
   faultsim_save hook point); (g) PhaseTwo: finalize each journal.
3. Playback integration: the journal tail's MJ pointer parsed by the new
   playback (S31.7.3) — MJ missing → not hot (4.4.x.7/4.6.x).
4. Fixture: none needed for .1b (runtime_var steps already pinned); the
   4.4.2.*.8 padded-prefix flavor needs the mkdir/cd emulation — card to
   S31.10.
Verify: the 22 .1b leaves green (o/o512/w1/w2/o1a/o1b/m1 = 0; n/f/t1/p1 = 1);
4.4.x.4-6 rollback via restore must show PRE-txn rows (needs S31.8 +
playback); pagerfault/wal sweeps unchanged.

### S31.10 — U7c padded-prefix flavor (4.4.2.*, 11 leaves) — card if time runs out
- Harness: emulate `set nPadding [expr 511 - $::mj_filename_length]` +
  dirname mkdir + cd (a `cd` step changing the fixture cwd; faultsim paths
  follow cwd). The prefix = test.db + (nPadding%32) x's; full path = 511.
- Fixture: hand-pin the tn1=2 setup (eval $tcl block) as steps.

## Files expected to change
- internal/pager/pagercommit.go (commit order, phase split, xWrite/xTruncate
  events), internal/pager/pagerplayback.go (NEW — playback + delsuper),
  internal/pager/journal.go (hook events through vfsCall, remove old recovery
  or repoint), internal/pager/pager.go (pending byte, lckPgno, shmAvailable,
  mxPgno field plumbing), internal/pager/pageropen.go (hot-journal at open),
  internal/pager/pagerpage.go (lock-page CORRUPT read path),
  internal/exec/transaction.go (super-journal commit), internal/exec/hooks.go
  (SetShmAvailable), internal/execdml/insert_*.go (explicit-rowid conflict),
  internal/execquery/* (same-table subquery cursor isolation — 11.5),
  internal/execpragma/* (page_count counts ptrmap), frigolite_harness_test.go
  (reopen probe, faultsim_restore_reopen multi-conn, file_exists/glob_files/
  testctl_pending_byte/open_named_db ""/xTruncate result codes), plus new
  engine-level unit tests.
- testdata/pager1.json hand-pins ONLY (name-set equality checked by script).
- Remove the stray FAULTDBG stderr spam in pagercommit.go (ungated Fprintf at
  :319) — leftover debug.

## Verification per slice
FRIGOLITE_TEST=pager1 sweep + the full-family diff by NAME SETS
(pager, pager2, pagerfault, wal, wal2, wal3, jrnlmode, journal*, lock*,
tkt2820, analyze3) before each commit; quality gate on changed non-test files
(gocognit<=15, gocyclo<=12, file-size caps); pre-commit hook active.
