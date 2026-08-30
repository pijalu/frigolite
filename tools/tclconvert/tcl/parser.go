// SPDX-License-Identifier: GPL-3.0-or-later
// Package tcl implements a minimal TCL interpreter.
// This file implements the TCL tokenizer/parser that splits source
// text into commands (lists of RawWord).
package tcl

// RawWord represents one word in a TCL command before substitution.
type RawWord struct {
	Text   string // raw content (for braced words, literal; for others, may contain $var or [cmd])
	Braced bool   // true if word was { ... } quoted (literal, no substitution)
	Quoted bool   // true if word was " ... " quoted (substitution applies)
}

// rawWord is an alias for backward compatibility within the package.
type rawWord = RawWord

// parseCommands splits TCL source text into commands (internal, kept for backward compat).
func parseCommands(src string) [][]rawWord {
	return ParseCommands(src)
}

// ParseCommands splits TCL source text into commands.
// Each command is a slice of RawWord.
// Commands are separated by newlines or semicolons (outside braces/brackets).
// Lines starting with # are comments (skipped).
func ParseCommands(src string) [][]RawWord {
	var commands [][]rawWord
	var current []rawWord
	pos := 0
	atStartOfCommand := true // true when we're at the beginning of a new command

	for pos < len(src) {
		ch := src[pos]

		// Newline or semicolon = command separator (outside braces)
		if ch == '\n' || ch == ';' {
			commands = pushCommand(commands, current)
			current = nil
			atStartOfCommand = true
			pos++
			continue
		}

		// Line continuation: backslash + newline → skip both
		if isLineContinuation(src, pos) {
			pos += 2
			continue
		}

		// Comment: # at start of command → skip to end of line
		if ch == '#' && atStartOfCommand {
			pos = skipToLineEnd(src, pos)
			continue
		}

		// Whitespace: separates words (but not at start of command — keeps atStartOfCommand)
		if isWhitespace(ch) {
			pos++
			continue
		}

		// We're at a non-whitespace character → start of a word
		atStartOfCommand = false

		switch ch {
		case '{':
			word, next := readBraceWord(src, pos)
			pos = next
			current = append(current, rawWord{Text: word, Braced: true})

		case '"':
			word, next := readQuoteWord(src, pos)
			pos = next
			current = append(current, rawWord{Text: word, Quoted: true})

		default:
			word, next := readPlainWord(src, pos)
			pos = next
			current = append(current, rawWord{Text: word})
		}
	}

	// Flush last command
	commands = pushCommand(commands, current)

	return commands
}

// pushCommand appends a non-empty command to the command list.
func pushCommand(commands [][]rawWord, current []rawWord) [][]rawWord {
	if len(current) > 0 {
		commands = append(commands, current)
	}
	return commands
}

// isLineContinuation reports whether src[pos] starts a backslash-newline.
func isLineContinuation(src string, pos int) bool {
	return pos+1 < len(src) && src[pos] == '\\' && src[pos+1] == '\n'
}

// isWhitespace reports whether ch is a TCL word-separator whitespace char.
func isWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r'
}

// skipToLineEnd advances pos past the rest of the current line.
func skipToLineEnd(src string, pos int) int {
	for pos < len(src) && src[pos] != '\n' {
		pos++
	}
	return pos
}

// readBraceWord reads a { ... } braced word with nesting, returning the word
// text (without braces) and the position after the closing brace.
func readBraceWord(src string, pos int) (string, int) {
	depth := 1
	start := pos + 1
	pos++
	for pos < len(src) && depth > 0 {
		if src[pos] == '\\' {
			if pos+1 >= len(src) {
				pos++
				continue
			}
			pos += 2
			continue
		}
		if src[pos] == '{' {
			depth++
		} else if src[pos] == '}' {
			depth--
		}
		if depth > 0 {
			pos++
		}
	}
	word := src[start:pos]
	if pos < len(src) {
		pos++ // skip closing }
	}
	return word, pos
}

// readQuoteWord reads a " ... " quoted word, returning the word text and the
// position after the closing quote. Inside a quoted word, `[...]` command
// substitutions may contain their own quoted sub-words: the parser must
// respect bracket depth so the inner `" OR oid = "` of
//
//	"...WHERE oid = [join $delete " OR oid = "]"
//
// is not mistaken for the outer string's closing quote. We track both
// bracket depth (so we don't end the outer word on a `]` inside a `[`)
// and quote depth (so we don't end the outer word on a `"` inside a
// `[`); the closing quote is the `"` that matches the opening one at
// depth 0 in both counters.
func readQuoteWord(src string, pos int) (string, int) {
	start := pos + 1
	pos++
	bracketDepth := 0
	quoteDepth := 0
	for pos < len(src) {
		ch := src[pos]
		if ch == '\\' && pos+1 < len(src) {
			pos += 2
			continue
		}
		if ch == '[' {
			bracketDepth++
			pos++
			continue
		}
		if ch == ']' {
			if bracketDepth > 0 {
				bracketDepth--
			}
			pos++
			continue
		}
		if ch == '"' {
			if bracketDepth > 0 {
				// Inside a [cmd ...] substitution; the inner "..." is
				// a separate quoted word of the sub-command. Treat it
				// as a regular character so the outer string remains
				// open.
				// (Skipping over the inner quoted word keeps the
				//  readQuoteWord's job — collecting raw text — simple
				//  enough; the bracketed text itself is not
				//  re-tokenized here.)
				pos++
				innerDepth := 1
				for pos < len(src) && innerDepth > 0 {
					if src[pos] == '\\' && pos+1 < len(src) {
						pos += 2
						continue
					}
					if src[pos] == '"' {
						innerDepth--
					}
					pos++
				}
				continue
			}
			break
		}
		pos++
	}
	word := src[start:pos]
	if pos < len(src) {
		pos++ // skip closing "
	}
	_ = quoteDepth
	return word, pos
}

// readPlainWord reads an unquoted, unbraced word, tracking [ ] depth so
// command-substitution brackets are not split. A { that immediately follows
// $ is a ${varname} variable reference (TCL braces delimit the variable
// name), not a braced-word boundary — the whole ${...} stays in the word.
func readPlainWord(src string, pos int) (string, int) {
	start := pos
	bracketDepth := 0
	braceDepth := 0
	for pos < len(src) {
		c := src[pos]
		if c == '\\' {
			pos = skipWordEscape(src, pos)
			continue
		}
		stop, nb, nbr := plainWordStep(src, pos, start, bracketDepth, braceDepth)
		if stop {
			break
		}
		bracketDepth, braceDepth = nb, nbr
		pos++
	}
	return src[start:pos], pos
}

// skipWordEscape advances past a backslash escape inside a bare word.
func skipWordEscape(src string, pos int) int {
	if pos+1 >= len(src) {
		return pos + 1
	}
	return pos + 2
}

// plainWordStep classifies the character at src[pos] inside a bare word and
// reports whether it terminates the word, along with updated bracket/brace
// depths. It mirrors the hand-written parser's bare-word rules.
func plainWordStep(src string, pos, start, bracketDepth, braceDepth int) (bool, int, int) {
	c := src[pos]
	switch {
	case c == '[':
		return false, bracketDepth + 1, braceDepth
	case c == ']':
		if bracketDepth > 0 {
			return false, bracketDepth - 1, braceDepth
		}
		return false, bracketDepth, braceDepth
	case c == '{':
		if bracketDepth == 0 && (pos == start || src[pos-1] != '$') {
			return true, bracketDepth, braceDepth // a brace starts after whitespace (unless ${var})
		}
		return false, bracketDepth, braceDepth + 1
	case c == '}':
		if braceDepth > 0 {
			return false, bracketDepth, braceDepth - 1
		}
		return false, bracketDepth, braceDepth
	case isWordSep(c) && bracketDepth == 0:
		return true, bracketDepth, braceDepth
	}
	return false, bracketDepth, braceDepth
}

// isWordSep reports whether c is a bare-word separator (whitespace or ;).
func isWordSep(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';'
}
