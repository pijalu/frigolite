// LIKE-optimization range synthesis for single-table scans. This ports
// whereexpr.c's exprAnalyze LIKE/GLOB branch: an index-usable
// "x LIKE 'abc%'" term gains the virtual range constraints
// x >= 'abc' AND x < 'abd' (last prefix byte incremented), and when the
// pattern is exactly prefix + one trailing match-all wildcard the range
// provably decides the row, so the like()/glob() call is elided the way
// wherecode.c's TERM_LIKEOPT/TERM_LIKECOND code generation omits it.
// Unlike EXPLAIN-time collectLikeRef this rewrites the scan-local WHERE, so
// both the visited-row set and the sqlite3_like_count contract
// (like.test 3.x) match SQLite.

package execquery

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/value"
)

// likeOptimizedScanWhere returns where with every top-level LIKE/GLOB
// conjunct that is index-usable for the scanned table replaced by a clone
// carrying its synthesized LikeRangeOpt. Non-conjunct LIKE terms (inside OR
// arms, NOT wrappers, subqueries) keep the plain match, mirroring
// exprAnalyze's top-level WHERE-clause term analysis.
func (e *SelectEngine) likeOptimizedScanWhere(s *sql.SelectStmt, colDefs []sql.ColumnDef, where sql.Expr) sql.Expr {
	tableName := s.From.Name
	if tableName == "" || s.From.Subquery != nil {
		return where
	}
	conjuncts := likeOptConjuncts(where, nil)
	decorated := false
	for i, c := range conjuncts {
		binop, ok := c.(*sql.BinaryOp)
		if !ok || (binop.Operator != "LIKE" && binop.Operator != "GLOB") {
			continue
		}
		if rng := e.likeRangeForTerm(binop, tableName, colDefs, where); rng != nil {
			conjuncts[i] = &sql.BinaryOp{
				Left: binop.Left, Right: binop.Right, Operator: binop.Operator,
				Escape: binop.Escape, HasEscape: binop.HasEscape, LikeRange: rng,
			}
			decorated = true
		}
	}
	if !decorated {
		return where
	}
	return rebuildAND(conjuncts)
}

// likeOptConjuncts flattens the top-level AND tree of a WHERE clause into
// its conjuncts, looking through parentheses (SQLite's WHERE-clause term
// analysis is parenthesis-transparent for AND).
func likeOptConjuncts(expr sql.Expr, out []sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return likeOptConjuncts(v.Expr, out)
	case *sql.BinaryOp:
		if v.Operator == "AND" {
			out = likeOptConjuncts(v.Left, out)
			return likeOptConjuncts(v.Right, out)
		}
	}
	return append(out, expr)
}

// rebuildAND rebuilds a right-leaning AND tree from flattened conjuncts.
func rebuildAND(conjuncts []sql.Expr) sql.Expr {
	result := conjuncts[len(conjuncts)-1]
	for i := len(conjuncts) - 2; i >= 0; i-- {
		result = &sql.BinaryOp{Left: conjuncts[i], Right: result, Operator: "AND"}
	}
	return result
}

// likeRangeForTerm synthesizes the LikeRangeOpt for one LIKE/GLOB term on
// tableName, or nil when any SQLite refusal condition applies (no usable
// index, collation mismatch, non-constant pattern, leading wildcard,
// numeric-looking prefix on a non-TEXT column — isLikeOrGlob's guards).
func (e *SelectEngine) likeRangeForTerm(binop *sql.BinaryOp, tableName string, colDefs []sql.ColumnDef, where sql.Expr) *sql.LikeRangeOpt {
	colRef, ok := binop.Left.(*sql.ColumnRef)
	if !ok {
		return nil
	}
	pattern, ok := likePatternConst(binop.Right)
	if !ok {
		// isLikeOrGlob's bound-parameter branch (TK_VARIABLE): the pattern
		// value is read at prepare time and drives the range, unless the
		// query planner stability guarantee is active. Only $parameters
		// carry a value here (the TCL-driver binding); anything else
		// evaluates to NULL and is refused like SQLite's NULL bound value.
		pattern, ok = e.likePatternVariable(binop.Right)
		if !ok {
			return nil
		}
	}
	noCase := binop.Operator == "LIKE" && !e.ctx.CaseSensitiveLike()
	matchAll, matchOne, matchSet := byte('%'), byte('_'), byte(0)
	if binop.Operator == "GLOB" {
		matchAll, matchOne, matchSet = '*', '?', '['
	}
	prefix, isComplete, ok := likePrefixRange(pattern, binop.Escape, binop.HasEscape, matchAll, matchOne, matchSet)
	if !ok || prefix == "" {
		return nil
	}
	idxName := e.findIndexOnColumn(tableName, colRef.Name, where)
	if idxName == "" {
		return nil
	}
	coll := e.indexColumnCollation(tableName, idxName, colRef.Name)
	if !likeCollationCompatible(coll, noCase) {
		return nil
	}
	if likeRangeRefuseNumeric(scanColumnAffinity(colDefs, colRef.Name), prefix) {
		return nil
	}
	low, high, isComplete := likeRangeBounds(prefix, noCase, isComplete)
	return &sql.LikeRangeOpt{
		Low:        low,
		High:       high,
		Collation:  likeRangeCollation(noCase),
		IsComplete: isComplete,
		NoCase:     noCase,
	}
}

// likeCollationCompatible reports whether an index column collation can
// drive the range for a comparison of the given case mode (likeIndexCompatible
// made operator-aware: GLOB always compares case-sensitively, so it needs a
// BINARY-collated column regardless of PRAGMA case_sensitive_like).
func likeCollationCompatible(coll string, noCase bool) bool {
	if !noCase {
		return coll == "" || strings.EqualFold(coll, "BINARY")
	}
	return strings.EqualFold(coll, "NOCASE")
}

// likePatternVariable resolves a bound-parameter LIKE pattern the way
// isLikeOrGlob reads the bound value of a TK_VARIABLE operand: the current
// value of a $name / $::name TCL variable (sqlite3VdbeGetBoundValue), when
// the QPSG guarantee is off.
func (e *SelectEngine) likePatternVariable(rhs sql.Expr) (string, bool) {
	if e.ctx.QPSG() {
		return "", false
	}
	param, ok := rhs.(*sql.ParameterExpr)
	if !ok {
		return "", false
	}
	val, ok := e.ctx.TCLParam(param.Name)
	if !ok || val == "" {
		return "", false
	}
	return val, true
}

// likeRangeCollation names the collation of the synthesized range
// comparisons (exprAnalyze: zCollSeqName = noCase ? "NOCASE" : "BINARY").
func likeRangeCollation(noCase bool) string {
	if noCase {
		return "NOCASE"
	}
	return "BINARY"
}

// scanColumnAffinity resolves the declared affinity of a scanned table's
// column (0 when unknown; value.Affinity for the declared type).
func scanColumnAffinity(colDefs []sql.ColumnDef, colName string) rune {
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			return value.Affinity(cd.Type)
		}
	}
	return 0
}

// likeRangeRefuseNumeric mirrors isLikeOrGlob's numeric guard: when the LHS
// is not a TEXT-affinity column the prefix boundaries "must not look like a
// number, otherwise the pattern might be treated as a number, which will
// invalidate the LIKE optimization". Both the prefix and its incremented
// form are tested (the 2018-2021 c94369cae9b561b1 family of fixes).
func likeRangeRefuseNumeric(affinity rune, prefix string) bool {
	if affinity == 'T' {
		return false
	}
	if likeLooksNumeric(prefix) {
		return true
	}
	// Incremented form: plain last-byte increment, as in isLikeOrGlob.
	inc := []byte(prefix)
	inc[len(inc)-1]++
	return likeLooksNumeric(string(inc))
}

// likeLooksNumeric mirrors the sqlite3AtoF test plus the lone "-" special
// case from isLikeOrGlob.
func likeLooksNumeric(s string) bool {
	if s == "-" {
		return true
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// likeRangeBounds derives the range bounds from the prefix (exprAnalyze):
// with NoCase the lower bound is upper-cased and the upper bound
// lower-cased so the bounds also work for BLOBs; the upper bound is the
// prefix with its last byte incremented (with a 0xBF carry through
// multi-byte sequences). A folded upper bound ending at '@' increments
// into the alphabetic range where case conversion breaks the inequality,
// so isComplete is cleared (exprAnalyze: if *pC=='A'-1 isComplete=0) and
// the matcher keeps deciding the visited rows.
func likeRangeBounds(prefix string, noCase bool, isComplete bool) (low, high string, complete bool) {
	complete = isComplete
	lowB, highB := []byte(prefix), []byte(prefix)
	last := len(highB) - 1
	if noCase {
		for i, c := range lowB {
			lowB[i] = sqliteAsciiUpper(c)
		}
		for i, c := range highB {
			highB[i] = sqliteAsciiLower(c)
		}
		if highB[last] == '@' {
			complete = false
		}
	}
	// Increment the last byte, carrying 0xBF backwards (utf-8 sequences).
	i := last
	for i > 0 && highB[i] == 0xBF {
		highB[i] = 0x80
		i--
	}
	highB[i]++
	return string(lowB), string(highB), complete
}

// sqliteAsciiUpper/lower mirror sqlite3Toupper/sqlite3Tolower: ASCII-only
// case folding, every other byte unchanged.
func sqliteAsciiUpper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

func sqliteAsciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// likePrefixRange extracts the non-wildcard prefix of a LIKE/GLOB pattern
// that a range scan can use, mirroring isLikeOrGlob's counting loop: the
// scan stops at the first wildcard, honors single-ASCII-byte escapes,
// advances whole UTF-8 sequences, and stops (before the offending byte) at
// 0xFF or malformed UTF-8 because such characters cannot be range-bounded.
// isComplete reports that the pattern is exactly prefix + one trailing
// match-all wildcard (wherecode.c elides the like() call only then).
// Escape handling follows SQLite: the ESCAPE clause must be a single
// character; an empty ESCAPE disables the optimization.
func likePrefixRange(pattern, escape string, hasEscape bool, matchAll, matchOne, matchSet byte) (prefix string, isComplete bool, ok bool) {
	if hasEscape && len(escape) != 1 {
		return "", false, false
	}
	esc := byte(0)
	if escape != "" {
		esc = escape[0]
	}
	cnt := 0
	for cnt < len(pattern) {
		c := pattern[cnt]
		if c == matchAll || c == matchOne || (matchSet != 0 && c == matchSet) {
			// A "complete" match if the only wildcard is the match-all
			// at the very end of the pattern.
			return likePrefixUnescaped(pattern, cnt, esc), c == matchAll && cnt == len(pattern)-1, true
		}
		next, truncated, malformed := advanceLikeLiteral(pattern, cnt, esc)
		if truncated {
			return "", false, false // trailing escape char
		}
		if malformed {
			return likePrefixUnescaped(pattern, cnt, esc), false, true
		}
		cnt = next
	}
	// No wildcard: the whole pattern is the prefix. The optimization also
	// requires at least one literal character after escape removal
	// (isLikeOrGlob's (cnt>1 || z[0]!=wc[3]) guard).
	return likePrefixComplete(pattern, cnt, esc)
}

// likePrefixComplete finalizes a wildcard-free prefix: the optimization
// requires at least one literal character after escape removal.
func likePrefixComplete(pattern string, cnt int, esc byte) (string, bool, bool) {
	unesc := likePrefixUnescaped(pattern, cnt, esc)
	if len(unesc) == 0 || (len(pattern) == 1 && esc != 0 && pattern[0] == esc) {
		return "", false, false
	}
	return unesc, false, true
}

// advanceLikeLiteral advances past the literal byte at cnt: an escape pair
// moves two bytes, a well-formed UTF-8 sequence moves to the next rune
// boundary. truncated reports a trailing escape char; malformed reports a
// malformed sequence (isLikeOrGlob stops the prefix there).
func advanceLikeLiteral(pattern string, cnt int, esc byte) (next int, truncated bool, malformed bool) {
	if esc != 0 && pattern[cnt] == esc {
		if cnt+1 >= len(pattern) {
			return 0, true, false
		}
		return cnt + 2, false, false
	}
	if pattern[cnt] >= 0x80 {
		next := likeUTF8WidthAt(pattern, cnt)
		if next == 0 {
			return 0, false, true
		}
		return next, false, false
	}
	return cnt + 1, false, false
}

// likePrefixUnescaped strips escape bytes from the pattern prefix
// pattern[:end], keeping each escaped character (exprAnalyze's zNew loop).
func likePrefixUnescaped(pattern string, end int, esc byte) string {
	if esc == 0 {
		return pattern[:end]
	}
	out := make([]byte, 0, end)
	for i := 0; i < end; i++ {
		if pattern[i] == esc {
			i++
			if i >= end {
				break
			}
		}
		out = append(out, pattern[i])
	}
	return string(out)
}

// likeUTF8WidthAt returns the byte index just past the UTF-8 sequence at
// pattern[i] when it is a well-formed non-0xFF sequence, or 0 when the
// sequence is 0xFF or malformed (isLikeOrGlob stops the prefix there).
func likeUTF8WidthAt(pattern string, i int) int {
	c := pattern[i]
	if c == 0xFF {
		return 0
	}
	if c < 0xC0 {
		// Lone continuation byte: reads as itself, scan continues.
		return i + 1
	}
	width := utf8SeqWidthRange(c)
	end := i + int(width)
	if end > len(pattern) {
		return 0
	}
	if !utf8.ValidString(pattern[i:end]) {
		return 0
	}
	return end
}

// utf8SeqWidthRange returns the UTF-8 sequence width for a lead byte
// (2-4; mirrored from the lexer's utf8SeqWidth).
func utf8SeqWidthRange(c byte) int {
	switch {
	case c >= 0xF0:
		return 4
	case c >= 0xE0:
		return 3
	default:
		return 2
	}
}
