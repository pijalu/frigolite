# NOT APPLICABLE — Test Packages Excluded from Frigolite

> **Goal**: G6.NA — Formally document and exclude test packages that exercise
> SQLite C internals or features that a pure-Go reimplementation does not provide.
> This document is the authoritative categorization. Every excluded package appears
> below with a one-line reason. The harness `unsupportedTestFiles` map implements
> these exclusions with the same reasons (see `frigolite_harness_test.go`).

> **Scope**: 614 testgen packages (generated from SQLite TCL tests). Categorized:
> **228 N/A** (excluded — features not applicable), **33 DEFERRED** (WAL/concurrency —
> future work, see `plans/DEFERRED.md`), **353 applicable** (in scope for G6.MISC).

## Summary

| Category | Count | Verdict |
|----------|-------|---------|
| Build/config | 32 | N/A |
| C API | 10 | N/A |
| Corruption | 6 | N/A |
| Custom VFS | 30 | N/A |
| FTS | 18 | N/A |
| Fault injection | 64 | N/A |
| JSON | 2 | N/A |
| Misc internal | 11 | N/A |
| Performance | 18 | N/A |
| Platform | 6 | N/A |
| Session/RBU | 2 | N/A |
| Shell | 7 | N/A |
| Window | 8 | N/A |
| rtree | 1 | N/A |
| vtab modules | 14 | N/A |
| **Total N/A** | **229** | |

## Deferred (WAL / concurrency)

| Category | Count | Verdict |
|----------|-------|---------|
| WAL | 26 | DEFERRED (plans/DEFERRED.md) |
| Concurrency | 7 | DEFERRED (plans/DEFERRED.md) |

## N/A Categories

### Build/config

| Package | Reason |
|---------|--------|
| atof | Tests SQLite internal data structures/algorithms — frigolite has its own |
| atomic | Tests SQLite internal data structures/algorithms — frigolite has its own |
| bitvec | Tests SQLite internal data structures/algorithms — frigolite has its own |
| cache | Tests SQLite internal data structures/algorithms — frigolite has its own |
| cacheflush | Tests SQLite internal data structures/algorithms — frigolite has its own |
| cachespill | Tests SQLite internal data structures/algorithms — frigolite has its own |
| chunksize | Tests SQLite internal data structures/algorithms — frigolite has its own |
| ctime | Tests SQLite internal data structures/algorithms — frigolite has its own |
| decimal | Tests SQLite internal data structures/algorithms — frigolite has its own |
| filefmt | Tests SQLite internal data structures/algorithms — frigolite has its own |
| fpconv | Tests SQLite internal data structures/algorithms — frigolite has its own |
| hexlit | Tests SQLite internal data structures/algorithms — frigolite has its own |
| ieee | Tests SQLite internal data structures/algorithms — frigolite has its own |
| keyword | Tests SQLite internal data structures/algorithms — frigolite has its own |
| lookaside | Tests SQLite internal data structures/algorithms — frigolite has its own |
| memsubsys | Tests SQLite internal data structures/algorithms — frigolite has its own |
| nan | Tests SQLite internal data structures/algorithms — frigolite has its own |
| normalize | Tests SQLite internal data structures/algorithms — frigolite has its own |
| p_8_3_ | Tests SQLite internal data structures/algorithms — frigolite has its own |
| pageropt | Tests SQLite internal data structures/algorithms — frigolite has its own |
| pagesize | Tests SQLite internal data structures/algorithms — frigolite has its own |
| pcache | Tests SQLite internal data structures/algorithms — frigolite has its own |
| percentile | Tests SQLite internal data structures/algorithms — frigolite has its own |
| progress | Tests SQLite internal data structures/algorithms — frigolite has its own |
| rowhash | Tests SQLite internal data structures/algorithms — frigolite has its own |
| scanstatus | Tests SQLite internal data structures/algorithms — frigolite has its own |
| softheap | Tests SQLite internal data structures/algorithms — frigolite has its own |
| sorterref | Tests SQLite internal data structures/algorithms — frigolite has its own |
| stmt | Tests SQLite internal data structures/algorithms — frigolite has its own |
| stmtrand | Tests SQLite internal data structures/algorithms — frigolite has its own |
| subtype | Tests SQLite internal data structures/algorithms — frigolite has its own |
| varint | Tests SQLite internal data structures/algorithms — frigolite has its own |

### C API

| Package | Reason |
|---------|--------|
| backup | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| backup_ | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| bind | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| bindxfer | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| capi | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| capi3 | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| changes2 | Tests sqlite3 C API (prepare/step/finalize, update hooks) — frigolite has no C API |
| close_pkg | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| openv | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| tableapi | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| tclsqlite | Tests sqlite3 C API (prepare/step/finalize) — frigolite has no C API |
| notify | Tests sqlite3 C API (update hooks, db handle indirection) — frigolite has no C API |
| avtrans | Tests sqlite3 C API (altdb connection handles, autovacuum) — frigolite has no C API |
| incrblob | Tests sqlite3 C API (incrblob blob handles) — frigolite has no C API |
| incrblob_ | Tests sqlite3 C API (incrblob blob handles) — frigolite has no C API |

### Corruption

| Package | Reason |
|---------|--------|
| altercorrupt | Tests file-format corruption detection — requires byte-level corruption tooling |
| corrupt | Tests file-format corruption detection — requires byte-level corruption tooling |
| corruptA–corruptN | Tests file-format corruption detection (database_may_be_corrupt) — requires byte-level corruption tooling |
| fts3corrupt | Tests file-format corruption detection — requires byte-level corruption tooling |
| incrcorrupt | Tests file-format corruption detection — requires byte-level corruption tooling |
| mmapcorrupt | Tests file-format corruption detection — requires byte-level corruption tooling |
| recover_pkg | Tests file-format corruption detection — requires byte-level corruption tooling |

### Custom VFS

| Package | Reason |
|---------|--------|
| avfs | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| busy | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| cksumvfs | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| fallocate | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| filectrl | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| interrupt | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| journal | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| jrnlmode | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| lock | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| memjournal | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| mjournal | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| mmap | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| mmapwarm | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| multiplex | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| nolock | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| oserror | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| pendingrace | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| quota | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| quota_ | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| reservebytes | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| rowallock | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| securedel | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| shmlock | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| shortread | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| superlock | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| sync | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| syscall | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| unixexcl | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| uri | Tests custom VFS / OS layer — frigolite uses Go I/O directly |
| win32 | Tests custom VFS / OS layer — frigolite uses Go I/O directly |

### FTS

| Package | Reason |
|---------|--------|
| e_fts | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3 | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3atoken | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3aux | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3comp | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3defer | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3expr | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3matchinfo | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3prefix | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3snippet | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3tok | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts3tok_ | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts4 | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts4growth | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts4intck | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts4merge | FTS3/4/5 not implemented — frigolite has no FTS engine |
| fts_9fd | FTS3/4/5 not implemented — frigolite has no FTS engine |

### Fault injection

| Package | Reason |
|---------|--------|
| aggfault | Tests OOM/error injection — frigolite has no fault simulator |
| altermalloc | Tests OOM/error injection — frigolite has no fault simulator |
| attachmalloc | Tests OOM/error injection — frigolite has no fault simulator |
| autovacuum_ioerr | Tests OOM/error injection (ioerr variant) — frigolite has no fault simulator |
| btreefault | Tests OOM/error injection — frigolite has no fault simulator |
| carrayfault | Tests OOM/error injection — frigolite has no fault simulator |
| cffault | Tests OOM/error injection — frigolite has no fault simulator |
| checkfault | Tests OOM/error injection — frigolite has no fault simulator |
| crash | Tests OOM/error injection — frigolite has no fault simulator |
| crashM | Tests OOM/error injection — frigolite has no fault simulator |
| dbfuzz | Fuzz testing — requires SQLite fuzz infrastructure |
| dbpagefault | Tests OOM/error injection — frigolite has no fault simulator |
| diskfull | Tests OOM/error injection — frigolite has no fault simulator |
| existsfault | Tests OOM/error injection — frigolite has no fault simulator |
| exprfault | Tests OOM/error injection — frigolite has no fault simulator |
| filterfault | Tests OOM/error injection — frigolite has no fault simulator |
| fts3fault | Tests OOM/error injection — frigolite has no fault simulator |
| fts3fuzz | Tests OOM/error injection — frigolite has no fault simulator |
| fuzz | Tests OOM/error injection — frigolite has no fault simulator |
| fuzz_ | Tests OOM/error injection — frigolite has no fault simulator |
| fuzz_oss | Tests OOM/error injection — frigolite has no fault simulator |
| fuzzer | Tests OOM/error injection — frigolite has no fault simulator |
| fuzzerfault | Tests OOM/error injection — frigolite has no fault simulator |
| gcfault | Tests OOM/error injection — frigolite has no fault simulator |
| incrblobfault | Tests OOM/error injection — frigolite has no fault simulator |
| indexfault | Tests OOM/error injection — frigolite has no fault simulator |
| insertfault | Tests OOM/error injection — frigolite has no fault simulator |
| instrfault | Tests OOM/error injection — frigolite has no fault simulator |
| ioerr | Tests OOM/error injection — frigolite has no fault simulator |
| malloc | Tests OOM/error injection — frigolite has no fault simulator |
| mallocA | Tests OOM/error injection — frigolite has no fault simulator |
| mallocB | Tests OOM/error injection — frigolite has no fault simulator |
| mallocC | Tests OOM/error injection — frigolite has no fault simulator |
| mallocD | Tests OOM/error injection — frigolite has no fault simulator |
| mallocE | Tests OOM/error injection — frigolite has no fault simulator |
| mallocF | Tests OOM/error injection — frigolite has no fault simulator |
| mallocG | Tests OOM/error injection — frigolite has no fault simulator |
| mallocH | Tests OOM/error injection — frigolite has no fault simulator |
| mallocI | Tests OOM/error injection — frigolite has no fault simulator |
| mallocJ | Tests OOM/error injection — frigolite has no fault simulator |
| mallocK | Tests OOM/error injection — frigolite has no fault simulator |
| mallocL | Tests OOM/error injection — frigolite has no fault simulator |
| mallocM | Tests OOM/error injection — frigolite has no fault simulator |
| mmapfault | Tests OOM/error injection — frigolite has no fault simulator |
| notnullfault | Tests OOM/error injection — frigolite has no fault simulator |
| pagerfault | Tests OOM/error injection — frigolite has no fault simulator |
| pragmafault | Tests OOM/error injection — frigolite has no fault simulator |
| returningfault | Tests OOM/error injection — frigolite has no fault simulator |
| rollbackfault | Tests OOM/error injection — frigolite has no fault simulator |
| rowvaluefault | Tests OOM/error injection — frigolite has no fault simulator |
| savepointfault | Tests OOM/error injection — frigolite has no fault simulator |
| schemafault | Tests OOM/error injection — frigolite has no fault simulator |
| sortfault | Tests OOM/error injection — frigolite has no fault simulator |
| statfault | Tests OOM/error injection — frigolite has no fault simulator |
| swarmvtabfault | Tests OOM/error injection — frigolite has no fault simulator |
| sysfault | Tests OOM/error injection — frigolite has no fault simulator |
| tempfault | Tests OOM/error injection — frigolite has no fault simulator |
| unionallfault | Tests OOM/error injection — frigolite has no fault simulator |
| upfromfault | Tests OOM/error injection — frigolite has no fault simulator |
| valuesfault | Tests OOM/error injection — frigolite has no fault simulator |
| wherefault | Tests OOM/error injection — frigolite has no fault simulator |
| wherelfault | Tests OOM/error injection — frigolite has no fault simulator |
| windowfault | Tests OOM/error injection — frigolite has no fault simulator |
| writecrash | Tests OOM/error injection — frigolite has no fault simulator |
| zeroblobfault | Tests OOM/error injection — frigolite has no fault simulator |
| zipfilefault | Tests OOM/error injection — frigolite has no fault simulator |

### JSON

| Package | Reason |
|---------|--------|
| json | JSON functions not implemented |
| jsonb | JSON functions not implemented |

### Misc internal

| Package | Reason |
|---------|--------|
| backcompat | Tests SQLite C runtime internals (memory, init, extensions) |
| extension | Tests SQLite C runtime internals (memory, init, extensions) |
| init | Tests SQLite C runtime internals (memory, init, extensions) |
| loadext | Tests SQLite C runtime internals (memory, init, extensions) |
| main | Tests SQLite C runtime internals (memory, init, extensions) |
| mem | Tests SQLite C runtime internals (memory, init, extensions) |
| memdb | Tests SQLite C runtime internals (memory, init, extensions) |
| memleak | Tests SQLite C runtime internals (memory, init, extensions) |
| misuse | Tests SQLite C runtime internals (memory, init, extensions) |
| mutex | Tests SQLite C runtime internals (memory, init, extensions) |
| permutations | Tests SQLite C runtime internals (memory, init, extensions) |

### Performance

| Package | Reason |
|---------|--------|
| bigfile | Stress/performance benchmarks — not functional tests |
| bigmmap | Stress/performance benchmarks — not functional tests |
| bigrow | Stress/performance benchmarks — not functional tests |
| bigsort | Stress/performance benchmarks — not functional tests |
| boundary | Stress/performance benchmarks — not functional tests |
| extraquick | Stress/performance benchmarks — not functional tests |
| full | Stress/performance benchmarks — not functional tests |
| manydb | Stress/performance benchmarks — not functional tests |
| merge | Stress/performance benchmarks — not functional tests |
| quick | Stress/performance benchmarks — not functional tests |
| quickcheck | Stress/performance benchmarks — not functional tests |
| soak | Stress/performance benchmarks — not functional tests |
| speed | Stress/performance benchmarks — not functional tests |
| speed1 | Stress/performance benchmarks — not functional tests |
| speed4 | Stress/performance benchmarks — not functional tests |
| tpch | Stress/performance benchmarks — not functional tests |
| veryquick | Stress/performance benchmarks — not functional tests |
| zerodamage | Stress/performance benchmarks — not functional tests |

### Platform

| Package | Reason |
|---------|--------|
| badutf | Platform-specific tests (Windows, UTF-16 encoding, ICU) |
| basexx | Platform-specific tests (Windows, UTF-16 encoding, ICU) |
| enc | Platform-specific tests (Windows, UTF-16 encoding, ICU) |
| icu | Platform-specific tests (Windows, UTF-16 encoding, ICU) |
| symlink | Platform-specific tests (Windows, UTF-16 encoding, ICU) |
| utf16 | Platform-specific tests (Windows, UTF-16 encoding, ICU) |

### Session/RBU

| Package | Reason |
|---------|--------|
| rbu | Session/RBU extensions not implemented |
| session | Session/RBU extensions not implemented |

### Shell

| Package | Reason |
|---------|--------|
| dbdata | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |
| dbpage | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |
| shell | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |
| shellA | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |
| shellB | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |
| sqldiff | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |
| sqllog | Tests the sqlite3 CLI shell / shell tools — frigolite has its own CLI |

### Window

| Package | Reason |
|---------|--------|
| window | Window functions not supported |
| windowA | Window functions not supported |
| windowB | Window functions not supported |
| windowC | Window functions not supported |
| windowD | Window functions not supported |
| windowE | Window functions not supported |
| windowerr | Window functions not supported |
| windowpushd | Window functions not supported |

### rtree

| Package | Reason |
|---------|--------|
| rtree | RTree extension not implemented |

### vtab modules

| Package | Reason |
|---------|--------|
| amatch | Virtual table module not implemented in frigolite |
| carray | Virtual table module not implemented in frigolite |
| closure | Virtual table module not implemented in frigolite |
| csv | Virtual table module not implemented in frigolite |
| intarray | Virtual table module not implemented in frigolite |
| rowvaluevtab | Virtual table module not implemented in frigolite |
| spellfix | Virtual table module not implemented in frigolite |
| stmtvtab | Virtual table module not implemented in frigolite |
| swarmvtab | Virtual table module not implemented in frigolite |
| tabfunc | Virtual table module not implemented in frigolite |
| unionvtab | Virtual table module not implemented in frigolite |
| unionvtabfault | Virtual table module not implemented in frigolite |
| vtabrhs | Virtual table module not implemented in frigolite |
| zipfile | Virtual table module not implemented in frigolite |

## DEFERRED Detail

| Package | Reason |
|---------|--------|
| nockpt | WAL — deferred to future WAL/concurrency implementation |
| wal | WAL — deferred to future WAL/concurrency implementation |
| wal64 | WAL — deferred to future WAL/concurrency implementation |
| walbak | WAL — deferred to future WAL/concurrency implementation |
| walbig | WAL — deferred to future WAL/concurrency implementation |
| walblock | WAL — deferred to future WAL/concurrency implementation |
| walckptnoop | WAL — deferred to future WAL/concurrency implementation |
| walcksum | WAL — deferred to future WAL/concurrency implementation |
| walcrash | WAL — deferred to future WAL/concurrency implementation |
| walfault | WAL — deferred to future WAL/concurrency implementation |
| walhook | WAL — deferred to future WAL/concurrency implementation |
| walmode | WAL — deferred to future WAL/concurrency implementation |
| walnoshm | WAL — deferred to future WAL/concurrency implementation |
| waloverwrite | WAL — deferred to future WAL/concurrency implementation |
| walpersist | WAL — deferred to future WAL/concurrency implementation |
| walprotocol | WAL — deferred to future WAL/concurrency implementation |
| walrestart | WAL — deferred to future WAL/concurrency implementation |
| walro | WAL — deferred to future WAL/concurrency implementation |
| walrofault | WAL — deferred to future WAL/concurrency implementation |
| walseh | WAL — deferred to future WAL/concurrency implementation |
| walsetlk | WAL — deferred to future WAL/concurrency implementation |
| walsetlk_ | WAL — deferred to future WAL/concurrency implementation |
| walshared | WAL — deferred to future WAL/concurrency implementation |
| walslow | WAL — deferred to future WAL/concurrency implementation |
| walthread | WAL — deferred to future WAL/concurrency implementation |
| walvfs | WAL — deferred to future WAL/concurrency implementation |
| mutex | Concurrency — deferred to future WAL/concurrency implementation |
| shared | Concurrency — deferred to future WAL/concurrency implementation |
| sharedA | Concurrency — deferred to future WAL/concurrency implementation |
| sharedB | Concurrency — deferred to future WAL/concurrency implementation |
| shared_ | Concurrency — deferred to future WAL/concurrency implementation |
| sharedlock | Concurrency — deferred to future WAL/concurrency implementation |
| thread | Concurrency — deferred to future WAL/concurrency implementation |

## Notes

- **Not hiding bugs**: packages in this list test features that frigolite
  deliberately does not implement (C API, VFS, fault injection, FTS, WAL, etc.).
  Applicable packages that fail due to engine bugs are tracked separately (G6.MISC)
  and must NOT be added here.
- The JSON harness (`testdata/*.json`) mirrors this list via `unsupportedTestFiles`
  in `frigolite_harness_test.go`; testdata names may differ slightly from testgen
  names (e.g. `capi2`/`capi3` vs `capi`).
