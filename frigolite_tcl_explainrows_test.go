// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"fmt"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// explain-row callbacks (index9's sqluses proc): `db eval "EXPLAIN $sql" a
// {body}` drives a per-row Tcl callback over the EXPLAIN row stream — the
// array elements a(addr), a(opcode), a(p1)..a(p5), a(comment) bind each row's
// columns, the body's writes go to fixture globals (`set ::T($a(p2)) 1`), and
// those globals round trip back into the harness runtime set (the S25
// push/pull over a fresh converter interpreter).

// runExplainRowsCallback executes step's per-row Tcl body over res's rows:
// a fresh interpreter per step, present runtime targets pushed before the
// walk, written globals (plain and array-element keys) pulled after.
func runExplainRowsCallback(step TestStep, res *Result, runtimeVars map[string]string) error {
	interp := tcl.NewInterp()
	targets := tcl.NotifyScriptVarTargets(step.ExplainBody)
	if len(targets) == 0 {
		return fmt.Errorf("explain_rows body writes no fixture globals")
	}
	arrayTargets := make(map[string]bool)
	plainTargets := make([]string, 0, len(targets))
	for _, name := range targets {
		if arr := tclArrayWriteTarget(name); arr != "" {
			arrayTargets[arr] = true
			continue
		}
		plainTargets = append(plainTargets, name)
	}
	// The C proc's `array unset ::T` runs before every walk (sqluses resets
	// its collection per call): clear the array targets' runtime keys.
	for arr := range arrayTargets {
		prefix := arr + "("
		for key := range runtimeVars {
			if len(key) > len(prefix) && key[:len(prefix)] == prefix && key[len(key)-1] == ')' {
				delete(runtimeVars, key)
			}
		}
	}
	// Push present runtime values before the walk (push-present-only: a
	// runtime_var_reset between leaves must land in the interp).
	for _, name := range plainTargets {
		if v, ok := runtimeVars[name]; ok {
			interp.SetVariable(name, v)
		}
	}
	for arr := range arrayTargets {
		prefix := arr + "("
		for key, v := range runtimeVars {
			if len(key) > len(prefix) && key[:len(prefix)] == prefix && key[len(key)-1] == ')' {
				interp.SetVariable(key, v)
			}
		}
	}
	// The EXPLAIN result columns: addr, opcode, p1, p2, p3, p4, p5, comment.
	col := map[string]int{}
	for i, c := range res.Columns {
		col[c] = i
	}
	for _, row := range res.Rows {
		for _, cname := range []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment"} {
			idx, ok := col[cname]
			value := ""
			if ok && idx < len(row) && row[idx] != nil {
				value = formatHarnessValue("", row[idx])
			}
			interp.SetVariable(step.Variable+"("+cname+")", value)
		}
		if err := interp.Execute(step.ExplainBody); err != nil {
			return fmt.Errorf("body: %v", err)
		}
	}
	// Pull the written globals back: plain targets directly, array targets
	// through their full key set (the body wrote set ::T($a(p2)) 1 — the
	// interp holds T(2), T(3), ...).
	for _, name := range plainTargets {
		if v, ok := interp.Variable(name); ok {
			runtimeVars[name] = v
		} else if v, ok := interp.Variable("::" + name); ok {
			runtimeVars[name] = v
		}
	}
	for arr := range arrayTargets {
		for _, key := range interp.ArrayKeys(arr) {
			full := arr + "(" + key + ")"
			if v, ok := interp.Variable(full); ok {
				runtimeVars[full] = v
			}
		}
	}
	return nil
}

// tclArrayWriteTarget reports the array name when a NotifyScriptVarTargets
// entry is an array-element write ("T($a(p2))" — the target carries the
// literal parenthesized suffix), or "" for a plain variable.
func tclArrayWriteTarget(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '(' {
			return name[:i]
		}
	}
	return ""
}
