// SPDX-License-Identifier: GPL-3.0-or-later
package tclparser

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorpusMatchesHandWritten runs the go-lemon parser and the hand-written
// parser over every .test file in the SQLite test corpus and reports any
// structural differences. This is the acceptance gate for the drop-in parser
// swap in tcl2go.
func TestCorpusMatchesHandWritten(t *testing.T) {
	corpusDirs := []string{
		"../../../ori/sqlite/test",
		"../../../../ori/sqlite/test",
		"/Users/muaddib/dev/sqlite/test",
	}
	var root string
	for _, d := range corpusDirs {
		if fi, err := os.Stat(filepath.Clean(d)); err == nil && fi.IsDir() {
			root = filepath.Clean(d)
			break
		}
	}
	if root == "" {
		t.Skip("SQLite test corpus not found, skipping corpus comparison")
	}

	files, err := filepath.Glob(filepath.Join(root, "*.test"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .test files found")
	}

	total := 0
	diffs := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		total++

		got := ParseCommands(string(src))
		expected := handWrittenParseCommands(string(src))

		if !equalCommands(got, expected) {
			diffs++
			// Find first difference
			for i := 0; i < len(got) && i < len(expected); i++ {
				if !equalWords(got[i], expected[i]) {
					t.Errorf("%s: first difference at command %d:\n  got:  %v\n  want: %v",
						filepath.Base(f), i, formatWords(got[i]), formatWords(expected[i]))
					break
				}
			}
			if len(got) != len(expected) {
				t.Errorf("%s: command count mismatch: got %d, want %d",
					filepath.Base(f), len(got), len(expected))
			}
		}
	}

	t.Logf("corpus: %d files, %d with differences", total, diffs)
}
