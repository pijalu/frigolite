package util

import (
	"strings"
	"unicode/utf8"
)

// maxFoldKeywordLen bounds the stack buffer used to uppercase a word in
// place. Every SQL keyword in the engine's keyword tables is far shorter.
const maxFoldKeywordLen = 64

// LookupUpperASCII returns m[strings.ToUpper(s)] without allocating for
// ASCII words: already-uppercase words are looked up directly and words with
// lowercase letters are uppercased into a stack buffer (the Go compiler
// lowers m[string(buf)] to an allocation-free map access). Non-ASCII words
// fall back to strings.ToUpper, preserving Unicode case-folding semantics.
//
// This is the hot path for SQL keyword classification, which runs once per
// keyword token per parse.
func LookupUpperASCII[V any](m map[string]V, s string) (V, bool) {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if !ascii {
		v, ok := m[strings.ToUpper(s)]
		return v, ok
	}
	hasLower := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			hasLower = true
			break
		}
	}
	if !hasLower {
		v, ok := m[s]
		return v, ok
	}
	if len(s) > maxFoldKeywordLen {
		v, ok := m[strings.ToUpper(s)]
		return v, ok
	}
	var buf [maxFoldKeywordLen]byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		buf[i] = c
	}
	v, ok := m[string(buf[:len(s)])]
	return v, ok
}
