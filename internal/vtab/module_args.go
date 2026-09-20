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
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			args = append(args, s)
		}
	}
	for i := 0; i < len(argsStr); i++ {
		c := argsStr[i]
		switch {
		case quote != 0:
			var open bool
			i, open = stepQuoted(&cur, argsStr, i, quote)
			if !open {
				quote = 0
			}
		case depth == 0 && c == ',':
			flush()
			cur.Reset()
		case c == '(':
			depth++
			cur.WriteByte(c)
		case c == ')':
			if depth--; depth < 0 {
				// Past the final close paren: stop.
				flush()
				return args
			}
			cur.WriteByte(c)
		default:
			if q, ok := quoteOpener(c); ok {
				quote = q
			}
			cur.WriteByte(c)
		}
	}
	flush()
	return args
}

// stepQuoted consumes one byte inside a quoted span: a doubled closing quote
// is an escaped quote inside the string; the span closes at a lone closing
// quote (SQLite quote rules). Returns the next index and whether the span is
// still open.
func stepQuoted(cur *strings.Builder, argsStr string, i int, quote byte) (int, bool) {
	c := argsStr[i]
	cur.WriteByte(c)
	if c != quote {
		return i, true
	}
	if i+1 < len(argsStr) && argsStr[i+1] == quote {
		cur.WriteByte(argsStr[i+1])
		return i + 1, true
	}
	return i, false
}

// quoteOpener reports the closing byte for a quote opener ('\”, '"', '`',
// '[' → ']').
func quoteOpener(c byte) (byte, bool) {
	switch c {
	case '\'', '"', '`':
		return c, true
	case '[':
		return ']', true
	}
	return 0, false
}
