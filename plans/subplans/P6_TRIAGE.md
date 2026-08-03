# Sub-Plan: P6 — Triage (3 sub-goals)

> **Prerequisite**: P1–P5 complete.
> **Packages**: ~367 uncategorized

---

## G6.NA — Not Applicable (document ~125 packages)

### Goal
```
Objective: Formally document and exclude the ~125 packages that test SQLite C
internals not applicable to a pure-Go reimplementation.
Completion criterion: plans/NOT_APPLICABLE.md exists with every excluded package
and a one-line reason; the skipFiles mechanism documents exclusions (not hides bugs).
Verify: test -f plans/NOT_APPLICABLE.md && grep -c "^|" plans/NOT_APPLICABLE.md
Fresh context: true
```

### Categories to document:

| Category | Packages | Reason |
|----------|----------|--------|
| C API | capi, capi3, bind, bindxfer, tableapi, tclsqlite, backup* | Tests sqlite3_prepare/step/finalize C functions — frigolite has no C API |
| Fault injection | *fault, *malloc*, *ioerr, *crash, *fuzz*, sysfault, diskfull | Tests OOM/error injection — frigolite has no fault simulator |
| Corruption | corrupt–corruptN, *corrupt* | Tests SQLite file-format corruption detection — requires byte-level corruption tooling |
| Custom VFS | avfs, cksumvfs, tvfs, vfs, unixexcl, multiplex, mmap*, memjournal, journal, jrnlmode, etc. | Tests custom Virtual File System layers — frigolite uses Go I/O directly |
| Shell | shell, shellA, shellB | Tests the sqlite3 CLI shell — frigolite has its own CLI |
| Build/config | bitvec, atomic, ctime, keyword, memsubsys, pcache, lookaside, varint | Tests SQLite internal data structures/algorithms — frigolite has its own |
| Performance | speed, speed1, speed4, soak, veryquick, extraquick, quick | Stress/performance benchmarks — not functional tests |
| Platform | win32, utf16, enc, badutf, basexx, symlink | Platform-specific tests (Windows, UTF-16 encoding) |

### Steps
- [x] **1. Write `plans/NOT_APPLICABLE.md`** with full categorized list.
      Commit: `G6.NA.1: document not-applicable test packages`
- [x] **2. Update the harness exclusion map (`unsupportedTestFiles` in
      `frigolite_harness_test.go`)** — the "skipFiles" mechanism in this codebase.
      Repurposed to explicitly document exclusions with reasons (not hide bugs);
      entries are grouped by category with comments. 4 stale entries removed
      (bigrow2, fts3defer3, fts3expr, fts4merge2); 252 N/A testdata packages added.
      Commit: `G6.NA.2: repurpose skipFiles as documented exclusion list`

---

## G6.DEFERRED — Deferred WAL/Concurrency (~33 packages)

### Goal
```
Objective: Document the ~33 WAL/concurrency packages as deferred (future work),
not excluded. They remain as failing tests driving future WAL implementation.
Completion criterion: plans/DEFERRED.md documents each deferred package and the
feature it requires.
Verify: test -f plans/DEFERRED.md
Fresh context: true
```

### Packages:
- **WAL** (30): wal, wal64, walbak, walbig, walblock, walckptnoop, walcksum,
  walcrash, walfault, walhook, walmode, walnoshm, waloverwrite, walpersist,
  walprotocol, walrestart, walro, walrofault, walseh, walsetlk, walsetlk_,
  walshared, walslow, walthread, walvfs, nockpt, snapshot, snapshot_
- **Concurrency**: thread, walthread, mutex, shared, shared_, sharedA, sharedB,
  sharedlock

### Why deferred:
- WAL mode requires a completely different journaling and concurrency model.
- Frigolite uses rollback journal only.
- These are legitimate future goals, not exclusions.
- They remain as `t.Errorf` (failing) — visible signal for future work.

### Steps
1. **Write `plans/DEFERRED.md`**.
   Commit: `G6.DEFERRED.1: document deferred WAL/concurrency packages`

---

## G6.MISC — Applicable Misc (~209 packages)

### Goal
```
Objective: Make all applicable misc test packages PASS. These test real SQL
functionality not covered by P1-P5 categories.
Completion criterion: All applicable packages in the 6c list PASS.
Verify: bash scripts/verify_all_applicable.sh (per-package, with timeout)
Fresh context: true
```

### Sub-groups (each a batch goal):

#### G6.MISC.tkt — Ticket regression tests (~70 packages)
`tkt`, `tkt35`, `tkt_*` (68 packages)

These are SQLite bug-fix regression tests. Each tests a specific historical bug.
Most exercise real SQL. Batch approach:
1. Run all tkt_* packages, capture failures.
2. Group failures by root cause (many will share causes).
3. Fix root causes, re-run affected packages.
4. Commit per root-cause fix.

#### G6.MISC.rowvalue — Row value / vector operations
`rowvalue`, `rowvalueA`, `rowvaluevtab`
- Vector expressions: `(a,b) IN (SELECT x,y FROM t)`
- Row value comparisons: `(a,b) < (c,d)`

#### G6.MISC.index — Index features
`bloom`, `coveridxscan`, `descidx`, `expridx`, `numindex`, `seekscan`, `skipscan`
- Covering index scans
- Descending indexes
- Skip-scan optimization
- Expression indexes
- Bloom filter

#### G6.MISC.misc — General SQL tests
`alias`, `auth`, `autoinc`, `btree`, `cache`, `changes`, `colmeta`, `colname`,
`columncount`, `createtab`, `cursorhint`, `dataversion`, `emptytable`, `errmsg`,
`errofst`, `eval`, `exclusive`, `exec`, `external_`, `fordelete`, `format`,
`func_pkg`, `gencol`, `imposter`, `in`, `io`, `lastinsert`, `laststmtchanges`,
`mem`, `memdb`, `misc`, `misuse`, `normalize`, `pager`, `pageropt`, `parser`,
`prefixes`, `ptrchng`, `qrf`, `queryonly`, `quickcheck`, `rdonly`, `readonly`,
`reindex`, `resolver`, `rollback`, `rowid`, `securedel`, `stmt`, `stmtrand`,
`table`, `tempdb`, `temptable`, `tokenize`, `tpch`, `trustschema`, `unique`,
`unordered`, `wherelimit`, `widetab`, `zerodamage`, `zipfile`

#### G6.MISC.bigdata — Large data / limits
`bigfile`, `bigmmap`, `bigrow`, `bigsort`, `boundary`, `manydb`, `merge`, `full`

#### G6.MISC.incr — Incremental blob / vacuum
`incrblob`, `incrblob_`, `incrvacuum`, `incrvacuum_`, `autovacuum`

#### G6.MISC.e_tests — e_* comprehensive tests
`e_`, `e_fts`, `e_select` — these use complex TCL frameworks (`do_select_tests`,
`do_createtable_tests`). Need significant transpiler work.
1. Implement `do_select_tests` TCL command in the transpiler.
2. Implement `do_createtable_tests` TCL command.
3. These generate many sub-tests from compact TCL — high value.

### Steps (general pattern for each sub-group):
1. **Run all packages in the sub-group** — capture pass/fail per package.
2. **Group failures by error pattern** — syntax error, result mismatch, etc.
3. **Fix root causes** (smallest fix per root cause).
4. **Re-run affected packages**.
5. **Commit per root-cause fix**.
6. **Update this plan** with results.

### Commit format: `G6.MISC.<subgroup>.<N>: <description>`
