# PLAN — WAL lock-protocol error propagation (2026-11-25)

## Target

TODO.md review item 3 / MEDIUM: `TestWalLockProtocol/recover-range-busy`
(`frigolite_wallocks_test.go:319`) expects `locking protocol` on SELECT but
gets nil. Oracle: `sqlite/test/walprotocol.test` 1.3/1.4 — an xShmLock hook
that persistently returns SQLITE_BUSY for `{1 2 lock exclusive}` (or
`{0 1 lock exclusive}`) burns the 100-round WAL_RETRY budget and the
statement fails with `{1 {locking protocol}}`.

## Diagnosis (reproduced, instrumented)

`/tmp/walproto_debug.log` (temporary `TestZZDebugWalProto`): both Open and
SELECT issue 101 recovery rounds; every `{1 2 lock exclusive}` is vetoed,
`busyToRetryOrRecovery` converts to `errWalRetry`, and
`walIndexReadHdr` returns `errWalProtocol` at round 100 — then the error
vanishes. Both `Open` and `Query` return `err=<nil>, rows=1`.

Two swallow sites:

1. `internal/pager/pageropen.go:378` `attachWalAtOpen`'s header-2/2 /
   missing-`-wal` branch: `if w, werr := openWal(...); werr == nil`.
   On failure it skips the attach and returns nil (lazy retry is C-correct:
   `sqlite3_open` does not recover).
2. `internal/pager/external.go:91` `CheckExternalFile`'s per-statement
   `pagerOpenWalIfPresent` equivalent: `else if w, werr := openWal(...);
   werr == nil` — the failed retry's protocol error is dropped, and
   `CheckExternalFileErr` (the ONLY caller) appends `, nil`.

C contract (`../sqlite/src/pager.c:5432-5438`):
`rc = pagerOpenWalIfPresent(pPager); assert(pPager->pWal==0 || rc==SQLITE_OK);`
then `pagerBeginReadTransaction` propagates `walIndexReadHdr`'s rc. The
per-statement attach error is statement-visible; the open-time swallow is
left in place (C's open is lazy, our retry at the next statement is the
faithful point).

## Change (smallest complete, no simplification)

- `internal/pager/external.go`: `CheckExternalFile() (bool, error)`;
  - WAL branch: return `walIndexRefreshLocked`'s error (dead via current
    caller but keeps the method self-consistent).
  - per-statement attach: propagate `openWal`'s `werr` and the post-attach
    `readPageLocked(1)` `rerr`.
- `internal/pager/wallocks.go`: `CheckExternalFileErr` returns
  `p.CheckExternalFile()` (now `(bool, error)`) in the no-wal branch.
- No test/hook weakening. No dependency change.

## Ordered steps

1. This plan (done).
2. Edit `external.go`, then `wallocks.go`.
3. Delete `zz_debug_walproto_test.go`.
4. Focused: `go test -run '^TestWalLockProtocol$' -count=1 .`.
5. Verify cmd:
   `go test -run '^TestWalLockProtocol$' -count=1 . && go test -run TestSOLID_ ./...`.
6. WAL-family harness fail-name diff vs `/tmp/t30_full_gate.log`: NEW must be 0.
7. `go test ./...`; quality gate on changed files.
8. Commit; update TODO.md item 3 (DONE + evidence) and write a handoff.

## Residual risk

Other `openWal` failures now surface at the statement where they were
previously dropped. Full `go test ./...` and the WAL-family gate diff are
the guard; the change follows C's rc propagation rather than a special case
for `errWalProtocol`.
