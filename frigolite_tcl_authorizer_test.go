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
// callbacks use (see frigolite_tcl_vtab_callback_test.go). Recording procs
// append each event to a fixture global (`lappend ::authdata [lrange $args
// 0 4]`, savepoint.test:564) and a later runtime_var step asserts the
// collected sequence — the same interpreter-to-runtime round trip the S25
// UDF-global fix built for wherelimit2's log UDF: push the runtime values
// into the interp before each call, pull the written values back after.
type harnessTclAuthorizer struct {
	interp      *tcl.Interp
	proc        string
	runtimeVars map[string]string
	targets     []string
}

// newHarnessTclAuthorizer builds an authorizer that calls the named proc.
// The proc is loaded from the step's callback procedures. runtimeVars is
// the harness runtime set the recorded globals sync into (nil = no sync).
func newHarnessTclAuthorizer(step TestStep, runtimeVars map[string]string) *harnessTclAuthorizer {
	runtime := &harnessTclAuthorizer{interp: tcl.NewInterp(), proc: step.AuthProc, runtimeVars: runtimeVars}
	procs := make([]tcl.Proc, 0, len(step.CallbackProcs))
	for _, proc := range step.CallbackProcs {
		procs = append(procs, tcl.Proc{Name: proc.Name, Args: proc.Args, Body: proc.Body})
		targets := tcl.NotifyScriptVarTargets(proc.Body)
		for _, name := range targets {
			if !containsString(runtime.targets, name) {
				runtime.targets = append(runtime.targets, name)
			}
		}
	}
	runtime.interp.LoadProcedures(procs)
	for name, value := range step.CallbackVars {
		runtime.interp.SetVariable(name, value)
	}
	return runtime
}

// containsString reports whether list holds value.
func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// Authorize implements auth.Authorizer: it calls the Tcl proc and maps its
// answer onto the author's result. Before the call the recorded globals'
// runtime values are pushed into the interp (a runtime_var_reset on the
// shared variable must land here too — push-present-only per the S25
// lesson); after it the proc's writes are pulled back into the runtime set.
func (a *harnessTclAuthorizer) Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {
	if a == nil || a.proc == "" {
		return auth.ResultOK
	}
	a.pushRuntimeVars()
	answer, err := a.interp.InvokeProcedure(a.proc, action.String(), arg1, arg2, arg3, arg4)
	a.pullRuntimeVars()
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

// pushRuntimeVars seeds the interp's recorded globals from the runtime set.
func (a *harnessTclAuthorizer) pushRuntimeVars() {
	if a.runtimeVars == nil {
		return
	}
	for _, name := range a.targets {
		if v, ok := a.runtimeVars[name]; ok {
			a.interp.SetVariable(name, v)
		}
	}
}

// pullRuntimeVars copies the interp's recorded globals back into the
// runtime set (the value the fixture's runtime_var step compares).
func (a *harnessTclAuthorizer) pullRuntimeVars() {
	if a.runtimeVars == nil {
		return
	}
	for _, name := range a.targets {
		if v, ok := a.interp.Variable(name); ok {
			a.runtimeVars[name] = v
		} else if v, ok := a.interp.Variable("::" + name); ok {
			a.runtimeVars[name] = v
		}
	}
}
