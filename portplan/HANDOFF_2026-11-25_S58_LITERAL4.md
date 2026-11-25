# Session handoff — S58: malformed numeric separator token text (2026-11-25, 976c3f169)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code committed at `976c3f169`; this docs commit records the
verified state and the next unit.

## Landed

- `literal-4.2`, `literal-4.13`, `literal-4.14` are GONE, along with the
  `literal` family-level FAIL line.
- Numeric lexing now follows SQLite's QNUMBER contract:
  `tokenize.c:421-497` consumes every `_` in an integer/fraction/exponent/hex
  run, and `util.c:332-352` validates separator neighbours before dequoting.
  Malformed separators report the ORIGINAL whole token (`1_.4`, `12._34`,
  `12_.34`, `0xFF__EF`, ...). Valid separators (`123_456`, `1.4_2`,
  `1.0e1_2`, `0xFF_FF`) are unchanged.
- `internal/sql/lexer.go` shed its 195-line numeric block; the scanner lives in
  `internal/sql/lexer_number.go` (the old file was 992 lines, one short of the
  hard 1000-line gate). `frigolite_literal4_t23_pin_test.go` is the pure-Go pin.

## Verification

- Pin was RED first (`SELECT 1_.4` → `unrecognized token: "1_"`), GREEN after
  the scanner port. It covers all three target leaves, the neighbouring 4.x
  token-boundary shapes, malformed hex separators, and valid separator values.
- Focused gates: `literal`, `literal2`, `hexlit` all 0 leaves.
- Full gate `/tmp/t23_full_gate.log` (1253 leaves, 1146s): vs `/tmp/t22_full.log`
  GONE = `literal` + the three target leaves, NEW=1
  (`temptable2/temptable2-4.1.1`, the documented flake: present in durable t10,
  absent in t22 and in the focused `/tmp/t23_temptable2.log`). NEW=0 vs the
  durable `/tmp/t10_full2.log`.
- Quality `/tmp/t23_quality2.log`: normalized staticcheck/gocognit/gocyclo
  NEW=0 vs t22 (one helper split removed the initial `validSeparators`
  complexity finding); overall exit 1 remains the pre-existing file-size gate.
- Converter `/tmp/t23_tclconvert_all.log`: 8/8 failure names identical to
  `/tmp/head_tclconvert_all.log`. `go test ./internal/... -count=1` green.

## Restart point / next unit

Next unit: printf `%j/%J` JSON quoting leaves (`printf-20.9`–`20.27` subset).
After that: `quote` double-quoted identifier rejection, `unhex`, interleaved
row-callback step, then pager1 directory hierarchy / S34 journal cards /
temptable2-9.x and where2-2.4 watchlist. Read `TODO.md` first; use raw-byte
leaf-name fail sets and a pure-Go pin before converter/engine edits.

## Risks / working rules

- Do not patch the three strings in isolation. The QNUMBER scan/dequote rule
  is the contract; malformed separators must keep the whole raw token.
- Exponent detection still decides the token boundary: `1.4e+_4` errors on
  `1.4e`, not the full input.
- Hex separators validate against hex digits, decimal against digits.
- Keep full gates detached with `-timeout 30m` and status files; never edit the
  engine while one is running. Compare raw fail sets, never counts or line
  greps.
