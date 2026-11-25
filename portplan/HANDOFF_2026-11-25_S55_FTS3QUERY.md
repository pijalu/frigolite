# Session handoff — S55 slice 1: fts3query converter + rowid (2026-11-25, c02ff098a)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Code committed at `c02ff098a`; this docs commit records the
verified state and the next unit.

## Landed

- Converter: `do_select_tests -errorformat` now emits catch expectations; TCL
  `expr` handles the int64-min unary-minus boundary; `fts3query.json`
  regenerated (11 leaves closed).
- Engine: template-cache substitutions refuse signed/hex `NumericLit` slots.
  The normalizer extracts only unsigned magnitudes, so substituting a positive
  value into a parser-folded `-9223372036854775808` slot erased the unary
  minus on the next same-shape INSERT. Explicit rowids near the signed bounds
  are again distinct on both ordinary and FTS tables.
- Verification: focused `fts3query` 0 leaves; full gate `/tmp/t19_full2.log`
  (1273 leaves) NEW=0 vs t18 and durable t10, 11 fts3query leaves GONE;
  converter failure-name set NEW=0; quality static/gocognit/gocyclo NEW=0.
  `atof1-2.40` remains a documented random-dice flake (focused green).

## Restart point

Next unit: remaining raw leaves in the same census region:

- `e_fts3` (setup_1/2/3/4/8, e_fts3-4.4/4.5) — 8 leaves
- `fts3auto/setup_2` — 1 leaf
- `fts3defer3/fts3defer3-1.7` — 1 leaf

Triage from the first failing step. `fts3query` is closed; do not reuse its
fixture symptoms as an engine bug without a fresh pure-Go pin.

## Risks / working rules

- Template-cache substitutions must reproduce a fresh parse of the current
  SQL text. Signed/hex numeric slots are not substitutable; force the COW /
  full-parse path instead of patching rowid handling downstream.
- When refactoring a slot collector, non-literal tuple items (NULL,
  ColumnRef) are NOT slots. Registering them panics `stashValues`; changed-
  package tests caught this before the final full gate.
- Converter runtime command heads and dispatch must change together.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps.
- `tools/tclconvert` is ignored; force-add converter files and verify
  `git show --stat` before docs.
