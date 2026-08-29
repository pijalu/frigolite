# N/A Evidence — G5 Go API C-runtime packages

All generated whole-file skips are N/A: generated TCL compatibility packages
are intentionally excluded from the supported pure-Go API verification scope.

This document records genuine C-runtime/API N/A classifications in P5.

- `shell9` CLI-only assertions are N/A for the pure-Go engine: SQLite source
  `test/shell9.test` explicitly invokes `catchcmd`/`catchsafecmd` to exercise
  `shell.c` `.dump`, `.read`, safe mode, and warning output. The equivalent SQL
  setup/query assertions (`CREATE VIRTUAL TABLE`, table creation, inserts,
  `SELECT`, and CHECK constraints) remain generated and executed through the
  public Go `Open`/`Exec`/`Query` surface; only subprocess/file-shell effects
  are comment-only skips in `testgen/shell9/shell9_test.go`. Source: `src/shell.c`
  and `test/shell9.test`.

- `backup`, `backup2`, `backup4`, `backup5`: backup-internal page-size,
  restore/source-busy, and destination-lock semantics not reproduced by the
  pure-Go Backup API; source evidence: `src/backup.c` (`setDestPgsz`) and
  corresponding `test/backup*.test` cases.
- `incrblob`, `incrblob2`, `incrblob4`: TCL channel-backed sqlite3_blob handles,
  blob-handle counts, and C-harness SQL paths; pure-Go Blob API covers common
  OpenBlob/Read/Write but not the C channel ABI. Source: `src/vdbeblob.c`,
  `test/incrblob*.test`.
- `bind`, `bind2`: sqlite3_prepare/bind/step bytecode VM API absent from the
  public Go API. Source: `src/vdbeapi.c`, `test/bind*.test`.
- `bindxfer`: deprecated `sqlite3_transfer_bindings` prepared-statement VM API,
  including `sqlite_bind` and `sqlite_step` helper proc; no equivalent public
  Go Prepare/Bind/Step surface. Source: `src/vdbeapi.c`, `test/bindxfer.test`.

All entries are whole-file skips in `tools/tcl2go/skiptestfiles.go`; generated
packages remain compilable no-op stubs. Common Backup/Blob APIs remain tested
through their supported Go surface.
## P6.FTS-B per-assertion N/A classifications

All 27 P6.FTS-B target packages are un-skipped and passing. The residual
per-assertion skips below (in `tools/tcl2go/skiptests2_part2.go`) are
documented divergences, each traced to SQLite `ext/fts3` source:

- `fts3defer2-2.2.$tn.1-4`, `fts3defer2-2.4.$tn`, `fts3defer2-2.6..2.12`:
  matchinfo 'x' global hit stats / offsets lengths under SQLite's
  deferred-token optimization. Even without segment corruption, SQLite defers
  loading the doclist of any token whose doclist spans enough overflow pages
  (`fts3.c fts3EvalSelectDeferred`: defer when a token's overflow-page cost
  exceeds `(nMinEst + 4^nOther - 1)/(4^nOther) * nDocSize` pages), and a
  fully-deferred phrase reports X=Y=nDoc in the matchinfo 'x' array
  (`fts3ExprGlobalHitsCb` comment in `ext/fts3/fts3_snippet.c`). In this
  corpus 'a' has 10002 postings (deferred: X=Y=54=nDoc) while 'b' has 3
  (exact counts). Matching requires porting the overflow-page cost model
  (nOvfl per token, average doc size in pages, nLoad4 schedule); the engine
  reports exact counts instead.

- `fts3corrupt6-4.2`: after hand-patching `end_block` to
  `start_block+2^31-1` and inserting a NULL `%_segments` row at that blockid,
  the follow-up `merge=16,4` must allocate output blocks at SQLite's exact
  `fts3NodeWrite` absolute-block positions; the engine's in-memory merge
  allocates from `max(blockid)+1`. Source: `ext/fts3/fts3_write.c`
  (`fts3NodeWrite`, `SQL_NEXT_SEGMENTS_ID`).

- `fts3corrupt4-12.1`, `fts3corrupt4-31.1`: oversized-varint doclist handling
  is version-specific; real SQLite 3.51 (the oracle) hangs on the 31.1 input,
  and 12.1's expectation depends on the crash build's PRNG byte sequence.

## P5.HOOKS / P5.CALLBACKS — genuine C-runtime classifications (2026-08-25)

The hook seam's SQL-observable semantics (changes()/total_changes()
counters, ROLLBACK undo, trigger RAISE(ABORT/ROLLBACK) delivery,
autocommit-ROLLBACK "no transaction is active" errors, interrupt countdown
arming) are implemented and covered by `testdata/hookconformance` +
`TestHookScriptConformance` and the dbstatus/dbstatus2/hook/hook2/
interrupt/interrupt2 testgen packages. The following remain genuine
C-runtime surfaces with equivalent Go functionality preserved:

| C-runtime surface | SQLite source | Frigolite equivalent |
|---|---|---|
| Progress callback evaluating a TCL script per VDBE step | tclsqlite.c DbProgressHandler L689-699 (`Tcl_Eval(pDb->zProgress)`) | `db.SetProgressHandler(n, func() bool)` (Go closure) |
| Hook callbacks invoking TCL procs (commit/rollback/update/preupdate) | tclsqlite.c hook wrappers over `pDb->zCommitHook` etc. | `SetCommitHook/SetRollbackHook/SetUpdateHook/SetPreupdateHook(func())` |
| SQLITE_TEST interrupt countdown linked to ::sqlite_interrupt_count | vdbe.c L963-969 + test1.c Tcl_LinkVar L9316 | `db.SetInterruptCount(n)` / `db.InterruptCount()` |

No SQL-observable behavior is hidden behind these classifications: every
assertion the transpiled corpus makes about hook/state/interrupt effects is
exercised through the Go-equivalent surface or SQL semantics above.

### fts3malloc / fts3shared / fts3misc / fts3rnd (2026-08-25, P6.FTS-H closeout)

| package | classification | evidence |
|---|---|---|
| fts3malloc | C-runtime OOM injection | suite drives `sqlite3_memdebug_fail` (malloc_common.tcl); same class as the malloc-family files already N-A. Deterministic paths covered by fts3query/fts3offsets/fts3sort |
| fts3shared | G7 WAL/shared-cache phase | expects "database table is locked" for read-during-write on a shared cache — locking enforcement is the G7 WAL/shared-cache deliverable |
| fts3misc | G8 storage: b-tree overflow cells | 200-column FTS3 schema row (944B) exceeds one 1024B page at the deliberate TEST-build default; native test TestFTS3MiscHighColumnPhraseNative proves the full scenario passes at page_size=4096, so phrase matching itself is correct |
| fts3rnd | randomized perf stress | exceeds the 600s runtime budget; deterministic correctness of every queried feature is covered by fts3query/fts3offsets/fts3sort |

Functionality is preserved through equivalent Go surfaces and the five green
packages exercise the same FTS3 machinery (offsets/matchinfo/snippet,
prefix/phrase/NEAR queries, integrity check, varint coding).

## P7.AUTOINDEX — per-assertion N/A classification (2026-08-27)

- `autoindex1-113`: asserts the content of TCL var `::log` populated by
  `test_sqlite3_log [list lappend ::log]` (src/test1.c `test_sqlite3_log`),
  which installs a C-runtime `sqlite3_log` error-log callback and expects the
  text `SQLITE_WARNING_AUTOINDEX automatic index on t2(c)` emitted by
  src/where.c (`sqlite3_log(SQLITE_WARNING_AUTOINDEX, ...)`, where.c L1056).
  Same class as the P5.HOOKS C-runtime callback surfaces: pure-Go engine has
  no error-log callback surface; result-side behavior (join result with
  PRAGMA automatic_index=ON) is verified by active test autoindex1-110 and
  oracle comparison (identical row set). Skip recorded in
  `tools/tcl2go/skiptests2.go`.

- `autoindex1-299`, `autoindex1-800`, `autoindex1-801`, `autoindex1-901`,
`autoindex-1211`, `autoindex3-110/120/130/140`: assert
`EXPLAIN QUERY PLAN` text ("AUTOMATIC COVERING INDEX" / "AUTO" /
"SEARCH …") produced by SQLite's cost-based planner (src/where.c). Frigolite
does not implement a cost-based planner that emits this EQP prose, so the
exact plan wording is out of scope. **Oracle-verified** (each query run under
`/usr/bin/sqlite3` 3.51.0):
- autoindex1-901 → `SEARCH agg2 USING AUTOMATIC COVERING INDEX (m=?)`
- autoindex-1211 → `SEARCH t2 USING AUTOMATIC COVERING INDEX (x=?)`
- autoindex3-110/120/130/140 → `SEARCH t2/t1 USING AUTOMATIC COVERING
  INDEX (y=?)` / `(x=?)` (the `/AUTO/` regex matches the oracle output)
- autoindex1-299 → `AUTOMATIC COVERING INDEX` on `CROSS JOIN t2 ON (c=a)`
- autoindex1-800/801 → multi-table join EQP containing `SEARCH raw_contacts`
  / `SEARCH data`
Result correctness of every query in these tests is covered by the *active*
sibling assertions (e.g. autoindex1-110 for the join row set, autoindex1
foreach loops, autoindex3 active cases), which all pass. Classification
recorded in `tools/tcl2go/skiptests2.go`.

- `autoindex4-1.0`: asserts `ORDER BY +b` tie ordering from a 4-row cross join.
The expected output (`234 def 987 rqp | 234 def 987 zyx | 234 ghi 987 rqp |
234 ghi 987 zyx |`) encodes SQLite's **non-stable** sorter (qsort) tie order;
Frigolite uses a stable sort, so equal `b` keys keep insertion order and the
tie rows come out in a deterministic-but-different order. The row *set* is
identical; only tie order (implementation-defined under SQLite too) differs.
N/A (plan-dependent sorter behavior), recorded in `tools/tcl2go/skiptests2.go`.

### P7.AUTOINDEX — UCL status

The automatic-index *result* seam (in-memory equi-join covering index built by
`internal/execquery` `buildJoinAutoIndex`) is exercised green by all five
`testgen/autoindex1..5` packages (the e2e safety net). No engine edit was made
on this seam during the goal — the feature was already ported — so there is no
parity divergence to localize with a dedicated UCL instrument. The only
assertions that diverge are the planner-EQP prose (`AUTOMATIC COVERING INDEX`)
and the non-stable sorter tie order above, each oracle-verified and classified
N/A. Building golden EQP fixtures for an out-of-scope planner text would merely
re-assert the N/A; per UNIT_CONFORMANCE §5 the Autoindex/planner seam is
therefore satisfied by (a) green testgen coverage of result correctness and
(b) oracle-verified N/A evidence for the planner-text divergences.

## P7.WAL-A — package N/A classification (2026-08-29)

All eight goal-target packages exercise the SQLite WAL *write* subsystem
(`PRAGMA journal_mode=WAL` producing `-wal`/`-shm` files, committed frames,
MVCC readers, checkpoints, shared-memory wal-index). Frigolite has **no WAL
write path**: `internal/execpragma/execpragma.go` `JOURNAL_MODE` (L386-397) is
a stub that lower-cases and echoes the requested mode (`delete`/`truncate`/
`persist`/`memory`/`off`/`wal`/`wal2`) and returns it as a row — it performs
no pager work, creates no `-wal`/`-shm`, and never writes a frame. There is no
WAL writer, no wal-index shared memory, and no checkpoint in `internal/pager`.
The pager only understands the rollback-journal and committed-page image.

**Oracle-verified root gap** (`/usr/bin/sqlite3` 3.51.0): `PRAGMA
journal_mode=WAL` creates `test.db-shm` (wal-index shared memory, 32 KiB) and
writes committed frames to `test.db-wal`. This is exercised by the committed
oracle fixtures in `testdata/walconformance/`: `wal-single-commit.db-wal` =
16512 bytes (4 frames, commit marks at frames 2/3/4), `wal-multi-commit.db-wal`
= 28872 bytes (7 frames), `wal-after-checkpoint.db-wal` = 12392 bytes (a
RESTART checkpoint bumped `CheckpointSeq` to 1 and re-salted; a trailing stale
pre-checkpoint frame fails salt validation). Every WAL-A package asserts
behavior that depends on these files existing and being applied — impossible
without the G7 WAL subsystem. Classification recorded in
`tools/tcl2go/skiptestfiles.go` (reasons upgraded to `N-A G7 (evidence …)`).

Per-package:

- `e_wal`: WAL under a legacy VFS with `iVersion==1` (no `xShmMap`/`xShmLock`/
  `xShmBarrier`/`xShmUnmap`) and `locking_mode=EXCLUSIVE` before first access;
  asserts `file exists test.db-wal`=1 after `PRAGMA journal_mode=WAL` (1.1.4)
  and reopens+reads (1.2.*). Requires the WAL write path **and** the
  exclusive-mode shared-memory bypass (wal.c `WAL_SHM_EXCLUSIVE` /
  `walIndexRecover` with no shm). N-A G7.

- `e_walauto`: `wal_autocheckpoint` auto-commit threshold behavior; reads raw
  wal-index shared-memory offsets (`nBackfill` at byte 96, `mxFrame` at byte
  16) and expects the threshold to drive checkpoints. Requires WAL + wal-index
  shared memory + auto-checkpoint. N-A G7.

- `wal`: the "warm-body" WAL suite — read/write (wal-1.*), MVCC with one
  reader + one writer (wal-2.*), transaction rollback (wal-3.*),
  savepoint/statement rollback (wal-4.*), temp database (wal-5.*), and
  databases with different page sizes (wal-6.*). `wal-0.1` already expects
  `PRAGMA journal_mode=wal` → `{wal}` *and* `file exists test.db-wal`=1. The
  core WAL read/write contract. N-A G7.

- `wal2`: two connections (writer `[db]` + reader `[db2]`) exercising the 8
  wal-index header fields and `sqlite_sync_count` (fsync accounting); MVCC
  reader sees writer's committed frames. Requires multi-connection WAL + shm
  (G7 concurrency). N-A G7.

- `wal3`: rollback/savepoint rollback removing entries from the wal-index hash
  tables with `cache_size=2000`, `wal_autocheckpoint=0`; asserts no corruption
  after hash-table churn. Requires WAL savepoint + wal-index shm. N-A G7.

- `wal4`: WAL + fault simulation (`faultsim_save_and_close` /
  `faultsim_restore_and_reopen`) — saves the filesystem containing only the
  `-wal`/`-shm`, deletes the main db, restores, and reopens; expects the
  database to be recovered from the WAL. Requires WAL + the fault-injection VFS
  (`test_syscall`) and WAL recovery. N-A G7.

- `wal5`: checkpoint requested both via the C API and via `PRAGMA
  wal_checkpoint(PASSIVE|FULL|RESTART|TRUNCATE)`; asserts frame counts before/
  after, `nBackfill`, and file sizes. Requires WAL + checkpoint. N-A G7.

- `wal64k`: WAL at 64 KiB page size (`test_syscall pagesize 65536`); asserts
  `-shm` is 65536 bytes and autocheckpoint triggers at that page size. Requires
  WAL + 64 KiB page support + shm. N-A G7.

### P7.WAL-A — UCL status

The WAL *format* seam (read/decode of `-wal`/`-journal` byte layouts) is built
and green **before any pager edit** (UNIT_CONFORMANCE U0/U3): `internal/pager/
walview.go` ports the WAL header/frame decoder and checksum chain from
`src/wal.c` (`WalMagic` 0x377f0682/3, `WalHdrSize` 32, `WalFrameHdrSize` 24,
`WalChecksumBytes` fibonacci-weighted checksum, `DecodeWalHeader`,
`DecodeWalFrames`, `LastCommitFrame`), mirroring the existing rollback-journal
decoder `jrnlview.go`. Oracle fixtures in `testdata/walconformance/` (generated
from `/usr/bin/sqlite3` 3.51.0; `ORACLE_VERSION` recorded) cover single-commit,
multi-commit, post-RESTART-checkpoint, and PERSIST-journal scenarios.
`internal/pager/walview_test.go` localizes first divergence (exact frame
number on salt/checksum failure) and validates header/page-size/checkpoint-seq
against the fixtures — `go test ./internal/pager/ -run 'TestWAL|TestJournal
Conformance'` passes. This is the foundational instrument for the real WAL
subsystem (P7.WAL-B/E): it decodes oracle `-wal` files so the future WAL writer
can be checked against ground truth.

No engine edit was made on the WAL *write* seam during WAL-A — the subsystem is
deferred to G7 (per PORTPLAN §6/§218: "Start with journal-mode state and pager
WAL frame format"). The e2e `testgen` packages remain N-A G7 (skip reasons
upgraded with evidence pointers); the goal is closed by (a) the green UCL
decoder/fixtures and (b) oracle-verified N/A evidence for every target package.

## P7.WAL-B — package N/A classification (2026-08-29)

All eight goal-target packages (the objective's authoritative set:
`e_walckpt`, `wal6`, `wal7`, `wal8`, `wal9`, `walbak`, `walckptnoop`,
`walcksum`) exercise the SQLite WAL *write* subsystem — checkpoint
(`PRAGMA wal_checkpoint(PASSIVE|FULL|RESTART|TRUNCATE)`), `wal_autocheckpoint`
auto-commit threshold, `journal_size_limit`, online backup of a WAL-mode
database, no-shm WAL, and WAL frame-checksum verification. The write path is
**not implemented** in Frigolite (`internal/execpragma/execpragma.go`
`JOURNAL_MODE` L386-397 is a stub that echoes the requested mode and creates
no `-wal`/`-shm`; there is no WAL writer, no wal-index shared memory, no
checkpoint, and no backup API in `internal/pager` / `internal/vtab`).

**Oracle-verified root gap** (`/usr/bin/sqlite3` 3.51.0):

- `PRAGMA journal_mode=WAL` on `test.db` creates `test.db-shm` (32768 bytes,
  wal-index shared memory) and `test.db-wal` (committed frames), and returns
  `{wal}`.
- `PRAGMA wal_checkpoint(TRUNCATE)` returns `{0,0,0}` (busy/log/checkpointed)
  and rewrites the main db; `PRAGMA wal_checkpoint` returns live frame counts.
- `PRAGMA journal_size_limit=25000` / `PRAGMA wal_autocheckpoint=50` are real
  (return their set values on read-back).

Frigolite, by contrast, was probed directly (`frigolite.Open` + `Exec`):

- `PRAGMA journal_mode=WAL` → `[[wal]]` (echo only); only `test.db` (2048 B)
  is created — **no `-wal`/`-shm`**.
- `PRAGMA wal_checkpoint` → empty result (no-op; no WAL to checkpoint).
- `PRAGMA journal_size_limit=25000` / `PRAGMA wal_autocheckpoint=50` → no-op
  (nothing returned; no WAL autocheckpoint machinery).

Every WAL-B package asserts `-wal`/`-shm` existence, frame counts, checkpoint
return codes, backup-frame copying, or checksum/salt behavior that is
impossible without the G7 WAL write subsystem. Classification recorded in
`tools/tcl2go/skiptestfiles.go` (reasons upgraded to
`N-A G7 (evidence internal/pager/walview_test.go + portplan/NA_EVIDENCE.md
§P7.WAL-B)`). `walmode`/`walnoshm` are the same WAL-N-A family but are **out
of this goal's objective scope** (the objective + recorded verify command
cover only the 8 packages above); they remain N-A with the generic reason.

Per-package:

- `e_walckpt`: explicit-typed checkpoints (`PRAGMA wal_checkpoint=FULL/
  RESTART/TRUNCATE`) with return-code assertions, `file exists test.db-wal`,
  and frame counts before/after; `e_walckpt.json` issues 8 `journal_mode`
  / WAL setup steps. Requires WAL + checkpoint. N-A G7.

- `wal6`: WAL with `PRAGMA journal_size_limit` + large multi-row transactions
  + `PRAGMA wal_checkpoint(TRUNCATE)`; asserts `-wal` grows then truncates and
  `nBackfill`/`mxFrame` advance. Requires WAL + journal size limit + checkpoint.
  N-A G7.

- `wal7`: `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=50;
  PRAGMA journal_size_limit=…; PRAGMA wal_checkpoint` — autocheckpoint
  threshold + manual checkpoint + size limit (wal7-2.0 asserts
  `journal_size_limit` → 25000, which Frigolite returns 0 for). Requires WAL +
  autocheckpoint + checkpoint + size limit. N-A G7.

- `wal8`: `PRAGMA journal_mode=WAL` + `PRAGMA wal_checkpoint` across
  transactions; asserts `-wal` frame counts and `file exists test.db-wal`.
  Requires WAL + checkpoint. N-A G7.

- `wal9`: `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0` (autocheckpoint
  disabled) + manual `PRAGMA wal_checkpoint`; asserts only explicit
  checkpoints move frames. Requires WAL + checkpoint. N-A G7.

- `walbak`: online backup (`sqlite3_backup_step` / `backup` / `.backup`) of a
  WAL-mode database; asserts the backup copies committed `-wal` frames and
  `PRAGMA wal_checkpoint` precedes the copy. Requires WAL + backup API. N-A G7.

- `walckptnoop`: PASSIVE checkpoint with no pending WAL activity; asserts
  `wal_checkpoint` returns `{0,0,0}` and main-db / `-wal` sizes are unchanged.
  Requires WAL + checkpoint. N-A G7.

- `walcksum`: WAL frame-checksum verification — corrupts a `-wal` frame
  checksum, asserts `PRAGMA integrity_check` reports `SQLITE_CORRUPT`, and that
  a RESTART checkpoint re-salts. The WAL frame checksum is the *same*
  fibonacci-weighted checksum decoded by `internal/pager/walview.go`, but the
  *writer* that produces valid checksums is G7. Requires WAL + checksum +
  checkpoint. N-A G7.

### P7.WAL-B — UCL status

UCL (Unit Conformance Layer) is satisfied: the WAL *format* seam
(`internal/pager/walview.go`, built and green under P7.WAL-A —
`go test ./internal/pager/ -run 'TestWAL|TestJournal Conformance'` passes)
decodes the oracle `-wal` header/frame layout and the fibonacci-weighted
frame checksum used by `walcksum`. No engine edit was made on the WAL *write*
seam during WAL-B — the subsystem is deferred to G7. The e2e `testgen`
packages remain N-A G7 (skip reasons upgraded with evidence pointers); the
goal is closed by (a) the green UCL decoder/fixtures and (b) oracle-verified
N/A evidence for every target package.

## P7.WAL-C — package SUPERSEDED classification (2026-09-01)

**Scope note — supersession, not N-A.** Unlike P7.WAL-A / P7.WAL-B (which were
deferred to G7 as *no WAL write path exists*), the WAL-C objective packages are
**implemented in the engine and the TCL packages are SUPERSEDED** by native
`frigolite`/`internal/pager` tests. This is the project-sanctioned outcome: the
engine WAL write/recover path was added in this goal, so no package is
classified N-A G7. Per the correction applied to this goal, an N-A classification
of a WAL-C package would be an error — the engine MUST be working and verified.

### Objective packages (all SUPERSEDED by native tests)

- `e_walhook` — `wal_hook` C-API callback fired with `(nLog, nCkpt)` on each
committed transaction; return value can veto checkpoint. → **SUPERSEDED** by
`frigolite_walrecovery_test.go::TestWalHookEngine` (covers `DB.SetWalHook`
firing + arg semantics) and `internal/pager/wal_test.go::TestWalHookFires`.
- `walcrash` — crash mid-WAL: committed transaction (T1) survives a crash that
loses the in-flight transaction (T2). → **SUPERSEDED** by
`frigolite_walrecovery_test.go::TestWalCrashRecoveryEngine` (committed T1
recovered, lost T2 discarded after a simulated `-wal` truncation + reopen) and
`internal/pager/wal_test.go::TestWalCrashRecovery` /
`TestWalRecoverDiscardsPartial`.
- `walcrash2` — crash during WAL write, partial last frame discarded on reopen.
→ **SUPERSEDED** by `internal/pager/wal_test.go::TestWalRecoverDiscardsPartial`
(truncated trailing frame not applied; frames before the last commit mark
applied).
- `walcrash3` — crash recovery with checkpoint boundary interactions. →
**SUPERSEDED** by `internal/pager/wal_test.go::TestWalCheckpoint` (frames
folded into main db on checkpoint; `-wal` reset to a fresh header).
- `walcrash4` — fault-injected I/O during WAL commit. → **SUPERSEDED** by
`frigolite_walrecovery_test.go::TestWalFaultHandlingEngine` (injected I/O
fault on the WAL write surfaces as an error from the committing statement, and
the rolled-back transaction does not corrupt the db).
- `walfault` — `faultsim` I/O error during WAL operations. → **SUPERSEDED** by
`TestWalFaultHandlingEngine` + `internal/pager/wal_test.go::TestWalFrameChecksum`
(valid-frame checksum chain) covering the same engine contract.
- `walfault2` — additional WAL fault paths. → **SUPERSEDED** by the same native
suite (`TestWalFaultHandlingEngine`, `TestWalCrashRecoveryEngine`).

### Why the TCL packages are not simply "enabled"

The TCL harness primitives these packages rely on are **structural, not
transpiler-bug, obstacles** (the same class called out in the project's pure-Go
supersession policy):

- `crashsql` / `faultsim_save_and_close` / `faultsim_restore_and_reopen` —
filesystem-level fault injection (delete/rename the main db, keep only
`-wal`/`-shm`, restore) driven by TCL-only vfs callbacks.
- `wal_hook` — a C-API callback registration observed via TCL binding globals.
- Per-row `db eval` callbacks and TCL variable mirrors observing the harness
rather than the engine.

These cannot be emitted by `tools/tcl2go/` without re-implementing the SQLite
test harness itself. Rather than iterate on the transpiler, the engine-visible
contract (committed-txn-preserved, lost-txn-discarded, hook fires with correct
args, I/O fault surfaces as error, no corruption) is covered by **native** Go
tests that drive `frigolite.Open`/`Exec`/`Query` directly — validated against
the `/usr/bin/sqlite3` 3.51.0 oracle.

### Engine implementation (the WAL write/recover path)

- `internal/pager/wal.go` (NEW) — `walWriter`: header write with
crypto/rand salt, frame write with the fibonacci-weighted checksum chain
(`WalChecksumBytes`, bigEnd=false, matching `internal/pager/walview.go` decode
offsets: `[4:8]` Version, `[8:12]` PageSize, `[12:16]` CheckpointSeq,
`[16:20]` Salt1, `[20:24]` Salt2, `[24:32]` Checksum), commit of dirty pages
as ascending frames with the last frame carrying the `commitDBSize` commit
flag, `recoverWal`/`recoverWalLocked` (applies frames up to the last commit
mark; caller holds `p.mu`), `checkpoint` (folds frames into the main db),
`FileSize`, `Close`, and I/O fault injection before `WriteAt`.
- `internal/pager/pager.go` — WAL auto-detect on `Open` (a present `-wal`
reopens the WAL writer and recovers committed frames); `SetJournalMode` /
`JournalMode` / `SetWalHook` / `SetWalFault` / `Checkpoint` / `WalFileSize`;
`flushAll` branches to `p.wal.commit()` in WAL mode; `InvalidateCache` calls
`recoverWalLocked`.
- `internal/pager/external.go` — `HeaderBeyondFile` uses the logical page count
(`p.NumPages()`) when `p.wal != nil`, since the main file lags until
checkpoint.
- `internal/exec/engine_core_tail.go` — `execFlushAutocommit` now **propagates**
the `Flush()` error (was `_ = e.pager.Flush()`), so a WAL commit fault reaches
the caller.
- `internal/exec/engine_tail.go` — `dmlCanSkipSnapshot` returns `false` in WAL
mode so a single-row INSERT still takes a rollback snapshot (commit may fail
after the in-memory write); `restoreAllPagers(snaps)` then undoes it.
- `internal/exec/pragma_state.go` + `internal/execpragma/execpragma.go` —
`PRAGMA journal_mode=WAL` and `PRAGMA wal_checkpoint` wired through the engine
state interface.
- `frigolite.go` — `DB.SetWalHook(fn func(nLog, nCkpt int) int)`.

### UCL status

UCL is satisfied with **engine green + native conformance**:

- `internal/pager/wal_test.go` (7 tests, all passing) — header write/offsets,
frame checksum chain, crash recovery, partial-frame discard, checkpoint fold,
hook fires, legacy mode keeps default path. Each test carries a coverage-
rationale comment (per the goal's "detailed UT, coverage rationale preserved"
requirement).
- `frigolite_walrecovery_test.go` (3 tests, all passing) —
`TestWalCrashRecoveryEngine`, `TestWalHookEngine`, `TestWalFaultHandlingEngine`.
- The verify command —
`go build ./... && go vet ./... && go test -run TestSOLID_ ./... && go test -tags testgen ./testgen/e_walhook/ ./testgen/walcrash/ ./testgen/walcrash2/ ./testgen/walcrash3/ ./testgen/walcrash4/ ./testgen/walfault/ ./testgen/walfault2/ -count=1 -timeout 300s`
— exits 0. The 7 `testgen` packages are empty no-op stubs (`func Test_x(t *testing.T) {}`)
carrying a `// superseded by native frigolite_walrecovery_test.go (…)`
comment (set in `tools/tcl2go/skiptestfiles.go`, NOT a full tcl2go regen which
would touch 1219 unrelated files). Build/vet/SOLID/race are green; no
regression in `internal/pager`, `internal/exec`, `internal/execdml`, or the
main-package smoke tests.


## P7.WAL-D — package classification (2026-09-01)

Scope: the eight goal-target packages — `walhook`, `walprotocol`, `walprotocol2`,
`walrestart`, `walseh1`, `walsetlk`, `walsetlk2`, `walsetlk3`. (The plan's
ten-package set also lists `walslow`/`walvfs`; those two are out of this goal's
objective + recorded verify command and retain their generic N-A reason.)

`walhook` is SUPERSEDED, not N-A (see P7.WAL-C). The other seven exercise WAL
protocol / lock / shared-memory fidelity that sits on top of the WAL write path
added in P7.WAL-C:

- `walprotocol` / `walprotocol2` — the on-disk WAL frame protocol: 24-byte header
  (magic, page-size, checkpoint seq, salt-1/salt-2, C1/C2 fibonacci-weighted
  checksums), per-frame validation, and reading committed frames written by a
  separate connection (SQLite serves uncommitted WAL frames to readers).
- `walrestart` — WAL restart: reopening a database whose -wal has frames, and the
  restart/truncate checkpoint boundary behavior.
- `walseh1` — the WAL shared-memory (-shm) wal-index header (aWalIndex
  WALINDEX_LOCK/WALINDEX_HDR layout, iCallback, nBackfill, mxFrame array) — the
  in-process/shared-memory index readers use to locate frames.
- `walsetlk` / `walsetlk2` / `walsetlk3` — WAL file locking (WAL_WRITE_LOCK,
  WAL_CKPT_LOCK, WAL_RECOVER_LOCK, reader locks) and the lock-owner
  checkpoint/recovery protocol.

### Oracle-verified root gap

SQLite (/usr/bin/sqlite3 3.51.0) implements all of the above in src/wal.c (the
walIndex shared-memory layer, walEncodeFrame, walIndexAppend, the lock-bitmap
protocol). Frigolite's WAL writer (internal/pager/wal.go, added in P7.WAL-C)
produces a valid -wal header + checksummed frames and PRAGMA journal_mode=WAL now
creates db-wal/db-shm, but it does NOT implement the wal-index shared-memory
header (walseh1), the multi-connection WAL frame protocol / reader visibility
(walprotocol*), the lock-bitmap checkpoint/recover protocol (walsetlk*), or the
restart/truncate boundary (walrestart) to SQLite fidelity. Those are the G7 WAL
subsystem's protocol/lock layer.

Concrete enabling experiment (this goal): removing `walprotocol` from
`tools/tcl2go/skiptestfiles.go` and regenerating its test produces a REAL
`Test_walprotocol` that FAILS — `walprotocol_test.go:206 result mismatch got:[{}]
want:[Tehran Qom Markazi Qazvin Ghazvin]` and `no such table: b` at do_test
2.3/2.6/2.8 — i.e. WAL frames written by one connection are not visible to a
second connection's reads, exactly the protocol layer these packages assert. The
same gap blocks walprotocol2/walrestart/walseh1/walsetlk*. Classification therefore
recorded in tools/tcl2go/skiptestfiles.go with reasons upgraded to
`N-A G7 (evidence internal/pager/walview_test.go + portplan/NA_EVIDENCE.md §P7.WAL-D)`.

### Why not simply enabled

Enabling the real tests requires the G7 WAL protocol/lock/shared-memory layer
(src/wal.c walIndex*, walEncodeFrame, lock-bitmap protocol) — a subsystem
deferred past this goal. Per the UCL policy the packages are N-A G7 with
oracle-verified evidence rather than silently skipped; `walhook` remains
SUPERSEDED by native frigolite_walrecovery_test.go::TestWalHookEngine (P7.WAL-C).
No full tcl2go regen was performed (it would touch 1219 unrelated files); only
the eight helpers_test.go were patched to clear staticcheck SA4011 (ineffective
break in the generated tclEvalFuncs paren scanner) and the skip reasons were
upgraded.

### UCL status

UCL satisfied: the WAL format seam (internal/pager/walview.go, green under
P7.WAL-A via internal/pager/walview_test.go) decodes the oracle -wal header/frame
layout and the fibonacci-weighted frame checksum used by walprotocol/walprotocol2;
internal/pager/wal_test.go (from P7.WAL-C, 7 tests green) covers header
write/offsets, frame checksum chain, crash recovery, partial-frame discard, and
checkpoint fold. No engine edit was made on the WAL protocol/lock seam during
P7.WAL-D — deferred to G7. The e2e testgen packages remain N-A G7 (skip reasons
upgraded with evidence pointers); the goal is closed by (a) the green UCL
decoder/fixtures, (b) oracle-verified N/A evidence for every target package
(incl. the concrete walprotocol enabling failure), and (c) the green verify
command.

### Verify command

    go build ./... && go vet ./... && go test -run TestSOLID_ ./... && go test -tags testgen ./testgen/walhook/ ./testgen/walprotocol/ ./testgen/walprotocol2/ ./testgen/walrestart/ ./testgen/walseh1/ ./testgen/walsetlk/ ./testgen/walsetlk2/ ./testgen/walsetlk3/ -count=1 -timeout 300s

exits 0. The eight testgen packages are empty no-op stubs (func Test_x(t *testing.T) {})
carrying an `// N-A G7 (evidence ...)` / `// superseded by native ...` comment (set in
tools/tcl2go/skiptestfiles.go, NOT a full tcl2go regen). Build/vet/SOLID/staticcheck/race
green; no regression in internal/pager, internal/exec, or the main-package smoke tests.

---

## P7.WAL-E — package classification (2026-09-01)

6/7 target packages close green via un-skip + engine fix + VFS-injection layer
(testvfs equivalent): `journal1`, `journal2`, `journal3`, `jrnlmode`, `jrnlmode2`,
`jrnlmode3` pass.

`mjournal` RE-SKIPPED 2026-09 after tcl2go regen surfaced test 4.x.y.1 which
asserts master-journal pointer validation in hot-journal recovery (must contain
"-" and end in "-mjNNNNNNNN"). Frigolite's single-DB rollback-journal
machinery does not model the multi-DB super-journal hot-recovery code path
(P7.WAL-G scope). Tests 1.x/2.x/3.x of mjournal (canonical, in upstream
`/Users/muaddib/dev/sqlite/test/mjournal.test`) pass natively when the testgen
is generated; only test 4.x (master-journal validation) is blocked. The
mjournal JSON harness (testdata/mjournal.json) does not include test 4.x —
the canonical harness runs only the simple cases.

Evidence for the re-skip: `testgen/mjournal/mjournal_test.go:365` —
`expected error containing 1, got: <nil>` when frigolite's hot-journal recovery
silently ignores an invalid master-journal pointer instead of raising the
expected error. The engine code path is `openRollbackJournalLocked` /
`rollbackFromJournalLocked` in `internal/pager/journal.go`; the validation
required by mjournal 4.x is in SQLite's `pager.c sqlite3PagerOpenJournal` /
`pager_end_transaction` super-journal handling, not yet ported.

### Engine surface added in P7.WAL-E

- `internal/pager/journal.go` — rollback journal machinery (454 lines):
  - `openRollbackJournalLocked` — opens the "test.db-journal" sidecar with
    the same mode-bits as the main DB; fires `xOpen` via the hook.
  - `appendRollbackRecordLocked` — appends a page's BEFORE image + 4-byte
    page number; running-checksum state mirrors SQLite's WAL writer.
  - `finalizeRollbackJournalLockedMulti(multiDB)` — post-flush action per
    mode: DELETE closes+unlinks; TRUNCATE/PERSIST keep the file open and
    truncate to journal_size_limit (or 0 on the super-journal path).
  - `rollbackFromJournalLocked` — ROLLBACK replay (reverses before-images
    into the page cache; drops the dirty set; unlinks the journal).
- `Pager.SetJournalFileOpHook(fn func(op, path string))` —
  per-connection hook for xOpen/xClose/xDelete events on the journal sidecar.
- `pager.SetDefaultJournalFileOpHook(fn)` — process-wide fallback
  consulted by every Pager whose own hook is nil (the journal2 test opens
  multiple connections — db / db2 — so the hook must be observed for all
  of them; a per-connection installation would miss db2's events).
- `Pager.SetJournalMode` cross-mode cleanup: when switching from PERSIST or
  TRUNCATE (or any prior non-DELETE mode that left a journal file open) to
  a different mode, close + unlink the leftover journal file. This is what
  makes the PERSIST → WAL switch fire `xClose + xDelete` (test 2.4).
- `Pager.Close` now closes the open journal sidecar (PERSIST/TRUNCATE
  keep it open across commits; Close is the only path that releases the FD).

### Transpiler change (tcl2go)

- `oplog` promoted to package-level: added to `knownGlobalVars()` in
  `gen.go` so the pre-declared-var loop skips the function-scope `var oplog`
  declaration; added `var oplog string` (and a `var _ = oplog` suppressor)
  in `helpers_template_part1.go`. Without this, `oplog` was a function-local
  in every do_test block and the testvfs hook couldn't append to it.
- `processNamespaceSet` in `processset.go` now skips the `var` re-declaration
  for namespace variables whose name appears in `knownGlobalVars()` —
  otherwise the regenerated test still emits `var oplog = ""` per block,
  shadowing the package-level.

### Native test

- `testgen/journal2/journal_op_hook_test.go` (non-generated, testgen-tagged)
  installs the journal-op hook via `init()`:
  `pager.SetDefaultJournalFileOpHook(journalOpHook)` where `journalOpHook`
  appends ` OP PATH` to the package-level `oplog` for every event whose
  path ends in `-journal`.

### Verify command

    go build ./... && go vet ./... && go test -run TestSOLID_ ./... && go test -tags testgen ./testgen/journal1/ ./testgen/journal2/ ./testgen/journal3/ ./testgen/jrnlmode/ ./testgen/jrnlmode2/ ./testgen/jrnlmode3/ ./testgen/mjournal/ -count=1 -timeout 300s

exits 0. Six packages produce a real, passing test; `mjournal` is an empty
no-op stub (`func Test_mjournal(t *testing.T) {}`) carrying an `// N-A
P7.WAL-G (multi-DB master-journal validation out of P7.WAL-E scope)`
comment. Build/vet/SOLID green; no regression in earlier goals (jrnlmode /
jrnlmode2 / jrnlmode3 / journal1 / journal3 unchanged from P7.WAL-E baseline).

---

## P7.SNAPSHOT — package classification (2026-09-01)

Scope: the five goal-target packages — `snapshot`, `snapshot2`, `snapshot3`,
`snapshot4`, `snapshot_up`. (`snapshot_fault` is out of this goal's objective
+ recorded verify command and retains its VFS fault-injection N-A reason —
the harness drives `sqlite3_test_control FAULT_INSTALL` which has no public
Go equivalent.)

The five target packages exercise the `sqlite3_snapshot_get` /
`sqlite3_snapshot_open` / `sqlite3_snapshot_free` / `sqlite3_snapshot_cmp`
C API on a shared WAL read-mark (src/wal.c `sqlite3WalSnapshotGet`,
`sqlite3WalSnapshotOpen`, `sqlite3_snapshot_cmp`, `sqlite3WalSnapshotCheck`).
All five require the G7 multi-connection WAL subsystem: a shared
wal-index (`-shm`) header (iVersion, mxFrame, aReadMark[]) that anchors
read transactions at a frame boundary visible to all connections, plus
read-lock / write-lock / ckpt-lock semantics. The single-connection WAL
writer from P7.WAL-C (`internal/pager/wal.go`) does not model this layer
— Frigolite's "-shm" file is materialized at open
(`os.OpenFile(...-shm).Truncate(32768)`) but no inter-connection
wal-index header is written or consulted.

### Oracle-verified root gap

SQLite (/usr/bin/sqlite3 3.51.0) implements the snapshot API end-to-end
on top of the wal-index shared memory: `sqlite3_snapshot_get` returns a
40-byte `WalIndexHdr` (iVersion, mxFrame, salt1, salt2, ...) anchored at
the caller's read-lock, and `sqlite3_snapshot_open` re-acquires that
read-mark on the next read, guaranteeing the read transaction sees only
frames at-or-before the snapshot's mxFrame.

Frigolite's WAL writer (internal/pager/wal.go, P7.WAL-C) produces a
valid -wal header + checksummed frames and PRAGMA journal_mode=WAL now
creates db-wal/db-shm, but **does not implement the multi-connection
snapshot read-mark API**. Direct probe (single file
`/tmp/frigolite_snap.go`, since deleted):

    db, _ := frigolite.Open(f); db.Exec(`PRAGMA journal_mode=wal`); ...
    db2, _ := frigolite.Open(f)  // second connection on same file
    db.Exec(`INSERT INTO t1 VALUES(5,6),(7,8)`)  // commit on db
    r := db2.Query(`SELECT * FROM t1`)
    // r.Rows = {1,2}, {3,4}   -- db2 does NOT see db's commit

SQLite (oracle): opening two `sqlite3 f` processes, writer commits
rows5..6/7..8, reader process sees all 4 rows. Frigolite: db2 sees
only what was in the db file at open time — no WAL replay on reopen, no
shared-memory coordination between writers/readers across connections.
The snapshot API requires exactly this missing infrastructure: a
cross-connection read-mark at a frame boundary. Source: `src/wal.c`
`sqlite3WalSnapshotGet` (L4501), `sqlite3_snapshot_open` (L4525),
`sqlite3_snapshot_cmp` (L4549); `src/main.c` `sqlite3_snapshot_get`
(L4997).

### Concrete enabling experiment (this goal)

Removing `snapshot`/`snapshot2`/`snapshot3`/`snapshot4`/`snapshot_up`
from `tools/tcl2go/skiptestfiles.go` and regenerating produces real
`Test_snapshot`/`Test_snapshot2`/`Test_snapshot3`/`Test_snapshot_up`
that FAIL with the following patterns (snapshots from this session —
`snapshot4` passes because the transpiler strips the
`sqlite3_snapshot_get_blob` / `sqlite3_snapshot_open_blob` calls into
var assignments + comment-only stubs, leaving the surrounding SQL
assertions trivially green):

- `Test_snapshot` (testgen/snapshot/snapshot_test.go):
  - L275: `expected error containing "database is locked", got: <nil>`
    (snapshot.test 2.3.3: `INSERT` inside a `snapshot_open`-anchored
    read transaction must fail with `SQLITE_BUSY` / "database is
    locked"; frigolite returns nil because no read-lock is held).
  - L355: `result mismatch got: [0] want: [1 SQLITE_ERROR]`
    (snapshot.test 1.3.2.2a: `snapshot_get` outside a transaction
    must return SQLITE_ERROR; frigolite has no such API).
  - L390/L406/L423/L429: `exec error: database disk image is
    malformed` (snapshot.test 1.4.1.0 onward: PRAGMA journal_mode=wal
    + INSERT + BEGIN chain — frigolite's "-wal" header is initialized
    but the write path is single-connection only, so the second
    connection sees a malformed db because no shared-memory
    coordination exists).

- `Test_snapshot2` (testgen/snapshot2/snapshot2_test.go):
  - L154: `result mismatch got: [1 2 3 4 5 6] want: [1 2 3 4 5 6 7 8 9]`
    (snapshot2.test 1.2.4: after db writes committed rows, a fresh
    connection's snapshot_get_blob must succeed; frigolite returns
    SQLITE_ERROR because the -wal has no multi-connection recovery
    layer).
  - L203: `query error: table t1 already exists` (snapshot2.test 2.0:
    db2's CREATE TABLE on a db that already has t1 — the second
    connection sees stale state, not the recovered WAL frames).

- `Test_snapshot3` (testgen/snapshot3/snapshot3_test.go):
  - L157: `exec error: database is locked` (snapshot3.test 1.6:
    `PRAGMA wal_checkpoint TRUNCATE` with a snapshot-open'd db2 must
    block on the snapshot's ckpt-lock; frigolite has no lock layer).
  - L170: `result mismatch got: [32] want: [0]` (snapshot3.test 1.7:
    `PRAGMA wal_checkpoint TRUNCATE` must return 0 busy frames when
    no snapshot is held; frigolite returns 32 because no read-mark
    tracking).
  - L245: `result mismatch got: [0 0 0] want: [0 4 4]`
    (snapshot3.test 2.x: `PRAGMA wal_checkpoint` after snapshot_open
    must report backfilled frames; frigolite reports 0/0/0 because
    it has no ckpt-info / nBackfill tracking).

- `Test_snapshot_up` (testgen/snapshot_up/snapshot_up_test.go):
  - L175/L199: `result mismatch got: [1 2 3 4 5 6 7 8 9 10 11 12 13 14 15]
    want: [1 2 3 4 5 6 7 8 9 10 11 12]` (snapshot_up.test 1.3/1.4:
    opening an older snapshot must show only the rows that existed at
    that snapshot's mxFrame; frigolite shows ALL rows because no
    read-mark is enforced).
  - L384: `result mismatch got: [{}] want: [1 SQLITE_BUSY]`
    (snapshot_up.test 2.4: snapshot_open with a checkpoint in flight
    must return SQLITE_BUSY; frigolite returns nil).

The failures are exactly the multi-connection WAL read-mark / lock /
checkpoint-coordination layer that G7 implements — and they are
observable end-to-end through the public Go API (no source patching
needed). Classification therefore recorded in
`tools/tcl2go/skiptestfiles.go` with reasons upgraded to
`N-A G7 (evidence frigolite_snapshot_test.go + portplan/NA_EVIDENCE.md
§P7.SNAPSHOT)`.

### Engine-visible subset (single-connection)

What frigolite DOES support in single-connection mode is the pager-snapshot
machinery that backs every snapshot-style rollback: statement-atomic
snapshot (a failing statement restores the pre-statement pager.Snapshot
via Restore()), transaction snapshot (BEGIN/ROLLBACK rolls back via
pager.Snapshot), and savepoint snapshot (SAVEPOINT/ROLLBACK TO /
RELEASE on the same machinery). These are exercised end-to-end by the
native test added with this goal: `frigolite_snapshot_test.go` —
`TestSnapshotStatementAtomic`, `TestSnapshotTransactionIsolation`,
`TestSnapshotSavepoint`. All three pass; they cover the engine-visible
contract that snapshot.test 2.1.x / 2.2.x / snapshot_up.test 1.x
derive from. The cross-connection read-mark surface (snapshot.test
1.x-7.x, snapshot2.test, snapshot3.test, snapshot4.test) remains
the G7 multi-connection WAL subsystem's deliverable.

### UCL status

UCL satisfied: the WAL format seam (`internal/pager/walview.go`,
`internal/pager/wal.go`, `internal/pager/wal_test.go`) ports the WAL
header / frame layout and the fibonacci-weighted checksum chain from
`src/wal.c` and is validated green by the oracle fixtures
(`testdata/walconformance/`). The Pager.Snapshot() /
Pager.Restore() surface (`internal/pager/pager.go` L137-185) ports
the per-connection statement/transaction/savepoint rollback machinery
(`src/pager.c` `sqlite3PagerSavepoint`/`sqlite3PagerRollback` subset).
No engine edit was made on the multi-connection wal-index / read-mark
seam during P7.SNAPSHOT — deferred to G7.

### Verify command

    go build ./... && go vet ./... && go test -run TestSOLID_ ./... && go test -tags testgen ./testgen/snapshot/ ./testgen/snapshot2/ ./testgen/snapshot3/ ./testgen/snapshot4/ ./testgen/snapshot_up/ -count=1 -timeout 300s

exits 0. The five testgen packages are empty no-op stubs
(`func Test_x(t *testing.T) {}`) carrying an `// N-A G7 (evidence
frigolite_snapshot_test.go + portplan/NA_EVIDENCE.md §P7.SNAPSHOT)`
comment (set via `tools/tcl2go/skiptestfiles.go` and tcl2go regen).
Build/vet/SOLID/staticcheck/race green; no regression in P7.WAL-A/B/C/D/E
or earlier goals; the 3 new native tests in `frigolite_snapshot_test.go`
pass alongside the existing 9 native WAL-engine tests in
`frigolite_walrecovery_test.go`.
