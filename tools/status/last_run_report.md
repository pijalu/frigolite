Frigolite testgen status  (generated 2026-08-16T19:17:31Z)

FAMILY              TOTAL   PASS   FAIL   SKIP     PCT
----------------------------------------------------------
AGG                     7      4      3      0   57.1%
C-API                  48     20     15     13   41.7%
CONCURRENCY            39      4      0     35   10.3%
CRUD                   59     47     12      0   79.7%
CTE-WINDOW             34     21     13      0   61.8%
EXPR                   43     30     12      1   69.8%
FTS                    96     43      9     44   44.8%
FUNCTIONS              25     17      7      1   68.0%
JOIN                   37     20      9      8   54.1%
JSON                   12      0      0     12    0.0%
ORDER                  26     18      7      1   69.2%
OTHER                 506    276     61    169   54.5%
PLANNER                38      8      4     26   21.1%
RTREE                   1      1      0      0  100.0%
SCHEMA                121     79     26     16   65.3%
SESSION                 2      2      0      0  100.0%
VTAB                   49     16      4     29   32.7%
WAL                    49     10      3     36   20.4%
----------------------------------------------------------
TOTAL                1192    616    185    391   51.7%

PACKAGES
PKG                FAMILY         STATE     DETAIL
--------------------------------------------------------------------------------
aggerror           AGG            fail      1 files — t1 SELECT a+4 FROM t1;
            INSERT INTO t1 SELECT ...
aggfault           AGG            pass      1 files
aggnested          AGG            pass      1 files, 4 tests skipped
aggorderby         AGG            fail      1 files, 6 tests skipped — --- FAIL: Test_aggorderby (2.16s)
    aggorderby_test.go:...
count              AGG            fail      1 files — NTEGER PRIMARY KEY, b INT, c VARCHAR(1000));
          CR...
countofview        AGG            pass      1 files
having             AGG            pass      1 files
backup             C-API          fail      1 files — atabase aux]
          body: do_test backup-4.1.4
    bac...
backup2            C-API          fail      1 files — t2 SELECT a+4, (a+4)*2 FROM t2;
            INSERT INTO t...
backup4            C-API          fail      1 files — --- FAIL: Test_backup4 (0.00s)
    backup4_test.go:126: r...
backup5            C-API          fail      1 files — --- FAIL: Test_backup5 (0.01s)
    backup5_test.go:114: r...
backup_ioerr       C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
backup_malloc      C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
bind               C-API          fail      1 files — got:  [{}]
          want: [1 {} 1e+300 -1e-300]
        ...
bind2              C-API          fail      1 files — # github.com/pijalu/frigolite/testgen/bind2 [github.com/p...
bindxfer           C-API          fail      1 files — --- FAIL: Test_bindxfer (0.00s)
    bindxfer_test.go:138:...
blob               C-API          fail      1 files — [123456 7890AB CDEF12 345678]
          body: do_test blo...
capi2              C-API          pass      1 files
capi3              C-API          pass      1 files, 7 tests skipped
capi3b             C-API          pass      1 files
capi3c             C-API          pass      1 files, 6 tests skipped
capi3d             C-API          pass      1 files, 2 tests skipped
capi3e             C-API          pass      1 files
changes            C-API          fail      1 files — go:729 +0x448
github.com/pijalu/frigolite.(*DB).Exec(0x11...
changes2           C-API          pass      1 files
colmeta            C-API          pass      1 files
dbstatus           C-API          pass      1 files
exec               C-API          pass      1 files
hook               C-API          pass      1 files, 41 tests skipped
hook2              C-API          pass      1 files
imposter1          C-API          skipped   1 files, 1 whole-file skip (SQLITE_TESTCTRL_IMPOSTER test-control C API N-A)
incrblob           C-API          fail      1 files, 14 tests skipped — ch
          got:  [{}]
          want: [0 0 0 0 0]
     ...
incrblob2          C-API          fail      1 files — --- FAIL: Test_incrblob2 (0.13s)
    incrblob2_test.go:50...
incrblob3          C-API          pass      1 files
incrblob4          C-API          fail      1 files, 1 tests skipped — # github.com/pijalu/frigolite/testgen/incrblob4 [github.c...
incrblob_err       C-API          skipped   1 files, 1 whole-file skip (incremental-blob error paths C API N-A)
incrblobfault      C-API          pass      1 files
incrcorrupt        C-API          skipped   1 files, 1 whole-file skip (incremental-blob corrupt-db C API N-A)
incrvacuum         C-API          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
incrvacuum2        C-API          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
incrvacuum3        C-API          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
incrvacuum_ioerr   C-API          pass      1 files
interrupt          C-API          pass      1 files
interrupt2         C-API          pass      1 files
lastinsert         C-API          pass      1 files
laststmtchanges    C-API          pass      1 files
notify1            C-API          skipped   1 files, 1 whole-file skip (sqlite3_unlock_notify C API not implemented N-A)
notify2            C-API          skipped   1 files, 1 whole-file skip (sqlite3_unlock_notify C API not implemented N-A)
notify3            C-API          skipped   1 files, 1 whole-file skip (sqlite3_unlock_notify C API not implemented N-A)
progress           C-API          fail      1 files — --- FAIL: Test_progress (0.00s)
    progress_test.go:207:...
sqllog             C-API          fail      1 files — --- FAIL: Test_sqllog (0.00s)
    sqllog_test.go:110: res...
stmt               C-API          pass      1 files
stmtrand           C-API          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
stmtvtab1          C-API          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
tableapi           C-API          fail      1 files — # github.com/pijalu/frigolite/testgen/tableapi [github.co...
busy               CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection busy-handler locking DEFERRED)
busy2              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection busy-handler locking DEFERRED)
exclusive          CONCURRENCY    skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
lock               CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
lock2              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
lock3              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
lock4              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
lock5              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
lock6              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
lock7              CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection database locking not implemented DEFERRED)
manydb             CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
multiplex          CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
multiplex2         CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
multiplex3         CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
multiplex4         CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
nolock             CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
pendingrace        CONCURRENCY    skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rowallock          CONCURRENCY    pass      1 files
shared             CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared2            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared3            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared4            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared6            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared7            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared8            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shared9            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
sharedA            CONCURRENCY    pass      1 files
sharedB            CONCURRENCY    pass      1 files
shared_err         CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
sharedlock         CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
shmlock            CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
snapshot           CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
snapshot2          CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
snapshot3          CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
snapshot4          CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
snapshot_fault     CONCURRENCY    skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
snapshot_up        CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
superlock          CONCURRENCY    skipped   1 files, 1 whole-file skip (multi-connection/locking not implemented DEFERRED)
unixexcl           CONCURRENCY    pass      1 files
alias              CRUD           pass      1 files
all                CRUD           pass      1 files
default_pkg        CRUD           pass      1 files
delete2            CRUD           fail      1 files — --- FAIL: Test_delete2 (0.00s)
    delete2_test.go:166: r...
delete3            CRUD           pass      1 files
delete4            CRUD           pass      1 files
delete_db          CRUD           pass      1 files
delete_pkg         CRUD           fail      1 files — f1=3
    delete_test.go:712: result mismatch
          go...
emptytable         CRUD           pass      1 files
insert             CRUD           pass      1 files, 1 tests skipped
insert2            CRUD           pass      1 files
insert3            CRUD           pass      1 files
insert4            CRUD           fail      1 files — lt mismatch
          got:  [0]
          want: [1]
     ...
insert5            CRUD           pass      1 files
insertfault        CRUD           pass      1 files
intpkey            CRUD           pass      1 files, 2 tests skipped
queryonly          CRUD           pass      1 files
returning1         CRUD           pass      1 files
returningfault     CRUD           pass      1 files
rowid              CRUD           fail      1 files, 6 tests skipped — --- FAIL: Test_rowid (0.14s)
    rowid_test.go:199: resul...
select1            CRUD           fail      1 files — y 6]
          body: do_test select1-6.9.8
    select1_te...
select2            CRUD           fail      1 files — 5 6 7 8: 6 7 8]
          body: do_test select2-1.1
    s...
select3            CRUD           pass      1 files
select4            CRUD           fail      1 files — --- FAIL: Test_select4 (0.09s)
    select4_test.go:841: r...
select5            CRUD           pass      1 files
select6            CRUD           fail      1 files — SELECT * FROM t UNION ALL 
            SELECT l,m,l FROM ...
select7            CRUD           pass      1 files
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
    selectH_test.go:106: r...
tableopts          CRUD           pass      1 files, 1 tests skipped
tempdb             CRUD           pass      1 files
temptable          CRUD           pass      1 files, 6 tests skipped
types              CRUD           pass      1 files
update             CRUD           pass      1 files
update2            CRUD           fail      1 files — --- FAIL: Test_update2 (0.01s)
    update2_test.go:310: r...
upfrom1            CRUD           pass      1 files
upfrom2            CRUD           pass      1 files, 1 tests skipped
upfrom3            CRUD           pass      1 files
upfrom4            CRUD           pass      1 files
upfromfault        CRUD           pass      1 files
upsert1            CRUD           pass      1 files
upsert2            CRUD           pass      1 files
upsert3            CRUD           pass      1 files
upsert4            CRUD           pass      1 files
upsert5            CRUD           fail      1 files — --- FAIL: Test_upsert5 (0.04s)
    upsert5_test.go:707: e...
upsertfault        CRUD           pass      1 files
values             CRUD           fail      1 files — s to its right
          sql: SELECT * FROM t1 RIGHT JOIN...
valuesfault        CRUD           pass      1 files
view               CRUD           pass      1 files, 2 tests skipped
view2              CRUD           pass      1 files
view3              CRUD           pass      1 files
filter1            CTE-WINDOW     fail      1 files — or containing "misuse of aggregate function count()", got...
filter2            CTE-WINDOW     pass      1 files
filterfault        CTE-WINDOW     pass      1 files
window1            CTE-WINDOW     fail      1 files, 2 tests skipped — 8: result mismatch
          got:  [catchsql]
          w...
window2            CTE-WINDOW     pass      1 files
window3            CTE-WINDOW     pass      1 files
window4            CTE-WINDOW     pass      1 files
window5            CTE-WINDOW     pass      1 files, 4 tests skipped
window6            CTE-WINDOW     pass      1 files
window7            CTE-WINDOW     pass      1 files
window8            CTE-WINDOW     pass      1 files
window9            CTE-WINDOW     pass      1 files
windowA            CTE-WINDOW     fail      1 files — --- FAIL: Test_windowA (0.00s)
    windowA_test.go:269: r...
windowB            CTE-WINDOW     fail      1 files — BY fake_column))
          SELECT * FROM y;
    windowB_t...
windowC            CTE-WINDOW     fail      1 files — --- FAIL: Test_windowC (0.01s)
    windowC_test.go:147: r...
windowD            CTE-WINDOW     pass      1 files
windowE            CTE-WINDOW     fail      1 files — 4 0.0 487 0.0 488 0.0 489 0.0 490 0.0 491 0.0 494 0.0 495...
windowerr          CTE-WINDOW     pass      1 files
windowfault        CTE-WINDOW     fail      1 files — --- FAIL: Test_windowfault (0.06s)
    windowfault_test.g...
windowpushd        CTE-WINDOW     pass      1 files
with1              CTE-WINDOW     fail      1 files, 9 tests skipped — --- FAIL: Test_with1 (0.01s)
    with1_test.go:249: query...
with2              CTE-WINDOW     fail      1 files, 6 tests skipped — rsy - (0 - miny) * rsy / 2 / (maxy-miny)
                ...
with3              CTE-WINDOW     fail      1 files — --- FAIL: Test_with3 (0.03s)
    with3_test.go:102: exec ...
with4              CTE-WINDOW     pass      1 files
with5              CTE-WINDOW     pass      1 files
with6              CTE-WINDOW     pass      1 files
withM              CTE-WINDOW     pass      1 files
without_rowid1     CTE-WINDOW     fail      1 files, 3 tests skipped — --- FAIL: Test_without_rowid1 (0.00s)
    without_rowid1_...
without_rowid2     CTE-WINDOW     pass      1 files
without_rowid3     CTE-WINDOW     fail      1 files — main {} SQLITE_READ cross e main {} SQLITE_READ cross e m...
without_rowid4     CTE-WINDOW     pass      1 files
without_rowid5     CTE-WINDOW     pass      1 files
without_rowid6     CTE-WINDOW     fail      1 files — --- FAIL: Test_without_rowid6 (0.14s)
    without_rowid6_...
without_rowid7     CTE-WINDOW     pass      1 files
between            EXPR           pass      1 files
cast               EXPR           pass      1 files
coalesce           EXPR           pass      1 files
expr               EXPR           pass      1 files, 3 tests skipped
expr2              EXPR           pass      1 files
exprfault          EXPR           pass      1 files
exprfault2         EXPR           pass      1 files
expridx1           EXPR           fail      1 files, 6 tests skipped — INSERT INTO y1 VALUES(4, 5);
    expridx1_test.go:307: ex...
expridx2           EXPR           pass      1 files
hexlit             EXPR           pass      1 files
in                 EXPR           pass      1 files
istrue             EXPR           pass      1 files
literal            EXPR           fail      1 files — --- FAIL: Test_literal (0.00s)
    literal_test.go:116: e...
null               EXPR           fail      1 files — t2 values(2,null);
              insert into t2 values(3,...
numcast            EXPR           pass      1 files
where              EXPR           fail      1 files, 9 tests skipped — --- FAIL: Test_where (7.81s)
    where_test.go:545: query...
where2             EXPR           pass      1 files, 7 tests skipped
where3             EXPR           fail      1 files — --- FAIL: Test_where3 (0.04s)
    where3_test.go:149: exe...
where4             EXPR           pass      1 files, 2 tests skipped
where5             EXPR           pass      1 files
where6             EXPR           pass      1 files
where7             EXPR           pass      1 files
where8             EXPR           fail      1 files — ngs writings]
          want: []
          body: do_test ...
where9             EXPR           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
whereA             EXPR           fail      1 files, 2 tests skipped — --- FAIL: Test_whereA (0.00s)
    whereA_test.go:266: que...
whereB             EXPR           pass      1 files
whereC             EXPR           pass      1 files
whereD             EXPR           pass      1 files
whereE             EXPR           fail      1 files — --- FAIL: Test_whereE (0.12s)
    whereE_test.go:90: quer...
whereF             EXPR           pass      1 files, 3 tests skipped
whereG             EXPR           fail      1 files — --- FAIL: Test_whereG (0.03s)
    whereG_test.go:248: exe...
whereH             EXPR           pass      1 files, 16 tests skipped
whereI             EXPR           pass      1 files
whereJ             EXPR           pass      1 files
whereK             EXPR           pass      1 files
whereL             EXPR           pass      1 files
whereM             EXPR           pass      1 files
whereN             EXPR           fail      1 files — value, rid, randomblob(15)
                FROM src, gene...
wherefault         EXPR           pass      1 files
wherelfault        EXPR           pass      1 files
wherelimit         EXPR           pass      1 files
wherelimit2        EXPR           fail      1 files, 6 tests skipped — --- FAIL: Test_wherelimit2 (0.02s)
    wherelimit2_test.g...
wherelimit3        EXPR           fail      1 files — --- FAIL: Test_wherelimit3 (0.03s)
    wherelimit3_test.g...
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
fts3comp1          FTS            pass      1 files
fts3conf           FTS            fail      1 files — --- FAIL: Test_fts3conf (0.01s)
    fts3conf_test.go:208:...
fts3corrupt        FTS            pass      1 files
fts3corrupt2       FTS            pass      1 files
fts3corrupt3       FTS            fail      1 files — --- FAIL: Test_fts3corrupt3 (0.00s)
    fts3corrupt3_test...
fts3corrupt4       FTS            fail      1 files, 50 tests skipped — DBG opencursor fail: t1_segments 4 database disk image is...
fts3corrupt5       FTS            skipped   1 files, 1 whole-file skip (FTS3 corrupt-database tokenizer harness N-A (FTS not impl...)
fts3corrupt6       FTS            skipped   1 files, 1 whole-file skip (FTS3 corrupt-database tokenizer harness N-A (FTS not impl...)
fts3corrupt7       FTS            skipped   1 files, 1 whole-file skip (FTS3 corrupt-database tokenizer harness N-A (FTS not impl...)
fts3cov            FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3d              FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3defer          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3defer2         FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3defer3         FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3drop           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3dropmod        FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3e              FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3expr           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3expr2          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3expr3          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3expr4          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3expr5          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
fts3f              FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3fault          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3fault2         FTS            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fts3fault3         FTS            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
fts3first          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3fuzz001        FTS            pass      1 files
fts3integrity      FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3join           FTS            pass      1 files
fts3malloc         FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3matchinfo      FTS            pass      1 files
fts3matchinfo2     FTS            pass      1 files
fts3misc           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3near           FTS            pass      1 files
fts3offsets        FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3prefix         FTS            pass      1 files
fts3prefix2        FTS            pass      1 files
fts3query          FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3rank           FTS            pass      1 files
fts3rnd            FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3shared         FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3snippet        FTS            pass      1 files
fts3snippet2       FTS            pass      1 files
fts3sort           FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts3tok1           FTS            pass      1 files
fts3tok_err        FTS            pass      1 files
fts3varint         FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 feature beyond the basic module N-A (full FTS no...)
fts4aa             FTS            pass      1 files
fts4check          FTS            fail      1 files — 448
github.com/pijalu/frigolite.(*DB).Exec(0x34f2cf7aac00...
fts4content        FTS            pass      1 files, 7 tests skipped
fts4docid          FTS            pass      1 files
fts4growth         FTS            pass      1 files
fts4growth2        FTS            pass      1 files
fts4incr           FTS            pass      1 files
fts4intck1         FTS            pass      1 files
fts4langid         FTS            fail      1 files — --- FAIL: Test_fts4langid (0.01s)
    fts4langid_test.go:...
fts4lastrowid      FTS            pass      1 files
fts4merge          FTS            fail      1 files — 29 +0x448
github.com/pijalu/frigolite.(*DB).Exec(0x2f1ef7...
fts4merge2         FTS            pass      1 files
fts4merge3         FTS            pass      1 files
fts4merge4         FTS            fail      1 files — 02e155f0, 0x1b}, {0x102e1c9ff, 0x2e}, 0x3bdc36724378, {0x...
fts4merge5         FTS            pass      1 files
fts4min            FTS            pass      1 files
fts4noti           FTS            pass      1 files
fts4onepass        FTS            pass      1 files
fts4opt            FTS            fail      1 files — gine_core.go:729 +0x448
github.com/pijalu/frigolite.(*DB)...
fts4record         FTS            pass      1 files
fts4rename         FTS            pass      1 files
fts4umlaut         FTS            pass      1 files
fts4unicode        FTS            fail      1 files — 448
github.com/pijalu/frigolite.(*DB).Exec(0x36131420f1c0...
fts4upfrom         FTS            pass      1 files
fts_9fd058691      FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
badutf             FUNCTIONS      pass      1 files
ctime              FUNCTIONS      fail      1 files — --- FAIL: Test_ctime (0.00s)
    ctime_test.go:270: resul...
date               FUNCTIONS      fail      1 files — --- FAIL: Test_date (0.22s)
    date_test.go:619: query e...
decimal            FUNCTIONS      pass      1 files, 18 tests skipped
func2              FUNCTIONS      pass      1 files
func3              FUNCTIONS      fail      1 files — --- FAIL: Test_func3 (0.00s)
    func3_test.go:78: result...
func4              FUNCTIONS      fail      1 files, 16 tests skipped — ped string constant) to type int
testgen/func4/func4_test...
func5              FUNCTIONS      pass      1 files, 2 tests skipped
func6              FUNCTIONS      pass      1 files, 10 tests skipped
func7              FUNCTIONS      pass      1 files, 2 tests skipped
func8              FUNCTIONS      pass      1 files
func9              FUNCTIONS      pass      1 files
func_pkg           FUNCTIONS      fail      1 files, 50 tests skipped — '));
          
    func_test.go:1228: query error: frigo...
icu                FUNCTIONS      pass      1 files
instr              FUNCTIONS      pass      1 files
instrfault         FUNCTIONS      pass      1 files
like               FUNCTIONS      fail      1 files — esult mismatch
          got:  [sqlite3_exec_hex db SELEC...
nan                FUNCTIONS      fail      1 files — t1
    nan_test.go:105: exec error: no such table: t1
   ...
percentile         FUNCTIONS      pass      1 files
printf             FUNCTIONS      pass      1 files, 27 tests skipped
quote              FUNCTIONS      pass      1 files
substr             FUNCTIONS      pass      1 files
unhex              FUNCTIONS      pass      1 files
zeroblob           FUNCTIONS      skipped   1 files, 1 whole-file skip (C API not exposed N-A)
zeroblobfault      FUNCTIONS      pass      1 files
exists             JOIN           pass      1 files
existsexpr         JOIN           fail      1 files — AR SUBQUERY 1    `--SCAN x1]
          must not match pat...
existsexpr2        JOIN           fail      1 files — ;
          CREATE INDEX t1ab ON t1(a,b);
        
      ...
existsfault        JOIN           pass      1 files
full               JOIN           pass      1 files
join               JOIN           fail      1 files, 6 tests skipped — --- FAIL: Test_join (0.05s)
    join_test.go:1303: expect...
join2              JOIN           pass      1 files
join3              JOIN           pass      1 files
join4              JOIN           pass      1 files
join5              JOIN           fail      1 files — T);
          WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL...
join6              JOIN           pass      1 files
join7              JOIN           pass      1 files
join8              JOIN           fail      1 files, 5 tests skipped — VALUES('t2','t2b','48 1');
          INSERT INTO sqlite_s...
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
    rowvalue_test.go:859:...
rowvalue2          JOIN           pass      1 files
rowvalue3          JOIN           fail      1 files — --- FAIL: Test_rowvalue3 (0.03s)
    rowvalue3_test.go:22...
rowvalue4          JOIN           fail      1 files — .go:135: exec error: index t2abc already exists
         ...
rowvalue5          JOIN           skipped   1 files, 1 whole-file skip (TCL-implemented virtual table (register_tcl_module) N-A)
rowvalue6          JOIN           pass      1 files
rowvalue7          JOIN           pass      1 files
rowvalue8          JOIN           pass      1 files
rowvalue9          JOIN           pass      1 files
rowvalueA          JOIN           pass      1 files
rowvaluefault      JOIN           pass      1 files
rowvaluevtab       JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
subquery           JOIN           fail      1 files — ]
          body: do_test subquery-6.4
    subquery_test....
subquery2          JOIN           pass      1 files
subselect          JOIN           pass      1 files
json101            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json102            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json103            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json104            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json105            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json106            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json107            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json108            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json109            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json501            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
json502            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
jsonb01            JSON           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
distinct           ORDER          fail      1 files — --- FAIL: Test_distinct (0.01s)
    distinct_test.go:159:...
distinctagg        ORDER          pass      1 files
limit              ORDER          fail      1 files — --- FAIL: Test_limit (52.56s)
    limit_test.go:456: exec...
minmax             ORDER          fail      1 files — --- FAIL: Test_minmax (0.05s)
    minmax_test.go:98: resu...
orderby1           ORDER          fail      1 files, 21 tests skipped — );
            INSERT INTO album VALUES(1, '1-one'), (2, ...
orderby2           ORDER          pass      1 files, 3 tests skipped
orderby3           ORDER          pass      1 files
orderby4           ORDER          pass      1 files
orderby5           ORDER          pass      1 files, 24 tests skipped
orderby6           ORDER          pass      1 files
orderby7           ORDER          fail      1 files, 9 tests skipped — is a test of the fts3 virtual'),
                   (2,'t...
orderby8           ORDER          pass      1 files
orderby9           ORDER          pass      1 files
orderbyA           ORDER          pass      1 files
orderbyB           ORDER          pass      1 files
sort               ORDER          fail      1 files, 2 tests skipped — 00, random()%5000 FROM c;
          CREATE TABLE t2(d,e,f...
sort2              ORDER          pass      1 files
sort3              ORDER          pass      1 files, 2 tests skipped
sort4              ORDER          skipped   1 files, 1 whole-file skip (VDBE sorter internals (do_sorter_test) not implemented)
sort5              ORDER          pass      1 files
sorterref          ORDER          pass      1 files
sortfault          ORDER          pass      1 files
unionall           ORDER          pass      1 files, 1 tests skipped
unionall2          ORDER          pass      1 files
unionallfault      ORDER          pass      1 files
unordered          ORDER          fail      1 files — UES(1, 'xxx');
          INSERT INTO t1 SELECT a+1, b FRO...
affinity2          OTHER          pass      1 files
affinity3          OTHER          pass      1 files
atof1              OTHER          skipped   1 files, 1 whole-file skip (TCL expr rand/pow/format %.32e random float stress harnes...)
atof2              OTHER          skipped   1 files, 1 whole-file skip (TCL expr rand/pow/format %.32e random float stress harnes...)
atomic             OTHER          pass      1 files
atomic2            OTHER          pass      1 files
auth               OTHER          fail      1 files, 2 tests skipped — ody: do_test auth-8.1
    auth_test.go:3421: result misma...
auth2              OTHER          fail      1 files, 1 tests skipped — main {} SQLITE_READ v2 a main {} SQLITE_SELECT {} {} {} v...
auth3              OTHER          fail      1 files — --- FAIL: Test_auth3 (0.00s)
    auth3_test.go:176: resul...
autoanalyze1       OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autoindex1         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autoindex2         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autoindex3         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autoindex4         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autoindex5         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
avfs               OTHER          fail      1 files — --- FAIL: Test_avfs (0.00s)
    avfs_test.go:225: result ...
avtrans            OTHER          pass      1 files
backcompat         OTHER          pass      1 files
badutf2            OTHER          pass      1 files
basexx1            OTHER          pass      1 files
bigfile            OTHER          skipped   1 files, 1 whole-file skip (>4GB large-file TCL harness + msg redeclare transpiler bu...)
bigfile2           OTHER          skipped   1 files, 1 whole-file skip (>4GB large-file TCL harness + msg redeclare transpiler bu...)
bigmmap            OTHER          pass      1 files
bigrow             OTHER          pass      1 files
bigsort            OTHER          pass      1 files
bitvec             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bloom1             OTHER          fail      1 files — ANALYZE sqlite_schema;
    bloom1_test.go:199: exec error...
boundary1          OTHER          pass      1 files
boundary2          OTHER          pass      1 files
boundary3          OTHER          pass      1 files
boundary4          OTHER          pass      1 files
btree01            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
btree02            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
btreefault         OTHER          fail      1 files — --- FAIL: Test_btreefault (0.00s)
    btreefault_test.go:...
cache              OTHER          skipped   1 files, 1 whole-file skip (pager/btree cache internals DEFERRED)
cacheflush         OTHER          fail      1 files — , aa);
            CREATE TABLE tb(b, bb);
            IN...
cachespill         OTHER          fail      1 files — --- FAIL: Test_cachespill (0.01s)
    cachespill_test.go:...
cffault            OTHER          pass      1 files
chunksize          OTHER          fail      1 files — --- FAIL: Test_chunksize (0.00s)
    chunksize_test.go:10...
cksumvfs           OTHER          skipped   1 files, 1 whole-file skip (custom checksum VFS not implemented N-A)
close_pkg          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/close_pkg [github.c...
closure01          OTHER          skipped   1 files, 1 whole-file skip (transitive_closure virtual table extension not implemente...)
colname            OTHER          fail      1 files — --- FAIL: Test_colname (0.01s)
    colname_test.go:511: e...
columncount        OTHER          pass      1 files
conflict2          OTHER          pass      1 files
conflict3          OTHER          pass      1 files
contrib01          OTHER          pass      1 files
corrupt            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt2           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt3           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt4           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt5           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt6           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt7           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt8           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corrupt9           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corruptA           OTHER          pass      1 files
corruptB           OTHER          pass      1 files
corruptC           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corruptD           OTHER          pass      1 files
corruptE           OTHER          pass      1 files
corruptF           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corruptG           OTHER          pass      1 files
corruptH           OTHER          pass      1 files
corruptI           OTHER          pass      1 files
corruptJ           OTHER          pass      1 files
corruptK           OTHER          pass      1 files
corruptL           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
corruptM           OTHER          pass      1 files
corruptN           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
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
csv01              OTHER          skipped   1 files, 1 whole-file skip (extension not implemented N-A)
cursorhint2        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
dataversion1       OTHER          pass      1 files
date2              OTHER          pass      1 files
date3              OTHER          pass      1 files
date4              OTHER          pass      1 files
date5              OTHER          pass      1 files
dbdata             OTHER          skipped   1 files, 1 whole-file skip (sqlite_dbpage virtual table extension not implemented N-A)
dbfuzz001          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
dbstatus2          OTHER          pass      1 files
descidx1           OTHER          pass      1 files
descidx2           OTHER          pass      1 files
descidx3           OTHER          pass      1 files
diskfull           OTHER          pass      1 files
distinct2          OTHER          fail      1 files, 1 tests skipped — --- FAIL: Test_distinct2 (0.01s)
    distinct2_test.go:20...
e_blobbytes        OTHER          pass      1 files
e_blobclose        OTHER          pass      1 files, 5 tests skipped
e_blobopen         OTHER          pass      1 files
e_blobwrite        OTHER          pass      1 files
e_changes          OTHER          pass      1 files
e_createtable      OTHER          fail      1 files — pk SHORT INTEGER primary key);
          CREATE TABLE t9(...
e_delete           OTHER          pass      1 files
e_droptrigger      OTHER          pass      1 files
e_dropview         OTHER          pass      1 files
e_expr             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
e_fkey             OTHER          fail      1 files — E c1(c, d REFERENCES 'p 1 "parent one"' ON UPDATE CASCADE...
e_fts3             OTHER          fail      1 files, 13 tests skipped — 4 5 6 7 8]
          want: [6 7]
    e_fts3_test.go:1258:...
e_insert           OTHER          pass      1 files
e_reindex          OTHER          pass      1 files, 1 tests skipped
e_resolve          OTHER          pass      1 files
e_select           OTHER          pass      1 files
e_select2          OTHER          pass      1 files
e_totalchanges     OTHER          fail      1 files — --- FAIL: Test_e_totalchanges (0.03s)
    e_totalchanges_...
e_update           OTHER          fail      1 files — --- FAIL: Test_e_update (0.02s)
    e_update_test.go:401:...
e_uri              OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/e_uri
testgen/e_uri...
e_vacuum           OTHER          fail      1 files, 12 tests skipped — --- FAIL: Test_e_vacuum (0.04s)
    e_vacuum_test.go:142:...
e_wal              OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
e_walauto          OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
e_walckpt          OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
e_walhook          OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
enc                OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
enc2               OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
enc3               OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
enc4               OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
eqp2               OTHER          fail      1 files — r: database disk image is malformed
          sql: 
     ...
errmsg             OTHER          pass      1 files, 1 tests skipped
errofst1           OTHER          pass      1 files
eval               OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
exclusive2         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
extension01        OTHER          pass      1 files
external_reader    OTHER          pass      1 files
extraquick         OTHER          pass      1 files
fallocate          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
filectrl           OTHER          fail      1 files — --- FAIL: Test_filectrl (0.00s)
    filectrl_test.go:105:...
filefmt            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
fordelete          OTHER          pass      1 files
format4            OTHER          fail      1 files — --- FAIL: Test_format4 (0.00s)
    format4_test.go:81: re...
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
gencol1            OTHER          fail      1 files, 12 tests skipped — );
              CREATE INDEX x2 ON t1(a);
              ...
hidden             OTHER          pass      1 files, 1 tests skipped
ieee754            OTHER          pass      1 files, 2 tests skipped
in2                OTHER          pass      1 files
in3                OTHER          pass      1 files
in4                OTHER          fail      1 files, 5 tests skipped — in4_test.go:711: query error: database disk image is malf...
in5                OTHER          pass      1 files
in6                OTHER          fail      1 files, 1 tests skipped — ge is malformed
          sql: 
            CREATE TABLE ...
in7                OTHER          pass      1 files
init               OTHER          pass      1 files
intreal            OTHER          pass      1 files
io                 OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr              OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr2             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr3             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr4             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr5             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
ioerr6             OTHER          skipped   1 files, 1 whole-file skip (VFS I/O error simulation N-A)
journal1           OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
journal2           OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
journal3           OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
jrnlmode2          OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
jrnlmode3          OTHER          skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
keyword1           OTHER          fail      1 files — olite: parse error: near "without": syntax error
        ...
like2              OTHER          pass      1 files
like3              OTHER          pass      1 files
limit2             OTHER          fail      1 files — ',0,'travel'),('reality',0,'hour');
          CREATE INDE...
literal2           OTHER          pass      1 files
loadext            OTHER          pass      1 files
loadext2           OTHER          pass      1 files
lookaside          OTHER          pass      1 files
main               OTHER          skipped   1 files, 1 whole-file skip (sqlite3_complete C API + TCL namespace procs N-A)
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
memdb              OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memdb1             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memdb2             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memjournal         OTHER          pass      1 files
memjournal2        OTHER          pass      1 files
memleak            OTHER          pass      1 files
memsubsys1         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
memsubsys2         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
merge1             OTHER          pass      1 files
minmax2            OTHER          fail      1 files — --- FAIL: Test_minmax2 (0.04s)
    minmax2_test.go:81: re...
minmax3            OTHER          pass      1 files
minmax4            OTHER          pass      1 files
misc1              OTHER          fail      1 files, 1 tests skipped — 2: result mismatch
          got:  [{}]
          want: [...
misc2              OTHER          pass      1 files
misc3              OTHER          pass      1 files
misc4              OTHER          pass      1 files, 8 tests skipped
misc5              OTHER          pass      1 files
misc6              OTHER          skipped   1 files, 1 whole-file skip (C-API prepared-statement bind/column test (sqlite3_prepar...)
misc7              OTHER          pass      1 files, 19 tests skipped
misc8              OTHER          pass      1 files, 1 tests skipped
misuse             OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/misuse [github.com/...
mmap1              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap2              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap3              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmap4              OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmapcorrupt        OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mmapfault          OTHER          pass      1 files
mmapwarm           OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
mutex1             OTHER          pass      1 files
mutex2             OTHER          pass      1 files
normalize          OTHER          fail      1 files — nt: [0 {INSERT INTO t1(x)VALUES(?),(?),(?),(?);}]
       ...
nulls1             OTHER          fail      1 files, 4 tests skipped — --- FAIL: Test_nulls1 (0.02s)
    nulls1_test.go:383: exe...
nulls2             OTHER          pass      1 files
numindex1          OTHER          pass      1 files
offset1            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
openv2             OTHER          pass      1 files
oserror            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
ovfl               OTHER          pass      1 files
p_8_3_names        OTHER          pass      1 files
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
pcache2            OTHER          fail      1 files — --- FAIL: Test_pcache2 (0.01s)
    pcache2_test.go:82: re...
permutations       OTHER          pass      1 files
pragma             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma2            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma3            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma4            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma5            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragma6            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
pragmafault        OTHER          pass      1 files
prefixes           OTHER          pass      1 files
printf2            OTHER          pass      1 files
ptrchng            OTHER          fail      1 files — ) FROM t1 WHERE x=4
          
    ptrchng_test.go:118: q...
qrf01              OTHER          fail      1 files — es │ ' abcde' │ │ yes │ 'abcde ' │ │ yes │ ...
qrf02              OTHER          fail      1 files — p5  comment      
        ----  -------------  ----  ----...
qrf03              OTHER          fail      1 files — 69 28695 2 0 0 0 28770 28697 28769 28698 3 0 0 0 28767 28...
qrf04              OTHER          pass      1 files
qrf05              OTHER          pass      1 files
qrf06              OTHER          pass      1 files
quick              OTHER          pass      1 files
quickcheck         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
randexpr1          OTHER          fail      1 files — 00]
    randexpr1_test.go:16419: result mismatch
        ...
rdonly             OTHER          pass      1 files
readonly           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
recover_pkg        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
regexp1            OTHER          fail      1 files — --- FAIL: Test_regexp1 (0.01s)
    regexp1_test.go:151: r...
regexp2            OTHER          pass      1 files
reservebytes       OTHER          pass      1 files, 2 tests skipped
resetdb            OTHER          skipped   1 files, 1 whole-file skip (SQLITE_DBCONFIG_RESET_DATABASE C API N-A)
resolver01         OTHER          pass      1 files
round1             OTHER          pass      1 files
rowhash            OTHER          pass      1 files
scanstatus2        OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
securedel          OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
securedel2         OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
seekscan1          OTHER          fail      1 files — --- FAIL: Test_seekscan1 (2.59s)
    seekscan1_test.go:65...
shell1             OTHER          pass      1 files
shell2             OTHER          pass      1 files
shell3             OTHER          pass      1 files
shell4             OTHER          pass      1 files
shell5             OTHER          pass      1 files
shell6             OTHER          pass      1 files
shell7             OTHER          pass      1 files
shell8             OTHER          pass      1 files
shell9             OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
shellA             OTHER          skipped   1 files, 1 whole-file skip (CLI shell subprocess harness N-A)
shellB             OTHER          pass      1 files
shortread1         OTHER          fail      1 files — --- FAIL: Test_shortread1 (0.00s)
    shortread1_test.go:...
shrink             OTHER          pass      1 files
sidedelete         OTHER          pass      1 files
skipscan1          OTHER          skipped   1 files, 1 whole-file skip (skip-scan planner strategy + TCL assoc-array data N-A)
skipscan2          OTHER          skipped   1 files, 1 whole-file skip (skip-scan planner strategy + TCL assoc-array data N-A)
skipscan3          OTHER          skipped   1 files, 1 whole-file skip (skip-scan planner strategy + TCL assoc-array data N-A)
skipscan5          OTHER          skipped   1 files, 1 whole-file skip (skip-scan planner strategy + TCL assoc-array data N-A)
skipscan6          OTHER          skipped   1 files, 1 whole-file skip (skip-scan planner strategy + TCL assoc-array data N-A)
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
strict1            OTHER          fail      1 files — error containing "cannot store REAL value in BLOB column ...
strict2            OTHER          pass      1 files
subtype1           OTHER          skipped   1 files, 1 whole-file skip (value-subtype API (C-extension) not implemented)
symlink            OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
symlink2           OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
sync               OTHER          pass      1 files
sync2              OTHER          pass      1 files
syscall            OTHER          pass      1 files
sysfault           OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
tabfunc01          OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
table              OTHER          fail      1 files, 1 tests skipped — # github.com/pijalu/frigolite/testgen/table [github.com/p...
tclsqlite          OTHER          skipped   1 files, 1 whole-file skip (TCL binding tests N-A (TCL API))
tempdb2            OTHER          pass      1 files, 2 tests skipped
tempfault          OTHER          pass      1 files
temptable2         OTHER          fail      1 files, 4 tests skipped — BLE t1(a, b);
            CREATE INDEX i1 ON t1(a, b);
  ...
temptable3         OTHER          pass      1 files
thread001          OTHER          pass      1 files
thread002          OTHER          pass      1 files
thread003          OTHER          pass      1 files
thread004          OTHER          pass      1 files
thread005          OTHER          fail      1 files — # github.com/pijalu/frigolite/testgen/thread005 [github.c...
thread1            OTHER          pass      1 files
thread2            OTHER          pass      1 files
thread3            OTHER          pass      1 files
timediff1          OTHER          pass      1 files
tkt1435            OTHER          fail      1 files — , versionId, flavorId);
            ANALYZE;
            ...
tkt1443            OTHER          pass      1 files
tkt1444            OTHER          pass      1 files
tkt1449            OTHER          pass      1 files
tkt1473            OTHER          pass      1 files
tkt1501            OTHER          pass      1 files
tkt1512            OTHER          pass      1 files
tkt1514            OTHER          pass      1 files
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
tkt2332            OTHER          skipped   1 files, 1 whole-file skip (C-API incrblob blob handle + tclvar harness fn not implem...)
tkt2339            OTHER          pass      1 files
tkt2391            OTHER          pass      1 files
tkt2409            OTHER          skipped   1 files, 1 whole-file skip (cache-spill lock-failure simulation (read_lock_db harness...)
tkt2450            OTHER          pass      1 files
tkt2565            OTHER          fail      1 files — --- FAIL: Test_tkt2565 (0.00s)
    tkt2565_test.go:137: r...
tkt2640            OTHER          pass      1 files
tkt2643            OTHER          fail      1 files — --- FAIL: Test_tkt2643 (0.00s)
    tkt2643_test.go:63: ex...
tkt2686            OTHER          skipped   1 files, 1 whole-file skip (PRAGMA max_page_count not enforced (database or disk is f...)
tkt2767            OTHER          pass      1 files
tkt2817            OTHER          pass      1 files
tkt2820            OTHER          pass      1 files
tkt2822            OTHER          pass      1 files
tkt2832            OTHER          pass      1 files
tkt2854            OTHER          skipped   1 files, 1 whole-file skip (shared-cache multi-connection concurrency not implemented...)
tkt2920            OTHER          fail      1 files — --- FAIL: Test_tkt2920 (0.00s)
    tkt2920_test.go:70: re...
tkt2927            OTHER          pass      1 files
tkt2942            OTHER          pass      1 files
tkt3080            OTHER          skipped   1 files, 1 whole-file skip (test-harness execsql UDF (runs SQL from within a query) n...)
tkt3093            OTHER          skipped   1 files, 1 whole-file skip (multi-connection busy-handler locking not implemented DEF...)
tkt3121            OTHER          pass      1 files
tkt3201            OTHER          pass      1 files
tkt3292            OTHER          pass      1 files
tkt3298            OTHER          pass      1 files
tkt3334            OTHER          pass      1 files
tkt3346            OTHER          pass      1 files
tkt3357            OTHER          pass      1 files
tkt3419            OTHER          pass      1 files
tkt3424            OTHER          pass      1 files
tkt3442            OTHER          pass      1 files
tkt3457            OTHER          pass      1 files
tkt3461            OTHER          pass      1 files
tkt3493            OTHER          pass      1 files
tkt3508            OTHER          pass      1 files
tkt3522            OTHER          pass      1 files
tkt3527            OTHER          pass      1 files
tkt3541            OTHER          pass      1 files
tkt3554            OTHER          pass      1 files
tkt3581            OTHER          pass      1 files
tkt35xx            OTHER          pass      1 files
tkt3630            OTHER          pass      1 files
tkt3718            OTHER          skipped   1 files, 1 whole-file skip (test-harness SQL-executing UDFs f1/f2 not implemented N-A)
tkt3731            OTHER          pass      1 files
tkt3757            OTHER          fail      1 files — --- FAIL: Test_tkt3757 (0.00s)
    tkt3757_test.go:66: qu...
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
tkt3935            OTHER          pass      1 files
tkt3992            OTHER          fail      1 files — --- FAIL: Test_tkt3992 (0.00s)
    tkt3992_test.go:102: r...
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
tkt_4a03edc4c8     OTHER          fail      1 files — --- FAIL: Test_tkt_4a03edc4c8 (0.01s)
    tkt-4a03edc4c8_...
tkt_4c86b126f2     OTHER          pass      1 files
tkt_4dd95f6943     OTHER          pass      1 files
tkt_4ef7e3cfca     OTHER          pass      1 files
tkt_54844eea3f     OTHER          fail      1 files — --- FAIL: Test_tkt_54844eea3f (0.00s)
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
tkt_a8a0d2996a     OTHER          pass      1 files
tkt_b1d3a2e531     OTHER          pass      1 files
tkt_b351d95f9      OTHER          pass      1 files
tkt_b72787b1       OTHER          pass      1 files
tkt_b75a9ca6b0     OTHER          pass      1 files
tkt_ba7cbfaedc     OTHER          pass      1 files
tkt_bd484a090c     OTHER          fail      1 files — --- FAIL: Test_tkt_bd484a090c (0.00s)
    tkt-bd484a090c_...
tkt_bdc6bbbb38     OTHER          skipped   1 files, 1 whole-file skip (FTS4 virtual table not implemented N-A)
tkt_c48d99d690     OTHER          pass      1 files
tkt_c694113d5      OTHER          fail      1 files — --- FAIL: Test_tkt_c694113d5 (0.00s)
    tkt-c694113d5_te...
tkt_cbd054fa6b     OTHER          pass      1 files
tkt_d11f09d36e     OTHER          fail      1 files — pijalu/frigolite.(*DB).Exec(0x1d3c591bd5c0, {0x10073d382?...
tkt_d635236375     OTHER          pass      1 files
tkt_d82e3f3721     OTHER          pass      1 files, 3 tests skipped
tkt_f3e5abed55     OTHER          skipped   1 files, 1 whole-file skip (testvfs custom VFS + multi-connection ATTACH N-A/DEFERRED)
tkt_f67b41381a     OTHER          skipped   1 files, 1 whole-file skip (EXPLAIN VDBE opcode inspection N-A)
tkt_f777251dc7a    OTHER          pass      1 files
tkt_f7b4edec       OTHER          pass      1 files
tkt_f973c7ac31     OTHER          pass      1 files
tkt_fa7bf5ec       OTHER          pass      1 files
tkt_fc62af4523     OTHER          pass      1 files
tkt_fc7bd6358f     OTHER          pass      1 files
tokenize           OTHER          pass      1 files
tpch01             OTHER          fail      1 files — key
                                       and r_name = '...
trace2             OTHER          pass      1 files
trace3             OTHER          fail      1 files — ORDER BY a;\\}\\} -?d+ -?d+ -?d+ -?d+ -?d+ -?d+ -?d+ -?d+...
trustschema1       OTHER          pass      1 files, 2 tests skipped
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
analyze            PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze3           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze4           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze5           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze6           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze7           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze8           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyze9           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
analyzeC           PLANNER        fail      1 files, 6 tests skipped — database disk image is malformed
          sql: 
        ...
analyzeD           PLANNER        pass      1 files
analyzeE           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
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
bestindexA         PLANNER        pass      1 files
bestindexB         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexC         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexD         PLANNER        pass      1 files
bestindexE         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexF         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
bestindexG         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
cost               PLANNER        fail      1 files — --- FAIL: Test_cost (0.01s)
    cost_test.go:248: exec er...
cursorhint         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
eqp                PLANNER        fail      1 files — --- FAIL: Test_eqp (0.01s)
    eqp_test.go:404: exec erro...
pushdown           PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
scanstatus         PLANNER        skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
stat               PLANNER        pass      1 files, 2 tests skipped
statfault          PLANNER        pass      1 files
trace              PLANNER        fail      1 files — ]
          want: [SELECT '$::t6int', [$::t6int], 6, 6, "...
rtree              RTREE          pass      1 files
alter              SCHEMA         fail      1 files, 9 tests skipped — sql: 
            ALTER TABLE t3102b RENAME TO t3102b_ren...
alter2             SCHEMA         skipped   1 files, 1 whole-file skip (legacy file-format short-row tests (hexio helpers) not im...)
alter3             SCHEMA         pass      1 files
alter4             SCHEMA         pass      1 files
alterauth          SCHEMA         fail      1 files — --- FAIL: Test_alterauth (0.00s)
    alterauth_test.go:88...
alterauth2         SCHEMA         pass      1 files
altercol           SCHEMA         fail      1 files — or: database disk image is malformed
          sql: 
    ...
altercons          SCHEMA         fail      1 files, 13 tests skipped — s (0.02s)
    altercons_test.go:187: result mismatch
    ...
altercons2         SCHEMA         pass      1 files, 12 tests skipped
altercons3         SCHEMA         fail      1 files, 1 tests skipped — --- FAIL: Test_altercons3 (0.00s)
    altercons3_test.go:...
altercorrupt       SCHEMA         pass      1 files
alterdropcol       SCHEMA         fail      1 files — --- FAIL: Test_alterdropcol (0.00s)
    alterdropcol_test...
alterdropcol2      SCHEMA         pass      1 files
alterfault         SCHEMA         pass      1 files
alterlegacy        SCHEMA         fail      1 files, 13 tests skipped — --- FAIL: Test_alterlegacy (0.01s)
    alterlegacy_test.g...
altermalloc        SCHEMA         pass      1 files
altermalloc2       SCHEMA         pass      1 files
altermalloc3       SCHEMA         pass      1 files
alterqf            SCHEMA         pass      1 files, 1 tests skipped
altertab           SCHEMA         fail      1 files, 47 tests skipped — --- FAIL: Test_altertab (0.01s)
    altertab_test.go:367:...
altertab2          SCHEMA         pass      1 files, 3 tests skipped
altertab3          SCHEMA         pass      1 files, 14 tests skipped
altertrig          SCHEMA         pass      1 files
attach             SCHEMA         pass      1 files
attach2            SCHEMA         pass      1 files
attach3            SCHEMA         pass      1 files
attach4            SCHEMA         fail      1 files — --- FAIL: Test_attach4 (0.01s)
    attach4_test.go:157: r...
attachmalloc       SCHEMA         pass      1 files
autoinc            SCHEMA         pass      1 files
autovacuum         SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autovacuum2        SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
autovacuum_ioerr2  SCHEMA         pass      1 files
check              SCHEMA         pass      1 files
checkfault         SCHEMA         pass      1 files
collate1           SCHEMA         pass      1 files
collate2           SCHEMA         pass      1 files
collate3           SCHEMA         fail      1 files — --- FAIL: Test_collate3 (0.01s)
    collate3_test.go:457:...
collate4           SCHEMA         pass      1 files
collate5           SCHEMA         pass      1 files
collate6           SCHEMA         pass      1 files
collate7           SCHEMA         pass      1 files
collate8           SCHEMA         pass      1 files
collate9           SCHEMA         pass      1 files
collateA           SCHEMA         pass      1 files
collateB           SCHEMA         pass      1 files, 1 tests skipped
conflict           SCHEMA         fail      1 files — --- FAIL: Test_conflict (0.04s)
    conflict_test.go:873:...
coveridxscan       SCHEMA         pass      1 files, 4 tests skipped
createtab          SCHEMA         fail      1 files, 1 tests skipped — --- FAIL: Test_createtab (0.12s)
    createtab_test.go:93...
fkey1              SCHEMA         fail      1 files, 2 tests skipped — 1 (0.01s)
    fkey1_test.go:133: query error: no such tab...
fkey2              SCHEMA         fail      1 files — want: [SQLITE_UPDATE nought b main {} SQLITE_READ cross e...
fkey3              SCHEMA         pass      1 files
fkey4              SCHEMA         pass      1 files
fkey5              SCHEMA         fail      1 files, 13 tests skipped — --- FAIL: Test_fkey5 (0.04s)
    fkey5_test.go:583: resul...
fkey6              SCHEMA         fail      1 files — --- FAIL: Test_fkey6 (0.01s)
    fkey6_test.go:360: query...
fkey7              SCHEMA         pass      1 files
fkey8              SCHEMA         fail      1 files — KEY,
              pid REFERENCES p1(pid) ON UPDATE CASCA...
fkey_malloc        SCHEMA         pass      1 files
index              SCHEMA         fail      1 files — sql: 
            CREATE TABLE test1(a,b);
            CR...
index2             SCHEMA         pass      1 files
index3             SCHEMA         pass      1 files, 1 tests skipped
index4             SCHEMA         pass      1 files
index5             SCHEMA         fail      1 files — xec/engine_core.go:729 +0x448
github.com/pijalu/frigolite...
index6             SCHEMA         fail      1 files, 5 tests skipped — :149: query error: database disk image is malformed
     ...
index7             SCHEMA         fail      1 files, 5 tests skipped — e disk image is malformed
          sql: 
            CRE...
index8             SCHEMA         pass      1 files, 1 tests skipped
index9             SCHEMA         pass      1 files
indexA             SCHEMA         fail      1 files — sql: 
          CREATE INDEX ex1 ON t1(c) WHERE b IS 'abc...
indexedby          SCHEMA         pass      1 files, 2 tests skipped
indexexpr1         SCHEMA         fail      1 files, 40 tests skipped — expr1 (0.01s)
    indexexpr1_test.go:430: query error: da...
indexexpr2         SCHEMA         fail      1 files, 8 tests skipped — go:277: result mismatch
          got:  [0]
          wan...
indexexpr3         SCHEMA         pass      1 files
indexfault         SCHEMA         pass      1 files
notnull            SCHEMA         pass      1 files
notnull2           SCHEMA         pass      1 files
notnullfault       SCHEMA         pass      1 files
reindex            SCHEMA         pass      1 files
savepoint          SCHEMA         fail      1 files — want: [SQLITE_SAVEPOINT BEGIN sp1 {} {}]
          body: ...
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
trans              SCHEMA         pass      1 files
trans2             SCHEMA         pass      1 files
trans3             SCHEMA         pass      1 files
transitive1        SCHEMA         pass      1 files
trigger1           SCHEMA         pass      1 files
trigger2           SCHEMA         pass      1 files
trigger3           SCHEMA         pass      1 files
trigger4           SCHEMA         pass      1 files
trigger5           SCHEMA         pass      1 files
trigger6           SCHEMA         pass      1 files
trigger7           SCHEMA         pass      1 files
trigger8           SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
trigger9           SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
triggerA           SCHEMA         pass      1 files
triggerB           SCHEMA         pass      1 files
triggerC           SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
triggerD           SCHEMA         pass      1 files
triggerE           SCHEMA         pass      1 files
triggerF           SCHEMA         pass      1 files
triggerG           SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
triggerupfrom      SCHEMA         fail      1 files — --- FAIL: Test_triggerupfrom (0.01s)
    triggerupfrom_te...
unique             SCHEMA         pass      1 files
vacuum             SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum2            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum3            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum4            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum5            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum6            SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuum_into        SCHEMA         skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vacuummem          SCHEMA         pass      1 files, 1 tests skipped
rbu                SESSION        pass      1 files
session            SESSION        pass      1 files
amatch1            VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
carray01           VTAB           skipped   1 files, 1 whole-file skip (CARRAY extension not implemented N-A)
carray02           VTAB           skipped   1 files, 1 whole-file skip (CARRAY extension not implemented N-A)
carrayfault        VTAB           pass      1 files
dbpage             VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
dbpagefault        VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
intarray           VTAB           skipped   1 files, 1 whole-file skip (intarray extension not implemented N-A)
quota              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
quota2             VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
quota_glob         VTAB           skipped   1 files, 1 whole-file skip (quota VFS extension not implemented N-A)
spellfix           VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
spellfix2          VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
spellfix3          VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
spellfix4          VTAB           skipped   1 files, 1 whole-file skip (extension not implemented N-A)
swarmvtab          VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
swarmvtab2         VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
swarmvtab3         VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
swarmvtabfault     VTAB           pass      1 files
unionvtab          VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
unionvtabfault     VTAB           skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
vtab1              VTAB           fail      1 files, 26 tests skipped — # github.com/pijalu/frigolite/testgen/vtab1 [github.com/p...
vtab2              VTAB           pass      1 files, 15 tests skipped
vtab3              VTAB           fail      1 files — --- FAIL: Test_vtab3 (0.00s)
    vtab3_test.go:120: resul...
vtab4              VTAB           pass      1 files
vtab5              VTAB           pass      1 files
vtab6              VTAB           pass      1 files
vtab7              VTAB           skipped   1 files, 1 whole-file skip (echo module xSync callback trace (C test-module ABI) not ...)
vtab8              VTAB           pass      1 files
vtab9              VTAB           pass      1 files
vtabA              VTAB           pass      1 files
vtabB              VTAB           pass      1 files
vtabC              VTAB           pass      1 files
vtabD              VTAB           pass      1 files
vtabE              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtabF              VTAB           fail      1 files — INSERT INTO t1 VALUES(10,110);
            INSERT INTO t1...
vtabH              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtabI              VTAB           pass      1 files
vtabJ              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtabK              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtabL              VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtab_alter         VTAB           pass      1 files, 7 tests skipped
vtab_err           VTAB           pass      1 files
vtab_shared        VTAB           fail      1 files, 25 tests skipped — # github.com/pijalu/frigolite/testgen/vtab_shared [github...
vtabdistinct       VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtabdrop           VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
vtabrhs1           VTAB           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
zipfile            VTAB           skipped   1 files, 1 whole-file skip (zipfile extension not implemented N-A)
zipfile2           VTAB           skipped   1 files, 1 whole-file skip (zipfile extension not implemented N-A)
zipfilefault       VTAB           pass      1 files
jrnlmode           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
mjournal           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
nockpt             WAL            pass      1 files
rollback           WAL            skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rollback2          WAL            skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rollbackfault      WAL            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
subjournal         WAL            fail      1 files — --- FAIL: Test_subjournal (0.02s)
    subjournal_test.go:...
wal                WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal2               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal3               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal4               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal5               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal6               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal64k             WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
wal7               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal8               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
wal9               WAL            skipped   1 files, 1 whole-file skip (WAL journal mode not implemented N-A)
walbak             WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walbig             WAL            pass      1 files
walblock           WAL            pass      1 files
walckptnoop        WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcksum           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash2          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash3          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walcrash4          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walfault           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walfault2          WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walhook            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walmode            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walnoshm           WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
waloverwrite       WAL            pass      1 files
walpersist         WAL            pass      1 files
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
walsetlk_recover   WAL            fail      1 files — --- FAIL: Test_walsetlk_recover (0.01s)
    walsetlk_reco...
walsetlk_snapshot  WAL            fail      1 files — --- FAIL: Test_walsetlk_snapshot (0.00s)
    walsetlk_sna...
walshared          WAL            pass      1 files
walslow            WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
walthread          WAL            pass      1 files
walvfs             WAL            skipped   1 files, 1 whole-file skip (WAL/journal mode not implemented N-A)
