// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestParseCommandsTrailingBackslashNoPanic guards against a slice-bounds
// panic in the plain-word/brace/quote scanners when the input ends with a
// backslash. This happens in real TCL test files: a multi-line command
// substitution whose last line ends with a backslash-newline continuation
// (e.g. "foreach f [glob ... \n  $dir/*.test \\]") is trimmed by the
// transpiler, leaving a dangling backslash as the final character. Before
// the fix, the scanner did pos += 2 past the end of the slice and crashed
// with "slice bounds out of range". The trailing backslash is now kept as
// a literal word character.
func TestParseCommandsTrailingBackslashNoPanic(t *testing.T) {
	cases := []string{
		"glob -nocomplain            \\\n    $testdir/../ext/rtree/*.test       \\",
		"set x {abc\\",  // brace word ending in backslash
		"set x \"abc\\", // quoted word ending in backslash
		"set x abc\\",   // plain word ending in backslash
		"\\",            // lone backslash
	}
	for _, src := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseCommands(%q) panicked: %v", src, r)
				}
			}()
			_ = ParseCommands(src)
		}()
	}
}

// TestParseCommandsTrailingBackslashWord verifies the trailing backslash is
// retained as part of the last plain word rather than being dropped or
// crashing, matching the tokens tcl2go expects for a glob command.
func TestParseCommandsTrailingBackslashWord(t *testing.T) {
	src := "glob -nocomplain            \\\n    $testdir/../ext/rtree/*.test       \\"
	cmds := ParseCommands(src)
	if len(cmds) != 1 {
		t.Fatalf("got %d commands, want 1", len(cmds))
	}
	words := cmds[0]
	if len(words) != 4 {
		t.Fatalf("got %d words, want 4: %+v", len(words), words)
	}
	want := []string{"glob", "-nocomplain", "$testdir/../ext/rtree/*.test", `\`}
	for i, w := range want {
		if words[i].Text != w {
			t.Errorf("word %d = %q, want %q", i, words[i].Text, w)
		}
	}
}

// TestParseCommandsMarksArgumentExpansion verifies TCL {*} argument-expansion
// words are recognized as a single raw word with the prefix stripped: the
// remainder is parsed as a bare, braced, or quoted word and evaluated by the
// caller into list elements.
func TestParseCommandsMarksArgumentExpansion(t *testing.T) {
	cmds := ParseCommands("list {*}$data {*}{a b c} {*}\"x y\"")
	if len(cmds) != 1 || len(cmds[0]) != 4 {
		t.Fatalf("commands = %#v, want one command with four words", cmds)
	}
	words := cmds[0]
	if words[1].Expand != true || words[1].Braced || words[1].Text != "$data" {
		t.Errorf("bare expansion word = %+v, want Expand with text $data", words[1])
	}
	if words[2].Expand != true || !words[2].Braced || words[2].Text != "a b c" {
		t.Errorf("braced expansion word = %+v, want Expand with braced text a b c", words[2])
	}
	if words[3].Expand != true || !words[3].Quoted || words[3].Text != "x y" {
		t.Errorf("quoted expansion word = %+v, want Expand with quoted text x y", words[3])
	}
}

// TestParseCommandsBareExpansionPrefixIsLiteral verifies a word consisting of
// exactly {*} stays an ordinary braced word with the value "*" (TCL expands
// only when the prefix is followed by more word characters).
func TestParseCommandsBareExpansionPrefixIsLiteral(t *testing.T) {
	cmds := ParseCommands("list {*} {*}")
	words := cmds[0]
	if len(words) != 3 || words[1].Expand || !words[1].Braced || words[1].Text != "*" || words[2].Expand {
		t.Fatalf("words = %#v, want literal braced * values", words)
	}
}
