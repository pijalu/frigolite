package fts

import "unicode"

// ICUTokenizer implements the FTS3/4 ICU tokenizer's observable contract:
// case-fold the input, split it with word boundaries, and report folded terms
// with byte spans back into the original text (ext/fts3/fts3_icu.c icuOpen /
// icuNext). The locale from the tokenizer spec is retained for callers that
// need to distinguish dictionary-based word breaks.
type ICUTokenizer struct {
	locale string
}

// NewICUTokenizer creates an ICU tokenizer. args follow SQLite's xCreate
// convention: the first argument is the locale (icuCreate in fts3_icu.c).
func NewICUTokenizer(args []string) *ICUTokenizer {
	if len(args) == 0 {
		return &ICUTokenizer{}
	}
	return &ICUTokenizer{locale: args[0]}
}

// Locale returns the ICU locale supplied to the tokenizer constructor.
func (t *ICUTokenizer) Locale() string {
	if t == nil {
		return ""
	}
	return t.locale
}

// Tokenize returns the ICU word tokens in text, with sequential positions.
func (t *ICUTokenizer) Tokenize(text string) []Token {
	spans := t.TokenizeOffsets(text)
	tokens := make([]Token, len(spans))
	for i, span := range spans {
		tokens[i] = Token{Term: span.Term, Position: i}
	}
	return tokens
}

// TokenizeOffsets returns ICU word tokens with their original byte spans.
func (t *ICUTokenizer) TokenizeOffsets(text string) []OffsetToken {
	runes, offsets := icuFoldText(text)
	var tokens []OffsetToken
	for i := 0; i < len(runes); {
		if !icuRunRuneAt(runes, i) {
			i++
			continue
		}
		start := i
		for i < len(runes) && icuRunRuneAt(runes, i) {
			i++
		}
		tokens = append(tokens, OffsetToken{
			Term:  string(runes[start:i]),
			Start: offsets[start],
			End:   offsets[i],
		})
	}
	return tokens
}

// icuFoldText folds each input rune and records the byte offset of every
// folded rune, plus a sentinel for the end of the source.
func icuFoldText(text string) ([]rune, []int) {
	runes := make([]rune, 0, len(text))
	offsets := make([]int, 0, len(text)+1)
	for i := 0; i < len(text); {
		r, size := decodeRune(text[i:])
		offsets = append(offsets, i)
		runes = append(runes, unicode.ToLower(r))
		i += size
	}
	offsets = append(offsets, len(text))
	return runes, offsets
}

// icuRunRuneAt reports whether rune i belongs to the current ICU word run.
// Apostrophes between word runes stay inside the run (WB6/WB7: "don't").
func icuRunRuneAt(runes []rune, i int) bool {
	r := runes[i]
	if isICUWordRune(r) {
		return true
	}
	if r != '\'' && r != '\u2019' {
		return false
	}
	return i > 0 && i+1 < len(runes) &&
		isICUWordRune(runes[i-1]) && isICUWordRune(runes[i+1])
}

// isICUWordRune reports the UAX #29 word rune classes ICU keeps together:
// letters, numbers, marks and connector punctuation.
func isICUWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) ||
		unicode.IsMark(r) || unicode.Is(unicode.Pc, r)
}
