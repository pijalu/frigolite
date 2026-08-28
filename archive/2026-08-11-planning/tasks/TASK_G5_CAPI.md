# TASK G5 — C-API Paradigm Port (Go-idiomatic SQLite API surface)

> **Phase**: G5 (depends on G4 core goals green)
> **Goal IDs**: G5.STMT, G5.BIND, G5.BLOB, G5.BACKUP, G5.HOOKS
> **Read first**: `PORTPLAN.md` §0 (principle #4: stdlib; #5: small goals),
> **`portplan/DESIGN.md` §A (materialization constraint) + §G (C-API Go-idiomatic
> design: Stmt/Bind/Step, Blob, Backup, Hooks, transpiler mappings)**,
> `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started
>
> **Directive** (from project owner): "C-API should be ported to offer a Go
> similar functional approach." → **Port C-API constructs to a Go paradigm;
> never port the C API verbatim.** The 54 C-API testgen packages (backup, bind,
> blob, hooks, changes, notify, capi*, etc.) are currently skipped as "frigolite
> has no C API". **Implement the equivalent Go-idiomatic API and map the test
> patterns through the transpiler.**

---

## Objective

Frigolite currently exposes only `Open/Close/Exec/Query` (fire-and-forget, whole-
result-materializing). SQLite's TCL tests exercise the **C API** (prepare/step/
bind/reset/finalize, blob handles, backup, update/commit/rollback hooks, progress
handlers, changes/last-insert-rowid, authorizer, busy handler). To make those 54
packages green, build a **Go-idiomatic equivalent** and teach the transpiler to
emit it.

**Design principle**: a Go-native API, not a C-struct mimic. Use Go idioms:
- `*Stmt` (prepared statement) with `Step()`, `Reset()`, `Close()`; `Column*`
  accessors return typed Go values (`int64`, `float64`, `string`, `[]byte`,
  `interface{}`/nil).
- `Stmt.Bind(index, value)` (and a 1-based positional convenience).
- `*Blob` with incremental `Read`/`Write`/`Seek`/`Close` (implements
  `io.ReadWriteSeeker` where sensible) — uses Go stdlib `io`.
- `Backup` with stepwise `Step(nPages)`/`Remaining`/`PageCount`/`Finish`.
- Callback hooks as Go function types: `func(op, db, table string)`,
  `func() bool` (progress/busy), `func(op AuthorizerOp, ...) AuthResult`.

---

## Goal G5.STMT-BIND — Prepared statements + parameter binding

**Scope** (`internal/` new package or `internal/exec`): a `Stmt` type + the
existing engine. Transpiler emits `db.Prepare(sql)` → `stmt.Bind(...)`/`Step()`.
Reference SQLite `src/prepare.c`, `src/vdbeapi.c`, `src/bind.c`.

**Scope of tests (testgen)**: `bind`, `bind2`, `capi2`, `capi3`, `capi3b`–`capi3e`,
`resetdb`, `colmeta`, `tkt2332`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/bind/ ./testgen/capi3/ ./testgen/colmeta/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP5Stmt' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Design `Stmt` API in `frigolite.go` (or a new `frigolite/stmt.go`):
   `Prepare(sql) (*Stmt, error)`, `Stmt.Bind(index int, v interface{})`,
   `Stmt.Step() bool`, `Stmt.Column*()`, `Reset()`, `Close()`.
2. Implement on top of the existing exec engine (the engine already parses +
   plans; wire `Step` to row-by-row iteration instead of full materialization —
   this also enables the query read-locking needed by G2 `tkt1873`).
3. `sqlite3_bind_*` semantics: type coercion, SQLITE_RANGE on bad index, blob
   lifetime (copy vs static), clear-bindings.
4. Column metadata (`colmeta`): name, decltype, collation, origin flags.
5. Teach `tools/tcl2go/` to emit the new API for `sqlite3_prepare`/`step`/`bind`/
   `reset`/`finalize`/`column_*` TCL calls.
6. Un-skip the C-API bind/capi packages; regenerate; per fix: pre-test + oracle
   → fix → verify → commit.

---

## Goal G5.BLOB — Incremental BLOB I/O

**Scope**: `incrblob`, `incrblob2`–`incrblob4`, `incrblob_err`, `incrblob2_err`,
`e_blobopen`, `e_blobbytes`, `e_blobclose`, `e_blobwrite`, `zeroblob`.

**Design**: `db.OpenBlob(db, table, col, rowid, write bool) (*Blob, error)`;
`Blob` implements `io.ReadWriteSeeker` + `Size()`. Reference SQLite
`src/vdbeblob.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/incrblob/ ./testgen/zeroblob/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP5Blob' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `OpenBlob` opens a read/write handle to a BLOB column by rowid; `Blob` seeks,
   reads, writes in-place; grows on write-past-end.
2. Map `sqlite3_blob_open/read/write/bytes/close` in the transpiler.
3. Handle zeroblob, the `e_blob*` lifecycle/error tests.
4. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G5.BACKUP — Online backup

**Scope**: `backup`, `backup2`, `backup4`, `backup5`.

**Design**: `db.Backup(dest, destDbName) (*Backup, error)` → `Step(n) (Done,
error)`, `Remaining()`, `PageCount()`, `Finish()`. Reference SQLite `src/backup.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s ./testgen/backup/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP5Backup' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Page-level copy between two `Pager`s with reader/writer coordination.
2. Step semantics (n=-1 = all remaining); remaining/pagecount; concurrent-write
   retry.
3. Map `sqlite3_backup_*` in the transpiler.
4. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G5.HOOKS — Update/commit/rollback hooks, progress, busy, changes, notify

**Scope**: `hook`, `hook2`, `changes2`, `e_changes`, `e_totalchanges`, `notify`,
`notify1`–`notify3`, `dbstatus`, `interrupt`/`interrupt2`, `autoinc`,
`dataversion1`, `imposter1`.

**Design**: Go callback registration on `*DB` (already partial: authorizer,
progress, limit). Add: `SetUpdateHook(func(op, db, table string, rowid int64))`,
`SetCommitHook(func() bool)`, `SetRollbackHook(func())`, `SetBusyHandler(func(n)
bool)`, `Changes()`/`TotalChanges()`. Reference SQLite `src/main.c`
(hook registration), `src/notify.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/hook/ ./testgen/changes2/ ./testgen/notify/ ./testgen/e_changes/ \
  ./testgen/e_totalchanges/ ./testgen/autoinc/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP5Hooks' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Register + fire update/commit/rollback hooks at the right engine points.
2. `Changes`/`TotalChanges`/`LastInsertRowID` (partial — complete the contract).
3. Busy handler + progress handler + interrupt (cooperative cancel via a context-
   like flag). `dbstatus` memory stats (Go runtime-based, stdlib `runtime`).
4. Notify (threaded dispatch) — Go-native: the threading is the Go scheduler; map
   the SQL-observable behavior, not the C mutex internals.
5. `imposter1`: writable_schema imposter tables (test-only; map to a Go API).
6. Map `sqlite3_*_hook`/`update_hook`/`busy_handler`/`*_changes` in transpiler.
7. Per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All five goals green; pre-tests pass; quality + SOLID pass; no G1–G4 regression.
- The new Go-idiomatic API is documented (GoDoc on every exported symbol).
- `PORTPLAN.md` §5 G5 rows → 🟢.
