package normalize

// Tokenizer for the SQL normalizer: a faithful port of sqlite3GetToken()
// from ext/misc/normalize.c (itself a snapshot of the SQLite core
// tokenizer). One handler per CC_* character-class case; the handler
// table replaces the C switch so each branch stays a small, independently
// verifiable function. Do not reorder the logic — every scan below stops
// at the NUL terminator exactly like the C.

// Token classes produced by getToken, mirroring normalize.c's TK_* group
// defines. Token subtypes within a group (TK_LE, TK_CONCAT, TK_STRING,
// TK_BLOB, ...) are deliberately not distinguished: the normalizer never
// inspects them, exactly as the C's "#define TK_LE TK_PUNCT" family does.
const (
	tokSpace   = iota // whitespace and comments; dropped
	tokName           // identifiers and keywords
	tokLiteral        // string/blob/number/parameter; becomes "?"
	tokPunct          // operators and punctuation; kept verbatim
	tokError          // illegal token; normalization fails
)

// byteAt reads z[i], returning 0 — the C string terminator — past the end.
// Every tokenizer scan in normalize.c stops at that terminator, so this
// single helper reproduces all of its bounds behavior.
func byteAt(z []byte, i int) byte {
	if i < len(z) {
		return z[i]
	}
	return 0
}

func isSpace(c byte) bool  { return ctypeMap[c]&0x01 != 0 }
func isDigit(c byte) bool  { return ctypeMap[c]&0x04 != 0 }
func isXDigit(c byte) bool { return ctypeMap[c]&0x08 != 0 }

// idChar is normalize.c's IdChar(C) macro: true if C may appear inside an
// identifier (alphanumerics, '_', '$' and every non-ASCII byte).
func idChar(c byte) bool { return ctypeMap[c]&0x46 != 0 }

func toLower(c byte) byte { return upperToLower[c] }

// tokenFuncs dispatches on the character class of a token's first byte,
// one handler per CC_* case of normalize.c's sqlite3GetToken().
var tokenFuncs = [ccIllegal + 1]func(z []byte) (n int, typ int){
	ccSpace:    tokSpaceRun,
	ccMinus:    tokMinus,
	ccLP:       tokPunct1,
	ccRP:       tokPunct1,
	ccSemi:     tokPunct1,
	ccPlus:     tokPunct1,
	ccStar:     tokPunct1,
	ccSlash:    tokSlash,
	ccPercent:  tokPunct1,
	ccEQ:       tokEQ,
	ccLT:       tokLT,
	ccGT:       tokGT,
	ccBang:     tokBang,
	ccPipe:     tokPipe,
	ccComma:    tokPunct1,
	ccAnd:      tokPunct1,
	ccTilda:    tokPunct1,
	ccQuote:    tokQuote,
	ccDot:      tokDot,
	ccDigit:    tokDigit,
	ccQuote2:   tokBracket,
	ccVarNum:   tokVarNum,
	ccDollar:   tokVar,
	ccVarAlpha: tokVar,
	ccKywd:     tokKywd,
	ccX:        tokX,
	ccID:       tokID1,
	ccIllegal:  tokIllegal,
}

// getToken returns the length (in bytes) of the token that begins at z[0]
// and its token class. z must be non-empty and z[0] must not be NUL; the
// caller's loop guarantees both, as in the C.
func getToken(z []byte) (n int, typ int) {
	return tokenFuncs[charClass[z[0]]](z)
}

// tokID1 is the CC_ID case and the shared fall-through tail of the CC_X
// case: consume identifier characters, yield an identifier.
func tokID1(z []byte) (int, int) {
	return tokIDTail(z, 1)
}

// tokIDTail is the switch-exit tail of sqlite3GetToken: consume remaining
// identifier characters starting at index i, yield an identifier.
func tokIDTail(z []byte, i int) (int, int) {
	for idChar(byteAt(z, i)) {
		i++
	}
	return i, tokName
}

func tokIllegal(z []byte) (int, int) { return 1, tokError }

func tokPunct1(z []byte) (int, int) { return 1, tokPunct }

// tokSpaceRun is the CC_SPACE case: a run of whitespace (which absorbs
// bytes like 0x0b mid-run but is never started by them — see charClass).
func tokSpaceRun(z []byte) (int, int) {
	i := 1
	for isSpace(byteAt(z, i)) {
		i++
	}
	return i, tokSpace
}

// tokMinus is the CC_MINUS case: "--" introduces a comment running to the
// end of line (excluded); a lone '-' is punctuation.
func tokMinus(z []byte) (int, int) {
	if byteAt(z, 1) != '-' {
		return 1, tokPunct
	}
	i := 2
	for {
		c := byteAt(z, i)
		if c == 0 || c == '\n' {
			return i, tokSpace
		}
		i++
	}
}

// tokSlash is the CC_SLASH case: "/" unless it starts a complete "/*".
// An unterminated "/*" at end of input is the two tokens "/" and "*",
// matching the C's z[2]==0 early exit.
func tokSlash(z []byte) (int, int) {
	if byteAt(z, 1) != '*' || byteAt(z, 2) == 0 {
		return 1, tokPunct
	}
	return tokSlashComment(z)
}

// tokSlashComment scans a "/* ... */" comment started at z[0], per the C:
// the loop exits either on the matching "*/" (both included) or at the
// string terminator (comment consumed to the end).
func tokSlashComment(z []byte) (int, int) {
	i := 3
	c := byteAt(z, 2)
	for {
		if c == '*' && byteAt(z, i) == '/' {
			return i + 1, tokSpace
		}
		c = byteAt(z, i)
		if c == 0 {
			return i, tokSpace
		}
		i++
	}
}

// tokEQ is the CC_EQ case: "=" or "==".
func tokEQ(z []byte) (int, int) {
	if byteAt(z, 1) == '=' {
		return 2, tokPunct
	}
	return 1, tokPunct
}

// tokLT is the CC_LT case: "<=", "<>", "<<" or "<".
func tokLT(z []byte) (int, int) {
	switch byteAt(z, 1) {
	case '=', '>', '<':
		return 2, tokPunct
	}
	return 1, tokPunct
}

// tokGT is the CC_GT case: ">=", ">>" or ">".
func tokGT(z []byte) (int, int) {
	switch byteAt(z, 1) {
	case '=', '>':
		return 2, tokPunct
	}
	return 1, tokPunct
}

// tokBang is the CC_BANG case: "!=" or an illegal lone '!'.
func tokBang(z []byte) (int, int) {
	if byteAt(z, 1) != '=' {
		return 1, tokError
	}
	return 2, tokPunct
}

// tokPipe is the CC_PIPE case: "||" or "|".
func tokPipe(z []byte) (int, int) {
	if byteAt(z, 1) == '|' {
		return 2, tokPunct
	}
	return 1, tokPunct
}

// tokQuote is the CC_QUOTE case: '...', "..." or `...` with ”-style
// quote doubling. An apostrophe-delimited token is a string literal; the
// other two are quoted identifiers; unterminated tokens are illegal.
func tokQuote(z []byte) (int, int) {
	delim := z[0]
	i := 1
	c := byteAt(z, i)
	for c != 0 {
		if c == delim {
			if byteAt(z, i+1) == delim {
				i++ // doubled quote: part of the token
			} else {
				break
			}
		}
		i++
		c = byteAt(z, i)
	}
	switch {
	case c == '\'':
		return i + 1, tokLiteral // TK_STRING
	case c != 0:
		return i + 1, tokName // TK_ID
	default:
		return i, tokError // unterminated
	}
}

// tokBracket is the CC_QUOTE2 case: "[...]" quoted identifier, scanned to
// the first ']'; unterminated is illegal.
func tokBracket(z []byte) (int, int) {
	i := 1
	c := z[0]
	for c != ']' {
		c = byteAt(z, i)
		if c == 0 {
			return i, tokError
		}
		i++
	}
	return i, tokName
}

// tokVarNum is the CC_VARNUM case: "?" or "?NNN".
func tokVarNum(z []byte) (int, int) {
	i := 1
	for isDigit(byteAt(z, i)) {
		i++
	}
	return i, tokLiteral
}

// tokVar is the CC_DOLLAR / CC_VARALPHA case: "$", "@", "#" or ":"
// followed by identifier characters; also absorbs "::" pairs and, when at
// least one identifier character has been seen, a single parenthesized
// group ("$f(x)" style). A bare prefix character is illegal, as is an
// unterminated or space-containing group.
func tokVar(z []byte) (int, int) {
	n := 0
	i := 1
	c := byteAt(z, i)
	for c != 0 {
		if idChar(c) {
			n++
		} else if c == '(' && n > 0 {
			return tokVarGroup(z, i)
		} else if c == ':' && byteAt(z, i+1) == ':' {
			i++
		} else {
			break
		}
		i++
		c = byteAt(z, i)
	}
	if n == 0 {
		return i, tokError
	}
	return i, tokLiteral
}

// tokVarGroup scans the "(...)" group of a variable token; the opening
// '(' is at z[i]. The group ends at the first space or ')' after at least
// one character; only a ')' terminates the token legally.
func tokVarGroup(z []byte, i int) (int, int) {
	for {
		i++
		c := byteAt(z, i)
		if c == 0 || isSpace(c) {
			return i, tokError
		}
		if c == ')' {
			return i + 1, tokLiteral
		}
	}
}

// tokKywd is the CC_KYWD case: a maximal run of keyword characters
// (letters, '_', 'x'). If the next character is identifier-like the token
// continues as a plain identifier (fall-through to the CC_ID tail).
func tokKywd(z []byte) (int, int) {
	i := 1
	for charClass[byteAt(z, i)] <= ccKywd {
		i++
	}
	if idChar(byteAt(z, i)) {
		i++
		return tokIDTail(z, i)
	}
	return i, tokName
}

// tokX is the CC_X case: x'...' blob literal with an even number of hex
// digits, or (fall-through) an identifier starting with 'x'/'X'.
func tokX(z []byte) (int, int) {
	if byteAt(z, 1) == '\'' {
		return tokBlob(z)
	}
	return tokID1(z)
}

// tokBlob scans a blob literal "x'...'" opened at z[0]. An odd number of
// hex digits or a stray character makes the token illegal; the scan then
// runs to the closing quote (or terminator) exactly like the C.
func tokBlob(z []byte) (int, int) {
	i := 2
	for isXDigit(byteAt(z, i)) {
		i++
	}
	if byteAt(z, i) == '\'' && i%2 == 0 {
		return i + 1, tokLiteral
	}
	for byteAt(z, i) != 0 && byteAt(z, i) != '\'' {
		i++
	}
	if byteAt(z, i) != 0 {
		i++
	}
	return i, tokError
}

// tokDot is the CC_DOT case: a '.' operator, or a float like ".5"
// (fall-through to the CC_DIGIT case).
func tokDot(z []byte) (int, int) {
	if !isDigit(byteAt(z, 1)) {
		return 1, tokPunct
	}
	return tokDigit(z)
}

// tokDigit is the CC_DIGIT case: decimal/hex integers, floats with
// fraction and/or exponent. Trailing identifier characters (e.g. "123abc")
// make the whole token illegal, except after a hex literal, which the C
// ends without that check ("0x1p5" is the hex token 0x1 followed by "p5").
func tokDigit(z []byte) (int, int) {
	if z[0] == '0' && (byteAt(z, 1) == 'x' || byteAt(z, 1) == 'X') && isXDigit(byteAt(z, 2)) {
		return tokDigitHex(z)
	}
	i, typ := tokDigitDecimal(z)
	return tokDigitTail(z, i, typ)
}

// tokDigitHex scans the hex digits after a "0x"/"0X" prefix.
func tokDigitHex(z []byte) (int, int) {
	i := 3
	for isXDigit(byteAt(z, i)) {
		i++
	}
	return i, tokLiteral
}

// tokDigitDecimal scans digits with optional fraction, starting with the
// integer digits at z[0] (or directly at the fraction for ".5"-style
// fall-through from CC_DOT), then the optional exponent — in that order,
// as in the C.
func tokDigitDecimal(z []byte) (int, int) {
	i := 0
	for isDigit(byteAt(z, i)) {
		i++
	}
	if byteAt(z, i) == '.' {
		i++
		for isDigit(byteAt(z, i)) {
			i++
		}
	}
	return tokDigitExp(z, i, tokLiteral)
}

// tokDigitExp scans an optional exponent starting at z[i]; when present,
// two bytes ("e", "e+"/"e-") are consumed up front, then digits.
func tokDigitExp(z []byte, i int, typ int) (int, int) {
	c := byteAt(z, i)
	if c != 'e' && c != 'E' {
		return i, typ
	}
	c1 := byteAt(z, i+1)
	if !isDigit(c1) && ((c1 != '+' && c1 != '-') || !isDigit(byteAt(z, i+2))) {
		return i, typ
	}
	i += 2
	for isDigit(byteAt(z, i)) {
		i++
	}
	return i, tokLiteral
}

// tokDigitTail is the C's trailing identifier scan: any identifier
// character glued to the number makes the whole token illegal.
func tokDigitTail(z []byte, i, typ int) (int, int) {
	for idChar(byteAt(z, i)) {
		typ = tokError
		i++
	}
	return i, typ
}
