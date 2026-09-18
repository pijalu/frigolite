// SPDX-License-Identifier: GPL-3.0-or-later
// Command tclconvert converts SQLite TCL test files (.test) to JSON test
// data files (.json) using a mini TCL interpreter.
//
// Usage:
//
//	go run ./tools/tclconvert/ [-testdir DIR] [-outdir DIR] [file...]
//
// The converter executes each TCL test file through the interpreter, captures
// all SQL statements (with variable substitution and loop unrolling), and
// writes JSON in the same format as the existing testdata/*.json files.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// TestStep matches the JSON format expected by frigolite_harness_test.go.
type TestStep struct {
	Type   string `json:"type"`
	SQL    string `json:"sql,omitempty"`
	Expect string `json:"expect,omitempty"`
}

// TestCase matches the JSON format expected by frigolite_harness_test.go.
type TestCase struct {
	Name  string     `json:"name"`
	Steps []TestStep `json:"steps"`
}

// TestFileData matches the JSON format expected by frigolite_harness_test.go.
type TestFileData struct {
	File      string     `json:"file"`
	Name      string     `json:"name"`
	NullToken string     `json:"nullToken,omitempty"`
	Ordered   bool       `json:"ordered,omitempty"`
	Tests     []TestCase `json:"tests"`
}

func main() {
	testDir := flag.String("testdir", "ori/sqlite/test", "TCL test directory")
	outDir := flag.String("outdir", "testdata", "JSON output directory")
	verbose := flag.Bool("v", false, "verbose output")
	flag.Parse()

	files := selectTestFiles(*testDir)

	processed := 0
	skipped := 0
	errors := 0
	for _, testFile := range files {
		base := strings.TrimSuffix(filepath.Base(testFile), ".test")
		switch processTestFile(testFile, base, *outDir, *verbose) {
		case outcomeProcessed:
			processed++
		case outcomeSkipped:
			skipped++
		case outcomeError:
			errors++
		}
	}

	fmt.Printf("Processed %d/%d files (skipped %d, errors %d)\n", processed, len(files), skipped, errors)
}

// outcome classifies the result of processing one test file.
type outcome int

const (
	outcomeProcessed outcome = iota
	outcomeSkipped
	outcomeError
)

// processTestFile converts one .test file to JSON, writing the output file.
// It returns the outcome classification.
func processTestFile(testFile, base, outDir string, verbose bool) outcome {
	// Skip runner files (they source other .test files, not standalone)
	if isRunnerFile(base) {
		if verbose {
			fmt.Fprintf(os.Stderr, "skip %s: runner file\n", base)
		}
		return outcomeSkipped
	}

	src, err := os.ReadFile(testFile)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", base, err)
		}
		return outcomeSkipped
	}

	stmts, nullToken, err := runInterpreter(src)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "error %s: %v\n", base, err)
		}
		return outcomeError
	}

	// Convert captured statements to JSON
	td := convertToJSON(base, stmts, nullToken)
	if len(td.Tests) == 0 {
		if verbose {
			fmt.Fprintf(os.Stderr, "skip %s: no tests captured (%d stmts)\n", base, len(stmts))
		}
		return outcomeSkipped
	}

	// Write JSON output
	jsonData, err := json.MarshalIndent(td, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json error %s: %v\n", base, err)
		return outcomeError
	}

	outPath := filepath.Join(outDir, base+".json")
	if err := os.WriteFile(outPath, jsonData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write error %s: %v\n", outPath, err)
		return outcomeError
	}

	if verbose {
		fmt.Printf("%s: %d tests, %d statements\n", base, len(td.Tests), len(stmts))
	}
	return outcomeProcessed
}

// selectTestFiles returns the test files to process: all *.test files in the
// directory, or only the named ones when given on the command line.
func selectTestFiles(testDir string) []string {
	if flag.NArg() > 0 {
		var files []string
		for _, f := range flag.Args() {
			files = append(files, filepath.Join(testDir, f))
		}
		return files
	}
	matches, err := filepath.Glob(filepath.Join(testDir, "*.test"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "glob error: %v\n", err)
		os.Exit(1)
	}
	return matches
}

// isRunnerFile reports whether base is a TCL runner file (sources others).
func isRunnerFile(base string) bool {
	return base == "all" || base == "tester" || base == "shared" || base == "qtp"
}

// runInterpreter executes a TCL test file through the interpreter with a
// 30-second timeout.
func runInterpreter(src []byte) ([]tcl.Stmt, string, error) {
	type result struct {
		stmts     []tcl.Stmt
		nullToken string
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		interp := tcl.NewInterp()
		defer func() {
			if r := recover(); r != nil {
				ch <- result{nil, "", fmt.Errorf("panic: %v", r)}
			}
		}()
		interp.Execute(string(src))
		ch <- result{interp.Stmts(), interp.NullToken(), nil}
	}()

	select {
	case r := <-ch:
		return r.stmts, r.nullToken, r.err
	case <-time.After(30 * time.Second):
		return nil, "", fmt.Errorf("timeout")
	}
}

// convertToJSON converts captured TCL statements into the JSON test format.
// Statements are grouped into runs of equal TestName (contiguous-run
// grouping): a do_test whose name repeats later in the file (loop bodies)
// starts a NEW test case so each invocation keeps its own steps and
// expectation, mirroring the TCL harness. Statements with no TestName go
// into "setup" groups. Each group becomes a TestCase with Steps.
func convertToJSON(base string, stmts []tcl.Stmt, nullToken string) TestFileData {
	td := TestFileData{
		File:      base + ".test",
		Name:      base,
		NullToken: nullToken,
		// Statements are captured in TCL execution order; the harness must
		// not re-order them (see TestFileData.Ordered in the harness).
		Ordered: true,
	}

	var groups []*stmtGroup
	var cur *stmtGroup

	resetCounter := 0
	dropCounter := 0
	setupCounter := 0
	for _, s := range stmts {
		name, kind := classifyStmt(s, &resetCounter, &dropCounter)
		if cur == nil || !sameGroup(cur, name, kind) {
			name = finalizeGroupName(name, kind, &setupCounter)
			cur = &stmtGroup{name: name, kind: kind}
			groups = append(groups, cur)
		}
		cur.stmts = append(cur.stmts, s)
	}

	for _, g := range groups {
		if tc, ok := markerTestCase(g); ok {
			td.Tests = append(td.Tests, tc)
			continue
		}
		tc := groupToTestCase(g)
		if len(tc.Steps) > 0 {
			td.Tests = append(td.Tests, tc)
		}
	}

	return td
}

// classifyStmt assigns a grouping name and kind to a captured statement.
// reset_db / drop_all_tables get unique marker names so consecutive markers
// never merge; unnamed statements are "setup" runs.
func classifyStmt(s tcl.Stmt, resetCounter, dropCounter *int) (string, string) {
	switch {
	case s.Type == "reset_db":
		name := fmt.Sprintf("reset_db_%d", *resetCounter)
		*resetCounter++
		return name, "reset"
	case s.Type == "drop_all_tables":
		name := fmt.Sprintf("drop_all_tables_%d", *dropCounter)
		*dropCounter++
		return name, "droptables"
	case s.TestName == "":
		return "", "setup"
	default:
		return s.TestName, "test"
	}
}

// finalizeGroupName numbers a setup group at group-creation time.
func finalizeGroupName(name, kind string, setupCounter *int) string {
	if kind == "setup" {
		numbered := fmt.Sprintf("setup_%d", *setupCounter)
		*setupCounter++
		return numbered
	}
	return name
}

// sameGroup reports whether s belongs to the group cur. Setup statements
// always continue the current setup run (contiguity is positional); test
// statements must carry the same test name.
func sameGroup(cur *stmtGroup, name, kind string) bool {
	if cur.kind != kind {
		return false
	}
	return kind == "setup" || cur.name == name
}

// markerTestCase converts a marker group to its special TestCase; ok is
// false for ordinary statement groups.
func markerTestCase(g *stmtGroup) (TestCase, bool) {
	switch g.kind {
	case "reset":
		return TestCase{Name: "__RESET_DB__"}, true
	case "droptables":
		return TestCase{Name: "__DROP_ALL_TABLES__"}, true
	}
	return TestCase{}, false
}

// stmtGroup groups contiguous statements sharing one test name.
type stmtGroup struct {
	name  string
	kind  string // "test" or "setup" or "reset"
	stmts []tcl.Stmt
}

// groupToTestCase converts a statement group to a TestCase.
func groupToTestCase(g *stmtGroup) TestCase {
	tc := TestCase{Name: g.name}
	for _, s := range g.stmts {
		step := TestStep{
			Type: s.Type,
			SQL:  strings.TrimSpace(s.SQL),
		}
		if s.Expected != "" {
			step.Expect = s.Expected
		}
		if step.SQL != "" {
			tc.Steps = append(tc.Steps, step)
		}
	}
	return tc
}
