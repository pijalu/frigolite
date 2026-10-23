package function

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

func fnPRINTF(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	format := toString(args[0])
	goArgs := make([]interface{}, len(args)-1)
	copy(goArgs, args[1:])
	return sqlitePrintf(format, goArgs), nil
}

// sqlitePrintf implements SQLite's printf (src/printf.c), which differs from
// C/Go printf in several ways:
//   - The ',' flag inserts thousands separators into %d/%u/%f/%g/%e output.
//   - The '!' flag (altform2) is SQLite-specific: for %g it renders with the
//     full 20 significant digits and forces a decimal point; for %s it treats
//     width/precision as UTF-8 characters.
//   - %q escapes single quotes (” doubling), %Q quotes as '...' (NULL → the
//     text NULL, no quotes), %w escapes double quotes.
//   - Floating-point rendering matches SQLite's FpDecode (16 significant
//     digits default, 20 with the '!' flag), NOT C printf rounding.
//   - Precision/width are clamped to SQLITE_PRINTF_PRECISION_LIMIT (1e8).
//
// The implementation delegates standard conversions to Go's fmt with a
// SQLite-compatible pre-pass, then post-processes the SQLite-specific flags.
func sqlitePrintf(format string, args []interface{}) string {
	var out strings.Builder
	argi := 0
	for i := 0; i < len(format); {
		c := format[i]
		if c != '%' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(format) {
			out.WriteByte('%')
			break
		}
		flags, j := parsePrintfFlags(format, i+1)
		width, lj, j := parsePrintfWidth(format, args, &argi, j)
		if lj {
			flags.leftJustify = true
		}
		precision, j := parsePrintfPrecision(format, args, &argi, j)
		for j < len(format) && format[j] == 'l' {
			j++
		}
		if j >= len(format) {
			out.WriteString("%")
			break
		}
		verb := format[j]
		j++
		var val interface{}
		// %n does not consume an argument (SQLite etSIZE: it reports the
		// character count to a C location; the SQL function renders nothing).
		if verb != 'n' && argi < len(args) {
			val = args[argi]
			argi++
		}
		out.WriteString(sqliteFormatVerb(verb, flags.leftJustify, flags.prefix, flags.alt, flags.alt2, flags.zero, flags.thousand, width, precision, val))
		i = j
	}
	return out.String()
}

// printfFlags carries the parsed %-conversion flag characters.
type printfFlags struct {
	leftJustify bool
	prefix      byte
	alt, alt2   bool
	zero        bool
	thousand    bool
}

// parsePrintfFlags consumes the flag characters of a %-conversion starting at
// format[j] and returns the flags plus the index of the first non-flag byte.
func parsePrintfFlags(format string, j int) (printfFlags, int) {
	var f printfFlags
	for j < len(format) {
		switch format[j] {
		case '-':
			f.leftJustify = true
		case '+':
			f.prefix = '+'
		case ' ':
			if f.prefix == 0 {
				f.prefix = ' '
			}
		case '#':
			f.alt = true
		case '!':
			f.alt2 = true
		case '0':
			f.zero = true
		case ',':
			f.thousand = true
		default:
			return f, j
		}
		j++
	}
	return f, j
}

// parsePrintfDigits consumes decimal digits starting at format[j] and returns
// the parsed value (clamped to SQLITE_PRINTF_PRECISION_LIMIT) and the index
// after the digits.
func parsePrintfDigits(format string, j int) (int, int) {
	n := 0
	for j < len(format) && format[j] >= '0' && format[j] <= '9' {
		n = n*10 + int(format[j]-'0')
		if n > 100000000 {
			n = 100000000
		}
		j++
	}
	return n, j
}

// parsePrintfWidth consumes the width field (a '*' argument or digits) of a
// %-conversion. A negative '*' width selects left justification. Returns the
// width, whether left-justification was forced, and the next index.
func parsePrintfWidth(format string, args []interface{}, argi *int, j int) (int, bool, int) {
	width := 0
	leftJustify := false
	if j < len(format) && format[j] == '*' {
		if *argi < len(args) {
			width = int(toInt64(args[*argi]))
			*argi++
		}
		if width < 0 {
			leftJustify = true
			width = -width
		}
		if width > 100000000 {
			width = 100000000
		}
		return width, leftJustify, j + 1
	}
	width, j = parsePrintfDigits(format, j)
	return width, leftJustify, j
}

// parsePrintfPrecision consumes the optional ".precision" field of a
// %-conversion. Returns the precision (-1 when absent) and the next index.
func parsePrintfPrecision(format string, args []interface{}, argi *int, j int) (int, int) {
	precision := -1
	if j >= len(format) || format[j] != '.' {
		return precision, j
	}
	j++
	precision = 0
	if j < len(format) && format[j] == '*' {
		if *argi < len(args) {
			precision = int(toInt64(args[*argi]))
			*argi++
		}
		if precision < 0 {
			precision = -precision
		}
		if precision > 100000000 {
			precision = 100000000
		}
		return precision, j + 1
	}
	return parsePrintfDigits(format, j)
}

// applyPrintfPrecision truncates s to `precision` bytes (or UTF-8 characters
// when alt2/'!' is set). A negative precision leaves s unchanged.
func applyPrintfPrecision(s string, precision int, alt2 bool) string {
	if precision < 0 {
		return s
	}
	if alt2 {
		runes := []rune(s)
		if precision < len(runes) {
			return string(runes[:precision])
		}
	} else if precision < len(s) {
		return s[:precision]
	}
	return s
}

// sqliteFormatVerb renders a single %conversion with SQLite printf semantics.
// val is the (possibly missing) argument. Missing arguments render as 0/""
// like SQLite's getIntArg/getDoubleArg/getTextArg return for exhausted lists.
func sqliteFormatVerb(verb byte, leftJustify bool, prefix byte, alt, alt2, zero, thousand bool, width, precision int, val interface{}) string {
	// Normalize missing arguments.
	missing := val == nil
	switch verb {
	case 'd', 'i', 'u', 'o', 'x', 'X':
		return sqliteFormatInt(verb, leftJustify, prefix, alt, alt2, zero, thousand, width, precision, val, missing)
	case 'f', 'e', 'E', 'g', 'G':
		return sqliteFormatFloat(verb, leftJustify, prefix, alt, alt2, zero, thousand, width, precision, val, missing)
	case 's', 'z', 'J', 'j':
		return sqliteFormatString(leftJustify, alt, alt2, zero, width, precision, val, missing)
	case 'q', 'Q', 'w':
		return sqliteFormatEscape(verb, leftJustify, alt, alt2, width, precision, val, missing)
	case '%':
		return "%"
	case 'n':
		// %n is silently ignored and does NOT consume an argument.
		return ""
	case 'p':
		// %p is an alias for %X (uppercase hex, no 0x prefix).
		return sqliteFormatInt('X', leftJustify, prefix, alt, alt2, zero, thousand, width, precision, val, missing)
	case 'c':
		return sqliteFormatChar(leftJustify, zero, width, precision, val, missing)
	case 'r':
		return renderOrdinal(missing, val)
	default:
		// Unknown conversion: SQLite returns "%!<verb>" for invalid types
		// (Go renders %!(BADVERB)); match SQLite's %!(<verb>...) form loosely
		// by emitting the C-style marker.
		return fmt.Sprintf("%%!(%c)", verb)
	}
}

// renderOrdinal renders %r (1st, 2nd, ...) — used internally by SQLite's VDBE
// explain output; render the number plus ordinal suffix.
func renderOrdinal(missing bool, val interface{}) string {
	n := int64(0)
	if !missing {
		n = toInt64(val)
	}
	s := strconv.FormatInt(n, 10)
	x := int(n % 10)
	if x >= 4 || (n/10)%10 == 1 {
		x = 0
	}
	return s + "thstndrd"[x*2:x*2+2]
}

// unsignedIntString renders an unsigned integer conversion (%u %o %x %X).
func unsignedIntString(verb byte, u uint64) string {
	switch verb {
	case 'o':
		return strconv.FormatUint(u, 8)
	case 'x':
		return strconv.FormatUint(u, 16)
	case 'X':
		return strings.ToUpper(strconv.FormatUint(u, 16))
	default: // u
		return strconv.FormatUint(u, 10)
	}
}

// unsignedAltPrefix returns the alternate-form prefix (0x/0X/0) for unsigned
// conversions; suppressed for zero values.
func unsignedAltPrefix(verb byte, n int64, alt bool) string {
	if !alt || n == 0 {
		return ""
	}
	switch verb {
	case 'x':
		return "0x"
	case 'X':
		return "0X"
	case 'o':
		return "0"
	}
	return ""
}

// sqliteFormatInt renders integer conversions %d %i %u %o %x %X %c %p.
func sqliteFormatInt(verb byte, leftJustify bool, prefix byte, alt, alt2, zero, thousand bool, width, precision int, val interface{}, missing bool) string {
	if verb == 'u' || verb == 'o' || verb == 'x' || verb == 'X' {
		var n int64
		if !missing {
			n = toInt64(val)
		}
		s := unsignedIntString(verb, uint64(n))
		pre := unsignedAltPrefix(verb, n, alt)
		if thousand && verb == 'u' {
			s = addThousands(s, ',')
		}
		return padInt(s, pre, "", leftJustify, zero, width, precision)
	}
	// Signed: d i c p
	n := int64(0)
	if !missing {
		n = toInt64(val)
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	} else if prefix != 0 {
		sign = string(prefix)
	}
	s := strconv.FormatInt(n, 10)
	if thousand {
		s = addThousands(s, ',')
	}
	return padInt(s, sign, "", leftJustify, zero, width, precision)
}

// padFloatField pads a float rendering to `width` (SQLite pads the whole
// field; zero-padding is suppressed for exponent forms).
func padFloatField(s string, width int, leftJustify, zero bool) string {
	if width <= len(s) {
		return s
	}
	pad := width - len(s)
	if leftJustify {
		return s + strings.Repeat(" ", pad)
	}
	if zero && !strings.ContainsAny(s, "eE") {
		return strings.Repeat("0", pad) + s
	}
	return strings.Repeat(" ", pad) + s
}

// sqliteFormatFloat renders %f %e %E %g %G with SQLite's FpDecode semantics.
func sqliteFormatFloat(verb byte, leftJustify bool, prefix byte, alt, alt2, zero, thousand bool, width, precision int, val interface{}, missing bool) string {
	r := 0.0
	if !missing {
		f, err := toFloat64(val)
		if err == nil {
			r = f
		}
	}
	if precision < 0 {
		precision = 6
	}
	// SQLite's FpDecode: 16 significant digits by default, 26 with '!'.
	maxSig := 16
	if alt2 {
		maxSig = 26
	}
	s := renderSQLiteFloat(verb, r, precision, alt, alt2, maxSig)
	// Sign prefix (sign already embedded for negatives).
	if r >= 0 && prefix != 0 && s != "NaN" && s != "Inf" && s != "-Inf" {
		s = string(prefix) + s
	}
	if thousand {
		s = insertThousandsFloat(s, ',')
	}
	return padFloatField(s, width, leftJustify, zero)
}

// sqliteFormatString renders %s (and %z/%J/%j) with SQLite semantics: NULL →
// "", precision truncates bytes (or characters with '!'), width pads.
func sqliteFormatString(leftJustify, alt, alt2, zero bool, width, precision int, val interface{}, missing bool) string {
	s := ""
	if !missing && val != nil {
		s = toString(val)
	}
	s = applyPrintfPrecision(s, precision, alt2)
	// Width: SQLite counts bytes for %s, but with '!' counts characters.
	count := len(s)
	if alt2 {
		count = len([]rune(s))
	}
	if width > count {
		pad := width - count
		if leftJustify {
			s += strings.Repeat(" ", pad)
		} else {
			s = strings.Repeat(" ", pad) + s
		}
	}
	return s
}

// sqliteFormatChar renders %c: a single UTF-8 character. When the argument is
// a string, the first character is used (SQLite's getTextArg); the precision
// causes the character to repeat. NULL/missing renders the NUL character.
func sqliteFormatChar(leftJustify, zero bool, width, precision int, val interface{}, missing bool) string {
	ch := ""
	if !missing && val != nil {
		switch v := val.(type) {
		case int64:
			// %c of an INTEGER uses the codepoint (printf('%c',65) → 'A').
			ch = string(rune(v))
		case float64:
			ch = string(rune(int64(v)))
		default:
			s := toString(val)
			if s != "" {
				ch = string([]rune(s)[0])
			}
		}
	}
	// Precision: repeat the character `precision` times (SQLite etCHARX).
	n := 1
	if precision > 1 {
		n = precision
	}
	s := strings.Repeat(ch, n)
	count := len([]rune(s))
	if width > count {
		pad := width - count
		if leftJustify {
			s += strings.Repeat(" ", pad)
		} else if zero {
			s = strings.Repeat("0", pad) + s
		} else {
			s = strings.Repeat(" ", pad) + s
		}
	}
	return s
}

// escapeSQLString applies the SQL escaping rule for %q (single-quote doubling),
// %Q (quoted, NULL handled by caller) and %w (double-quote doubling).
func escapeSQLString(verb byte, s string) string {
	switch verb {
	case 'Q':
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	case 'q':
		return strings.ReplaceAll(s, "'", "''")
	default: // w
		return strings.ReplaceAll(s, "\"", "\"\"")
	}
}

// sqliteFormatEscape renders %q %Q %w (SQL escaping conversions).
func sqliteFormatEscape(verb byte, leftJustify, alt, alt2 bool, width, precision int, val interface{}, missing bool) string {
	var s string
	if missing || val == nil {
		if verb == 'Q' {
			return "NULL" // %Q: NULL → the text NULL (no quotes)
		}
		s = "(NULL)" // %q/%w: NULL → (NULL)
	} else {
		s = toString(val)
	}
	s = applyPrintfPrecision(s, precision, alt2)
	s = escapeSQLString(verb, s)
	// Width: SQLite counts characters when the '!' flag is present (the
	// escaping output's length in characters, not bytes).
	count := len(s)
	if alt2 {
		count = len([]rune(s))
	}
	if width > count {
		pad := width - count
		if leftJustify {
			s += strings.Repeat(" ", pad)
		} else {
			s = strings.Repeat(" ", pad) + s
		}
	}
	return s
}

// padInt pads an integer rendering to width with zero/space and applies
// precision digit-count padding.
func padInt(s, sign, pre string, leftJustify, zero bool, width, precision int) string {
	if precision >= 0 && precision > len(s) {
		s = strings.Repeat("0", precision-len(s)) + s
	}
	full := sign + pre + s
	if width > len(full) {
		pad := width - len(full)
		if leftJustify {
			full += strings.Repeat(" ", pad)
		} else if zero {
			full = sign + pre + strings.Repeat("0", pad) + s
		} else {
			full = strings.Repeat(" ", pad) + full
		}
	}
	return full
}

// addThousands inserts a thousands separator into an integer string.
func addThousands(s string, sep byte) string {
	sign := ""
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		sign = s[:1]
		s = s[1:]
	}
	var b strings.Builder
	b.WriteString(sign)
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(sep)
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// insertThousandsFloat inserts thousands separators into the integer part of
// a float rendering (the digits before '.' or 'e').
func insertThousandsFloat(s string, sep byte) string {
	i := strings.IndexAny(s, ".eE")
	if i < 0 {
		i = len(s)
	}
	intPart := s[:i]
	rest := s[i:]
	// intPart may start with a sign.
	sign := ""
	if len(intPart) > 0 && (intPart[0] == '-' || intPart[0] == '+') {
		sign = intPart[:1]
		intPart = intPart[1:]
	}
	return sign + addThousands(intPart, sep) + rest
}

// renderSQLiteFloat renders a float with SQLite printf semantics.
//
// It uses Go's strconv for the core digit generation (which matches SQLite's
// FpDecode for the common cases — both produce the shortest/rounded decimal)
// then applies SQLite's %f/%e/%g formatting rules and its '!' and '#' flags.
func renderSQLiteFloat(verb byte, r float64, precision int, alt, alt2 bool, maxSig int) string {
	// Special values.
	if math.IsNaN(r) {
		return "NaN"
	}
	if math.IsInf(r, 1) {
		return "Inf"
	}
	if math.IsInf(r, -1) {
		return "-Inf"
	}
	switch verb {
	case 'f':
		return renderFloatF(r, precision, alt, alt2, maxSig)
	case 'e', 'E':
		return renderFloatE(r, precision, alt, alt2, verb == 'E', maxSig)
	case 'g', 'G':
		return renderFloatG(r, precision, alt, alt2, verb == 'G', maxSig)
	}
	return strconv.FormatFloat(r, 'g', -1, 64)
}

// renderFloatF renders %f: fixed-point with `precision` digits after the
// decimal point. SQLite always shows the decimal point and rounds using the
// FpDecode significant digits (so e.g. %.0f of 0.9 is "1"). The '#' flag
// keeps a trailing decimal point when precision is 0 ("0.").
func renderFloatF(r float64, precision int, alt, alt2 bool, maxSig int) string {
	if alt2 {
		return renderDecodedFloatF(r, precision, maxSig)
	}
	s := strconv.FormatFloat(r, 'f', precision, 64)
	if alt && !strings.Contains(s, ".") && precision == 0 {
		s += "."
	}
	return s
}

// renderDecodedFloatF uses SQLite's double-double decimal decoder for %!f.
func renderDecodedFloatF(r float64, precision, maxSig int) string {
	d := decodeSQLiteFloat(r, -precision, maxSig)
	var out strings.Builder
	if d.negative {
		out.WriteByte('-')
	}
	if d.decimalPoint <= 0 {
		out.WriteByte('0')
	} else {
		for pos := d.decimalPoint - 1; pos >= 0; pos-- {
			out.WriteByte(digitAt(d.digits, pos))
		}
	}
	out.WriteByte('.')
	for pos := d.decimalPoint; pos < d.decimalPoint+precision; pos++ {
		out.WriteByte(digitAt(d.digits, pos))
	}
	fixed := out.String()
	if dot := strings.IndexByte(fixed, '.'); dot >= 0 {
		fixed = strings.TrimRight(fixed, "0")
		if strings.HasSuffix(fixed, ".") {
			fixed += "0"
		}
	}
	return fixed
}

type sqliteFloatDigits struct {
	digits       []byte
	decimalPoint int
	negative     bool
}

func digitAt(digits []byte, pos int) byte {
	if pos < 0 || pos >= len(digits) {
		return '0'
	}
	return digits[pos]
}

// decodeSQLiteFloat mirrors sqlite3FpDecode's scaling, extraction, and rounding.
func decodeSQLiteFloat(value float64, round, maxRound int) sqliteFloatDigits {
	negative := math.Signbit(value)
	r := math.Abs(value)
	if r == 0 {
		return sqliteFloatDigits{digits: []byte{'0'}, decimalPoint: 1, negative: negative}
	}
	var exponent int
	x := [2]float64{r, 0}
	for x[0] > 9.223372036854774784e+118 {
		exponent += 100
		dekkerMul2(&x, 1e-100, -1.99918998026028836196e-117)
	}
	if x[0] > 9.223372036854774784e+18 {
		for x[0] > 9.223372036854774784e+28 {
			exponent += 10
			dekkerMul2(&x, 1e-10, -3.6432197315497741579e-27)
		}
		for x[0] > 9.223372036854774784e+18 {
			exponent++
			dekkerMul2(&x, 0.1, -5.5511151231257827021e-18)
		}
	} else {
		for x[0] < 9.223372036854774784e-83 {
			exponent -= 100
			dekkerMul2(&x, 1e100, -1.5902891109759918046e+83)
		}
		for x[0] < 9.223372036854774784e+07 {
			exponent -= 10
			dekkerMul2(&x, 1e10, 0)
		}
		for x[0] < 9.22337203685477478e+17 {
			exponent--
			dekkerMul2(&x, 10, 0)
		}
	}
	var integer uint64
	if x[1] < 0 {
		integer = uint64(x[0]) - uint64(-x[1])
	} else {
		integer = uint64(x[0]) + uint64(x[1])
	}
	digits := []byte(strconv.FormatUint(integer, 10))
	dp := len(digits) + exponent
	if round <= 0 {
		round = dp - round
		if round == 0 && digits[0] >= '5' {
			digits = append([]byte{'0'}, digits...)
			dp++
			round = 1
		}
	}
	if round > 0 && (round < len(digits) || len(digits) > maxRound) {
		if round > maxRound {
			round = maxRound
		}
		if round < len(digits) && digits[round] >= '5' {
			for j := round - 1; j >= 0; j-- {
				if digits[j] < '9' {
					digits[j]++
					break
				}
				digits[j] = '0'
				if j == 0 {
					digits = append([]byte{'1'}, digits...)
					dp++
					break
				}
			}
		}
		if round < len(digits) {
			digits = digits[:round]
		}
	}
	for len(digits) > 1 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
	}
	return sqliteFloatDigits{digits: digits, decimalPoint: dp, negative: negative}
}

func dekkerMul2(x *[2]float64, y, yy float64) {
	const mask uint64 = 0xfffffffffc000000
	hx := math.Float64frombits(math.Float64bits(x[0]) & mask)
	tx := x[0] - hx
	hy := math.Float64frombits(math.Float64bits(y) & mask)
	ty := y - hy
	p := hx * hy
	q := hx*ty + tx*hy
	c := p + q
	cc := p - c + q + tx*ty
	cc = x[0]*yy + x[1]*y + cc
	x[0] = c + cc
	x[1] = c - x[0]
	x[1] += cc
}

// renderFloatE renders %e: scientific with `precision` digits after the
// decimal point and a two-digit exponent. The '!' flag forces a decimal point
// in the mantissa (so %!.0e of -1e100 is "-1.0e+100").
func renderFloatE(r float64, precision int, alt, alt2 bool, upper bool, maxSig int) string {
	s := strconv.FormatFloat(r, 'e', precision, 64)
	if alt2 && !strings.Contains(s, ".") {
		i := strings.IndexAny(s, "eE")
		s = s[:i] + ".0" + s[i:]
	}
	// Go renders "1.999900e+08" — SQLite uses the same form.
	return s
}

// renderFloatG renders %g: shortest %e or %f depending on exponent, matching
// SQLite's etGENERIC handling:
//   - precision>0 is the number of significant digits (SQLite uses
//     precision-1 for the exp threshold, and FpDecode rounds to precision).
//   - exp<-4 or exp>precision → %e form, else %f form.
//   - '#' keeps trailing zeros; '!' forces a decimal point and trailing zero.
func renderFloatG(r float64, precision int, alt, alt2 bool, upper bool, maxSig int) string {
	if precision == 0 {
		precision = 1
	}
	sig := precision
	if sig > maxSig {
		sig = maxSig
	}
	// Go's %g with precision sig produces the significant-digit form; then we
	// adjust to SQLite's exp threshold (exp<-4 or exp>precision-1 → %e).
	exp := exponentOf(r)
	// SQLite clamps the effective precision to mxRound (16/20 sig digits);
	// digits beyond the rounded value are zeros that 'g' strips. Cap the
	// decimal digits so a huge requested precision (e.g. %.2147483647g)
	// renders the rounded value, not the full binary expansion.
	effP := precision
	if effP > maxSig {
		effP = maxSig
	}
	// SQLite: for etGENERIC, precision-- then if exp<-4 || exp>precision → e-form.
	if exp < -4 || exp > effP-1 {
		return renderGScientific(r, effP, alt, alt2, upper)
	}
	return renderGFixed(r, exp, effP, alt, alt2)
}

// renderGFixed renders the %f branch of %g: `effP-1-exp` digits after the
// decimal point, then strip trailing zeros unless '#'.
func renderGFixed(r float64, exp, effP int, alt, alt2 bool) string {
	dp := effP - 1 - exp
	if dp < 0 {
		dp = 0
	}
	s := strconv.FormatFloat(r, 'f', dp, 64)
	if !alt {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if alt2 {
		// '!' forces a decimal point; if no fractional digits, add .0
		if !strings.Contains(s, ".") {
			s += ".0"
		}
	}
	return s
}

// renderGScientific renders the %e branch of %g. SQLite's etGENERIC strips
// trailing zeros (flag_rtz is !flag_alternateform) unless the '#' flag is
// present, and '!' forces a decimal point in the mantissa.
func renderGScientific(r float64, effP int, alt, alt2 bool, upper bool) string {
	s := strconv.FormatFloat(r, 'e', effP-1, 64)
	if upper {
		s = strings.ToUpper(s)
	}
	if !alt {
		// Strip trailing zeros from the mantissa (but keep at least one
		// digit; SQLite's rtz keeps the '.' removal too, so '2.00e+08'
		// becomes '2e+08').
		i := strings.IndexAny(s, "eE")
		mant := s[:i]
		exp := s[i:]
		mant = strings.TrimRight(mant, "0")
		mant = strings.TrimRight(mant, ".")
		s = mant + exp
	}
	if alt2 && !strings.Contains(s, ".") {
		// '!' forces a decimal point in the mantissa.
		i := strings.IndexAny(s, "eE")
		s = s[:i] + ".0" + s[i:]
	}
	return s
}

// exponentOf returns the base-10 exponent of a non-zero finite float (the
// position of the decimal point minus one, matching FpDecode's iDP-1).
func exponentOf(r float64) int {
	if r == 0 {
		return 0
	}
	return int(math.Floor(math.Log10(math.Abs(r))))
}
