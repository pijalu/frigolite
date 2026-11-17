// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// tclExprVal is a value produced by the harness Tcl-expression evaluator.
// kind is 'i' (int), 'f' (float), or 's' (string).
type tclExprVal struct {
	kind byte
	i    int64
	f    float64
	s    string
}

func intVal(v int64) tclExprVal     { return tclExprVal{kind: 'i', i: v} }
func floatVal(v float64) tclExprVal { return tclExprVal{kind: 'f', f: v} }
func strVal(v string) tclExprVal    { return tclExprVal{kind: 's', s: v} }

// format renders the value the way Tcl's expr command does.
func (v tclExprVal) format() string {
	switch v.kind {
	case 'i':
		return strconv.FormatInt(v.i, 10)
	case 'f':
		s := strconv.FormatFloat(v.f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEnN") {
			s += ".0"
		}
		return s
	default:
		return v.s
	}
}

// numeric returns the value's numeric form, reporting whether it can be
// interpreted as a number (Tcl's "looks like a number" rule).
func (v tclExprVal) numeric() (float64, bool) {
	switch v.kind {
	case 'i':
		return float64(v.i), true
	case 'f':
		return v.f, true
	default:
		s := strings.TrimSpace(v.s)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
}

// integer returns the value as an int64 when it is integral.
func (v tclExprVal) integer() (int64, bool) {
	switch v.kind {
	case 'i':
		return v.i, true
	case 'f':
		if v.f == math.Trunc(v.f) && v.f >= math.MinInt64 && v.f <= math.MaxInt64 {
			return int64(v.f), true
		}
		return 0, false
	default:
		s := strings.TrimSpace(v.s)
		ival, err := strconv.ParseInt(s, 0, 64)
		if err == nil {
			return ival, true
		}
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr == nil && f == math.Trunc(f) && f >= math.MinInt64 && f <= math.MaxInt64 {
			return int64(f), true
		}
		return 0, false
	}
}

// truthy applies Tcl's boolean interpretation.
func (v tclExprVal) truthy() (bool, error) {
	if num, ok := v.numeric(); ok {
		return num != 0, nil
	}
	switch strings.ToLower(strings.TrimSpace(v.s)) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("expected boolean value but got %q", v.format())
}

// evalHarnessTclExpr evaluates the Tcl expression subset used by converted
// do_test bodies whose final command is `expr` over runtime-captured values
// (e.g. date.test:545-548). Supported: $var / $::var references resolved from
// vars, numbers, double-quoted and braced strings, boolean literals,
// parentheses, unary ! + -, arithmetic + - * / %, comparison == != < > <= >=
// plus the string forms eq/ne, logical && ||, and the ?: ternary. Unsupported
// syntax returns an error so the assertion fails loudly instead of silently
// passing.
func evalHarnessTclExpr(expr string, vars map[string]string) (string, error) {
	return evalHarnessTclExprResolving(expr, vars, nil)
}

// evalHarnessTclExprResolving is evalHarnessTclExpr with a lifecycle path
// resolver for the command layer's `file size <name>` (the resolver maps the
// fixture's relative database name onto its lifecycle path).
func evalHarnessTclExprResolving(expr string, vars map[string]string, resolvePath func(string) string) (string, error) {
	p := &tclExprParser{input: expr, vars: vars, pathResolve: resolvePath}
	v, err := p.parseTernary()
	if err != nil {
		return "", err
	}
	p.skipSpace()
	if p.pos != len(p.input) {
		return "", fmt.Errorf("unexpected trailing text %q", p.input[p.pos:])
	}
	return v.format(), nil
}

type tclExprParser struct {
	input       string
	pos         int
	vars        map[string]string
	pathResolve func(string) string
}

func (p *tclExprParser) skipSpace() {
	for p.pos < len(p.input) && unicode.IsSpace(rune(p.input[p.pos])) {
		p.pos++
	}
}

// matchKeyword matches a bare keyword at the current position, ensuring a word
// boundary follows it.
func (p *tclExprParser) matchKeyword(word string) bool {
	p.skipSpace()
	end := p.pos + len(word)
	if end > len(p.input) || p.input[p.pos:end] != word {
		return false
	}
	if end < len(p.input) {
		c := rune(p.input[end])
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' {
			return false
		}
	}
	p.pos = end
	return true
}

func (p *tclExprParser) matchSymbol(sym string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.input[p.pos:], sym) {
		p.pos += len(sym)
		return true
	}
	return false
}

func (p *tclExprParser) parseTernary() (tclExprVal, error) {
	cond, err := p.parseOr()
	if err != nil {
		return tclExprVal{}, err
	}
	if !p.matchSymbol("?") {
		return cond, nil
	}
	whenTrue, err := p.parseTernary()
	if err != nil {
		return tclExprVal{}, err
	}
	if !p.matchSymbol(":") {
		return tclExprVal{}, fmt.Errorf("missing : in ternary expression")
	}
	whenFalse, err := p.parseTernary()
	if err != nil {
		return tclExprVal{}, err
	}
	truth, err := cond.truthy()
	if err != nil {
		return tclExprVal{}, err
	}
	if truth {
		return whenTrue, nil
	}
	return whenFalse, nil
}

func (p *tclExprParser) parseOr() (tclExprVal, error) {
	left, err := p.parseAnd()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.input[p.pos:], "||") {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseAnd()
		if err != nil {
			return tclExprVal{}, err
		}
		lt, err := left.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		rt, err := right.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		left = intVal(boolToInt(lt || rt))
	}
}

func (p *tclExprParser) parseAnd() (tclExprVal, error) {
	left, err := p.parseComparison()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.input[p.pos:], "&&") {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseComparison()
		if err != nil {
			return tclExprVal{}, err
		}
		lt, err := left.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		rt, err := right.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		left = intVal(boolToInt(lt && rt))
	}
}

// comparisonOps lists the comparison operators longest-first so prefixes like
// `<` do not shadow `<=`.
var comparisonOps = []string{"<=", ">=", "==", "!=", "<", ">", "eq", "ne"}

func (p *tclExprParser) parseComparison() (tclExprVal, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		op := ""
		for _, candidate := range comparisonOps {
			if len(candidate) == 2 && candidate[0] >= 'a' && candidate[0] <= 'z' {
				if p.matchKeyword(candidate) {
					op = candidate
					break
				}
				continue
			}
			if p.matchSymbol(candidate) {
				op = candidate
				break
			}
		}
		if op == "" {
			return left, nil
		}
		right, err := p.parseAdditive()
		if err != nil {
			return tclExprVal{}, err
		}
		truth, err := compareTclValues(left, right, op)
		if err != nil {
			return tclExprVal{}, err
		}
		left = intVal(boolToInt(truth))
	}
}

func (p *tclExprParser) parseAdditive() (tclExprVal, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		var op byte
		p.skipSpace()
		if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
			op = p.input[p.pos]
			p.pos++
		} else {
			return left, nil
		}
		right, err := p.parseMultiplicative()
		if err != nil {
			return tclExprVal{}, err
		}
		left, err = arithmeticTclValues(left, right, op)
		if err != nil {
			return tclExprVal{}, err
		}
	}
}

func (p *tclExprParser) parseMultiplicative() (tclExprVal, error) {
	left, err := p.parseUnary()
	if err != nil {
		return tclExprVal{}, err
	}
	for {
		var op byte
		p.skipSpace()
		if p.pos < len(p.input) && (p.input[p.pos] == '*' || p.input[p.pos] == '/' || p.input[p.pos] == '%') {
			op = p.input[p.pos]
			p.pos++
		} else {
			return left, nil
		}
		right, err := p.parseUnary()
		if err != nil {
			return tclExprVal{}, err
		}
		left, err = arithmeticTclValues(left, right, op)
		if err != nil {
			return tclExprVal{}, err
		}
	}
}

func (p *tclExprParser) parseUnary() (tclExprVal, error) {
	if p.matchSymbol("!") {
		v, err := p.parseUnary()
		if err != nil {
			return tclExprVal{}, err
		}
		truth, err := v.truthy()
		if err != nil {
			return tclExprVal{}, err
		}
		return intVal(boolToInt(!truth)), nil
	}
	if p.matchSymbol("-") {
		v, err := p.parseUnary()
		if err != nil {
			return tclExprVal{}, err
		}
		iv, ok := v.integer()
		if ok {
			return intVal(-iv), nil
		}
		fv, ok := v.numeric()
		if !ok {
			return tclExprVal{}, fmt.Errorf("can't negate %q", v.format())
		}
		return floatVal(-fv), nil
	}
	if p.matchSymbol("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *tclExprParser) parsePrimary() (tclExprVal, error) {
	p.skipSpace()
	if p.pos >= len(p.input) {
		return tclExprVal{}, fmt.Errorf("unexpected end of expression")
	}
	switch c := p.input[p.pos]; {
	case c == '(':
		p.pos++
		v, err := p.parseTernary()
		if err != nil {
			return tclExprVal{}, err
		}
		if !p.matchSymbol(")") {
			return tclExprVal{}, fmt.Errorf("missing closing parenthesis")
		}
		return v, nil
	case c == '"':
		return p.parseQuotedString()
	case c == '{':
		return p.parseBracedString()
	case c == '$':
		return p.parseVariable()
	case c == '[':
		return p.parseCommandSubstitution()
	case unicode.IsDigit(rune(c)) || c == '.':
		return p.parseNumber()
	case unicode.IsLetter(rune(c)) || c == '_':
		return p.parseIdentifier()
	default:
		return tclExprVal{}, fmt.Errorf("unexpected character %q", string(c))
	}
}

func (p *tclExprParser) parseQuotedString() (tclExprVal, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if c == '\\' && p.pos+1 < len(p.input) {
			p.pos++
			b.WriteByte(tclBackslash(p.input[p.pos]))
			p.pos++
			continue
		}
		if c == '"' {
			p.pos++
			return strVal(b.String()), nil
		}
		b.WriteByte(c)
		p.pos++
	}
	return tclExprVal{}, fmt.Errorf("unterminated quoted string")
}

func (p *tclExprParser) parseBracedString() (tclExprVal, error) {
	p.pos++ // opening brace
	depth := 1
	var b strings.Builder
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		switch c {
		case '\\':
			if p.pos+1 < len(p.input) {
				b.WriteByte(c)
				b.WriteByte(p.input[p.pos+1])
				p.pos += 2
				continue
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				p.pos++
				return strVal(b.String()), nil
			}
		}
		b.WriteByte(c)
		p.pos++
	}
	return tclExprVal{}, fmt.Errorf("unterminated braced string")
}

func (p *tclExprParser) parseVariable() (tclExprVal, error) {
	p.pos++ // $
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' || c == ':' {
			p.pos++
			continue
		}
		if c == '(' {
			depth := 1
			end := p.pos + 1
			for end < len(p.input) && depth > 0 {
				if p.input[end] == '(' {
					depth++
				} else if p.input[end] == ')' {
					depth--
				}
				end++
			}
			p.pos = end
			continue
		}
		break
	}
	name := p.input[start:p.pos]
	if name == "" {
		return tclExprVal{}, fmt.Errorf("empty variable reference")
	}
	value, ok := p.vars[strings.TrimPrefix(name, "::")]
	if !ok {
		value, ok = p.vars[name]
	}
	if !ok {
		return tclExprVal{}, fmt.Errorf("runtime variable %q is not set", name)
	}
	return strVal(value), nil
}

func (p *tclExprParser) parseNumber() (tclExprVal, error) {
	start := p.pos
	sawDot := false
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if unicode.IsDigit(rune(c)) {
			p.pos++
			continue
		}
		if c == '.' && !sawDot {
			sawDot = true
			p.pos++
			continue
		}
		if (c == 'e' || c == 'E') && p.pos+1 < len(p.input) {
			p.pos++
			if p.input[p.pos] == '+' || p.input[p.pos] == '-' {
				p.pos++
			}
			continue
		}
		break
	}
	text := p.input[start:p.pos]
	if !sawDot {
		if iv, err := strconv.ParseInt(text, 0, 64); err == nil {
			return intVal(iv), nil
		}
	}
	fv, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return tclExprVal{}, fmt.Errorf("bad number %q", text)
	}
	return floatVal(fv), nil
}

func (p *tclExprParser) parseIdentifier() (tclExprVal, error) {
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' {
			p.pos++
			continue
		}
		break
	}
	word := p.input[start:p.pos]
	switch strings.ToLower(word) {
	case "true", "yes", "on":
		return intVal(1), nil
	case "false", "no", "off":
		return intVal(0), nil
	}
	if value, ok := p.vars[word]; ok {
		return strVal(value), nil
	}
	return tclExprVal{}, fmt.Errorf("unknown identifier %q", word)
}

// tclBackslash maps a backslash escape to its byte.
func tclBackslash(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	default:
		return c
	}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func TestCompareTclValuesMixedNumericAndStringFallsBackToString(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		op          string
		want        bool
	}{
		{"1", "", "==", false},
		{"1", "", "!=", true},
		{"10", "2x", "<", true},
	} {
		got, err := compareTclValues(strVal(tc.left), strVal(tc.right), tc.op)
		if err != nil || got != tc.want {
			t.Errorf("compare(%q %s %q) = %v, %v; want %v", tc.left, tc.op, tc.right, got, err, tc.want)
		}
	}
}

// compareTclValues implements Tcl comparison semantics for the evaluator.
// eq/ne always compare string representations; ==/!=/</>/<=/>= compare
// numerically when both operands look like numbers and as strings otherwise.
func compareTclValues(left, right tclExprVal, op string) (bool, error) {
	switch op {
	case "eq":
		return left.format() == right.format(), nil
	case "ne":
		return left.format() != right.format(), nil
	}
	lnum, lok := left.numeric()
	rnum, rok := right.numeric()
	if lok && rok {
		switch op {
		case "==":
			return lnum == rnum, nil
		case "!=":
			return lnum != rnum, nil
		case "<":
			return lnum < rnum, nil
		case ">":
			return lnum > rnum, nil
		case "<=":
			return lnum <= rnum, nil
		case ">=":
			return lnum >= rnum, nil
		}
	}
	if !lok || !rok {
		lstr, rstr := left.format(), right.format()
		switch op {
		case "==":
			return lstr == rstr, nil
		case "!=":
			return lstr != rstr, nil
		case "<":
			return lstr < rstr, nil
		case ">":
			return lstr > rstr, nil
		case "<=":
			return lstr <= rstr, nil
		case ">=":
			return lstr >= rstr, nil
		}
	}
	return false, fmt.Errorf("can't compare %q with %q using %s", left.format(), right.format(), op)
}

// arithmeticTclValues applies + - * / % with Tcl integer semantics when both
// operands are integral.
func arithmeticTclValues(left, right tclExprVal, op byte) (tclExprVal, error) {
	li, lok := left.integer()
	ri, rok := right.integer()
	if lok && rok {
		switch op {
		case '+':
			return intVal(li + ri), nil
		case '-':
			return intVal(li - ri), nil
		case '*':
			return intVal(li * ri), nil
		case '/':
			if ri == 0 {
				return tclExprVal{}, fmt.Errorf("divide by zero")
			}
			return intVal(floorDiv(li, ri)), nil
		case '%':
			if ri == 0 {
				return tclExprVal{}, fmt.Errorf("divide by zero")
			}
			return intVal(floorMod(li, ri)), nil
		}
	}
	lf, lok2 := left.numeric()
	rf, rok2 := right.numeric()
	if !lok2 || !rok2 {
		return tclExprVal{}, fmt.Errorf("can't use %q and %q with operator %s", left.format(), right.format(), string(op))
	}
	switch op {
	case '+':
		return floatVal(lf + rf), nil
	case '-':
		return floatVal(lf - rf), nil
	case '*':
		return floatVal(lf * rf), nil
	case '/':
		if rf == 0 {
			return tclExprVal{}, fmt.Errorf("divide by zero")
		}
		return floatVal(lf / rf), nil
	case '%':
		if rf == 0 {
			return tclExprVal{}, fmt.Errorf("divide by zero")
		}
		return floatVal(math.Mod(lf, rf)), nil
	}
	return tclExprVal{}, fmt.Errorf("unknown operator %q", string(op))
}

// floorDiv implements Tcl's integer division (results round toward negative
// infinity).
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// floorMod implements Tcl's modulo (result carries the divisor's sign).
func floorMod(a, b int64) int64 {
	m := a % b
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

// TestHarnessTclExprEvaluator pins the expression subset the harness must
// evaluate for converted runtime_expr assertions.
func TestHarnessTclExprEvaluator(t *testing.T) {
	vars := map[string]string{
		"date":  "2008-06-12 00:00:00",
		"date2": "2008-06-11 23:59:59",
		"x":     "5",
		"s":     "abc",
	}
	cases := []struct {
		expr string
		want string
	}{
		{`$date eq "2008-06-12 00:00:00" || $date eq "2008-06-11 23:59:59"`, "1"},
		{`$date2 eq "2008-06-12 00:00:00" || $date2 eq "2008-06-11 23:59:59"`, "1"},
		{`$s eq "2008-06-12 00:00:00" || $s eq "2008-06-11 23:59:59"`, "0"},
		{`$x < 10`, "1"},
		{`$x < 10 && $x > 0`, "1"},
		{`1 + 2`, "3"},
		{`7 / 2`, "3"},
		{`-7 / 2`, "-4"},
		{`7 % 3`, "1"},
		{`-7 % 3`, "2"},
		{`!($x == 5)`, "0"},
		{`$s eq "abc" ? 10 : 20`, "10"},
		{`1.0`, "1.0"},
		{`2.5 * 2`, "5.0"},
		{`"5" == 5`, "1"},
		{`{braced} eq "braced"`, "1"},
	}
	for _, c := range cases {
		got, err := evalHarnessTclExpr(c.expr, vars)
		if err != nil {
			t.Errorf("evalHarnessTclExpr(%q) error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("evalHarnessTclExpr(%q) = %q, want %q", c.expr, got, c.want)
		}
	}

	errorCases := []string{
		`$missing eq "x"`,
		`unknownIdent eq "x"`,
		`$x <`,
	}
	for _, expr := range errorCases {
		if got, err := evalHarnessTclExpr(expr, vars); err == nil {
			t.Errorf("evalHarnessTclExpr(%q) = %q, want error", expr, got)
		}
	}
}

// --- the runtime command layer ---

// parseCommandSubstitution parses and evaluates one `[cmd arg ...]`
// substitution in a runtime expression (vacuum2's
// `[file size test.db]/$pageSize`, bestindexF's `[lsearch $vm IdxInsert]`):
// the command text runs to the matching bracket, splits into Tcl words with
// $var and nested [cmd] substitutions applied, and dispatches through
// evalHarnessTclCommand. An unknown command errors loudly (the subst lesson:
// silent emptiness hides real gaps).
func (p *tclExprParser) parseCommandSubstitution() (tclExprVal, error) {
	text, err := p.readBracketed()
	if err != nil {
		return tclExprVal{}, err
	}
	words, err := splitHarnessTclCommandWords(text, p.vars, p.pathResolve)
	if err != nil {
		return tclExprVal{}, err
	}
	if len(words) == 0 {
		return tclExprVal{}, fmt.Errorf("empty command substitution")
	}
	out, err := evalHarnessTclCommand(words, p.vars, p.pathResolve)
	if err != nil {
		return tclExprVal{}, err
	}
	return harnessCommandValue(out), nil
}

// readBracketed consumes the `[` at the current position and returns the
// command text up to the matching `]` (brackets, quotes and braces nest).
func (p *tclExprParser) readBracketed() (string, error) {
	if p.pos >= len(p.input) || p.input[p.pos] != '[' {
		return "", fmt.Errorf("expected [ command substitution")
	}
	start := p.pos + 1
	depth := 1
	for i := start; i < len(p.input); i++ {
		switch p.input[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				p.pos = i + 1
				return p.input[start:i], nil
			}
		case '"':
			if j := skipTclQuoted(p.input, i); j > i {
				i = j - 1
			}
		case '{':
			if j := skipTclBraced(p.input, i); j > i {
				i = j - 1
			}
		}
	}
	return "", fmt.Errorf("unterminated [ command substitution")
}

// skipTclQuoted returns the index just past the quoted string starting at i
// (backslash escapes honored), or i when unterminated.
func skipTclQuoted(text string, i int) int {
	i++ // opening quote
	for i < len(text) {
		if text[i] == '\\' {
			i += 2
			continue
		}
		if text[i] == '"' {
			return i + 1
		}
		i++
	}
	return i
}

// skipTclBraced returns the index just past the braced body starting at i
// (braces nest; no escape processing inside braces), or i when unterminated.
func skipTclBraced(text string, i int) int {
	depth := 0
	for i < len(text) {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return i
}

// splitHarnessTclCommandWords splits one command's text into Tcl words:
// whitespace-separated, with braced bodies verbatim, quoted words carrying
// their substitutions, and bare words carrying $var and [cmd] substitution.
func splitHarnessTclCommandWords(text string, vars map[string]string, resolvePath func(string) string) ([]string, error) {
	var words []string
	i := 0
	for i < len(text) {
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r' || text[i] == ';') {
			i++
		}
		if i >= len(text) {
			break
		}
		word, next, err := readTclCommandWord(text, i, vars, resolvePath)
		if err != nil {
			return nil, err
		}
		words = append(words, word)
		i = next
	}
	return words, nil
}

// readTclCommandWord reads one Tcl word starting at i and returns it with the
// index of the next word's start.
func readTclCommandWord(text string, i int, vars map[string]string, resolvePath func(string) string) (string, int, error) {
	var b strings.Builder
	switch text[i] {
	case '{':
		end := skipTclBraced(text, i)
		if end <= i {
			return "", i, fmt.Errorf("unterminated braced word")
		}
		return text[i+1 : end-1], end, nil
	case '"':
		end := skipTclQuoted(text, i)
		if end <= i {
			return "", i, fmt.Errorf("unterminated quoted word")
		}
		body := text[i+1 : end-1]
		out, err := substituteTclWord(body, vars, resolvePath)
		if err != nil {
			return "", i, err
		}
		return out, end, nil
	}
	for i < len(text) && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' && text[i] != ';' {
		switch {
		case text[i] == '{':
			end := skipTclBraced(text, i)
			if end <= i {
				return "", i, fmt.Errorf("unterminated braced word")
			}
			b.WriteString(text[i:end])
			i = end
		case text[i] == '"':
			end := skipTclQuoted(text, i)
			if end <= i {
				return "", i, fmt.Errorf("unterminated quoted word")
			}
			out, err := substituteTclWord(text[i+1:end-1], vars, resolvePath)
			if err != nil {
				return "", i, err
			}
			b.WriteString(out)
			i = end
		case text[i] == '[':
			depth := 0
			start := i
			for i < len(text) {
				if text[i] == '[' {
					depth++
				} else if text[i] == ']' {
					depth--
					if depth == 0 {
						i++
						break
					}
				} else if text[i] == '"' {
					if j := skipTclQuoted(text, i); j > i {
						i = j - 1
					}
				}
				i++
			}
			inner := text[start:i]
			if !strings.HasSuffix(inner, "]") {
				return "", i, fmt.Errorf("unterminated [ command substitution in word")
			}
			out, err := evalHarnessTclCommandText(inner[1:len(inner)-1], vars, resolvePath)
			if err != nil {
				return "", i, err
			}
			b.WriteString(out)
		case text[i] == '$':
			out, next, err := readTclVarRef(text, i, vars)
			if err != nil {
				return "", i, err
			}
			b.WriteString(out)
			i = next
		case text[i] == '\\' && i+1 < len(text):
			b.WriteByte(tclBackslash(text[i+1]))
			i += 2
		default:
			b.WriteByte(text[i])
			i++
		}
	}
	return b.String(), i, nil
}

// readTclVarRef reads a $name / $name(key) reference at i and returns its
// value with the index past the reference (an absent variable renders empty —
// Tcl's substitution semantics).
func readTclVarRef(text string, i int, vars map[string]string) (string, int, error) {
	start := i + 1
	end := start
	for end < len(text) {
		c := text[end]
		if unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' || c == ':' {
			end++
			continue
		}
		if c == '(' {
			depth := 1
			end++
			for end < len(text) && depth > 0 {
				if text[end] == '(' {
					depth++
				} else if text[end] == ')' {
					depth--
				}
				end++
			}
			break
		}
		break
	}
	name := text[start:end]
	if name == "" {
		return "", i + 1, nil // a bare $ renders empty (Tcl)
	}
	if v, ok := vars[strings.TrimPrefix(name, "::")]; ok {
		return v, end, nil
	}
	if v, ok := vars[name]; ok {
		return v, end, nil
	}
	return "", end, nil
}

// substituteTclWord applies $var and [cmd] substitution inside a quoted word
// body (backslash escapes honored).
func substituteTclWord(body string, vars map[string]string, resolvePath func(string) string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(body); {
		switch {
		case body[i] == '\\' && i+1 < len(body):
			b.WriteByte(tclBackslash(body[i+1]))
			i += 2
		case body[i] == '[':
			end := skipTclBracketed(body, i)
			if end <= i {
				return "", fmt.Errorf("unterminated [ command substitution")
			}
			out, err := evalHarnessTclCommandText(body[i+1:end-1], vars, resolvePath)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
			i = end
		case body[i] == '$':
			out, next, err := readTclVarRef(body, i, vars)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
			i = next
		default:
			b.WriteByte(body[i])
			i++
		}
	}
	return b.String(), nil
}

// skipTclBracketed returns the index just past the [ ... ] group starting at
// i (nested brackets and quotes honored), or i when unterminated.
func skipTclBracketed(text string, i int) int {
	depth := 0
	for i < len(text) {
		switch text[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '"':
			if j := skipTclQuoted(text, i); j > i {
				i = j - 1
			}
		case '{':
			if j := skipTclBraced(text, i); j > i {
				i = j - 1
			}
		}
		i++
	}
	return i
}

// evalHarnessTclCommandText evaluates one [command] body (the brackets
// stripped): word-split then dispatch.
func evalHarnessTclCommandText(text string, vars map[string]string, resolvePath func(string) string) (string, error) {
	words, err := splitHarnessTclCommandWords(text, vars, resolvePath)
	if err != nil {
		return "", err
	}
	if len(words) == 0 {
		return "", nil // a bare empty substitution renders empty
	}
	return evalHarnessTclCommand(words, vars, resolvePath)
}

// evalHarnessTclCommand dispatches the runtime command layer's supported
// commands (the Tcl subset the converted fixtures use in runtime
// expressions). Anything else errors loudly.
func evalHarnessTclCommand(words []string, vars map[string]string, resolvePath func(string) string) (string, error) {
	switch strings.ToLower(words[0]) {
	case "lsearch":
		return harnessCmdLsearch(words)
	case "lindex":
		return harnessCmdLindex(words)
	case "join":
		return harnessCmdJoin(words)
	case "array":
		return harnessCmdArrayNames(words, vars)
	case "file":
		return harnessCmdFileSize(words, resolvePath)
	case "expr":
		if len(words) < 2 {
			return "", fmt.Errorf("expr: wrong # of arguments")
		}
		return evalHarnessTclExprResolving(strings.Join(words[1:], " "), vars, resolvePath)
	default:
		return "", fmt.Errorf("invalid command name %q in runtime expression", words[0])
	}
}

// harnessCmdLsearch implements `lsearch ?-exact|-glob? list needle`
// (Tcl's default mode is -glob). The result is the first matching element's
// index, or -1.
func harnessCmdLsearch(words []string) (string, error) {
	glob := true
	args := words[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch strings.ToLower(args[0]) {
		case "-glob":
			glob = true
		case "-exact":
			glob = false
		default:
			return "", fmt.Errorf("lsearch: unsupported mode %q", args[0])
		}
		args = args[1:]
	}
	if len(args) != 2 {
		return "", fmt.Errorf("lsearch: wrong # of arguments")
	}
	list := tcl.SplitList(args[0])
	needle := args[1]
	for i, el := range list {
		if glob {
			if tcl.StringMatch(needle, el) {
				return strconv.Itoa(i), nil
			}
			continue
		}
		if el == needle {
			return strconv.Itoa(i), nil
		}
	}
	return "-1", nil
}

// harnessCmdLindex implements `lindex list index` (one index level: the
// fixtures' `lindex $vm [expr $ii+4]` shape; an out-of-range index renders
// empty per Tcl).
func harnessCmdLindex(words []string) (string, error) {
	if len(words) < 2 {
		return "", fmt.Errorf("lindex: wrong # of arguments")
	}
	list := tcl.SplitList(words[1])
	if len(words) == 2 {
		return strings.Join(list, " "), nil
	}
	idx, convErr := strconv.Atoi(strings.TrimSpace(words[2]))
	if convErr != nil {
		return "", fmt.Errorf("lindex: bad index %q", words[2])
	}
	if idx < 0 || idx >= len(list) {
		return "", nil
	}
	return list[idx], nil
}

// harnessCmdJoin implements `join list ?joiner?` (the elements joined with
// joiner, default a single space).
func harnessCmdJoin(words []string) (string, error) {
	if len(words) < 2 || len(words) > 3 {
		return "", fmt.Errorf("join: wrong # of arguments")
	}
	elements := tcl.SplitList(words[1])
	sep := " "
	if len(words) == 3 {
		sep = words[2]
	}
	return strings.Join(elements, sep), nil
}

// harnessCmdArrayNames implements `array names ?arr?` over the flat-key
// runtime model: every vars entry "name(key)" contributes key (sorted, like
// the harness's deterministic rendering of Tcl's unordered answer).
func harnessCmdArrayNames(words []string, vars map[string]string) (string, error) {
	if len(words) < 2 {
		return "", fmt.Errorf("array names: wrong # of arguments")
	}
	if strings.ToLower(words[1]) != "names" {
		return "", fmt.Errorf("array: unsupported subcommand %q", words[1])
	}
	if len(words) < 3 {
		return "", fmt.Errorf("array names: wrong # of arguments")
	}
	name := strings.TrimPrefix(words[2], "::")
	prefix := name + "("
	var keys []string
	for k := range vars {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, ")") {
			keys = append(keys, k[len(prefix):len(k)-1])
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aerr := strconv.Atoi(keys[i])
		b, berr := strconv.Atoi(keys[j])
		if aerr == nil && berr == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return strings.Join(keys, " "), nil
}

// harnessCmdFileSize implements `file size <name>`: the lifecycle database
// path's byte size (the resolver maps the fixture's relative name; a nil
// resolver errors loudly).
func harnessCmdFileSize(words []string, resolvePath func(string) string) (string, error) {
	if len(words) < 2 {
		return "", fmt.Errorf("file: wrong # of arguments")
	}
	if strings.ToLower(words[1]) != "size" {
		return "", fmt.Errorf("file: unsupported subcommand %q", words[1])
	}
	if len(words) < 3 {
		return "", fmt.Errorf("file size: wrong # of arguments")
	}
	if resolvePath == nil {
		return "", fmt.Errorf("file size %q: no lifecycle path resolver in this context", words[2])
	}
	info, err := os.Stat(resolvePath(words[2]))
	if err != nil {
		return "", fmt.Errorf("file size %q: %v", words[2], err)
	}
	return strconv.FormatInt(info.Size(), 10), nil
}

// harnessCommandValue converts a command's string answer to an expression
// value: a number keeps its type, anything else is a string.
func harnessCommandValue(out string) tclExprVal {
	text := strings.TrimSpace(out)
	if text == "" {
		return strVal(out)
	}
	if iv, err := strconv.ParseInt(text, 10, 64); err == nil {
		return intVal(iv)
	}
	if fv, err := strconv.ParseFloat(text, 64); err == nil {
		return floatVal(fv)
	}
	return strVal(out)
}

// harnessRuntimeCommands lists the command layer's head words: a runtime_expr
// whose first word is one of these evaluates as a COMMAND (`join [array
// names ::T] ,` — index9's set-in line) rather than as an expression.
var harnessRuntimeCommands = map[string]bool{
	"lsearch": true, "lindex": true, "join": true,
	"array": true, "file": true, "expr": true,
}

// firstWordIsRuntimeCommand reports whether the text's leading word is one of
// the runtime command layer's commands.
func firstWordIsRuntimeCommand(text string) bool {
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	start := i
	for i < len(text) && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' && text[i] != ';' {
		i++
	}
	return harnessRuntimeCommands[strings.ToLower(text[start:i])]
}

// evalHarnessRuntimeExpr evaluates one runtime_expr value: a command head
// routes through the command layer, everything else through the expression
// evaluator.
func evalHarnessRuntimeExpr(text string, vars map[string]string, resolvePath func(string) string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if firstWordIsRuntimeCommand(trimmed) {
		return evalHarnessTclCommandText(trimmed, vars, resolvePath)
	}
	return evalHarnessTclExprResolving(trimmed, vars, resolvePath)
}
