package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fallbackFamily is used when no families.tsv pattern matches a package.
const fallbackFamily = "OTHER"

// familyPattern is one line of families.tsv: a filepath.Match glob applied
// to the testgen package directory name.
type familyPattern struct {
	pattern string
	family  string
}

// families holds the ordered pattern list from families.tsv. First match
// wins.
type families struct {
	patterns []familyPattern
}

// loadFamilies reads tools/status/families.tsv: lines of
// "<glob-pattern>\t<family>"; blank lines and lines starting with '#' are
// ignored.
func loadFamilies(path string) (*families, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var fams families
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.SplitN(text, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"pattern\\tfamily\", got %q", path, line, text)
		}
		pattern := strings.TrimSpace(parts[0])
		family := strings.TrimSpace(parts[1])
		if pattern == "" || family == "" {
			return nil, fmt.Errorf("%s:%d: empty pattern or family", path, line)
		}
		fams.patterns = append(fams.patterns, familyPattern{pattern: pattern, family: family})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return &fams, nil
}

// classify maps a testgen package name to a family. The first matching glob
// wins; unmatched packages fall back to OTHER.
func (f *families) classify(pkg string) string {
	for _, p := range f.patterns {
		ok, err := filepath.Match(p.pattern, pkg)
		if err == nil && ok {
			return p.family
		}
	}
	return fallbackFamily
}
