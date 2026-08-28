// Package main implements the tcl2go tool.
//
// This file normalizes expected-result words for db eval comparisons.
package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// isBareTCLVarRef reports whether s is a bare TCL variable reference such as
// "$sql" or "$::sql" (a `$` followed by an optionally namespace-qualified
// identifier, with nothing else).
func isBareTCLVarRef(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "$") {
		return false
	}
	rest := strings.TrimPrefix(s[1:], "::")
	if rest == "" {
		return false
	}
	if !isTCLIdentifier(rest) {
		return false
	}
	return true
}

// isTCLIdentifier reports whether s is a valid TCL identifier (starts with a
// letter or underscore, continues with letters, digits, or underscores).
func isTCLIdentifier(s string) bool {
	for i, r := range s {
		if i == 0 {
			if !isTCLIdentStart(r) {
				return false
			}
			continue
		}
		if !isTCLIdentPart(r) {
			return false
		}
	}
	return true
}

func isTCLIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isTCLIdentPart(r rune) bool {
	return isTCLIdentStart(r) || (r >= '0' && r <= '9')
}

// normalizeExpectedWord collapses whitespace in a braced do_test expected
// value so it matches the space-joined query result (TCL lists normalize
// whitespace). It also unwraps one layer of TCL list-rendering braces: a
// single {...} group spanning the whole value is how TCL renders a one-element
// list (e.g. 'Mass in B Minor, BWV 232' → {Mass in B Minor, BWV 232}) and
// flatten() produces the element without those rendering braces. Values with
// structural separators ('|' for row separators, '/' or '~' for regex
// patterns, '=' for key=value) are left as-is.
func normalizeExpectedWord(w tcl.RawWord) (tcl.RawWord, bool) {
	// The second return reports whether the word was already normalized to
	// its final flat form (brace-list flattening); callers must then NOT
	// apply the runtime tclListFlatten strip again.
	if !w.Braced {
		// An unbraced double-quoted expected word (json101 9.4's "null") is
		// a TCL quoted list element: its value is the unquoted, unescaped
		// content. Words carrying substitution ($, [, ;) keep their runtime
		// handling.
		if w.Quoted {
			text := strings.TrimSpace(w.Text)
			if !strings.ContainsAny(text, "$[;") {
				if v, ok := tclElementValue(text); ok && v != text {
					return tcl.RawWord{Text: v, Braced: true}, true
				}
			}
		}
		return w, false
	}
	text := strings.TrimSpace(w.Text)
	// The TCL test framework processes the expected value with substitution
	// (TCL `subst`-like unescaping): `\"` becomes `"`. Mirror that so a
	// braced expected like {\"\"\"} (collate1 6.1, the triple-quote collation
	// name) normalizes to the raw `"""` value that flatten() produces.
	text, changed := unescapeExpectedText(text)
	if text == "" {
		// An empty braced expected value means an empty result set; the
		// generated want should be the empty string, not the raw whitespace.
		return tcl.RawWord{Text: "", Braced: true}, false
	}
	// Unwrap TCL list-rendering braces for a single-element list. A `{}` with
	// empty inner content is NOT rendering braces: it is how TCL db eval
	// renders a one-element list containing NULL/"" (and flatten() renders
	// NULL as "{}"), so leave it as-is.
	text, unwrapped := unwrapSingleBraceGroup(text)
	// TCL catchsql regex expected value: `{/1 <pattern>/}` (basexx1 118-119's
	// `/1.*too big.*/`, with2 6.7-6.9's `/1 {near .* syntax error}/`). The
	// leading "/1" and trailing "/" must survive to the catchsql regex-form
	// detection in emitCatchSQLComparison, so return the raw text (without
	// the surrounding braces) instead of falling into the structural-content
	// branch that would keep the braced word.
	if strings.HasPrefix(text, "/1") && strings.HasSuffix(text, "/") {
		return tcl.RawWord{Text: text, Braced: true}, false
	}
	// Multi-element TCL list: db eval renders each result row as a braced
	// element, so a multi-row expected value is a list of braced strings.
	// flatten() produces the space-joined unbraced form, so split the list
	// (respecting nested braces) and join the elements with single spaces.
	// This must run before the structural-preservation checks below, which
	// would otherwise keep the raw braces for lists containing '=' (e.g. a
	// row value like CHECK (c!="null")). Only brace-delimited lists are
	// flattened — bare multi-field words (e.g. "1 4 9") keep their existing
	// handling to minimize churn.
	if strings.Contains(text, "{") {
		if flat, ok := flattenBraceList(text); ok {
			return tcl.RawWord{Text: flat, Braced: true}, true
		}
	}
	// Preserve structural content.
	if hasStructuralContent(text) {
		if unwrapped || changed {
			return tcl.RawWord{Text: text, Braced: true}, false
		}
		return w, false
	}
	// Collapse internal whitespace to single spaces (multi-row results in
	// do_test expected values are space-joined by flatten()). A single
	// unwrapped braced element keeps its internal whitespace verbatim — the
	// spaces are part of the cell value (e.g. printf2-5.100's '(       ⭢)').
	if unwrapped {
		return tcl.RawWord{Text: text, Braced: true}, false
	}
	fields := strings.Fields(text)
	if len(fields) < 2 {
		// A single-field braced expected value: the TCL test framework treats
		// the braced block as a one-element list, and the surrounding
		// whitespace (e.g. "{\n  0\n}") is list formatting, not part of the
		// cell value. flatten() renders the single value with no newlines, so
		// emit the trimmed text. When the single field contains internal
		// whitespace (e.g. printf2-5.100's "(       ⭢)"), the spaces ARE part
		// of the cell value and must be preserved verbatim.
		if len(fields) == 1 && fields[0] == text {
			// A bare or quoted single element is parsed by TCL with its
			// escapes processed / quotes stripped (json101 1.1.01's
			// `[1,"{\\"abc\\"...}",99]` → single-backslash form; 9.4's
			// "null" quoted string). Braced elements stay verbatim — the
			// braces ARE the value quoting (json101 8.1's `{{[...\u0001...]}}`).
			if v, ok := tclElementValue(text); ok && v != text {
				return tcl.RawWord{Text: v, Braced: true}, true
			}
			return tcl.RawWord{Text: text, Braced: true}, false
		}
		if changed {
			return tcl.RawWord{Text: text, Braced: true}, false
		}
		return w, false
	}
	return tcl.RawWord{Text: strings.Join(fields, " "), Braced: true}, false
}

// tclElementValue computes the VALUE of a single TCL list element per its
// quoting form, mirroring TCL list parsing: a quoted element ("...") is
// unquoted with escapes processed, a bare element with backslashes has its
// escapes processed (\\ → \, \" → "), and anything else (braced elements,
// bare words without escapes) is returned verbatim. ok=false when a quoted
// element does not parse (unescaped inner quote).
func tclElementValue(text string) (string, bool) {
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		inner := text[1 : len(text)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' {
				i++
				continue
			}
			if inner[i] == '"' {
				return "", false
			}
		}
		return unescapeBareWord(inner), true
	}
	// A braced element is verbatim — its braces are the value's own quoting
	// (JSON output like {"a":1,"b":[2]} spelled {{"a":1,"b":[2]}}).
	if strings.HasPrefix(text, "{") {
		return text, true
	}
	if strings.Contains(text, `\`) {
		return unescapeBareWord(text), true
	}
	return text, true
}

// unwrapSingleBraceGroup unwraps one layer of TCL list-rendering braces from
// a single-element list, trimming only trailing whitespace (a trailing
// newline/spaces is TCL list formatting; a leading space is part of the cell
// value like like-16.1's ' 1x'). Returns the (possibly unwrapped) text and
// whether it was unwrapped.
func unwrapSingleBraceGroup(text string) (string, bool) {
	if !isSingleBraceGroup(text) {
		return text, false
	}
	inner := text[1 : len(text)-1]
	if strings.TrimSpace(inner) == "" {
		return text, false
	}
	return strings.TrimRight(inner, " \t\n\r"), true
}

// unescapeExpectedText applies the TCL subst-like unescaping to a braced
// expected value: `\"` → `"`, `\ ` → ` ` (escaped list spaces), and
// backslash-newline → space (list line continuation). Returns the unescaped
// text and whether any substitution happened.
func unescapeExpectedText(text string) (string, bool) {
	changed := false
	// TCL braced words do NOT process backslash escapes, so `\"` inside a
	// braced expected is literal data. Only unescape when the ENTIRE content
	// consists solely of quotes/backslashes (collate1 6.1's {\"\"\"} →
	// """); otherwise the escapes are part of the value itself
	// (json101-8.1's [...!\"#xyz] must keep its backslash).
	if strings.Contains(text, `\"`) {
		// Only when every character is a quote, backslash or blank is this
		// a subst-style escaped scalar (collate1 6.1) rather than JSON data.
		cutset := " " + `\` + `"`
		trimmed := strings.Trim(text, cutset)
		if trimmed == "" {
			text = strings.ReplaceAll(text, `\"`, `"`)
			changed = true
		}
	}
	if strings.Contains(text, `\ `) {
		text = strings.ReplaceAll(text, `\ `, ` `)
		changed = true
	}
	if strings.Contains(text, "\\\n") {
		text = strings.ReplaceAll(text, "\\\n", " ")
		changed = true
	}
	return text, changed
}

// flattenBraceList splits a brace-delimited TCL list and joins its elements
// with single spaces (the flatten() rendering). Returns ok=false when the
// text is not a multi-element braced list.
func flattenBraceList(text string) (string, bool) {
	if elems := tclSplitList(text); len(elems) > 1 {
		var parts []string
		for _, e := range elems {
			raw := e
			// A braced element's trailing newline is PART of the cell value
			// (basexx1.test's base64/base85 output ends with '\n'), not TCL
			// list formatting: tclSplitList already consumed the separator
			// whitespace between elements, so what remains inside the braces
			// is the exact value db eval returns. Do NOT trim it.
			if e == "" {
				// tclSplitList returns the INNER content of each braced
				// element, so a `{}` element (db eval's rendering of a
				// NULL / empty-string row) arrives as the empty string.
				// flatten() renders NULL as "{}", so keep it as `{}` —
				// dropping it would corrupt the expected value (e.g.
				// `{{} 1 {} 2}` must stay `{} 1 {} 2`, not ` 1  2`).
				// Distinguish `{}` (empty, TCL NULL/empty rendering)
				// from `{ }` (a single space string, which is a real
				// value — collate1 8.2 stores a space and expects it).
				if strings.TrimSpace(raw) == "" && strings.Contains(raw, " ") {
					parts = append(parts, " ")
				} else {
					parts = append(parts, "{}")
				}
				continue
			}
			parts = append(parts, e)
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

// hasStructuralContent reports whether an expected value carries structural
// separators that must be preserved verbatim: '|' for row separators, '/' or
// '~' for regex patterns, '=' for key=value, and multi-line SQL schema
// statements.
func hasStructuralContent(text string) bool {
	// A multi-line result list: '=' inside it is cell data (fts4unicode
	// 10.1: the term ".single=word"), not key=value structure — only the
	// regex/~ forms are structural.
	if strings.Contains(text, "\n") {
		return hasStructuralSeparatorMultiLine(text) || isSQLSchemaStatement(text)
	}
	if hasStructuralSeparator(text) {
		return true
	}
	return false
}

// hasStructuralSeparator reports whether text carries a structural separator:
// '~' or the /.../ form for regex patterns, '=' for key=value. '|' is NOT a
// structural separator here — it is literal data in many tests (e.g.
// "SELECT ..., '|'" output columns such as returning1 and window1 28.2.2), and
// flatten() space-joins it like any other cell value. The slash form must be a
// complete /pattern/ (both ends), matching isTCLRegexPattern — a bare leading
// slash is a filesystem path (e.g. the with1 6.2 expected list of /bin paths)
// and must not block list flattening.
func hasStructuralSeparator(text string) bool {
	if strings.Contains(text, "~") || strings.Contains(text, "=") {
		return true
	}
	t := strings.TrimSpace(text)
	return len(t) >= 2 && t[0] == '/' && t[len(t)-1] == '/'
}

// hasStructuralSeparatorMultiLine is hasStructuralSeparator for multi-line
// expected values. '=' is EXCLUDED: in a multi-line result list an '=' is
// cell data (fts4unicode 10.1: the term ".single=word"), not a key=value
// structure; only the regex/~/SQL-schema structural forms survive.
func hasStructuralSeparatorMultiLine(text string) bool {
	if strings.Contains(text, "~") {
		return true
	}
	t := strings.TrimSpace(text)
	return len(t) >= 2 && t[0] == '/' && t[len(t)-1] == '/'
}

// isSQLSchemaStatement reports whether a string begins with a SQL DDL
// statement keyword whose stored form (sqlite_schema.sql) keeps its original
// newlines verbatim (CREATE TABLE/VIEW/TRIGGER/INDEX, ALTER, DROP).
func isSQLSchemaStatement(s string) bool {
	t := strings.TrimSpace(strings.ToUpper(s))
	for _, kw := range []string{"CREATE ", "ALTER ", "DROP ", "INSERT ", "SELECT ", "UPDATE ", "DELETE "} {
		if strings.HasPrefix(t, kw) {
			return true
		}
	}
	return false
}

// dbEvalExpected detects the TCL "[db eval { SQL }]" pattern used as a
// do_test expected value and returns the SQL to run (the query result,
// flattened, is the expected value). It also handles the nested
// "[db eval [subst -novar { SQL }]]" form: subst -novar substitutes [cmd]
// but leaves $var for db eval to bind. The second return value reports
// whether the SQL came from a subst -novar wrapper (callers then render
// [cmd] raw and $var as SQL literals).
func dbEvalExpected(w tcl.RawWord) (string, bool, bool, bool) {
	text := strings.TrimSpace(w.Text)
	// The command substitution may have newlines inside the brackets
	// (e.g. "[\n  db eval \"SQL\"\n]") — normalize to "[db eval ...".
	if strings.HasPrefix(text, "[") {
		text = "[" + strings.Join(strings.Fields(text[1:]), " ")
	}
	if strings.HasPrefix(text, "[db eval ") && strings.HasSuffix(text, "]") {
		inner := strings.TrimSpace(text[len("[db eval ") : len(text)-1])
		isSubst := false
		quoted := false
		if resolved, ok := substNovarBody(inner); ok {
			inner = resolved
			isSubst = true
		}
		if strings.HasPrefix(inner, "{") && strings.HasSuffix(inner, "}") {
			inner = strings.TrimSpace(inner[1 : len(inner)-1])
		} else {
			// TCL line continuations (backslash-newline) may precede the SQL;
			// strip them before the quoted/braced detection.
			inner = strings.ReplaceAll(inner, "\\n", " ")
			inner = strings.ReplaceAll(inner, "\\", " ")
			inner = strings.Join(strings.Fields(inner), " ")
			if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
				// Double-quoted db eval SQL: $var substitutes as RAW TEXT (TCL
				// string substitution before db eval), e.g. rowvalue2's expected
				// [db eval "SELECT ... WHERE $e1 ..."].
				inner = inner[1 : len(inner)-1]
				quoted = true
			} else if isBareTCLVarRef(inner) {
				// `db eval $sql` — the variable holds the SQL text itself
				// (TCL substitutes $sql before db eval runs), e.g. orderby6's
				// do_test ... [db eval $sql1]. Render the variable raw, never
				// as a bound sqlLiteral.
				quoted = true
			}
		}
		if inner != "" {
			return inner, isSubst, quoted, true
		}
	}
	return "", false, false, false
}

// substNovarBody detects the "[subst -novar { ... }]" (or bare
// "subst -novar { ... }") wrapper and returns the braced body with $var
// references preserved (db eval binds them at runtime). Returns ok=false if
// text is not a subst -novar form.
func substNovarBody(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, "subst"))
	// Strip any flags (-nobackslashes, -nocommands, -novar).
	for strings.HasPrefix(rest, "-") {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		flag := fields[0]
		if flag != "-novar" && flag != "-nobackslashes" && flag != "-nocommands" {
			return "", false
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, flag))
	}
	if !strings.HasPrefix(rest, "{") || !strings.HasSuffix(rest, "}") {
		return "", false
	}
	body := rest[1 : len(rest)-1]
	if strings.TrimSpace(body) == "" {
		return "", false
	}
	return body, true
}

// isSingleBracedStructuredLiteral reports whether the Go string literal expr
// holds exactly one fully-braced element whose inner content is structured
// data (JSON/braced punctuation). flatten() emits the raw cell text for such
// values, so the expectation must be compared verbatim: running it through
// tclListFlatten would strip the DATA braces (json101-2.x '{"a":1,...}').
func isSingleBracedStructuredLiteral(expr string) bool {
	if len(expr) < 2 || !strings.HasPrefix(expr, `"`) || !strings.HasSuffix(expr, `"`) {
		return false
	}
	unq, err := strconv.Unquote(expr)
	if err != nil {
		return false
	}
	s := strings.TrimSpace(unq)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 && i != len(s)-1 {
				return false
			}
		}
	}
	if depth != 0 {
		return false
	}
	inner := s[1 : len(s)-1]
	return strings.ContainsAny(inner, "{}\"[:")
}

// expectLiteral normalizes an expected-result word into a Go string literal.
// When normalizeExpectedWord already flattened a braced TCL list into its
// final space-joined form, tp.expectPreFlattened is set and callers must
// emit the literal directly instead of wrapping it in tclListFlatten (which
// would strip the data braces a second time).
func (tp *transpiler) expectLiteral(w tcl.RawWord) string {
	nw, flat := normalizeExpectedWord(w)
	tp.expectPreFlattened = flat
	return tp.goStringLiteral(nw)
}
