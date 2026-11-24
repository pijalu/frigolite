# Session handoff — S53 slice 2: FTS4 deferred matchinfo (2026-11-24, fb00dedc7)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Engine slice committed at `fb00dedc7`; this docs commit
records the remaining mid-unit state.

## Landed

- `fb00dedc7` — deferred-token matchinfo stats for FTS4: overflow-sized
  tokens in an AND cluster report nDoc for an all-deferred phrase and use the
  non-deferred tokens for a mixed phrase. Pure-Go pins cover both. Full gate
  `/tmp/t13_full.log` (1172.342s, 1326 leaves): raw-byte diff vs durable
  `/tmp/t10_full2.log` = NEW=0, GONE=25; vs `/tmp/t12_full.log` GONE=6
  fts3defer2 leaves, NEW=1 documented `temptable2-4.1.1` flake (present in
  t10). No new complexity findings in touched functions.

## Restart point

Next unit: **the remaining fts3defer2 deferred MATCH evaluation**. Focused
`/tmp/t13_fts3defer2_3.log` still has 14 leaves:

- `1.2.0/1.2.1/1.2.2/1.2.3`: `MATCH 'f (e a)'` / NEAR variants return no
  rows after the large `t1_segments` blocks were zeroblobbed. C's deferred
  tokens assume the large term matches and rescan the content row for its
  positions; our query tree only reads index postings, so the corrupted
  segment leaves it with no candidates.
- `2.2.1.3/2.2.2.3`: `g OR (g z)` local hits for the second `g` are 0 where
  C reports 1 — the deferred `z` makes the AND arm's gate pass in the OR
  branch that only contains `g`.
- `2.2.3.1-4`: after the `t2_segments` corruption, MATCH returns no rows for
  `a b`, `g z`, `g OR (g z)`, `e "g z"` even though C still finds the rows
  via deferred evaluation.
- `2.4.1-3`: the same deferred/content-rescan class on `t3` for `"a b c"`.

The in-memory index carries the original documents (`InvertedIndex.docs`),
so a faithful fix can recompute deferred-token positions from document text
with the table's tokenizer instead of relying on the corrupted postings.
C's entry points are `fts3EvalTestDeferred` / `fts3EvalDeferredDoclist`
(fts3.c:5660+) and `fts3EvalSelectDeferred` (fts3.c:5053).

## Risks / working rules

- Do not re-apply nDoc stats under a `NearNode` gate; NEAR uses gathered,
  distance-trimmed stats and is already correct where segments are intact.
- Keep the deferral proxy tied to AND clusters, FTS4 and no external content
  table; `content=` tables disable deferral in C.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Full gates
  need `-timeout 30m` and detached execution (tool cap is 300s).
