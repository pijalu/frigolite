# Plan — S54 slice 4: regenerate fts3cov with wrappers (2026-11-25)

## Problem

`fts3cov` has 2 leaves: `setup_2` (`near ")": syntax error`) and
`fts3cov-11.1` (`table xx already exists`). The family uses
`malloc_common.tcl`'s `do_write_test`, `do_error_test` and `do_malloc_test`;
the existing fixture predates the new wrapper handlers, so its setup and
database-lifecycle steps are incomplete/stale.

## Execution note (2026-11-25)

Regenerating the fixture closed `fts3cov-11.1` but exposed `2.3/2.5`: the
fixture's `read_fts3varint [string range $root 1 end] left_child` was dropped,
so the corruption step used an unset variable. Fix that class in both layers:

- `tclconvert`: emit `read_fts3varint` as a `runtime_expr` that assigns the
  decoded left-child block to its variable.
- Harness runtime command layer: implement `read_fts3varint` and
  `string range` so the expression can be evaluated with the runtime `$root`
  blob.

No engine change is needed for the converter step itself; the engine already
reports malformed segments when the extracted child block is actually removed.

## Exact changes

1. Regenerate only `testdata/fts3cov.json` with the current converter
   (`-testdir ../sqlite/test -outdir testdata -only fts3cov`).
2. Inspect the focused raw fail set vs `/tmp/t17_full.log`; if new leaves
   appear, triage from first failing step before any engine change.
3. Add a pure-Go pin only if the regenerated fixture still fails on an
   engine-visible contract.

## Verification

- Focused `fts3cov` raw diff: NEW=0, target leaves gone if fixed.
- Converter full-package failure-name set vs clean HEAD unchanged (8).
- Quality NEW=0; detached full gate raw diff vs `/tmp/t10_full2.log`
  NEW=0.

## Execution record (2026-11-25)

- Regenerated `testdata/fts3cov.json`; committed with converter/harness support
  at `b140ccfa7`. No engine change.
- Focused gate `/tmp/t18_focus3.log`: exit 0; raw fail-set diff vs
  `/tmp/t17_full.log` NEW=0, GONE=2 (`fts3cov`, `fts3cov-11.1`).
- Converter full-package failures vs `/tmp/t17_tclconvert_all.log`: base=8,
  cur=8, NEW=0.
- Quality `/tmp/t18_quality.log`: staticcheck/gocognit/gocyclo NEW=0; only
  pre-existing file-size violations remain.
- Full gate `/tmp/t18_full.log` (1172.895s, 1286 leaves): vs `/tmp/t17_full.log`
  NEW=`atof1`, `atof1/atof1-2.40` only; GONE=3 (`fts3cov`,
  `fts3cov-11.1`, `resetdb/resetdb-300`). vs durable `/tmp/t10_full2.log`
  NEW is only `atof1`/`atof1-2.40`, the documented random-dice flake;
  focused `/tmp/t18_atof1_focus.log` is green.

## Risks / non-goals

- No hand-editing JSON; regenerate only this family.
- No engine change unless a pure-Go pin fails.
- Full gates use raw-byte leaf-name sets only.
