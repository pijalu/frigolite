# Plan — S53 slice 2: FTS4 deferred matchinfo stats (2026-11-24)

Goal: close the `fts3defer2` matchinfo value mismatches that appeared once the
`mit` Tcl UDF could render the blob. Baseline: `/tmp/t12_full.log` (1331
failing leaves); focused `/tmp/t12_fts3defer2_scan.log`.

## Findings

`hex(matchinfo(t2,'pcxnal'))` from a direct Go probe showed the blob layout
was correct (`p c x... n a l`); n=54, a=372 and l=8 already agreed with C.
Only the `'x'` global-hits triples for common tokens were wrong: phrase `a`
was `1 10002 3` where C reports `1 54 54` (nDoc), and `z` in `g z` showed the
same class. C's `sqlite3Fts3EvalPhraseStats` (fts3.c:5960) sets X=Y=nDoc for
an all-deferred phrase whose parent is not NEAR, and for a mixed phrase uses
the non-deferred tokens' potential occurrences. `fts3EvalSelectDeferred`
(fts3.c:5053) only defers inside an AND/NEAR cluster with multiple tokens
and an overflow-sized token doclist.

## Changes

- `internal/fts/fts3_tail3.go`: classify phrase tokens as deferred using an
  estimated packed-doclist size (>4 KiB), only for FTS4 tables without an
  external content table and only under an `AndNode` gate.
  - all tokens deferred → per-column global occurrence/row stats = nDoc;
  - some tokens deferred → rebuild the phrase from the loaded tokens and
    count those across all matching docs.
- `frigolite_fts3defer2_t13_pin_test.go`: direct Go pins for both contracts.

## Verification

- `TestFTS3DeferredMatchinfoPins` green; no new gocognit/gocyclo findings.
- Focused `FRIGOLITE_TEST=fts3defer2` 20→14 leaves; no NEW leaves in
  `fts3matchinfo`, `fts3matchinfo2`, `fts3first`, `fts3query`, `fts3auto`,
  `fts3cov`, `fts3malloc`, `fts4umlaut` or `fts3expr`.
- Full gate `/tmp/t13_full.log` (1172.342s, 1326 leaves): raw-byte diff vs
  durable `/tmp/t10_full2.log` = NEW=0, GONE=25; vs `/tmp/t12_full.log`
  GONE=6 (the fts3defer2 matchinfo leaves), NEW=1 documented
  `temptable2-4.1.1` unseeded-randomblob flake (already failing in t10).
