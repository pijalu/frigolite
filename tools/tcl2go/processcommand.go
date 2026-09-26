// Package main implements the tcl2go tool.
//
// This file dispatches top-level TCL commands to their Go emitters; the
// command-name -> emitter table lives in tclcommandtable.go.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ---- Command processing ----

func (tp *transpiler) processCommands(cmds [][]tcl.RawWord) {
	for _, cmd := range cmds {
		tp.processCommand(cmd)
	}
}

// processCommand dispatches a single TCL command to its Go emitter.
func (tp *transpiler) processCommand(words []tcl.RawWord) {
	if len(words) == 0 {
		return
	}
	cmdName := words[0].Text
	args := words[1:]

	// Skip tests that exercise unsupported engine features by name (see
	// skipTests), so the generated test still compiles and runs.
	if tp.skipUnsupportedTest(cmdName, args) {
		return
	}

	// File-local proc bodies override same-named hardcoded handlers,
	// EXCEPT the recover.test harness procs (recover_with_opts /
	// do_recover_test / compare_result / compare_dbs), which the TCL file
	// defines as CLI-subprocess wrappers: the hardcoded in-process
	// RecoverSQL handlers always win for those names.
	// rtree8.test and rtreeA.test define their own create_t1/populate_t1/
	// truncate_node (unrelated to incrblob4's), which previously hijacked
	// the incrblob4 fillers and corrupted the fixture.
	if tp.emitUserProcOverride(cmdName, args) {
		return
	}
	if handler, ok := tclHandlers()[cmdName]; ok {
		handler(tp, args)
		return
	}
	// `$dbVar close` — the command NAME is a runtime connection-name
	// variable (quota.test 3.2.X: foreach db {db1a db2a db2b db1b}
	// { catch { $db close } }). Close through the runtime connection
	// registry so the underlying pager actually closes.
	if tp.emitConnVarClose(cmdName, args) {
		return
	}
	// Inline user procs recorded by processProc (zero-arg or single
	// defaulted-param calls): bind the default, then transpile the body.
	if tp.emitInlineZeroArgProc(cmdName, args) {
		return
	}
	tp.processDefaultCommand(cmdName, args)
}

// emitUserProcOverride emits the file-local proc body override for cmdName
// when one is registered (recover/side-effect-only procs never override).
// Returns true when emitted.
func (tp *transpiler) emitUserProcOverride(cmdName string, args []tcl.RawWord) bool {
	if _, isRecover := recoverProcNames[cmdName]; isRecover {
		return false
	}
	if _, isSideEffect := sideEffectOnlyProcs[cmdName]; isSideEffect {
		return false
	}
	if body, ok := globalProcBodies[cmdName]; ok {
		if em := userProcEmitterFor(cmdName, body); em != "" {
			tp.emitUserProc(em, goArgWords(args))
			return true
		}
	}
	return false
}

// emitConnVarClose handles `$dbVar close` — a close on a runtime
// connection-name variable. Returns true when handled.
func (tp *transpiler) emitConnVarClose(cmdName string, args []tcl.RawWord) bool {
	if !strings.HasPrefix(cmdName, "$") || len(args) < 1 || args[0].Text != "close" {
		return false
	}
	goVar := tclVarToGo(strings.TrimPrefix(cmdName, "$"))
	// A loop var shadowing a connection name (foreach db {db1a db2a}
	// { $db close }) resolves through the rename map (db → db_iter),
	// and the loop var holds a connection NAME string at runtime.
	if renamed, ok := tp.varRenames[goVar]; ok {
		goVar = renamed
	}
	if !isValidGoIdent(goVar) {
		return false
	}
	tp.emitLine("tclConnByName(%s, db, db1, db2, db3, db4, db5, db6, db7, db8, db9).Close()", goVar)
	return true
}

// emitInlineZeroArgProc handles the inline user procs recorded by processProc
// (zero-arg calls): bind the default parameter, then transpile the body.
// Returns true when handled.
func (tp *transpiler) emitInlineZeroArgProc(cmdName string, args []tcl.RawWord) bool {
	if len(args) != 0 || tp.inlineProcs == nil {
		return false
	}
	body, ok := tp.inlineProcs[cmdName]
	if !ok {
		return false
	}
	raw := ""
	if tp.inlineProcParams != nil {
		raw = tp.inlineProcParams[cmdName]
	}
	defs := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimSpace(raw), "{"), "}"))
	if a := inlineProcDefaultAssign(defs); a != "" {
		tp.emitLine("%s", a)
	}
	tp.processCommands(tcl.ParseCommands(body))
	return true
}

// skipUnsupportedTest reports whether a test command is listed in skipTests
// (unsupported engine features) and emits its skip side effects. TCL
// tester.tcl prefixes bare test names with `testprefix`, so resolve the
// effective name first ("4.0" → "whereF-4.0") and fall back to the raw name
// for explicitly-prefixed tests.
func (tp *transpiler) skipUnsupportedTest(cmdName string, args []tcl.RawWord) bool {
	if !isTestCommand(cmdName) {
		return false
	}
	if name := testCommandName(args); name != "" {
		if reason, ok := skipTestReason(name); ok {
			tp.emitSkippedTestSideEffects(cmdName, args, name, reason)
			return true
		}
		if tp.testPrefix != "" {
			prefixed := tp.testPrefix + "-" + name
			if reason, ok := skipTestReason(prefixed); ok {
				tp.emitSkippedTestSideEffects(cmdName, args, prefixed, reason)
				return true
			}
		}
	}
	return false
}

// processDefaultCommand handles commands without a registered handler:
// secondary db connections (dbN), bare query-proc calls, create_test_data,
// backup objects (B step/finish/...), and unknown commands (emitted as
// comments so tests still compile).
func (tp *transpiler) processDefaultCommand(cmdName string, args []tcl.RawWord) {
	// Backup object subcommands (B step N / B finish / B remaining /
	// B pagecount) for a declared *frigolite.Backup variable.
	if goName := tclVarToGo(cmdName); isValidGoIdent(goName) {
		if tp.processBackupObject(goName, args) {
			return
		}
	}
	// Check for dbN pattern (secondary db connections like db2, db3)
	if len(cmdName) > 2 && cmdName[:2] == "db" && cmdName[2] >= '0' && cmdName[2] <= '9' {
		tp.processDBForName(cmdName, args)
		return
	}
	// Bare query-proc / eqp / reopen-db procs (all inline a value or setup).
	if tp.inlineDefaultQueryProc(cmdName, args) {
		return
	}
	if tp.emitDefaultSpecialProc(cmdName, args) {
		return
	}
	if emitFTS5RegisterStrStmt(tp, cmdName, args) {
		return
	}
	if emitSetErrmsgStmt(tp, cmdName, args) {
		return
	}
	// Unsupported command — emit as comment to avoid test failures
	if len(args) > 0 {
		tp.emitLine("// %s %s (unsupported command, not transpiled)", cmdName, sanitizeTCLComment(describeArgsShort(args)))
	} else {
		tp.emitLine("// %s (unsupported command, not transpiled)", cmdName)
	}
}

// emitDefaultSpecialProc handles the default-command local procs that the
// transpiler inlines verbatim (sql36231, sql_uses_stmt, create_test_data,
// prepare_for_optimize, rebuild_t1, delete_all_data). Returns true when
// handled.
func (tp *transpiler) emitDefaultSpecialProc(cmdName string, args []tcl.RawWord) bool {
	switch cmdName {
	case "sql36231":
		return tp.emitSQL36231(args)
	case "sql_uses_stmt":
		return tp.emitSQLUsesStmt(args)
	case "create_test_data":
		return tp.emitCreateTestData(args)
	case "prepare_for_optimize":
		return tp.emitPrepareForOptimize(args)
	case "rebuild_t1":
		tp.emitRebuildT1()
		return true
	case "delete_all_data":
		tp.emitDeleteAllData()
		return true
	}
	return tp.emitDefaultFixtureProc(cmdName, args)
}

// emitDefaultFixtureProc handles the default-command fixture procs
// (sqlite3_drop_modules, read_fts3varint, rtree geometry/echo registration,
// corrupt-file helpers). Returns true when handled.
func (tp *transpiler) emitDefaultFixtureProc(cmdName string, args []tcl.RawWord) bool {
	switch cmdName {
	case "sqlite3_drop_modules":
		tp.emitSqlite3DropModules(args)
		return true
	case "read_fts3varint":
		return tp.emitReadFTS3Varint(args)
	case "register_cube_geom", "register_circle_geom":
		tp.emitRegisterRtreeGeom(cmdName, args)
		return true
	case "register_echo_module":
		tp.emitRegisterEchoModule(args)
		return true
	case "corrupt_freelist":
		return tp.emitCorruptFreelist(args)
	case "make_corrupt_file":
		return tp.emitMakeCorruptFile(args)
	}
	return false
}

// emitSQL36231 inlines sql36231 (tester.tcl): runs SQL on a second connection
// then restores the db-size header words (offsets 28 + 92), hiding the
// page-count growth from the filefmt-2.x assertions.
func (tp *transpiler) emitSQL36231(args []tcl.RawWord) bool {
	if len(args) < 1 {
		return false
	}
	sqlExpr := tp.collectSQLExpression(args)
	tp.emitLine("_r36231A := tclHexioRead(\"test.db\", 28, 4)")
	tp.emitLine("_r36231B := tclHexioRead(\"test.db\", 92, 8)")
	tp.emitLine("db36231, _err36231 := frigolite.Open(\"test.db\")")
	tp.emitLine("if _err36231 == nil {")
	tp.emitLine("\tdb36231.RegisterFunction(\"a_string\", func(args []interface{}) (interface{}, error) {")
	tp.emitLine("\t\tif len(args) < 1 || args[0] == nil { return \"\", nil }")
	tp.emitLine("\t\treturn tclAString(&a_string_counter, tclToInt(tclStr(args[0]))), nil")
	tp.emitLine("\t}, 1, 1)")
	tp.emitLine("\t_res36231 := db36231.Exec(%s)", sqlExpr)
	tp.emitLine("\t_ = _res36231")
	tp.emitLine("\tdb36231.Close()")
	tp.emitLine("}")
	tp.emitLine("tclHexioWrite(\"test.db\", 28, _r36231A)")
	tp.emitLine("tclHexioWrite(\"test.db\", 92, _r36231B)")
	tp.emitLine("_r = \"\"")
	return true
}

// emitSQLUsesStmt inlines sql_uses_stmt db $SQL — the TCL test-framework
// probe for whether a statement is executed via sqlite3_prepare_v2
// (statement-journal usage). The probe RUNS the SQL first (so the side
// effects matter for later tests: fts4onepass 2.x INSERT/DELETE/UPDATE fire
// triggers on the FTS table), then reports whether the VM used a statement
// journal. The pure-Go engine always prepares and its statements are atomic,
// so the journal probe is not meaningful; execute the SQL and skip the probe
// result.
func (tp *transpiler) emitSQLUsesStmt(args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	tp.emitLine("// sql_uses_stmt db $%s (statement-journal probe skipped; SQL executes)", sanitizeTCLComment(args[1].Text))
	arg := strings.TrimSpace(args[1].Text)
	if strings.HasPrefix(arg, "$") {
		goVar := tclVarToGo(strings.TrimPrefix(arg, "$"))
		if isValidGoIdent(goVar) {
			tp.emitLine("_res = db.Exec(%s)", goVar)
			tp.emitLine("_ = _res")
			return true
		}
	}
	sqlExpr := tp.goStringLiteral(args[1])
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("_ = _res")
	return true
}

// emitCreateTestData inlines create_test_data N (wherelimit.test): a local
// proc building a size×size t1 grid (DROP/CREATE/BEGIN + nested INSERT loop
// + COMMIT).
func (tp *transpiler) emitCreateTestData(args []tcl.RawWord) bool {
	if len(args) < 1 {
		return false
	}
	size := strings.TrimSpace(args[0].Text)
	tp.emitLine("// create_test_data %s (inlined)", size)
	tp.emitLine("_res = db.Exec(\"DROP TABLE IF EXISTS t1; CREATE TABLE t1(x int, y int); BEGIN;\")")
	tp.emitLine("if _res.Error != nil { t.Errorf(\"create_test_data drop/create: %%v\", _res.Error) }")
	tp.emitLine("for _ci := 1; _ci <= %s; _ci++ {", size)
	tp.emitLine("for _cj := 1; _cj <= %s; _cj++ {", size)
	tp.emitLine("if rerr := db.Exec(fmt.Sprintf(\"INSERT INTO t1 VALUES(%%d,%%d)\", _ci, _cj)).Error; rerr != nil { t.Errorf(\"create_test_data insert: %%v\", rerr) }")
	tp.emitLine("}")
	tp.emitLine("}")
	tp.emitLine("if rerr := db.Exec(\"COMMIT;\").Error; rerr != nil { t.Errorf(\"create_test_data commit: %%v\", rerr) }")
	return true
}

// emitPrepareForOptimize inlines prepare_for_optimize DB TBL (fts4opt.test):
// a local proc that rewrites the FTS %_segdir table, collapsing all segments
// in each level-group (level/1024) into a single level 1024*(level/1024)+32
// with recomputed idx values (sqlite3_db_config DEFENSIVE is irrelevant for
// the Go engine).
func (tp *transpiler) emitPrepareForOptimize(args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	tbl := strings.TrimSpace(args[1].Text)
	if tbl == "" || strings.ContainsAny(tbl, "$[") {
		return false
	}
	tp.emitLine("// prepare_for_optimize %s (inlined)", sanitizeTCLComment(tbl))
	tp.emitLine("_res = db.Exec(tclPrepareForOptimizeSQL(%q))", tbl)
	tp.emitLine("if _res.Error != nil { t.Errorf(\"prepare_for_optimize: %%v\", _res.Error) }")
	return true
}

// emitRebuildT1 inlines rebuild_t1 (e_delete.test): a local proc that drops
// and recreates the t1 test table with five fixed rows, then used as a
// do_select_tests -repair.
func (tp *transpiler) emitRebuildT1() {
	tp.emitLine("// rebuild_t1 (inlined)")
	tp.emitLine("_res = db.Exec(\"DROP TABLE IF EXISTS t1\")")
	tp.emitLine("_ = _res // catchsql")
	tp.emitLine("_res = db.Exec(\"CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 'one'); INSERT INTO t1 VALUES(2, 'two'); INSERT INTO t1 VALUES(3, 'three'); INSERT INTO t1 VALUES(4, 'four'); INSERT INTO t1 VALUES(5, 'five');\")")
	tp.emitLine("if _res.Error != nil { t.Errorf(\"rebuild_t1: %%v\", _res.Error) }")
}

// emitDeleteAllData inlines delete_all_data (SQLite test framework): deletes
// all rows from every table in every schema (main/temp/attached) so later
// tests start from empty tables (e_insert's count(*) subqueries depend on it).
func (tp *transpiler) emitDeleteAllData() {
	tp.emitLine("// delete_all_data (inlined)")
	tp.emitLine("for _, _t := range db.Query(\"SELECT name FROM sqlite_master WHERE type IN('table') AND name NOT LIKE 'sqlite_%%'\").Rows {")
	tp.emitLine("\t_res = db.Exec(\"DELETE FROM \" + tclQuoteIdent(fmt.Sprint(_t[0])))")
	tp.emitLine("\t_ = _res")
	tp.emitLine("}")
}

// emitSqlite3DropModules inlines sqlite3_drop_modules DB ?NAME...? — keep the
// named virtual table modules and drop all others (fts3dropmod.test).
func (tp *transpiler) emitSqlite3DropModules(args []tcl.RawWord) {
	quoted := make([]string, 0, len(args)-1)
	for _, a := range args[1:] {
		quoted = append(quoted, fmt.Sprintf("%q", strings.TrimSpace(a.Text)))
	}
	tp.emitLine("%s.UnregisterVTabModulesExcept([]string{%s})", tp.dbVar, strings.Join(quoted, ", "))
}

// emitReadFTS3Varint inlines read_fts3varint BLOB VARNAME — decode an FTS3
// varint from the front of BLOB, assign its value to VARNAME, return bytes
// consumed (fts3cov 2.x).
func (tp *transpiler) emitReadFTS3Varint(args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	blobExpr := tp.buildStringExpr(args[0].Text)
	varName := tclVarToGo(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "$")))
	tp.emitLine("_nRead, _ftsVar := tclReadFTS3Varint(%s)", blobExpr)
	tp.emitLine("%s = _ftsVar", varName)
	tp.emitLine("_ = _nRead")
	return true
}

// emitRegisterRtreeGeom inlines register_cube_geom DB /
// register_circle_geom DB — install the harness r-tree geometry callbacks
// from src/test_rtree.c (rtree9.test).
func (tp *transpiler) emitRegisterRtreeGeom(cmdName string, args []tcl.RawWord) {
	conn := tp.dbVar
	if len(args) >= 2 && strings.TrimSpace(args[1].Text) != "" {
		conn = tclVarToGo(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "$"))
	}
	geom := "cube"
	if cmdName == "register_circle_geom" {
		geom = "circle"
	}
	tp.emitLine("if err := %s.RegisterRtreeGeometry(%q); err != nil { t.Fatal(err) }", conn, geom)
}

// emitRegisterEchoModule inlines register_echo_module
// [sqlite3_connection_pointer db] / register_echo_module db — register the
// echo test module (src/test8.c) on the named connection
// (sqlite3_create_module parity). The module is per-connection, so the vtab
// lifecycle tests observe both the unregistered state ("no such module:
// echo") and the registered state (vtab1-1.x, vtab3, vtab6).
func (tp *transpiler) emitRegisterEchoModule(args []tcl.RawWord) {
	conn := tp.dbVar
	if len(args) >= 1 {
		if name := connNameFromPointerArg(args[0].Text); name != "" {
			conn = tclVarToGo(name)
		}
	}
	tp.emitLine("%s.RegisterEchoModule()", conn)
}

// emitCorruptFreelist inlines corrupt_freelist FILE N — corrupt9.test's proc
// that overwrites the freelist trunk's leaf entries with duplicates of the
// first leaf page number (creating duplicate free-list entries).
func (tp *transpiler) emitCorruptFreelist(args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	fileExpr := tp.goStringLiteral(args[0])
	nExpr := tp.valueExpr(args[1])
	tp.emitLine("tclCorruptFreelist(%s, %s)", fileExpr, nExpr)
	return true
}

// emitMakeCorruptFile inlines make_corrupt_file FNAME — the zipfile2.test
// proc that writes a crafted archive (60000-byte entry name, huge extra) to
// FNAME.
func (tp *transpiler) emitMakeCorruptFile(args []tcl.RawWord) bool {
	if len(args) < 1 {
		return false
	}
	fname := strings.TrimSpace(args[0].Text)
	tp.emitLine("tclMakeCorruptFile(%s)", tp.goStringLiteral(tcl.RawWord{Text: fname}))
	return true
}

// optionalConnVar resolves an optional leading connection-name argument,
// defaulting to the main db variable.
func (tp *transpiler) optionalConnVar(args []tcl.RawWord) string {
	connVar := tp.dbVar
	if len(args) >= 1 {
		if v := strings.TrimSpace(args[0].Text); isValidGoIdent(tclVarToGo(v)) {
			connVar = tclVarToGo(v)
		}
	}
	return connVar
}

// digitsOrZero renders n as a numeric literal, or "0" when it is empty or
// not all digits.
func digitsOrZero(n string) string {
	if n == "" {
		return "0"
	}
	for _, ch := range n {
		if ch < '0' || ch > '9' {
			return "0"
		}
	}
	return n
}

// inlineDefaultQueryProc handles the default-command procs that inline a
// value or setup: bare query-proc calls, eqp (EXPLAIN QUERY PLAN detail), and
// the reopen-db procs. Returns true when the command was handled.
func (tp *transpiler) inlineDefaultQueryProc(cmdName string, args []tcl.RawWord) bool {
	// A bare query-proc call (e.g. `signature` where `proc signature {}
	// { return [db eval {SQL}] }`) returns the query result; inline it
	// so a do_test body ending in `signature` compares the result.
	// memdb.test's `signature` (with-args proc over SELECT x FROM t3)
	// is fingerprinted as memdb_signature: emit the t3 fingerprint.
	if len(args) == 0 {
		if body, ok := globalProcBodies[cmdName]; ok && userProcEmitterFor(cmdName, body) == "memdb_signature" {
			tp.emitLine("_r = tclMemdbSignature(%s)", tp.dbVar)
			return true
		}
	}
	if len(tp.queryFuncs) > 0 {
		if sql, ok := tp.queryFuncs[cmdName]; ok {
			sqlExpr := tp.buildSQLStringExpr(sql)
			tp.emitLine("_r = tclExecSQL(db, %s)", sqlExpr)
			return true
		}
	}
	if tp.emitValueProcCall(cmdName, args) {
		return true
	}
	return tp.emitQuotaEQPProcCall(cmdName, args)
}

// emitValueProcCall handles the fixed-name value procs: t1sig (table
// fingerprint), cksum (database fingerprint), and the pager change-counter
// readers/writer (exclusive2.test). Returns true when handled.
func (tp *transpiler) emitValueProcCall(cmdName string, args []tcl.RawWord) bool {
	// t1sig [CONN] (exclusive2.test): table fingerprint (count + md5sum).
	// The optional argument names the connection variable (default "db").
	if body, ok := globalProcBodies[cmdName]; ok && userProcEmitterFor(cmdName, body) == "table_sig" {
		table, col, _ := tableSigProcInfo(body)
		tp.emitLine("_r = tclTableSig(%s, %q, %q)", tp.optionalConnVar(args), table, col)
		return true
	}
	// cksum [CONN] (tester.tcl framework proc): the database fingerprint.
	if cmdName == "cksum" {
		tp.emitLine("_r = tclCksum(%s)", tp.optionalConnVar(args))
		return true
	}
	// readPagerChangeCounter FILE (exclusive2.test): the database header
	// change counter (big-endian uint32 at offset 24).
	if cmdName == "readPagerChangeCounter" && len(args) == 1 {
		tp.emitLine("_r = tclReadPagerChangeCounter(%s)", tp.goStringLiteral(args[0]))
		return true
	}
	// pagerChangeCounter FILE N [FD] (exclusive2.test): write the change
	// counter and return the re-read value. The optional channel argument
	// only changes which TCL channel performs the write.
	if cmdName == "pagerChangeCounter" && len(args) >= 2 {
		pathExpr := tp.goStringLiteral(args[0])
		tp.emitLine("_r = tclSetPagerChangeCounter(%s, %s)", pathExpr, digitsOrZero(strings.TrimSpace(args[1].Text)))
		return true
	}
	return false
}

// emitQuotaEQPProcCall handles the quota_list / quota_size / eqp value procs.
// Returns true when handled.
func (tp *transpiler) emitQuotaEQPProcCall(cmdName string, args []tcl.RawWord) bool {
	// quota_list (quota.test): the sorted list of quota-group patterns from
	// sqlite3_quota_dump. A do_test body ending in `quota_list` compares
	// against the pattern list.
	if cmdName == "quota_list" && len(args) == 0 {
		tp.emitLine("_r = tclQuotaList()")
		return true
	}
	// quota_size NAME (quota.test): the tracked size of the quota group
	// named NAME (0 when absent).
	if cmdName == "quota_size" && len(args) >= 1 {
		tp.emitLine("_r = tclQuotaSize(%s)", tp.goStringLiteral(args[0]))
		return true
	}
	// eqp "SQL" (e_fkey.test): run EXPLAIN QUERY PLAN and collect the raw
	// detail values. A do_test body ending in `eqp ...` compares the result
	// against an expected detail list ($delete/$update concat).
	if cmdName == "eqp" && len(args) >= 1 {
		sqlExpr := tp.buildSQLStringExpr(strings.TrimSpace(args[0].Text))
		tp.emitLine("_r = tclEQP(db, %s)", sqlExpr)
		return true
	}
	// reopen-db procs (e_resolve/e_droptrigger/e_dropview.test): local procs
	// that close the db, delete the database files, reopen test.db, and re-run
	// a fresh multi-schema setup. Inline their bodies via the shared helper.
	if cmdName == "resolve_reopen_db" || cmdName == "droptrigger_reopen_db" || cmdName == "dropview_reopen_db" {
		tp.inlineReopenDB(cmdName, args)
		return true
	}
	return false
}

// inlineReopenDB inlines the local reopen-db procs from e_resolve,
// e_droptrigger and e_dropview: close the db, delete the database files,
// reopen test.db, and re-run the test's fresh multi-schema setup so the
// assertions run against a clean per-schema state.
func (tp *transpiler) inlineReopenDB(cmdName string, args []tcl.RawWord) {
	tp.emitLine("// %s (inlined)", cmdName)
	tp.emitLine("db.Close()")
	tp.emitLine("os.Remove(\"test.db\")")
	tp.emitLine("os.Remove(\"test.db2\")")
	if cmdName == "resolve_reopen_db" {
		tp.emitLine("os.Remove(\"test.db3\")")
	}
	tp.emitLine("db, err = frigolite.Open(\"test.db\")")
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("tcl_nullvalue = \"{}\" // fresh connection resets nullvalue")
	switch cmdName {
	case "resolve_reopen_db":
		tp.emitLine("_res = db.Exec(schema)")
		tp.emitLine("if _res.Error != nil { t.Errorf(\"schema exec: %%v\", _res.Error) }")
	case "droptrigger_reopen_db":
		event := "INSERT"
		if len(args) >= 1 && strings.TrimSpace(args[0].Text) != "" {
			event = strings.ToUpper(strings.TrimSpace(args[0].Text))
		}
		tp.emitLine("// droptrigger event %s", event)
		tp.emitLine("triggers_fired = \"\"")
		tp.emitLine("db.RegisterFunction(\"r\", func(args []interface{}) (interface{}, error) {")
		tp.emitLine("\tif len(args) > 0 && args[0] != nil {")
		tp.emitLine("\t\tif triggers_fired != \"\" { triggers_fired += \" \" }")
		tp.emitLine("\t\ttriggers_fired += fmt.Sprint(args[0])")
		tp.emitLine("\t}")
		tp.emitLine("\treturn nil, nil")
		tp.emitLine("}, 0, -1)")
		tp.emitLine("_res = db.Exec(\"ATTACH 'test.db2' AS aux; CREATE TEMP TABLE t1(a, b); INSERT INTO t1 VALUES('a', 'b'); CREATE TRIGGER tr1 AFTER %s ON t1 BEGIN SELECT r('temp.tr1'); END; CREATE TABLE t2(a, b); INSERT INTO t2 VALUES('a', 'b'); CREATE TRIGGER tr1 BEFORE %s ON t2 BEGIN SELECT r('main.tr1'); END; CREATE TRIGGER tr2 AFTER %s ON t2 BEGIN SELECT r('main.tr2'); END; CREATE TABLE aux.t3(a, b); INSERT INTO t3 VALUES('a', 'b'); CREATE TRIGGER aux.tr1 BEFORE %s ON t3 BEGIN SELECT r('aux.tr1'); END; CREATE TRIGGER aux.tr2 AFTER %s ON t3 BEGIN SELECT r('aux.tr2'); END; CREATE TRIGGER aux.tr3 AFTER %s ON t3 BEGIN SELECT r('aux.tr3'); END;\")", event, event, event, event, event, event)
		tp.emitLine("if _res.Error != nil { t.Errorf(\"droptrigger_reopen_db: %%v\", _res.Error) }")
	case "dropview_reopen_db":
		tp.emitLine("_res = db.Exec(\"ATTACH 'test.db2' AS aux; CREATE TABLE t1(a, b); INSERT INTO t1 VALUES('a main', 'b main'); CREATE VIEW v1 AS SELECT * FROM t1; CREATE VIEW v2 AS SELECT * FROM t1; CREATE TEMP TABLE t1(a, b); INSERT INTO temp.t1 VALUES('a temp', 'b temp'); CREATE VIEW temp.v1 AS SELECT * FROM t1; CREATE TABLE aux.t1(a, b); INSERT INTO aux.t1 VALUES('a aux', 'b aux'); CREATE VIEW aux.v1 AS SELECT * FROM t1; CREATE VIEW aux.v2 AS SELECT * FROM t1; CREATE VIEW aux.v3 AS SELECT * FROM t1;\")")
		tp.emitLine("if _res.Error != nil { t.Errorf(\"dropview_reopen_db: %%v\", _res.Error) }")
	}
}

func describeArgsShort(args []tcl.RawWord) string {
	var parts []string
	for _, a := range args {
		if a.Braced {
			s := a.Text
			s = strings.ReplaceAll(s, "\n", "\\n")
			s = strings.ReplaceAll(s, "\r", "")
			if len(s) > 50 {
				s = s[:50] + "..."
			}
			parts = append(parts, "{"+s+"}")
		} else {
			s := a.Text
			s = strings.ReplaceAll(s, "\n", "\\n")
			s = strings.ReplaceAll(s, "\r", "")
			if len(s) > 50 {
				s = s[:50] + "..."
			}
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// processBinaryCommand dispatches `binary scan` / `binary encode hex` /
// `binary decode hex` etc. as top-level commands (i.e. not in a `set`
// RHS, which is handled separately in processset_part2.go).
// Currently only `binary scan $b S name` and `binary scan $b I name`
// are supported (corrupt*.test reads big-endian cell-pointer and
// child-page bytes from disk and decodes them into integer strings).
// Other forms fall through to processInfraComment.
func (tp *transpiler) processBinaryCommand(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.processInfraComment("binary", args)
		return
	}
	sub := args[0].Text
	// binary scan $b FORMAT name — convert bytes to int-string.
	if sub == "scan" {
		tp.processBinaryScan(args)
		return
	}
	// Fall through for any other binary form.
	tp.processInfraComment("binary", args)
}

// processBinaryScan handles the `binary scan $b FORMAT name` form.
func (tp *transpiler) processBinaryScan(args []tcl.RawWord) {
	if len(args) != 4 {
		// multi-result or other exotic form: fall through
		tp.processInfraComment("binary", args)
		return
	}
	bsrc := args[1].Text
	format := args[2].Text
	varName := args[3].Text
	goSrc := tclVarToGo(bsrc)
	if !isValidGoIdent(goSrc) {
		tp.processInfraComment("binary", args)
		return
	}
	goName := tclVarToGo(varName)
	if !isValidGoIdent(goName) {
		tp.processInfraComment("binary", args)
		return
	}
	switch format {
	case "S":
		tp.assignSetValue(goName, "tclBinaryScanBigUint16("+goSrc+")")
	case "I":
		tp.assignSetValue(goName, "tclBinaryScanBigUint32("+goSrc+")")
	default:
		tp.processInfraComment("binary", args)
	}
}
