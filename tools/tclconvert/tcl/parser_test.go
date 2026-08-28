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
