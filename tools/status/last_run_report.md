Frigolite testgen status  (generated 2026-09-19T23:22:27Z)

FAMILY              TOTAL   PASS   FAIL   SKIP     PCT
----------------------------------------------------------
AGG                     7      7      0      0  100.0%
C-API                  48     38      0     10   79.2%
CONCURRENCY            39     12      0     27   30.8%
CRUD                   59     59      0      0  100.0%
CTE-WINDOW             34     34      0      0  100.0%
EXPR                   43     41      0      2   95.3%
FTS                   240    170     23     47   70.8%
FUNCTIONS              25     25      0      0  100.0%
JOIN                   37     30      0      7   81.1%
JSON                   11     11      0      0  100.0%
ORDER                  26     25      0      1   96.2%
OTHER                 507    386     11    110   76.1%
PLANNER                38     19      0     19   50.0%
RTREE                  27     19      1      7   70.4%
SCHEMA                121    114      0      7   94.2%
SESSION                 2      2      0      0  100.0%
VTAB                   49     41      1      7   83.7%
WAL                    49     11      2     36   22.4%
----------------------------------------------------------
TOTAL                1362   1044     38    280   76.7%

PACKAGES
PKG                FAMILY         STATE     DETAIL
--------------------------------------------------------------------------------
aggerror           AGG            pass      1 files
aggfault           AGG            pass      1 files
aggnested          AGG            pass      1 files, 4 tests skipped
aggorderby         AGG            pass      1 files, 6 tests skipped
count              AGG            pass      1 files
countofview        AGG            pass      1 files
having             AGG            pass      1 files
backup             C-API          pass      1 files
backup2            C-API          pass      1 files
backup4            C-API          pass      1 files
backup5            C-API          pass      1 files
backup_ioerr       C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
backup_malloc      C-API          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
bind               C-API          pass      1 files
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
notify2            C-API          pass      1 files
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
lock               CONCURRENCY    pass      1 files
lock2              CONCURRENCY    pass      1 files
lock3              CONCURRENCY    pass      1 files
lock4              CONCURRENCY    skipped   1 files, 1 whole-file skip (two-process fixture emulation (test2-script.tcl subproces...)
lock5              CONCURRENCY    pass      1 files
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
shared             CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared2            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared3            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared4            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared6            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared7            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared8            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shared9            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
sharedA            CONCURRENCY    pass      1 files
sharedB            CONCURRENCY    pass      1 files
shared_err         CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
sharedlock         CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 5: shared-cache is a separate subsystem (glo...)
shmlock            CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (vfs_shmlock custom TCL command transpiles...)
snapshot           CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 4 superseded (C-API harness sqlite3_snapshot...)
snapshot2          CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 4 superseded (C-API harness sqlite3_snapshot...)
snapshot3          CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 4 superseded (C-API harness: cross-connectio...)
snapshot4          CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 4 superseded (testvfs-instrumented C-API har...)
snapshot_fault     CONCURRENCY    skipped   1 files, 1 whole-file skip (VFS fault-injection harness N-A (sqlite3_test_control FAU...)
snapshot_up        CONCURRENCY    skipped   1 files, 1 whole-file skip (N-A G7 slice 4 superseded (C-API harness sqlite3_snapshot...)
superlock          CONCURRENCY    skipped   1 files, 1 whole-file skip (WAL/shared-memory (sqlite3demo_superlock) not implemented...)
unixexcl           CONCURRENCY    pass      1 files
alias              CRUD           pass      1 files
all                CRUD           pass      1 files
default_pkg        CRUD           pass      1 files
delete2            CRUD           pass      1 files, 1 tests skipped
delete3            CRUD           pass      1 files
delete4            CRUD           pass      1 files
delete_db          CRUD           pass      1 files
delete_pkg         CRUD           pass      1 files, 3 tests skipped
emptytable         CRUD           pass      1 files
insert             CRUD           pass      1 files, 1 tests skipped
insert2            CRUD           pass      1 files
insert3            CRUD           pass      1 files
insert4            CRUD           pass      1 files, 6 tests skipped
insert5            CRUD           pass      1 files
insertfault        CRUD           pass      1 files
intpkey            CRUD           pass      1 files, 2 tests skipped
queryonly          CRUD           pass      1 files
returning1         CRUD           pass      1 files
returningfault     CRUD           pass      1 files
rowid              CRUD           pass      1 files, 9 tests skipped
select1            CRUD           pass      1 files, 3 tests skipped
select2            CRUD           pass      1 files, 3 tests skipped
select3            CRUD           pass      1 files, 1 tests skipped
select4            CRUD           pass      1 files, 2 tests skipped
select5            CRUD           pass      1 files, 1 tests skipped
select6            CRUD           pass      1 files
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
selectH            CRUD           pass      1 files, 1 tests skipped
tableopts          CRUD           pass      1 files
tempdb             CRUD           pass      1 files
temptable          CRUD           pass      1 files, 6 tests skipped
types              CRUD           pass      1 files
update             CRUD           pass      1 files, 1 tests skipped
update2            CRUD           pass      1 files, 1 tests skipped
upfrom1            CRUD           pass      1 files
upfrom2            CRUD           pass      1 files, 1 tests skipped
upfrom3            CRUD           pass      1 files
upfrom4            CRUD           pass      1 files
upfromfault        CRUD           pass      1 files
upsert1            CRUD           pass      1 files
upsert2            CRUD           pass      1 files
upsert3            CRUD           pass      1 files
upsert4            CRUD           pass      1 files
upsert5            CRUD           pass      1 files
upsertfault        CRUD           pass      1 files
values             CRUD           pass      1 files, 1 tests skipped
valuesfault        CRUD           pass      1 files
view               CRUD           pass      1 files, 2 tests skipped
view2              CRUD           pass      1 files
view3              CRUD           pass      1 files
filter1            CTE-WINDOW     pass      1 files
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
windowB            CTE-WINDOW     pass      1 files
windowC            CTE-WINDOW     pass      1 files
windowD            CTE-WINDOW     pass      1 files
windowE            CTE-WINDOW     pass      1 files, 1 tests skipped
windowerr          CTE-WINDOW     pass      1 files
windowfault        CTE-WINDOW     pass      1 files
windowpushd        CTE-WINDOW     pass      1 files
with1              CTE-WINDOW     pass      1 files, 9 tests skipped
with2              CTE-WINDOW     pass      1 files, 6 tests skipped
with3              CTE-WINDOW     pass      1 files
with4              CTE-WINDOW     pass      1 files
with5              CTE-WINDOW     pass      1 files
with6              CTE-WINDOW     pass      1 files
withM              CTE-WINDOW     pass      1 files
without_rowid1     CTE-WINDOW     pass      1 files
without_rowid2     CTE-WINDOW     pass      1 files
without_rowid3     CTE-WINDOW     pass      1 files, 13 tests skipped
without_rowid4     CTE-WINDOW     pass      1 files, 3 tests skipped
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
null               EXPR           pass      1 files
numcast            EXPR           pass      1 files
where              EXPR           pass      1 files, 9 tests skipped
where2             EXPR           pass      1 files, 7 tests skipped
where3             EXPR           pass      1 files
where4             EXPR           pass      1 files, 2 tests skipped
where5             EXPR           pass      1 files
where6             EXPR           pass      1 files
where7             EXPR           pass      1 files
where8             EXPR           skipped   1 files, 1 whole-file skip (hash/btree DISTINCT ordering fuzz N-A (where8-4.x SELECT ...)
where9             EXPR           skipped   1 files, 1 whole-file skip (count_steps harness proc (statement-count instrumentation...)
whereA             EXPR           pass      1 files, 2 tests skipped
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
whereL             EXPR           pass      1 files, 2 tests skipped
whereM             EXPR           pass      1 files
whereN             EXPR           pass      1 files
wherefault         EXPR           pass      1 files
wherelfault        EXPR           pass      1 files
wherelimit         EXPR           pass      1 files
wherelimit2        EXPR           pass      1 files, 7 tests skipped
wherelimit3        EXPR           pass      1 files
fts3               FTS            pass      1 files
fts3aa             FTS            pass      1 files
fts3ab             FTS            pass      1 files
fts3ac             FTS            pass      1 files
fts3ad             FTS            pass      1 files
fts3ae             FTS            pass      1 files
fts3af             FTS            pass      1 files
fts3ag             FTS            pass      1 files
fts3ah             FTS            skipped   1 files, 1 whole-file skip (tcl2go cannot inline the user TCL proc bigtermdoc - doc f...)
fts3ai             FTS            pass      1 files
fts3aj             FTS            pass      1 files, 1 tests skipped
fts3ak             FTS            pass      1 files
fts3al             FTS            pass      1 files
fts3am             FTS            pass      1 files
fts3an             FTS            pass      1 files, 3 tests skipped
fts3ao             FTS            skipped   1 files, 1 whole-file skip (engine gaps (T27 regen+run): snippet() renders leftmost c...)
fts3atoken         FTS            skipped   1 files, 1 whole-file skip (fts3_tokenizer() two-arg tokenizer registry (C function-p...)
fts3atoken2        FTS            skipped   1 files, 1 whole-file skip (fts3_tokenizer() two-arg tokenizer registry (C function-p...)
fts3auto           FTS            skipped   1 files, 1 whole-file skip (TCL-computed oracle harness (get_near_results/do_fts3quer...)
fts3aux1           FTS            skipped   1 files, 1 whole-file skip (fts4aux virtual table is a NoopModule stub (internal/vtab...)
fts3aux2           FTS            skipped   1 files, 1 whole-file skip (fts4aux virtual table is a NoopModule stub (internal/vtab...)
fts3b              FTS            fail      1 files — olite/frigolite_vacuum.go:73 +0x18c
github.com/pijalu/fri...
fts3c              FTS            pass      1 files
fts3comp1          FTS            pass      1 files
fts3conf           FTS            pass      1 files, 2 tests skipped
fts3corrupt        FTS            fail      1 files — sql: 
          CREATE VIRTUAL TABLE f using fts3(a,b);
 ...
fts3corrupt2       FTS            pass      1 files
fts3corrupt3       FTS            pass      1 files
fts3corrupt4       FTS            pass      1 files, 59 tests skipped
fts3corrupt5       FTS            pass      1 files
fts3corrupt6       FTS            pass      1 files, 1 tests skipped
fts3corrupt7       FTS            pass      1 files
fts3cov            FTS            pass      1 files
fts3d              FTS            pass      1 files
fts3defer          FTS            fail      1 files — o:821 +0x3c0
github.com/pijalu/frigolite.(*DB).Query(0x60...
fts3defer2         FTS            pass      1 files, 13 tests skipped
fts3defer3         FTS            pass      1 files, 1 tests skipped
fts3drop           FTS            pass      1 files
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
fts3fuzz001        FTS            pass      1 files, 5 tests skipped
fts3integrity      FTS            pass      1 files
fts3join           FTS            pass      1 files
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
fts3tok1           FTS            pass      1 files
fts3tok_err        FTS            pass      1 files
fts3varint         FTS            pass      1 files
fts4aa             FTS            pass      1 files
fts4check          FTS            fail      1 files — , 0x653a16610780, {0x1028e3319?, 0x4?}, {0x1028e67c2?, 0x...
fts4content        FTS            pass      1 files, 7 tests skipped
fts4docid          FTS            pass      1 files
fts4growth         FTS            pass      1 files, 12 tests skipped
fts4growth2        FTS            pass      1 files
fts4incr           FTS            pass      1 files
fts4intck1         FTS            pass      1 files
fts4langid         FTS            pass      1 files
fts4lastrowid      FTS            pass      1 files
fts4merge          FTS            fail      1 files — , 0x43b1e29896e0, {0x10309d68b?, 0xf?}, {0x0?, 0x10309c85...
fts4merge2         FTS            pass      1 files
fts4merge3         FTS            pass      1 files
fts4merge4         FTS            fail      1 files — c0
github.com/pijalu/frigolite.(*DB).Exec(0x706c9927cb40,...
fts4merge5         FTS            pass      1 files
fts4min            FTS            pass      1 files
fts4noti           FTS            pass      1 files
fts4onepass        FTS            pass      1 files
fts4opt            FTS            pass      1 files
fts4record         FTS            pass      1 files
fts4rename         FTS            pass      1 files
fts4umlaut         FTS            pass      1 files
fts4unicode        FTS            pass      1 files
fts4upfrom         FTS            pass      1 files
fts5aa             FTS            pass      1 files
fts5ab             FTS            pass      1 files
fts5ac             FTS            pass      1 files
fts5ad             FTS            pass      1 files
fts5ae             FTS            pass      1 files
fts5af             FTS            pass      1 files
fts5ag             FTS            pass      1 files
fts5ah             FTS            pass      1 files
fts5ai             FTS            pass      1 files
fts5aj             FTS            fail      1 files — /engine_core.go:821 +0x3c0
github.com/pijalu/frigolite.(*...
fts5ak             FTS            pass      1 files
fts5al             FTS            pass      1 files
fts5alter          FTS            pass      1 files
fts5auto           FTS            pass      1 files
fts5aux            FTS            skipped   1 files, 1 whole-file skip (8.x wants wrap multi-row highlight output in TCL quote ch...)
fts5aux2           FTS            pass      1 files
fts5auxdata        FTS            pass      1 files
fts5bigid          FTS            fail      1 files — e.go:801 +0x184
github.com/pijalu/frigolite.(*DB).Exec(0x...
fts5bigpl          FTS            fail      1 files — 0x104b00753, 0x1b}, {0x104b0a8a1, 0x2d}, 0x586d3b6cc480, ...
fts5bigtok         FTS            pass      1 files
fts5blob           FTS            pass      1 files
fts5cat            FTS            pass      1 files
fts5circref        FTS            fail      1 files — go:114: expected error containing "database disk image is...
fts5colset         FTS            skipped   1 files, 1 whole-file skip (5.2/5.3 wants strip the term quotes and colset braces C's...)
fts5columnsize     FTS            pass      1 files
fts5config         FTS            skipped   1 files, 1 whole-file skip (4.1.x reads the rank value through the 'first' UDF regist...)
fts5conflict       FTS            pass      1 files
fts5connect        FTS            pass      1 files
fts5content        FTS            fail      1 files — want: [one two]
    fts5content_test.go:642: result misma...
fts5contentless    FTS            fail      1 files — --- FAIL: Test_fts5contentless (0.01s)
    fts5contentles...
fts5contentless2   FTS            fail      1 files — lite.(*DB).Exec(0x354ef17887e0, {0x354ef2f7aaa0, 0x48})
	...
fts5contentless3   FTS            fail      1 files — [3]
          want: [200]
    fts5contentless3_test.go:27...
fts5contentless4   FTS            fail      1 files — --- FAIL: Test_fts5contentless4 (0.06s)
    fts5contentle...
fts5contentless5   FTS            pass      1 files
fts5corrupt        FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go Te...)
fts5corrupt2       FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go; e...)
fts5corrupt3       FTS            skipped   1 files, 1 whole-file skip (N-A harness (fts5_rnddoc C test UDF + fts5_common.tcl pro...)
fts5corrupt4       FTS            pass      1 files
fts5corrupt5       FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (sqlite3_deserialize + decode_hexdb pre-built...)
fts5corrupt6       FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go Te...)
fts5corrupt7       FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go Te...)
fts5corrupt8       FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go Te...)
fts5corruptbig     FTS            pass      1 files
fts5delete         FTS            fail      1 files — +0x3c0
github.com/pijalu/frigolite.(*DB).Exec(0x1fe259446...
fts5detail         FTS            skipped   1 files, 1 whole-file skip (3.x wants are the unresolved TCL variable literal "matchd...)
fts5determin       FTS            pass      1 files
fts5dlidx          FTS            pass      1 files
fts5doclist        FTS            pass      1 files
fts5ea             FTS            pass      1 files
fts5eb             FTS            pass      1 files
fts5expr           FTS            pass      1 files
fts5fault1         FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (C test-VFS fault-injection harness, PORTPLAN...)
fts5fault2         FTS            pass      1 files
fts5fault3         FTS            pass      1 files
fts5fault4         FTS            pass      1 files
fts5fault5         FTS            pass      1 files
fts5fault6         FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (C test-VFS fault-injection harness, PORTPLAN...)
fts5fault7         FTS            pass      1 files
fts5fault8         FTS            pass      1 files
fts5fault9         FTS            pass      1 files
fts5faultA         FTS            pass      1 files
fts5faultB         FTS            pass      1 files
fts5faultD         FTS            pass      1 files
fts5faultE         FTS            pass      1 files
fts5faultF         FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (C test-VFS fault-injection harness, PORTPLAN...)
fts5faultG         FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (C test-VFS fault-injection harness, PORTPLAN...)
fts5faultH         FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (C test-VFS fault-injection harness, PORTPLAN...)
fts5faultI         FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (C test-VFS fault-injection harness, PORTPLAN...)
fts5first          FTS            pass      1 files
fts5full           FTS            pass      1 files
fts5fuzz1          FTS            pass      1 files
fts5hash           FTS            fail      1 files — --- FAIL: Test_fts5hash (0.19s)
    fts5hash_test.go:137:...
fts5integrity      FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go Te...)
fts5integrity2     FTS            pass      1 files
fts5interrupt      FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5interrupt_test.go;...)
fts5join           FTS            pass      1 files
fts5lastrowid      FTS            pass      1 files
fts5leftjoin       FTS            fail      1 files — --- FAIL: Test_fts5leftjoin (0.01s)
    fts5leftjoin_test...
fts5limits         FTS            pass      1 files
fts5locale         FTS            skipped   1 files, 1 whole-file skip (N/A: all sections build tables with tokenize=tcl register...)
fts5matchinfo      FTS            pass      1 files
fts5merge          FTS            fail      1 files — .go:821 +0x3c0
github.com/pijalu/frigolite.(*DB).Exec(0x4...
fts5merge2         FTS            pass      1 files
fts5misc           FTS            fail      1 files — esult mismatch
          got:  [{}]
          want: [b wo...
fts5multi          FTS            pass      1 files
fts5multiclient    FTS            pass      1 files
fts5near           FTS            pass      1 files
fts5onepass        FTS            pass      1 files
fts5optimize       FTS            fail      1 files — github.com/pijalu/frigolite.(*DB).Exec(0x29e2a6d204e0, {0...
fts5optimize2      FTS            pass      1 files
fts5optimize3      FTS            pass      1 files
fts5origintext     FTS            pass      1 files
fts5origintext2    FTS            skipped   1 files, 1 whole-file skip (N/A: every section runs under the 'origintext' tokenizer ...)
fts5origintext3    FTS            pass      1 files
fts5origintext4    FTS            pass      1 files
fts5origintext5    FTS            skipped   1 files, 1 whole-file skip (N/A: same sqlite3_fts5_register_origintext harness class ...)
fts5origintext6    FTS            pass      1 files
fts5phrase         FTS            pass      1 files
fts5plan           FTS            pass      1 files
fts5porter         FTS            pass      1 files
fts5porter2        FTS            pass      1 files
fts5prefix         FTS            fail      1 files — x3c0
github.com/pijalu/frigolite.(*DB).Query(0x3af31e9e20...
fts5prefix2        FTS            pass      1 files
fts5query          FTS            pass      1 files
fts5rank           FTS            skipped   1 files, 1 whole-file skip (1.3's want drops the second string-map pair (y->[y]; the ...)
fts5rebuild        FTS            pass      1 files
fts5restart        FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5restart_test.go Te...)
fts5rowid          FTS            skipped   1 files, 1 whole-file skip (6.0-6.2 pin C's physical %_data block counts (32/34/36 de...)
fts5savepoint      FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5savepoint_test.go ...)
fts5secure         FTS            pass      1 files
fts5secure2        FTS            pass      1 files, 2 tests skipped
fts5secure3        FTS            skipped   1 files, 1 whole-file skip (N-A superseded (evidence frigolite_fts5corrupt_test.go; t...)
fts5secure4        FTS            pass      1 files
fts5secure5        FTS            pass      1 files
fts5secure6        FTS            skipped   1 files, 1 whole-file skip (N-A harness (the progress-handler proc is untranspilable ...)
fts5secure7        FTS            pass      1 files
fts5secure8        FTS            pass      1 files
fts5securefault    FTS            pass      1 files
fts5simple         FTS            pass      1 files, 2 tests skipped
fts5simple2        FTS            pass      1 files
fts5simple3        FTS            pass      1 files
fts5synonym        FTS            pass      1 files
fts5synonym2       FTS            pass      1 files
fts5tok1           FTS            skipped   1 files, 1 whole-file skip (1.13.2's explicit t1.* expansion includes the HIDDEN inpu...)
fts5tok2           FTS            pass      1 files
fts5tokendata      FTS            pass      1 files
fts5tokenizer      FTS            pass      1 files, 10 tests skipped
fts5tokenizer2     FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (fts5_tcl.c dynamic tokenizer registration sq...)
fts5tokenizer3     FTS            skipped   1 files, 1 whole-file skip (Genuine N/A (fts5_tcl.c dynamic tokenizer registration sq...)
fts5trigram        FTS            pass      1 files
fts5trigram2       FTS            pass      1 files
fts5ubsan          FTS            pass      1 files
fts5umlaut         FTS            pass      1 files
fts5unicode        FTS            pass      1 files
fts5unicode2       FTS            skipped   1 files, 1 whole-file skip (RUNAWAY — unbounded temp growth (~9G/min) in the curren...)
fts5unicode3       FTS            pass      1 files
fts5unicode4       FTS            pass      1 files
fts5unindexed      FTS            fail      1 files — --- FAIL: Test_fts5unindexed (0.01s)
    fts5unindexed_te...
fts5unindexed2     FTS            pass      1 files
fts5update         FTS            pass      1 files
fts5update2        FTS            pass      1 files
fts5version        FTS            pass      1 files
fts5vocab          FTS            pass      1 files
fts5vocab2         FTS            skipped   1 files, 1 whole-file skip (5.2's db-eval loop expects the write-conflict abort to br...)
fts_9fd058691      FTS            skipped   1 files, 1 whole-file skip (FTS3/4/5 beyond basic module N-A)
badutf             FUNCTIONS      pass      1 files
ctime              FUNCTIONS      pass      1 files
date               FUNCTIONS      pass      1 files
decimal            FUNCTIONS      pass      1 files, 18 tests skipped
func2              FUNCTIONS      pass      1 files
func3              FUNCTIONS      pass      1 files, 3 tests skipped
func4              FUNCTIONS      pass      1 files, 16 tests skipped
func5              FUNCTIONS      pass      1 files, 2 tests skipped
func6              FUNCTIONS      pass      1 files, 10 tests skipped
func7              FUNCTIONS      pass      1 files, 2 tests skipped
func8              FUNCTIONS      pass      1 files
func9              FUNCTIONS      pass      1 files
func_pkg           FUNCTIONS      pass      1 files, 54 tests skipped
icu                FUNCTIONS      pass      1 files
instr              FUNCTIONS      pass      1 files
instrfault         FUNCTIONS      pass      1 files
like               FUNCTIONS      pass      1 files
nan                FUNCTIONS      pass      1 files, 1 tests skipped
percentile         FUNCTIONS      pass      1 files
printf             FUNCTIONS      pass      1 files, 27 tests skipped
quote              FUNCTIONS      pass      1 files
substr             FUNCTIONS      pass      1 files
unhex              FUNCTIONS      pass      1 files
zeroblob           FUNCTIONS      pass      1 files
zeroblobfault      FUNCTIONS      pass      1 files
exists             JOIN           pass      1 files
existsexpr         JOIN           pass      1 files
existsexpr2        JOIN           pass      1 files
existsfault        JOIN           pass      1 files
full               JOIN           pass      1 files
join               JOIN           pass      1 files, 11 tests skipped
join2              JOIN           pass      1 files
join3              JOIN           pass      1 files
join4              JOIN           pass      1 files
join5              JOIN           pass      1 files
join6              JOIN           pass      1 files
join7              JOIN           pass      1 files
join8              JOIN           pass      1 files, 8 tests skipped
join9              JOIN           skipped   1 files, 1 whole-file skip (outer-join column synthesis: unmatched rows of the outer ...)
joinA              JOIN           pass      1 files
joinB              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinC              JOIN           pass      1 files
joinD              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinE              JOIN           pass      1 files
joinF              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinH              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
joinI              JOIN           skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
rowvalue           JOIN           pass      1 files
rowvalue2          JOIN           pass      1 files
rowvalue3          JOIN           pass      1 files
rowvalue4          JOIN           pass      1 files
rowvalue5          JOIN           skipped   1 files, 1 whole-file skip (TCL-implemented virtual table (register_tcl_module) N-A)
rowvalue6          JOIN           pass      1 files
rowvalue7          JOIN           pass      1 files
rowvalue8          JOIN           pass      1 files
rowvalue9          JOIN           pass      1 files
rowvalueA          JOIN           pass      1 files
rowvaluefault      JOIN           pass      1 files
rowvaluevtab       JOIN           pass      1 files
subquery           JOIN           pass      1 files, 7 tests skipped
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
json501            JSON           pass      1 files
json502            JSON           pass      1 files
jsonb01            JSON           pass      1 files
distinct           ORDER          pass      1 files
distinctagg        ORDER          pass      1 files
limit              ORDER          pass      1 files
minmax             ORDER          pass      1 files, 4 tests skipped
orderby1           ORDER          pass      1 files, 21 tests skipped
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
sort5              ORDER          pass      1 files
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
autoanalyze1       OTHER          skipped   1 files, 1 whole-file skip (N-A debug-build-only (PRAGMA stats; oracle skips; evidenc...)
autoindex1         OTHER          pass      1 files, 6 tests skipped
autoindex2         OTHER          pass      1 files
autoindex3         OTHER          pass      1 files, 4 tests skipped
autoindex4         OTHER          pass      1 files, 1 tests skipped
autoindex5         OTHER          pass      1 files
avfs               OTHER          fail      1 files — --- FAIL: Test_avfs (0.01s)
    avfs_test.go:244: result ...
avtrans            OTHER          fail      1 files — ine_core.go:821 +0x3c0
github.com/pijalu/frigolite.(*DB)....
backcompat         OTHER          pass      1 files
badutf2            OTHER          pass      1 files
basexx1            OTHER          pass      1 files
bigfile            OTHER          pass      1 files
bigfile2           OTHER          pass      1 files
bigmmap            OTHER          pass      1 files
bigrow             OTHER          fail      1 files — 89 i 9290 j 9291 k 9292 l 9293 m 9294 n 9295 o 9296 p 929...
bigsort            OTHER          pass      1 files
bitvec             OTHER          skipped   1 files, 1 whole-file skip (N/A: test-only C Bitvec self-test + SQLITE_MEMDEBUG mallo...)
bloom1             OTHER          pass      1 files
boundary1          OTHER          pass      1 files
boundary2          OTHER          pass      1 files
boundary3          OTHER          pass      1 files
boundary4          OTHER          pass      1 files
btree01            OTHER          pass      1 files
btree02            OTHER          pass      1 files
btreefault         OTHER          fail      1 files — --- FAIL: Test_btreefault (0.01s)
    btreefault_test.go:...
cache              OTHER          pass      1 files
cacheflush         OTHER          pass      1 files
cachespill         OTHER          pass      1 files
cffault            OTHER          pass      1 files
chunksize          OTHER          fail      1 files — --- FAIL: Test_chunksize (0.00s)
    chunksize_test.go:11...
cksumvfs           OTHER          pass      2 files
close_pkg          OTHER          pass      1 files
closure01          OTHER          pass      1 files, 3 tests skipped
colname            OTHER          pass      1 files
columncount        OTHER          pass      1 files
conflict2          OTHER          pass      1 files
conflict3          OTHER          pass      1 files
contrib01          OTHER          pass      1 files
corrupt            OTHER          fail      1 files — --- FAIL: Test_corrupt (20.20s)
    corrupt_test.go:462: ...
corrupt2           OTHER          pass      1 files, 2 tests skipped
corrupt3           OTHER          pass      1 files
corrupt4           OTHER          pass      1 files
corrupt5           OTHER          pass      1 files
corrupt6           OTHER          pass      1 files
corrupt7           OTHER          pass      1 files
corrupt8           OTHER          pass      1 files
corrupt9           OTHER          pass      1 files
corruptA           OTHER          pass      1 files
corruptB           OTHER          pass      1 files
corruptC           OTHER          skipped   1 files, 1 whole-file skip (transpiler fuzzer loss (proc random + string-compare earl...)
corruptD           OTHER          pass      1 files
corruptE           OTHER          pass      1 files
corruptF           OTHER          pass      1 files, 2 tests skipped
corruptG           OTHER          pass      1 files
corruptH           OTHER          pass      1 files
corruptI           OTHER          pass      1 files
corruptJ           OTHER          pass      1 files
corruptK           OTHER          pass      1 files
corruptL           OTHER          pass      1 files, 10 tests skipped
corruptM           OTHER          pass      1 files
corruptN           OTHER          pass      1 files, 7 tests skipped
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
csv01              OTHER          pass      1 files
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
e_droptrigger      OTHER          pass      1 files
e_dropview         OTHER          pass      1 files
e_expr             OTHER          skipped   1 files, 1 whole-file skip (typed-value operator matrix sections (6.x '||' concat pai...)
e_fkey             OTHER          pass      1 files
e_fts3             OTHER          pass      1 files, 13 tests skipped
e_insert           OTHER          pass      1 files
e_reindex          OTHER          pass      1 files, 1 tests skipped
e_resolve          OTHER          pass      1 files
e_select           OTHER          skipped   1 files, 1 whole-file skip (DISTINCT collation ordering P1.E-SQL deep gap N-A (e_sele...)
e_select2          OTHER          pass      1 files
e_totalchanges     OTHER          pass      1 files
e_update           OTHER          skipped   1 files, 1 whole-file skip (UPDATE aux schema + trigger cascade P1.E-SQL deep gap N-A)
e_uri              OTHER          skipped   1 files, 1 whole-file skip (C test-VFS sqlite3_open_v2 URI probing (testvfs vfs1/vfs2...)
e_vacuum           OTHER          skipped   1 files, 1 whole-file skip (VACUUM aux (attached-db vacuum) unimplemented ('unknown d...)
e_wal              OTHER          skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
e_walauto          OTHER          skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
e_walckpt          OTHER          skipped   1 files, 1 whole-file skip (N-A G7 (evidence internal/pager/walview_test.go + portpla...)
e_walhook          OTHER          skipped   1 files, 1 whole-file skip (db wal_hook TCL-proc callback (sqlite3_wal_hook seam) unt...)
enc                OTHER          pass      1 files
enc2               OTHER          pass      1 files
enc3               OTHER          skipped   1 files, 1 whole-file skip (UTF-16 storage not implemented N-A (evidence frigolite_en...)
enc4               OTHER          pass      1 files
eqp2               OTHER          pass      1 files
errmsg             OTHER          pass      1 files, 1 tests skipped
errofst1           OTHER          pass      1 files
eval               OTHER          skipped   1 files, 1 whole-file skip (the eval-2.x section drives DELETE/UPDATE through test_ev...)
exclusive2         OTHER          pass      1 files
extension01        OTHER          pass      1 files
external_reader    OTHER          pass      1 files
extraquick         OTHER          pass      1 files
fallocate          OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
filectrl           OTHER          pass      1 files
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
in3                OTHER          pass      1 files
in4                OTHER          pass      1 files, 5 tests skipped
in5                OTHER          pass      1 files
in6                OTHER          pass      1 files, 2 tests skipped
in7                OTHER          pass      1 files
init               OTHER          skipped   1 files, 1 whole-file skip (N-A harness (sqlite3_initialize/sqlite3_shutdown + test_i...)
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
keyword1           OTHER          pass      1 files
like2              OTHER          pass      1 files
like3              OTHER          pass      1 files
limit2             OTHER          pass      1 files
literal2           OTHER          pass      1 files
loadext            OTHER          pass      1 files
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
minmax2            OTHER          pass      1 files, 4 tests skipped
minmax3            OTHER          pass      1 files
minmax4            OTHER          pass      1 files
misc1              OTHER          pass      1 files, 3 tests skipped
misc2              OTHER          pass      1 files
misc3              OTHER          pass      1 files, 1 tests skipped
misc4              OTHER          pass      1 files, 8 tests skipped
misc5              OTHER          pass      1 files
misc6              OTHER          pass      1 files
misc7              OTHER          pass      1 files, 19 tests skipped
misc8              OTHER          pass      1 files, 1 tests skipped
misuse             OTHER          pass      1 files
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
offset1            OTHER          skipped   1 files, 1 whole-file skip (LIMIT/OFFSET over compound (UNION ALL) selects applies pe...)
openv2             OTHER          pass      1 files
oserror            OTHER          skipped   1 files, 1 whole-file skip (N/A: C VFS syscall injection via test_syscall + sqlite3_l...)
ovfl               OTHER          pass      1 files
p_8_3_names        OTHER          pass      1 files
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
pcache             OTHER          pass      1 files
pcache2            OTHER          pass      1 files, 2 tests skipped
permutations       OTHER          pass      1 files
pragma             OTHER          pass      1 files, 13 tests skipped
pragma2            OTHER          pass      1 files
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
qrf01              OTHER          skipped   1 files, 1 whole-file skip (N/A: db format = shell Query Result Formatter (QRF), CLI-...)
qrf02              OTHER          skipped   1 files, 1 whole-file skip (N/A: db format = shell Query Result Formatter (QRF), EXPL...)
qrf03              OTHER          skipped   1 files, 1 whole-file skip (N/A: db format = shell Query Result Formatter (QRF), styl...)
qrf04              OTHER          pass      1 files
qrf05              OTHER          pass      1 files
qrf06              OTHER          pass      1 files
quick              OTHER          pass      1 files
quickcheck         OTHER          pass      1 files
randexpr1          OTHER          pass      1 files
rdonly             OTHER          pass      1 files
readonly           OTHER          pass      1 files
recover_pkg        OTHER          pass      1 files
regexp1            OTHER          pass      1 files
regexp2            OTHER          pass      1 files
reservebytes       OTHER          pass      1 files
resetdb            OTHER          pass      1 files
resolver01         OTHER          pass      1 files
round1             OTHER          pass      1 files
rowhash            OTHER          pass      1 files
scanstatus2        OTHER          pass      1 files, 1 tests skipped
securedel          OTHER          pass      1 files
securedel2         OTHER          pass      1 files
seekscan1          OTHER          pass      1 files
shell1             OTHER          pass      1 files
shell2             OTHER          pass      1 files
shell3             OTHER          pass      1 files
shell4             OTHER          pass      1 files
shell5             OTHER          pass      1 files
shell6             OTHER          pass      1 files
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
speed1             OTHER          pass      1 files
speed1p            OTHER          pass      1 files
speed2             OTHER          pass      1 files
speed3             OTHER          pass      1 files
speed4             OTHER          skipped   1 files, 1 whole-file skip (execution-speed benchmark N-A)
speed4p            OTHER          pass      1 files
sqldiff1           OTHER          skipped   1 files, 1 whole-file skip (N/A: external sqldiff tool binary seam (test_find_sqldiff...)
sqllimits1         OTHER          pass      1 files, 1 tests skipped
starschema1        OTHER          skipped   1 files, 1 whole-file skip (EQP join-order: planner lacks star-schema fact-first reor...)
strict1            OTHER          pass      1 files
strict2            OTHER          pass      1 files
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
thread003          OTHER          pass      1 files
thread004          OTHER          pass      1 files
thread005          OTHER          pass      1 files
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
tkt2332            OTHER          pass      1 files
tkt2339            OTHER          pass      1 files
tkt2391            OTHER          pass      1 files
tkt2409            OTHER          skipped   1 files, 1 whole-file skip (cache-spill lock-failure simulation (read_lock_db harness...)
tkt2450            OTHER          pass      1 files
tkt2565            OTHER          pass      1 files, 1 tests skipped
tkt2640            OTHER          pass      1 files
tkt2643            OTHER          pass      1 files
tkt2686            OTHER          pass      1 files
tkt2767            OTHER          pass      1 files
tkt2817            OTHER          pass      1 files
tkt2820            OTHER          pass      1 files
tkt2822            OTHER          pass      1 files
tkt2832            OTHER          pass      1 files
tkt2854            OTHER          skipped   1 files, 1 whole-file skip (shared-cache multi-connection concurrency not implemented...)
tkt2920            OTHER          pass      1 files
tkt2927            OTHER          pass      1 files
tkt2942            OTHER          pass      1 files
tkt3080            OTHER          pass      1 files
tkt3093            OTHER          skipped   1 files, 1 whole-file skip (multi-connection busy-handler locking not implemented DEF...)
tkt3121            OTHER          pass      1 files
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
tkt3508            OTHER          pass      1 files
tkt3522            OTHER          pass      1 files
tkt3527            OTHER          pass      1 files
tkt3541            OTHER          pass      1 files
tkt3554            OTHER          pass      1 files
tkt3581            OTHER          pass      1 files
tkt35xx            OTHER          pass      1 files
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
tkt3935            OTHER          pass      1 files
tkt3992            OTHER          pass      1 files
tkt3997            OTHER          pass      1 files
tkt4018            OTHER          pass      1 files
tkt_02a8e81d44     OTHER          pass      1 files
tkt_18458b1a       OTHER          pass      1 files
tkt_26ff0c2d1e     OTHER          pass      1 files
tkt_2a5629202f     OTHER          pass      1 files
tkt_2d1a5c67d      OTHER          pass      1 files
tkt_2ea2425d34     OTHER          pass      1 files
tkt_31338dca7e     OTHER          pass      1 files
tkt_313723c356     OTHER          pass      1 files
tkt_385a5b56b9     OTHER          pass      1 files
tkt_38cb5df375     OTHER          pass      1 files
tkt_3998683a16     OTHER          pass      1 files
tkt_3a77c9714e     OTHER          pass      1 files
tkt_3fe897352e     OTHER          skipped   1 files, 1 whole-file skip (UTF-16 hex test-harness functions N-A)
tkt_4a03edc4c8     OTHER          pass      1 files
tkt_4c86b126f2     OTHER          pass      1 files
tkt_4dd95f6943     OTHER          pass      1 files
tkt_4ef7e3cfca     OTHER          pass      1 files
tkt_54844eea3f     OTHER          pass      1 files
tkt_5d863f876e     OTHER          pass      1 files
tkt_5e10420e8d     OTHER          pass      1 files
tkt_5ee23731f      OTHER          pass      1 files
tkt_6bfb98dfc0     OTHER          pass      1 files
tkt_752e1646fc     OTHER          pass      1 files
tkt_78e04e52ea     OTHER          pass      1 files, 1 tests skipped
tkt_7a31705a7e6    OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
tkt_7bbfb7d442     OTHER          skipped   1 files, 1 whole-file skip (deep-engine applicable gap DEFERRED (tracked for later ph...)
tkt_80ba201079     OTHER          pass      1 files, 1 tests skipped
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
tkt_bd484a090c     OTHER          pass      1 files
tkt_bdc6bbbb38     OTHER          skipped   1 files, 1 whole-file skip (FTS4 virtual table not implemented N-A)
tkt_c48d99d690     OTHER          pass      1 files
tkt_c694113d5      OTHER          pass      1 files
tkt_cbd054fa6b     OTHER          pass      1 files
tkt_d11f09d36e     OTHER          fail      1 files — /pijalu/frigolite.(*DB).Exec(0x73565e2b4720, {0x102d4614b...
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
tpch01             OTHER          pass      1 files
trace2             OTHER          pass      1 files
trace3             OTHER          pass      1 files, 4 tests skipped
trustschema1       OTHER          pass      1 files, 2 tests skipped
types2             OTHER          pass      1 files
types3             OTHER          pass      1 files
unique2            OTHER          pass      1 files
uri                OTHER          skipped   1 files, 1 whole-file skip (converter gaps: file-isdir helper + error-variable emissi...)
uri2               OTHER          skipped   1 files, 1 whole-file skip (converter + engine gaps: %00-in-URI rejection (ENABLE_URI...)
utf16align         OTHER          pass      1 files
varint             OTHER          pass      1 files
veryquick          OTHER          pass      1 files
widetab1           OTHER          pass      1 files
win32heap          OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
win32lock          OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
win32longpath      OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
win32nolock        OTHER          skipped   1 files, 1 whole-file skip (win32 platform-specific tests N-A)
writecrash         OTHER          skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
zerodamage         OTHER          pass      1 files
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
bestindex1         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex2         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex3         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex4         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex5         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex6         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex7         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex8         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindex9         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexA         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexB         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexC         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexD         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexE         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexF         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
bestindexG         PLANNER        skipped   1 files, 1 whole-file skip (N-A register_tcl_module harness (evidence frigolite_besti...)
cost               PLANNER        pass      1 files
cursorhint         PLANNER        skipped   1 files, 1 whole-file skip (VDBE codeCursorHint() opcode P4 introspection + MySQL pus...)
eqp                PLANNER        pass      1 files
pushdown           PLANNER        skipped   1 files, 1 whole-file skip (VDBE codeCursorHint() opcode P4 introspection + MySQL pus...)
scanstatus         PLANNER        skipped   1 files, 1 whole-file skip (sqlite3_stmt_scanstatus/sqlite3_db_scanstatus C-API intro...)
stat               PLANNER        pass      1 files
statfault          PLANNER        pass      1 files
trace              PLANNER        pass      1 files, 5 tests skipped
rtree              RTREE          pass      1 files
rtree1             RTREE          pass      1 files
rtree2             RTREE          pass      1 files
rtree3             RTREE          pass      1 files
rtree4             RTREE          pass      1 files
rtree5             RTREE          pass      1 files
rtree6             RTREE          pass      1 files
rtree7             RTREE          pass      1 files
rtree8             RTREE          skipped   1 files, 1 whole-file skip (N-A SQLITE_LOCKED_VTAB cursor-write lock unobservable thr...)
rtree9             RTREE          pass      1 files
rtreeA             RTREE          skipped   1 files, 1 whole-file skip (N-A set_tree_depth binary blob surgery N-A (evidence frig...)
rtreeB             RTREE          pass      1 files
rtreeC             RTREE          pass      1 files
rtreeD             RTREE          pass      1 files
rtreeE             RTREE          pass      1 files
rtreeF             RTREE          pass      1 files
rtreeG             RTREE          pass      1 files
rtreeH             RTREE          fail      1 files — ec/engine_core.go:821 +0x3c0
github.com/pijalu/frigolite....
rtreeI             RTREE          pass      1 files
rtreeJ             RTREE          skipped   1 files, 1 whole-file skip (N-A db-eval callback procs (restore_t1) N-A (evidence fri...)
rtreecheck         RTREE          pass      1 files
rtreecirc          RTREE          pass      1 files
rtreeconnect       RTREE          pass      1 files
rtreedoc           RTREE          skipped   1 files, 1 whole-file skip (N-A rtree_util.tcl procs (column_size/count/name_list) + ...)
rtreedoc2          RTREE          skipped   1 files, 1 whole-file skip (N-A register_box_geom wraps a TCL-script callback (invoke...)
rtreedoc3          RTREE          skipped   1 files, 1 whole-file skip (N-A register_box_query untranspiled — the generated inp...)
rtreefuzz001       RTREE          skipped   1 files, 1 whole-file skip (N-A database_may_be_corrupt stale matchers + untranspilab...)
alter              SCHEMA         pass      1 files, 11 tests skipped
alter2             SCHEMA         skipped   1 files, 1 whole-file skip (legacy file-format short-row semantics require the hexio ...)
alter3             SCHEMA         pass      1 files
alter4             SCHEMA         pass      1 files
alterauth          SCHEMA         skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_alterauth_pin_tes...)
alterauth2         SCHEMA         pass      1 files
altercol           SCHEMA         pass      1 files, 2 tests skipped
altercons          SCHEMA         pass      1 files, 13 tests skipped
altercons2         SCHEMA         pass      1 files, 12 tests skipped
altercons3         SCHEMA         pass      1 files, 1 tests skipped
altercorrupt       SCHEMA         pass      1 files
alterdropcol       SCHEMA         pass      1 files
alterdropcol2      SCHEMA         pass      1 files
alterfault         SCHEMA         pass      1 files
alterlegacy        SCHEMA         pass      1 files, 15 tests skipped
altermalloc        SCHEMA         pass      1 files
altermalloc2       SCHEMA         pass      1 files
altermalloc3       SCHEMA         pass      1 files
alterqf            SCHEMA         pass      1 files, 1 tests skipped
altertab           SCHEMA         pass      1 files, 53 tests skipped
altertab2          SCHEMA         pass      1 files, 3 tests skipped
altertab3          SCHEMA         pass      1 files, 14 tests skipped
altertrig          SCHEMA         pass      1 files
attach             SCHEMA         pass      1 files
attach2            SCHEMA         pass      1 files
attach3            SCHEMA         pass      1 files
attach4            SCHEMA         pass      1 files
attachmalloc       SCHEMA         pass      1 files
autoinc            SCHEMA         pass      1 files, 2 tests skipped
autovacuum         SCHEMA         pass      1 files
autovacuum2        SCHEMA         pass      1 files, 4 tests skipped
autovacuum_ioerr2  SCHEMA         pass      1 files
check              SCHEMA         pass      1 files
checkfault         SCHEMA         pass      1 files
collate1           SCHEMA         pass      1 files
collate2           SCHEMA         pass      1 files
collate3           SCHEMA         pass      1 files
collate4           SCHEMA         pass      1 files
collate5           SCHEMA         pass      1 files
collate6           SCHEMA         pass      1 files
collate7           SCHEMA         pass      1 files
collate8           SCHEMA         pass      1 files
collate9           SCHEMA         pass      1 files
collateA           SCHEMA         pass      1 files
collateB           SCHEMA         pass      1 files
conflict           SCHEMA         pass      1 files
coveridxscan       SCHEMA         pass      1 files, 4 tests skipped
createtab          SCHEMA         pass      1 files, 1 tests skipped
fkey1              SCHEMA         pass      1 files, 2 tests skipped
fkey2              SCHEMA         pass      1 files, 8 tests skipped
fkey3              SCHEMA         pass      1 files
fkey4              SCHEMA         pass      1 files
fkey5              SCHEMA         pass      1 files, 13 tests skipped
fkey6              SCHEMA         pass      1 files
fkey7              SCHEMA         pass      1 files
fkey8              SCHEMA         pass      1 files
fkey_malloc        SCHEMA         pass      1 files
index              SCHEMA         pass      1 files
index2             SCHEMA         pass      1 files
index3             SCHEMA         pass      1 files, 1 tests skipped
index4             SCHEMA         pass      1 files
index5             SCHEMA         pass      1 files
index6             SCHEMA         pass      1 files, 4 tests skipped
index7             SCHEMA         pass      1 files, 4 tests skipped
index8             SCHEMA         pass      1 files, 1 tests skipped
index9             SCHEMA         pass      1 files
indexA             SCHEMA         pass      1 files, 4 tests skipped
indexedby          SCHEMA         pass      1 files, 2 tests skipped
indexexpr1         SCHEMA         pass      1 files, 40 tests skipped
indexexpr2         SCHEMA         pass      1 files, 12 tests skipped
indexexpr3         SCHEMA         pass      1 files
indexfault         SCHEMA         pass      1 files
notnull            SCHEMA         pass      1 files
notnull2           SCHEMA         pass      1 files
notnullfault       SCHEMA         pass      1 files
reindex            SCHEMA         pass      1 files
savepoint          SCHEMA         pass      1 files, 5 tests skipped
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
trans2             SCHEMA         skipped   1 files, 1 whole-file skip (N/A: performance-limited - per-statement pager-snapshot/j...)
trans3             SCHEMA         pass      1 files
transitive1        SCHEMA         pass      1 files
trigger1           SCHEMA         pass      1 files
trigger2           SCHEMA         pass      1 files
trigger3           SCHEMA         pass      1 files
trigger4           SCHEMA         pass      1 files
trigger5           SCHEMA         pass      1 files
trigger6           SCHEMA         pass      1 files
trigger7           SCHEMA         pass      1 files
trigger8           SCHEMA         pass      1 files
trigger9           SCHEMA         pass      1 files
triggerA           SCHEMA         pass      1 files
triggerB           SCHEMA         pass      1 files
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
intarray           VTAB           fail      1 files — _core.go:821 +0x3c0
github.com/pijalu/frigolite.(*DB).Exe...
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
vtab1              VTAB           pass      1 files, 26 tests skipped
vtab2              VTAB           pass      1 files, 15 tests skipped
vtab3              VTAB           pass      1 files
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
vtabE              VTAB           pass      1 files
vtabF              VTAB           pass      1 files
vtabH              VTAB           pass      1 files
vtabI              VTAB           pass      1 files
vtabJ              VTAB           pass      1 files, 3 tests skipped
vtabK              VTAB           pass      1 files
vtabL              VTAB           pass      1 files
vtab_alter         VTAB           pass      1 files, 7 tests skipped
vtab_err           VTAB           pass      1 files
vtab_shared        VTAB           pass      1 files, 26 tests skipped
vtabdistinct       VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_vtabdistinct_test...)
vtabdrop           VTAB           pass      1 files, 6 tests skipped
vtabrhs1           VTAB           skipped   1 files, 1 whole-file skip (superseded by native Go port (frigolite_vtabrhs1_test.go))
zipfile            VTAB           pass      1 files
zipfile2           VTAB           pass      1 files
zipfilefault       VTAB           pass      1 files
jrnlmode           WAL            pass      1 files
mjournal           WAL            skipped   1 files, 1 whole-file skip (master-journal pointer validation in hot-journal recovery...)
nockpt             WAL            pass      1 files
rollback           WAL            pass      1 files
rollback2          WAL            pass      1 files
rollbackfault      WAL            skipped   1 files, 1 whole-file skip (VFS/fault-injection harness N-A)
subjournal         WAL            pass      1 files
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
walcrash           WAL            skipped   1 files, 1 whole-file skip (crashsql mid-WAL-write crash-recovery simulation N-A (cra...)
walcrash2          WAL            skipped   1 files, 1 whole-file skip (crashsql mid-WAL-write crash-recovery simulation N-A (cra...)
walcrash3          WAL            skipped   1 files, 1 whole-file skip (crashsql crash simulation + testvfs VFS instrumentation N...)
walcrash4          WAL            skipped   1 files, 1 whole-file skip (faultsim (sqlite3_test_control) fault-injection harness N...)
walfault           WAL            skipped   1 files, 1 whole-file skip (faultsim (sqlite3_test_control) fault-injection harness (...)
walfault2          WAL            skipped   1 files, 1 whole-file skip (faultsim (sqlite3_test_control) fault-injection harness N...)
walhook            WAL            skipped   1 files, 1 whole-file skip (db wal_hook TCL-proc callback (sqlite3_wal_hook seam) unt...)
walmode            WAL            skipped   1 files, 1 whole-file skip (VFS sync-count + byte-exact file-size instrumentation (wa...)
walnoshm           WAL            skipped   1 files, 1 whole-file skip (testvfs -iversion 1 custom VFS (WAL requires locking_mode...)
waloverwrite       WAL            pass      1 files
walpersist         WAL            fail      1 files — pplied
          sql:  INSERT INTO t1 VALUES(randomblob(5...
walprotocol        WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 1 superseded (xShmLock sequence instrumentat...)
walprotocol2       WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 1 superseded (testvfs two-connection harness...)
walrestart         WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (sqlite3_test_control faultsim injection d...)
walro              WAL            pass      1 files
walro2             WAL            pass      1 files
walrofault         WAL            pass      1 files
walseh1            WAL            skipped   1 files, 1 whole-file skip (SEH fault-injection via sqlite3_test_control_fault_instal...)
walsetlk           WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (testvfs -fullshm + xSleep counting for bl...)
walsetlk2          WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (sqlite3_setlk_timeout C-API + db .timeout...)
walsetlk3          WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (sqlite3_setlk_timeout blocking-lock C-API...)
walsetlk_recover   WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (testfixture_nb subprocess + testvfs -full...)
walsetlk_snapshot  WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 2 (testfixture_nb + testvfs -fullshm harness...)
walshared          WAL            skipped   1 files, 1 whole-file skip (N-A G7 slice 5 (WAL + shared-cache needs the btree table-...)
walslow            WAL            skipped   1 files, 1 whole-file skip (reopen_db close/reopen churn + save/restore_prng_state ha...)
walthread          WAL            pass      1 files
walvfs             WAL            skipped   1 files, 1 whole-file skip (testvfs xSync-count + IOCAP_SEQUENTIAL + -iversion 2 VFS ...)
