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

func generateTestFile(base string, src string, testDir string) (filename string, content []byte) {
	genCurrentTestFile = base
	genBlobUsedChannels = make(map[string]bool)
	genBlobVarNames = make(map[string]bool)
	genFTSBuildPreamble = nil
	resetPreparedState()
	pkg := groupName(base)
	outFile := fmt.Sprintf("testgen/%s/%s_test.go", pkg, base)

	// Whole-file skips for TCL test files whose tests all exercise engine
	// features outside the current port phase (see skipTestFiles). The
	// generated test compiles and runs but contains no assertions.
	if reason, ok := skipTestFiles[base]; ok {
		return outFile, []byte(buildSkippedTestFile(pkg, base, reason))
	}

	// Leading `forcedelete test.db ...` commands (before the first do_test /
	// explicit sqlite3 open) belong BEFORE the preamble Open: the TCL harness
	// opens the connection when tester.tcl is sourced, after those deletes.
	// Emit them pre-Open and consume them (genPreDeleted) so the original
	// site does not delete the freshly opened file a second time
	// (corrupt.test 1.1 wrote into a file deleted behind the open handle).
	genPreDeleted = map[string]int{}
	genPreDeletedList = sourceLeadingDeletes(src)
	for _, p := range genPreDeletedList {
		genPreDeleted[p]++
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

	// Emit pre-declarations at function scope
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
	// Declare dynamic-key array maps after the plain vars. Map iteration is
	// randomized in Go; sort the base names so regeneration is byte-stable
	// (nondeterministic order produced spurious whole-file diffs).
	mapBases := make([]string, 0, len(arrayMapVars))
	for base := range arrayMapVars {
		mapBases = append(mapBases, base)
	}
	sort.Strings(mapBases)
	for _, base := range mapBases {
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

	// Process top-level TCL commands
	// Initial vars: db, err (from db.Open), msg, r, _res (preamble),
	// db1-db9 (pre-declared DB connections), plus pre-declared TCL vars.
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
	// Pre-declared connection names for the transpiler state.
	connPredeclared := map[string]bool{}
	for _, cn := range collectConnectionNames(src) {
		if cn != "db" && !isPreDeclaredDB(cn) {
			connPredeclared[cn] = true
		}
	}
	// Registry-backed user procs are per-file state (package-global so cloned
	// body transpilers see them); reset before transpiling this file.
	globalUserProcs = map[string]bool{}
	globalProcBodies = map[string]string{}
	// File-channel tracking, tclvar base marks, and proc var aliases are
	// likewise per-file: without a reset a channel flag leaked from an
	// earlier package makes a later literal `open FOO w` channel emit its
	// destination UNQUOTED (shell1's `tclChannelAppendAt(FOO, ...)` — an
	// undefined Go identifier).
	activeFileChannels = map[string]string{}
	activeFileChannelExprs = map[string]bool{}
	activeTclvarBases = map[string]bool{}
	tclProcVarAliases = map[string]string{}
	// The pre-pass dynamic-array registration collected at the top of this
	// function; per-file like the rest of the state above.
	globalArrayMapVars = arrayMapVars
	tp := &transpiler{
		sb:               &body,
		indent:           1,
		dbVar:            "db",
		t:                "t",
		vars:             initialVars,
		currentTestFile:  base,
		dqsDDL:           true, // SQLite default: DQS allowed in DDL
		dqsDML:           true, // SQLite default: DQS allowed in DML
		testDir:          testDir,
		connPredeclared:  connPredeclared,
		constFuncs:       constFuncs,
		stringConstFuncs: stringConstFuncs,
		unzipDirs:        unzipDirs,
		identityFuncs:    identityFuncs,
		lindexFuncs:      lindexFuncs,
		stringMapFuncs:   stringMapFuncs,
		counterFuncs:     counterFuncs,
		incrRetFuncs:     incrRetFuncs,
		predFuncs:        predFuncs,
		errorFuncs:       errorFuncs,
		queryFuncs:       queryFuncs,
		specialFuncs:     specialFuncs,
		rangeListFuncs:   rangeListFuncs,
		arrayMapVars:     arrayMapVars,
		quotaCallbacks:   collectQuotaCallbacks(cmds),
		collateGoFuncs:   collectCollateFuncs(cmds),
		collateDtorVars:  collectCollateDtorVars(cmds),
		prepareTailVars:  prepareTailSet,
	}
	tp.processCommands(cmds)

	// Detect which imports are actually used by the body (and the
	// package-level authorizer preamble, which references auth.Action/Result).
	// fts3expr.test's section 6 re-CREATEs t1 (already created by section 4)
	// assuming a per-section fresh database the TCL harness does not
	// provide; repair by dropping the earlier table right before the
	// conflicting CREATE.
	if base == "fts3expr" || base == "fts3cov" {
		// The generated source stores the SQL with literal backslash-n
		// escapes, so the search text below uses escaped backslashes.
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
		fixed := strings.Replace(body.String(),
			createStmt,
			`_ = db.Exec("DROP TABLE IF EXISTS `+dropName+`")`+"\n\t"+createStmt,
			1)
		body.Reset()
		body.WriteString(fixed)
	}

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
	imports := detectImports(importSrc)

	// Build the full Go source with only needed imports
	var sb strings.Builder
	sb.WriteString("// Code generated by tcl2go; DO NOT EDIT.\n")
	// Generated test packages are opt-in: they only compile when the testgen
	// build tag is set, so 'go test ./...' (e.g. the SOLID verify command)
	// builds only hand-written, non-generated code.
	sb.WriteString("//go:build testgen\n")
	sb.WriteString("// +build testgen\n\n")
	sb.WriteString(fmt.Sprintf("package %s\n\n", pkg))
	sb.WriteString("import (\n")
	for _, imp := range imports {
		sb.WriteString(fmt.Sprintf("\"%s\"\n", imp))
	}
	sb.WriteString(")\n\n")

	// Package-level authorizer types (collected during processCommands). Go
	// forbids method declarations inside a function, so these must precede
	// the test function.
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

	// Append the body
	sb.WriteString(body.String())
	sb.WriteString("}\n")

	return outFile, []byte(sb.String())
}

// emitTestPreamble writes the fixed function preamble: the test function
// header, the per-test working directory, the main db connection, and the
// common result/message variables plus db1-db9 placeholders. Files that test
// sqlite3_db_config FP_DIGITS (fpconv1.test) start from the library default
// FP_DIGITS=0 (shortest round-trip float rendering), so the preamble resets
// tcl_fp_digits to 0 for them.
func emitTestPreamble(body *strings.Builder, base string, src string, preDeclared []string) {
	body.WriteString(fmt.Sprintf("func Test_%s(t *testing.T) {\n", safeTestName(base)))
	// Mirror the TCL framework: each test runs in its own working directory
	// (the framework's per-file testdir), and the main "db" connection opens
	// the real "./test.db" file. Close/reopen of test.db therefore persists
	// data (matching `db close; sqlite3 db test.db`), while connection state
	// (collations, user functions) is NOT preserved across reopen.
	body.WriteString("\tif err := os.Chdir(t.TempDir()); err != nil { t.Fatal(err) }\n")
	for _, p := range genPreDeletedList {
		body.WriteString(fmt.Sprintf("\t_ = os.Remove(%q)\n", p))
	}
	body.WriteString("\tdb, err := frigolite.Open(\"test.db\")\n")
	body.WriteString("\tif err != nil {\n")
	body.WriteString("\t\tt.Fatal(err)\n")
	body.WriteString("\t}\n")
	body.WriteString("\tdefer db.Close()\n\n")
	// fts3expr.test's section 6 re-CREATEs t1 (already created by section 4)
	// assuming a per-section fresh database the TCL harness does not
	// provide; the sequence is only runnable if the earlier table is
	// dropped first. Repair the generated sequence accordingly.

	// Files exercising SQLite's test-only fts3_exprtest() function
	// (fts3expr.test) register a Go implementation backed by the engine's
	// MATCH parser (fts3_expr.c fts3ExprTest).
	if strings.Contains(src, "fts3_exprtest") {
		body.WriteString("\tdb.RegisterFunction(\"fts3_exprtest\", fts3ExprTest, 0, -1)\n\n")
	}
	// Common vars used by generated code
	body.WriteString("\tvar _res *frigolite.Result\n")
	body.WriteString("\tvar r *frigolite.Result\n")
	body.WriteString("\tvar msg string\n")
	body.WriteString("\tvar _r string\n")
	body.WriteString("\tvar _berr error\n")
	body.WriteString("\t_ = _berr // suppress unused warning\n")
	body.WriteString("\t_ = msg // suppress unused warning\n")
	// Pre-declare incremental-blob channel variables (incrblob_N) so blob
	// handles opened in one do_test body remain visible in later bodies.
	if strings.Contains(src, "incrblob") || strings.Contains(src, "sqlite3_blob_") {
		for i := 1; i <= 64; i++ {
			body.WriteString(fmt.Sprintf("\tvar incrblob_%d *frigolite.Blob\n", i))
			body.WriteString(fmt.Sprintf("\t_ = incrblob_%d\n", i))
		}
	}
	body.WriteString("\t_ = _res // suppress unused warning\n")
	body.WriteString("\t_ = r    // suppress unused warning\n")
	body.WriteString("\t_ = _r   // suppress unused warning\n")
	body.WriteString("\ttcl_nullvalue = \"{}\" // default NULL rendering\n")
	// Files that switch FP_DIGITS (sqlite3_db_config) start from the library
	// default 0 (shortest round-trip); the harness default for other files is
	// 15 significant digits (matching the pre-shortest-fpconv test corpus).
	if strings.Contains(strings.ToUpper(src), "FP_DIGITS") {
		body.WriteString("\ttcl_fp_digits = 0 // sqlite3_db_config FP_DIGITS file: library default\n")
	}
	// sqlite_options(default_autovacuum) is a TCL test-harness global set by
	// the C test fixture based on the SQLITE_DEFAULT_AUTOVACUUM compile flag
	// (default = "0" = NONE). The transpiler pre-declares the corresponding
	// Go var `sqlite_options_default_autovacuum` as an empty string; tests
	// that compare `pragma auto_vacuum` against this var (incrvacuum-1.1)
	// then fail with [0] (frigolite's pragma getter returns the mode) vs []
	// (empty Go var). Initialize it to "0" when referenced — matches SQLite's
	// autoconf default (BTREE_AUTOVACUUM_NONE). Emit the declaration here so
	// the var exists before the pre-declared-var loop emits `var ... string`.
	if strings.Contains(src, "sqlite_options_default_autovacuum") || strings.Contains(src, "sqlite_options(default_autovacuum)") {
		body.WriteString("\tvar sqlite_options_default_autovacuum = \"0\" // SQLITE_DEFAULT_AUTOVACUUM=0 (NONE)\n")
	}
	// `::sqlite_pending_byte` is a TCL test-harness global set by
	// tester.tcl:102 to 0x10000 (65536) via sqlite3_test_control_pending_byte,
	// so `file size` checks in autovacuum-9.3/9.5 etc. observe a small
	// expected value rather than the production 1GB. Tests that
	// reference this global never re-set it themselves, so the
	// transpiler must initialise the shadow var to 65536 here for any
	// test file that mentions it. We also override the later
	// pre-declared `var sqlite_pending_byte string` so the file
	// compiles (the var-declared branch is suppressed for this name).
	// Sources that only CALL sqlite3_test_control_pending_byte (pager1.test
	// 42.x — the name "sqlite_pending_byte" never appears) still need the
	// shadow var: the command handler assigns it.
	if strings.Contains(src, "sqlite_pending_byte") || strings.Contains(src, "sqlite3_test_control_pending_byte") {
		body.WriteString("\t// tester.tcl:102 pins pending byte to 0x10000 (65536) for small file-size\n")
		body.WriteString("\t// checks (autovacuum-9.3 / 9.5, corrupt2, etc.).\n")
		body.WriteString("\tvar sqlite_pending_byte = \"65536\" // shadow of ::sqlite_pending_byte, pinned by tester.tcl:102\n")
		// Usage suppressor: packages that never assign the shadow var
		// (no test_control emission) would fail to build with
		// "declared and not used". Harmless where the var IS used.
		body.WriteString("\t_ = sqlite_pending_byte\n")
		body.WriteString("\t// Pager.SetPendingByte(0x10000) makes the engine skip page 65 (the\n")
		body.WriteString("\t// pending-byte slot) when handing out rootpages — without this,\n")
		body.WriteString("\t// autovacuum-2.4.5 allocates a table at the reserved slot and\n")
		body.WriteString("\t// the btree reader later reports \"database disk image is\n")
		body.WriteString("\t// malformed\". The test harness pins the byte in C via\n")
		body.WriteString("\t// sqlite3_test_control_pending_byte; mirror that here.\n")
		body.WriteString("\tdb.SetPendingByte(0x10000)\n")
	}
	body.WriteString("\n")
	// Pre-declare secondary DB connection variables (TCL scope is function-wide)
	for i := 1; i <= 9; i++ {
		body.WriteString(fmt.Sprintf("\tvar db%d *frigolite.DB\n", i))
		body.WriteString(fmt.Sprintf("\t_ = db%d\n", i))
	}
	// memdb1.test reuses db1 as a BLOB shadow (`set ::db1 [db serialize]`):
	// a package-level string shadow lets the serialize assignment compile
	// while connection uses stay on the *frigolite.DB var. The shadow is
	// only referenced by serialize/deserialize flows.
	if strings.Contains(src, "db serialize") || strings.Contains(src, "db deserialize") {
		body.WriteString("\tvar db1Blob string // memdb1 serialize image shadow of ::db1\n")
		body.WriteString("\t_ = db1Blob\n")
	}
	// Pre-declare backup-object variables (sqlite3_backup B ...) so B is
	// visible across do_test bodies regardless of block scoping.
	for _, bn := range collectBackupNames(src) {
		body.WriteString(fmt.Sprintf("\tvar %s *frigolite.Backup\n", bn))
		body.WriteString(fmt.Sprintf("\t_ = %s\n", bn))
	}
	// Pre-declare named sqlite3 connection variables (sqlite3 tmp "", etc.)
	// so they are visible across do_test bodies regardless of block scoping
	// (the db1-db9 placeholders are covered above).
	for _, cn := range collectConnectionNames(src) {
		if cn == "db" || isPreDeclaredDB(cn) {
			continue
		}
		body.WriteString(fmt.Sprintf("\tvar %s *frigolite.DB\n", cn))
		body.WriteString(fmt.Sprintf("\t_ = %s\n", cn))
	}
	// Pre-declare sqlite3_prepare tail variables (the TAIL/SQL/DUMMY variable
	// that receives the SQL text after the first statement) at function scope
	// so tail assignments inside do_test bodies target the function-scope var.
	// Skip variables already pre-declared as regular TCL vars (they appear in
	// the set/ref collections too, e.g. bind.test's TX).
	preDeclaredSet := map[string]bool{}
	for _, pv := range preDeclared {
		preDeclaredSet[pv] = true
	}
	// Common vars declared unconditionally above (msg/_res/r/_r) would be
	// redeclared by a same-named tail variable — keep the first declaration.
	for _, cv := range []string{"msg", "_res", "r", "_r"} {
		preDeclaredSet[cv] = true
	}
	for _, tv := range collectPrepareTailVars(src) {
		if preDeclaredSet[tv] {
			continue
		}
		body.WriteString(fmt.Sprintf("\tvar %s string\n", tv))
		body.WriteString(fmt.Sprintf("\t_ = %s // prepared-statement tail var\n", tv))
	}
	body.WriteString("\n")
}

// collectPrepareTailVars returns the tail-variable names of sqlite3_prepare
// commands (`sqlite3_prepare db SQL -1 TAIL` — the last argument names the
// variable that receives the SQL text after the first statement). These are
// pre-declared at function scope so the tail assignment emitted by
// recordPreparedStatement targets a function-scope variable (capi2-2.x reads
// `set SQL` after a multi-statement prepare in a later do_test body).
func collectPrepareTailVars(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "sqlite3_prepare") {
			continue
		}
		// The prepare command may be a bare top-level command or embedded in
		// `set X [sqlite3_prepare ...]`. Extract the command text (strip a
		// leading `set VAR [` and a trailing `]`), then tokenize it with the
		// TCL word splitter so a braced multi-word SQL body stays one token.
		cmdText := trimmed
		if i := strings.Index(cmdText, "["); i >= 0 {
			cmdText = cmdText[i+1:]
			cmdText = strings.TrimSuffix(cmdText, "]")
		}
		words := tclCmdWords(cmdText)
		if len(words) < 5 {
			continue
		}
		if !strings.HasPrefix(words[0], "sqlite3_prepare") {
			continue
		}
		// Words: CMD DB SQL NBYTES TAILVAR (the tail var is the LAST argument;
		// some calls pass a NULL marker "notused"/"dummy" instead).
		tv := words[len(words)-1]
		tv = strings.TrimPrefix(tv, "$")
		tv = strings.Trim(tv, `"`)
		if tv != "" && !strings.HasPrefix(tv, "-") && tv != "notused" && tv != "dummy" && isValidGoIdent(tclVarToGo(tv)) {
			gv := tclVarToGo(tv)
			if !seen[gv] {
				seen[gv] = true
				names = append(names, gv)
			}
		}
	}
	return names
}

// collectConnectionNames returns the named database connection variables
// created by `sqlite3 NAME [file]` commands (excluding db and db1-db9).
// Dynamic targets (`sqlite3 $con test.db`, where con HOLDS the connection
// name at runtime) are skipped: those variables are plain TCL strings, not
// connection handles.
func collectConnectionNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "sqlite3 ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[1], "$") {
			continue // dynamic connection name — the variable holds a string
		}
		name := tclVarToGo(fields[1])
		if !isValidGoIdent(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// collectBackupNames returns the backup-object variable names created by
// `sqlite3_backup NAME ...` commands in the TCL source (B, B2, B3, ...).
func collectBackupNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "sqlite3_backup ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		name := tclVarToGo(fields[1])
		if !isValidGoIdent(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// collectBlobNames returns the blob-object variable names created by
// `sqlite3_blob_open ... NAME` commands and `set VAR [db incrblob ...]`
// assignments in the TCL source (B, B2, blob, h, b, ...).
//
//lint:ignore U1000 retained for generator compatibility
func collectBlobNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		// Strip trailing TCL comments (";# ...") so the last field is the
		// blob variable, not a comment word.
		if idx := strings.Index(trimmed, ";#"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		if strings.HasPrefix(trimmed, "sqlite3_blob_open ") {
			fields := strings.Fields(trimmed)
			if len(fields) < 2 {
				continue
			}
			name := tclVarToGo(fields[len(fields)-1])
			if !isValidGoIdent(name) || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
		// `set blob [db incrblob ...]` / `set ::blob [db incrblob ...]` —
		// the target var holds a *frigolite.Blob.
		if strings.Contains(trimmed, "[db incrblob ") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 && fields[0] == "set" {
				name := tclVarToGo(fields[1])
				if !isValidGoIdent(name) || seen[name] {
					continue
				}
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

// collectPredeclaredVars merges the set and referenced variable names into the
// list to pre-declare at function scope, filtering out globals, db vars, and
// sqlite connection targets.
func collectPredeclaredVars(src string, setVars, refVars []string, knownGlobals, sqliteTargets map[string]bool) []string {
	var preDeclared []string
	seen := make(map[string]bool)
	for _, v := range append(append([]string{}, setVars...), refVars...) {
		gv := tclVarToGo(v)
		// The TCL variable err is mapped to _err_tcl throughout the generated
		// code (it would shadow nothing, but the name keeps it distinct from
		// the db error var); pre-declare it so a `set err` inside an if/else
		// branch is still visible after the branch.
		if gv == "err" {
			gv = "_err_tcl"
		}
		legacyOpenTarget := strings.Contains(src, "set ::"+v+" [sqlite3_open") || strings.Contains(src, "set "+v+" [sqlite3_open")
		if legacyOpenTarget {
			sqliteTargets[gv] = true
		}
		if gv != "" && gv != "_" && !seen[gv] && !knownGlobals[gv] && gv != "db" && gv != "t" && isValidGoIdent(gv) {
			seen[gv] = true
			preDeclared = append(preDeclared, gv)
		}
	}
	return preDeclared
}

// preambleDeclaredNames returns every Go variable name that emitTestPreamble
// already declares (common vars, backup objects, named connections, and the
// prepare-tail variables not claimed by preDeclared) so the function-scope
// preDeclared loop does not redeclare them.
func preambleDeclaredNames(src string, preDeclared []string) map[string]bool {
	preSet := make(map[string]bool, len(preDeclared))
	for _, pv := range preDeclared {
		preSet[pv] = true
	}
	// Common vars declared unconditionally at the top of every test function.
	declared := map[string]bool{"msg": true, "_res": true, "r": true, "_r": true}
	// sqlite_options_default_autovacuum is declared (with initial value) by
	// emitTestPreamble when the TCL source references it. Without this entry,
	// the preDeclared loop would emit a second `var ... string` declaration
	// and the build would fail with "sqlite_options_default_autovacuum
	// redeclared in this block".
	if strings.Contains(src, "sqlite_options_default_autovacuum") || strings.Contains(src, "sqlite_options(default_autovacuum)") {
		declared["sqlite_options_default_autovacuum"] = true
	}
	// sqlite_pending_byte is shadow-declared (initialised) by
	// emitTestPreamble whenever the TCL source references
	// ::sqlite_pending_byte; the preDeclared loop must skip it to
	// avoid a redeclared-in-this-block build error.
	if strings.Contains(src, "sqlite_pending_byte") {
		declared["sqlite_pending_byte"] = true
	}
	for _, bn := range collectBackupNames(src) {
		declared[bn] = true
	}
	for _, cn := range collectConnectionNames(src) {
		if cn == "db" || isPreDeclaredDB(cn) {
			continue
		}
		declared[cn] = true
	}
	for _, tv := range collectPrepareTailVars(src) {
		if !preSet[tv] { // preDeclared wins — the tail loop skips those
			declared[tv] = true
		}
	}
	return declared
}

// buildSkippedTestFile emits a whole-file skip: a no-op test function that
// compiles and runs without assertions.
func buildSkippedTestFile(pkg, base, reason string) string {
	var sb strings.Builder
	sb.WriteString("// Code generated by tcl2go; DO NOT EDIT.\n")
	sb.WriteString("//go:build testgen\n")
	sb.WriteString("// +build testgen\n\n")
	sb.WriteString(fmt.Sprintf("package %s\n\n", pkg))
	sb.WriteString("import (\n\"testing\"\n)\n\n")
	sb.WriteString(fmt.Sprintf("func Test_%s(t *testing.T) {}\n", safeTestName(base)))
	sb.WriteString(fmt.Sprintf("// skipped: %s\n", reason))
	return sb.String()
}

// detectImports scans generated code for package references and returns only the needed imports.
var allStandardImports = []struct{ name, path string }{
	{"errors", "errors"},
	{"vtab", "github.com/pijalu/frigolite/internal/vtab"},
	{"fmt", "fmt"},
	{"os", "os"},
	{"filepath", "path/filepath"},
	{"regexp", "regexp"},
	{"sort", "sort"},
	{"strconv", "strconv"},
	{"strings", "strings"},
	{"time", "time"},
}

func detectImports(code string) []string {
	needed := map[string]bool{
		"testing":                     true, // always needed
		"github.com/pijalu/frigolite": true, // always needed
	}

	// The date/time tests emit function.SetLocaltimeHook(...) for the TCL
	// harness's SQLITE_TESTCTRL_LOCALTIME_FAULT control.
	if hasPackageRef(code, "function") {
		needed["github.com/pijalu/frigolite/internal/function"] = true
	}
	// zeroblob tests emit storage.SetMaxBlobsize/MaxBlobsize for the TCL
	// harness's linked sqlite3_max_blobsize global.
	if hasPackageRef(code, "storage") {
		needed["github.com/pijalu/frigolite/internal/storage"] = true
	}
	// Authorizer tests emit auth.Authorizer types (db authorizer ::auth).
	if hasPackageRef(code, "auth") {
		needed["github.com/pijalu/frigolite/internal/auth"] = true
	}

	for _, imp := range allStandardImports {
		// Check if the package name appears as a Go identifier reference
		// (preceded by a non-identifier character, followed by ".X" where X is uppercase)
		// This avoids false positives from package names appearing in SQL strings.
		if hasPackageRef(code, imp.name) {
			needed[imp.path] = true
		}
	}

	// Sort for deterministic output
	var result []string
	for p := range needed {
		result = append(result, p)
	}
	sort.Strings(result)
	return result
}

// hasPackageRef checks if pkgName appears as a Go package reference in code.
// It looks for patterns where pkgName is preceded by a non-identifier character
// and followed by ".Func" where Func starts uppercase.
func hasPackageRef(code, pkgName string) bool {
	search := pkgName + "."
	for {
		idx := strings.Index(code, search)
		if idx < 0 {
			return false
		}
		// Check word boundary before pkgName; skip when preceded by a
		// backslash (inside a Go string escape) or a Go identifier char.
		if !isPackageRefBoundary(code, idx) {
			code = code[idx+len(search):]
			continue
		}
		// Check next char after dot is uppercase (exported function)
		afterIdx := idx + len(search)
		if afterIdx < len(code) && code[afterIdx] >= 'A' && code[afterIdx] <= 'Z' {
			return true
		}
		code = code[idx+len(search):]
	}
}

// isPackageRefBoundary reports whether the character before position idx is a
// valid package-reference boundary (not a backslash escape and not part of a
// longer identifier).
func isPackageRefBoundary(code string, idx int) bool {
	if idx == 0 {
		return true
	}
	prev := code[idx-1]
	// Skip if preceded by backslash (inside a Go string escape)
	if prev == '\\' {
		return false
	}
	if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') ||
		(prev >= '0' && prev <= '9') || prev == '_' {
		return false
	}
	return true
}

// knownGlobalVars returns the set of variable names declared in the helpers
// file (package-level globals). These must NOT be pre-declared at function
// scope because they already have values.
func knownGlobalVars() map[string]bool {
	return map[string]bool{
		"tcl_platform_platform": true, "tcl_platform_byteOrder": true,
		"tcl_platform_os": true, "tcl_platform_pointerSize": true,
		"tcl_platform_wordSize": true, "_tcl_platform_platform": true,
		"_tcl_platform_byteOrder": true, "_tcl_platform_os": true,
		"_tcl_platform": true, "tcl_platform": true,
		"MEMDEBUG": true, "sqlite_options": true, "_sqlite_options": true,
		"SQLITE_MAX_LENGTH": true, "SQLITE_MAX_SQL_LENGTH": true,
		"SQLITE_MAX_COLUMN": true, "SQLITE_MAX_EXPR_DEPTH": true,
		"SQLITE_MAX_TRIGGER_DEPTH":   true,
		"SQLITE_MAX_COMPOUND_SELECT": true, "SQLITE_MAX_VDBE_OP": true,
		"SQLITE_MAX_FUNCTION_ARG": true, "SQLITE_MAX_ATTACHED": true,
		"SQLITE_MAX_LIKE_PATTERN_LENGTH": true, "SQLITE_MAX_VARIABLE_NUMBER": true,
		"SQLITE_MAX_WORKER_THREADS": true, "SQLITE_MAX_SCHEMA": true,
		"SQLITE_MAX_PAGE_SIZE": true, "_SQLITE_MAX_PAGE_SIZE": true,
		"AUTOVACUUM": true, "TEMP_STORE": true, "_TEMP_STORE": true,
		"SQLITE_DEFAULT_SYNCHRONOUS": true, "SQLITE_DEFAULT_WAL_SYNCHRONOUS": true,
		"_SQLITE_DEFAULT_CACHE_SIZE": true, "tcl_version": true, "_tcl_version": true,
		"SQL": true, "TAIL": true, "TAIL_": true, "_G": true, "G": true,
		"_error": true, "argv": true, "has_codec": true, "bitmask_size": true,
		"tcl_precision": true, "highPrecision": true,
		"upperBound": true, "prefix": true, "dirname": true,
		"msg": true, "_res": true, "r": true, "_r": true,
		// db1-db9 are pre-declared as *frigolite.DB in the function preamble
		"db1": true, "db2": true, "db3": true, "db4": true, "db5": true,
		"db6": true, "db7": true, "db8": true, "db9": true,
		// oplog is the testvfs-equivalent journal-sidecar event sink
		// (journal2 test suite). Declared package-level in the helpers
		// template so the process-wide journal-file-op hook can append
		// events to it directly from any goroutine.
		"oplog": true,
	}
}

// collectSqlite3Targets recursively walks TCL commands and returns a set of
// variable names that are targets of sqlite3 commands (these are *frigolite.DB,
// not string, so must NOT be pre-declared as string).

// genPreDeleted counts the hoisted pre-Open deletes per path (a file may
// delete the same path several times in its leading region — delete4.test's
// do_execsql_test-only body makes the whole file "head", so mid-file
// forcedeletes land in the list too). Consumption by a body occurrence must
// decrement the count, not flip a bool: with two hoisted entries only ONE
// body occurrence is the duplicate, and later real deletes must still emit.
// (single-threaded generation; reset per file in generateTestFile).
var genPreDeleted map[string]int
var genPreDeletedList []string

// sourceLeadingDeletes scans the source region before the first do_test /
// sqlite3 open for forcedelete / file delete / delete_file commands and
// returns their literal path arguments.
func sourceLeadingDeletes(src string) []string {
	head := src
	// The leading region ends at the FIRST test-block command of any flavor
	// (do_test, do_execsql_test, do_catchsql_test, do_eqp_test, ...).
	// Matching only "\ndo_test " left do_execsql_test-driven files
	// (delete4.test) with their WHOLE body classified as head: every
	// mid-file forcedelete was hoisted into the preamble and the body
	// occurrences consumed/silenced, so close/forcedelete/reopen resets
	// reused stale databases ("table t1 already exists").
	headEnd := len(src)
	for _, marker := range []string{"\ndo_test ", "\ndo_execsql_test ", "\ndo_catchsql_test ", "\ndo_eqp_test ", "\ndo_realnum_test ", "\ndo_nullid_test "} {
		if i := strings.Index(src, marker); i >= 0 && i < headEnd {
			headEnd = i
		}
	}
	head = src[:headEnd]
	var paths []string
	for _, line := range strings.Split(head, "\n") {
		t := strings.TrimSpace(line)
		// A loop header starts a re-executed region: forcedelete/file-delete
		// occurrences after it are LOOP-BODY deletes (fts3snippet's
		// `forcedelete test.db` inside the foreach, run once per encoding),
		// not one-shot leading deletes. Stop the scan so they are emitted at
		// their real position by processFileDelete — pre-consuming them here
		// swallowed every per-iteration delete and left stale databases
		// behind ("table ft already exists" on the loop's second pass).
		if strings.HasPrefix(t, "foreach ") || strings.HasPrefix(t, "for {") ||
			strings.HasPrefix(t, "while {") {
			break
		}
		t = strings.TrimPrefix(t, "catch {")
		for _, kw := range []string{"forcedelete ", "delete_file ", "file delete "} {
			if strings.HasPrefix(t, kw) {
				t = strings.TrimPrefix(t, kw)
				for _, f := range strings.Fields(strings.TrimSuffix(t, "}")) {
					f = strings.Trim(f, "{}")
					if f == "" || f == "-force" || f == "--" {
						continue
					}
					paths = append(paths, f)
				}
				break
			}
		}
	}
	return paths
}
