Frigolite testgen status  (generated 2026-09-09T16:50:56Z)

FAMILY              TOTAL   PASS   FAIL   SKIP     PCT
----------------------------------------------------------
AGG                     7      3      4      0   42.9%
C-API                  48     36      2     10   75.0%
CONCURRENCY            39     10      2     27   25.6%
CRUD                   59     36     23      0   61.0%
CTE-WINDOW             34     25      9      0   73.5%
EXPR                   43     36      5      2   83.7%
FTS                    96     56     20     20   58.3%
FUNCTIONS              25     18      7      0   72.0%
JOIN                   37     24      6      7   64.9%
JSON                   12     10      1      1   83.3%
ORDER                  26     20      5      1   76.9%
OTHER                 507    315     79    113   62.1%
PLANNER                38     18      3     17   47.4%
RTREE                  27     11     16      0   40.7%
SCHEMA                121     79     37      5   65.3%
SESSION                 2      2      0      0  100.0%
VTAB                   49     35      7      7   71.4%
WAL                    49      9      7     33   18.4%
----------------------------------------------------------
TOTAL                1219    743    233    243   61.0%

PACKAGES
PKG                FAMILY         STATE     DETAIL
--------------------------------------------------------------------------------
aggerror           AGG            fail      1 files — T 7;
            SELECT x_count(*) FROM t1;
          
  ...
aggfault           AGG            pass      1 files
aggnested          AGG            fail      1 files, 4 tests skipped — --- FAIL: Test_aggnested (0.02s)
    aggnested_test.go:74...
aggorderby         AGG            fail      1 files, 6 tests skipped — --- FAIL: Test_aggorderby (1.73s)
    aggorderby_test.go:...
count              AGG            fail      1 files — --- FAIL: Test_count (2.61s)
    count_test.go:301: expec...
countofview        AGG            pass      1 files
having             AGG            pass      1 files
backup             C-API          pass      1 files
backup2            C-API          pass      1 files
backup4            C-API          pass      1 files
backup5            C-API          pass      1 files
backup_ioerr       C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
backup_malloc      C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
bind               C-API          fail      1 files — got:  [$::two]
          want: []
          body: do_test...
bind2              C-API          pass      1 files
bindxfer           C-API          skipped   1 files, 1 whole-file skip (sqlite3_transfer_bindings deprecated prepared-statement V...)
blob               C-API          pass      1 files
capi2              C-API          pass      1 files, 1 tests skipped
capi3              C-API          pass      1 files, 7 tests skipped
capi3b             C-API          pass      1 files
capi3c             C-API          pass      1 files, 6 tests skipped
capi3d             C-API          pass      1 files
capi3e             C-API          pass      1 files
changes            C-API          pass      1 files
changes2           C-API          pass      1 files
colmeta            C-API          pass      1 files
dbstatus           C-API          pass      1 files
exec               C-API          pass      1 files
hook               C-API          pass      1 files, 41 tests skipped
hook2              C-API          pass      1 files
imposter1          C-API          skipped   1 files, 1 whole-file skip (sqlite3_test_control(SQLITE_TESTCTRL_IMPOSTER) test-only ...)
incrblob           C-API          skipped   1 files, 1 whole-file skip (incremental-blob TCL channel + SQL/constraint paths not r...)
incrblob2          C-API          skipped   1 files, 1 whole-file skip (incremental-blob TCL channel + UNIQUE-constraint INSERT.....)
incrblob3          C-API          pass      1 files
incrblob4          C-API          skipped   1 files, 1 whole-file skip (incremental-blob TCL channel + blob-handle count assertio...)
incrblob_err       C-API          pass      1 files
incrblobfault      C-API          pass      1 files
incrcorrupt        C-API          pass      1 files
incrvacuum         C-API          pass      1 files
incrvacuum2        C-API          pass      1 files
incrvacuum3        C-API          pass      2 files
incrvacuum_ioerr   C-API          pass      1 files
interrupt          C-API          pass      1 files
interrupt2         C-API          pass      1 files
lastinsert         C-API          pass      1 files
laststmtchanges    C-API          pass      1 files
notify1            C-API          pass      1 files
notify2            C-API          fail      1 files — # github.com/pijalu/frigolite/testgen/notify2 [github.com...
notify3            C-API          pass      1 files
progress           C-API          skipped   1 files, 1 whole-file skip (dynamic TCL progress callback procedure harness N-A)
sqllog             C-API          skipped   1 files, 1 whole-file skip (SQLite test_sqllog.c extension / VFS SQL logger C-runtime...)
stmt               C-API          pass      1 files
stmtrand           C-API          pass      1 files
stmtvtab1          C-API          skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_stmtvtab1_test.go))
tableapi           C-API          pass      1 files
busy               CONCURRENCY    skipped   1 files, 1 whole-file skip (busy-handler (sqlite3_busy_handler C-API; `db busy` trans...)
busy2              CONCURRENCY    skipped   1 files, 1 whole-file skip (busy-handler (sqlite3_busy_handler C-API; `db busy` trans...)
exclusive          CONCURRENCY    pass      1 files
lock               CONCURRENCY    fail      1 files — lock_test.go:460: result mismatch
          got:  [1 data...
lock2              CONCURRENCY    pass      1 files
lock3              CONCURRENCY    pass      1 files
lock4              CONCURRENCY    skipped   1 files, 1 whole-file skip (two-process fixture emulation (test2-script.tcl subproces...)
lock5              CONCURRENCY    fail      1 files — --- FAIL: Test_lock5 (0.27s)
    lock5_test.go:378: query...
lock6              CONCURRENCY    pass      1 files
lock7              CONCURRENCY    pass      1 files
manydb             CONCURRENCY    skipped   1 files, 1 whole-file skip (TCL `file channels`/`ulimit` file-descriptor leak harness...)
multiplex          CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
multiplex2         CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
multiplex3         CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
multiplex4         CONCURRENCY    skipped   1 files, 1 whole-file skip (custom multiplex VFS (sqlite3_multiplex_initialize file s...)
nolock             CONCURRENCY    skipped   1 files, 1 whole-file skip (testvfs VFS lock-call counting needs a VFS instrumentatio...)
pendingrace        CONCURRENCY    pass      1 files, 1 tests skipped
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
delete2            CRUD           fail      1 files — --- FAIL: Test_delete2 (0.01s)
    delete2_test.go:168: r...
delete3            CRUD           pass      1 files
delete4            CRUD           fail      1 files — 4;
          CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)...
delete_db          CRUD           pass      1 files
delete_pkg         CRUD           fail      1 files — -4.2
    delete_test.go:754: result mismatch
          go...
emptytable         CRUD           pass      1 files
insert             CRUD           fail      1 files, 1 tests skipped — esult mismatch
          got:  [1 table test1 has 1 value...
insert2            CRUD           pass      1 files
insert3            CRUD           fail      1 files — --- FAIL: Test_insert3 (0.09s)
    insert3_test.go:131: e...
insert4            CRUD           pass      1 files, 6 tests skipped
insert5            CRUD           pass      1 files
insertfault        CRUD           pass      1 files
intpkey            CRUD           pass      1 files, 2 tests skipped
queryonly          CRUD           pass      1 files
returning1         CRUD           pass      1 files
returningfault     CRUD           pass      1 files
rowid              CRUD           fail      1 files, 6 tests skipped — --- FAIL: Test_rowid (0.19s)
    rowid_test.go:211: resul...
select1            CRUD           fail      1 files — test.go:2101: result mismatch
          got:  [{}]
      ...
select2            CRUD           fail      1 files — 8: 6 7 8]
          body: do_test select2-1.1
    select2...
select3            CRUD           fail      1 files — --- FAIL: Test_select3 (0.04s)
    select3_test.go:192: e...
select4            CRUD           pass      1 files, 2 tests skipped
select5            CRUD           fail      1 files — --- FAIL: Test_select5 (0.01s)
    select5_test.go:136: e...
select6            CRUD           pass      1 files
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
selectH            CRUD           fail      1 files — --- FAIL: Test_selectH (0.01s)
    selectH_test.go:108: r...
tableopts          CRUD           fail      1 files — --- FAIL: Test_tableopts (0.00s)
    tableopts_test.go:13...
tempdb             CRUD           pass      1 files
temptable          CRUD           pass      1 files, 6 tests skipped
types              CRUD           pass      1 files
update             CRUD           fail      1 files — tch
          got:  [0 {}]
          want: [1 no such fun...
update2            CRUD           fail      1 files — --- FAIL: Test_update2 (0.03s)
    update2_test.go:318: r...
upfrom1            CRUD           pass      1 files
upfrom2            CRUD           fail      1 files, 1 tests skipped — --- FAIL: Test_upfrom2 (0.01s)
    upfrom2_test.go:192: e...
upfrom3            CRUD           pass      1 files
upfrom4            CRUD           pass      1 files
upfromfault        CRUD           pass      1 files
upsert1            CRUD           fail      1 files — nt: [ok]
    upsert1_test.go:308: query error: database d...
upsert2            CRUD           fail      1 files — --- FAIL: Test_upsert2 (0.00s)
    upsert2_test.go:80: qu...
upsert3            CRUD           fail      1 files — --- FAIL: Test_upsert3 (0.00s)
    upsert3_test.go:122: q...
upsert4            CRUD           fail      1 files — --- FAIL: Test_upsert4 (0.01s)
    upsert4_test.go:135: q...
upsert5            CRUD           fail      1 files — --- FAIL: Test_upsert5 (0.05s)
    upsert5_test.go:93: qu...
upsertfault        CRUD           pass      1 files
values             CRUD           pass      1 files, 1 tests skipped
valuesfault        CRUD           pass      1 files
view               CRUD           fail      1 files, 2 tests skipped — --- FAIL: Test_view (0.03s)
    view_test.go:358: expecte...
view2              CRUD           pass      1 files
view3              CRUD           fail      1 files — VIEW v1024 AS SELECT * FROM v512 UNION SELECT * FROM v512...
filter1            CTE-WINDOW     fail      1 files — --- FAIL: Test_filter1 (0.01s)
    filter1_test.go:391: r...
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
windowB            CTE-WINDOW     fail      1 files — --- FAIL: Test_windowB (0.01s)
    windowB_test.go:170: e...
windowC            CTE-WINDOW     fail      1 files — --- FAIL: Test_windowC (0.02s)
    windowC_test.go:151: r...
windowD            CTE-WINDOW     pass      1 files
windowE            CTE-WINDOW     fail      1 files — 4 0.0 487 0.0 488 0.0 489 0.0 490 0.0 491 0.0 494 0.0 495...
windowerr          CTE-WINDOW     pass      1 files
windowfault        CTE-WINDOW     fail      1 files — --- FAIL: Test_windowfault (0.07s)
    windowfault_test.g...
windowpushd        CTE-WINDOW     pass      1 files
with1              CTE-WINDOW     fail      1 files, 9 tests skipped — --- FAIL: Test_with1 (3.40s)
    with1_test.go:395: expec...
with2              CTE-WINDOW     fail      1 files, 6 tests skipped — ny) * rsy / (maxy-miny)
                WHEN 0 >= maxy TH...
with3              CTE-WINDOW     pass      1 files
with4              CTE-WINDOW     pass      1 files
with5              CTE-WINDOW     pass      1 files
with6              CTE-WINDOW     pass      1 files
withM              CTE-WINDOW     pass      1 files
without_rowid1     CTE-WINDOW     pass      1 files
without_rowid2     CTE-WINDOW     pass      1 files
without_rowid3     CTE-WINDOW     fail      1 files, 10 tests skipped — nt default
          sql:  ALTER TABLE t2 ADD COLUMN g DE...
without_rowid4     CTE-WINDOW     fail      1 files — or containing "UNIQUE constraint failed: tbl.a", got: <ni...
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
expridx1           EXPR           pass      1 files, 6 tests skipped
expridx2           EXPR           pass      1 files
hexlit             EXPR           pass      1 files
in                 EXPR           pass      1 files
istrue             EXPR           pass      1 files
literal            EXPR           pass      1 files
null               EXPR           fail      1 files — t2 values(2,null);
              insert into t2 values(3,...
numcast            EXPR           pass      1 files
where              EXPR           fail      1 files, 9 tests skipped — ALUES(100,int(log100/log2),10201)
    where_test.go:159: ...
where2             EXPR           pass      1 files, 7 tests skipped
where3             EXPR           pass      1 files
where4             EXPR           pass      1 files, 2 tests skipped
where5             EXPR           pass      1 files
where6             EXPR           pass      1 files
where7             EXPR           pass      1 files
where8             EXPR           skipped   1 files, 1 whole-file skip (hash/btree DISTINCT ordering fuzz N-A (where8-4.x SELECT ...)
where9             EXPR           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
whereA             EXPR           fail      1 files, 2 tests skipped — --- FAIL: Test_whereA (0.01s)
    whereA_test.go:204: res...
whereB             EXPR           pass      1 files
whereC             EXPR           pass      1 files
whereD             EXPR           pass      1 files
whereE             EXPR           pass      1 files
whereF             EXPR           pass      1 files, 3 tests skipped
whereG             EXPR           pass      1 files
whereH             EXPR           pass      1 files, 16 tests skipped
whereI             EXPR           pass      1 files
whereJ             EXPR           pass      1 files
whereK             EXPR           pass      1 files
whereL             EXPR           pass      1 files
whereM             EXPR           pass      1 files
whereN             EXPR           pass      1 files
wherefault         EXPR           pass      1 files
wherelfault        EXPR           pass      1 files
wherelimit         EXPR           fail      1 files — ror
          sql: DELETE FROM t1 ORDER BY x
    wherelim...
wherelimit2        EXPR           fail      1 files, 6 tests skipped — [a e b e c e d e e e f e g e h e]
          want: [a a b ...
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
fts3b              FTS            pass      1 files
fts3c              FTS            pass      1 files
fts3comp1          FTS            fail      1 files — smatch
          got:  [{}]
          want: [eight 1 1 fo...
fts3conf           FTS            fail      1 files — --- FAIL: Test_fts3conf (0.03s)
    fts3conf_test.go:331:...
fts3corrupt        FTS            fail      1 files — sql: 
          CREATE VIRTUAL TABLE f using fts3(a,b);
 ...
fts3corrupt2       FTS            pass      1 files
fts3corrupt3       FTS            fail      1 files — --- FAIL: Test_fts3corrupt3 (0.00s)
    fts3corrupt3_test...
fts3corrupt4       FTS            fail      1 files, 52 tests skipped — --- FAIL: Test_fts3corrupt4 (0.05s)
    fts3corrupt4_test...
fts3corrupt5       FTS            pass      1 files
fts3corrupt6       FTS            fail      1 files, 1 tests skipped — nil>
          sql: 
          CREATE VIRTUAL TABLE main....
fts3corrupt7       FTS            pass      1 files
fts3cov            FTS            pass      1 files
fts3d              FTS            pass      1 files
fts3defer          FTS            fail      1 files — --- FAIL: Test_fts3defer (76.75s)
    fts3defer_test.go:2...
fts3defer2         FTS            pass      1 files, 13 tests skipped
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
fts3fuzz001        FTS            fail      1 files — --- FAIL: Test_fts3fuzz001 (0.01s)
    fts3fuzz001_test.g...
fts3integrity      FTS            pass      1 files
fts3join           FTS            fail      1 files — --- FAIL: Test_fts3join (0.01s)
    fts3join_test.go:104:...
fts3malloc         FTS            skipped   1 files, 1 whole-file skip (sqlite3_memdebug_fail OOM-injection C API N-A (malloc fam...)
fts3matchinfo      FTS            pass      1 files
fts3matchinfo2     FTS            pass      1 files
fts3misc           FTS            skipped   1 files, 1 whole-file skip (200-column FTS3 schema row exceeds one page at TEST-defau...)
fts3near           FTS            pass      1 files
fts3offsets        FTS            pass      1 files
fts3prefix         FTS            pass      1 files
fts3prefix2        FTS            pass      1 files
fts3query          FTS            pass      1 files
fts3rank           FTS            pass      1 files
fts3rnd            FTS            skipped   1 files, 1 whole-file skip (randomized stress suite exceeds runtime budget (>600s); d...)
fts3shared         FTS            skipped   1 files, 1 whole-file skip (shared-cache read-during-write locking ('database table i...)
fts3snippet        FTS            pass      1 files
fts3snippet2       FTS            pass      1 files
fts3sort           FTS            pass      1 files
fts3tok1           FTS            fail      1 files — --- FAIL: Test_fts3tok1 (0.00s)
    fts3tok1_test.go:86: ...
fts3tok_err        FTS            pass      1 files
fts3varint         FTS            pass      1 files
fts4aa             FTS            pass      1 files
fts4check          FTS            fail      1 files — INSERT INTO t3(x, y, langid) 
              SELECT x, y, ...
fts4content        FTS            fail      1 files, 7 tests skipped — ning "SQL logic error", got: 't1' is not a function
     ...
fts4docid          FTS            pass      1 files
fts4growth         FTS            fail      1 files — 8 118006 0 4 598 118006 0 5 718 118006 1 0 23694]
    fts...
fts4growth2        FTS            pass      1 files
fts4incr           FTS            pass      1 files
fts4intck1         FTS            pass      1 files
fts4langid         FTS            pass      1 files
fts4lastrowid      FTS            pass      1 files
fts4merge          FTS            fail      1 files — esult mismatch
          got:  [{}]
          want: [0 0 ...
fts4merge2         FTS            pass      1 files
fts4merge3         FTS            pass      1 files
fts4merge4         FTS            fail      1 files — d jbe jbf jbg jbh jbi jbj jca jcb jcc jcd jce jcf jcg jch...
fts4merge5         FTS            pass      1 files
fts4min            FTS            pass      1 files
fts4noti           FTS            fail      1 files — --- FAIL: Test_fts4noti (0.09s)
    fts4noti_test.go:232:...
fts4onepass        FTS            fail      1 files — --- FAIL: Test_fts4onepass (0.02s)
    fts4onepass_test.g...
fts4opt            FTS            fail      1 files — ) 
    fts4opt_test.go:293: result mismatch
          got...
fts4record         FTS            pass      1 files
fts4rename         FTS            pass      1 files
fts4umlaut         FTS            pass      1 files
fts4unicode        FTS            fail      1 files — got:  [one {} 1 1 one 0 1 1 onebtwoathree {} 1 1 onebtwoa...
fts4upfrom         FTS            pass      1 files
fts_9fd058691      FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
badutf             FUNCTIONS      pass      1 files
ctime              FUNCTIONS      pass      1 files
date               FUNCTIONS      pass      1 files
decimal            FUNCTIONS      pass      1 files, 18 tests skipped
func2              FUNCTIONS      pass      1 files
func3              FUNCTIONS      fail      1 files — --- FAIL: Test_func3 (0.00s)
    func3_test.go:81: result...
func4              FUNCTIONS      fail      1 files, 16 tests skipped — --- FAIL: Test_func4 (0.01s)
    func4_test.go:1457: expe...
func5              FUNCTIONS      pass      1 files, 2 tests skipped
func6              FUNCTIONS      pass      1 files, 10 tests skipped
func7              FUNCTIONS      pass      1 files, 2 tests skipped
func8              FUNCTIONS      pass      1 files
func9              FUNCTIONS      pass      1 files
func_pkg           FUNCTIONS      fail      1 files, 50 tests skipped — [md5 "this${midres}program${midres}is${midres}free${midre...
icu                FUNCTIONS      pass      1 files
instr              FUNCTIONS      pass      1 files
instrfault         FUNCTIONS      pass      1 files
like               FUNCTIONS      fail      1 files — esult mismatch
          got:  [sqlite3_exec_hex db SELEC...
nan                FUNCTIONS      fail      1 files — --- FAIL: Test_nan (0.01s)
    nan_test.go:227: result mi...
percentile         FUNCTIONS      fail      1 files — inf input to percentile_disc()
          sql: 
          ...
printf             FUNCTIONS      pass      1 files, 27 tests skipped
quote              FUNCTIONS      fail      1 files — EATE TABLE xyz(a, b, c CHECK (c!="null") )]
          wan...
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
join8              JOIN           pass      1 files, 8 tests skipped
join9              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinA              JOIN           pass      1 files
joinB              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinC              JOIN           pass      1 files
joinD              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinE              JOIN           pass      1 files
joinF              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinH              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinI              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rowvalue           JOIN           fail      1 files — --- FAIL: Test_rowvalue (0.05s)
    rowvalue_test.go:875:...
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
    json501_test.go:141: r...
json502            JSON           pass      1 files
jsonb01            JSON           pass      1 files
distinct           ORDER          fail      1 files — --- FAIL: Test_distinct (0.02s)
    distinct_test.go:161:...
distinctagg        ORDER          pass      1 files
limit              ORDER          fail      1 files — ce(1)
          
    limit_test.go:545: expected error co...
minmax             ORDER          fail      1 files — --- FAIL: Test_minmax (0.05s)
    minmax_test.go:101: res...
orderby1           ORDER          fail      1 files, 21 tests skipped — --- FAIL: Test_orderby1 (0.50s)
    orderby1_test.go:606:...
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
sort5              ORDER          fail      1 files — sort5_test.go:211: result mismatch
          got:  []
   ...
sorterref          ORDER          pass      1 files
sortfault          ORDER          pass      1 files
unionall           ORDER          pass      1 files, 1 tests skipped
unionall2          ORDER          pass      1 files
unionallfault      ORDER          pass      1 files
unordered          ORDER          pass      1 files
affinity2          OTHER          pass      1 files
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
    avfs_test.go:244: result ...
avtrans            OTHER          fail      1 files — --- FAIL: Test_avtrans (171.56s)
    avtrans_test.go:495:...
backcompat         OTHER          pass      1 files
badutf2            OTHER          pass      1 files
basexx1            OTHER          pass      1 files
bigfile            OTHER          pass      1 files
bigfile2           OTHER          pass      1 files
bigmmap            OTHER          pass      1 files
bigrow             OTHER          fail      1 files — 89 i 9290 j 9291 k 9292 l 9293 m 9294 n 9295 o 9296 p 929...
bigsort            OTHER          pass      1 files
bitvec             OTHER          skipped   1 files, 1 whole-file skip (N/A: test-only C Bitvec self-test + SQLITE_MEMDEBUG mallo...)
bloom1             OTHER          fail      1 files — EXT, y INT, z TEXT);
          INSERT INTO t1(rowid,x,y,z...
boundary1          OTHER          pass      1 files
boundary2          OTHER          pass      1 files
boundary3          OTHER          pass      1 files
boundary4          OTHER          pass      1 files
btree01            OTHER          pass      1 files
btree02            OTHER          pass      1 files
btreefault         OTHER          fail      1 files — --- FAIL: Test_btreefault (0.01s)
    btreefault_test.go:...
cache              OTHER          pass      1 files
cacheflush         OTHER          fail      1 files — , aa);
            CREATE TABLE tb(b, bb);
            IN...
cachespill         OTHER          pass      1 files
cffault            OTHER          pass      1 files
chunksize          OTHER          fail      1 files — --- FAIL: Test_chunksize (0.00s)
    chunksize_test.go:11...
cksumvfs           OTHER          pass      2 files
close_pkg          OTHER          pass      1 files
closure01          OTHER          fail      1 files, 3 tests skipped — --- FAIL: Test_closure01 (10.40s)
    closure01_test.go:9...
colname            OTHER          fail      1 files — --- FAIL: Test_colname (0.01s)
    colname_test.go:489: e...
columncount        OTHER          pass      1 files
conflict2          OTHER          fail      1 files — led: t2.e", got: <nil>
          sql: 
            BEGIN;...
conflict3          OTHER          fail      1 files — 6,4);
    conflict3_test.go:512: expected error containin...
contrib01          OTHER          pass      1 files
corrupt            OTHER          fail      1 files — body: do_test corrupt-2.1962.8
    corrupt_test.go:237: r...
corrupt2           OTHER          pass      1 files, 2 tests skipped
corrupt3           OTHER          pass      1 files
corrupt4           OTHER          pass      1 files
corrupt5           OTHER          pass      1 files
corrupt6           OTHER          pass      1 files
corrupt7           OTHER          pass      1 files
corrupt8           OTHER          pass      1 files
corrupt9           OTHER          pass      1 files
corruptA           OTHER          pass      1 files
corruptB           OTHER          fail      1 files — bcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefgh...
corruptC           OTHER          fail      1 files — ib/dev/frigolite/internal/pager/pager.go:317
github.com/p...
corruptD           OTHER          pass      1 files
corruptE           OTHER          pass      1 files
corruptF           OTHER          fail      1 files — --- FAIL: Test_corruptF (0.43s)
    corruptF_test.go:95: ...
corruptG           OTHER          pass      1 files
corruptH           OTHER          pass      1 files
corruptI           OTHER          pass      1 files
corruptJ           OTHER          pass      1 files
corruptK           OTHER          pass      1 files
corruptL           OTHER          fail      1 files — go:43338: expected error containing "database disk image ...
corruptM           OTHER          pass      1 files
corruptN           OTHER          fail      1 files, 1 tests skipped — Have the trigger
          -- clear page 136 and its chil...
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
date2              OTHER          pass      1 files
date3              OTHER          pass      1 files
date4              OTHER          pass      1 files
date5              OTHER          pass      1 files
dbdata             OTHER          pass      1 files
dbfuzz001          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
dbstatus2          OTHER          pass      1 files
descidx1           OTHER          pass      1 files
descidx2           OTHER          pass      1 files
descidx3           OTHER          pass      1 files
diskfull           OTHER          pass      1 files
distinct2          OTHER          pass      1 files, 4 tests skipped
e_blobbytes        OTHER          pass      1 files
e_blobclose        OTHER          pass      1 files, 5 tests skipped
e_blobopen         OTHER          pass      1 files
e_blobwrite        OTHER          pass      1 files
e_changes          OTHER          pass      1 files
e_createtable      OTHER          skipped   1 files, 1 whole-file skip (CREATE TABLE type-noise P1.E-SQL deep gap N-A (engine CRE...)
e_delete           OTHER          skipped   1 files, 1 whole-file skip (multi-db trigger cascade P1.E-SQL deep gap N-A (e_delete-...)
e_droptrigger      OTHER          fail      1 files — ]
testgen/e_droptrigger/e_droptrigger_test.go:344:11: can...
e_dropview         OTHER          fail      1 files — frigolite.DB value in assignment
testgen/e_dropview/e_dro...
e_expr             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
e_fkey             OTHER          fail      1 files — smatch - \"c7\" referencing \"p7\"", got: <nil>
         ...
e_fts3             OTHER          fail      1 files, 13 tests skipped — --- FAIL: Test_e_fts3 (0.03s)
    e_fts3_test.go:844: que...
e_insert           OTHER          pass      1 files
e_reindex          OTHER          fail      1 files, 1 tests skipped — ndex [github.com/pijalu/frigolite/testgen/e_reindex.test]...
e_resolve          OTHER          fail      1 files — n1]
    e_resolve_test.go:180: result mismatch
          ...
e_select           OTHER          skipped   1 files, 1 whole-file skip (DISTINCT collation ordering P1.E-SQL deep gap N-A (e_sele...)
e_select2          OTHER          pass      1 files
e_totalchanges     OTHER          fail      1 files — --- FAIL: Test_e_totalchanges (0.04s)
    e_totalchanges_...
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
exclusive2         OTHER          pass      1 files
extension01        OTHER          pass      1 files
external_reader    OTHER          pass      1 files
extraquick         OTHER          pass      1 files
fallocate          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
filectrl           OTHER          fail      1 files — --- FAIL: Test_filectrl (0.00s)
    filectrl_test.go:105:...
filefmt            OTHER          pass      1 files
fordelete          OTHER          pass      1 files
format4            OTHER          skipped   1 files, 1 whole-file skip (legacy_file_format file-size harness N-A)
fpconv1            OTHER          pass      1 files, 2 tests skipped
fuzz               OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz2              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz3              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz4              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzz_malloc        OTHER          pass      1 files
fuzz_oss1          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzzer1            OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzzer2            OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fuzzerfault        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
gcfault            OTHER          pass      1 files
gencol1            OTHER          pass      1 files, 12 tests skipped
hidden             OTHER          pass      1 files
ieee754            OTHER          pass      1 files, 2 tests skipped
in2                OTHER          pass      1 files
in3                OTHER          fail      1 files — sql: INSERT INTO t1 VALUES(98,int(log98/log2),9801)
    i...
in4                OTHER          pass      1 files, 5 tests skipped
in5                OTHER          pass      1 files
in6                OTHER          pass      1 files, 2 tests skipped
in7                OTHER          pass      1 files
init               OTHER          fail      1 files — t init-1.3.4
    init_test.go:115: expected error contain...
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
lookaside          OTHER          pass      1 files
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
mem5               OTHER          pass      1 files
memdb              OTHER          pass      1 files
memdb1             OTHER          pass      1 files
memdb2             OTHER          pass      1 files
memjournal         OTHER          pass      1 files
memjournal2        OTHER          pass      1 files
memleak            OTHER          pass      1 files
memsubsys1         OTHER          skipped   1 files, 1 whole-file skip (N/A: SQLITE_CONFIG_MALLOC custom C-allocator subsystem + ...)
memsubsys2         OTHER          skipped   1 files, 1 whole-file skip (N/A: SQLITE_CONFIG_MALLOC custom C-allocator subsystem + ...)
merge1             OTHER          pass      1 files
minmax2            OTHER          fail      1 files — --- FAIL: Test_minmax2 (0.04s)
    minmax2_test.go:83: re...
minmax3            OTHER          pass      1 files
minmax4            OTHER          pass      1 files
misc1              OTHER          fail      1 files, 1 tests skipped — go:655: expected success, got error: table t10 already ex...
misc2              OTHER          pass      1 files
misc3              OTHER          fail      1 files — --- FAIL: Test_misc3 (0.07s)
    misc3_test.go:321: resul...
misc4              OTHER          fail      1 files, 8 tests skipped — regate functions are not allowed in the GROUP BY clause",...
misc5              OTHER          fail      1 files — --- FAIL: Test_misc5 (0.14s)
    misc5_test.go:200: expec...
misc6              OTHER          pass      1 files
misc7              OTHER          fail      1 files, 19 tests skipped — s)
    misc7_test.go:222: query error: attempt to write a...
misc8              OTHER          fail      1 files, 1 tests skipped — sql: 
          INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,n...
misuse             OTHER          fail      1 files — --- FAIL: Test_misuse (0.00s)
    misuse_test.go:360: res...
mmap1              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap2              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap3              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap4              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmapcorrupt        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmapfault          OTHER          pass      1 files
mmapwarm           OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mutex1             OTHER          fail      1 files — --- FAIL: Test_mutex1 (0.00s)
    mutex1_test.go:121: res...
mutex2             OTHER          skipped   1 files, 1 whole-file skip (N/A: SQLITE_MUTEX subsystem instrumentation (disable_mute...)
normalize          OTHER          pass      1 files
nulls1             OTHER          pass      1 files, 4 tests skipped
nulls2             OTHER          pass      1 files
numindex1          OTHER          pass      1 files
offset1            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
openv2             OTHER          pass      1 files
oserror            OTHER          skipped   1 files, 1 whole-file skip (N/A: C VFS syscall injection via test_syscall + sqlite3_l...)
ovfl               OTHER          pass      1 files
p_8_3_names        OTHER          fail      1 files — --- FAIL: Test_t_8_3_names (0.80s)
    8_3_names_test.go:...
pager1             OTHER          pass      1 files
pager2             OTHER          pass      1 files
pager3             OTHER          pass      1 files
pager4             OTHER          pass      1 files
pagerfault         OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
pagerfault2        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
pagerfault3        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
pageropt           OTHER          pass      1 files
pagesize           OTHER          pass      1 files
parser1            OTHER          pass      1 files
pcache             OTHER          fail      1 files — SELECT * FROM t1 ORDER BY a; SELECT * FROM t1;
          ...
pcache2            OTHER          fail      1 files — --- FAIL: Test_pcache2 (0.01s)
    pcache2_test.go:80: re...
permutations       OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/permutations [githu...
pragma             OTHER          pass      1 files, 13 tests skipped
pragma2            OTHER          fail      1 files — --- FAIL: Test_pragma2 (0.11s)
    pragma2_test.go:277: q...
pragma3            OTHER          pass      1 files, 12 tests skipped
pragma4            OTHER          pass      1 files, 1 tests skipped
pragma5            OTHER          pass      1 files
pragma6            OTHER          pass      1 files
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
quickcheck         OTHER          pass      1 files
randexpr1          OTHER          fail      1 files — 38]
    randexpr1_test.go:16419: result mismatch
        ...
rdonly             OTHER          pass      1 files
readonly           OTHER          pass      1 files
recover_pkg        OTHER          pass      1 files
regexp1            OTHER          fail      1 files — --- FAIL: Test_regexp1 (0.01s)
    regexp1_test.go:149: r...
regexp2            OTHER          pass      1 files
reservebytes       OTHER          pass      1 files
resetdb            OTHER          pass      1 files
resolver01         OTHER          fail      1 files — sql: 
            CREATE TABLE t1(x, y); INSERT INTO t1 V...
round1             OTHER          pass      1 files
rowhash            OTHER          pass      1 files
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
sqldiff1           OTHER          skipped   1 files, 1 whole-file skip (N/A: external sqldiff tool binary seam (test_find_sqldiff...)
sqllimits1         OTHER          pass      1 files, 1 tests skipped
starschema1        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
strict1            OTHER          pass      1 files
strict2            OTHER          fail      1 files — [non-BLOB value in t1.e]
    strict2_test.go:335: result ...
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
tempdb2            OTHER          pass      1 files, 2 tests skipped
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
tkt1512            OTHER          pass      1 files
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
tkt2332            OTHER          pass      1 files
tkt2339            OTHER          pass      1 files
tkt2391            OTHER          pass      1 files
tkt2409            OTHER          skipped   1 files, 1 whole-file skip (cache-spill lock-failure simulation (read_lock_db harness...)
tkt2450            OTHER          pass      1 files
tkt2565            OTHER          fail      1 files — --- FAIL: Test_tkt2565 (0.00s)
    tkt2565_test.go:157: r...
tkt2640            OTHER          pass      1 files
tkt2643            OTHER          pass      1 files
tkt2686            OTHER          pass      1 files
tkt2767            OTHER          pass      1 files
tkt2817            OTHER          pass      1 files
tkt2820            OTHER          pass      1 files
tkt2822            OTHER          fail      1 files — t ORDER BY term out of range - should be between 1 and 25...
tkt2832            OTHER          pass      1 files
tkt2854            OTHER          skipped   1 files, 1 whole-file skip (shared-cache multi-connection concurrency not implemented...)
tkt2920            OTHER          pass      1 files
tkt2927            OTHER          pass      1 files
tkt2942            OTHER          pass      1 files
tkt3080            OTHER          pass      1 files
tkt3093            OTHER          skipped   1 files, 1 whole-file skip (multi-connection busy-handler locking not implemented DEF...)
tkt3121            OTHER          fail      1 files — --- FAIL: Test_tkt3121 (0.00s)
    tkt3121_test.go:70: qu...
tkt3201            OTHER          pass      1 files
tkt3292            OTHER          pass      1 files
tkt3298            OTHER          pass      1 files
tkt3334            OTHER          pass      1 files
tkt3346            OTHER          pass      1 files
tkt3357            OTHER          pass      1 files
tkt3363            OTHER          pass      1 files
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
    tkt35xx_test.go:101: e...
tkt3630            OTHER          pass      1 files
tkt3718            OTHER          skipped   1 files, 1 whole-file skip (nested-statement-journal across UDF-driven recursive SQL ...)
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
tkt3929            OTHER          pass      1 files
tkt3935            OTHER          fail      1 files — equired before ON", got: <nil>
          sql:  SELECT a F...
tkt3992            OTHER          fail      1 files — --- FAIL: Test_tkt3992 (0.00s)
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
tkt_5e10420e8d     OTHER          pass      1 files
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
tkt_d11f09d36e     OTHER          pass      1 files
tkt_d635236375     OTHER          pass      1 files
tkt_d82e3f3721     OTHER          pass      1 files, 3 tests skipped
tkt_f3e5abed55     OTHER          skipped   1 files, 1 whole-file skip (testvfs custom VFS + multi-connection ATTACH N-A/DEFERRED)
tkt_f67b41381a     OTHER          skipped   1 files, 1 whole-file skip (EXPLAIN VDBE opcode inspection N-A)
tkt_f777251dc7a    OTHER          fail      1 files — --- FAIL: Test_tkt_f777251dc7a (0.00s)
    tkt-f777251dc7...
tkt_f7b4edec       OTHER          pass      1 files
tkt_f973c7ac31     OTHER          pass      1 files
tkt_fa7bf5ec       OTHER          pass      1 files
tkt_fc62af4523     OTHER          fail      1 files — --- FAIL: Test_tkt_fc62af4523 (0.02s)
    tkt-fc62af4523_...
tkt_fc7bd6358f     OTHER          pass      1 files
tokenize           OTHER          fail      1 files — ECT 1.0e-:
    tokenize_test.go:106: expected error conta...
tpch01             OTHER          fail      1 files — art USING INDEX bootleg_pti (p_type=? AND r_name=?) |--SE...
trace2             OTHER          pass      1 files
trace3             OTHER          fail      1 files — do_test trace3-6.1
    trace3_test.go:337: result mismatc...
trustschema1       OTHER          fail      1 files, 2 tests skipped — ismatch
          got:  [{}]
          want: [2]
    trus...
types2             OTHER          pass      1 files
types3             OTHER          pass      1 files
unique2            OTHER          pass      1 files
uri                OTHER          skipped   1 files, 1 whole-file skip (converter gaps: file-isdir helper + error-variable emissi...)
uri2               OTHER          skipped   1 files, 1 whole-file skip (converter + engine gaps: %00-in-URI rejection (ENABLE_URI...)
utf16align         OTHER          pass      1 files
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
stat               PLANNER        pass      1 files
statfault          PLANNER        pass      1 files
trace              PLANNER        fail      1 files — ]
          want: [SELECT '$::t6int', [$::t6int], 6, 6, "...
rtree              RTREE          pass      1 files
rtree1             RTREE          fail      1 files — --- FAIL: Test_rtree1 (0.20s)
    rtree1_test.go:927: res...
rtree2             RTREE          fail      1 files — [1]
          body: do_test rtree2-rtree.1.5.980.1
    rt...
rtree3             RTREE          fail      1 files — or: database disk image is malformed
          sql:  INSE...
rtree4             RTREE          pass      1 files
rtree5             RTREE          pass      1 files
rtree6             RTREE          pass      1 files
rtree7             RTREE          fail      1 files — --- FAIL: Test_rtree7 (0.05s)
    rtree7_test.go:84: quer...
rtree8             RTREE          fail      1 files — _test rtree8-1.3.3
    rtree8_test.go:257: result mismatc...
rtree9             RTREE          fail      1 files — int failed: rt32_node.nodeno
          sql:  INSERT INTO ...
rtreeA             RTREE          fail      1 files — ror: UNIQUE constraint failed: t1_node.nodeno
    rtreeA_...
rtreeB             RTREE          pass      1 files
rtreeC             RTREE          pass      1 files
rtreeD             RTREE          pass      1 files
rtreeE             RTREE          fail      1 files — --- FAIL: Test_rtreeE (0.05s)
    rtreeE_test.go:89: quer...
rtreeF             RTREE          pass      1 files
rtreeG             RTREE          pass      1 files
rtreeH             RTREE          fail      1 files — ox-49,49]
    rtreeH_test.go:219: result mismatch
       ...
rtreeI             RTREE          pass      1 files
rtreeJ             RTREE          fail      1 files — .0]
          body: do_test 1.7
    rtreeJ_test.go:307: r...
rtreecheck         RTREE          fail      1 files — --- FAIL: Test_rtreecheck (0.19s)
    rtreecheck_test.go:...
rtreecirc          RTREE          fail      1 files — -- FAIL: Test_rtreecirc (0.01s)
    rtreecirc_test.go:119...
rtreeconnect       RTREE          pass      1 files
rtreedoc           RTREE          fail      1 files — NULL, minX+0.2, maxX+0.2, minY, maxY FROM demo_index;
   ...
rtreedoc2          RTREE          fail      1 files — --- FAIL: Test_rtreedoc2 (0.00s)
    rtreedoc2_test.go:13...
rtreedoc3          RTREE          fail      1 files — --- FAIL: Test_rtreedoc3 (0.95s)
    rtreedoc3_test.go:18...
rtreefuzz001       RTREE          fail      1 files — 06: expected error containing "database disk image is mal...
alter              SCHEMA         fail      1 files, 9 tests skipped — column: id
          sql: 
            CREATE TABLE t1(a ...
alter2             SCHEMA         skipped   1 files, 1 whole-file skip (legacy file-format short-row tests (hexio helpers) not im...)
alter3             SCHEMA         fail      1 files — --- FAIL: Test_alter3 (0.02s)
    alter3_test.go:341: exe...
alter4             SCHEMA         pass      1 files
alterauth          SCHEMA         fail      1 files — .go:123: result mismatch
          got:  [{}]
          w...
alterauth2         SCHEMA         pass      1 files
altercol           SCHEMA         fail      1 files, 2 tests skipped — --- FAIL: Test_altercol (0.04s)
    altercol_test.go:1116...
altercons          SCHEMA         fail      1 files, 13 tests skipped — --- FAIL: Test_altercons (0.03s)
    altercons_test.go:19...
altercons2         SCHEMA         pass      1 files, 12 tests skipped
altercons3         SCHEMA         pass      1 files, 1 tests skipped
altercorrupt       SCHEMA         pass      1 files
alterdropcol       SCHEMA         pass      1 files
alterdropcol2      SCHEMA         pass      1 files
alterfault         SCHEMA         pass      1 files
alterlegacy        SCHEMA         pass      1 files, 15 tests skipped
altermalloc        SCHEMA         pass      1 files
altermalloc2       SCHEMA         pass      1 files
altermalloc3       SCHEMA         fail      1 files — ec error: no such column: four
          sql: 
          ...
alterqf            SCHEMA         fail      1 files, 1 tests skipped — FROM t1 WHERE EXISTS ( SELECT 1 FROM t1 AS o WHERE o."a" ...
altertab           SCHEMA         pass      1 files, 53 tests skipped
altertab2          SCHEMA         fail      1 files, 3 tests skipped — ELECT col1 FROM "newname")
                SELECT x FROM ...
altertab3          SCHEMA         fail      1 files, 14 tests skipped — --- FAIL: Test_altertab3 (1.52s)
    altertab3_test.go:77...
altertrig          SCHEMA         pass      1 files
attach             SCHEMA         fail      1 files — error: file is not a database
          sql: 
           ...
attach2            SCHEMA         fail      1 files — --- FAIL: Test_attach2 (0.01s)
    attach2_test.go:291: e...
attach3            SCHEMA         pass      1 files
attach4            SCHEMA         fail      1 files — --- FAIL: Test_attach4 (0.01s)
    attach4_test.go:164: r...
attachmalloc       SCHEMA         pass      1 files
autoinc            SCHEMA         fail      1 files — --- FAIL: Test_autoinc (0.04s)
    autoinc_test.go:850: r...
autovacuum         SCHEMA         fail      1 files — 1 412 413 414 415 416 417 418 419 420 421 422 423 424 425...
autovacuum2        SCHEMA         pass      1 files, 4 tests skipped
autovacuum_ioerr2  SCHEMA         pass      1 files
check              SCHEMA         fail      1 files — --- FAIL: Test_check (0.04s)
    check_test.go:496: expec...
checkfault         SCHEMA         pass      1 files
collate1           SCHEMA         pass      1 files
collate2           SCHEMA         pass      1 files
collate3           SCHEMA         fail      1 files — t:  [0]
          want: [1]
          body: do_test colla...
collate4           SCHEMA         fail      1 files — --- FAIL: Test_collate4 (0.03s)
    collate4_test.go:429:...
collate5           SCHEMA         pass      1 files
collate6           SCHEMA         pass      1 files
collate7           SCHEMA         fail      1 files — --- FAIL: Test_collate7 (0.00s)
    collate7_test.go:108:...
collate8           SCHEMA         pass      1 files
collate9           SCHEMA         pass      1 files
collateA           SCHEMA         pass      1 files
collateB           SCHEMA         pass      1 files
conflict           SCHEMA         fail      1 files — a", got: UNIQUE constraint failed: t5
          sql: 
   ...
coveridxscan       SCHEMA         pass      1 files, 4 tests skipped
createtab          SCHEMA         pass      1 files, 1 tests skipped
fkey1              SCHEMA         fail      1 files, 2 tests skipped — --- FAIL: Test_fkey1 (0.02s)
    fkey1_test.go:180: resul...
fkey2              SCHEMA         fail      1 files — short VALUES(1, 3, 2) 
    fkey2_test.go:2563: expected e...
fkey3              SCHEMA         pass      1 files
fkey4              SCHEMA         pass      1 files
fkey5              SCHEMA         pass      1 files, 13 tests skipped
fkey6              SCHEMA         fail      1 files — --- FAIL: Test_fkey6 (0.01s)
    fkey6_test.go:370: query...
fkey7              SCHEMA         pass      1 files
fkey8              SCHEMA         pass      1 files
fkey_malloc        SCHEMA         pass      1 files
index              SCHEMA         fail      1 files — NORE
              );
            
    index_test.go:1033...
index2             SCHEMA         fail      1 files — # github.com/pijalu/frigolite/testgen/index2 [github.com/...
index3             SCHEMA         pass      1 files, 1 tests skipped
index4             SCHEMA         pass      1 files
index5             SCHEMA         pass      1 files
index6             SCHEMA         pass      1 files, 4 tests skipped
index7             SCHEMA         pass      1 files, 4 tests skipped
index8             SCHEMA         pass      1 files, 1 tests skipped
index9             SCHEMA         pass      1 files
indexA             SCHEMA         pass      1 files, 4 tests skipped
indexedby          SCHEMA         fail      1 files, 2 tests skipped — --- FAIL: Test_indexedby (0.02s)
    indexedby_test.go:14...
indexexpr1         SCHEMA         fail      1 files, 40 tests skipped — --- FAIL: Test_indexexpr1 (0.05s)
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
savepoint2         SCHEMA         pass      1 files
savepoint4         SCHEMA         skipped   1 files, 1 whole-file skip (crashsql crash-simulation while loop not transpilable N-A)
savepoint5         SCHEMA         pass      1 files
savepoint6         SCHEMA         skipped   1 files, 1 whole-file skip (dynamic TCL proc harness (eval/insert_rows/random_integer...)
savepoint7         SCHEMA         pass      1 files
savepointfault     SCHEMA         pass      1 files
schema             SCHEMA         pass      1 files
schema2            SCHEMA         pass      1 files
schema3            SCHEMA         pass      1 files
schema4            SCHEMA         pass      1 files
schema5            SCHEMA         pass      1 files
schema6            SCHEMA         pass      1 files
schemafault        SCHEMA         pass      1 files
temptrigger        SCHEMA         pass      1 files
trans              SCHEMA         fail      1 files — --- FAIL: Test_trans (5.32s)
    trans_test.go:520: expec...
trans2             SCHEMA         fail      1 files — # github.com/pijalu/frigolite/testgen/trans2 [github.com/...
trans3             SCHEMA         pass      1 files
transitive1        SCHEMA         fail      1 files — --- FAIL: Test_transitive1 (0.03s)
    transitive1_test.g...
trigger1           SCHEMA         fail      1 files — : 
            create view v1 as select * from t1;
      ...
trigger2           SCHEMA         fail      1 files — :555: expected error containing "UNIQUE constraint failed...
trigger3           SCHEMA         pass      1 files
trigger4           SCHEMA         fail      1 files — --- FAIL: Test_trigger4 (0.10s)
    trigger4_test.go:132:...
trigger5           SCHEMA         pass      1 files
trigger6           SCHEMA         pass      1 files
trigger7           SCHEMA         fail      1 files — --- FAIL: Test_trigger7 (0.01s)
    trigger7_test.go:74: ...
trigger8           SCHEMA         pass      1 files
trigger9           SCHEMA         pass      1 files
triggerA           SCHEMA         pass      1 files
triggerB           SCHEMA         fail      1 files — ed error containing "no such column: wen.x", got: UNIQUE ...
triggerC           SCHEMA         skipped   1 files, 1 whole-file skip (recursive trigger cascade causes hang (deep-engine applic...)
triggerD           SCHEMA         pass      1 files
triggerE           SCHEMA         pass      1 files
triggerF           SCHEMA         pass      1 files
triggerG           SCHEMA         pass      1 files
triggerupfrom      SCHEMA         pass      1 files
unique             SCHEMA         pass      1 files
vacuum             SCHEMA         pass      1 files
vacuum2            SCHEMA         pass      1 files
vacuum3            SCHEMA         pass      1 files
vacuum4            SCHEMA         pass      1 files
vacuum5            SCHEMA         pass      1 files
vacuum6            SCHEMA         pass      1 files
vacuum_into        SCHEMA         pass      1 files
vacuummem          SCHEMA         skipped   1 files, 1 whole-file skip (N/A: sqlite3_memory_used/highwater C-allocator watermark ...)
rbu                SESSION        pass      1 files
session            SESSION        pass      1 files
amatch1            VTAB           pass      1 files
carray01           VTAB           pass      1 files
carray02           VTAB           pass      1 files
carrayfault        VTAB           pass      1 files
dbpage             VTAB           pass      1 files, 4 tests skipped
dbpagefault        VTAB           pass      1 files
intarray           VTAB           fail      1 files — --- FAIL: Test_intarray (56.45s)
    intarray_test.go:131...
quota              VTAB           pass      1 files
quota2             VTAB           pass      1 files
quota_glob         VTAB           pass      1 files
spellfix           VTAB           pass      1 files
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
vtabD              VTAB           pass      1 files
vtabE              VTAB           pass      1 files
vtabF              VTAB           pass      1 files
vtabH              VTAB           pass      1 files
vtabI              VTAB           pass      1 files
vtabJ              VTAB           pass      1 files, 3 tests skipped
vtabK              VTAB           pass      1 files
vtabL              VTAB           pass      1 files
vtab_alter         VTAB           pass      1 files, 7 tests skipped
vtab_err           VTAB           pass      1 files
vtab_shared        VTAB           fail      1 files, 26 tests skipped — b_shared_test.go:105: expected error containing "no such ...
vtabdistinct       VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_vtabdistinct_test...)
vtabdrop           VTAB           pass      1 files, 6 tests skipped
vtabrhs1           VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_vtabrhs1_test.go))
zipfile            VTAB           fail      1 files — --- FAIL: Test_zipfile (0.05s)
    zipfile_test.go:925: r...
zipfile2           VTAB           pass      1 files
zipfilefault       VTAB           pass      1 files
jrnlmode           WAL            pass      1 files
mjournal           WAL            skipped   1 files, 1 whole-file skip (master-journal pointer validation in hot-journal recovery...)
nockpt             WAL            pass      1 files
rollback           WAL            pass      1 files
rollback2          WAL            pass      1 files
rollbackfault      WAL            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
subjournal         WAL            fail      1 files — --- FAIL: Test_subjournal (0.02s)
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
    walbig_test.go:99: exec...
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
waloverwrite       WAL            fail      1 files — waloverwrite_test.go:135: exec error: database disk image...
walpersist         WAL            fail      1 files — pplied
          sql:  INSERT INTO t1 VALUES(randomblob(5...
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
walsetlk_snapshot  WAL            fail      1 files — --- FAIL: Test_walsetlk_snapshot (0.00s)
    walsetlk_sna...
walshared          WAL            fail      1 files — --- FAIL: Test_walshared (0.00s)
    walshared_test.go:87...
walslow            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walthread          WAL            pass      1 files
walvfs             WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
