// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// harnessTclAuthorizer runs a Tcl authorizer proc for every action.
//
// The testfixture's `db auth <proc>` installs a Tcl callback that receives
// (action, arg1, arg2, arg3, arg4) and answers SQLITE_OK, SQLITE_DENY or
// SQLITE_IGNORE (altercons2.test's xAuth denies SQLITE_ALTER_TABLE).
//
// The proc body travels with the fixture step, so the harness evaluates it
// with the converter's Tcl interpreter — the same interpreter the vtab
// callbacks use (see frigolite_tcl_vtab_callback_test.go).
type harnessTclAuthorizer struct {
	interp *tcl.Interp
	proc   string
}

// newHarnessTclAuthorizer builds an authorizer that calls the named proc.
// The proc is loaded from the step's callback procedures.
func newHarnessTclAuthorizer(step TestStep) *harnessTclAuthorizer {
	runtime := &harnessTclAuthorizer{interp: tcl.NewInterp(), proc: step.AuthProc}
	procs := make([]tcl.Proc, 0, len(step.CallbackProcs))
	for _, proc := range step.CallbackProcs {
		procs = append(procs, tcl.Proc{Name: proc.Name, Args: proc.Args, Body: proc.Body})
	}
	runtime.interp.LoadProcedures(procs)
	for name, value := range step.CallbackVars {
		runtime.interp.SetVariable(name, value)
	}
	return runtime
}

// Authorize implements auth.Authorizer: it calls the Tcl proc and maps its
// answer onto the author's result.
func (a *harnessTclAuthorizer) Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {
	if a == nil || a.proc == "" {
		return auth.ResultOK
	}
	answer, err := a.interp.InvokeProcedure(a.proc, action.String(), arg1, arg2, arg3, arg4)
	if err != nil {
		// A failing authorizer denies the operation (SQLite reports the
		// error from the callback as "not authorized").
		return auth.ResultDeny
	}
	switch strings.ToUpper(strings.TrimSpace(answer)) {
	case "SQLITE_DENY":
		return auth.ResultDeny
	case "SQLITE_IGNORE":
		return auth.ResultIgnore
	default:
		return auth.ResultOK
	}
}
