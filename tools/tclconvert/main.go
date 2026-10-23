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
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// TestStep matches the JSON format expected by frigolite_harness_test.go.
type TestStep struct {
	RepeatCount       int               `json:"repeatCount,omitempty"`
	RepeatBody        []TestStep        `json:"repeatBody,omitempty"`
	Type              string            `json:"type"`
	SQL               string            `json:"sql,omitempty"`
	Variable          string            `json:"variable,omitempty"`
	ResultAll         bool              `json:"resultAll,omitempty"`
	ResultRows        bool              `json:"resultRows,omitempty"`
	StmtStatus        bool              `json:"stmtStatus,omitempty"`
	SortMarker        bool              `json:"sortMarker,omitempty"`
	TransformProc     string            `json:"transformProc,omitempty"`
	TransformBody     string            `json:"transformBody,omitempty"`
	TransformArgs     []string          `json:"transformArgs,omitempty"`
	Condition         string            `json:"condition,omitempty"`
	CatchResult       bool              `json:"catchResult,omitempty"`
	AuthProc          string            `json:"authProc,omitempty"`
	CatchMessage      string            `json:"catchMessage,omitempty"`
	ExpectedExpr      string            `json:"expectedExpr,omitempty"`
	ListValues        []string          `json:"listValues,omitempty"`
	InternalFunctions bool              `json:"internalFunctions,omitempty"`
	Expect            string            `json:"expect,omitempty"`
	ResultTransform   string            `json:"resultTransform,omitempty"`
	FunctionName      string            `json:"functionName,omitempty"`
	CollationName     string            `json:"collationName,omitempty"`
	CollationProc     string            `json:"collationProc,omitempty"`
	CollationBody     string            `json:"collationBody,omitempty"`
	DestructorBody    string            `json:"destructorBody,omitempty"`
	CallbackVars      map[string]string `json:"callbackVars,omitempty"`
	CallbackProcs     []tcl.Proc        `json:"callbackProcs,omitempty"`
	ModuleArgs        []string          `json:"moduleArgs,omitempty"`
	FunctionProc      string            `json:"functionProc,omitempty"`
	FunctionArgs      []string          `json:"functionArgs,omitempty"`
	FunctionBody      string            `json:"functionBody,omitempty"`
	MinArgs           int               `json:"minArgs,omitempty"`
	MaxArgs           int               `json:"maxArgs,omitempty"`
	Deterministic     bool              `json:"deterministic,omitempty"`
	Innocuous         bool              `json:"innocuous,omitempty"`
	DirectOnly        bool              `json:"directOnly,omitempty"`
	Database          string            `json:"database,omitempty"`
	TableName         string            `json:"tableName,omitempty"`
	FileFormat        int               `json:"fileFormat,omitempty"`
	SchemaSQL         string            `json:"schemaSQL,omitempty"`
	FileData          string            `json:"fileData,omitempty"`
	FileTarget        string            `json:"fileTarget,omitempty"`
	AuxSQL            string            `json:"sql2,omitempty"`
	FileOffset        int               `json:"fileOffset,omitempty"`
	LoopVar           string            `json:"loopVar,omitempty"`
	LoopStart         string            `json:"loopStart,omitempty"`
	LoopCond          string            `json:"loopCond,omitempty"`
	LoopStep          int               `json:"loopStep,omitempty"`
	LoopBody          []TestStep        `json:"loopBody,omitempty"`
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

const compressedFixtureThreshold = 1 << 20

func compressFixtureJSON(data []byte) ([]byte, error) {
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	writer.Header.ModTime = time.Time{}
	writer.Header.Name = ""
	writer.Header.Comment = ""
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
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
	for i := range td.Tests {
		td.Tests[i].Steps = compactRepeatedSteps(td.Tests[i].Steps)
	}
	if len(td.Tests) == 0 {
		if verbose {
			fmt.Fprintf(os.Stderr, "skip %s: no tests captured (%d stmts)\n", base, len(stmts))
		}
		return outcomeSkipped
	}

	// Write JSON output. Compress large fixtures: many compatibility corpora
	// contain repeated SQL and compress dramatically without changing schema.
	jsonData, err := json.MarshalIndent(td, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json error %s: %v\n", base, err)
		return outcomeError
	}

	outPath := filepath.Join(outDir, base+".json")
	if len(jsonData) > compressedFixtureThreshold {
		outPath += ".gz"
		jsonData, err = compressFixtureJSON(jsonData)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gzip error %s: %v\n", base, err)
			return outcomeError
		}
	}
	if err := os.WriteFile(outPath, jsonData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write error %s: %v\n", outPath, err)
		return outcomeError
	}
	alternatePath := filepath.Join(outDir, base+".json")
	if outPath == alternatePath {
		alternatePath += ".gz"
	}
	if err := os.Remove(alternatePath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "remove stale fixture %s: %v\n", alternatePath, err)
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

// statementBudget bounds how many statements a single file may capture.
// Runtime-terminated loops (`for {set i 1} {$stop == 0} {incr i}` in
// backup_ioerr.test) otherwise unroll to the 50k loop cap times their nesting
// and produce multi-hundred-megabyte fixtures (1.8M tests / 6.6M statements /
// 1.1 GB for backup_ioerr), which blow up both this converter and any test
// run that loads them. Files over budget are conversion errors: the installed
// fixture stays in place.
const statementBudget = 150000

// conversionDeadline bounds the wall-clock time one file may take.
const conversionDeadline = 30 * time.Second

// runInterpreter executes a TCL test file through the interpreter with a
// statement budget and deadline. Both are enforced by the interpreter itself
// as it runs (checkRunaway), so a runaway loop unwinds and frees its memory.
// The previous implementation used select/time.After, which abandoned a
// goroutine that kept executing and appending to i.stmts forever — one
// leaked runaway loop per timed-out file (multi-GB converter process).
func runInterpreter(src []byte) (stmts []tcl.Stmt, nullToken string, err error) {
	interp := tcl.NewInterp()
	interp.SetStatementBudget(statementBudget)
	interp.SetDeadline(time.Now().Add(conversionDeadline))
	defer func() {
		if r := recover(); r != nil {
			stmts, nullToken, err = nil, "", fmt.Errorf("panic: %v", r)
		}
	}()
	if execErr := interp.Execute(string(src)); execErr != nil {
		return nil, "", execErr
	}
	return interp.Stmts(), interp.NullToken(), nil
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
		groupName := name
		if s.GroupName != "" {
			groupName = s.GroupName
		}
		if cur == nil || !sameGroup(cur, groupName, kind) {
			name = finalizeGroupName(name, kind, &setupCounter)
			cur = &stmtGroup{name: name, kind: kind}
			cur.name = name
			cur.groupName = groupName
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

// compactRepeatedSteps encodes consecutive identical steps as count plus body.
func compactRepeatedSteps(steps []TestStep) []TestStep {
	out := make([]TestStep, 0, len(steps))
	for i := 0; i < len(steps); {
		j := i + 1
		for j < len(steps) && reflect.DeepEqual(steps[i], steps[j]) {
			j++
		}
		if j-i >= 2 {
			out = append(out, TestStep{Type: "repeat", RepeatCount: j - i, RepeatBody: []TestStep{steps[i]}})
		} else {
			out = append(out, steps[i])
		}
		i = j
	}
	return out
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
	case s.Type == "drop_main_tables":
		return "drop_main_tables", "dropmaintables"

	case s.Type == "delete_file":
		name := fmt.Sprintf("delete_file_%d", *dropCounter)
		*dropCounter++
		return name, "deletefile"
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
	return kind == "setup" || cur.groupName == name
}

// markerTestCase converts a marker group to its special TestCase; ok is
// false for ordinary statement groups.
func markerTestCase(g *stmtGroup) (TestCase, bool) {
	switch g.kind {
	case "reset":
		return TestCase{Name: "__RESET_DB__"}, true
	case "droptables":
		return TestCase{Name: "__DROP_ALL_TABLES__"}, true
	case "dropmaintables":
		return TestCase{Name: "__DROP_MAIN_TABLES__"}, true

	case "deletefile":
		if len(g.stmts) != 1 {
			return TestCase{}, false
		}
		return TestCase{Name: "delete_file", Steps: []TestStep{{Type: "delete_file", SQL: g.stmts[0].SQL}}}, true
	}
	return TestCase{}, false
}

// stmtGroup groups contiguous statements sharing one harness group.
type stmtGroup struct {
	name      string
	groupName string
	kind      string
	stmts     []tcl.Stmt
}

// groupToTestCase converts a statement group to a TestCase.
func groupToTestCase(g *stmtGroup) TestCase {
	tc := TestCase{Name: tcl.StripRuntimeValues(g.name)}
	for _, s := range g.stmts {
		step := stepFromStmt(s)
		if step.SQL != "" || step.Type == "runtime_loop" || step.Type == "register_function" || step.Type == "register_collation" || step.Type == "delete_collation" || step.Type == "runtime_var" || step.Type == "register_echo_module" || step.Type == "register_tcl_module" || step.Type == "register_test_funcs" || step.Type == "defensive" || step.Type == "auth_analyze_deny" || step.Type == "auth_proc" || step.Type == "close_db" || step.Type == "reopen_db" || step.Type == "open_empty_db" || step.Type == "open_named_db" || step.Type == "close_named_db" || step.Type == "nullvalue" || step.Type == "set_current_time" || step.Type == "set_limit" || step.Type == "alter_table" || step.Type == "header_format" || step.Type == "write_file" || step.Type == "copy_file" || step.Type == "prepare_stmt" || step.Type == "stmt_step" || step.Type == "stmt_reset" || step.Type == "stmt_finalize" || step.Type == "runtime_assert" || step.Type == "cacheflush" {
			tc.Steps = append(tc.Steps, step)
		}
	}
	return tc
}

// stripRuntimeValuesDeep removes the runtime markers from every string of a
// value. The markers are the converter's internal representation of a query
// whose rows exist only at run time; they must never reach a fixture.
func stripRuntimeValuesDeep(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(tcl.StripRuntimeValues(v.String()))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			stripRuntimeValuesDeep(v.Index(i))
		}
	case reflect.Map:
		if v.Type().Elem().Kind() == reflect.String {
			for _, key := range v.MapKeys() {
				val := v.MapIndex(key)
				cleaned := reflect.ValueOf(tcl.StripRuntimeValues(val.String())).Convert(v.Type().Elem())
				v.SetMapIndex(key, cleaned)
			}
			return
		}
		for _, key := range v.MapKeys() {
			stripRuntimeValuesDeep(v.MapIndex(key))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			stripRuntimeValuesDeep(v.Field(i))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			stripRuntimeValuesDeep(v.Elem())
		}
	}
}

// stepFromStmt converts one captured statement into a fixture step.
func stepFromStmt(s tcl.Stmt) TestStep {
	step := TestStep{
		Type:              s.Type,
		SQL:               tcl.StripRuntimeValues(strings.TrimSpace(s.SQL)),
		Variable:          tcl.StripRuntimeValues(s.Variable),
		ResultAll:         s.ResultAll,
		ResultRows:        s.ResultRows,
		StmtStatus:        s.StmtStatus,
		SortMarker:        s.SortMarker,
		TransformProc:     s.TransformProc,
		TransformBody:     s.TransformBody,
		TransformArgs:     s.TransformArgs,
		Condition:         tcl.StripRuntimeValues(s.Condition),
		CatchResult:       s.CatchResult,
		AuthProc:          s.AuthProc,
		CatchMessage:      s.CatchMessage,
		ExpectedExpr:      tcl.StripRuntimeValues(s.ExpectedExpr),
		ListValues:        s.ListValues,
		InternalFunctions: s.Type == "internal_functions" && s.SQL == "enable",
		Database:          s.Database,
		ResultTransform:   s.ResultTransform,
		FunctionName:      s.FunctionName,
		CollationName:     s.CollationName,
		CollationProc:     s.CollationProc,
		CollationBody:     s.CollationBody,
		DestructorBody:    s.DestructorBody,
		CallbackVars:      s.CallbackVars,
		CallbackProcs:     s.CallbackProcs,
		ModuleArgs:        s.ModuleArgs,
		FunctionProc:      s.FunctionProc,
		FunctionArgs:      s.FunctionArgs,
		FunctionBody:      s.FunctionBody,
		MinArgs:           s.MinArgs,
		MaxArgs:           s.MaxArgs,
		Deterministic:     s.Deterministic,
		Innocuous:         s.Innocuous,
		DirectOnly:        s.DirectOnly,
		TableName:         s.TableName,
		FileFormat:        s.FileFormat,
		SchemaSQL:         s.SchemaSQL,
		FileData:          s.FileData,
		FileTarget:        s.FileTarget,
		AuxSQL:            s.AuxSQL,
		FileOffset:        s.FileOffset,
		LoopVar:           s.LoopVar,
		LoopStart:         s.LoopStart,
		LoopCond:          s.LoopCond,
		LoopStep:          s.LoopStep,
	}
	stripRuntimeValuesDeep(reflect.ValueOf(&step).Elem())
	if s.Type == "runtime_loop" {
		for _, b := range s.LoopBody {
			step.LoopBody = append(step.LoopBody, stepFromStmt(b))
		}
	}
	if s.Expected != "" {
		step.Expect = s.Expected
	}
	return step
}
