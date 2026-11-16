# TRIAGE — the WAL-arc residue (2026-11-16, S35, read-only)

Truth source: the verified full-suite gate `/tmp/full_s33s34_merge_gate.log`
(1317 leaves; python raw byte scan `re.findall(rb'--- FAIL: TestSQLiteSuite/([^\s(]+)')`,
leaf-only). Re-derived in the worktree at `ed154d87f` (branch `s35-wal-triage`):
`FRIGOLITE_TEST=wal` (82 leaves) + `journal2` + `subjournal` + `interrupt2`
(7 leaves) = the same 89-leaf name set, family by family. Extracted got/want
detail per leaf is in `/tmp/s35_details.json` / `/tmp/s35_details_full.json`
(family → leaf lists in `/tmp/s35_fails.json`).

Method notes kept from the lessons file: python raw byte scan only (faultsim
NUL corruption), FRIGOLITE_TEST is Contains-based (never `wal` for one
family), family-context verification only (leaf filters skip setup), name-set
diffs never counts.

New engine probes (throwaway worktree files, deleted before commit):
- **P1 (walfault2 shape)** — native `db.Query` of the 1.0 SQL returns `[wal]`
  and journal_mode stays `wal`: the engine is CORRECT; the harness `[delete]`
  comes from `setupDB` opening `:memory:` (frigolite_test.go:57) because the
  family has no lifecycle trigger, and `journal_mode=wal` on a memory db
  refuses.
- **P2 (wal9 shape)** — two connections on one file, WAL-only schema:
  db2's `SELECT * FROM t` succeeds natively. The fixture failure is the
  harness's `:memory:` main + a file-backed db2.
- **P3/P5 (wal-12.1 shape)** — under WAL mode the engine reproduces C's
  contract exactly: main=1024, wal=32+1048*5=5272. wal-12.1 is converter-only.
- **P4 (walvfs-1.2 shape, PSO-capable VFS)** — our checkpoint after 2 INSERTs
  is `[0 2 2]`; the C oracle (3.51.0 testfixture, same script) ALSO returns
  `0 2 2` without the tvfs. The `[0 4 4]` want requires `tvfs devchar
  sequential` (PSO dropped) → padToSectorBoundary fires under
  synchronous=normal (the commit sync bits are non-zero), same mechanism as
  S10's walvfs-5.3 but at NORMAL.

---

## 1. Per-family tables (89 leaves)

Verdicts: **E** = ENGINE GAP (fix in internal/...) · **C** = CONVERTER GAP
(tools/tclconvert) · **H** = HARNESS GAP (frigolite_harness_test.go) ·
**CARD** = carded class, cite and pick up · **NA** = unexpressible/card for
consistency.

### e_wal (13) — WAL without shared memory (the `oldvfs -iversion 1` doc test)
Source: ../sqlite/test/e_wal.test (the R-58297/R-00449 evidence blocks).
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.1.1 | `[wal]` / `[delete]` | journal_mode=WAL refused on a shm-less VFS | E+C |
| 1.2.1, 1.3.1, 3.4.1 | success / `1 {unable to open database file}` | read/write/SELECT on a WAL db without shm → CANTOPEN | E |
| 2.1.1 | `[exclusive 1 2 3 4 3 4]` / `[exclusive 1 2 3 4]` | cascade of 1.3.1's wrongly-succeeded INSERT + EXCLUSIVE-first-access heap wal-index works | E |
| 2.2.1 | `[normal ...]` / `[exclusive ...]` | locking_mode stuck EXCLUSIVE while WAL+no-shm (NORMAL is a no-op) | E |
| 2.2.2, 2.3.2, 3.2.2, 3.2.5 | success / `1 {database is locked}` | db2 contention under the stuck-EXCLUSIVE mode | E |
| 2.3.1, 2.3.3 | row cascade | cascade of 1.x | E (fixes itself) |
C pins: e_wal.test:19-79/86-130/138-200; vdbe.c OP_JournalMode's
`!sqlite3PagerWalSupported(pPager) → eNew=eOld`; pager.c:7596 (exclusiveMode →
bNoShm at sqlite3WalOpen); wal.c sqlite3WalOpen's bNoShm heap wal-index path.
Substrate: `Pager.SetShmUnavailable` (internal/pager/pagerconfig.go:235),
harness `noShm` fixture flag (frigolite_harness_test.go:4821, 6884-6887),
S30's walExclusiveLatched/heapMemory. The fixture drops the registration:
setup_0 is only `close_db` — the converter has no `-iversion 1` arg handling
in commandTestvfs.

### e_walhook (10) — the sqlite3_wal_hook doc test
Source: ../sqlite/test/e_walhook.test.
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.1.1, 1.1.2 | `[""]` / `[0]` | the hook registration + counter never emitted (`db wal_hook` unconverted) | C+H |
| 2.1 | `[""]` / `[1]` | hook proc opens db2 + reads inside the callback (nested connection in a callback interp) | H |
| 4.1 | success / `1 {SQL logic error}` | hook rc=SQLITE_ERROR propagates to the statement | E |
| 4.2 | success / `1 {database is locked}` | hook rc=SQLITE_BUSY | E |
| 4.3 | success / `1 {unable to open database file}` | hook rc=SQLITE_CANTOPEN | E |
| 5.1, 5.2 | `[""]` / `[1]` | registration replaces the previous hook; the replaced counter stops advancing | E+C |
| 6.1.1, 6.1.2 | `[""]` / `[1]` | `PRAGMA wal_autocheckpoint` invokes sqlite3_wal_hook and OVERWRITES the custom hook | E+C |
C pins: e_walhook.test:29-52 (rollback-mode suppression), 54-66 (post-commit
invocation), 119-131 (rc propagation — the hook rc flows out of
pagerWalFrames/pager_commit and the statement reports it), 133-155
(replacement), 157-170 (the pragma overwrites). `TestWalHookEngine` exists
(walhook.json skip note) — some engine substrate is present.

### wal2 (25)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.2.2–1.12.2 (11) | `[""]` / exact xShmLock trace lists | the carded `-shm` byte class (`incr_tvfs_hdr` forces recovery) + the lock trace never recorded | CARD |
| 2.2.3–2.9.3 (9) | partial `{4 1 lock shared} {4 1 unlock shared}` / full writer dance | same card (writer lock trace missing) | CARD |
| 5.1 | `[{1 1 lock exclusive} {1 1 unlock exclusive}]` / 16-element checkpoint dance | the checkpoint lock-order trace (wal_common.tcl:95/123 byte ops + wal2.test:349-415) | CARD |
| 10.1.4 | success / `1 {unable to open database file}` | `wal_set_walhdr` (new WAL format version 3007001) → recovery refuses (CANTOPEN) | CARD (wal_set_walhdr) |
| 10.2.3 | success / same error | `set_tvfs_hdr` + `wal_fix_walindex_cksum` (new wal-index version) → refuse | CARD |
| 4.2 | success / `1 {unable to open database file}` | `testvfs tvfs -noshm 1` flag dropped by the converter (harness `noShm` support EXISTS at :4821) | C |
| 6.6.2 | `exec error: locking protocol` / success | exit-exclusive xShmLock veto must be SWALLOWED (stay exclusive, no statement error); ours surfaces SQLITE_PROTOCOL | E |
| 6.6.3 | error mismatch | follow-on of 6.6.2 (db2 must be locked) | E |
C pins: wal2.test:337-346 (-noshm), 349-415 (5.1 lock dance), 726-780 (6.6.x
`lock_control` veto), 924-975 (10.x version refusal — wal.c walIndexRecover's
version check). The card citation: TODO.md queue item 1 + HANDOFF §Queued 3.

### walvfs (7) — the error-injection surfaces (carded bucket, now with specifics)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.2 | `[0 2 2]` / `[0 4 4]` | padToSectorBoundary under `devchar sequential` at synchronous=normal (P4: the engine only pads at FULL) | E |
| 3.1 | success / `1 interrupted` | xWrite callback calls `sqlite3_interrupt db` mid-checkpoint (the S8 copyback probe exists; the callback command surface missing) | H+E |
| 3.2 | success / `1 {out of memory}` | `sqlite3_memdebug_fail 1` inside the xWrite callback → OOM at the next malloc | H+E |
| 3.2.2 | `[""]` / `[1 {out of memory}]` | the callback-scope `catchsql {SELECT hex(randomblob(4000))}` capture | H |
| 4.2 | success / `1 {attempt to write a readonly database}` | tvfs `xShmMap` script returning SQLITE_READONLY must fail the SELECT | H+E |
| 6.2 | success / `1 {locking protocol}` | the WAL-restart protocol: the read-mark unlock → exclusive re-acquire fails BUSY → retry → SQLITE_PROTOCOL (C: wal.c walRestartLog + the 100× busy loop) | E |
| 9.1 | `attempt to write a readonly database` / `disk I/O error` | error precedence: xShmLock `{3 1 lock shared}` → SQLITE_IOERR must surface as "disk I/O error", not the CANTINIT readonly errstr | E |
C pins: walvfs.test:30-86 (1.x), 115-170 (3.x), 193-225 (4.x), 320-334 (6.2),
425-440 (9.1); wal.c:2260/2314/4099/4172-4190 (sync + padding), test_vfs.c
method dispatch. 6.2's opposite number is wal2-6.6.2 (same lock dance, opposite
swallow rule) — fix together.

### journal2 (5) — the on-disk rollback-journal substrate
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.1 | `[""]` / `[xOpen test.db-journal xClose test.db-journal xDelete test.db-journal]` | the engine writes NO journal files → no xOpen/xClose/xDelete events | E+C (testvfs -default 1 dropped) |
| 1.2 | `[""]` / `[xOpen test.db-journal]` | journal_mode=truncate keeps the journal open (no close/delete per txn) | E |
| 1.5 | success / `1 {disk I/O error}` | xDelete veto (open_journals) → IOERR surfaces | E+H |
| 1.13 | success / `1 {disk I/O error}` | COMMIT with xWrite/xTruncate IOERR → hot journal left → "disk I/O error" | E+H |
| 1.17 | success / integrity ok | hot-journal recovery against a REAL journal written by db2's failed commit | E |
C pins: journal2.test:28-90 (tvfs setup + oplog), 108-199 (the fault
choreography); pager.c journal write path (pagerJournalWrite/...
sqlite3PagerCommitPhaseOne), test_vfs.c xDelete veto. S31.7/S31.8 landed the
RECOVERY side (RecoverHotJournalIfNeeded) and the super-journal writer; the
single-db rollback-journal writer is the U6 card (lessons §S30 U5/U6).

### wal3 (5)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 3.3 | flags `[test.db-wal full ×3 test.db full]` / `[...normal ×4]` | walSyncFlags derivation: C renders SQLITE_SYNC_NORMAL unless PAGER_FULLFSYNC (pager.c:3638-3647 + the 605-616 comment: commit = `syncFlags<<2`, checkpoint = `fullSync ? syncFlags : 0`); ours renders the synchronous LEVEL | E (small) |
| 6.2.3 | `[""]` / `[h h l b]` | the xShmLock callback drives db2's BEGIN+SELECT mid-lock (re-entrant callback interp at the lock point) | CARD |
| 6.2.5 | `exec error: cannot commit` / `[1]` | cascade of the same callback choreography (db2's txn state) | CARD |
| 7.1.4, 7.2.2 | `[""]` / exact lock traces | the reader lock-trace with INSERTs fired inside the callback | CARD |
C pins: wal3.test:195-258 (3.x sync classes), 440-560 (6.2.x/7.x). Card
citation: HANDOFF §Queued 3 ("wal2 (61) + wal3 (5) xShmLock trace residue").

### walckptnoop (7) — `PRAGMA wal_checkpoint = noop` (2025 feature)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.0 | `query error: unexpected character '%'` / `{0 wal}` | `db func myrandomblob myrandomblob` — the harness registers the UDF without the fixture-interp proc round trip (S25 machinery); the body's `[expr % 256]` leaks as SQL | H |
| 1.1–1.5 (5) | `[0 0 0]` / `[0 298 0]` etc. | cascade of 1.0 (t1 empty) + the NOOP checkpoint mode (report {0 nLog nBackfill} without running) | E (after 1.0) |
| 1.7 | success / `1 {database table is locked}` | checkpoint inside a write txn refuses (the S7c veto on the PRAGMA path) | E (small) |
C pins: walckptnoop.test:34-90; the C NOOP mode lands in wal.c/pager.c 2025
(sqlite3WalCheckpoint's noop branch — verify against the 3.51 tree:
`PRAGMA wal_checkpoint = noop` parses in pragma.c's checkpoint pragma and
skips the run). Note 1.6/1.8/1.9 pass today only because the WAL is empty.

### wal (2)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 12.1 | expect baked `'1 {}'` → "expected error but got success" | the do_test body `list [expr [file size test.db]/1024] [file size test.db-wal]` — the converter stamped a broken expect; P5 proves the engine numbers are exactly C's | C (hand-pin, S30 style: file_size steps + the `wal_file_size` proc the harness already has) |
| 23.4 | `[""]` / `[SQLITE_NOTICE_RECOVER_WAL {recovered 2 frames from WAL file }]` | the sqlite3_log surface: `test_sqlite3_log [list lappend ::log]` unconverted; the engine emits no recovery NOTICE; the fixture is a husk (the whole setup dropped) | C+H+E |
C pins: wal.test:779-796 (12.1), 1405-1445 (23.x). sqlite3_log precedent:
the autoindex1 card (autoindex1's sqlite3_log class).

### wal4 (1)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.3 | success / `1 {no such table: t1}` | C's comment (wal4.test:33): "the file-system contains the wal and wal-index files only (no database file)" — an EMPTY main + a WAL → C DELETES the WAL (pager.c:3337 pagerOpenWalIfPresent nPage==0 → sqlite3OsDelete) → empty db. Ours replays the WAL. OPEN QUESTION for the unit: when does C write the main-file page-1 header (wal-12.1 wants 1024 with an explicit page_size pragma; wal4 has none) — pin with the oracle before editing | E |

### wal6 (2)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 4.4.2 | success / `1 {database disk image is malformed}` | a ZEROED WAL header (`hexio_write test.db-wal 0 00×2000`, wal6.test:222-236) must void the log: recovery treats a zero/garbage header as empty (wal.c walIndexRecover's version/magic check) → t2's un-checkpointed pages are gone → malformed. Ours keeps serving WAL frames | E |
| 5.2 | `[1 2 3 4]` / `[1 {database is locked} SQLITE_BUSY_SNAPSHOT]` | the interleaved row-callback (per-row `db eval` body runs `BEGIN EXCLUSIVE` + `sqlite3_extended_errcode db` mid-scan) + the BUSY_SNAPSHOT extended code on a stale-snapshot BEGIN EXCLUSIVE | CARD (the row-callback class) + E (BUSY_SNAPSHOT) |

### wal9 (2)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.1, 1.7 | `exec error: no such table: t` / `{}`-ok | P2: the ENGINE is correct; the harness runs the main handle on `:memory:` (no lifecycle trigger) while db2 opens fixtureDir/test.db | H |

### walfault (2)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 11 | `faultsim walfault-11: the trial loop hit the 512-trial cap` | the carded trial cap (bc722d011; the per-reopen close-time checkpoints multiply the fault schedule; ~12 min) | CARD |
| 12 | `unhandled prep step register_testvfs (default)` | the faultsim trial runner lacks the register_testvfs prep handler; the -prep also needs `shmfault shm test.db <40 zero bytes>` (the -shm byte-write surface) | H (+CARD B2 for the shm bytes) |
C pins: walfault.test:383-456.

### walfault2 (2)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.0, 2.0 | `[delete]` / `[wal]` | P1: the ENGINE is correct natively. The converter dropped BOTH `do_faultsim_test` trials (the fixture has only the two setup queries + `__RESET_DB__`), and the family then runs on `:memory:` where WAL is refused | C (+H: faultsim families need the file-backed lifecycle when faultsim_save steps are present) |
C pins: walfault2.test:23-73. The case-1 prep has `sqlite3 db file:test.db?psow=0 -uri 1` + `file_control_powersafe_overwrite db 0` — likely the drop trigger; case-2's prep is plain.

### walmode (1)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 4.9 | success / `1 {database is locked}` | WAL→delete with a second connection open: C's sqlite3PagerCloseWal needs the EXCLUSIVE db lock (pager.c:7663 "SQLITE_BUSY is returned and the log connection is not closed") via vdbe.c OP_JournalMode's CloseWal call | E (small) |

### walsetlk_recover (2) — the cross-process choreography
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.2 | `[""]` / `[database is locked]` | `testfixture_nb` (a SECOND PROCESS holds xRead 4s) + `db timeout 500` → busy. Unexpressible in-process without a simulated holder | NA (consistent with the walsetlk_snapshot skip: "testfixture_nb subprocess ... untranspilable") |
| 1.5.2 | `[0]` / `[1]` | `sleep_count>0` — the xSleep events during the busy wait; follows 1.2 | NA |
C pins: walsetlk_recover.test:31-75 (testfixture_nb, after 4000 in xRead).
RECOMMENDATION: skip-list with the same NA_EVIDENCE pointer as
walsetlk_snapshot, unless the coordinator wants a "background holder" harness
step (a goroutine-held connection + an arming delay — estimated ~150-250 LOC
harness, medium risk of flake).

### subjournal (1)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 2.1 | `[229]` / `[SQLITE_OK]` | the do_test body `set nPage [db one {PRAGMA page_count}]; B step [expr $nPage-10]` — the converter dropped the backup-object step and stamped SQLITE_OK on the query_assign. The backup_db step machinery exists (S7g) — needs the do_test-body emission with a runtime `expr` arg | C |

### walcrash2 (1)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 1.3 | `exec error: no such table: t1` / `[0]` | same harness class as wal9 (P2): the `launch_testfixture` loop (1023 crash trials) is dropped, and db2 opens the file while main is `:memory:` | H |

### interrupt2 (1)
| leaf | got / want | class | verdict |
|---|---|---|---|
| 2.1 | full row render `[1 1 2 2 ... 49 49]` / `[1 {SQLITE_INTERRUPT - interrupted} 1 interrupted]` | the carded interleaved row-callback step (S8 record; the body was dropped; the query degenerates to a full-row render) | CARD |

---

## 2. The bucket map (16 buckets, 89 leaves)

| # | bucket | leaves | verdict mix | C anchors | score (leaves/effort) |
|---|---|---|---|---|---|
| B1 | WAL-no-shared-memory (e_wal + wal2-4.2) | 14 | E+C | e_wal.test 19-200; wal2.test:337-346; vdbe.c OP_JournalMode; pager.c:7596; wal.c bNoShm | MEDIUM: ~250-400 LOC. Substrate exists (SetShmUnavailable, noShm fixtures, heapMemory) |
| B2 | the carded xShmLock-trace / -shm byte class (wal2 1.x/2.x/5.1/10.x/6.6.x) | 24 | CARD+E | wal_common.tcl:95-146; wal2.test:349-415, 726-780, 924-975; test_vfs.c:1637 | HIGH effort, already carded — keep the standing queue slot |
| B3 | faultsim-trial converter + file lifecycle (walfault2, walfault-12) | 3 | C+H | walfault2.test:23-73; walfault.test:438-456 | MEDIUM-SMALL: ~150-250 LOC |
| B4 | file-backed lifecycle for named-db families (wal9, walcrash2) | 3 | H | tester.tcl reopen semantics; P2 | TINY: ~30 LOC (one `usesReopen` trigger) |
| B5 | do_test-body value-source (wal-12.1, subjournal-2.1) | 2 | C | wal.test:779; subjournal.test:43-49; S30 hand-pin pattern | TINY: ~60 LOC (hand-pins) + backup emission ~60 |
| B6 | error-injection + lock-protocol surfaces (walvfs 7) | 7 | E+H | walvfs.test 30-440; wal.c:2260/2314/4172-4190; walRestartLog | MEDIUM: ~250-400 LOC total; 1.2 is a ~20 LOC gate fix |
| B7 | the on-disk rollback-journal write path (journal2) | 5 | E+C | journal2.test; pager.c journal write/commit | LARGE: ~600-1000 LOC — its own arc (U6 card) |
| B8 | walckptnoop (UDF round trip + NOOP mode + in-txn veto) | 7 | H+E | walckptnoop.test:34-90; S25 UDF machinery | SMALL-MEDIUM: ~150-250 LOC |
| B9 | walSyncFlags derivation (wal3-3.3) | 1 | E | pager.c:605-616, 3638-3647; wal.c:2260/2314 | TINY: ~30 LOC |
| B10 | the wal-hook surface (e_walhook) | 10 | E+C+H | e_walhook.test; tclsqlite.c wal-hook dispatch; pager.c hook rc flow | MEDIUM: ~250-400 LOC |
| B11 | the interleaved row-callback step (interrupt2-2.1, wal6-5.2) | 2 | CARD+E | S8 record; wal6.test:246-259 | MEDIUM: ~200-300 LOC (carded) |
| B12 | WAL-recovery rules (wal6-4.4.2 zeroed header, wal-23.4 sqlite3_log) | 2 | E+C | wal.c walIndexRecover; main.c sqlite3_log | SMALL-MEDIUM: ~150-250 LOC |
| B13 | the WAL→rollback mode-change veto (walmode-4.9) | 1 | E | pager.c:7663; vdbe.c OP_JournalMode | TINY: ~40-60 LOC |
| B14 | the cross-process choreography (walsetlk_recover) | 2 | NA | walsetlk_recover.test:31-75 | 0 LOC if NA-carded; ~150-250 if a holder step is wanted |
| B15 | the empty-main + WAL rule (wal4-1.3) | 1 | E | pager.c:3337-3351 (nPage==0 → delete the WAL) | SMALL: ~40-80 LOC + oracle pinning for the header-bootstrap question |
| — | walfault-11 (the faultsim trial cap) | 1 | CARD | bc722d011 record | carded, standing |

Dependencies: B6's 6.2 and B2's wal2-6.6.2/6.6.3 share the restart/exit-exclusive
lock choreography (fix together). B8's 1.x cascade needs 1.0 first (same unit).
B1's e_wal 2.x/3.x cascades off 1.1.1/1.2.1/1.3.1 (same unit). B3's walfault2
trials need the tvfs-shm surface only for walfault-12, not for walfault2.
B15's header-bootstrap question should be settled by the oracle BEFORE editing
(it touches the S5d/S5b behavior wal-12.1 depends on — P5 is green, do not
regress it).

Fixture freshness: every slice starts with
`go run ./tools/tclconvert -testdir ../sqlite/test -outdir /tmp/x -only <fam>`
+ diff against committed. Known converter drops this triage proved
(e_wal setup, e_walhook registrations, walfault2 trials, wal2 -noshm flag,
journal2 testvfs -default, wal-23.4 setup, subjournal backup step) are NOT
fixed by regen — the converter must learn them first (or hand-pin, S30/S32
style, when a wholesale regen would churn drifted cases).

---

## 3. Recommended slice order (smallest-first; 7 implementation units + 3 doc moves)

### S35a — the lifecycle-for-named-db harness rule (B4) — 3 leaves, ~30 LOC, zero engine risk
frigolite_harness_test.go: an `open_named_db` step whose target resolves to
the lifecycle filename (test.db) sets `usesReopen` (the pre-scan loop at
~3477). Verify no churn on the other open_named_db families (a two-direction
name-set diff across a sample: journal2, wal3, e_walckpt, wal2, attach*).
```
FRIGOLITE_TEST=wal9 go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
FRIGOLITE_TEST=walcrash2 go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
```

### S35b — the small engine + converter pins (B9, B13, B5) — 4 leaves, ~150 LOC
1. walSyncFlags derivation (internal/pager — checkpoint bits = fullSync?
   syncFlags : 0, commit = syncFlags<<2; the tvfs xSync callback must see
   NORMAL unless fullfsync). wal3-3.2 must STAY green.
2. WAL→delete BUSY when other connections hold the db (exec's
   journal-mode path + pager CloseWal equivalent). walmode-4.10/4.11 must
   stay green (the same path).
3. Hand-pin wal-12.1 (file_size steps + wal_file_size expr; expect
   `{1 5272}`-shape — compute via the harness's wal_file_size) and
   subjournal-2.1 (emit backup_db with the runtime page-count arg; expect
   SQLITE_OK on the step, keep query_assign for nPage).
```
FRIGOLITE_TEST=wal3 go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
FRIGOLITE_TEST=walmode go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
FRIGOLITE_TEST=wal go test ... ; FRIGOLITE_TEST=subjournal go test ...
```

### S35c — the WAL-recovery/frame rules (B6-1.2, B12, B15) — 4 leaves, ~150-250 LOC
1. walvfs-1.2: extend padToSectorBoundary's gate to `WAL_SYNC_FLAGS(commit
   bits) != 0 && !PSO` (not synchronous==FULL). walvfs-5.3 and 1.1/1.3 must
   stay green (P4 is the pin).
2. wal6-4.4.2: WAL-header validation in recovery — a zeroed/invalid header
   (magic/version/salts) voids the log like C's walIndexRecover.
3. wal4-1.3: the empty-main + WAL delete rule — FIRST settle the
   header-bootstrap question with the oracle (does C write page 1 to the main
   file without an explicit page_size pragma?), then align
   pagerOpenWalIfPresent without regressing wal-12.1/wal-14 (P5 pin).
```
FRIGOLITE_TEST=walvfs go test ...; FRIGOLITE_TEST=wal6 go test ...; FRIGOLITE_TEST=wal4 go test ...
DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture /tmp/<probe>.tcl   # the wal4 pin
```

### S35d — walckptnoop (B8) — 7 leaves, ~150-250 LOC
1. Harness: register_function with a fixture-proc UDF → the S25
   push/pull harnessTclFunction round trip (myrandomblob; kills the '%'
   error). walckptnoop-1.0 then cascades 1.1-1.5.
2. Engine: the NOOP checkpoint mode (report {0 nLog nBackfill} without
   running) on the `PRAGMA wal_checkpoint = noop` path, and the
   in-write-txn refusal "database table is locked" (1.7; the S7c veto on the
   PRAGMA path — the API path already has it per S7c).
```
FRIGOLITE_TEST=walckptnoop go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
```

### S35e — the no-shm WAL class (B1) — 14 leaves, ~250-400 LOC
Converter: `testvfs X -iversion 1` / `-noshm 1` → register_testvfs with
sql="iversion"/"noshm" (the harness already consumes both). Engine: (a)
journal_mode=WAL refused on a shm-unavailable pager (stays delete; and the
memory-db answer must match C's "memory" — probe), (b) WAL access without shm
→ CANTOPEN "unable to open database file", (c) EXCLUSIVE-first-access → heap
wal-index works (extend S30's walExclusiveLatched), (d) the stuck-EXCLUSIVE
no-op rule until WAL exits, (e) db2 contention → "database is locked".
C-evidence per leaf in §1 (e_wal table).
```
FRIGOLITE_TEST=e_wal go test ... ; FRIGOLITE_TEST=wal2 go test ...
go test -run 'TestSOLID_' ./...   # layer hygiene
```

### S35f — the wal-hook surface (B10) — 10 leaves, ~250-400 LOC
Converter: `db wal_hook <proc>` → a register_wal_hook step; the counter
globals via the callback runtime. Engine: the commit-end hook invocation with
rc propagation (the 4.x errstrs), replacement (5.x), the
wal_autocheckpoint-pragma overwrite (6.x). Reuse TestWalHookEngine's
substrate; wire the callback interp (the harnessTclCallbackRuntime machinery,
S19/S25 patterns).
```
FRIGOLITE_TEST=e_walhook go test -run '^TestSQLiteSuite$' -count=1 -timeout 20m -v
```

### S35g — error-injection surfaces + faultsim converter (B6 rest, B3) — 10 leaves, ~300-450 LOC
1. Harness: the tvfs callback command set gains sqlite3_interrupt +
   sqlite3_memdebug_fail (3.1, 3.2, 3.2.2) and the xShmMap script surface
   (4.2); engine consumes the map/lock errors with C's precedence (9.1).
2. Engine: the restart/exit-exclusive lock choreography (6.2 + the
   wal2-6.6.2/6.6.3 swallow rule — ONE fix, two directions).
3. Converter: do_faultsim_test emission for walfault2 (incl. the URI open +
   file_control_powersafe_overwrite prep) + the faultsim runner's
   register_testvfs prep handler (walfault-12; the shm byte-write lands with
   B2 or is stubbed to the 40-zero-byte form).
```
FRIGOLITE_TEST=walvfs go test ...; FRIGOLITE_TEST=wal2 go test ...; FRIGOLITE_TEST=walfault go test ... ; FRIGOLITE_TEST=walfault2 go test ...
```

### Doc moves (no code)
- **NA-card walsetlk_recover (B14)** in unsupportedTestFiles with the same
  NA_EVIDENCE pointer as walsetlk_snapshot (-2 leaves).
- **Keep carded**: B2 (24), B11 (2), walfault-11 (1), and B7 (journal
  substrate, 5) as its own future arc — B7 unblocks the wider
  journal-fidelity families (pagerfault etc.) beyond this residue.

### Expected residue after S35a-S35g
89 − (3+4+4+7+14+10+10) − 2 (NA) = **35 leaves**, all in the carded buckets
(B2 24, B11 2, B7 5, walfault-11 1, walfault-12's shm-byte remainder 3) —
matching the standing TODO.md queue item 1.

---

## 4. What changes in the standing TODO.md queue

1. Queue item 1 (WAL residue) should be re-anchored: the NEW families
   (e_wal, e_walhook, walckptnoop, wal9, walsetlk_recover, subjournal,
   walmode-4.9, wal4, wal6, walcrash2, journal2, walfault-12, walfault2)
   decompose into the buckets above; S35a-S35g are the concrete slice queue
   in order, BEFORE the carded B2 work (which stays last: highest effort,
   lowest per-leaf value).
2. Two engine behaviors the old docs mis-attributed: wal9/walcrash2 are
   HARNESS lifecycle bugs (the engine is green — P2), walfault2 is a
   CONVERTER drop + the `:memory:` lifecycle (the engine is green — P1).
   Do not open engine units for them.
3. walvfs-1.2 joins the S10 padToSectorBoundary family (the gate is
   synchronous-level based; it must be sync-bits based) — fold into S35c,
   not a new mechanism.
4. The wal4-1.3 empty-main rule interacts with S5d's "main must become
   non-empty" work — settle with the oracle first; wal-12.1/wal-14 and P5's
   green contract are the regression guards.
5. The `:memory:` default for faultsim families (walfault2) suggests a
   general harness rule: any family whose source calls faultsim_save should
   get the file-backed lifecycle — check for other affected families when
   landing S35g.
