# WAL index races under `-race` (2026-11-26)

## Repro

`FRIGOLITE_TEST=__none__ go test -timeout 30m -race -count=1 -run '^Test[^C]' ./...`
failed the root package at `TestWalRestartConcurrent` and
`TestWalSetlkRecoverEngine` (12 `WARNING: DATA RACE` reports,
`/tmp/final_race_nonharness.log`). Non-race gates do not expose these.

## Root cause

`WALIndex.mu` guards `pages`, `dirty`, and `aLock`; methods suffixed
`Locked` require the caller to hold it (normally via `WriterSection`).

1. `walWriter.ckptBackfillPass` (`internal/pager/wal.go:1095` and `:1103`)
   calls `setCkptInfoLocked` directly while holding only the READ_LOCK(0)
   shm byte. Concurrent `tryHeaderRefresh` / `commit` WriterSections call
   `persistAllLocked`, racing on the dirty map and page-0 bytes (11 reports).
2. `walWriter.walRecoverWithWriterLock` (`internal/pager/wal.go:361`, `:372`)
   calls `shmTryLockHeld` / `shmUnlockHeld` while explicitly NOT holding
   `WALIndex.mu` (the recovery reads run private buffers). Those helpers
   mutate `aLock` as if the mutex were held, racing with a peer's public
   `shmTryLock` (1 report in `TestWalSetlkRecoverEngine`).

## Fix

- Wrap both `setCkptInfoLocked` calls in `ckptBackfillPass` with small
  `w.wi.WriterSection` sections, preserving the existing rule that callback-
  driven lock attempts never run under the mutex. The copyback itself still
  runs outside the section.
- In `walRecoverWithWriterLock`, use the public non-held `w.shmTryLock` /
  `w.shmUnlock` primitives, which take `WALIndex.mu` around the shadow
  mutation. Hooks therefore fire before lock acquisition, consistent with
  other public lock paths and avoiding hook re-entry deadlocks.
- No file-format, lock ordering, or SQL behavior change.

## Verification

1. `go test -race -count=20 -run '^(TestWalRestartConcurrent|TestWalSetlkRecoverEngine)$' .`
2. Full non-harness race step with `FRIGOLITE_TEST=__none__`.
3. Quick gates: pinned staticcheck, complexity ratchet, SOLID, coercion pins,
   `go build`, `go vet`, SHarded harness NEW=0, coverage gate.
4. Push to main and observe CI/CD green.

## Residual risk

POSIX flock locks remain per-process; the in-process `aLock` shadow is still
the intra-process arbiter. The fix only restores the documented mutex
contract — it does not redesign WAL lock arbitration.
