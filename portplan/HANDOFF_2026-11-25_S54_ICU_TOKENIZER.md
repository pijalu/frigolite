# Session handoff — S54 slice 1: ICU tokenizer (2026-11-25, 73b414639)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Engine slice committed at `73b414639`; this docs commit
records the verified state and the next unit.

## Landed

- `73b414639` — pure-Go ICU tokenizer for FTS3/4. `TOKENIZE icu en_US` now
  constructs a case-folding word-boundary tokenizer with original byte spans;
  mid-word apostrophes stay inside a token. Registered in both `NewTokenizer`
  and `NewTokenizerFromSpec`.
- `frigolite_icu_tokenizer_t15_pin_test.go` pins the ICU token text/spans and
  the FTS4 `MATCH 'a b'` row set with an overflow-sized `b` doclist.
- Verification: focused `fts3defer` raw diff vs `/tmp/t14_full2.log` NEW=0,
  GONE=4 (`fts3defer` root + `5.2.1/2/3`); full gate `/tmp/t15_full.log`
  (1154.984s, 1307 leaves) raw-byte diff vs durable `/tmp/t10_full2.log`
  NEW=0, GONE=44; quality staticcheck/gocognit/gocyclo NEW=0 vs the prior
  clean gate. `fts3atoken` is skipped at HEAD, so it added no new surface.

## Restart point

Next unit: remaining fts3* residue (~37 leaves), starting with `fts3query`
(11), `fts4umlaut` (13) or `fts3malloc` (5). Current full-gate leaves:

- `fts3auto/setup_2`, `fts3cov/fts3cov-11.1`, `fts3defer3-1.7`
- `fts3malloc` 2.0, 3.0, 4.0, 4.2
- `fts3query` 5.2.1, 5.3.1, 5.4.1, 7.2.*, 7.3..2, 7.3..4
- `fts4umlaut` 1.4.3/1.4.4, 1.5.*, 1.6.*, 1.7.*

Use the SQLite oracle and a pure-Go pin before changing the engine. The
remaining leaves are baseline residues, not regressions from this slice.

## Risks / working rules

- ICU is a pure-Go approximation of `UBRK_WORD`: locale is retained, but no
  external ICU dictionary is linked. The closed leaves only require ASCII
  word breaks and case folding; do not overclaim Thai/CJK fidelity.
- Deferral caches still live on `InvertedIndex` and are invalidated on every
  index mutation; do not add per-row content rescans.
- Compare full gates by raw-byte leaf-name sets (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps. Full gates
  need `-timeout 30m` and detached execution (tool cap is 300s).
- Commit each verified unit immediately; use a worktree for HEAD baselines.
