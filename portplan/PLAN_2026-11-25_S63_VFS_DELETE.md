# Plan — S63: unregister test VFS on `tv delete` (2026-11-25)

Mode: debug — `pager1-35` reproduced at HEAD with the converter dropping the
Tcl `tv delete` command, leaving a stale noShm default VFS installed on later
reopens.

## Evidence

- Focused gate `/tmp/t28_pager1_focus0.log`: only `pager1/pager1-35` fails.
- Failure: `hexio_write test.db-shm offset 16384: no such file or directory`.
- Diagnostic runs (`/tmp/t28_pager1_diag.log`, `/tmp/t28_pager1_diag2.log`):
  at the `hexio_write` step the connection reports `wal=false` and the
  fixture directory has `test.db`/`test.db-journal` but no `-wal`/`-shm`.
  The WAL `exec` step reported no error, so the mode change was a silent
  refusal, not an execution failure.
- `../sqlite/test/pager1.test:2112-2125` registers `tv -noshm`, then
  `db2 close; tv delete`, re-registers `tv -iversion`, then deletes it again.
- `../sqlite/src/test_vfs.c:1304-1307`: `CMD_DELETE` calls
  `Tcl_DeleteCommand` — it unregisters the VFS object. The trailing filename
  (if any) is not consumed by that command.
- The converter's `testvfsOptionArity("delete")` was 1, so the zero-argument
  `tv delete` fell through `commandTestVFSConfig`'s `len(args)<2` guard and
  emitted nothing. The harness therefore kept `testvfsDefault="tv"` while
  `testvfsFixtures["tv"]` had been overwritten by the noShm fixture;
  `reopen_db` reinstalled noShm on main and WAL was refused
  (`ErrWALUnsupported` → current mode `delete`).

## Change (one coherent slice)

1. Pure-Go pin first (`frigolite_pager1_t28_pin_test.go`): a fresh file DB
   enters WAL mode, writes, starts a write transaction, and requires
   `test.db-shm` to exist and cover the patched 16KiB range. It passes at
   HEAD, proving the engine materializes the wal-index correctly.
2. Converter (`tools/tclconvert/tcl/interp_stmt.go`):
   - `delete` becomes a zero-argument testvfs option.
   - `commandTestVFSConfig` emits a `delete_vfs` step carrying the VFS name
     in both `SQL` and `Database`, and removes the name from `testvfsNames`
     (C deletes the Tcl command).
   - Non-empty `SQL` keeps the step through the existing `groupToTestCase`
     whitelist; no complexity branch is added there.
3. Harness (`frigolite_harness_test.go`):
   - `delete_vfs` removes the fixture from `testvfsFixtures` and clears
     `testvfsDefault` when it names the deleted VFS.
   - The step is excluded from named-connection routing (it names a VFS,
     not a `sqlite3` handle).
   - `runFaultsimStep` handles the same step for trial scope.
4. Fixture (`testdata/pager1.json`): add the two missing `delete_vfs` steps
   in `setup_232` and `setup_233` (the `tv delete` calls after the noShm and
   iVersion probes). Do NOT wholesale-regenerate the family: the current
   converter rewrites thousands of unrelated steps and regresses the family.
5. Converter regression test (`tools/tclconvert/tcl/testvfs_delete_test.go`)
   asserts the register/delete sequence and command unregistration.

## Verify

- Pure-Go pin GREEN; focused `pager1` gate 0 leaves.
- Converter full package failure-name set unchanged vs
  `/tmp/head_tclconvert_all.log`.
- Quality normalized diff vs `/tmp/t27_quality2.log` NEW=0.
- Detached full gate; raw-byte diff vs `/tmp/t27_full_gate.log` and durable
  `/tmp/t10_full2.log`, NEW=0 except documented flakes; `pager1-35` GONE.

## Landed (code/fixture commit `6266740fd`)

- The converter now recognizes zero-argument `tv delete` as a testvfs
  unregistration (`delete_vfs` step) instead of dropping it. The step carries
  the VFS name as both `SQL` and `Database`, so it passes the existing
  `groupToTestCase` whitelist without adding a complexity branch.
- Harness `delete_vfs` removes the fixture and clears `testvfsDefault` when it
  named that VFS; named-connection routing excludes the step. `runFaultsimStep`
  handles the same marker for trial scope.
- Fixture `pager1.json` gained exactly two `delete_vfs` steps in `setup_232`
  and `setup_233`. A whole-family regen was rejected: the current converter
  rewrites thousands of unrelated steps and regressed the family.
- `TestPager1ShmMaterialized` pins the engine-side wal-index file contract;
  `TestTestVFSDeleteUnregistersWrapper` pins the converter behavior.

## Verification evidence

- Pure-Go pin PASS (`/tmp/t28_pager1_pin_final.log`).
- Focused `pager1` gate 0 leaves (`/tmp/t28_pager1_focus4.log`).
- Converter failure-name set 8/8, NEW=0/GONE=0
  (`/tmp/t28_tclconvert_all2.log` vs `/tmp/head_tclconvert_all.log`).
- Quality normalized 659/659, NEW=0/GONE=0
  (`/tmp/t28_quality2.log` vs `/tmp/t27_quality2.log`).
- Build/vet/SOLID green (`/tmp/t28_build.log`, `/tmp/t28_vet.log`,
  `/tmp/t28_solid.log`).
- Full gate `/tmp/t28_full_gate.log` (1207.385s, 1220 leaves): raw-byte leaf
  diff vs `/tmp/t27_full_gate.log` NEW=0, GONE=`pager1` and
  `pager1/pager1-35`; vs durable `/tmp/t10_full2.log` NEW=0, GONE=131; vs
  `/tmp/t26_full_gate.log` NEW=0, GONE=6 (the four S62 leaves plus these two).

## Residual risk

- Other `tv delete` sites in the source are still absent from checked-in
  fixtures. Future fixture regeneration will emit them; their runtime effect
  is now implemented, but this slice patched only the two teardowns that
  block `pager1-35`. A later corpus-migration unit should install the full set
  with its own gate.

## Risks

- A full family regeneration is a corpus migration, not this slice; apply
  only the two fixture insertions.
- Existing connections keep their installed VFS hook until close/reopen,
  matching C's `sqlite3_vfs_unregister`: deletion affects future opens.
- The old `testvfs_config delete FILENAME` file-removal branch remains for
  backward compatibility with older fixtures; all new converter output uses
  `delete_vfs` for Tcl `tv delete`.
