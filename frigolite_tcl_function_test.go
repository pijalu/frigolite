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
}

// newHarnessTclFunction builds a SQL function that calls the named proc.
// A registration whose callback is a Tcl *script* ("db func cross {joinx
// cross}" — func8.test) loads the procedures the script invokes and runs
// the script with the SQL arguments appended as extra command words, the
// way tclsqlite does.
func newHarnessTclFunction(step TestStep, runtimeVars map[string]string) *harnessTclFunction {
	runtime := &harnessTclFunction{interp: tcl.NewInterp(), proc: step.FunctionProc, script: step.FunctionScript}
	procs := step.CallbackProcs
	if step.FunctionBody != "" {
		procs = append(procs, callbackProc{Name: step.FunctionProc, Args: step.FunctionArgs, Body: step.FunctionBody})
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
	if f.script != "" {
		// tclsqlite's script callback: the SQL arguments are appended to the
		// script as extra command words ("joinx cross" + (1..7) invokes
		// `joinx cross 1 2 3 4 5 6 7`).
		words := strings.Fields(f.script)
		if len(words) == 0 {
			return nil, nil
		}
		answer, err := f.interp.InvokeCommand(words[0], append(words[1:], values...)...)
		if err != nil {
			return nil, err
		}
		return answer, nil
	}
	if f.interp.HasProcedure(f.proc) {
		answer, err := f.interp.InvokeProcedure(f.proc, values...)
		if err != nil {
			return nil, err
		}
		return answer, nil
	}
	if f.interp.HasCommand(f.proc) {
		// The function may map onto a Tcl *command* rather than a user
		// procedure ("db func lindex lindex").
		answer, err := f.interp.InvokeCommand(f.proc, values...)
		if err != nil {
			return nil, err
		}
		return answer, nil
	}
	return nil, fmt.Errorf("invalid command name %q", f.proc)
}
