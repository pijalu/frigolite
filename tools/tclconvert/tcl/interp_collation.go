// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"regexp"
	"strings"
)

// cloneTclVars returns a snapshot copy of the interpreter's global variables.
func cloneTclVars(vars map[string]string) map[string]string {
	copy := make(map[string]string, len(vars))
	for name, value := range vars {
		copy[name] = value
	}
	return copy
}

// tclCallbackVarWrite matches global variable writes inside callback bodies:
// `lappend ::x`, `set ::x ...`, `incr ::x`, `append ::x`.
var tclCallbackVarWrite = regexp.MustCompile(`(?m)\b(?:lappend|set|incr|append)\s+::([A-Za-z_][A-Za-z0-9_]*)`)

// trackCallbackVars records globals whose values are produced by runtime
// callbacks: variables captured with a registration (the harness seeds them
// from CallbackVars) and globals written by a callback body. A later
// `do_test ... {set name}` for one of these is a runtime assertion, so it is
// captured as a runtime_var step even for a bare name (e_expr-15.1.2:
// `set likeargs` after the like() override recorded its arguments).
func (i *Interp) trackCallbackVars(callbackVars map[string]string, bodies ...string) {
	for name := range callbackVars {
		if name = strings.TrimPrefix(name, "::"); name != "" {
			i.trackedVars[name] = true
		}
	}
	for _, body := range bodies {
		for _, m := range tclCallbackVarWrite.FindAllStringSubmatch(body, -1) {
			i.trackedVars[strings.TrimPrefix(m[1], "::")] = true
		}
	}
}

func commandCreateCollation(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	if len(args) < 4 {
		return nil
	}
	procName := strings.TrimPrefix(strings.TrimSpace(args[2]), "::")
	marker := Stmt{Type: "register_collation", Database: args[0], CollationName: args[1], CollationProc: procName, DestructorBody: args[3], CallbackVars: cloneTclVars(i.vars), TestName: i.curTest}
	if proc, ok := i.procs[procName]; ok {
		marker.CollationBody = proc.Body
		marker.FunctionArgs = append([]string(nil), proc.Args...)
	}
	i.trackCallbackVars(marker.CallbackVars, marker.CollationBody, marker.DestructorBody)
	i.stmts = append(i.stmts, marker)
	return nil
}

func commandDeleteCollation(i *Interp, rawWords []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	i.stmts = append(i.stmts, Stmt{Type: "delete_collation", Database: args[0], CollationName: args[1], TestName: i.curTest})
	return nil
}
