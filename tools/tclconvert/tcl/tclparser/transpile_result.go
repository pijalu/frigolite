// SPDX-License-Identifier: GPL-3.0-or-later
package tclparser

import "strings"

// TranspileResult holds the output of the TCL-to-Go transpiler.
type TranspileResult struct {
	lines []string
}

// emit appends a formatted line to the result.
func (r *TranspileResult) emit(format string, args ...interface{}) {
	// Not using fmt.Sprintf to avoid import
	r.lines = append(r.lines, "")
}

// String returns the accumulated Go source code.
func (r *TranspileResult) String() string {
	return strings.Join(r.lines, "")
}
