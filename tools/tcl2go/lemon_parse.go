// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
	"github.com/pijalu/frigolite/tools/tclconvert/tcl/tclparser"
)

// parseCommands parses TCL source text using the go-lemon generated LALR(1)
// parser (tclparser.ParseCommands, generated from tcl_grammar.y) and converts
// the result into the tcl package's RawWord type.
//
// tclparser.RawWord and tcl.RawWord are structurally identical (same fields:
// Text, Braced, Quoted), so this is a field-by-field copy. Keeping the two
// types distinct avoids making the go-lemon parser package depend on the
// hand-written parser it replaces.
func parseCommands(src string) [][]tcl.RawWord {
	cmds := tclparser.ParseCommands(src)
	if len(cmds) == 0 {
		// Preserve the hand-written parser's nil semantics for zero commands:
		// callers use bodyCmds != nil to distinguish "no body" from "empty
		// body", so an empty (non-nil) slice would change generated output.
		return nil
	}
	out := make([][]tcl.RawWord, len(cmds))
	for i, cmd := range cmds {
		out[i] = make([]tcl.RawWord, len(cmd))
		for j, w := range cmd {
			out[i][j] = tcl.RawWord{Text: w.Text, Braced: w.Braced, Quoted: w.Quoted}
		}
	}
	return out
}
