// Package main implements the tcl2go tool.
//
// This file transpiles TCL command substitution [cmd ...] into Go expressions:
// the cmdExpr dispatcher plus the misc per-command handlers (expr, format,
// subst, db, file, glob, catch, sqlite3 reopen, status queries, execsql and
// the unknown-command fallback).
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// cmdExpr converts a TCL command text (inside [...]) to a Go expression.
func (tp *transpiler) cmdExpr(cmdText string) string {
	cmdText = strings.TrimSpace(cmdText)
	args := tclCmdWords(cmdText)
	if len(args) == 0 {
		return `""`
	}

	cmdName := args[0]
	rest := args[1:]

	if e, ok := tp.cmdExprBuiltinSpecial(cmdName, rest); ok {
		return e
	}

	if h, ok := cmdExprHandlersRef()[cmdName]; ok {
		return h(tp, cmdName, cmdText, rest)
	}
	// [get_pwd] / [pwd] — the TCL harness proc returning the process
	// working directory (tester.tcl get_pwd wraps [pwd]). Resolved at TEST
	// runtime via os.Getwd because the generated test os.Chdir's into its
	// temp dir before running (pragma.test 9.5:
	// `set pwd [string map {' ''} [file nativename [get_pwd]]]`).
	if cmdName == "get_pwd" || cmdName == "pwd" {
		return "tclGetPwd()"
	}
	// A registered single-arg string-map proc (`[tx $ins]`, json101's
	// JSON-shorthand translator) becomes a Go strings.NewReplacer chain on
	// the argument expression.
	if e, ok := tp.cmdExprStringMapProc(cmdName, rest); ok {
		return e
	}
	// [read $CHAN] on an incremental-blob channel — read the blob value
	// (dbstatus2.test 1.7: `set len [string length [read $fd]]`). The
	// channel must be a registered blob channel (db incrblob); file
	// channels are not readable in expression context.
	if cmdName == "read" && len(rest) >= 1 {
		if ch := tp.resolveBlobChannel(tcl.RawWord{Text: rest[0]}); ch != "" {
			return fmt.Sprintf("string(blobReadAll(%s, 0))", ch)
		}
	}
	// Backup-object subcommand substitution: [B step N] / [B finish] /
	// [B remaining] / [B pagecount] for a declared *frigolite.Backup var.
	if e, ok := tp.cmdExprBackupSub(cmdName, rest); ok {
		return e
	}
	return tp.cmdExprDefault(cmdName, cmdText, rest)
}

// cmdExprBuiltinSpecial handles the suite's special-value command
// substitutions that precede the dispatch table. Returns ("", false) when
// cmdName is not one of them.
func (tp *transpiler) cmdExprBuiltinSpecial(cmdName string, rest []string) (string, bool) {
	switch cmdName {
	case "permutation":
		// [permutation] evaluates to the name of the current test permutation,
		// or the empty string when the suite runs without one. testgen always
		// runs without a permutation, so conditions like
		// {[permutation]=="prepare"} become "" == "prepare" (false), which
		// skips the prepare/step C-API blocks the transpiler cannot reproduce.
		return `""`, true
	case "clang_sanitize_address":
		// [clang_sanitize_address] — the TCL harness proc that reports whether
		// the library was built with -fsanitize=address. testgen runs a normal
		// build, so it returns 0 (false); conditions like
		// {[clang_sanitize_address]==0 && 0} then evaluate to false.
		return `"0"`, true
	case "md5":
		if len(rest) >= 1 {
			return tp.cmdExprMD5(rest[len(rest)-1]), true
		}
	case "detail_is_none", "detail_is_col", "detail_is_full":
		// [detail_is_none] / [detail_is_col] / [detail_is_full] —
		// fts5_common.tcl predicates over the foreach_detail_mode loop
		// variable (rendered as the generated _fdmModeN Go var). Resolved to
		// a runtime "1"/"0" so they compose both as bare conditions and
		// inside ==0 numeric comparisons.
		mode := strings.TrimPrefix(cmdName, "detail_is_")
		return fmt.Sprintf("tclBool01(_fdmMode%d == %q)", tp.fdmSeq, mode), true
	case "sqlite3_exec_hex":
		// [sqlite3_exec_hex DB SQL] - test1.c's sqlite3_exec_hex: decodes
		// percent-H-H to raw bytes, executes SQL, returns "<rc> <column names
		// and values>" (like-9.3.1 reads the result for a LIKE with a raw
		// 0x78/0x25 pattern).
		if len(rest) >= 2 {
			sql := strings.TrimSpace(rest[len(rest)-1])
			sql = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(sql, "{"), "}"))
			return fmt.Sprintf("tclExecHex(%s, %q)", tp.dbVar, sql), true
		}
	case "sqlite3_fts5_tokenize":
		// [sqlite3_fts5_tokenize DB TOKENIZER TEXT] — the fts5_tcl.c test
		// bridge (f5tTokenize): returns the flat TCL list "token start end
		// ..." for TEXT tokenized through TOKENIZER (a TCL list of spec
		// words). The generated fts5TclTokenize helper (emitted on demand)
		// routes the request through the engine's own tokenizer registry
		// (internal/fts5).
		if len(rest) >= 3 {
			useFTS5Tokenize()
			spec := tp.buildStringExpr(rest[len(rest)-2])
			input := tp.buildStringExpr(rest[len(rest)-1])
			return fmt.Sprintf("fts5TclTokenize(%s, %s, %s)", tp.dbVar, spec, input), true
		}
	}
	return "", false
}

// cmdExprMD5 renders the [md5 STRING] substitution: the test suite's
// C-extension md5 command (test/md5.c, registered as a TCL command in every
// test through the test build) returns the lowercase hex MD5 digest of
// STRING. func.test 24.7 uses it to build the expected value of md5sum()
// over many arguments (set result [md5 "this${midres}program..."]); the
// harness helper tclMD5 is the faithful implementation. STRING is rendered
// through the string-parts path so ${var} interpolation inside the quoted
// word applies (TCL double-quote substitution semantics).
func (tp *transpiler) cmdExprMD5(arg string) string {
	if strings.Contains(arg, "$") || strings.Contains(arg, "[") {
		return fmt.Sprintf("tclMD5(%s)", tp.buildStringExpr(arg))
	}
	return fmt.Sprintf("tclMD5(%q)", arg)
}

// cmdExprStringMapProc renders a call to a registered single-arg string-map
// proc (e.g. json101's `[tx $ins]`) as a Go strings.NewReplacer chain on the
// argument expression. Returns ok=false when cmdName is not a registered
// string-map proc or the call does not have exactly one argument.
func (tp *transpiler) cmdExprStringMapProc(cmdName string, rest []string) (string, bool) {
	pairs, ok := tp.procStringMaps[cmdName]
	if !ok || len(rest) != 1 {
		return "", false
	}
	quoted := make([]string, len(pairs))
	for i, p := range pairs {
		quoted[i] = strconv.Quote(p)
	}
	return fmt.Sprintf("strings.NewReplacer(%s).Replace(%s)", strings.Join(quoted, ", "), tp.buildStringExpr(rest[0])), true
}

// cmdExprBackupSub handles backup-object subcommand substitution: [B step N]
// / [B finish] / [B remaining] / [B pagecount] for a declared
// *frigolite.Backup var. Here cmdName is the backup variable (B) and
// rest[0] is the subcommand. Returns ("", false) when cmdName is not a valid
// Go identifier or rest is empty (the caller falls through to the default).
func (tp *transpiler) cmdExprBackupSub(cmdName string, rest []string) (string, bool) {
	goName := tclVarToGo(cmdName)
	if !isValidGoIdent(goName) || len(rest) < 1 {
		return "", false
	}
	switch strings.ToLower(rest[0]) {
	case "step":
		if len(rest) >= 2 {
			return cmdExprBackupStep(goName, rest[1]), true
		}
		return fmt.Sprintf("tclBackupStep(%s, 0)", goName), true
	case "finish":
		return cmdExprBackupFinish(goName), true
	case "remaining":
		return cmdExprBackupRemaining(goName), true
	case "pagecount":
		return cmdExprBackupPagecount(goName), true
	}
	return "", false
}

// cmdExprCols handles `[cols s f]` and `[exprs s f]` — TCL test procs from
// existsexpr.test. cols generates "c<s>, c<s+1>, ..., c<f>" (zero-padded to 2
// digits); exprs generates "c<s> = o AND ... AND c<f> = o". Both take numeric
// literals here, so the result is computed at transpile time.
func (tp *transpiler) cmdExprCols(cmdName, cmdText string, args []string) string {
	if len(args) != 2 {
		return fmt.Sprintf("%q", cmdText)
	}
	s, err1 := strconv.Atoi(args[0])
	f, err2 := strconv.Atoi(args[1])
	if err1 != nil || err2 != nil {
		return fmt.Sprintf("%q", cmdText)
	}
	var parts []string
	for i := s; i <= f; i++ {
		if cmdName == "exprs" {
			parts = append(parts, fmt.Sprintf("c%02d = o", i))
		} else {
			parts = append(parts, fmt.Sprintf("c%02d", i))
		}
	}
	if cmdName == "exprs" {
		return fmt.Sprintf("%q", strings.Join(parts, " AND "))
	}
	return fmt.Sprintf("%q", strings.Join(parts, ", "))
}

// cmdExprVals handles `[vals n val]` — TCL test proc from existsexpr.test that
// generates "val, val, ..." n times. The value may be a $var (bound at
// runtime).
func (tp *transpiler) cmdExprVals(cmdName, cmdText string, args []string) string {
	if len(args) != 2 {
		return fmt.Sprintf("%q", cmdText)
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Sprintf("%q", cmdText)
	}
	valExpr := tp.buildStringExpr(args[1])
	if tp.vars != nil && strings.HasPrefix(strings.TrimSpace(args[1]), "$") {
		var segs []string
		for i := 0; i < n; i++ {
			segs = append(segs, "sqlLiteral("+tclVarToGo(strings.TrimPrefix(strings.TrimSpace(args[1]), "$"))+")")
		}
		return strings.Join(segs, " + \", \" + ")
	}
	var segs []string
	for i := 0; i < n; i++ {
		segs = append(segs, valExpr)
	}
	return strings.Join(segs, " + \", \" + ")
}

// eqNeExpr matches a whole-expression `$var eq/ne $var` TCL comparison.
var eqNeExpr = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)\s+(eq|ne)\s+\$([A-Za-z_][A-Za-z0-9_]*)$`)

// cmdExprEval handles `[expr {...}]` — evaluate constant expressions at
// generation time, or emit runtime evaluation for variable expressions.
func (tp *transpiler) cmdExprEval(cmdName, cmdText string, args []string) string {
	exprStr := strings.TrimSpace(strings.TrimPrefix(cmdText, "expr"))
	if len(exprStr) >= 2 && exprStr[0] == '{' && exprStr[len(exprStr)-1] == '}' {
		exprStr = exprStr[1 : len(exprStr)-1]
	}
	if res, err := tcl.EvalExpr(exprStr, nil, nil); err == nil {
		return fmt.Sprintf("%q", res)
	}
	// `$a eq $b` / `$a ne $b` — emit a native Go string comparison so huge
	// runtime values (rtree2.test dump comparisons) don't pass through the
	// token-wise runtime evaluator.
	if m := eqNeExpr.FindStringSubmatch(exprStr); m != nil {
		op := "=="
		if m[2] == "ne" {
			op = "!="
		}
		return fmt.Sprintf("tclBool01(%s %s %s)",
			tp.exprVarValue(strings.TrimPrefix(m[1], "$")), op,
			tp.exprVarValue(strings.TrimPrefix(m[3], "$")))
	}
	// Hexio arithmetic shape: "[hexio_get_int [hexio_read FILE OFF N]]+K"
	// (vacuum2-2.x expected values: the change counter plus an increment).
	if m := hexioReadExpr.FindStringSubmatch(exprStr); m != nil {
		off := m[2]
		return fmt.Sprintf("strconv.FormatInt(tclHexioReadInt(%s, %s, %s)%s, 10)",
			tp.goStringLiteral(tcl.RawWord{Text: m[1]}), off, m[3], m[4])
	}
	// Runtime evaluation: substitute $var references with the Go variable
	// values via a side map, and convert common TCL math functions to Go.
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

// cmdExprStrftime handles `[strftime FORMAT UNIXTIMESTAMP]` (test1.c
// strftime_cmd) — access to the C-library strftime() in UTC, so its results
// can be compared against SQLite's strftime SQL function. Emit a runtime call
// to the tclStrftime helper.
func (tp *transpiler) cmdExprStrftime(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return fmt.Sprintf("%q", cmdText)
	}
	formatExpr := tp.buildStringExpr(args[0])
	tsExpr := tp.buildStringExpr(args[1])
	return fmt.Sprintf("tclStrftime(%s, %s)", formatExpr, tsExpr)
}

// cmdExprFormat handles `[format formatString ?arg ...?]` — printf-style
// formatting. Args may contain $var refs, so the result is computed at runtime
// by the tclFormat helper.
func (tp *transpiler) cmdExprFormat(cmdName, cmdText string, args []string) string {
	if len(args) == 0 {
		return `""`
	}
	formatExpr := tp.buildStringExpr(args[0])
	argExprs := make([]string, 0, len(args)-1)
	for _, a := range args[1:] {
		argExprs = append(argExprs, tp.buildStringExpr(a))
	}
	if len(argExprs) == 0 {
		return fmt.Sprintf("tclFormat(%s)", formatExpr)
	}
	return fmt.Sprintf("tclFormat(%s, %s)", formatExpr, strings.Join(argExprs, ", "))
}

// cmdExprSubst handles `[subst [-nobackslashes] [-nocommands] [-novar] string]`.
func (tp *transpiler) cmdExprSubst(cmdName, cmdText string, args []string) string {
	content := strings.TrimSpace(cmdText[len("subst"):])
	noVar := false
	noCommands := false
	for strings.HasPrefix(content, "-") {
		flag := strings.Fields(content)[0]
		switch flag {
		case "-novar":
			noVar = true
		case "-nocommands":
			noCommands = true
		}
		content = strings.TrimSpace(strings.TrimPrefix(content, flag))
	}
	if len(content) >= 2 && content[0] == '{' && content[len(content)-1] == '}' {
		content = content[1 : len(content)-1]
	}
	content = strings.TrimSpace(content)
	if noVar {
		// subst -novar substitutes [cmd] but NOT $var. In a SQL context
		// (do_execsql_test / execsql), the $var refs are bound as VALUES
		// by db eval, so render them as SQL literals, while [cmd] (e.g.
		// [set op] yielding a comparison operator) renders as raw SQL
		// syntax.
		return tp.renderSubstNovarSQL(content)
	}
	if noCommands {
		// subst -nocommands substitutes $var and backslash escapes but
		// leaves [...] as literal text (e.g. SQL bracket-quoted
		// identifiers like [t1'x1]).
		return tp.buildStringExprNoCmd(content)
	}
	return tp.buildStringExpr(content)
}

// cmdExprSet handles `[set var]` — returns the value of a variable, exactly
// like $var. A DYNAMIC name (`[set $lang]`) reads the variable whose NAME the
// expression evaluates to at runtime: emit a registry lookup against the
// vtab TCL-variable mirror (the named variable is registered there by every
// `set` emitter — fts3ab 1.0's fill_multilanguage_fulltext_t1 builds its
// rows from `[lindex [set $lang] $j]`, picking words from the english/
// spanish/german variables as lang iterates).
func (tp *transpiler) cmdExprSet(cmdName, cmdText string, args []string) string {
	if len(args) >= 1 {
		if strings.HasPrefix(args[0], "$") {
			return fmt.Sprintf("vtab.TclVarGet(%s, \"\")", tclVarToGo(args[0]))
		}
		return tclVarToGo(args[0])
	}
	return `""`
}

// cmdExprDb handles [db one {SQL}] / [db eval {SQL}] value substitution: run
// the query at runtime. `db one` returns the first column of the first row;
// `db eval` returns the flattened query result list. Other `db` subcommands
// fall through to the empty string (autovacuum-2.x's `set av1_data [db eval
// {...}]` path is the common case where the namespace-set codepath also
// handles set, but cmdExpr fallback (e.g. inside a concat/lappend) needs
// both forms).
func (tp *transpiler) cmdExprDb(cmdName, cmdText string, args []string) string {
	rest := strings.TrimSpace(cmdText)
	// [db serialize ?SCHEMA?] — raw image bytes as a Go string
	// (memdb1.test 100: [db serialize] length == page_size × page_count).
	if strings.HasPrefix(rest, "db serialize") {
		schema := strings.TrimSpace(strings.TrimPrefix(rest, "db serialize"))
		schema = strings.Trim(schema, "{} ")
		if schema == "" {
			schema = "main"
		}
		return fmt.Sprintf("string(tclSerialize(%s, %q))", tp.dbVar, schema)
	}
	// [db last_insert_rowid] — the TCL binding of sqlite3_last_insert_rowid.
	// The SQL function last_insert_rowid() reads the same connection state
	// (func.c last_insert_rowid is a wrapper around the C API), so translate
	// the command into a query on the same connection (func.test 7.1).
	if strings.TrimSpace(rest) == "db last_insert_rowid" {
		return fmt.Sprintf("tclDbOne(%s, %q)", tp.dbVar, "SELECT last_insert_rowid()")
	}
	// [db eval {SQL}] — flattened query result via tclExecSQL.
	if strings.HasPrefix(rest, "db eval") {
		sql := strings.TrimSpace(rest[len("db eval"):])
		if len(sql) >= 2 && sql[0] == '{' && sql[len(sql)-1] == '}' {
			sql = sql[1 : len(sql)-1]
		}
		return fmt.Sprintf("tclExecSQL(%s, %q)", tp.dbVar, sql)
	}
	// [db one {SQL}] — first column of first row.
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "db one"))
	if rest == "" {
		return `""`
	}
	if len(rest) >= 2 && rest[0] == '{' && rest[len(rest)-1] == '}' {
		rest = rest[1 : len(rest)-1]
	}
	return fmt.Sprintf("tclDbOne(%s, %q)", tp.dbVar, rest)
}

// cmdExprPwd handles [pwd]: the process working directory as a runtime
// expression (vtabH 3.x builds absolute glob patterns from it).
func (tp *transpiler) cmdExprPwd(cmdName, cmdText string, args []string) string {
	return `func() string { wd, _ := os.Getwd(); return wd }()`
}

// cmdExprGlob handles `[glob -nocomplain PATTERN]` — return the TCL list of
// matching file paths (tclGlob). SQLite's glob never raises on no match, so
// -nocomplain is implicit; other flags (-directory, -join, -tails, -types,
// --) are skipped. The pattern is rendered as a string expression so variable
// references work. The caller (e.g. foreach) wraps the result in tclSplitList
// to iterate the matches.
func (tp *transpiler) cmdExprGlob(cmdName, cmdText string, args []string) string {
	var patterns []string
	for _, a := range args {
		if a == "-nocomplain" || a == "-join" || a == "-tails" || a == "-types" || a == "--" || strings.HasPrefix(a, "-") {
			continue
		}
		patterns = append(patterns, tp.buildStringExpr(a))
	}
	if len(patterns) == 0 {
		return `""`
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = fmt.Sprintf("tclGlob(%s)", p)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, " + \" \" + ")
}

// cmdExprCatch handles `[catch {db eval {SQL}}]` — return "1" when the SQL
// uses a function the engine does not implement (so guards like
// ![catch {SELECT f(...)}] correctly skip the body, matching SQLite's #ifdef
// feature selection).
func (tp *transpiler) cmdExprCatch(cmdName, cmdText string, args []string) string {
	joined := strings.Join(args, " ")
	for _, fn := range []string{"soundex(", "unistr_quote("} {
		if strings.Contains(strings.ToLower(joined), fn) {
			return `"1"`
		}
	}
	// A caught reopen — `lindex [catch {sqlite3 db test.db}] 0` (collate3-4.8.2)
	// — must still reopen the connection: the catch only wraps the rc, the
	// sqlite3 command is the observable side effect. Delegate to the sqlite3
	// reopen expression and report rc 0 (open success). TCL's real catch
	// returns 1 on failure, but a failed reopen is fatal in the generated
	// tests anyway (t.Fatal), mirroring the direct-emission shape.
	if len(args) >= 1 {
		if cmds := tcl.ParseCommands(strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(args[0]), "}"), "{")); len(cmds) == 1 && len(cmds[0]) >= 3 && cmds[0][0].Text == "sqlite3" {
			sqliteArgs := make([]string, 0, len(cmds[0])-1)
			for _, w := range cmds[0][1:] {
				sqliteArgs = append(sqliteArgs, w.Text)
			}
			reopen := tp.cmdExprSqlite3("sqlite3", strings.Join(sqliteArgs, " "), sqliteArgs)
			if strings.HasSuffix(reopen, "}()") {
				return fmt.Sprintf("func() string { _ = %s; return \"0\" }()", reopen)
			}
		}
	}
	// Simplified: catch just returns "0" (no error)
	return `"0"`
}

// cmdExprFile handles `[file tail $path]` — basename of a path (used by
// attach4's database_list callback to strip the directory from the file
// column).
func (tp *transpiler) cmdExprFile(cmdName, cmdText string, args []string) string {
	if len(args) >= 2 && args[0] == "join" {
		// [file join P ...] — platform path join (unix "/" separator).
		// quota.test 3.3.1's want: [file join [get_pwd] test.db].
		parts := make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			parts = append(parts, tp.buildStringExpr(a))
		}
		return fmt.Sprintf("filepath.Join(%s)", strings.Join(parts, ", "))
	}
	if len(args) >= 2 && args[0] == "tail" {
		pathExpr := tp.buildStringExpr(args[1])
		return fmt.Sprintf("filepath.Base(%s)", pathExpr)
	}
	if len(args) >= 2 && args[0] == "dirname" {
		pathExpr := tp.buildStringExpr(args[1])
		return fmt.Sprintf("filepath.Dir(%s)", pathExpr)
	}
	if len(args) >= 2 && args[0] == "size" {
		return cmdExprFileSize(tp, cmdName, cmdText, args[1:])
	}
	if len(args) >= 2 && args[0] == "nativename" {
		// [file nativename P] — the platform-native form of a path. On
		// unix (the testgen platform) this is the path itself with no
		// transformation (Tcl only rewrites separators/escapes on
		// Windows). pragma.test 9.5: [file nativename [get_pwd]].
		return tp.buildStringExpr(args[1])
	}
	return fmt.Sprintf("%q", cmdText)
}

// cmdExprSqlite3 handles `[sqlite3 db <file>]` — reopen a connection inside a
// command substitution (TCL: `set ::DB [sqlite3 db test.db]`). Emit a
// side-effecting closure that reassigns the connection, returning an empty
// string placeholder (the handle is not used in Go).
func (tp *transpiler) cmdExprSqlite3(cmdName, cmdText string, args []string) string {
	// `[sqlite3 -has-codec]` probes the codec build flag (always false in
	// this pure-Go port; autovacuum.test picks ptrmap pages {207,412}
	// on the false branch).
	if len(args) == 1 && strings.HasPrefix(args[0], "-") {
		return `""`
	}
	if len(args) < 2 {
		return `""`
	}
	goName := tclVarToGo(args[0])
	filename := tp.buildStringExpr(args[1])
	// A preceding forcedelete of the file means the reopen starts from
	// a fresh database on the real file (matching SQLite).
	if tp.pendingFileReset[args[1]] {
		delete(tp.pendingFileReset, args[1])
		filename = tp.buildStringExpr(args[1])
	}
	tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
	tp.dqsDML = true
	return fmt.Sprintf("func() string { %s, err = frigolite.Open(%s); if err != nil { t.Fatal(err) }; return \"\" }()", goName, filename)
}

// cmdExprDbStatus handles `[sqlite3_db_status db NAME reset]` — return the
// TCL list "{current highwater 0}" for the named per-connection status
// counter. dbstatus.test extracts the current value with `lindex ... 1`.
func (tp *transpiler) cmdExprDbStatus(cmdName, cmdText string, args []string) string {
	dbConn := "db"
	if len(args) >= 1 {
		dbConn = tp.dbArgGo(args[0])
	}
	name := "\"SQLITE_DBSTATUS_CACHE_USED\""
	if len(args) >= 2 {
		name = tp.buildStringExpr(args[1])
	}
	return fmt.Sprintf("tclDbStatus(%s, %s)", dbConn, name)
}

// cmdExprStatus handles `[sqlite3_status NAME reset]` — return the TCL list
// "{current highwater 0}" for the named global status counter. The engine
// reports through the current connection (the tests use one connection).
func (tp *transpiler) cmdExprStatus(cmdName, cmdText string, args []string) string {
	name := "\"SQLITE_STATUS_MEMORY_USED\""
	if len(args) >= 1 {
		name = tp.buildStringExpr(args[0])
	}
	return fmt.Sprintf("tclStatus(%s, %s)", tp.dbVar, name)
}

// cmdExprStmtStatus handles `[sqlite3_stmt_status $stmt NAME reset]` — return
// the named prepared-statement counter as a decimal string (usable in
// `expr [sqlite3_stmt_status ...]>0` comparisons, dbstatus.test 5.5.x).
func (tp *transpiler) cmdExprStmtStatus(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `"0"`
	}
	nameExpr := tp.buildStringExpr(args[1])
	return fmt.Sprintf("strconv.FormatInt(%s.StmtStatus(%s), 10)", tp.dbVar, nameExpr)
}

// cmdExprExecSQL handles `[execsql {SQL}]` / `[execsql2 {SQL}]` — execute SQL
// and return the joined result values as a space-separated string (for
// string-equal comparisons in tests). The argument may be a double-quoted word
// (strip quotes, resolve backslash escapes and line continuations) with
// $var/[cmd] refs.
func (tp *transpiler) cmdExprExecSQL(cmdName, cmdText string, args []string) string {
	sqlText := strings.TrimSpace(cmdText[len(cmdName):])
	if len(sqlText) >= 2 && sqlText[0] == '"' && sqlText[len(sqlText)-1] == '"' {
		sqlText = sqlText[1 : len(sqlText)-1]
	}
	// Braced word ({SELECT ...}) — the standard TCL quoting for SQL text.
	if len(sqlText) >= 2 && sqlText[0] == '{' && sqlText[len(sqlText)-1] == '}' {
		sqlText = sqlText[1 : len(sqlText)-1]
	}
	sqlText = tclUnescapeQuoted(sqlText)
	return fmt.Sprintf("tclExecSQL(db, %s)", tp.buildStringExpr(sqlText))
}

// cmdExprDefault handles unknown command substitutions: test-infrastructure
// procs (scramble/random_uuid/hash1/hash2) with runtime Go equivalents, and
// otherwise the raw command text as a literal.
func (tp *transpiler) cmdExprDefault(cmdName, cmdText string, args []string) string {
	// [eval SCRIPT] — TCL's eval runs the script as a command. The common
	// testgen pattern is `[eval concat $list]` which flattens a list of
	// lists into a single space-separated list.
	if cmdName == "eval" {
		return tp.cmdExprDefaultEval(cmdText)
	}
	// [catchsql DB SQL] inside an expression (zipfile2: [lindex [catchsql
	// db {SQL}] 0]) evaluates to the TCL list text {code rows-or-message}.
	if cmdName == "catchsql" && len(cmdText) > len("catchsql") {
		return tp.cmdExprDefaultCatchsql(cmdText, args)
	}
	// Range-list procs (e.g. vtabI.test's all_col_list building "c1 ... cN")
	// return generated data, not SQL: substitute the collected list value.
	if listVal, ok := tp.rangeListFuncs[cmdName]; ok {
		return fmt.Sprintf("%q", listVal)
	}
	// Test-infrastructure procs (scramble/random_uuid/hash1/hash2) with
	// runtime Go equivalents. The template's $data placeholder is replaced
	// with the first argument (e.g. `[scramble $data]` →
	// tclScramble(data)); hash1/hash2 read the global data list variable.
	if tmpl, ok := tp.specialFuncs[cmdName]; ok {
		return tp.cmdExprDefaultSpecial(tmpl, cmdName, cmdText, args)
	}
	return fmt.Sprintf("%q", cmdText)
}

// cmdExprDefaultEval renders [eval SCRIPT]: re-tokenize the script and
// recursively call cmdExpr on it — when the first word is a list-producing
// command (concat / list / lsort), the result IS the list value. Other `eval`
// forms (procedures, math) are N-A for the testgen — emit an empty string so
// the caller doesn't crash.
func (tp *transpiler) cmdExprDefaultEval(cmdText string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(cmdText, "eval"))
	words := tclCmdWords(rest)
	if len(words) == 0 {
		return `""`
	}
	// Re-emit the script and recurse into cmdExpr so the inner
	// command (concat/list/lsort) is evaluated through its handler.
	inner := strings.Join(words, " ")
	return tp.cmdExpr(inner)
}

// cmdExprDefaultCatchsql renders [catchsql DB SQL] as the TCL list text
// {code rows-or-message}.
func (tp *transpiler) cmdExprDefaultCatchsql(cmdText string, args []string) string {
	// NOTE: tclCmdWords can drop a multi-line braced SQL argument, so
	// split the connection word from cmdText directly.
	tail := strings.TrimSpace(cmdText[len("catchsql"):])
	var dbExpr, sqlText string
	switch {
	case strings.HasPrefix(tail, "{"):
		// catchsql {SQL} — default connection.
		dbExpr = tp.dbArgGo("db")
		sqlText = tail
	default:
		i := strings.IndexAny(tail, " \t\n")
		if i < 0 {
			i = len(tail)
		}
		connTok := tail[:i]
		if len(args) >= 2 {
			dbExpr = tp.dbArgGo(args[0])
			sqlText = strings.TrimSpace(strings.TrimPrefix(tail, connTok))
		} else {
			dbExpr = tp.dbArgGo(connTok)
			sqlText = strings.TrimSpace(tail[i:])
		}
	}
	if strings.HasPrefix(sqlText, "{") && strings.HasSuffix(sqlText, "}") {
		sqlText = sqlText[1 : len(sqlText)-1]
	}
	sqlExpr := tp.goStringLiteral(tcl.RawWord{Text: strings.TrimSpace(sqlText)})
	return fmt.Sprintf("tclCatchsqlStr(%s, %s)", dbExpr, sqlExpr)
}

// cmdExprDefaultSpecial renders a registered test-infrastructure proc call
// through its Go template.
func (tp *transpiler) cmdExprDefaultSpecial(tmpl, cmdName, cmdText string, args []string) string {
	if strings.Contains(tmpl, "$data") {
		dataExpr := "data"
		if len(args) >= 1 {
			dataExpr = tp.buildStringExpr(args[0])
		}
		return strings.Replace(tmpl, "$data", dataExpr, 1)
	}
	// blob() hex decoder used as a value: decode the argument's
	// hex text into the raw byte string (zipfile2 `set blob [blob $x]`).
	if tmpl == "tclBlobHexDecode" {
		argExpr := `""`
		// Tokenize the tail with the TCL word splitter so nested
		// bracket substitutions survive intact
		// ([blob [string map {0800 0900} $a]] used to lose the map).
		rest := strings.TrimSpace(strings.TrimPrefix(cmdText, cmdName))
		if w := tclCmdWords(rest); len(w) >= 1 {
			argExpr = tp.buildStringExpr(w[0])
		}
		return fmt.Sprintf("string(tclHexDecode(%s))", argExpr)
	}
	// 2-arg template (e.g. `tclMakeStr($a, $b)` for autovacuum.test's
	// `make_str char len` proc): substitute $a with args[0] and $b
	// with args[1] (both rendered as Go string expressions). When
	// fewer args are supplied, fall back to zero values to keep the
	// generated code compiling.
	if strings.Contains(tmpl, "$a") && strings.Contains(tmpl, "$b") {
		aExpr := `""`
		bExpr := "0"
		if len(args) >= 1 {
			aExpr = tp.buildStringExpr(args[0])
		}
		if len(args) >= 2 {
			// `len` is a TCL integer; buildStringExpr returns a
			// string literal — wrap with strconv.Atoi when the
			// runtime helper wants an int.
			bExpr = "tclToInt(" + tp.buildStringExpr(args[1]) + ")"
		}
		out := strings.Replace(tmpl, "$a", aExpr, 1)
		out = strings.Replace(out, "$b", bExpr, 1)
		return out
	}
	return tmpl
}

// hexioReadExpr matches an expected-value expression of the shape
// "[hexio_get_int [hexio_read FILE OFF N]]+K" (or -K): a header-word read
// plus a small integer adjustment (vacuum2-2.x).
var hexioReadExpr = regexp.MustCompile(`(?i)^\[hexio_get_int \[hexio_read (\S+) (\d+) (\d+)\]\]([+-]\d+)$`)

// hoistedHexioReadExpr recognizes a do_test EXPECTED argument that reads the
// database header through hexio (the vacuum2-2.x change-counter checks). TCL
// evaluates the expected argument BEFORE running the body, so the read must
// be hoisted ahead of the generated body — inlining it at comparison time
// (the buildStringExpr path) reads the counter twice after the body ran,
// making the assertion unpassable. Returns the Go expression to hoist.
func (tp *transpiler) hoistedHexioReadExpr(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	if strings.HasPrefix(text, "[expr ") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[len("[expr ") : len(text)-1])
		// [expr {...}] — a braced expr script: strip the script braces.
		if len(text) >= 2 && text[0] == '{' && text[len(text)-1] == '}' {
			text = strings.TrimSpace(text[1 : len(text)-1])
		}
	}
	m := hexioReadExpr.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return fmt.Sprintf("strconv.FormatInt(tclHexioReadInt(%s, %s, %s)%s, 10)",
		tp.goStringLiteral(tcl.RawWord{Text: m[1]}), m[2], m[3], m[4]), true
}
