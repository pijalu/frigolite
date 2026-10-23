// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
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
}

// newHarnessTclFunction builds a SQL function that calls the named proc.
func newHarnessTclFunction(step TestStep) *harnessTclFunction {
	runtime := &harnessTclFunction{interp: tcl.NewInterp(), proc: step.FunctionProc}
	if step.FunctionBody != "" {
		runtime.interp.LoadProcedures([]tcl.Proc{{
			Name: step.FunctionProc,
			Args: step.FunctionArgs,
			Body: step.FunctionBody,
		}})
	}
	for name, value := range step.CallbackVars {
		runtime.interp.SetVariable(name, value)
	}
	return runtime
}

// Call implements the SQL function: it calls the Tcl proc with the arguments
// rendered as text.
func (f *harnessTclFunction) Call(args []interface{}) (interface{}, error) {
	if f == nil || f.proc == "" {
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
			values[i] = strings.TrimSpace(formatSQLiteValue(v))
		}
	}
	answer, err := f.interp.InvokeProcedure(f.proc, values...)
	if err != nil {
		// The function may map onto a Tcl *command* rather than a user
		// procedure ("db func lindex lindex").
		answer, err = f.interp.InvokeCommand(f.proc, values...)
		if err != nil {
			return nil, err
		}
	}
	return answer, nil
}
