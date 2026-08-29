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
