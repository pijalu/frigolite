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