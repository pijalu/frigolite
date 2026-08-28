// Package main implements the tcl2go tool.
//
// This file handles execsql / db commands (processExecSQL, processDB,
// processDBForName).
package main

import (
	"fmt"
	"regexp"
	"strconv"
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
	dbConn := "db"
	connIdx := sqlConnectionIndex(args)
	if connIdx >= 0 && connIdx < len(args) {
		h := tclVarToGo(args[connIdx].Text)
		// A TCL variable shadowed by a foreach loop var (e.g. `foreach db
		// {db db2}` renames db→db_iter): resolve the reference through the
		// rename map.
		if renamed, ok := tp.varRenames[h]; ok {
			h = renamed
		}
		// A runtime connection-name variable (foreach db {db db2} loop var):
		// dispatch through tclConnByName at runtime.
		if h != "" && tp.runtimeConnVars[h] {
			return "tclConnByName(" + h + ", db, db1, db2, db3, db4, db5, db6, db7, db8, db9)"
		}
		isConn := h == "db" || isPreDeclaredDB(h) || tp.dbConnVars[h]
		if h != "" && h != "db" && isConn {
			if target, ok := tp.dbAliases[h]; ok {
				dbConn = target
			} else {
				dbConn = h
			}
		} else if h != "" && h != "db" {
			// The argument is a variable (e.g. `set db_dest db2` then
			// `execsql {SQL} $db_dest`); resolve its constant value to a
			// connection name when it names a declared DB variable.
			if conn, ok := tp.varConstValues[h]; ok {
				connGo := tclVarToGo(conn)
				if connGo != "" && connGo != "db" && (isPreDeclaredDB(connGo) || tp.dbConnVars[connGo]) {
					dbConn = connGo
				} else if connGo == "db" {
					dbConn = "db"
				}
			}
		}
	}
	return dbConn
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

func (tp *transpiler) processDB(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	subCmd := args[0].Text
	rest := args[1:]

	switch subCmd {
	case "close":
		tp.processDBClose()
	case "backup":
		tp.processDBBackupRestore("backup", rest)
	case "restore":
		tp.processDBBackupRestore("restore", rest)
	case "null", "nullvalue":
		// TCL "db null <value>" / "db nullvalue <value>" sets how SQL NULL
		// renders in query results.
		if len(rest) >= 1 {
			tp.emitLine("tcl_nullvalue = %s", tp.goStringLiteral(rest[0]))
		}
	case "eval":
		tp.processDBEval(rest)
	case "onecolumn":
		tp.processDBOnecolumn(rest)
	case "transaction":
		tp.processDBTransaction(rest)
	case "function", "func":
		tp.processDBFunction(rest)
	case "collate":
		tp.processDBCollate(rest)
	case "deserialize":
		tp.processDBDeserialize(rest)
	case "progress":
		tp.processDBProgress(rest)
	case "authorizer":
		tp.processNamedDBAuthorizer("db", rest)
	case "incrblob":
		tp.processDBIncrblob(rest)
	case "changes":
		tp.emitLine("_r = strconv.FormatInt(db.Changes(), 10)")
	case "total_changes":
		tp.emitLine("_r = strconv.FormatInt(db.TotalChanges(), 10)")
	case "preupdate":
		tp.processDBPreupdate(rest)
	case "commit_hook":
		tp.processDBCommitHook(rest)
	case "rollback_hook":
		tp.processDBRollbackHook(rest)
	case "update_hook":
		tp.processDBUpdateHook(rest)
	case "complete":
		// db complete {SQL} — sqlite3_complete test: returns 1 when the SQL
		// ends in a complete statement (semicolon outside strings/comments,
		// trigger-aware ";END;" detection), 0 otherwise. Mirrors src/complete.c.
		// Unlike execsql, db complete's braced argument is a LITERAL SQL string,
		// not a substituted one, so $var inside it is NOT a TCL substitution.
		if len(rest) >= 1 {
			sqlExpr := tp.goStringLiteral(rest[0])
			tp.emitLine("_r = tclBool01(db.Complete(%s))", sqlExpr)
		}
	default:
		// no-op for other db subcommands
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

// processDBDeserialize handles `db deserialize [decode_hexdb {...}]`: it
// builds the database image from the hexdb block, writes it to a temp file,
// and reopens the connection on that file. The hexdb format is the .open
// --hexdb dump produced by sqlite3 (each line is `| <offset>: <hex bytes>`
// grouped by `| page N offset M`).
func (tp *transpiler) processDBDeserialize(rest []tcl.RawWord) {
	if len(rest) < 1 {
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

// extractHexdbBlock pulls the braced block out of `[decode_hexdb {...}]`.
func extractHexdbBlock(text string) string {
	idx := strings.Index(text, "{")
	if idx < 0 {
		return ""
	}
	depth := 0
	for i := idx; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[idx+1 : i]
			}
		}
	}
	return text[idx+1:]
}

// parseHexdbImage converts an .open --hexdb block into the raw database bytes.
// Lines are `| <offset>: <hex bytes>  <ascii>` grouped by `| page N offset M`.
func parseHexdbImage(hexdb string) ([]byte, error) {
	// The header lines carry the total size; pages fill the rest.
	size := 0
	pageSize := 4096
	for _, line := range strings.Split(hexdb, "\n") {
		line = strings.TrimSpace(line)
		if m := hexdbKV(line, "size"); m != "" {
			size, _ = strconv.Atoi(m)
		}
		if m := hexdbKV(line, "pagesize"); m != "" {
			pageSize, _ = strconv.Atoi(m)
		}
	}
	if size <= 0 {
		size = pageSize // fall back to one page
	}
	out := make([]byte, size)
	curPage := 0
	for _, line := range strings.Split(hexdb, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		rest := strings.TrimSpace(trimmed[1:])
		if strings.HasPrefix(rest, "page ") {
			// page N offset M
			fields := strings.Fields(rest)
			if len(fields) >= 2 {
				curPage, _ = strconv.Atoi(fields[1])
			}
			continue
		}
		// <offset>: <hex bytes>
		colon := strings.Index(rest, ":")
		if colon < 0 {
			continue
		}
		off, _ := strconv.Atoi(strings.TrimSpace(rest[:colon]))
		hexPart := rest[colon+1:]
		// Hex bytes are the first 2-char groups; strip the ascii column.
		pairs := hexBytePairs(hexPart)
		for i, b := range pairs {
			pos := (curPage-1)*pageSize + off + i
			if pos < len(out) {
				out[pos] = b
			}
		}
	}
	return out, nil
}

// hexdbKV extracts a `key value` pair from an .open header line like
// `| size 24576 pagesize 4096 filename x`.
func hexdbKV(line, key string) string {
	for i := 0; i+len(key) <= len(line); i++ {
		if line[i:i+len(key)] == key {
			j := i + len(key)
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			k := j
			for k < len(line) && line[k] != ' ' && line[k] != '\t' {
				k++
			}
			return line[j:k]
		}
	}
	return ""
}

// hexBytePairs extracts 2-hex-digit byte values from a hex dump line (the
// part before the ASCII column).
func hexBytePairs(s string) []byte {
	var out []byte
	for i := 0; i+1 < len(s); i++ {
		if isHexByte(s[i]) && isHexByte(s[i+1]) {
			if i+2 < len(s) && isHexByte(s[i+2]) {
				// A triple of hex digits is a stray run (ASCII column); stop.
				break
			}
			b := byte((hexByteVal(s[i]) << 4) | hexByteVal(s[i+1]))
			out = append(out, b)
			i++
		}
	}
	return out
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexByteVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// processDBEval handles `db eval {SQL}` and the row-callback form
// `db eval {SQL} {body}`.
func (tp *transpiler) processDBEval(rest []tcl.RawWord) {
	// db eval {SQL} ARRAYVAR: TCL populates the array variable with the
	// first row's column names/values (column name is the key). The harness
	// checks array keys via `set A(*)` which becomes the expected column names.
	// For `db eval {SQL} A` where A is a known array variable, capture the
	// query's column names into A_arr for the subsequent `set A(*)` check.
	if len(rest) >= 2 && !rest[1].Braced && rest[1].Text != "" {
		arrName := rest[1].Text
		goName := tclVarToGo(arrName)
		if goName != "" {
			// Known array — capture column names (arrayKeys may be nil if declared in outer scope; still handle).
			isArr := false
			if tp.arrayKeys != nil {
				_, isArr = tp.arrayKeys[arrName]
			}
			// Also treat single-letter array vars like A as arrays even if not pre-registered
			if isArr || (len(arrName) == 1 && arrName[0] >= 'A' && arrName[0] <= 'Z') {
				// Array + BODY form (`db eval $sql X { ... }`): each row sets
				// X(column) for every result column, then the body runs —
				// once per row (fts3sort.test).
				if len(rest) >= 3 && rest[2].Braced {
					tp.emitDBEvalArrayRows(arrName, rest)
					return
				}
				sqlExpr := tp.collectSQLExpression(rest[:1])
				if sqlExpr != `""` {
					tp.emitLine("r = db.Query(%s)", sqlExpr)
					tp.emitLine("if r.Error != nil {")
					tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
					tp.emitLine("}")
					arrStar := tclVarToGo(arrName + "(*)")
					if !tp.isVarDeclared(arrStar) {
						tp.emitLine("var %s string", arrStar)
						tp.vars = append(tp.vars, arrStar)
					}
					tp.emitLine("%s = strings.Join(r.Columns, \" \")", arrStar)
					tp.emitLine("_res = &frigolite.Result{Columns: r.Columns, Rows: r.Rows}")
					return
				}
			} else if len(rest) >= 3 && rest[2].Braced && isValidGoIdent(goName) {
				// `db eval SQL IDENT {BODY}` with an unregistered identifier:
				// TCL semantics bind IDENT(column) per row then run BODY
				// (lock.test's `db eval {SELECT ...} qv {set x ...}`).
				// Register as array and use the array-rows emitter.
				if tp.arrayKeys == nil {
					tp.arrayKeys = map[string][]string{}
				}
				tp.arrayKeys[arrName] = nil
				tp.emitDBEvalArrayRows(arrName, rest)
				return
			}
		}
		// Also handle the dynamic case: `db eval {SQL} $arrVar` or unknown array var.
		// Fall through to generic Exec path; the harness may check via different var.
	}
	// db eval {SQL} {body}: TCL's row-callback form. SQLite steps the
	// SELECT row by row, running the braced body for each row. A ROLLBACK
	// executed inside the body succeeds but aborts the active SELECT with
	// "abort due to ROLLBACK" (SQLite's sqlite3_step returns SQLITE_ABORT
	// after a ROLLBACK invalidates the statement). Other statements
	// (COMMIT, DML) inside the body do not abort the iteration.
	if len(rest) >= 2 && rest[1].Braced {
		tp.emitDBEvalCallback(rest)
		return
	}
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	sqlText := ""
	if len(rest) > 0 {
		sqlText = rest[0].Text
	}
	if reason := unsupportedSQL(sanitizeSQL(sqlText)); reason != "" {
		// A VACUUM mixed with other statements: the engine no-ops VACUUM
		// (P8.VACUUM), but the surrounding statements still have side
		// effects later tests depend on (e.g. nan-3.1's DELETE + INSERT
		// 0.5 before VACUUM). Split the body and run the non-VACUUM
		// statements. Keep the whole-block skip when the body contains a
		// row-producing query (SELECT/WITH/VALUES/EXPLAIN): its result
		// order/contents can be VACUUM-dependent (whereA-1.7) and running
		// it without VACUUM would produce a different value.
		if reVACUUM.MatchString(sqlText) && !bodyHasRowProducingQuery(sqlText) {
			var sideEffects []string
			for _, st := range splitSQLStatements(sqlText) {
				if !reVACUUM.MatchString(st) {
					sideEffects = append(sideEffects, st)
				}
			}
			if len(sideEffects) > 0 {
				tp.emitLine("_res = db.Exec(%q)", strings.Join(sideEffects, "; "))
				tp.emitLine("_ = _res // VACUUM skipped (P8.VACUUM); side effects run")
				return
			}
		}
		tp.emitLine("// db eval skipped: %s", reason)
		return
	}
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	if tp.rollbackFlag != "" && isRollbackStmt(sqlText) {
		// A ROLLBACK executed inside a db eval callback aborts the
		// enclosing row iteration (SQLite "abort due to ROLLBACK").
		tp.emitLine("%s = true", tp.rollbackFlag)
	}
	if tp.catchMode {
		tp.emitLine("if _res.Error != nil { _catchErr = _res.Error }")
	} else {
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
		tp.emitLine("}")
	}
}

// processDBOnecolumn handles `db onecolumn {SQL}`.
func (tp *transpiler) processDBOnecolumn(rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	tp.emitLine("r = db.Query(%s)", sqlExpr)
	if tp.catchMode {
		tp.emitLine("if r.Error != nil { _catchErr = r.Error }")
	} else {
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("}")
	}
}

// processDBTransaction handles `db transaction {BODY}` — transpile the body
// as regular code.
func (tp *transpiler) processDBTransaction(rest []tcl.RawWord) {
	if len(rest) == 0 || !rest[0].Braced {
		return
	}
	bodyCmds := parseCommands(rest[0].Text)
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		forIncrs:     tp.forIncrs,
		testPrefix:   tp.testPrefix, preparedState: tp.preparedState,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
}

// processDBFunction handles `db function NAME procName` / `db func NAME
// procName` — register a scalar SQL function whose behavior is a TCL proc.
func (tp *transpiler) processDBFunction(rest []tcl.RawWord) {
	if len(rest) < 2 {
		tp.emitDBVarFunc(rest)
		return
	}
	name := strings.TrimSpace(rest[0].Text)
	procName := procNameFromRest(rest)
	// `db func eval <proc>` where the proc runs SQL text (misc8.test's dbeval:
	// `proc dbeval {sql} { db eval $sql }`). The engine's built-in eval()
	// already runs SQL and returns the joined result (EvalExecSQL), so skip
	// the variable-reader stub registration — a nil-returning stub would
	// shadow the real eval and break DELETE/SELECT execution.
	if strings.EqualFold(name, "eval") {
		tp.emitLine("// db func eval %s (db-eval passthrough — built-in eval used)", procName)
		return
	}
	if tp.emitRegisteredFunction(name, procName, rest) {
		return
	}
	// `db func extract extract` (fts3offsets.test) — the proc annotates the
	// document text with parentheses at each offsets() hit span.
	if name == "extract" && procName == "extract" {
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tif len(args) < 2 { return \"\", nil }")
		tp.emitLine("\treturn tclExtractOffsets(tclStr(args[0]), tclStr(args[1])), nil")
		tp.emitLine("}, 2, 2)")
		return
	}
	// `db func blob blob` — the proc is a specialFunc (e.g. the test-harness
	// blob() hex decoder, fts3corrupt4). Emit a RegisterFunction whose body
	// decodes the argument.
	if tp.specialFuncs != nil {
		if tmpl, ok := tp.specialFuncs[procName]; ok && name != "" {
			if tmpl == "tclBlobHexDecode" {
				tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
				tp.emitLine("\tif len(args) < 1 || args[0] == nil { return []byte{}, nil }")
				// Engine-level BLOBs (zipfile() archive results feeding
				// remove_timestamps) arrive as []byte, not hex text.
				if procName == "remove_timestamps" {
					tp.emitLine("\tif b, ok := args[0].([]byte); ok { return tclRemoveTimestamps(b), nil }")
					tp.emitLine("\tif s, ok := args[0].(string); ok { return tclRemoveTimestamps(tclHexDecode(s)), nil }")
					tp.emitLine("\treturn tclHexDecode(tclStr(args[0])), nil")
				} else {
					tp.emitLine("\treturn tclHexDecode(tclStr(args[0])), nil")
				}
				tp.emitLine("}, 0, -1)")
				return
			}
			if tmpl == "tclMatchinfoDecode" {
				tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
				tp.emitLine("\tif len(args) < 1 || args[0] == nil { return \"\", nil }")
				tp.emitLine("\treturn tclMatchinfoDecode(args[0]), nil")
				tp.emitLine("}, 0, -1)")
				return
			}
			if tmpl == "tclFts3Record" {
				// `db func record make_record_wrapper` — the wrapper calls
				// make_fts3record $args: build an FTS3 segment record blob
				// from the SQL function's arguments (fts4record.test).
				tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
				tp.emitLine("\treturn tclFts3Record(args), nil")
				tp.emitLine("}, 0, -1)")
				return
			}
		}
	}
	// `db func swap_int32 swap_int32` / `db func set_int32 set_int32` —
	// rtreecheck.test's blob surgery over %_node data blobs (big-endian u32
	// word swap / overwrite). Real closures, not $data templates: both procs
	// take exactly three arguments.
	if procName == "swap_int32" && (name == procName || !strings.HasPrefix(name, "$")) {
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tb, err := tclSwapInt32Args(args, false)")
		tp.emitLine("\tif err != nil { return nil, err }")
		tp.emitLine("\treturn b, nil")
		tp.emitLine("}, 3, 3)")
		return
	}
	if procName == "set_int32" && (name == procName || !strings.HasPrefix(name, "$")) {
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tb, err := tclSwapInt32Args(args, true)")
		tp.emitLine("\tif err != nil { return nil, err }")
		tp.emitLine("\treturn b, nil")
		tp.emitLine("}, 3, 3)")
		return
	}
	// `db func $zip zip` — the name is a runtime variable (loop var holding
	// "zip" or "z.i.p!!") and the proc is the fts3comp1 compression harness.
	// Emit a stateful closure registered under the runtime variable value so
	// FTS4 compress='<value>' finds it (fts3comp1 1.x: content table stores
	// the integer keys returned by zip).
	if strings.HasPrefix(name, "$") && tp.zipUnzipFunc(procName) {
		if tp.specialFuncs != nil {
			if tmpl, ok := tp.specialFuncs[procName]; ok {
				tp.emitZipUnzipFunction(strings.TrimPrefix(name, "$"), procName, tmpl)
				return
			}
		}
	}
	tp.emitDBVarFunc(rest)
}

// procNameFromRest finds the TCL proc name in `db func NAME [-deterministic]
// PROC` — the first non-flag argument (a braced word like {joinx cross}
// contributes its first token).
func procNameFromRest(rest []tcl.RawWord) string {
	skipNext := false
	for _, a := range rest[1:] {
		arg := strings.TrimSpace(a.Text)
		if arg == "" {
			continue
		}
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			// Flags taking a separate value (-argcount 2) consume it.
			if !strings.Contains(arg, "=") {
				skipNext = true
			}
			continue
		}
		fields := strings.Fields(arg)
		if len(fields) == 0 {
			continue
		}
		return fields[0]
	}
	return ""
}

// emitRegisteredFunction emits a RegisterFunction call for a recognized
// test-suite proc pattern. Returns true when a pattern matched.
func (tp *transpiler) emitRegisteredFunction(name, procName string, rest []tcl.RawWord) bool {
	if tp.emitSleeperFunction(name, procName) {
		return true
	}
	if tp.emitConstFunction(name, procName) {
		return true
	}
	if tp.emitStringMapFunction(name, procName) {
		return true
	}
	if tp.emitIdentityFunction(name, procName, rest) {
		return true
	}
	if tp.emitLIndexFunction(name, procName) {
		return true
	}
	if tp.emitIncrRetFunction(name, procName, rest) {
		return true
	}
	if tp.emitCounterFunction(name, procName) {
		return true
	}
	// db func int2str int2str — the test-harness int2str builds a
	// 900-char deterministic string from its integer argument.
	if procName == "int2str" && name != "" {
		tp.emitInt2strFunction(name)
		return true
	}
	// db func NAME {joinx PREFIX} — the join proc is called with a
	// literal prefix plus the SQL arguments (func8.test's cross/full/
	// inner/... functions): cross(a,b,c) → "cross-a-b-c".
	if tp.emitJoinFunction(name, procName, rest) {
		return true
	}
	// db func my_changes my_changes — the e_changes.test harness proc:
	// `proc my_changes {x} { set res [db changes]; lappend ::changes $x
	// $res; return $res }`. The SQL function returns the connection's
	// changes() count and records the (arg, count) pair in the ::changes
	// TCL global (verified by do_test 5.1.2).
	if procName == "my_changes" && name != "" {
		tp.emitMyChangesFunction(name)
		return true
	}
	// db func NAME NAME — a prefix proc (window6.test's winproc):
	// window('hello world') → "window: hello world".
	if tp.emitPrefixFunction(name, procName) {
		return true
	}
	// Predicate proc: `proc myfunc {x} {expr $x < 10}` becomes a
	// scalar SQL function applying the comparison to its first
	// argument (numeric).
	if tp.emitPredFunction(name, procName) {
		return true
	}
	// Error-raising proc: `proc NAME {} { error "MSG" }` becomes a
	// scalar SQL function that returns the error (regexp2.test's
	// `proc sql_error {} { error "SQL error!" }` registered as
	// `db func error sql_error`).
	if msg, ok := tp.errorFuncs[procName]; ok && name != "" {
		tp.emitErrorFunction(name, msg)
		return true
	}
	return false
}

// emitSleeperFunction registers the sleeper proc (`proc sleeper {} {after
// 100}`), which pauses 100ms and returns NULL. It is used by date.test to
// verify that 'now' is cached per statement across a user-function sleep.
func (tp *transpiler) emitSleeperFunction(name, procName string) bool {
	if procName != "sleeper" || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { time.Sleep(100 * time.Millisecond); return nil, nil }, 0, -1)", tp.dbVar, name)
	return true
}

// emitMyChangesFunction registers the e_changes.test my_changes harness
// function: `proc my_changes {x} { set res [db changes]; lappend ::changes $x
// $res; return $res }`. The SQL function returns the connection's changes()
// count and appends "(arg, count)" to the ::changes TCL-global variable
// (verified by do_test 5.1.2). The Go variable for ::changes is `changes`.
func (tp *transpiler) emitMyChangesFunction(name string) {
	tp.emitLine("// db func %s: my_changes (returns db changes, logs to ::changes)", name)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tv := db.Changes()")
	tp.emitLine("\targ := \"\"")
	tp.emitLine("\tif len(args) > 0 { arg = tclStr(args[0]) }")
	tp.emitLine("\tchanges = tclListAppend(changes, arg, strconv.FormatInt(v, 10))")
	tp.emitLine("\treturn v, nil")
	tp.emitLine("}, 0, -1)")
}

// emitConstFunction registers a constant-returning proc as a scalar SQL
// function returning the constant.
func (tp *transpiler) emitConstFunction(name, procName string) bool {
	constVal, ok := tp.constFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return int64(%s), nil }, 0, -1)", tp.dbVar, name, constVal)
	return true
}

// emitIdentityFunction registers an identity proc (`proc NAME {x} {return
// $x}`) as a scalar SQL function returning its first argument. It honors the
// SQLite function-safety flags in `db function NAME [-innocuous]
// [-directonly] [-deterministic] PROC` by emitting RegisterFunctionFlags
// (trustschema1's f1/f2/f3).
func (tp *transpiler) emitIdentityFunction(name, procName string, rest []tcl.RawWord) bool {
	if !tp.identityFuncs[procName] || name == "" {
		return false
	}
	innocuous, directOnly := dbFunctionSafetyFlags(rest)
	flags := "false, false"
	if innocuous && !directOnly {
		flags = "true, false"
	} else if directOnly {
		flags = "false, true"
	}
	tp.emitLine("%s.RegisterFunctionFlags(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn args[0], nil")
	tp.emitLine("}, 0, -1, %s)", flags)
	return true
}

// dbFunctionSafetyFlags extracts the SQLite function-safety flags from a
// `db function NAME [flags] PROC` argument list: -innocuous and -directonly
// (SQLITE_INNOCUOUS / SQLITE_DIRECTONLY).
func dbFunctionSafetyFlags(rest []tcl.RawWord) (innocuous, directOnly bool) {
	for _, a := range rest[1:] {
		switch strings.ToLower(strings.TrimSpace(a.Text)) {
		case "-innocuous":
			innocuous = true
		case "-directonly":
			directOnly = true
		}
	}
	return innocuous, directOnly
}

// emitLIndexFunction registers a list-index proc (`proc NAME {x} { lindex $x
// N }`) as a scalar SQL function returning the N-th element of its first
// argument split as a TCL list (fts4growth.test's second: "0 114" → "114").
func (tp *transpiler) emitLIndexFunction(name, procName string) bool {
	idx, ok := tp.lindexFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn tclLIndex(tclStr(args[0]), %d), nil", idx)
	tp.emitLine("}, 0, -1)")
	return true
}

// emitStringMapFunction registers a string-map proc (`proc NAME {x} {
// return [string map {OLD NEW ...} $x] }`) as a scalar SQL function that
// applies each OLD→NEW replacement in order (fts4intck1.test's slang:
// th→d, e→eh makes 'the' → 'deh'). TCL string map applies left-to-right on
// the current value, so chained strings.ReplaceAll is faithful.
func (tp *transpiler) emitStringMapFunction(name, procName string) bool {
	pairs, ok := tp.stringMapFuncs[procName]
	if !ok || name == "" {
		return false
	}
	items := tclCmdWords(pairs)
	if len(items) < 2 || len(items)%2 != 0 {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\ts := tclStr(args[0])")
	for i := 0; i+1 < len(items); i += 2 {
		tp.emitLine("\ts = strings.ReplaceAll(s, %q, %q)", items[i], items[i+1])
	}
	tp.emitLine("\treturn s, nil")
	tp.emitLine("}, 0, -1)")
	return true
}

// emitCounterFunction registers a counter proc as a scalar SQL function that
// increments a dedicated Go counter var.
func (tp *transpiler) emitCounterFunction(name, procName string) bool {
	goVar, ok := tp.counterFuncs[procName]
	if !ok || name == "" {
		return false
	}
	counterVar := goVar + "Counter"
	tp.emitLine("var %s int64", counterVar)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { %s++; return %s, nil }, 0, -1)", tp.dbVar, name, counterVar, counterVar)
	return true
}

// emitJoinFunction registers a join proc called with a literal prefix plus
// the SQL arguments (func8.test's cross/full/inner/... functions).
func (tp *transpiler) emitJoinFunction(name, procName string, rest []tcl.RawWord) bool {
	sep, ok := tp.joinFuncs[procName]
	if !ok || name == "" {
		return false
	}
	prefix := joinPrefixFromRest(rest, procName)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tvar parts []string")
	tp.emitLine("\tparts = append(parts, %q)", prefix)
	tp.emitLine("\tfor _, a := range args { if a != nil { parts = append(parts, tclStr(a)) } }")
	tp.emitLine("\treturn strings.Join(parts, %q), nil", sep)
	tp.emitLine("}, 0, -1)")
	return true
}

// emitPrefixFunction registers a prefix proc as a scalar SQL function that
// joins its args with a space and prepends the fixed prefix (window6.test's
// winproc: window('hello world') → "window: hello world").
func (tp *transpiler) emitPrefixFunction(name, procName string) bool {
	prefix, ok := tp.prefixFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tvar parts []string")
	tp.emitLine("\tfor _, a := range args { if a != nil { parts = append(parts, tclStr(a)) } }")
	tp.emitLine("\treturn %q + strings.Join(parts, \" \"), nil", prefix)
	tp.emitLine("}, 0, -1)")
	return true
}

// emitPredFunction registers a predicate proc as a scalar SQL function
// applying the comparison to its first argument (numeric).
func (tp *transpiler) emitPredFunction(name, procName string) bool {
	pred, ok := tp.predFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\targ, _ := strconv.ParseFloat(tclStr(args[0]), 64)")
	tp.emitLine("\tif %s { return int64(1), nil }", pred)
	tp.emitLine("\treturn int64(0), nil")
	tp.emitLine("}, 0, -1)")
	return true
}

// emitInt2strFunction registers the test-harness int2str scalar function.
func (tp *transpiler) emitInt2strFunction(name string) {
	tp.emitLine("%s.RegisterFunction(\"int2str\", func(args []interface{}) (interface{}, error) {", tp.dbVar)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn tclInt2str(args[0]), nil")
	tp.emitLine("}, 0, -1)")
}

// emitErrorFunction registers an error-raising scalar SQL function.
func (tp *transpiler) emitErrorFunction(name, msg string) {
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\treturn nil, fmt.Errorf(%q)", msg)
	tp.emitLine("}, 0, -1)")
}

// joinPrefixFromRest extracts the literal prefix from a braced registration
// word like `{joinx cross}` (the second token when the first is the proc
// name).

// emitDBEvalArrayRows handles `db eval SQL ARRAYVAR {BODY}`: each result row
// binds ARRAYVAR(column) to the row's cell values, then BODY runs — once per
// row (fts3sort.test's per-row array capture).
func (tp *transpiler) emitDBEvalArrayRows(arrName string, rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest[:1])
	if sqlExpr == `""` {
		return
	}
	bodyText := strings.TrimSpace(rest[2].Text)
	bodyText = strings.TrimSuffix(strings.TrimPrefix(bodyText, "{"), "}")

	// Collect the ARRAY(key) references the body reads so per-row scalar
	// bindings can be pre-declared.
	keyRe := regexp.MustCompile(`\$` + regexp.QuoteMeta(arrName) + `\(([A-Za-z0-9_]+)\)`)
	var keys []string
	seen := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(bodyText, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}

	arrStar := tclVarToGo(arrName + "(*)")
	if !tp.isVarDeclared(arrStar) {
		tp.emitLine("var %s string", arrStar)
		tp.vars = append(tp.vars, arrStar)
		tp.emitLine("_ = %s // suppress unused warning", arrStar)
	}
	for _, k := range keys {
		kv := tclVarToGo(arrName + "(" + k + ")")
		if tp.isVarDeclared(kv) || !isValidGoIdent(kv) {
			continue
		}
		tp.emitLine("var %s string", kv)
		tp.vars = append(tp.vars, kv)
		tp.emitLine("_ = %s // suppress unused warning", kv)
	}

	rowsVar := fmt.Sprintf("_dbevalRows%d", tp.varCount)
	tp.varCount++
	flatVar := fmt.Sprintf("_%sFlat%d", arrName, tp.varCount)
	tp.varCount++
	if tp.rowFlatVars == nil {
		tp.rowFlatVars = make(map[string]string)
	}
	tp.rowFlatVars[arrName] = flatVar
	defer func() { delete(tp.rowFlatVars, arrName) }()
	tp.emitLine("%s := db.Query(%s)", rowsVar, sqlExpr)
	tp.emitLine("if %s.Error == nil {", rowsVar)
	tp.indent++
	// Active-read wrapper: the scanned SELECT is a RUN-state VM for the whole
	// callback loop upstream (db->nVdbeRead) — DDL in the body hits the
	// OP_Destroy interlock.
	tp.emitLine("db.BeginActiveStatement()")
	arrStarAssign := tclVarToGo(arrName + "(*)")
	tp.emitLine("%s = strings.Join(%s.Columns, \" \")", arrStarAssign, rowsVar)
	tp.emitLine("for _ri := 0; _ri < len(%s.Rows); _ri++ {", rowsVar)
	tp.indent++
	tp.emitLine("%s := tclRowFlatPairs(%s.Columns, %s.Rows[_ri])", flatVar, rowsVar, rowsVar)
	tp.emitLine("_ = %s", flatVar)
	tp.emitLine("for _ci := 0; _ci < len(%s.Columns); _ci++ {", rowsVar)
	tp.indent++
	tp.emitLine("switch %s.Columns[_ci] {", rowsVar)
	tp.indent++
	for _, k := range keys {
		kv := tclVarToGo(arrName + "(" + k + ")")
		if !isValidGoIdent(kv) {
			continue
		}
		tp.emitLine("case %q:", k)
		tp.indent++
		tp.emitLine("%s = tclStr(%s.Rows[_ri][_ci])", kv, rowsVar)
		tp.indent--
	}
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		forIncrs:     tp.forIncrs,
		testPrefix:   tp.testPrefix,
		queryVars:    tp.queryVars,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		collateGoFuncs:   tp.collateGoFuncs,
		preparedState:    tp.preparedState,
		varConstValues:   tp.varConstValues,
		sqlVarValues:     tp.sqlVarValues,
		foreachLitValues: tp.foreachLitValues,
		rowFlatVars:      tp.rowFlatVars,
	}
	bodyTP.processCommands(parseCommands(bodyText))
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("db.EndActiveStatement()")
	tp.indent--
	tp.emitLine("}")
}

// emitIncrRetFunction registers `db func OP -argcount N PROC` where PROC's
// body is `incr ::VAR [AMOUNT]; return RET`: the closure increments the Go
// variable mirroring ::VAR and returns RET, so harness counters observe one
// invocation per TRUE operator evaluation (vtabH 2.x).
func (tp *transpiler) emitIncrRetFunction(name, procName string, rest []tcl.RawWord) bool {
	info, ok := tp.incrRetFuncs[procName]
	if !ok || name == "" {
		return false
	}
	arityLo, arityHi := 0, -1
	if n, has := dbFuncArgCount(rest); has {
		arityLo, arityHi = n, n
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif n, err := strconv.Atoi(%s); err == nil { %s = strconv.Itoa(n + %d) }", info.GoVar, info.GoVar, info.Amount)
	tp.emitLine("\treturn int64(%d), nil", info.Ret)
	tp.emitLine("}, %d, %d)", arityLo, arityHi)
	return true
}
