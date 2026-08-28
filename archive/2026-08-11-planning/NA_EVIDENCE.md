# N/A Evidence — documented exclusions for whole-file test skips

> Companion to `STATUS.md` and `PORTPLAN.md` §10. Each whole-file skip
> reason in `tools/tcl2go/skiptestfiles.go` is listed verbatim with the
> files it covers and the justification. `tools/status -audit` fails unless
> every skip reason appears here. **N-A** = the test exercises a surface
> Frigolite deliberately does not expose (C runtime / OS / optional
> extension / test-only C ABI); **DEFERRED** = a genuine applicable engine
> gap tracked for a later phase (G4–G7), *not* an exclusion.

## Reasons

### `deep-engine applicable gap DEFERRED (tracked for later phase)`

- **Category**: DEFERRED
- **Files (264)**: 8_3_names, affinity2, affinity3, aggnested, altercol, altercorrupt, alterlegacy, altertab, altertab2, altertab3, analyze, analyze3, analyze4, analyze5, analyze6, analyze7, analyze8, analyze9, analyzeE, autoanalyze1, autoindex1, autoindex2, autoindex3, autoindex4, autoindex5, autovacuum, autovacuum2, bestindex1, bestindex2, bestindex3, bestindex4, bestindex5, bestindex6, bestindex7, bestindex8, bestindex9, bestindexB, bestindexC, bestindexE, bestindexF, bestindexG, bigrow, bitvec, btree01, btree02, corrupt, corrupt2, corrupt3, corrupt4, corrupt5, corrupt6, corrupt7, corrupt8, corrupt9, corruptC, corruptF, corruptL, corruptN, createtab, ctime, cursorhint, cursorhint2, date, date2, date3, date4, date5, distinct, distinct2, e_createtable, e_delete, e_droptrigger, e_dropview, e_expr, e_fkey, e_insert, e_reindex, e_resolve, e_select, e_update, e_uri, e_vacuum, enc, enc2, enc3, enc4, eval, exclusive, exclusive2, filefmt, format4, fpconv1, in, in2, in3, in4, in5, in6, in7, incrvacuum, incrvacuum2, incrvacuum3, indexA, indexexpr1, indexexpr2, insert, insert2, insert3, insert4, insert5, join, join2, join3, join4, join5, join6, join7, join8, join9, joinB, joinD, joinF, joinH, joinI, keyword1, limit, limit2, memdb, memdb1, memdb2, memsubsys1, memsubsys2, nan, normalize, offset1, oserror, pager1, pager2, pager3, pager4, pagesize, parser1, pendingrace, pragma, pragma2, pragma3, pragma4, pragma5, pragma6, pushdown, quickcheck, quota, quota2, readonly, recover, resolver01, returning1, rollback, rollback2, rowvaluevtab, scanstatus, scanstatus2, securedel, securedel2, select4, selectD, shell1, shell2, shell3, shell4, shell5, shell6, shell7, shell8, shell9, speed1, speed1p, speed2, speed3, sqldiff1, sqllimits1, starschema1, stmtrand, stmtvtab1, swarmvtab, swarmvtab2, swarmvtab3, symlink, symlink2, tabfunc01, table, temptable, temptable2, temptable3, temptrigger, tkt-7a31705a7e6, tkt-7bbfb7d442, trigger1, trigger2, trigger3, trigger4, trigger5, trigger6, trigger7, trigger8, trigger9, triggerC, triggerG, trustschema1, unionvtab, upfrom1, upfrom2, upfrom3, upfrom4, upsert1, upsert2, upsert3, upsert4, upsert5, uri, uri2, utf16align, vacuum, vacuum-into, vacuum2, vacuum3, vacuum4, vacuum5, vacuum6, values, vtabE, vtabH, vtabJ, vtabK, vtabL, vtabdistinct, vtabdrop, vtabrhs1, where, where2, where3, where4, where5, where6, where7, where8, where9, widetab1, windowD, windowpushd, with1, with2, with3, with4, with5, with6, without_rowid1, without_rowid2, without_rowid3, without_rowid4, without_rowid5, without_rowid6, without_rowid7, zerodamage

### `FTS3/4/5 feature beyond the basic module N-A (full FTS not implemented)`

- **Category**: N-A (FTS beyond basic module)
- **Files (66)**: e_fts3, fts3aa, fts3ab, fts3ac, fts3ad, fts3ae, fts3af, fts3ag, fts3ah, fts3ai, fts3aj, fts3ak, fts3al, fts3am, fts3an, fts3ao, fts3atoken, fts3auto, fts3b, fts3c, fts3conf, fts3corrupt, fts3cov, fts3d, fts3defer, fts3drop, fts3dropmod, fts3e, fts3expr, fts3f, fts3fault, fts3first, fts3integrity, fts3join, fts3malloc, fts3matchinfo, fts3misc, fts3near, fts3offsets, fts3prefix, fts3query, fts3rank, fts3rnd, fts3shared, fts3snippet, fts3sort, fts3tok1, fts3varint, fts4aa, fts4check, fts4content, fts4docid, fts4growth, fts4incr, fts4langid, fts4lastrowid, fts4merge, fts4min, fts4noti, fts4onepass, fts4opt, fts4record, fts4rename, fts4umlaut, fts4unicode, fts4upfrom

### `WAL/journal mode not implemented N-A`

- **Category**: N-A (WAL/journal)
- **Files (33)**: e_wal, e_walauto, e_walckpt, e_walhook, journal1, journal2, journal3, jrnlmode, jrnlmode2, jrnlmode3, mjournal, wal64k, walbak, walckptnoop, walcksum, walcrash, walcrash2, walcrash3, walcrash4, walfault, walfault2, walhook, walmode, walnoshm, walprotocol, walprotocol2, walrestart, walseh1, walsetlk, walsetlk2, walsetlk3, walslow, walvfs

### `VFS/fault-injection harness N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (30)**: backup_ioerr, backup_malloc, dbfuzz001, fallocate, fts3fault2, fts3fault3, fuzz, fuzz-oss1, fuzz2, fuzz3, fuzz4, fuzzer1, fuzzer2, fuzzerfault, mallocI, mallocK, mmap1, mmap2, mmap3, mmap4, mmapcorrupt, mmapwarm, pagerfault, pagerfault2, pagerfault3, rollbackfault, snapshot_fault, sysfault, unionvtabfault, writecrash

### `extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (23)**: amatch1, csv01, dbpage, dbpagefault, extension01, json101, json102, json103, json104, json105, json106, json107, json108, json109, json501, json502, jsonb01, loadext, loadext2, spellfix, spellfix2, spellfix3, spellfix4

### `multi-connection/locking not implemented DEFERRED`

- **Category**: DEFERRED
- **Files (23)**: manydb, multiplex, multiplex2, multiplex3, multiplex4, nolock, shared, shared2, shared3, shared4, shared6, shared7, shared8, shared9, shared_err, sharedlock, shmlock, snapshot, snapshot2, snapshot3, snapshot4, snapshot_up, superlock

### `FTS3/4/5 beyond basic module N-A`

- **Category**: N-A (FTS beyond basic module)
- **Files (20)**: fts-9fd058691, fts3atoken2, fts3aux1, fts3aux2, fts3defer2, fts3defer3, fts3expr2, fts3expr3, fts3expr4, fts3expr5, fts3matchinfo2, fts3prefix2, fts3snippet2, fts3tok_err, fts4growth2, fts4intck1, fts4merge2, fts4merge3, fts4merge4, fts4merge5

### `C API not exposed N-A`

- **Category**: N-A (C API)
- **Files (11)**: capi3b, capi3c, capi3d, capi3e, e_blobbytes, e_blobclose, e_blobopen, e_blobwrite, e_changes, e_totalchanges, zeroblob

### `window functions not implemented N-A`

- **Category**: N-A (not implemented surface)
- **Files (10)**: window, window1, window2, window3, window4, window5, window6, window7, window8, window9

### `WAL journal mode not implemented N-A`

- **Category**: N-A (WAL/journal)
- **Files (9)**: wal, wal2, wal3, wal4, wal5, wal6, wal7, wal8, wal9

### `sqlite3_memdebug memory-accounting C API N-A`

- **Category**: N-A (C API)
- **Files (8)**: malloc, malloc3, malloc4, malloc5, malloc6, malloc7, malloc8, malloc9

### `crashsql crash-recovery simulation N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (8)**: crash, crash2, crash3, crash4, crash5, crash6, crash7, crash8

### `multi-connection database locking not implemented DEFERRED`

- **Category**: DEFERRED
- **Files (7)**: lock, lock2, lock3, lock4, lock5, lock6, lock7

### `VFS I/O error simulation N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (7)**: io, ioerr, ioerr2, ioerr3, ioerr4, ioerr5, ioerr6

### `skip-scan planner strategy + TCL assoc-array data N-A`

- **Category**: N-A (TCL harness)
- **Files (6)**: skipscan, skipscan1, skipscan2, skipscan3, skipscan5, skipscan6

### `FTS3 corrupt-database tokenizer harness N-A (FTS not implemented)`

- **Category**: N-A (FTS beyond basic module)
- **Files (6)**: fts3corrupt2, fts3corrupt3, fts3corrupt4, fts3corrupt5, fts3corrupt6, fts3corrupt7

### `win32 platform-specific tests N-A`

- **Category**: N-A (platform)
- **Files (5)**: win32, win32heap, win32lock, win32longpath, win32nolock

### `sqlite3_backup C API not implemented N-A`

- **Category**: N-A (C API)
- **Files (4)**: backup, backup2, backup4, backup5

### `C-API incremental blob I/O not implemented N-A`

- **Category**: N-A (not implemented surface)
- **Files (4)**: incrblob, incrblob2, incrblob3, incrblob4

### `sqlite3_unlock_notify C API not implemented N-A`

- **Category**: N-A (C API)
- **Files (4)**: notify, notify1, notify2, notify3

### `TCL authorizer procs not transpiled into Go authorizer callbacks N-A`

- **Category**: N-A (TCL harness)
- **Files (3)**: auth, auth2, auth3

### `TCL expr rand/pow/format %.32e random float stress harness N-A`

- **Category**: N-A (TCL harness)
- **Files (3)**: atof, atof1, atof2

### `shared-cache multi-connection concurrency not implemented DEFERRED`

- **Category**: DEFERRED
- **Files (2)**: tkt2854, tkt3793

### `>4GB large-file TCL harness + msg redeclare transpiler bug N-A`

- **Category**: N-A (TCL harness)
- **Files (2)**: bigfile, bigfile2

### `quota VFS extension not implemented N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (2)**: quota_, quota-glob

### `CARRAY extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (2)**: carray01, carray02

### `sqlite3_exec invalid-UTF-8 C API N-A`

- **Category**: N-A (C API)
- **Files (2)**: badutf, badutf2

### `sqlite3_bind C API N-A`

- **Category**: N-A (C API)
- **Files (2)**: bind, bind2

### `multi-connection busy-handler locking DEFERRED`

- **Category**: DEFERRED
- **Files (2)**: busy, busy2

### `C-API prepared statements N-A`

- **Category**: N-A (C API)
- **Files (2)**: capi2, capi3

### `sqlite3_*_hook C API N-A`

- **Category**: N-A (C API)
- **Files (2)**: hook, hook2

### `sqlite3_interrupt C API N-A`

- **Category**: N-A (C API)
- **Files (2)**: interrupt, interrupt2

### `sqlite3_db_status C API N-A`

- **Category**: N-A (C API)
- **Files (2)**: dbstatus, dbstatus2

### `zipfile extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (2)**: zipfile, zipfile2

### `crashsql crash-simulation while loop not transpilable N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (1)**: savepoint4

### `dynamic TCL proc harness (eval/insert_rows/random_integers) N-A`

- **Category**: N-A (extension)
- **Files (1)**: savepoint6

### `C-API prepared-statement changes() test (sqlite3_prepare/step) N-A`

- **Category**: N-A (C API)
- **Files (1)**: changes2

### `row-value IN subquery with NULLs not implemented (G2.SUBQUERY)`

- **Category**: N-A (not implemented surface)
- **Files (1)**: nulls2

### `echo module xSync callback trace (C test-module ABI) not applicable`

- **Category**: N-A/DEFERRED
- **Files (1)**: vtab7

### `JSON json_extract expression indexes not supported (JSON extension excluded)`

- **Category**: N-A (extension)
- **Files (1)**: indexexpr3

### `legacy file-format short-row tests (hexio helpers) not implemented`

- **Category**: N-A (not implemented surface)
- **Files (1)**: alter2

### `VDBE sorter internals (do_sorter_test) not implemented`

- **Category**: N-A (not implemented surface)
- **Files (1)**: sort4

### `value-subtype API (C-extension) not implemented`

- **Category**: N-A (extension)
- **Files (1)**: subtype1

### `TCL-implemented virtual table (register_tcl_module) N-A`

- **Category**: N-A (TCL harness)
- **Files (1)**: rowvalue5

### `C-API incrblob blob handle + tclvar harness fn not implemented N-A`

- **Category**: N-A (TCL harness)
- **Files (1)**: tkt2332

### `cache-spill lock-failure simulation (read_lock_db harness) N-A`

- **Category**: N-A/DEFERRED
- **Files (1)**: tkt2409

### `PRAGMA max_page_count not enforced (database or disk is full) N-A; MAX_PAGE_COUNT NEEDED`

- **Category**: N-A/DEFERRED
- **Files (1)**: tkt2686

### `test-harness execsql UDF (runs SQL from within a query) not implemented N-A`

- **Category**: N-A (TCL harness)
- **Files (1)**: tkt3080

### `multi-connection busy-handler locking not implemented DEFERRED`

- **Category**: DEFERRED
- **Files (1)**: tkt3093

### `multi-connection schema staleness not implemented DEFERRED`

- **Category**: DEFERRED
- **Files (1)**: tkt3810

### `test-harness SQL-executing UDFs f1/f2 not implemented N-A`

- **Category**: N-A (TCL harness)
- **Files (1)**: tkt3718

### `FTS4 virtual table not implemented N-A`

- **Category**: N-A (FTS beyond basic module)
- **Files (1)**: tkt-bdc6bbbb38

### `UTF-16 hex test-harness functions N-A`

- **Category**: N-A (TCL harness)
- **Files (1)**: tkt-3fe897352e

### `JSON operators (->>) not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: tkt-99378177930f87bd

### `custom VFS device simulation + OOM fault injection N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (1)**: tkt-9d68c883

### `faultsim OOM/injection tests N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (1)**: tkt-9f2eb3abac

### `testvfs custom VFS + multi-connection ATTACH N-A/DEFERRED`

- **Category**: DEFERRED
- **Files (1)**: tkt-f3e5abed55

### `EXPLAIN VDBE opcode inspection N-A`

- **Category**: N-A/DEFERRED
- **Files (1)**: tkt-f67b41381a

### `recursive CTE stat4 sampling + TCL expr function body N-A`

- **Category**: N-A (TCL harness)
- **Files (1)**: analyzeF

### `db prepare DDL comment-escape transpiler bug + C-API prepare N-A`

- **Category**: N-A (C API)
- **Files (1)**: autoinc

### `sqlite_dbpage virtual table extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: dbdata

### `decimal extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: decimal

### `sqlite3_complete C API + TCL namespace procs N-A`

- **Category**: N-A (C API)
- **Files (1)**: main

### `SQLITE_DBCONFIG_RESET_DATABASE C API N-A`

- **Category**: N-A (C API)
- **Files (1)**: resetdb

### `CLI shell subprocess harness N-A`

- **Category**: N-A/DEFERRED
- **Files (1)**: shellA

### `TCL binding tests N-A (TCL API)`

- **Category**: N-A (TCL harness)
- **Files (1)**: tclsqlite

### `SQLITE_FCNTL_DATA_VERSION file-control C API N-A`

- **Category**: N-A (C API)
- **Files (1)**: dataversion1

### `sqlite3_table_column_metadata C API N-A`

- **Category**: N-A (C API)
- **Files (1)**: colmeta

### `SQLITE_TESTCTRL_IMPOSTER test-control C API N-A`

- **Category**: N-A (C API)
- **Files (1)**: imposter1

### `base64/base85 extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: basexx1

### `custom checksum VFS not implemented N-A`

- **Category**: N-A (VFS/fault harness)
- **Files (1)**: cksumvfs

### `transitive_closure virtual table extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: closure01

### `pager/btree cache internals DEFERRED`

- **Category**: DEFERRED
- **Files (1)**: cache

### `percentile extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: percentile

### `execution-speed benchmark N-A`

- **Category**: N-A/DEFERRED
- **Files (1)**: speed4

### `ieee754 extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: ieee754

### `intarray extension not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: intarray

### `incremental-blob corrupt-db C API N-A`

- **Category**: N-A (C API)
- **Files (1)**: incrcorrupt

### `incremental-blob error paths C API N-A`

- **Category**: N-A (C API)
- **Files (1)**: incrblob_err

### `C-API prepared-statement bind/column test (sqlite3_prepare/step/column_text) N-A`

- **Category**: N-A (C API)
- **Files (1)**: misc6

### `eval() loadable extension (ext/misc/eval.c) not implemented N-A`

- **Category**: N-A (extension)
- **Files (1)**: misc8

### `FTS3 compression option tests N-A (FTS not implemented)`

- **Category**: N-A (FTS beyond basic module)
- **Files (1)**: fts3comp1

---

## Maintenance

- Add a verbatim reason here (same string as in `skiptestfiles.go`) when a
  new whole-file skip is introduced.
- Re-classifying a skip as N-A requires evidence of an equivalent-or-better
  pure-Go implementation or that the test exercises C-runtime internals
  with no SQL-functional surface (PORTPLAN §10).
- DEFERRED entries are *tracked*, not closed: they must be revisited in the
  phase that implements the feature (window → G4, WAL → G7, ...).
