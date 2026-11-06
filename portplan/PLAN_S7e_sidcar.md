# PLAN — S7e: sidecar lifecycle (WAL→rollback switch + close-time deletion + reopen re-attach)

State: e_walckpt 0 (4.2-4.5 superseded — the C source's expectations predate
its own 3.51 oracle, a33195076). Targets: wal-24.2 (`file exists test.db-wal
→ 0` after close), walbak-4.3.1/4.3.3 (the journal_mode switch + db2 CREATE).

## C contract (verified in source + live oracle)

- **Switch** (vdbe.c:8040-8095 OP_JournalMode): transaction guard →
  `sqlite3PagerCloseWal` (pager.c) → `sqlite3WalClose` (wal.c:2488):
  EXCLUSIVE file lock (fails BUSY under a second connection → the switch
  fails, eNew=eOld) → PASSIVE checkpoint (no busy handler; the sole
  connection guarantees full backfill) → persist-wal check (unset in the
  tests → isDelete=1) → walIndexClose deletes the -shm → the -wal fd
  closes → the -wal UNLINKED. **The EXCLUSIVE lock is not released.**
  Then `sqlite3BtreeSetVersion(pBt, 1)` — the version bytes 18/19 = 1/1
  written through a ROLLBACK-journal transaction (vdbe.c:8069) — AFTER
  the WAL is gone (otherwise the header change lands as a WAL frame that
  the deletion destroys).
- **Close-time** (sqlite3PagerClose → sqlite3WalClose, same body): the
  last-connection close checkpoints and DELETES both sidecars when the
  checkpoint succeeded (isDelete=1); the version bytes STAY 2/2. Oracle
  probe (testfixture 3.51, /tmp/probe4x.test): after a clean close
  `wal-exists-after-close: 0`; the reopen reports journal_mode=wal
  (btree.c:3337 — a 2/2-header file re-attaches the WAL via
  sqlite3PagerOpenWal, CREATING the -wal when absent) and the checkpoint
  returns {0 0 0}.
- The e_walckpt sweep's close leaves the -wal PRESENT at 0 bytes — its
  close-time checkpoint raced a contended lock (isDelete=0); both close
  outcomes are legitimate C behavior depending on state.

## Ordering (the wal-24.x hazard)

1. Sole-connection gate (wi.RefCount()==1) else BUSY.
2. End this connection's read txn; PASSIVE checkpoint (full backfill).
3. Detach: w.Close(), p.wal=nil, DELETE -shm + -wal files.
4. ONLY THEN the version bytes 1/1 flushed through the rollback path
   (p.mu held, p.wal nil — the flush must never route through the WAL).

## Changes

- `pagerconfig.go` SetJournalMode: WAL→legacy goes through the new
  leaveWALModeLocked(m) (steps 1-4 above) instead of setLegacy's bare
  w.Close(). The transaction-deferred path (pendingJournalMode) is
  untouched (jrnlmode3-3.3 pinned).
- `pager.go` walCloseCheckpoint: truncate-0 → DELETE -wal + -shm on the
  successful close-time checkpoint (version bytes stay 2/2).
- `external.go` CheckExternalFile: a non-empty file whose header says
  2/2 with no -wal → openWal (btree.c:3337 parity — the WAL identity
  survives close/reopen once close deletes).

## Verification

- e_walckpt (0 baseline), wal sweep, pager sweep — name-set diffs, 0 new.
- wal-24.2, walbak-4.3.1/4.3.3 expected to fix; walbak's other residue
  and wal-24.3-24.5 (auto_vacuum sizes — a different gap) as-is.

## IMPLEMENTATION FINDINGS (2026-11-06 — the attempt was REVERTED; the unit stays queued)

The three-piece implementation (leaveWALModeLocked, the close-time
deletion, the open-time 2/2 re-attach) cascaded to 428 wal-group
failures. Concrete hazards found — the next attempt starts here:

1. **The double-close trap**: walWriter.Close() early-returns when
   w.file is already nil WITHOUT releasing the wal-index reference —
   pre-closing w.file left p.wal pointing at a dead writer (wi=nil) and
   the next statement's CheckExternalFileErr panicked on the nil
   wal-index. The detach must be ONE w.Close() call (it sets w.file=nil
   itself; a pre-close makes the file-close error surface AFTER the wi
   release).
2. **The 2/2 reopen is a readOnly trap**: a 2/2-header file reopened
   without a -wal hits pageropen.go:137 (`header[18] > 1 && wal == nil`
   → readOnly) and the schema-init reports "file is not a database".
   The re-attach must happen at open (attachWalAtOpen) — but doing it
   there cascaded e_walckpt (per-scenario forcedelete + reopen cycles
   saw stale content: "table t1 already exists"), so the interaction
   with the harness's per-scenario file lifecycle needs its own design
   (probably: re-attach only when the -shm registry has no live entry
   AND the -wal is absent AND the file was NOT just force-deleted).
3. **The e_walckpt passive scenarios broke** (1.passive.1.5's sync fired
   on a partial checkpoint; setup_9's db2 COMMIT "no transaction is
   active") — the close-time deletion changed the per-scenario reopen
   state that the mode-sweep's reader choreography depends on.

The switch itself (leaveWALModeLocked minus the double-close) was never
isolated — bisect it ALONE first next time, then the close-time deletion,
then the open-time re-attach, each behind the e_walckpt + wal-24 + walbak
diffs.

## BISECTION RESULT (2026-11-06)

- **Piece 1 — the switch — LANDED (1ab97052c)**: leaveWALModeLocked with
  ONE w.Close() call (no pre-close). walbak 22 → 17 (4.3.1/4.3.3 +
  4.1.1-3); wal sweep 201 → 178 (23 fixed, 0 new); e_walckpt stays 0;
  pager unchanged.
- **Piece 3 — the 2/2-reopen re-attach — LANDED (5d9f97c3f)**: attachWal
  AtOpen creates the WAL for a 2/2-header file without a -wal (btree.c:
  3337 parity). Innocent and C-faithful; e_walckpt stays 0.
- **Piece 2 — the close-time deletion — STILL QUEUED (reverted twice)**:
  with deletion + re-attach together, e_walckpt's mode-sweep cascades at
  setup_9 ("cannot commit - no transaction is active" on db2 — the
  reader's transaction is lost across the per-scenario reopen boundary —
  and then "table t1 already exists"). With deletion-off + re-attach-on
  everything is green, so the deletion itself changes state the sweep's
  reader choreography depends on. Next attempt: debug setup_9's db2
  transaction lifecycle at the harness level with the deletion ON
  (which step ends the txn — the close-time path runs INSIDE Pager.Close
  after flushAll; the -shm removal races the wi registry entry?) before
  touching the deletion again.

## PIECE 2 LANDED (2026-11-06, b72123592) — the root cause was a PATH BUG

The setup_9 question resolved: the transaction was never lost — the
DELETION was a silent no-op. w.path IS the "-wal" path; the first attempt
removed "t.db-wal-wal"/"t.db-wal-shm" (nothing), so every sidecar
survived, the journal_mode switches resurrected the old generation at the
next attach (the stale-hash reads: "table t1 already exists" /
"no such table: t2"), and the -shm/-wal paths derive from the DATABASE
path ("t.db-shm" = TrimSuffix(w.path, "-wal")+"-shm").

Landed: the close-time deletion (correct paths), the garbage rule at OPEN
gated on the registry (walIndexLiveEntry — a held log is never deleted),
the BUSY-echo for the contended journal_mode switch (vdbe.c:8087), and
TestWalCloseCheckpointSidecars (the native lifecycle pin).

Results: wal sweep 201 -> 165 (36 net fixed; wal-24.2 passes), walbak 17
-> 15, e_walckpt 0, pager sweep unchanged, 0 regressions.
Residue: wal-12.4 (ONE case: the copied-pair scenario's frame-level
divergence — the copied -wal/hash interaction under the leaked-
connection wi; needs a frame-level dump), walbak-4.3.5/4.4.5 (the BACKUP
unit: backup.c:430 sqlite3BtreeSetVersion(p->pDest, 2) — the backup
destination's journal-mode handling, a separate slice).
