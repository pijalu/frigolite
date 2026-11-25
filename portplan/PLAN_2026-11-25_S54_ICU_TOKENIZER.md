# Plan — S54 slice 1: ICU tokenizer for FTS3/4 (2026-11-25)

## Problem

`fts3defer-5.2.1` fails at `CREATE VIRTUAL TABLE x3 USING FTS4(a, b,
TOKENIZE icu en_US)` with `unknown tokenizer: icu`; 5.2.2/5.2.3 then fail
only because the table does not exist. SQLite's ICU tokenizer is registered
in `ext/fts3/fts3_icu.c`; Frigolite's factory (`internal/fts/tokenizer.go`)
only knows simple/unicode61/porter.

## SQLite contract (fts3_icu.c)

- `icuCreate` stores `argv[0]` as the locale (extra args ignored).
- `icuOpen` decodes UTF-8, applies `u_foldCase` to each codepoint, and
  builds an offset map back to the original UTF-8 bytes.
- `icuNext` iterates ICU `UBRK_WORD` boundaries, skips leading whitespace
  runs inside a boundary, and emits the folded boundary text with original
  byte offsets and sequential positions. Mid-word apostrophes stay in the
  token (`fts3atoken-3.3`: `I don't see how` → `i`, `don't`, `see`, `how`).

## Exact changes

1. New `internal/fts/icu.go`
   - `ICUTokenizer` stores the locale from the tokenizer spec.
   - `TokenizeOffsets`: decode runes, fold case, map folded-rune indices to
     original byte offsets, split at UBRK_WORD-equivalent boundaries
     (letters/numbers/marks/connector punctuation and mid-word apostrophe
     stay together; other runes separate), skip whitespace-only boundaries,
     emit folded terms with original byte spans.
   - `Tokenize`: reuse `TokenizeOffsets`, assigning sequential positions.
2. `internal/fts/tokenizer.go`
   - Add `icu` to `NewTokenizer` and `NewTokenizerFromSpec`; pass spec args
     to the ICU constructor so `en_US` is available.
3. New `frigolite_icu_tokenizer_t15_pin_test.go`
   - Pin `TOKENIZE icu en_US` table creation, `MATCH 'a b'` row set after
     large `b` inserts, and the `don't` / case-folding token text.

## Verification

- `go test -run '^TestICUTokenizer' -count=1 .`
- `FRIGOLITE_TEST=fts3defer FRIGOLITE_RUN_SLOW=1 go test -timeout 120s ...`
  raw-byte diff vs `/tmp/t14_full2.log`: NEW=0, GONE=3 (`5.2.1/2/3`).
- Focused `fts3atoken` raw diff vs the full baseline: NEW=0.
- `tools/quality_gate.sh`: staticcheck/gocognit/gocyclo NEW=0 vs clean HEAD.
- Detached full gate `-timeout 30m`; raw diff vs `/tmp/t10_full2.log`
  must stay NEW=0.

## Risks / non-goals

- No CGO, ICU dependency, or external Go module. Go's Unicode tables provide
  the pure-Go word/case classification; locale is stored and used to select
  segmentation behavior where needed, but no external ICU dictionary.
- No change to simple/unicode61/porter or deferral caches.
- No harness leaf weakening or skipping.

## Execution record (committed 73b414639)

Added `internal/fts/icu.go` with `ICUTokenizer`, case folding via
`unicode.ToLower`, UAX #29-style word runs (letters, numbers, marks, connector
punctuation, mid-word apostrophes), original byte spans, and the locale from
the tokenizer spec. Registered `icu` in both factories. `fts3atoken` remains
an unsupported family at HEAD, so there is no new harness surface there.

Verification:

- `go test -run '^TestICUTokenizer' -count=1 .` — green.
- `FRIGOLITE_TEST=fts3defer FRIGOLITE_RUN_SLOW=1 go test -timeout 120s ...` —
  raw fail-set diff vs `/tmp/t14_full2.log` NEW=0, GONE=4.
- `tools/quality_gate.sh` — staticcheck/gocognit/gocyclo NEW=0 vs the prior
  clean gate; pre-existing hard file-size failures unchanged.
- Detached full gate `/tmp/t15_full.log` (1154.984s, 1307 leaves): raw-byte
  diff vs durable `/tmp/t10_full2.log` NEW=0, GONE=44; vs `/tmp/t14_full2.log`
  NEW=0, GONE=4.
