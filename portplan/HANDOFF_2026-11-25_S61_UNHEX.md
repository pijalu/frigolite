# Session handoff — S61: unhex fixture parameters and escapes (2026-11-25, 3a9e6ed2d)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code/fixture committed at `3a9e6ed2d`; this docs commit
records the verified state and opens the next unit.

## Landed

- All five `unhex` leaves are GONE (`unhex`, `1.1.1`, `1.1.2`, `6.1`, `6.3`).
- `tools/tclconvert/tcl/interp_sql.go`: `tclSQLLiteral` now quotes
  non-canonical decimal spellings with leading zeros. Tcl's `db eval` binds
  `$hex` as TEXT (`tclsqlite.c:1505-1544`), so `"0000"` must become SQL
  `'0000'`, not integer `0`.
- `tools/tclconvert/tcl/interp.go`: `\u` decoding consumes 1–4 hex digits
  via `decodeUnicodeEscape`. `unhex.test` uses both `\u0E01` and `\uE01`;
  the old exactly-4-digit scan left literal `uE01` bytes in the fixture.
- Converter regressions: `TestTclSQLLiteral` leading-zero cases and
  `TestDecodeBackslashEscapeShortUnicode`.
- Pure-Go pin `frigolite_unhex_t26_pin_test.go` proves the engine already
  returns `0000` for bound text and handles real Thai UTF-8 separators.

## Verification

- Pin GREEN (`/tmp/t26_unhex_pin.log`); focused `unhex` gate
  `/tmp/t26_unhex_focus1.log`: 0 leaves.
- `go test ./internal/... -count=1` green (`/tmp/t26_internal.log`).
- Converter `/tmp/t26_tclconvert_all2.log`: 8/8 failure names identical to
  `/tmp/head_tclconvert_all.log`, NEW=0/GONE=0.
- Quality `/tmp/t26_quality2.log`: normalized NEW=0, GONE=1 (lower complexity
  in `decodeBackslashEscape`) vs `/tmp/t25_quality2.log`; overall exit 1 is
  the pre-existing file-size gate.
- Full gate `/tmp/t26_full_gate.log` (1226 leaves, 1162s): raw-byte diff vs
  `/tmp/t25_full_gate.log` has GONE=the five target leaves plus documented
  `temptable2-4.1.1`, NEW=documented `resetdb-300`; vs durable
  `/tmp/t10_full2.log` NEW=0, GONE=125. No `unhex` leaves remain.

## Restart point / next unit

Next unit: interleaved row-callback step. After that: pager1 directory
hierarchy / S34 journal cards / temptable2-9.x and where2-2.4 watchlist.
Read `TODO.md` first; use raw-byte leaf-name fail sets and a pure-Go pin
before converter/engine edits.

## Risks / working rules

- Do not change `unhex` engine behavior. The failures were fixture
  conversion: bound TEXT became a bare numeric literal, and 3-digit Tcl
  `\u` escapes were not decoded.
- Do not quote all numeric-looking Tcl values: canonical numeric inlining is
  load-bearing elsewhere. Only non-canonical leading-zero spellings change.
- Unicode escape parsing must stop at the first non-hex byte; blindly taking
  four bytes would turn `\uE01ABCD` into U+E01A.
- Keep full gates detached with `-timeout 30m` and status files; compare raw
  fail sets, never counts or line greps.
