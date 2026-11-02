# Shared-Cache + Unlock-Notify — notify1 (122 leaves) — M3.PC-SC

Goal: close the notify1 family by porting SQLite's shared-cache subsystem
(btree.c table locks + notify.c unlock-notify) with the same fundamental
approach — the global enable flag, a shared store per file, table-level
locks with the exact error contract, and the blocked-connection notification
graph. This supersedes the 2026-09-14 N-A disposition (the gap-closure plan
re-queued notify1 as M3 work).

## C contract (ground truth)

### Table locks (btree.c)

- `querySharedCacheTableLock(p, iTab, eLock)` (btree.c:327): no-op when not
  sharable. Another connection holding BTS_EXCLUSIVE on the BtShared →
  SQLITE_LOCKED_SHAREDCACHE. Otherwise scan the BtLock list: any OTHER Btree
  holding `eLock != requested` on the same iTab → SQLITE_LOCKED_SHAREDCACHE
  (+ sqlite3ConnectionBlocked(p->db, owner->db); WRITE_LOCK failure sets
  BTS_PENDING).
- `sqlite3BtreeLockTable(p, iTab, isWriteLock)` (btree.c:11354): query then
  set. READ_LOCK acquired when a cursor reads a table; WRITE_LOCK when a
  statement writes it (OP_TableLock per table in the VDBE program).
- Errors: OP_TableLock (vdbe.c:8204) renders `database table is locked: %s`
  with the table name. Bare `database table is locked` (main.c:1647,
  SQLITE_LOCKED errstr) surfaces when the failure path carries no table
  name (notify1-1.2's INSERT — the implicit-transaction path).

### Unlock-notify (notify.c)

- Global blocked list (`sqlite3BlockedList`); per connection:
  `pBlockingConnection` (who currently blocks me — set by
  `sqlite3ConnectionBlocked(db, blocker)` on every lock failure),
  `pUnlockConnection` (who I registered to wait for), one `xUnlockNotify`
  callback + arg (re-registration overrides).
- `sqlite3_unlock_notify(db, cb, arg)` (notify.c:148):
  - cb==0 → unregister.
  - No pBlockingConnection → invoke cb **immediately**.
  - Deadlock: walk `p=db->pBlockingConnection; for(; p && p!=db;
    p=p->pUnlockConnection)` — reaching db ⇒ SQLITE_LOCKED with errmsg
    `database is deadlocked`.
  - Else `pUnlockConnection = pBlockingConnection`, re-insert into the
    blocked list grouped by callback (inserted before the first entry with
    a different xUnlockNotify).
- `sqlite3ConnectionUnlocked(db)` (fires when db's transaction ends): walk
  the list; clear pBlockingConnection==db; where pUnlockConnection==db fire
  the callback — consecutive same-callback entries batch into ONE invocation;
  different callbacks break the batch. Firing order = list order.
- Verified against notify1-2.3.6..11 (registration order makes db's chain
  terminate at db2's NULL pUnlockConnection; db3's registration walks
  db→db2→db3 = cycle; each COMMIT fires exactly the groups waiting on that
  connection, producing {db3}, {db3 db}, {db3 db db2}).

### Enable flag (main.c sqlite3_enable_shared_cache)

Process-global; connections opened while it is set share caches; N=0
restores. (Exact guard semantics for open connections verified during
implementation.)

## Architecture mapping (this codebase)

| C | frigolite |
|---|---|
| sqlite3_enable_shared_cache | package-level flag + DB constructor consults it (mirrors the memdb shared-store registry precedent, frigolite.go openSharedMem) |
| shared BtShared per file | shared pager registry keyed by canonical path (refcounted; torn down when the last connection closes) — connections keep their own DB/engine/schema |
| BtLock list on BtShared | lock table on the shared pager coordinator: {owner *DB, tableRoot, READ/WRITE} |
| OP_TableLock per program table | statement-level acquisition in exec: every table the statement reads → READ lock, every table it writes → WRITE lock (our statements execute atomically, so statement-level = per-op) |
| BTS_EXCLUSIVE | locking_mode=exclusive on the shared store |
| SQLITE_LOCKED_SHAREDCACHE | both message forms: `database table is locked: t` (statement table path) and `database table is locked` (implicit-transaction path) |
| notify.c blocked graph | per-DB blockedBy/unlockConnection + one notify callback (script) on the *DB handle; global list mirroring the grouping rule |
| sqlite3ConnectionUnlocked | fires at COMMIT/ROLLBACK (txn end) of the blocking connection |

## Slices

- **S1 engine core**: enable flag; shared pager registry (canonical path
  key, refcount, last-close teardown); lock table + query/set on the shared
  coordinator; statement-level acquisition wired into exec (read/write table
  collection incl. trigger bodies); error plumbing (both message forms).
  Native pins replace the documented-gap section of
  frigolite_shared_test.go.
- **S2 unlock-notify**: per-DB blocked-state + callback registration,
  immediate fire, deadlock walk, grouped firing at txn end. Native pins for
  2.3.6-11 ordering.
- **S3 fixture machinery**: converter commands (`sqlite3_enable_shared_cache`,
  `db unlock_notify {script}`), single-file regeneration of notify1.json
  (`-only notify1`), harness step execution (callback scripts mutate runtime
  vars; deadlock error capture). Family runs after this slice.
- **S4 schema coherence**: shared-cache connections observe each other's DDL
  (schema-cookie reload at statement start) — also targets the multiplex
  3.1.x residue class.
- **S5 residuals**: >16 waiters (notify1-3.x/4.x), schema locks
  ("database schema is locked"), drop-table notification, remaining leaf
  classes discovered by the family run.

## Verification protocol

- Per-family leaf register after S3+: `FRIGOLITE_TEST=notify1`, leaf-exact
  extraction (`grep -a -- '--- FAIL: TestSQLiteSuite/.*/'` on a FILE).
- Leaf-exact sweep of the spot list after each engine slice (the lock
  acquisition must be inert while shared cache is disabled — the default).
- Native pins: frigolite_shared_test.go (oracle contract documented there).
- Full suite only at the round checkpoint.
