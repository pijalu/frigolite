// SPDX-License-Identifier: GPL-3.0-or-later
// Command tclconvert converts SQLite TCL test files (.test) to JSON test
// data files (.json) using a mini TCL interpreter.
//
// Usage:
//   go run ./tools/tclconvert/ [-testdir DIR] [-outdir DIR] [file...]
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
	File  string     `json:"file"`
	Name  string     `json:"name"`
	Tests []TestCase `json:"tests"`
}

func main() {
	testDir := flag.String("testdir", "ori/sqlite/test", "TCL test directory")
	outDir := flag.String("outdir", "testdata", "JSON output directory")
	verbose := flag.Bool("v", false, "verbose output")
	flag.Parse()

	// Determine which files to process
	var files []string
	if flag.NArg() > 0 {
		for _, f := range flag.Args() {
			files = append(files, filepath.Join(*testDir, f))
		}
	} else {
		matches, err := filepath.Glob(filepath.Join(*testDir, "*.test"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "glob error: %v\n", err)
			os.Exit(1)
		}
		files = matches
	}

	processed := 0
	skipped := 0
	errors := 0
	for _, testFile := range files {
		base := strings.TrimSuffix(filepath.Base(testFile), ".test")
		
		// Skip runner files (they source other .test files, not standalone)
		if base == "all" || base == "tester" || base == "shared" || base == "qtp" {
			if *verbose {
				fmt.Fprintf(os.Stderr, "skip %s: runner file\n", base)
			}
			skipped++
			continue
		}
		
		src, err := os.ReadFile(testFile)
		if err != nil {
			if *verbose {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", base, err)
			}
			skipped++
			continue
		}

		// Execute TCL test file through the interpreter (with timeout)
		type result struct {
			stmts []tcl.Stmt
			err   error
		}
		ch := make(chan result, 1)
		go func() {
			interp := tcl.NewInterp()
			defer func() {
				if r := recover(); r != nil {
					ch <- result{nil, fmt.Errorf("panic: %v", r)}
				}
			}()
			interp.Execute(string(src))
			ch <- result{interp.Stmts(), nil}
		}()
		
		var stmts []tcl.Stmt
		select {
		case r := <-ch:
			if r.err != nil {
				if *verbose {
					fmt.Fprintf(os.Stderr, "error %s: %v\n", base, r.err)
				}
				errors++
				continue
			}
			stmts = r.stmts
		case <-time.After(30 * time.Second):
			if *verbose {
				fmt.Fprintf(os.Stderr, "timeout %s\n", base)
			}
			errors++
			continue
		}

		// Convert captured statements to JSON
		td := convertToJSON(base, stmts)
		if len(td.Tests) == 0 {
			if *verbose {
				fmt.Fprintf(os.Stderr, "skip %s: no tests captured (%d stmts)\n", base, len(stmts))
			}
			skipped++
			continue
		}

		// Write JSON output
		jsonData, err := json.MarshalIndent(td, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "json error %s: %v\n", base, err)
			continue
		}

		outPath := filepath.Join(*outDir, base+".json")
		if err := os.WriteFile(outPath, jsonData, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write error %s: %v\n", outPath, err)
			continue
		}

		processed++
		if *verbose {
			fmt.Printf("%s: %d tests, %d statements\n", base, len(td.Tests), len(stmts))
		}
	}

	fmt.Printf("Processed %d/%d files (skipped %d, errors %d)\n", processed, len(files), skipped, errors)
}

// convertToJSON converts captured TCL statements into the JSON test format.
// Statements are grouped by TestName. Statements with no TestName go into
// a "setup" group. Each group becomes a TestCase with Steps.
func convertToJSON(base string, stmts []tcl.Stmt) TestFileData {
	td := TestFileData{
		File: base,
		Name: base,
	}

	// Group statements by TestName, preserving capture order
	type group struct {
		name string
		stmts []tcl.Stmt
	}
	var groups []*group
	groupIdx := make(map[string]*group)

	setupCounter := 0
	for _, s := range stmts {
		name := s.TestName
		if name == "" {
			name = fmt.Sprintf("setup_%d", setupCounter)
			setupCounter++
		}

		g, ok := groupIdx[name]
		if !ok {
			g = &group{name: name}
			groupIdx[name] = g
			groups = append(groups, g)
		}
		g.stmts = append(g.stmts, s)
	}

	// Convert groups to TestCases
	for _, g := range groups {
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
		if len(tc.Steps) > 0 {
			td.Tests = append(td.Tests, tc)
		}
	}

	return td
}
