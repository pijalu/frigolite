// Package main implements the tcl2go tool.
//
// This file contains the command-expression dispatch table
// ([cmd ...] → Go expression) and the handlers registered in it.
package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// cmdExprHandler emits a Go expression for a TCL command substitution.
// args excludes the command name word; cmdText is the full command text.
type cmdExprHandler func(tp *transpiler, cmdName, cmdText string, args []string) string

// cmdExprHandlers maps TCL command names to their Go expression emitters.
// Built lazily (see cmdExprHandlersRef) because handler bodies may transitively
// reference cmdExpr through the string-expression builders.
var (
	cmdExprHandlersOnce sync.Once
	cmdExprHandlers     map[string]cmdExprHandler
)

// cmdExprHandlersRef returns the command-expression dispatch table, building it
// on first use.
func cmdExprHandlersRef() map[string]cmdExprHandler {
	cmdExprHandlersOnce.Do(func() {
		cmdExprHandlers = buildCmdExprHandlers()
	})
	return cmdExprHandlers
}

func buildCmdExprHandlers() map[string]cmdExprHandler {
	return map[string]cmdExprHandler{
		"cols":                (*transpiler).cmdExprCols,
		"exprs":               (*transpiler).cmdExprCols,
		"vals":                (*transpiler).cmdExprVals,
		"expr":                (*transpiler).cmdExprEval,
		"strftime":            (*transpiler).cmdExprStrftime,
		"format":              (*transpiler).cmdExprFormat,
		"subst":               (*transpiler).cmdExprSubst,
		"set":                 (*transpiler).cmdExprSet,
		"concat":              (*transpiler).cmdExprConcat,
		"string":              (*transpiler).cmdExprString,
		"binary":              (*transpiler).cmdExprBinary,
		"db":                  (*transpiler).cmdExprDb,
		"catch":               (*transpiler).cmdExprCatch,
		"list":                (*transpiler).cmdExprList,
		"lindex":              (*transpiler).cmdExprLIndex,
		"llength":             (*transpiler).cmdExprLLength,
		"split":               (*transpiler).cmdExprSplit,
		"lsearch":             (*transpiler).cmdExprLSearch,
		"lrange":              (*transpiler).cmdExprLRange,
		"lreplace":            (*transpiler).cmdExprLReplace,
		"lsort":               (*transpiler).cmdExprLSort,
		"file":                (*transpiler).cmdExprFile,
		"glob":                (*transpiler).cmdExprGlob,
		"pwd":                 (*transpiler).cmdExprPwd,
		"sqlite3":             (*transpiler).cmdExprSqlite3,
		"join":                (*transpiler).cmdExprJoin,
		"execsql":             (*transpiler).cmdExprExecSQL,
		"execsql2":            (*transpiler).cmdExprExecSQL,
		"sqlite3_db_status":   (*transpiler).cmdExprDbStatus,
		"sqlite3_status":      (*transpiler).cmdExprStatus,
		"sqlite3_stmt_status": (*transpiler).cmdExprStmtStatus,
		"sqlite3_step": func(tp *transpiler, cmdName, cmdText string, args []string) string {
			return `"SQLITE_ROW"` // stepping implicit in frigolite
		},
		"sqlite3_finalize": func(tp *transpiler, cmdName, cmdText string, args []string) string {
			return `"SQLITE_OK"` // finalize of a successful statement returns SQLITE_OK
		},
		"sqlite3_next_stmt": func(tp *transpiler, cmdName, cmdText string, args []string) string {
			// The engine has no statement registry to iterate; sqlite3_next_stmt
			// on a connection with no prepared statements returns NULL ("").
			return `""`
		},
		"stepsql": func(tp *transpiler, cmdName, cmdText string, args []string) string {
			// stepsql DB {SQL} runs the SQL (side effects via the top-level
			// processStepsql handler) and returns the first result code; a
			// successful step returns 0. Used by `set x [stepsql ...]` bodies
			// whose first list element is the result code.
			return `"0"`
		},
		"sqlite3_prepare_v2": func(tp *transpiler, cmdName, cmdText string, args []string) string {
			return `""` // preparation handled by frigolite internally
		},
		"sqlite3_bind_parameter_count": sqlite3BindParameterCount,
		"sqlite3_bind_parameter_name":  (*transpiler).sqlite3BindParameterName,
		"sqlite3_bind_parameter_index": (*transpiler).sqlite3BindParameterIndex,
		"sqlite3_column_count":         sqlite3ColumnCount,
		"sqlite3_data_count":           sqlite3DataCount,
		"sqlite3_column_name":          (*transpiler).sqlite3ColumnName,
		"sqlite3_column_text":          (*transpiler).sqlite3ColumnText,
		"sqlite3_column_int":           (*transpiler).sqlite3ColumnInt,
		"sqlite3_column_double":        (*transpiler).sqlite3ColumnDouble,
		"build_database":               (*transpiler).cmdExprBuildDatabase,
		"regexp":                       (*transpiler).cmdExprRegexp,
		"array":                        (*transpiler).cmdExprArrayExpr,
		"info":                         (*transpiler).cmdExprInfo,
		"sqlite3_errmsg":               cmdExprErrmsg,
		"sqlite3_errcode":              cmdExprErrcode,
		"sqlite3_set_errmsg":           sqlite3SetErrmsgExpr,
		"sqlite3_bind_int":             sqlite3BindExpr,
		"sqlite3_bind_int64":           sqlite3BindExpr,
		"sqlite3_bind_text":            sqlite3BindExpr,
		"sqlite3_bind_text16":          sqlite3BindExpr,
		"sqlite3_bind_double":          sqlite3BindExpr,
		"sqlite3_bind_null":            sqlite3BindExpr,
		"sqlite3_bind_blob":            sqlite3BindExpr,
		"sqlite3_open":                 sqlite3OpenExpr,
		"sqlite3_open16":               sqlite3OpenExpr,
		"sqlite3_open_v2":              sqlite3OpenExpr,
		"sqlite3_open_new":             sqlite3OpenExpr,
		"sqlite3_open_old":             sqlite3OpenExpr,
	}
}

// sqlite3BindParameterCount renders [sqlite3_bind_parameter_count $VM].
func sqlite3BindParameterCount(tp *transpiler, cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `"0"`
	}
	return fmt.Sprintf("strconv.Itoa(tclParamCountOf(%q))", stmtVarFromArg(args[0]))
}

// sqlite3BindParameterName renders [sqlite3_bind_parameter_name $VM N].
func (tp *transpiler) sqlite3BindParameterName(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `""`
	}
	return fmt.Sprintf("tclParamNameOf(%q, %s)", stmtVarFromArg(args[0]), tp.exprIntArg(args[1]))
}

// sqlite3BindParameterIndex renders [sqlite3_bind_parameter_index $VM NAME].
func (tp *transpiler) sqlite3BindParameterIndex(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `"0"`
	}
	return fmt.Sprintf("strconv.Itoa(tclParamIndexOf(%q, %s))", stmtVarFromArg(args[0]), tp.buildStringExpr(args[1]))
}

// sqlite3ColumnCount renders [sqlite3_column_count $VM].
func sqlite3ColumnCount(tp *transpiler, cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `"0"`
	}
	return fmt.Sprintf("strconv.Itoa(tclColumnCount(%q))", stmtVarFromArg(args[0]))
}

// sqlite3DataCount renders [sqlite3_data_count $VM].
func sqlite3DataCount(tp *transpiler, cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `"0"`
	}
	return fmt.Sprintf("strconv.Itoa(tclDataCount(%q))", stmtVarFromArg(args[0]))
}

// sqlite3ColumnName renders [sqlite3_column_name $VM N].
func (tp *transpiler) sqlite3ColumnName(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `""`
	}
	return fmt.Sprintf("tclColumnNameOf(%q, %s)", stmtVarFromArg(args[0]), tp.exprIntArg(args[1]))
}

// sqlite3ColumnText renders [sqlite3_column_text $VM N].
func (tp *transpiler) sqlite3ColumnText(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `""`
	}
	return fmt.Sprintf("tclColumnTextOf(%q, %s)", stmtVarFromArg(args[0]), tp.exprIntArg(args[1]))
}

// sqlite3ColumnInt renders [sqlite3_column_int $VM N].
func (tp *transpiler) sqlite3ColumnInt(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `"0"`
	}
	return fmt.Sprintf("tclColumnTextOf(%q, %s)", stmtVarFromArg(args[0]), tp.exprIntArg(args[1]))
}

// sqlite3ColumnDouble renders [sqlite3_column_double $VM N].
func (tp *transpiler) sqlite3ColumnDouble(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `"0"`
	}
	return fmt.Sprintf("tclColumnDoubleOf(%q, %s)", stmtVarFromArg(args[0]), tp.exprIntArg(args[1]))
}

// cmdExprRegexp renders [regexp PATTERN STRING]: an unanchored ARE match,
// "1"/"0" (misc3-6.11: [regexp { 4.5678 } $x] capability probes).
func (tp *transpiler) cmdExprRegexp(cmdName, cmdText string, args []string) string {
	if len(args) != 2 {
		return fmt.Sprintf("%q", cmdText)
	}
	pattern := strings.TrimSpace(args[0])
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "{"), "}")
	return fmt.Sprintf("tclRegexpMatch(%q, %s)", pattern, tp.buildStringExpr(args[1]))
}

// cmdExprBuildDatabase renders [build_database N PARAM] — fts3sort.test's
// fixture table builder (fts3SortBuildDatabase).
func (tp *transpiler) cmdExprBuildDatabase(cmdName, cmdText string, args []string) string {
	nRowExpr := "1000"
	paramExpr := `""`
	if len(args) > 0 {
		nRowExpr = tp.intArgExpr(args[0])
	}
	if len(args) > 1 {
		paramExpr = tp.buildStringExpr(args[1])
	}
	return fmt.Sprintf("fts3SortBuildDatabase(db, %s, %s)", nRowExpr, paramExpr)
}

// cmdExprArrayExpr renders [array get VAR] / [array names ARR] value
// substitution.
func (tp *transpiler) cmdExprArrayExpr(cmdName, cmdText string, args []string) string {
	// [array get VAR]: flattened key/value pairs. Inside a db-eval
	// row loop, VAR refers to the current row's column bindings
	// (pre-computed flat expression); otherwise it is a dynamic-key
	// Go map (XxxMap) — but only when VAR is a registered dynamic
	// array (the preamble declares only those as maps). Unregistered
	// arrays keep the literal-text fallback (mutex1 2.x iterates
	// [array get counters] whose proc writer is unsupported anyway).
	if len(args) >= 1 && args[0] == "get" && len(args) == 2 {
		if e, ok := tp.cmdExprArrayGet(args[1]); ok {
			return e
		}
	}
	// [array names ARR]: the keys of a literal-key array are known
	// at generation time (trackArrayKey records every `set arr(K)
	// V`), so emit them as a literal TCL list string. TCL's `array
	// names` returns keys in unspecified order; consumers either
	// sort them or use them as a mapping table (fts4unicode 1.x
	// builds the `mappings` table for `string map` from
	// `[array names map]`).
	if len(args) == 2 && args[0] == "names" {
		base := strings.TrimPrefix(strings.TrimSpace(args[1]), "::")
		if keys, ok := tp.arrayKeys[base]; ok && len(keys) > 0 {
			return strconv.Quote(strings.Join(keys, " "))
		}
	}
	return fmt.Sprintf("%q", cmdText)
}

// cmdExprArrayGet renders [array get VAR] when VAR resolves to a known
// value source. Returns ok=false to keep the literal-text fallback.
func (tp *transpiler) cmdExprArrayGet(arg string) (string, bool) {
	base := strings.TrimPrefix(strings.TrimSpace(arg), "::")
	if e, ok := tp.rowFlatVars[base]; ok {
		return e, true
	}
	if tp.arrayMapVars[base] || tp.arrayMapVars["::"+base] {
		return fmt.Sprintf("tclArrayGetFlat(%s)", tclVarToGo(base)+"Map"), true
	}
	return "", false
}

// cmdExprInfo renders [info exists ...] value substitution.
func (tp *transpiler) cmdExprInfo(cmdName, cmdText string, args []string) string {
	// [info exists ARR($key)] / [info exists VAR]
	// [info exists VAR] — scalar existence goes through the shared
	// variable registry so harness guards like
	// {[info exists ::UNZIP]} reflect whether an earlier branch ran.
	if len(args) == 2 && args[0] == "exists" {
		return tp.cmdExprInfoExists(cmdText, args[1])
	}
	if len(args) == 3 && args[0] == "exists" {
		return tp.cmdExprInfoExistsKeyed(cmdText, args[1], args[2])
	}
	return fmt.Sprintf("%q", cmdText)
}

// cmdExprInfoExists renders the 2-arg [info exists NAME] form: the harness
// options array probe, the dynamic-key map form, and the scalar registry
// lookup.
func (tp *transpiler) cmdExprInfoExists(cmdText, arg string) string {
	nm := strings.TrimPrefix(strings.TrimPrefix(arg, "$"), "::")
	// The harness options array ::G is set by the TCL test
	// runner's command line (-soak, -perm, ...). The Go harness
	// never sets any option, so `info exists ::G(anything)` is
	// always false (corruptC's issoak, corruptN's perm:presql).
	if nm == "G" || strings.HasPrefix(nm, "G(") {
		return `"0"`
	}
	// Dynamic-key form: `info exists NAME($key)` (parsed
	// as a single arg with `(` because the TCL parser
	// does not split it). Translate to a Go map lookup
	// when the array is registered in arrayMapVars
	// (set by `array set`).
	if idx := strings.Index(nm, "("); idx > 0 {
		if e, ok := tp.cmdExprInfoMapLookup(nm, idx); ok {
			return e
		}
	}
	if isValidGoIdent(tclVarToGo(nm)) {
		return fmt.Sprintf("tclBool01(vtab.TclVarExists(%q, \"\"))", nm)
	}
	return fmt.Sprintf("%q", cmdText)
}

// cmdExprInfoMapLookup renders the dynamic-key form
// `info exists NAME($key)` as a Go map lookup when the array is registered
// in arrayMapVars (its XxxMap is declared in the preamble); anything else
// (thread003's thread_spawn-populated finished(), permutations' ::env)
// would reference an undeclared map — fall back to the tclvar registry,
// which stays compilable and answers "no" for arrays the harness never
// populated.
func (tp *transpiler) cmdExprInfoMapLookup(nm string, idx int) (string, bool) {
	rawBase := nm[:idx]
	base := tclVarToGo(rawBase + "Map")
	rawKey := nm[idx+1 : len(nm)-1] // e.g. "$i" (dynamic) or "5" (literal)
	key := strings.TrimPrefix(rawKey, "$")
	if !isValidGoIdent(base[:len(base)-len("Map")]) {
		return "", false
	}
	// A leading $ sigil means the key is a variable
	// reference — the Go-side var of that name holds the
	// runtime value; anything else is a literal key and
	// must be quoted. (Decide BEFORE stripping the sigil:
	// `unusable_page($i)` and `unusable_page(i)` differ
	// only in it.)
	if isArrayMapBacked(tp, rawBase) {
		if strings.HasPrefix(rawKey, "$") {
			return fmt.Sprintf("tclBool01(%s[%s] != \"\")", base, key), true
		}
		return fmt.Sprintf("tclBool01(%s[%q] != \"\")", base, key), true
	}
	if strings.HasPrefix(rawKey, "$") {
		// Route the key variable through the sanitizer (a TCL
		// var named `t` maps to Go `_t`, never *testing.T).
		return fmt.Sprintf("tclBool01(vtab.TclVarExists(%q, %s))", rawBase, tclVarToGo(key)), true
	}
	return fmt.Sprintf("tclBool01(vtab.TclVarExists(%q, %q))", rawBase, key), true
}

// cmdExprInfoExistsKeyed renders the 3-arg [info exists NAME KEY] form (the
// TCL parser split the parenthesized key into a separate word).
func (tp *transpiler) cmdExprInfoExistsKeyed(cmdText, name, keyArg string) string {
	key := strings.TrimPrefix(keyArg, "$")
	if idx := strings.Index(name, "("); idx > 0 {
		rawBase := strings.TrimSuffix(name[:idx], "(")
		base := tclVarToGo(rawBase + "Map")
		kv := tclVarToGo(key)
		if isArrayMapBacked(tp, rawBase) {
			return fmt.Sprintf("tclBool01(%s[%s] != \"\")", base, kv)
		}
		// Unregistered array: the tclvar registry keeps the check
		// compilable (the harness never populates such arrays).
		return fmt.Sprintf("tclBool01(vtab.TclVarExists(%q, %s))", rawBase, kv)
	}
	return fmt.Sprintf("%q", cmdText)
}

// sqlite3BindExpr returns "" for parameter binding (handled via SQL $N/?
// syntax).
func sqlite3BindExpr(tp *transpiler, cmdName, cmdText string, args []string) string {
	return `""`
}

// sqlite3OpenExpr returns "" — sqlite3_open returns a handle; represent as an
// empty string placeholder.
func sqlite3OpenExpr(tp *transpiler, cmdName, cmdText string, args []string) string {
	return `""`
}

// stmtVarFromArg converts a TCL statement-handle argument ("$VM") to the
// registry name used by the tclPrepared map ("VM").
func stmtVarFromArg(arg string) string {
	return strings.TrimPrefix(strings.TrimSpace(arg), "$")
}

// exprIntArg renders a TCL integer argument as a Go integer expression: a
// literal when numeric, otherwise the corresponding variable reference.
func (tp *transpiler) exprIntArg(text string) string {
	t := strings.TrimSpace(text)
	if _, err := strconv.Atoi(t); err == nil {
		return t
	}
	if strings.HasPrefix(t, "$") {
		gv := tclVarToGo(strings.TrimPrefix(t, "$"))
		if isValidGoIdent(gv) {
			return gv
		}
	}
	return "0"
}
