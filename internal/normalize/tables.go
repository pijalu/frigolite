package normalize

// This file transcribes the lookup tables of SQLite's ext/misc/normalize.c
// verbatim: aiClass (character classes), sqlite3CtypeMap (ctype bits) and
// sqlite3UpperToLower (ASCII case folding). Do not "improve" them; the
// tokenizer's behavior on every single byte value depends on them.

// Character classes for getToken, mirroring the CC_* defines of
// ext/misc/normalize.c.
const (
	ccX        = 0  // the letter 'x', or start of BLOB literal
	ccKywd     = 1  // alphabetics or '_'; usable in a keyword
	ccID       = 2  // unicode characters usable in IDs
	ccDigit    = 3  // digits
	ccDollar   = 4  // '$'
	ccVarAlpha = 5  // '@', '#', ':'; alphabetic SQL variables
	ccVarNum   = 6  // '?'; numeric SQL variables
	ccSpace    = 7  // space characters
	ccQuote    = 8  // '"', '\'' or '`'; string literals, quoted ids
	ccQuote2   = 9  // '['; [...] style quoted ids
	ccPipe     = 10 // '|'; bitwise OR or concatenate
	ccMinus    = 11 // '-'; minus or SQL-style comment
	ccLT       = 12 // '<'; part of < or <= or <>
	ccGT       = 13 // '>'; part of > or >=
	ccEQ       = 14 // '='; part of = or ==
	ccBang     = 15 // '!'; part of !=
	ccSlash    = 16 // '/'; / or c-style comment
	ccLP       = 17 // '('
	ccRP       = 18 // ')'
	ccSemi     = 19 // ';'
	ccPlus     = 20 // '+'
	ccStar     = 21 // '*'
	ccPercent  = 22 // '%'
	ccComma    = 23 // ','
	ccAnd      = 24 // '&'
	ccTilda    = 25 // '~'
	ccDot      = 26 // '.'
	ccIllegal  = 27 // illegal character
)

// charClass is normalize.c's aiClass[]: maps each byte to its tokenizer
// character class. Index by the raw byte value.
var charClass = [256]byte{
	/*         x0  x1  x2  x3  x4  x5  x6  x7  x8  x9  xa  xb  xc  xd  xe  xf */
	/* 0x */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 7, 7, 27, 7, 7, 27, 27,
	/* 1x */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* 2x */ 7, 15, 8, 5, 4, 22, 24, 8, 17, 18, 21, 20, 23, 11, 26, 16,
	/* 3x */ 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 5, 19, 12, 14, 13, 6,
	/* 4x */ 5, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	/* 5x */ 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 9, 27, 27, 27, 1,
	/* 6x */ 8, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	/* 7x */ 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 27, 10, 27, 25, 27,
	/* 8x */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* 9x */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* Ax */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* Bx */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* Cx */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* Dx */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* Ex */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	/* Fx */ 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
}

// ctypeMap is normalize.c's sqlite3CtypeMap[]: per-byte bit flags
//
//	0x01 space, 0x02 alpha, 0x04 digit, 0x06 alnum, 0x08 xdigit,
//	0x20 needs toupper, 0x40 SQLite identifier character, 0x80 quote.
var ctypeMap = [256]byte{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 00..07  ........
	0x00, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, // 08..0f  ........
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 10..17  ........
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 18..1f  ........
	0x01, 0x00, 0x80, 0x00, 0x40, 0x00, 0x00, 0x80, // 20..27   !"#$%&'
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 28..2f  ()*+,-./
	0x0c, 0x0c, 0x0c, 0x0c, 0x0c, 0x0c, 0x0c, 0x0c, // 30..37  01234567
	0x0c, 0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 38..3f  89:;<=>?

	0x00, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x02, // 40..47  @ABCDEFG
	0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, // 48..4f  HIJKLMNO
	0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, // 50..57  PQRSTUVW
	0x02, 0x02, 0x02, 0x80, 0x00, 0x00, 0x00, 0x40, // 58..5f  XYZ[\]^_
	0x80, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x22, // 60..67  `abcdefg
	0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, // 68..6f  hijklmno
	0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, // 70..77  pqrstuvw
	0x22, 0x22, 0x22, 0x00, 0x00, 0x00, 0x00, 0x00, // 78..7f  xyz{|}~.

	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // 80..87  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // 88..8f  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // 90..97  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // 98..9f  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // a0..a7  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // a8..af  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // b0..b7  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // b8..bf  ........

	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // c0..c7  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // c8..cf  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // d0..d7  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // d8..df  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // e0..e7  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // e8..ef  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // f0..f7  ........
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, // f8..ff  ........
}

// upperToLower is normalize.c's sqlite3UpperToLower[]: maps each byte to
// its lowercase ASCII form, identity for everything else. It is computed
// here because the C table is exactly that function over 0..255.
var upperToLower = func() [256]byte {
	var t [256]byte
	for i := 0; i < 256; i++ {
		t[i] = byte(i)
	}
	for c := byte('A'); c <= 'Z'; c++ {
		t[c] = c - 'A' + 'a'
	}
	return t
}()
