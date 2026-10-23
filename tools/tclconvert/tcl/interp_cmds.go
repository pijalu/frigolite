// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// commandHandler executes a single TCL command. rawWords are the pre-substitution
// words; args are the already-substituted argument strings.
type commandHandler func(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error

// commandHandlers maps TCL command names to their implementations. Dispatch
// through this map keeps execCommand small and makes each command an
// independently testable unit. It is populated in init() to avoid a package
// initialization cycle (handlers call EvalExpr/substitute, which call back
// into dispatchCommand).
var commandHandlers map[string]commandHandler

func init() {
	commandHandlers = map[string]commandHandler{
		"subst":    commandSubst,
		"set":      commandSet,
		"unset":    commandUnset,
		"incr":     commandIncr,
		"append":   commandAppend,
		"expr":     commandExpr,
		"if":       commandIf,
		"switch":   commandSwitch,
		"for":      commandFor,
		"foreach":  commandForeach,
		"while":    commandWhile,
		"proc":     commandProc,
		"return":   commandReturn,
		"break":    commandBreak,
		"continue": commandContinue,

		"list":        commandList,
		"lappend":     commandLappend,
		"llength":     commandLLength,
		"lindex":      commandLIndex,
		"lrange":      commandLRange,
		"lreplace":    commandLReplace,
		"lsearch":     commandLSearch,
		"lsort":       commandLSort,
		"md5":         commandMD5,
		"random_uuid": commandRandomUUID,
		"concat":      commandConcat,
		"join":        commandJoin,
		"split":       commandSplit,

		"format":   commandFormat,
		"strftime": commandStrftime,
		"string":   commandString,
		"regexp":   commandRegexp,
		"regsub":   commandRegsub,

		"catch":   commandCatch,
		"error":   commandError,
		"uplevel": commandUplevel,
		"upvar":   commandUpvar,
		"global":  noopCommand,
		"eval":    commandEval,
		"info":    commandInfo,

		"namespace":                   noopCommand,
		"rename":                      noopCommand,
		"array":                       commandArray,
		"foreach_kv":                  noopCommand,
		"foreach_u":                   noopCommand,
		"execsql":                     commandExecSQL,
		"execsql_intout":              commandExecSQL,
		"stepsql":                     commandStepSQL,
		"execsql2":                    commandExecSQL2,
		"catchsql":                    commandCatchSQL,
		"db":                          commandDB,
		"do_execsql_test":             commandDoExecSQL,
		"do_test":                     commandDoTest,
		"do_catchsql_test":            commandDoCatchSQL,
		"do_select_tests":             commandDoSelectTests,
		"do_eqp_test":                 commandDoEQP,
		"do_timed_execsql_test":       commandDoExecSQL,
		"do_execsql2_test":            commandDoExecSQL,
		"reset_db":                    commandResetDB,
		"faultsim_delete_and_reopen":  commandResetDB,
		"db_delete_and_reopen":        commandResetDB,
		"sqlite3":                     commandSQLite3,
		"sqlite3_exec":                commandSQLite3Exec,
		"sqlite3_prepare_v2":          commandPrepareStmt,
		"sqlite3_prepare_v3":          commandPrepareStmt,
		"sqlite3_prepare":             commandPrepareStmt,
		"sqlite3_step":                commandStepStmt,
		"sqlite3_reset":               commandResetStmt,
		"sqlite3_finalize":            commandFinalizeStmt,
		"sqlite3_limit":               commandSQLite3Limit,
		"sqlite3_test_control":        commandSQLite3TestControl,
		"sqlite3_db_cacheflush":       commandCacheFlush,
		"sqlite3_create_collation_v2": commandCreateCollation,
		"sqlite3_create_function":     commandSQLiteCreateFunction,
		"sqlite_delete_collation":     commandDeleteCollation,
		"ifcapable":                   commandIfCapable,
		"drop_all_tables":             commandDropAllTables,
		"drop_all_tables_and_views":   commandDropAllMainTables,
		"register_echo_module":        commandRegisterEchoModule,
		"register_tcl_module":         commandRegisterTCLModule,
		"sqlite3_db_config":           commandDBConfig,
		"forcedelete":                 commandForceDelete,
		"forcecopy":                   commandForceCopy,
		"copy_file":                   commandForceCopy,
		"hexio_write":                 commandHexioWrite,
		"file":                        commandFile,
		"open":                        commandOpen,
		"puts":                        commandPuts,
		"close":                       commandClose,

		// Test infrastructure stubs — all no-ops.
		"finish_test":               noopCommand,
		"test_finish":               noopCommand,
		"exit":                      noopCommand,
		"output1":                   noopCommand,
		"output2":                   noopCommand,
		"output2_if_no_verbose":     noopCommand,
		"fix_testname":              noopCommand,
		"incr_ntest":                noopCommand,
		"sqlite3_memdebug_settitle": noopCommand,
		"flush":                     noopCommand,
		"source":                    noopCommand,
		"ifnotcapable":              noopCommand,
	}
}

// dispatchCommand runs cmd via the command-handler map, falling back to
// user-defined procs and finally silently ignoring unknown commands.
func (i *Interp) dispatchCommand(cmd string, rawWords []rawWord, args []string, localVars map[string]string) error {
	if fn, ok := commandHandlers[cmd]; ok {
		return fn(i, rawWords, args, localVars)
	}
	if len(args) > 0 && (args[0] == "func" || args[0] == "function") {
		start := len(i.stmts)
		if err := i.dbFunction(args); err != nil {
			return err
		}
		for idx := start; idx < len(i.stmts); idx++ {
			i.stmts[idx].Database = cmd
		}
		return nil
	}
	if len(args) > 0 && (args[0] == "eval" || args[0] == "close") {
		if args[0] == "close" {
			i.stmts = append(i.stmts, Stmt{Type: "close_named_db", Database: cmd, TestName: i.curTest})
			return nil
		}
		if len(rawWords) > 2 {
			if err := i.dbEval(rawWords, args, localVars); err != nil {
				return err
			}
			if len(i.stmts) > 0 {
				i.stmts[len(i.stmts)-1].Database = cmd
			}
			return nil
		}
	}
	if fn, ok := i.callbackCommands[cmd]; ok {
		result, err := fn(args)
		i.vars[""] = result
		return err
	}
	if proc, ok := i.procs[cmd]; ok {
		return i.callProc(proc, args, localVars)
	}
	// Unknown command — silently ignore (many TCL commands are not needed)
	return nil
}

// noopCommand is the handler for commands whose body is intentionally skipped.
func noopCommand(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return nil
}

// commandEval implements Tcl's `eval arg1 arg2 ...`: the arguments are
// concatenated (separated by a space) and the result is evaluated as a
// script. The test suite uses it to run a variable-held list of statements
// (trigger2.test's `eval $test_varset`), so the statements must be captured
// in order — without it the variables the list sets stay empty and the
// generated SQL is wrong.
func commandEval(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	return i.execScript(strings.Join(args, " "), localVars)
}

// commandSubst implements Tcl's subst command for captured test scripts.
// Command substitution is enabled by default; variable and backslash
// substitution can be disabled with the corresponding Tcl options.
func commandSubst(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	commands, variables, backslashes := true, true, true
	var text string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") && text == "" {
			option, ok := matchSubstOption(arg)
			if !ok {
				return fmt.Errorf("bad option %q: must be -nobackslashes, -nocommands, or -novariables", arg)
			}
			switch option {
			case "-nocommands":
				commands = false
			case "-novariables":
				variables = false
			case "-nobackslashes":
				backslashes = false
			}
			continue
		}
		text = arg
	}
	if text == "" && len(args) == 0 {
		return fmt.Errorf("wrong # args: should be \"subst ?-nobackslashes? ?-nocommands? ?-novariables? string\"")
	}
	result := i.substituteOptions(text, localVars, variables, commands, backslashes)
	i.vars[""] = result
	return nil
}

// matchSubstOption resolves a subst option word, honoring Tcl's unambiguous
// abbreviations: `subst -novar` means -novariables (rowvalue2.test uses the
// short form), while an ambiguous prefix such as `-no` is rejected.
func matchSubstOption(arg string) (string, bool) {
	names := []string{"-nobackslashes", "-nocommands", "-novariables"}
	match := ""
	for _, n := range names {
		if strings.HasPrefix(n, arg) {
			if match != "" {
				return "", false // ambiguous abbreviation
			}
			match = n
		}
	}
	return match, match != ""
}

// runtimeVarRef reports whether a word is a bare "$name" reference and
// returns the name.
func runtimeVarRef(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "$") || strings.HasPrefix(text, "$$") {
		return "", false
	}
	name := text[1:]
	if strings.HasPrefix(name, "{") {
		end := strings.IndexByte(name, '}')
		if end < 0 {
			return "", false
		}
		name = name[1:end]
	}
	if name == "" {
		return "", false
	}
	for _, r := range name {
		if !(r == '_' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "", false
		}
	}
	return name, true
}

// commandSet implements `set`.
func commandSet(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	queryVar := ""
	if len(args) >= 2 && len(rawWords) >= 3 && strings.HasPrefix(rawWords[2].Text, "[execsql") && len(i.stmts) > 0 {
		last := &i.stmts[len(i.stmts)-1]
		if (last.Type == "exec" || last.Type == "query") && last.TestName == i.curTest && isQueryStmt(firstStatement(last.SQL)) {
			last.Type = "query_assign"
			last.Variable = strings.TrimPrefix(args[0], "::")
			// Tcl's `execsql` returns the whole first row as a list of
			// columns, unlike `db one` which returns a single value.
			last.ResultAll = true
			i.runtimeVars[last.Variable] = last.runtimeID
			i.dynamicVars[last.Variable] = true
			queryVar = last.Variable
		}
	}
	// `set X [db eval { SELECT ... }]` / `[db one {...}]` assigns the runtime
	// query result, exactly like the execsql form above (date.test:402 drives
	// the clock override this way; autoinc.test:775 reads rootpages).
	assignedFromQuery := false
	if len(args) >= 2 && len(rawWords) >= 3 && len(i.stmts) > 0 {
		text := strings.TrimSpace(rawWords[2].Text)
		if strings.HasPrefix(text, "[") {
			inner := strings.TrimSpace(text[1:])
			if strings.HasPrefix(inner, "db eval") || strings.HasPrefix(inner, "db one") || strings.HasPrefix(inner, "db onecolumn") {
				last := &i.stmts[len(i.stmts)-1]
				if last.TestName == i.curTest && (last.Type == "exec" || last.Type == "query") && isQueryStmt(firstStatement(last.SQL)) {
					last.Type = "query_assign"
					last.Variable = strings.TrimPrefix(args[0], "::")
					// `db eval` yields the full result set (a list of rows);
					// `db one`/`db onecolumn` yield a single value.
					last.ResultAll = strings.HasPrefix(inner, "db eval")
					i.dynamicVars[last.Variable] = true
					i.runtimeVars[last.Variable] = last.runtimeID
					assignedFromQuery = true
					queryVar = last.Variable
				}
			}
		}
	}
	// `set X [expr {...}]` where the expression reads runtime-captured values
	// (rtree2: `set rc [expr {$t1 eq $t2}]`) cannot be evaluated at
	// conversion time: capture it as a runtime expression step that also
	// stores its result in X, so the do_test expectation can be checked
	// against the value the harness computes.
	if len(args) >= 2 && len(rawWords) >= 3 {
		text := strings.TrimSpace(rawWords[2].Text)
		if strings.HasPrefix(text, "[expr") {
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[len("[expr"):]), "]"))
			// The expression is usually a braced word: `expr {$a eq $b}` —
			// the braces delimit the expression, they are not part of it.
			if len(inner) >= 2 && strings.HasPrefix(inner, "{") && strings.HasSuffix(inner, "}") {
				inner = strings.TrimSpace(inner[1 : len(inner)-1])
			}
			if inner != "" && i.exprReadsRuntimeVar(inner) {
				name := strings.TrimPrefix(args[0], "::")
				i.stmts = append(i.stmts, Stmt{
					Type:     "runtime_expr",
					SQL:      i.bakeKnownExprVars(inner, localVars),
					Variable: name,
					TestName: i.curTest,
				})
				i.dynamicVars[name] = true
				i.setVar(args[0], "$"+name, localVars)
				i.vars[""] = "$" + name
				return nil
			}
		}
	}
	// An assignment to a variable that already holds a runtime value must be
	// a runtime assignment too: the harness has to see the new value, or a
	// later condition reads a stale one (pager2: `set lowpoint $now` makes
	// lowpoint runtime, and a later `set lowpoint $x` has to update it).
	//
	// A query capture (`set X [db eval {...}]`) is handled above and keeps
	// priority: the query result is the value.
	// `set X [some_proc]` where the proc returns a query's rows: the value
	// is only known at run time. The query becomes the step that fills the
	// variable, so the harness holds the value the Tcl variable would hold.
	if len(args) >= 2 && len(rawWords) >= 3 && !assignedFromQuery {
		if stmt := i.runtimeValueStatement(args[1]); stmt != nil && stmt.Type == "query" {
			name := strings.TrimPrefix(args[0], "::")
			stmt.Type = "query_assign"
			stmt.Variable = name
			stmt.ResultAll = true
			i.dynamicVars[name] = true
			i.runtimeVars[name] = stmt.runtimeID
			queryVar = name
		}
	}
	if len(args) >= 2 && len(rawWords) >= 3 && !assignedFromQuery {
		name := strings.TrimPrefix(args[0], "::")
		src, isVarRef := runtimeVarRef(rawWords[2].Text)
		switch {
		case isVarRef && i.dynamicVars[src]:
			i.stmts = append(i.stmts, Stmt{
				Type:     "runtime_expr",
				SQL:      "$" + src,
				Variable: name,
				TestName: i.curTest,
			})
			i.dynamicVars[name] = true
			i.setVar(args[0], "$"+name, localVars)
			i.vars[""] = "$" + name
			return nil
		case i.dynamicVars[name] && !strings.HasPrefix(strings.TrimSpace(rawWords[2].Text), "["):
			// The new value is known at conversion time (a literal or a
			// compile-time variable), but the harness must store it so the
			// runtime variable holds the current value. A command
			// substitution ("[...]") is NOT such a value: it is a query
			// capture, handled above.
			i.stmts = append(i.stmts, Stmt{
				Type:     "runtime_expr",
				SQL:      i.bakeKnownExprVars(rawWords[2].Text, localVars),
				Variable: name,
				TestName: i.curTest,
			})
			i.setVar(args[0], "$"+name, localVars)
			i.vars[""] = "$" + name
			return nil
		}
	}
	// `set rc [catch { <sql> }]`: the result code is only known when the
	// harness runs the statement, so the variable holds a runtime value and
	// the captured statement stores the code in it.
	if len(args) >= 2 && args[1] == catchResultPlaceholder {
		name := strings.TrimPrefix(args[0], "::")
		i.dynamicVars[name] = true
		if len(i.stmts) > 0 {
			last := &i.stmts[len(i.stmts)-1]
			last.Variable = name
			last.CatchResult = true
		}
		i.setVar(args[0], "$"+name, localVars)
		i.vars[""] = "$" + name
		return nil
	}
	if len(args) >= 2 && strings.TrimPrefix(args[0], "::") == "sqlite_fts3_enable_parentheses" {
		// The FTS3 query syntax has two variants: the release build uses
		// the legacy syntax (no parentheses, OR binds tighter than AND)
		// and the test build can switch to the parenthesis syntax. The
		// FTS3 expression tests set the variable, so the harness must
		// tell the engine which syntax is in force.
		i.stmts = append(i.stmts, Stmt{Type: "set_fts3_parens", SQL: args[1], TestName: i.curTest})
		i.setVar(args[0], args[1], localVars)
		return nil
	}
	if len(args) >= 2 && strings.TrimPrefix(args[0], "::") == "sqlite_current_time" && !assignedFromQuery {
		i.stmts = append(i.stmts, Stmt{Type: "set_current_time", SQL: args[1], TestName: i.curTest})
	}
	if len(args) >= 2 && strings.TrimPrefix(args[0], "::") == "echo_module_ignore_usable" {
		i.stmts = append(i.stmts, Stmt{Type: "echo_ignore_usable", SQL: args[1], TestName: i.curTest})
	}
	if err := i.cmdSet(args, localVars); err != nil {
		return err
	}
	if queryVar != "" {
		// The value exists only at run time (query_assign). Store a $name
		// reference so reads of the variable and expectations built from it
		// (e_expr-1: `[list [if ... {set a3} {set a2}]]`) defer to the
		// harness's captured runtime value instead of the empty
		// capture-time placeholder.
		placeholder := "$" + queryVar
		i.setVar(args[0], placeholder, localVars)
		i.vars[""] = placeholder
	} else if len(args) >= 2 {
		// A plain assignment replaces an earlier runtime-assigned value:
		// the variable holds a known value again, so later reads must use
		// it instead of the runtime placeholder (e_expr-8.2: the same
		// variable is assigned from a query in one branch and from a
		// computed list in the other).
		delete(i.dynamicVars, strings.TrimPrefix(args[0], "::"))
	}
	return nil
}

// commandUnset implements `unset var...`.
func commandUnset(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	for _, a := range args {
		if strings.TrimPrefix(a, "::") == "echo_module_ignore_usable" {
			i.stmts = append(i.stmts, Stmt{Type: "echo_ignore_usable", SQL: "0", TestName: i.curTest})
		}
		delete(i.vars, a)
		delete(localVars, a)
	}
	return nil
}

// commandAppend implements Tcl append, updating a variable and returning its new value.
func commandAppend(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) == 0 {
		return fmt.Errorf("wrong # args: should be \"append varName ?value ...?\"")
	}
	name := args[0]
	current, _ := i.getVar(name, localVars)
	current += strings.Join(args[1:], "")
	i.setVar(name, current, localVars)
	i.vars[""] = current
	return nil
}

// commandIncr implements `incr`.
func commandIncr(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdIncr(args, localVars)
}

// commandExpr implements `expr`, evaluating a TCL expression.
func commandExpr(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	exprStr := strings.Join(args, " ")
	result, err := EvalExpr(exprStr, i, localVars)
	if err != nil {
		return err
	}
	i.vars[""] = result
	return nil
}

// commandIf implements `if`.
func commandIf(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdIf(rawWords, localVars)
}

func commandSwitch(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	mode := "glob"
	index := 0
	for index < len(args) && strings.HasPrefix(args[index], "-") {
		switch args[index] {
		case "-exact", "-glob", "-regexp":
			mode = strings.TrimPrefix(args[index], "-")
		case "--":
			index++
			goto optionsDone
		default:
			return fmt.Errorf("bad switch option %q", args[index])
		}
		index++
	}
optionsDone:
	if len(args)-index < 2 {
		return fmt.Errorf("wrong # args: should be \"switch ?options? string {pattern body ...}\"")
	}
	value := args[index]
	pairs := splitList(strings.Join(args[index+1:], " "))
	if len(pairs)%2 != 0 {
		return fmt.Errorf("switch pattern list must contain pattern/body pairs")
	}
	defaultBody := ""
	for n := 0; n < len(pairs); n += 2 {
		pattern, body := pairs[n], pairs[n+1]
		if pattern == "default" {
			defaultBody = body
			continue
		}
		matched := false
		switch mode {
		case "exact":
			matched = value == pattern
		case "glob":
			matched, _ = filepath.Match(pattern, value)
		case "regexp":
			matched, _ = regexp.MatchString(pattern, value)
		}
		if matched {
			// TCL switch fallthrough: a body of "-" defers to the next
			// pattern's body (make_expr2's `== - IS {...}`).
			for body == "-" && n+2 < len(pairs) {
				n += 2
				body = pairs[n+1]
			}
			return i.execScript(body, localVars)
		}
	}
	if defaultBody != "" {
		return i.execScript(defaultBody, localVars)
	}
	return nil
}

// commandFor implements `for`.
func commandFor(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdFor(rawWords, localVars)
}

// commandForeach implements `foreach`.
func commandForeach(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdForeach(rawWords, localVars)
}

// commandWhile implements `while`.
func commandWhile(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdWhile(rawWords, localVars)
}

// commandProc implements `proc`.
func commandProc(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdProc(rawWords)
}

// commandReturn implements `return`.
func commandReturn(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	val := ""
	if len(args) > 0 {
		val = args[0]
	}
	return &ControlFlow{Kind: "return", Result: val}
}

// commandBreak implements `break`.
func commandBreak(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return &ControlFlow{Kind: "break"}
}

// commandContinue implements `continue`.
func commandContinue(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return &ControlFlow{Kind: "continue"}
}

// commandList implements `list`.
func commandList(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.vars[""] = tclList(args)
	return nil
}

// commandLappend implements `lappend`.
func commandLappend(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdLappend(args, localVars)
}

// commandLLength implements `llength`.
func commandLLength(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) > 0 {
		i.vars[""] = strconv.Itoa(tclLLength(args[0]))
	}
	return nil
}

// commandLIndex implements `lindex`.
func commandLIndex(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 2 {
		items := splitList(args[0])
		idx, ok := parseTclIndex(args[1], len(items))
		if !ok {
			i.vars[""] = ""
			return nil
		}
		i.vars[""] = tclLIndex(args[0], idx)
	}
	return nil
}

// commandLRange implements `lrange`.
func commandLRange(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 3 {
		items := splitList(args[0])
		start, okStart := parseTclIndex(args[1], len(items))
		end, okEnd := parseTclIndex(args[2], len(items))
		if !okStart || !okEnd {
			i.vars[""] = ""
			return nil
		}
		i.vars[""] = tclLRange(args[0], start, end)
	}
	return nil
}

// commandLReplace implements lreplace and records post-query list edits for the runtime harness.
func commandLReplace(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 3 {
		return fmt.Errorf("wrong # args: should be \"lreplace list first last ?element ...?\"")
	}
	items := splitList(args[0])
	start, okStart := parseTclIndex(args[1], len(items))
	end, okEnd := parseTclIndex(args[2], len(items))
	if !okStart || !okEnd {
		return fmt.Errorf("bad index in lreplace")
	}
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	if end < start-1 {
		end = start - 1
	}
	if end >= len(items) {
		end = len(items) - 1
	}
	prefix, suffix := append([]string(nil), items[:start]...), []string(nil)
	if end+1 < len(items) {
		suffix = items[end+1:]
	}
	items = append(prefix, args[3:]...)
	items = append(items, suffix...)
	i.vars[""] = tclList(items)
	return nil
}

// commandLSearch implements Tcl's default glob-match list search used by SQLite test helpers.
func commandLSearch(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 2 {
		i.vars[""] = "-1"
		return nil
	}
	list, pattern := splitList(args[0]), args[1]
	for index, value := range list {
		matched, err := filepath.Match(pattern, value)
		if err == nil && matched {
			i.vars[""] = strconv.Itoa(index)
			return nil
		}
	}
	i.vars[""] = "-1"
	return nil
}

// commandLSort implements `lsort` (simplified: identity).
// commandConcat implements `concat`.
func commandConcat(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.vars[""] = strings.Join(args, " ")
	return nil
}

// commandJoin implements `join LIST ?SEP?`.
func commandJoin(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 1 {
		sep := " "
		if len(args) >= 2 {
			sep = args[1]
		}
		items := splitList(args[0])
		i.vars[""] = strings.Join(items, sep)
	}
	return nil
}

// commandSplit implements `split string ?splitChars?` with Tcl semantics: the
// separator is any single character from splitChars (default whitespace),
// adjacent separators produce empty elements, and an empty splitChars splits
// into individual characters (pragma4.test: `lindex [split $sql =] 0`).
func commandSplit(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	s := args[0]
	sep := " \t\n\r"
	if len(args) >= 2 {
		sep = args[1]
	}
	var parts []string
	if sep == "" {
		for _, r := range s {
			parts = append(parts, string(r))
		}
	} else {
		var cur strings.Builder
		for _, r := range s {
			if strings.ContainsRune(sep, r) {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
			cur.WriteRune(r)
		}
		parts = append(parts, cur.String())
	}
	i.vars[""] = tclList(parts)
	return nil
}

// commandFormat implements Tcl format for numeric formats used by test scripts.
func commandFormat(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) == 0 {
		return fmt.Errorf("wrong # args: should be \"format formatString ?arg ...?\"")
	}
	i.vars[""] = tclFormat(args[0], args[1:])
	return nil
}

// tclFormat implements Tcl's `format` semantics for the conversions used by
// the SQLite test scripts. C/Tcl parse the argument according to the
// conversion: integers for d/i/o/u/x/X/c, floating point for a/A/e/E/f/F/g/G
// (g/G default to 6 significant digits and strip trailing zeros). Go's fmt
// would otherwise render a verb applied to a string argument as
// "%!e(string=3.5)" (atof1.test's `format %.32e $x`).
func tclFormat(format string, args []string) string {
	var b strings.Builder
	argIndex := 0
	nextArg := func() string {
		if argIndex < len(args) {
			argIndex++
			return args[argIndex-1]
		}
		return ""
	}
	for pos := 0; pos < len(format); {
		if format[pos] != '%' {
			b.WriteByte(format[pos])
			pos++
			continue
		}
		if pos+1 < len(format) && format[pos+1] == '%' {
			b.WriteByte('%')
			pos += 2
			continue
		}
		spec, verb, next, ok := parseTclFormatSpec(format, pos)
		if !ok {
			b.WriteString(format[pos:])
			break
		}
		writeTclFormat(&b, spec, verb, nextArg())
		pos = next
	}
	return b.String()
}

// parseTclFormatSpec parses one conversion at format[pos] (which is '%').
// It returns the spec with length modifiers stripped, the conversion verb,
// the index past the conversion, and ok=false for a truncated spec. g/G
// conversions without an explicit precision get C's default of 6 digits.
func parseTclFormatSpec(format string, pos int) (string, byte, int, bool) {
	var spec strings.Builder
	spec.WriteByte('%')
	pos++
	for pos < len(format) && strings.ContainsRune("-+ #0", rune(format[pos])) {
		spec.WriteByte(format[pos])
		pos++
	}
	for pos < len(format) && (isASCIIDigit(format[pos]) || format[pos] == '*') {
		spec.WriteByte(format[pos])
		pos++
	}
	precision := false
	if pos < len(format) && format[pos] == '.' {
		precision = true
		spec.WriteByte('.')
		pos++
		for pos < len(format) && (isASCIIDigit(format[pos]) || format[pos] == '*') {
			spec.WriteByte(format[pos])
			pos++
		}
	}
	for pos < len(format) && strings.ContainsRune("lhLqjzt", rune(format[pos])) {
		pos++
	}
	if pos >= len(format) {
		return spec.String(), 0, len(format), false
	}
	verb := format[pos]
	if (verb == 'g' || verb == 'G') && !precision {
		spec.WriteString(".6")
	}
	return spec.String(), verb, pos + 1, true
}

// isASCIIDigit reports whether c is an ASCII decimal digit.
func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// writeTclFormat renders one parsed conversion, parsing the argument per the
// verb's C/Tcl type rules. A non-numeric argument for a numeric verb keeps
// Go's fmt artifact (e.g. "%!e(string=x)") so the mismatch stays visible.
func writeTclFormat(b *strings.Builder, spec string, verb byte, arg string) {
	trimmed := strings.TrimSpace(arg)
	switch verb {
	case 'd', 'i', 'o', 'x', 'X':
		if v, err := strconv.ParseInt(trimmed, 0, 64); err == nil {
			outVerb := string(verb)
			if verb == 'i' {
				outVerb = "d"
			}
			fmt.Fprintf(b, spec+outVerb, v)
			return
		}
		fmt.Fprintf(b, spec+string(verb), arg)
	case 'u':
		if u, err := strconv.ParseUint(trimmed, 0, 64); err == nil {
			fmt.Fprintf(b, spec+"d", u)
			return
		}
		if v, err := strconv.ParseInt(trimmed, 0, 64); err == nil {
			// Tcl's %u takes the unsigned 32-bit interpretation.
			fmt.Fprintf(b, spec+"d", uint32(v))
			return
		}
		fmt.Fprintf(b, spec+string(verb), arg)
	case 'c':
		if v, err := strconv.ParseInt(trimmed, 0, 64); err == nil {
			fmt.Fprintf(b, spec+"c", rune(v))
			return
		}
		fmt.Fprintf(b, spec+string(verb), arg)
	case 'e', 'E', 'f', 'F', 'g', 'G', 'a', 'A':
		if v, err := strconv.ParseFloat(trimmed, 64); err == nil {
			fmt.Fprintf(b, spec+string(verb), v)
			return
		}
		fmt.Fprintf(b, spec+string(verb), arg)
	case 's':
		fmt.Fprintf(b, spec+"s", arg)
	default:
		fmt.Fprintf(b, spec+string(verb), arg)
	}
}

// commandString implements `string`.
func commandString(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdString(args)
}

// commandRegexp implements `regexp`.
func commandRegexp(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdRegexp(args)
}

// commandRegsub implements `regsub`.
func commandRegsub(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdRegsub(args, localVars)
}

// catchResultPlaceholder marks a `catch` whose body captured a SQL
// statement: the statement's success is only known when the harness runs it,
// so the catch result is a RUNTIME value (the harness stores the result code
// in the catch's variable).
const catchResultPlaceholder = "$__catch_result"

// commandCatch implements `catch { body } ?var?`.
func commandCatch(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(rawWords) >= 2 {
		body := rawWords[1]
		if body.Braced {
			statementsBefore := len(i.stmts)
			i.catchDepth++
			err := i.execScript(body.Text, localVars)
			i.catchDepth--
			if err != nil {
				i.vars[""] = "1"
				if len(args) >= 2 {
					i.setVar(args[1], err.Error(), localVars)
				}
			} else if len(i.stmts) > statementsBefore {
				// The body captured a statement: its outcome is a runtime
				// fact, so the catch result must be one too.
				//
				// `catch { ... } msg` also captures the error MESSAGE in
				// the second variable (tester.tcl's do_ioerr_test uses it:
				// `set r [catch $::ioerrorbody msg]`), so the message
				// variable is a runtime value as well.
				i.vars[""] = catchResultPlaceholder
				if len(args) >= 2 {
					msgVar := strings.TrimPrefix(args[1], "::")
					i.setVar(args[1], "$"+msgVar, localVars)
					// The message is a runtime value: mark it dynamic so a
					// later expression keeps the $name reference instead of
					// baking the placeholder as a literal.
					i.dynamicVars[msgVar] = true
					i.dynamicVars[args[1]] = true
					if len(i.stmts) > 0 {
						i.stmts[len(i.stmts)-1].CatchMessage = msgVar
					}
				}
			} else {
				i.vars[""] = "0"
			}
		}
	}
	return nil
}

// commandError implements `error`.
func commandError(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return fmt.Errorf("%s", strings.Join(args, " "))
}

// commandUplevel implements `uplevel N { script }`.
func commandUplevel(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(rawWords) >= 2 {
		bodyIdx := 1
		if len(rawWords) >= 3 {
			bodyIdx = 2 // skip level arg
		}
		if rawWords[bodyIdx].Braced {
			return i.execScript(rawWords[bodyIdx].Text, localVars)
		}
	}
	if len(args) > 0 {
		return i.execScript(strings.Join(args, " "), localVars)
	}
	return nil
}

// commandUpvar implements `upvar` (simplified: copy).
func commandUpvar(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 2 {
		orig := args[len(args)-2]
		alias := args[len(args)-1]
		if v, ok := i.getVar(orig, localVars); ok {
			i.setVar(alias, v, localVars)
		}
	}
	return nil
}

func commandArray(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 3 && args[0] == "set" {
		values := splitList(args[2])
		if len(values)%2 != 0 {
			return fmt.Errorf("list must have an even number of elements")
		}
		for j := 0; j < len(values); j += 2 {
			i.setVar(args[1]+"("+values[j]+")", values[j+1], localVars)
		}
	}
	return nil
}

// commandInfo implements `info` queries needed by Tcl test procedures.
func commandInfo(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 2 && args[0] == "exists" {
		_, ok := i.getVar(args[1], localVars)
		if ok {
			i.vars[""] = "1"
		} else {
			i.vars[""] = "0"
		}
		return nil
	}
	i.vars[""] = ""
	return nil
}

func commandSQLite3Limit(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 3 {
		i.stmts = append(i.stmts, Stmt{Type: "set_limit", SQL: args[1] + ":" + args[2], TestName: i.curTest})
	}
	return nil
}

// commandStepSQL captures SQL passed to tester.tcl's non-callback API.
// Database handle argument is resolved by the Tcl script; `stepsql` in
// SQLite's tester is invoked with the main connection's opaque pointer.
func commandStepSQL(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	for _, word := range rawWords[1:] {
		if !word.Braced || strings.TrimSpace(word.Text) == "" {
			continue
		}
		sql := i.bindSQLParams(word.Text, localVars)
		i.stmts = append(i.stmts, Stmt{Type: i.stmtType("exec"), SQL: sql, TestName: i.curTest})
		return nil
	}
	return nil
}

// commandExecSQL implements `execsql`.
func commandExecSQL(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdSQL(rawWords, args, "exec", localVars)
}

// commandExecSQL2 implements `execsql2` result formatting.
func commandExecSQL2(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdSQL(rawWords, args, "query2", localVars)
}

// commandCatchSQL implements `catchsql`.
func commandCatchSQL(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdSQL(rawWords, args, "catch", localVars)
}

// commandDB implements `db ...`.
func commandDB(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdDB(rawWords, args, localVars)
}

// commandDoExecSQL implements `do_execsql_test` and friends.
func commandDoExecSQL(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	defer func() { i.curTest = "" }()
	return i.cmdDoExecSQL(rawWords, localVars)
}

// commandDoCatchSQL implements `do_catchsql_test`.
func commandDoCatchSQL(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdDoCatchSQL(rawWords, localVars)
}

// commandDoTest implements `do_test`.
func commandDoTest(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdDoTest(rawWords, localVars)
}

// commandDoEQP implements `do_eqp_test`.
func commandDoSelectTests(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	prefix := args[0]
	i.selectGroup++
	groupName := fmt.Sprintf("select_tests_%d", i.selectGroup)
	var query, queryMode, repair, errorFormat string
	errorMode := false
	for pos := 1; pos+1 < len(args)-1; pos++ {
		switch args[pos] {
		case "-query":
			query = args[pos+1]
			queryMode = "query"
		case "-tclquery":
			query = args[pos+1]
			queryMode = "tclquery"
		case "-error":
			errorMode = true
			errorFormat = args[pos+1]
		case "-repair":
			repair = args[pos+1]
		}
		if args[pos] == "-query" || args[pos] == "-tclquery" || args[pos] == "-repair" || args[pos] == "-error" {
			pos++
		}
	}
	triples := splitList(args[len(args)-1])
	if len(triples) == 0 || len(triples)%3 != 0 {
		return fmt.Errorf("SELECT test list must contain test-number, SQL, expected-result triples")
	}
	for pos := 0; pos < len(triples); pos += 3 {
		name := prefix + "." + triples[pos]
		if repair != "" {
			if err := i.captureSelectTestRepair(repair, name, localVars); err != nil {
				return err
			}
		}
		stmtType := "exec"
		if errorMode {
			stmtType = "catch"
		}
		i.stmts = append(i.stmts, Stmt{Type: stmtType, SQL: triples[pos+1], TestName: name, GroupName: groupName})
		if query != "" {
			// Canonical TCL list serialization keeps empty elements as {}
			// (a plain space-join would lose them: {{} {} 1 i} -> "  1 i").
			expected := tclList(splitList(triples[pos+2]))
			i.stmts = append(i.stmts, Stmt{Type: queryMode, SQL: query, Expected: expected, TestName: name, GroupName: groupName})
		} else {
			expected := tclList(splitList(triples[pos+2]))
			if errorMode {
				formattedError := strings.TrimSpace(errorFormat)
				if strings.Contains(errorFormat, "%") && expected != "" {
					formatValues := splitList(triples[pos+2])
					for index, value := range formatValues {
						formatValues[index] = strings.Trim(value, "{}")
					}
					formattedError = strings.TrimSpace(tclFormat(errorFormat, formatValues))
				}
				expected = "1"
				if formattedError != "" {
					expected += " {" + formattedError + "}"
				}
			}
			i.stmts[len(i.stmts)-1].Expected = expected
		}
		if repair != "" {
			if err := i.captureSelectTestRepair(repair, name, localVars); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *Interp) captureSelectTestRepair(script, testName string, localVars map[string]string) error {
	previous := i.curTest
	i.curTest = testName
	err := i.execScript(script, localVars)
	i.curTest = previous
	return err
}

func commandDoEQP(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdDoEQP(rawWords, localVars)
}

// commandResetDB captures a reset_db call as a marker statement: the harness
// reopens a fresh database when it sees the __RESET_DB__ test case.
func commandResetDB(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "reset_db"})
	return nil
}

// commandSQLite3 captures connection replacement in source order. Empty and
// :memory: opens replace main with a NEW EMPTY database (the on-disk fixture
// files are left intact); an existing file reopens without discarding
// contents.
func commandSQLite3(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) == 1 && args[0] == "-has-codec" {
		// testfixture's sqlite3 command reports the codec capability of the
		// build (0 = none). date.test:519 guards the raw-hexio tests with
		// `if {0==[sqlite3 -has-codec]}`.
		i.vars[""] = "0"
		return nil
	}
	if len(args) < 2 {
		return nil
	}
	name, filename := args[0], args[1]
	if filename == ":memory:" || filename == "" {
		if name == "db" {
			// `sqlite3 db ""` / `sqlite3 db :memory:` swaps the main handle
			// for a fresh empty database. It must NOT delete the fixture's
			// lifecycle file: triggerupfrom.test:85-92 does
			// `db close; sqlite3 db ""` and then ATTACHes the test.db built
			// by the preceding test (a deleting reset would lose it).
			i.stmts = append(i.stmts, Stmt{Type: "open_empty_db", TestName: i.curTest})
		} else if filename == ":memory:" {
			i.stmts = append(i.stmts, Stmt{Type: "open_named_db", SQL: filename, Database: name, TestName: i.curTest})
		} else {
			i.stmts = append(i.stmts, Stmt{Type: "close_named_db", Database: name})
		}
		return nil
	}
	path := filename
	if strings.HasPrefix(path, "file:") {
		path = strings.TrimPrefix(path, "file:")
		if n := strings.IndexByte(path, '?'); n >= 0 {
			path = path[:n]
		}
	}
	path = filepath.Clean(path)
	// The harness reopens the fixture's lifecycle file, so strip any
	// `file:` prefix and URI query (8_3_names / VFS options are
	// compile-time features, not part of the file path).
	localPath := filepath.Base(path)
	if name == "db" && i.deletedFiles[path] {
		delete(i.deletedFiles, path)
		i.stmts = append(i.stmts, Stmt{Type: "reopen_db", SQL: localPath, TestName: i.curTest})
		return nil
	}
	if name == "db" {
		i.stmts = append(i.stmts, Stmt{Type: "reopen_db", SQL: localPath, TestName: i.curTest})
	} else {
		i.stmts = append(i.stmts, Stmt{Type: "open_named_db", SQL: filename, Database: name, TestName: i.curTest})
	}
	return nil
}

func commandSQLite3TestControl(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) != 2 || args[0] != "SQLITE_TESTCTRL_INTERNAL_FUNCTIONS" || args[1] != "db" {
		return nil
	}
	i.internalFunctions = !i.internalFunctions
	state := "disable"
	if i.internalFunctions {
		state = "enable"
	}
	i.stmts = append(i.stmts, Stmt{Type: "internal_functions", SQL: state, TestName: i.curTest})
	return nil
}

// commandSQLite3Exec captures direct SQLite test-helper execution with its connection.
func commandSQLite3Exec(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	if len(rawWords) < 3 || len(args) < 2 {
		return nil
	}
	connection := args[0]
	database := ""
	if connection != "db" {
		database = connection
	}
	sql := rawWords[2].Text
	if rawWords[2].Braced {
		sql = i.bindSQLParams(sql, nil)
	}
	i.stmts = append(i.stmts, Stmt{Type: "sqlite3_exec", SQL: sql, Database: database, TestName: i.curTest})
	return nil
}

func decodeSQLiteExecHex(sql string) string {
	var out strings.Builder
	for pos := 0; pos < len(sql); {
		if sql[pos] == '%' && pos+2 < len(sql) {
			hi, e1 := strconv.ParseUint(sql[pos+1:pos+2], 16, 4)
			lo, e2 := strconv.ParseUint(sql[pos+2:pos+3], 16, 4)
			if e1 == nil && e2 == nil {
				out.WriteByte(byte(hi<<4 | lo))
				pos += 3
				continue
			}
		}
		out.WriteByte(sql[pos])
		pos++
	}
	return out.String()
}

// commandForceDelete handles `forcedelete FILE...`: the named files are
// removed, so a subsequent `sqlite3 db FILE` opens a fresh database.
func commandForceCopy(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) >= 2 {
		i.stmts = append(i.stmts, Stmt{Type: "copy_file", SQL: args[0], FileTarget: args[1], TestName: i.curTest})
	}
	return nil
}

func commandForceDelete(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if i.deletedFiles == nil {
		i.deletedFiles = make(map[string]bool)
	}
	for _, a := range args {
		i.deletedFiles[filepath.Clean(a)] = true
		i.stmts = append(i.stmts, Stmt{Type: "delete_file", SQL: a, TestName: i.curTest})
	}
	return nil
}

// commandHexioWrite handles `hexio_write FILENAME OFFSET HEXDATA` from
// src/test_hexio.c:147: DATA is hexadecimal and is written into FILENAME
// starting at OFFSET. Corruption fixtures patch raw database bytes between a
// `db close` and the following reopen (date.test:528, fordeleting etc.), so the
// marker is executed by the harness on the lifecycle file.
func commandHexioWrite(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 3 {
		return nil
	}
	offset, err := strconv.Atoi(strings.TrimSpace(args[1]))
	if err != nil {
		return nil
	}
	i.stmts = append(i.stmts, Stmt{
		Type:       "hexio_write",
		SQL:        args[0],
		FileTarget: args[0],
		FileOffset: offset,
		FileData:   args[2],
		TestName:   i.curTest,
	})
	return nil
}

func commandOpen(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 || args[1] != "w" {
		i.vars[""] = ""
		return nil
	}
	i.nextFileHandle++
	handle := fmt.Sprintf("tclfile%d", i.nextFileHandle)
	i.fileWrites[handle] = &tclFileWrite{path: args[0]}
	i.vars[""] = handle
	return nil
}

func commandPuts(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	write := i.fileWrites[args[0]]
	if write == nil {
		return nil
	}
	write.data.WriteString(args[1])
	write.data.WriteByte('\n')
	return nil
}

func commandClose(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := args[0]
	write := i.fileWrites[handle]
	if write == nil {
		return nil
	}
	i.stmts = append(i.stmts, Stmt{Type: "write_file", SQL: write.path, FileData: write.data.String(), TestName: i.curTest})
	delete(i.fileWrites, handle)
	return nil
}

// commandFile handles `file delete ...`: tracks removed files the same way
// as forcedelete (only the `delete` subcommand matters for state tracking).
func commandFile(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 1 && args[0] == "delete" {
		return commandForceDelete(i, rawWords, args[1:], localVars)
	}
	return nil
}

// commandDropAllTables mirrors tester.tcl's drop_all_tables: with foreign
// keys disabled, tables and views are dropped from every attached database.
// The harness executes the __DROP_ALL_TABLES__ marker with the same
// semantics (a plain reset would also detach aux databases, which
// tester.tcl does not do).
func commandDropAllTables(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "drop_all_tables"})
	return nil
}

func commandDropAllMainTables(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "drop_main_tables"})
	return nil
}

func commandRegisterEchoModule(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	database := ""
	for _, word := range rawWords {
		if strings.Contains(word.Text, "sqlite3_connection_pointer") {
			fields := strings.Fields(strings.Trim(word.Text, "[]"))
			if len(fields) > 1 {
				database = fields[len(fields)-1]
			}
		}
	}
	if database == "" && len(args) > 0 {
		database = args[len(args)-1]
	}
	if database == "db" {
		database = ""
	}
	i.stmts = append(i.stmts, Stmt{Type: "register_echo_module", Database: database, TestName: i.curTest})
	return nil
}

func commandRegisterTCLModule(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	moduleArgs := append([]string(nil), args...)
	if len(moduleArgs) == 0 && len(rawWords) > 1 {
		moduleArgs = make([]string, 0, len(rawWords)-1)
		for _, word := range rawWords[1:] {
			value, err := i.evalWord(word, localVars)
			if err == nil {
				moduleArgs = append(moduleArgs, value)
			}
		}
	}
	database := ""
	if len(moduleArgs) > 0 && moduleArgs[0] != "db" {
		database = moduleArgs[0]
	}
	i.stmts = append(i.stmts, Stmt{
		Type: "register_tcl_module", Database: database,
		ModuleArgs: moduleArgs, CallbackProcs: i.CallbackProcs(), CallbackVars: i.callbackVars(), TestName: i.curTest,
	})
	i.updateTclCallbackSnapshots()
	return nil
}

// commandSQLiteCreateFunction handles testfixture's `sqlite3_create_function DB`
// helper (test1.c test_create_function), which registers the built-in test SQL
// functions (pointer_change, x_coalesce, hex8, tkt2213func, counter1/2, ...).
// The harness installs its Go equivalents when it sees the emitted marker
// (ptrchng.test, bind.test).
func commandSQLiteCreateFunction(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	database := ""
	if len(args) > 0 && args[0] != "" && args[0] != "db" {
		database = args[0]
	}
	i.stmts = append(i.stmts, Stmt{Type: "register_test_funcs", Database: database, TestName: i.curTest})
	return nil
}

func commandDBConfig(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 3 && args[1] == "DEFENSIVE" {
		i.stmts = append(i.stmts, Stmt{Type: "defensive", SQL: args[2], TestName: i.curTest})
	}
	return nil
}

// commandIfCapable handles `ifcapable EXPR BODY ?else ELSEBODY?`. Each EXPR is
// a boolean combination of SQLITE_* capability names with !, && and ||. The
// converter targets a full-featured build except for UTF-16 database encoding,
// which this engine does not support. When the expression evaluates true the
// (braced) body is executed, otherwise the optional else body is.
func commandIfCapable(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	expr, body, elseBody := capableExprAndBodies(rawWords, args)
	if body == nil || expr == "" {
		return nil
	}
	// Capability names are evaluated by the engine-specific rewrite below.
	val, err := EvalExpr(rewriteCapExpr(expr), i, localVars)
	if err != nil {
		// Unparseable capability expression: skip the body rather than
		// aborting the whole file conversion.
		return nil
	}
	if isTrue(val) {
		return i.execScript(body.Text, localVars)
	}
	if elseBody != nil {
		return i.execScript(elseBody.Text, localVars)
	}
	return nil
}

// capableExprAndBodies extracts the capability expression, the true-branch
// body and the optional else-branch body from an ifcapable command. TCL's
// ifcapable accepts `ifcapable EXPR BODY ?else ELSEBODY?`; the elaborated
// body is the last braced word before the `else` keyword word (or the last
// braced word overall when no else branch is present). Capability words are
// joined with && and left for rewriteCapExpr to evaluate.
func capableExprAndBodies(rawWords []rawWord, args []string) (string, *rawWord, *rawWord) {
	if len(rawWords) < 2 {
		return "", nil, nil
	}
	elseIdx := -1
	for idx := len(rawWords) - 1; idx >= 1; idx-- {
		if !rawWords[idx].Braced && rawWords[idx].Text == "else" {
			elseIdx = idx
			break
		}
	}
	searchEnd := len(rawWords) - 1
	if elseIdx >= 0 {
		searchEnd = elseIdx - 1
	}
	bodyIdx := -1
	for idx := searchEnd; idx >= 1; idx-- {
		if rawWords[idx].Braced {
			bodyIdx = idx
			break
		}
	}
	if bodyIdx < 0 {
		return "", nil, nil
	}
	terms := make([]string, 0, bodyIdx)
	for idx := 1; idx < bodyIdx; idx++ {
		if idx-1 < len(args) {
			terms = append(terms, args[idx-1])
		}
	}
	var elseBody *rawWord
	if elseIdx >= 0 && elseIdx+1 < len(rawWords) {
		elseBody = &rawWords[elseIdx+1]
	}
	body := rawWords[bodyIdx]
	return strings.Join(terms, " && "), &body, elseBody
}

// rewriteCapExpr maps capability identifiers to boolean literals, treating
// capabilities the engine does not implement as absent while retaining the
// full-build defaults for others. Operators, whitespace and parentheses are
// copied verbatim.
func rewriteCapExpr(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		switch {
		case ch == '!' && (i+1 >= len(expr) || expr[i+1] != '='):
			end := skipCapName(expr, i)
			if !capabilityPresent(expr[i+1 : end+1]) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			i = end
		case isCapNameChar(ch) && (i == 0 || !isCapNameChar(expr[i-1])):
			end := skipCapName(expr, i)
			if capabilityPresent(expr[i : end+1]) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			i = end
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// capabilityPresent reports whether the engine implements the named SQLite
// compile-time capability. The converter targets a full-featured build except
// for UTF-16 database encoding and 8_3 filename translation, which frigolite
// does not implement. A standard SQLite testfixture lacks the same two
// options and skips their tests via `ifcapable !cap { finish_test; return }`.
func capabilityPresent(name string) bool {
	switch strings.ToLower(name) {
	case "utf16", "8_3_names":
		return false
	}
	return true
}

// skipCapName returns the index of the last character of the identifier that
// starts at or after i (the caller's loop continues past it).
func skipCapName(expr string, i int) int {
	for i+1 < len(expr) && isCapNameChar(expr[i+1]) {
		i++
	}
	return i
}

// isCapNameChar reports whether c can appear in a capability identifier.
func isCapNameChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
