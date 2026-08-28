// Package main implements the tcl2go tool.
//
// This file handles the TCL set command.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ---- Variable handlers ----

func (tp *transpiler) processSetBracketValue(goName, cmdText string) bool {
	cmdParts := strings.Fields(cmdText)
	// set VAR [userproc args...] with a registry-backed implementation
	// (rtree4's rand/randincr/scramble): emit a direct runtime call instead
	// of falling through to the raw-text fallback. `$var` arguments pass the
	// Go variable's value; bare tokens go through as literals.
	if globalUserProcs[cmdParts[0]] {
		callArgs := make([]string, 0, len(cmdParts)-1)
		for _, a := range cmdParts[1:] {
			if strings.HasPrefix(a, "$") && !strings.Contains(a, "(") {
				if gv := tclVarToGo(strings.TrimPrefix(a, "$")); gv != "" {
					callArgs = append(callArgs, gv)
					continue
				}
			}
			callArgs = append(callArgs, strconv.Quote(a))
		}
		tp.assignSetValue(goName,
			fmt.Sprintf("callTclUserProc(%q, %s)", cmdParts[0], strings.Join(callArgs, ", ")))
		return true
	}
	// set VAR [catch {sqlite3_intarray_create DB NAME} RESULTVAR] — the
	// intarray create runs inside a catch; RESULTVAR receives the create's
	// RETURN VALUE (the handle), while the bracket result (assigned to VAR) is
	// the catch code. test_intarray.c 1.1b.
	if cmdParts[0] == "catch" {
		for i := 0; i < len(cmdParts); i++ {
			if strings.HasPrefix(cmdParts[i], "{sqlite3_intarray_create") && i+2 < len(cmdParts) {
				name := strings.TrimSuffix(cmdParts[i+2], "}")
				name = strings.Trim(name, "'\"")
				resultVar := tclVarToGo(cmdParts[len(cmdParts)-1])
				tp.emitLine("_r = vtab.IntarrayRegisterHandle(%q)", name)
				tp.emitLine("_res = %s.Exec(\"CREATE VIRTUAL TABLE temp.%s USING intarray('%s')\")", tp.dbVar, name, name)
				tp.emitLine("if _res.Error != nil { t.Errorf(\"intarray create: %%v\", _res.Error) }")
				if resultVar != "" && resultVar != goName {
					tp.emitLine("%s = _r", resultVar)
				}
				tp.emitLine("_r = \"0\"")
				return true
			}
		}
	}
	// [lreverse $VAR] — reverse a TCL list variable at runtime
	// (fts3first.test's order=DESC comparisons).
	if len(cmdParts) == 2 && cmdParts[0] == "lreverse" && strings.HasPrefix(cmdParts[1], "$") {
		if gv := tclVarToGo(strings.TrimPrefix(cmdParts[1], "$")); gv != "" {
			tp.emitLine("%s = tclLreverse(%s)", goName, gv)
			tp.emitLine("\t_ = %s // suppress unused warning", goName)
			return true
		}
	}
	if len(cmdParts) > 0 && (cmdParts[0] == "sqlite3_prepare" || cmdParts[0] == "sqlite3_prepare_v2") {
		tp.recordPreparedStatement(goName, "["+cmdText+"]")
		return true
	}
	if isLsearchCmd(cmdParts) {
		return tp.setLsearchValue(goName, cmdText, cmdParts)
	}
	if cmdParts[0] == "string" && len(cmdParts) > 1 {
		// set VAR [string first NEEDLE HAYSTACK ?START?] and friends: render
		// the runtime string-op result into VAR (zipfile.test 24.x patches a
		// central-directory offset computed from $zip at runtime).
		if setStringFuncValue(tp, goName, cmdParts) {
			return true
		}
	}
	if cmdParts[0] == "findall" && len(cmdParts) == 3 {
		// set L [findall NEEDLE HAYSTACK]: the zipfile2.test proc returning
		// every occurrence index (runtime call — $archive patches below).
		if n, ok := tp.stringWordExpr(cmdParts[1]); ok {
			if h, ok2 := tp.stringWordExpr(cmdParts[2]); ok2 {
				tp.assignSetValue(goName, fmt.Sprintf("tclFindAll(%s, %s)", n, h))
				return true
			}
		}
	}
	if cmdParts[0] == "create_db" {
		return tp.setCreateDBValue(goName)
	}
	if isMakeExprCmd(cmdParts) {
		return tp.setMakeExprValue(goName, cmdText, cmdParts[0])
	}
	if isRegexpCmd(cmdParts) {
		return tp.setRegexpValue(goName, cmdParts)
	}
	if tp.inlineQueryFuncValue(goName, cmdParts) {
		return true
	}
	if isDBEvalCmd(cmdParts) {
		return tp.setDBEvalValue(goName, cmdText, cmdParts)
	}
	if isDBOneCmd(cmdParts) {
		return tp.setDBOneValue(goName, cmdText, cmdParts)
	}
	// set VAR [catchsql {SQL}] — execute the SQL and bind VAR to the catchsql
	// result string ("0 {rows}" on success, "1 {msg}" on error), matching TCL's
	// [catchsql] command-substitution value (ctime.test's out-of-bounds
	// sqlite_compileoption_get checks).
	if cmdParts[0] == "catchsql" {
		return tp.setCatchsqlSetValue(goName, cmdText, cmdParts)
	}
	// set VAR [db total_changes] / [db changes] — the connection's change
	// counters (fts4merge5 1.4: the merge loop breaks when total_changes
	// stops growing).
	if len(cmdParts) >= 2 && cmdParts[0] == "db" {
		switch cmdParts[1] {
		case "total_changes":
			tp.assignSetValue(goName, "strconv.FormatInt(db.TotalChanges(), 10)")
			return true
		case "changes":
			tp.assignSetValue(goName, "strconv.FormatInt(db.Changes(), 10)")
			return true
		}
	}
	// set VAR [<conn> incrblob ...] — assign the *frigolite.Blob to VAR and
	// register it as a blob channel (read/seek/puts/close on $VAR route to
	// the Blob methods).
	if isDBIncrblobCmd(cmdParts) {
		connName := cmdParts[0]
		restText := strings.TrimSpace(strings.TrimPrefix(cmdText, connName))
		restText = strings.TrimSpace(strings.TrimPrefix(restText, "incrblob"))
		rest := strings.Fields(restText)
		restWords := make([]tcl.RawWord, 0, len(rest))
		for _, r := range rest {
			restWords = append(restWords, tcl.RawWord{Text: r})
		}
		tp.processDBIncrblobTo(goName, connName, restWords)
		return true
	}
	if isListCmd(cmdParts) {
		return tp.setListValue(goName, cmdText)
	}
	// set VAR [binary format SPEC ARGS...] — build a byte string (the
	// corruption tests slice/modify the segdir root blob).
	if cmdParts[0] == "binary" && len(cmdParts) >= 3 && cmdParts[1] == "format" {
		if _, ok := tp.setBinaryFormatValue(goName, cmdText); ok {
			return true
		}
	}
	// set VAR [sqlite3_intarray_create DB NAME] — create the intarray vtab
	// and bind VAR to the returned handle (test_intarray.c).
	if cmdParts[0] == "sqlite3_intarray_create" {
		if tp.intarrayCreateSetValue(goName, cmdText) {
			return true
		}
	}
	if isSqlite3OpenCmd(cmdParts) {
		return tp.setSqlite3Value(goName, cmdParts)
	}
	if isExprCmd(cmdParts) {
		return tp.setExprValue(goName, cmdText)
	}
	// set VAR [open PATH MODE] — store the file path in VAR (a file channel).
	// The mode argument (e.g. "rb", "w") is TCL-only; tclReadFile/tclFileSize
	// helpers operate on the path alone.
	if cmdParts[0] == "open" {
		restText := strings.TrimSpace(strings.TrimPrefix(cmdText, "open"))
		fields := strings.Fields(restText)
		path := restText
		if len(fields) > 0 {
			path = fields[0]
		}
		pathWord := tcl.RawWord{Text: path}
		pathExpr := tp.sqlStringValue(pathWord)
		tp.assignSetValue(goName, pathExpr)
		return true
	}
	// set VAR [read $CHAN] — read the file channel's whole content (CHAN
	// holds a path stored by `set CHAN [open PATH ...]`).
	if cmdParts[0] == "read" && len(cmdParts) >= 2 && strings.HasPrefix(cmdParts[1], "$") {
		chanGo := tclVarToGo(strings.TrimPrefix(cmdParts[1], "$"))
		if isValidGoIdent(chanGo) && tp.isVarDeclared(chanGo) {
			tp.assignSetValue(goName, "tclReadFile("+chanGo+")")
			return true
		}
	}
	// set var [sqlite3_blob_bytes $B] — assign the blob byte count.
	if cmdParts[0] == "sqlite3_blob_bytes" {
		tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(%s.Bytes())", tp.blobArgExpr(tcl.RawWord{Text: cmdText[len("sqlite3_blob_bytes"):]})))
		return true
	}
	// set VAR [file_control_data_version db [schema]] — the database file's
	// data-version counter (dataversion1.test).
	if cmdParts[0] == "file_control_data_version" && len(cmdParts) >= 2 {
		dbExpr := tp.dbArgGo(cmdParts[1])
		schemaExpr := `""`
		if len(cmdParts) >= 3 {
			schemaExpr = tp.goStringLiteral(tcl.RawWord{Text: cmdParts[2]})
		}
		tp.assignSetValue(goName, fmt.Sprintf("tclDataVersion(%s, %s)", dbExpr, schemaExpr))
		return true
	}
	// set VAR [db one {SQL}] — run the SQL at runtime and take the first
	// column of the first row as a TCL string (zipfile2 5.0/8.x build their
	// fixtures with SELECT X'...' blobs).
	if cmdParts[0] == "db" && len(cmdParts) >= 3 && cmdParts[1] == "one" {
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmdText), "db one"))
		tp.assignSetValue(goName, fmt.Sprintf("tclDbOne(%s, %s)", tp.dbVar, tp.goStringLiteral(tcl.RawWord{Text: body})))
		return true
	}
	// set VAR [binary encode hex $b] / [binary decode hex $hex] — the TCL
	// 8.x binary codec used all over zipfile2 to move between blob bytes
	// and their hex text form.
	if cmdParts[0] == "binary" && len(cmdParts) >= 4 {
		var expr string
		switch cmdParts[1] + " " + cmdParts[2] {
		case "encode hex":
			expr = tp.hexCodecArgExpr(cmdParts, 3)
			if expr != "" {
				tp.assignSetValue(goName, fmt.Sprintf("tclHexEncode(%s)", expr))
				return true
			}
		case "decode hex":
			expr = tp.hexCodecArgExpr(cmdParts, 3)
			if expr != "" {
				tp.assignSetValue(goName, fmt.Sprintf("string(tclHexDecode(%s))", expr))
				return true
			}
		}
	}
	// set VAR [proc arg...] — a registered special-func proc (int2str,
	// utf8_to_hstr, ...) whose Go helper is in specialFuncs.
	if tmpl, ok := tp.specialFuncs[cmdParts[0]]; ok {
		if strings.Contains(tmpl, "$data") && len(cmdParts) >= 2 {
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmdText), cmdParts[0]))
			dataExpr := tp.buildStringExpr(rest)
			tp.assignSetValue(goName, strings.Replace(tmpl, "$data", dataExpr, 1))
		} else if tmpl == "tclBlobHexDecode" {
			// blob() hex decoder as a value (zipfile2 `set blob [blob $a]`):
			// decode the argument's hex text into raw bytes. Tokenize with the
			// TCL word splitter so nested bracket substitutions survive
			// ([blob [string map {0800 0900} $archive2]]).
			argExpr := `""`
			if w := tclCmdWords(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmdText), cmdParts[0]))); len(w) >= 1 {
				argExpr = tp.buildStringExpr(w[0])
			}
			tp.assignSetValue(goName, fmt.Sprintf("string(tclHexDecode(%s))", argExpr))
		} else {
			tp.assignSetValue(goName, tmpl)
		}
		return true
	}
	// set tstbody [concat sqlite3_table_column_metadata $DB $params] —
	// record the column-metadata command (colmeta.test). The do_test body
	// that catch-runs $tstbody is later emitted as a direct
	// tclTableColumnMetadata call (see emitColmetaBody).
	if strings.HasPrefix(cmdText, "concat sqlite3_table_column_metadata") {
		if tp.colmetaCmds == nil {
			tp.colmetaCmds = make(map[string]string)
		}
		tp.colmetaCmds[goName] = strings.TrimSpace(strings.TrimPrefix(cmdText, "concat sqlite3_table_column_metadata"))
		tp.assignSetValue(goName, `""`)
		return true
	}
	// set VAR [sqlite3_exec db {SQL}] — run SQL (with %XX decoding) and store
	// the "{code {headers values}}" result (badutf2.test).
	if cmdParts[0] == "sqlite3_exec" && len(cmdParts) >= 3 {
		dbExpr := tp.dbArgGo(cmdParts[1])
		sqlArg := strings.TrimSpace(cmdText)
		sqlArg = strings.TrimSpace(strings.TrimPrefix(sqlArg, "sqlite3_exec"))
		sqlArg = strings.TrimSpace(strings.TrimPrefix(sqlArg, cmdParts[1]))
		// A bare $var SQL argument passes the Go variable directly; a braced
		// or quoted SQL text becomes a Go string literal.
		var sqlExpr string
		if strings.HasPrefix(sqlArg, "$") {
			gv := tclVarToGo(strings.TrimPrefix(sqlArg, "$"))
			if isValidGoIdent(gv) {
				sqlExpr = gv
			}
		}
		if sqlExpr == "" {
			sqlExpr = tp.goStringLiteral(tcl.RawWord{Text: sqlArg})
		}
		tp.assignSetValue(goName, fmt.Sprintf("tclExec(%s, %s)", dbExpr, sqlExpr))
		return true
	}
	// set VAR [sqlite3_limit db LIMIT -1] — query the current limit value
	// (basexx1.test's `set inLimit [sqlite3_limit db SQLITE_LIMIT_LENGTH -1]`
	// restore pattern). The -1 argument means "query, don't change".
	if cmdParts[0] == "sqlite3_limit" && len(cmdParts) >= 4 && strings.TrimSpace(cmdParts[3]) == "-1" {
		tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(db.Limit(%q))", cmdParts[2]))
		return true
	}
	if tp.setMiscBracketValue(goName, cmdText, cmdParts) {
		return true
	}
	return false
}

// setMiscBracketValue handles the remaining `set VAR [...]` bracket values:
// dbcksum (tester.tcl checksum), sqlite3_connection_pointer (connection
// alias for sqlite3_errmsg/errcode resolution), catch, and concat-of-eqp.
// Returns true when the command was handled.
func (tp *transpiler) setMiscBracketValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] == "dbcksum" || cmdParts[0] == "sqlite3_connection_pointer" {
		// set VAR [dbcksum db [schema]] — compute the tester.tcl checksum;
		// set VAR [sqlite3_connection_pointer db] — alias a connection to a
		// variable so later sqlite3_errmsg $VAR / sqlite3_errcode $VAR calls
		// resolve to the real *frigolite.DB handle (see dbArgGo).
		return tp.setCAPIValue(goName, cmdParts)
	}
	if isCatchCmd(cmdParts) {
		return tp.setCatchValue(goName, cmdText)
	}
	if cmdParts[0] == "concat" && strings.Contains(cmdText, "[eqp ") {
		return tp.setConcatEQPValue(goName, cmdText)
	}
	return false
}

// setConcatEQPValue handles `set VAR [concat [eqp SQL] [eqp SQL] ...]` — the
// TCL test-suite idiom that concatenates EXPLAIN QUERY PLAN detail lists
// (e_fkey-26.x). Each [eqp "SQL"] is inlined as tclEQP(db, SQL) and the
// results are space-joined into the Go variable.
func (tp *transpiler) setConcatEQPValue(goName, cmdText string) bool {
	var exprs []string
	rest := cmdText
	for {
		i := strings.Index(rest, "[eqp ")
		if i < 0 {
			break
		}
		rest = rest[i+len("[eqp "):]
		// The eqp argument is a double-quoted or braced SQL string ending
		// before the matching "]".
		arg := ""
		if strings.HasPrefix(rest, "\"") {
			end := strings.Index(rest[1:], "\"")
			if end < 0 {
				break
			}
			arg = rest[1 : 1+end]
			rest = rest[2+end:]
		} else {
			end := strings.Index(rest, "]")
			if end < 0 {
				break
			}
			arg = strings.TrimSpace(rest[:end])
			rest = rest[end+1:]
		}
		sqlExpr := tp.goStringLiteral(tcl.RawWord{Text: arg})
		exprs = append(exprs, "tclEQP(db, "+sqlExpr+")")
	}
	if len(exprs) == 0 {
		return false
	}
	tp.emitLine("%s = %s", goName, strings.Join(exprs, " + \" \" + "))
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setCAPIValue dispatches `set VAR [dbcksum ...]` and `set VAR
// [sqlite3_connection_pointer ...]` to their handlers (the two C-API set
// values the transpiler emulates; see processSetBracketValue).
func (tp *transpiler) setCAPIValue(goName string, cmdParts []string) bool {
	if cmdParts[0] == "sqlite3_connection_pointer" {
		return tp.setConnectionPointerValue(goName, cmdParts)
	}
	// dbcksum db [schema] — compute the tester.tcl checksum.
	schemaName := "\"main\""
	dbConn := "db"
	if len(cmdParts) >= 2 {
		dbConn = tp.dbArgGo(cmdParts[1])
	}
	if len(cmdParts) >= 3 {
		schemaName = fmt.Sprintf("%q", cmdParts[2])
	}
	tp.assignSetValue(goName, "tclDBCksum("+dbConn+", "+schemaName+")")
	return true
}

// setConnectionPointerValue handles `set VAR [sqlite3_connection_pointer db]`
// — the TCL test suite's way of aliasing a connection to a variable (the
// returned "pointer" is an opaque C-API string). The Go value is the plain
// connection name; the alias is recorded in varConstValues so later
// `sqlite3_errmsg $VAR` / `sqlite3_errcode $VAR` / `sqlite3_prepare $VAR`
// calls resolve to the real *frigolite.DB handle. Returns false when the
// target is not a known connection (the generic set path handles it).
func (tp *transpiler) setConnectionPointerValue(goName string, cmdParts []string) bool {
	if len(cmdParts) < 2 {
		return false
	}
	connGo := tclVarToGo(strings.TrimPrefix(cmdParts[1], "$"))
	if !(connGo == "db" || isPreDeclaredDB(connGo) || tp.dbConnVars[connGo]) {
		return false
	}
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = connGo
	tp.assignSetValue(goName, fmt.Sprintf("%q", connGo))
	return true
}

// setCreateDBValue handles `set nPage [create_db "..."]` (e_vacuum.test):
// emits the t1/t2 table setup so later VACUUM tests see the tables, and binds
// the file-size result (VACUUM-dependent) to "0" — the assertions that use it
// are skipped as VACUUM.
func (tp *transpiler) setCreateDBValue(goName string) bool {
	tp.emitLine("_res = db.Exec(\"PRAGMA page_size = 1024;\")")
	tp.emitLine("_ = _res")
	tp.emitLine("_res = db.Exec(\"CREATE TABLE t1(a PRIMARY KEY, b UNIQUE); INSERT INTO t1 VALUES(1, randomblob(400)); INSERT INTO t1 SELECT a+1, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+2, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+4, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+8, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+16, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+32, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+64, randomblob(400) FROM t1; CREATE TABLE t2(a PRIMARY KEY, b UNIQUE); INSERT INTO t2 SELECT * FROM t1;\")")
	tp.emitLine("if _res.Error != nil { t.Errorf(\"create_db: %%v\", _res.Error) }")
	tp.emitLine("%s = \"0\"", goName)
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setLsearchValue handles `set idx [lsearch $prg OpenEphemeral]` — search a
// program string (from tclExecSQL EXPLAIN output) for an opcode name and store
// the result as a string index ("-1" when not found). Skips leading option
// flags (e.g. lsearch -exact $list $opcode).
// setBinaryFormatValue handles `set VAR [binary format SPEC ARGS...]` —
// assigns the resulting byte string to VAR (the corruption tests build the
// modified segdir root blob this way).
func (tp *transpiler) setBinaryFormatValue(goName, cmdText string) (string, bool) {
	inner := strings.TrimSpace(strings.TrimPrefix(cmdText, "binary format"))
	fields := tclCmdWords(inner)
	if len(fields) < 2 {
		return "", false
	}
	spec := fields[0]
	vals := make([]string, 0, len(fields)-1)
	for _, a := range fields[1:] {
		vals = append(vals, tp.binaryArgExpr(a))
	}
	expr, ok := binaryFormatGoExpr(spec, vals)
	if !ok {
		return "", false
	}
	tp.assignSetValue(goName, expr)
	return expr, true
}

// setStringFuncValue handles `set VAR [string first NEEDLE HAY ?START?]`:
// runtime semantics of TCL string first (zipfile.test 24.x patches a
// central-directory offset computed from $zip at runtime). Returns false for
// unsupported subcommands so callers keep their fallbacks.
func setStringFuncValue(tp *transpiler, goName string, cmdParts []string) bool {
	rest := cmdParts[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:] // option flags (-nocase ...): none affect runtime index
	}
	if len(rest) < 2 || rest[0] != "first" {
		return false
	}
	needle, ok := tp.stringWordExpr(rest[1])
	if !ok {
		return false
	}
	hayIdx := 2
	hay, ok := tp.stringWordExpr(rest[hayIdx])
	if !ok {
		return false
	}
	start := ""
	if len(rest) > hayIdx+1 {
		s, ok2 := tp.stringWordExpr(rest[hayIdx+1])
		if !ok2 {
			return false
		}
		start = ", toInt(" + s + ")"
	}
	tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(tclStrIndex(%s, %s%s))", hay, needle, start))
	return true
}

// stringWordExpr renders one TCL word as a Go STRING expression: $var binds
// its variable's value, a quoted literal binds the literal text. Anything
// else (brackets, braced scripts) is not supported here.
func (tp *transpiler) stringWordExpr(word string) (string, bool) {
	if strings.HasPrefix(word, "$") && !strings.Contains(word, "(") {
		gv := tclVarToGo(strings.TrimPrefix(word, "$"))
		if gv != "" && isValidGoIdent(gv) {
			return gv, true
		}
		return "", false
	}
	return strconv.Quote(strings.Trim(word, `"`)), true
}

// hexCodecArgExpr renders the VALUE argument of a [binary encode|decode hex]
// word list as a Go string expression; "" when unsupported.
func (tp *transpiler) hexCodecArgExpr(cmdParts []string, idx int) string {
	if idx >= len(cmdParts) {
		return ""
	}
	expr, ok := tp.stringWordExpr(cmdParts[idx])
	if !ok {
		return ""
	}
	return expr
}

// setLsearchValue handles `set VAR [lsearch ...]` — assigns the lsearch
// result to VAR.
func (tp *transpiler) setLsearchValue(goName, cmdText string, cmdParts []string) bool {
	rest := cmdParts[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	if len(rest) < 2 {
		return false
	}
	listExpr := strings.TrimPrefix(rest[0], "$")
	goList := tclVarToGo(listExpr)
	if !isValidGoIdent(goList) {
		return false
	}
	opcode := strings.Trim(rest[1], `"`)
	tp.emitLine("%s = strconv.Itoa(strings.Index(%s, %q))", goName, goList, opcode)
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setMakeExprValue handles `set VAR [make_exprN cList vList op]` —
// rowvalue2's expression-building procs. Emits a call to the Go helper with
// the runtime variable values.
func (tp *transpiler) setMakeExprValue(goName, cmdText, cmdName string) bool {
	goFn := map[string]string{"make_expr1": "tclMakeExpr1", "make_expr2": "tclMakeExpr2", "make_expr3": "tclMakeExpr3"}[cmdName]
	words := tclCmdWords(cmdText)
	if len(words) < 4 {
		return false
	}
	argExprs := make([]string, 3)
	for i := 1; i <= 3; i++ {
		w := words[i]
		if strings.HasPrefix(w, "$") {
			argExprs[i-1] = tclVarToGo(strings.TrimPrefix(w, "$"))
		} else if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			argExprs[i-1] = tp.goStringLiteral(tcl.RawWord{Text: w[1 : len(w)-1]})
		} else {
			argExprs[i-1] = tp.goStringLiteral(tcl.RawWord{Text: strings.Trim(w, `"`)})
		}
	}
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("%s = %s(%s, %s, %s)", goName, goFn, argExprs[0], argExprs[1], argExprs[2])
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setRegexpValue handles `set VAR [regexp PATTERN [db one {SQL}]]` — evaluate
// the capability regexp against the engine's answer. UTF-16 encoding is not
// supported (PRAGMA encoding is always UTF-8), so isutf16 = "0".
func (tp *transpiler) setRegexpValue(goName string, cmdParts []string) bool {
	setTo := "0"
	pattern := strings.Trim(cmdParts[1], `"`)
	if !strings.Contains(pattern, "16") {
		// Non-UTF16 capability check: no reliable answer; leave 0.
		setTo = "0"
	}
	tp.emitLine("%s = %q // capability regexp %q not matched (engine default)", goName, setTo, pattern)
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	return true
}

// setDBEvalValue handles `set var [db eval "SQL"]` — run the query and assign
// the flattened result. The double-quoted SQL substitutes $var as RAW TEXT
// (TCL string substitution) before db eval runs, so a variable holding an
// expression fragment (rowvalue4: WHERE $where) embeds verbatim. Braced db
// eval binds $var as VALUES (buildSQLStringExpr handles that path elsewhere).
func (tp *transpiler) setDBEvalValue(goName, cmdText string, cmdParts []string) bool {
	sqlText := strings.TrimSpace(strings.TrimPrefix(cmdText, "db eval"))
	sqlText = strings.TrimSpace(sqlText)
	braced := len(sqlText) >= 2 && sqlText[0] == '{' && sqlText[len(sqlText)-1] == '}'
	if braced {
		sqlText = strings.TrimSpace(sqlText[1 : len(sqlText)-1])
	} else {
		sqlText = strings.Trim(sqlText, `"`)
	}
	var sqlExpr string
	if braced {
		// Braced db eval binds $var as SQL VALUES (sqlLiteral), and [cmd]
		// bracket-quoted identifiers stay literal.
		sqlExpr = tp.buildSQLStringExprNoCmd(sqlText)
	} else {
		sqlExpr = tp.buildStringExpr(sqlText)
	}
	dbEvalVar := fmt.Sprintf("_dbeval%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclExecSQL(db, %s)", dbEvalVar, sqlExpr)
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, dbEvalVar)
	} else {
		tp.emitLine("var %s = %s", goName, dbEvalVar)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setDBOneValue handles `set var [db one "SQL"]` / [db onecolumn "SQL"] — run
// the query and assign the first column of the first row (TCL's db one alias
// for onecolumn). The Go variable holds the rendered value as a string for
// later expected-value comparisons.
func (tp *transpiler) setDBOneValue(goName, cmdText string, cmdParts []string) bool {
	sqlText := strings.TrimSpace(strings.TrimPrefix(cmdText, "db "+cmdParts[1]))
	sqlText = strings.TrimSpace(strings.Trim(sqlText, `"`))
	sqlExpr := tp.buildSQLStringExpr(sqlText)
	oneVar := fmt.Sprintf("_dbone%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclExecSQL(db, %s)", oneVar, sqlExpr)
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, oneVar)
	} else {
		tp.emitLine("var %s = %s", goName, oneVar)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setCatchsqlSetValue handles `set VAR [catchsql {SQL}]` — execute the SQL
// and bind VAR to the catchsql result string ("0 {rows}" on success,
// "1 {msg}" on error) via tclCatchsqlString, matching TCL's [catchsql]
// command-substitution value. The SQL may reference runtime $vars (e.g.
// `set ans [catchsql {SELECT compileoption_get($N)}]`).
func (tp *transpiler) setCatchsqlSetValue(goName, cmdText string, cmdParts []string) bool {
	// Re-parse cmdText with brace-aware splitting (cmdParts from
	// processSetBracketValue uses strings.Fields, which breaks braced SQL
	// like {SELECT ...} into separate tokens). This recovers the full SQL
	// body and any trailing connection argument.
	parts := tclCmdWords(cmdText)
	if len(parts) < 2 {
		tp.assignSetValue(goName, `""`)
		return true
	}
	// parts[1] is the brace-stripped SQL body (or a $var holding SQL).
	sqlWord := tcl.RawWord{Text: parts[1], Braced: strings.HasPrefix(parts[1], "{")}
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{sqlWord})
	if sqlExpr == `""` {
		tp.assignSetValue(goName, `""`)
		return true
	}
	// A trailing connection argument (catchsql {SQL} db2) is rare here;
	// resolve through the standard connection dispatcher when present.
	dbConn := "db"
	if len(parts) >= 3 {
		args := []tcl.RawWord{{Text: "catchsql"}, sqlWord}
		for _, extra := range parts[2:] {
			args = append(args, tcl.RawWord{Text: extra})
		}
		dbConn = tp.resolveSQLConnection(args)
	}
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("%s = tclCatchsqlString(_res)", goName)
	return true
}

// setListValue handles `set var [list a b c]` — build a TCL-list string
// without the "list" command word so tclSplitList at Go runtime returns
// exactly the list elements. TCL backslash-newline continuations are removed
// (they are not part of the list value).
func (tp *transpiler) setListValue(goName, cmdText string) bool {
	listText := strings.TrimPrefix(cmdText, "list")
	listText = strings.ReplaceAll(listText, "\\\r\n", " ")
	listText = strings.ReplaceAll(listText, "\\\n", " ")
	listText = strings.TrimSpace(listText)
	// A [list 1 {message}] form (catchsql-style error) is often stored
	// for later do_catchsql_test $var comparisons; keep just the
	// message text so strings.Contains against a real error matches.
	// Guard on the EXACT two-element shape: a longer data list whose first
	// element happens to be 1 (json102's correct_answer starts
	// "1 {$.id} 123 ...") must keep every element.
	if elems := tclSplitList(listText); len(elems) == 2 && elems[0] == "1" {
		msg := strings.TrimSpace(elems[1])
		if len(msg) >= 2 && msg[0] == '{' && msg[len(msg)-1] == '}' {
			msg = strings.TrimSpace(msg[1 : len(msg)-1])
		}
		listText = msg
	}
	valExpr := tp.goStringLiteral(tcl.RawWord{Text: listText})
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, valExpr)
	} else {
		tp.emitLine("var %s = %s", goName, valExpr)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = listText
	return true
}

// setSqlite3Value handles `set var [sqlite3 db <file>]` — reopen a connection
// as a side effect. The connection handle is not used in Go, so assign an
// empty placeholder after performing the reopen. A preceding "db close"
// already emitted db.Close().
func (tp *transpiler) setSqlite3Value(goName string, cmdParts []string) bool {
	goName2 := tclVarToGo(cmdParts[1])
	filename := tp.buildStringExpr(cmdParts[2])
	if tp.pendingFileReset[cmdParts[2]] {
		delete(tp.pendingFileReset, cmdParts[2])
		filename = tp.buildStringExpr(cmdParts[2])
	}
	tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
	tp.dqsDML = true
	tp.emitLine("%s, err = frigolite.Open(%s)", goName2, filename)
	tp.emitLine("if err != nil { t.Fatal(err) }")
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = \"\"", goName)
	} else {
		tp.emitLine("var %s = \"\"", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setExprValue handles `set var [expr {...}]` — evaluate constant expressions
// at generation time, or emit runtime evaluation for variable/command/query
// expressions.
func (tp *transpiler) setExprValue(goName, cmdText string) bool {
	exprStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmdText), "expr"))
	if len(exprStr) >= 2 && exprStr[0] == '{' && exprStr[len(exprStr)-1] == '}' {
		exprStr = exprStr[1 : len(exprStr)-1]
	}
	// A bare integer literal keeps its exact decimal text: EvalExpr's
	// float64 path would render 9223372036854775807 as
	// "9.223372036854776e+18" (unionvtab.test 3.8/4.x S/L bounds).
	if n, perr := strconv.ParseInt(strings.TrimSpace(exprStr), 10, 64); perr == nil {
		tp.assignSetValue(goName, fmt.Sprintf("%q", strconv.FormatInt(n, 10)))
		return true
	}
	result, err := tcl.EvalExpr(exprStr, nil, nil)
	valExpr := ""
	if err == nil {
		valExpr = fmt.Sprintf("%q", result)
	} else if fs, ok := fileSizeArithExpr(exprStr); ok {
		// [file size PATH] arithmetic (e.g. `[file size test.db]/1024`) —
		// render the file size and arithmetic at runtime.
		valExpr = "strconv.Itoa(" + fs + ")"
	} else if strings.Contains(exprStr, "[") && strings.Contains(exprStr, "]") {
		// [cmd] command substitutions (e.g. [string length $word]).
		if goExpr, ok := tp.exprCmdToGo(exprStr); ok {
			valExpr = "strconv.Itoa(" + goExpr + ")"
		}
	} else if strings.Contains(exprStr, "rand(") {
		// rand() is the one TCL math function the runtime evaluator
		// (tclEvalFuncs) cannot compute, so render it as native Go code
		// (the helpers import math/rand). Other functions (log, pow,
		// int, sqrt, ...) and pure $var arithmetic fall through to
		// tclExprWith, which evaluates at runtime with float semantics
		// (critical when a var holds a real like 2460369.5 — toInt()
		// would truncate it).
		if goExpr, ok := tp.exprCmdToGo(exprStr); ok {
			valExpr = "strconv.Itoa(" + goExpr + ")"
		}
	}
	if valExpr == "" {
		valExpr = tp.runtimeExprValue(exprStr)
	}
	tp.assignSetValue(goName, valExpr)
	return true
}

// runtimeExprValue builds a Go expression that evaluates a TCL expr string at
// runtime (with live $var values), handling the db-eval-eq-empty form and the
// generic tclExprWith fallback.
func (tp *transpiler) runtimeExprValue(exprStr string) string {
	// `$a eq $b` / `$a ne $b` — native Go string comparison; avoids the
	// token-wise runtime evaluator on multi-word values (rtree2 dumps).
	if m := eqNeExpr.FindStringSubmatch(exprStr); m != nil {
		op := "=="
		if m[2] == "ne" {
			op = "!="
		}
		return fmt.Sprintf("tclBool01(%s %s %s)",
			tp.exprVarValue(strings.TrimPrefix(m[1], "$")), op,
			tp.exprVarValue(strings.TrimPrefix(m[3], "$")))
	}
	// TCL `[expr {[db eval {SQL}] eq {{}}}]` — a boolean computed from
	// a query result (e.g. func4.test's highPrecision flags). Emit a
	// runtime db eval that runs the SQL and compares the flattened
	// result against the empty string, returning "1"/"0" like TCL
	// expr. This cannot be evaluated at generation time because the
	// transpiler has no engine.
	if dbEvalSQL, ok := dbEvalEqEmptyExpr(exprStr); ok {
		sqlExpr := tp.buildSQLStringExpr(dbEvalSQL)
		// The TCL expr compares the db eval result (rendered, NULL as
		// "{}") against the empty list {}: a NULL result is equal.
		return fmt.Sprintf("func() string { _r := tclExecSQL(db, %s); if _r == \"\" || _r == \"{}\" { return \"1\" }; return \"0\" }()", sqlExpr)
	}
	// Runtime evaluation with live $var values.
	exprVarNames, exprGo := tclExprToGo(exprStr, tp.vars)
	if len(exprVarNames) == 0 {
		return fmt.Sprintf("tclExpr(%q)", exprGo)
	}
	var parts []string
	for _, name := range exprVarNames {
		parts = append(parts, fmt.Sprintf("%q: %s", name, tp.exprVarValue(name)))
	}
	return fmt.Sprintf("tclExprWith(%q, map[string]string{%s})", exprGo, strings.Join(parts, ", "))
}

// assignSetValue emits an assignment (declaration or update) of valueExpr to
// goName with the standard suppress-unused line.
func (tp *transpiler) assignSetValue(goName, valueExpr string) {
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, valueExpr)
	} else {
		tp.emitLine("var %s = %s", goName, valueExpr)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
}

// findBracedBody scans s for the first complete {...} group and returns the
// text inside the braces and the trimmed remainder after the closing brace.
func findBracedBody(s string) (body, rest string, ok bool) {
	if !strings.HasPrefix(s, "{") {
		return "", "", false
	}
	depth := 0
	bodyStart := -1
	for i, c := range s {
		if c == '{' {
			if depth == 0 {
				bodyStart = i + 1
			}
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 && bodyStart >= 0 {
				return s[bodyStart:i], strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

// setCatchValue handles `set v [catch {execsql ...} msg]` — transpile the
// catch body as a Go block that records the result code and error message.
func (tp *transpiler) setCatchValue(goName, cmdText string) bool {
	varName := goName
	errVar := "_catchErrMsg"
	// Find the braced body and optional error var
	restAfterCatch := cmdText
	restAfterCatch = strings.TrimSpace(strings.TrimPrefix(restAfterCatch, "catch"))
	bodyStr, restStr, ok := findBracedBody(restAfterCatch)
	if !ok {
		return false
	}
	// If there's an error variable name
	errVar = "_catchErrMsg"
	if restStr != "" {
		errVar = tclVarToGo(restStr)
	}
	// Avoid using Go's 'err' (error type) as catch error var
	if errVar == "err" {
		errVar = "_err_tcl"
	}
	tp.emitCatchBlock(varName, errVar, bodyStr)
	return true
}

// singleDbEvalSelectRows reports whether the catch body is exactly one
// "db eval {SELECT ...}" command (any connection: db, db2, ...) with no array
// variable or trailing row script. TCL assigns the query's flattened row
// values to the catch message variable on success (tclsqlite.c: the command
// result is the row list), so emitCatchBlock binds errVar from _res.Rows in
// that case (lock.test 1.21/1.22). Returns false for every other body shape.
func singleDbEvalSelectRows(body string) bool {
	b := strings.TrimSpace(body)
	// Connection token: "db" optionally followed by digits, then "eval".
	i := 0
	if !strings.HasPrefix(b, "db") {
		return false
	}
	i = 2
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	rest := strings.TrimSpace(b[i:])
	if !strings.HasPrefix(rest, "eval") {
		return false
	}
	rest = strings.TrimSpace(rest[len("eval"):])
	if len(rest) == 0 || rest[0] != '{' {
		return false
	}
	// Find the matching close brace; nothing but whitespace may follow.
	depth := 0
	end := -1
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = j
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 || strings.TrimSpace(rest[end+1:]) != "" {
		return false
	}
	sql := strings.ToUpper(strings.TrimSpace(rest[1:end]))
	return strings.HasPrefix(sql, "SELECT") || strings.HasPrefix(sql, "WITH") || strings.HasPrefix(sql, "VALUES")
}

// scope, assigning the TCL result code (1/0) to varName and the error message
// to errVar.
func (tp *transpiler) emitCatchBlock(varName, errVar, bodyStr string) {
	// Declare variables at function scope (indent 1)
	// so they're accessible from all do_test blocks.
	savedIndent := tp.indent
	tp.indent = 1
	if !tp.isVarDeclared(varName) {
		tp.emitLine("var %s string", varName)
		tp.vars = append(tp.vars, varName)
	}
	tp.emitLine("_ = %s // suppress unused warning", varName)
	// msg is declared at function level in preamble
	if errVar != "msg" && !tp.isVarDeclared(errVar) {
		tp.emitLine("var %s string", errVar)
		tp.vars = append(tp.vars, errVar)
	}
	tp.emitLine("_ = %s // suppress unused warning", errVar)
	tp.indent = savedIndent
	tp.emitLine("{ // catch block")
	tp.indent++
	tp.emitLine("var _catchErr error")
	// Parse and transpile the body
	bodyCmds := parseCommands(bodyStr)
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, dbClosed: tp.dbClosed, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, dbAliases: tp.dbAliases, queryVars: tp.queryVars, unsetVars: tp.unsetVars, dbVarFuncs: tp.dbVarFuncs, constFuncs: tp.constFuncs, rangeListFuncs: tp.rangeListFuncs, varCount: tp.varCount, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.dbClosed = bodyTP.dbClosed
	tp.dqsDDL = bodyTP.dqsDDL
	tp.dqsDML = bodyTP.dqsDML
	tp.varCount = bodyTP.varCount
	tp.queryVars = bodyTP.queryVars
	tp.unsetVars = bodyTP.unsetVars
	tp.dbVarFuncs = bodyTP.dbVarFuncs
	tp.constFuncs = bodyTP.constFuncs
	tp.dbAliases = bodyTP.dbAliases
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	// Copy connection-state maps unconditionally so deletes (a reopen clearing
	// a prior closed/failed-open state) propagate to the outer transpiler.
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	// After body, set result and error message
	tp.emitLine("if _catchErr != nil {")
	tp.indent++
	tp.emitLine("%s = \"1\"", varName)
	tp.emitLine("%s = _catchErr.Error()", errVar)
	tp.indent--
	tp.emitLine("} else {")
	tp.indent++
	tp.emitLine("%s = \"0\"", varName)
	// TCL `catch {db eval {SELECT...}} msg` assigns the query RESULT (the
	// flattened row values) to msg on success (tclsqlite.c: the command
	// result is the row list). When the body is exactly one db-eval SELECT,
	// bind errVar from the captured rows (lock.test 1.21/1.22).
	if singleDbEvalSelectRows(bodyStr) {
		tp.emitLine("%s = tclRowValuesFlat(_res)", errVar)
	} else {
		tp.emitLine("%s = \"\"", errVar)
	}
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processSetTimeValue handles `set var [time { SCRIPT }]` — transpile the
// inner script and bind the variable to "".
func (tp *transpiler) processSetTimeValue(goName, bracketText string) {
	cmdText := strings.TrimPrefix(bracketText, "[")
	cmdText = strings.TrimSuffix(cmdText, "]")
	cmdText = strings.TrimSpace(strings.TrimPrefix(cmdText, "time"))
	if bodyStr, _, ok := findBracedBody(cmdText); ok {
		bodyCmds := parseCommands(bodyStr)
		bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState}
		bodyTP.processCommands(bodyCmds)
		tp.indent = bodyTP.indent
	}
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = \"\"", goName)
	} else {
		tp.emitLine("var %s = \"\"", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
}

// processSetLindexTimeValue handles `set var [lindex [time { SCRIPT }] N]` —
// the timing command wraps a script (usually a db eval) as
// `[lindex [time {...}] 0]`. Transpile the inner script as statements and bind
// the variable to "0".
func (tp *transpiler) processSetLindexTimeValue(goName, bracketText string) {
	cmdText := strings.TrimSuffix(strings.TrimPrefix(bracketText, "["), "]")
	// cmdText now: lindex [time {SCRIPT}] N
	if timeIdx := strings.Index(cmdText, "[time "); timeIdx >= 0 {
		afterTime := cmdText[timeIdx+len("[time "):]
		if bodyStr, _, ok := findBracedBody(afterTime); ok {
			bodyCmds := parseCommands(bodyStr)
			bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState}
			bodyTP.processCommands(bodyCmds)
			tp.indent = bodyTP.indent
		}
	}
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = \"0\"", goName)
	} else {
		tp.emitLine("var %s = \"0\"", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
}

// processSetGeneric handles the plain `set var value` assignment: build the
// value expression, track query vars / testprefix, and emit the assignment.
func (tp *transpiler) processSetGeneric(goName string, args, rest []tcl.RawWord) {
	valueExpr := tp.varValueExpr(rest)
	// Track simple bare-word constants (set var value) so later commands can
	// resolve the value statically — e.g. `set db_dest db2` followed by
	// `execsql {SQL} $db_dest` resolves the connection to db2.
	if len(rest) >= 1 && !rest[0].Braced && !rest[0].Quoted &&
		!strings.HasPrefix(rest[0].Text, "[") && !strings.HasPrefix(rest[0].Text, "$") {
		if tp.varConstValues == nil {
			tp.varConstValues = make(map[string]string)
		}
		tp.varConstValues[goName] = rest[0].Text
	}
	// Track quoted string assignments so later commands (e.g. the
	// colmeta.test `set tests "concat $tests {LIST}"` accumulation) can
	// resolve the variable's constant list value.
	if len(rest) >= 1 && rest[0].Quoted {
		if tp.varConstValues == nil {
			tp.varConstValues = make(map[string]string)
		}
		tp.varConstValues[goName] = tclUnescapeQuoted(rest[0].Text)
	}
	// Track variables whose assigned value is (or begins with) a query
	// statement, so `execsql $var` bodies can be recognized as queries. Also
	// record braced SQL constants in a dedicated map so sqlite3_prepare can
	// classify a $var SQL (capi3-1.7 prepares `SELECT namex ...` via a $sql
	// variable) without disturbing the varConstValues concat/list machinery.
	if len(rest) >= 1 && rest[0].Braced {
		tp.markQueryVar(args[0].Text, rest[0].Text)
		if !strings.HasPrefix(strings.TrimSpace(rest[0].Text), "$") {
			if tp.sqlVarValues == nil {
				tp.sqlVarValues = make(map[string]string)
			}
			tp.sqlVarValues[goName] = strings.TrimSpace(rest[0].Text)
		}
	} else if len(rest) >= 1 && !rest[0].Braced {
		tp.markQueryVar(args[0].Text, rest[0].Text)
	}
	// Track `set testprefix NAME` so the skipTests lookup can resolve bare
	// test names (e.g. whereF's "4.0") to their TCL-effective names
	// ("whereF-4.0"), matching tester.tcl's prefixing. This keeps generic
	// keys like "4.0" from colliding across packages.
	if args[0].Text == "testprefix" && len(rest) >= 1 && !rest[0].Braced {
		tp.testPrefix = strings.TrimSpace(rest[0].Text)
	}
	// Use := for first declaration, = for subsequent assignment to avoid redeclaration
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, valueExpr)
	} else {
		tp.emitLine("var %s = %s", goName, valueExpr)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
}
