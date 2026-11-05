package tcl

import (
	"fmt"
	"strconv"
	"strings"
)

// commandSQLite3UnlockNotify records `sqlite3_unlock_notify DB` (test1.c):
// the registered callback evaluates the fixed "unlock_notify" proc on the
// named connection when the blocker's transaction ends.
func commandSQLite3UnlockNotify(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	step := Stmt{Type: "unlock_notify", Database: strings.TrimPrefix(args[0], "::"), TestName: i.curTest, FunctionScript: "unlock_notify"}
	step.CallbackProcs = i.notifyScriptProcs("unlock_notify")
	i.stmts = append(i.stmts, step)
	i.vars[""] = "SQLITE_OK"
	return nil
}

// NotifyScriptVarTargets lists the variables a callback script writes: the
// object of each set/lappend/append/incr command head. The unlock-notify
// machinery uses it both to track the globals a fired callback maintains
// (conversion) and to sync the writes back into the runtime variables
// (harness).
func NotifyScriptVarTargets(script string) []string {
	var names []string
	seen := make(map[string]bool)
	fields := strings.Fields(script)
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "set", "lappend", "append", "incr":
			name := strings.TrimPrefix(fields[i+1], "::")
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

// commandEnableSharedCache records the sqlite3_enable_shared_cache C-API
// call (notify1.test's file-level setup): the harness toggles the
// process-global shared-cache flag; connections opened after "1" join their
// file's shared-cache group.
func commandEnableSharedCache(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	flag := "1"
	if len(args) > 0 && strings.TrimSpace(args[0]) == "0" {
		flag = "0"
	}
	i.stmts = append(i.stmts, Stmt{Type: "enable_shared_cache", SQL: flag, TestName: i.curTest})
	// The previous flag travels to `set ::enable_shared_cache [...]` so the
	// file's closing restore records the value it restores TO ("0").
	previous := "0"
	if i.sharedCacheFlag {
		previous = "1"
	}
	i.sharedCacheFlag = flag == "1"
	i.vars[""] = previous
	return nil
}

// commandCacheFlush captures `sqlite3_db_cacheflush`.
func commandCacheFlush(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	database := ""
	if len(args) > 0 && args[0] != "db" {
		database = args[0]
	}
	i.stmts = append(i.stmts, Stmt{Type: "cacheflush", Database: database, TestName: i.curTest})
	i.vars[""] = "__CACHEFLUSH__"
	return nil
}

func commandPrepareStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	i.preparedSeq++
	handle := fmt.Sprintf("stmt_%d", i.preparedSeq)
	i.preparedSQL[handle] = args[1]
	database := args[0]
	if database == "db" {
		database = ""
	}
	i.stmts = append(i.stmts, Stmt{Type: "prepare_stmt", SQL: args[1], Variable: handle, Database: database, TestName: i.curTest})
	i.vars[""] = handle
	return nil
}

func commandStepStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	i.stmts = append(i.stmts, Stmt{Type: "stmt_step", Variable: handle, SQL: i.preparedSQL[handle], TestName: i.curTest})
	i.vars[""] = "__STEP_" + handle + "__"
	return nil
}

func commandResetStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	i.stmts = append(i.stmts, Stmt{Type: "stmt_reset", Variable: handle, SQL: i.preparedSQL[handle], TestName: i.curTest})
	i.vars[""] = "SQLITE_OK"
	return nil
}

func commandFinalizeStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	i.stmts = append(i.stmts, Stmt{Type: "stmt_finalize", Variable: handle, SQL: i.preparedSQL[handle], TestName: i.curTest})
	delete(i.preparedSQL, handle)
	i.vars[""] = "SQLITE_OK"
	return nil
}

// commandBindStmt captures `sqlite3_bind_text $STMT N VALUE LEN` /
// `sqlite3_bind_blob $STMT N VALUE LEN` (func-13.7/16.1): the harness binds
// the prepared statement's parameter before stepping.
func commandBindStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	// args excludes the command name (dispatchCommand convention):
	// `sqlite3_bind_text $STMT 1 hello\000 -1` → args = [stmt_1 1 hello\000 -1].
	if len(args) < 4 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	idx, err := strconv.Atoi(strings.TrimSpace(args[1]))
	if err != nil {
		return nil
	}
	blob := sqlite3BindKind == "blob"
	i.stmts = append(i.stmts, Stmt{
		Type:      "stmt_bind",
		Variable:  handle,
		SQL:       i.preparedSQL[handle],
		BindIndex: idx,
		BindValue: args[2],
		BindBlob:  blob,
		TestName:  i.curTest,
	})
	i.vars[""] = "SQLITE_OK"
	return nil
}

// sqlite3BindKind is set by the dispatch wrapper: which sqlite3_bind_*
// command invoked commandBindStmt.
var sqlite3BindKind string

func commandBindText(i *Interp, rw []rawWord, args []string, lv map[string]string) error {
	sqlite3BindKind = "text"
	defer func() { sqlite3BindKind = "" }()
	return commandBindStmt(i, rw, args, lv)
}

func commandBindBlob(i *Interp, rw []rawWord, args []string, lv map[string]string) error {
	sqlite3BindKind = "blob"
	defer func() { sqlite3BindKind = "" }()
	return commandBindStmt(i, rw, args, lv)
}

// stmtCollectShape is the while-loop row-collection contract (func-13.7):
//
//	while { "SQLITE_ROW"==[sqlite3_step $STMT] } {
//	  lappend res [sqlite3_column_text $STMT 0]
//	}
type stmtCollectShape struct {
	handle  string
	column  int
	listVar string
}

// stmtCollectWhileShape reports whether a while loop is the C-API row
// collection shape and extracts its parts. Handles arrive as raw `$STMT`
// references (cond/body are unevaluated words) and are resolved through the
// variable table to the prepare machinery's stmt_N handle — the identity the
// prepare_stmt/stmt_finalize steps use.
func (i *Interp) stmtCollectWhileShape(cond, body rawWord, localVars map[string]string) (stmtCollectShape, bool) {
	condText := cond.Text
	if !strings.Contains(condText, `"SQLITE_ROW"==[sqlite3_step`) &&
		!strings.Contains(condText, `"SQLITE_ROW" == [sqlite3_step`) {
		return stmtCollectShape{}, false
	}
	resolveHandle := func(raw string) (string, bool) {
		name := strings.TrimPrefix(raw, "$")
		if name == raw {
			return "", false
		}
		val, ok := i.getVar(name, localVars)
		if !ok || val == "" || strings.HasPrefix(val, "$") {
			return "", false
		}
		return val, true
	}
	cmds := parseCommands(body.Text)
	if len(cmds) == 0 {
		return stmtCollectShape{}, false
	}
	handle := ""
	column := -1
	listVar := ""
	for _, cmd := range cmds {
		if len(cmd) != 3 || cmd[0].Text != "lappend" {
			return stmtCollectShape{}, false
		}
		if listVar == "" {
			listVar = strings.TrimPrefix(cmd[1].Text, "$")
		} else if strings.TrimPrefix(cmd[1].Text, "$") != listVar {
			return stmtCollectShape{}, false
		}
		inner := strings.TrimSpace(cmd[2].Text)
		if !strings.HasPrefix(inner, "[sqlite3_column_text ") || !strings.HasSuffix(inner, "]") {
			return stmtCollectShape{}, false
		}
		fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(inner, "[sqlite3_column_text "), "]"))
		if len(fields) != 2 {
			return stmtCollectShape{}, false
		}
		h, ok := resolveHandle(fields[0])
		if !ok {
			return stmtCollectShape{}, false
		}
		if handle == "" {
			handle = h
		} else if h != handle {
			return stmtCollectShape{}, false
		}
		col, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return stmtCollectShape{}, false
		}
		if column >= 0 && col != column {
			return stmtCollectShape{}, false
		}
		column = col
	}
	// The condition's step handle must agree with the body's. Slice the
	// bracketed command out of the condition (`...==[sqlite3_step $STMT] ...`):
	// word 0 is the command name, word 1 the `$STMT` handle.
	stepIdx := strings.Index(condText, "[sqlite3_step")
	rest := condText[stepIdx+1:]
	if end := strings.Index(rest, "]"); end >= 0 {
		rest = rest[:end]
	}
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return stmtCollectShape{}, false
	}
	if h, ok := resolveHandle(fields[1]); !ok || h != handle {
		return stmtCollectShape{}, false
	}
	return stmtCollectShape{handle: handle, column: column, listVar: listVar}, true
}

// notifyScriptProcs collects the user procedures an unlock-notify callback
// script invokes (notify1-7.4's `db2 unlock_notify unlock_notify` — the
// file-level `proc unlock_notify {}` is what fires): the harness loads them
// into the script interpreter.
func (i *Interp) notifyScriptProcs(script string) []Proc {
	var procs []Proc
	for _, word := range strings.Fields(script) {
		name := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(word), "$"), "::")
		proc, ok := i.procs[name]
		if !ok {
			continue
		}
		dup := false
		for _, p := range procs {
			if p.Name == name {
				dup = true
			}
		}
		if !dup {
			procs = append(procs, *proc)
		}
	}
	return procs
}

// preRegisterNotifyTargets seeds the tracked/dynamic variable sets with the
// variables an unlock_notify callback script in THIS body writes, BEFORE the
// body executes — notify1-6.2.1's `set unlocked 0` reset precedes the
// `db unlock_notify {set unlocked 1}` registration in the body, and the
// reset must still reach the harness as a runtime_expr.
func (i *Interp) preRegisterNotifyTargets(bodyText string) {
	for _, cmd := range parseCommands(bodyText) {
		var script string
		switch {
		case len(cmd) >= 2 && cmd[0].Text == "sqlite3_unlock_notify":
			// No script argument (the C callback evals the fixed proc).
			continue
		case len(cmd) >= 3 && cmd[1].Text == "unlock_notify":
			script = cmd[2].Text
		case len(cmd) >= 3 && cmd[0].Text == "unlock_notify":
			script = cmd[2].Text
		default:
			continue
		}
		if i.trackedVars == nil {
			i.trackedVars = make(map[string]bool)
		}
		if i.dynamicVars == nil {
			i.dynamicVars = make(map[string]bool)
		}
		for _, name := range NotifyScriptVarTargets(script) {
			i.trackedVars[name] = true
			i.dynamicVars[name] = true
		}
	}
}

// commandTestVFS records `testvfs <name>` — the named testvfs wrapper VFS
// (test_vfs.c). The object's option calls (`<name> script <proc>`,
// `<name> filter <methods>`, `<name> devchar <chars>`, ...) dispatch through
// the named-object fallthrough into commandTestVFSConfig.
func commandTestVFS(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	name := args[0]
	if i.testvfsNames == nil {
		i.testvfsNames = make(map[string]bool)
	}
	i.testvfsNames[name] = true
	// `-default 1` makes the wrapper the process DEFAULT VFS: every
	// connection opened afterwards (db and the named handles) routes its
	// I/O through it (pagerfault's faultsim fixtures).
	option := ""
	for k := 1; k+1 < len(args); k += 2 {
		if args[k] == "-default" && strings.TrimSpace(args[k+1]) != "0" {
			option = "default"
		}
	}
	i.stmts = append(i.stmts, Stmt{Type: "register_testvfs", Variable: name, SQL: option, TestName: i.curTest})
	return nil
}

// testvfsOptionArity reports the argument count each testvfs option takes
// (false = the option is not recognized here).
func testvfsOptionArity(opt string) (int, bool) {
	switch opt {
	case "script", "filter", "devchar", "sectorsize", "ioerr", "fullerr", "cantopenerr", "shm", "delete":
		return 1, true
	}
	return 0, false
}

// commandTestVFSConfig records a testvfs object option call
// (`tvfs script xSync`, `tvfs devchar sequential`, `tvfs filter xSync`):
// SQL carries the option, FunctionProc the script proc (option "script"),
// Variable the option's value.
func commandTestVFSConfig(i *Interp, args []string, cmd string) error {
	if len(args) < 2 {
		return nil
	}
	step := Stmt{Type: "testvfs_config", Database: cmd, SQL: args[0], Variable: args[1], TestName: i.curTest}
	if args[0] == "script" {
		step.FunctionProc = args[1]
		// The script proc's body travels with the step (the harness loads
		// it into the fixture's callback interpreter).
		if proc, ok := i.procs[strings.TrimPrefix(args[1], "::")]; ok {
			step.CallbackProcs = []Proc{*proc}
			i.seedCallbackTargets(proc.Body)
		}
		if i.testvfsScriptProcs == nil {
			i.testvfsScriptProcs = make(map[string]bool)
		}
		i.testvfsScriptProcs[strings.TrimPrefix(args[1], "::")] = true
	}
	i.stmts = append(i.stmts, step)
	return nil
}

// seedCallbackTargets marks the variables a callback script writes as
// runtime-tracked globals: later `set NAME <value>` resets (the file-level
// `set ::sync_count 0` between walvfs tests) record runtime_expr steps.
func (i *Interp) seedCallbackTargets(body string) {
	if i.trackedVars == nil {
		i.trackedVars = make(map[string]bool)
	}
	if i.dynamicVars == nil {
		i.dynamicVars = make(map[string]bool)
	}
	for _, name := range NotifyScriptVarTargets(body) {
		i.trackedVars[name] = true
		i.dynamicVars[name] = true
	}
}

// AttachLateCallbackProcs post-processes the captured statements: steps that
// NAME a proc before the file defines it (TCL late binding — walvfs's
// `tvfs script xSync` precedes `proc xSync {...}`) carry the proc body from
// here. The interpreter is done executing, so i.procs holds every
// file-level definition.
func (i *Interp) AttachLateCallbackProcs() {
	for idx := range i.stmts {
		s := &i.stmts[idx]
		if s.FunctionProc == "" || len(s.CallbackProcs) > 0 {
			continue
		}
		if proc, ok := i.procs[strings.TrimPrefix(s.FunctionProc, "::")]; ok {
			s.CallbackProcs = []Proc{*proc}
		}
	}
}

// PreScanCallbackTargets seeds the callback write-target sets from the whole
// source BEFORE execution: a file-level `set ::cnt 2` reset can precede BOTH
// the `tvfs script xWrite` binding and the `proc xWrite {...}` definition
// (walvfs-3.x), so neither in-flight seeding point sees it.
func (i *Interp) PreScanCallbackTargets(src string) {
	bodies := make(map[string]string)
	for _, cmd := range parseCommands(src) {
		if len(cmd) >= 4 && cmd[0].Text == "proc" {
			bodies[strings.TrimPrefix(cmd[1].Text, "::")] = cmd[3].Text
		}
	}
	for _, cmd := range parseCommands(src) {
		if len(cmd) >= 3 && cmd[1].Text == "script" {
			if body, ok := bodies[strings.TrimPrefix(cmd[2].Text, "::")]; ok {
				i.seedCallbackTargets(body)
			}
		}
		// `db busy [list busy_handler $mode $bm]` — the busy-handler proc is
		// a callback writer too (e_walckpt's busy_handler_counter: the
		// `set ::busy_handler_counter 0` reset can precede nothing but the
		// binding, and the proc body is defined before it).
		if len(cmd) >= 3 && cmd[0].Text == "db" && cmd[1].Text == "busy" {
			if procName := busyHandlerProcName(cmd[2].Text); procName != "" {
				if body, ok := bodies[procName]; ok {
					i.seedCallbackTargets(body)
				}
			}
		}
	}
}

// busyHandlerProcName extracts the proc name from a `db busy <script>` word:
// the script is `[list <proc> <fixed args>]` — the first word after "list".
func busyHandlerProcName(script string) string {
	fields := strings.Fields(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(script), "["), "]")))
	if len(fields) >= 2 && fields[0] == "list" {
		return strings.TrimPrefix(fields[1], "::")
	}
	if len(fields) >= 1 && fields[0] != "list" {
		return strings.TrimPrefix(fields[0], "::")
	}
	return ""
}

// faultsimCaptureScript executes script and returns the statements it
// captured (pulled out of the main stream): the faultsim -prep/-body bodies
// become the step trees the harness replays per trial.
func (i *Interp) faultsimCaptureScript(script string, localVars map[string]string) []Stmt {
	mark := len(i.stmts)
	i.execScript(script, localVars)
	captured := append([]Stmt(nil), i.stmts[mark:]...)
	i.stmts = i.stmts[:mark]
	return captured
}

// faultsimTestPatterns extracts the (rc, result) pattern list from the -test
// script: the `faultsim_test_result <list>...` call's arguments (each a
// braced two-element list). The class's own injected-error list is appended
// by the harness (faultsim_test_result_int prepends -injecterrlist).
func faultsimTestPatterns(testScript string) []string {
	for _, cmd := range parseCommands(testScript) {
		if len(cmd) >= 2 && cmd[0].Text == "faultsim_test_result" {
			patterns := make([]string, 0, len(cmd)-1)
			for _, w := range cmd[1:] {
				patterns = append(patterns, strings.TrimSpace(w.Text))
			}
			return patterns
		}
	}
	return nil
}

// commandDoFaultsimTest records do_faultsim_test NAME ?-faults LIST?
// -prep {..} -body {..} -test {..} (malloc_common.tcl): ONE dynamic step —
// the harness owns the trial loop (the trial count is a runtime property:
// stop at the first trial whose injected-fault count is zero).
func commandDoFaultsimTest(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	name := args[0]
	faults := "ioerr-transient ioerr-persistent full"
	prepRaw, bodyRaw, testRaw := "", "", ""
	// args excludes the command name: the option sits at args[k] (rawWords
	// [k+1]) and its braced value at args[k+1] (rawWords[k+2]).
	for k := 1; k+1 < len(args); k += 2 {
		switch args[k] {
		case "-faults":
			faults = args[k+1]
		case "-prep":
			prepRaw = rawWords[k+2].Text
		case "-body":
			bodyRaw = rawWords[k+2].Text
		case "-test":
			testRaw = rawWords[k+2].Text
		}
	}
	previous := i.curTest
	i.curTest = name
	prep := i.faultsimCaptureScript(prepRaw, localVars)
	body := i.faultsimCaptureScript(bodyRaw, localVars)
	i.curTest = previous
	i.stmts = append(i.stmts, Stmt{
		Type:         "faultsim_test",
		Variable:     name,
		SQL:          faults,
		Prep:         prep,
		Body:         body,
		TestScript:   testRaw,
		TestPatterns: faultsimTestPatterns(testRaw),
		TestName:     name,
	})
	return nil
}

// commandFaultsimSave records the faultsim snapshot steps: the file pair
// copies of test.db* (tester.tcl's db_save/db_restore family).
func commandFaultsimSave(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	step := Stmt{Type: "faultsim_save", TestName: i.curTest}
	i.stmts = append(i.stmts, step)
	return nil
}

func commandFaultsimSaveClose(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "faultsim_save_close", TestName: i.curTest})
	i.vars[""] = ""
	return nil
}

func commandFaultsimRestore(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "faultsim_restore", TestName: i.curTest})
	return nil
}

func commandFaultsimRestoreReopen(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	i.stmts = append(i.stmts, Stmt{Type: "faultsim_restore_reopen", TestName: i.curTest})
	return nil
}
