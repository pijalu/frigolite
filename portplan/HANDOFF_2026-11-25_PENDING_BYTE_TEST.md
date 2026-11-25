# Handoff — pending-byte native test premise (2026-11-25)

## Goal

Fix `TestNativePendingBytePageNeverAllocated` (frigolite_autovacuum_native_test.go).
Pre-existing failure at line 130: the test acquired page 65 via
`db.pager.ReadPage(65)`, but `internal/pager/pagerpage.go:245-246` now
deliberately returns `ErrCorruptPage` for the pending-byte/lock-byte page
(C `pager.c:5577-5580` parity). The assertion mechanism, not the engine, was
stale.

## Evidence — confirm invariant before touching the test

Throwaway probe (`/tmp/pg65probe/main.go`, exported API only; `replace` to the
repo module) reproduced the exact SQL setup with `SetPendingByte(0x10000)`:

- `PRAGMA page_size` = 1024
- `PRAGMA page_count` = 158, file size 161792 bytes
- `PRAGMA integrity_check` = ok
- raw `ReadAt` at offset `(65-1)*1024 = 65536`, 1024 bytes: **0 nonzero bytes**

So page 65 truly holds no b-tree content. This is an engine guarantee; the fix
belongs in the test, not the engine. Original failure reproduced first at HEAD:
`read page 65: database disk image is malformed (lock-byte page 65)`.

## Change

`frigolite_autovacuum_native_test.go` only (plus docs):

- Added `os` and `strconv` imports.
- Replaced the `pager.ReadPage(65)` content check with a raw-file check:
  query `PRAGMA page_size`, parse it, `os.Open(path)`, `ReadAt` at
  `(65-1)*pageSize`, assert every byte is zero.
- Retained, unchanged: the per-row DELETE drain, its `PRAGMA integrity_check`
  after each delete, and the final `rootpage != 65` assertion.
- No pager guard touched (verified `git diff --name-only`).

`internal/pager/pagerpage.go:245-246` kills the previous "overridden pending
byte page is ordinary" engine bug; the guard is load-bearing and stays.

## Verification

- `go test -run '^TestNativePendingBytePageNeverAllocated$' -count=1 .` — PASS
- `go test -run TestSOLID_ ./...` — all packages ok
- `gofmt -l` clean on the changed Go file; `go vet .` clean.
- Targeted test repeated 3x, all PASS.
- Full 12-shard harness via `tools/run_json_shards.sh` (binary
  `/tmp/pg65gate/frigolite.test`, run dir
  `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/frigolite-shards.20EPSo`):
  12/12 shard logs present, no timeout/kill. Leaf FAIL set vs
  `/tmp/t30_full_gate.log` (= `/tmp/t30_baseline_leaves_clean.txt`, 1028
  leaves): **NEW=0**, GONE=6 (`insert-17.15`, `insert-5.3`, `resetdb-100`,
  `resetdb-300`, `temptable2-4.1.4/5.1.2`, `vacuum-into-500` — all fixed in
  commits already at HEAD). Family-level FAIL set: 190 vs t30's 192, **NEW=0**,
  GONE=2 (`insert`, `temptable2`). Family-level lines are NOT leaves; a broad
  `TestSQLiteSuite/\S+` regex reports 190 spurious NEW families.

## Docs

- `TODO.md`: pending-byte item marked DONE.
- `.agents/lessons_learned.md`: new "Pending-byte page: assert at the
  observable" section.
- This handoff.
