// SPDX-License-Identifier: GPL-3.0-or-later
package tclparser

// Token codes for the go-lemon parser.
// These are symIndex values (1-based terminal codes used in the parse tables),
// NOT the TK_ constants from the generated grammar (which are alphabetically sorted).
const (
	tokSEPARATOR  = 1
	tokBRACE_WORD = 2
	tokQUOTE_WORD = 3
	tokBARE_WORD  = 4
)

// tclLexer tokenizes TCL source text into tokens for the go-lemon parser.
// It mirrors the hand-written ParseCommands() logic.
type tclLexer struct {
	src     string
	pos     int
	atStart bool // true at beginning of a command (after separator or at input start)
	done    bool // true when EOF has been returned
}

// next returns the next token (type code and semantic value).
// Token type 0 means EOF/YYNOCODE.
func (l *tclLexer) next() (int, interface{}) {
	for l.pos < len(l.src) {
		ch := l.src[l.pos]

		// Backslash continuation: \<newline> → skip both
		if ch == '\\' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\n' {
			l.pos += 2
			continue
		}

		// Whitespace: space, tab, carriage return
		if ch == ' ' || ch == '\t' || ch == '\r' {
			l.pos++
			continue
		}

		// Comment: # at start of command → skip to end of line
		if ch == '#' && l.atStart {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}

		// Separator: newline or semicolon
		if ch == '\n' || ch == ';' {
			l.pos++
			if l.atStart {
				// Consecutive separators at command start → skip (no empty commands)
				continue
			}
			l.atStart = true
			return tokSEPARATOR, nil
		}

		// --- Start of a word ---
		l.atStart = false

		switch ch {
		case '{':
			return l.readBraceWord()
		case '"':
			return l.readQuoteWord()
		default:
			return l.readBareWord()
		}
	}

	// End of input
	if l.done {
		return 0, nil // YYNOCODE
	}
	l.done = true
	return 0, nil // EOF
}

// readBraceWord reads a { ... } braced word with nesting.
func (l *tclLexer) readBraceWord() (int, interface{}) {
	depth := 1
	start := l.pos + 1
	l.pos++
	for l.pos < len(l.src) && depth > 0 {
		if l.src[l.pos] == '\\' {
			l.pos += 2
			continue
		}
		if l.src[l.pos] == '{' {
			depth++
		} else if l.src[l.pos] == '}' {
			depth--
		}
		if depth > 0 {
			l.pos++
		}
	}
	word := l.src[start:l.pos]
	if l.pos < len(l.src) {
		l.pos++ // skip closing }
	}
	return tokBRACE_WORD, RawWord{Text: word, Braced: true}
}

// readQuoteWord reads a " ... " quoted word.
func (l *tclLexer) readQuoteWord() (int, interface{}) {
	start := l.pos + 1
	l.pos++
	for l.pos < len(l.src) && l.src[l.pos] != '"' {
		if l.src[l.pos] == '\\' {
			l.pos += 2
			continue
		}
		l.pos++
	}
	word := l.src[start:l.pos]
	if l.pos < len(l.src) {
		l.pos++ // skip closing "
	}
	return tokQUOTE_WORD, RawWord{Text: word, Quoted: true}
}

// readBareWord reads a bare (unquoted, unbraced) word.
// It tracks [bracket] depth so command substitution brackets are not split.
func (l *tclLexer) readBareWord() (int, interface{}) {
	var wordBuf []byte
	bracketDepth := 0
	braceDepth := 0
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\\' {
			// Backslash escape: include both backslash and next char as
			// literal word text (matches hand-written parser). Note: unlike
			// the top-level lexer loop, a backslash-newline inside a word is
			// preserved, not treated as a line continuation.
			wordBuf = append(wordBuf, c)
			l.pos++
			if l.pos < len(l.src) {
				wordBuf = append(wordBuf, l.src[l.pos])
				l.pos++
			}
			continue
		}
		if c == '[' {
			bracketDepth++
		} else if c == ']' {
			if bracketDepth > 0 {
				bracketDepth--
			}
		} else if c == '{' {
			if bracketDepth == 0 {
				break // a brace starts after whitespace
			}
			braceDepth++
		} else if c == '}' {
			if braceDepth > 0 {
				braceDepth--
			}
		} else if (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';') && bracketDepth == 0 {
			break
		}
		wordBuf = append(wordBuf, c)
		l.pos++
	}
	return tokBARE_WORD, RawWord{Text: string(wordBuf)}
}
