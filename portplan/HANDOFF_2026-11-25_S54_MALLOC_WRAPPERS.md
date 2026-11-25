# Session handoff — S54 slice 3: malloc_common wrappers (2026-11-25, 7aced89b1)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Converter/fixture slice committed at `7aced89b1`; this
docs commit records the verified state and the next unit.

## Landed

- `7aced89b1` — `tclconvert` now handles `malloc_common.tcl`'s
  `do_write_test` and `do_error_test`: successful writes become exec steps
  and expected errors become catch steps (`1 {msg}`). Regenerated
  `testdata/fts3malloc.json`; added a converter unit test.
- Verification: focused `fts3malloc` 0 leaves (raw diff vs `/tmp/t16_full.log`
  NEW=0, GONE=5); full gate `/tmp/t17_full.log` (1183.785s, 1287 leaves)
  raw-byte diff vs durable `/tmp/t10_full2.log` NEW=0, GONE=64; converter
  failure-name set unchanged vs clean HEAD (8 baseline failures); quality
  staticcheck/gocognit/gocyclo NEW=0. `resetdb-300` was absent in the prior
  run but present in t10 and a focused re-run — documented flake.

## Restart point

Next unit: remaining fts3* residue (~17 leaves), starting with `fts3query`
(11), `fts3defer3-1.7`, or the two `fts3cov` leaves. Current full-gate
leaves:

- `fts3auto/setup_2`, `fts3cov/fts3cov-11.1`, `fts3defer3-1.7`
- `fts3query` 5.2.1, 5.3.1, 5.4.1, 7.2, 7.2.1.1.1, 7.2.1.2.1,
  7.2.6.1.1, 7.2.6.2.1, 7.3..2, 7.3..4

`fts3cov` also uses malloc_common wrappers; regenerating that family with
the new handlers may close its two leaves without an engine change.

## Risks / working rules

- The converter materializes only the completed SQL/error behavior of the
  malloc wrappers; it does not emulate OOM injection.
- Converter tests are in a `.gitignore`d subtree: force-add new unit tests
  if `git add` prints an ignore warning, and verify `git show --stat`.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Full gates
  need `-timeout 30m` and detached execution (tool cap is 300s).
- Commit each verified unit immediately; use a worktree for HEAD baselines.
