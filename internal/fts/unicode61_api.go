package fts

// Exported wrappers over the unicode61 primitives (unicode61.go, a port of
// fts3_unicode2.c). The fts5 package (ext/fts5/fts5_tokenize.c) reuses the
// same Unicode tables for its own unicode61 tokenizer — which additionally
// supports the categories= option — so the primitives are exposed here
// instead of duplicated.

// Unicode61IsAlnum reports whether codepoint c is alphanumeric for the
// unicode61 tokenizer (sqlite3FtsUnicodeIsalnum).
func Unicode61IsAlnum(c int) bool { return unicode61IsAlnum(c) }

// Unicode61IsDiacritic reports whether codepoint c is a combining diacritical
// mark (sqlite3FtsUnicodeIsdiacritic).
func Unicode61IsDiacritic(c int) bool { return unicode61IsDiacritic(c) }

// Unicode61Fold folds codepoint c to lower case, optionally removing
// diacritics (sqlite3FtsUnicodeFold; eRemoveDiacritic 0 = keep, 1 = simple,
// 2 = complex).
func Unicode61Fold(c, eRemoveDiacritic int) int { return unicode61Fold(c, eRemoveDiacritic) }
