// Package main implements the tcl2go tool.
//
// This file handles execsql / db commands (processExecSQL, processDB and the
// db serialize/deserialize dispatch). UDF registrations live in
// processdb_func.go / processdb_registered.go / processdb_varfuncs.go, the
// hexdb image decode in processdb_hexdb.go, and the row-callback eval forms
// in processdb_dbeval.go / processdb_dbevalrows.go.
package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ---- SQL execution handlers ----

func (tp *transpiler) processExecSQL(args []tcl.RawWord, sqlType string) {
	if len(args) == 0 {
		return
	}
	sqlExpr := tp.collectSQLExpression(args)
	if sqlExpr == `""` {
		return
	}

	sqlText := ""
	if len(args) > 0 {
		sqlText = args[0].Text
	}
	if reason := unsupportedSQL(sanitizeSQL(sqlText)); reason != "" {
		tp.emitLine("// execsql skipped: %s", reason)
		return
	}

	dbConn := tp.resolveSQLConnection(args)

	if sqlType == "catch" {
		tp.emitCatchExec(dbConn, sqlExpr)
		return
	}
	// Use db.Query when the SQL contains ANY query statement (not just the
	// last): a multi-statement execsql like "INSERT...; SELECT...;
	// UPDATE..." returns the rows of every SELECT in between, and db.Query
	// must be used to collect them (db.Exec discards query results).
	if tp.sqlContainsQuery(args, sqlText) {
		tp.emitQueryExec(dbConn, sqlExpr)
	} else {
		tp.emitPlainExec(dbConn, sqlExpr)
	}
}

// resolveSQLConnection resolves the connection name for `execsql {SQL} db2`
// (default db). Route through the alias map: a secondary connection on the
// main test file ("sqlite3 db2 test.db") is aliased to db, so execute on the
// underlying handle. The connection name is args[1] for `execsql $sql db2`,
// or args[2] for `execsql {SQL} db2`.
func (tp *transpiler) resolveSQLConnection(args []tcl.RawWord) string {
	connIdx := sqlConnectionIndex(args)
	if connIdx < 0 || connIdx >= len(args) {
		return "db"
	}
	if conn := tp.resolveSQLConnHandle(tp.sqlConnHandle(args[connIdx].Text)); conn != "" {
		return conn
	}
	return "db"
}

// sqlConnHandle converts a raw connection-name word into its Go variable
// name, following the foreach rename map (a TCL variable shadowed by a
// foreach loop var, e.g. `foreach db {db db2}` renames db→db_iter).
func (tp *transpiler) sqlConnHandle(raw string) string {
	h := tclVarToGo(raw)
	if renamed, ok := tp.varRenames[h]; ok {
		return renamed
	}
	return h
}

// resolveSQLConnHandle maps a Go connection handle to the connection the
// harness executes on, or "" when the handle names nothing known.
func (tp *transpiler) resolveSQLConnHandle(h string) string {
	if h == "" {
		return ""
	}
	// A runtime connection-name variable (foreach db {db db2} loop var):
	// dispatch through tclConnByName at runtime.
	if tp.runtimeConnVars[h] {
		return "tclConnByName(" + h + ", db, db1, db2, db3, db4, db5, db6, db7, db8, db9)"
	}
	if h == "db" {
		return "db"
	}
	if isPreDeclaredDB(h) || tp.dbConnVars[h] {
		return tp.dbAliasOrSelf(h)
	}
	// The argument is a variable (e.g. `set db_dest db2` then
	// `execsql {SQL} $db_dest`); resolve its constant value to a
	// connection name when it names a declared DB variable.
	return tp.resolveConstConnValue(h)
}

// dbAliasOrSelf returns the aliased connection for h, or h itself.
func (tp *transpiler) dbAliasOrSelf(h string) string {
	if target, ok := tp.dbAliases[h]; ok {
		return target
	}
	return h
}

// resolveConstConnValue resolves a connection-holding variable's constant
// value to a connection name ("db", a declared DB variable, or "" when the
// value names no known connection).
func (tp *transpiler) resolveConstConnValue(h string) string {
	conn, ok := tp.varConstValues[h]
	if !ok {
		return ""
	}
	connGo := tclVarToGo(conn)
	if connGo == "db" {
		return "db"
	}
	if connGo != "" && (isPreDeclaredDB(connGo) || tp.dbConnVars[connGo]) {
		return connGo
	}
	return ""
}

// sqlConnectionIndex finds the connection-name argument index for
// `execsql {SQL} db2` / `execsql $sql db2` (default -1 = main db).
func sqlConnectionIndex(args []tcl.RawWord) int {
	if len(args) < 2 {
		return -1
	}
	if args[1].Braced || args[1].Quoted || strings.HasPrefix(args[1].Text, "${") {
		if len(args) >= 3 {
			return 2
		}
		return -1
	}
	return 1
}

// sqlContainsQuery reports whether an execsql argument is (or references a
// variable known to hold) a query.
func (tp *transpiler) sqlContainsQuery(args []tcl.RawWord, sqlText string) bool {
	// A $var argument whose value is known to hold query SQL (tracked by
	// markQueryVar) must go through db.Query so the rows are collected.
	if len(args) > 0 {
		if varName := strings.TrimPrefix(args[0].Text, "$"); tp.queryVars[varName] {
			return true
		}
	}
	return bodySQLContainsQuery(sqlText)
}

// emitCatchExec emits a catchsql-style exec with the given connection.
func (tp *transpiler) emitCatchExec(dbConn, sqlExpr string) {
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	if tp.catchMode {
		tp.emitLine("if _res.Error != nil { _catchErr = _res.Error }")
	} else {
		tp.emitLine("_ = _res // catchsql")
	}
}

// emitQueryExec emits a query-style exec (rows are collected into r).
func (tp *transpiler) emitQueryExec(dbConn, sqlExpr string) {
	tp.emitLine("r = %s.Query(%s)", dbConn, sqlExpr)
	if tp.catchMode {
		tp.emitLine("if r.Error != nil { _catchErr = r.Error }")
	} else {
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("}")
	}
}

// emitPlainExec emits a plain exec (result into _res).
func (tp *transpiler) emitPlainExec(dbConn, sqlExpr string) {
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	if tp.catchMode {
		tp.emitLine("if _res.Error != nil { _catchErr = _res.Error }")
	} else {
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
		tp.emitLine("}")
	}
}

// dbSubCmdHandlers lazily maps `db <subcommand>` to its transpile method. A
// missing key is the former switch's no-op default. The main "db" connection
// name is fixed for the delegating handlers. The map is built on first use
// (not at package init) because the referenced handlers transitively reach
// back into processDB, which would make a package-level initializer cycle.
func dbSubCmdHandler(sub string) func(*transpiler, []tcl.RawWord) {
	if dbSubCmdHandlers == nil {
		dbSubCmdHandlers = map[string]func(*transpiler, []tcl.RawWord){
			"close":            func(tp *transpiler, _ []tcl.RawWord) { tp.processDBClose() },
			"backup":           func(tp *transpiler, rest []tcl.RawWord) { tp.processDBBackupRestore("backup", rest) },
			"restore":          func(tp *transpiler, rest []tcl.RawWord) { tp.processDBBackupRestore("restore", rest) },
			"null":             (*transpiler).processDBNullValue,
			"nullvalue":        (*transpiler).processDBNullValue,
			"eval":             (*transpiler).processDBEval,
			"onecolumn":        (*transpiler).processDBOnecolumn,
			"transaction":      (*transpiler).processDBTransaction,
			"function":         (*transpiler).processDBFunction,
			"func":             (*transpiler).processDBFunction,
			"collate":          (*transpiler).processDBCollate,
			"collation_needed": func(tp *transpiler, rest []tcl.RawWord) { tp.processNamedDBCollationNeeded("db", rest) },
			"deserialize":      (*transpiler).processDBDeserialize,
			"serialize":        (*transpiler).processDBSerialize,
			"progress":         (*transpiler).processDBProgress,
			"authorizer":       func(tp *transpiler, rest []tcl.RawWord) { tp.processNamedDBAuthorizer("db", rest) },
			"incrblob":         (*transpiler).processDBIncrblob,
			"changes":          (*transpiler).processDBChanges,
			"total_changes":    (*transpiler).processDBTotalChanges,
			"preupdate":        (*transpiler).processDBPreupdate,
			"commit_hook":      (*transpiler).processDBCommitHook,
			"rollback_hook":    (*transpiler).processDBRollbackHook,
			"update_hook":      (*transpiler).processDBUpdateHook,
			"trace":            func(tp *transpiler, rest []tcl.RawWord) { tp.processNamedDBTraceProfile("db", rest, "trace") },
			"profile":          func(tp *transpiler, rest []tcl.RawWord) { tp.processNamedDBTraceProfile("db", rest, "profile") },
			"trace_v2":         func(tp *transpiler, rest []tcl.RawWord) { tp.processNamedDBTraceV2("db", rest) },
			"busy":             func(tp *transpiler, rest []tcl.RawWord) { tp.processNamedDBBusy("db", rest) },
			"complete":         (*transpiler).processDBComplete,
		}
	}
	return dbSubCmdHandlers[sub]
}

// dbSubCmdHandlers is the `db <subcommand>` dispatch table (see
// dbSubCmdHandler).
var dbSubCmdHandlers map[string]func(*transpiler, []tcl.RawWord)

func (tp *transpiler) processDB(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	if fn := dbSubCmdHandler(args[0].Text); fn != nil {
		fn(tp, args[1:])
	}
	// no-op for other db subcommands
}

// processDBNullValue handles TCL "db null <value>" / "db nullvalue <value>":
// it sets how SQL NULL renders in query results.
func (tp *transpiler) processDBNullValue(rest []tcl.RawWord) {
	if len(rest) >= 1 {
		tp.emitLine("tcl_nullvalue = %s", tp.goStringLiteral(rest[0]))
	}
}

// processDBChanges emits the `db changes` query result.
func (tp *transpiler) processDBChanges(_ []tcl.RawWord) {
	tp.emitLine("_r = strconv.FormatInt(db.Changes(), 10)")
}

// processDBTotalChanges emits the `db total_changes` query result.
func (tp *transpiler) processDBTotalChanges(_ []tcl.RawWord) {
	tp.emitLine("_r = strconv.FormatInt(db.TotalChanges(), 10)")
}

// processDBComplete handles `db complete {SQL}` — sqlite3_complete test:
// returns 1 when the SQL ends in a complete statement (semicolon outside
// strings/comments, trigger-aware ";END;" detection), 0 otherwise. Mirrors
// src/complete.c. Unlike execsql, db complete's braced argument is a LITERAL
// SQL string, not a substituted one, so $var inside it is NOT a TCL
// substitution.
func (tp *transpiler) processDBComplete(rest []tcl.RawWord) {
	if len(rest) >= 1 {
		sqlExpr := tp.goStringLiteral(rest[0])
		tp.emitLine("_r = tclBool01(db.Complete(%s))", sqlExpr)
	}
}

// processDBBackupRestore handles `db backup [schema] FILE` and `db restore
// [schema] FILE` (the TCL sqlite3 backup/restore methods, wrappers over the
// sqlite3_backup C API). The command raises a TCL error on failure with the
// "backup failed: ..." / "restore failed: ..." / "cannot open source
// database: ..." message; catch-mode bodies capture it via _catchErr.
func (tp *transpiler) processDBBackupRestore(kind string, rest []tcl.RawWord) {
	schemaName := "\"main\""
	fileArg := ""
	if len(rest) >= 2 {
		schemaName = tp.goStringLiteral(rest[0])
		fileArg = tp.goStringLiteral(rest[1])
	} else if len(rest) == 1 {
		fileArg = tp.goStringLiteral(rest[0])
	} else {
		tp.emitLine("// db %s (wrong # args)", kind)
		if tp.catchMode {
			tp.emitLine("_catchErr = fmt.Errorf(\"wrong # args: should be \\\"db %s ?DATABASE? FILENAME\\\"\")", kind)
		}
		return
	}
	if !tp.catchMode {
		tp.emitLine("var _catchErr error")
	}
	tp.emitLine("_catchErr = tclDBBackupRestore(db, %q, %s, %s)", kind, schemaName, fileArg)
	tp.emitLine("if _catchErr != nil { _r = \"\" }")
}

// processDBClose handles `db close`: closes the main connection, firing
// registered collation destructors (sqlite3_create_collation_v2 xDestroy),
// matching SQLite's behavior on connection close.
func (tp *transpiler) processDBClose() {
	// Inside a testfixture script, `db close` closes the fixture connection
	// stored in tclFixtureDBs[tp.fixtureVar] and removes it from the map (the
	// fixture process would terminate), so a later testfixture call reopens a
	// fresh one.
	if tp.fixtureVar != "" {
		tp.emitLine("tclFixtureDBs[%q].Close()", tp.fixtureVar)
		tp.emitLine("tclFixtureDBs[%q] = nil", tp.fixtureVar)
		return
	}
	// TCL "db close" closes the main connection. A subsequent
	// "sqlite3 db <file>" reopens it; the emitLine below pairs with
	// the reopen logic in processSet/processSqlite3.
	if tp.collateDtorVars != nil {
		for _, incrVar := range tp.collateDtorVars {
			tp.emitIncrCounter(incrVar)
		}
		tp.collateDtorVars = nil
	}
	tp.emitLine("db.Close()")
	tp.dbClosed = true
}

// processDBSerialize handles `db serialize ?SCHEMA?` (memdb1.test: [db
// serialize] / [db serialize main]): the raw image bytes land in _r as a
// Go string (length == page_size × page_count).
func (tp *transpiler) processDBSerialize(rest []tcl.RawWord) {
	schema := "main"
	if len(rest) >= 1 {
		schema = strings.Trim(rest[0].Text, "{} ")
		if schema == "" {
			schema = "main"
		}
	}
	if len(rest) > 1 {
		if tp.catchMode {
			tp.emitLine("_catchErr = fmt.Errorf(%q)", "wrong # args: should be \"db serialize ?DATABASE?\"")
		} else {
			tp.emitLine("t.Errorf(%q)", "wrong # args: should be \"db serialize ?DATABASE?\"")
		}
		return
	}
	tp.emitLine("_r = string(tclSerialize(db, %q))", schema)
}

// processDBDeserialize handles `db deserialize [decode_hexdb {...}]`: it
// builds the database image from the hexdb block, writes it to a temp file,
// and reopens the connection on that file. The hexdb format is the .open
// --hexdb dump produced by sqlite3 (each line is `| <offset>: <hex bytes>`
// grouped by `| page N offset M`).
func (tp *transpiler) processDBDeserialize(rest []tcl.RawWord) {
	if len(rest) < 1 {
		tp.emitDeserializeWrongArgs()
		return
	}
	// memdb1.test forms: `db deserialize $db1` / `db deserialize main $ser` /
	// `db deserialize -readonly 1 $db1` / `db deserialize -maxsize N $db1` /
	// `db deserialize aux1 $direct` / `db deserialize {}` / `db deserialize
	// not-a-database`. These carry a Go string/bytes variable (db1Blob for
	// ::db1), NOT a hexdb block — route through DB.Deserialize.
	if hexdb := extractHexdbBlock(rest[0].Text); hexdb == "" || !strings.Contains(rest[0].Text, "decode_hexdb") {
		tp.emitDBDeserializeValue(rest)
		return
	}
	// The argument is usually `[decode_hexdb {<block>}]`; extract the
	// braced block. Fall back to the raw text when the block is absent.
	hexdb := extractHexdbBlock(rest[0].Text)
	if hexdb == "" {
		tp.emitLine("// db deserialize (no hexdb block)")
		return
	}
	img, err := parseHexdbImage(hexdb)
	if err != nil || len(img) == 0 {
		tp.emitLine("// db deserialize (unparseable hexdb: %v)", err)
		return
	}
	// Emit a Go literal for the image and reopen db on a temp file.
	goBytes := tp.goByteArrayLiteral(img)
	tp.emitLine("// db deserialize [decode_hexdb {...}]")
	tp.emitLine("deserPath := filepath.Join(t.TempDir(), \"deser.db\")")
	tp.emitLine("if werr := os.WriteFile(deserPath, %s, 0o644); werr != nil { t.Fatal(werr) }", goBytes)
	tp.emitLine("db.Close()")
	tp.emitLine("db, err = frigolite.Open(deserPath)")
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
	tp.dqsDML = true
}

// recoverProcNames are TCL proc names whose hardcoded in-process handlers
// take precedence over file-local proc bodies (see processCommand).
var recoverProcNames = map[string]bool{
	"recover_with_opts": true,
	"do_recover_test":   true,
	"compare_result":    true,
	"compare_dbs":       true,
}

// sideEffectOnlyProcs are TCL procs whose bodies read C-internal counters
// (::sqlite_search_count / ::sqlite_found_count — VDBE scan/found stats the
// engine does not expose) wrapped around plain SQL execution. The hardcoded
// handler runs the SQL for its TRANSACTIONAL side effects (BEGIN/ROLLBACK
// bookkeeping the surrounding tests depend on) and drops only the count part
// of the expected value; file-local proc bodies must not shadow it (same
// precedence rule as recoverProcNames).
var sideEffectOnlyProcs = map[string]bool{
	"execsqlS": true,
}
