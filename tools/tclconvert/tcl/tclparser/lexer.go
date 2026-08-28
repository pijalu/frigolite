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

		// Skip ignorable characters (continuation, whitespace, comments).
		if l.skipIgnorable(ch) {
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

// skipIgnorable consumes continuation lines, whitespace, and comments,
// reporting whether the caller should continue scanning.
func (l *tclLexer) skipIgnorable(ch byte) bool {
	// Backslash continuation: \<newline> → skip both
	if ch == '\\' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\n' {
		l.pos += 2
		return true
	}

	// Whitespace: space, tab, carriage return
	if ch == ' ' || ch == '\t' || ch == '\r' {
		l.pos++
		return true
	}

	// Comment: # at start of command → skip to end of line
	if ch == '#' && l.atStart {
		l.pos = lexerSkipToLineEnd(l.src, l.pos)
		return true
	}

	return false
}

// lexerSkipToLineEnd advances pos past the rest of the current line.
func lexerSkipToLineEnd(src string, pos int) int {
	for pos < len(src) && src[pos] != '\n' {
		pos++
	}
	return pos
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
		stop, nb, nbr, buf := bareWordStep(l.src, l.pos, wordBuf, bracketDepth, braceDepth)
		wordBuf = buf
		if stop {
			break
		}
		bracketDepth, braceDepth = nb, nbr
		l.pos++
	}
	return tokBARE_WORD, RawWord{Text: string(wordBuf)}
}

// bareWordStep classifies the character at src[pos] inside a bare word,
// reporting whether it terminates the word, updated bracket/brace depths, and
// the appended word buffer. It mirrors the hand-written parser's rules.
func bareWordStep(src string, pos int, wordBuf []byte, bracketDepth, braceDepth int) (bool, int, int, []byte) {
	c := src[pos]
	switch {
	case c == '[':
		return false, bracketDepth + 1, braceDepth, append(wordBuf, c)
	case c == ']':
		return closeBracket(wordBuf, bracketDepth, braceDepth)
	case c == '{':
		return openBrace(src, pos, wordBuf, bracketDepth, braceDepth)
	case c == '}':
		return closeBrace(wordBuf, bracketDepth, braceDepth)
	case (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';') && bracketDepth == 0:
		return true, bracketDepth, braceDepth, wordBuf
	}
	return false, bracketDepth, braceDepth, append(wordBuf, c)
}

// closeBracket handles a ] inside a bare word.
func closeBracket(wordBuf []byte, bracketDepth, braceDepth int) (bool, int, int, []byte) {
	if bracketDepth > 0 {
		return false, bracketDepth - 1, braceDepth, append(wordBuf, ']')
	}
	return false, bracketDepth, braceDepth, append(wordBuf, ']')
}

// openBrace handles a { inside a bare word.
func openBrace(src string, pos int, wordBuf []byte, bracketDepth, braceDepth int) (bool, int, int, []byte) {
	// A { immediately following $ is a ${varname} variable
	// reference (TCL braces delimit the variable name), not a
	// braced-word boundary — consume the whole ${...} in the word.
	if bracketDepth == 0 && !(len(wordBuf) > 0 && wordBuf[len(wordBuf)-1] == '$') {
		return true, bracketDepth, braceDepth, wordBuf // a brace starts after whitespace
	}
	return false, bracketDepth, braceDepth + 1, append(wordBuf, '{')
}

// closeBrace handles a } inside a bare word.
func closeBrace(wordBuf []byte, bracketDepth, braceDepth int) (bool, int, int, []byte) {
	if braceDepth > 0 {
		return false, bracketDepth, braceDepth - 1, append(wordBuf, '}')
	}
	return false, bracketDepth, braceDepth, append(wordBuf, '}')
}
