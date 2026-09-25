// Package main implements the tcl2go tool.
//
// This file handles `set ::var ...` (TCL namespace variable) assignments:
// the ordered special-case guard chain behind processNamespaceSet and the
// generic namespace-set emitter.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// namespaceSetChain is the ORDERED guard chain behind processNamespaceSet.
// Order is load-bearing: it replicates the original if-else ladder exactly —
// the first guard that returns true handled the set; false falls through to
// the next guard (and finally the generic emitter). It is a builder function
// (like tclHandlers) because a package-level slice of method expressions
// would form a var-initialization cycle through processSet.
func namespaceSetChain() []func(tp *transpiler, args []tcl.RawWord) bool {
	return []func(tp *transpiler, args []tcl.RawWord) bool{
		nsSetCurrentTime,
		nsSetMaxBlobsize,
		nsSetInvalidIdent,
		nsSetPrepare,
		nsSetOpen,
		nsSetDB1Serialize,
		nsSetPredeclaredDB,
		nsSetNoValue,
		nsSetBracketProcs,
		nsSetInlineQuery,
		nsSetExpr,
		nsSetRead,
		nsSetDBOne,
		nsSetIncrblob,
		nsSetQuota,
	}
}

// nsSetCurrentTime handles `set ::sqlite_current_time N` — the TCL test
// harness pins 'now' for CURRENT_TIME/DATE/TIMESTAMP and
// date()/time()/datetime('now'). Install a fixed clock so the generated test
// is deterministic.
func nsSetCurrentTime(tp *transpiler, args []tcl.RawWord) bool {
	varName := args[0].Text
	if varName != "::sqlite_current_time" || len(args) < 2 {
		return false
	}
	val := strings.TrimSpace(args[1].Text)
	if _, err := strconv.ParseInt(val, 10, 64); err == nil {
		tp.emitLine("function.SetNowFunc(func() time.Time { return time.Unix(%s, 0) })", val)
		return true
	}
	return false
}

// nsSetMaxBlobsize handles `set ::sqlite3_max_blobsize N` — the TCL harness
// links SQLite's test-only global (test1.c Tcl_LinkVar of src/vdbe.c
// sqlite3_max_blobsize). Writes go to the engine tracker; reads are
// re-materialized so do_test bodies can compare the value.
func nsSetMaxBlobsize(tp *transpiler, args []tcl.RawWord) bool {
	varName := args[0].Text
	if varName != "::sqlite3_max_blobsize" {
		return false
	}
	goName := tclVarToGo(varName)
	decl := ""
	if !tp.isVarDeclared(goName) {
		decl = "var "
		tp.vars = append(tp.vars, goName)
	}
	if len(args) >= 2 {
		val := strings.TrimSpace(args[1].Text)
		if n, err := strconv.Atoi(val); err == nil {
			tp.emitLine("%s%s = %q // linked sqlite3_max_blobsize", decl, goName, val)
			tp.emitLine("storage.SetMaxBlobsize(%d)", n)
			return true
		}
	}
	// Query form: refresh the shadow variable from the tracker.
	tp.emitLine("%s = strconv.Itoa(storage.MaxBlobsize()) // linked sqlite3_max_blobsize", goName)
	tp.emitLine("_ = %s", goName)
	return true
}

// nsSetInvalidIdent skips namespace variables whose Go name is invalid.
func nsSetInvalidIdent(tp *transpiler, args []tcl.RawWord) bool {
	goName := tclVarToGo(args[0].Text)
	if isValidGoIdent(goName) {
		return false
	}
	tp.emitLine("// set %s (invalid identifier, skipped)", args[0].Text)
	return true
}

// nsSetPrepare handles the legacy prepared statement assignment in namespace
// form (`set ::STMT [sqlite3_prepare ...]`).
func nsSetPrepare(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 || !strings.HasPrefix(strings.TrimSpace(args[1].Text), "[sqlite3_prepare") {
		return false
	}
	tp.recordPreparedStatement(tclVarToGo(args[0].Text), strings.TrimSpace(args[1].Text))
	return true
}

// nsSetOpen handles the legacy `set ::dbx [sqlite3_open FILE]` — it creates a
// connection handle. Preserves its type so later sqlite3_close receives
// *frigolite.DB. Returns false (fall through) when no path argument exists.
func nsSetOpen(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 || !strings.Contains(args[1].Text, "sqlite3_open") {
		return false
	}
	openArg := sqlite3OpenArg(args[1].Text)
	if openArg == "" {
		return false
	}
	goName := tclVarToGo(args[0].Text)
	filename := tp.goStringLiteral(tcl.RawWord{Text: strings.TrimSuffix(openArg, "]")})
	tp.emitLine("%s, err := frigolite.Open(%s)", goName, filename)
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("defer %s.Close()", goName)
	if tp.dbConnVars == nil {
		tp.dbConnVars = make(map[string]bool)
	}
	tp.dbConnVars[goName] = true
	return true
}

// nsSetDB1Serialize handles memdb1.test's BLOB shadow (`set ::db1 [db
// serialize]`): routes the image bytes into db1Blob so the *frigolite.DB var
// stays a connection handle.
func nsSetDB1Serialize(tp *transpiler, args []tcl.RawWord) bool {
	varName := args[0].Text
	if len(args) < 2 || tclVarToGo(varName) != "db1" || !strings.Contains(strings.TrimSpace(args[1].Text), "db serialize") {
		return false
	}
	bracket := strings.TrimSpace(args[1].Text)
	schema := "main"
	if idx := strings.Index(bracket, "serialize"); idx >= 0 {
		rest := strings.Trim(strings.TrimSuffix(strings.TrimSpace(bracket[idx+len("serialize"):]), "]"), "{} ")
		if rest != "" {
			schema = rest
		}
	}
	tp.emitLine("db1Blob = string(tclSerialize(db, %q)) // ::db1 image shadow", schema)
	tp.emitLine("vtab.TclVarSet(%q, %q, db1Blob)", strings.TrimPrefix(varName, "::"), "")
	tp.emitLine("_ = db1Blob")
	return true
}

// nsSetPredeclaredDB skips assignments to DB connection variables (type
// conflict).
func nsSetPredeclaredDB(tp *transpiler, args []tcl.RawWord) bool {
	goName := tclVarToGo(args[0].Text)
	if !isPreDeclaredDB(goName) && goName != "db" {
		return false
	}
	if len(args) >= 2 {
		tp.emitLine("// set %s (skipped, DB connection)", args[0].Text)
	}
	return true
}

// nsSetNoValue handles `set ::var` without a value — a query or unset, never
// a redeclaration.
func nsSetNoValue(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) >= 2 {
		return false
	}
	tp.emitLine("_ = %s // TCL namespace variable (query)", tclVarToGo(args[0].Text))
	return true
}

// nsSetBracketProcs handles `set ::var [queryProc]`-style assignments whose
// value is a known fixture proc call: memdb.test's signature (with args),
// cksum, and the table-signature procs. Returns false so other bracket forms
// keep falling through.
func nsSetBracketProcs(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	inner := strings.TrimSpace(args[1].Text)
	if !strings.HasPrefix(inner, "[") || !strings.HasSuffix(inner, "]") {
		return false
	}
	parts := strings.Fields(strings.TrimSpace(inner[1 : len(inner)-1]))
	if len(parts) < 1 {
		return false
	}
	goName := tclVarToGo(args[0].Text)
	if body, ok := globalProcBodies[parts[0]]; ok && userProcEmitterFor(parts[0], body) == "memdb_signature" {
		tp.assignSetValue(goName, fmt.Sprintf("tclMemdbSignature(%s)", tp.dbVar))
		return true
	}
	if parts[0] == "cksum" {
		tp.assignSetValue(goName, fmt.Sprintf("tclCksum(%s)", tp.nsConnArg(parts)))
		return true
	}
	if body, ok := globalProcBodies[parts[0]]; ok && userProcEmitterFor(parts[0], body) == "table_sig" {
		table, col, _ := tableSigProcInfo(body)
		tp.assignSetValue(goName, fmt.Sprintf("tclTableSig(%s, %q, %q)", tp.nsConnArg(parts), table, col))
		return true
	}
	return false
}

// nsConnArg resolves the optional connection argument of a bracketed proc
// call (defaulting to the transpiler's main db variable).
func (tp *transpiler) nsConnArg(parts []string) string {
	connVar := tp.dbVar
	if len(parts) >= 2 {
		if v := strings.TrimSpace(parts[1]); isValidGoIdent(tclVarToGo(v)) {
			connVar = tclVarToGo(v)
		}
	}
	return connVar
}

// nsSetInlineQuery inlines a bare query-proc value (`set ::sig [signature]`;
// see inlineNamespaceQuery).
func nsSetInlineQuery(tp *transpiler, args []tcl.RawWord) bool {
	return tp.inlineNamespaceQuery(tclVarToGo(args[0].Text), args[1])
}

// nsSetExpr handles `set ::var [expr ...]` — evaluate constant/runtime
// expressions through setExprValue (file-size arithmetic, string ops),
// matching how plain `set var [expr ...]` is handled. Without this, `set
// ::size [expr [file size $::cmdlinearg(INFO_SCRIPT)]]` would be emitted as a
// raw tclExprWith call referencing an undeclared array-map variable.
func nsSetExpr(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 || !strings.HasPrefix(strings.TrimSpace(args[1].Text), "[expr ") {
		return false
	}
	goName := tclVarToGo(args[0].Text)
	return tp.setExprValue(goName, strings.TrimSuffix(strings.TrimSpace(args[1].Text)[1:], "]"))
}

// nsSetRead handles `set ::data [read $fd2]` — read a file channel (fd2 holds
// a path). Returns false so unsupported read shapes fall through.
func nsSetRead(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 || !strings.HasPrefix(strings.TrimSpace(args[1].Text), "[read $") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "[read $"), "]")
	// `read $fd N` includes a byte count after the channel var; we want
	// only the channel name. `read $fd` (whole file) has no N.
	parts := strings.Fields(inner)
	if len(parts) == 0 {
		return false
	}
	chanVar := parts[0]
	goChan := tclVarToGo(chanVar)
	if !isValidGoIdent(goChan) || !tp.isVarDeclared(goChan) {
		return false
	}
	goName := tclVarToGo(args[0].Text)
	if len(parts) >= 2 {
		// The count may be a bracket-balanced `[expr ...]`
		// (memdb1.test 8.x: `read $fd [expr 20*1024]`); take
		// everything after the channel var so whitespace inside the
		// expr survives the Fields split.
		countExpr, ok := tp.readCountExpr(strings.TrimSpace(inner[len(chanVar):]))
		if !ok {
			return false
		}
		tp.assignSetValue(goName, fmt.Sprintf("tclReadFileWithLen(%s, %s)", goChan, countExpr))
	} else {
		tp.assignSetValue(goName, "tclReadFile("+goChan+")")
	}
	return true
}

// nsSetDBOne handles `set ::var [db one {SQL}]` — execute the db-onecolumn
// query and assign.
func nsSetDBOne(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 || !strings.HasPrefix(strings.TrimSpace(args[1].Text), "[db one") {
		return false
	}
	cmdText := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "["), "]")
	goName := tclVarToGo(args[0].Text)
	return tp.setDBOneValue(goName, cmdText, strings.Fields(cmdText))
}

// nsSetIncrblob handles `set ::blob [<conn> incrblob ...]` and the
// `eval db incrblob` form — assign the *frigolite.Blob to the namespace var
// and register it as a blob channel.
func nsSetIncrblob(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 || !strings.Contains(strings.TrimSpace(args[1].Text), " incrblob ") {
		return false
	}
	cmdText := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "["), "]")
	cmdParts := strings.Fields(cmdText)
	if len(cmdParts) >= 2 && isDBIncrblobCmd(cmdParts) {
		connName := cmdParts[0]
		restText := strings.TrimSpace(strings.TrimPrefix(cmdText, connName))
		restText = strings.TrimSpace(strings.TrimPrefix(restText, "incrblob"))
		rest := strings.Fields(restText)
		restWords := make([]tcl.RawWord, 0, len(rest))
		for _, f := range rest {
			restWords = append(restWords, tcl.RawWord{Text: f})
		}
		tp.processDBIncrblobTo(tclVarToGo(args[0].Text), connName, restWords)
		return true
	}
	// set ::b [eval db incrblob $arg t1 d 1] — the eval form with a
	// dynamic option variable ($arg is "" or "-readonly").
	if len(cmdParts) >= 4 && cmdParts[0] == "eval" && cmdParts[1] == "db" && cmdParts[2] == "incrblob" {
		rest := cmdParts[3:]
		restWords := make([]tcl.RawWord, 0, len(rest))
		for _, f := range rest {
			restWords = append(restWords, tcl.RawWord{Text: f})
		}
		// $arg (if present) is the first word: "" or "-readonly".
		tp.processDBIncrblobEvalTo(tclVarToGo(args[0].Text), "db", restWords)
		return true
	}
	return false
}

// nsSetQuota handles `set ::var [sqlite3_quota_* ARGS]` — quota commands are
// value-producing (fopen handles, fread content, ...): run the same statement
// handler (which leaves its result in _r) and assign it (quota2.test
// 1.1/1.3: set ::h1 [sqlite3_quota_fopen ...]).
func nsSetQuota(tp *transpiler, args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	bracket := strings.TrimSpace(args[1].Text)
	if !strings.HasPrefix(bracket, "[") || !strings.HasSuffix(bracket, "]") {
		return false
	}
	cmdText := strings.TrimSuffix(strings.TrimPrefix(bracket, "["), "]")
	cmdParts := strings.Fields(cmdText)
	if len(cmdParts) > 0 && strings.HasPrefix(cmdParts[0], "sqlite3_quota_") {
		if h, ok := tclHandlers()[cmdParts[0]]; ok {
			raws := tcl.ParseCommands(cmdText)
			if len(raws) > 0 {
				h(tp, raws[0][1:])
				tp.assignSetValue(tclVarToGo(args[0].Text), "_r")
				return true
			}
		}
	}
	return false
}

// processNamespaceSet handles `set ::var ...` (TCL namespace variables) and the
// testdir infrastructure skip. Returns true when the set was fully handled.
func (tp *transpiler) processNamespaceSet(args []tcl.RawWord) bool {
	varName := args[0].Text
	if varName == "testdir" {
		tp.emitLine("// set testdir: test directory (not used in Go test context)")
		return true
	}
	// set ::arr($key) V — a dynamic-key array assignment with a namespace
	// prefix. Route through the same map-store path as plain arrays
	// (fts4aa.test: set ::fts4aa_res($q) [db eval ...]).
	if strings.HasPrefix(varName, "::") {
		if base, key, isDyn := tp.dynamicArraySet(varName); isDyn {
			tp.emitDynamicArraySet(base, key, args)
			return true
		}
	}
	if !strings.HasPrefix(varName, "::") {
		return false
	}
	for _, h := range namespaceSetChain() {
		if h(tp, args) {
			return true
		}
	}
	tp.emitNamespaceSetGeneric(args)
	return true
}

// emitNamespaceSetGeneric is the generic namespace-set fallback: register the
// variable in the tclvar registry, resolve the testprefix, and emit the
// assignment (declaration or update).
func (tp *transpiler) emitNamespaceSetGeneric(args []tcl.RawWord) {
	varName := args[0].Text
	goName := tclVarToGo(varName)
	valExpr := tp.varValueExpr(args[1:])
	// Namespace variables are TCL globals: register into the tclvar registry
	// so USING tclvar scans see them (vtabH 2.0: set ::xyz 10).
	nm := strings.TrimPrefix(varName, "::")
	_, _, isElem := splitArrayElement(nm)
	if !isElem && isValidGoIdent(tclVarToGo(nm)) {
		tp.emitLine("vtab.TclVarSet(%q, %q, %s)", nm, "", valExpr)
		tp.emitTclProcAliasRegistrations(nm, valExpr)
	}
	tp.resolveNamespacePrefix(varName, valExpr)
	// Namespace variables whose names appear in knownGlobalVars (e.g.
	// `oplog` — the journal2 testvfs sink) are package-level helpers-
	// template variables; emit a plain assignment, not a `var` redeclaration.
	if tp.isVarDeclared(goName) || knownGlobalVars()[goName] {
		tp.emitLine("%s = %s // TCL namespace variable", goName, valExpr)
	} else {
		tp.emitLine("var %s = %s // TCL namespace variable", goName, valExpr)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	tp.maybeArmInterruptCount(goName)
	// Track simple string-literal assignments so later commands
	// (e.g. sqlite3_create_collation_v2's $cmd destructor) can
	// resolve the variable's constant value.
	tp.trackVarConstValue(goName, args)
}

// resolveNamespacePrefix updates tp.testPrefix when a set ::testprefix or
// set testprefix value is assigned. valExpr is a Go expression (usually a
// quoted string literal); resolve it to the plain name for the skip lookup,
// stripping the surrounding quotes.
func (tp *transpiler) resolveNamespacePrefix(varName, valExpr string) {
	if varName != "::testprefix" && varName != "testprefix" {
		return
	}
	prefix := strings.TrimSpace(valExpr)
	if len(prefix) >= 2 && prefix[0] == '"' && prefix[len(prefix)-1] == '"' {
		prefix = prefix[1 : len(prefix)-1]
	}
	tp.testPrefix = prefix
}

// inlineNamespaceQuery inlines a query-proc result assigned to a TCL
// namespace variable (`set ::sig [signature]`). Returns true when the value
// was a recognized query proc. A memdb.test-style `set ::sig [signature
// one]` call (proc with ARGS, not a bare query proc) is NOT inlined: the
// signature proc takes a filename argument and returns a TCL list, so the
// assignment falls through to the generic set handling (literal text), and
// the do_test comparison then operates on error variables, not fabricated
// rows. Only a bare `[procname]` (no args) consults queryFuncs.
func (tp *transpiler) inlineNamespaceQuery(goName string, valWord tcl.RawWord) bool {
	if len(valWord.Text) < 2 || !strings.HasPrefix(valWord.Text, "[") || !strings.HasSuffix(valWord.Text, "]") || len(tp.queryFuncs) == 0 {
		return false
	}
	innerCmd := strings.TrimSuffix(strings.TrimPrefix(valWord.Text, "["), "]")
	cmdParts := strings.Fields(innerCmd)
	if len(cmdParts) != 1 {
		return false
	}
	if sql, ok := tp.queryFuncs[cmdParts[0]]; ok {
		tp.emitQueryVarAssign(goName, sql)
		return true
	}
	return false
}

// trackVarConstValue records a simple string-literal assignment ("lit" or
// {lit}) in varConstValues so later commands can resolve the constant.
func (tp *transpiler) trackVarConstValue(goName string, args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	lit := args[1].Text
	if len(lit) < 2 {
		return
	}
	if !((lit[0] == '"' && lit[len(lit)-1] == '"') || (lit[0] == '{' && lit[len(lit)-1] == '}')) {
		return
	}
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = lit[1 : len(lit)-1]
}

// emitQueryVarAssign emits an assignment of a query-proc result to goName.
func (tp *transpiler) emitQueryVarAssign(goName, sql string) {
	sqlExpr := tp.buildSQLStringExpr(sql)
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
}
