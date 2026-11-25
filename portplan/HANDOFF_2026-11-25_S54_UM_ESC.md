# Session handoff — S54 slice 2: fts4umlaut Unicode escapes (2026-11-25, 8b669c9cb)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Converter/fixture slice committed at `8b669c9cb`; this
docs commit records the verified state and the next unit.

## Landed

- `8b669c9cb` — `tclconvert.tcl.unescapeListElement` now uses the same
  backslash decoder as double-quoted word substitution, so `\uXXXX` in a
  quoted foreach-list element becomes a real Unicode character. Regenerated
  `testdata/fts4umlaut.json`; added a converter unit test and a pure-Go port
  of the FTS4 umlaut contract. The engine was already correct.
- Verification: focused `fts4umlaut` 0 leaves (raw diff vs `/tmp/t15_full.log`
  NEW=0, GONE=15); full gate `/tmp/t16_full.log` (1183.085s, 1291 leaves)
  raw-byte diff vs durable `/tmp/t10_full2.log` NEW=0, GONE=60; converter
  failure-name set unchanged vs clean HEAD (8 pre-existing failures); quality
  staticcheck/gocognit/gocyclo NEW=0.

## Restart point

Next unit: remaining fts3* residue (~22 leaves), starting with `fts3query`
(11), `fts3malloc` (5), or `fts3defer3-1.7`. Current full-gate leaves:

- `fts3auto/setup_2`, `fts3cov/fts3cov-11.1`, `fts3defer3-1.7`
- `fts3malloc` 2.0, 3.0, 4.0, 4.2
- `fts3query` 5.2.1, 5.3.1, 5.4.1, 7.2, 7.2.1.1.1, 7.2.1.2.1,
  7.2.6.1.1, 7.2.6.2.1, 7.3..2, 7.3..4

Use the SQLite oracle and a pure-Go pin before changing the engine. Several
fts3query leaves are result-shape/count problems; triage by first failing
step message before assuming an engine defect.

## Risks / working rules

- Prefer a pure-Go port when a harness leaf fails: if it passes, fix the
  converter/fixture class (`fts4umlaut` was a literal `uXXXX` artifact).
- Regenerate fixtures with `-only <family>`; do not hand-edit JSON.
- Converter full package has 8 baseline failures; compare raw failure-name
  sets, not counts, and require newly added unit tests to pass.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Full gates
  need `-timeout 30m` and detached execution (tool cap is 300s).
