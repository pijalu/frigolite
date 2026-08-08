# DEFERRED — WAL / Concurrency Test Packages

> **Goal**: G6.DEFERRED — Document the 33 testgen packages that require WAL
> mode or shared-memory concurrency as **deferred** (future work), not
> excluded. These packages remain a visible signal for future WAL work.
>
> **Status**: Documented (this file). No engine changes made — WAL
> implementation is out of scope for the current phase.

## Summary

Frigolite implements the **rollback journal** only. The following 33 testgen
packages (generated from SQLite TCL tests) exercise SQLite's **write-ahead log
(WAL)** and **shared-memory / multi-connection concurrency** features, which
require a completely different journaling and locking architecture. They are
**deferred**: legitimate future work, not excluded as not-applicable, and not
hidden from the test suite.

| Category | Count | Verdict |
|----------|-------|---------|
| WAL | 26 | DEFERRED — needs WAL implementation |
| Concurrency | 7 | DEFERRED — needs shared-memory/locking implementation |
| **Total** | **33** | |

## Why deferred

SQLite WAL mode differs fundamentally from rollback journal mode:

1. **Write-ahead log file** (`-wal`) — new pages appended to a log instead of
   in-place updates; requires a log reader/writer, log-format parsing
   (frame headers, checksums, commit marks), and log replay on open.
2. **Shared-memory index** (`-shm`) — a memory-mapped index coordinating
   readers/writers across connections (WAL-index hash table, read marks);
   requires `mmap`-style shared memory or an equivalent.
3. **Checkpointing** — moving committed WAL frames back into the database file
   with reader coordination (`PRAGMA wal_checkpoint`, automatic checkpoints).
4. **Concurrency model** — snapshot isolation for readers, single writer,
   different locking rules (`WAL` vs `DELETE`/`PERSIST`/`TRUNCATE` journal
   modes). Multi-connection tests also assume real OS-level file locking and
   thread-safe connections.

Frigolite's `internal/pager` implements rollback journal transactions with a
single-connection page cache. Supporting WAL requires substantial new
infrastructure (log storage, shm coordination, checkpointing, snapshot
readers) — out of scope for the current phase.

## Deferred packages (33)

### WAL (26)

| Package | Feature required | Current status |
|---------|------------------|----------------|
| wal | WAL mode basics: `PRAGMA journal_mode=WAL`, read/write, checkpoint | COMPILE FAIL (transpiler: multi-DB code) |
| wal64 | 64-bit WAL frame numbers / large files | vacuous PASS |
| walbak | WAL mode + `VACUUM INTO`/backup interplay | FAIL |
| walbig | Large WAL file growth / big transactions | FAIL |
| walblock | WAL blocking / locking contention | vacuous PASS |
| walckptnoop | Checkpoint no-op cases (nothing to checkpoint) | FAIL |
| walcksum | WAL checksum verification (frames, headers) | FAIL |
| walcrash | WAL crash recovery: torn writes, interrupted checkpoints | FAIL (timeout) |
| walfault | WAL fault injection (OOM/IO error handling) | FAIL |
| walhook | `wal_hook` callback API | vacuous PASS |
| walmode | `PRAGMA journal_mode=WAL` switching, persistence | FAIL |
| walnoshm | WAL without shared memory (single connection) | FAIL |
| waloverwrite | WAL overwrite of old frames / reuse | FAIL (timeout) |
| walpersist | Persistent WAL mode (`journal_mode=PERSIST`-like semantics) | FAIL |
| walprotocol | WAL frame protocol details (commit marks, page framing) | FAIL |
| walrestart | WAL restart after checkpoint | vacuous PASS |
| walro | Read-only WAL access (read-only connections) | FAIL |
| walrofault | Read-only WAL fault handling | FAIL |
| walseh | WAL segment headers / salt values | FAIL |
| walsetlk | WAL lock acquisition (WAL_WRITE_LOCK etc.) | FAIL |
| walsetlk_ | WAL lock variants (write lock recovery) | FAIL |
| walshared | WAL shared-memory index behavior | vacuous PASS |
| walslow | Slow WAL operations (large checkpoints) | FAIL (timeout) |
| walthread | WAL + multi-threaded connections | vacuous PASS |
| walvfs | WAL through custom VFS (xShmMap etc.) | FAIL |
| nockpt | No-checkpoint behavior (readers block writers) | vacuous PASS |

### Concurrency (7)

| Package | Feature required | Current status |
|---------|------------------|----------------|
| thread | Multi-threaded connections (sqlite3_threadsafe) | vacuous PASS |
| mutex | SQLite mutex subsystem semantics | vacuous PASS |
| shared | Shared-cache mode (SQLITE_OPEN_SHAREDCACHE) | COMPILE FAIL (transpiler: multi-DB code) |
| shared_ | Shared-cache error cases | FAIL (panic) |
| sharedA | Shared-cache A (multi-connection read/write) | vacuous PASS |
| sharedB | Shared-cache B (multi-connection locking) | vacuous PASS |
| sharedlock | Shared-cache locking (table-level locks) | vacuous PASS |
| tkt2854 | Shared-cache multi-connection (sqlite3_enable_shared_cache, db/db2 shared cache, db3 private; cross-connection read-locks) | skipped DEFERRED (transpiler skipTestFiles) |

> **Note on "vacuous PASS"**: packages marked *vacuous PASS* compile and exit 0,
> but the transpiler converts their WAL/thread-specific TCL commands
> (`sqlite3_wal_checkpoint`, `sqlite3_snapshot_get`, thread spawns, multi-DB
> handles) into no-ops or self-skips, so they do **not** actually exercise the
> deferred feature. They are recorded here so they are not mistaken for real
> coverage of WAL/concurrency behavior.

## Relationship to the JSON harness

The `testdata/*.json` harness equivalents of these packages are already
excluded with explicit DEFERRED reasons in `unsupportedTestFiles`
(`frigolite_harness_test.go`), grouped under:

```go
// WAL mode — DEFERRED (rollback journal only)
"wal5": "WAL mode not implemented (rollback journal only) - DEFERRED",
...
// Concurrency / threads — DEFERRED (shared-memory locking not implemented)
"shared": "Thread/concurrency tests require shared-memory locking - DEFERRED",
```

Testdata names differ from testgen names (e.g. `wal5`, `wal64k`, `wal7`,
`wal8` vs `wal`, `wal64`; `shared3`–`shared9` vs `shared`/`sharedA`/`sharedB`).
This document is the authoritative categorization for the **testgen**
packages; `plans/NOT_APPLICABLE.md` §DEFERRED Detail holds the short table.

## What a future WAL implementation needs

1. **WAL log storage** — append-only log file, frame format
   (page number, size, checksum, commit flag), log header (magic, version,
   page size, salt values), replay on open, truncation on checkpoint.
2. **Shared-memory index** — WAL-index hash table for page lookup, read marks
   for snapshot isolation; in pure Go this can be a process-local shared
   structure (no `mmap` requirement) as long as cross-connection semantics
   hold within a process.
3. **Checkpoint machinery** — `PRAGMA wal_checkpoint(FULL/PASSIVE/TRUNCATE)`,
   automatic checkpoint thresholds, reader/writer coordination.
4. **Locking** — WAL lock protocol (WAL_WRITE_LOCK, WAL_CKPT_LOCK,
   WAL_READ_LOCK(n)), busy handling; multi-connection file locking.
5. **`PRAGMA journal_mode`** — accept and persist `WAL`; keep rollback journal
   modes working unchanged.
6. **Snapshot API** — `sqlite3_snapshot_get/open/free` equivalents for the
   `snapshot`, `snapshot_` testgen packages (see Edge cases).

## Edge cases

- `snapshot`, `snapshot_` (testgen) use the WAL snapshot C API
  (`sqlite3_snapshot_get`/`_open`/`_cmp`) plus `PRAGMA journal_mode=WAL`.
  They currently pass vacuously (unsupported commands are no-ops) and are
  **not** in the 33-package deferred list. They become real tests once the
  WAL snapshot API exists (roadmap item 6).
- `wal64k` (testdata) is the 64 KiB-page-size WAL variant of `wal64`
  (testgen). Both are deferred; `wal64k` is excluded in the harness.

## Tracking

- Sub-plan: `plans/subplans/P6_DEFERRED.md` (this goal).
- Triage overview: `plans/subplans/P6_TRIAGE.md` (§G6.DEFERRED).
- Exclusions doc: `plans/NOT_APPLICABLE.md` (§Deferred).
- Harness map: `unsupportedTestFiles` in `frigolite_harness_test.go`.
