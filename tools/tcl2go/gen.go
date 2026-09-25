// SPDX-License-Identifier: GPL-3.0-or-later
// Package main implements the tcl2go tool: a TCL-to-Go transpiler that converts
// SQLite TCL test files (.test) into standalone Go test files (_test.go).
//
// # Architecture
//
// tcl2go is a TRANSPILER (not an interpreter). It:
//  1. Reads a .test TCL file
//  2. Parses TCL commands using parseCommands() from tools/tclconvert/tcl/
//  3. Walks the parsed command tree and emits Go source code directly
//
// No TCL execution happens at generation time. All TCL control flow constructs
// (foreach, for, while, if) become native Go control flow that runs at test
// runtime. This yields a >200x speedup over the old interpreter approach
// (all 1002+ test files generated in ~0.5s vs timeout at 120s+).
//
// Key transpilation mappings:
//
//	TCL Construct          → Go Output
//	─────────────────────────────────────────────────────────────
//	do_execsql_test ...    → Go test block with db.Query/db.Exec
//	do_catchsql_test ...   → Go test block with error checking
//	do_test ...            → Go test block with transpiled body
//	execsql {SQL}          → db.Exec("SQL") with error check
//	db eval {SQL}          → db.Exec("SQL") with error check
//	foreach V L {BODY}     → Go for range over string slice
//	for {I} {C} {N} {B}   → Go for loop
//	while {C} {B}         → Go for loop
//	if {C} {B} else       → Go if/else
//	set VAR VALUE         → Go variable assignment
//	incr VAR [N]          → Go strconv-based increment
//	$var / ${var}         → Go variable access (string concatenation)
//	[expr {CONST}]        → Evaluated at generation time
//	reset_db              → db.Close + db.Open
//	source, finish_test   → No-op (infrastructure skipped)
//
// This file contains the per-file driver: generateTestFile and its phases
// (state reset, variable pre-collection, pre-declaration emission, final
// assembly). The source scans live in genscan.go, the preamble emission in
// genpreamble.go, and import detection in genimports.go.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// generateTestFile takes TCL source code and generates a Go test file.
// Returns the relative path and file content.
// package-level blob channel bookkeeping that persists across all bodyTP
// copies during one file's transpilation (the tp fields are not reliably
// shared across every body-block copy).
var genBlobUsedChannels map[string]bool
var genBlobVarNames map[string]bool

// genTclEvalSetVars holds the scalar variables set through SQL function
// calls routed to the TCL eval command (`db function tcl eval` +
// `tcl('set VAR', <value>)` — the only corpus instance is tkt3992-2.3).
// Pre-scanned from the raw file source because the registration site
// precedes the trigger SQL that contains the calls.
var genTclEvalSetVars []string

// scanTclEvalSetVars scans TCL source for SQL `tcl('set VAR', ...)`
// invocations (single- or double-quoted first argument of exactly two
// words "set VAR") and returns the unique variable names in order.
func scanTclEvalSetVars(src string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(src); {
		arg, next, ok := scanNextTclSetArg(src, i)
		if !ok {
			return out
		}
		i = next
		if fields := strings.Fields(arg); len(fields) == 2 && fields[0] == "set" && !seen[fields[1]] {
			seen[fields[1]] = true
			out = append(out, fields[1])
		}
	}
	return out
}

// isTclIdentByte reports whether c is a TCL identifier character.
func isTclIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// scanNextTclSetArg finds the next `tcl('...')` call site at or after from
// in src and returns its quoted first argument. ok is false when no further
// call exists; next is the offset to resume scanning from.
func scanNextTclSetArg(src string, from int) (arg string, next int, ok bool) {
	for i := from; i+4 <= len(src); {
		j := strings.Index(src[i:], "tcl(")
		if j < 0 {
			return "", len(src), false
		}
		i += j
		if i > 0 && isTclIdentByte(src[i-1]) { // not a word boundary (e.g. fts3tcl()
			i += 4
			continue
		}
		i += 4
		if i >= len(src) || (src[i] != '\'' && src[i] != '"') {
			continue
		}
		end := strings.IndexByte(src[i+1:], src[i])
		if end < 0 {
			return "", len(src), false
		}
		return src[i+1 : i+1+end], i + end + 1, true
	}
	return "", len(src), false
}

func generateTestFile(base string, src string, testDir string) (filename string, content []byte) {
	genCurrentTestFile = base
	genInitFileState(src)
	pkg := groupName(base)
	outFile := fmt.Sprintf("testgen/%s/%s_test.go", pkg, base)

	// Whole-file skips for TCL test files whose tests all exercise engine
	// features outside the current port phase (see skipTestFiles). The
	// generated test compiles and runs but contains no assertions.
	if reason, ok := skipTestFiles[base]; ok {
		return outFile, []byte(buildSkippedTestFile(pkg, base, reason))
	}

	// Parse TCL into commands
	cmds := parseCommands(src)

	// Pre-collect all variable names from the TCL source so we can pre-declare
	// them at function scope. This prevents "undefined" and "redeclared" errors
	// that arise from Go's block scoping (variables set inside if/for/foreach
	// blocks are not visible outside).
	setVars := collectSetVars(cmds)
	refVars := collectRefVars(src)
	// File-level set of TCL variables with an actual assignment site; braced
	// SQL $var substitution (tclsqlite's bind-if-defined semantics) must only
	// target these, never mere references.
	activeAssignedVars = activeAssignedVars[:0]
	for _, v := range setVars {
		if gv := tclVarToGo(v); gv != "" {
			activeAssignedVars = append(activeAssignedVars, gv)
		}
	}
	sqliteTargets := collectSqlite3Targets(cmds)
	knownGlobals := knownGlobalVars()
	incrOnly := collectIncrOnlyVars(cmds)
	constFuncs := collectConstFuncs(cmds)
	stringConstFuncs := collectStringConstFuncs(cmds)
	unzipDirs := collectUnzipDirs(cmds)
	identityFuncs := collectIdentityFuncs(cmds)
	lindexFuncs := collectLIndexFuncs(cmds)
	stringMapFuncs := collectStringMapFuncs(cmds)
	counterFuncs := collectCounterFuncs(cmds)
	incrRetFuncs := collectIncrRetFuncs(cmds)
	predFuncs := collectPredFuncs(cmds)
	errorFuncs := collectErrorFuncs(cmds)
	queryFuncs := collectQueryFuncs(cmds)
	specialFuncs := collectSpecialFuncs(cmds)
	rangeListFuncs := collectRangeListFuncs(cmds)
	arrayMapVars := collectArrayMapVars(cmds)

	// Merge: pre-declare all set variables + referenced-but-not-global variables
	preDeclared := collectPredeclaredVars(src, setVars, refVars, knownGlobals, sqliteTargets)

	// The prepare-tail variables (sqlite3_prepare's TAIL argument), as a set
	// for the transpiler to recognize tail-var comparisons.
	prepareTailSet := map[string]bool{}
	for _, tv := range collectPrepareTailVars(src) {
		prepareTailSet[tv] = true
	}

	// Build the Go source body first (to detect used imports)
	var body strings.Builder
	emitTestPreamble(&body, base, src, preDeclared)

	// Names already declared by the preamble (common vars, backup objects,
	// named connections, prepare-tail vars) must not be redeclared below.
	preambleDeclared := preambleDeclaredNames(src, preDeclared)

	genEmitPredeclared(&body, preDeclared, arrayMapVars, sqliteTargets, incrOnly, preambleDeclared)

	// Process top-level TCL commands
	initialVars := genBuildInitialVars(src, preDeclared)
	connPredeclared := genConnPredeclared(src)
	genResetFileState(arrayMapVars)
	tp := &transpiler{
		sb:                  &body,
		indent:              1,
		dbVar:               "db",
		t:                   "t",
		vars:                initialVars,
		currentTestFile:     base,
		dqsDDL:              true, // SQLite default: DQS allowed in DDL
		dqsDML:              true, // SQLite default: DQS allowed in DML
		testDir:             testDir,
		connPredeclared:     connPredeclared,
		constFuncs:          constFuncs,
		stringConstFuncs:    stringConstFuncs,
		unzipDirs:           unzipDirs,
		identityFuncs:       identityFuncs,
		lindexFuncs:         lindexFuncs,
		stringMapFuncs:      stringMapFuncs,
		counterFuncs:        counterFuncs,
		incrRetFuncs:        incrRetFuncs,
		predFuncs:           predFuncs,
		errorFuncs:          errorFuncs,
		queryFuncs:          queryFuncs,
		specialFuncs:        specialFuncs,
		rangeListFuncs:      rangeListFuncs,
		arrayMapVars:        arrayMapVars,
		quotaCallbacks:      collectQuotaCallbacks(cmds),
		collateGoFuncs:      collectCollateFuncs(cmds),
		collateEmittedProcs: make(map[string]string),
		collateDtorVars:     collectCollateDtorVars(cmds),
		prepareTailVars:     prepareTailSet,
	}
	tp.processCommands(cmds)

	// Detect which imports are actually used by the body (and the
	// package-level authorizer preamble, which references auth.Action/Result).
	genApplyFTS3ExprRepair(base, &body)

	// P9.PERF.T1: rewrite write-only string accumulators (only ever
	// appended/wholesale-assigned, never read) to strings.Builder — TCL's
	// append is amortized O(1) while `+=` is quadratic, which dominated the
	// speed-family packages' wall clock. Read-having vars are left
	// untouched, so packages without write-only accumulators regenerate
	// byte-identically.
	amortized := amortizeStringAppends(body.String())
	body.Reset()
	body.WriteString(amortized)

	importSrc := genImportSource(&body, tp)
	imports := detectImports(importSrc)

	// Build the full Go source with only needed imports
	var sb strings.Builder
	genFileHeader(&sb, pkg, imports)
	genAssemblePreambles(&sb, tp)

	// Append the body
	sb.WriteString(body.String())
	sb.WriteString("}\n")

	return outFile, []byte(sb.String())
}

// genInitFileState resets the per-file generator state: the package-level
// blob-channel bookkeeping, the FTS preamble builders, the tcl-eval set-var
// scan, the prepared-statement registry, and the hoisted pre-Open deletes.
//
// Leading `forcedelete test.db ...` commands (before the first do_test /
// explicit sqlite3 open) belong BEFORE the preamble Open: the TCL harness
// opens the connection when tester.tcl is sourced, after those deletes.
// Emit them pre-Open and consume them (genPreDeleted) so the original
// site does not delete the freshly opened file a second time
// (corrupt.test 1.1 wrote into a file deleted behind the open handle).
func genInitFileState(src string) {
	genBlobUsedChannels = make(map[string]bool)
	genBlobVarNames = make(map[string]bool)
	genFTSBuildPreamble = nil
	genFTS5TokenizePreamble = nil
	genTclEvalSetVars = scanTclEvalSetVars(src)
	resetPreparedState()
	genPreDeleted = map[string]int{}
	genPreDeletedList = sourceLeadingDeletes(src)
	for _, p := range genPreDeletedList {
		genPreDeleted[p]++
	}
}

// genEmitPredeclared emits the function-scope variable pre-declarations: the
// plain TCL variables (typed by their target kind), then the dynamic-key
// array maps (sorted so regeneration is byte-stable — map iteration is
// randomized in Go, and a nondeterministic order produced spurious
// whole-file diffs).
func genEmitPredeclared(body *strings.Builder, preDeclared []string, arrayMapVars, sqliteTargets, incrOnly, preambleDeclared map[string]bool) {
	for _, gv := range preDeclared {
		// Dynamic-key arrays are Go maps (declared in the preamble); skip the
		// plain string declaration for them (they are not in preDeclared as
		// plain vars — collectPredeclaredVars sees the base name and would
		// declare the base as a string; filter below).
		if arrayMapVars[gv] {
			continue
		}
		if preambleDeclared[gv] {
			continue
		}
		// Variables that are only incremented (never set to a value) start at
		// "0" in TCL (undefined == 0 for incr); others start as "".
		if sqliteTargets[gv] {
			body.WriteString(fmt.Sprintf("\tvar %s *frigolite.DB\n", gv))
		} else if incrOnly[gv] {
			body.WriteString(fmt.Sprintf("\tvar %s = \"0\"\n", gv))
		} else {
			body.WriteString(fmt.Sprintf("\tvar %s string\n", gv))
		}
		body.WriteString(fmt.Sprintf("\t_ = %s // pre-declared from TCL source\n", gv))
	}
	for _, base := range sortedMapBases(arrayMapVars) {
		gv := tclVarToGo(base)
		if gv == "" {
			continue
		}
		body.WriteString(fmt.Sprintf("\t%sMap := map[string]string{}\n", gv))
		body.WriteString(fmt.Sprintf("\t_ = %sMap // dynamic-key array from TCL source\n", gv))
	}
	if len(preDeclared) > 0 || len(arrayMapVars) > 0 {
		body.WriteString("\n")
	}
}

// sortedMapBases returns the dynamic-key array base names in sorted order.
func sortedMapBases(arrayMapVars map[string]bool) []string {
	mapBases := make([]string, 0, len(arrayMapVars))
	for base := range arrayMapVars {
		mapBases = append(mapBases, base)
	}
	sort.Strings(mapBases)
	return mapBases
}

// genBuildInitialVars builds the transpiler's initial variable scope:
// db/err (from db.Open), msg/r/_res (preamble), _r, db1-db9 (pre-declared DB
// connections), backup-object variables, incremental-blob channels, named
// sqlite3 connections, pre-declared TCL vars, and sqlite3_prepare tail vars.
func genBuildInitialVars(src string, preDeclared []string) []string {
	initialVars := []string{"db", "err", "msg", "r", "_res", "_r"}
	for i := 1; i <= 9; i++ {
		initialVars = append(initialVars, fmt.Sprintf("db%d", i))
	}
	// Backup-object variables (B, B2, ...) are pre-declared in the preamble.
	initialVars = append(initialVars, collectBackupNames(src)...)
	// Incremental-blob channel variables (incrblob_N) are pre-declared in the
	// preamble when the source uses blob I/O.
	if strings.Contains(src, "incrblob") || strings.Contains(src, "sqlite3_blob_") {
		for i := 1; i <= 64; i++ {
			initialVars = append(initialVars, fmt.Sprintf("incrblob_%d", i))
		}
	}
	// Named sqlite3 connection variables (sqlite3 tmp "") are pre-declared.
	for _, cn := range collectConnectionNames(src) {
		if cn == "db" || isPreDeclaredDB(cn) {
			continue
		}
		initialVars = append(initialVars, cn)
	}
	initialVars = append(initialVars, preDeclared...)
	// sqlite3_prepare tail variables are pre-declared in the preamble.
	initialVars = append(initialVars, collectPrepareTailVars(src)...)
	return initialVars
}

// genConnPredeclared returns the pre-declared connection names for the
// transpiler state (every named connection except db and the db1-db9
// placeholders).
func genConnPredeclared(src string) map[string]bool {
	connPredeclared := map[string]bool{}
	for _, cn := range collectConnectionNames(src) {
		if cn != "db" && !isPreDeclaredDB(cn) {
			connPredeclared[cn] = true
		}
	}
	return connPredeclared
}

// genResetFileState resets the per-file transpiler registries: registry-backed
// user procs, proc bodies, file-channel tracking, tclvar base marks, and proc
// var aliases — without a reset a channel flag leaked from an earlier package
// makes a later literal `open FOO w` channel emit its destination UNQUOTED
// (shell1's `tclChannelAppendAt(FOO, ...)` — an undefined Go identifier).
func genResetFileState(arrayMapVars map[string]bool) {
	globalUserProcs = map[string]bool{}
	globalProcBodies = map[string]string{}
	activeFileChannels = map[string]string{}
	activeFileChannelExprs = map[string]bool{}
	activeTclvarBases = map[string]bool{}
	tclProcVarAliases = map[string]string{}
	// The pre-pass dynamic-array registration collected in generateTestFile;
	// per-file like the rest of the state above.
	globalArrayMapVars = arrayMapVars
}

// genApplyFTS3ExprRepair repairs the generated sequence for the fts3expr/fts3cov
// corpus: fts3expr.test's section 6 re-CREATEs t1 (already created by section
// 4) assuming a per-section fresh database the TCL harness does not provide;
// repair by dropping the earlier table right before the conflicting CREATE.
func genApplyFTS3ExprRepair(base string, body *strings.Builder) {
	if base != "fts3expr" && base != "fts3cov" {
		return
	}
	var createStmt string
	var dropName string
	if base == "fts3expr" {
		createStmt = `_res = db.Exec("\n    CREATE VIRTUAL TABLE t1 USING fts3(a);\n  ")`
		dropName = "t1"
	} else {
		// fts3cov-11.1 re-CREATEs xx (created by section 9) assuming a
		// per-section fresh database the TCL harness does not provide.
		createStmt = `_res = db.Exec(" \n    CREATE VIRTUAL TABLE xx USING fts3;\n    INSERT INTO xx VALUES('one two three');\n    INSERT INTO xx VALUES('four five six');\n    DELETE FROM xx WHERE docid = 1;\n  ")`
		dropName = "xx"
	}
	// The generated source stores the SQL with literal backslash-n
	// escapes, so the search text below uses escaped backslashes.
	fixed := strings.Replace(body.String(),
		createStmt,
		`_ = db.Exec("DROP TABLE IF EXISTS `+dropName+`")`+"\n\t"+createStmt,
		1)
	body.Reset()
	body.WriteString(fixed)
}

// genImportSource concatenates the transpiled body with the package-level
// preamble builders so import detection sees every emitted reference.
func genImportSource(body *strings.Builder, tp *transpiler) string {
	importSrc := body.String()
	if tp.authPreamble != nil {
		importSrc += tp.authPreamble.String()
	}
	if tp.genesisPreamble != nil {
		importSrc += tp.genesisPreamble.String()
	}
	if genFTSBuildPreamble != nil {
		importSrc += genFTSBuildPreamble.String()
	}
	if genFTS5TokenizePreamble != nil {
		importSrc += genFTS5TokenizePreamble.String()
	}
	return importSrc
}

// genAssemblePreambles writes the package-level preamble blocks ahead of the
// test function (Go forbids method declarations inside a function).
func genAssemblePreambles(sb *strings.Builder, tp *transpiler) {
	// Package-level authorizer types (collected during processCommands).
	if tp.authPreamble != nil && tp.authPreamble.Len() > 0 {
		sb.WriteString(tp.authPreamble.String())
		sb.WriteString("\n")
	}

	// Package-level FTS Genesis data loader (fts_kjv_genesis helper), emitted
	// before the test function like the authorizer preamble.
	if tp.genesisPreamble != nil && tp.genesisPreamble.Len() > 0 {
		sb.WriteString(tp.genesisPreamble.String())
		sb.WriteString("\n")
	}

	// Package-level FTS data loaders (fts3BuildDB1/fts3BuildDB2 helpers),
	// emitted before the test function like the genesis loader. genFTSBuildPreamble
	// is a package-level var shared by every bodyTP copy, so it is read here
	// directly rather than through tp (a fts3_build_db_1/2 call may appear only
	// inside a do_test/foreach body whose sub-transpiler is discarded).
	if genFTSBuildPreamble != nil && genFTSBuildPreamble.Len() > 0 {
		sb.WriteString(genFTSBuildPreamble.String())
		sb.WriteString("\n")
	}

	// Package-level sqlite3_fts5_tokenize bridge (fts5TclTokenize helper),
	// emitted before the test function like the fts3 data loaders.
	// genFTS5TokenizePreamble is a package-level var shared by every bodyTP
	// copy, so it is read here directly (the tokenize call may appear only
	// inside a do_test/foreach body whose sub-transpiler is discarded).
	if genFTS5TokenizePreamble != nil && genFTS5TokenizePreamble.Len() > 0 {
		sb.WriteString(genFTS5TokenizePreamble.String())
		sb.WriteString("\n")
	}
}
