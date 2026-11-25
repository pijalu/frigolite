# Session handoff — S55 slice 2: e_fts3 fixture regen (2026-11-25, d96724729)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Fixture committed at `d96724729`; this docs commit records
the verified state and the next unit.

## Landed

- `e_fts3.json` regenerated with current `do_write_test`/`do_error_test`
  wrapper support. Old fixture had 25 tests and omitted the DDL/write steps
  that create `docs`, `t4`, and `ta`; new fixture has 432 tests.
- Focused `e_fts3` gate 0 leaves; full gate `/tmp/t20_full.log` (1268 leaves)
  NEW=0 vs durable t10 apart from the documented `atof1-2.40` dice flake,
  8 `e_fts3` leaves GONE. `resetdb-300` also flaked but is absent in the
  focused re-run. Converter failure-name set and quality findings unchanged.

## Restart point

Next unit: final two FTS3 residue leaves:

- `fts3auto/setup_2`
- `fts3defer3/fts3defer3-1.7`

Triage the first failing step before editing. Both may be fixture/converter
gaps, but confirm with a pure-Go pin if an engine-visible contract fails.

## Risks / working rules

- A stale JSON fixture can hide wrapper-generated DDL; regeneration can
  change test count dramatically and still be the correct fix.
- No hand-editing JSON; regenerate one family with `-only`.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps.
- `atof1-2.40` and `resetdb-300` are documented flakes; confirm with focused
  runs before counting them as regressions.
