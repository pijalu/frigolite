# Session handoff — S56: fts3auto + fts3defer3 residue (2026-11-25, 56b228df9)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code committed at `56b228df9`; this docs commit records
the verified state and the next unit.

## Landed

- `fts3auto/setup_2` and `fts3defer3/fts3defer3-1.7` are GONE.
- Pure-Go pins first exposed engine gaps, not just fixture gaps:
  - `Stmt.Columns()` was populated only after `Step`; SQLite fixes VDBE
    column names at prepare. Added `exec.Engine.PrepareColumnNames` plus
    a `Stmt.preparedColumns` snapshot, with hidden FTS columns dropped.
  - An all-zero segment leaf was parsed as an empty term. Zero-length first
    term is now a hard corrupt framing error, matching
    `ext/fts3/fts3_write.c:1394-1399`.
  - A failed interior child now records its boundary term as a corrupt
    doclist; deferred tokens skip that check, matching
    `ext/fts3/fts3.c:5195-5240` (`nToken<2` never defers).
- Converter support: `sqlite3_column_name` emits `stmt_column_name`, and
  `db incrblob` NUL writes emit the equivalent `UPDATE ... SET block =
  zeroblob(length(block))`. Unit pins in `tools/tclconvert/tcl/interp_t21_test.go`.
- `fts3defer3.json` regenerated (corruption step restored). Full regeneration
  of `fts3auto.json` exposed unrelated proc-arg `lindex $args` capture, so
  the durable fixture was retained and patched only for the real column list
  and the missing `COMMIT` (source `catchsql {COMMIT}` per pending case).

## Verification

- Focused gates: `fts3auto` 0 leaves, `fts3defer3` 0 leaves.
- Full gate `/tmp/t21_full.log` (1262 leaves): NEW=0 vs `/tmp/t20_full.log`
  and durable `/tmp/t10_full2.log`; GONE vs t20 exactly
  `fts3auto/setup_2`, `fts3defer3/fts3defer3-1.7`, `atof1-2.40` (dice flake),
  plus their family-level FAIL lines.
- Documented flakes `vacuum-into-500` and `resetdb-300` reproduce in focused
  runs; `temptable2-4.1.1` and `atof1-2.40` are absent in this full gate.
- Quality `/tmp/t21_quality2.log`: normalized staticcheck/gocognit/gocyclo
  NEW=0 vs `/tmp/t20_quality.log` (overall exit 1 remains the pre-existing
  hard file-size gate).
- Converter failure-name set: 8/8 identical to committed HEAD (the stale
  `/tmp/t20_tclconvert_all.log` predates HEAD's collation-lifecycle failure).
- Ignored converter subtree forced into commit; `git show --stat` verified.

## Restart point / next unit

Next unit: remaining non-FTS residue cards in TODO queue item 4 —
`binary format`/file-write substrate, the interleaved row-callback step,
then pager1 directory hierarchy / S34 journal / temptable2 / where2 watchlist
before the performance phase. Start by reading TODO.md and this handoff;
use the durable baselines and raw-byte fail-set diffs.

## Risks / working rules

- A stale JSON fixture can hide wrapper-generated steps; full regeneration
  can also introduce unrelated converter drift. Prefer a pure-Go pin, then
  the smallest fixture change that exercises the real engine contract.
- Corrupt-doclist failures must respect deferral: only undeferred terms read
  the segment doclist.
- No hand-editing JSON when a clean regeneration is available; document any
  targeted patch and why regeneration was unsafe.
- Full gates use `-timeout 30m`, detached execution, status files, and raw
  leaf-name fail-set diffs — never count or line greps.
