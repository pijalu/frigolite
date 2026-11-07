# PLAN S9 — wal-18.2.x (24): the WAL log page-size validation surface

## Diagnosis (verified 2026-11-07)

The 24 `wal-18.2.x` failures are NOT interrupt choreography (the TODO/
HANDOFF label was stale). The C source (wal.test:1145-1236) crafts, per
(page-size, page-number) pair, a hand-written WAL: header magic 0x377F0682,
version 3007000, page-size `$pgsz`, checkpoint seq 1234, salts 22/23, the
LE-mode header checksum, then ONE frame (pgno `$pg`, commit nTruncate=5,
salts 22/23, chained checksums) whose body is pgsz random bytes.

- Valid pgsz (512..65536, power of 2) → recovery applies the garbage frame
  → the db is corrupt → `integrity_check` fails (works=1).
- Invalid pgsz (128, 256, 131072, 1016) → the log is ignored → `ok`
  (works=0).

The converter DROPS the crafting `do_test` body entirely (`binary format`,
`binary scan` via the logcksum proc, `open/puts/close` file writing are
unmodeled). The fixture's .5 assertions therefore run against a NONEXISTENT
-wal: the 12 invalid-pgsz cases pass vacuously; the 24 valid-pgsz cases
(expect corruption) fail.

The ENGINE already implements the contract: `internal/pager/walrecover.go`
`parseRecoverHeaderLocked` rejects magic≠WAL, szPage non-power-of-2,
szPage<512 or >65536 (wal.c's walIndexRecover header gate) and walks frames
with walDecodeFrame's salt+checksum rules.

Converter note: `binary format`/`binary scan` appear in **43** ../sqlite
test files — a whole converter class, carded separately (see TODO queue).
Per the pure-Go supersession policy, this unit pins the engine-visible
contract natively and skips the 24 affected fixture assertions with the
citation, keeping the 12 invalid-pgsz fixture assertions (they pass and
remain live).

## Changes

1. **New `frigolite_wal18pgsz_pin_test.go`** — `TestWalLogPageSizeValidation`:
   - Build the wal-18.0 fixture db natively: page_size=1024, WAL, CREATE
     TABLE t1(a,b UNIQUE), one row, checkpoint, three more rows (the
     6-frame log), copy test.db + test.db-wal aside, close.
   - For pgsz ∈ {128,256,512,1024,2048,4096,8192,16384,32768,65536,131072,
     1016} × pg ∈ {1,2,3}: restore test.db, delete the -wal, craft the WAL
     exactly as the TCL (BE fields, `pager.WalChecksumBytes(false, …)`
     chain, deterministic pseudo-random frame body), open the db, run
     `PRAGMA integrity_check` under catch: valid pgsz ⇒ rc≠0 OR msg≠"ok";
     invalid pgsz ⇒ exactly "ok".
   - Also assert the crafted file size is 32+24+pgsz (the TCL .4 want).
2. **`harnessSkipSubtests`** — add the 24 keys `wal/wal-18.2.<tn>.<pg>.5`
   for tn ∈ 3..10, pg ∈ 1..3, reason: the binary-format crafting surface is
   not expressible in the converter; contract pinned by
   TestWalLogPageSizeValidation.

## Verification

- `go test -run TestWalLogPageSizeValidation -count=1 .` green.
- wal sweep name-set diff vs portplan/baseline_wal_sweep_2026-11-07_S8.txt:
  exactly the 24 wal-18.2.x names removed, zero new, run COMPLETED check.
- Lessons + TODO/HANDOFF records updated; the 43-file binary-format
  converter class added to the carded queue.
