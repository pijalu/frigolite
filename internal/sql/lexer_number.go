package sql

// numberTokenScanner mirrors sqlite3GetToken's CC_DIGIT/CC_DOT case
// (tokenize.c:421-497). Numeric runs consume every '_' and defer validation
// to sqlite3DequoteNumber (util.c:332-352), which reports the ORIGINAL token
// when a separator has a non-digit neighbour.
type numberTokenScanner struct {
	input string
	start int
	pos   int
	// qnumber is SQLite's TK_QNUMBER: the raw token contains at least one
	// underscore separator.
	qnumber bool
	// tail records the C while(IdChar(z[i])) suffix; any such suffix turns
	// the token into TK_ILLEGAL regardless of qnumber.
	tail bool
}

// readNumber scans the numeric literal at the current tokenizer position.
func (t *Tokenizer) readNumber() Token {
	start := t.pos
	raw, value, next, ok := scanNumberToken(t.input, start)
	t.pos = next
	if !ok {
		t.last = Token{Type: TokenUnrecognized, Value: raw, Pos: start}
		return t.last
	}
	t.last = Token{Type: TokenNumber, Value: value, Pos: start}
	return t.last
}

// scanNumberToken returns the raw token text, the dequoted number text (for
// valid underscore separators), and the position after the token. ok is false
// for SQLite's TK_ILLEGAL tokens and for malformed QNUMBER separators.
func scanNumberToken(input string, start int) (raw, value string, next int, ok bool) {
	s := numberTokenScanner{input: input, start: start, pos: start}
	s.scan()
	raw = input[start:s.pos]
	if s.tail || (s.qnumber && !validSeparators(raw)) {
		return raw, "", s.pos, false
	}
	if s.qnumber {
		return raw, dequoteNumber(raw), s.pos, true
	}
	return raw, raw, s.pos, true
}

func (s *numberTokenScanner) scan() {
	if s.input[s.pos] == '.' {
		// CC_DOT with a following digit falls through to CC_DIGIT; the
		// fraction loop consumes the leading dot in C, so skip it here.
		s.pos++
		s.scanDigitRun()
		s.scanExponent()
		s.scanIdentTail()
		return
	}
	if s.hasHexPrefix() {
		s.pos += 2 // skip 0x / 0X
		s.scanHexRun()
	} else {
		s.scanDigitRun()
		s.scanFraction()
		s.scanExponent()
	}
	s.scanIdentTail()
}

func (s *numberTokenScanner) hasHexPrefix() bool {
	return s.start+2 < len(s.input) &&
		s.input[s.start] == '0' &&
		(s.input[s.start+1] == 'x' || s.input[s.start+1] == 'X') &&
		isHexDigit(s.input[s.start+2])
}

func (s *numberTokenScanner) scanDigitRun() {
	for s.pos < len(s.input) {
		ch := s.input[s.pos]
		if !isDigitByte(ch) && ch != '_' {
			return
		}
		if ch == '_' {
			s.qnumber = true
		}
		s.pos++
	}
}

func (s *numberTokenScanner) scanHexRun() {
	for s.pos < len(s.input) {
		ch := s.input[s.pos]
		if !isHexDigit(ch) && ch != '_' {
			return
		}
		if ch == '_' {
			s.qnumber = true
		}
		s.pos++
	}
}

func (s *numberTokenScanner) scanFraction() {
	if s.pos < len(s.input) && s.input[s.pos] == '.' {
		s.pos++
		s.scanDigitRun()
	}
}

func (s *numberTokenScanner) scanExponent() {
	if s.pos >= len(s.input) {
		return
	}
	ch := s.input[s.pos]
	if ch != 'e' && ch != 'E' {
		return
	}
	j := s.pos + 1
	if j < len(s.input) && (s.input[j] == '+' || s.input[j] == '-') {
		j++
	}
	if j >= len(s.input) || !isDigitByte(s.input[j]) {
		// Leave e/E for the identifier tail, matching tokenize.c's
		// exponent lookahead (e.g. SELECT 1.4e+_4 errors on "1.4e").
		return
	}
	s.pos = j + 1
	s.scanDigitRun()
}

func (s *numberTokenScanner) scanIdentTail() {
	for s.pos < len(s.input) && isIdentPart(s.input[s.pos]) {
		s.tail = true
		s.pos++
	}
}

func validSeparators(raw string) bool {
	hex := len(raw) >= 2 && raw[0] == '0' && (raw[1] == 'x' || raw[1] == 'X')
	for i := 0; i < len(raw); i++ {
		if raw[i] != '_' {
			continue
		}
		if !validSeparatorAt(raw, i, hex) {
			return false
		}
	}
	return true
}

func validSeparatorAt(raw string, i int, hex bool) bool {
	if i == 0 || i+1 >= len(raw) {
		return false
	}
	prev, next := raw[i-1], raw[i+1]
	if hex {
		return isHexDigit(prev) && isHexDigit(next)
	}
	return isDigitByte(prev) && isDigitByte(next)
}

func dequoteNumber(raw string) string {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '_' {
			out = append(out, raw[i])
		}
	}
	return string(out)
}

func isDigitByte(ch byte) bool {
	return ch >= '0' && ch <= '9'
}
