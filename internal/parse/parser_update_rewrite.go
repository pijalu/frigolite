// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// This file holds the UPDATE ... SET (c,d) = (expr) row-value assignment
// rewriter: it rewrites paren-set setlist entries into per-column assignments
// the LALR tables accept.

package parse

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// spanBytesEnd returns the byte offset of the end of a token span (sp.end is
// a token index).
func spanBytesEnd(toks []sql.Token, sp stmtSpan, inputLen int) int {
	if sp.end <= sp.start || sp.end > len(toks) {
		return inputLen
	}
	lastTok := toks[sp.end-1]
	return lastTok.Pos + len(lastTok.Value)
}

// parenRewriteSpan records a paren-set rewrite so the ORIGINAL statement text
// can be restored when capturing RawSQL (SQLite stores CREATE TRIGGER text
// verbatim, including row-value SET assignments, altertab2-4.x).
// outStart/outEnd are byte offsets in the REWRITTEN input marking the
// "col = expr[, col = expr...]" text; origText is the original paren-set text
// from the pre-rewrite input.
type parenRewriteSpan struct {
	outStart int
	outEnd   int
	origText string
}

// rewriteSpan rewrites one statement span's paren-set setlist entries. The
// plain text before each paren-set stays, the paren-set becomes
// "col = expr[, col = expr...]". Returns the byte offset after the span.
// lenient is set for CREATE TRIGGER statement spans: SQLite parses the body at
// CREATE time without validating column/value arity (the error appears only
// when the trigger fires), so an arity mismatch duplicates the RHS instead of
// failing the parse (altertab-33.0).
func rewriteSpan(sb *strings.Builder, input string, toks []sql.Token, sp stmtSpan, last int, spans *[]parenRewriteSpan, lenient bool) (int, error) {
	spEndBytes := spanBytesEnd(toks, sp, len(input))
	// Find every "SET (" at depth 0 inside an UPDATE statement, plus any
	// "(col, col) =" paren-set that follows a comma in a setlist (the
	// tokenizer sees it as LParen...RParen EQ, not preceded by SET).
	setIdxs := findUpdateParenSets(toks, sp)
	if len(setIdxs) == 0 {
		if spEndBytes >= last {
			sb.WriteString(input[last:spEndBytes])
		}
		return spEndBytes, nil
	}
	cur := last
	for _, setIdx := range setIdxs {
		// setIdx may be the SET keyword token (SET (c,d)=...), the LParen, or
		// the closing RParen (SET b=8, (c,d)=...). Normalize to (lp, closeParen).
		lp, closeParen := parenSetBounds(toks, setIdx, sp)
		if lp < 0 || closeParen < 0 {
			continue
		}
		eqIdx := findSetEq(toks, closeParen, sp)
		if eqIdx < 0 {
			continue
		}
		cols := extractSetCols(toks, lp, closeParen)
		if len(cols) == 0 {
			continue
		}
		// Find the RHS expression end (next depth-0 COMMA or statement end).
		rhsStart := toks[eqIdx].Pos + 1
		rhsEnd := findSetRhsEnd(toks, eqIdx, sp, spEndBytes)
		rhs := strings.TrimSpace(input[rhsStart:rhsEnd])
		assigns, err := buildSetAssigns(cols, rhs)
		if err != nil {
			if !lenient {
				return 0, err
			}
			// CREATE TRIGGER body: SQLite accepts an arity-mismatched
			// row-value assignment at CREATE time and only reports the
			// mismatch when the trigger fires. Duplicate the RHS per column
			// so the body parses; the original text is restored for RawSQL.
			assigns = make([]string, len(cols))
			for i := range cols {
				assigns[i] = fmt.Sprintf("%s = %s", cols[i], rhs)
			}
		}
		// beforeText already ends with "SET " (SET (c,d)=...) or ", "
		// (SET b=8, (c,d)=...), so the replacement continues the setlist
		// without adding another SET keyword.
		sb.WriteString(input[cur:toks[lp].Pos])
		outStart := sb.Len()
		joined := strings.Join(assigns, ", ")
		// Preserve the whitespace between the RHS and its terminator
		// (rowvalue7-1.x: "SET (c) = 99 WHERE" must not become "99WHERE").
		rhsRaw := input[rhsStart:rhsEnd]
		joined += rhsRaw[len(strings.TrimRight(rhsRaw, " \t\n\r\f")):]
		sb.WriteString(joined)
		// Record the span so RawSQL capture can restore the original
		// paren-set text (SQLite stores DDL text verbatim).
		*spans = append(*spans, parenRewriteSpan{
			outStart: outStart,
			outEnd:   outStart + len(joined),
			origText: input[toks[lp].Pos:rhsEnd],
		})
		cur = rhsEnd
	}
	sb.WriteString(input[cur:spEndBytes])
	return spEndBytes, nil
}

// rewriteParenSet rewrites UPDATE ... SET (c1, c2) = (expr) — SQLite's
// row-value assignment — into per-column assignments the LALR tables accept,
// returning the rewritten input plus the parenRewriteSpans needed to restore
// the original text for RawSQL capture.
//
//	SET (c,d) = (SELECT y,z FROM t WHERE ...) →
//	  SET c = (SELECT y FROM t WHERE ...), d = (SELECT z FROM t WHERE ...)
//	SET (c) = 99 → SET c = 99
//
// A scalar RHS (SET (c) = 99) assigns that value to every listed column.
// When the RHS is a subquery whose column count differs from the column list,
// the rewrite leaves the statement unchanged so the engine reports the arity
// mismatch at parse time (it fails to parse, and the caller surfaces it).
func rewriteParenSet(input string) (string, []parenRewriteSpan, error) {
	if !strings.Contains(strings.ToUpper(input), "SET") || !strings.Contains(input, "(") {
		return input, nil, nil
	}
	toks, ok := tokenizeInput(input)
	if !ok {
		return input, nil, nil
	}
	spans := splitTopLevelStatements(toks)
	var sb strings.Builder
	var rewrites []parenRewriteSpan
	last := 0
	for _, sp := range spans {
		lenient := isCreateTriggerSpan(toks, sp)
		next, err := rewriteSpan(&sb, input, toks, sp, last, &rewrites, lenient)
		if err != nil {
			return "", nil, err
		}
		last = next
	}
	sb.WriteString(input[last:])
	return sb.String(), rewrites, nil
}

// isCreateTriggerSpan reports whether a statement span is a CREATE TRIGGER
// statement (its body's row-value assignments are validated at fire time, not
// CREATE time).
func isCreateTriggerSpan(toks []sql.Token, sp stmtSpan) bool {
	for j := sp.start; j < sp.end; j++ {
		t := toks[j]
		switch {
		case t.Type == sql.TokenLParen:
			return false
		case t.Type == sql.TokenKeyword && strings.EqualFold(t.Value, "CREATE"):
			// look ahead for TRIGGER
			for k := j + 1; k < sp.end && k <= j+3; k++ {
				if toks[k].Type == sql.TokenKeyword && strings.EqualFold(toks[k].Value, "TRIGGER") {
					return true
				}
			}
			return false
		case t.Type == sql.TokenSemicolon:
			return false
		}
	}
	return false
}

// setKeywordIdx reports whether the token at j is a SET keyword immediately
// followed by LParen at depth 0, and returns the token index to record.
func setKeywordIdx(toks []sql.Token, j int, sp stmtSpan) (int, bool) {
	if toks[j].Type != sql.TokenKeyword || !strings.EqualFold(toks[j].Value, "SET") {
		return 0, false
	}
	if j+1 < sp.end && toks[j+1].Type == sql.TokenLParen {
		return j, true
	}
	return 0, false
}

// parenSetAfterClose reports whether a closed paren at token j is a paren-set
// setlist entry: depth 0, followed by EQ, inside an UPDATE's SET clause (not
// WHERE), and not already recorded via a preceding SET keyword.
func parenSetAfterClose(toks []sql.Token, j int, sp stmtSpan, inUpdate, inWhere bool, d int) bool {
	return inUpdate && !inWhere && d == 0 && j+1 < sp.end && toks[j+1].Type == sql.TokenEq &&
		!precededBySet(toks, j, sp)
}

// setParenScanner tracks the state needed to find paren-set setlist entries
// within an UPDATE statement span.
type setParenScanner struct {
	toks     []sql.Token
	sp       stmtSpan
	d        int
	setIdxs  []int
	inUpdate bool
	inInsert bool
	inWhere  bool
}

// trackKeyword updates the UPDATE/WHERE state flags for a keyword token.
func (s *setParenScanner) trackKeyword(j int) {
	kw := s.toks[j]
	if kw.Type != sql.TokenKeyword {
		return
	}
	if !s.inUpdate && strings.EqualFold(kw.Value, "UPDATE") {
		s.inUpdate = true
	}
	// INSERT ... ON CONFLICT DO UPDATE SET (c,d)=... has no bare UPDATE
	// keyword before the SET; recognize the INSERT ... UPDATE form.
	if strings.EqualFold(kw.Value, "INSERT") {
		s.inInsert = true
	}
	if s.inInsert && strings.EqualFold(kw.Value, "UPDATE") {
		s.inUpdate = true
	}
	// Paren-sets only appear in the SET clause; a `) =` after WHERE
	// is a WHERE expression (e.g. abs(x)=248), not a setlist entry.
	if s.d == 0 && strings.EqualFold(kw.Value, "WHERE") {
		s.inWhere = true
	}
}

// record processes token j, updating paren depth and recording paren-set
// entries (SET keyword + LParen form, or RParen + EQ form).
func (s *setParenScanner) record(j int) {
	s.trackKeyword(j)
	switch s.toks[j].Type {
	case sql.TokenLParen:
		s.d++
	case sql.TokenRParen:
		if s.d > 0 {
			s.d--
		}
		// A closed paren followed by EQ at depth 0 is a paren-set
		// (SET (c,d) = ... or SET b=8, (c,d) = ...). Skip the SET (
		// form (already recorded via the SET keyword below) and any
		// paren in the WHERE clause (a function call or comparison).
		if parenSetAfterClose(s.toks, j, s.sp, s.inUpdate, s.inWhere, s.d) {
			s.setIdxs = append(s.setIdxs, j)
		}
	case sql.TokenKeyword:
		if idx, ok := setKeywordIdx(s.toks, j, s.sp); ok {
			s.setIdxs = append(s.setIdxs, idx)
		}
	}
}

// findUpdateParenSets scans one statement span for paren-set setlist entries
// (SET (c,d)=... or SET b=8, (c,d)=...), returning the token indices to
// rewrite. The index may be the SET keyword, the LParen, or the closing RParen
// of the paren group.
func findUpdateParenSets(toks []sql.Token, sp stmtSpan) []int {
	s := &setParenScanner{toks: toks, sp: sp}
	for j := sp.start; j < sp.end; j++ {
		s.record(j)
	}
	return s.setIdxs
}

// precededBySet reports whether the paren group ending at token j is preceded
// by a SET ( form (SET (c,d)=...), in which case the SET keyword was already
// recorded and the closing-paren form must not be recorded again.
func precededBySet(toks []sql.Token, j int, sp stmtSpan) bool {
	dd := 0
	for k := j; k >= 0; k-- {
		if toks[k].Type == sql.TokenLParen {
			dd++
		}
		if toks[k].Type == sql.TokenRParen {
			dd--
		}
		if dd == 0 && toks[k].Type == sql.TokenKeyword &&
			strings.EqualFold(toks[k].Value, "SET") &&
			k+1 < sp.end && toks[k+1].Type == sql.TokenLParen {
			return true
		}
	}
	return false
}

// parenSetBounds normalizes a setIdx (SET keyword, LParen, or RParen) to the
// paren group's (lp, closeParen) token indices. Returns (-1, -1) when the
// group cannot be found.
func parenSetBounds(toks []sql.Token, setIdx int, sp stmtSpan) (int, int) {
	switch {
	case toks[setIdx].Type == sql.TokenLParen:
		return findMatchingClose(toks, setIdx, sp)
	case toks[setIdx].Type == sql.TokenRParen:
		lp, ok := findMatchingOpen(toks, setIdx)
		if !ok {
			return -1, -1
		}
		return lp, setIdx
	case toks[setIdx].Type == sql.TokenKeyword && strings.EqualFold(toks[setIdx].Value, "SET"):
		// SET (c,d)=... : the LParen is the next token.
		if setIdx+1 < sp.end && toks[setIdx+1].Type == sql.TokenLParen {
			return findMatchingClose(toks, setIdx+1, sp)
		}
	}
	return -1, -1
}

// findMatchingClose returns (lp, closeParen) for a paren group whose LParen
// is at token index lp (searching forward for the matching RParen).
func findMatchingClose(toks []sql.Token, lp int, sp stmtSpan) (int, int) {
	depth := 0
	for j := lp; j < sp.end; j++ {
		switch toks[j].Type {
		case sql.TokenLParen:
			depth++
		case sql.TokenRParen:
			depth--
			if depth == 0 {
				return lp, j
			}
		}
	}
	return -1, -1
}

// findMatchingOpen returns the LParen matching the RParen at token index close.
func findMatchingOpen(toks []sql.Token, close int) (int, bool) {
	depth := 0
	for j := close; j >= 0; j-- {
		switch toks[j].Type {
		case sql.TokenRParen:
			depth++
		case sql.TokenLParen:
			depth--
			if depth == 0 {
				return j, true
			}
		}
	}
	return -1, false
}

// findSetEq returns the first EQ token after the closing paren of a paren-set.
func findSetEq(toks []sql.Token, closeParen int, sp stmtSpan) int {
	for j := closeParen + 1; j < sp.end; j++ {
		if toks[j].Type == sql.TokenEq {
			return j
		}
	}
	return -1
}

// extractSetCols returns the column identifiers inside a paren-set group.
func extractSetCols(toks []sql.Token, lp, closeParen int) []string {
	var cols []string
	for j := lp + 1; j < closeParen; j++ {
		if toks[j].Type == sql.TokenIdentifier || toks[j].Type == sql.TokenKeyword {
			if !strings.EqualFold(toks[j].Value, "") {
				cols = append(cols, toks[j].Value)
			}
		}
	}
	return cols
}

// isSetlistTerminator reports whether a depth-0 keyword ends the SET list
// (WHERE, FROM, ORDER, LIMIT, RETURNING).
func isSetlistTerminator(value string) bool {
	switch strings.ToUpper(value) {
	case "WHERE", "FROM", "ORDER", "LIMIT", "RETURNING":
		return true
	}
	return false
}

// findSetRhsEnd finds the byte offset where a paren-set RHS expression ends
// (the next depth-0 COMMA or a setlist-terminating keyword).
func findSetRhsEnd(toks []sql.Token, eqIdx int, sp stmtSpan, spEndBytes int) int {
	rhsEnd := spEndBytes
	depth := 0
	for j := eqIdx + 1; j < sp.end; j++ {
		switch toks[j].Type {
		case sql.TokenLParen:
			depth++
		case sql.TokenRParen:
			if depth > 0 {
				depth--
			}
		case sql.TokenComma:
			if depth == 0 {
				rhsEnd = toks[j].Pos
			}
		case sql.TokenKeyword:
			// A depth-0 keyword that ends the setlist (WHERE, FROM,
			// ORDER, LIMIT, RETURNING) terminates the RHS.
			if depth == 0 && isSetlistTerminator(toks[j].Value) {
				rhsEnd = toks[j].Pos
			}
		}
		if rhsEnd < spEndBytes {
			break
		}
	}
	return rhsEnd
}

// buildSetAssigns builds the "col = expr" assignments for a paren-set RHS.
func buildSetAssigns(cols []string, rhs string) ([]string, error) {
	var assigns []string
	if len(rhs) >= 2 && rhs[0] == '(' && rhs[len(rhs)-1] == ')' {
		inner := strings.TrimSpace(rhs[1 : len(rhs)-1])
		if strings.HasPrefix(strings.ToUpper(inner), "SELECT") {
			if sqlParts, ok := splitSelectList(inner, len(cols)); ok {
				for i, col := range cols {
					assigns = append(assigns, fmt.Sprintf("%s = (%s)", col, sqlParts[i]))
				}
				return assigns, nil
			}
			// Arity mismatch: SQLite reports "N columns assigned M
			// values" at prepare time.
			m := len(splitTopLevelComma(strings.TrimSpace(strings.TrimPrefix(inner, "SELECT "))))
			return nil, fmt.Errorf("%d columns assigned %d values", len(cols), m)
		}
		// A parenthesized value list: SET (c,a) = ('four', 4) assigns each
		// value to its column positionally.
		parts := splitTopLevelComma(inner)
		if len(parts) != len(cols) {
			return nil, fmt.Errorf("%d columns assigned %d values", len(cols), len(parts))
		}
		for i, col := range cols {
			assigns = append(assigns, fmt.Sprintf("%s = %s", col, strings.TrimSpace(parts[i])))
		}
		return assigns, nil
	}
	for _, col := range cols {
		assigns = append(assigns, fmt.Sprintf("%s = %s", col, rhs))
	}
	return assigns, nil
}

// findTopLevelKeyword finds a keyword at paren depth 0 (or returns -1).
func findTopLevelKeyword(toks []sql.Token, keyword string) int {
	d := 0
	for j, t := range toks {
		switch t.Type {
		case sql.TokenLParen:
			d++
		case sql.TokenRParen:
			if d > 0 {
				d--
			}
		case sql.TokenKeyword:
			if d == 0 && strings.EqualFold(t.Value, keyword) {
				return j
			}
		}
	}
	return -1
}

// splitSelectList splits a SELECT statement's select list into per-column
// subqueries: given "SELECT y, z FROM t WHERE ...", returns
// ["SELECT y FROM t WHERE ...", "SELECT z FROM t WHERE ..."]. Returns ok=false
// when the list has a different number of columns than expected.
func splitSelectList(selectSQL string, wantCols int) ([]string, bool) {
	rest := strings.TrimSpace(selectSQL)
	re := regexp.MustCompile(`(?is)^SELECT\s+(.*)$`)
	m := re.FindStringSubmatch(rest)
	if m == nil {
		return nil, false
	}
	listAndFrom := m[1]
	// Find the FROM keyword at depth 0 (the select list ends there).
	toks, ok := tokenizeInput(listAndFrom)
	if !ok {
		return nil, false
	}
	fromIdx := findTopLevelKeyword(toks, "FROM")
	listEnd := len(listAndFrom)
	fromPart := ""
	if fromIdx >= 0 {
		listEnd = toks[fromIdx].Pos
		fromPart = strings.TrimSpace(listAndFrom[toks[fromIdx].Pos:])
	}
	list := strings.TrimSpace(listAndFrom[:listEnd])
	cols := splitTopLevelComma(list)
	if len(cols) != wantCols {
		return nil, false
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = "SELECT " + c + " " + fromPart
	}
	return out, true
}

// splitTopLevelComma splits a string on commas at parenthesis depth 0.
func splitTopLevelComma(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, c := range s {
		switch c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}
