// Package normalize implements SQLite's sqlite3_normalize() function: a
// faithful, structurally identical port of ext/misc/normalize.c from the
// SQLite reference tree (public domain; the C file carries the usual
// SQLite blessing header — this port keeps the same disposition).
//
// Normalize(sql) rewrites an SQL string into a canonical form:
//
//   - every literal (string, blob literal, integer/float constant, and a
//     "NULL" that is not the IS [NOT] NULL / NOT NULL keyword) becomes "?"
//   - all superfluous whitespace and comments are removed; required
//     whitespace between two identifier characters becomes a single space
//   - all bytes are lowercased via SQLite's ASCII-only UpperToLower map
//   - an IN or NOT IN operator followed by a parenthesized value list
//     that is not a WITH/SELECT is rewritten to "in(?,?,?)"
//
// The output of a successful normalization is either the empty string
// (for input that is only whitespace/comments) or text terminated by a
// single ';'. Parameters (?NNN, :name, @name, $name) are literals here:
// every parameter token collapses to "?" regardless of its spelling.
//
// # Mapping to the C source
//
//	normalize.c                      this package
//	------------------------------   --------------------------------
//	aiClass[256]                     charClass (tables.go)
//	sqlite3CtypeMap[256]             ctypeMap (tables.go)
//	sqlite3UpperToLower[256]         upperToLower (tables.go)
//	sqlite3GetToken                  getToken + the tok* handlers
//	sqlite3_normalize first pass     Normalize loop + appendToken
//	"in(...)" second pass            collapseINLists
//	char *sqlite3_normalize(const*)  Normalize(sql string) (string, error)
//
// The tokenizer is a copy of SQLite's tokenize.c snapshot as embedded in
// normalize.c (2018-01-08), so it does NOT know about later core tokens;
// in particular '{'/'}' are illegal characters here. Token subtypes
// (TK_LE, TK_CONCAT, ...) all collapse to tokPunct exactly as the C's
// TK_* → TK_PUNCT defines do, because the normalizer never inspects them.
//
// # Error semantics
//
// The C function returns NULL both for SQLITE_NOMEM and for an illegal
// token (TK_ILLEGAL, e.g. an unterminated string, "0x", "?", "!" alone,
// or any control character). In Go those map to a non-nil *error value;
// ErrSyntax is the sentinel (errors.Is-friendly), wrapped with the byte
// offset of the offending token. Memory-allocation failure paths have no
// Go equivalent and are not modelled.
//
// # Deviations from the C (all documented, none observable in practice)
//
//   - Input is a Go string; matching C-string semantics, the text is
//     effectively truncated at the first NUL byte (sqlite3_normalize
//     receives a const char* and never sees past an embedded NUL).
//   - The second pass reads zIn[-1] to check whether "in(" is preceded by
//     an identifier character. When "in(" starts the string that read is
//     out of bounds in C (undefined; observed as "not an identifier" on
//     the reference oracle). The port treats offset 0 as "not preceded by
//     an identifier", i.e. the replacement proceeds.
//   - The C realloc failure path in the second pass (which leaks and
//     returns NULL) has no Go counterpart; slice growth cannot fail.
//
// # Relation to sqlite3_normalized_sql
//
// This package is the standalone string-to-string normalizer of
// ext/misc/normalize.c, exercised by the sqlite3_normalize() vectors in
// SQLite's test/normalize.test (tests 100-180). It is a different
// algorithm from src/tokenize.c's sqlite3Normalize(), which backs the
// sqlite3_normalized_sql(sqlite3_stmt*) API: that variant needs the
// parse tree (Vdbe) to preserve identifier case and spacing and to
// dequote/refix quoted identifiers, so it belongs to the statement
// layer, not to this leaf package.
//
// Harness integration plan (not implemented here): a later goal will
// expose the normalized text from exec's Stmt (mirroring
// sqlite3_normalized_sql's role for the JSON-harness "normalize" family).
// When the harness needs the plain sqlite3_normalize() form — or as a
// building block for the case-preserving variant — it should call
// Normalize on the statement's SQL text. This package deliberately
// imports nothing from the rest of frigolite, so any layer can use it.
package normalize
