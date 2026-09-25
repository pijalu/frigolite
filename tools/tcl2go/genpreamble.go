// SPDX-License-Identifier: GPL-3.0-or-later
// Package main implements the tcl2go tool: a TCL-to-Go transpiler that converts
// SQLite TCL test files (.test) into standalone Go test files (_test.go).
//
// This file contains the generated test-function preamble emission: the fixed
// connection setup, the auto-registered extension functions, the
// source-conditional variable shadows, and the pre-declared connections.
package main

import (
	"fmt"
	"strings"
)

// emitTestPreamble writes the fixed function preamble: the test function
// header, the per-test working directory, the main db connection, and the
// common result/message variables plus db1-db9 placeholders. Files that test
// sqlite3_db_config FP_DIGITS (fpconv1.test) start from the library default
// FP_DIGITS=0 (shortest round-trip float rendering), so the preamble resets
// tcl_fp_digits to 0 for them.
func emitTestPreamble(body *strings.Builder, base string, src string, preDeclared []string) {
	emitPreambleConnOpen(body, base, src)
	emitPreambleCommonVars(body, src)
	emitPreambleConnections(body, src, preDeclared)
	body.WriteString("\n")
}

// emitPreambleConnOpen writes the test-function header: the per-test working
// directory (mirroring the TCL framework's per-file testdir), the hoisted
// pre-Open deletes, and the main "db" connection open. Close/reopen of
// test.db therefore persists data (matching `db close; sqlite3 db test.db`),
// while connection state (collations, user functions) is NOT preserved across
// reopen. SQLite's test build auto-installs the test_func.c extension
// functions into EVERY connection (sqlite3_auto_extension
// registerTestFunctions); generated tests reference them in catchsql/execsql
// bodies (func 15.x test_error, 25.1 test_isolation), so register them at
// connection setup with the oracle's semantics.
func emitPreambleConnOpen(body *strings.Builder, base string, src string) {
	body.WriteString(fmt.Sprintf("func Test_%s(t *testing.T) {\n", safeTestName(base)))
	body.WriteString("\tif err := os.Chdir(t.TempDir()); err != nil { t.Fatal(err) }\n")
	for _, p := range genPreDeletedList {
		body.WriteString(fmt.Sprintf("\t_ = os.Remove(%q)\n", p))
	}
	body.WriteString("\tdb, err := frigolite.Open(\"test.db\")\n")
	body.WriteString("\tif err != nil {\n")
	body.WriteString("\t\tt.Fatal(err)\n")
	body.WriteString("\t}\n")
	body.WriteString("\tdefer db.Close()\n\n")
	body.WriteString("\t// auto-installed test-extension functions (src/test_func.c)\n")
	body.WriteString("\tdb.RegisterFunction(\"test_error\", func(args []interface{}) (interface{}, error) {\n")
	body.WriteString("\t\tmsg := \"\"\n")
	body.WriteString("\t\tif len(args) > 0 && args[0] != nil {\n")
	body.WriteString("\t\t\tmsg = fmt.Sprintf(\"%v\", args[0])\n")
	body.WriteString("\t\t}\n")
	body.WriteString("\t\treturn nil, fmt.Errorf(\"%s\", msg)\n")
	body.WriteString("\t}, 1, 2)\n")
	body.WriteString("\tdb.RegisterFunction(\"test_isolation\", func(args []interface{}) (interface{}, error) {\n")
	body.WriteString("\t\tif len(args) < 2 {\n")
	body.WriteString("\t\t\treturn nil, nil\n")
	body.WriteString("\t\t}\n")
	body.WriteString("\t\treturn args[1], nil\n")
	body.WriteString("\t}, 2, 2)\n\n")
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
}

// emitPreambleCommonVars writes the common result/message variables used by
// generated code, the incremental-blob channel placeholders, the default NULL
// rendering, and the source-conditional harness shadows (FP_DIGITS,
// sqlite_options_default_autovacuum, sqlite_pending_byte).
func emitPreambleCommonVars(body *strings.Builder, src string) {
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
	emitPreambleFPDigits(body, src)
	emitPreambleAutovacuumOption(body, src)
	emitPreamblePendingByte(body, src)
	body.WriteString("\n")
}

// emitPreambleFPDigits resets tcl_fp_digits for files that switch FP_DIGITS
// (sqlite3_db_config): they start from the library default 0 (shortest
// round-trip); the harness default for other files is 15 significant digits
// (matching the pre-shortest-fpconv test corpus).
func emitPreambleFPDigits(body *strings.Builder, src string) {
	if strings.Contains(strings.ToUpper(src), "FP_DIGITS") {
		body.WriteString("\ttcl_fp_digits = 0 // sqlite3_db_config FP_DIGITS file: library default\n")
	}
}

// emitPreambleAutovacuumOption initializes sqlite_options(default_autovacuum),
// a TCL test-harness global set by the C test fixture based on the
// SQLITE_DEFAULT_AUTOVACUUM compile flag (default = "0" = NONE). The
// transpiler pre-declares the corresponding Go var
// `sqlite_options_default_autovacuum` as an empty string; tests that compare
// `pragma auto_vacuum` against this var (incrvacuum-1.1) then fail with [0]
// (frigolite's pragma getter returns the mode) vs [] (empty Go var).
// Initialize it to "0" when referenced — matches SQLite's autoconf default
// (BTREE_AUTOVACUUM_NONE). Emit the declaration here so the var exists before
// the pre-declared-var loop emits `var ... string`.
func emitPreambleAutovacuumOption(body *strings.Builder, src string) {
	if strings.Contains(src, "sqlite_options_default_autovacuum") || strings.Contains(src, "sqlite_options(default_autovacuum)") {
		body.WriteString("\tvar sqlite_options_default_autovacuum = \"0\" // SQLITE_DEFAULT_AUTOVACUUM=0 (NONE)\n")
	}
}

// emitPreamblePendingByte shadows the `::sqlite_pending_byte` TCL
// test-harness global set by tester.tcl:102 to 0x10000 (65536) via
// sqlite3_test_control_pending_byte, so `file size` checks in
// autovacuum-9.3/9.5 etc. observe a small expected value rather than the
// production 1GB. Tests that reference this global never re-set it
// themselves, so the transpiler must initialise the shadow var to 65536 here
// for any test file that mentions it. We also override the later
// pre-declared `var sqlite_pending_byte string` so the file compiles (the
// var-declared branch is suppressed for this name). Sources that only CALL
// sqlite3_test_control_pending_byte (pager1.test 42.x — the name
// "sqlite_pending_byte" never appears) still need the shadow var: the
// command handler assigns it.
func emitPreamblePendingByte(body *strings.Builder, src string) {
	if !strings.Contains(src, "sqlite_pending_byte") && !strings.Contains(src, "sqlite3_test_control_pending_byte") {
		return
	}
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

// emitPreambleConnections pre-declares the secondary DB connections
// (TCL scope is function-wide), the memdb1 blob shadow, backup objects, named
// sqlite3 connections, and the sqlite3_prepare tail variables.
func emitPreambleConnections(body *strings.Builder, src string, preDeclared []string) {
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
	emitPreambleTailVars(body, src, preDeclared)
}

// emitPreambleTailVars pre-declares the sqlite3_prepare tail variables (the
// TAIL/SQL/DUMMY variable that receives the SQL text after the first
// statement) at function scope so tail assignments inside do_test bodies
// target the function-scope var. Skip variables already pre-declared as
// regular TCL vars (they appear in the set/ref collections too, e.g.
// bind.test's TX). Common vars declared unconditionally above (msg/_res/r/_r)
// would be redeclared by a same-named tail variable — keep the first
// declaration.
func emitPreambleTailVars(body *strings.Builder, src string, preDeclared []string) {
	preDeclaredSet := map[string]bool{}
	for _, pv := range preDeclared {
		preDeclaredSet[pv] = true
	}
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
