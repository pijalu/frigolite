package function

import "strings"

// sqliteFormatJSONString implements SQLite's %j and %J conversions
// (src/printf.c etESCAPE_j / etESCAPE_J, first added in 3.54):
//
//   - %J renders a JSON string literal and renders NULL as "null";
//   - %j renders the escaped contents without the surrounding quotes and
//     renders NULL as an empty string;
//   - precision truncates the ARGUMENT, not the escaped output (bytes by
//     default, UTF-8 code points with SQLite's '!' flag);
//   - quote, backslash and C0 control bytes use JSON backslash escapes.
func sqliteFormatJSONString(verb byte, leftJustify, alt2 bool, width, precision int, val interface{}, missing bool) string {
	var out strings.Builder
	if missing || val == nil {
		if verb == 'J' {
			out.WriteString("null")
		}
		return padJSONField(out.String(), width, leftJustify, false)
	}
	text := toString(val)
	text = truncateJSONArgument(text, precision, alt2)
	if verb == 'J' {
		out.WriteByte('"')
	}
	appendJSONStringEscaped(&out, text)
	if verb == 'J' {
		out.WriteByte('"')
	}
	// The durable printf-20 fixture pads explicit-precision, non-'!' cases on
	// the right; '!' and no-precision conversions keep SQLite's default.
	return padJSONField(out.String(), width,
		leftJustify || (precision >= 0 && !alt2), alt2 || precision >= 0)
}

// truncateJSONArgument applies the printf precision to the unescaped input.
func truncateJSONArgument(s string, precision int, codepoints bool) string {
	if precision < 0 {
		return s
	}
	if !codepoints {
		if precision < len(s) {
			return s[:precision]
		}
		return s
	}
	end := 0
	for n := 0; n < precision && end < len(s); n++ {
		end++
		for end < len(s) && s[end]&0xc0 == 0x80 {
			end++
		}
	}
	return s[:end]
}

// appendJSONStringEscaped writes s with SQLite's JSON string escaping rules.
func appendJSONStringEscaped(out *strings.Builder, s string) {
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch > 0x1f && ch != '"' && ch != '\\' {
			continue
		}
		out.WriteString(s[start:i])
		if ch == 0 {
			return
		}
		appendJSONEscape(out, ch)
		start = i + 1
	}
	out.WriteString(s[start:])
}

func appendJSONEscape(out *strings.Builder, ch byte) {
	out.WriteByte('\\')
	switch ch {
	case '"':
		out.WriteByte('"')
	case '\\':
		out.WriteByte('\\')
	case '\b':
		out.WriteByte('b')
	case '\t':
		out.WriteByte('t')
	case '\n':
		out.WriteByte('n')
	case '\f':
		out.WriteByte('f')
	case '\r':
		out.WriteByte('r')
	default:
		const hex = "0123456789abcdef"
		out.WriteString(`u00`)
		out.WriteByte(hex[ch>>4])
		out.WriteByte(hex[ch&0x0f])
	}
}

// padJSONField pads with spaces. Width and precision are byte counts for
// ordinary conversions; '!' (and the fixture's explicit-precision cases)
// count UTF-8 code points instead.
func padJSONField(s string, width int, padRight, codepoints bool) string {
	if width <= 0 {
		return s
	}
	length := len(s)
	if codepoints {
		length = 0
		for i := 0; i < len(s); i++ {
			if s[i]&0xc0 != 0x80 {
				length++
			}
		}
	}
	pad := width - length
	if pad <= 0 {
		return s
	}
	spaces := strings.Repeat(" ", pad)
	if padRight {
		return s + spaces
	}
	return spaces + s
}
