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
			if len(current) > 0 {
				commands = append(commands, current)
				current = nil
			}
			atStartOfCommand = true
			pos++
			continue
		}

		// Line continuation: backslash + newline → skip both
		if ch == '\\' && pos+1 < len(src) && src[pos+1] == '\n' {
			pos += 2
			continue
		}

		// Comment: # at start of command → skip to end of line
		if ch == '#' && atStartOfCommand {
			for pos < len(src) && src[pos] != '\n' {
				pos++
			}
			continue
		}

		// Whitespace: separates words
		if ch == ' ' || ch == '\t' || ch == '\r' {
			atStartOfCommand = false
			pos++
			continue
		}

		// We're at a non-whitespace character → start of a word
		atStartOfCommand = false

		switch ch {
		case '{':
			// Brace word — read until matching }
			depth := 1
			start := pos + 1
			pos++
			for pos < len(src) && depth > 0 {
				if src[pos] == '\\' {
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
			// pos now at closing } (or EOF)
			word := src[start:pos]
			if pos < len(src) {
				pos++ // skip closing }
			}
			current = append(current, rawWord{Text: word, Braced: true})

		case '"':
			// Quote word — read until matching "
			start := pos + 1
			pos++
			for pos < len(src) && src[pos] != '"' {
				if src[pos] == '\\' {
					pos += 2
					continue
				}
				pos++
			}
			word := src[start:pos]
			if pos < len(src) {
				pos++ // skip closing "
			}
			current = append(current, rawWord{Text: word, Quoted: true})

		default:
			// Plain word — read until whitespace, newline, or semicolon.
			// Track [ ] depth so we don't split inside command substitution.
			start := pos
			bracketDepth := 0
			braceDepth := 0
			for pos < len(src) {
				c := src[pos]
				if c == '\\' {
					pos += 2
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
				pos++
			}
			word := src[start:pos]
			current = append(current, rawWord{Text: word})
		}
	}

	// Flush last command
	if len(current) > 0 {
		commands = append(commands, current)
	}

	return commands
}
