// Package main implements the tcl2go tool.
//
// This file builds Go string expressions from TCL string parts (including
// bind-parameter resolution and no-command substitution modes).
package main

import (
	"fmt"
	"strconv"
	"strings"
)

// (imports managed by goimports)

func (tp *transpiler) buildStringExpr(s string) string {
	parts := parseStringParts(s, false)
	return tp.renderStringExpr(parts, false)
}

// buildListStringExpr converts TCL text with $var/[cmd] references into a Go
// TCL-list string expression. Unlike buildStringExpr, each $var value is
// rendered through tclListElem so an empty variable stays a {} list element
// (matching TCL's [list $empty] behavior) instead of vanishing from the
// concatenated string. Used for foreach loop lists built with [list ...]
// from variables that may be empty (e.g. rowvalue's $lt/$gt/$le/$ge).
func (tp *transpiler) buildListStringExpr(s string) string {
	parts := parseStringParts(s, false)
	return tp.renderListStringExpr(parts)
}

// buildListStringExprNoCmd is buildListStringExpr with TCL command
// substitution disabled: [...] is kept as literal text (subst -nocommands
// semantics). Used for BRACED foreach lists, whose elements keep [...]
// literally (fts4unicode.test section 9: `tokenize=unicode61
// [tokenchars= .]` must reach the SQL parser as a bracket-quoted identifier,
// not be rendered through tclListElem).
func (tp *transpiler) buildListStringExprNoCmd(s string) string {
	parts := parseStringPartsMode(s, false, true)
	return tp.renderListStringExpr(parts)
}

// buildStringExprNoCmd is like buildStringExpr but treats [...] as literal
// text instead of TCL command substitution. It implements the semantics of
// TCL `subst -nocommands`, where bracket-quoted SQL identifiers such as
// [t1'x1] must be preserved verbatim.
func (tp *transpiler) buildStringExprNoCmd(s string) string {
	parts := parseStringPartsMode(s, false, true)
	return tp.renderStringExpr(parts, false)
}

// buildSQLStringExpr converts TCL text with $var/[cmd] references into a Go
// SQL string expression. Unlike buildStringExpr, variable and command values
// are rendered as SQL literals via sqlLiteral(...) rather than concatenated
// as raw SQL text. TCL `db eval`/`execsql` bind $var as a value, so e.g.
// `db eval {SELECT CAST($str AS real)}` must become
// `CAST(' 876xyz' AS real)`, not `CAST( 876xyz AS real)` (a parse error).
func (tp *transpiler) buildSQLStringExpr(s string) string {
	parts := parseStringParts(s, true)
	// Colon-bound parameters (:varname bound from TCL vars) need the same
	// literal substitution as $var: TCL `db eval {SELECT ... LIMIT :limit}`
	// binds the TCL var `limit` to `:limit`, so rewrite `:limit` as a SQL
	// literal before rendering.
	parts = tp.resolveColonParamRefs(parts)
	// TCL db eval/execsql binds $var references as positional parameters
	// (first $var → ?1, second → ?2, ...). A literal ?NNN in the same SQL
	// refers to the N-th bound parameter and therefore gets the same value
	// as the N-th $var. Replace those literals with the bound variable's
	// sqlLiteral so `INSERT INTO t VALUES($one, ?1)` yields (3, 3) matching
	// SQLite's TCL binding semantics.
	parts = tp.resolveBindParamRefs(parts)
	return tp.renderStringExpr(parts, true)
}

// resolveBindParamRefs rewrites literal ?NNN placeholders in parsed string
// parts to reference the N-th $var bound by TCL db eval/execsql. TCL binds
// $var references in order of appearance as parameters ?1, ?2, ...; a literal
// ?N in the same statement refers to the N-th such parameter, so its value is
// the N-th variable's value. Literals before the first $var that contain ?N
// cannot be resolved (no binding exists yet) and are left unchanged.
func (tp *transpiler) resolveBindParamRefs(parts []stringPart) []stringPart {
	// Collect the variables in binding order (only the first occurrence of
	// each parameter slot matters: each $var is one parameter).
	var boundVars []string
	for _, p := range parts {
		if p.variable != "" {
			boundVars = append(boundVars, p.variable)
		}
	}
	if len(boundVars) == 0 {
		return parts
	}
	// Replace ?N in literal parts with a reference to the N-th bound var.
	out := make([]stringPart, len(parts))
	copy(out, parts)
	for i := range out {
		p := &out[i]
		if p.literal == "" {
			continue
		}
		newParts, replaced := resolveBindLiteral(p.literal, boundVars)
		if !replaced {
			continue
		}
		// Splice newParts into out at position i.
		out = append(out[:i], append(newParts, out[i+1:]...)...)
		i += len(newParts) - 1
	}
	return out
}

// resolveBindLiteral splits a literal part on ?N patterns (outside SQL string
// literals is implicit — literals are already outside strings), replacing ?N
// with a synthetic \x00var\x00 marker referencing the N-th bound variable.
// Returns the rebuilt part list and whether a substitution happened.
func resolveBindLiteral(lit string, boundVars []string) ([]stringPart, bool) {
	var rebuilt strings.Builder
	j := 0
	for j < len(lit) {
		if repl, next, isPattern := bindPatternAt(lit, j, boundVars); isPattern {
			rebuilt.WriteString(repl)
			j = next
			continue
		}
		rebuilt.WriteByte(lit[j])
		j++
	}
	if rebuilt.Len() == len(lit) {
		return nil, false
	}
	// There was a substitution; re-parse the rebuilt text into parts so the
	// synthetic \x00var\x00 markers become variable parts.
	sub := rebuilt.String()
	var newParts []stringPart
	for idx := 0; idx < len(sub); {
		if sub[idx] == '\x00' {
			end := strings.IndexByte(sub[idx+1:], '\x00')
			if end < 0 {
				newParts = append(newParts, stringPart{literal: sub[idx:]})
				break
			}
			name := sub[idx+1 : idx+1+end]
			newParts = append(newParts, stringPart{variable: name})
			idx += end + 2
		} else {
			start := idx
			for idx < len(sub) && sub[idx] != '\x00' {
				idx++
			}
			newParts = append(newParts, stringPart{literal: sub[start:idx]})
		}
	}
	return newParts, true
}

// bindPatternAt detects a ?N bind pattern at position j in lit. Returns the
// replacement text (a synthetic \x00var\x00 marker when N is in range, the
// original text otherwise), the next index to scan, and whether the position
// matched a ?N pattern at all.
func bindPatternAt(lit string, j int, boundVars []string) (string, int, bool) {
	if lit[j] != '?' || j+1 >= len(lit) || !isDigit(lit[j+1]) {
		return "", 0, false
	}
	k := j + 1
	for k < len(lit) && isDigit(lit[k]) {
		k++
	}
	n, _ := strconv.Atoi(lit[j+1 : k])
	if n >= 1 && n <= len(boundVars) {
		// Emit a synthetic variable reference so renderStringExpr wraps it
		// in sqlLiteral(boundVar).
		return "\x00" + boundVars[n-1] + "\x00", k, true
	}
	return lit[j:k], k, true
}

// resolveColonParamRefs rewrites :varname literals that correspond to declared
// TCL vars into variable parts so they render as sqlLiteral(var). TCL binds
// `db eval {SELECT ... LIMIT :limit}`'s `:limit` from the TCL var `limit`;
// the raw `:limit` text would otherwise be an unbound SQL parameter that
// evaluates to NULL (datatype mismatch in a LIMIT clause).
func (tp *transpiler) resolveColonParamRefs(parts []stringPart) []stringPart {
	if len(parts) == 0 {
		return parts
	}
	var out []stringPart
	for _, p := range parts {
		if p.literal == "" {
			out = append(out, p)
			continue
		}
		colonParts, hasColon := splitColonBindings(p.literal, tp)
		if !hasColon {
			out = append(out, p)
			continue
		}
		out = append(out, colonParts...)
	}
	return out
}

// splitColonBindings splits a literal on :varname bindings that resolve to
// declared vars, emitting alternating literal/variable parts. Returns the
// rebuilt part list and whether a colon binding was replaced.
func splitColonBindings(lit string, tp *transpiler) ([]stringPart, bool) {
	var parts []stringPart
	i := 0
	replaced := false
	for i < len(lit) {
		if lit[i] != ':' || i+1 >= len(lit) || !isVarStartChar(lit[i+1]) {
			// Not a :varname at this position — advance to next ':' or end.
			next := strings.IndexByte(lit[i+1:], ':')
			if next < 0 {
				parts = append(parts, stringPart{literal: lit[i:]})
				break
			}
			parts = append(parts, stringPart{literal: lit[i : i+1+next]})
			i += 1 + next
			continue
		}
		j := i + 1
		for j < len(lit) && isVarChar(lit[j]) {
			j++
		}
		name := lit[i+1 : j]
		goName := tclVarToGo(name)
		if isValidGoIdent(goName) && tp.isVarDeclared(goName) {
			// Flush preceding literal up to ':'.
			if i > 0 && len(parts) == 0 {
				// handled by earlier branch
			}
			// The literal segment before ':' is already emitted; split here.
			// Trim the already-emitted prefix: re-emit correctly.
			// Instead, emit the segment before ':' plus the ':' as literal,
			// then a variable part — but we've already emitted literals piecewise
			// above, so here just drop the ':' prefix and emit variable.
			// If parts ends with a literal that includes the prefix before ':',
			// it's already correct (we advanced chunk-wise). So just emit var.
			parts = append(parts, stringPart{variable: name})
			replaced = true
		} else {
			// Not a bound var — keep the whole :name as literal.
			end := j
			// Coalesce with previous literal if possible.
			if len(parts) > 0 && parts[len(parts)-1].variable == "" && parts[len(parts)-1].command == "" {
				parts[len(parts)-1].literal += lit[i:end]
			} else {
				parts = append(parts, stringPart{literal: lit[i:end]})
			}
		}
		i = j
	}
	// `i>0` above tried to flush prefix literally — but the loop now handles
	// it chunk-wise, so the `parts` already carries the right literals. Fix up:
	// the first element should contain the prefix before the first :varn.
	// The above loop's `next`-branch already emits the prefix correctly; no
	// further fixup needed.
	_ = i
	return parts, replaced
}

// buildSQLStringExprNoCmd is like buildSQLStringExpr but treats [...] as
// literal text instead of TCL command substitution. It implements the
// semantics of TCL `subst -nocommands` / preserving bracket-quoted SQL
// identifiers (e.g. [t1'x1], [4]) verbatim while still binding $var as SQL
// literals. SQL in a braced execsql/db eval word has no [cmd] substitution.
func (tp *transpiler) buildSQLStringExprNoCmd(s string) string {
	parts := parseStringPartsMode(s, true, true)
	parts = tp.resolveColonParamRefs(parts)
	parts = tp.resolveBindParamRefs(parts)
	return tp.renderStringExpr(parts, true)
}

// renderStringExpr assembles parsed string parts into a Go string
// concatenation. When sqlMode is true, $var/[cmd] parts are wrapped with the
// sqlLiteral() helper so their values become valid SQL literals.
func (tp *transpiler) renderStringExpr(parts []stringPart, sqlMode bool) string {
	// Build concatenation
	if len(parts) == 0 {
		return `""`
	}

	// If only one part and it's a literal
	if len(parts) == 1 && parts[0].variable == "" && parts[0].command == "" {
		return fmt.Sprintf("%q", parts[0].literal)
	}

	var result strings.Builder
	for i, p := range parts {
		if i > 0 {
			result.WriteString(" + ")
		}
		result.WriteString(tp.renderStringPart(p, sqlMode))
	}
	return result.String()
}

// renderStringPart renders one parsed string part (literal / $var / [cmd])
// into a Go string-expression segment.
func (tp *transpiler) renderStringPart(p stringPart, sqlMode bool) string {
	var b strings.Builder
	if p.literal != "" {
		b.WriteString(fmt.Sprintf("%q", p.literal))
	}
	if p.variable != "" {
		if p.literal != "" {
			b.WriteString(" + ")
		}
		b.WriteString(tp.renderVarPart(p.variable, sqlMode))
	}
	if p.command != "" {
		if p.literal != "" || p.variable != "" {
			b.WriteString(" + ")
		}
		expr := tp.cmdExpr(p.command)
		if sqlMode {
			b.WriteString("sqlLiteral(" + expr + ")")
		} else {
			b.WriteString(expr)
		}
	}
	return b.String()
}

// renderVarPart renders a $var reference: 'err' is the Go error type in the
// preamble (TCL assignments to 'err' are redirected to _err_tcl), 'db' is a
// *frigolite.DB handle (no string value). In sqlMode the value is wrapped in
// sqlLiteral(); an unset var renders as SQL NULL. A $arr($keyvar) reference
// (array name + dynamic key) renders as a runtime selection among the array's
// literal-key variables (arr_0, arr_1, ...).
func (tp *transpiler) renderVarPart(vn string, sqlMode bool) string {
	var inner string
	if base, key := splitTclArrayRef(vn); base != "" {
		inner = tp.arrayLookupExpr(base, key)
	} else if vn == "err" {
		inner = "_err_tcl"
	} else if vn == "db" {
		inner = `""`
	} else {
		inner = tclVarToGo(vn)
	}
	if sqlMode {
		if tp.unsetVars != nil && tp.unsetVars[vn] {
			// TCL `unset var` then `$var` in db eval binds SQL NULL.
			return "sqlLiteral(nil)"
		}
		return "sqlLiteral(" + inner + ")"
	}
	return inner
}

// splitTclArrayRef splits a TCL array reference "name(key)" into (name, key).
// Returns ("", "") when vn is not an array reference.
func splitTclArrayRef(vn string) (string, string) {
	idx := strings.Index(vn, "(")
	if idx <= 0 || !strings.HasSuffix(vn, ")") {
		return "", ""
	}
	base := vn[:idx]
	key := vn[idx+1 : len(vn)-1]
	if base == "" || key == "" {
		return "", ""
	}
	return base, key
}

// arrayLookupExpr builds a Go expression that selects among an array's
// literal-key Go variables (arr_0, arr_1, ...) by the runtime key variable. For
// a dynamic-key array (registered in arrayMapVars) it returns a map lookup
// arrMap[key] instead.
func (tp *transpiler) arrayLookupExpr(base, key string) string {
	if tp.arrayMapVars != nil && tp.arrayMapVars[base] {
		mapVar := tclVarToGo(base) + "Map"
		keyExpr := strings.TrimPrefix(key, "$")
		if keyExpr == key {
			// Literal key: $arr(3) → arrMap["3"]
			return fmt.Sprintf("%s[%q]", mapVar, key)
		}
		return fmt.Sprintf("%s[%s]", mapVar, tclVarToGo(keyExpr))
	}
	// Literal array keys resolve directly; only dynamic `$key` selectors
	// require a switch over tracked elements.
	if !strings.HasPrefix(key, "$") {
		return tclVarToGo(base + "(" + key + ")")
	}
	keyVar := strings.TrimPrefix(key, "$")
	keys := tp.arrayKeys[base]
	if len(keys) == 0 {
		// No tracked keys: fall back to the static name the old transpiler
		// produced so output still compiles.
		return tclVarToGo(base + "(" + key + ")")
	}
	var b strings.Builder
	b.WriteString("(func() string { switch ")
	// Route the selector through the same identifier sanitizer as every other
	// variable reference: a TCL var literally named `error` must become Go
	// `_error`, never the builtin type (rtree1 catch {db eval} error).
	b.WriteString(tclVarToGo(keyVar))
	b.WriteString(" { ")
	for _, k := range keys {
		b.WriteString("case ")
		b.WriteString(fmt.Sprintf("%q", k))
		b.WriteString(": return ")
		b.WriteString(tclVarToGo(base + "(" + k + ")"))
		b.WriteString("; ")
	}
	b.WriteString("default: return \"\" } }())")
	return b.String()
}

// renderListStringExpr renders parsed string parts into a Go TCL-list string
// expression: each variable value outside a TCL double-quoted section goes
// through tclListElem so empty values remain {} list elements (TCL [list]
// semantics); variables inside quotes are part of the quoted element and stay
// raw. The parts come from parseStringParts, which keeps TCL quote characters
// as literal parts — toggle on a literal `"` to track quote state.
func (tp *transpiler) renderListStringExpr(parts []stringPart) string {
	if len(parts) == 0 {
		return `""`
	}
	if len(parts) == 1 && parts[0].variable == "" && parts[0].command == "" {
		return fmt.Sprintf("%q", parts[0].literal)
	}
	var result strings.Builder
	inQuote := false
	for i, p := range parts {
		if i > 0 {
			result.WriteString(" + ")
		}
		result.WriteString(tp.renderListStringPart(p, &inQuote))
	}
	return result.String()
}

// renderListStringPart renders one parsed string part inside a TCL-list
// expression, toggling the double-quote state on literal quote characters.
func (tp *transpiler) renderListStringPart(p stringPart, inQuote *bool) string {
	var b strings.Builder
	if p.literal != "" {
		toggleQuoteState(p.literal, inQuote)
		b.WriteString(fmt.Sprintf("%q", p.literal))
	}
	if p.variable != "" {
		if p.literal != "" {
			b.WriteString(" + ")
		}
		b.WriteString(renderListVarPart(tclVarToGo(p.variable), *inQuote))
	}
	if p.command != "" {
		if p.literal != "" || p.variable != "" {
			b.WriteString(" + ")
		}
		if *inQuote {
			b.WriteString(tp.cmdExpr(p.command))
		} else {
			b.WriteString("tclListElem(" + tp.cmdExpr(p.command) + ")")
		}
	}
	return b.String()
}

// toggleQuoteState toggles the TCL double-quote state on each quote character
// in a literal part. Escaped quotes are rare in foreach lists; a simple
// per-char scan handles the common unescaped case.
func toggleQuoteState(lit string, inQuote *bool) {
	for _, ch := range lit {
		if ch == '"' {
			*inQuote = !*inQuote
		}
	}
}

// renderListVarPart renders a $var reference inside a TCL-list expression:
// outside quotes the value goes through tclListElem so empty values remain {}
// list elements (TCL [list] semantics); inside quotes it stays raw.
func renderListVarPart(vn string, inQuote bool) string {
	var inner string
	if vn == "err" {
		inner = "_err_tcl"
	} else if vn == "db" {
		inner = `""`
	} else {
		inner = vn
	}
	if inQuote {
		return inner
	}
	return "tclListElem(" + inner + ")"
}

// renderSubstNovarSQL renders the body of a `subst -novar { ... }` used in a
// SQL context (do_execsql_test / execsql). TCL subst -novar substitutes
// [cmd] but NOT $var; the $var refs are then bound as VALUES by db eval. So
// $var parts render as sqlLiteral(var) (a SQL value) while [cmd] parts render
// as raw SQL text — the command typically yields SQL syntax, e.g.
// `[set op]` produces a comparison operator.
func (tp *transpiler) renderSubstNovarSQL(s string) string {
	parts := parseStringParts(s, true)
	if len(parts) == 0 {
		return `""`
	}
	if len(parts) == 1 && parts[0].variable == "" && parts[0].command == "" {
		return fmt.Sprintf("%q", parts[0].literal)
	}
	var result strings.Builder
	for i, p := range parts {
		if i > 0 {
			result.WriteString(" + ")
		}
		result.WriteString(tp.renderSubstNovarPart(p))
	}
	return result.String()
}

// renderSubstNovarPart renders one parsed string part in a subst -novar SQL
// context.
func (tp *transpiler) renderSubstNovarPart(p stringPart) string {
	var b strings.Builder
	if p.literal != "" {
		b.WriteString(fmt.Sprintf("%q", p.literal))
	}
	if p.variable != "" {
		if p.literal != "" {
			b.WriteString(" + ")
		}
		b.WriteString(tp.renderSubstVarPart(tclVarToGo(p.variable)))
	}
	if p.command != "" {
		if p.literal != "" || p.variable != "" {
			b.WriteString(" + ")
		}
		b.WriteString(tp.cmdExpr(p.command))
	}
	return b.String()
}

// renderSubstVarPart renders a $var reference in a subst -novar SQL context:
// the value binds as a SQL literal; an unset var binds SQL NULL.
func (tp *transpiler) renderSubstVarPart(vn string) string {
	var inner string
	if vn == "err" {
		inner = "tclStr(err)"
	} else if vn == "db" {
		inner = `""`
	} else {
		inner = vn
	}
	if tp.unsetVars != nil && tp.unsetVars[vn] {
		// TCL `unset var` then `$var` in db eval binds SQL NULL.
		return "sqlLiteral(nil)"
	}
	return "sqlLiteral(" + inner + ")"
}

// stringPart is one segment of an interpolated TCL string.
type stringPart struct {
	literal  string
	variable string // non-empty if this is a $var reference
	command  string // non-empty if this is a [cmd] reference
}

// parseStringParts splits a TCL string into literal / $var / [cmd] parts.
// When sqlQuoted is true, text inside single-quoted SQL string literals is
// treated literally (a regex/glob class like '[Aa]' or '$' must not become a
// $var or [cmd] substitution), matching how TCL passes braced SQL to db eval.
func parseStringParts(s string, sqlQuoted bool) []stringPart {
	return parseStringPartsMode(s, sqlQuoted, false)
}

// parseStringPartsNoCmd is like parseStringParts but treats [...] as literal
// text rather than TCL command substitution (subst -nocommands semantics).
func parseStringPartsMode(s string, sqlQuoted, noCommands bool) []stringPart {
	// Quick scan: if no $ or [ or \, just quote it
	simple := true
	for i := 0; i < len(s); i++ {
		if s[i] == '$' || s[i] == '[' || s[i] == '\\' {
			simple = false
			break
		}
	}
	if simple {
		return []stringPart{{literal: s}}
	}
	p := &stringPartsParser{s: s, sqlQuoted: sqlQuoted, noCommands: noCommands}
	return p.parse()
}

// stringPartsParser walks a TCL string, splitting it into literal/variable/
// command parts while honoring SQL single-quote state and backslash escapes.
type stringPartsParser struct {
	s          string
	pos        int
	inSQL      bool // inside a single-quoted SQL string literal
	sqlQuoted  bool
	noCommands bool
	parts      []stringPart
}

// parse walks the input string and returns the parsed parts.
func (p *stringPartsParser) parse() []stringPart {
	for p.pos < len(p.s) {
		ch := p.s[p.pos]
		if p.handleChar(ch) {
			continue
		}
		p.appendLiteral(ch)
		p.pos++
	}
	// Clean up literal-only trailing/leading parts
	if len(p.parts) > 0 && p.parts[0].literal == "" && p.parts[0].variable == "" && p.parts[0].command == "" {
		p.parts = p.parts[1:]
	}
	return p.parts
}

// handleChar dispatches the current character to its parser. Returns true
// when the character was consumed by a special handler.
func (p *stringPartsParser) handleChar(ch byte) bool {
	if p.sqlQuoted && ch == '\'' {
		p.handleQuote()
		return true
	}
	if ch == '\\' && p.pos+1 < len(p.s) {
		p.handleEscape()
		return true
	}
	if ch == '$' && p.pos+1 < len(p.s) && !p.inSQL {
		p.handleDollar()
		return true
	}
	if ch == '[' && !p.inSQL && !p.noCommands {
		p.handleBracket()
		return true
	}
	return false
}

// appendLiteral adds a regular character to the current literal part.
func (p *stringPartsParser) appendLiteral(ch byte) {
	if len(p.parts) == 0 || p.parts[len(p.parts)-1].variable != "" || p.parts[len(p.parts)-1].command != "" {
		p.parts = append(p.parts, stringPart{})
	}
	p.parts[len(p.parts)-1].literal += string([]byte{ch})
}

// handleQuote toggles SQL single-quote state (SQL escapes ” as two adjacent
// quotes, so toggling twice IN→OUT→IN keeps us inside).
func (p *stringPartsParser) handleQuote() {
	p.inSQL = !p.inSQL
	p.appendLiteral('\'')
	p.pos++
}

// handleEscape processes a backslash escape: for the interpolation-sensitive
// chars ($ [ ] { }), TCL's backslash escape makes them literal, so drop the
// backslash (the escaped char must not become a $var or [cmd] substitution).
// Other escapes (e.g. \\ and \" that survive the upstream unescape) keep the
// backslash for the Go %q round-trip. Inside a SQL string literal (inSQL),
// backslashes are SQL text (e.g. a regex '\[', a LIKE '\%'), never TCL
// escapes, so preserve them verbatim.
func (p *stringPartsParser) handleEscape() {
	next := p.s[p.pos+1]
	p.pos += 2
	if len(p.parts) == 0 || p.parts[len(p.parts)-1].variable != "" || p.parts[len(p.parts)-1].command != "" {
		p.parts = append(p.parts, stringPart{})
	}
	last := &p.parts[len(p.parts)-1]
	if !p.inSQL {
		switch next {
		case '$', '[', ']', '{', '}':
			last.literal += string([]byte{next})
			return
		}
	}
	last.literal += string([]byte{'\\', next})
}

// handleDollar processes a $var reference (plain, ${braced}, or array
// $var(key) form).
func (p *stringPartsParser) handleDollar() {
	p.pos++
	varStart := p.pos
	if p.s[p.pos] == '{' {
		p.appendBracedVar()
		return
	}
	if isVarStartChar(p.s[p.pos]) {
		for p.pos < len(p.s) && isVarChar(p.s[p.pos]) {
			p.pos++
		}
		varName := p.s[varStart:p.pos]
		// Handle TCL array syntax: $var(key) → include key in var name
		varName = p.appendArrayKey(varName)
		p.parts = append(p.parts, stringPart{variable: varName})
		return
	}
	p.appendLiteral('$')
}

// appendBracedVar appends a ${varname} reference.
func (p *stringPartsParser) appendBracedVar() {
	p.pos++
	varStart := p.pos
	for p.pos < len(p.s) && p.s[p.pos] != '}' {
		p.pos++
	}
	varName := p.s[varStart:p.pos]
	if p.pos < len(p.s) {
		p.pos++ // skip }
	}
	p.parts = append(p.parts, stringPart{variable: varName})
}

// appendArrayKey extends varName with a TCL array key when the input has
// $var(key) syntax, advancing pos past the closing paren.
func (p *stringPartsParser) appendArrayKey(varName string) string {
	if p.pos >= len(p.s) || p.s[p.pos] != '(' {
		return varName
	}
	keyStart := p.pos + 1
	keyEnd := keyStart
	for keyEnd < len(p.s) && p.s[keyEnd] != ')' {
		keyEnd++
	}
	if keyEnd >= len(p.s) {
		return varName
	}
	key := p.s[keyStart:keyEnd]
	p.pos = keyEnd + 1 // skip past )
	return varName + "(" + key + ")"
}

// handleBracket processes a [cmd ...] command substitution, scanning to the
// matching close bracket (nested brackets are counted).
func (p *stringPartsParser) handleBracket() {
	depth := 1
	start := p.pos + 1
	p.pos++
	for p.pos < len(p.s) && depth > 0 {
		ch := p.s[p.pos]
		if ch == '[' {
			depth++
		} else if ch == ']' {
			depth--
		}
		if depth > 0 {
			p.pos++
		}
	}
	cmdText := p.s[start:p.pos]
	if p.pos < len(p.s) {
		p.pos++ // skip ]
	}
	// A bracket containing a SINGLE identifier character is literal text,
	// not a command substitution: the TCL test corpus spells snippet/offsets
	// highlight markers as `[N]` `[K]` etc. inside brace-quoted lists
	// (fts4content 2.4: snippet(ft2, '[', ']', ...) expects "..O B [N] [K]
	// [N] E.."). A real command has a word with length > 1 (e.g. `[list ...]`,
	// `[string length $x]`) or a $/space. Treating the single-char bracket as
	// literal preserves the markers; the generic command path still handles
	// every real command form.
	if len(cmdText) == 1 && cmdText[0] != ' ' && cmdText[0] != '$' && cmdText[0] != '[' && cmdText[0] != ']' {
		p.appendLiteral('[')
		p.appendLiteral(cmdText[0])
		p.appendLiteral(']')
		return
	}
	p.parts = append(p.parts, stringPart{command: cmdText})
}
