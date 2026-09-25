// Package main implements the tcl2go tool.
//
// This file holds the tail of the `set VAR [cmd ...]` bracket-value dispatch
// chain started in processset_bracket.go, plus the misc/C-API/concat-EQP
// value emitters it routes to.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// setBracketDBChangesValue handles `set VAR [db total_changes]` /
// `[db changes]` — the connection's change counters (fts4merge5 1.4: the
// merge loop breaks when total_changes stops growing).
func (tp *transpiler) setBracketDBChangesValue(goName, cmdText string, cmdParts []string) bool {
	if len(cmdParts) < 2 || cmdParts[0] != "db" {
		return false
	}
	switch cmdParts[1] {
	case "total_changes":
		tp.assignSetValue(goName, "strconv.FormatInt(db.TotalChanges(), 10)")
		return true
	case "changes":
		tp.assignSetValue(goName, "strconv.FormatInt(db.Changes(), 10)")
		return true
	}
	return false
}

// setBracketIncrblobValue handles `set VAR [<conn> incrblob ...]` — assign the
// *frigolite.Blob to VAR and register it as a blob channel (read/seek/puts/
// close on $VAR route to the Blob methods).
func (tp *transpiler) setBracketIncrblobValue(goName, cmdText string, cmdParts []string) bool {
	if !isDBIncrblobCmd(cmdParts) {
		return false
	}
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

// setBracketListValue routes `set VAR [list ...]` to setListValue.
func (tp *transpiler) setBracketListValue(goName, cmdText string, cmdParts []string) bool {
	if !isListCmd(cmdParts) {
		return false
	}
	return tp.setListValue(goName, cmdText)
}

// setBracketBinaryFormatValue handles `set VAR [binary format SPEC ARGS...]` —
// build a byte string (the corruption tests slice/modify the segdir root
// blob).
func (tp *transpiler) setBracketBinaryFormatValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "binary" || len(cmdParts) < 3 || cmdParts[1] != "format" {
		return false
	}
	if _, ok := tp.setBinaryFormatValue(goName, cmdText); ok {
		return true
	}
	return false
}

// setBracketIntarrayCreateValue handles `set VAR [sqlite3_intarray_create DB
// NAME]` — create the intarray vtab and bind VAR to the returned handle
// (test_intarray.c).
func (tp *transpiler) setBracketIntarrayCreateValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_intarray_create" {
		return false
	}
	return tp.intarrayCreateSetValue(goName, cmdText)
}

// setBracketSqlite3OpenValue routes `set var [sqlite3 db <file>]` to
// setSqlite3Value.
func (tp *transpiler) setBracketSqlite3OpenValue(goName, cmdText string, cmdParts []string) bool {
	if !isSqlite3OpenCmd(cmdParts) {
		return false
	}
	return tp.setSqlite3Value(goName, cmdParts)
}

// setBracketLimitFallbackValue catches any sqlite3_limit arity the top
// prior-value branch declined (the 3-part db-less form below covers the
// harness shorthand).
func (tp *transpiler) setBracketLimitFallbackValue(goName, cmdText string, cmdParts []string) bool {
	if len(cmdParts) < 3 || cmdParts[0] != "sqlite3_limit" {
		return false
	}
	lim := cmdParts[len(cmdParts)-2]
	val := strings.TrimSpace(cmdParts[len(cmdParts)-1])
	if strings.TrimSpace(val) == "-1" {
		tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(db.Limit(%q))", lim))
		return true
	}
	tp.emitLimitPriorAssign(goName, lim, val)
	return true
}

// setBracketExprValue routes `set VAR [expr ...]` to setExprValue.
func (tp *transpiler) setBracketExprValue(goName, cmdText string, cmdParts []string) bool {
	if !isExprCmd(cmdParts) {
		return false
	}
	return tp.setExprValue(goName, cmdText)
}

// setBracketOpenValue handles `set VAR [open PATH MODE]` — store the file path
// in VAR (a file channel). The mode argument (e.g. "rb", "w") is TCL-only;
// tclReadFile/tclFileSize helpers operate on the path alone.
func (tp *transpiler) setBracketOpenValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "open" {
		return false
	}
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

// setBracketReadValue handles `set VAR [read $CHAN]` — read the file channel's
// whole content (CHAN holds a path stored by `set CHAN [open PATH ...]`).
func (tp *transpiler) setBracketReadValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "read" || len(cmdParts) < 2 || !strings.HasPrefix(cmdParts[1], "$") {
		return false
	}
	chanGo := tclVarToGo(strings.TrimPrefix(cmdParts[1], "$"))
	if !isValidGoIdent(chanGo) || !tp.isVarDeclared(chanGo) {
		return false
	}
	if len(cmdParts) >= 3 {
		// `read $CHAN N` — read N bytes from the channel's current
		// seek position. Used by corrupt* tests to capture cell
		// pointers / child-page bytes. N may be a bracket-balanced
		// `[expr ...]` that the Fields split fragments; reconstruct
		// it from the raw cmdText.
		prefix := cmdParts[0] + " " + cmdParts[1]
		countText := strings.TrimSpace(cmdText)
		countText = strings.TrimSpace(strings.TrimPrefix(countText, prefix))
		countExpr, ok := tp.readCountExpr(countText)
		if !ok {
			countExpr = cmdParts[2]
		}
		tp.assignSetValue(goName, fmt.Sprintf("tclReadFileWithLen(%s, %s)", chanGo, countExpr))
	} else {
		tp.assignSetValue(goName, "tclReadFile("+chanGo+")")
	}
	return true
}

// setBracketBlobBytesValue handles `set var [sqlite3_blob_bytes $B]` — assign
// the blob byte count.
func (tp *transpiler) setBracketBlobBytesValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_blob_bytes" {
		return false
	}
	tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(%s.Bytes())", tp.blobArgExpr(tcl.RawWord{Text: cmdText[len("sqlite3_blob_bytes"):]})))
	return true
}

// setBracketDataVersionValue handles `set VAR [file_control_data_version db
// [schema]]` — the database file's data-version counter (dataversion1.test).
func (tp *transpiler) setBracketDataVersionValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "file_control_data_version" || len(cmdParts) < 2 {
		return false
	}
	dbExpr := tp.dbArgGo(cmdParts[1])
	schemaExpr := `""`
	if len(cmdParts) >= 3 {
		schemaExpr = tp.goStringLiteral(tcl.RawWord{Text: cmdParts[2]})
	}
	tp.assignSetValue(goName, fmt.Sprintf("tclDataVersion(%s, %s)", dbExpr, schemaExpr))
	return true
}

// setBracketDBOneRawValue handles `set VAR [db one {SQL}]` — run the SQL at
// runtime and take the first column of the first row as a TCL string
// (zipfile2 5.0/8.x build their fixtures with SELECT X'...' blobs).
func (tp *transpiler) setBracketDBOneRawValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "db" || len(cmdParts) < 3 || cmdParts[1] != "one" {
		return false
	}
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmdText), "db one"))
	tp.assignSetValue(goName, fmt.Sprintf("tclDbOne(%s, %s)", tp.dbVar, tp.goStringLiteral(tcl.RawWord{Text: body})))
	return true
}

// setBracketBinaryCodecValue handles `set VAR [binary encode hex $b]` /
// `[binary decode hex $hex]` — the TCL 8.x binary codec used all over
// zipfile2 to move between blob bytes and their hex text form.
func (tp *transpiler) setBracketBinaryCodecValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "binary" || len(cmdParts) < 4 {
		return false
	}
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
	return false
}

// setBracketSpecialFuncValue handles `set VAR [proc arg...]` — a registered
// special-func proc (int2str, utf8_to_hstr, ...) whose Go helper is in
// specialFuncs.
func (tp *transpiler) setBracketSpecialFuncValue(goName, cmdText string, cmdParts []string) bool {
	tmpl, ok := tp.specialFuncs[cmdParts[0]]
	if !ok {
		return false
	}
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

// setBracketColmetaValue handles `set tstbody [concat
// sqlite3_table_column_metadata $DB $params]` — record the column-metadata
// command (colmeta.test). The do_test body that catch-runs $tstbody is later
// emitted as a direct tclTableColumnMetadata call (see emitColmetaBody).
func (tp *transpiler) setBracketColmetaValue(goName, cmdText string, cmdParts []string) bool {
	if !strings.HasPrefix(cmdText, "concat sqlite3_table_column_metadata") {
		return false
	}
	if tp.colmetaCmds == nil {
		tp.colmetaCmds = make(map[string]string)
	}
	tp.colmetaCmds[goName] = strings.TrimSpace(strings.TrimPrefix(cmdText, "concat sqlite3_table_column_metadata"))
	tp.assignSetValue(goName, `""`)
	return true
}

// setBracketExecValue handles `set VAR [sqlite3_exec db {SQL}]` — run SQL
// (with %XX decoding) and store the "{code {headers values}}" result
// (badutf2.test).
func (tp *transpiler) setBracketExecValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_exec" || len(cmdParts) < 3 {
		return false
	}
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

// setBracketLimitQueryValue handles `set VAR [sqlite3_limit db LIMIT -1]` —
// query the current limit value (basexx1.test's `set inLimit [sqlite3_limit db
// SQLITE_LIMIT_LENGTH -1]` restore pattern). The -1 argument means "query,
// don't change".
func (tp *transpiler) setBracketLimitQueryValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_limit" || len(cmdParts) < 4 || strings.TrimSpace(cmdParts[3]) != "-1" {
		return false
	}
	tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(db.Limit(%q))", cmdParts[2]))
	return true
}

// setBracketLimitSetValue handles `set VAR [sqlite3_limit db LIMIT N]` — set
// the limit, return the PRIOR value (sqllimits1-1.30: `set prior
// [sqlite3_limit db SQLITE_LIMIT_LENGTH 1]` captures the old limit for
// restore). NOTE: `set prior [sqlite3_limit SQLITE_LIMIT_LENGTH 1]` (no db
// arg, as emitted for 1.30's inner set) has cmdParts = [sqlite3_limit LIMIT
// VAL]: db is args[1], so the limit is cmdParts[1].
func (tp *transpiler) setBracketLimitSetValue(goName, cmdText string, cmdParts []string) bool {
	if cmdParts[0] != "sqlite3_limit" || (len(cmdParts) != 4 && len(cmdParts) != 3) {
		return false
	}
	lim, val := "", ""
	if len(cmdParts) == 4 {
		lim = cmdParts[2]
		val = strings.TrimSpace(cmdParts[3])
	} else {
		lim = cmdParts[1]
		val = strings.TrimSpace(cmdParts[2])
	}
	if strings.TrimSpace(val) == "-1" {
		tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(db.Limit(%q))", lim))
		return true
	}
	tp.emitLine("{ // set %s [sqlite3_limit %s %s]", goName, lim, val)
	tp.indent++
	tp.emitLine("_prior := db.Limit(%q)", lim)
	tp.emitLine("db.SetLimit(%q, toInt(%q))", lim, val)
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = strconv.Itoa(_prior)", goName)
	} else {
		tp.emitLine("var %s = strconv.Itoa(_prior)", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s", goName)
	tp.indent--
	tp.emitLine("}")
	return true
}

// setBracketMiscValue routes the remaining bracket values (dbcksum,
// sqlite3_connection_pointer, catch, concat-of-eqp) to setMiscBracketValue.
func (tp *transpiler) setBracketMiscValue(goName, cmdText string, cmdParts []string) bool {
	return tp.setMiscBracketValue(goName, cmdText, cmdParts)
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
