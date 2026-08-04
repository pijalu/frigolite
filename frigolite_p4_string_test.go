package frigolite

import (
	"testing"
)

// P4 pre-tests: hand-written tests for G4.STRING string functions, written
// BEFORE running the TCL testgen packages (instr, substr, hexlit, blob,
// quote, regexp). Each expectation was verified against sqlite3 3.51 as the
// oracle; the tests document the exact SQLite semantics frigolite must match:
// character (not byte) positions for TEXT, byte positions only when both
// instr() arguments are blobs, ASCII-only UPPER/LOWER, NULL propagation,
// uppercase X'..' hex in quote(), and UTF-8 encoding in CHAR().

// TestP4String_Substr covers substr(X, start[, length]) edge cases:
// negative start (count from end), start=0 (treated as 1 for a non-NULL
// start), NULL start/length (NULL result), and UTF-8 character boundaries.
func TestP4String_Substr(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT substr('abcdefg',2)", "bcdefg"},
		{"SELECT substr('abcdefg',2,3)", "bcd"},
		{"SELECT substr('abcdefg',0)", "abcdefg"},      // start=0 means 1 for non-NULL start
		{"SELECT substr('abcdefg',0,0)", ""},           // length 0
		{"SELECT substr('abcdefg',0,2)", "a"},          // start 0 with length: first length-1 chars
		{"SELECT substr('abcdefg',-3)", "efg"},         // negative start: from end
		{"SELECT substr('abcdefg',-3,2)", "ef"},        // negative start with length
		{"SELECT substr('abcdefg',-100)", "abcdefg"},   // negative beyond start: whole string
		{"SELECT substr('héllo',2,2)", "él"},           // UTF-8 character (not byte) offsets
		{"SELECT substr('héllo',-2)", "lo"},
		{"SELECT substr('héllo',3)", "llo"},
		{"SELECT substr(x'616263',2,1)", "x'62'"},      // blob: byte offsets, blob result
		{"SELECT substr(x'616263',-1)", "x'63'"},
		{"SELECT ifnull(substr('abcdefg',NULL,1),'nil')", "nil"}, // NULL start
		{"SELECT ifnull(substr('abcdefg',1,NULL),'nil')", "nil"}, // NULL length
		{"SELECT ifnull(substr('abcdefg',NULL,NULL),'nil')", "nil"},
		{"SELECT ifnull(substr(NULL,1),'nil')", "nil"}, // NULL input
		{"SELECT ifnull(substr(NULL,1,2),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_Instr covers instr(X, Y): NULL propagation, empty needle
// (result 1), empty haystack (result 0), case sensitivity, character
// positions for TEXT, byte positions only when BOTH args are blobs, and
// text search when exactly one argument is a blob.
func TestP4String_Instr(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT instr('abcdefg','')", "1"},            // empty needle
		{"SELECT instr('','x')", "0"},                  // empty haystack
		{"SELECT instr('abcdefg','efg')", "5"},
		{"SELECT instr('abcdefg','x')", "0"},
		{"SELECT instr('Hello','h')", "0"},             // case sensitive
		{"SELECT instr('Hello','ello')", "2"},
		{"SELECT instr(12345,34)", "3"},                // numbers convert to text
		{"SELECT instr(123456.78,34)", "3"},
		{"SELECT instr('äbcdefg','efg')", "5"},         // character positions for TEXT
		{"SELECT instr('€xyzzy','xyz')", "2"},
		{"SELECT instr('abc€xyzzy','xyz')", "5"},
		{"SELECT instr('abc€xyzzy','€xyz')", "4"},
		{"SELECT instr(x'78c3a4e282ac79',x'79')", "7"}, // both blobs: byte position
		{"SELECT instr(x'78c3a4e282ac79',x'a4')", "3"},
		{"SELECT instr(x'78c3a4e282ac79','y')", "4"},   // blob haystack + text needle: text search
		{"SELECT instr('xä€y',x'79')", "4"},            // text haystack + blob needle: text search
		{"SELECT instr('xä€y',x'a4')", "0"},            // lone continuation byte not a char
		{"SELECT ifnull(instr(NULL,'x'),'nil')", "nil"},
		{"SELECT ifnull(instr('x',NULL),'nil')", "nil"},
		{"SELECT ifnull(instr(NULL,NULL),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_Trim covers trim/ltrim/rtrim with and without the optional
// character-set argument. Default trim removes ASCII space only (not tab);
// an empty char set trims nothing.
func TestP4String_Trim(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT trim('  hello  ')", "hello"},
		{"SELECT trim('xxhelloxx','x')", "hello"},
		{"SELECT trim('xyxxhelloxy','xy')", "hello"},   // char set, any order
		{"SELECT trim('  hi  ','')", "  hi  "},         // empty set: no trim
		{"SELECT trim('\\thello\\t')", "\\thello\\t"},  // default: space only, tab kept
		{"SELECT ltrim('  hello  ')", "hello  "},
		{"SELECT ltrim('xyxxhello','xy')", "hello"},
		{"SELECT ltrim('\\thello\\t')", "\\thello\\t"},
		{"SELECT rtrim('  hello  ')", "  hello"},
		{"SELECT rtrim('helloxxy','xy')", "hello"},
		{"SELECT ifnull(trim(NULL),'nil')", "nil"},
		{"SELECT ifnull(ltrim(NULL,'x'),'nil')", "nil"},
		{"SELECT ifnull(rtrim(NULL,'x'),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_UpperLower covers UPPER/LOWER: ASCII case folding only —
// non-ASCII characters are left unchanged (no Unicode case mapping without
// ICU), NULL propagation, and numeric input conversion to text.
func TestP4String_UpperLower(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT upper('hello')", "HELLO"},
		{"SELECT upper('Hello World')", "HELLO WORLD"},
		{"SELECT upper('héllo')", "HéLLO"},             // non-ASCII unchanged
		{"SELECT upper('ä')", "ä"},
		{"SELECT lower('HELLO')", "hello"},
		{"SELECT lower('HÉLLO')", "hÉllo"},             // non-ASCII unchanged
		{"SELECT lower('Ä')", "Ä"},
		{"SELECT upper(123)", "123"},                   // numbers → text
		{"SELECT ifnull(upper(NULL),'nil')", "nil"},
		{"SELECT ifnull(lower(NULL),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_Length covers LENGTH: character count for TEXT, byte count
// for blobs and numbers (their text length), NULL propagation.
func TestP4String_Length(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT length('hello')", "5"},
		{"SELECT length('héllo')", "5"},        // characters, not bytes
		{"SELECT length('€')", "1"},
		{"SELECT length(x'68656c6c6f')", "5"}, // blob: bytes
		{"SELECT length(x'c3a4')", "2"},       // blob with UTF-8 bytes: 2 bytes
		{"SELECT length(12345)", "5"},         // number: text length
		{"SELECT length(12.5)", "4"},
		{"SELECT length('')", "0"},
		{"SELECT ifnull(length(NULL),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_Quote covers quote(): SQL literal rendering — strings quoted
// with doubled quotes, blobs as uppercase X'HEX', integers/REALs as their
// numeric literal, NULL input returns NULL (not the string 'NULL').
func TestP4String_Quote(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT quote('hello')", "'hello'"},
		{"SELECT quote('it''s')", "'it''s'"},
		{"SELECT quote('a''b''c')", "'a''b''c'"},
		{"SELECT quote(x'bb')", "X'BB'"}, // uppercase hex
		{"SELECT quote(x'0102ff')", "X'0102FF'"},
		{"SELECT quote(x'')", "X''"},
		{"SELECT quote(42)", "42"},
		{"SELECT quote(-17)", "-17"},
		{"SELECT quote(5.5)", "5.5"},
		{"SELECT quote(0.0)", "0.0"},
		{"SELECT quote(-0.0)", "0.0"},
		{"SELECT ifnull(quote(NULL),'nil')", "nil"}, // NULL → NULL
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_HexUnhex covers hex()/unhex(): hex of text is the uppercase
// hex of its UTF-8 bytes; hex of a number is the hex of its TEXT form;
// unhex parses hex text into a blob; odd-length/invalid input yields NULL
// (3.51 returns an empty blob for invalid input, which hex() renders as '').
func TestP4String_HexUnhex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT hex('hello')", "68656C6C6F"},
		{"SELECT hex('')", ""},
		{"SELECT hex(x'0102ff')", "0102FF"},
		{"SELECT hex(65)", "3635"},    // hex of text '65'
		{"SELECT hex(65.5)", "36352E35"}, // hex of text '65.5'
		{"SELECT hex(x'')", ""},
		{"SELECT hex(unhex('68656C6C6F'))", "68656C6C6F"},
		{"SELECT hex(unhex('68656c6c6f'))", "68656C6C6F"}, // lowercase input ok
		{"SELECT hex(unhex(''))", ""},
		{"SELECT ifnull(hex(NULL),'nil')", "nil"},
		{"SELECT ifnull(hex(unhex(NULL)),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_CharUnicode covers char()/unicode(): char encodes codepoints
// as UTF-8 (multi-byte for >127); unicode returns the first codepoint of its
// argument, 0... NULL for empty string per SQLite; NULL args are skipped.
func TestP4String_CharUnicode(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT char(72,105)", "Hi"},
		{"SELECT char(104,101,108,108,111)", "hello"},
		{"SELECT hex(char(233))", "C3A9"},       // é as UTF-8
		{"SELECT char(233)", "é"},
		{"SELECT hex(char(8364))", "E282AC"},    // € as UTF-8
		{"SELECT hex(char(0))", "00"},           // NUL char
		{"SELECT char(65,NULL,66)", "AB"},       // NULL arg skipped
		{"SELECT unicode('hello')", "104"},
		{"SELECT unicode('é')", "233"},
		{"SELECT unicode('€')", "8364"},
		{"SELECT unicode('abc')", "97"},
		{"SELECT ifnull(unicode(''),'nil')", "nil"}, // empty string → NULL
		{"SELECT ifnull(unicode(NULL),'nil')", "nil"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_ReplaceConcat covers replace() (empty find returns original,
// NULL propagation) and the || concatenation operator (NULL propagates,
// numbers convert to text, blobs concatenate as bytes but yield text).
func TestP4String_ReplaceConcat(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT replace('hello','l','L')", "heLLo"},
		{"SELECT replace('hello','x','y')", "hello"},
		{"SELECT replace('abc','','x')", "abc"}, // empty find: unchanged
		{"SELECT replace('hello','ll','')", "heo"},
		{"SELECT replace('aaa','a','bb')", "bbbbbb"},
		{"SELECT ifnull(replace(NULL,'a','b'),'nil')", "nil"},
		{"SELECT ifnull(replace('abc',NULL,'x'),'nil')", "nil"},
		{"SELECT ifnull(replace('abc','a',NULL),'nil')", "nil"},
		{"SELECT 'a'||'b'", "ab"},
		{"SELECT 'hello '||'world'", "hello world"},
		{"SELECT ifnull('a'||NULL,'nil')", "nil"},
		{"SELECT ifnull(NULL||'b','nil')", "nil"},
		{"SELECT 1||2", "12"},
		{"SELECT 'x'||123", "x123"},
		{"SELECT quote(x'41'||x'42')", "'AB'"}, // blob concat → text 'AB'
		{"SELECT 'abc'||''", "abc"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4String_All runs every P4 string sub-test; used by the verify command
// `go test -run TestP4String`.
func TestP4String_All(t *testing.T) {
	for _, sub := range []string{
		"Substr", "Instr", "Trim", "UpperLower", "Length", "Quote",
		"HexUnhex", "CharUnicode", "ReplaceConcat",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "Substr":
				TestP4String_Substr(t)
			case "Instr":
				TestP4String_Instr(t)
			case "Trim":
				TestP4String_Trim(t)
			case "UpperLower":
				TestP4String_UpperLower(t)
			case "Length":
				TestP4String_Length(t)
			case "Quote":
				TestP4String_Quote(t)
			case "HexUnhex":
				TestP4String_HexUnhex(t)
			case "CharUnicode":
				TestP4String_CharUnicode(t)
			case "ReplaceConcat":
				TestP4String_ReplaceConcat(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

