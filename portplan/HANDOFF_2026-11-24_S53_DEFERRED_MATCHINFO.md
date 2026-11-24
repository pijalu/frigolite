# Session handoff — S53 slice 3: FTS4 deferred MATCH (2026-11-25, 9b58d6b0c)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Engine slice committed at `9b58d6b0c`; this docs commit
records the verified state and the next unit.

## Landed

- `9b58d6b0c` — deferred MATCH evaluation for FTS4. A prepared query tree now
  carries deferral marks on term/prefix/phrase nodes and is shared by row
  filtering, matchinfo, offsets and snippet. Deferred tokens rescan stored
  `%_content` positions after segment loss; reload restores text/doc-size rows
  without adding postings. Defer decisions and content-size estimates are
  cached on `InvertedIndex` and invalidated on every index mutation.
- `frigolite_fts3defer2_t14_pin_test.go` pins the t2 `g z` / `g OR (g z)` row
  and matchinfo contract and the t3 partial-defer stats after corruption.
- Verification: focused `fts3defer2` 0 leaves; full gate `/tmp/t14_full2.log`
  (1171.345s, 1311 leaves) raw-byte diff vs durable `/tmp/t10_full2.log`
  NEW=0, GONE=40; quality staticcheck/gocognit/gocyclo NEW=0 vs clean HEAD.
  `vacuum-into-500` is a known flake, present in the durable baseline and a
  focused re-run; it only differs from the older `head_full_t11` baseline.

## Restart point

Next unit: remaining fts3* residue (~39 leaves), starting with
`fts3query`/`fts4umlaut`/`fts3malloc`, or the `fts3defer-5.2.x` leaves. Current
full-gate leaves under fts3 families:

- `fts3auto/setup_2`, `fts3cov/fts3cov-11.1`
- `fts3defer-5.2.1/2/3`, `fts3defer3-1.7`
- `fts3malloc` 2.0, 3.0, 4.0, 4.2
- `fts3query` 5.2.1, 5.3.1, 5.4.1, 7.2.*, 7.3..2, 7.3..4
- `fts4umlaut` 1.4.3/1.4.4, 1.5.*, 1.6.*, 1.7.*

Use the oracle and a pure-Go pin before changing the engine; most remaining
leaves are baseline residues, not regressions from this slice.

## Risks / working rules

- Deferral is FTS4-only and disabled for `content=` tables; marks live on the
  prepared query tree, never in hidden index state.
- Text-only restore must not register postings or pending rows; a missing doc
  row must be restored so global matchinfo counts stay correct.
- All-deferred matchinfo shortcuts stay disabled under NEAR; NEAR local counts
  use distance-trimmed positions, and snippet uses the same participating set.
- MATCH prepares once per candidate row: any per-term estimate must be cached
  and invalidated on every index mutation, or large tables time out.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Full gates
  need `-timeout 30m` and detached execution (tool cap is 300s).
