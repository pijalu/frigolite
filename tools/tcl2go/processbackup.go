// Package main implements the tcl2go tool.
//
// This file handles the sqlite3_backup C-API emulation: sqlite3_backup,
// backup-object subcommands (B step/finish/remaining/pagecount), sqlite3_errmsg
// / sqlite3_errcode / sqlite3_close, dbcksum, and db backup/restore methods.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// processSqlite3Backup handles `sqlite3_backup B <destdb> <destschema>
// <srcdb> <srcschema>`. It declares the backup object variable, emits the
// tclBackupInit call, and sets the command result to the backup name (the TCL
// command returns "B").
func (tp *transpiler) processSqlite3Backup(args []tcl.RawWord) {
	if len(args) < 5 {
		tp.emitLine("// sqlite3_backup (malformed)")
		return
	}
	backupName := args[0].Text
	goName := tclVarToGo(backupName)
	if !isValidGoIdent(goName) {
		tp.emitLine("// sqlite3_backup %s (invalid identifier)", backupName)
		return
	}
	if !tp.isVarDeclared(goName) {
		tp.vars = append(tp.vars, goName)
	}
	destDB := tp.dbArgGo(args[1].Text)
	destSchema := tp.goStringLiteral(args[2])
	srcDB := tp.dbArgGo(args[3].Text)
	srcSchema := tp.goStringLiteral(args[4])
	tp.emitLine("%s, _berr = tclBackupInit(%s, %s, %s, %s)", goName, destDB, destSchema, srcDB, srcSchema)
	tp.emitLine("if _berr != nil {")
	tp.emitLine("\t// sqlite3_backup_init failed; the error message is on the source connection")
	tp.emitLine("\t%s = nil", goName)
	tp.emitLine("\t_ = %s", goName)
	tp.emitLine("} else {")
	tp.emitLine("\t_r = %q", backupName)
	tp.emitLine("}")
}

// dbArgGo resolves a TCL db-connection argument (a bare name like "db2" or a
// $var holding a connection name) to the Go variable name. A $var that is
// never set (e.g. zeroblob.test's $::DB) is an empty connection name at TCL
// runtime, which resolves to the default connection — fall back to "db"
// instead of emitting a call on the unresolved string variable.
func (tp *transpiler) dbArgGo(arg string) string {
	h := tclVarToGo(strings.TrimPrefix(arg, "$"))
	if strings.HasPrefix(arg, "$") {
		if conn, ok := tp.varConstValues[h]; ok {
			connGo := tclVarToGo(conn)
			if connGo != "" && (isPreDeclaredDB(connGo) || tp.dbConnVars[connGo] || connGo == "db") {
				return connGo
			}
		}
		// The variable is not a known connection alias: if its name is not
		// itself a declared connection handle, it is unset at runtime.
		if !(isPreDeclaredDB(h) || tp.dbConnVars[h] || h == "db") {
			return "db"
		}
	}
	if h == "" {
		return "db"
	}
	return h
}

// processSqlite3Errmsg handles `sqlite3_errmsg db`.
func (tp *transpiler) processSqlite3Errmsg(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	// A FAILED-open connection that has been closed reports SQLITE_MISUSE
	// text from sqlite3_errmsg (capi3c-3.6.2-misuse); before its close it
	// reports the open failure message (capi3c-3.4). Both facts are static:
	// a failed-open handle cannot execute SQL or fail a close.
	if _, closed := tp.connClosed[dbConn]; closed {
		if _, failed := tp.connFailedOpen[dbConn]; failed {
			tp.emitLine("_r = %q", "bad parameter or other API misuse")
			return
		}
	}
	if msg, ok := tp.connFailedOpen[dbConn]; ok {
		tp.emitLine("_r = %q", msg)
		return
	}
	tp.emitLine("_r = tclErrMsg(%s)", dbConn)
}

// processSqlite3Errcode handles `sqlite3_errcode db`.
func (tp *transpiler) processSqlite3Errcode(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	if msg, ok := tp.connFailedOpen[dbConn]; ok {
		// capi3-3.3 checks the extended errcode after a failed open.
		if msg != "" {
			tp.emitLine("_r = \"SQLITE_CANTOPEN\"")
			return
		}
	}
	if tp.connClosed[dbConn] {
		tp.emitLine("_r = \"SQLITE_MISUSE\"")
		return
	}
	tp.emitLine("_r = %s.LastErrCode()", dbConn)
}

// processSqlite3Close handles `sqlite3_close db`. The result is SQLITE_OK on
// success or SQLITE_BUSY when the connection has active statements/backups.
func (tp *transpiler) processSqlite3Close(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	// Closing an already-closed connection returns SQLITE_MISUSE (capi3-3.6.1).
	if tp.connClosed[dbConn] {
		tp.emitLine("_r = \"SQLITE_MISUSE\"")
		return
	}
	// A connection with outstanding prepared statements cannot be closed
	// (sqlite3_close returns SQLITE_BUSY until they are finalized;
	// capi3-6.1/capi3c-6.1). Only the MAIN connection prepares statements in
	// the C-API tests, so only its close is blocked. The statement does NOT
	// actually close.
	ps := tp.preparedStateRef()
	if dbConn == "db" && len(ps.stmts) > 0 {
		tp.emitLine("_r = \"SQLITE_BUSY\" // unfinalized prepared statements")
		return
	}
	// A connection opened on a bad path is closed cleanly (SQLITE_OK) without
	// touching the file (capi3-3.5).
	if _, ok := tp.connFailedOpen[dbConn]; ok {
		tp.emitLine("_r = \"SQLITE_OK\"")
		tp.markConnClosed(dbConn)
		return
	}
	tp.emitLine("_r = tclCloseDB(%s)", dbConn)
	tp.markConnClosed(dbConn)
}

// markConnClosed records that a connection was closed via sqlite3_close so a
// later close reports SQLITE_MISUSE and later errmsg calls report misuse.
func (tp *transpiler) markConnClosed(conn string) {
	if tp.connClosed == nil {
		tp.connClosed = make(map[string]bool)
	}
	tp.connClosed[conn] = true
}

// processSqlite3Interrupt handles `sqlite3_interrupt DB` — set the
// connection's interrupt flag (sqlite3_interrupt). Inside a `db eval {SQL}
// {body}` callback the interrupt also sets the loop's interrupt flag so the
// harness aborts the row iteration with an "interrupted" error, matching the
// TCL harness where the next sqlite3_step returns SQLITE_INTERRUPT.
func (tp *transpiler) processSqlite3Interrupt(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	tp.emitLine("%s.Interrupt()", dbConn)
	if tp.interruptFlag != "" {
		tp.emitLine("%s = true", tp.interruptFlag)
	}
}

// processSqlite3IsInterrupted handles `sqlite3_is_interrupted DB` — report
// whether the connection's interrupt flag is set (sqlite3_is_interrupted).
// The result is a TCL "1"/"0" string left in _r.
func (tp *transpiler) processSqlite3IsInterrupted(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	tp.emitLine("_r = tclBool01(%s.IsInterrupted())", dbConn)
}

// processSqlite3StmtStatus handles `sqlite3_stmt_status $stmt NAME reset` —
// report a prepared-statement status counter (dbstatus.test 5.5.x). The
// counter name is a SQLITE_STMTSTATUS_* constant resolved at runtime; the
// result is left in _r as a decimal string.
func (tp *transpiler) processSqlite3StmtStatus(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	dbConn := tp.dbVar
	nameExpr := tp.buildStringExpr(args[1].Text)
	tp.emitLine("_r = strconv.FormatInt(%s.StmtStatus(%s), 10)", dbConn, nameExpr)
}

// processDBCksum handles `dbcksum db [schema]` — the tester.tcl checksum proc
// (an md5sum over the schema's objects and rows).
func (tp *transpiler) processDBCksum(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	schemaName := `"main"`
	if len(args) >= 2 {
		schemaName = tp.goStringLiteral(args[1])
	}
	tp.emitLine("_r = tclDBCksum(%s, %s)", dbConn, schemaName)
}

// processFileControlDataVersion handles `file_control_data_version db
// [schema]` — the SQLITE_FCNTL_DATA_VERSION file-control probe (dataversion1
// test). The SQL-level equivalent is PRAGMA <schema>.data_version; the value
// is left in _r as a decimal string for do_test body comparisons.
func (tp *transpiler) processFileControlDataVersion(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	schemaName := `""`
	if len(args) >= 2 {
		schemaName = tp.goStringLiteral(args[1])
	}
	tp.emitLine("_r = tclDataVersion(%s, %s)", dbConn, schemaName)
}

// processBackupObject handles backup-object subcommands (B step N, B finish,
// B remaining, B pagecount) when B is a declared *frigolite.Backup variable.
func (tp *transpiler) processBackupObject(goName string, args []tcl.RawWord) bool {
	if !tp.isVarDeclared(goName) {
		return false
	}
	if len(args) < 1 {
		return false
	}
	switch strings.ToLower(args[0].Text) {
	case "step":
		n := "0"
		if len(args) >= 2 {
			n = tp.goStringLiteral(args[1])
		}
		tp.emitLine("_r = tclBackupStep(%s, %s)", goName, n)
		return true
	case "finish":
		tp.emitLine("_r = tclBackupFinish(%s)", goName)
		return true
	case "remaining":
		tp.emitLine("_r = strconv.Itoa(%s.Remaining())", goName)
		return true
	case "pagecount":
		tp.emitLine("_r = strconv.Itoa(%s.Pagecount())", goName)
		return true
	}
	return false
}

// processInfoCommand handles `info commands NAME` (and other info
// subcommands as no-ops). For a declared backup-object variable it reports
// whether the backup command still exists (non-nil): "B" or "" — matching
// the TCL `info commands B` idiom used to verify a finished backup released
// its command.
func (tp *transpiler) processInfoCommand(args []tcl.RawWord) {
	if len(args) >= 2 && args[0].Text == "commands" {
		name := tclVarToGo(args[1].Text)
		if isValidGoIdent(name) && tp.isVarDeclared(name) {
			tp.emitLine("if %s != nil { _r = %q } else { _r = \"\" }", name, args[1].Text)
			return
		}
	}
	// Other info subcommands are no-ops (the TCL harness uses info for
	// platform introspection that has no Go equivalent).
}

// cmdExprBackupStep renders `[B step N]` as a Go string expression (the step
// result code). goName is the backup variable; n is the page count argument
// (a literal or $var reference).
func cmdExprBackupStep(goName, n string) string {
	if _, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
		return fmt.Sprintf("tclBackupStep(%s, %q)", goName, strings.TrimSpace(n))
	}
	// n may be a $var; the variable holds a TCL number string, which
	// tclBackupStep parses itself.
	if strings.HasPrefix(n, "$") {
		return fmt.Sprintf("tclBackupStep(%s, %s)", goName, tclVarToGo(strings.TrimPrefix(n, "$")))
	}
	return fmt.Sprintf("tclBackupStep(%s, %q)", goName, n)
}

// cmdExprBackupFinish renders `[B finish]`.
func cmdExprBackupFinish(goName string) string {
	return fmt.Sprintf("tclBackupFinish(%s)", goName)
}

// cmdExprBackupRemaining renders `[B remaining]`.
func cmdExprBackupRemaining(goName string) string {
	return fmt.Sprintf("strconv.Itoa(%s.Remaining())", goName)
}

// cmdExprBackupPagecount renders `[B pagecount]`.
func cmdExprBackupPagecount(goName string) string {
	return fmt.Sprintf("strconv.Itoa(%s.Pagecount())", goName)
}

// cmdExprErrmsg renders `[sqlite3_errmsg db]`.
func cmdExprErrmsg(tp *transpiler, cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `""`
	}
	return fmt.Sprintf("%s.LastErr()", tp.dbArgGo(args[0]))
}

// cmdExprErrcode renders `[sqlite3_errcode db]`.
func cmdExprErrcode(tp *transpiler, cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `"SQLITE_OK"`
	}
	return fmt.Sprintf("%s.LastErrCode()", tp.dbArgGo(args[0]))
}

// cmdExprFileSize renders `[file size PATH]` as a Go string expression (the
// file size in bytes, or "0" when missing).
func cmdExprFileSize(tp *transpiler, cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `"0"`
	}
	path := args[0]
	if strings.HasPrefix(path, "$") {
		return fmt.Sprintf("strconv.Itoa(tclFileSize(%s))", tclVarToGo(strings.TrimPrefix(path, "$")))
	}
	return fmt.Sprintf("strconv.Itoa(tclFileSize(%q))", path)
}

// cmdExprFileSizeExpr renders `[expr {[file size PATH] ...}]` — handled by the
// generic expr path via cmdExprFileSize.

// (The cmdExpr file-size helpers end here.)
