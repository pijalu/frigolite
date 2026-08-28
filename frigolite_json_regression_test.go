package frigolite

import (
	"fmt"
	"testing"
)

// TestJSONNullPathArgs verifies the sqlite NULL-path rules (src/json.c):
// json_type/json_array_length/json_extract/json_remove return SQL NULL when
// a PATH argument is NULL; json_set/json_replace/json_insert skip only that
// pair (later pairs still apply); json_patch(X,NULL) is NULL; the -> and ->>
// operators return NULL for a NULL path. Expectations mirror the sqlite3
// oracle (quote()-verified).
func TestJSONNullPathArgs(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct{ sql, want string }{
		{"SELECT quote(json_type('{a:5,b:7}',NULL));", "NULL"},
		{"SELECT quote(json_array_length('[1,2,3]',NULL));", "NULL"},
		{"SELECT quote(json_extract('{\"a\":1}',NULL));", "NULL"},
		{"SELECT quote(json_remove('{\"a\":1}',NULL));", "NULL"},
		{"SELECT quote(json_patch('{\"a\":1}',NULL));", "NULL"},
		{"SELECT quote(json('{\"a\":1}') -> NULL);", "NULL"},
		{"SELECT quote(json('{\"a\":1}') ->> NULL);", "NULL"},
		// NULL path pairs are skipped; later pairs still apply.
		{"SELECT json_set('{\"a\":1}',NULL,9,'$.b',2);", `{"a":1,"b":2}`},
		{"SELECT json_replace('{\"a\":1,\"b\":0}',NULL,9,'$.b',2);", `{"a":1,"b":2}`},
		{"SELECT json_insert('{\"a\":1}',NULL,9,'$.b',2);", `{"a":1,"b":2}`},
		{"SELECT json_set('{\"a\":1}',NULL,9);", `{"a":1}`},
		// A NULL path does not suppress the odd-argument-count error.
		{"SELECT json_set('{\"a\":1}',NULL,9,'$.b');", "ERR:json_set() needs an odd number of arguments"},
	} {
		r := db.Query(tc.sql)
		if r.Error != nil {
			if "ERR:"+r.Error.Error() != tc.want {
				t.Errorf("%s => error %v, want %s", tc.sql, r.Error, tc.want)
			}
			continue
		}
		got := "<nil>"
		if len(r.Rows) == 1 && len(r.Rows[0]) == 1 && r.Rows[0][0] != nil {
			got = fmt.Sprintf("%v", r.Rows[0][0])
		}
		if got != tc.want {
			t.Errorf("%s => %s, want %s", tc.sql, got, tc.want)
		}
	}
}

// TestJSONEachNullRoot verifies sqlite jsonEachFilter: a NULL root argument
// makes json_each/json_tree produce zero rows (not the whole document).
func TestJSONEachNullRoot(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, name := range []string{"json_each", "json_tree"} {
		r := db.Query("SELECT count(*) FROM " + name + "('{\"a\":1,\"b\":2}',NULL);")
		if r.Error != nil {
			t.Fatalf("%s: %v", name, r.Error)
		}
		if n, ok := r.Rows[0][0].(int64); !ok || n != 0 {
			t.Errorf("%s(...,NULL) => %v rows, want 0", name, r.Rows[0][0])
		}
	}
}

// TestJSONTVFWHEREPushdown validates correlated json_each over rows whose
// argument is only sometimes valid JSON: a WHERE conjunct referencing only
// the outer table (json_valid(user.phone)) must gate TVF materialization
// per row, mirroring sqlite's WHERE-clause pushdown into outer loops
// (sqlite json102-1011). Oracle-verified: only Cindy matches.
func TestJSONTVFWHEREPushdown(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE user(name TEXT PRIMARY KEY, phone TEXT);",
		"INSERT INTO user VALUES('Alice','[\"919-555-1234\"]');",
		"INSERT INTO user VALUES('Bob','919-555-1234');", // plain text: invalid JSON
		"INSERT INTO user VALUES('Cindy','[\"704-555-1234\"]');",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("SELECT name FROM user, json_each(user.phone)\n   WHERE json_valid(user.phone)\n     AND json_each.value LIKE '704-%';")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "Cindy" {
		t.Errorf("rows = %v, want [Cindy]", r.Rows)
	}
}

// TestJSONBCorruptBlobHandling pins sqlite's layered handling of corrupt
// JSONB blobs (jsonb01-2.0/3.0, json101-26.x): the lenient jsonArgIsJsonb
// header check accepts them, but blob→text translation fails with
// "malformed JSON"; json_each's cursor walk stays lenient (a final label
// without a value renders as its own value); json_valid(X,8) reports 0.
func TestJSONBCorruptBlobHandling(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// jsonb01-2.0: object header with a FLOAT5 child whose 0xffffffff size
	// runs past the blob end — containment must fail during translation.
	r := db.Query(`SELECT x'8ce6ffffffff171333' -> '$';`)
	if r.Error == nil || r.Error.Error() != "malformed JSON" {
		t.Errorf("-> on corrupt blob: error = %v, want malformed JSON", r.Error)
	}
	// jsonb01-3.0: FLOAT5 payload "-" is not a valid number text.
	r = db.Query(`SELECT json(x'6B37616263162d');`)
	if r.Error == nil || r.Error.Error() != "malformed JSON" {
		t.Errorf("json() on bad FLOAT5: error = %v, want malformed JSON", r.Error)
	}
	// json101-26.1: json_each stays lenient — a final label without a value
	// becomes its own value ("eee").
	r = db.Query(`SELECT value FROM json_each(x'CC141761133117621332176313331764133437656565') WHERE key='eee';`)
	if r.Error != nil {
		t.Fatalf("json_each on corrupt blob: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "eee" {
		t.Errorf("rows = %v, want [eee]", r.Rows)
	}
	// json101-26.2: the full structural validity check reports invalid.
	r = db.Query(`SELECT json_valid(x'CC141761133117621332176313331764133437656565',8);`)
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(0) {
		t.Errorf("json_valid(x,8) = %v (err %v), want 0", r.Rows, r.Error)
	}
}
