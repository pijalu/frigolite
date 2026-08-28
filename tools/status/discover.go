package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// helpersFile is the shared generated helper filename, excluded from
// per-package test-file accounting.
const helpersFile = "helpers_test.go"

// wholeFileSkipRE matches the top-level "// skipped: reason" marker emitted
// by the transpiler for a whole-file skip stub. Per-test skips use
// '{ // "name" — skipped: reason' (inside braces, with an em-dash), which
// does not match.
var wholeFileSkipRE = regexp.MustCompile(`(?m)^// skipped: `)

// perTestSkipRE matches the inline skip marker emitted for an individual
// skipped test: `{ // "name" — skipped: reason`. The "(SQL side effects
// only)" variant also carries the em-dash marker.
var perTestSkipRE = regexp.MustCompile(`(?m)\{ // "[^"]*" — skipped: `)

// discoverPackages scans the testgen tree and classifies each package:
//   - family: from families.tsv
//   - testFiles: generated *_test.go files excluding helpers_test.go
//   - skipFiles: how many of those are whole-file skip stubs
//   - skipTests: how many per-test skip markers appear across real files
//   - reason: whole-file skip reason when every test file is skipped
//   - state: "skipped" when all test files are whole-file stubs, else
//     "not-run" (the runner sets pass/fail).
func discoverPackages(testgenDir string, skips *skipMaps, fams *families) ([]*pkgInfo, error) {
	entries, err := os.ReadDir(testgenDir)
	if err != nil {
		return nil, err
	}

	var pkgs []*pkgInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		p := &pkgInfo{
			name:   name,
			family: fams.classify(name),
			state:  stateNotRun,
		}
		files, err := filepath.Glob(filepath.Join(testgenDir, name, "*_test.go"))
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		if err := classifyPackageFiles(p, files, skips); err != nil {
			return nil, err
		}

		// A package whose generated test files are ALL whole-file skip stubs
		// is classified as skipped; it is never run (go test would pass
		// trivially on the empty test functions).
		if p.testFiles > 0 && p.skipFiles == p.testFiles {
			p.state = stateSkipped
		}
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].name < pkgs[j].name })
	return pkgs, nil
}

// classifyPackageFiles counts test files, whole-file skip stubs, and per-test
// skip markers for one generated test package.
func classifyPackageFiles(p *pkgInfo, files []string, skips *skipMaps) error {
	for _, file := range files {
		if filepath.Base(file) == helpersFile {
			continue
		}
		p.testFiles++
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		base := strings.TrimSuffix(filepath.Base(file), "_test.go")
		if wholeFileSkipRE.Match(content) {
			p.skipFiles++
			if reason, ok := skips.skipTestFiles[base]; ok {
				p.reason = reason
			} else if p.reason == "" {
				p.reason = extractSkipReason(content)
			}
			continue
		}
		p.skipTests += len(perTestSkipRE.FindAll(content, -1))
	}
	return nil
}

// extractSkipReason pulls the reason text out of a whole-file skip stub's
// "// skipped: reason" comment.
func extractSkipReason(content []byte) string {
	lines := strings.Split(string(content), "\n")
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "// skipped:") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "// skipped:"))
		}
	}
	return ""
}
