package fts5

import (
	"fmt"
	"strconv"
	"strings"
)

// This file ports fts5_config.c's rank-specification parsing
// (sqlite3Fts5ConfigParseRank: `bareword ( SQL-literal, ... )`) and the
// special MATCH queries (fts5_main.c fts5SpecialMatch: '*reads'/'*id').

// FTS5DefaultRank mirrors FTS5_DEFAULT_RANK.
const FTS5DefaultRank = "bm25"

// RankSpec is a resolved rank function: the auxiliary function name and its
// raw argument text (C keeps zRank/zRankArgs; the args are evaluated as a
// SELECT list at use time).
type RankSpec struct {
	Func string
	Args string
}

// ParseRankSpec parses a rank function specification (sqlite3Fts5ConfigParseRank):
// a bareword, an opening parenthesis and a comma-separated list of SQL
// literals. A malformed specification fails like C's bare SQLITE_ERROR.
func ParseRankSpec(zIn string) (*RankSpec, error) {
	p := skipRankWS(zIn)
	bare, rest := skipRankBareword(p)
	if bare == "" {
		return nil, errRankLogic()
	}
	p = skipRankWS(rest)
	if !strings.HasPrefix(p, "(") {
		return nil, errRankLogic()
	}
	// An empty argument list is valid (fts5_config.c: fts5ConfigSkipArgs only
	// runs when the byte after '(' is not ')') — "bm25()" carries no args.
	q := skipRankWS(p[1:])
	if strings.HasPrefix(q, ")") {
		return &RankSpec{Func: bare}, nil
	}
	args, ok := skipRankArgs(q)
	if !ok {
		return nil, errRankLogic()
	}
	return &RankSpec{Func: bare, Args: args}, nil
}

// errRankLogic is C's no-message SQLITE_ERROR for malformed rank specs.
func errRankLogic() error { return fmt.Errorf("SQL logic error") }

// RankParseError is the per-cursor rank-override parse failure
// ("parse error in rank function: %s", fts5_main.c fts5CursorParseRank).
type RankParseError struct{ Text string }

func (e *RankParseError) Error() string { return "parse error in rank function: " + e.Text }

// skipRankWS skips ASCII whitespace (fts5ConfigSkipWhitespace).
func skipRankWS(s string) string { return strings.TrimLeft(s, " \t\n\r\v\f") }

// skipRankBareword consumes a bareword (fts5ConfigSkipBareword); empty means
// failure.
func skipRankBareword(s string) (string, string) {
	i := 0
	for i < len(s) && isFts5Bareword(s[i]) {
		i++
	}
	if i == 0 {
		return "", s
	}
	return s[:i], s[i:]
}

// skipRankArgs consumes a comma-separated literal list up to the closing
// parenthesis (fts5ConfigSkipArgs + fts5ConfigSkipLiteral), returning the
// argument text between the parentheses.
func skipRankArgs(s string) (string, bool) {
	p := s
	for {
		p = skipRankWS(p)
		var ok bool
		p, ok = skipRankLiteral(p)
		if !ok {
			return "", false
		}
		p = skipRankWS(p)
		if p == "" {
			return "", false
		}
		if p[0] == ')' {
			return s[:len(s)-len(p)], true
		}
		if p[0] != ',' {
			return "", false
		}
		p = p[1:]
	}
}

// skipRankLiteral consumes one SQL literal (fts5ConfigSkipLiteral): NULL, an
// X'...' blob, a '...' string or a number.
func skipRankLiteral(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	switch s[0] {
	case 'n', 'N':
		return skipRankNull(s)
	case 'x', 'X':
		return skipRankBlob(s)
	case '\'':
		return skipRankQuoted(s)
	default:
		return skipRankNumber(s)
	}
}

// skipRankNull consumes NULL (case-insensitive).
func skipRankNull(s string) (string, bool) {
	if len(s) >= 4 && strings.EqualFold(s[:4], "null") {
		return s[4:], true
	}
	return "", false
}

// skipRankBlob consumes X'...' with an even number of hex digits
// (fts5ConfigSkipLiteral's blob branch).
func skipRankBlob(s string) (string, bool) {
	if len(s) < 3 || s[1] != '\'' {
		return "", false
	}
	i := 2
	for i < len(s) && isRankHex(s[i]) {
		i++
	}
	if i < len(s) && s[i] == '\'' && (i-2)%2 == 0 {
		return s[i+1:], true
	}
	return "", false
}

// skipRankQuoted consumes a '...' string with ” escapes.
func skipRankQuoted(s string) (string, bool) {
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			i++
			continue
		}
		return s[i+1:], true
	}
	return "", false
}

// skipRankNumber consumes a number with optional sign, integer part and
// fractional part (fts5ConfigSkipLiteral's number branch).
func skipRankNumber(s string) (string, bool) {
	i := 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	i = skipRankDigits(s, i)
	if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
		i = skipRankDigits(s, i+2)
	}
	if i == 0 {
		return "", false
	}
	return s[i:], true
}

// skipRankDigits advances past ASCII digits starting at i.
func skipRankDigits(s string, i int) int {
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// isRankHex reports whether b is a hexadecimal digit.
func isRankHex(b byte) bool {
	return (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') || (b >= '0' && b <= '9')
}

// RankValue computes the rank pseudo-column value for one document with the
// given rank function (fts5_main.c fts5CursorRank). Only the built-in bm25
// rank function exists; an unknown name fails like C's function resolution.
// aq may be nil (no MATCH constraint): bm25 then evaluates to -0.0.
func (t *Table) RankValue(spec *RankSpec, aq *AuxQuery, rowid int64) (interface{}, error) {
	name := spec.Func
	if name == "" {
		name = FTS5DefaultRank
	}
	if !strings.EqualFold(name, "bm25") {
		return nil, fmt.Errorf("no such function: %s", name)
	}
	var weights []float64
	if spec.Args != "" {
		// The rank arguments are evaluated as an SQL SELECT list
		// (fts5_main.c fts5CursorRankArgs builds "SELECT <args>").
		rows, err := t.db.ExecSQL("SELECT " + spec.Args)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			for _, v := range rows[0] {
				weights = append(weights, rankWeight(v))
			}
		}
	}
	if aq == nil {
		aq = t.NewScanAux()
	}
	return aq.Bm25(rowid, weights), nil
}

// rankWeight coerces one rank-argument value to a column weight
// (sqlite3_value_double).
func rankWeight(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case string:
		return rankAtof(x)
	case []byte:
		return rankAtof(string(x))
	}
	return 0
}

// rankAtof parses the leading floating-point number of s (sqlite3AtoF
// behavior: a non-numeric prefix yields 0).
func rankAtof(s string) float64 {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	n := skipRankDigits(s, i)
	if n < len(s) && s[n] == '.' {
		n = skipRankDigits(s, n+1)
	}
	f, err := strconv.ParseFloat(s[:n], 64)
	if err != nil {
		return 0
	}
	return f
}

// SpecialQueryValue resolves a special MATCH query (fts5SpecialMatch): a
// query beginning with '*' names a directive after the star. Only 'reads' and
// 'id' are recognized; both report a single row whose rowid carries the value
// (frigolite reports 0 for both: no read counter, first cursor).
func (t *Table) SpecialQueryValue(query string) (int64, error) {
	z := strings.TrimLeft(query[1:], " ")
	n := strings.IndexByte(z, ' ')
	if n < 0 {
		n = len(z)
	}
	name := z[:n]
	switch {
	case strings.EqualFold(name, "reads"):
		return 0, nil
	case strings.EqualFold(name, "id"):
		return 0, nil
	}
	return 0, fmt.Errorf("unknown special query: %s", name)
}

// SpecialCursorValue resolves the hidden-column value a special query's
// cursor carries: '*id' reports the evaluating cursor's own id
// (pCsr->iSpecial = pCsr->iCsrId) and '*reads' the index read counter
// (frigolite performs no tracked reads: 0).
func (t *Table) SpecialCursorValue(query string, csrID int64) (int64, error) {
	z := strings.TrimLeft(query[1:], " ")
	n := strings.IndexByte(z, ' ')
	if n < 0 {
		n = len(z)
	}
	name := z[:n]
	switch {
	case strings.EqualFold(name, "reads"):
		return 0, nil
	case strings.EqualFold(name, "id"):
		return csrID, nil
	}
	return 0, fmt.Errorf("unknown special query: %s", name)
}
