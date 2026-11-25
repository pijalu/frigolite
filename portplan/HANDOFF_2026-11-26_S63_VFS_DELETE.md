# Session handoff — S63: test-VFS unregistration (2026-11-26, 6266740fd)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code/fixture committed at `6266740fd`; this docs commit
records the verified state and opens the next unit.

## Landed

- `pager1` and `pager1/pager1-35` GONE.
- Root cause: `tv delete` was dropped by the converter. The default testvfs
  fixture (`tv`) was then overwritten by the noShm/iVersion registrations in
  pager1-21.x and reinstalled on the next `reopen_db`, so
  `PRAGMA journal_mode=WAL` silently refused (`ErrWALUnsupported`), no
  `-wal`/`-shm` was created, and the fixture's `hexio_write test.db-shm`
  failed with "no such file or directory".
- Converter: `delete` is a zero-argument testvfs option; `commandTestVFSConfig`
  emits `delete_vfs` and removes the command from `testvfsNames`. C's
  `test_vfs.c CMD_DELETE` calls `Tcl_DeleteCommand` (src/test_vfs.c:1304).
- Harness: `delete_vfs` removes the fixture, clears a matching
  `testvfsDefault`, is excluded from named-connection routing, and is handled
  in faultsim trial scope too.
- Fixture: two `delete_vfs` steps added in `setup_232`/`setup_233`. No wholesale
  regeneration — that current-converter output rewrote thousands of unrelated
  steps and regressed the family.
- Native pins: `frigolite_pager1_t28_pin_test.go` (wal-index shm file contract)
  and `tools/tclconvert/tcl/testvfs_delete_test.go` (converter semantics).

## Verification

- Pins PASS (`/tmp/t28_pager1_pin_final.log`); focused pager1 0 leaves
  (`/tmp/t28_pager1_focus4.log`).
- Converter failure-name set 8/8, NEW=0/GONE=0
  (`/tmp/t28_tclconvert_all2.log` vs `/tmp/head_tclconvert_all.log`).
- Quality normalized 659/659, NEW=0/GONE=0
  (`/tmp/t28_quality2.log` vs `/tmp/t27_quality2.log`).
- Build/vet/SOLID green (`/tmp/t28_build.log`, `/tmp/t28_vet.log`,
  `/tmp/t28_solid.log`).
- Full gate `/tmp/t28_full_gate.log` (1207.385s, 1220 leaves): raw-byte leaf
  diff vs `/tmp/t27_full_gate.log` NEW=0, GONE=`pager1` and
  `pager1/pager1-35`; vs durable `/tmp/t10_full2.log` NEW=0, GONE=131.

## Restart point / next unit

Next unit: S34's two journal cards — (a) ROLLBACK leaves a DELETE-mode
`test.db-journal` on disk (Restore never ends the journal epoch; C's
`pager_playback → pager_end_transaction` unlinks it), and (b) close mid-txn on
a non-WAL pager flushes instead of rolling back (`Pager.Close → flushAll`;
C's `pagerUnlockAndRollback` rolls back). See `portplan/PLAN_S34_regress.md`
§Carded. Write pure-Go pins for the on-disk residue and the close semantics
before any engine edit.

## Risks / working rules

- `tv delete` unregisters the VFS command for FUTURE opens; an already-open
  connection keeps its installed hook until close/reopen, matching
  `sqlite3_vfs_unregister`.
- Other source `tv delete` sites are still absent from checked-in fixtures;
  installing them is a corpus migration with its own gate.
- Do not use a whole-family converter regen for a focused fixture patch.
- Keep full gates detached with `-timeout 30m` and status files; compare raw
  fail sets, never counts or line greps. Use the wait time for investigation
  and non-conflicting checks, never engine edits.
