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
//   TokenEOF=0, TokenError=1, TokenIdentifier=2, TokenString=3,
//   TokenNumber=4, TokenBlob=5, TokenKeyword=6, TokenEq=7,
//   TokenNeq=8, TokenLt=9, TokenGt=10, TokenArrow=11,
//   TokenDoubleArrow=12, TokenLe=13, TokenGe=14, TokenPlus=15,
//   TokenMinus=16, TokenStar=17, TokenSlash=18, TokenMod=19,
//   TokenBitAnd=20, TokenTilde=21, TokenLParen=22, TokenRParen=23,
//   TokenComma=24, TokenSemicolon=25, TokenDot=26, TokenConcat=27,
//   TokenParam=28
func tokenCode(tokenType int, tokenValue string) int {
	switch {
	case tokenType == 0: // TokenEOF
		return 0
	case tokenType == 6: // TokenKeyword
		return keywordToCode(tokenValue)
	case tokenType == 2: // TokenIdentifier
		return TK_ID
	case tokenType == 3: // TokenString
		return TK_STRING
	case tokenType == 4: // TokenNumber
		if strings.ContainsAny(tokenValue, ".eE") {
			return TK_FLOAT
		}
		return TK_INTEGER
	case tokenType == 5: // TokenBlob
		return TK_BLOB
	case tokenType == 7: // TokenEq
		return TK_EQ
	case tokenType == 8: // TokenNeq
		return TK_NE
	case tokenType == 9: // TokenLt
		return TK_LT
	case tokenType == 10: // TokenGt
		return TK_GT
	case tokenType == 13: // TokenLe
		return TK_LE
	case tokenType == 14: // TokenGe
		return TK_GE
	case tokenType == 15: // TokenPlus
		return TK_PLUS
	case tokenType == 16: // TokenMinus
		return TK_MINUS
	case tokenType == 17: // TokenStar
		return TK_STAR
	case tokenType == 18: // TokenSlash
		return TK_SLASH
	case tokenType == 19: // TokenMod
		return TK_REM
	case tokenType == 20: // TokenBitAnd
		return TK_BITAND
	case tokenType == 21: // TokenTilde
		return TK_BITNOT
	case tokenType == 22: // TokenLParen
		return TK_LP
	case tokenType == 23: // TokenRParen
		return TK_RP
	case tokenType == 24: // TokenComma
		return TK_COMMA
	case tokenType == 25: // TokenSemicolon
		return TK_SEMI
	case tokenType == 26: // TokenDot
		return TK_DOT
	case tokenType == 27: // TokenConcat
		return TK_CONCAT
	case tokenType == 28: // TokenParam
		return TK_VARIABLE
	case tokenType == 11: // TokenArrow
		return TK_PTR
	default:
		return -1 // unknown
	}
}

// keywordToCode maps an uppercase SQL keyword string to its TK_* code.
func keywordToCode(kw string) int {
	switch kw {
	case "ABORT":
		return TK_ABORT
	case "ACTION":
		return TK_ACTION
	case "ADD":
		return TK_ADD
	case "AFTER":
		return TK_AFTER
	case "ALL":
		return TK_ALL
	case "ALTER":
		return TK_ALTER
	case "ANALYZE":
		return TK_ANALYZE
	case "AND":
		return TK_AND
	case "AS":
		return TK_AS
	case "ASC":
		return TK_ASC
	case "ATTACH":
		return TK_ATTACH
	case "AUTOINCREMENT":
		return TK_AUTOINCR
	case "BEFORE":
		return TK_BEFORE
	case "BEGIN":
		return TK_BEGIN
	case "BETWEEN":
		return TK_BETWEEN
	case "BY":
		return TK_BY
	case "CASCADE":
		return TK_CASCADE
	case "CASE":
		return TK_CASE
	case "CAST":
		return TK_CAST
	case "CHECK":
		return TK_CHECK
	case "COLLATE":
		return TK_COLLATE
	case "COLUMN":
		return TK_COLUMNKW
	case "COMMIT":
		return TK_COMMIT
	case "CONFLICT":
		return TK_CONFLICT
	case "CONSTRAINT":
		return TK_CONSTRAINT
	case "CREATE":
		return TK_CREATE
	case "CURRENT":
		return TK_CURRENT
	case "DATABASE":
		return TK_DATABASE
	case "DEFAULT":
		return TK_DEFAULT
	case "DEFERRABLE":
		return TK_DEFERRABLE
	case "DEFERRED":
		return TK_DEFERRED
	case "DELETE":
		return TK_DELETE
	case "DESC":
		return TK_DESC
	case "DETACH":
		return TK_DETACH
	case "DISTINCT":
		return TK_DISTINCT
	case "DO":
		return TK_DO
	case "DROP":
		return TK_DROP
	case "EACH":
		return TK_EACH
	case "ELSE":
		return TK_ELSE
	case "END":
		return TK_END
	case "ESCAPE":
		return TK_ESCAPE
	case "EXCEPT":
		return TK_EXCEPT
	case "EXCLUSIVE":
		return TK_EXCLUSIVE
	case "EXISTS":
		return TK_EXISTS
	case "EXPLAIN":
		return TK_EXPLAIN
	case "FAIL":
		return TK_FAIL
	case "FALSE":
		return TK_ID // treated as identifier
	case "FILTER":
		return TK_FILTER
	case "FIRST":
		return TK_FIRST
	case "FOLLOWING":
		return TK_FOLLOWING
	case "FOR":
		return TK_FOR
	case "FOREIGN":
		return TK_FOREIGN
	case "FROM":
		return TK_FROM
	case "FULL":
		return TK_JOIN_KW
	case "GENERATED":
		return TK_GENERATED
	case "GLOB":
		return TK_LIKE_KW
	case "GROUP":
		return TK_GROUP
	case "GROUPS":
		return TK_GROUPS
	case "HAVING":
		return TK_HAVING
	case "IF":
		return TK_IF
	case "IGNORE":
		return TK_IGNORE
	case "IMMEDIATE":
		return TK_IMMEDIATE
	case "IN":
		return TK_IN
	case "INDEX":
		return TK_INDEX
	case "INDEXED":
		return TK_INDEXED
	case "INITIALLY":
		return TK_INITIALLY
	case "INNER":
		return TK_JOIN_KW
	case "INSERT":
		return TK_INSERT
	case "INSTEAD":
		return TK_INSTEAD
	case "INTERSECT":
		return TK_INTERSECT
	case "INTO":
		return TK_INTO
	case "IS":
		return TK_IS
	case "ISNULL":
		return TK_ISNULL
	case "JOIN":
		return TK_JOIN
	case "KEY":
		return TK_KEY
	case "LAST":
		return TK_LAST
	case "LEFT":
		return TK_JOIN_KW
	case "LIKE":
		return TK_LIKE_KW
	case "LIMIT":
		return TK_LIMIT
	case "MATCH":
		return TK_MATCH
	case "MATERIALIZED":
		return TK_MATERIALIZED
	case "NATURAL":
		return TK_JOIN_KW
	case "NO":
		return TK_NO
	case "NOT":
		return TK_NOT
	case "NOTHING":
		return TK_NOTHING
	case "NOTNULL":
		return TK_NOTNULL
	case "NULL":
		return TK_NULL
	case "NULLS":
		return TK_NULLS
	case "OF":
		return TK_OF
	case "OFFSET":
		return TK_OFFSET
	case "ON":
		return TK_ON
	case "OR":
		return TK_OR
	case "ORDER":
		return TK_ORDER
	case "OUTER":
		return TK_JOIN_KW
	case "OVER":
		return TK_OVER
	case "PARTITION":
		return TK_PARTITION
	case "PLAN":
		return TK_PLAN
	case "PRAGMA":
		return TK_PRAGMA
	case "PRECEDING":
		return TK_PRECEDING
	case "PRIMARY":
		return TK_PRIMARY
	case "QUERY":
		return TK_QUERY
	case "RAISE":
		return TK_RAISE
	case "RANGE":
		return TK_RANGE
	case "RECURSIVE":
		return TK_RECURSIVE
	case "REFERENCES":
		return TK_REFERENCES
	case "REGEXP":
		return TK_LIKE_KW
	case "REINDEX":
		return TK_REINDEX
	case "RELEASE":
		return TK_RELEASE
	case "RENAME":
		return TK_RENAME
	case "REPLACE":
		return TK_REPLACE
	case "RESTRICT":
		return TK_RESTRICT
	case "RETURNING":
		return TK_RETURNING
	case "RIGHT":
		return TK_JOIN_KW
	case "ROLLBACK":
		return TK_ROLLBACK
	case "ROW":
		return TK_ROW
	case "ROWS":
		return TK_ROWS
	case "SAVEPOINT":
		return TK_SAVEPOINT
	case "SELECT":
		return TK_SELECT
	case "SET":
		return TK_SET
	case "TABLE":
		return TK_TABLE
	case "TEMP":
		return TK_TEMP
	case "TEMPORARY":
		return TK_TEMP
	case "THEN":
		return TK_THEN
	case "TO":
		return TK_TO
	case "TRANSACTION":
		return TK_TRANSACTION
	case "TRIGGER":
		return TK_TRIGGER
	case "TRUE":
		return TK_ID // treated as identifier
	case "UNBOUNDED":
		return TK_UNBOUNDED
	case "UNION":
		return TK_UNION
	case "UNIQUE":
		return TK_UNIQUE
	case "UPDATE":
		return TK_UPDATE
	case "USING":
		return TK_USING
	case "VACUUM":
		return TK_VACUUM
	case "VALUES":
		return TK_VALUES
	case "VIEW":
		return TK_VIEW
	case "VIRTUAL":
		return TK_VIRTUAL
	case "WHEN":
		return TK_WHEN
	case "WHERE":
		return TK_WHERE
	case "WINDOW":
		return TK_WINDOW
	case "WITH":
		return TK_WITH
	case "WITHOUT":
		return TK_WITHOUT
	default:
		return TK_ID // fallback: unknown keywords treated as identifiers
	}
}
