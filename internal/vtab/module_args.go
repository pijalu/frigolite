package vtab

import "strings"

// SplitModuleArgs splits a virtual-table module argument list (the text
// between the outer parentheses of "CREATE VIRTUAL TABLE ... USING
// module(args)") into its individual arguments. Commas inside quoted strings
// ('...', "...", `...`, [...]) or inside nested parentheses do not separate
// arguments; each argument is TrimSpace'd verbatim text (SQLite tokenizes the
// CREATE VIRTUAL TABLE argument list before handing argv to xCreate). This
// matters for FTS4 options whose values contain commas, e.g. prefix='1,3,6'
// or notindexed=a,b.
//
// CREATE-time and re-instantiation-from-schema-SQL callers MUST share this
// one implementation: a module's argv at scan time must be byte-identical to
// its argv at CREATE time (a leading space defeats later dequoting —
// swarmvtab.test 3.3.2's missing='fetch_db' became unresolvable).
func SplitModuleArgs(argsStr string) []string {
	var args []string
	var cur strings.Builder
	depth := 0
	var quote byte
	for i := 0; i < len(argsStr); i++ {
		c := argsStr[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				// Doubled quote is an escaped quote inside the string; only
				// close when not doubled (SQLite quote rules).
				if i+1 < len(argsStr) && argsStr[i+1] == quote {
					cur.WriteByte(argsStr[i+1])
					i++
				} else {
					quote = 0
				}
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
			cur.WriteByte(c)
		case c == '[':
			quote = ']'
			cur.WriteByte(c)
		case c == '(':
			depth++
			cur.WriteByte(c)
		case c == ')':
			depth--
			if depth < 0 {
				// Past the final close paren: stop.
				if s := strings.TrimSpace(cur.String()); s != "" {
					args = append(args, s)
				}
				return args
			}
			cur.WriteByte(c)
		case c == ',' && depth == 0:
			if s := strings.TrimSpace(cur.String()); s != "" {
				args = append(args, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		args = append(args, s)
	}
	return args
}
