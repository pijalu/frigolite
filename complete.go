// Package frigolite provides sqlite3_complete emulation.
package frigolite

// Token classes for the sqlite3_complete state machine (src/complete.c).
const (
	tkSEMI uint8 = iota
	tkWS
	tkOTHER
	tkEXPLAIN
	tkCREATE
	tkTEMP
	tkTRIGGER
	tkEND
)

// trans is the 8x8 transition table from src/complete.c.
var trans = [8][8]uint8{
	{1, 0, 2, 3, 4, 2, 2, 2},
	{1, 1, 2, 3, 4, 2, 2, 2},
	{1, 2, 2, 2, 2, 2, 2, 2},
	{1, 3, 3, 2, 4, 2, 2, 2},
	{1, 4, 2, 2, 2, 4, 5, 2},
	{6, 5, 5, 5, 5, 5, 5, 5},
	{6, 6, 5, 5, 5, 5, 5, 7},
	{1, 7, 5, 5, 5, 5, 5, 5},
}

// sqliteComplete reports whether sql ends in a complete statement.
// Faithful port of src/complete.c sqlite3_complete (state machine with
// trigger-aware ";END;" detection and full tokenisation).
func sqliteComplete(sql string) bool {
	state := uint8(0)
	for i := 0; i < len(sql); i++ {
		next, token, done := nextToken(sql, i)
		if done {
			return false // unterminated comment/bracket/quote: incomplete
		}
		if token == tkWS && next == -1 {
			// Line comment ran to EOF; complete iff in "idle after ;" state.
			return state == 1
		}
		i = next
		state = trans[state][token]
	}
	return state == 1
}

// nextToken classifies the token starting at i. It returns the index of the
// token's last consumed byte, its class, and done=true when an unterminated
// block comment / bracket / quoted string means the statement is incomplete.
// A line comment running to end of input returns (next=-1, tkWS).
func nextToken(sql string, i int) (int, uint8, bool) {
	switch c := sql[i]; c {
	case ';':
		return i, tkSEMI, false
	case ' ', '\r', '\t', '\n', '\f':
		return i, tkWS, false
	case '/':
		return nextSlashToken(sql, i)
	case '-':
		return nextDashToken(sql, i)
	case '[':
		j, ok := scanUntil(sql, i+1, ']')
		return j, tkOTHER, !ok
	case '`', '"', '\'':
		j, ok := scanUntil(sql, i+1, c)
		return j, tkOTHER, !ok
	default:
		if isIDChar(c) {
			j := scanIDTail(sql, i)
			return j - 1, classifyKeyword(sql[i:j]), false
		}
		return i, tkOTHER, false
	}
}

// nextSlashToken handles '/' at pos: a block comment (tkWS) or tkOTHER.
func nextSlashToken(sql string, pos int) (int, uint8, bool) {
	if pos+1 < len(sql) && sql[pos+1] == '*' {
		j, ok := scanBlockComment(sql, pos)
		return j, tkWS, !ok
	}
	return pos, tkOTHER, false
}

// nextDashToken handles '-' at pos: a line comment (tkWS, or -1 at EOF) or
// tkOTHER.
func nextDashToken(sql string, pos int) (int, uint8, bool) {
	if pos+1 < len(sql) && sql[pos+1] == '-' {
		for pos < len(sql) && sql[pos] != '\n' {
			pos++
		}
		if pos >= len(sql) {
			return -1, tkWS, false
		}
		return pos, tkWS, false
	}
	return pos, tkOTHER, false
}

// scanBlockComment consumes a C-style comment starting at the '/' at pos.
// Returns the index of the closing '/' and true when terminated.
func scanBlockComment(sql string, pos int) (int, bool) {
	i := pos + 2
	for i < len(sql) && !(sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/') {
		i++
	}
	if i >= len(sql) {
		return i, false
	}
	return i + 1, true
}

// scanUntil scans from start until the terminator byte. Returns the index of
// the terminator and true when found before end of input.
func scanUntil(sql string, start int, term byte) (int, bool) {
	i := start
	for i < len(sql) && sql[i] != term {
		i++
	}
	if i >= len(sql) {
		return i, false
	}
	return i, true
}

// scanIDTail returns the exclusive end index of the identifier starting at i.
func scanIDTail(sql string, i int) int {
	j := i + 1
	for j < len(sql) && isIDChar(sql[j]) {
		j++
	}
	return j
}

// classifyKeyword maps an identifier to its state-machine token class.
func classifyKeyword(id string) uint8 {
	n := len(id)
	switch id[0] | 0x20 {
	case 'c':
		return classifyCreateKeyword(id, n)
	case 't':
		return classifyTKeyword(id, n)
	case 'e':
		return classifyEKeyword(id, n)
	}
	return tkOTHER
}

// classifyCreateKeyword matches the CREATE keyword (tkCREATE).
func classifyCreateKeyword(id string, n int) uint8 {
	if n == 6 && equalFold(id, "create") {
		return tkCREATE
	}
	return tkOTHER
}

// classifyTKeyword matches TRIGGER/TEMP/TEMPORARY (tkTRIGGER/tkTEMP).
func classifyTKeyword(id string, n int) uint8 {
	switch {
	case n == 7 && equalFold(id, "trigger"):
		return tkTRIGGER
	case n == 4 && equalFold(id, "temp"), n == 9 && equalFold(id, "temporary"):
		return tkTEMP
	}
	return tkOTHER
}

// classifyEKeyword matches END/EXPLAIN (tkEND/tkEXPLAIN).
func classifyEKeyword(id string, n int) uint8 {
	switch {
	case n == 3 && equalFold(id, "end"):
		return tkEND
	case n == 7 && equalFold(id, "explain"):
		return tkEXPLAIN
	}
	return tkOTHER
}

func isIDChar(c byte) bool {
	if c >= '0' && c <= '9' {
		return true
	}
	if c >= 'A' && c <= 'Z' {
		return true
	}
	if c >= 'a' && c <= 'z' {
		return true
	}
	if c == '_' || c == '$' {
		return true
	}
	if c >= 0x80 {
		return true
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		cb := b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Complete reports whether sql is a complete SQL statement (sqlite3_complete).
func (db *DB) Complete(sql string) bool {
	return sqliteComplete(sql)
}

// Complete reports whether sql is a complete SQL statement (package-level helper).
func Complete(sql string) bool {
	return sqliteComplete(sql)
}
