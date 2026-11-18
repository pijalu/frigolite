# PLAN S35e — the no-shm WAL class (B1): e_wal's 13 leaves, ~150-250 LOC

Unit S35e: WAL works without shared memory ONLY under locking_mode=EXCLUSIVE
(C's bNoShm heap wal-index); otherwise refused. Extends
TRIAGE_wal_residue_2026-11-16.md §B1. Branches off feature/perf @ f4dd43f05.

## C contract (e_wal.test:19-200, the 3.51 oracle)

- **The heap path is EXCLUSIVE-mode-keyed, VFS-independent**: bNoShm =
  pPager->exclusiveMode at sqlite3WalOpen (pager.c:7596). EXCLUSIVE at the
  first WAL access → heap wal-index → **no -shm file ever exists**
  (1.1.4/1.2.3/1.3.3/2.1.2 assert {0 1}); NORMAL access → shm wal-index →
  -shm created (3.1's {1 1}).
- **journal_mode=WAL refused** on a shm-less VFS without EXCLUSIVE:
  1.1.1 answers `delete` (eNew=eOld), 3.4.2 answers `delete` on a fresh db.
- **The refusal is lazy + retried**: reopen on a WAL db via oldvfs → SELECT/
  INSERT fail CANTOPEN "unable to open database file" (1.2.1/1.3.1/3.4.1);
  `PRAGMA locking_mode = EXCLUSIVE` then access → WORKS (1.2.2/1.3.2) —
  the pager re-runs pagerOpenWal at the next read-txn with exclusiveMode
  set, and the heap path adopts the existing -wal (1.2.2 reads {1 2}).
- **locking_mode is stuck EXCLUSIVE** while the WAL runs on the heap index:
  NORMAL answers `exclusive` (2.2.1, a no-op — the pager1-7.1.6 rule).
  db2 (normal) gets `database is locked` (2.2.2, 3.2.2, 3.2.5).
- **The exit choreography**: journal_mode=DELETE exits WAL (2.3.1) but the
  file lock stays exclusive until locking_mode=NORMAL (2.3.2 db2 still
  locked; 2.3.3 answers `normal`; 2.3.4 db2 works).
- Flip-flopping locking_mode works freely on a SHM wal-index (3.2.1).

## Our divergence (probed 2026-11-18)

- attachWalAtOpen refuses whenever shmUnavailable — even under the EXCLUSIVE
  latch (1.1.2/1.2.2/1.3.2 wrongly fail); the refusal is never retried.
- openWal ALWAYS acquires the registry wal-index → a -shm file exists under
  EXCLUSIVE (the {0 1} asserts fail); there is no unregistered heap WALIndex.
- enableWALModeLocked has no shmUnavailable gate → the PRAGMA answers `wal`
  (1.1.1/3.4.2 want `delete`).
- The converter's commandTestVFS parses only `-default` — `testvfs oldvfs
  -iversion 1` loses the registration and e_wal's setup_0 dropped to a bare
  close_db (the fixture runs against the default VFS = vacuous).

## Delivery

1. **Converter** (tools/tclconvert/tcl/interp_stmt.go commandTestVFS):
   parse `-noshm 1` → SQL "noshm", `-iversion 1` → SQL "iversion" (the
   harness's register_testvfs already consumes both). testvfsOptionArity
   gains nothing (these take a value 1 — already covered by the loop shape).
   Regenerate testdata/e_wal.json from ori/sqlite/test (per-unit `-only`).
2. **Pager — the heap wal-index** (internal/pager): when the exclusive
   latch is set at WAL-open, openWal builds an UNREGISTERED heap WALIndex
   (fresh struct, shmFd=nil, pages in memory) instead of acquireWALIndex —
   no -shm file is created (C's bNoShm). release() skips the registry for
   heap indexes (a `heap` field). walInitHeader's heap branch (S35c.2's
   salt/capacity rebuild) already guards it.
3. **Pager — the refusal gate + retry**:
   - enableWALModeLocked: shmUnavailable && !walExclusiveLatched → refuse
     (a typed error; setJournalMode echoes the CURRENT mode — C's eNew=eOld,
     the 1.1.1/3.4.2 `delete` answers).
   - shmUnavailable && walExclusiveLatched → attach via the heap path.
   - attachWalAtOpen's walOpenRefused branch: the first access after the
     exclusive latch clears the refusal and re-attaches heap (the lazy
     retry of 1.2.2/1.3.2) — the per-statement shared-lock point retries
     attachWalAtOpen when p.walExclusiveLatched && p.walOpenRefused.
4. **Locking choreography** (already partially in): the WALHeapMemory
   NORMAL-refusal (2.2.1) exists (pager1-7.1.6); the EXCLUSIVE file lock
   must block db2's SHARED acquisitions in-process (2.2.2/3.2.2/3.2.5 —
   sharedA/pager1 machinery; verify, wire only if missing); journal_mode=
   DELETE from the heap WAL tears the heap index down without registry
   side effects; the exclusive FILE lock survives the DELETE until
   locking_mode=NORMAL (2.3.1-2.3.4).
5. Tests: pure-Go probe (exclusive-first WAL: no -shm file + reads work;
   normal-first: -shm exists; reopen-CANTOPEN + the exclusive retry; the
   NORMAL no-op answer; db2 BUSY), then the family sweeps.

## Verify

- FRIGOLITE_TEST=e_wal — 13→0 (python raw byte scan name-set diff).
- Identity: pager1 (the 21.x CANTOPEN class must stay), shared/sharedA/
  exclusive/snapshot3/snapshot4/wal/wal2/wal3/wal6/walvfs/e_walckpt/
  walckptnoop — leaf-identical or better (FULL family universe).
- go test -run 'TestSOLID_' ./...; go build ./...; the full gate at the
  unit merge.

## Baseline (f4dd43f05)

e_wal {1.1.1, 1.2.1, 1.3.1, 2.1.1, 2.2.1, 2.2.2, 2.3.1, 2.3.2, 2.3.3,
3.2.2, 3.2.5, 3.4.1} = 12 in the 1218 set (+1 flap = 13 observed).

## UPDATE 2026-11-18 (post-trace): the C choreography pinned from source

The reopen-CANTOPEN mechanism above was WRONG: a close's checkpoint+unlink is
correct C behavior (wal.c sqlite3WalClose L2495-2545, pager.c:4180-4200);
the reopen CANTOPEN comes from the version-2/2 MAIN HEADER requiring WAL.
Full source chain pinned this session (sqlite 3.51 tree @ ../sqlite):

1. **btree.c:3337 lockBtree**: `page1[19]==2 && !BTS_NO_WAL →
   sqlite3PagerOpenWal` — the WAL entry for a header-2/2 db with no -wal
   runs at the first SCHEMA-NEEDING statement's parse (parse.c cmd::=select
   → sqlite3ReadSchema; a bare PRAGMA does NOT load the schema — pragma
   productions never call it). So `PRAGMA locking_mode=EXCLUSIVE` in the
   same execsql as the first SELECT lands BEFORE the WAL opens.
2. **pager.c:7641 sqlite3PagerOpenWal**: `!sqlite3PagerWalSupported →
   SQLITE_CANTOPEN` (WalSupported = exclusiveMode || iVersion≥2,
   pager.c:7548) — the 1.2.1/1.3.1/3.4.1 refusal. pager.c:7647 sets
   journalMode=WAL on success.
3. **pager.c:7576-7604 pagerOpenWal**: `if( exclusiveMode )
   pagerExclusiveLock(pPager)` — the EXCLUSIVE main-file lock taken BEFORE
   sqlite3WalOpen (bNoShm heap index), HELD for the connection's life.
4. **pager.c:5928-5938 sqlite3PagerBegin**: a WAL write txn under
   exclusiveMode (WAL in NORMAL shm mode — the op=-1 dry run) takes
   `pagerLockDb(EXCLUSIVE)` and flips WalExclusiveMode(1). [Already
   ported: WALBeginWrite's exclusive flip — the FILE-lock take is new.]
5. **pager.c:2158-2162 pager_end_transaction**:
   `if( !exclusiveMode && (!useWal || WalExclusiveMode(pWal,0)) )
   pagerUnlockDb(SHARED)`. The op=0-on-ALREADY-NORMAL-wal path
   (wal.c:4471-4475) returns rc=0 → the unlock is SKIPPED → **a NORMAL
   WAL connection holds SHARED on the main file from its first txn on**
   (POSIX SHARED is compatible with SHARED — it blocks only another
   connection's EXCLUSIVE-file-lock acquisition: 3.2.5's {1 database is
   locked}).
6. **pager.c:7329 sqlite3PagerLockingMode**: the set is a NO-OP while
   sqlite3WalHeapMemory (2.2.1) — already ported (WALHeapMemory guard).
7. **pager.c:7663-7700 sqlite3PagerCloseWal** (the journal_mode=DELETE
   exit): pagerExclusiveLock + sqlite3WalClose — "An EXCLUSIVE lock may
   still be held on the database file after a successful return" (2.3.2:
   db2 still blocked after the DELETE exit; 2.3.3's locking_mode=NORMAL
   releases it).

### Our adaptation (open-time schema load divergence)

Our engine loads the schema AT OPEN (frigolite.go:301) and attaches the
WAL at open — C's "first access" is therefore open, EXCEPT the
locking_mode pragma arrives after open. Bridge: `walHeapEligible` on the
Pager — set by the 2/2-header-no-wal open attach (the C state "first WAL
access not yet happened"), cleared at the first statement-level WAL
read-txn entry (walIndexRefreshLocked — never called during open's
schema load; page reads don't go through it).

### Delivery items (pieces B + C)

1. **lockreg**: `walFileShared` per-path holder set (the piece-5 quirk:
   a WAL-mode connection's persistent file-SHARED) + SetWALFileShared /
   WALFileSharedByOther / ClearConn drop. Consulted ONLY by the
   EXCLUSIVE-file-lock take — never by reads or ordinary writes.
2. **Pager**: `walHeapEligible` flag; the conversion in
   SetWALExclusiveRequested(true) when eligible (RefCount==1, readLock<0,
   !writeLock): swap wi→newHeapWALIndex, heapMemory=true, old.release(),
   os.Remove(<db>-shm), re-run walInitHeader (the heap path re-validates
   against the -wal file — S35c.2 semantics). Contended conversion keeps
   the shm index (C's non-eligible choreography; unobserved in corpus).
3. **exec**: the quirk mark set per autocommit statement on WAL-mode
   pagers (before the ForeignMarks early-out); the EXCLUSIVE take
   (SetExclusive) when the conversion ran and at WAL write statements
   under exclusiveMode, gated on
   WALFileSharedByOther||SharedTxByOther||ExclusiveLockedByOther →
   "database is locked"; clearPersistentShared also clears the
   exclusive mark; WAL exit keeps the exclusive mark (item 7) and
   clears this connection's quirk mark.

## STATUS (2026-11-18): DELIVERED — e_wal 10→0, the WAL universe leaf-identical

Pieces (all landed on feature/perf, pre-merge):
- **Piece A** (the per-statement CANTOPEN): Pager.WALAttachRefusal wired
  into execDBFileChecks — gated on pinWAL (stmtSkipsWalReadPin statements
  — locking_mode MUST run to set the latch). The ReadPage hook alone
  missed the refusal when the schema caches stayed warm ("no such table"
  instead of "unable to open database file").
- **Piece B** (the heap conversion): walConvertHeapEligibleLocked at
  SetWALExclusiveRequested(true) — swap the registry index for
  newHeapWALIndex, os.Remove the -shm, walInitHeader re-validates from
  the -wal file. Window: `!walDataTouched` — set at the first
  statement-level WAL entry (the gate's refresh / WALBeginWrite), with
  SuppressAccessMark bracketing the open's own schema load and the
  harness reopen prime (C's lazy sqlite3_open is not an access;
  wal2-6.6.1's real page-1 SELECT IS — the prime/page-scope distinction
  was the second design iteration).
- **Piece C** (the EXCLUSIVE file-lock hold): lockreg.walFileShared (the
  NORMAL-WAL persistent file-SHARED — pager.c:2158's unlock-skip quirk;
  consulted ONLY by the EXCLUSIVE acquisition) + e.walExclusiveFileHeld
  (the take at the conversion and at WAL+exclusive write statements —
  pager.c:5928; released by clearPersistentShared; survives the
  journal_mode=DELETE exit per pager.c:7663). The locking_mode=NORMAL
  no-op on a heap index (WALHeapMemory) already existed.
- The sqlite_master LIMIT 1 harness prime is wrapped in
  SuppressAccessMark (frigolite_harness_test.go).

Verify: e_wal 10→0 (raw-scan); the 27-family identity diff vs a
6b7513599 worktree run — EVERY family leaf-identical (e_walhook 10=10,
wal2 25=25 after the suppression fix, walckptnoop 5=5, resetdb 7=7 —
resetdb-300's worktree pass was a contention flake; it fails at both).
Full gate: portplan/baselines/s35e_gate_fails.txt (the durable baseline —
/tmp baselines proved ephemeral across sessions).
