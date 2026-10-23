// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// harnessTclCollation is a collation sequence implemented by a Tcl proc.
//
// The testfixture registers custom collations with `db collation <name>
// <proc>`; the proc compares two strings and answers -1, 0 or 1. The proc
// body travels with the fixture step, so the harness evaluates it with the
// converter's Tcl interpreter — the same interpreter the authorizer and the
// virtual-table callbacks use.
//
// A proc that fails answers the BINARY comparison: the collation still
// orders the rows, and the test's own expectation reports the difference.
type harnessTclCollation struct {
	interp *tcl.Interp
	proc   string
}

// newHarnessTclCollation builds a collation that calls the named proc.
func newHarnessTclCollation(step TestStep) *harnessTclCollation {
	runtime := &harnessTclCollation{interp: tcl.NewInterp(), proc: step.CollationProc}
	if step.CollationBody != "" {
		runtime.interp.LoadProcedures([]tcl.Proc{{
			Name: step.CollationProc,
			Args: step.FunctionArgs,
			Body: step.CollationBody,
		}})
	}
	for name, value := range step.CallbackVars {
		runtime.interp.SetVariable(name, value)
	}
	return runtime
}

// Compare implements the collation sequence: it calls the Tcl proc with the
// two strings and maps its answer onto a comparison result.
func (c *harnessTclCollation) Compare(a, b string) int {
	if c == nil || c.proc == "" {
		return strings.Compare(a, b)
	}
	answer, err := c.interp.InvokeProcedure(c.proc, a, b)
	if err != nil {
		return strings.Compare(a, b)
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil {
		return strings.Compare(a, b)
	}
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
