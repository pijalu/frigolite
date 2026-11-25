# Plan — S57: unsigned hex literal two's-complement parity

## Evidence

Full gate `/tmp/t21_full.log`:

- `hexlit-163`: `SELECT 0x8000000000000000` → `hex literal too big`
  (expected MinInt64).
- `hexlit-164`: `SELECT 0XFFFFFFFFFFFFFFFF` → `hex literal too big`
  (expected -1).
- `literal-1.5.1/1.5.2/1.5.3`: same overflow path in `typeof`, ALTER TABLE
  DEFAULT, and CREATE TABLE DEFAULT (expected `integer -1`).

Oracle (`/usr/bin/sqlite3`, 3.54):

- `0x8000000000000000` → `-9223372036854775808`, `typeof` integer.
- `0xFFFFFFFFFFFFFFFF` → `-1`, `typeof` integer.
- Leading zeros beyond 16 total digits are allowed; the significant digits
  decide the width.
- A signed hex literal (`-0x8000000000000000`) is rejected with
  `hex literal too big`, matching current Frigolite behavior.

SQLite source `../sqlite/src/util.c:890-920` (`sqlite3DecOrHexToI64`):

- Accumulates hex digits into a `u64`, `memcpy`s the bits into the `i64`
  output, rejects more than 16 significant hex digits, and ignores leading
  zeros for the width check.

Current defect: `evalHexLiteral` in
`internal/execexpr/expression_rowvalue.go:117-135` rejects any unsigned
magnitude above `MaxInt64` instead of preserving the 64-bit pattern.

## Ordered steps

1. Add a pure-Go pin `frigolite_hexlit_t22_pin_test.go` for the five
   engine-visible contracts above (two SELECT values, `typeof`, DDL/ALTER
   defaults, signed-hex rejection, 17-significant-digit rejection).
2. Change `evalHexLiteral` to mirror `sqlite3DecOrHexToI64`: require an
   unsigned `0x`/`0X` prefix, skip leading zeros, keep at most 16 significant
   hex digits, parse into `uint64`, and reinterpret as `int64`. Preserve the
   existing `hex literal too big: <literal>` error for signed forms and
   oversized magnitudes.
3. Run focused gates `FRIGOLITE_TEST=hexlit` and `FRIGOLITE_TEST=literal`.
4. Run package tests, converter fail-set comparison, and quality NEW=0.
5. Detached full gate with `-timeout 30m`; raw-byte leaf-name diff versus
   `/tmp/t21_full.log` and `/tmp/t10_full2.log`. Target leaves GONE, NEW=0.
6. Commit code, then docs/handoff/lessons/TODO.

## Verification

- Pure-Go pin fails before the fix and passes after it.
- Focused family gates zero target leaves.
- Full gate loses exactly the five target leaves and gains none (documented
  flakes allowed only with focused confirmation).
- Quality/converter fail sets NEW=0 versus committed HEAD.

## Execution notes

- Pure-Go pin failed at `0x8000000000000000` before the fix.
- `evalHexLiteral` now mirrors `sqlite3DecOrHexToI64`: 16 significant hex
  digits max, unsigned bit pattern reinterpreted as int64, sign applied
  afterward, with MinInt64 negation still reported as an oversized literal.
- Focused `hexlit` gate is green; focused `literal` lost all three target
  leaves and retains three unrelated lexer-error-text leaves (4.2/4.13/4.14).
- FULL gate `/tmp/t22_full.log` (1256 leaves): NEW=0 vs t21 and durable t10;
  the five target leaves GONE. Documented `vacuum-into-500` and
  `resetdb-300` flakes reproduce in focused runs.
- Code committed as `1851faaa0`.
