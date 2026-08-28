# TASK G7 — Query Planner, WAL, Concurrency, Session/RBU

> **Phase**: G7 (depends on G6 core goals green)
> **Goal IDs**: G7.PLANNER-EXPLAIN, G7.WAL, G7.LOCKING, G7.SESSION-RBU
> **Read first**: `PORTPLAN.md` §0,
> **`portplan/DESIGN.md` §H (WAL frame format + locking + snapshot) + §K
> (planner/index-scan/EXPLAIN/stat)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started
>
> **Directive**: WAL (33+ packages), locking/concurrency (37 packages), and the
> query planner/EXPLAIN/ANALYZE packages were deferred. **Implement them.**

---

## Goal G7.PLANNER-EXPLAIN — Query planner, EXPLAIN/EXPLAIN QUERY PLAN, ANALYZE

**Scope**: `where*` EQP cases, `eqp`, `explain`, `analyze`, `analyzeC`–`analyzeG`,
`autoindex`, `bestindex`, `skipscan`, `where2`, `whereH`/`whereK`/`whereL`/
`whereM`/`whereN`, `cost`, `analyzer1`.

**Key areas**: `internal/exec/` (planner/index selection), `internal/storage/`
(stat tables). Reference SQLite `src/where*.c`, `src/analyze.c`, `src/shell.c`
(EXPLAIN output).

> Frigolite currently has *no* cost-based planner (no VDBE). The goal is a
> planner that (a) uses indexes when present (correct results + reasonable order),
> (b) emits a *plausible* EXPLAIN QUERY PLAN that matches SQLite's shape where the
> test asserts it, and (c) consumes `sqlite_stat1`/`stat4`. Planner *row order*
> differences where the result SET is identical are documented N-A per-test with
> evidence (e.g. rowvalue-32.1) — but only after confirming via oracle.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/explain/ ./testgen/analyze/ ./testgen/autoindex/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP7Planner' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Index selection: pick an index for WHERE/ORDER BY to satisfy both correctness
   and the covering-scan order (G2.G2.INDEX built the index b-trees).
2. `sqlite_stat1`/`stat4` population via ANALYZE; `sqlite_stat` introspection.
3. EXPLAIN (bytecode) — Frigolite has no VDBE; emit a *simulated* EXPLAIN table
   whose rows match SQLite's opcode text for the queries the tests assert on
   (this is a mapping exercise; document the limitation). Prefer matching the
   observable plan over a full bytecode engine.
4. EXPLAIN QUERY PLAN: emit `SCAN`/`SEARCH … USING INDEX`/`USE TEMP B-TREE` text
   matching SQLite for the asserted queries.
5. autoindex, skipscan, bestindex (vtab) integration.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G7.WAL — Write-Ahead Log mode

**Scope**: `wal`, `wal64`, `walbak`, `walbig`, `walblock`, `walckptnoop`,
`walcksum`, `walcrash`, `walfault`, `walhook`, `walmode`, `walnoshm`,
`waloverwrite`, `walpersist`, `walprotocol`, `walrestart`, `walro`,
`walrofault`, `walseh`, `walsetlk`/`walsetlk_`, `walshared`, `walslow`,
`walthread`, `walvfs`, `nockpt`. (26 packages.)

**Key areas**: `internal/pager/` (new WAL path), `internal/wal/` (new).
Reference SQLite `src/wal.c`, `src/wal.h`, `src/walrecover.c`.
**stdlib**: `os`/`io`/`sync` for the WAL file + shm; no mmap required
(process-local shm is acceptable for single-process multi-connection).

**Verify command** (incremental — basics first):
```bash
go test -tags testgen -count=1 -timeout 120s ./testgen/walmode/ ./testgen/wal/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP7Wal' -count=1 . && go build ./... && make quality
```

**Todos** (decompose into sub-goals; this is large):
1. WAL log storage: append-only `-wal` file; frame format (page no, size,
   checksum, commit flag); header (magic, version, page size, salts); replay on open.
2. Shared-memory index: process-local WAL-index hash table for page lookup; read
   marks for snapshot isolation.
3. Checkpoint: `PRAGMA wal_checkpoint(TRUNCATE/FULL/PASSIVE)`; automatic threshold.
4. `PRAGMA journal_mode=WAL` accept + persist; rollback modes unchanged.
5. Reader snapshot isolation; single writer; lock protocol (WAL_WRITE_LOCK,
   WAL_CKPT_LOCK, WAL_READ_LOCK(n)).
6. Un-skip WAL packages; regenerate; per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G7.LOCKING — Multi-connection locking, shared cache, busy, snapshot

**Scope**: `lock`–`lock7`, `shared`–`shared9`, `shared_err`, `sharedlock`,
`shmlock`, `busy`/`busy2`, `manydb`, `multiplex`–`multiplex4`, `nolock`,
`superlock`, `snapshot`–`snapshot4`, `snapshot_up`, `tkt2854`, `tkt3093`,
`tkt3793`, `tkt3810`, `tkt-f3e5abed55`. (37 packages.)

**Key areas**: `internal/pager/` (file locking), `internal/` (connection model).
Reference SQLite `src/os_unix.c` (locking), `src/main.c` (shared cache).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s ./testgen/lock/ ./testgen/busy/ ./testgen/shared/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP7Lock' -count=1 . && go build ./... && make quality
```

**Todos**:
1. File locking (POSIX advisory via `syscall` Flock/Fcntl, or Go `os` file locks
   — portable subset) for multi-connection reader/writer exclusion.
2. Shared-cache mode (SQLITE_OPEN_SHAREDCACHE): table-level read/write locks
   across connections sharing a cache.
3. Busy handler + busy timeout; `SQLITE_BUSY`/`SQLITE_LOCKED` distinction.
4. `sqlite3_snapshot_get/open/free/cmp` (depends on WAL from G7.WAL).
5. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G7.SESSION-RBU — Session extension + RBU

**Scope**: `session`, `session2`–`session6`, `rbu`, `rbu*`.

**Key areas**: new `internal/session/` (changeset/patchset recording + apply),
`internal/rbu/` (resumable bulk update). Reference SQLite `ext/session/`,
`ext/rbu/`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s ./testgen/session/ ./testgen/rbu/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP7Session' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Session: record changes (INSERT/UPDATE/DELETE) into a changeset; apply/invert/
   concat; conflict handling.
2. RBU: staged bulk update against a temp DB + checkpoint swap.
3. Map `sqlite3session_*`/`sqlite3changeset_*` to a Go-idiomatic API (record the
   session on a `*DB`; `Changeset` as a value type).
4. Per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All four goals green; pre-tests pass; quality + SOLID pass; no G1–G6 regression.
- `PORTPLAN.md` §5 G7 rows → 🟢.
