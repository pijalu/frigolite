# Plan — S58: malformed numeric separator token text (2026-11-25)

Mode: debug — three raw-byte leaf failures are reproduced; localize to the
lexer's numeric separator scan and make the smallest SQLite-shaped fix.

## Evidence

- Focused literal gate retains exactly `literal-4.2`, `literal-4.13`,
  `literal-4.14`:
  - `SELECT 1_.4` → got `unrecognized token: "1_"`, want
    `unrecognized token: "1_.4"`.
  - `SELECT 12._34` → got success, want
    `unrecognized token: "12._34"`.
  - `SELECT 12_.34` → got `unrecognized token: "12_"`, want
    `unrecognized token: "12_.34"`.
- Oracle `/usr/bin/sqlite3` 3.54 confirms the full-token messages above and
  also confirms valid separators (`123_456`, `1.4_2`, `0xFF_FF`) stay valid.
- SQLite ground truth: `tokenize.c:433-497` scans every `_` in a numeric run
  as `TK_QNUMBER` (it does **not** require a following digit at scan time);
  `util.c:332-352` (`sqlite3DequoteNumber`) then rejects each separator unless
  both neighbours are digits (or hex digits for `0x...`) and reports the
  ORIGINAL whole token.

## Root cause

`internal/sql/lexer.go` (`readDigits`, `scanHexDigits`) only skips `_` when
the next byte is a digit/hex digit. When the separator is followed by `.`,
`e`, another `_`, or end-of-input, Frigolite stops early and either emits a
truncated identifier tail (`1_`, `12_`) or consumes the `_` in the wrong
fraction context (`12._34` becomes `12.34`).

## Change (one coherent slice)

1. **Pin first (RED).** Add `frigolite_literal4_t23_pin_test.go` driving
   `frigolite.Open/Query` directly. Cover the three target leaves plus the
   neighbouring 4.x shapes (`1_`, `1e_4`, `1_e4`, `1.4_e4`, `1.4e+_4`,
   `1.4e4_`, `12__34`, `12.34_`, `1.0e1_______2`, `0xFF__EF`, `0xFFEF_`)
   and the valid separator contracts (`123_456`, `1.4_2`,
   `1.0e1_2 = 1e12`, `0xFF_FF`, `0xFF_EF`). Run it before engine edits and
   record the exact failure.

2. **Port the C-shaped scanner.** Create `internal/sql/lexer_number.go` and
   move all numeric-token helpers out of `lexer.go` (that file is already 992
   lines; the hard gate is 1000). Implement:
   - `scanNumberToken(input, start) (raw, value string, next int, ok bool)`.
   - Integer / fraction / exponent / hex runs accept every `_` and set a
     `qnumber` flag, matching `tokenize.c`.
   - Exponent recognition still requires a digit (optionally after `+/-`)
     immediately after `e/E`, preserving `1.4e+_4` → `1.4e`.
   - The C `while (IdChar(z[i]))` tail is appended after numeric scanning;
     any tail makes the token illegal.
   - For `qnumber` tokens with no tail, validate each `_` with digit/hex-digit
     neighbours; invalid → `TokenUnrecognized` carrying the full raw token,
     valid → `TokenNumber` carrying the dequoted token.
   - `Tokenizer.readNumber` becomes a thin wrapper; `lexer.go` loses the old
     `readNumber`, `scanDigits`, `isSimpleIntegerEnd`, `isHexPrefix`,
     `tryReadHexNumber`, `scanHexDigits`, `consumeIdentTail`, `readFraction`,
     `readExponent`, `readDigits` implementations.
   - No harness/converter edits: the existing `lexErrorToken` already formats
     `TokenUnrecognized.Value` as `unrecognized token: "%s"`.

3. **Verify.**
   - `go test -run '^TestUnsignedHexLiteralInt64Wrap$' -count=1 .` stays green.
   - New pin green.
   - `FRIGOLITE_TEST=literal go test -run '^TestSQLiteSuite$' -count=1 -v`
     leaves = ∅.
   - `FRIGOLITE_TEST=literal2 go test ...` and `FRIGOLITE_TEST=hexlit go test
     ...` stay at their current 0-fail state.
   - Full gate detached (status file, `-timeout 30m`); raw-byte leaf fail-set
     diff vs `/tmp/t22_full.log` (1256) and durable `/tmp/t10_full2.log`:
     NEW=0, target three GONE, only documented flakes remain.
   - `tools/quality_gate.sh` normalized findings NEW=0 vs
     `/tmp/t22_quality.log`; overall file-size failure remains pre-existing.
   - `go test ./internal/... -count=1` green.

4. **Land.** Commit code separately. Then write
   `portplan/HANDOFF_2026-11-25_S58_LITERAL4.md`, update `TODO.md` state /
   immediate next action, append durable lessons to
   `.agents/lessons_learned.md`, update the goal todo, and commit docs.

## Carded / risks

- Do not "simplify" by only patching the three strings: the fix must cover
  the whole QNUMBER scan/dequote rule or other malformed separator shapes
  will regress.
- Keep `1.4e+_4` truncated at `1.4e`: exponent detection, not separator
  validation, decides that token boundary.
- Hex separators validate against `isxdigit`, not `isdigit`.
- Full gates are detached and never edited while running. Use raw-byte
  leaf-name fail sets, never counts or line greps.
