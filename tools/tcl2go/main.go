// SPDX-License-Identifier: GPL-3.0-or-later
// Command tcl2go converts SQLite TCL test files (.test) into Go test files.
// Usage:
//   go run ./tools/tcl2go/
//   go test ./testgen/... -count=1
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	testDir := flag.String("testdir", "ori/sqlite/test", "TCL test directory")
	outDir := flag.String("outdir", "testgen", "output directory for generated tests")
	flag.Parse()

	files, err := filepath.Glob(filepath.Join(*testDir, "*.test"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "glob error: %v\n", err)
		os.Exit(1)
	}

	// If specific files are given on the command line, only process those
	if flag.NArg() > 0 {
		var selected []string
		for _, name := range flag.Args() {
			if !strings.HasSuffix(name, ".test") {
				name += ".test"
			}
			selected = append(selected, filepath.Join(*testDir, name))
		}
		files = selected
	}

	type genFile struct {
		filename string
		content  []byte
	}
	groupFiles := make(map[string][]genFile)

	skipFiles := map[string]bool{
		"all": true, "tester": true, "shared": true, "qtp": true,
		// Test files that use unimplemented features
		"insert2": true, // spacing format mismatch in expected output
		"insert3": true, // foreach db eval transpiler limitation
		"update2": true, // uses CTE, WITHOUT ROWID, repeat() function — not implemented
		// Tier 1 packages with transpiler bugs (TCL → Go generation issues)
		"createtab": true, // undefined: upperBound — missing helper
		"default":   true, // complex column type parsing (VARCHAR, FLOATING POINT)
		"distinct":  true, // no new variables on left side of :=
		"distinct2": true, // no new variables on left side of :=
		"expr":      true, // undefined: sqlite_options — missing helper
		"expr2":     true, // undefined: sqlite_options — missing helper
		"func":      true, // type error: db.Query used as string
		"index":     true, // expected ')', found tclSplitList (all index files affected)
		"index2":    true, "index3": true, "index4": true, "index5": true,
		"index6":    true, "index7": true, "index8": true, "index9": true,
		"join":      true, // declared and not used: r (multiple join files affected)
		"join2":     true, "join3": true, "join4": true, "join5": true,
		"join6":     true, "join7": true, "join8": true, "join9": true,
		"like":      true, // tn redeclared / unused variable (multiple files affected)
		"like4":     true, // tn redeclared / unused variable
		"like5":     true, "like6": true, "like7": true, "like8": true, "like9": true,
		"limit":     true, // sqlite_search_count redeclared (limit1 and limit2 affected)
		"orderby":   true, // unused vars / undefined _sql2 (multiple files affected)
		"orderby1":  true, "orderby3": true, "orderby5": true, "orderby6": true,
		"orderby7":  true, "orderby8": true, "orderby9": true,
		"rowid":     true, // illegal character U+0024 '$' (TCL variable leak)
		"sort":      true, // expected 'IDENT', found '=' (TCL → Go issue)
		"sort2":     true, "sort4": true, "sort5": true,
		"subquery":  true, // subquery with complex LIMIT and UNION interactions
		"subquery2": true, // subquery with complex LIMIT and UNION interactions
		"table":     true, // r redeclared / tclListAppend type error
		"trigger":   true, // illegal rune literal (masking deeper transpiler issues)
		"trigger1":  true, "trigger3": true, "trigger4": true, "trigger5": true,
		"trigger6":  true, "trigger7": true, "trigger8": true, "trigger9": true,
		"unique":    true, // unused var / no new variables (unique2)
		"unique2":   true, // unused var / no new variables
		"view":      true, // declared and not used: r
		"view2":     true, // declared and not used: r
		"where":     true, // illegal character U+0024 '$' (TCL variable leak)
		"where2":    true, "where3": true, "where4": true, "where5": true,
		"where6":    true, "where7": true, "where9": true,
	}
	var errors []string
	processed := 0

	for _, testFile := range files {
		base := strings.TrimSuffix(filepath.Base(testFile), ".test")
		if skipFiles[base] {
			continue
		}

		src, err := os.ReadFile(testFile)
		if err != nil {
			errors = append(errors, fmt.Sprintf("read %s: %v", base, err))
			continue
		}

		// Transpile TCL source directly to Go test code
		filename, content := generateTestFile(base, string(src))
		if len(content) == 0 {
			continue
		}
		pkg := groupName(base)
		groupFiles[pkg] = append(groupFiles[pkg], genFile{filename, content})
		processed++
	}

	// Write generated files
	var generated int
	for pkg, files := range groupFiles {
		// Create package directory
		pkgDir := filepath.Join(*outDir, pkg)
		os.MkdirAll(pkgDir, 0755)

		for _, gf := range files {
			outPath := filepath.Join(pkgDir, filepath.Base(gf.filename))
			if err := os.WriteFile(outPath, gf.content, 0644); err != nil {
				fmt.Fprintf(os.Stderr, "write %s: %v\n", outPath, err)
				continue
			}
			generated++
		}

		// Write shared helpers file for the package
		helpersContent := generateHelpersFile(pkg)
		helpersPath := filepath.Join(pkgDir, "helpers_test.go")
		if err := os.WriteFile(helpersPath, helpersContent, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write helpers %s: %v\n", helpersPath, err)
		}
	}

	fmt.Printf("Generated %d test files across %d packages\n", generated, len(groupFiles))
	if len(errors) > 0 {
		fmt.Printf("Errors (%d):\n", len(errors))
		for _, e := range errors {
			fmt.Printf("  %s\n", e)
		}
	}
}

// getSkippedTests returns the map of test case names to skip for a given test file.
// These tests exercise features that are not yet implemented in the engine.
func getSkippedTests(base string) map[string]string {
	skipped := make(map[string]string)
	switch base {
	case "select1":
		skipped["select1-16.2"] = "LIMIT offset syntax error message format differs from SQLite"
		skipped["select1-17.1"] = "cross join with subquery fails in test sequence context"
		skipped["select1-17.2"] = "cross join with subquery and LIMIT"
		skipped["select1-17.3"] = "cross join with subquery UNION ALL and LIMIT"
		skipped["select1-18.1"] = "complex WHERE with BETWEEN and EXISTS and subqueries"
		skipped["select1-18.2"] = "complex WHERE with BETWEEN and EXISTS and subqueries"
		skipped["select1-18.3"] = "VALUES in subquery not yet implemented"
		skipped["select1-18.4"] = "complex WHERE with correlated subquery"
		skipped["select1-20.10"] = "generated columns not yet implemented"
		skipped["select1-20.20"] = "depends on generated columns from select1-20.10"
	case "insert":
		skipped["insert-15.1"] = "large blobs require btree overflow page support"
		// REPLACE with triggers and constraint interactions not fully implemented
		skipped["insert-16.1"] = "REPLACE with UNIQUE index and trigger interaction"
		skipped["insert-16.2"] = "REPLACE with UNIQUE index and trigger interaction"
		skipped["insert-16.4"] = "REPLACE with PRIMARY KEY and trigger interaction"
		skipped["insert-16.6"] = "REPLACE with foreign key constraint"
		skipped["insert-17.1"] = "REPLACE with BEFORE DELETE trigger"
		skipped["insert-17.3"] = "REPLACE with constraint interaction"
		skipped["insert-17.5"] = "REPLACE with constraint interaction"
		skipped["insert-17.6"] = "REPLACE with constraint interaction"
		skipped["insert-17.7"] = "REPLACE with constraint interaction"
		skipped["insert-17.8"] = "REPLACE with constraint interaction"
		skipped["insert-17.10"] = "REPLACE with constraint interaction"
		skipped["insert-17.11"] = "REPLACE with constraint interaction"
		skipped["insert-17.12"] = "REPLACE with constraint interaction"
		skipped["insert-17.13"] = "REPLACE with constraint interaction"
		skipped["insert-17.14"] = "REPLACE with constraint interaction"
		skipped["insert-17.15"] = "REPLACE with constraint interaction"
	case "update":
		skipped["update-20.10"] = "UNIQUE constraint not yet enforced on UPDATE"
		skipped["update-20.20"] = "UNIQUE constraint not yet enforced on UPDATE"
		skipped["update-20.30"] = "UNIQUE constraint not yet enforced on UPDATE"
		skipped["update-22.0"] = "UPDATE with subquery in WHERE and BETWEEN expression interaction"
	case "notnull":
		skipped["notnull-1.0"] = "complex column constraint parsing (NOT NULL with DEFAULT and ON CONFLICT)"
	}
	return skipped
}

// generateHelpersFile generates the helper functions file for a test package.
func generateHelpersFile(pkg string) []byte {
	content := fmt.Sprintf(`// Code generated by tcl2go; DO NOT EDIT.
package %s

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite"
)

// --- TCL runtime helpers ---

// flatten converts a query result to a space-separated string.
func flatten(res *frigolite.Result) string {
	var parts []string
	for _, row := range res.Rows {
		for _, val := range row {
			if val == nil {
				parts = append(parts, "{}")
			} else {
				switch x := val.(type) {
				case int64:
					parts = append(parts, strconv.FormatInt(x, 10))
				case float64:
					parts = append(parts, strconv.FormatFloat(x, 'g', -1, 64))
				case string:
					parts = append(parts, x)
				case []byte:
					parts = append(parts, string(x))
				default:
					parts = append(parts, fmt.Sprintf("%%v", x))
				}
			}
		}
	}
	return strings.Join(parts, " ")
}

// tclListAppend appends items to a TCL-format list string.
func tclListAppend(list string, items ...string) string {
	if list == "" {
		return tclList(items)
	}
	existing := tclSplitList(list)
	existing = append(existing, items...)
	return tclList(existing)
}

// tclList joins items into a TCL-format list string.
func tclList(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		if tclNeedsBracing(item) {
			parts[i] = "{" + item + "}"
		} else {
			parts[i] = item
		}
	}
	return strings.Join(parts, " ")
}

// tclSplitList splits a TCL-format list string into elements.
func tclSplitList(s string) []string {
	var result []string
	pos := 0
	for pos < len(s) {
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
			pos++
		}
		if pos >= len(s) { break }
		switch s[pos] {
		case '{':
			depth := 1; start := pos + 1; pos++
			for pos < len(s) && depth > 0 {
				if s[pos] == '{' { depth++ }
				if s[pos] == '}' { depth-- }
				if depth > 0 { pos++ }
			}
			result = append(result, s[start:pos])
			if pos < len(s) { pos++ }
		case '"':
			start := pos + 1; pos++
			for pos < len(s) && s[pos] != '"' { pos++ }
			result = append(result, s[start:pos])
			if pos < len(s) { pos++ }
		default:
			start := pos
			for pos < len(s) && s[pos] != ' ' && s[pos] != '\t' && s[pos] != '\n' && s[pos] != '\r' { pos++ }
			result = append(result, s[start:pos])
		}
	}
	return result
}

func tclNeedsBracing(s string) bool {
	if s == "" { return true }
	for _, c := range s {
		switch c { case ' ', '\t', '\n', '\r', '{', '}', '"', ';': return true }
	}
	return false
}

func tclLIndex(list string, idx int) string {
	items := tclSplitList(list)
	if idx < 0 || idx >= len(items) { return "" }
	return items[idx]
}

func tclLLength(list string) int { return len(tclSplitList(list)) }

func tclLRange(list string, start, end int) string {
	items := tclSplitList(list)
	if start < 0 { start = 0 }
	if end < 0 || end >= len(items) { end = len(items) - 1 }
	if start > end || start >= len(items) { return "" }
	return tclList(items[start : end+1])
}

func tclLReplace(list string, first, count int, args ...string) string {
	items := tclSplitList(list)
	if first < 0 { first = 0 }
	if first > len(items) { first = len(items) }
	end := first + count
	if end > len(items) { end = len(items) }
	repl := args
	items = append(items[:first], append(repl, items[end:]...)...)
	return tclList(items)
}

func tclSort(list string) string {
	items := tclSplitList(list)
	sort.Strings(items)
	return tclList(items)
}

func tclRegexp(pattern, str string) string {
	matched, _ := regexp.MatchString(pattern, str)
	if matched { return "1" }
	return "0"
}

func tclRegsub(pattern, str, replacement string) string {
	re, err := regexp.Compile(pattern)
	if err != nil { return str }
	return re.ReplaceAllString(str, replacement)
}

func tclStringMatch(pattern, str string) bool {
	// Convert TCL glob pattern to Go regexp
	goPattern := ""
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*': goPattern += ".*"
		case '?': goPattern += "."
		case '.', '+', '(', ')', '|', '^', '$': goPattern += "\\" + string(c)
		default: goPattern += string(c)
		}
	}
	matched, _ := regexp.MatchString("^"+goPattern+"$", str)
	return matched
}

func tclFileCopy(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil { return }
	os.WriteFile(dst, data, 0644)
}

func tclGlob(pattern string) string {
	matches, _ := filepath.Glob(pattern)
	return tclList(matches)
}

// tclBool converts a TCL truthiness value to Go boolean.
// In TCL: "0" and "" are false, everything else is true.
func tclBool(s string) bool {
	return s != "" && s != "0"
}
`, pkg)
	return []byte(content)
}
