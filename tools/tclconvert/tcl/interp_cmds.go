// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"fmt"
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
		"set":      commandSet,
		"unset":    commandUnset,
		"incr":     commandIncr,
		"expr":     commandExpr,
		"if":       commandIf,
		"for":      commandFor,
		"foreach":  commandForeach,
		"while":    commandWhile,
		"proc":     commandProc,
		"return":   commandReturn,
		"break":    commandBreak,
		"continue": commandContinue,

		"list":    commandList,
		"lappend": commandLappend,
		"llength": commandLLength,
		"lindex":  commandLIndex,
		"lrange":  commandLRange,
		"lsearch": commandLSearch,
		"lsort":   commandLSort,
		"concat":  commandConcat,
		"join":    commandJoin,

		"string": commandString,
		"regexp": commandRegexp,
		"regsub": commandRegsub,

		"catch":   commandCatch,
		"error":   commandError,
		"uplevel": commandUplevel,
		"upvar":   commandUpvar,
		"global":  noopCommand,
		"info":    commandInfo,

		"namespace":             noopCommand,
		"rename":                noopCommand,
		"array":                 noopCommand,
		"foreach_kv":            noopCommand,
		"foreach_u":             noopCommand,
		"execsql":               commandExecSQL,
		"catchsql":              commandCatchSQL,
		"db":                    commandDB,
		"do_execsql_test":       commandDoExecSQL,
		"do_catchsql_test":      commandDoCatchSQL,
		"do_test":               commandDoTest,
		"do_eqp_test":           commandDoEQP,
		"do_timed_execsql_test": commandDoExecSQL,
		"do_execsql2_test":      commandDoExecSQL,
		"reset_db":              commandResetDB,
		"sqlite3":               commandSQLite3,
		"ifcapable":             commandIfCapable,
		"drop_all_tables":       commandDropAllTables,
		"forcedelete":           commandForceDelete,
		"file":                  commandFile,

		// Test infrastructure stubs — all no-ops.
		"finish_test":               noopCommand,
		"test_finish":               noopCommand,
		"exit":                      noopCommand,
		"puts":                      noopCommand,
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

// commandSet implements `set`.
func commandSet(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdSet(args, localVars)
}

// commandUnset implements `unset var...`.
func commandUnset(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	for _, a := range args {
		delete(i.vars, a)
		delete(localVars, a)
	}
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
		idx, _ := strconv.Atoi(args[1])
		i.vars[""] = tclLIndex(args[0], idx)
	}
	return nil
}

// commandLRange implements `lrange`.
func commandLRange(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 3 {
		start, _ := strconv.Atoi(args[1])
		end, _ := strconv.Atoi(args[2])
		i.vars[""] = tclLRange(args[0], start, end)
	}
	return nil
}

// commandLSearch implements `lsearch` (simplified: always 0).
func commandLSearch(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.vars[""] = "0"
	return nil
}

// commandLSort implements `lsort` (simplified: identity).
func commandLSort(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.vars[""] = args[0]
	return nil
}

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
	return i.cmdRegsub(args)
}

// commandCatch implements `catch { body } ?var?`.
func commandCatch(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(rawWords) >= 2 {
		body := rawWords[1]
		if body.Braced {
			i.catchDepth++
			err := i.execScript(body.Text, localVars)
			i.catchDepth--
			if err != nil {
				i.vars[""] = "1"
				if len(args) >= 2 {
					i.setVar(args[1], err.Error(), localVars)
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

// commandInfo implements `info` (simplified: empty result).
func commandInfo(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.vars[""] = ""
	return nil
}

// commandExecSQL implements `execsql`.
func commandExecSQL(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdSQL(rawWords, args, "exec", localVars)
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
func commandDoEQP(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	return i.cmdDoEQP(rawWords, localVars)
}

// commandResetDB captures a reset_db call as a marker statement: the harness
// reopens a fresh database when it sees the __RESET_DB__ test case.
func commandResetDB(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "reset_db"})
	return nil
}

// commandSQLite3 handles `sqlite3 HANDLE FILENAME ?flags?`. Reopening an
// in-memory database discards all state, so it is captured as a reset marker
// exactly like reset_db. Reopening a file that was removed by a preceding
// forcedelete / `file delete` is equally fresh, so it also emits a marker.
// Reopening an existing file-backed database preserves data (the JSON
// harness models it with the current connection, i.e. a no-op).
func commandSQLite3(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 2 {
		if args[1] == ":memory:" {
			i.stmts = append(i.stmts, Stmt{Type: "reset_db"})
			return nil
		}
		if i.deletedFiles[args[1]] {
			delete(i.deletedFiles, args[1])
			i.stmts = append(i.stmts, Stmt{Type: "reset_db"})
		}
	}
	return nil
}

// commandForceDelete handles `forcedelete FILE...`: the named files are
// removed, so a subsequent `sqlite3 db FILE` opens a fresh database.
func commandForceDelete(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if i.deletedFiles == nil {
		i.deletedFiles = make(map[string]bool)
	}
	for _, a := range args {
		i.deletedFiles[a] = true
	}
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

// commandIfCapable handles `ifcapable EXPR ?EXPR...? BODY`. Each EXPR is a
// boolean combination of SQLITE_* capability names with !, && and ||. The
// converter targets a full-featured build: every capability is assumed
// present, so a plain name evaluates true and a !-negated name false. When
// the expression evaluates true the (braced) body is executed.
func commandIfCapable(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	expr, body := capableExprAndBody(rawWords, args)
	if body == nil || expr == "" {
		return nil
	}
	val, err := EvalExpr(expr, i, localVars)
	if err != nil {
		// Unparseable capability expression: skip the body rather than
		// aborting the whole file conversion.
		return nil
	}
	if isTrue(val) {
		return i.execScript(body.Text, localVars)
	}
	return nil
}

// capableExprAndBody extracts the capability expression and the braced body
// word from an ifcapable command. The body is the LAST braced word (the
// capability expression itself may also be braced, as in
// `ifcapable {update_delete_limit} {...}`). Capability words are mapped to
// "1" (present) or "0" (negated with !); operators and parentheses are kept.
func capableExprAndBody(rawWords []rawWord, args []string) (string, *rawWord) {
	if len(rawWords) < 2 {
		return "", nil
	}
	bodyIdx := -1
	for idx := len(rawWords) - 1; idx >= 1; idx-- {
		if rawWords[idx].Braced {
			bodyIdx = idx
			break
		}
	}
	if bodyIdx < 0 {
		return "", nil
	}
	terms := make([]string, 0, bodyIdx)
	for idx := 1; idx < bodyIdx; idx++ {
		if idx-1 < len(args) {
			terms = append(terms, args[idx-1])
		}
	}
	body := rawWords[bodyIdx]
	return rewriteCapExpr(strings.Join(terms, " && ")), &body
}

// rewriteCapExpr maps capability identifiers to boolean literals: a plain
// name becomes "1", a !-negated name becomes "0". Operators, whitespace and
// parentheses are copied verbatim.
func rewriteCapExpr(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		switch {
		case ch == '!' && (i+1 >= len(expr) || expr[i+1] != '='):
			b.WriteByte('0')
			i = skipCapName(expr, i)
		case isCapNameChar(ch) && (i == 0 || !isCapNameChar(expr[i-1])):
			b.WriteByte('1')
			i = skipCapName(expr, i)
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
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
