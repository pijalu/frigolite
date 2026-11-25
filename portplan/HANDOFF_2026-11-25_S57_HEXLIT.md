# Session handoff — S57: unsigned hex literal wrap (2026-11-25, 1851faaa0)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code committed at `1851faaa0`; this docs commit records
the verified state and the next unit.

## Landed

- `hexlit-163/164` and `literal-1.5.1/1.5.2/1.5.3` are GONE.
- `evalHexLiteral` now mirrors `sqlite3DecOrHexToI64` (`util.c:890-920`):
  up to 16 significant hex digits accumulate into a `u64`, the bit pattern
  is reinterpreted as `int64`, and a leading sign is applied afterward.
  Negating MinInt64 still reports `hex literal too big`, matching SQLite's
  `-0x8000000000000000` parser error.
- Pure-Go pin covers SELECT, typeof, ALTER/CREATE defaults, leading zeros,
  signed wraps, and the 17-significant-digit rejection.

## Verification

- Focused `hexlit` gate 0 leaves; focused `literal` retains only three
  unrelated lexer-error-text leaves (4.2/4.13/4.14) — the three 1.5.* leaves
  are GONE.
- Full gate `/tmp/t22_full.log` (1256 leaves): NEW=0 vs `/tmp/t21_full.log`
  and durable `/tmp/t10_full2.log`; exactly the five target leaves GONE.
- Quality `/tmp/t22_quality.log`: normalized findings NEW=0 vs t20
  (overall exit 1 remains the pre-existing file-size gate).
- Documented flakes `vacuum-into-500` and `resetdb-300` reproduce in focused
  runs; `temptable2-4.1.1` and `atof1-2.40` are absent in this full gate.

## Restart point / next unit

Next unit: remaining literal-4.x lexer error text (`1_.4`, `12._34`) —
the engine accepts or reports a truncated token where SQLite includes the
trailing dot/digits. After that: printf `%j/%J` JSON quoting, quote's
double-quoted identifier rejection, and unhex. Read TODO.md first; use raw
leaf-name fail-set diffs and a pure-Go pin before converter/engine edits.

## Risks / working rules

- Unsigned hex wraps before the sign: `-0xFFFFFFFFFFFFFFFF` is 1 in SQLite,
  but `-0x8000000000000000` is a parser error. Preserve both shapes.
- Leading zeros do not count toward the 16-digit hex width.
- Keep full gates detached with `-timeout 30m` and status files; compare raw
  fail sets, never counts or line greps.
