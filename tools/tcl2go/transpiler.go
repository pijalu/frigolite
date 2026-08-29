// Package main implements the tcl2go tool.
//
// This file defines the transpiler type and low-level emission helpers.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// transpiler converts TCL commands to Go code.
type transpiler struct {
	expectPreFlattened  bool // expectLiteral: word already normalized to flat form
	sb                  *strings.Builder
	indent              int
	dbVar               string
	t                   string
	varCount            int
	vars                []string
	currentTestFile     string                  // TCL test file base name (e.g. "fts4aa"), for wantOverrides lookup
	catchMode           bool                    // true when transpiling inside a catch {} block
	forIncrs            [][][]tcl.RawWord       // stack of for-loop increment clauses (empty for while/foreach)
	pendingFileReset    map[string]bool         // file removed by forcedelete; next sqlite3 open resets the db
	varsetLoopVars      map[string]varsetInfo   // loop vars that iterate over varset structs
	dbAliases           map[string]string       // secondary connection name -> main db var it aliases (same file)
	dqsDDL              bool                    // current SQLITE_DBCONFIG_DQS_DDL state (default true)
	dqsDML              bool                    // current SQLITE_DBCONFIG_DQS_DML state (default true)
	unsetVars           map[string]bool         // TCL vars unset via `unset`; `$var` renders as SQL NULL
	dbVarFuncs          map[string]bool         // `db function NAME proc` registrations: NAME reads a TCL var
	constFuncs          map[string]string       // `proc NAME {args} { return CONST }`: NAME returns CONST
	identityFuncs       map[string]bool         // `proc NAME {x} { return $x }`: NAME returns its first argument
	lindexFuncs         map[string]int          // `proc NAME {x} { lindex $x N }`: NAME returns element N of its arg
	stringMapFuncs      map[string]string       // `proc NAME {x} { return [string map {O N ...} $x] }`: NAME applies replacements in order
	counterFuncs        map[string]string       // `proc NAME {} { incr ::VAR }`: NAME increments VAR
	incrRetFuncs        map[string]IncrProcInfo // `proc N {a} { incr ::V [n]; return K }`: vtabH-style counters
	predFuncs           map[string]string       // `proc NAME {x} { expr $x < N }`: NAME compares its arg
	errorFuncs          map[string]string       // `proc NAME {} { error "MSG" }`: NAME raises MSG
	queryFuncs          map[string]string       // `proc NAME {} { return [db eval {SQL}] }`: NAME returns a query result
	specialFuncs        map[string]string       // test-infra procs (scramble/random_uuid/hash1/hash2) mapped to Go helper calls
	procStringMaps      map[string][]string     // single-arg procs of the form `proc N x {return [string map [list K V ...] $x]}` (flat old/new pairs)
	colmetaCmds         map[string]string       // colmeta.test: TCL var holding "sqlite3_table_column_metadata <args>"
	collateGoFuncs      map[string]string       // `proc NAME {a b} {BODY}`: NAME is a collation proc → Go closure expr
	collateDtorVars     map[string]string       // collation NAME → Go var incremented by sqlite3_create_collation_v2 destructor
	unzipDirs           map[string]bool         // dirs created by `file mkdir D` + `exec ... -d D` procs (extraction skipped)
	joinFuncs           map[string]string       // `proc NAME {args} { return [join $args -] }`: NAME joins its args with SEP
	prefixFuncs         map[string]string       // `proc NAME {args} { return "P: $args" }`: NAME prepends a fixed prefix to its args
	rangeListFuncs      map[string]string       // `proc NAME {} { set L [list]; for ... lappend ... }`: NAME returns a generated list
	varConstValues      map[string]string       // TCL var name → last simple string value (set var "lit")
	foreachLitValues    map[string][]string     // foreach var name → literal braced list values (eval $var inlining)
	intarrayEvalVars    map[string]bool         // TCL var built as an `sqlite3_intarray_bind` script (eval $var → runtime)
	dbConnVars          map[string]bool         // Go var names that are *frigolite.DB connections (opened via sqlite3)
	connPredeclared     map[string]bool         // Go var names pre-declared as *frigolite.DB in the preamble (sqlite3 NAME)
	dbClosed            bool                    // main "db" connection was closed via `db close`
	inEvalScript        bool                    // transpiling inside an eval-inlined script (real sqlite3 db reopen)
	runtimeConnVars     map[string]bool         // Go vars holding a connection NAME at runtime (foreach db {db db2})
	varRenames          map[string]string       // TCL var name → Go loop var it is shadowed by (foreach db {db db2} → db→db_iter)
	testPrefix          string                  // TCL `set testprefix NAME`; prepended to bare test names in skip lookup
	mainDBAlias         string                  // dbconfig_maindbname_<alias>: the test-hook alias for the main database
	queryVars           map[string]bool         // TCL vars known to hold query SQL (set/append to SELECT...)
	regsubSpecs         map[string]regsubSpec    // `set VAR "[regsub ...]"` → captured regsub spec per var; do_test body comparisons against $VAR apply tclRegsub
	arrayKeys           map[string][]string     // TCL array name → literal keys seen (set arr(K) V)
	arrayMapVars        map[string]bool         // TCL array names using dynamic keys (set arr($k) V) → Go map var
	rollbackFlag        string                  // when set, `db eval ROLLBACK` also assigns this Go bool var (db eval {SQL} {body} callback abort)
	interruptFlag       string                  // when set, `sqlite3_interrupt` in a db-eval callback also assigns this Go bool var; the loop aborts after the body
	preupdateHookBody   string                  // body of the TCL `proc preupdate_hook {args} {...}` (emitted as the db preupdate hook closure)
	commitHookBodies    map[string]string       // TCL proc name → body for commit_hook/rollback_hook/update_cb/preupdate_cb procs
	seenProcs           map[string]string       // TCL proc name → body last seen by processProc (redefinition detection)
	procBodies          map[string]string       // every TCL proc body, for later registration sites (preupdate hooks)
	inlineProcs         map[string]string       // zero-parameter procs transpiled inline at call sites
	inlineProcParams    map[string]string       // inline proc name → RAW parameter word (for default binding)
	rowFlatVars         map[string]string       // db-eval array var -> Go expression with the current row's flattened key/value pairs
	commitHookName      string                  // name of the last registered commit hook proc (for `db commit_hook` queries)
	rollbackHookName    string                  // name of the last registered rollback hook proc
	updateHookName      string                  // name of the last registered update hook callback
	preparedState       *preparedState          // shared sqlite3_prepare/bind/step emulation state (pointer so bodyTP copies share it)
	prepareTailVars     map[string]bool         // sqlite3_prepare TAIL argument variables (whitespace-insensitive set-var comparison)
	sqlVarValues        map[string]string       // braced set var → SQL text (for sqlite3_prepare $var classification; kept separate from varConstValues so concat/list accumulation is unaffected)
	connFailedOpen      map[string]string       // connection → sqlite3_open error message (bad-path open emulation)
	connClosed          map[string]bool         // connection closed via sqlite3_close (double-close misuse)
	authTypeName        string                  // Go type name of the last registered TCL authorizer proc (db authorizer ::name)
	authProcCount       int                     // counter for unique generated authorizer type names
	authProcGo          map[string]string       // TCL authorizer proc name → emitted Go type name
	authPreamble        *strings.Builder        // package-level authorizer declarations (authCurrent var + dispatcher)
	authCurrentDeclared bool                    // authCurrent + authDispatcher already emitted in the preamble
	testDir             string                  // TCL test directory (for sourcing helper files like genesis.tcl)
	genesisPreamble     *strings.Builder        // package-level ftsKJVGenesis helper (fts_kjv_genesis data loader)
	ftsBuildPreamble    *strings.Builder        // package-level fts3BuildDB1/fts3BuildDB2 helpers (fts3_build_db_1/2 data loaders)

	// blobChans maps the TCL variable names that hold incremental-blob
	// channel names (`set blob [db incrblob ...]` → blob holds "incrblob_1")
	// to the channel name. read/seek/puts/flush/close on $blob route to the
	// Blob methods via blobChannelVars.
	blobChans map[string]blobChannel
	// blobChannelVars maps a blob channel name (the string returned by
	// `db incrblob`, e.g. "incrblob_1") to its Go *frigolite.Blob variable.
	blobChannelVars map[string]string
	// blobVarNames records every TCL variable name ever used as a blob
	// channel (immune to the blobChans map being wiped between body blocks),
	// so close/read on a channel var can fall back to runtime resolution.
	blobVarNames map[string]bool
	// usedChannels records every blob channel name ever allocated (immune to
	// the blobChannelVars map being wiped between body blocks), so
	// newBlobChannel never reuses a channel across body-block copies.
	usedChannels map[string]bool
	// blobSeq numbers blob channels for unique Go variable names.
	blobSeq int

	// fixtureVar holds the fixture name (e.g. "tf1" from `testfixture $::tf1
	// {...}`) when transpiling inside a testfixture script. While set, `db`
	// refers to that fixture's persistent connection (tclFixtureDBs[name]) and
	// `sqlite3 db FILE` / `db close` operate on the fixture map instead of the
	// main test connection — emulating SQLite's launch_testfixture second
	// process (cross-connection lock semantics apply).
	fixtureVar string
}

// preparedState tracks the prepared-statement emulation state (sqlite3_prepare
// + sqlite3_bind_* + sqlite3_step + sqlite3_reset + sqlite3_finalize). It is
// shared across bodyTP copies via a pointer so a statement prepared in one
// block can be bound/stepped in a later block.
type preparedState struct {
	stmts map[string]string         // TCL stmt var -> prepared SQL text
	binds map[string]map[int]string // stmt var -> bind index -> SQL literal
	conns map[string]string         // stmt var -> Go connection handle
}

// varsetInfo describes a foreach loop variable whose elements are TCL "varset"
// scripts (a sequence of `set name {value}` commands). The transpiler turns
// such loops into Go struct slices and rewrites `eval $var` into field
// assignments.
type varsetInfo struct {
	fields     []string // TCL variable names set by the varsets (struct field order)
	structName string
}

func (tp *transpiler) emit(format string, args ...interface{}) {
	tp.sb.WriteString(strings.Repeat("\t", tp.indent))
	tp.sb.WriteString(fmt.Sprintf(format, args...))
}

func (tp *transpiler) emitLine(format string, args ...interface{}) {
	tp.emit(format, args...)
	tp.sb.WriteString("\n")
}

// isVarDeclared checks if a TCL variable name has already been declared in Go scope.
func (tp *transpiler) isVarDeclared(name string) bool {
	for _, v := range tp.vars {
		if v == name {
			return true
		}
	}
	return false
}

// activePreparedState anchors the prepared-statement emulation state for the
// file currently being generated. A file-level singleton is required because
// sqlite3_prepare registrations inside a do_test body must stay visible to
// later bodies of the same file (bind.test prepares inside its first do_test);
// per-transpiler copies broke that visibility.
var activePreparedState *preparedState

// resetPreparedState clears the per-file prepared-statement emulation state.
func resetPreparedState() { activePreparedState = nil }

// activeAssignedVars holds the Go names of TCL variables that have an
// assignment site in the file currently being generated (see generateTestFile).
// Braced-SQL $var substitution consults this set: tclsqlite.c leaves unknown
// $tokens in the SQL for the engine to report, so mere references (never
// assigned) must not be rewritten into sqlLiteral() substitutions.
var activeAssignedVars []string

// isAssignedTCLVar reports whether gv is a variable with an assignment site.
func isAssignedTCLVar(gv string) bool {
	for _, v := range activeAssignedVars {
		if v == gv {
			return true
		}
	}
	return false
}

// stmtVMTestFiles are the TCL test files whose prepared-statement lifecycle is
// emulated through the runtime Stmt VM helpers (tclPrepareStmt/tclBindStmt/
// tclStepStmt/...). Other files keep the historical literal-substitution
// emulation, whose output the wider corpus was greened against.
var stmtVMTestFiles = map[string]bool{
	"bind": true, "bind2": true,
	"capi2": true, "capi3": true, "capi3b": true,
	"capi3c": true, "capi3d": true, "capi3e": true,
}

// stmtVMEnabled reports whether the file currently being generated uses the
// runtime Stmt VM emulation.
func stmtVMEnabled() bool { return stmtVMTestFiles[genCurrentTestFile] }

// preparedStateRef returns the shared prepared-statement emulation state,
// creating it if needed. Because the state lives at package level and the
// generator processes files sequentially, binds recorded anywhere in a file
// are visible everywhere else in that file.
func (tp *transpiler) preparedStateRef() *preparedState {
	if activePreparedState == nil {
		activePreparedState = &preparedState{
			stmts: make(map[string]string),
			binds: make(map[string]map[int]string),
			conns: make(map[string]string),
		}
	}
	return activePreparedState
}

// isIntegerLiteral reports whether s is a bare integer literal (optionally
// signed), used to decide whether a db progress period or expression-depth
// limit can be transpiled as a Go integer expression.
func isIntegerLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isPreDeclaredDB checks if a variable name is a pre-declared DB connection (db1-db9).
func isPreDeclaredDB(name string) bool {
	if len(name) != 3 || name[:2] != "db" {
		return false
	}
	return name[2] >= '1' && name[2] <= '9'
}

// isMainTestFile reports whether path refers to the main test database file
// (the file that reset_db opens as the primary "db" connection). The TCL
// framework uses "./test.db" (or "test.db") for the main database; secondary
// connections opened on this same file share the primary database.
func isMainTestFile(path string) bool {
	p := strings.TrimSpace(path)
	return p == "test.db" || p == "./test.db" || p == `"test.db"` || p == `"./test.db"`
}

// isValidGoIdent returns true if s is a valid Go identifier (letters, digits,
// underscores; not starting with a digit; no parens or other special chars).
func isValidGoIdent(s string) bool {
	if s == "" {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// tclVarToGo converts a TCL variable name to a valid Go identifier.
func tclVarToGo(name string) string {
	// Strip leading :: (global namespace prefix) so $::var maps to same name as $var
	name = strings.TrimPrefix(name, "::")
	name = strings.ReplaceAll(name, "::", "_")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.ReplaceAll(name, "$", "")
	name = strings.ReplaceAll(name, "!", "_")
	name = strings.ReplaceAll(name, "#", "_")
	name = strings.ReplaceAll(name, "@", "_")
	// Handle TCL array syntax: var(key) → var_key
	if idx := strings.Index(name, "("); idx > 0 && strings.HasSuffix(name, ")") {
		key := name[idx+1 : len(name)-1]
		base := name[:idx]
		if key == "" || key == "*" {
			// Can't represent empty/wildcard key, skip
			return "_" + base + "_arr"
		}
		name = base + "_" + key
	}
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, ".", "_")
	name = strings.ReplaceAll(name, ",", "_")
	name = strings.ReplaceAll(name, "+", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "%", "_pct_")
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "(", "_")
	name = strings.ReplaceAll(name, ")", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "[", "_")
	name = strings.ReplaceAll(name, "]", "_")
	// Quote and control-character sanitize: mangled command-substitution
	// names (resetdb.test's multi-line sqlite3_prepare statement name) can
	// contain quotes/newlines that would break single-line Go identifiers.
	// Only structurally-impossible characters are mapped; operators like
	// '='/'&' must stay INVALID so isValidGoIdent guards downstream keep
	// rejecting garbage names (where2-2.4's "$out2 && $out2!=$out3").
	name = strings.Map(func(r rune) rune {
		switch r {
		case '"', '\'', '`', '\n', '\r', '\t', '\v', '\f':
			return '_'
		}
		return r
	}, name)
	if len(name) > 0 && name[0] >= '0' && name[0] <= '9' {
		name = "v_" + name
	}
	// Avoid Go keywords and names that shadow test framework variables
	switch name {
	case "type", "range", "string", "func", "go", "map", "chan",
		"interface", "struct", "select", "import", "defer",
		"error", "len", "cap", "copy", "append", "new", "make",
		"panic", "print", "println", "complex", "real", "imag",
		"iota", "nil", "true", "false", "var", "const", "package",
		"continue", "break", "goto", "fallthrough", "switch", "case", "default":
		name = "_" + name
	// Avoid shadowing the test framework variable t (*testing.T) and result vars r/_res
	case "t":
		name = "_t"
	case "r":
		name = "_r"
	// Avoid shadowing stdlib imports used by generated code (time, os, strings, etc.)
	case "time", "os", "strings", "strconv", "fmt", "regexp", "filepath", "sort":
		name = "_" + name
	}
	return name
}

// goStringLiteral converts a TCL word to a Go string expression.
// For braces words it's a Go string literal.
// For unbraced words it may contain $var and [cmd] references
// and produces a Go string concatenation expression.
func (tp *transpiler) goStringLiteral(w tcl.RawWord) string {
	if w.Braced {
		return fmt.Sprintf("%q", w.Text)
	}
	if w.Quoted {
		// TCL double-quoted words process backslash escapes: "\n" is a real
		// newline. Resolve them before interpolation so the emitted Go
		// string carries the actual character (not a literal backslash).
		return tp.buildStringExpr(tclUnescapeQuoted(w.Text))
	}
	// Unquoted (bare) words also perform TCL backslash substitution, but
	// $var and [cmd] references must remain for buildStringExpr. Unescape
	// only the sequences that cannot collide with interpolation syntax:
	// octal (\123), hex (\x41), and the recognized letter escapes
	// (\n \t \r \v \f \b \a). \$ and \[ are left intact so buildStringExpr
	// does not misread an escaped literal as a variable/command.
	return tp.buildStringExpr(unescapeBareWord(w.Text))
}

// goByteArrayLiteral renders a byte slice as a Go []byte composite literal.
func (tp *transpiler) goByteArrayLiteral(b []byte) string {
	var sb strings.Builder
	sb.WriteString("[]byte{")
	for i, v := range b {
		if i%20 == 0 {
			sb.WriteString("\n\t")
		}
		sb.WriteString(fmt.Sprintf("0x%02x,", v))
	}
	sb.WriteString("\n}")
	return sb.String()
}

// isHexDigit reports whether c is an ASCII hexadecimal digit.
func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// hexVal returns the numeric value of an ASCII hexadecimal digit.
func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// exprVarValue renders the Go value expression for a TCL variable reference
// inside an expression. sqlite_interrupt_count is special-cased: its live
// value is the engine's leftover interrupt countdown, not the last assigned
// literal (vdbe.c decrements it per opcode).
func (tp *transpiler) exprVarValue(name string) string {
	if name == "sqlite_interrupt_count" {
		return "strconv.Itoa(db.InterruptCount())"
	}
	return tclVarToGo(name)
}
