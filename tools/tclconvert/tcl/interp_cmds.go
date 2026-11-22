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

		"format":                      commandFormat,
		"binary":                      commandBinary,
		"sqlite3_bind_text":           commandBindText,
		"sqlite3_enable_shared_cache": commandEnableSharedCache,
		"sqlite3_unlock_notify":       commandSQLite3UnlockNotify,
		"sqlite3_bind_blob":           commandBindBlob,
		"strftime":                    commandStrftime,
		"wal_file_size":               commandWalFileSize,
		"string":                      commandString,
		"regexp":                      commandRegexp,
		"regsub":                      commandRegsub,

		"catch":   commandCatch,
		"error":   commandError,
		"uplevel": commandUplevel,
		"upvar":   commandUpvar,
		"global":  commandGlobal,
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
		"do_searchcount_test":         commandDoSearchCount,
		"do_test":                     commandDoTest,
		"do_catchsql_test":            commandDoCatchSQL,
		"do_select_tests":             commandDoSelectTests,
		"do_eqp_test":                 commandDoEQP,
		"do_timed_execsql_test":       commandDoExecSQL,
		"do_execsql2_test":            commandDoExecSQL,
		"reset_db":                    commandResetDB,
		"faultsim_delete_and_reopen":  commandResetDB,
		"do_faultsim_test":            commandDoFaultsimTest,
		"do_malloc_test":              commandDoMallocTest,
		"faultsim_save":               commandFaultsimSave,
		"faultsim_save_and_close":     commandFaultsimSaveClose,
		"faultsim_restore":            commandFaultsimRestore,
		"faultsim_restore_and_reopen": commandFaultsimRestoreReopen,
		"db_delete_and_reopen":        commandResetDB,
		// malloc_common.tcl:169-172 — faultsim_save IS db_save (and the
		// restore pair): tester.tcl's snapshot helpers share the exact
		// implementation, so the interrupt2 section-3 rounds trip through
		// the same save-image steps.
		"db_save":                     commandFaultsimSave,
		"db_save_and_close":           commandFaultsimSaveClose,
		"db_restore":                  commandFaultsimRestore,
		"db_restore_and_reopen":       commandFaultsimRestoreReopen,
		"sqlite3":                     commandSQLite3,
		"sqlite3_backup":              commandSqlite3Backup,
		"testvfs":                     commandTestVFS,
		"register_circle_geom":        commandRegisterCircleGeom,
		"register_box_geom":           commandRegisterBoxGeom,
		"sqlite3_exec":                commandSQLite3Exec,
		"sqlite3_prepare_v2":          commandPrepareStmt,
		"sqlite3_prepare_v3":          commandPrepareStmt,
		"sqlite3_prepare":             commandPrepareStmt,
		"sqlite3_step":                commandStepStmt,
		"sqlite3_column_text":         commandColumnText,
		"sqlite3_db_readonly":         commandDBReadonly,
		"sqlite3_db_status":           commandDBStatus,
		"sqlite3_reset":               commandResetStmt,
		"sqlite3_finalize":            commandFinalizeStmt,
		"sqlite3_limit":               commandSQLite3Limit,
		"sqlite3_test_control":        commandSQLite3TestControl,
		"sqlite3_wal_checkpoint_v2":   commandSQLite3WalCheckpointV2,
		"sqlite3_wal_checkpoint":      commandSQLite3WalCheckpoint,
		"sqlite3_db_cacheflush":       commandCacheFlush,
		"vfs_set_readmark":            commandVfsSetReadmark,
		"sqlite3_create_collation_v2": commandCreateCollation,
		"sqlite3_create_function":     commandSQLiteCreateFunction,
		"sqlite_delete_collation":     commandDeleteCollation,
		"ifcapable":                   commandIfCapable,
		"drop_all_tables":             commandDropAllTables,
		"drop_all_tables_and_views":   commandDropAllMainTables,
		"drop_all_indexes":            commandDropAllIndexes,
		"register_echo_module":        commandRegisterEchoModule,
		// The multiplexor control surface (src/test_multiplex.c): the
		// conversion emits multiplex_control() SQL calls the engine's
		// multiplexor group answers.
		"sqlite3_multiplex_control":    commandMultiplexControl,
		"sqlite3_multiplex_initialize": noopCommand,
		"sqlite3_multiplex_shutdown":   noopCommand,
		"register_tcl_module":          commandRegisterTCLModule,
		"sqlite3_db_config":            commandDBConfig,
		"forcedelete":                  commandForceDelete,
		"forcecopy":                    commandForceCopy,
		"copy_file":                    commandForceCopy,
		"hexio_write":                  commandHexioWrite,
		"file":                         commandFile,
		"open":                         commandOpen,
		"puts":                         commandPuts,
		"close":                        commandClose,

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
	// A callback interp's registered commands (the harness binds `db` and
	// the named connection adapters) drive LIVE connections — they precede
	// every converter branch: e_walckpt's busy_handler `db2 eval commit`
	// at the invocation count commits db2's transaction, and the tvfs
	// callbacks' db5/db6 reads run against the live connections. The
	// converter itself never registers callback commands, so conversion
	// time is unaffected.
	if fn, ok := i.callbackCommands[cmd]; ok {
		result, err := fn(args)
		i.vars[""] = result
		return err
	}
	if fn, ok := commandHandlers[cmd]; ok {
		return fn(i, rawWords, args, localVars)
	}
	// e_walckpt's file-hash helpers (e_walckpt.test:27-44): the procs hash
	// the fixture databases with md5file and report which files a
	// checkpoint changed. Their bodies need a runtime md5file command and a
	// runtime conditional — the semantics are fully pinned in the C, so the
	// invocation emits the purpose-built step directly.
	if cmd == "save_db_hashes" {
		i.stmts = append(i.stmts, Stmt{Type: "save_db_hashes", SQL: "test.db test.db2 test.db3", TestName: i.curTest})
		i.vars[""] = ""
		return nil
	}
	if cmd == "compare_db_hashes" {
		i.stmts = append(i.stmts, Stmt{Type: "compare_db_hashes", SQL: "test.db test.db2 test.db3", TestName: i.curTest})
		i.vars[""] = catchResultPlaceholder
		return nil
	}
	// The harness's per-call default: resolve fixture connection names
	// against the live set — BEFORE the converter's named-db capture, which
	// would silently swallow a callback interp's `db5 eval` (the adapters
	// were bound at interp build time, before later scenarios opened their
	// connections).
	if i.defaultCallbackCmd != nil {
		if result, err, ok := i.defaultCallbackCmd(cmd, args); ok {
			i.vars[""] = result
			return err
		}
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
	if len(args) > 0 && args[0] == "update_hook" {
		// <db> update_hook <proc> — sqlite3_update_hook registration
		// (tkt-f7b4edec.test: db1 update_hook update_hook). The callback
		// receives (INSERT|UPDATE|DELETE, database, table, rowid) and
		// recording procs append it to a fixture global the do_test bodies
		// read back (`lappend ::HOOKS $args`). With an empty proc name the
		// hook clears (test1.c sqlite3_update_hook's NULL form); the bare
		// query form is not used by the corpus and fails loudly.
		if len(args) > 1 && strings.TrimSpace(args[1]) != "" {
			procName := strings.TrimPrefix(strings.TrimSpace(args[1]), "::")
			step := Stmt{Type: "update_hook", Database: cmd, TestName: i.curTest, AuthProc: procName}
			if proc, ok := i.procs[procName]; ok {
				step.CallbackProcs = []Proc{{Name: proc.Name, Args: proc.Args, Body: proc.Body}}
				step.FunctionProc = proc.Name
				step.FunctionBody = proc.Body
				if i.trackedVars == nil {
					i.trackedVars = make(map[string]bool)
				}
				if i.dynamicVars == nil {
					i.dynamicVars = make(map[string]bool)
				}
				for _, name := range NotifyScriptVarTargets(proc.Body) {
					i.trackedVars[name] = true
					// A `set NAME <value>` reset before the next callback
					// run is a runtime_var_reset (the hook list starts over).
					i.dynamicVars[name] = true
				}
			}
			i.trackCallbackVars(step.CallbackVars, step.FunctionBody)
			i.stmts = append(i.stmts, step)
			return nil
		}
		if len(args) > 1 {
			i.stmts = append(i.stmts, Stmt{Type: "update_hook", Database: cmd, TestName: i.curTest})
			return nil
		}
		return &UnsupportedCommandError{Cmd: cmd + " update_hook (bare query form)"}
	}
	if len(args) > 0 && (args[0] == "eval" || args[0] == "close" || args[0] == "unlock_notify") {
		if args[0] == "close" {
			i.stmts = append(i.stmts, Stmt{Type: "close_named_db", Database: cmd, TestName: i.curTest})
			return nil
		}
		if args[0] == "unlock_notify" {
			// sqlite3_unlock_notify on a named connection (notify1.test):
			// with a script, the callback fires when the blocking
			// connection's transaction ends (or immediately when none);
			// with no script, the pending callback is cancelled. The
			// script's write targets are globals the callbacks maintain
			// (lappend lUnlock db) — a later `set lUnlock` body value is a
			// runtime_var assertion against them.
			step := Stmt{Type: "unlock_notify", Database: cmd, TestName: i.curTest}
			if len(args) > 1 {
				step.CallbackProcs = i.notifyScriptProcs(args[1])
				step.FunctionScript = args[1]
				if i.trackedVars == nil {
					i.trackedVars = make(map[string]bool)
				}
				if i.dynamicVars == nil {
					i.dynamicVars = make(map[string]bool)
				}
				for _, name := range NotifyScriptVarTargets(args[1]) {
					i.trackedVars[name] = true
					// A `set NAME <value>` reset before the next callback
					// run is a runtime_expr (notify1-6.2.1's
					// `set unlocked 0` must reach the harness).
					i.dynamicVars[name] = true
				}
			}
			i.stmts = append(i.stmts, step)
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
	if i.testvfsNames[cmd] {
		if _, ok := testvfsOptionArity(args[0]); ok {
			return commandTestVFSConfig(i, args, cmd)
		}
	}
	if len(args) > 0 {
		if _, ok := i.backupHandles[cmd]; ok {
			if _, known := backupHandleArity(args[0]); known {
				return i.backupHandleMethod(cmd, args)
			}
		}
	}
	if proc, ok := i.procs[cmd]; ok {
		return i.callProc(proc, args, localVars)
	}
	// Unknown command — silently ignore (many TCL commands are not needed)
	return nil
}

// backupHandleMethod dispatches a sqlite3_backup command object's method
// (test_backup.c's backupTestCmd). `step N` emits the backup_db step — the
// harness runs the logical copy to completion, matching C's step when the
// page budget covers the source (walbak-3's `B step 10000`); `finish`
// releases the backup and DELETES the command object, and C's finish copies
// nothing itself, so a finish with no preceding step emits no step.
// remaining/pagecount report progress — no converted script consumes them.
func (i *Interp) backupHandleMethod(cmd string, args []string) error {
	info := i.backupHandles[cmd]
	switch args[0] {
	case "step":
		pages := ""
		if len(args) >= 2 {
			pages = args[1]
		}
		i.stmts = append(i.stmts, Stmt{
			Type:     "backup_db",
			SQL:      pages,
			Database: info.dest,
			DestDB:   info.destDB,
			SrcConn:  info.src,
			SrcDB:    info.srcDB,
			TestName: i.curTest,
		})
		info.stepped = true
		i.backupHandles[cmd] = info
		// C returns sqlite3ErrName: DONE once the budget covers the source.
		i.vars[""] = "SQLITE_DONE"
	case "finish":
		delete(i.backupHandles, cmd)
		i.vars[""] = "SQLITE_OK"
	case "remaining", "pagecount":
		i.vars[""] = "0"
	}
	return nil
}

// commandGlobal implements Tcl's `global NAME...` inside a proc body: the
// names alias interpreter globals for the call — reads see the global's
// current value (seeded into the local scope) and writes land in the global
// slot as well (setVar's write-through), so multiplex_set's
// `global g_chunk_size; set g_chunk_size ...` updates the value later
// references read.
func commandGlobal(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if localVars == nil {
		return nil // top-level `global` is a no-op
	}
	for _, a := range args {
		name := strings.TrimPrefix(a, "::")
		if name == "" {
			continue
		}
		i.procGlobals[name] = true
		if _, exists := localVars[name]; !exists {
			localVars[name] = i.vars[name]
		}
	}
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
	// `eval $var` whose value exists only at run time (notify1-1.5's
	// zScript, written by the unlock-notify callback): the conversion-time
	// value is empty, so nothing can execute here — record an eval_script
	// step and the harness evaluates the variable's script with a db
	// adapter. A conversion-time-known script keeps executing here.
	if len(args) == 1 && len(rawWords) >= 2 && strings.TrimSpace(args[0]) == "" {
		if name, ok := runtimeVarRef(strings.TrimSpace(rawWords[1].Text)); ok {
			i.stmts = append(i.stmts, Stmt{Type: "eval_script", Variable: strings.TrimPrefix(name, "::"), TestName: i.curTest})
			return nil
		}
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
	// A command substitution that FAILED inside the substituted text must
	// abort: substituteCmd already stored the error in the sticky substErr,
	// but commandSubst was ignoring it — the missing command's "" then
	// silently dropped out of the result, shifting every list element after
	// it (pager1-7.1's foreach column binding became garbage for tn>=6).
	if i.substErr != nil {
		err := i.substErr
		i.substErr = nil
		return err
	}
	i.vars[""] = result
	return nil
}

// commandWalFileSize mirrors wal_common.tcl:16-18
// (`proc wal_file_size {nFrame pgsz} { expr {32 + ($pgsz+24)*$nFrame} }`):
// the WAL file's byte size for nFrame frames of pgsz page size. pager1.test
// sources wal_common.tcl and calls the proc inside the
// `foreach {tn sql res js ws} [subst {...}]` list of its 7.1 section — an
// unknown command there would silently yield "" and shift every loop
// variable after the first missing element (round1's silent-garbage class).
func commandWalFileSize(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) != 2 {
		return fmt.Errorf("wrong # args: should be \"wal_file_size nFrame pgsz\"")
	}
	nFrame, err1 := strconv.Atoi(strings.TrimSpace(args[0]))
	pgsz, err2 := strconv.Atoi(strings.TrimSpace(args[1]))
	if err1 != nil || err2 != nil {
		return fmt.Errorf("wal_file_size: expected integer arguments, got %q %q", args[0], args[1])
	}
	i.vars[""] = strconv.Itoa(32 + (pgsz+24)*nFrame)
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
	// `set F [loadhex test.db]` (func6.test) reads the database FILE into a
	// hex string the record-verification UDFs compare against. The file's
	// bytes exist only at run time: record a load_hex_file step and defer F
	// to the harness's runtime variables.
	if len(args) >= 2 && len(rawWords) >= 3 && strings.HasPrefix(args[0], "::") == false {
		text := strings.TrimSpace(rawWords[2].Text)
		if strings.HasPrefix(text, "[loadhex ") && strings.HasSuffix(text, "]") {
			path := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "[loadhex "), "]"))
			name := strings.TrimPrefix(args[0], "::")
			i.stmts = append(i.stmts, Stmt{Type: "load_hex_file", SQL: path, Variable: name, TestName: i.curTest})
			i.setVar(args[0], "$"+name, localVars)
			i.vars[""] = "$" + name
			return nil
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
			// A backslash-newline continuation inside the word (func4.test
			// wraps its probe exprs over two lines) separates the braced
			// body from the [expr head; Tcl treats it as a space.
			inner = strings.TrimSpace(strings.ReplaceAll(inner, "\\\n", " "))
			// The expression is usually a braced word: `expr {$a eq $b}` —
			// the braces delimit the expression, they are not part of it.
			if len(inner) >= 2 && strings.HasPrefix(inner, "{") && strings.HasSuffix(inner, "}") {
				inner = strings.TrimSpace(inner[1 : len(inner)-1])
			}
			// The expression may embed a query command substitution instead
			// of a variable reference (func4.test's floating-point platform
			// probes: `set highPrecision(3) [expr {[db eval {SELECT
			// toreal(...);}] eq {{}}}]`): the query ran while the value word
			// was evaluated and its rows exist only at run time. The
			// embedded substitution becomes a probe variable the harness
			// fills from the captured query, so the expression is deferred
			// against it instead of statically comparing the runtime marker
			// (which silently answers 0 and records the `if`'s else branch).
			if inner != "" && !i.exprReadsRuntimeVar(inner) {
				if rewritten, ok := i.rewriteEmbeddedQueryExpr(inner); ok {
					inner = rewritten
				}
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
	// `set in [join [array names ::T] ,]` (index9's sqluses) — a runtime
	// command over callback-written globals: capture it as a runtime_expr
	// the harness command layer evaluates (evalHarnessRuntimeExpr routes
	// command heads; unknown commands error loudly).
	if i.conversionMode && len(args) >= 2 && len(rawWords) >= 3 {
		text := strings.TrimSpace(rawWords[2].Text)
		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			inner := strings.TrimSpace(text[1 : len(text)-1])
			if head := runtimeCommandHead(inner); head != "" {
				name := strings.TrimPrefix(args[0], "::")
				i.stmts = append(i.stmts, Stmt{
					Type:     "runtime_expr",
					SQL:      inner,
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
	// A GLOBAL literal assignment (`set ::NAME <literal>`) must reach the
	// harness's runtime variables: the callback procs read and increment
	// these globals (e_walhook's `set ::wal_hook_count 0` pre-set before
	// 1.1.1's runtime_var read — the hook never fired in rollback mode, so
	// the pre-set is the only writer). Emit a runtime_expr step whose
	// literal evaluates into the runtime set. Local (non-::) sets stay
	// conversion-time state.
	if len(args) >= 2 && strings.HasPrefix(args[0], "::") && !assignedFromQuery {
		value := strings.TrimSpace(args[1])
		if value != "" && !strings.ContainsAny(value, "$[]\"{") {
			i.stmts = append(i.stmts, Stmt{
				Type:     "runtime_expr",
				SQL:      value,
				Variable: strings.TrimPrefix(args[0], "::"),
				TestName: i.curTest,
			})
		}
	}
	return nil
}

// rewriteEmbeddedQueryExpr rewrites an expression that reads an embedded
// query substitution: `... [db eval {SELECT ...}] ...` becomes
// `... $runtime_probe_N ...`, where the probe variable is assigned from the
// query that evaluating the substituted word just captured (the newest
// statement). A query inside an expr is a runtime value — func4.test's
// floating-point platform probes compare one against the empty list:
//
//	set highPrecision(3) [expr \
//	    {[db eval {SELECT toreal(9007199254740992 + 1);}] eq {{}}}]
//
// Statically the substitution carries the runtime marker and the comparison
// answers 0, silently recording every conditional's else branch. ok is false
// when the expression embeds no (single) query substitution; the caller
// keeps the original text then.
func (i *Interp) rewriteEmbeddedQueryExpr(inner string) (string, bool) {
	depth, start := 0, -1
	spanStart, spanEnd := 0, 0
	matchedCmd := ""
	found := 0
	for pos := 0; pos < len(inner); pos++ {
		switch inner[pos] {
		case '[':
			if depth == 0 {
				start = pos
			}
			depth++
		case ']':
			depth--
			if depth == 0 && start >= 0 {
				cmd := strings.TrimSpace(inner[start+1 : pos])
				if strings.HasPrefix(cmd, "db eval ") || strings.HasPrefix(cmd, "db one ") ||
					strings.HasPrefix(cmd, "db onecolumn ") || strings.HasPrefix(cmd, "execsql ") {
					spanStart, spanEnd, matchedCmd = start, pos+1, cmd
					found++
				}
				start = -1
			}
		}
	}
	if found != 1 || len(i.stmts) == 0 {
		return inner, false
	}
	last := &i.stmts[len(i.stmts)-1]
	if last.TestName != i.curTest || (last.Type != "exec" && last.Type != "query") ||
		!isQueryStmt(firstStatement(last.SQL)) {
		return inner, false
	}
	name := fmt.Sprintf("runtime_probe_%d", i.runtimeProbeCount)
	i.runtimeProbeCount++
	// `db eval`/`execsql` yield the whole result (a list of rows), `db one`/
	// `db onecolumn` a single value (see commandSet's direct substitutions).
	last.Type = "query_assign"
	last.Variable = name
	last.ResultAll = !strings.HasPrefix(matchedCmd, "db one")
	i.dynamicVars[name] = true
	i.runtimeVars[name] = last.runtimeID
	return inner[:spanStart] + "$" + name + inner[spanEnd:], true
}

// commandBinary implements the `binary encode hex <string>` form
// (func6.test's loadhex/hexrecord build their comparisons with it): each
// byte renders as two lowercase hex digits, exactly TCL's output — and the
// `binary format` form's `c` field (tclBinary.c FormatBinary's 'c' case):
// `c*` packs every element of the list argument as one byte
// (walckptnoop.test's myrandomblob).
func commandBinary(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) >= 3 && args[0] == "encode" && args[1] == "hex" {
		const digits = "0123456789abcdef"
		s := args[2]
		out := make([]byte, 0, len(s)*2)
		for k := 0; k < len(s); k++ {
			out = append(out, digits[s[k]>>4], digits[s[k]&0x0f])
		}
		i.vars[""] = string(out)
		return nil
	}
	if len(args) >= 3 && args[0] == "format" {
		out, err := binaryFormatFields(args[1], args[2:])
		if err != nil {
			return err
		}
		i.vars[""] = string(out)
		return nil
	}
	return fmt.Errorf("unsupported binary form: %s", strings.Join(args, " "))
}

// binaryFormatFields packs the values per the format string's `c` fields: a
// count suffix of `*` consumes every element of the list argument; an
// integer count consumes exactly that many; a bare letter consumes one.
// TCL truncates each integer to its low 8 bits; a non-integer element fails
// loudly like C's "expected integer but got". The other field letters are
// the separate binary-class unit (TODO queue).
func binaryFormatFields(format string, valueArgs []string) ([]byte, error) {
	if len(valueArgs) == 0 {
		return nil, fmt.Errorf("wrong # args: should be \"binary format formatString ?arg ...?\"")
	}
	list := strings.Fields(valueArgs[0])
	var out []byte
	pos := 0
	for k := 0; k < len(format); {
		// A field is a letter optionally followed by a count: digits
		// (repeat that many times) or `*` (consume the whole list).
		letter := format[k]
		k++
		count, star := 1, false
		if k < len(format) && (format[k] == '*' || format[k] >= '0' && format[k] <= '9') {
			if format[k] == '*' {
				star = true
				k++
			} else {
				start := k
				for k < len(format) && format[k] >= '0' && format[k] <= '9' {
					k++
				}
				n, err := strconv.Atoi(format[start:k])
				if err != nil {
					return nil, fmt.Errorf("bad field specifier %q", format)
				}
				count = n
			}
		}
		switch letter {
		case 'c':
			if star || pos+count > len(list) {
				count = len(list) - pos
			}
			for k := 0; k < count; k++ {
				v, err := strconv.Atoi(strings.TrimSpace(list[pos+k]))
				if err != nil {
					return nil, fmt.Errorf("expected integer but got %q", list[pos+k])
				}
				out = append(out, byte(v))
			}
			pos += count
		default:
			return nil, fmt.Errorf("unsupported binary field %q", string(letter))
		}
	}
	return out, nil
}

// runtimeCommandHead reports the head word when the command text starts with
// one of the harness runtime command layer's commands (the capture shapes of
// `set X [cmd ...]`), or "" otherwise.
func runtimeCommandHead(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	switch strings.ToLower(fields[0]) {
	case "join", "lsearch", "lindex", "array", "file":
		return strings.ToLower(fields[0])
	}
	return ""
}

// commandUnset implements `unset var...`.
func commandUnset(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	for _, a := range args {
		if strings.TrimPrefix(a, "::") == "echo_module_ignore_usable" {
			i.stmts = append(i.stmts, Stmt{Type: "echo_ignore_usable", SQL: "0", TestName: i.curTest})
		}
		name := strings.TrimPrefix(a, "::")
		if i.conversionMode && i.dynamicVars[name] {
			// A runtime-carrying variable must LEAVE the runtime set at run
			// time too (index9's `unset -nocomplain y` before the 1.4 probe:
			// the raw expansion then leaves $y in the SQL text — the engine
			// binds it as an unbound named parameter).
			i.stmts = append(i.stmts, Stmt{Type: "runtime_var_unset", Variable: name, TestName: i.curTest})
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
		// TCL's catch executes its first argument as a script whether it
		// was a braced literal or a quoted string (the quoted form is
		// substituted first — e_walckpt-4.x's
		// `catch "wal_checkpoint_v2 db $mode" msg` with the foreach-baked
		// mode): both shapes run through the same capture-and-stamp path.
		statementsBefore := len(i.stmts)
		i.catchDepth++
		// A non-braced body was already substituted at word-evaluation
		// time; TCL still executes the resulting string as a script.
		err := i.execScript(body.Text, localVars)
		{
			i.catchDepth--
			if len(i.stmts) > statementsBefore {
				// Statement capture is authoritative even when the
				// conversion-time body evaluation ALSO errored: an error
				// raised after a statement was captured (fts3expr3's
				// strip_phrase_data recursion over the runtime placeholder
				// hitting the proc-depth limiter) is a conversion artifact —
				// the runtime outcome is the statement's own rc and message,
				// never the artifact text baked into $msg.
				//
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
						last := &i.stmts[len(i.stmts)-1]
						last.CatchMessage = msgVar
						// The catch's rc is consumed by the body's value
						// (`list [catch {...} msg] $msg` — walvfs-5.5/5.6)
						// without a named `set rc` binding: stamp the
						// statement with a deterministic rc variable so the
						// harness records it (the runtime_list element
						// references $__catchrc_<msg>). A statement that
						// already carries a Variable keeps its binding (the
						// named `set rc [catch ...]` path re-stamps it).
						if last.Variable == "" {
							rcVar := "__catchrc_" + msgVar
							last.Variable = rcVar
							last.CatchResult = true
							i.dynamicVars[rcVar] = true
						}
					}
				} else if len(i.stmts) > 0 {
					// A bare `catch { ... }` (fts3expr3-4.1.2): no message
					// variable exists, but the body's value is still the
					// catch rc — a runtime fact of the captured statement.
					// Stamp a deterministic rc variable so the do_test
					// body-value comparison can reference it.
					last := &i.stmts[len(i.stmts)-1]
					if last.Variable == "" {
						last.Variable = "__catchrc"
						last.CatchResult = true
						i.dynamicVars["__catchrc"] = true
					}
				}
			} else if err != nil {
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

// commandSQLite3WalCheckpointV2 implements the testfixture command
// `sqlite3_wal_checkpoint_v2 db MODE ?NAME?` (test1.c:7615): the checkpoint
// runs at harness time on the named connection for the named schema (no
// NAME — the C passes a NULL zDb — checkpoints every attached WAL
// database), and the command returns the {busy nLog nCkpt} triple. The
// fixture's wal_checkpoint_v2 wrapper (e_walckpt.test:57) calls this
// through `uplevel` from the checkpoint procs the foreach-script unroll
// inlines.
func commandSQLite3WalCheckpointV2(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	step := Stmt{Type: "wal_checkpoint_v2", Database: args[0], SQL: args[1], TestName: i.curTest}
	if len(args) >= 3 {
		// The optional NAME argument (main/aux/aux2/...); the C's "-" value
		// means NULL zDb (the fixture converts it to the no-arg form).
		// FileTarget — not Variable — carries it: the `set rc [catch {...}
		// msg]` wrapper stamps Variable with the catch's result variable.
		step.FileTarget = args[2]
	}
	i.stmts = append(i.stmts, step)
	i.vars[""] = catchResultPlaceholder
	return nil
}

// commandSQLite3WalCheckpoint implements `sqlite3_wal_checkpoint db ?NAME?`
// (test1.c): the PASSIVE checkpoint whose TCL result is the result-code
// NAME ("SQLITE_OK"/"SQLITE_BUSY"/...) rather than the triple — the
// fixture's script-3 checkpoint proc compares it against SQLITE_OK.
func commandSQLite3WalCheckpoint(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	step := Stmt{Type: "wal_checkpoint_rc", Database: args[0], SQL: "passive", TestName: i.curTest}
	if len(args) >= 2 {
		// FileTarget carries the schema (see commandSQLite3WalCheckpointV2).
		step.FileTarget = args[1]
	}
	i.stmts = append(i.stmts, step)
	i.vars[""] = catchResultPlaceholder
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
// commandVfsSetReadmark captures vfs_set_readmark DB DBNAME SLOT ?VALUE?
// (test_vfs.c test_vfs_set_readmark): the reader form returns the slot's
// current wal-index read-mark value, the writer form stores VALUE first —
// both return the value now stored. The harness answers through the named
// connection's pager (the C command takes the connection pointer).
func commandVfsSetReadmark(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 4 {
		return nil
	}
	stmt := Stmt{Type: "set_readmark", Database: args[0], SQL: args[2], TestName: i.curTest}
	if len(args) >= 4 {
		// The writer form: vfs_set_readmark DB DBNAME SLOT VALUE (the
		// reader form stops at SLOT).
		stmt.WriteValue = args[3]
	}
	i.stmts = append(i.stmts, stmt)
	// The call's result is the value now stored — a runtime value the
	// do_test promotion attaches the body expectation to.
	i.vars[""] = i.markRuntimeValue(&i.stmts[len(i.stmts)-1])
	return nil
}

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

// commandDoSearchCount implements whereD.test's do_searchcount_test helper
// (a do_execsql_test whose body resets SQLITE_TEST's search counter and
// appends it with the "search" label): emit the query with SearchCount plus
// the labeled style.
func commandDoSearchCount(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.searchCountLabeled = true
	defer func() { i.searchCountLabeled = false }()
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
	// Options follow the filename: `-vfs <name>` opens through the named
	// testvfs wrapper (walvfs.test's `sqlite3 db test.db -vfs tvfs`);
	// `-readonly 1` opens SQLITE_OPEN_READONLY (pager1-4.5.6's
	// `sqlite3 db test.db -readonly 1`).
	vfsName := ""
	readOnly := false
	for k := 2; k+1 < len(args); k += 2 {
		if args[k] == "-vfs" {
			vfsName = args[k+1]
		}
		if args[k] == "-readonly" {
			readOnly = args[k+1] == "1"
		}
	}
	i.pendingVFS = vfsName
	i.pendingReadOnly = readOnly
	defer func() { i.pendingVFS = ""; i.pendingReadOnly = false }()
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
		} else if name == "db" {
			i.stmts = append(i.stmts, Stmt{Type: "close_named_db", Database: name})
		} else {
			// `sqlite3 tmp ""` (temptable2-8.2): an empty filename opens a
			// private temporary database on the named connection — NOT a
			// close. The harness maps the empty name to a private db.
			i.stmts = append(i.stmts, Stmt{Type: "open_named_db", SQL: "", Database: name, TestName: i.curTest})
		}
		return nil
	}
	// The open target: only the URI's PATH PART feeds the deletedFiles
	// lifecycle lookup (forcedelete test.db → `sqlite3 db test.db` must
	// reopen).
	uri := ""
	path := filename
	if strings.HasPrefix(filename, "file:") {
		uri = filename
		path = strings.TrimPrefix(path, "file:")
		if n := strings.IndexByte(path, '?'); n >= 0 {
			path = path[:n]
		}
	}
	path = filepath.Clean(path)
	if name == "db" && i.deletedFiles[path] {
		// The deletedFiles branch models the harness's LIFECYCLE file: the
		// reopen targets the fixture basename (8_3_names's
		// `forcedelete test.db; sqlite3 db file:./test.db?8_3_names=1`
		// reopens the lifecycle test.db — the VFS query is a compile-time
		// feature the harness does not model).
		delete(i.deletedFiles, path)
		i.stmts = append(i.stmts, Stmt{Type: "reopen_db", SQL: filepath.Base(path), VFS: i.pendingVFS, ReadOnly: i.pendingReadOnly, TestName: i.curTest})
		return nil
	}
	// The normal branch: a `file:` URI stays INTACT (the engine's Open
	// parses mode=memory / cache=shared / vfs=memdb URIs itself —
	// frigolite.Open's sharedMemURI / memdbName routing — and collapsing
	// `file:?mode=memory&cache=shared` to the cleaned empty path "."
	// turned pager1-37.1's shared-memory open into a directory open).
	openTarget := filepath.Base(path)
	if uri != "" {
		openTarget = uri
	}
	if name == "db" {
		i.stmts = append(i.stmts, Stmt{Type: "reopen_db", SQL: openTarget, VFS: i.pendingVFS, ReadOnly: i.pendingReadOnly, TestName: i.curTest})
	} else {
		i.stmts = append(i.stmts, Stmt{Type: "open_named_db", SQL: filename, Database: name, VFS: i.pendingVFS, ReadOnly: i.pendingReadOnly, TestName: i.curTest})
	}
	return nil
}

// commandDBReadonly captures `sqlite3_db_readonly db NAME` (pager1-4.8.x):
// the C-API returns 1 when the database file lacks write permission, 0 when
// it is writable, and -1 when NAME matches no open database.
func commandDBReadonly(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	i.stmts = append(i.stmts, Stmt{Type: "capi_db_readonly", Database: args[0], SQL: args[1], TestName: i.curTest})
	return nil
}

// commandDBStatus captures `sqlite3_db_status db CACHE_MISS N` (pager1-43.x):
// the do_test body value is the {code, counter, unused} triple of the
// connection's page-cache statistic. The reset flag zeroes the counter
// after reading it (pager.c sqlite3PagerCacheStat).
func commandDBStatus(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	op := strings.ToUpper(strings.TrimSpace(args[1]))
	if op != "CACHE_MISS" {
		// Only the CACHE_MISS statistic is asserted by the corpus today;
		// an unrecognised op would bake a wrong expectation.
		return &UnsupportedCommandError{Cmd: "sqlite3_db_status " + op}
	}
	reset := false
	if len(args) >= 3 {
		reset = strings.TrimSpace(args[2]) == "1"
	}
	i.stmts = append(i.stmts, Stmt{Type: "capi_db_status", Database: args[0], SQL: op, StatusReset: reset, TestName: i.curTest})
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
	if len(args) == 4 && args[0] == "attributes" {
		return commandFileAttributes(i, args)
	}
	// Path-inspection subcommands the fixture callbacks use (walvfs's
	// xSync dispatches on `file tail $file`).
	switch {
	case len(args) == 2 && args[0] == "tail":
		i.vars[""] = filepath.Base(args[1])
	case len(args) == 2 && args[0] == "dirname":
		i.vars[""] = filepath.Dir(args[1])
	case len(args) == 2 && args[0] == "extension":
		i.vars[""] = filepath.Ext(args[1])
	}
	return nil
}

// commandFileAttributes captures `file attributes PATH -permissions X` /
// `-readonly N` (pager1-4.7.2 / 4.8.x): the fixture makes files unwritable
// to exercise the readonly error paths. The attribute is a RUNTIME chmod on
// the target path — the harness applies the permission bits when the step
// executes. (TCL's catch{} around the call tolerates a missing file; so
// does the harness handler.)
func commandFileAttributes(i *Interp, args []string) error {
	attr, val := args[2], args[3]
	if attr != "-permissions" && attr != "-readonly" {
		return nil
	}
	i.stmts = append(i.stmts, Stmt{Type: "file_attr", SQL: args[1], Variable: attr, BindValue: val, TestName: i.curTest})
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

// commandDropAllIndexes mirrors tester.tcl's drop_all_indexes: every
// EXPLICIT index in main (`sql LIKE 'create%'` — implicit auto-indexes have
// no SQL) is dropped. rowvalue4.test's section loop re-creates t2abc with a
// different definition per iteration; without the drop the second CREATE
// fails "already exists" and every leaf of the iteration starves.
func commandDropAllIndexes(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "drop_all_indexes"})
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
	// Flush the pending file-level array sets that the registered callback
	// procs READ: the sets' values must reach the harness's runtime set
	// BEFORE the register snapshot (the callback bodies resolve them at
	// invocation — bestindex1's vtab_command reads the G sets the file
	// level established at :173, with the register at :239). The reads'
	// set is complete here because the reading procs are already
	// registered.
	reads := make(map[string]bool)
	for _, proc := range i.CallbackProcs() {
		for _, name := range ScriptVarReads(proc.Body) {
			reads[name] = true
		}
	}
	if len(i.pendingFileSets) > 0 {
		remaining := make([]string, 0, len(i.pendingFileSets))
		for _, entry := range i.pendingFileSets {
			sep := strings.Index(entry, "\x00")
			if sep < 0 {
				continue
			}
			base := strings.TrimSuffix(entry[:sep], "(")
			base = strings.TrimPrefix(base, "::")
			if reads[base] {
				i.stmts = append(i.stmts, Stmt{
					Type: "runtime_expr", Variable: entry[:sep],
					SQL: `"` + entry[sep+1:] + `"`, TestName: i.curTest,
				})
				continue
			}
			remaining = append(remaining, entry)
		}
		i.pendingFileSets = remaining
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
	case "null_trim":
		// SQLITE_ENABLE_NULL_TRIM is a non-default compile option: the
		// standard build (and this engine) keeps trailing NULLs in records
		// (func6.test's bNullTrim probes).
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

// commandMultiplexControl implements sqlite3_multiplex_control HANDLE DBNAME
// SUB-COMMAND ?INT-VALUE? (src/test_multiplex.c's test_multiplex_control):
// the sub-command maps to the multiplex_control() SQL function's op —
// enable→1, chunk_size→2, max_chunks→3 — and the conversion emits the
// function call as an exec step (the engine's multiplex_control resolves the
// main pager's multiplexor group). The value passes through verbatim so a
// loop variable's per-iteration literal reaches the generated SQL.
func commandMultiplexControl(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 3 {
		return nil
	}
	op := 0
	switch args[2] {
	case "enable":
		op = 1
	case "chunk_size":
		op = 2
	case "max_chunks":
		op = 3
	default:
		return nil
	}
	val := "0"
	if len(args) > 3 {
		val = args[3]
	}
	i.stmts = append(i.stmts, Stmt{
		Type:     "exec",
		SQL:      fmt.Sprintf("SELECT multiplex_control(%d, %s);", op, val),
		TestName: i.curTest,
	})
	return nil
}
