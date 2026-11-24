// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "strings"

// runtimeRegexpListExpr recognizes the do_test body shape where a runtime
// capture is probed by several Tcl `regexp` command substitutions and
// collected with lappend (misc3-6.11-utf8). It returns a runtime
// `list [regexp ...] ...` expression, or "" when the body is not this shape.
func (i *Interp) runtimeRegexpListExpr(body [][]rawWord) string {
	var cmds []string
	for _, cmd := range body {
		for _, w := range cmd {
			if w.Braced {
				continue // a braced literal cannot contain a substitution
			}
			cmds = append(cmds, bracketedRegexpCommands(w.Text)...)
		}
	}
	if len(cmds) == 0 {
		return ""
	}
	return "list [" + strings.Join(cmds, "] [") + "]"
}

// bracketedRegexpCommands extracts `[regexp ...]` substitutions whose last
// word is a variable reference (a probe over a runtime-captured value).
func bracketedRegexpCommands(text string) []string {
	var out []string
	for {
		start := strings.Index(text, "[regexp ")
		if start < 0 {
			return out
		}
		end := strings.IndexByte(text[start:], ']')
		if end < 0 {
			return out
		}
		inner := text[start+1 : start+end]
		if strings.Contains(inner, "$") {
			out = append(out, inner)
		}
		text = text[start+end+1:]
	}
}
