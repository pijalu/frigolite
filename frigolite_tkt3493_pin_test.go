package frigolite

import "testing"

// Pure-Go pin for testgen tkt3493: LEFT OUTER JOIN chain whose ON clauses
// reference only left-side tables must not raise
// "ON clause references tables to its right" (sqlite test/tkt3493.test).
func TestTkt3493Pin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"BEGIN",
		"CREATE TABLE A (id INTEGER PRIMARY KEY AUTOINCREMENT, val TEXT)",
		"INSERT INTO A VALUES(1,'123')",
		"INSERT INTO A VALUES(2,'456')",
		"CREATE TABLE B (id INTEGER PRIMARY KEY AUTOINCREMENT, val TEXT)",
		"INSERT INTO B VALUES(1,1)",
		"INSERT INTO B VALUES(2,2)",
		"CREATE TABLE A_B (B_id INTEGER NOT NULL, A_id INTEGER)",
		"INSERT INTO A_B VALUES(1,1)",
		"INSERT INTO A_B VALUES(2,2)",
		"COMMIT",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	for i, q := range []string{
		"SELECT * FROM B LEFT JOIN A_B ON B.id = A_B.B_id",
		"SELECT * FROM B LEFT JOIN A_B ON B.id = A_B.B_id LEFT JOIN A ON A.id = A_B.A_id",
		"SELECT * FROM B LEFT JOIN A ON A.id = B.id",
		"SELECT * FROM b LEFT JOIN a_b ON b.ID = a_b.b_id",
	} {
		r := db.Query(q)
		if r.Error != nil {
			t.Errorf("q%d: %v", i, r.Error)
		}
	}
	// A qualifier absent from the FROM is "no such column" even on an outer
	// join (oracle: Parse error: no such column: A_B.A_id).
	r := db.Query("SELECT * FROM B LEFT JOIN A ON A.id = A_B.A_id")
	if r.Error == nil || r.Error.Error() != "no such column: A_B.A_id" {
		t.Fatalf("bad-qualifier: want no such column error, got %v", r.Error)
	}
	r = db.Query("SELECT CASE WHEN B.val = 1 THEN 'XYZ' ELSE A.val END AS Col1 FROM B LEFT OUTER JOIN A_B ON B.id = A_B.B_id LEFT OUTER JOIN A ON A.id = A_B.A_id ORDER BY Col1 ASC")
	if r.Error != nil {
		t.Fatalf("two-join chain: %v", r.Error)
	}
}
