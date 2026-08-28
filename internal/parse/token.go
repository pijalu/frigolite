// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Token adapter: maps Frigolite lexer tokens to LALR(1) parser token codes
// (SQLite TK_* constants extracted from parse.c).

package parse

import "strings"

// TK_* token codes from SQLite's parse.c.
const (
	TK_SEMI         = 1
	TK_EXPLAIN      = 2
	TK_QUERY        = 3
	TK_PLAN         = 4
	TK_BEGIN        = 5
	TK_TRANSACTION  = 6
	TK_DEFERRED     = 7
	TK_IMMEDIATE    = 8
	TK_EXCLUSIVE    = 9
	TK_COMMIT       = 10
	TK_END          = 11
	TK_ROLLBACK     = 12
	TK_SAVEPOINT    = 13
	TK_RELEASE      = 14
	TK_TO           = 15
	TK_TABLE        = 16
	TK_CREATE       = 17
	TK_IF           = 18
	TK_NOT          = 19
	TK_EXISTS       = 20
	TK_TEMP         = 21
	TK_LP           = 22
	TK_RP           = 23
	TK_AS           = 24
	TK_COMMA        = 25
	TK_WITHOUT      = 26
	TK_ABORT        = 27
	TK_ACTION       = 28
	TK_AFTER        = 29
	TK_ANALYZE      = 30
	TK_ASC          = 31
	TK_ATTACH       = 32
	TK_BEFORE       = 33
	TK_BY           = 34
	TK_CASCADE      = 35
	TK_CAST         = 36
	TK_CONFLICT     = 37
	TK_DATABASE     = 38
	TK_DESC         = 39
	TK_DETACH       = 40
	TK_EACH         = 41
	TK_FAIL         = 42
	TK_OR           = 43
	TK_AND          = 44
	TK_IS           = 45
	TK_ISNOT        = 46
	TK_MATCH        = 47
	TK_LIKE_KW      = 48
	TK_BETWEEN      = 49
	TK_IN           = 50
	TK_ISNULL       = 51
	TK_NOTNULL      = 52
	TK_NE           = 53
	TK_EQ           = 54
	TK_GT           = 55
	TK_LE           = 56
	TK_LT           = 57
	TK_GE           = 58
	TK_ESCAPE       = 59
	TK_ID           = 60
	TK_COLUMNKW     = 61
	TK_DO           = 62
	TK_FOR          = 63
	TK_IGNORE       = 64
	TK_INITIALLY    = 65
	TK_INSTEAD      = 66
	TK_NO           = 67
	TK_KEY          = 68
	TK_OF           = 69
	TK_OFFSET       = 70
	TK_PRAGMA       = 71
	TK_RAISE        = 72
	TK_RECURSIVE    = 73
	TK_REPLACE      = 74
	TK_RESTRICT     = 75
	TK_ROW          = 76
	TK_ROWS         = 77
	TK_TRIGGER      = 78
	TK_VACUUM       = 79
	TK_VIEW         = 80
	TK_VIRTUAL      = 81
	TK_WITH         = 82
	TK_NULLS        = 83
	TK_FIRST        = 84
	TK_LAST         = 85
	TK_CURRENT      = 86
	TK_FOLLOWING    = 87
	TK_PARTITION    = 88
	TK_PRECEDING    = 89
	TK_RANGE        = 90
	TK_UNBOUNDED    = 91
	TK_EXCLUDE      = 92
	TK_GROUPS       = 93
	TK_OTHERS       = 94
	TK_TIES         = 95
	TK_GENERATED    = 96
	TK_ALWAYS       = 97
	TK_MATERIALIZED = 98
	TK_REINDEX      = 99
	TK_RENAME       = 100
	TK_CTIME_KW     = 101
	TK_ANY          = 102
	TK_BITAND       = 103
	TK_BITOR        = 104
	TK_LSHIFT       = 105
	TK_RSHIFT       = 106
	TK_PLUS         = 107
	TK_MINUS        = 108
	TK_STAR         = 109
	TK_SLASH        = 110
	TK_REM          = 111
	TK_CONCAT       = 112
	TK_PTR          = 113
	TK_COLLATE      = 114
	TK_BITNOT       = 115
	TK_ON           = 116
	TK_INDEXED      = 117
	TK_STRING       = 118
	TK_JOIN_KW      = 119
	TK_CONSTRAINT   = 120
	TK_DEFAULT      = 121
	TK_NULL         = 122
	TK_PRIMARY      = 123
	TK_UNIQUE       = 124
	TK_CHECK        = 125
	TK_REFERENCES   = 126
	TK_AUTOINCR     = 127
	TK_INSERT       = 128
	TK_DELETE       = 129
	TK_UPDATE       = 130
	TK_SET          = 131
	TK_DEFERRABLE   = 132
	TK_FOREIGN      = 133
	TK_DROP         = 134
	TK_UNION        = 135
	TK_ALL          = 136
	TK_EXCEPT       = 137
	TK_INTERSECT    = 138
	TK_SELECT       = 139
	TK_VALUES       = 140
	TK_DISTINCT     = 141
	TK_DOT          = 142
	TK_FROM         = 143
	TK_JOIN         = 144
	TK_USING        = 145
	TK_ORDER        = 146
	TK_GROUP        = 147
	TK_HAVING       = 148
	TK_LIMIT        = 149
	TK_WHERE        = 150
	TK_RETURNING    = 151
	TK_INTO         = 152
	TK_NOTHING      = 153
	TK_FLOAT        = 154
	TK_BLOB         = 155
	TK_INTEGER      = 156
	TK_VARIABLE     = 157
	TK_CASE         = 158
	TK_WHEN         = 159
	TK_THEN         = 160
	TK_ELSE         = 161
	TK_INDEX        = 162
	TK_ALTER        = 163
	TK_ADD          = 164
	TK_WINDOW       = 165
	TK_OVER         = 166
	TK_FILTER       = 167
	TK_COLUMN       = 168
	TK_QNUMBER      = 183
)

// tokenCode maps a Frigolite TokenType + token value to an LALR parser token code.
// TokenType values match the iota order in internal/sql/lexer.go:
//
//	TokenEOF=0, TokenError=1, TokenIdentifier=2, TokenString=3,
//	TokenNumber=4, TokenBlob=5, TokenKeyword=6, TokenEq=7,
//	TokenNeq=8, TokenLt=9, TokenGt=10, TokenArrow=11,
//	TokenDoubleArrow=12, TokenLe=13, TokenGe=14, TokenPlus=15,
//	TokenMinus=16, TokenStar=17, TokenSlash=18, TokenMod=19,
//	TokenBitAnd=20, TokenBitOr=21, TokenLShift=22, TokenRShift=23,
//	TokenTilde=24, TokenLParen=25, TokenRParen=26, TokenComma=27,
//	TokenSemicolon=28, TokenDot=29, TokenConcat=30, TokenParam=31
//
// tokenTypeToCode maps lexer token types to TK_* parser codes.
var tokenTypeToCode = map[int]int{
	0:  0,           // TokenEOF
	2:  TK_ID,       // TokenIdentifier
	3:  TK_STRING,   // TokenString
	5:  TK_BLOB,     // TokenBlob
	7:  TK_EQ,       // TokenEq
	8:  TK_NE,       // TokenNeq
	9:  TK_LT,       // TokenLt
	10: TK_GT,       // TokenGt
	11: TK_PTR,      // TokenArrow
	12: TK_PTR,      // TokenDoubleArrow — same PTR terminal as '->' (SQLite
	                 // uses one TK_PTR token; the operator text distinguishes)
	13: TK_LE,       // TokenLe
	14: TK_GE,       // TokenGe
	15: TK_PLUS,     // TokenPlus
	16: TK_MINUS,    // TokenMinus
	17: TK_STAR,     // TokenStar
	18: TK_SLASH,    // TokenSlash
	19: TK_REM,      // TokenMod
	20: TK_BITAND,   // TokenBitAnd
	21: TK_BITOR,    // TokenBitOr
	22: TK_LSHIFT,   // TokenLShift
	23: TK_RSHIFT,   // TokenRShift
	24: TK_BITNOT,   // TokenTilde
	25: TK_LP,       // TokenLParen
	26: TK_RP,       // TokenRParen
	27: TK_COMMA,    // TokenComma
	28: TK_SEMI,     // TokenSemicolon
	29: TK_DOT,      // TokenDot
	30: TK_CONCAT,   // TokenConcat
	31: TK_VARIABLE, // TokenParam
}

func tokenCode(tokenType int, tokenValue string) int {
	if tokenType == 6 { // TokenKeyword
		return keywordToCode(strings.ToUpper(tokenValue))
	}
	if tokenType == 4 { // TokenNumber
		if strings.ContainsAny(tokenValue, ".eE") {
			return TK_FLOAT
		}
		return TK_INTEGER
	}
	code, ok := tokenTypeToCode[tokenType]
	if !ok {
		return -1 // unknown
	}
	return code
}

// keywordToCode maps an uppercase SQL keyword string to its TK_* code.
// keywordToCodeMap maps an uppercase SQL keyword string to its TK_* code.
var keywordToCodeMap = map[string]int{
	"ABORT":         TK_ABORT,
	"ACTION":        TK_ACTION,
	"ADD":           TK_ADD,
	"AFTER":         TK_AFTER,
	"ALL":           TK_ALL,
	"ALTER":         TK_ALTER,
	"ANALYZE":       TK_ANALYZE,
	"AND":           TK_AND,
	"AS":            TK_AS,
	"ASC":           TK_ASC,
	"ATTACH":        TK_ATTACH,
	"ALWAYS":        TK_ALWAYS,
	"AUTOINCREMENT": TK_AUTOINCR,
	"BEFORE":        TK_BEFORE,
	"BEGIN":         TK_BEGIN,
	"BETWEEN":       TK_BETWEEN,
	"BY":            TK_BY,
	"CASCADE":       TK_CASCADE,
	"CASE":          TK_CASE,
	"CAST":          TK_CAST,
	"CHECK":         TK_CHECK,
	"COLLATE":       TK_COLLATE,
	"COLUMN":        TK_COLUMNKW,
	"COMMIT":        TK_COMMIT,
	"CONFLICT":      TK_CONFLICT,
	"CONSTRAINT":    TK_CONSTRAINT,
	"CREATE":        TK_CREATE,
	"CROSS":         TK_JOIN_KW,
	"CURRENT":       TK_CURRENT,
	"DATABASE":      TK_DATABASE,
	"DEFAULT":       TK_DEFAULT,
	"DEFERRABLE":    TK_DEFERRABLE,
	"DEFERRED":      TK_DEFERRED,
	"DELETE":        TK_DELETE,
	"DESC":          TK_DESC,
	"DETACH":        TK_DETACH,
	"DISTINCT":      TK_DISTINCT,
	"DO":            TK_DO,
	"DROP":          TK_DROP,
	"EACH":          TK_EACH,
	"ELSE":          TK_ELSE,
	"END":           TK_END,
	"ESCAPE":        TK_ESCAPE,
	"EXCEPT":        TK_EXCEPT,
	"EXCLUDE":       TK_EXCLUDE,
	"EXCLUSIVE":     TK_EXCLUSIVE,
	"EXISTS":        TK_EXISTS,
	"EXPLAIN":       TK_EXPLAIN,
	"FAIL":          TK_FAIL,
	"FALSE":         TK_ID,
	"FILTER":        TK_FILTER,
	"FIRST":         TK_FIRST,
	"FOLLOWING":     TK_FOLLOWING,
	"FOR":           TK_FOR,
	"FOREIGN":       TK_FOREIGN,
	"FROM":          TK_FROM,
	"FULL":          TK_JOIN_KW,
	"GENERATED":     TK_GENERATED,
	"GLOB":          TK_LIKE_KW,
	"GROUP":         TK_GROUP,
	"GROUPS":        TK_GROUPS,
	"HAVING":        TK_HAVING,
	"IF":            TK_IF,
	"IGNORE":        TK_IGNORE,
	"IMMEDIATE":     TK_IMMEDIATE,
	"IN":            TK_IN,
	"INDEX":         TK_INDEX,
	"INDEXED":       TK_INDEXED,
	"INITIALLY":     TK_INITIALLY,
	"INNER":         TK_JOIN_KW,
	"INSERT":        TK_INSERT,
	"INSTEAD":       TK_INSTEAD,
	"INTERSECT":     TK_INTERSECT,
	"INTO":          TK_INTO,
	"IS":            TK_IS,
	"ISNULL":        TK_ISNULL,
	"JOIN":          TK_JOIN,
	"KEY":           TK_KEY,
	"LAST":          TK_LAST,
	"LEFT":          TK_JOIN_KW,
	"LIKE":          TK_LIKE_KW,
	"LIMIT":         TK_LIMIT,
	"MATCH":         TK_MATCH,
	"MATERIALIZED":  TK_MATERIALIZED,
	"NATURAL":       TK_JOIN_KW,
	"NO":            TK_NO,
	"NOT":           TK_NOT,
	"NOTHING":       TK_NOTHING,
	"NOTNULL":       TK_NOTNULL,
	"NULL":          TK_NULL,
	"NULLS":         TK_NULLS,
	"OF":            TK_OF,
	"OFFSET":        TK_OFFSET,
	"ON":            TK_ON,
	"OTHERS":        TK_OTHERS,
	"OR":            TK_OR,
	"ORDER":         TK_ORDER,
	"OUTER":         TK_JOIN_KW,
	"OVER":          TK_OVER,
	"PARTITION":     TK_PARTITION,
	"PLAN":          TK_PLAN,
	"PRAGMA":        TK_PRAGMA,
	"PRECEDING":     TK_PRECEDING,
	"PRIMARY":       TK_PRIMARY,
	"QUERY":         TK_QUERY,
	"RAISE":         TK_RAISE,
	"RANGE":         TK_RANGE,
	"RECURSIVE":     TK_RECURSIVE,
	"REFERENCES":    TK_REFERENCES,
	"REGEXP":        TK_LIKE_KW,
	"REINDEX":       TK_REINDEX,
	"RELEASE":       TK_RELEASE,
	"RENAME":        TK_RENAME,
	"REPLACE":       TK_REPLACE,
	"RESTRICT":      TK_RESTRICT,
	"RETURNING":     TK_RETURNING,
	"RIGHT":         TK_JOIN_KW,
	"ROLLBACK":      TK_ROLLBACK,
	"ROW":           TK_ROW,
	"ROWS":          TK_ROWS,
	"SAVEPOINT":     TK_SAVEPOINT,
	"SELECT":        TK_SELECT,
	"SET":           TK_SET,
	"TABLE":         TK_TABLE,
	"TEMP":          TK_TEMP,
	"TEMPORARY":     TK_TEMP,
	"THEN":          TK_THEN,
	"TIES":          TK_TIES,
	"TO":            TK_TO,
	"TRANSACTION":   TK_TRANSACTION,
	"TRIGGER":       TK_TRIGGER,
	"TRUE":          TK_ID,
	"UNBOUNDED":     TK_UNBOUNDED,
	"UNION":         TK_UNION,
	"UNIQUE":        TK_UNIQUE,
	"UPDATE":        TK_UPDATE,
	"USING":         TK_USING,
	"VACUUM":        TK_VACUUM,
	"VALUES":        TK_VALUES,
	"VIEW":          TK_VIEW,
	"VIRTUAL":       TK_VIRTUAL,
	"WHEN":          TK_WHEN,
	"WHERE":         TK_WHERE,
	"WINDOW":        TK_WINDOW,
	"WITH":          TK_WITH,
	"WITHOUT":       TK_WITHOUT,
}

// keywordToCode maps an uppercase SQL keyword string to its TK_* code.
func keywordToCode(kw string) int {
	if code, ok := keywordToCodeMap[kw]; ok {
		return code
	}
	return TK_ID // fallback: unknown keywords treated as identifiers
}
