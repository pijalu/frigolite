// Package fts5 implements the SQLite fts5 full-text engine.
//
// This file ports fts5_locale() (fts5_main.c fts5LocaleFunc:3618): an SQL
// scalar that wraps a text value with a locale tag. A tagged value is a
// blob laid out as
//
//	localeHdr(16) || locale || 0x00 || text
//
// recognized by IsLocaleValue (sqlite3Fts5IsLocaleValue) through its
// 16-byte header: a 128-bit pseudo-random vector XORed with fixed
// constants (fts5_main.c:3779). The header is process-unique, so locale
// blobs persisted by an EARLIER process are not recognized — matching C.
package fts5

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// localeHdr is the per-process fts5_locale() value header.
var localeHdr [16]byte

func init() {
	if _, err := rand.Read(localeHdr[:]); err != nil {
		// crypto/rand unavailable: fall back to the C XOR constants only.
		for i := range localeHdr {
			localeHdr[i] = 0
		}
	}
	// fts5_main.c:3782-3785 — fold the fixed vectors into the randomness.
	for i, x := range [4]uint32{0xF924976D, 0x16596E13, 0x7C80BEAA, 0x9B03A67F} {
		cur := binary.LittleEndian.Uint32(localeHdr[i*4:])
		binary.LittleEndian.PutUint32(localeHdr[i*4:], cur^x)
	}
}

// LocaleFunc implements fts5_locale(locale, text): an empty or NULL locale
// returns text unchanged (sqlite3_result_text); any other locale wraps text
// in the header/locale blob above.
func LocaleFunc(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("wrong number of arguments to function fts5_locale()")
	}
	locale := localeText(args[0])
	text := localeText(args[1])
	if locale == "" {
		return text, nil
	}
	blob := make([]byte, 0, len(localeHdr)+len(locale)+1+len(text))
	blob = append(blob, localeHdr[:]...)
	blob = append(blob, locale...)
	blob = append(blob, 0x00)
	blob = append(blob, text...)
	return blob, nil
}

// IsLocaleValue reports whether v is an fts5_locale() blob created by this
// process (sqlite3Fts5IsLocaleValue: BLOB with the header prefix).
func IsLocaleValue(v interface{}) bool {
	b, ok := v.([]byte)
	return ok && len(b) > len(localeHdr) && string(b[:len(localeHdr)]) == string(localeHdr[:])
}

// DecodeLocaleValue splits an fts5_locale() blob into its locale and text
// parts (sqlite3Fts5DecodeLocaleValue). ok is false when v is not a
// process-local locale blob.
func DecodeLocaleValue(v interface{}) (locale, text string, ok bool) {
	if !IsLocaleValue(v) {
		return "", "", false
	}
	b := v.([]byte)[len(localeHdr):]
	for i, c := range b {
		if c == 0x00 {
			return string(b[:i]), string(b[i+1:]), true
		}
	}
	return "", "", false
}

// localeText coerces a function argument to its text form
// (sqlite3_value_text semantics for the string path).
func localeText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	}
	return fmt.Sprintf("%v", v)
}
