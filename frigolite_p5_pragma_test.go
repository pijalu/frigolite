package frigolite

import (
	"strings"
	"testing"
)

// TestP5PragmaTableInfo verifies PRAGMA table_info / table_xinfo output:
// column order, types, notnull, dflt_value, pk, and hidden columns.
func TestP5PragmaTableInfo(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE t(
		a INTEGER PRIMARY KEY,
		b TEXT NOT NULL,
		c REAL DEFAULT 1.5,
		d
	)`); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	r := db.Query("PRAGMA table_info(t)")
	if r.Error != nil {
		t.Fatalf("table_info: %v", r.Error)
	}
	want := [][]interface{}{
		{int64(0), "a", "INTEGER", int64(0), nil, int64(1)},
		{int64(1), "b", "TEXT", int64(1), nil, int64(0)},
		{int64(2), "c", "REAL", int64(0), "1.5", int64(0)},
		{int64(3), "d", "", int64(0), nil, int64(0)},
	}
	if len(r.Rows) != len(want) {
		t.Fatalf("table_info rows: got %d want %d", len(r.Rows), len(want))
	}
	for i, row := range r.Rows {
		for j := range want[i] {
			if !valuesEqual(row[j], want[i][j]) {
				t.Errorf("table_info[%d][%d]: got %v want %v", i, j, row[j], want[i][j])
			}
		}
	}

	// table_xinfo adds the hidden column (0 for ordinary tables).
	r = db.Query("PRAGMA table_xinfo(t)")
	if r.Error != nil {
		t.Fatalf("table_xinfo: %v", r.Error)
	}
	if len(r.Rows) != 4 || len(r.Columns) != 7 {
		t.Fatalf("table_xinfo: got %d rows %d cols", len(r.Rows), len(r.Columns))
	}

	// Function-style: SELECT * FROM pragma_table_info('t')
	r = db.Query("SELECT name, pk FROM pragma_table_info('t') WHERE pk=1")
	if r.Error != nil {
		t.Fatalf("pragma_table_info fn: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "a" {
		t.Fatalf("pragma_table_info fn: got %v", r.Rows)
	}
}

// TestP5PragmaIndexInfo verifies index_info / index_list / index_xinfo output.
func TestP5PragmaIndexInfo(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE t(a, b, c); CREATE INDEX i1 ON t(b, c);`); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	r := db.Query("PRAGMA index_list(t)")
	if r.Error != nil {
		t.Fatalf("index_list: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][1] != "i1" {
		t.Fatalf("index_list: got %v", r.Rows)
	}

	r = db.Query("PRAGMA index_info(i1)")
	if r.Error != nil {
		t.Fatalf("index_info: %v", r.Error)
	}
	// seqno, cid, name — columns b (cid 1) and c (cid 2).
	if len(r.Rows) != 2 {
		t.Fatalf("index_info rows: got %d want 2", len(r.Rows))
	}
	if r.Rows[0][1] != int64(1) || r.Rows[0][2] != "b" {
		t.Errorf("index_info[0]: got %v", r.Rows[0])
	}
	if r.Rows[1][1] != int64(2) || r.Rows[1][2] != "c" {
		t.Errorf("index_info[1]: got %v", r.Rows[1])
	}

	// index_xinfo has desc/coll/key columns.
	r = db.Query("PRAGMA index_xinfo(i1)")
	if r.Error != nil {
		t.Fatalf("index_xinfo: %v", r.Error)
	}
	if len(r.Columns) != 6 {
		t.Fatalf("index_xinfo cols: got %d want 6", len(r.Columns))
	}
}

// TestP5PragmaForeignKeyListAndToggle verifies foreign_key_list output and the
// foreign_keys behavior toggle (ON enables FK enforcement).
func TestP5PragmaForeignKeyListAndToggle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE parent(id INTEGER PRIMARY KEY);
		CREATE TABLE child(pid INTEGER REFERENCES parent(id));`); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	r := db.Query("PRAGMA foreign_key_list(child)")
	if r.Error != nil {
		t.Fatalf("foreign_key_list: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("foreign_key_list rows: got %d want 1", len(r.Rows))
	}
	// id, seq, table, from, to, on_update, on_delete, match
	if r.Rows[0][0] != int64(0) || r.Rows[0][1] != int64(0) ||
		r.Rows[0][2] != "parent" || r.Rows[0][3] != "pid" || r.Rows[0][4] != "id" {
		t.Errorf("foreign_key_list: got %v", r.Rows[0])
	}

	// With foreign_keys OFF (default), an orphan insert succeeds.
	if res := db.Exec("INSERT INTO child VALUES(99)"); res.Error != nil {
		t.Fatalf("insert with FK off: %v", res.Error)
	}

	// foreign_key_check reports the violation rows (child, rowid, parent, fkid).
	r = db.Query("PRAGMA foreign_key_check")
	if r.Error != nil {
		t.Fatalf("foreign_key_check: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "child" || r.Rows[0][2] != "parent" {
		t.Errorf("foreign_key_check: got %v", r.Rows)
	}

	// foreign_keys=ON enables enforcement: a NEW orphan insert now fails.
	if res := db.Exec("PRAGMA foreign_keys=ON"); res.Error != nil {
		t.Fatalf("set foreign_keys: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO child VALUES(100)"); res.Error == nil {
		t.Fatalf("expected FK violation with foreign_keys=ON, got success")
	}
}

// TestP5PragmaIntrospection verifies collation_list, database_list, and
// compile_options return the expected rows.
func TestP5PragmaIntrospection(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := db.Query("PRAGMA collation_list")
	if r.Error != nil {
		t.Fatalf("collation_list: %v", r.Error)
	}
	// At least BINARY, NOCASE, RTRIM are registered.
	names := map[string]bool{}
	for _, row := range r.Rows {
		if len(row) >= 2 {
			names[row[1].(string)] = true
		}
	}
	for _, want := range []string{"BINARY", "NOCASE", "RTRIM"} {
		if !names[want] {
			t.Errorf("collation_list missing %s: %v", want, r.Rows)
		}
	}

	r = db.Query("PRAGMA database_list")
	if r.Error != nil {
		t.Fatalf("database_list: %v", r.Error)
	}
	if len(r.Rows) == 0 || r.Rows[0][1] != "main" {
		t.Errorf("database_list: got %v", r.Rows)
	}

	r = db.Query("PRAGMA compile_options")
	if r.Error != nil {
		t.Fatalf("compile_options: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Errorf("compile_options returned no rows")
	}
}

// TestP5PragmaIntegrity verifies integrity_check / quick_check on a clean DB
// (ok) and detects CHECK/NOT NULL violations.
func TestP5PragmaIntegrity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE t(a CHECK(a>0), b NOT NULL); INSERT INTO t VALUES(1, 1);`); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check clean: got %v", r.Rows)
	}

	r = db.Query("PRAGMA quick_check")
	if r.Error != nil {
		t.Fatalf("quick_check: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("quick_check clean: got %v", r.Rows)
	}

	// A CHECK-violating row (written under ignore_check_constraints) is caught.
	if res := db.Exec("PRAGMA ignore_check_constraints=ON; INSERT INTO t VALUES(-1, 2); PRAGMA ignore_check_constraints=OFF"); res.Error != nil {
		t.Fatalf("insert bad: %v", res.Error)
	}
	r = db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check bad: %v", r.Error)
	}
	if len(r.Rows) != 1 || !strings.Contains(r.Rows[0][0].(string), "CHECK constraint failed") {
		t.Fatalf("integrity_check violation: got %v", r.Rows)
	}
}

// TestP5PragmaHeaderRoundTrip verifies user_version / application_id set+get
// round-trips persist in the database header.
func TestP5PragmaHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hv.db"

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("PRAGMA user_version = 42"); res.Error != nil {
		t.Fatalf("set user_version: %v", res.Error)
	}
	if res := db.Exec("PRAGMA application_id = 0x12345678"); res.Error != nil {
		t.Fatalf("set application_id: %v", res.Error)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the header values persist.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	r := db2.Query("PRAGMA user_version")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(42) {
		t.Fatalf("user_version round-trip: got %v err %v", r.Rows, r.Error)
	}
	r = db2.Query("PRAGMA application_id")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(0x12345678) {
		t.Fatalf("application_id round-trip: got %v err %v", r.Rows, r.Error)
	}
}

// TestP5PragmaCaseSensitiveLike verifies the case_sensitive_like toggle changes
// LIKE behavior.
func TestP5PragmaCaseSensitiveLike(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec("CREATE TABLE t(s); INSERT INTO t VALUES('Hello'),('hello'),('HELLO');"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	// Default (case-insensitive): all three match 'h%'.
	r := db.Query("SELECT count(*) FROM t WHERE s LIKE 'h%'")
	if r.Error != nil || r.Rows[0][0] != int64(3) {
		t.Fatalf("LIKE default: got %v err %v", r.Rows, r.Error)
	}

	// case_sensitive_like=ON: only the lowercase 'hello' matches.
	if res := db.Exec("PRAGMA case_sensitive_like=ON"); res.Error != nil {
		t.Fatalf("set case_sensitive_like: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM t WHERE s LIKE 'h%'")
	if r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("LIKE case-sensitive: got %v err %v", r.Rows, r.Error)
	}

	// Toggle back off.
	if res := db.Exec("PRAGMA case_sensitive_like=OFF"); res.Error != nil {
		t.Fatalf("set case_sensitive_like off: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM t WHERE s LIKE 'h%'")
	if r.Error != nil || r.Rows[0][0] != int64(3) {
		t.Fatalf("LIKE after toggle: got %v err %v", r.Rows, r.Error)
	}
}

// TestP5PragmaRecursiveTriggers verifies the recursive_triggers toggle changes
// trigger recursion behavior.
func TestP5PragmaRecursiveTriggers(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`
		CREATE TABLE log(msg);
		CREATE TABLE t(a);
		CREATE TRIGGER trg AFTER INSERT ON t WHEN NEW.a < 3 BEGIN
			INSERT INTO log VALUES('fired');
			INSERT INTO t VALUES(NEW.a+1);
		END;`); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	// recursive_triggers=OFF (default): the trigger fires once; the nested
	// INSERT does not re-fire the trigger. One log row.
	if res := db.Exec("INSERT INTO t VALUES(1)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	r := db.Query("SELECT count(*) FROM log")
	if r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("recursive off: got %v err %v", r.Rows, r.Error)
	}

	// recursive_triggers=ON: the nested INSERT re-fires, recursing until
	// NEW.a >= 3. Log rows for the a=1 and a=2 firings (2 total); the a=3
	// insert's WHEN clause is false so it does not fire.
	if res := db.Exec("PRAGMA recursive_triggers=ON; DELETE FROM t; DELETE FROM log; INSERT INTO t VALUES(1)"); res.Error != nil {
		t.Fatalf("insert recursive: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM log")
	if r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Fatalf("recursive on: got %v err %v", r.Rows, r.Error)
	}
}

// TestP5PragmaFunctionStyle verifies the function-style table-valued pragmas.
func TestP5PragmaFunctionStyle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec("CREATE TABLE t(x, y); CREATE INDEX i1 ON t(y);"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	// pragma_table_info('t')
	r := db.Query("SELECT count(*) FROM pragma_table_info('t')")
	if r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Fatalf("pragma_table_info: got %v err %v", r.Rows, r.Error)
	}

	// pragma_index_list('t')
	r = db.Query("SELECT name FROM pragma_index_list('t')")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "i1" {
		t.Fatalf("pragma_index_list: got %v err %v", r.Rows, r.Error)
	}

	// pragma_index_info('i1')
	r = db.Query("SELECT name FROM pragma_index_info('i1')")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "y" {
		t.Fatalf("pragma_index_info: got %v err %v", r.Rows, r.Error)
	}

	// pragma_table_list()
	r = db.Query("SELECT name FROM pragma_table_list() WHERE type='table' AND name='t'")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("pragma_table_list: got %v err %v", r.Rows, r.Error)
	}
}

// valuesEqual compares two SQL values with the engine's numeric/string
// conventions for the pre-test assertions.
func valuesEqual(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return as == bs
	}
	ai, aok2 := a.(int64)
	bi, bok2 := b.(int64)
	if aok2 && bok2 {
		return ai == bi
	}
	return a == b
}
