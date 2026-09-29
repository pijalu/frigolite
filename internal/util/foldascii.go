package util

// Allocation-free ASCII case-folding scans. These run once or several times
// per statement in hot gates (WITHOUT ROWID checks, vtable-prefix checks),
// where the historical strings.ToUpper(text) allocated a full copy of the
// SQL per call. Bytes >= 0x80 compare literally — the callers only fold
// ASCII keywords.

// asciiLower folds an ASCII byte to lower case; bytes >= 0x80 are returned
// unchanged.
func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// IndexFoldASCII returns the index of the first occurrence of sub in s under
// ASCII case folding, or -1.
func IndexFoldASCII(s, sub string) int {
	n := len(sub)
	if n == 0 {
		return 0
	}
	if n > len(s) {
		return -1
	}
	c0 := asciiLower(sub[0])
	for i := 0; i+n <= len(s); i++ {
		if asciiLower(s[i]) != c0 {
			continue
		}
		j := 1
		for ; j < n; j++ {
			if asciiLower(s[i+j]) != asciiLower(sub[j]) {
				break
			}
		}
		if j == n {
			return i
		}
	}
	return -1
}

// ContainsFoldASCII reports whether s contains sub under ASCII case folding.
func ContainsFoldASCII(s, sub string) bool {
	return IndexFoldASCII(s, sub) >= 0
}

// HasPrefixFoldASCII reports whether s begins with prefix under ASCII case
// folding.
func HasPrefixFoldASCII(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if asciiLower(s[i]) != asciiLower(prefix[i]) {
			return false
		}
	}
	return true
}
