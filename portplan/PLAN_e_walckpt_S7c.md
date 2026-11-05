# PLAN — P-C1 S7c: e_walckpt blocking choreography + callback counters

State: 129 e_walckpt failures (pb12.txt == pb11.txt, live-verified 2026-11-05).
Pre-change sweep baselines: /tmp/sweep-wal-before.txt, /tmp/sweep-pager-before.txt.

## C contract (read and confirmed in sqlite 3.51 source)

- `sqlite3WalCheckpoint` (wal.c:4290-4408): CKPT lock FIRST (direct, no
  handler — R-53820-33897), then for non-PASSIVE the WRITER lock via
  `walBusyLock` (4335): do{lock}while(xBusy && busy && xBusy(arg)) — the
  handler is invoked once per contended attempt, retrying while it returns
  nonzero (retry). On BUSY: **demote** — eMode2=PASSIVE, xBusy2=0, rc=OK;
  the checkpoint still runs (passive backfill below readers' marks) and
  sqlite3WalCheckpoint returns SQLITE_BUSY (eMode!=eMode2). The triple is
  set (wal.c:4375 `if(rc==OK||BUSY)` INSIDE `if(rc==OK)`): demoted →
  {1, mxFrame, nBackfill}; CKPT-vetoed → outer guard skips → {1,-1,-1}
  (walvfs-7.1 stays green — it vetoes the CKPT lock, no handler involved).
- `walCheckpoint` (wal.c:2193-2375): read-mark pass via walBusyLock(xBusy)
  — give-up → mxSafeFrame=y AND xBusy=0 for the rest; READ_LOCK(0) +
  backfill; complete → truncate db to szDb + xSync db file (sync_counter);
  PASS2 RESTART/TRUNCATE: walBusyLock(READ_LOCK(1..4)) with the (possibly
  zeroed) xBusy; TRUNCATE truncates the -wal to 0.
- Busy counting (main.c:1760-1770, reset per statement at 2576): the
  handler receives the CURRENT count (first invocation n=0) and the count
  increments ONLY on a retry answer; a give-up poisons it. One count
  session spans the whole checkpoint statement (writer + marks + restart).
- TCL busy semantics (tclsqlite.c:672-686 DbBusyHandler): script result 0 →
  retry; nonzero (or error) → give up. The fixture: bm1 returns 1 (give up
  after 1 invocation); bm2 commits db2@n==5, db3@n==10, gives up at n==15;
  bm3 additionally commits db6@n==15.
- test_vfs.c:1574 `p->mask = TESTVFS_ALL_MASK` — an UNSET filter fires ALL
  methods; `tvfs filter X` restricts; `tvfs filter {}` silences. frigolite's
  hook never fires for unset — parity bug.

## Failure classes → fixes

1. no-such-table (1.full/restart/truncate.*.14/8): writer-lock give-up must
   DEMOTE to a passive backfill (today: skip everything → abc.db empty of t1).
2. COMMIT-expected-error (1.full.2.7/3.7 etc.) + busy_handler_counter(1) +
   seen_writer_lock + write_ok/read_ok + did_restart_blocking: the busy
   handler must be invoked during checkpoint lock acquisition (writer +
   marks + restart), with the C counting; harness registers it.
3. sync_counter (1.passive.1.7 want 1): complete backfill → db-file xSync.
   Needs the read-mark pass to succeed post-COMMIT (verify; also the
   unset-filter fix — tvfs_callback currently never fires in setup_11).
4. write_count (1.3.2.2 want 2): xWrite per backfilled page already emitted
   (wal.go:925); the var init is dropped by the transpiler (`set
   ::write_count 0` precedes the `tvfs script write_callback` seeding).
5. Transpiler drops: `db busy [list busy_handler ...]` (no step type) and
   any `set ::X` whose dynamic marking would come from a proc body captured
   LATER than the set.

## Changes (ordered, each committed after its diff gate)

### C1 — engine: pager busy handler + checkpoint choreography
- `internal/pager/pagerwal.go` (+ wal.go): `Pager.SetBusyHandler(fn)` →
  walWriter.busyHandler; engine `SetBusyHandler` forwards to each pager.
- `checkpoint()` (wal.go:681): CKPT lock FIRST (unchanged contract —
  {1,-1,-1} on veto); writer lock (non-passive) via new
  `walBusyLockHandler(walLockWrite,1)` — try; on busy invoke
  handler(count++) while it says retry; give-up → BUSY → **demote**:
  run the passes as PASSIVE with the handler disabled; result busy=1 with
  nLog/nCkpt from the demoted run (NOT {1,-1,-1}).
- `ckptPass1ReaderMarks` / `ckptBackfillPass` / `ckptPass2`: for non-passive
  modes with a live handler, contended lock attempts invoke the handler in
  the retry loop (shared count session); give-up ⇒ mark contended (or
  restart busy) and disable the handler thereafter (xBusy=0). Passive
  modes: unchanged try-once, handler never invoked.
- Gate: e_walckpt name-set diff vs pb12.txt (expect removals: seen_writer_lock,
  sync counters, no-such-table, write_ok/read_ok classes; busy counters that
  need the harness step stay). wal*/pager sweeps: NO new failures.

### C2 — harness: unset-filter = all-methods (test_vfs.c parity)
- `frigolite_harness_test.go` testvfsFixture: track filterConfigured;
  hook fires when !filterConfigured || filters[op]. `filter {}` still
  silences (empty set configured).
- Gate: wal*/pager sweeps — no new failures (any divergence would already
  be a C divergence).

### C3 — transpiler + fixture + harness: db busy + proc-seeded inits
- tclconvert `proc` definition: seedCallbackTargets(proc.Body) at definition
  time (precedent: preRegisterNotifyTargets) — fixes `set
  ::busy_handler_counter 0` / `set ::write_count 0` inits.
- tclconvert `db busy <proc-call>` → new step `register_busy_handler`
  (modeled on `db auth`): Database=conn, FunctionProc=proc, SQL=baked fixed
  args ("full 1"), CallbackProcs snapshot.
- Harness: execute register_busy_handler — Engine.SetBusyHandler wrapper
  that seeds the interp targets, invokes proc(mode…, count), syncs runtime
  vars back, maps TCL result 0→retry(true) else false.
- Regen e_walckpt fixture (`-only e_walckpt` → testdata/), verify no other
  family's fixture changes.
- Gate: e_walckpt diff (expect the two classes gone; ~30-60 remaining from
  deeper choreography), wal*/pager sweeps clean, then full go test.

## Post-unit bookkeeping
- TESTVFS_P_C1_PLAN.md record, TODO.md + HANDOFF refresh, lessons_learned.

## OUTCOME (2026-11-05, post-implementation)

e_walckpt: **129 -> 0** (pb24 empty; verified against pb12/pb11 baselines).

- C1 (committed eb2dc2f0e): CKPT-first order, writer-lock walBusyLock with
  the busy session (count 0-based, advance-on-retry), demote-to-PASSIVE on
  give-up, per-phase handler loops, mini WriterSections (nested sections
  self-deadlocked once — caught by the goroutine dump; the passes now own
  their sections). db-file sync gated on mxSafeFrame==mxFrame (partial
  checkpoints leave the file unsynced — wal.c:2309-2316).
- C2 (committed f91bd0c5f): unset-filter = ALL methods (test_vfs.c:1574);
  the tvfs interp builds lazily when the script binds after the open.
  Unmasked 2 wal2 exclusive-mode trace failures (wal2-6.4.5.2/6.4.8.2,
  previously vacuous) — queued as the exclusive-mode slice.
- C3 (this commit): `db busy` -> register_busy_handler (fixed args baked
  from the unrolled loop; the [list ...] word arrives unevaluated — the
  transpiler resolves $-args itself); handler registered on the engine,
  forwarded to every dbCtx pager; TCL result 0=retry/nonzero=give-up.
- Engine companions that C3 surfaced:
  - backfill dedupes by PAGE (walIterator semantics) — write_count was
    per-FRAME (5 vs 2).
  - BEGIN IMMEDIATE/EXCLUSIVE probes the WRITER shm byte (WALWriterProbe;
    sqlite3BtreeBeginTrans parity) — a checkpointer holding the byte
    blocks a concurrent BEGIN (write_ok=0).
  - PASSIVE checkpoints never invoke the handler (pager passes xBusy=0).
- Harness companions:
  - callbackCommands dispatch now PRECEDES the converter's commandHandlers
    and named-db capture (a callback interp's `db2 eval commit` drives the
    live connection); a RegisterDefaultCallbackCommand fallback resolves
    connection names against the live set per call (adapters bind objects
    at interp build time, scenarios reopen connections later).
  - registerInterpDBCommands extracted; the tvfs interp registers it too
    (tvfs_callback's db5 BEGIN probe / db6 reader BEGIN were silently
    transpile-captured before).
- Native test: TestProbeBusyHandlerCrossCommit (frigolite_walckpt_busy_probe_test.go)
  pins the cross-connection busy-handler commit contract + Query-BEGIN
  persistence + BEGIN-under-held-writer-byte.

## NEXT QUEUE (updated)

1. wal2 exclusive-mode xShmLock traces (wal2-6.4.5.2/6.4.8.2, unmasked by
   C2) — sqlite3WalExclusiveMode's enter/exit choreography.
2. Regen-drift follow-up: e_walauto/interrupt2/wal5 (S6 procedure).
3. wal2 (61->56?) / wal3 (5) xShmLock trace residue — re-baseline.
4. wal-18.2.x interrupt choreography (24).
5. Fix B: padToSectorBoundary + PSO/sector-size devchar (walvfs-5.3).
6. Sidecar lifecycle (WAL->rollback switch).
7. Census top non-WAL families.
8. Performance phase.
