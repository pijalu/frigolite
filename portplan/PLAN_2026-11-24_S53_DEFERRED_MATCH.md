# Plan — S53 slice 3: FTS4 deferred MATCH evaluation (2026-11-24)

## Goal

Close the 14 remaining `fts3defer2` leaves. C's `fts3EvalStart` marks
expensive tokens in an AND/NEAR cluster as deferred. During iteration a
deferred phrase is treated as matching every candidate row; before the row is
returned, `sqlite3Fts3CacheDeferredDoclists()` re-tokenizes the `%_content`
row and computes that token's real positions. Frigolite currently reads only
in-memory postings, so zeroblobbed large segment blocks leave no candidates.

## Exact changes

1. `internal/fts/query.go`
   - Add `Deferred bool`, `Tokenizer Tokenizer` to `TermNode` and `PrefixNode`.
   - Add parallel `Deferred []bool` and `Tokenizer Tokenizer` to `PhraseNode`.
   - Make term/prefix/phrase/NEAR/Column matching use the deferred-aware
     position helpers in the new file below.
2. New `internal/fts/deferred.go`
   - `tokenPositionsFromContent`: re-tokenize one stored column with the
     table tokenizer (langid-aware where applicable), honoring `notindexed`
     and `^` first-token pins.
   - `termPostingsForNode`: deferred term/prefix positions from content,
     ordinary positions from the postings map.
   - `phraseOccurrenceLastPositions`: phrase matches from per-token position
     sets so a phrase can mix deferred and loaded tokens.
   - `markDeferredTokens`: for each phrase/AND/NEAR cluster with >1 token and
     at least one overflow-sized doclist, mark the overflowing tokens.
3. `internal/fts/storage.go`, `internal/fts/matchinfo.go`, `internal/fts/aux.go`
   - Route phrase matching, matchinfo local/global counts, NEAR counts and
     offsets/snippet position extraction through the same helpers.
4. `internal/fts/fts3_tail3.go`
   - Add `FTS3Table.PrepareMatchQuery` / `MatchPhrases`, use them from
     `MatchDocIDs`, `MatchQuery` and `ftsMatchPhrases` (execddl) so the
     filter and aux functions see one prepared, deferred-marked tree.
   - Update partial/all-deferred matchinfo stats to read the marks directly;
     keep the all-deferred shortcut disabled under NEAR.
5. Tests
   - Extend `frigolite_fts3defer2_t14_pin_test.go` until it pins the t1/t2/t3
     row sets and aux values from `testdata/fts3defer2.json`.
   - Run focused `fts3defer2`, then the FTS regression families and the full
     gate. Compare raw-byte leaf-name sets only.

## Verification

- `go test -run '^TestFTS3Deferred' -count=1 .`
- `FRIGOLITE_TEST=fts3defer2 go test -run '^TestSQLiteSuite$' -v`
- FTS regression families: `fts3matchinfo`, `fts3matchinfo2`, `fts3first`,
  `fts3query`, `fts3auto`, `fts3cov`, `fts3malloc`, `fts4umlaut`, `fts3expr`.
- `go test -timeout 30m -run '^TestSQLiteSuite$' -v` detached, raw diff vs
  `/tmp/t10_full2.log` (NEW must stay 0).

## Risks / non-goals

- No change to segment serialization, segment corruption detection or
  imposter/index behavior.
- Deferred marking remains FTS4-only and disabled for `content=` tables.
- No weakening or skipping of any harness leaf.

## Execution record (committed 9b58d6b0c)

Implemented as planned with two additional correctness/performance details:

- `restoreFTSFromContent` now restores `%_content` text even when the segment
  load recorded `LoadErr`; `InvertedIndex.RestoreDocRow` creates a document and
  size info without postings, so deferral can rescan text without inventing
  index hits.
- MATCH prepares the query once per candidate row. Caching the final defer
  decision and the content-rescan size on `InvertedIndex` is required: without
  it, a 16k-row, 19-term query (`fts3defer-4.2`) re-tokenizes the table for each
  term on each row and times out. Both caches are invalidated by every index
  mutation (`insertDoc`, `Delete`, `RestoreDocRow`, `addPosting`,
  `deleteDocFromTerm`, `Restore`).
- `PhraseColumnCounts` was split into hit-column collection and count
  accumulation to keep gocognit/gocyclo at baseline.

Verification:

- `go test -run '^TestFTS3Deferred' -count=1 .` — green.
- `FRIGOLITE_TEST=fts3defer2 go test -run '^TestSQLiteSuite$' -v` — 0 leaves.
- `FRIGOLITE_TEST=fts3defer FRIGOLITE_RUN_SLOW=1 go test -timeout 120s ...` —
  4.757s; raw fail-set diff vs `/tmp/t13_full.log` NEW=0, GONE=14 (the target
  fts3defer2 leaves). Remaining fts3defer/fts3defer3 leaves are baseline leaves.
- `tools/quality_gate.sh` — staticcheck/gocognit/gocyclo NEW=0 vs clean HEAD;
  gate still fails on pre-existing hard file-size violations.
- Detached full gate `/tmp/t14_full2.log` (1171.345s, 1311 leaves): raw-byte
  diff vs durable `/tmp/t10_full2.log` NEW=0, GONE=40. The single delta vs the
  older `/tmp/head_full_t11.log`, `vacuum-into-500`, is the documented flake and
  is present in both the durable baseline and a focused re-run.
