# Test Taxonomy — All 614 testgen Packages Categorized

> **Generated**: 2026-08-02 from `testgen/` directory + SQLite TCL test analysis.
> **Rule**: Priority = CRUD first, then query features, then schema, then functions,
> then advanced SQL, then triage.

---

## Summary

| Category | Count | Description |
|----------|-------|-------------|
| **P1 — CRUD Core** | 58 | CREATE/INSERT/SELECT/UPDATE/DELETE + WHERE + types + expr |
| **P2 — Query Features** | 30 | JOIN, subquery, aggregate, ORDER BY, DISTINCT, UNION, VIEW |
| **P3 — Schema & Constraints** | 47 | ALTER, index, trigger, FK, UNIQUE, CHECK, collation, upsert |
| **P4 — Functions & Expressions** | 32 | string/date/printf/numeric functions |
| **P5 — Advanced SQL** | 90 | FTS, vtab, window, CTE, ATTACH, pragma, JSON |
| **P6a — Not Applicable** | ~125 | C API, fault injection, corruption, VFS, shell, internals |
| **P6b — Deferred** | ~33 | WAL, concurrency, shared cache |
| **P6c — Applicable Misc** | ~209 | ticket tests, misc SQL, rowvalue, descidx, etc. |
| **Total** | 614 | |

---

## P1 — CRUD Core (58 packages)

### P1a — CREATE TABLE / DDL basics
`select1` `types` `strict` `tableopts`(P3) `without_rowid`(P3) `createtab`(P6c)

### P1b — INSERT
`insert` `insertfault` `values` `valuesfault` `default_pkg`

### P1c — SELECT (basic + variants)
`select2` `select3` `select4` `select5` `select6` `select7` `select8` `select9`
`selectA` `selectB` `selectC` `selectD` `selectE` `selectF` `selectG` `selectH`

### P1d — WHERE / filtering
`where` `whereA` `whereB` `whereC` `whereD` `whereE` `whereF` `whereG` `whereH`
`whereI` `whereJ` `whereK` `whereL` `whereM` `whereN`

### P1e — UPDATE
`update` `returning`

### P1f — DELETE
`delete_` `delete2` `delete3` `delete4` `delete_pkg`

### P1g — Types & affinity
`affinity` `cast` `numcast` `types` `intpkey` `intreal` `nulls` `null`

### P1h — Expressions
`expr` `between` `coalesce` `literal` `istrue` `cse` `subtype`

---

## P2 — Query Features (30 packages)

### P2a — JOIN
`join` `joinA` `joinB` `joinC` `joinD` `joinE` `joinF` `joinH` `joinI`

### P2b — Subquery
`subquery` `subselect` `exists` `existsexpr`

### P2c — Aggregate / GROUP BY
`count` `having` `distinct` `distinctagg` `aggerror` `aggfault`
`aggnested`(P6c) `aggorderby`(P6c)

### P2d — ORDER BY / LIMIT
`orderby` `orderbyA` `orderbyB` `limit` `minmax` `sort` `sorterref` `starschema`

### P2e — Set operations
`unionall` `unionallfault`(P6c) `unionvtab`(P6c)

### P2f — VIEW
`view` `countofview`

---

## P3 — Schema & Constraints (47 packages)

### P3a — ALTER TABLE
`alter` `alterauth` `altercol` `altercons` `altercorrupt` `alterdropcol`
`alterfault` `alterlegacy` `altermalloc` `alterqf` `altertab` `altertrig`

### P3b — Index
`index` `indexfault` `indexedby` `indexexpr` `indexA`(P6c) `descidx`(P6c)
`coveridxscan`(P6c) `skipscan`(P6c) `seekscan`(P6c) `expridx`(P6c)
`numindex`(P6c) `bloom`(P6c)

### P3c — Trigger
`trigger` `triggerA` `triggerB` `triggerC` `triggerD` `triggerE` `triggerF`
`triggerG` `triggerupfrom` `temptrigger`

### P3d — Foreign keys
`fkey` `fkey_`

### P3e — Constraints
`conflict` `notnull` `check` `upsert` `upsertfault`(P6c) `unique`(P6c)
`trans` `transitive`

### P3f — Collation
`collate` `collateA` `collateB`

### P3g — Schema management
`schema` `savepoint` `savepointfault` `without_rowid` `tableopts`

---

## P4 — Functions & Expressions (32 packages)

### P4a — String functions
`instr` `substr` `like` `regexp` `hexlit` `blob` `quote`

### P4b — Date/time
`date` `timediff`

### P4c — Numeric
`round` `decimal` `percentile` `atof` `fpconv` `ieee` `nan` `unhex`
`zeroblob` `zeroblobfault` `instrfault`

### P4d — Printf
`printf`

### P4e — Other functions
`func2` `func3` `func4` `func5` `func6` `func7` `func8` `func9` `func_pkg`(P6c)
`spellfix` `closure` `hidden`

---

## P5 — Advanced SQL (90 packages)

### P5a — Full-text search
`fts` `fts3` `fts4` `fts4merge` `fts3atoken` `fts3aux` `fts3comp` `fts3corrupt`
`fts3defer` `fts3expr` `fts3fault` `fts3fuzz` `fts3matchinfo` `fts3prefix`
`fts3snippet` `fts3tok` `fts3tok_` `fts_9fd` `fts4growth`(P6c) `fts4intck`(P6c)
`e_fts`(P6c) `amatch`(P6c)

### P5b — Virtual tables
`vtab` `vtab_` `vtabA`–`vtabL` `vtabrhs` `vtabdistinct` `vtabdrop`
`bestindex`–`bestindexG` `stmtvtab`(P6c) `swarmvtab`(P6c) `unionvtab`(P6c)
`rowvaluevtab`(P6c)

### P5c — Window functions
`window`–`windowE` `windowerr` `windowfault` `windowpushd`

### P5d — CTE (WITH)
`with` `withM`

### P5e — Pragmas
`pragma` `pragmafault`

### P5f — ATTACH
`attach` `attachmalloc`(P6c)

### P5g — JSON
`json` `jsonb`

### P5h — R-tree
`rtree`

### P5i — Query planner / ANALYZE
`analyze` `analyzeC`–`analyzeG` `analyzer` `autoindex` `eqp` `pushdown`
`scanstatus` `cost` `filter` `filterfault` `autoanalyze`(P6c)

### P5j — Vacuum
`vacuum` `vacuum_` `vacuummem`

### P5k — Other extensions
`carray` `intarray` `tabfunc` `session` `upfrom` `csv` `recover_pkg`
`stat` `statfault` `rbu`(P6c) `icu`(P6c) `extension`(P6c) `loadext`(P6c)
`zipfile`(P6c)

---

## P6a — Not Applicable (~125 packages)

These test SQLite C internals, fault injection, corruption, or platform-specific
features that frigolite (pure Go) will not implement.

### C API (not applicable — pure Go, no C API)
`capi` `capi3` `bind` `bindxfer` `tableapi` `tclsqlite` `backup` `backup_`
`carrayfault` `cffault`

### Fault injection / OOM / fuzz
All `*fault` `*malloc*` `*ioerr` `*crash` `*fuzz*` `*corrupt*` packages:
`aggfault`(moved here from P2) `alterfault`(P3) `altercorrupt`(P3)
`altermalloc` `attachmalloc` `bestindex`–fault `btreefault` `cacheflush`
`cachespill` `corrupt`–`corruptN` `crash` `crashM` `dbfuzz` `diskfull`
`exprfault` `fkey2`/`8` faults `fts3corrupt` `fts3fault` `fts3fuzz`
`gcfault` `incrblobfault` `incrcorrupt` `insertfault` `ioerr` `journal`
`jrnlmode` `malloc`–`mallocM` `memleak` `mmapcorrupt` `mmapfault`
`pagerfault` `schemafault` `sortfault` `sysfault` `vacuummem`
`vtabrhs`-fault `walcrash` `walfault` `writecrash` `zipfilefault`

### Custom VFS
`avfs` `cksumvfs` `tvfs` `vfs` `unixexcl` `multiplex` `mmap` `memjournal`
`subjournal` `shmlock` `nocpt` `nolock` `lock` `superlock` `avtrans`
`backcompat` `syscall` `oserror` `shortread` `sync` `fallocate`
`filectrl` `filefmt` `reservedbytes` `chunksize` `pagesize` `p_8_3_`
`uri` `openv` `dbpage` `dbpagefault` `dbdata` `dbstatus`

### Shell (CLI-specific)
`shell` `shellA` `shellB` `sqldiff` `sqllimits` `sqllog`

### Build / config / internals
`bitvec` `atomic` `ctime` `keyword` `memsubsys` `pcache` `lookaside`
`varint` `contrib` `main` `init` `permutations` `speed` `speed1` `speed4`
`soak` `veryquick` `extraquick` `quick` `softheap` `resetdb` `close_pkg`
`notify` `memsubsys` `progress` `hook` `trace` `interrupt` `busy`

### Platform-specific
`win32` `utf16` `enc` `badutf` `basexx` `symlink`

---

## P6b — Deferred (~33 packages)

WAL mode and concurrency — frigolite uses rollback journal only.

### WAL (all wal* packages — ~30)
`wal` `wal64` `walbak` `walbig` `walblock` `walckptnoop` `walcksum`
`walcrash` `walfault` `walhook` `walmode` `walnoshm` `waloverwrite`
`walpersist` `walprotocol` `walrestart` `walro` `walrofault` `walseh`
`walsetlk` `walsetlk_` `walshared` `walslow` `walthread` `walvfs`
`nockpt` `snapshot` `snapshot_`

### Concurrency
`thread` `walthread` `mutex` `shared` `shared_` `sharedA` `sharedB` `sharedlock`

---

## P6c — Applicable Misc (~209 packages)

Standard SQL functionality tests that should eventually pass.

### Row value / vector
`rowvalue` `rowvalueA` `rowvaluevtab` `rowvaluefault`(P6a)

### Big data / limits
`bigfile` `bigmmap` `bigrow` `bigsort` `boundary` `manydb` `merge` `full`
`sqllimits` `offset` `ovfl` `rowallock` `rowhash` `shrink` `sidedelete`

### Incr / vacuum / blob
`incrblob` `incrblob_` `incrvacuum` `incrvacuum_` `autovacuum`
`autovacuum_ioerr`(P6a)

### Index features
`bloom` `coveridxscan` `descidx` `expridx` `numindex` `seekscan` `skipscan`
`indexA` `autoindex`(P5)

### Misc SQL
`alias` `auth` `autoinc` `btree` `cache` `changes` `colmeta` `colname`
`columncount` `createtab` `cursorhint` `dataversion` `emptytable`
`errmsg` `errofst` `eval` `exclusive` `exec` `external_` `fordelete`
`format` `func_pkg` `gencol` `imposter` `in` `io` `lastinsert`
`laststmtchanges` `mem` `memdb` `misc` `misuse` `normalize` `pager`
`pageropt` `parser` `prefixes` `ptrchng` `qrf` `queryonly` `quickcheck`
`quota` `quota_` `randexpr` `rdonly` `readonly` `reindex` `resolver`
`rollback` `rowid` `securedel` `stmt` `stmtrand` `swarmvtab` `table`
`tempdb` `temptable` `tokenize` `tpch` `trustschema` `unique` `unordered`
`wherelimit` `wherelfault`(P6a) `wherefault`(P6a) `widetab` `zerodamage`
`zipfile`

### Ticket-specific regression tests (~70 tkt_* packages)
These are SQLite bug-fix regression tests. Each tests a specific past bug.
Most exercise real SQL functionality. Grouped as one batch goal (G6.MISC.tkt):
`tkt` `tkt35` `tkt_*` (68 packages)

### e_* (comprehensive feature tests)
`e_` `e_fts` `e_select` — these use TCL test framework helpers extensively
and may need significant transpiler work.

---

## Per-Package Verification Command

To check the current status of a specific package:
```bash
go test -tags testgen ./testgen/<pkg>/ -count=1 -timeout 60s -v 2>&1 | tail -20
```

To check a tier:
```bash
bash scripts/verify_tier.sh <tier-number>
```

## Notes on Categorization

1. Some packages appear in multiple categories (e.g., `aggfault` tests both
   aggregates and fault injection). Primary category is the feature tested.
2. The `tkt_*` packages (68) are SQLite bug-fix regressions. They test real SQL
   but are numerous — batch them in G6.MISC.tkt.
3. Packages marked "(P6a)" or "(P5)" in a section are cross-referenced from
   another category. Their primary home is the parenthesized category.
4. The `e_*` packages use complex TCL frameworks (`do_select_tests`,
   `do_createtable_tests`) that need transpiler support.
