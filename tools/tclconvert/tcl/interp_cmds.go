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

		"namespace":  noopCommand,
		"rename":     noopCommand,
		"array":      noopCommand,
		"foreach_kv": noopCommand,
		"foreach_u":  noopCommand,

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
		"ifcapable":                 noopCommand,
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
			err := i.execScript(body.Text, localVars)
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
