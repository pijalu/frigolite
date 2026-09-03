Frigolite testgen status  (generated 2026-09-03T16:50:35Z)

FAMILY              TOTAL   PASS   FAIL   SKIP     PCT
----------------------------------------------------------
AGG                     7      4      3      0   57.1%
C-API                  48     27     10     11   56.2%
CONCURRENCY            39      9      1     29   23.1%
CRUD                   59     32     27      0   54.2%
CTE-WINDOW             34     25      9      0   73.5%
EXPR                   43     33      8      2   76.7%
FTS                    96     49     27     20   51.0%
FUNCTIONS              25     18      7      0   72.0%
JOIN                   37     23      7      7   62.2%
JSON                   12     10      1      1   83.3%
ORDER                  26     20      5      1   76.9%
OTHER                 533    277    115    141   52.0%
PLANNER                38     18      3     17   47.4%
RTREE                   1      1      0      0  100.0%
SCHEMA                121     60     50     11   49.6%
SESSION                 2      2      0      0  100.0%
VTAB                   49     29     10     10   59.2%
WAL                    49      7      7     35   14.3%
----------------------------------------------------------
TOTAL                1219    644    290    285   52.8%

PACKAGES
PKG                FAMILY         STATE     DETAIL
--------------------------------------------------------------------------------
aggerror           AGG            fail      1 files — T 7;
            SELECT x_count(*) FROM t1;
          
  ...
aggfault           AGG            pass      1 files
aggnested          AGG            pass      1 files, 4 tests skipped
aggorderby         AGG            fail      1 files, 6 tests skipped — --- FAIL: Test_aggorderby (1.91s)
    aggorderby_test.go:...
count              AGG            fail      1 files — --- FAIL: Test_count (3.85s)
    count_test.go:299: expec...
countofview        AGG            pass      1 files
having             AGG            pass      1 files
backup             C-API          fail      1 files — --- FAIL: Test_backup (4.65s)
    backup_test.go:1435: ex...
backup2            C-API          pass      1 files
backup4            C-API          pass      1 files
backup5            C-API          pass      1 files
backup_ioerr       C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
backup_malloc      C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
bind               C-API          fail      1 files — got:  [$one]
          want: []
          body: do_test b...
bind2              C-API          pass      1 files
bindxfer           C-API          skipped   1 files, 1 whole-file skip (sqlite3_transfer_bindings deprecated prepared-statement V...)
blob               C-API          pass      1 files
capi2              C-API          pass      1 files, 1 tests skipped
capi3              C-API          pass      1 files, 7 tests skipped
capi3b             C-API          pass      1 files
capi3c             C-API          pass      1 files, 6 tests skipped
capi3d             C-API          pass      1 files, 2 tests skipped
capi3e             C-API          pass      1 files
changes            C-API          fail      1 files — ine_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB)....
changes2           C-API          pass      1 files
colmeta            C-API          pass      1 files
dbstatus           C-API          pass      1 files
exec               C-API          pass      1 files
hook               C-API          fail      1 files, 41 tests skipped — internal/exec/engine_core.go:736 +0x360
github.com/pijalu...
hook2              C-API          pass      1 files
imposter1          C-API          skipped   1 files, 1 whole-file skip (sqlite3_test_control(SQLITE_TESTCTRL_IMPOSTER) test-only ...)
incrblob           C-API          skipped   1 files, 1 whole-file skip (incremental-blob TCL channel + SQL/constraint paths not r...)
incrblob2          C-API          skipped   1 files, 1 whole-file skip (incremental-blob TCL channel + UNIQUE-constraint INSERT.....)
incrblob3          C-API          fail      1 files — --- FAIL: Test_incrblob3 (0.01s)
    incrblob3_test.go:57...
incrblob4          C-API          skipped   1 files, 1 whole-file skip (incremental-blob TCL channel + blob-handle count assertio...)
incrblob_err       C-API          pass      1 files
incrblobfault      C-API          pass      1 files
incrcorrupt        C-API          pass      1 files
incrvacuum         C-API          fail      1 files — PRAGMA incremental_vacuum;
    incrvacuum_test.go:358: ex...
incrvacuum2        C-API          fail      1 files — x2f0
github.com/pijalu/frigolite.(*DB).Query(0x64cd84bda5...
incrvacuum3        C-API          fail      2 files — t.go:141: result mismatch
          got:  [*** in databas...
incrvacuum_ioerr   C-API          pass      1 files
interrupt          C-API          pass      1 files
interrupt2         C-API          pass      1 files
lastinsert         C-API          fail      1 files — --- FAIL: Test_lastinsert (0.01s)
    lastinsert_test.go:...
laststmtchanges    C-API          pass      1 files
notify1            C-API          pass      1 files
notify2            C-API          fail      1 files — # github.com/pijalu/frigolite/testgen/notify2 [github.com...
notify3            C-API          pass      1 files
progress           C-API          skipped   1 files, 1 whole-file skip (dynamic TCL progress callback procedure harness N-A)
sqllog             C-API          skipped   1 files, 1 whole-file skip (SQLite test_sqllog.c extension / VFS SQL logger C-runtime...)
stmt               C-API          pass      1 files
stmtrand           C-API          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
stmtvtab1          C-API          skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_stmtvtab1_test.go))
tableapi           C-API          pass      1 files
busy               CONCURRENCY    skipped   1 files, 1 whole-file skip (busy-handler (sqlite3_busy_handler C-API; `db busy` trans...)
busy2              CONCURRENCY    skipped   1 files, 1 whole-file skip (busy-handler (sqlite3_busy_handler C-API; `db busy` trans...)
exclusive          CONCURRENCY    skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
lock               CONCURRENCY    pass      1 files
lock2              CONCURRENCY    pass      1 files
lock3              CONCURRENCY    pass      1 files
lock4              CONCURRENCY    skipped   1 files, 1 whole-file skip (two-process fixture emulation (test2-script.tcl subproces...)
lock5              CONCURRENCY    pass      1 files
lock6              CONCURRENCY    fail      1 files — # github.com/pijalu/frigolite/testgen/lock6 [github.com/p...
lock7              CONCURRENCY    pass      1 files
manydb             CONCURRENCY    skipped   1 files, 1 whole-file skip (TCL `file channels`/`ulimit` file-descriptor leak harness...)
multiplex          CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
multiplex2         CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
multiplex3         CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
multiplex4         CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
nolock             CONCURRENCY    skipped   1 files, 1 whole-file skip (testvfs VFS lock-call counting needs a VFS instrumentatio...)
pendingrace        CONCURRENCY    skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rowallock          CONCURRENCY    pass      1 files
shared             CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared2            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared3            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared4            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared6            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared7            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared8            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shared9            CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
sharedA            CONCURRENCY    pass      1 files
sharedB            CONCURRENCY    pass      1 files
shared_err         CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
sharedlock         CONCURRENCY    skipped   1 files, 1 whole-file skip (shared-cache (sqlite3_enable_shared_cache/table-level loc...)
shmlock            CONCURRENCY    skipped   1 files, 1 whole-file skip (WAL shared-memory (vfs_shmlock) locking not implemented N-A)
snapshot           CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 (evidence frigolite_snapshot_test.go + portplan/NA...)
snapshot2          CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 (evidence frigolite_snapshot_test.go + portplan/NA...)
snapshot3          CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 (evidence frigolite_snapshot_test.go + portplan/NA...)
snapshot4          CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 (evidence frigolite_snapshot_test.go + portplan/NA...)
snapshot_fault     CONCURRENCY    skipped   1 files, 1 whole-file skip (VFS fault-injection harness N-A (sqlite3_test_control FAU...)
snapshot_up        CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 (evidence frigolite_snapshot_test.go + portplan/NA...)
superlock          CONCURRENCY    skipped   1 files, 1 whole-file skip (WAL/shared-memory (sqlite3demo_superlock) not implemented...)
unixexcl           CONCURRENCY    pass      1 files
alias              CRUD           pass      1 files
all                CRUD           pass      1 files
default_pkg        CRUD           pass      1 files
delete2            CRUD           fail      1 files — --- FAIL: Test_delete2 (0.00s)
    delete2_test.go:171: r...
delete3            CRUD           pass      1 files
delete4            CRUD           pass      1 files
delete_db          CRUD           pass      1 files
delete_pkg         CRUD           fail      1 files — core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Exec...
emptytable         CRUD           pass      1 files
insert             CRUD           fail      1 files, 1 tests skipped — --- FAIL: Test_insert (0.07s)
    insert_test.go:384: exp...
insert2            CRUD           pass      1 files
insert3            CRUD           fail      1 files — --- FAIL: Test_insert3 (0.16s)
    insert3_test.go:131: e...
insert4            CRUD           fail      1 files, 6 tests skipped — ge 9: never used Page 19: never used Page 20: never used]...
insert5            CRUD           pass      1 files
insertfault        CRUD           pass      1 files
intpkey            CRUD           fail      1 files, 2 tests skipped — --- FAIL: Test_intpkey (0.07s)
    intpkey_test.go:475: q...
queryonly          CRUD           pass      1 files
returning1         CRUD           fail      1 files — --- FAIL: Test_returning1 (0.00s)
    returning1_test.go:...
returningfault     CRUD           pass      1 files
rowid              CRUD           fail      1 files, 6 tests skipped — --- FAIL: Test_rowid (0.18s)
    rowid_test.go:208: resul...
select1            CRUD           fail      1 files — test.go:1787: result mismatch
          got:  [{}]
      ...
select2            CRUD           fail      1 files — 8: 6 7 8]
          body: do_test select2-1.1
    select2...
select3            CRUD           fail      1 files — --- FAIL: Test_select3 (0.06s)
    select3_test.go:192: e...
select4            CRUD           fail      1 files, 2 tests skipped — --- FAIL: Test_select4 (0.07s)
    select4_test.go:833: e...
select5            CRUD           fail      1 files — --- FAIL: Test_select5 (0.01s)
    select5_test.go:136: e...
select6            CRUD           fail      1 files — SELECT * FROM t UNION ALL 
            SELECT l,m,l FROM ...
select7            CRUD           fail      1 files — 475 UNION ALL SELECT 476 UNION ALL SELECT 477 UNION ALL S...
select8            CRUD           pass      1 files
select9            CRUD           pass      1 files
selectA            CRUD           pass      1 files
selectB            CRUD           pass      1 files
selectC            CRUD           pass      1 files
selectD            CRUD           pass      1 files
selectE            CRUD           pass      1 files
selectF            CRUD           pass      1 files
selectG            CRUD           pass      1 files
selectH            CRUD           fail      1 files — --- FAIL: Test_selectH (0.02s)
    selectH_test.go:108: r...
tableopts          CRUD           fail      1 files, 1 tests skipped — --- FAIL: Test_tableopts (0.00s)
    tableopts_test.go:12...
tempdb             CRUD           pass      1 files
temptable          CRUD           pass      1 files, 6 tests skipped
types              CRUD           pass      1 files
update             CRUD           fail      1 files — SET a=1;
          
    update_test.go:1044: query error:...
update2            CRUD           fail      1 files — {} 5 {} 6 {} 7 {} 8 {} 9 {} 10 {} 11 {}]
    update2_test...
upfrom1            CRUD           fail      1 files — --- FAIL: Test_upfrom1 (0.01s)
    upfrom1_test.go:157: q...
upfrom2            CRUD           pass      1 files, 1 tests skipped
upfrom3            CRUD           pass      1 files
upfrom4            CRUD           fail      1 files — --- FAIL: Test_upfrom4 (0.00s)
    upfrom4_test.go:79: qu...
upfromfault        CRUD           pass      1 files
upsert1            CRUD           fail      1 files — --- FAIL: Test_upsert1 (0.00s)
    upsert1_test.go:68: qu...
upsert2            CRUD           fail      1 files — --- FAIL: Test_upsert2 (0.00s)
    upsert2_test.go:68: qu...
upsert3            CRUD           pass      1 files
upsert4            CRUD           fail      1 files — nstraint failed: c
          sql: 
            INSERT INT...
upsert5            CRUD           fail      1 files — want: [1 2 3 4 5]
    upsert5_test.go:505: result mismatc...
upsertfault        CRUD           pass      1 files
values             CRUD           pass      1 files, 1 tests skipped
valuesfault        CRUD           pass      1 files
view               CRUD           fail      1 files, 2 tests skipped — --- FAIL: Test_view (0.03s)
    view_test.go:356: expecte...
view2              CRUD           pass      1 files
view3              CRUD           fail      1 files — VIEW v1024 AS SELECT * FROM v512 UNION SELECT * FROM v512...
filter1            CTE-WINDOW     fail      1 files — --- FAIL: Test_filter1 (0.01s)
    filter1_test.go:234: r...
filter2            CTE-WINDOW     pass      1 files
filterfault        CTE-WINDOW     pass      1 files
window1            CTE-WINDOW     pass      1 files, 2 tests skipped
window2            CTE-WINDOW     pass      1 files
window3            CTE-WINDOW     pass      1 files
window4            CTE-WINDOW     pass      1 files
window5            CTE-WINDOW     pass      1 files, 4 tests skipped
window6            CTE-WINDOW     pass      1 files
window7            CTE-WINDOW     pass      1 files
window8            CTE-WINDOW     pass      1 files
window9            CTE-WINDOW     pass      1 files
windowA            CTE-WINDOW     pass      1 files
windowB            CTE-WINDOW     fail      1 files — --- FAIL: Test_windowB (0.02s)
    windowB_test.go:166: e...
windowC            CTE-WINDOW     fail      1 files — --- FAIL: Test_windowC (0.04s)
    windowC_test.go:149: r...
windowD            CTE-WINDOW     pass      1 files
windowE            CTE-WINDOW     fail      1 files — 4 0.0 487 0.0 488 0.0 489 0.0 490 0.0 491 0.0 494 0.0 495...
windowerr          CTE-WINDOW     pass      1 files
windowfault        CTE-WINDOW     fail      1 files — --- FAIL: Test_windowfault (0.16s)
    windowfault_test.g...
windowpushd        CTE-WINDOW     pass      1 files
with1              CTE-WINDOW     fail      1 files, 9 tests skipped — --- FAIL: Test_with1 (5.64s)
    with1_test.go:632: resul...
with2              CTE-WINDOW     fail      1 files, 6 tests skipped — ny) * rsy / (maxy-miny)
                WHEN 0 >= maxy TH...
with3              CTE-WINDOW     pass      1 files
with4              CTE-WINDOW     pass      1 files
with5              CTE-WINDOW     pass      1 files
with6              CTE-WINDOW     pass      1 files
withM              CTE-WINDOW     pass      1 files
without_rowid1     CTE-WINDOW     pass      1 files, 3 tests skipped
without_rowid2     CTE-WINDOW     pass      1 files
without_rowid3     CTE-WINDOW     fail      1 files, 10 tests skipped — - FAIL: Test_without_rowid3 (0.21s)
    without_rowid3_te...
without_rowid4     CTE-WINDOW     fail      1 files — om/pijalu/frigolite.(*DB).Exec(0x6e95d33d02d0, {0x103118b...
without_rowid5     CTE-WINDOW     pass      1 files
without_rowid6     CTE-WINDOW     pass      1 files
without_rowid7     CTE-WINDOW     pass      1 files
between            EXPR           pass      1 files
cast               EXPR           pass      1 files
coalesce           EXPR           pass      1 files
expr               EXPR           pass      1 files, 3 tests skipped
expr2              EXPR           pass      1 files
exprfault          EXPR           pass      1 files
exprfault2         EXPR           pass      1 files
expridx1           EXPR           fail      1 files, 6 tests skipped — --- FAIL: Test_expridx1 (3.42s)
    expridx1_test.go:361:...
expridx2           EXPR           pass      1 files
hexlit             EXPR           pass      1 files
in                 EXPR           fail      1 files — in_test.go:544: expected error containing "SELECTs to the...
istrue             EXPR           pass      1 files
literal            EXPR           pass      1 files
null               EXPR           fail      1 files — t2 values(2,null);
              insert into t2 values(3,...
numcast            EXPR           pass      1 files
where              EXPR           fail      1 files, 9 tests skipped — sult mismatch
          got:  [{}]
          want: [54 5 ...
where2             EXPR           pass      1 files, 7 tests skipped
where3             EXPR           pass      1 files
where4             EXPR           pass      1 files, 2 tests skipped
where5             EXPR           pass      1 files
where6             EXPR           pass      1 files
where7             EXPR           pass      1 files
where8             EXPR           skipped   1 files, 1 whole-file skip (hash/btree DISTINCT ordering fuzz N-A (where8-4.x SELECT ...)
where9             EXPR           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
whereA             EXPR           fail      1 files, 2 tests skipped — --- FAIL: Test_whereA (0.01s)
    whereA_test.go:207: res...
whereB             EXPR           pass      1 files
whereC             EXPR           pass      1 files
whereD             EXPR           pass      1 files
whereE             EXPR           pass      1 files
whereF             EXPR           pass      1 files, 3 tests skipped
whereG             EXPR           fail      1 files — ack
             WHERE likelihood(cname LIKE '%bach%', 1....
whereH             EXPR           pass      1 files, 16 tests skipped
whereI             EXPR           pass      1 files
whereJ             EXPR           pass      1 files
whereK             EXPR           pass      1 files
whereL             EXPR           pass      1 files
whereM             EXPR           pass      1 files
whereN             EXPR           pass      1 files
wherefault         EXPR           pass      1 files
wherelfault        EXPR           pass      1 files
wherelimit         EXPR           fail      1 files — wherelimit_test.go:82: expected error containing "ORDER B...
wherelimit2        EXPR           fail      1 files, 6 tests skipped — --- FAIL: Test_wherelimit2 (0.02s)
    wherelimit2_test.g...
wherelimit3        EXPR           pass      1 files
fts3               FTS            pass      1 files
fts3aa             FTS            pass      1 files
fts3ab             FTS            pass      1 files
fts3ac             FTS            pass      1 files
fts3ad             FTS            pass      1 files
fts3ae             FTS            pass      1 files
fts3af             FTS            pass      1 files
fts3ag             FTS            pass      1 files
fts3ah             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3ai             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3aj             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3ak             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3al             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3am             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3an             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3ao             FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3atoken         FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3atoken2        FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3auto           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3aux1           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3aux2           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3b              FTS            fail      1 files — 9901 9902 9903 9904 9905 9906 9907 9908 9909 9910 9911 99...
fts3c              FTS            pass      1 files
fts3comp1          FTS            fail      1 files — smatch
          got:  [{}]
          want: [eight 1 1 fo...
fts3conf           FTS            fail      1 files — ain.t1 malformed inverted index for FTS4 table main.t3]
 ...
fts3corrupt        FTS            fail      1 files — INSERT INTO f_segments values (0, x'');
          INSERT ...
fts3corrupt2       FTS            pass      1 files
fts3corrupt3       FTS            fail      1 files — --- FAIL: Test_fts3corrupt3 (0.01s)
    fts3corrupt3_test...
fts3corrupt4       FTS            pass      1 files, 52 tests skipped
fts3corrupt5       FTS            pass      1 files
fts3corrupt6       FTS            fail      1 files, 1 tests skipped — --- FAIL: Test_fts3corrupt6 (0.26s)
    fts3corrupt6_test...
fts3corrupt7       FTS            pass      1 files
fts3cov            FTS            pass      1 files
fts3d              FTS            fail      1 files — UPDATE t1 SET c = 'That was a test three' WHERE rowid = 2...
fts3defer          FTS            fail      1 files — --- FAIL: Test_fts3defer (0.01s)
    fts3defer_test.go:12...
fts3defer2         FTS            fail      1 files, 13 tests skipped — --- FAIL: Test_fts3defer2 (0.73s)
    fts3defer2_test.go:...
fts3defer3         FTS            pass      1 files, 1 tests skipped
fts3drop           FTS            fail      1 files — --- FAIL: Test_fts3drop (0.00s)
    fts3drop_test.go:85: ...
fts3dropmod        FTS            pass      1 files
fts3e              FTS            pass      1 files
fts3expr           FTS            pass      1 files
fts3expr2          FTS            pass      1 files
fts3expr3          FTS            pass      1 files
fts3expr4          FTS            pass      1 files
fts3expr5          FTS            pass      1 files
fts3f              FTS            pass      1 files
fts3fault          FTS            pass      1 files
fts3fault2         FTS            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fts3fault3         FTS            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fts3first          FTS            pass      1 files
fts3fuzz001        FTS            fail      1 files — sql: 
            INSERT INTO t1(t1) VALUES('integrity-ch...
fts3integrity      FTS            fail      1 files — --- FAIL: Test_fts3integrity (0.00s)
    fts3integrity_te...
fts3join           FTS            fail      1 files — --- FAIL: Test_fts3join (0.01s)
    fts3join_test.go:104:...
fts3malloc         FTS            skipped   1 files, 1 whole-file skip (sqlite3_memdebug_fail OOM-injection C API N-A (malloc fam...)
fts3matchinfo      FTS            fail      1 files — --- FAIL: Test_fts3matchinfo (0.22s)
    fts3matchinfo_te...
fts3matchinfo2     FTS            pass      1 files
fts3misc           FTS            skipped   1 files, 1 whole-file skip (200-column FTS3 schema row exceeds one page at TEST-defau...)
fts3near           FTS            pass      1 files
fts3offsets        FTS            pass      1 files
fts3prefix         FTS            pass      1 files
fts3prefix2        FTS            fail      1 files — +0x360
github.com/pijalu/frigolite.(*DB).Exec(0x7787f9ecc...
fts3query          FTS            pass      1 files
fts3rank           FTS            pass      1 files
fts3rnd            FTS            skipped   1 files, 1 whole-file skip (randomized stress suite exceeds runtime budget (>600s); d...)
fts3shared         FTS            skipped   1 files, 1 whole-file skip (shared-cache read-during-write locking ('database table i...)
fts3snippet        FTS            fail      1 files — twohundredseventynine twohundredeighty twohundredeightyon...
fts3snippet2       FTS            pass      1 files
fts3sort           FTS            pass      1 files
fts3tok1           FTS            fail      1 files — --- FAIL: Test_fts3tok1 (0.00s)
    fts3tok1_test.go:86: ...
fts3tok_err        FTS            pass      1 files
fts3varint         FTS            pass      1 files
fts4aa             FTS            pass      1 files
fts4check          FTS            fail      1 files — , 0x5ecf1240a2d0, {0x100780bb8?, 0x4?}, {0x100783cd0?, 0x...
fts4content        FTS            fail      1 files, 7 tests skipped — ning "SQL logic error", got: 't1' is not a function
     ...
fts4docid          FTS            pass      1 files
fts4growth         FTS            fail      1 files — o:736 +0x360
github.com/pijalu/frigolite.(*DB).Query(0x57...
fts4growth2        FTS            pass      1 files
fts4incr           FTS            pass      1 files
fts4intck1         FTS            pass      1 files
fts4langid         FTS            fail      1 files — --- FAIL: Test_fts4langid (6.50s)
    fts4langid_test.go:...
fts4lastrowid      FTS            pass      1 files
fts4merge          FTS            fail      1 files — 12 13 14 15 0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 0 1 2 3...
fts4merge2         FTS            pass      1 files
fts4merge3         FTS            pass      1 files
fts4merge4         FTS            fail      1 files — x360
github.com/pijalu/frigolite.(*DB).Exec(0x658c9f1d087...
fts4merge5         FTS            pass      1 files
fts4min            FTS            pass      1 files
fts4noti           FTS            fail      1 files — --- FAIL: Test_fts4noti (0.11s)
    fts4noti_test.go:232:...
fts4onepass        FTS            fail      1 files — +0x360
github.com/pijalu/frigolite.(*DB).Exec(0x7a141764c...
fts4opt            FTS            fail      1 files — ne_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).E...
fts4record         FTS            pass      1 files
fts4rename         FTS            pass      1 files
fts4umlaut         FTS            pass      1 files
fts4unicode        FTS            fail      1 files — +0x360
github.com/pijalu/frigolite.(*DB).Exec(0x4d3b5f3b6...
fts4upfrom         FTS            pass      1 files
fts_9fd058691      FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
badutf             FUNCTIONS      pass      1 files
ctime              FUNCTIONS      pass      1 files
date               FUNCTIONS      pass      1 files
decimal            FUNCTIONS      pass      1 files, 18 tests skipped
func2              FUNCTIONS      fail      1 files — xpected error containing "wrong number of arguments to fu...
func3              FUNCTIONS      fail      1 files — st be a constant between 0.0 and 1.0", got: <nil>
       ...
func4              FUNCTIONS      fail      1 files, 16 tests skipped — --- FAIL: Test_func4 (0.01s)
    func4_test.go:1457: expe...
func5              FUNCTIONS      pass      1 files, 2 tests skipped
func6              FUNCTIONS      pass      1 files, 10 tests skipped
func7              FUNCTIONS      pass      1 files, 2 tests skipped
func8              FUNCTIONS      pass      1 files
func9              FUNCTIONS      pass      1 files
func_pkg           FUNCTIONS      fail      1 files, 50 tests skipped — unt
          sql: 
              SELECT legacy_count() F...
icu                FUNCTIONS      pass      1 files
instr              FUNCTIONS      pass      1 files
instrfault         FUNCTIONS      pass      1 files
like               FUNCTIONS      fail      1 files — esult mismatch
          got:  [sqlite3_exec_hex db SELEC...
nan                FUNCTIONS      fail      1 files — --- FAIL: Test_nan (0.01s)
    nan_test.go:411: exec erro...
percentile         FUNCTIONS      fail      1 files — inf input to percentile_disc()
          sql: 
          ...
printf             FUNCTIONS      pass      1 files, 27 tests skipped
quote              FUNCTIONS      pass      1 files
substr             FUNCTIONS      pass      1 files
unhex              FUNCTIONS      pass      1 files
zeroblob           FUNCTIONS      pass      1 files
zeroblobfault      FUNCTIONS      pass      1 files
exists             JOIN           pass      1 files
existsexpr         JOIN           fail      1 files — QUERY]
    existsexpr_test.go:161: result mismatch
      ...
existsexpr2        JOIN           pass      1 files
existsfault        JOIN           pass      1 files
full               JOIN           pass      1 files
join               JOIN           fail      1 files, 7 tests skipped — join_test.go:425: expected error containing "unknown join...
join2              JOIN           pass      1 files
join3              JOIN           pass      1 files
join4              JOIN           pass      1 files
join5              JOIN           pass      1 files
join6              JOIN           pass      1 files
join7              JOIN           pass      1 files
join8              JOIN           fail      1 files, 8 tests skipped — nal/exec/engine_core.go:736 +0x360
github.com/pijalu/frig...
join9              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinA              JOIN           pass      1 files
joinB              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinC              JOIN           pass      1 files
joinD              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinE              JOIN           pass      1 files
joinF              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinH              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinI              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rowvalue           JOIN           fail      1 files — --- FAIL: Test_rowvalue (0.07s)
    rowvalue_test.go:869:...
rowvalue2          JOIN           pass      1 files
rowvalue3          JOIN           fail      1 files — --- FAIL: Test_rowvalue3 (0.03s)
    rowvalue3_test.go:22...
rowvalue4          JOIN           fail      1 files — NDEX t2abc ON t2(a ASC, b ASC, c ASC); 
    rowvalue4_tes...
rowvalue5          JOIN           skipped   1 files, 1 whole-file skip (TCL-implemented virtual table (register_tcl_module) N-A)
rowvalue6          JOIN           pass      1 files
rowvalue7          JOIN           pass      1 files
rowvalue8          JOIN           pass      1 files
rowvalue9          JOIN           pass      1 files
rowvalueA          JOIN           pass      1 files
rowvaluefault      JOIN           pass      1 files
rowvaluevtab       JOIN           pass      1 files
subquery           JOIN           fail      1 files — ected error containing "misuse of aggregate: count()", go...
subquery2          JOIN           pass      1 files
subselect          JOIN           pass      1 files
json101            JSON           pass      1 files
json102            JSON           pass      1 files
json103            JSON           pass      1 files
json104            JSON           pass      1 files
json105            JSON           pass      1 files
json106            JSON           pass      1 files
json107            JSON           pass      1 files
json108            JSON           pass      1 files
json109            JSON           skipped   1 files, 1 whole-file skip (remaining json1 function matrix long tail (P6.JSON next s...)
json501            JSON           fail      1 files — --- FAIL: Test_json501 (0.00s)
    json501_test.go:135: r...
json502            JSON           pass      1 files
jsonb01            JSON           pass      1 files
distinct           ORDER          fail      1 files — core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Quer...
distinctagg        ORDER          pass      1 files
limit              ORDER          fail      1 files — al/exec/engine_core.go:736 +0x360
github.com/pijalu/frigo...
minmax             ORDER          fail      1 files — --- FAIL: Test_minmax (0.05s)
    minmax_test.go:101: res...
orderby1           ORDER          fail      1 files, 21 tests skipped — --- FAIL: Test_orderby1 (0.53s)
    orderby1_test.go:613:...
orderby2           ORDER          pass      1 files, 3 tests skipped
orderby3           ORDER          pass      1 files
orderby4           ORDER          pass      1 files
orderby5           ORDER          pass      1 files, 24 tests skipped
orderby6           ORDER          pass      1 files
orderby7           ORDER          pass      1 files, 9 tests skipped
orderby8           ORDER          pass      1 files
orderby9           ORDER          pass      1 files
orderbyA           ORDER          pass      1 files
orderbyB           ORDER          pass      1 files
sort               ORDER          pass      1 files, 2 tests skipped
sort2              ORDER          pass      1 files
sort3              ORDER          pass      1 files, 2 tests skipped
sort4              ORDER          skipped   1 files, 1 whole-file skip (VDBE sorter internals (do_sorter_test) not implemented)
sort5              ORDER          fail      1 files — sort5_test.go:204: result mismatch
          got:  []
   ...
sorterref          ORDER          pass      1 files
sortfault          ORDER          pass      1 files
unionall           ORDER          pass      1 files, 1 tests skipped
unionall2          ORDER          pass      1 files
unionallfault      ORDER          pass      1 files
unordered          ORDER          pass      1 files
affinity2          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/affinity2 [github.c...
affinity3          OTHER          pass      1 files
atof1              OTHER          skipped   1 files, 1 whole-file skip (TCL expr rand/pow/format %.32e random float stress harnes...)
atof2              OTHER          skipped   1 files, 1 whole-file skip (TCL expr rand/pow/format %.32e random float stress harnes...)
atomic             OTHER          pass      1 files
atomic2            OTHER          pass      1 files
auth               OTHER          skipped   1 files, 1 whole-file skip (authorizer framework (db authorizer C callback harness N-A))
auth2              OTHER          skipped   1 files, 1 whole-file skip (authorizer framework (db authorizer C callback harness N-A))
auth3              OTHER          skipped   1 files, 1 whole-file skip (authorizer framework (db authorizer C callback harness N-A))
autoanalyze1       OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autoindex1         OTHER          pass      1 files, 6 tests skipped
autoindex2         OTHER          pass      1 files
autoindex3         OTHER          pass      1 files, 4 tests skipped
autoindex4         OTHER          pass      1 files, 1 tests skipped
autoindex5         OTHER          pass      1 files
avfs               OTHER          fail      1 files — --- FAIL: Test_avfs (0.01s)
    avfs_test.go:242: result ...
avtrans            OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/avtrans [github.com...
backcompat         OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/backcompat [github....
badutf2            OTHER          pass      1 files
basexx1            OTHER          fail      1 files — --- FAIL: Test_basexx1 (0.15s)
    basexx1_test.go:273: e...
bigfile            OTHER          skipped   1 files, 1 whole-file skip (>4GB large-file TCL harness + msg redeclare transpiler bu...)
bigfile2           OTHER          skipped   1 files, 1 whole-file skip (>4GB large-file TCL harness + msg redeclare transpiler bu...)
bigmmap            OTHER          pass      1 files
bigrow             OTHER          fail      1 files — 89 i 9290 j 9291 k 9292 l 9293 m 9294 n 9295 o 9296 p 929...
bigsort            OTHER          pass      1 files
bitvec             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bloom1             OTHER          pass      1 files
boundary1          OTHER          pass      1 files
boundary2          OTHER          pass      1 files
boundary3          OTHER          pass      1 files
boundary4          OTHER          pass      1 files
btree01            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
btree02            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
btreefault         OTHER          fail      1 files — --- FAIL: Test_btreefault (0.01s)
    btreefault_test.go:...
cache              OTHER          skipped   1 files, 1 whole-file skip (pager/btree cache internals DEFERRED)
cacheflush         OTHER          fail      1 files — , aa);
            CREATE TABLE tb(b, bb);
            IN...
cachespill         OTHER          pass      1 files
cffault            OTHER          pass      1 files
chunksize          OTHER          fail      1 files — --- FAIL: Test_chunksize (0.00s)
    chunksize_test.go:11...
cksumvfs           OTHER          skipped   1 files, 1 whole-file skip (custom checksum VFS not implemented N-A)
close_pkg          OTHER          pass      1 files
closure01          OTHER          pass      1 files, 3 tests skipped
colname            OTHER          fail      1 files — --- FAIL: Test_colname (0.01s)
    colname_test.go:511: e...
columncount        OTHER          pass      1 files
conflict2          OTHER          fail      1 files — rror: UNIQUE constraint failed: t2.c
          sql: 
    ...
conflict3          OTHER          fail      1 files — E constraint failed: t1.c", got: <nil>
          sql: INS...
contrib01          OTHER          pass      1 files
corrupt            OTHER          pass      1 files
corrupt2           OTHER          fail      1 files, 2 tests skipped — # github.com/pijalu/frigolite/testgen/corrupt2 [github.co...
corrupt3           OTHER          pass      1 files
corrupt4           OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/corrupt4 [github.co...
corrupt5           OTHER          pass      1 files
corrupt6           OTHER          pass      1 files
corrupt7           OTHER          pass      1 files
corrupt8           OTHER          pass      1 files
corrupt9           OTHER          fail      1 files — xpected error containing "database disk image is malforme...
corruptA           OTHER          pass      1 files
corruptB           OTHER          pass      1 files
corruptC           OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/corruptC [github.co...
corruptD           OTHER          pass      1 files
corruptE           OTHER          pass      1 files
corruptF           OTHER          fail      1 files — --- FAIL: Test_corruptF (0.00s)
    corruptF_test.go:80: ...
corruptG           OTHER          pass      1 files
corruptH           OTHER          pass      1 files
corruptI           OTHER          pass      1 files
corruptJ           OTHER          pass      1 files
corruptK           OTHER          pass      1 files
corruptL           OTHER          fail      1 files — core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Exec...
corruptM           OTHER          pass      1 files
corruptN           OTHER          fail      1 files, 2 tests skipped — # github.com/pijalu/frigolite/testgen/corruptN [github.co...
crash              OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash2             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash3             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash4             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash5             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash6             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash7             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crash8             OTHER          skipped   1 files, 1 whole-file skip (crashsql crash-recovery simulation N-A)
crashM             OTHER          pass      1 files
cse                OTHER          pass      1 files
csv01              OTHER          fail      1 files — t:  [{}]
          want: [abcd randomtext $ii]
    csv01_...
cursorhint2        OTHER          skipped   1 files, 1 whole-file skip (VDBE codeCursorHint() opcode P4 introspection + MySQL pus...)
dataversion1       OTHER          pass      1 files
date2              OTHER          fail      1 files — (a,b) SELECT x, julianday('2017-07-01')+x FROM c;
       ...
date3              OTHER          pass      1 files
date4              OTHER          pass      1 files
date5              OTHER          pass      1 files
dbdata             OTHER          pass      1 files
dbfuzz001          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
dbstatus2          OTHER          fail      1 files — --- FAIL: Test_dbstatus2 (0.00s)
    dbstatus2_test.go:32...
descidx1           OTHER          pass      1 files
descidx2           OTHER          pass      1 files
descidx3           OTHER          fail      1 files — error: UNIQUE constraint failed: t1
          sql: 
     ...
diskfull           OTHER          pass      1 files
distinct2          OTHER          pass      1 files, 4 tests skipped
e_blobbytes        OTHER          pass      1 files
e_blobclose        OTHER          pass      1 files, 5 tests skipped
e_blobopen         OTHER          fail      1 files — --- FAIL: Test_e_blobopen (0.03s)
    e_blobopen_test.go:...
e_blobwrite        OTHER          fail      1 files — --- FAIL: Test_e_blobwrite (0.01s)
    e_blobwrite_test.g...
e_changes          OTHER          fail      1 files — --- FAIL: Test_e_changes (0.06s)
    e_changes_test.go:62...
e_createtable      OTHER          skipped   1 files, 1 whole-file skip (CREATE TABLE type-noise P1.E-SQL deep gap N-A (engine CRE...)
e_delete           OTHER          skipped   1 files, 1 whole-file skip (multi-db trigger cascade P1.E-SQL deep gap N-A (e_delete-...)
e_droptrigger      OTHER          pass      1 files
e_dropview         OTHER          pass      1 files
e_expr             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
e_fkey             OTHER          fail      1 files — c/engine_core.go:736 +0x360
github.com/pijalu/frigolite.(...
e_fts3             OTHER          fail      1 files, 13 tests skipped — exec/engine_core.go:736 +0x360
github.com/pijalu/frigolit...
e_insert           OTHER          pass      1 files
e_reindex          OTHER          fail      1 files, 1 tests skipped — ndex [github.com/pijalu/frigolite/testgen/e_reindex.test]...
e_resolve          OTHER          fail      1 files — n1]
    e_resolve_test.go:180: result mismatch
          ...
e_select           OTHER          skipped   1 files, 1 whole-file skip (DISTINCT collation ordering P1.E-SQL deep gap N-A (e_sele...)
e_select2          OTHER          pass      1 files
e_totalchanges     OTHER          pass      1 files
e_update           OTHER          skipped   1 files, 1 whole-file skip (UPDATE aux schema + trigger cascade P1.E-SQL deep gap N-A)
e_uri              OTHER          skipped   1 files, 1 whole-file skip (C test-VFS sqlite3_open_v2 URI probing (testvfs vfs1/vfs2...)
e_vacuum           OTHER          skipped   1 files, 1 whole-file skip (VACUUM / file-size harness N-A (P1.E-SQL deep gap))
e_wal              OTHER          skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
e_walauto          OTHER          skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
e_walckpt          OTHER          skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
e_walhook          OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
enc                OTHER          pass      1 files
enc2               OTHER          pass      1 files
enc3               OTHER          skipped   1 files, 1 whole-file skip (UTF-16 storage not implemented N-A (evidence frigolite_en...)
enc4               OTHER          pass      1 files
eqp2               OTHER          pass      1 files
errmsg             OTHER          pass      1 files, 1 tests skipped
errofst1           OTHER          pass      1 files
eval               OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
exclusive2         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
extension01        OTHER          fail      1 files — --- FAIL: Test_extension01 (0.00s)
    extension01_test.g...
external_reader    OTHER          pass      1 files
extraquick         OTHER          pass      1 files
fallocate          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
filectrl           OTHER          fail      1 files — --- FAIL: Test_filectrl (0.00s)
    filectrl_test.go:105:...
filefmt            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
fordelete          OTHER          fail      1 files — --- FAIL: Test_fordelete (0.01s)
    fordelete_test.go:23...
format4            OTHER          skipped   1 files, 1 whole-file skip (legacy_file_format file-size harness N-A)
fpconv1            OTHER          pass      1 files, 2 tests skipped
fuzz               OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz2              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz3              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz4              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz_malloc        OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/fuzz_malloc [github...
fuzz_oss1          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzzer1            OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzzer2            OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzzerfault        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
gcfault            OTHER          pass      1 files
gencol1            OTHER          pass      1 files, 12 tests skipped
hidden             OTHER          pass      1 files, 1 tests skipped
ieee754            OTHER          pass      1 files, 2 tests skipped
in2                OTHER          pass      1 files
in3                OTHER          fail      1 files — sql: INSERT INTO t1 VALUES(98,int(log98/log2),9801)
    i...
in4                OTHER          pass      1 files, 5 tests skipped
in5                OTHER          pass      1 files
in6                OTHER          pass      1 files, 2 tests skipped
in7                OTHER          pass      1 files
init               OTHER          fail      1 files — brew/Cellar/go/1.27.0/libexec/src/testing/testing.go:2126...
intreal            OTHER          pass      1 files
io                 OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr              OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr2             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr3             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr4             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr5             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr6             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
journal1           OTHER          pass      1 files
journal2           OTHER          pass      2 files
journal3           OTHER          pass      1 files
jrnlmode2          OTHER          pass      1 files
jrnlmode3          OTHER          pass      1 files
keyword1           OTHER          skipped   1 files, 1 whole-file skip (bare-keyword-as-identifier parser N-A (keyword1))
like2              OTHER          pass      1 files
like3              OTHER          pass      1 files
limit2             OTHER          pass      1 files
literal2           OTHER          pass      1 files
loadext            OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/loadext [github.com...
loadext2           OTHER          pass      1 files
lookaside          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/lookaside [github.c...
main               OTHER          pass      1 files
malloc             OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc3            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc4            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc5            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc6            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc7            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc8            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
malloc9            OTHER          skipped   1 files, 1 whole-file skip (sqlite3_memdebug memory-accounting C API N-A)
mallocA            OTHER          pass      1 files
mallocAll          OTHER          pass      1 files
mallocB            OTHER          pass      1 files
mallocC            OTHER          pass      1 files
mallocD            OTHER          pass      1 files
mallocE            OTHER          pass      1 files
mallocF            OTHER          pass      1 files
mallocG            OTHER          pass      1 files
mallocH            OTHER          pass      1 files
mallocI            OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mallocJ            OTHER          pass      1 files
mallocK            OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mallocL            OTHER          pass      1 files
mallocM            OTHER          pass      1 files
mem5               OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/mem5 [github.com/pi...
memdb              OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memdb1             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memdb2             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memjournal         OTHER          pass      1 files
memjournal2        OTHER          fail      1 files — b=randomblob(700) WHERE a<=300;
          
    memjournal...
memleak            OTHER          pass      1 files
memsubsys1         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memsubsys2         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
merge1             OTHER          pass      1 files
minmax2            OTHER          fail      1 files — --- FAIL: Test_minmax2 (0.05s)
    minmax2_test.go:83: re...
minmax3            OTHER          pass      1 files
minmax4            OTHER          pass      1 files
misc1              OTHER          fail      1 files, 1 tests skipped — ternal/exec/engine_core.go:736 +0x360
github.com/pijalu/f...
misc2              OTHER          pass      1 files
misc3              OTHER          pass      1 files
misc4              OTHER          fail      1 files, 8 tests skipped — regate functions are not allowed in the GROUP BY clause",...
misc5              OTHER          fail      1 files — --- FAIL: Test_misc5 (0.18s)
    misc5_test.go:196: expec...
misc6              OTHER          pass      1 files
misc7              OTHER          fail      1 files, 19 tests skipped — --- FAIL: Test_misc7 (0.00s)
    misc7_test.go:127: frigo...
misc8              OTHER          fail      1 files, 1 tests skipped — nternal/exec/engine_core.go:736 +0x360
github.com/pijalu/...
misuse             OTHER          fail      1 files — --- FAIL: Test_misuse (0.00s)
    misuse_test.go:333: res...
mmap1              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap2              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap3              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap4              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmapcorrupt        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmapfault          OTHER          pass      1 files
mmapwarm           OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mutex1             OTHER          fail      1 files — --- FAIL: Test_mutex1 (0.00s)
    mutex1_test.go:121: res...
mutex2             OTHER          pass      1 files
normalize          OTHER          pass      1 files
nulls1             OTHER          pass      1 files, 4 tests skipped
nulls2             OTHER          pass      1 files
numindex1          OTHER          fail      1 files — --- FAIL: Test_numindex1 (0.01s)
    numindex1_test.go:10...
offset1            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
openv2             OTHER          fail      1 files — --- FAIL: Test_openv2 (0.00s)
    openv2_test.go:106: exp...
oserror            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
ovfl               OTHER          pass      1 files
p_8_3_names        OTHER          fail      1 files — --- FAIL: Test_t_8_3_names (0.83s)
    8_3_names_test.go:...
pager1             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pager2             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pager3             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pager4             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pagerfault         OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
pagerfault2        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
pagerfault3        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
pageropt           OTHER          pass      1 files
pagesize           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
parser1            OTHER          pass      1 files
pcache             OTHER          pass      1 files
pcache2            OTHER          fail      1 files — --- FAIL: Test_pcache2 (0.02s)
    pcache2_test.go:82: re...
permutations       OTHER          fail      1 files — pijalu/frigolite/testgen/permutations [github.com/pijalu/...
pragma             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma2            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma3            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma4            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma5            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma6            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragmafault        OTHER          pass      1 files
prefixes           OTHER          fail      1 files — --- FAIL: Test_prefixes (0.00s)
    prefixes_test.go:142:...
printf2            OTHER          pass      1 files
ptrchng            OTHER          fail      1 files — ) FROM t1 WHERE x=4
          
    ptrchng_test.go:118: q...
qrf01              OTHER          fail      1 files — es │ ' abcde' │ │ yes │ 'abcde ' │ │ yes │ ...
qrf02              OTHER          fail      1 files — --- FAIL: Test_qrf02 (0.00s)
    qrf02_test.go:80: result...
qrf03              OTHER          fail      1 files — 28774 28773 28706 1 0 0 0 28773 28706 28770 28685 1 0 0 0...
qrf04              OTHER          pass      1 files
qrf05              OTHER          pass      1 files
qrf06              OTHER          pass      1 files
quick              OTHER          pass      1 files
quickcheck         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
randexpr1          OTHER          fail      1 files — 00]
    randexpr1_test.go:16419: result mismatch
        ...
rdonly             OTHER          fail      1 files — --- FAIL: Test_rdonly (0.00s)
    rdonly_test.go:93: expe...
readonly           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
recover_pkg        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
regexp1            OTHER          fail      1 files — --- FAIL: Test_regexp1 (0.01s)
    regexp1_test.go:152: r...
regexp2            OTHER          pass      1 files
reservebytes       OTHER          pass      1 files, 2 tests skipped
resetdb            OTHER          pass      1 files
resolver01         OTHER          fail      1 files — --- FAIL: Test_resolver01 (0.01s)
    resolver01_test.go:...
round1             OTHER          pass      1 files
rowhash            OTHER          pass      1 files
rtree1             OTHER          fail      1 files — exec/engine_core.go:736 +0x360
github.com/pijalu/frigolit...
rtree2             OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/rtree2 [github.com/...
rtree3             OTHER          fail      1 files — ec/engine_core.go:736 +0x360
github.com/pijalu/frigolite....
rtree4             OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/rtree4 [github.com/...
rtree5             OTHER          pass      1 files
rtree6             OTHER          pass      1 files
rtree7             OTHER          pass      1 files
rtree8             OTHER          fail      1 files — ec/engine_core.go:736 +0x360
github.com/pijalu/frigolite....
rtree9             OTHER          fail      1 files — isk image is malformed
          sql:  INSERT INTO rt VAL...
rtreeA             OTHER          fail      1 files — ec/engine_core.go:736 +0x360
github.com/pijalu/frigolite....
rtreeB             OTHER          pass      1 files
rtreeC             OTHER          fail      1 files — NSERT INTO t1(x) SELECT x+4 FROM t1;   --   8
          I...
rtreeD             OTHER          pass      1 files
rtreeE             OTHER          fail      1 files — T 100+x+5*y, x*3+100, x*3+102, y*3, y*3+2 FROM x, y;
    ...
rtreeF             OTHER          pass      1 files
rtreeG             OTHER          pass      1 files
rtreeH             OTHER          fail      1 files — ox-49,49]
    rtreeH_test.go:219: result mismatch
       ...
rtreeI             OTHER          pass      1 files
rtreeJ             OTHER          fail      1 files — 2.0]
    rtreeJ_test.go:533: exec error: UNIQUE constrain...
rtreecheck         OTHER          fail      1 files — --- FAIL: Test_rtreecheck (0.02s)
    rtreecheck_test.go:...
rtreecirc          OTHER          fail      1 files — -- FAIL: Test_rtreecirc (0.01s)
    rtreecirc_test.go:119...
rtreeconnect       OTHER          pass      1 files
rtreedoc           OTHER          fail      1 files — NULL, minX+0.2, maxX+0.2, minY, maxY FROM demo_index;
   ...
rtreedoc2          OTHER          fail      1 files — --- FAIL: Test_rtreedoc2 (0.00s)
    rtreedoc2_test.go:13...
rtreedoc3          OTHER          fail      1 files — --- FAIL: Test_rtreedoc3 (0.04s)
    rtreedoc3_test.go:18...
rtreefuzz001       OTHER          fail      1 files — 98: expected error containing "database disk image is mal...
scanstatus2        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
securedel          OTHER          pass      1 files
securedel2         OTHER          pass      1 files
seekscan1          OTHER          pass      1 files
shell1             OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/shell1 [github.com/...
shell2             OTHER          pass      1 files
shell3             OTHER          pass      1 files
shell4             OTHER          pass      1 files
shell5             OTHER          pass      1 files
shell6             OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/shell6 [github.com/...
shell7             OTHER          pass      1 files
shell8             OTHER          pass      1 files
shell9             OTHER          pass      1 files
shellA             OTHER          skipped   1 files, 1 whole-file skip (CLI shell subprocess harness N-A)
shellB             OTHER          pass      1 files
shortread1         OTHER          fail      1 files — --- FAIL: Test_shortread1 (0.00s)
    shortread1_test.go:...
shrink             OTHER          pass      1 files
sidedelete         OTHER          pass      1 files
skipscan1          OTHER          skipped   1 files, 1 whole-file skip (OR-with-skip-scan planner branch N-A (skipscan1-8.1eqp); ...)
skipscan2          OTHER          pass      1 files
skipscan3          OTHER          pass      1 files
skipscan5          OTHER          pass      1 files
skipscan6          OTHER          pass      1 files
soak               OTHER          pass      1 files
softheap1          OTHER          pass      1 files
speed1             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
speed1p            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
speed2             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
speed3             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
speed4             OTHER          skipped   1 files, 1 whole-file skip (execution-speed benchmark N-A)
speed4p            OTHER          pass      1 files
sqldiff1           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
sqllimits1         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
starschema1        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
strict1            OTHER          pass      1 files
strict2            OTHER          fail      1 files — [non-BLOB value in t1.e]
    strict2_test.go:334: result ...
subtype1           OTHER          skipped   1 files, 1 whole-file skip (value-subtype API (C-extension) not implemented)
symlink            OTHER          skipped   1 files, 1 whole-file skip (VFS-layer symlink + -nofollow + PATH_MAX truncation N-A (...)
symlink2           OTHER          skipped   1 files, 1 whole-file skip (VFS-layer symlink resolution N-A (evidence frigolite_syml...)
sync               OTHER          pass      1 files
sync2              OTHER          pass      1 files
syscall            OTHER          pass      1 files
sysfault           OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
tabfunc01          OTHER          pass      1 files
table              OTHER          skipped   1 files, 1 whole-file skip (database-table-is-locked callback + statement-rollback no...)
tclsqlite          OTHER          skipped   1 files, 1 whole-file skip (TCL binding tests N-A (TCL API))
tempdb2            OTHER          fail      1 files, 2 tests skipped — --- FAIL: Test_tempdb2 (0.01s)
    tempdb2_test.go:102: e...
tempfault          OTHER          pass      1 files
temptable2         OTHER          skipped   1 files, 1 whole-file skip (PRAGMA page_count / mmap_size / backup harness N-A (tempt...)
temptable3         OTHER          pass      1 files
thread001          OTHER          pass      1 files
thread002          OTHER          pass      1 files
thread003          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/thread003 [github.c...
thread004          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/thread004 [github.c...
thread005          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/thread005 [github.c...
thread1            OTHER          pass      1 files
thread2            OTHER          pass      1 files
thread3            OTHER          pass      1 files
timediff1          OTHER          pass      1 files
tkt1435            OTHER          pass      1 files
tkt1443            OTHER          pass      1 files
tkt1444            OTHER          pass      1 files
tkt1449            OTHER          pass      1 files
tkt1473            OTHER          pass      1 files
tkt1501            OTHER          pass      1 files
tkt1512            OTHER          fail      1 files — --- FAIL: Test_tkt1512 (0.00s)
    tkt1512_test.go:88: re...
tkt1514            OTHER          fail      1 files — --- FAIL: Test_tkt1514 (0.00s)
    tkt1514_test.go:62: ex...
tkt1536            OTHER          pass      1 files
tkt1537            OTHER          pass      1 files
tkt1567            OTHER          pass      1 files
tkt1644            OTHER          pass      1 files
tkt1667            OTHER          pass      1 files
tkt1873            OTHER          pass      1 files, 1 tests skipped
tkt2141            OTHER          pass      1 files
tkt2192            OTHER          pass      1 files
tkt2213            OTHER          pass      1 files
tkt2251            OTHER          pass      1 files
tkt2285            OTHER          pass      1 files
tkt2332            OTHER          fail      1 files — gen/tkt2332.blobPuts(0x731c086522d0?, 0x0, {0x731c08af600...
tkt2339            OTHER          pass      1 files
tkt2391            OTHER          pass      1 files
tkt2409            OTHER          skipped   1 files, 1 whole-file skip (cache-spill lock-failure simulation (read_lock_db harness...)
tkt2450            OTHER          pass      1 files
tkt2565            OTHER          fail      1 files — pager: truncate: truncate test.db: file already closed
  ...
tkt2640            OTHER          pass      1 files
tkt2643            OTHER          pass      1 files
tkt2686            OTHER          skipped   1 files, 1 whole-file skip (PRAGMA max_page_count not enforced (database or disk is f...)
tkt2767            OTHER          pass      1 files
tkt2817            OTHER          pass      1 files
tkt2820            OTHER          pass      1 files
tkt2822            OTHER          fail      1 files — t ORDER BY term out of range - should be between 1 and 25...
tkt2832            OTHER          pass      1 files
tkt2854            OTHER          skipped   1 files, 1 whole-file skip (shared-cache multi-connection concurrency not implemented...)
tkt2920            OTHER          fail      1 files — --- FAIL: Test_tkt2920 (0.01s)
    tkt2920_test.go:134: e...
tkt2927            OTHER          pass      1 files
tkt2942            OTHER          pass      1 files
tkt3080            OTHER          skipped   1 files, 1 whole-file skip (test-harness execsql UDF (runs SQL from within a query) n...)
tkt3093            OTHER          skipped   1 files, 1 whole-file skip (multi-connection busy-handler locking not implemented DEF...)
tkt3121            OTHER          fail      1 files — --- FAIL: Test_tkt3121 (0.00s)
    tkt3121_test.go:70: qu...
tkt3201            OTHER          pass      1 files
tkt3292            OTHER          pass      1 files
tkt3298            OTHER          fail      1 files — --- FAIL: Test_tkt3298 (0.01s)
    tkt3298_test.go:69: qu...
tkt3334            OTHER          pass      1 files
tkt3346            OTHER          pass      1 files
tkt3357            OTHER          pass      1 files
tkt3363            OTHER          fail      1 files — --- FAIL: Test_tkt3363 (0.04s)
    tkt3363_test.go:84: ex...
tkt3419            OTHER          pass      1 files
tkt3424            OTHER          pass      1 files
tkt3442            OTHER          pass      1 files
tkt3457            OTHER          pass      1 files
tkt3461            OTHER          pass      1 files
tkt3493            OTHER          pass      1 files
tkt3508            OTHER          fail      1 files — SUBSTRATE_ISOFORM_ID VARCHAR(80),
              SUBSTRATE...
tkt3522            OTHER          pass      1 files
tkt3527            OTHER          pass      1 files
tkt3541            OTHER          pass      1 files
tkt3554            OTHER          pass      1 files
tkt3581            OTHER          pass      1 files
tkt35xx            OTHER          fail      1 files — --- FAIL: Test_tkt35xx (0.01s)
    tkt35xx_test.go:100: e...
tkt3630            OTHER          pass      1 files
tkt3718            OTHER          skipped   1 files, 1 whole-file skip (test-harness SQL-executing UDFs f1/f2 not implemented N-A)
tkt3731            OTHER          pass      1 files
tkt3757            OTHER          pass      1 files
tkt3761            OTHER          pass      1 files
tkt3762            OTHER          pass      1 files
tkt3773            OTHER          pass      1 files
tkt3791            OTHER          pass      1 files
tkt3793            OTHER          skipped   1 files, 1 whole-file skip (shared-cache multi-connection concurrency not implemented...)
tkt3810            OTHER          skipped   1 files, 1 whole-file skip (multi-connection schema staleness not implemented DEFERRED)
tkt3824            OTHER          pass      1 files
tkt3832            OTHER          pass      1 files
tkt3838            OTHER          pass      1 files
tkt3841            OTHER          pass      1 files
tkt3871            OTHER          pass      1 files
tkt3879            OTHER          pass      1 files
tkt3911            OTHER          pass      1 files
tkt3918            OTHER          pass      1 files
tkt3922            OTHER          pass      1 files
tkt3929            OTHER          fail      1 files — ngine_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB...
tkt3935            OTHER          fail      1 files — equired before ON", got: <nil>
          sql:  SELECT a F...
tkt3992            OTHER          fail      1 files — --- FAIL: Test_tkt3992 (0.01s)
    tkt3992_test.go:101: r...
tkt3997            OTHER          pass      1 files
tkt4018            OTHER          pass      1 files
tkt_02a8e81d44     OTHER          pass      1 files
tkt_18458b1a       OTHER          pass      1 files
tkt_26ff0c2d1e     OTHER          pass      1 files
tkt_2a5629202f     OTHER          fail      1 files — --- FAIL: Test_tkt_2a5629202f (0.00s)
    tkt-2a5629202f_...
tkt_2d1a5c67d      OTHER          pass      1 files
tkt_2ea2425d34     OTHER          pass      1 files
tkt_31338dca7e     OTHER          pass      1 files
tkt_313723c356     OTHER          pass      1 files
tkt_385a5b56b9     OTHER          pass      1 files
tkt_38cb5df375     OTHER          pass      1 files
tkt_3998683a16     OTHER          pass      1 files
tkt_3a77c9714e     OTHER          pass      1 files
tkt_3fe897352e     OTHER          skipped   1 files, 1 whole-file skip (UTF-16 hex test-harness functions N-A)
tkt_4a03edc4c8     OTHER          fail      1 files — --- FAIL: Test_tkt_4a03edc4c8 (0.00s)
    tkt-4a03edc4c8_...
tkt_4c86b126f2     OTHER          pass      1 files
tkt_4dd95f6943     OTHER          pass      1 files
tkt_4ef7e3cfca     OTHER          pass      1 files
tkt_54844eea3f     OTHER          fail      1 files — --- FAIL: Test_tkt_54844eea3f (0.01s)
    tkt-54844eea3f_...
tkt_5d863f876e     OTHER          pass      1 files
tkt_5e10420e8d     OTHER          fail      1 files — --- FAIL: Test_tkt_5e10420e8d (0.02s)
    tkt-5e10420e8d_...
tkt_5ee23731f      OTHER          pass      1 files
tkt_6bfb98dfc0     OTHER          pass      1 files
tkt_752e1646fc     OTHER          pass      1 files
tkt_78e04e52ea     OTHER          fail      1 files, 1 tests skipped — --- FAIL: Test_tkt_78e04e52ea (0.00s)
    tkt-78e04e52ea_...
tkt_7a31705a7e6    OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
tkt_7bbfb7d442     OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
tkt_80ba201079     OTHER          fail      1 files — --- FAIL: Test_tkt_80ba201079 (0.01s)
    tkt-80ba201079_...
tkt_80e031a00f     OTHER          pass      1 files
tkt_8454a207b9     OTHER          pass      1 files
tkt_868145d012     OTHER          pass      1 files
tkt_8c63ff0ec      OTHER          pass      1 files
tkt_91e2e8ba6f     OTHER          pass      1 files
tkt_99378177930f87bd OTHER          skipped   1 files, 1 whole-file skip (JSON operators (->>) not implemented N-A)
tkt_9a8b09f8e6     OTHER          pass      1 files
tkt_9d68c883       OTHER          skipped   1 files, 1 whole-file skip (custom VFS device simulation + OOM fault injection N-A)
tkt_9f2eb3abac     OTHER          skipped   1 files, 1 whole-file skip (faultsim OOM/injection tests N-A)
tkt_a7b7803e       OTHER          pass      1 files
tkt_a7debbe0       OTHER          pass      1 files
tkt_a8a0d2996a     OTHER          fail      1 files — --- FAIL: Test_tkt_a8a0d2996a (0.00s)
    tkt-a8a0d2996a_...
tkt_b1d3a2e531     OTHER          pass      1 files
tkt_b351d95f9      OTHER          pass      1 files
tkt_b72787b1       OTHER          pass      1 files
tkt_b75a9ca6b0     OTHER          pass      1 files
tkt_ba7cbfaedc     OTHER          pass      1 files
tkt_bd484a090c     OTHER          fail      1 files — --- FAIL: Test_tkt_bd484a090c (0.00s)
    tkt-bd484a090c_...
tkt_bdc6bbbb38     OTHER          skipped   1 files, 1 whole-file skip (FTS4 virtual table not implemented N-A)
tkt_c48d99d690     OTHER          pass      1 files
tkt_c694113d5      OTHER          pass      1 files
tkt_cbd054fa6b     OTHER          pass      1 files
tkt_d11f09d36e     OTHER          fail      1 files — om/pijalu/frigolite.(*DB).Exec(0x1bd8e12502d0, {0x1047af4...
tkt_d635236375     OTHER          pass      1 files
tkt_d82e3f3721     OTHER          pass      1 files, 3 tests skipped
tkt_f3e5abed55     OTHER          skipped   1 files, 1 whole-file skip (testvfs custom VFS + multi-connection ATTACH N-A/DEFERRED)
tkt_f67b41381a     OTHER          skipped   1 files, 1 whole-file skip (EXPLAIN VDBE opcode inspection N-A)
tkt_f777251dc7a    OTHER          fail      1 files — --- FAIL: Test_tkt_f777251dc7a (0.00s)
    tkt-f777251dc7...
tkt_f7b4edec       OTHER          pass      1 files
tkt_f973c7ac31     OTHER          pass      1 files
tkt_fa7bf5ec       OTHER          pass      1 files
tkt_fc62af4523     OTHER          pass      1 files
tkt_fc7bd6358f     OTHER          pass      1 files
tokenize           OTHER          fail      1 files — ror containing "unrecognized token: \"1.0E\"", got: frigo...
tpch01             OTHER          fail      1 files — art USING INDEX bootleg_pti (p_type=? AND r_name=?) |--SE...
trace2             OTHER          pass      1 files
trace3             OTHER          fail      1 files — do_test trace3-6.1
    trace3_test.go:327: result mismatc...
trustschema1       OTHER          fail      1 files, 2 tests skipped — ismatch
          got:  [{}]
          want: [2]
    trus...
types2             OTHER          pass      1 files
types3             OTHER          pass      1 files
unique2            OTHER          pass      1 files
uri                OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
uri2               OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
utf16align         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
varint             OTHER          pass      1 files
veryquick          OTHER          pass      1 files
widetab1           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
win32heap          OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
win32lock          OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
win32longpath      OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
win32nolock        OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
writecrash         OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
zerodamage         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze            PLANNER        pass      1 files
analyze3           PLANNER        pass      1 files, 6 tests skipped
analyze4           PLANNER        pass      1 files
analyze5           PLANNER        pass      1 files
analyze6           PLANNER        pass      1 files
analyze7           PLANNER        pass      1 files
analyze8           PLANNER        pass      1 files
analyze9           PLANNER        pass      1 files
analyzeC           PLANNER        pass      1 files, 6 tests skipped
analyzeD           PLANNER        pass      1 files
analyzeE           PLANNER        pass      1 files
analyzeF           PLANNER        pass      1 files
analyzeG           PLANNER        pass      1 files
analyzer1          PLANNER        pass      1 files
bestindex1         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex2         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex3         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex4         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex5         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex6         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex7         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex8         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindex9         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexA         PLANNER        fail      1 files — --- FAIL: Test_bestindexA (0.00s)
    bestindexA_test.go:...
bestindexB         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexC         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexD         PLANNER        fail      1 files — --- FAIL: Test_bestindexD (0.00s)
    bestindexD_test.go:...
bestindexE         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexF         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexG         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
cost               PLANNER        pass      1 files
cursorhint         PLANNER        skipped   1 files, 1 whole-file skip (VDBE codeCursorHint() opcode P4 introspection + MySQL pus...)
eqp                PLANNER        pass      1 files
pushdown           PLANNER        skipped   1 files, 1 whole-file skip (VDBE codeCursorHint() opcode P4 introspection + MySQL pus...)
scanstatus         PLANNER        skipped   1 files, 1 whole-file skip (sqlite3_stmt_scanstatus/sqlite3_db_scanstatus C-API intro...)
stat               PLANNER        pass      1 files, 2 tests skipped
statfault          PLANNER        pass      1 files
trace              PLANNER        fail      1 files — ]
          want: [SELECT '$::t6int', [$::t6int], 6, 6, "...
rtree              RTREE          pass      1 files
alter              SCHEMA         fail      1 files, 9 tests skipped — column: id
          sql: 
            CREATE TABLE t1(a ...
alter2             SCHEMA         skipped   1 files, 1 whole-file skip (legacy file-format short-row tests (hexio helpers) not im...)
alter3             SCHEMA         fail      1 files — error containing "Cannot add a NOT NULL column with defau...
alter4             SCHEMA         fail      1 files — ss, got error: duplicate column name: "c"
          sql: ...
alterauth          SCHEMA         fail      1 files — .go:123: result mismatch
          got:  [{}]
          w...
alterauth2         SCHEMA         pass      1 files
altercol           SCHEMA         pass      1 files, 2 tests skipped
altercons          SCHEMA         fail      1 files, 13 tests skipped — --- FAIL: Test_altercons (0.03s)
    altercons_test.go:18...
altercons2         SCHEMA         pass      1 files, 12 tests skipped
altercons3         SCHEMA         pass      1 files, 1 tests skipped
altercorrupt       SCHEMA         pass      1 files
alterdropcol       SCHEMA         fail      1 files — --- FAIL: Test_alterdropcol (58.84s)
    alterdropcol_tes...
alterdropcol2      SCHEMA         pass      1 files
alterfault         SCHEMA         pass      1 files
alterlegacy        SCHEMA         fail      1 files, 15 tests skipped — CREATE TABLE aux.p1(a INTEGER PRIMARY KEY, b);
          ...
altermalloc        SCHEMA         pass      1 files
altermalloc2       SCHEMA         pass      1 files
altermalloc3       SCHEMA         pass      1 files
alterqf            SCHEMA         fail      1 files, 1 tests skipped — string_agg("b", ',') OVER (ORDER BY c||'str');
          ...
altertab           SCHEMA         fail      1 files, 53 tests skipped — n;
          CREATE TABLE aux.p1(a INTEGER PRIMARY KEY, b...
altertab2          SCHEMA         fail      1 files, 3 tests skipped — ELECT col1 FROM "newname")
                SELECT x FROM ...
altertab3          SCHEMA         fail      1 files, 14 tests skipped — :736 +0x360
github.com/pijalu/frigolite.(*DB).Exec(0x5dca...
altertrig          SCHEMA         pass      1 files
attach             SCHEMA         fail      1 files — ATE TRIGGER r5 AFTER INSERT ON t5 BEGIN
                D...
attach2            SCHEMA         fail      1 files — t commit - no transaction is active", got: <nil>
        ...
attach3            SCHEMA         pass      1 files
attach4            SCHEMA         fail      1 files — --- FAIL: Test_attach4 (0.01s)
    attach4_test.go:163: r...
attachmalloc       SCHEMA         pass      1 files
autoinc            SCHEMA         fail      1 files — RIGGER t3928r3 BEFORE UPDATE ON t3928 
                WH...
autovacuum         SCHEMA         fail      1 files — 431 432 433 434 435 436 437 438 439 440 441 442 443 444 4...
autovacuum2        SCHEMA         pass      1 files, 5 tests skipped
autovacuum_ioerr2  SCHEMA         pass      1 files
check              SCHEMA         fail      1 files — ernal/exec/engine_core.go:736 +0x360
github.com/pijalu/fr...
checkfault         SCHEMA         pass      1 files
collate1           SCHEMA         pass      1 files
collate2           SCHEMA         pass      1 files
collate3           SCHEMA         fail      1 files — t:  [0]
          want: [1]
          body: do_test colla...
collate4           SCHEMA         fail      1 files — ne_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).E...
collate5           SCHEMA         pass      1 files
collate6           SCHEMA         pass      1 files
collate7           SCHEMA         fail      1 files — --- FAIL: Test_collate7 (0.00s)
    collate7_test.go:105:...
collate8           SCHEMA         pass      1 files
collate9           SCHEMA         pass      1 files
collateA           SCHEMA         pass      1 files
collateB           SCHEMA         pass      1 files, 1 tests skipped
conflict           SCHEMA         fail      1 files — constraint failed: t5.a", got: <nil>
          sql: 
    ...
coveridxscan       SCHEMA         pass      1 files, 4 tests skipped
createtab          SCHEMA         pass      1 files, 1 tests skipped
fkey1              SCHEMA         fail      1 files, 2 tests skipped — 1_test.go:185: exec error: FOREIGN KEY constraint failed
...
fkey2              SCHEMA         fail      1 files — IGN KEY(x,y) REFERENCES tce73(a,b));
            INSERT I...
fkey3              SCHEMA         fail      1 files — a'); 
    fkey3_test.go:174: exec error: FOREIGN KEY cons...
fkey4              SCHEMA         pass      1 files
fkey5              SCHEMA         fail      1 files, 13 tests skipped — st.go:221: result mismatch
          got:  [{}]
         ...
fkey6              SCHEMA         fail      1 files — INTO c1 VALUES(123);
            PRAGMA defer_foreign_key...
fkey7              SCHEMA         pass      1 files
fkey8              SCHEMA         pass      1 files
fkey_malloc        SCHEMA         pass      1 files
index              SCHEMA         fail      1 files — ernal/exec/engine_core.go:736 +0x360
github.com/pijalu/fr...
index2             SCHEMA         fail      1 files — # github.com/pijalu/frigolite/testgen/index2 [github.com/...
index3             SCHEMA         pass      1 files, 1 tests skipped
index4             SCHEMA         fail      1 files — xec/engine_core.go:736 +0x360
github.com/pijalu/frigolite...
index5             SCHEMA         fail      1 files — /exec/engine_core.go:736 +0x360
github.com/pijalu/frigoli...
index6             SCHEMA         fail      1 files, 5 tests skipped — used Page 23: never used Page 24: never used Page 25: nev...
index7             SCHEMA         fail      1 files, 5 tests skipped — exists
          sql: 
            CREATE INDEX bad1 ON t...
index8             SCHEMA         pass      1 files, 1 tests skipped
index9             SCHEMA         pass      1 files
indexA             SCHEMA         pass      1 files, 4 tests skipped
indexedby          SCHEMA         fail      1 files, 2 tests skipped — .go:736 +0x360
github.com/pijalu/frigolite.(*DB).Query(0x...
indexexpr1         SCHEMA         fail      1 files, 40 tests skipped — --- FAIL: Test_indexexpr1 (0.12s)
    indexexpr1_test.go:...
indexexpr2         SCHEMA         pass      1 files, 12 tests skipped
indexexpr3         SCHEMA         pass      1 files
indexfault         SCHEMA         pass      1 files
notnull            SCHEMA         fail      1 files — _test.go:461: expected success, got error: NOT NULL const...
notnull2           SCHEMA         pass      1 files
notnullfault       SCHEMA         pass      1 files
reindex            SCHEMA         fail      1 files — --- FAIL: Test_reindex (0.01s)
    reindex_test.go:114: e...
savepoint          SCHEMA         fail      1 files — {}]
          want: [SQLITE_SAVEPOINT BEGIN sp1 {} {}]
  ...
savepoint2         SCHEMA         fail      1 files — :736 +0x360
github.com/pijalu/frigolite.(*DB).Exec(0x60f5...
savepoint4         SCHEMA         skipped   1 files, 1 whole-file skip (crashsql crash-simulation while loop not transpilable N-A)
savepoint5         SCHEMA         pass      1 files
savepoint6         SCHEMA         skipped   1 files, 1 whole-file skip (dynamic TCL proc harness (eval/insert_rows/random_integer...)
savepoint7         SCHEMA         pass      1 files
savepointfault     SCHEMA         pass      1 files
schema             SCHEMA         pass      1 files
schema2            SCHEMA         pass      1 files
schema3            SCHEMA         pass      1 files
schema4            SCHEMA         fail      1 files — engine_core.go:736 +0x360
github.com/pijalu/frigolite.(*D...
schema5            SCHEMA         pass      1 files
schema6            SCHEMA         pass      1 files
schemafault        SCHEMA         pass      1 files
temptrigger        SCHEMA         pass      1 files
trans              SCHEMA         fail      1 files — # github.com/pijalu/frigolite/testgen/trans [github.com/p...
trans2             SCHEMA         fail      1 files — # github.com/pijalu/frigolite/testgen/trans2 [github.com/...
trans3             SCHEMA         pass      1 files
transitive1        SCHEMA         pass      1 files
trigger1           SCHEMA         fail      1 files — WHERE b IN (t1.a,127,t1.b)
                              ...
trigger2           SCHEMA         fail      1 files — ngine_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB...
trigger3           SCHEMA         pass      1 files
trigger4           SCHEMA         fail      1 files — ne_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).E...
trigger5           SCHEMA         pass      1 files
trigger6           SCHEMA         pass      1 files
trigger7           SCHEMA         fail      1 files — --- FAIL: Test_trigger7 (0.00s)
    trigger7_test.go:74: ...
trigger8           SCHEMA         pass      1 files
trigger9           SCHEMA         fail      1 files — e_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Qu...
triggerA           SCHEMA         pass      1 files
triggerB           SCHEMA         fail      1 files — o such column: wen.x", got: UNIQUE constraint failed: x.x...
triggerC           SCHEMA         skipped   1 files, 1 whole-file skip (recursive trigger cascade causes hang (deep-engine applic...)
triggerD           SCHEMA         fail      1 files — e_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Ex...
triggerE           SCHEMA         pass      1 files
triggerF           SCHEMA         pass      1 files
triggerG           SCHEMA         pass      1 files
triggerupfrom      SCHEMA         fail      1 files — --- FAIL: Test_triggerupfrom (0.00s)
    triggerupfrom_te...
unique             SCHEMA         pass      1 files
vacuum             SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum2            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum3            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum4            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum5            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum6            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum_into        SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuummem          SCHEMA         fail      1 files, 1 tests skipped — exec/src/testing/testing.go:2126 +0x2c8
panic({0x1008e629...
rbu                SESSION        pass      1 files
session            SESSION        pass      1 files
amatch1            VTAB           pass      1 files
carray01           VTAB           pass      1 files
carray02           VTAB           pass      1 files
carrayfault        VTAB           pass      1 files
dbpage             VTAB           fail      1 files, 4 tests skipped — --- FAIL: Test_dbpage (0.01s)
    dbpage_test.go:241: exe...
dbpagefault        VTAB           pass      1 files
intarray           VTAB           fail      1 files — _core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Que...
quota              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
quota2             VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
quota_glob         VTAB           skipped   1 files, 1 whole-file skip (quota VFS extension not implemented N-A)
spellfix           VTAB           fail      1 files — e_core.go:736 +0x360
github.com/pijalu/frigolite.(*DB).Ex...
spellfix2          VTAB           pass      1 files
spellfix3          VTAB           pass      1 files
spellfix4          VTAB           pass      1 files
swarmvtab          VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_swarm_contract_te...)
swarmvtab2         VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_swarmvtab2_test.go))
swarmvtab3         VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_swarmvtab3_test.go))
swarmvtabfault     VTAB           pass      1 files
unionvtab          VTAB           pass      1 files
unionvtabfault     VTAB           skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
vtab1              VTAB           fail      1 files, 26 tests skipped — # github.com/pijalu/frigolite/testgen/vtab1 [github.com/p...
vtab2              VTAB           pass      1 files, 15 tests skipped
vtab3              VTAB           fail      1 files — --- FAIL: Test_vtab3 (0.00s)
    vtab3_test.go:124: resul...
vtab4              VTAB           pass      1 files
vtab5              VTAB           fail      1 files — BEGIN
                SELECT 1, 2, 3;
              END;
...
vtab6              VTAB           fail      1 files — a, b, c FROM ab NATURAL JOIN bc;
          
    vtab6_tes...
vtab7              VTAB           skipped   1 files, 1 whole-file skip (echo module xSync callback trace (C test-module ABI) not ...)
vtab8              VTAB           pass      1 files
vtab9              VTAB           pass      1 files
vtabA              VTAB           pass      1 files
vtabB              VTAB           pass      1 files
vtabC              VTAB           pass      1 files
vtabD              VTAB           fail      1 files — al/exec/engine_core.go:736 +0x360
github.com/pijalu/frigo...
vtabE              VTAB           pass      1 files
vtabF              VTAB           pass      1 files
vtabH              VTAB           pass      1 files
vtabI              VTAB           pass      1 files
vtabJ              VTAB           pass      1 files, 3 tests skipped
vtabK              VTAB           fail      1 files — --- FAIL: Test_vtabK (0.01s)
    vtabK_test.go:140: resul...
vtabL              VTAB           pass      1 files
vtab_alter         VTAB           pass      1 files, 7 tests skipped
vtab_err           VTAB           pass      1 files
vtab_shared        VTAB           fail      1 files, 26 tests skipped — b_shared_test.go:103: expected error containing "no such ...
vtabdistinct       VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_vtabdistinct_test...)
vtabdrop           VTAB           pass      1 files, 6 tests skipped
vtabrhs1           VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_vtabrhs1_test.go))
zipfile            VTAB           pass      1 files
zipfile2           VTAB           pass      1 files
zipfilefault       VTAB           pass      1 files
jrnlmode           WAL            pass      1 files
mjournal           WAL            skipped   1 files, 1 whole-file skip (master-journal pointer validation in hot-journal recovery...)
nockpt             WAL            pass      1 files
rollback           WAL            skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rollback2          WAL            skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rollbackfault      WAL            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
subjournal         WAL            fail      1 files — --- FAIL: Test_subjournal (0.07s)
    subjournal_test.go:...
wal                WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal2               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal3               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal4               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal5               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal6               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal64k             WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal7               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal8               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
wal9               WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
walbak             WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
walbig             WAL            fail      1 files — --- FAIL: Test_walbig (0.00s)
    walbig_test.go:88: exec...
walblock           WAL            pass      1 files
walckptnoop        WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
walcksum           WAL            skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
walcrash           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash2          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash3          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash4          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walfault           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walfault2          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walhook            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walmode            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walnoshm           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
waloverwrite       WAL            fail      1 files — SELECT x FROM t1
    waloverwrite_test.go:123: query erro...
walpersist         WAL            fail      1 files — _test.go:185: result mismatch
          got:  [8416]
    ...
walprotocol        WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walprotocol2       WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walrestart         WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walro              WAL            pass      1 files
walro2             WAL            pass      1 files
walrofault         WAL            pass      1 files
walseh1            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walsetlk           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walsetlk2          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walsetlk3          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walsetlk_recover   WAL            fail      1 files — --- FAIL: Test_walsetlk_recover (0.00s)
    walsetlk_reco...
walsetlk_snapshot  WAL            fail      1 files — --- FAIL: Test_walsetlk_snapshot (0.01s)
    walsetlk_sna...
walshared          WAL            fail      1 files — --- FAIL: Test_walshared (0.00s)
    walshared_test.go:85...
walslow            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walthread          WAL            pass      1 files
walvfs             WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
