# PAGER1 SWEEP TRIAGE (2026-11-12, /tmp/wt_triage2, /tmp/pager1_triage_run.log)

The pager1 census family (101 isolated on 2026-11-04) re-run ISOLATED on
feature/perf @ 965d510db (branch s28-pager1-triage):

```
FRIGOLITE_TEST=pager1 go test -run "^TestSQLiteSuite$" -count=1 -timeout 30m .
```

Run COMPLETED (FAIL, 1.076s, no panic; exit 1 = failing leaves present).
Substring-filter check: `pager1` matched ONLY `TestSQLiteSuite/pager1/`
paths (testdata has pager1.json; pager2/3/4, pagerfault* do not contain the
substring). Family truth = **99 unique failing subtests** (each FAIL line
appears exactly once; the census's 101 is -2 drifted since 2026-11-04 — no
regression, two leaves fixed by the intervening S13-S21 work).

All 99 leaves bucket into **7 root-cause classes**. Attribution summary:
**engine 78, converter/fixture 8, harness-C-API 6, mixed (harness+engine)
7** (the big B5 cluster is one interlocked engine+harness arc).

C source: `/Users/muaddib/dev/sqlite/test/pager1.test` (checkout 3.51.0;
test names 37.x-44.x are defined bare, `do_test 37.$tn` — the converter
prefixes `pager1-`). Oracle used: `/usr/bin/sqlite3` **3.54.0**
(2026-04-09) — tagged below; the journal-header/super-journal formats cited
are stable file-format contracts.

## The per-class table

| # | class | n | representative got/want | root cause | attribution | C source |
|---|---|---|---|---|---|---|
| B1 | 7.1 subst-foreach garbage fixture | 3 | `sql: "exclusive"` / `sql: "-1"` expect `8` — the res/js foreach columns became SQL steps | the `foreach {tn sql res js ws} [subst {...}]` loop (tn 6..10 rows) mis-converted: column bindings shift, res becomes sql, the `.2` file-size probes leak as `sql: "-1"` steps | **converter** | pager1.test:1164-1185 (the `[subst]` foreach) |
| B2 | 37.x URI collapse + 43.x db_status fold | 5 | 37.1: `reopen_db` path `"."` (the `file:?mode=memory&cache=shared` URI collapsed); 43.1: `query SELECT * FROM t1` with expect `0 2 0` (the sqlite3_db_status return folded into the query step's expect) | converter artifacts: URI filenames not parsed; C-API result calls folded into SQL steps with the C-API result as the expectation | **converter** | pager1.test:2707-2735 (37.x shared-cache URIs), 2900-2930 (43.x sqlite3_db_status CACHE_MISS) |
| B3 | __STEP_ returns status, not column value | 2 | 39.1/39.2: `want [one] got SQLITE_ROW` — the runtime_assert `__STEP_stmt_1__` expanded to the step status instead of `sqlite3_column_text(stmt,0)` | harness C-API expansion captures the step code, not the column value | **harness (C-API surface)** | pager1.test:2751-2773 (prepare/step/column_text across a schema change) |
| B4 | hot-journal absent: no on-disk journal to corrupt/play back | 13 | 4.3.2-10 + setup_11/12: `hexio_write test.db-journal offset N: no such file or directory`; 4.5.3: got committed rows `I II 1 2 III IV 3 4` want rolled-back `I II III IV` (restore didn't change anything); 4.5.5: partial playback missing | engine never materializes the rollback journal on disk during the setup transaction, so the harness's hexio_write has no file and faultsim_save snapshots nothing to roll back; hot-journal header validation + record-checksum playback unimplemented | **engine (+faultsim hooks, see B5)** | pager1.test:492-529 (4.3.x offsets), 747-822 (4.5.x) |
| B5 | super-journal + faultsim/testvfs machinery | 61 | `.1b`: `runtime variable mismatch got [""] want [0]/[1]` (::mj_delete_cnt never set); `.4`: `ATTACH 'test.db2' AS aux` → "database aux is already in use" (stale connection kept after faultsim_restore_and_reopen); `.5/.6`: row residue `won too free double-you why zed` vs `won too free` (restore didn't roll back the multi-file txn); 4.4.2.*.8: `no such table: a` (padded-prefix + `cd` directory hierarchy emulation); 4.6.2: `got [1 t1.2] want [1 t1.1]` | engine creates no super-journal for multi-file commits (so the testvfs xDelete filter never fires); harness has no faultsim_save / faultsim_restore_and_reopen emulation (directory snapshot + close/reopen ALL named connections); tn1=2's filename padding derives from ::mj_filename_length (0 here → 31 x's seen in the log) | **engine + harness (interlocked)** | pager1.test:530-745 (4.4.x), 824-926 (4.6.x); vdbeaux.c:2950-3140 |
| B6 | readonly/permission surfaces | 4 | 4.5.6: "expected error but got success" (want `attempt to write a readonly database` under `-readonly 1`); 4.7.2: success (want `unable to open database file` for a read-only journal); 4.8.1/4.8.3: got the full table dump (want the `sqlite3_db_readonly` return 1/0) | file-attributes emulation (chmod) missing in the harness; `-readonly` open flag not plumbed; `sqlite3_db_readonly` C-API surface missing | **harness + engine (readonly error paths)** | pager1.test:793-822 (4.5.6), 956-990 (4.7.x/4.8.x) |
| B7 | misc engine contracts | 11 | 14.1.4: duplicate-rowid INSERT..SELECT succeeds (want `UNIQUE constraint failed: t1.rowid` under journal_mode=OFF); 14.1.6: got `1 2 2 2 2 2` want `1 2 2 2` (statement journal unavailable → pager error state); 11.2: COMMIT succeeds (want `disk I/O error` via testvfs lockout); 11.5: `got [6] want [32]` (count growth/rollback-recovery cascade); 21.1/21.2: WAL db opens without shm-capable VFS (want `unable to open database file`); 38.1#01: reopen detected "file is not a database" but CREATE TABLE then succeeded on a RESET db (NOTADB error state not sticky); 42.1: pending-byte page readable (want `database disk image is malformed`); 44.1: `got [30] want [31]` (auto_vacuum=FULL page_count misses the ptrmap page); 44.3: `got [30 16] want [31 31]` (max_page_count must adjust UPWARD on ROLLBACK); 8.2.2: named db2 on `""` (temp) never opened | six independent small engine gaps + two harness surfaces (testvfs IOERR injection; named-connection temp open) | **engine (6 units) + harness (2)** | pager1.test:1778-1803 (14.1.x), 1592-1611 (11.x), 2100-2124 (21.x), 2737-2749 (38.1), 2852-2898 (42.1 pending byte), 2752-2956 (44.x forum fix) |

## The pinned C contracts (oracle + source)

1. **Journal header** (pager.c readJournalHdr ~1577-1660; oracle hexdump of a
   hot journal from a SIGKILLed python-sqlite3 txn on h.db, page size 4096):
   `0-7` magic `d9d505f9 20a163d7` (pager.c:757 aJournalMagic); `8-11` nRec;
   `12-15` cksumInit; `16-19` original db size; `20-23` **sector-size**
   (invalid if <32 / >MAX_SECTOR_SIZE / not power of 2); `24-27` **page-size**
   (invalid if <512 / >SQLITE_MAX_PAGE_SIZE / not power of 2; **0 = pre-3.5.8
   compat** → journal still hot — pager1-31.1). JOURNAL_HDR_SZ =
   sectorSize (pager.c:771); records start at the first sector boundary:
   pgno(4) + page image + cksum(4), JOURNAL_PG_SZ = pageSize+8. The fixture's
   offsets match exactly: 4.3.2-6 write the sector-size field, 4.3.7-9 the
   page-size field (either invalid → journal NOT hot → committed `{1 2 3 4}`
   survives), 4.3.10 offset 32 = record-1 page-data → record cksum fails →
   playback stops → pre-txn `{1 2}`. 4.5.4/4.5.5 (setup_11/12, offsets 1338 /
   2370 = 512+4+1024-202 and +4+4+1024 more) corrupt record-1 / record-2 page
   data → full rollback vs partial playback.
2. **Super-journal decision** (vdbeaux.c:2950 aMJNeeded + the nTrans gate at
   ~2960-2986): a super-journal is created iff more than one db has an active
   write txn AND `safety_level != PAGER_SYNCHRONOUS_OFF` AND
   `aMJNeeded[journalmode]` (DELETE=1, PERSIST=1, OFF=0, TRUNCATE=1,
   MEMORY=0, WAL=0) AND main is not :memory:/temp. This reproduces the
   fixture's `usesMJ` column exactly (o/o512/w1/w2/o1a/o1b/m1 = 0;
   n/f/t1/p1 = 1). Name: `zMainFile + "-mj%06X9%02X"` (antepenultimate '9');
   each journal gets the pointer appended at sync; the super-journal is
   deleted last (pager_delsuper, pager.c ~2971-3000) — deletion ORDER is what
   4.6.x exercises.
3. **max_page_count upward-adjust on ROLLBACK** (pager1.test:2752-2956, the
   2022-03-01 forum fix): after 44.2's `PRAGMA max_page_count=2` +
   incremental_vacuum (page_count 16), `ROLLBACK` must restore BOTH
   page_count=31 AND max_page_count=31. Ours returns [30 16] — the rollback
   restored neither (and 30 vs 31 = the ptrmap page).
4. Oracle (3.54) run `PRAGMA page_count` on the 44.1 build: 31 (auto_vacuum=
   FULL, 50 rows zeroblob(1000), page_size 4096 — 1 ptrmap page included).

## Stale-fixture signature check

The task's heuristic (got [""] runtime-var mismatches + cascading "file is
not a database" reopens ⇒ regenerate first) does NOT trigger here:

- The 22 got-[""] `.1b` leaves have CORRECT fixtures (`runtime_var
  mj_delete_cnt` steps with the right expect) — the machinery
  (super-journal creation + testvfs xDelete hook) is genuinely absent; do
  not regenerate.
- The single "file is not a database" reopen cascade (38.1#01) matches its
  source exactly (pager1.test:2737-2745) — the fixture is fine; the engine's
  NOTADB error state is not sticky across the harness reopen.

The REAL fixture defects are different signatures — garbage SQL steps (B1:
`sql: "-1"`), collapsed paths (B2: `reopen_db "."`), and folded C-API
expects (B2: expect `0 2 0` on a query step). For those, regenerate first:

```
go run ./tools/tclconvert -testdir /Users/muaddib/dev/sqlite/test \
  -outdir /tmp/x28 -only pager1
diff /tmp/x28/pager1.json testdata/pager1.json   # expect diffs in 7.1.6-10, 37.1, 43.1-3
```

B1/B2 will NOT heal by regeneration alone — the converter's foreach+subst
column binding, URI parsing, and C-API step emission must be fixed first;
otherwise regeneration reproduces the same artifacts.

## Unit plans

**U1 — converter: subst-foreach column binding (B1, 3 leaves, smallest)**
- `tools/tclconvert/` — the `do_execsql_test pager1-7.1.$tn.1` loop inside
  `foreach {tn sql res js ws} [subst {...}]` (pager1.test:1164-1185).
- Approach: bind the loop variables from the LIST literal rows positionally
  (tn, sql, res, js, ws); today rows tn>=6 shift (res→sql, js→expect) and
  the `.2` file-size probes leak as `sql: "-1"` exec steps. Reproduce with a
  minimal foreach-subst case in the converter's test corpus, fix the
  binding, regenerate, diff.
- Verify: the 5 regenerated cases `pager1-7.1.{6,7,8,9,10}.1/.2` exist with
  real SQL; the 3 garbage names disappear.

**U2 — harness: __STEP_ column-text capture (B3, 2 leaves)**
- `frigolite_harness_test.go` — the `__STEP_<var>__` runtime_assert
  expansion (S18's C-API result-code machinery, ~:4466) must ALSO capture
  `sqlite3_column_text(stmt,0)` after each successful step so a
  `runtime_assert __STEP_stmt_1__` with a scalar expect compares the VALUE.
- Approach: keep the status carriers (`__STEP_` → status, `__FIN_` → code)
  and add `__COL_<var>_<idx>__` (or make a scalar `expect` on a stmt_step
  select the current row's column 0). Pick the minimal shape that matches
  the fixture (the existing expect "one"/"two" — no fixture change needed).
- Verify: pager1-39.1/39.2 green; tkt1644/tkt2820 (S18) unchanged.

**U3 — readonly/permission surfaces (B6, 4 leaves)**
- Harness: emulate `file attributes ... -permissions`/`-readonly` as a real
  chmod on the target path (a `file_attr` step type or a hook on the
  existing step); plumb a `-readonly` open flag into the reopen/open steps.
- Engine: `internal/pager` — readonly-open error paths: (a) readonly
  connection + hot journal present → `attempt to write a readonly database`
  (pager.c pagerOpenSavepoint/hasHotJournal write path); (b) journal file
  open fails EACCES → SQLITE_CANTOPEN `unable to open database file`
  (pager.c:956-961's 4.7.2); (c) `sqlite3_db_readonly` C-API surface
  (harness capi step returning 1 when the db file lacks write permission).
- C source: pager1.test:793-822, 956-990; pager.c hasHotJournal path.
- Verify: 4.5.6/4.7.2/4.8.1/4.8.3 green; chmod steps cleaned up after each
  case (the fixture restores perms itself).

**U4 — converter: URI parsing + capi_db_status emission (B2, 5 leaves)**
- Converter: (a) `file:?mode=memory&cache=shared` style URIs → reopen/open
  steps with the URI intact + `mode=memory&cache=shared` semantics
  (shared-cache named memory db keyed by the `cache=`/path value; empty path
  = unnamed shared memory db); sqlite3_config_uri 1 gate can be a no-op
  flag on the step. (b) emit a `capi_db_status` step
  (connection, CACHE_MISS) instead of folding the return into the next
  query step.
- Harness: shared-cache in-memory open for URIs; `sqlite3_db_status`
  CACHE_MISS counter surface (per-connection page-cache miss count — count
  page-cache entries materialized from disk per sqlite3_db_status.c).
- Engine: db_status CACHE_MISS counter in `internal/pager` (cache misses are
  observable today? if not, add the counter on the cache-miss path).
- C source: pager1.test:2707-2735 (37.x), 2900-2930 (43.x);
  sqlite3_db_status.c (CACHE_MISS contract: {code, hits-or-misses, unused}).
- Verify: 37.1 (db eval → {1}) + 37.1.2 (VACUUM keeps {1}); 43.1/43.2/43.3
  = {0 2 0} / {0 3 0} / {0 1 0}.

**U5 — misc engine contracts (B7, 11 leaves; six independent units, land in
any order) — COMPLETE (S31.1..S31.6, branch s31-pager-engine)**
- U5a page_count/max_page_count (44.1/44.3): auto_vacuum=FULL page_count
  must count ptrmap pages (pager.c:1070 — the super-journal page number is
  never a ptrmap page; ptrmap pages ARE counted in dbSize); on ROLLBACK
  restore dbSize AND mxPgno from the journal header's original-db-size field
  (the 16-19 header field — pager.c pagerRollback → restore mxPgno).
- U5b NOTADB sticky error state (38.1#01): open of a zero-page/malformed
  file must leave the connection in the pager error state so the next
  statement fails SQLITE_NOTADB (`file is not a database`) — the harness
  reopen currently resets to an empty db and lets CREATE succeed. Check
  pager.c pagerOpen/SQLITE_NOTADB handling; the engine must surface the
  error on first USE (not only at open).
- U5c journal_mode=OFF statement failure (14.1.4/14.1.6): the explicit-rowid
  INSERT..SELECT (`INSERT INTO t1(rowid,a,b) SELECT a+3,b,b`) must detect
  the rowid UNIQUE conflict on the second insert; with journal_mode=OFF the
  statement journal cannot be created → the pager enters the error state
  and the transaction is discarded at COMMIT (14.1.6 = pre-txn rows {1 2 2 2}
  — derive exactly which rows survive with the oracle before coding).
- U5d pending byte (42.1): sqlite3_test_control_pending_byte surface +
  pager enforcement — reads/writes crossing the pending-byte page fail
  SQLITE_CORRUPT (`database disk image is malformed`). Harness step to set
  the pending byte; engine check in the page-read path.
- U5e WAL-shm gate (21.1/21.2): opening a WAL-mode db through a VFS without
  xShm (iVersion<2 / -noshm) fails SQLITE_CANTOPEN. In frigolite the VFS is
  fixed — implement as the WAL-open contract check (journal_mode=WAL db +
  shm unavailable → CANTOPEN), harness testvfs-flag steps.
- U5f temp named connections + IOERR injection (8.2.2, 11.2/11.5): harness
  named-connection open of `""` (per-connection temp schema → db2 sees
  `no such table: x1`); testvfs script/filter IOERR injection steps (tv
  filter {xWrite xTruncate xSync} → SQLITE_IOERR) so COMMIT returns
  `disk I/O error`; then diagnose 11.5's count (6 vs 32 — the doubling
  INSERT..SELECT self-referential growth; check whether our engine's
  `INSERT INTO zz SELECT a_string((SELECT 222+max(rowid) FROM zz)) FROM zz`
  sees a stale max(rowid) snapshot — pure-Go probe first).
- C source pointers: pager1.test:2752-2956 (44.x), 2737-2749 (38.1),
  1778-1803 (14.1.x), 2852-2898 (42.1), 2100-2124 (21.x), 1296-1317 (8.x),
  1576-1611 (11.x).
- Landed status (S31, 2026-11-16): 44.1/44.3 (S31.1), 38.1#01 (S31.2),
  14.1.4/14.1.6 (S31.3), 42.1 (S31.4), 21.1/21.2 (S31.5), 8.2.2 +
  11.1/11.2/11.5 (S31.6 — the failed-COMMIT abort path rollbackOpenTx +
  BEGIN-snapshot lifetime, bare-rowid INNER-owning aggregates, testvfs
  lockout pins). pager1 90 → 79.

**U6 — hot-journal substrate (B4, 13 leaves) — COMPLETE (S31.7, branch
s31-pager-engine)**
- Engine `internal/pager`: (a) materialize the rollback journal on disk
  during write txns (DELETE mode: create at first page write, header at
  offset 0, records sector-aligned, header fields per contract #1); (b)
  hot-journal detection on open: magic + header sanity (sector-size@20,
  page-size@24 power-of-2/512..65536 checks; 0 page-size = pre-3.5.8
  compat) + record checksums (cksumInit seed, per-record) → full playback,
  stop-at-first-bad-record playback, or ignore-journal; (c) journal
  deletion at commit (so a snapshot taken mid-commit captures the file).
- Harness: the faultsim hooks this needs arrive with U7; U6 must land with
  at least the 4.3.1/4.5.1 setup snapshot working (testvfs xSync/xDelete
  filter → faultsim_save mid-commit).
- Approach mirrors pager.c pagerOpenSavepoint/syncJournal/readJournalHdr;
  no simplification — record checksums are the rollback contract
  (4.3.10/4.5.5 are checksum-observability tests).
- C source: pager.c readJournalHdr 1577-1660, pagerRollback ~2748-2900,
  pager_playback; pager1.test:492-529, 747-822, 2542-2578 (31.1 compat).
- Oracle: journal-header hexdump reproduced (see contract #1); journal
  playback semantics already asserted by the fixture.

**U7 — super-journal + faultsim/testvfs machinery (B5, 61 leaves; the big
lever, last because it needs U6's journal substrate) — COMPLETE (S31.8)**
- Engine `internal/pager` + `internal/exec` (the multi-db commit path):
  (a) nTrans gate per contract #2 (aMJNeeded table, sync!=OFF, memdb
  excluded, >1 write txn); (b) super-journal open/write (`-mj%06X9%02X`
  suffix, '9' antepenultimate, each write-txn db's journalname appended,
  NUL-terminated, synced); (c) each journal gets the super-journal pointer
  appended at sync (readSuperJournal contract: 4-byte len + name + cksum
  checksum over the name bytes); (d) commit = delete super-journal LAST
  (pager_delsuper), rollback = per-journal playback + delete; (e) the
  multi-file atomicity: restore-to-snapshot must roll back BOTH journals
  (4.4.x.5/.6).
- Harness: (a) testvfs filter/script steps (`tv filter xDelete`,
  `tv script <proc>`, `tv sectorsize N`) — the filter intercepts named VFS
  methods and runs the TCL proc per call (the fixture's
  copy_on_mj_delete / xSyncCallback / lockout procs); (b)
  `faultsim_save` (directory snapshot incl. -journal/-mj files) and
  `faultsim_restore_and_reopen` (restore + close/reopen ALL named
  connections, preserving their filenames and -vfs flags) — the restore
  must also reset attached-db state (the `.4` ATTACH "already in use"
  cascade); (c) runtime_var capture of ::mj_delete_cnt /
  ::mj_filename_length / ::mj_filename (set by the emulated procs);
  (d) the tn1=2 padded-prefix + `cd` directory-hierarchy handling
  (nPadding = 511 - ::mj_filename_length; mkdir d1234567890.../ tree).
- Order inside U7: harness faultsim_save/restore first (it unblocks the
  restore-cascade leaves even before the engine's super-journal exists —
  the restore-cascade is ~27 of the 61), then the super-journal engine
  work (the 22 mj_delete_cnt leaves), then the padded-prefix flavor (11).
- C source: vdbeaux.c:2950-3140 (decision + creation + delsuper),
  pager.c readSuperJournal 1274-1340 + pager_delsuper, pager1.test:530-745
  (4.4.x incl. the tn1=2 padding block 560-591), 824-926 (4.6.x).

## Final status (S31, 2026-11-16)

The pager1 family is 100% GREEN (957 leaves, 0 failures). U5 (11 leaves,
S31.1..S31.6), U6 (15 leaves incl. the 4.5.6/4.8.x riders, S31.7), U7 (62
leaves incl. 4.7.2, S31.8). Census 90 → 0 across the S31 arc. The tn1=2
padded-prefix leaves (4.4.2.*) run on the converter-baked 31-x prefix — the
C directory-hierarchy machinery (file mkdir / cd, TESTVFS_P_C1_PLAN) is
carded, not needed for the observable contracts.

## Recommended slice order (smallest-first)

| order | unit | leaves | scope |
|---|---|---|---|
| 1 | U1 converter subst-foreach | 3 | tools/tclconvert only |
| 2 | U2 __STEP_ column-text | 2 | harness only |
| 3 | U3 readonly/permission surfaces | 4 | harness + small engine error paths |
| 4 | U4 URI + capi_db_status | 5 | converter + harness + small engine |
| 5 | U5 misc engine contracts | 11 | six small independent engine units (U5a/U5b best value per LOC) |
| 6 | U6 hot-journal substrate | 13 | internal/pager journal-on-disk + playback |
| 7 | U7 super-journal + faultsim | 61 | engine multi-db commit + harness testvfs/faultsim |

Cumulative: U1+U2+U3+U4 = 14 leaves for converter/harness-only work;
U1-U5 = 25; U6-U7 = 74 (the pager arc — U6 must precede U7; the faultsim
restore half of U7 unblocks ~27 leaves before the super-journal engine
work lands).

## Verification commands

```
# the family sweep (this triage's baseline):
FRIGOLITE_TEST=pager1 go test -run "^TestSQLiteSuite$" -count=1 -timeout 30m .
# fixture regen check (after U1/U4):
go run ./tools/tclconvert -testdir /Users/muaddib/dev/sqlite/test -outdir /tmp/x28 -only pager1
```

Run log preserved at /tmp/pager1_triage_run.log; leaf list at
/tmp/pager1_leaves.txt (99 entries).
