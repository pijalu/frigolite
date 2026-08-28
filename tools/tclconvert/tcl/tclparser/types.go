// SPDX-License-Identifier: GPL-3.0-or-later
package tclparser

// RawWord represents one word in a TCL command before substitution.
// It matches the type in the parent tcl package exactly (same fields),
// allowing conversion via cast.
type RawWord struct {
	Text   string // raw content
	Braced bool   // true if word was { ... } quoted (literal, no substitution)
	Quoted bool   // true if word was " ... " quoted (substitution applies)
}

// TclResult holds the output of the TCL parser.
// Name intentionally avoids conflict with engine.ParseResult.
type TclResult struct {
	Commands [][]RawWord
}
