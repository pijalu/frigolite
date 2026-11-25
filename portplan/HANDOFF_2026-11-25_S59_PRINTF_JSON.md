# Session handoff — S59: printf `%j/%J` JSON quoting (2026-11-25, 796615dfe)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code committed at `796615dfe`; this docs commit records the
verified state and the next unit.

## Landed

- All 14 `printf-20.*` leaves plus the `printf` family-level FAIL are GONE
  (`20.9`–`20.16`, `20.23`–`20.27`).
- `internal/function/printf_json.go` ports SQLite's `%j`/`%J` conversion:
  `%J` renders a JSON string literal (`null` for NULL), `%j` renders only the
  escaped contents (empty for NULL), precision truncates the ARGUMENT, and
  quote/backslash/C0 bytes use JSON backslash escapes. `function_printf.go`
  dispatches `'j'/'J'` to the new helper.
- Pure-Go pin `frigolite_printf_json_t24_pin_test.go` covers all 27
  `printf-20.*` cases (control-byte hex, NULLs, UTF-8 precision, width, `!`,
  quote/backslash/control escapes, precision 0).

## Verification

- Pin RED first (`malformed JSON` on 20.9), GREEN after the port.
- Focused `printf` and `printf2` gates: 0 leaves each.
- Full gate `/tmp/t24_full_gate.log` (1238 leaves, 1177s): NEW=0 vs
  `/tmp/t23_full_gate.log` and durable `/tmp/t10_full2.log`; GONE = the 14
  target leaves + `printf` family + the documented `resetdb-300` flake.
- Quality `/tmp/t24_quality.log`: normalized staticcheck/gocognit/gocyclo
  NEW=0 vs t23 (line-number-only shifts ignored); overall exit 1 remains the
  pre-existing file-size gate.
- Converter `/tmp/t24_tclconvert_all.log`: 8/8 failure names identical to
  `/tmp/head_tclconvert_all.log`. `go test ./internal/... -count=1` green.

## Restart point / next unit

Next unit: `quote` double-quoted identifier rejection leaves
(`quote-2.1.1`–`2.5`). After that: `unhex`, interleaved row-callback step,
then pager1 directory hierarchy / S34 journal cards / temptable2-9.x and
where2-2.4 watchlist. Read `TODO.md` first; use raw-byte leaf-name fail sets
and a pure-Go pin before converter/engine edits.

## Risks / working rules

- `/usr/bin/sqlite3` 3.54 in this environment returns NULL for `%j/%J`
  (feature absent despite the version string). A canonical SQLite build from
  the `version-3.54.0` source tarball was used as the raw-output oracle;
  fixture expectations are Tcl-canonical and the harness strips one list
  layer.
- Fixture-specific: `printf-20.20` expects right-padded width for explicit
  precision without `!`, while the built oracle emitted the un-padded raw
  string. The durable fixture is the gate; the implementation and plan record
  this discrepancy rather than editing the fixture.
- Escape quote/backslash once: the shared backslash prefix plus the switch
  arm must not double-prefix. The 20.9 full-byte sequence pins that.
- Keep full gates detached with `-timeout 30m` and status files; compare raw
  fail sets, never counts or line greps.
