// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// harnessTclFunction is a SQL scalar function implemented by a Tcl proc.
//
// The testfixture registers functions with `db function <name> <proc>`; the
// proc receives the SQL arguments and returns the result. The proc body
// travels with the fixture step, so the harness evaluates it with the
// converter's Tcl interpreter — the same interpreter the authorizer, the
// collations and the virtual-table callbacks use.
//
// A proc that fails answers NULL: the SQL function's result is undefined
// then, and the test's own expectation reports the difference.
type harnessTclFunction struct {
	interp *tcl.Interp
	proc   string
	script string // db func NAME {script}: the SQL arguments append to the script words
	// varTargets are the Tcl globals the callback body writes (`lappend
	// ::log {*}$args` — wherelimit2.test's log recorder). TCL globals are
	// shared state: the proc reads what the test script's `set ::log {}`
	// wrote and its appends must reach the later `set ::log` assertion, so
	// each call pushes the harness's current values in and pulls the
	// written values back (the rtgeom recorder's syncVars and the
	// unlock-notify eval_script sync do the same round trip).
	varTargets  []string
	runtimeVars map[string]string
}

// newHarnessTclFunction builds a SQL function that calls the named proc.
// A registration whose callback is a Tcl *script* ("db func cross {joinx
// cross}" — func8.test) loads the procedures the script invokes and runs
// the script with the SQL arguments appended as extra command words, the
// way tclsqlite does.
func newHarnessTclFunction(step TestStep, runtimeVars map[string]string) *harnessTclFunction {
	runtime := &harnessTclFunction{interp: tcl.NewInterp(), proc: step.FunctionProc, script: step.FunctionScript, runtimeVars: runtimeVars}
	procs := step.CallbackProcs
	if step.FunctionBody != "" {
		procs = append(procs, callbackProc{Name: step.FunctionProc, Args: step.FunctionArgs, Body: step.FunctionBody})
	}
	seen := make(map[string]bool)
	for _, proc := range procs {
		for _, name := range tcl.NotifyScriptVarTargets(proc.Body) {
			if !seen[name] {
				seen[name] = true
				runtime.varTargets = append(runtime.varTargets, name)
			}
		}
	}
	loaded := make([]tcl.Proc, 0, len(procs))
	for _, proc := range procs {
		loaded = append(loaded, tcl.Proc{Name: strings.TrimPrefix(proc.Name, "::"), Args: proc.Args, Body: proc.Body})
	}
	if len(loaded) > 0 {
		runtime.interp.LoadProcedures(loaded)
	}
	for name, value := range step.CallbackVars {
		runtime.interp.SetVariable(name, value)
	}
	// The snapshot's values may be stale: variables the file assigned at run
	// time (func6.test's `set F [loadhex test.db]`) live in the harness's
	// runtime variables — they override the conversion-time snapshot.
	for name, value := range runtimeVars {
		runtime.interp.SetVariable(name, value)
	}
	return runtime
}

// Call implements the SQL function: it calls the Tcl proc with the arguments
// rendered as text.
func (f *harnessTclFunction) Call(args []interface{}) (interface{}, error) {
	if f == nil || (f.proc == "" && f.script == "") {
		return nil, nil
	}
	values := make([]string, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case nil:
			values[i] = ""
		case string:
			values[i] = v
		case []byte:
			values[i] = string(v)
		default:
			values[i] = strings.TrimSpace(formatSQLiteValue("", v))
		}
	}
	f.pushVars()
	answer, err := f.invoke(values)
	f.pullVars()
	return answer, err
}

// invoke runs the proc (or script callback) with the SQL arguments appended.
func (f *harnessTclFunction) invoke(values []string) (string, error) {
	if f.script != "" {
		// tclsqlite's script callback: the SQL arguments are appended to the
		// script as extra command words ("joinx cross" + (1..7) invokes
		// `joinx cross 1 2 3 4 5 6 7`).
		words := strings.Fields(f.script)
		if len(words) == 0 {
			return "", nil
		}
		return f.interp.InvokeCommand(words[0], append(words[1:], values...)...)
	}
	if f.interp.HasProcedure(f.proc) {
		return f.interp.InvokeProcedure(f.proc, values...)
	}
	if f.interp.HasCommand(f.proc) {
		// The function may map onto a Tcl *command* rather than a user
		// procedure ("db func lindex lindex").
		return f.interp.InvokeCommand(f.proc, values...)
	}
	return "", fmt.Errorf("invalid command name %q", f.proc)
}

// pushVars copies each variable the callback body writes from the harness's
// runtime set into the interp before the call: the Tcl global the proc
// appends to is SHARED state, so a `runtime_var_reset` between calls
// (wherelimit2's `set ::log {}`) must clear what earlier calls appended.
// A variable absent from the runtime set is not the test's shared state and
// stays untouched (present-and-empty values are pushed intentionally).
func (f *harnessTclFunction) pushVars() {
	for _, name := range f.varTargets {
		if value, ok := f.runtimeVars[name]; ok {
			f.interp.SetVariable(name, value)
		}
	}
}

// pullVars copies the callback-written variables' current interp values back
// into the runtime set, so the later `set ::log` runtime_var assertion (the
// rtgeom recorder's syncVars and the unlock-notify eval_script sync do the
// same round trip) sees what this call appended.
func (f *harnessTclFunction) pullVars() {
	for _, name := range f.varTargets {
		if value, ok := f.interp.Variable(name); ok {
			f.runtimeVars[name] = value
		}
	}
}
