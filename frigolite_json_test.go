package frigolite

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/function"
)

// jsonResultText unwraps a query result cell that is TEXT with the JSON
// subtype (function.JSONText) or a plain string, returning its text and
// whether it is text at all.
func jsonResultText(v interface{}) (string, bool) {
	if jt, ok := v.(function.JSONText); ok {
		return string(jt), true
	}
	s, ok := v.(string)
	return s, ok
}

// TestJSONExtract exercises json_extract() against the SQLite oracle
// (/usr/bin/sqlite3) behaviors verified during implementation.
func TestJSONExtract(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want interface{}
	}{
		// indexexpr3 inputs: relaxed JSON with unquoted keys.
		{`SELECT json_extract('{x:"one"}', '$.x')`, "one"},
		{`SELECT json_extract('{x:"two"}', '$.x')`, "two"},
		{`SELECT json_extract('{x:"three"}', '$.x')`, "three"},
		// Nested object and array paths.
		{`SELECT json_extract('{"a":{"b":123}}', '$.a.b')`, int64(123)},
		{`SELECT json_extract('[10,20,30]', '$[1]')`, int64(20)},
		{`SELECT json_extract('{"a":[{"b":true}]}', '$.a[0].b')`, int64(1)},
		{`SELECT json_extract('{"a":{"b":[1,{"c":2}]}}', '$.a.b[1].c')`, int64(2)},
		{`SELECT json_extract('[{"x":5}]', '$[0].x')`, int64(5)},
		// Type mapping: true/false -> 1/0, numbers, strings.
		{`SELECT json_extract('{"a":true}', '$.a')`, int64(1)},
		{`SELECT json_extract('{"a":false}', '$.a')`, int64(0)},
		{`SELECT json_extract('{"a":123}', '$.a')`, int64(123)},
		{`SELECT json_extract('{"a":1.5}', '$.a')`, 1.5},
		{`SELECT json_extract('{"a":"123"}', '$.a')`, "123"},
		{`SELECT json_extract('{"a":-5}', '$.a')`, int64(-5)},
		// null -> NULL, missing -> NULL.
		{`SELECT json_extract('{"a":null}', '$.a')`, nil},
		{`SELECT json_extract('{}', '$.missing')`, nil},
		{`SELECT json_extract('{"a":1}', '$.a.b.c')`, nil},
		{`SELECT json_extract('{"a":{"b":1}}', '$.a.b.c')`, nil},
		{`SELECT json_extract('123', '$.a')`, nil},
		// Root and whole-subtree extraction serialize back to JSON text.
		{`SELECT json_extract('{"a":1}', '$')`, `{"a":1}`},
		{`SELECT json_extract('{"a":{"b":1}}', '$.a')`, `{"b":1}`},
		{`SELECT json_extract('[1,2,3]', '$')`, `[1,2,3]`},
		{`SELECT json_extract('"str"', '$')`, "str"},
		{`SELECT json_extract('123', '$')`, int64(123)},
		// Quoted key in path.
		{`SELECT json_extract('{"a b":1}', '$."a b"')`, int64(1)},
		// Relaxed mode: single-quoted strings, trailing commas, hex, +, .5/1.
		{`SELECT json_extract("{'a':'one'}", '$.a')`, "one"},
		{`SELECT json_extract('{"a":1,}', '$.a')`, int64(1)},
		{`SELECT json_extract('[1,2,]', '$[1]')`, int64(2)},
		{`SELECT json_extract('{unquoted:1}', '$.unquoted')`, int64(1)},
		{`SELECT json_extract('{a_b:1}', '$.a_b')`, int64(1)},
		{`SELECT json_extract('{"a":0x10}', '$.a')`, int64(16)},
		{`SELECT json_extract('{"a":+5}', '$.a')`, int64(5)},
		{`SELECT json_extract('{"a":.5}', '$.a')`, 0.5},
		{`SELECT json_extract('{"a":1.}', '$.a')`, 1.0},
		{`SELECT json_extract('{"a":-0}', '$.a')`, int64(0)},
		{`SELECT json_extract('{"a":1e5}', '$.a')`, 100000.0},
		// Escapes. (Go backtick strings pass backslashes through literally, so
		// the SQL text contains the single-backslash JSON escapes.)
		{`SELECT json_extract('{"a":"line\nbreak"}', '$.a')`, "line\nbreak"},
		{`SELECT json_extract('{"a":"uni\u0041"}', '$.a')`, "uniA"},
		{`SELECT json_extract('{"a":"quo\"te"}', '$.a')`, `quo"te`},
		{`SELECT json_extract('{"a":"back\\slash"}', '$.a')`, `back\slash`},
		// NaN -> NULL.
		{`SELECT json_extract('{"a":NaN}', '$.a')`, nil},
		// Serialization of numbers preserves original text.
		{`SELECT json_extract('{"a":{"b":1.0}}', '$.a')`, `{"b":1.0}`},
		{`SELECT json_extract('{"a":{"b":1e2}}', '$.a')`, `{"b":1e2}`},
		{`SELECT json_extract('{"a":{"b":0.50}}', '$.a')`, `{"b":0.50}`},
		{`SELECT json_extract('{"a":{"b":0x10}}', '$.a')`, `{"b":16}`},
		{`SELECT json_extract('{"a":{"b":true}}', '$.a')`, `{"b":true}`},
		{`SELECT json_extract('{"a":{"b":null}}', '$.a')`, `{"b":null}`},
		{`SELECT json_extract('{"a":[1,2.5,"x"]}', '$.a')`, `[1,2.5,"x"]`},
		{`SELECT json_extract('{"a":{"b":-0}}', '$.a')`, `{"b":-0}`},
		{`SELECT json_extract('{"a":{"b":+5}}', '$.a')`, `{"b":5}`},
		// Multiple paths return a JSON array; missing paths become null.
		{`SELECT json_extract('{"a":1}', '$.b', '$.a')`, `[null,1]`},
		{`SELECT json_extract('{"a":1}', '$.a', '$.b')`, `[1,null]`},
		{`SELECT json_extract('{"a":{"b":2}}', '$.a', '$.missing', '$.a.b')`, `[{"b":2},null,2]`},
		{`SELECT json_extract('[1,2]', '$[0]', '$[5]')`, `[1,null]`},
		// 0/1 arguments validate and return NULL.
		{`SELECT json_extract('{"a":1}')`, nil},
		{`SELECT json_extract()`, nil},
	}

	for _, tc := range cases {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Errorf("%s: unexpected error: %v", tc.sql, res.Error)
			continue
		}
		if len(res.Rows) != 1 {
			t.Errorf("%s: expected 1 row, got %d", tc.sql, len(res.Rows))
			continue
		}
		got := res.Rows[0][0]
		// TEXT results carrying the JSON subtype (function.JSONText) are
		// string-equal to the expected plain text.
		if text, ok := jsonResultText(got); ok {
			got = text
		}
		if got != tc.want {
			t.Errorf("%s: got %v (%T), want %v (%T)", tc.sql, got, got, tc.want, tc.want)
		}
	}
}

// TestJSONExtractErrors exercises json_extract() error cases.
func TestJSONExtractErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	cases := []struct {
		sql     string
		errText string
	}{
		{`SELECT json_extract('not json', '$.a')`, "malformed JSON"},
		{`SELECT json_extract('{"a":}', '$.a')`, "malformed JSON"},
		{`SELECT json_extract('{"a":1,}', '$.a')`, ""}, // trailing comma OK
		{`SELECT json_extract('{"a":1}', '')`, "bad JSON path"},
		{`SELECT json_extract('{"a":1}', 'x')`, "bad JSON path"},
		{`SELECT json_extract('{"a":1}', '$.')`, "bad JSON path"},
		{`SELECT json_extract('{"a":1}', '$bad')`, "bad JSON path"},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if tc.errText == "" {
			if res.Error != nil {
				t.Errorf("%s: unexpected error: %v", tc.sql, res.Error)
			}
			continue
		}
		if res.Error == nil {
			t.Errorf("%s: expected error containing %q, got nil", tc.sql, tc.errText)
			continue
		}
		if !strings.Contains(res.Error.Error(), tc.errText) {
			t.Errorf("%s: expected error containing %q, got %q", tc.sql, tc.errText, res.Error.Error())
		}
	}
}

// TestJSONInsert exercises json_insert() against the SQLite oracle.
func TestJSONInsert(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want interface{}
	}{
		// indexexpr3 input.
		{`SELECT json_insert('{}', '$.y', 'two')`, `{"y":"two"}`},
		// Append new object keys at the end.
		{`SELECT json_insert('{"a":1}', '$.b', 'two')`, `{"a":1,"b":"two"}`},
		{`SELECT json_insert('{"a":1,"b":2}', '$.c', 9)`, `{"a":1,"b":2,"c":9}`},
		// Existing paths are not modified.
		{`SELECT json_insert('{"a":1}', '$.a', 'two')`, `{"a":1}`},
		{`SELECT json_insert('{"a":1,"b":2}', '$.b', 9)`, `{"a":1,"b":2}`},
		{`SELECT json_insert('{"a":{"b":1}}', '$.a.b', 9)`, `{"a":{"b":1}}`},
		{`SELECT json_insert('{"a":1}', '$.a.b', 'two')`, `{"a":1}`},
		// Arrays.
		{`SELECT json_insert('[1,2]', '$[2]', 'three')`, `[1,2,"three"]`},
		{`SELECT json_insert('[1,2]', '$[5]', 'x')`, `[1,2]`},
		{`SELECT json_insert('[1,2]', '$[0]', 'x')`, `[1,2]`},
		{`SELECT json_insert('{"a":[1,2]}', '$.a[2]', 9)`, `{"a":[1,2,9]}`},
		// Nested insertion creates intermediate objects/arrays.
		{`SELECT json_insert('{"a":1}', '$.x.y', 2)`, `{"a":1,"x":{"y":2}}`},
		{`SELECT json_insert('{}', '$.a.b', 1)`, `{"a":{"b":1}}`},
		{`SELECT json_insert('{}', '$.a[0]', 1)`, `{"a":[1]}`},
		{`SELECT json_insert('{"a":{}}', '$.a[0].b', 'x')`, `{"a":{}}`},
		{`SELECT json_insert('[]', '$[0].b', 'x')`, `[{"b":"x"}]`},
		{`SELECT json_insert('{"a":[]}', '$.a[0].b', 'x')`, `{"a":[{"b":"x"}]}`},
		{`SELECT json_insert('{}', '$.a[0].b', 'x')`, `{"a":[{"b":"x"}]}`},
		{`SELECT json_insert('{"a":[{"b":1}]}', '$.a[1].c', 2)`, `{"a":[{"b":1},{"c":2}]}`},
		// Value typing: NULL, integers, reals, strings.
		{`SELECT json_insert('{"a":1}', '$.a', NULL)`, `{"a":1}`},
		{`SELECT json_insert('{}', '$.y', 2)`, `{"y":2}`},
		{`SELECT json_insert('{}', '$.y', 1.5)`, `{"y":1.5}`},
		{`SELECT json_insert('{}', '$.y', 1.0)`, `{"y":1.0}`},
		{`SELECT json_insert('{}', '$.y', 'a"b')`, `{"y":"a\"b"}`},
		{`SELECT json_insert('{}', '$.y', 'a\b')`, `{"y":"a\\b"}`},
		{`SELECT json_insert('{}', '$.y', '')`, `{"y":""}`},
		// Multiple path/value pairs.
		{`SELECT json_insert('{}', '$.a', 1, '$.b', 2)`, `{"a":1,"b":2}`},
		// Root path is not insertable.
		{`SELECT json_insert('{"a":1}', '$', 'x')`, `{"a":1}`},
		// Single argument re-serializes the parsed JSON.
		{`SELECT json_insert('{"a":1}')`, `{"a":1}`},
		{`SELECT json_insert('{"a":1.50}')`, `{"a":1.50}`},
		// 0 arguments and NULL input return NULL.
		{`SELECT json_insert()`, nil},
		{`SELECT json_insert(NULL)`, nil},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Errorf("%s: unexpected error: %v", tc.sql, res.Error)
			continue
		}
		if len(res.Rows) != 1 {
			t.Errorf("%s: expected 1 row, got %d", tc.sql, len(res.Rows))
			continue
		}
		if tc.want == nil {
			if res.Rows[0][0] != nil {
				t.Errorf("%s: expected NULL, got %v (%T)", tc.sql, res.Rows[0][0], res.Rows[0][0])
			}
			continue
		}
		got, ok := jsonResultText(res.Rows[0][0])
		if !ok {
			t.Errorf("%s: expected string result, got %v (%T)", tc.sql, res.Rows[0][0], res.Rows[0][0])
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.sql, got, tc.want)
		}
	}
}

// TestJSONInsertErrors exercises json_insert() error cases.
func TestJSONInsertErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	cases := []struct {
		sql     string
		errText string
	}{
		{`SELECT json_insert('not json', '$.a', 'b')`, "malformed JSON"},
		{`SELECT json_insert('{}', '', 'b')`, "bad JSON path"},
		{`SELECT json_insert('{}', '$bad', 'b')`, "bad JSON path"},
		{`SELECT json_insert('{}', '$.a', x'0102')`, "JSON cannot hold BLOB values"},
		{`SELECT json_insert('{}', '$.a')`, "json_insert() needs an odd number of arguments"},
		{`SELECT json_insert('{}', '$.a', 1, '$.b')`, "json_insert() needs an odd number of arguments"},
		{`SELECT json_extract('not json', '$.a')`, "malformed JSON"},
		{`SELECT json_extract('{"a":1}', '$.b', 'x')`, "bad JSON path"},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if res.Error == nil {
			t.Errorf("%s: expected error containing %q, got nil", tc.sql, tc.errText)
			continue
		}
		if !strings.Contains(res.Error.Error(), tc.errText) {
			t.Errorf("%s: expected error containing %q, got %q", tc.sql, tc.errText, res.Error.Error())
		}
	}
}

// TestJSONExpressionIndex exercises CREATE INDEX with a json_extract()
// expression key over populated rows — the indexexpr3 test 1.0 scenario.
func TestJSONExpressionIndex(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	if res := db.Exec(`
		CREATE TABLE t1(a, j);
		INSERT INTO t1 VALUES(1, '{x:"one"}');
		INSERT INTO t1 VALUES(2, '{x:"two"}');
		INSERT INTO t1 VALUES(3, '{x:"three"}');
		CREATE INDEX i1 ON t1( json_extract(j, '$.x') );
		CREATE INDEX i2 ON t1( a, json_extract(j, '$.x') );
	`); res.Error != nil {
		t.Fatalf("CREATE INDEX with json_extract: %v", res.Error)
	}

	// The index keys must be usable: an equality search on the expression
	// should return the matching row.
	res := db.Query(`SELECT a FROM t1 WHERE json_extract(j, '$.x') = 'two'`)
	if res.Error != nil {
		t.Fatalf("query: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(2) {
		t.Errorf("expected rowid 2 for x=two, got %v", res.Rows)
	}
}
