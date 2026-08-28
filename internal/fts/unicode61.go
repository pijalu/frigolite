package fts

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// This file ports the SQLite unicode61 tokenizer (ext/fts3/fts3_unicode.c +
// fts3_unicode2.c) faithfully. It implements:
//
//   - sqlite3FtsUnicodeIsalnum: whether a codepoint is alphanumeric, using the
//     packed range table extracted from fts3_unicode2.c.
//   - sqlite3FtsUnicodeIsdiacritic: whether a codepoint is a combining
//     diacritical mark (U+0300..U+0331).
//   - remove_diacritic: maps a lower-case letter with a diacritic to its ASCII
//     base letter (fts3_unicode2.c).
//   - sqlite3FtsUnicodeFold: folds a codepoint to lower case, optionally
//     removing diacritics (eRemoveDiacritic 0/1/2).
//
// The Unicode61Tokenizer applies these together with per-table exceptions
// (tokenchars=/separators=), mirroring unicode_tokenizer in fts3_unicode.c.

// unicode61IsAlnum reports whether codepoint c is alphanumeric for the
// unicode61 tokenizer (sqlite3FtsUnicodeIsalnum, fts3_unicode2.c).
func unicode61IsAlnum(c int) bool {
	if c < 128 {
		return (unicodeIsalnumAscii[c>>5] & (1 << uint(c&0x1F))) == 0
	}
	if c >= 1<<22 {
		return true
	}
	key := (uint32(c) << 10) | 0x000003FF
	iLo, iHi := 0, len(unicodeIsalnumEntry)-1
	iRes := 0
	for iHi >= iLo {
		iTest := (iHi + iLo) / 2
		if key >= unicodeIsalnumEntry[iTest] {
			iRes = iTest
			iLo = iTest + 1
		} else {
			iHi = iTest - 1
		}
	}
	return uint32(c) >= (unicodeIsalnumEntry[iRes]>>10)+(unicodeIsalnumEntry[iRes]&0x3FF)
}

// unicode61IsDiacritic reports whether codepoint c is a combining diacritical
// mark (sqlite3FtsUnicodeIsdiacritic, fts3_unicode2.c).
func unicode61IsDiacritic(c int) bool {
	const mask0 = 0x08029FDF
	const mask1 = 0x000361F8
	if c < 768 || c > 817 {
		return false
	}
	if c < 768+32 {
		return (mask0 & (1 << uint(c-768))) != 0
	}
	return (mask1 & (1 << uint(c-768-32))) != 0
}

// unicode61RemoveDiacritic returns the ASCII base letter of a lower-case
// letter c that carries a diacritic (remove_diacritic, fts3_unicode2.c).
// bComplex selects remove_diacritics=2 semantics: complex (non-ASCII) folds
// are also applied.
func unicode61RemoveDiacritic(c int, bComplex bool) int {
	key := (uint32(c) << 3) | 0x00000007
	iLo, iHi := 0, len(unicodeDia)-1
	iRes := 0
	for iHi >= iLo {
		iTest := (iHi + iLo) / 2
		if key >= uint32(unicodeDia[iTest]) {
			iRes = iTest
			iLo = iTest + 1
		} else {
			iHi = iTest - 1
		}
	}
	base := unicodeDia[iRes]
	ch := unicodeDiaChar[iRes]
	if !bComplex && (ch&0x80) != 0 {
		return c
	}
	if c > int(base>>3)+int(base&0x07) {
		return c
	}
	return int(ch & 0x7F)
}

// unicode61Fold folds codepoint c to its lower-case equivalent, optionally
// removing diacritics (sqlite3FtsUnicodeFold, fts3_unicode2.c).
func unicode61Fold(c, eRemoveDiacritic int) int {
	ret := c
	if c < 128 {
		if c >= 'A' && c <= 'Z' {
			ret = c + ('a' - 'A')
		}
	} else if c < 65536 {
		iLo, iHi := 0, len(unicodeFoldEntry)-1
		iRes := -1
		for iHi >= iLo {
			iTest := (iHi + iLo) / 2
			if c-int(unicodeFoldEntry[iTest].iCode) >= 0 {
				iRes = iTest
				iLo = iTest + 1
			} else {
				iHi = iTest - 1
			}
		}
		if iRes >= 0 {
			p := unicodeFoldEntry[iRes]
			if c < int(p.iCode)+int(p.nRange) && (0x01&int(p.flags)&(int(p.iCode)^c)) == 0 {
				ret = (c + int(unicodeFoldOff[p.flags>>1])) & 0x0000FFFF
			}
		}
		if eRemoveDiacritic != 0 {
			ret = unicode61RemoveDiacritic(ret, eRemoveDiacritic == 2)
		}
	} else if c >= 66560 && c < 66600 {
		ret = c + 40
	}
	return ret
}

// Unicode61Tokenizer is a Unicode-aware tokenizer matching SQLite's unicode61
// (ext/fts3/fts3_unicode.c unicode_tokenizer). Tokens are sequences of
// alphanumeric characters (with per-table exceptions); combining diacritical
// marks continue a token; each character is case-folded and optionally
// diacritic-stripped.
type Unicode61Tokenizer struct {
	// eRemoveDiacritic: 0 = keep diacritics, 1 = remove Latin (simple),
	// 2 = remove all (complex).
	eRemoveDiacritic int
	// exceptions holds codepoints whose isalnum result is inverted
	// (tokenchars= and separators=), sorted ascending.
	exceptions []int
}

// newUnicode61Tokenizer creates a unicode61 tokenizer from constructor args
// (unicodeCreate, fts3_unicode.c): remove_diacritics=0|1|2, tokenchars=...,
// separators=.... Any unrecognized argument is an error.
func newUnicode61Tokenizer(args []string) (*Unicode61Tokenizer, error) {
	t := &Unicode61Tokenizer{eRemoveDiacritic: 1}
	for _, arg := range args {
		switch {
		case arg == "remove_diacritics=1":
			t.eRemoveDiacritic = 1
		case arg == "remove_diacritics=0":
			t.eRemoveDiacritic = 0
		case arg == "remove_diacritics=2":
			t.eRemoveDiacritic = 2
		case strings.HasPrefix(arg, "tokenchars="):
			if err := t.addExceptions(arg[len("tokenchars="):], true); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "separators="):
			if err := t.addExceptions(arg[len("separators="):], false); err != nil {
				return nil, err
			}
		default:
			return nil, errUnknownTokenizer
		}
	}
	return t, nil
}

// addExceptions adds the codepoints of s to the exceptions array, inverting
// their isalnum result to bAlnum (unicodeAddExceptions, fts3_unicode.c).
// Diacritical marks cannot be made exceptions and are ignored.
func (t *Unicode61Tokenizer) addExceptions(s string, bAlnum bool) error {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		cc := int(r)
		if unicode61IsAlnum(cc) != bAlnum && !unicode61IsDiacritic(cc) {
			t.exceptions = append(t.exceptions, cc)
		}
		i += size
	}
	sort.Ints(t.exceptions)
	return nil
}

// isAlnum reports whether codepoint c is a token character for this tokenizer
// (unicodeIsAlnum, fts3_unicode.c): isalnum XOR exception.
func (t *Unicode61Tokenizer) isAlnum(c int) bool {
	alnum := unicode61IsAlnum(c)
	if len(t.exceptions) > 0 {
		i := sort.SearchInts(t.exceptions, c)
		if i < len(t.exceptions) && t.exceptions[i] == c {
			alnum = !alnum
		}
	}
	return alnum
}

// fold folds codepoint c for this tokenizer (diacritic mode from options).
func (t *Unicode61Tokenizer) fold(c int) int {
	return unicode61Fold(c, t.eRemoveDiacritic)
}

// decodeTokenRune decodes one UTF-8 rune, treating an invalid byte as the raw
// byte value (SQLite's READ_UTF8 macro reads the byte as the codepoint when
// the sequence is invalid, fts3_unicode.c unicodeNext). This matters for
// bytes like 0xD6 (lone continuation byte): unicode61 must tokenize them as
// the Latin-1 character Ö, not skip them.
func decodeTokenRune(s string) (rune, int) {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return rune(s[0]), 1
	}
	return r, size
}

// Tokenize tokenizes text into tokens (unicodeNext, fts3_unicode.c): scan past
// separators, then consume alnum chars (and diacritics), folding each char.
func (t *Unicode61Tokenizer) Tokenize(text string) []Token {
	var tokens []Token
	pos := 0
	i := 0
	n := len(text)
	for i < n {
		// Skip separators.
		for i < n {
			r, size := decodeTokenRune(text[i:])
			if t.isAlnum(int(r)) {
				break
			}
			i += size
		}
		if i >= n {
			break
		}
		// Consume token chars (alnum or diacritic continuation).
		var sb strings.Builder
		for i < n {
			r, size := decodeTokenRune(text[i:])
			cc := int(r)
			if t.isAlnum(cc) || unicode61IsDiacritic(cc) {
				out := t.fold(cc)
				if out != 0 {
					sb.WriteRune(rune(out))
				}
				i += size
			} else {
				break
			}
		}
		tokens = append(tokens, Token{Term: sb.String(), Position: pos})
		pos++
	}
	return tokens
}

// TokenizeOffsets tokenizes text with byte spans (used by offsets()/snippet()
// and the fts3tokenize virtual table). The term is folded exactly as
// Tokenize produces it.
func (t *Unicode61Tokenizer) TokenizeOffsets(text string) []OffsetToken {
	var tokens []OffsetToken
	i := 0
	n := len(text)
	for i < n {
		for i < n {
			r, size := decodeTokenRune(text[i:])
			if t.isAlnum(int(r)) {
				break
			}
			i += size
		}
		if i >= n {
			break
		}
		start := i
		var sb strings.Builder
		for i < n {
			r, size := decodeTokenRune(text[i:])
			cc := int(r)
			if t.isAlnum(cc) || unicode61IsDiacritic(cc) {
				out := t.fold(cc)
				if out != 0 {
					sb.WriteRune(rune(out))
				}
				i += size
			} else {
				break
			}
		}
		tokens = append(tokens, OffsetToken{Term: sb.String(), Start: start, End: i})
	}
	return tokens
}

// errUnknownTokenizer mirrors SQLite's "unknown tokenizer" error.
var errUnknownTokenizer = errString("unknown tokenizer")

// errString is a simple string error.
type errString string

func (e errString) Error() string { return string(e) }
