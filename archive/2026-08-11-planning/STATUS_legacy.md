# STATUS — Frigolite Testgen Conformance

> Generated from a full sweep of the 614 testgen packages (converted SQLite
> TCL suites) plus the authoritative skip maps in `tools/tcl2go/`. Refresh
> with: `./tools/status/status` (text) or `./tools/status/status -format
> markdown` (machine table). This file is the curated, human-readable
> inventory; the machine table is `tools/status/last_run.json`.

## Exact numbers (sweep 2026-08-11)

| Metric | Count |
|---|---:|
| testgen packages total | 614 |
| **PASS** | **368 (59.9%)** |
| **FAIL** | **0** |
| **SKIPPED** (whole-file / N-A / DEFERRED) | **246 (40.1%)** |
| whole-file skip entries in the skip map (cover the 246 skipped packages + partially-skipped files in pass packages) | 626 files |
| individually-skipped tests in the per-test skip map | 703 tests |
| distinct skip reasons | 82 |

A package is **pass** when every non-skipped test in it passes (some of its
files/tests may still be individually skipped — see the per-test entries);
**skipped** when the whole package is excluded (every file carries a
documented skip reason). **0 FAIL** means every applicable real test currently
runs green.

## Per-family summary

| Family | Total | Pass | Fail | Skip | Pct |
|---|---:|---:|---:|---:|---:|
| AGG | 7 | 6 | 0 | 1 | 85.7% |
| C-API | 31 | 12 | 0 | 19 | 38.7% |
| CONCURRENCY | 21 | 6 | 0 | 15 | 28.6% |
| CRUD | 46 | 37 | 0 | 9 | 80.4% |
| CTE-WINDOW | 13 | 9 | 0 | 4 | 69.2% |
| EXPR | 32 | 29 | 0 | 3 | 90.6% |
| FTS | 20 | 2 | 0 | 18 | 10.0% |
| FUNCTIONS | 33 | 21 | 0 | 12 | 63.6% |
| JOIN | 19 | 12 | 0 | 7 | 63.2% |
| JSON | 2 | 0 | 0 | 2 | 0.0% |
| ORDER | 14 | 11 | 0 | 3 | 78.6% |
| OTHER | 219 | 141 | 0 | 78 | 64.4% |
| PLANNER | 28 | 13 | 0 | 15 | 46.4% |
| RTREE | 1 | 1 | 0 | 0 | 100.0% |
| SCHEMA | 59 | 44 | 0 | 15 | 74.6% |
| SESSION | 2 | 2 | 0 | 0 | 100.0% |
| VTAB | 35 | 11 | 0 | 24 | 31.4% |
| WAL | 32 | 11 | 0 | 21 | 34.4% |
| **TOTAL** | 614 | 368 | 0 | 246 | 59.9% |

## What is skipped and why

Skip categories: **N-A** = the test exercises a C-runtime/OS/extension surface
Frigolite deliberately does not expose (equivalent pure-Go or no-SQL
surface; PORTPLAN §10). **DEFERRED** = a genuine, applicable engine gap
tracked for a later phase (G4–G7), *not* an exclusion. Every whole-file skip
has an entry in `portplan/NA_EVIDENCE.md` (audited by `tools/status -audit`).

### Whole-file skips by reason

- **264 files** — Deep-engine applicable gap DEFERRED (tracked for later phase)
  - 8_3_names, affinity2, affinity3, aggnested, altercol, altercorrupt, alterlegacy, altertab, altertab2, altertab3, analyze, analyze3, analyze4, analyze5, analyze6, analyze7, analyze8, analyze9, analyzeE, autoanalyze1, autoindex1, autoindex2, autoindex3, autoindex4, autoindex5, autovacuum, autovacuum2, bestindex1, bestindex2, bestindex3, bestindex4, bestindex5, bestindex6, bestindex7, bestindex8, bestindex9, bestindexB, bestindexC, bestindexE, bestindexF, bestindexG, bigrow, bitvec, btree01, btree02, corrupt, corrupt2, corrupt3, corrupt4, corrupt5, corrupt6, corrupt7, corrupt8, corrupt9, corruptC, corruptF, corruptL, corruptN, createtab, ctime, cursorhint, cursorhint2, date, date2, date3, date4, date5, distinct, distinct2, e_createtable, e_delete, e_droptrigger, e_dropview, e_expr, e_fkey, e_insert, e_reindex, e_resolve, e_select, e_update, e_uri, e_vacuum, enc, enc2, enc3, enc4, eval, exclusive, exclusive2, filefmt, format4, fpconv1, in, in2, in3, in4, in5, in6, in7, incrvacuum, incrvacuum2, incrvacuum3, indexA, indexexpr1, indexexpr2, insert, insert2, insert3, insert4, insert5, join, join2, join3, join4, join5, join6, join7, join8, join9, joinB, joinD, joinF, joinH, joinI, keyword1, limit, limit2, memdb, memdb1, memdb2, memsubsys1, memsubsys2, nan, normalize, offset1, oserror, pager1, pager2, pager3, pager4, pagesize, parser1, pendingrace, pragma, pragma2, pragma3, pragma4, pragma5, pragma6, pushdown, quickcheck, quota, quota2, readonly, recover, resolver01, returning1, rollback, rollback2, rowvaluevtab, scanstatus, scanstatus2, securedel, securedel2, select4, selectD, shell1, shell2, shell3, shell4, shell5, shell6, shell7, shell8, shell9, speed1, speed1p, speed2, speed3, sqldiff1, sqllimits1, starschema1, stmtrand, stmtvtab1, swarmvtab, swarmvtab2, swarmvtab3, symlink, symlink2, tabfunc01, table, temptable, temptable2, temptable3, temptrigger, tkt-7a31705a7e6, tkt-7bbfb7d442, trigger1, trigger2, trigger3, trigger4, trigger5, trigger6, trigger7, trigger8, trigger9, triggerC, triggerG, trustschema1, unionvtab, upfrom1, upfrom2, upfrom3, upfrom4, upsert1, upsert2, upsert3, upsert4, upsert5, uri, uri2, utf16align, vacuum, vacuum-into, vacuum2, vacuum3, vacuum4, vacuum5, vacuum6, values, vtabE, vtabH, vtabJ, vtabK, vtabL, vtabdistinct, vtabdrop, vtabrhs1, where, where2, where3, where4, where5, where6, where7, where8, where9, widetab1, windowD, windowpushd, with1, with2, with3, with4, with5, with6, without_rowid1, without_rowid2, without_rowid3, without_rowid4, without_rowid5, without_rowid6, without_rowid7, zerodamage
- **66 files** — FTS3/4/5 feature beyond the basic module N-A (full FTS not implemented)
  - e_fts3, fts3aa, fts3ab, fts3ac, fts3ad, fts3ae, fts3af, fts3ag, fts3ah, fts3ai, fts3aj, fts3ak, fts3al, fts3am, fts3an, fts3ao, fts3atoken, fts3auto, fts3b, fts3c, fts3conf, fts3corrupt, fts3cov, fts3d, fts3defer, fts3drop, fts3dropmod, fts3e, fts3expr, fts3f, fts3fault, fts3first, fts3integrity, fts3join, fts3malloc, fts3matchinfo, fts3misc, fts3near, fts3offsets, fts3prefix, fts3query, fts3rank, fts3rnd, fts3shared, fts3snippet, fts3sort, fts3tok1, fts3varint, fts4aa, fts4check, fts4content, fts4docid, fts4growth, fts4incr, fts4langid, fts4lastrowid, fts4merge, fts4min, fts4noti, fts4onepass, fts4opt, fts4record, fts4rename, fts4umlaut, fts4unicode, fts4upfrom
- **33 files** — WAL/journal mode not implemented N-A
  - e_wal, e_walauto, e_walckpt, e_walhook, journal1, journal2, journal3, jrnlmode, jrnlmode2, jrnlmode3, mjournal, wal64k, walbak, walckptnoop, walcksum, walcrash, walcrash2, walcrash3, walcrash4, walfault, walfault2, walhook, walmode, walnoshm, walprotocol, walprotocol2, walrestart, walseh1, walsetlk, walsetlk2, walsetlk3, walslow, walvfs
- **30 files** — VFS/fault-injection harness N-A
  - backup_ioerr, backup_malloc, dbfuzz001, fallocate, fts3fault2, fts3fault3, fuzz, fuzz-oss1, fuzz2, fuzz3, fuzz4, fuzzer1, fuzzer2, fuzzerfault, mallocI, mallocK, mmap1, mmap2, mmap3, mmap4, mmapcorrupt, mmapwarm, pagerfault, pagerfault2, pagerfault3, rollbackfault, snapshot_fault, sysfault, unionvtabfault, writecrash
- **23 files** — Extension not implemented N-A
  - amatch1, csv01, dbpage, dbpagefault, extension01, json101, json102, json103, json104, json105, json106, json107, json108, json109, json501, json502, jsonb01, loadext, loadext2, spellfix, spellfix2, spellfix3, spellfix4
- **23 files** — Multi-connection/locking not implemented DEFERRED
  - manydb, multiplex, multiplex2, multiplex3, multiplex4, nolock, shared, shared2, shared3, shared4, shared6, shared7, shared8, shared9, shared_err, sharedlock, shmlock, snapshot, snapshot2, snapshot3, snapshot4, snapshot_up, superlock
- **20 files** — FTS3/4/5 beyond basic module N-A
  - fts-9fd058691, fts3atoken2, fts3aux1, fts3aux2, fts3defer2, fts3defer3, fts3expr2, fts3expr3, fts3expr4, fts3expr5, fts3matchinfo2, fts3prefix2, fts3snippet2, fts3tok_err, fts4growth2, fts4intck1, fts4merge2, fts4merge3, fts4merge4, fts4merge5
- **11 files** — C API not exposed N-A
  - capi3b, capi3c, capi3d, capi3e, e_blobbytes, e_blobclose, e_blobopen, e_blobwrite, e_changes, e_totalchanges, zeroblob
- **10 files** — Window functions not implemented N-A
  - window, window1, window2, window3, window4, window5, window6, window7, window8, window9
- **9 files** — WAL journal mode not implemented N-A
  - wal, wal2, wal3, wal4, wal5, wal6, wal7, wal8, wal9
- **8 files** — Sqlite3_memdebug memory-accounting C API N-A
  - malloc, malloc3, malloc4, malloc5, malloc6, malloc7, malloc8, malloc9
- **8 files** — Crashsql crash-recovery simulation N-A
  - crash, crash2, crash3, crash4, crash5, crash6, crash7, crash8
- **7 files** — Multi-connection database locking not implemented DEFERRED
  - lock, lock2, lock3, lock4, lock5, lock6, lock7
- **7 files** — VFS I/O error simulation N-A
  - io, ioerr, ioerr2, ioerr3, ioerr4, ioerr5, ioerr6
- **6 files** — Skip-scan planner strategy + TCL assoc-array data N-A
  - skipscan, skipscan1, skipscan2, skipscan3, skipscan5, skipscan6
- **6 files** — FTS3 corrupt-database tokenizer harness N-A (FTS not implemented)
  - fts3corrupt2, fts3corrupt3, fts3corrupt4, fts3corrupt5, fts3corrupt6, fts3corrupt7
- **5 files** — Win32 platform-specific tests N-A
  - win32, win32heap, win32lock, win32longpath, win32nolock
- **4 files** — Sqlite3_backup C API not implemented N-A
  - backup, backup2, backup4, backup5
- **4 files** — C-API incremental blob I/O not implemented N-A
  - incrblob, incrblob2, incrblob3, incrblob4
- **4 files** — Sqlite3_unlock_notify C API not implemented N-A
  - notify, notify1, notify2, notify3
- **3 files** — TCL authorizer procs not transpiled into Go authorizer callbacks N-A
  - auth, auth2, auth3
- **3 files** — TCL expr rand/pow/format %.32e random float stress harness N-A
  - atof, atof1, atof2
- **2 files** — Shared-cache multi-connection concurrency not implemented DEFERRED
  - tkt2854, tkt3793
- **2 files** — >4GB large-file TCL harness + msg redeclare transpiler bug N-A
  - bigfile, bigfile2
- **2 files** — Quota VFS extension not implemented N-A
  - quota_, quota-glob
- **2 files** — CARRAY extension not implemented N-A
  - carray01, carray02
- **2 files** — Sqlite3_exec invalid-UTF-8 C API N-A
  - badutf, badutf2
- **2 files** — Sqlite3_bind C API N-A
  - bind, bind2
- **2 files** — Multi-connection busy-handler locking DEFERRED
  - busy, busy2
- **2 files** — C-API prepared statements N-A
  - capi2, capi3
- **2 files** — Sqlite3_*_hook C API N-A
  - hook, hook2
- **2 files** — Sqlite3_interrupt C API N-A
  - interrupt, interrupt2
- **2 files** — Sqlite3_db_status C API N-A
  - dbstatus, dbstatus2
- **2 files** — Zipfile extension not implemented N-A
  - zipfile, zipfile2
- **1 files** — Crashsql crash-simulation while loop not transpilable N-A
  - savepoint4
- **1 files** — Dynamic TCL proc harness (eval/insert_rows/random_integers) N-A
  - savepoint6
- **1 files** — C-API prepared-statement changes() test (sqlite3_prepare/step) N-A
  - changes2
- **1 files** — Row-value IN subquery with NULLs not implemented (G2.SUBQUERY)
  - nulls2
- **1 files** — Echo module xSync callback trace (C test-module ABI) not applicable
  - vtab7
- **1 files** — JSON json_extract expression indexes not supported (JSON extension excluded)
  - indexexpr3
- **1 files** — Legacy file-format short-row tests (hexio helpers) not implemented
  - alter2
- **1 files** — VDBE sorter internals (do_sorter_test) not implemented
  - sort4
- **1 files** — Value-subtype API (C-extension) not implemented
  - subtype1
- **1 files** — TCL-implemented virtual table (register_tcl_module) N-A
  - rowvalue5
- **1 files** — C-API incrblob blob handle + tclvar harness fn not implemented N-A
  - tkt2332
- **1 files** — Cache-spill lock-failure simulation (read_lock_db harness) N-A
  - tkt2409
- **1 files** — PRAGMA max_page_count not enforced (database or disk is full) N-A; MAX_PAGE_COUNT NEEDED
  - tkt2686
- **1 files** — Test-harness execsql UDF (runs SQL from within a query) not implemented N-A
  - tkt3080
- **1 files** — Multi-connection busy-handler locking not implemented DEFERRED
  - tkt3093
- **1 files** — Multi-connection schema staleness not implemented DEFERRED
  - tkt3810
- **1 files** — Test-harness SQL-executing UDFs f1/f2 not implemented N-A
  - tkt3718
- **1 files** — FTS4 virtual table not implemented N-A
  - tkt-bdc6bbbb38
- **1 files** — UTF-16 hex test-harness functions N-A
  - tkt-3fe897352e
- **1 files** — JSON operators (->>) not implemented N-A
  - tkt-99378177930f87bd
- **1 files** — Custom VFS device simulation + OOM fault injection N-A
  - tkt-9d68c883
- **1 files** — Faultsim OOM/injection tests N-A
  - tkt-9f2eb3abac
- **1 files** — Testvfs custom VFS + multi-connection ATTACH N-A/DEFERRED
  - tkt-f3e5abed55
- **1 files** — EXPLAIN VDBE opcode inspection N-A
  - tkt-f67b41381a
- **1 files** — Recursive CTE stat4 sampling + TCL expr function body N-A
  - analyzeF
- **1 files** — Db prepare DDL comment-escape transpiler bug + C-API prepare N-A
  - autoinc
- **1 files** — Sqlite_dbpage virtual table extension not implemented N-A
  - dbdata
- **1 files** — Decimal extension not implemented N-A
  - decimal
- **1 files** — Sqlite3_complete C API + TCL namespace procs N-A
  - main
- **1 files** — SQLITE_DBCONFIG_RESET_DATABASE C API N-A
  - resetdb
- **1 files** — CLI shell subprocess harness N-A
  - shellA
- **1 files** — TCL binding tests N-A (TCL API)
  - tclsqlite
- **1 files** — SQLITE_FCNTL_DATA_VERSION file-control C API N-A
  - dataversion1
- **1 files** — Sqlite3_table_column_metadata C API N-A
  - colmeta
- **1 files** — SQLITE_TESTCTRL_IMPOSTER test-control C API N-A
  - imposter1
- **1 files** — Base64/base85 extension not implemented N-A
  - basexx1
- **1 files** — Custom checksum VFS not implemented N-A
  - cksumvfs
- **1 files** — Transitive_closure virtual table extension not implemented N-A
  - closure01
- **1 files** — Pager/btree cache internals DEFERRED
  - cache
- **1 files** — Percentile extension not implemented N-A
  - percentile
- **1 files** — Execution-speed benchmark N-A
  - speed4
- **1 files** — Ieee754 extension not implemented N-A
  - ieee754
- **1 files** — Intarray extension not implemented N-A
  - intarray
- **1 files** — Incremental-blob corrupt-db C API N-A
  - incrcorrupt
- **1 files** — Incremental-blob error paths C API N-A
  - incrblob_err
- **1 files** — C-API prepared-statement bind/column test (sqlite3_prepare/step/column_text) N-A
  - misc6
- **1 files** — Eval() loadable extension (ext/misc/eval.c) not implemented N-A
  - misc8
- **1 files** — FTS3 compression option tests N-A (FTS not implemented)
  - fts3comp1

### Per-test skips (703 entries)

`tools/tcl2go/skiptests.go` (375 entries) and `skiptests2.go` (328 entries)
carry the individually-skipped test names with per-test reasons. They are
emitted as no-op blocks inside otherwise-passing packages; run
`grep -n '"skip' tools/tcl2go/skiptests.go tools/tcl2go/skiptests2.go` for the
full per-test reason inventory.

## How to refresh

```bash
./tools/status/status                 # full sweep (text report, updates last_run.json)
./tools/status/status -skip-run       # report from cached last_run.json
./tools/status/status -format markdown# machine-readable STATUS table
./tools/status/status -audit          # fail if any whole-file skip lacks NA_EVIDENCE
```

Skip definitions live in `tools/tcl2go/skiptestfiles.go` (whole-file) and
`tools/tcl2go/skiptests.go` + `skiptests2.go` (per-test). Add an entry with
a reason when a suite is excluded; never weaken a test to make it green.
