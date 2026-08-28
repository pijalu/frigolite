// Package main implements the tcl2go tool.
//
// This file handles sqlite3 / bind / step / reset / finalize commands.
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// processSqlite3 handles: sqlite3 dbName [filename]
// Opens a new database connection. The Go equivalent is frigolite.Open().
// The dbName is the TCL variable name for this connection.
func (tp *transpiler) processSqlite3(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	dbName := args[0].Text
	filename := `""`
	if len(args) >= 2 {
		filename = tp.goStringLiteral(args[1])
	}

	// A dynamic connection name (`sqlite3 $con test.db` where con HOLDS the
	// TCL connection name): the variable is a plain string, so no Go variable
	// can receive the handle. Open into a throwaway handle to preserve the
	// SQL side effect (the file is created/opened), matching the observable
	// filesystem state of the TCL run.
	if strings.HasPrefix(dbName, "$") {
		tp.emitLine("// sqlite3 %s %s (dynamic connection name)", sanitizeTCLComment(dbName), strings.TrimSpace(args[len(args)-1].Text))
		tp.emitLine("_dbtmp%d, err := frigolite.Open(%s)", tp.varCount, filename)
		tp.emitLine("if err != nil { t.Logf(\"open dynamic connection failed: %%v (not fatal)\", err) }")
		tp.emitLine("_ = _dbtmp%d", tp.varCount)
		tp.varCount++
		return
	}

	goName := tclVarToGo(dbName)

	// A URI-mode open (file:test.db?mode=ro/rw/rwc) probes the C-API URI
	// filename feature, which the pure-Go engine does not implement. Emit a
	// no-op so the test still compiles (the URI-mode assertions are skipped
	// by the do_test body detection).
	if len(args) >= 2 && strings.Contains(args[1].Text, "?mode=") {
		tp.emitLine("// sqlite3 %s %s (URI-mode open not implemented)", dbName, sanitizeTCLComment(args[1].Text))
		if !tp.isVarDeclared(goName) {
			tp.emitLine("var %s *frigolite.DB", goName)
			tp.vars = append(tp.vars, goName)
		}
		return
	}

	// Secondary connections opened on the main test database file
	// ("sqlite3 db2 test.db") are real independent connections in the TCL
	// framework. The transpiler runs against real files (testgen mode: the
	// main "db" is frigolite.Open("test.db")), so a real second connection
	// sees the same committed state and supports cross-connection scenarios
	// (e.g. attach2-4.1 attaches the same file under the same name on two
	// connections). No alias is emitted; the connection is opened normally
	// below.
	if goName != "db" && len(args) >= 2 && isMainTestFile(args[1].Text) {
		// Fall through to the normal open path below (real connection).
		// Keep any prior alias bookkeeping consistent.
		tp.clearSecondaryAlias(goName)
	}

	// A secondary connection reopened on a DIFFERENT file clears any prior
	// alias to the main connection (the TCL suite may open db2 on test.db
	// for shared-cache tests, then reopen it on another file later).
	if goName != "db" {
		tp.clearSecondaryAlias(goName)
	}

	// emitFatal reports whether the caller emits the t.Fatal(err) check
	// (the already-declared tmp-var branch returns early instead).
	rawFilename := ""
	if len(args) >= 2 {
		rawFilename = args[1].Text
	}
	emitFatal := tp.emitSqlite3Open(dbName, goName, filename, rawFilename, args)
	if emitFatal {
		tp.emitLine("if err != nil { t.Fatal(err) }")
	}
}

// processSqlite3Exec handles the TCL test-harness `sqlite3_exec db {SQL}`
// command: percent-decode %XX in the SQL (badutf/badutf2 embed raw bytes),
// execute it, and leave the "{code {headers values}}" result in _r for the
// do_test body comparison.
func (tp *transpiler) processSqlite3Exec(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	dbConn := tp.dbArgGo(args[0].Text)
	var sqlExpr string
	sqlArg := args[1].Text
	if strings.HasPrefix(strings.TrimSpace(sqlArg), "$") {
		gv := tclVarToGo(strings.TrimPrefix(strings.TrimSpace(sqlArg), "$"))
		if isValidGoIdent(gv) {
			sqlExpr = gv
		}
	}
	if sqlExpr == "" {
		sqlExpr = tp.goStringLiteral(args[1])
	}
	tp.emitLine("_r = tclExec(%s, %s)", dbConn, sqlExpr)
}

// clearSecondaryAlias removes a secondary connection from the alias map,
// resetting the map to nil when it becomes empty.
func (tp *transpiler) clearSecondaryAlias(goName string) {
	if tp.dbAliases == nil {
		return
	}
	if _, wasAliased := tp.dbAliases[goName]; !wasAliased {
		return
	}
	delete(tp.dbAliases, goName)
	if len(tp.dbAliases) == 0 {
		tp.dbAliases = nil
	}
}

// emitSqlite3Open emits the frigolite.Open call for a sqlite3 connection,
// dispatching on the connection kind (predeclared dbN, main db reset modes,
// new variable, closed-then-reopen, or already-declared). Returns true when
// the caller should emit the trailing t.Fatal(err) check.
func (tp *transpiler) emitSqlite3Open(dbName, goName, filename, rawFilename string, args []tcl.RawWord) bool {
	// Record that goName holds a *frigolite.DB connection so execsql/db
	// dispatch resolves it as a connection rather than a string variable.
	wasOpened := tp.dbConnVars[goName]
	if tp.dbConnVars == nil {
		tp.dbConnVars = make(map[string]bool)
	}
	tp.dbConnVars[goName] = true
	// db1-db9 are pre-declared at function level; always use = for them
	if isPreDeclaredDB(goName) {
		tp.emitLine("%s, err = frigolite.Open(%s)", goName, filename)
		return true
	}
	if goName == "db" && (filename == `""` || filename == `":memory:"` || filename == `"'':memory:''"`) {
		// SQLite's "db close; sqlite3 db :memory:" resets the main test
		// connection to a fresh database (dropping all prior tables).
		// Reopen it empty. (The preceding "db close" already emitted Close.)
		tp.emitLine("db, err = frigolite.Open(\"\")")
		tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
		tp.dqsDML = true
		return true
	}
	if goName == "db" && len(args) >= 2 && tp.pendingFileReset[args[1].Text] {
		// "forcedelete test.db; sqlite3 db test.db": start from a fresh
		// database on the real file (deleted by forcedelete, recreated
		// empty by the reopen). Reopening on the actual filename matters:
		// a later "db close; sqlite3 db test.db" must find writes made
		// after the reset, matching SQLite's file-based close+reopen
		// semantics (see default-4.0/default-4.1).
		delete(tp.pendingFileReset, args[1].Text)
		tp.emitLine("db, err = frigolite.Open(%s)", filename)
		tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
		tp.dqsDML = true
		return true
	}
	if !tp.isVarDeclared(goName) {
		// New DB connection variable
		tp.emitLine("%s, err := frigolite.Open(%s)", goName, filename)
		tp.emitLine("defer %s.Close()", goName)
		tp.vars = append(tp.vars, goName)
		return true
	}
	// A named connection pre-declared in the preamble (sqlite3 tmp "") that
	// has not been opened yet: emit a real open with assignment. The var is in
	// tp.vars (predeclared) but not in dbConnVars (never opened).
	if goName != "db" && !isPreDeclaredDB(goName) && tp.isVarDeclared(goName) && !wasOpened {
		tp.emitLine("%s, err = frigolite.Open(%s)", goName, filename)
		tp.emitLine("if err != nil { t.Fatal(err) }")
		return true
	}
	if goName == "db" && tp.dbClosed {
		// "db close" then "sqlite3 db <file>": the main connection was
		// closed, so reopen it on the same file so prior writes persist
		// (matching SQLite's close+reopen semantics). The compat suite
		// runs in-memory; the filename keeps the logical database alive.
		tp.emitLine("db, err = frigolite.Open(%s)", filename)
		tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
		tp.dqsDML = true
		tp.dbClosed = false
		return true
	}
	// Variable already declared (possibly as string from set) —
	// use a temp variable to avoid type conflicts. Reopening a FILE
	// database ("sqlite3 db test.db") is a no-op: the compat suite
	// expects the test to keep running in-memory, and forcedelete
	// emits os.Remove for explicit resets. Inside an eval-inlined script
	// (backup.test's `eval $zOpenScript` with `sqlite3 db $zSrcFile`), or
	// when the filename is a variable (backup-10's `sqlite3 db $file` in a
	// foreach), the reopen must create a FRESH connection (the TCL replaces
	// the connection), so emit a real close+reopen.
	if goName == "db" && (tp.inEvalScript || strings.HasPrefix(strings.TrimSpace(rawFilename), "$")) {
		tp.emitLine("db.Close()")
		tp.emitLine("db, err = frigolite.Open(%s)", filename)
		tp.emitLine("if err != nil { t.Fatal(err) }")
		tp.dqsDDL = true
		tp.dqsDML = true
		return false
	}
	tmpVar := fmt.Sprintf("_dbtmp%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s, err := frigolite.Open(%s)", tmpVar, filename)
	tp.emitLine("_ = %s // sqlite3 db connection", tmpVar)
	tp.emitLine("if err != nil { t.Logf(\"open connection side effect failed: %%v (not fatal)\", err) }")
	tp.emitLine("_ = err")
	// Reopening the MAIN connection ("sqlite3 db test.db") creates a fresh
	// sqlite3 handle whose changes()/total_changes() counters start at zero
	// (e_totalchanges.test resets total_changes this way). The in-memory
	// engine keeps the same DB handle for schema/data continuity, so reset
	// the counters explicitly.
	if goName == "db" {
		tp.emitLine("db.ResetChangesCounters()")
	}
	return false
}

// debugTcl2go enables transpiler tracing when set (debug aid).
var debugTcl2go = os.Getenv("TCL2GO_DEBUG") != ""

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// bindCmdKind maps a sqlite3_bind_* command name to its tclBindStmt kind.
func bindCmdKind(cmdName string) string {
	switch cmdName {
	case "sqlite3_bind_null":
		return "null"
	case "sqlite3_bind_int", "sqlite3_bind_int64":
		return "int"
	case "sqlite3_bind_double":
		return "double"
	case "sqlite3_bind_blob":
		return "blob"
	default: // sqlite3_bind_text / sqlite3_bind_text16
		return "text"
	}
}

// processBind emits a runtime typed bind against the named prepared
// statement (tclBindStmt), preserving C-API semantics: typeof-preserving
// values, optional byte-length truncation for text, and SQLITE_RANGE errors
// recorded on the connection.
func (tp *transpiler) processBind(cmdName string, args []tcl.RawWord) {
	kind := bindCmdKind(cmdName)
	minArgs := 3
	if kind == "null" {
		minArgs = 2 // sqlite3_bind_null STMT IDX takes no value
	}
	if len(args) < minArgs {
		tp.emitLine("// %s (malformed)", cmdName)
		return
	}
	stmtVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	ps := tp.preparedStateRef()
	sql, known := ps.stmts[stmtVar]
	if !known {
		if debugTcl2go {
			fmt.Fprintf(os.Stderr, "DEBUG bind unknown: %q known=%v\n", stmtVar, keys(ps.stmts))
		}
		tp.emitLine("// %s $%s (unknown prepared statement)", cmdName, stmtVar)
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(args[1].Text))
	if err != nil {
		tp.emitLine("// %s $%s %s (non-numeric bind index)", cmdName, stmtVar, args[1].Text)
		return
	}
	conn := ps.conns[stmtVar]
	if conn == "" {
		conn = "db"
	}
	if !stmtVMEnabled() {
		// Legacy literal-recording emulation.
		lit := tp.bindValueSQL(cmdName, args[2].Text)
		if ps.binds[stmtVar] == nil {
			ps.binds[stmtVar] = make(map[int]string)
		}
		ps.binds[stmtVar][idx] = lit
		tp.emitLine("// %s $%s %d %s → %s", cmdName, stmtVar, idx, args[2].Text, lit)
		return
	}
	nlen := -1
	// sqlite3_bind_text takes an explicit byte count as a 4th argument.
	if len(args) >= 4 && kind == "text" {
		if n, nerr := strconv.Atoi(strings.TrimSpace(args[3].Text)); nerr == nil {
			nlen = n
		}
	}
	rawExpr := `""`
	if kind != "null" {
		rawExpr = tp.buildStringExpr(args[2].Text)
	}
	_ = sql
	tp.emitLine("_r = tclBindStmt(%s, %q, %d, %q, %s, %d)", conn, stmtVar, idx, kind, rawExpr, nlen)
}

// bindValueSQL renders a bound TCL value as a SQL literal for the INSERT
// emulation, matching SQLite's sqlite3_bind_* conversion rules:
//   - bind_double NaN converts to NULL (SQLite never stores NaN); +-Inf is
//     stored as REAL +-Inf (a 1e400 literal overflows to +Inf on parse).
//   - bind_null is NULL; bind_int/int64 is the numeric literal; bind_text is a
//     quoted string; bind_blob is an X'...' hex literal.
func (tp *transpiler) bindValueSQL(cmdName, val string) string {
	switch cmdName {
	case "sqlite3_bind_null":
		return "NULL"
	case "sqlite3_bind_int", "sqlite3_bind_int64":
		return val
	case "sqlite3_bind_text", "sqlite3_bind_text16":
		return "'" + strings.ReplaceAll(val, "'", "''") + "'"
	case "sqlite3_bind_blob":
		return "X'" + hex.EncodeToString([]byte(unescapeBareWord(val))) + "'"
	case "sqlite3_bind_double":
		switch strings.TrimSpace(val) {
		case "NaN", "-NaN", "NaN0", "-NaN0":
			return "NULL"
		case "+Inf", "Inf":
			return "1e400"
		case "-Inf":
			return "-1e400"
		}
		return val
	}
	return val
}

// processSqliteStepTCL handles bind.test's `sqlite_step STMT N VALS COLS`
// TCL wrapper proc over sqlite3_step.
func (tp *transpiler) processSqliteStepTCL(args []tcl.RawWord) {
	if !stmtVMEnabled() {
		tp.emitLine("// sqlite_step %s (unsupported command, not transpiled)", sanitizeTCLComment(describeArgsShort(args)))
		return
	}
	tp.processStep(args)
}

// processLegacyBind handles deprecated `sqlite_bind STMT INDEX VALUE TYPE`
// (test1.c test_bind): null / static (TCL-linked sqlite_static_bind_value) /
// normal (transient text) / blob10 (fixed 10-byte "abc\0xyz\0pq" text).
func (tp *transpiler) processLegacyBind(args []tcl.RawWord) {
	if len(args) < 4 {
		return
	}
	if !stmtVMEnabled() {
		// Legacy: record as an equivalent modern bind literal.
		kind := strings.ToLower(strings.TrimSpace(args[3].Text))
		cmd := "sqlite3_bind_text"
		switch kind {
		case "null":
			cmd = "sqlite3_bind_null"
		case "normal", "static":
			cmd = "sqlite3_bind_text"
		}
		tp.processBind(cmd, args[:3])
		return
	}
	stmtVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	ps := tp.preparedStateRef()
	sql, known := ps.stmts[stmtVar]
	if !known {
		tp.emitLine("// sqlite_bind $%s (unknown prepared statement)", stmtVar)
		return
	}
	_ = sql
	idx, err := strconv.Atoi(strings.TrimSpace(args[1].Text))
	if err != nil {
		tp.emitLine("// sqlite_bind $%s %s (non-numeric index)", stmtVar, args[1].Text)
		return
	}
	conn := ps.conns[stmtVar]
	if conn == "" {
		conn = "db"
	}
	switch strings.ToLower(strings.TrimSpace(args[3].Text)) {
	case "null":
		tp.emitLine("_r = tclBindStmt(%s, %q, %d, \"null\", \"\", -1)", conn, stmtVar, idx)
	case "static":
		// The TCL-linked static buffer: bound as text from the Go variable
		// mirroring ::sqlite_static_bind_value.
		staticVar := tclVarToGo("sqlite_static_bind_value")
		tp.emitLine("_r = tclBindStmt(%s, %q, %d, \"text\", %s, -1)", conn, stmtVar, idx, staticVar)
	case "blob10":
		tp.emitLine("_r = tclBindStmt(%s, %q, %d, \"blob10\", %q, 10)", conn, stmtVar, idx, args[2].Text)
	default: // normal: transient text of VALUE
		rawExpr := tp.buildStringExpr(args[2].Text)
		tp.emitLine("_r = tclBindStmt(%s, %q, %d, \"text\", %s, -1)", conn, stmtVar, idx, rawExpr)
	}
}

// processTransferBindings copies source statement bindings to destination.
func (tp *transpiler) processTransferBindings(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	ps := tp.preparedStateRef()
	src := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	dst := tclVarToGo(strings.TrimPrefix(args[1].Text, "$"))
	if b := ps.binds[src]; b != nil {
		ps.binds[dst] = make(map[int]string, len(b))
		for i, v := range b {
			ps.binds[dst][i] = v
		}
	}
}

// processStep steps the named prepared statement (sqlite3_step) through the
// runtime Stmt emulation, emitting the result code into _r. Statements whose
// SQL text is a TCL variable fall back to the SQL side-effect path.
func (tp *transpiler) processStep(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	stmtVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	ps := tp.preparedStateRef()
	sql, ok := ps.stmts[stmtVar]
	if !ok {
		tp.emitLine("// sqlite3_step $%s (unknown prepared statement)", stmtVar)
		return
	}
	if stmtVMEnabled() {
		conn := ps.conns[stmtVar]
		if conn == "" {
			conn = "db"
		}
		tp.emitLine("_r = tclStepStmt(%s, %q)", conn, stmtVar)
		return
	}
	// Legacy: substitute recorded literal binds into the SQL text and run it.
	rendered := renderPreparedSQL(sql, ps.binds[stmtVar])
	sqlExpr := fmt.Sprintf("%q", rendered)
	if strings.HasPrefix(rendered, "$") {
		gv := tclVarToGo(strings.TrimPrefix(rendered, "$"))
		if isValidGoIdent(gv) {
			sqlExpr = gv
		}
	}
	// A prepared ATTACH whose database name is a `file:` URI (e_uri.test)
	// probes C-API URI filename handling; detach first so re-running the
	// ATTACH stays idempotent.
	if strings.HasPrefix(strings.TrimSpace(rendered), "ATTACH") {
		if m := attachDBNames(rendered); len(m) > 0 {
			tp.emitLine("_res = db.Exec(\"DETACH %s\")", m[len(m)-1])
			tp.emitLine("_ = _res // tolerate not-attached")
		}
	}
	conn := "db"
	if c := ps.conns[stmtVar]; c != "" {
		conn = c
	}
	tp.emitLine("_res = %s.Exec(%s)", conn, sqlExpr)
	tp.emitLine("if _res.Error != nil { %s.SetLastErr(_res.Error.Error(), %s.ErrorCodeFor(_res.Error)) }", conn, conn)
	tp.emitLine("_ = _res // step result (SQLITE_ROW/SQLITE_CONSTRAINT) is C-API state; side effect only")
}

// processReset resets the prepared statement (sqlite3_reset), emitting the
// result code (always SQLITE_OK for a live statement).
func (tp *transpiler) processReset(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	stmtVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	ps := tp.preparedStateRef()
	delete(ps.binds, stmtVar)
	if stmtVMEnabled() {
		tp.emitLine("_r = tclResetStmtCode(%q)", stmtVar)
		return
	}
	tp.emitLine("tclResetPrepared(%q)", stmtVar)
	tp.emitLine("// sqlite3_reset $%s", stmtVar)
}

// tclFinalizeStmt is declared in the helpers template; the transpiler emits
// calls to it from processFinalize.

// processFinalize finalizes the named prepared statement (sqlite3_finalize),
// emitting its result code: SQLITE_OK, or the re-reported error of the
// statement's most recent failed step (vdbeapi.c sqlite3VdbeFinalize).
func (tp *transpiler) processFinalize(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	stmtVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	ps := tp.preparedStateRef()
	conn := ps.conns[stmtVar]
	if conn == "" {
		conn = "db"
	}
	delete(ps.stmts, stmtVar)
	delete(ps.binds, stmtVar)
	if !stmtVMEnabled() {
		tp.emitLine("tclFinalizePrepared(%q)", stmtVar)
		tp.emitLine("// sqlite3_finalize $%s", stmtVar)
		return
	}
	tp.emitLine("_r = tclFinalizeStmt(%s, %q)", conn, stmtVar)
}

// stmtVarArg extracts the statement-registry name from args[0] ("$VM" → "VM").
func stmtVarArg(args []tcl.RawWord) string {
	if len(args) == 0 {
		return ""
	}
	return tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
}

// argAt returns args[i], or a zero RawWord when out of range.
func argAt(args []tcl.RawWord, i int) tcl.RawWord {
	if i < len(args) {
		return args[i]
	}
	return tcl.RawWord{}
}

// intArgExpr renders a TCL integer argument as a Go int expression.
func (tp *transpiler) intArgExpr(text string) string {
	t := strings.TrimSpace(text)
	if n, err := strconv.Atoi(t); err == nil {
		return strconv.Itoa(n)
	}
	if strings.HasPrefix(t, "$") {
		gv := tclVarToGo(strings.TrimPrefix(t, "$"))
		if isValidGoIdent(gv) {
			return "tclInt(" + gv + ")"
		}
	}
	return "0"
}

// processClearBindings handles sqlite3_clear_bindings STMT: drop all bound
// values; the next step sees NULLs (bind-13.4).
func (tp *transpiler) processClearBindings(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	if !stmtVMEnabled() {
		tp.emitLine("// sqlite3_clear_bindings %s (unsupported command, not transpiled)", sanitizeTCLComment(describeArgsShort(args)))
		return
	}
	stmtVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	tp.emitLine("_r = tclClearBindingsStmt(%q)", stmtVar)
}

// processCreateFunction handles `sqlite3_create_function $DB` (test1.c
// test_create_function), which registers the harness SQL functions
// x_coalesce/hex8/tkt2213func/... Only x_coalesce is exercised by the
// transpiled suite (bind-12.2); it behaves like ifnull over two arguments.
func (tp *transpiler) processCreateFunction(args []tcl.RawWord) {
	if !stmtVMEnabled() {
		tp.emitLine("// sqlite3_create_function $DB (unsupported command, not transpiled)")
		return
	}
	conn := "db"
	if len(args) > 0 {
		conn = tp.dbArgGo(args[0].Text)
	}
	tp.emitLine("%s.RegisterFunction(\"x_coalesce\", func(a []interface{}) (interface{}, error) {", conn)
	tp.emitLine("\tfor _, v := range a { if v != nil { return v, nil } }")
	tp.emitLine("\treturn nil, nil")
	tp.emitLine("}, 1, -1)")
}

// attachDBNames parses an ATTACH statement's database path and alias name
// (ATTACH 'file:path' AS aux), returning them in order (path, alias). Returns
// nil when the statement is not a well-formed ATTACH.
func attachDBNames(sql string) []string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	if !strings.HasPrefix(upper, "ATTACH") {
		return nil
	}
	rest := strings.TrimSpace(sql[len("ATTACH"):])
	// Optional DATABASE keyword.
	if strings.HasPrefix(strings.ToUpper(rest), "DATABASE") {
		rest = strings.TrimSpace(rest[len("DATABASE"):])
	}
	// Path: quoted string or bare word.
	var names []string
	if len(rest) >= 2 && rest[0] == '\'' {
		end := strings.Index(rest[1:], "'")
		if end < 0 {
			return nil
		}
		names = append(names, rest[1:end+1])
		rest = strings.TrimSpace(rest[end+2:])
	} else {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil
		}
		names = append(names, fields[0])
		rest = strings.TrimSpace(rest[len(fields[0]):])
	}
	// AS alias (optional keyword AS).
	upperRest := strings.ToUpper(rest)
	if strings.HasPrefix(upperRest, "AS") {
		rest = strings.TrimSpace(rest[2:])
	}
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		names = append(names, fields[0])
	}
	return names
}

// renderPreparedSQL replaces ? placeholders in a prepared statement's SQL with
// the bound SQL literals (in bind-index order). A literal ?NNN (numbered
// placeholder) maps to the N-th bound value, matching SQLite's binding rules.
func renderPreparedSQL(sql string, binds map[int]string) string {
	if len(binds) == 0 {
		return sql
	}
	var out strings.Builder
	i := 0
	for i < len(sql) {
		if repl, next, ok := renderBindAt(sql, i, binds); ok {
			out.WriteString(repl)
			i = next
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String()
}

// renderBindAt resolves a ? placeholder at position i in prepared SQL against
// the bind map. Numbered ?NNN maps to the N-th bound value; a plain ? uses the
// first bind (positional order). Returns the replacement, the next index, and
// whether a placeholder was matched.
func renderBindAt(sql string, i int, binds map[int]string) (string, int, bool) {
	if sql[i] == '?' && i+1 < len(sql) && isDigit(sql[i+1]) {
		k := i + 1
		for k < len(sql) && isDigit(sql[k]) {
			k++
		}
		n, _ := strconv.Atoi(sql[i+1 : k])
		if lit, ok := binds[n]; ok {
			return lit, k, true
		}
		return sql[i:k], k, true
	}
	if sql[i] == '?' {
		// Positional ? binds in order. Prepared statements in the TCL
		// tests use a single ? (bind index 1) almost exclusively, so
		// substitute the first bind for each ?.
		if lit, ok := binds[1]; ok {
			return lit, i + 1, true
		}
		return "", i + 1, true
	}
	return "", 0, false
}

// emitUnsupportedStmtCmd emits the historical unsupported-command comment for
// prepared-statement commands outside the Stmt-VM files.
func (tp *transpiler) emitUnsupportedStmtCmd(cmdName string, args []tcl.RawWord) {
	if len(args) > 0 {
		tp.emitLine("// %s %s (unsupported command, not transpiled)", cmdName, sanitizeTCLComment(describeArgsShort(args)))
		return
	}
	tp.emitLine("// %s (unsupported command, not transpiled)", cmdName)
}
