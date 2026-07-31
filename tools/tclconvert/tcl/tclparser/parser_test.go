// SPDX-License-Identifier: GPL-3.0-or-later
package tclparser

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseCommandsBasic tests basic TCL command parsing against expected outputs.
func TestParseCommandsBasic(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected [][]RawWord
	}{
		{
			name:     "empty input",
			input:    ``,
			expected: [][]RawWord{},
		},
		{
			name:     "single bare word",
			input:    `hello`,
			expected: [][]RawWord{{{Text: "hello"}}},
		},
		{
			name:     "two commands",
			input:    "hello\nworld",
			expected: [][]RawWord{{{Text: "hello"}}, {{Text: "world"}}},
		},
		{
			name:     "three words one command",
			input:    "a b c",
			expected: [][]RawWord{{{Text: "a"}, {Text: "b"}, {Text: "c"}}},
		},
		{
			name:     "semicolon separator",
			input:    "a;b",
			expected: [][]RawWord{{{Text: "a"}}, {{Text: "b"}}},
		},
		{
			name:     "trailing newline",
			input:    "a\n",
			expected: [][]RawWord{{{Text: "a"}}},
		},
		{
			name:     "comment",
			input:    "# comment\na",
			expected: [][]RawWord{{{Text: "a"}}},
		},
		{
			name:     "braced word",
			input:    `{hello world}`,
			expected: [][]RawWord{{{Text: "hello world", Braced: true}}},
		},
		{
			name:     "quoted word",
			input:    `"hello world"`,
			expected: [][]RawWord{{{Text: "hello world", Quoted: true}}},
		},
		{
			name:     "multiple word types",
			input:    `set name {John Doe}`,
			expected: [][]RawWord{{{Text: "set"}, {Text: "name"}, {Text: "John Doe", Braced: true}}},
		},
		{
			name:     "backslash continuation",
			input:    "a\\\nb",
			expected: [][]RawWord{{{Text: "ab"}}},
		},
		{
			name:     "bracket command substitution in bare word",
			input:    "[cmd arg]",
			expected: [][]RawWord{{{Text: "[cmd arg]"}}},
		},
		{
			name:     "multiple consecutive newlines",
			input:    "a\n\n\nb",
			expected: [][]RawWord{{{Text: "a"}}, {{Text: "b"}}},
		},
		{
			name:     "bare word with bracket depth",
			input:    "foo[bar[baz]]qux",
			expected: [][]RawWord{{{Text: "foo[bar[baz]]qux"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCommands(tt.input)
			if !equalCommands(got, tt.expected) {
				t.Errorf("ParseCommands(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

// equalCommands compares two [][]RawWord values for equality.
func equalCommands(a, b [][]RawWord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j].Text != b[i][j].Text {
				return false
			}
			if a[i][j].Braced != b[i][j].Braced {
				return false
			}
			if a[i][j].Quoted != b[i][j].Quoted {
				return false
			}
		}
	}
	return true
}

// TestAgainstHandWritten compares the go-lemon parser output with the
// hand-written parser from the parent tcl package for select1.test.
func TestAgainstHandWritten(t *testing.T) {
	// Find the test file
	// First try the project root
	candidates := []string{
		"../../../ori/sqlite/test/select1.test",
		"../../../../ori/sqlite/test/select1.test",
		"/Users/muaddib/dev/sqlite/test/select1.test",
		"../../../testdata/select1.test",
	}

	var src []byte
	var err error
	for _, path := range candidates {
		src, err = os.ReadFile(filepath.Clean(path))
		if err == nil {
			break
		}
	}
	if src == nil {
		t.Skip("select1.test not found, skipping comparison test")
	}

	input := string(src)

	// Parse with go-lemon parser
	got := ParseCommands(input)

	// Parse with hand-written parser
	expected := handWrittenParseCommands(input)

	// Compare
	if !equalCommands(got, expected) {
		// Find first difference
		for i := 0; i < len(got) && i < len(expected); i++ {
			if !equalWords(got[i], expected[i]) {
				t.Errorf("First difference at command %d:\n  got:  %v\n  want: %v",
					i, formatWords(got[i]), formatWords(expected[i]))
				break
			}
		}
		if len(got) != len(expected) {
			t.Errorf("Command count mismatch: got %d, want %d", len(got), len(expected))
		}
	}
}

// handWrittenParseCommands replicates the logic from the parent tcl package
// for comparison testing, to avoid circular imports.
func handWrittenParseCommands(src string) [][]RawWord {
	var commands [][]RawWord
	var current []RawWord
	pos := 0
	atStartOfCommand := true

	for pos < len(src) {
		ch := src[pos]

		if ch == '\n' || ch == ';' {
			if len(current) > 0 {
				commands = append(commands, current)
				current = nil
			}
			atStartOfCommand = true
			pos++
			continue
		}

		if ch == '\\' && pos+1 < len(src) && src[pos+1] == '\n' {
			pos += 2
			continue
		}

		if ch == '#' && atStartOfCommand {
			for pos < len(src) && src[pos] != '\n' {
				pos++
			}
			continue
		}

		if ch == ' ' || ch == '\t' || ch == '\r' {
			pos++
			continue
		}

		atStartOfCommand = false

		switch ch {
		case '{':
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
			word := src[start:pos]
			if pos < len(src) {
				pos++
			}
			current = append(current, RawWord{Text: word, Braced: true})

		case '"':
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
				pos++
			}
			current = append(current, RawWord{Text: word, Quoted: true})

		default:
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
						break
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
			current = append(current, RawWord{Text: word})
		}
	}

	if len(current) > 0 {
		commands = append(commands, current)
	}

	return commands
}

func equalWords(a, b []RawWord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Text != b[i].Text || a[i].Braced != b[i].Braced || a[i].Quoted != b[i].Quoted {
			return false
		}
	}
	return true
}

func formatWords(words []RawWord) string {
	s := "["
	for i, w := range words {
		if i > 0 {
			s += " "
		}
		s += "{Text:" + w.Text + " Braced:" + boolStr(w.Braced) + " Quoted:" + boolStr(w.Quoted) + "}"
	}
	return s + "]"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
