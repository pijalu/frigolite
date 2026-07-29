package fts

import (
	"strings"
	"unicode"
)

// Token represents a single token from tokenization.
type Token struct {
	Term     string
	Position int
}

// Tokenizer tokenizes text into tokens for the inverted index.
type Tokenizer interface {
	Tokenize(text string) []Token
}

// NewTokenizer creates a tokenizer by name (simple, unicode61, porter).
func NewTokenizer(name string) Tokenizer {
	switch strings.ToLower(name) {
	case "unicode61":
		return &Unicode61Tokenizer{}
	case "porter":
		// Porter tokenizer chains simple + porter stemming
		return &PorterTokenizer{}
	case "simple":
		fallthrough
	default:
		return &SimpleTokenizer{}
	}
}

// --- Simple tokenizer ---

// SimpleTokenizer splits on non-alphanumeric characters and lowercases.
type SimpleTokenizer struct{}

func (t *SimpleTokenizer) Tokenize(text string) []Token {
	var tokens []Token
	pos := 0
	current := strings.Builder{}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, Token{Term: current.String(), Position: pos})
				pos++
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, Token{Term: current.String(), Position: pos})
	}
	return tokens
}

// --- Unicode61 tokenizer ---

// Unicode61Tokenizer is a Unicode-aware tokenizer.
// Tokens are sequences of letters and digits.
type Unicode61Tokenizer struct{}

func (t *Unicode61Tokenizer) Tokenize(text string) []Token {
	var tokens []Token
	pos := 0
	current := strings.Builder{}
	for _, r := range text {
		if isUnicode61Char(r) {
			current.WriteRune(unicode.ToLower(r))
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, Token{Term: current.String(), Position: pos})
				pos++
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, Token{Term: current.String(), Position: pos})
	}
	return tokens
}

func isUnicode61Char(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// --- Porter tokenizer ---

// PorterTokenizer applies SimpleTokenizer then Porter stemming.
type PorterTokenizer struct {
	simple SimpleTokenizer
}

func (t *PorterTokenizer) Tokenize(text string) []Token {
	tokens := t.simple.Tokenize(text)
	for i := range tokens {
		tokens[i].Term = stemPorter(tokens[i].Term)
	}
	return tokens
}

// stemPorter implements the Porter stemming algorithm.
func stemPorter(word string) string {
	if len(word) <= 2 {
		return word
	}
	w := []byte(word)

	// Step 1a
	w = replaceSuffix(w, "sses", "ss")
	w = replaceSuffix(w, "ies", "i")
	w = replaceSuffix(w, "ss", "ss")
	w = replaceSuffix(w, "s", "")

	// Step 1b
	if m, stemmed := stripSuffix(w, "eed"); m {
		if measure(stemmed) > 0 {
			w = append(stemmed, "ee"...)
		}
	} else if m, stemmed := stripSuffix(w, "ed"); m {
		if containsVowel(stemmed) {
			w = applyStep1bRules(stemmed)
		}
	} else if m, stemmed := stripSuffix(w, "ing"); m {
		if containsVowel(stemmed) {
			w = applyStep1bRules(stemmed)
		}
	}

	// Step 1c: replace trailing y with i
	if len(w) > 0 && w[len(w)-1] == 'y' {
		w[len(w)-1] = 'i'
	}

	// Step 2
	suffixes2 := []stepRule{
		{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"},
		{"anci", "ance"}, {"izer", "ize"}, {"abli", "able"},
		{"alli", "al"}, {"entli", "ent"}, {"eli", "e"},
		{"ousli", "ous"}, {"ization", "ize"}, {"ation", "ate"},
		{"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"},
		{"fulness", "ful"}, {"ousness", "ous"}, {"aliti", "al"},
		{"iviti", "ive"}, {"biliti", "ble"},
	}
	for _, rule := range suffixes2 {
		if m, stem := stripSuffix(w, rule.old); m && measure(stem) > 0 {
			w = append(stem, []byte(rule.new)...)
			break
		}
	}

	// Step 3
	suffixes3 := []stepRule{
		{"icate", "ic"}, {"ative", ""}, {"alize", "al"},
		{"iciti", "ic"}, {"ical", "ic"}, {"ful", ""}, {"ness", ""},
	}
	for _, rule := range suffixes3 {
		if m, stem := stripSuffix(w, rule.old); m && measure(stem) > 0 {
			w = append(stem, []byte(rule.new)...)
			break
		}
	}

	// Step 4
	suffixes4 := []string{
		"al", "ance", "ence", "er", "ic", "able", "ible",
		"ant", "ement", "ment", "ent", "ion", "ou", "ism", "ate",
		"iti", "ous", "ive", "ize",
	}
	for _, suf := range suffixes4 {
		if m, stem := stripSuffix(w, suf); m {
			if suf == "ion" {
				if len(stem) > 0 && (stem[len(stem)-1] == 's' || stem[len(stem)-1] == 't') {
					if measure(stem) > 1 {
						w = stem
						break
					}
				}
			} else if measure(stem) > 1 {
				w = stem
				break
			}
		}
	}

	// Step 5a
	if len(w) > 1 && w[len(w)-1] == 'e' {
		m := measure(w[:len(w)-1])
		if m > 1 || (m == 1 && !hasCVCEnding(w[:len(w)-1])) {
			w = w[:len(w)-1]
		}
	}

	// Step 5b
	if len(w) > 1 && w[len(w)-1] == 'l' && measure(w) > 1 && w[len(w)-2] == 'l' {
		w = w[:len(w)-1]
	}

	return string(w)
}

type stepRule struct {
	old, new string
}

func replaceSuffix(w []byte, old, new string) []byte {
	if hasSuffix(w, old) {
		return append(w[:len(w)-len(old)], []byte(new)...)
	}
	return w
}

func stripSuffix(w []byte, suffix string) (bool, []byte) {
	if hasSuffix(w, suffix) {
		return true, w[:len(w)-len(suffix)]
	}
	return false, w
}

func hasSuffix(w []byte, suffix string) bool {
	if len(w) < len(suffix) {
		return false
	}
	return string(w[len(w)-len(suffix):]) == suffix
}

// measure returns the "measure" m (number of VC sequences).
func measure(w []byte) int {
	m := 0
	n := len(w)
	prevVowel := false
	for i := 0; i < n; i++ {
		isV := isVowelLetter(w[i])
		if !prevVowel && isV {
			prevVowel = true
		} else if prevVowel && !isV {
			m++
			prevVowel = false
		}
	}
	return m
}

func isVowelLetter(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

func containsVowel(w []byte) bool {
	for _, b := range w {
		if isVowelLetter(b) {
			return true
		}
	}
	return false
}

func applyStep1bRules(w []byte) []byte {
	// Handle -e, -ble, -ize endings
	suffixes := []stepRule{
		{"at", "ate"}, {"bl", "ble"}, {"iz", "ize"},
	}
	for _, rule := range suffixes {
		if m, stem := stripSuffix(w, rule.old); m {
			return append(stem, []byte(rule.new)...)
		}
	}
	// Double last consonant if cvc conditions met
	if len(w) >= 2 && w[len(w)-1] == w[len(w)-2] && w[len(w)-1] != 'l' && w[len(w)-1] != 's' && w[len(w)-1] != 'z' {
		return w[:len(w)-1]
	}
	// Add -e if measure is 1 and has CVC
	if measure(w) == 1 && hasCVCEnding(w) {
		w = append(w, 'e')
	}
	return w
}

func hasCVCEnding(w []byte) bool {
	n := len(w)
	if n < 3 {
		return false
	}
	return !isVowelLetter(w[n-3]) && isVowelLetter(w[n-2]) && !isVowelLetter(w[n-1]) && w[n-1] != 'w' && w[n-1] != 'x' && w[n-1] != 'y'
}
