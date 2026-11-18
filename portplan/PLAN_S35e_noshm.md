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
