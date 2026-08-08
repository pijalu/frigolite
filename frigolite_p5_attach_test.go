package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestP5AttachBasics verifies ATTACH opens a second pager under the alias and
// schema-qualified names resolve to the attached database.
func TestP5AttachBasics(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE t1(a INTEGER, b TEXT)`); res.Error != nil {
		t.Fatalf("create main: %v", res.Error)
	}
	if res := db.Exec(`INSERT INTO t1 VALUES(1,'one'),(2,'two')`); res.Error != nil {
		t.Fatalf("insert main: %v", res.Error)
	}

	// Second connection creates the file to be attached.
	auxDB, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	if res := auxDB.Exec(`CREATE TABLE t2(x INTEGER, y TEXT)`); res.Error != nil {
		t.Fatalf("create aux: %v", res.Error)
	}
	if res := auxDB.Exec(`INSERT INTO t2 VALUES(10,'ten'),(20,'twenty')`); res.Error != nil {
		t.Fatalf("insert aux: %v", res.Error)
	}
	auxDB.Close()

	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}

	// Schema-qualified read.
	r := db.Query("SELECT x, y FROM aux.t2 ORDER BY x")
	if r.Error != nil {
		t.Fatalf("select aux.t2: %v", r.Error)
	}
	want := [][]interface{}{{int64(10), "ten"}, {int64(20), "twenty"}}
	if len(r.Rows) != len(want) {
		t.Fatalf("aux.t2 rows: got %d want %d", len(r.Rows), len(want))
	}
	for i, row := range r.Rows {
		for j := range want[i] {
			if !valuesEqual(row[j], want[i][j]) {
				t.Errorf("aux.t2[%d][%d]: got %v want %v", i, j, row[j], want[i][j])
			}
		}
	}

	// DML on the attached database.
	if res := db.Exec(`INSERT INTO aux.t2 VALUES(30,'thirty')`); res.Error != nil {
		t.Fatalf("insert aux.t2: %v", res.Error)
	}
	if res := db.Exec(`UPDATE aux.t2 SET y='THIRTY' WHERE x=30`); res.Error != nil {
		t.Fatalf("update aux.t2: %v", res.Error)
	}
	if res := db.Exec(`DELETE FROM aux.t2 WHERE x=10`); res.Error != nil {
		t.Fatalf("delete aux.t2: %v", res.Error)
	}
	r = db.Query("SELECT x, y FROM aux.t2 ORDER BY x")
	if r.Error != nil {
		t.Fatalf("select aux.t2 after dml: %v", r.Error)
	}
	want = [][]interface{}{{int64(20), "twenty"}, {int64(30), "THIRTY"}}
	if len(r.Rows) != len(want) {
		t.Fatalf("aux.t2 after dml: got %d rows want %d", len(r.Rows), len(want))
	}
	for i, row := range r.Rows {
		for j := range want[i] {
			if !valuesEqual(row[j], want[i][j]) {
				t.Errorf("aux.t2 dml[%d][%d]: got %v want %v", i, j, row[j], want[i][j])
			}
		}
	}
}

// TestP5AttachCrossSchema verifies cross-schema queries and joins between
// main and an attached database.
func TestP5AttachCrossSchema(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.Exec(`CREATE TABLE t1(a INTEGER, b TEXT)`)
	db.Exec(`INSERT INTO t1 VALUES(1,'one'),(2,'two')`)

	auxDB, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	auxDB.Exec(`CREATE TABLE t2(x INTEGER, y TEXT)`)
	auxDB.Exec(`INSERT INTO t2 VALUES(10,'ten'),(20,'twenty')`)
	auxDB.Close()

	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}

	// Cross-schema join: main.t1 JOIN aux.t2.
	r := db.Query("SELECT a, x FROM t1 JOIN aux.t2 ON a*10=x ORDER BY a")
	if r.Error != nil {
		t.Fatalf("cross-schema join: %v", r.Error)
	}
	want := [][]interface{}{{int64(1), int64(10)}, {int64(2), int64(20)}}
	if len(r.Rows) != len(want) {
		t.Fatalf("join rows: got %d want %d", len(r.Rows), len(want))
	}
	for i, row := range r.Rows {
		for j := range want[i] {
			if !valuesEqual(row[j], want[i][j]) {
				t.Errorf("join[%d][%d]: got %v want %v", i, j, row[j], want[i][j])
			}
		}
	}

	// UNION across schemas.
	r = db.Query("SELECT b FROM t1 UNION ALL SELECT y FROM aux.t2 ORDER BY 1")
	if r.Error != nil {
		t.Fatalf("cross-schema union: %v", r.Error)
	}
	if len(r.Rows) != 4 {
		t.Fatalf("union rows: got %d want 4", len(r.Rows))
	}
}

// TestP5AttachDetach verifies DETACH closes the second pager and that main /
// temp / non-attached names cannot be detached.
func TestP5AttachDetach(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	auxDB, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	auxDB.Exec(`CREATE TABLE t2(x)`)
	auxDB.Close()

	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}

	// Query works while attached.
	if r := db.Query("SELECT * FROM aux.t2"); r.Error != nil {
		t.Fatalf("aux.t2: %v", r.Error)
	}

	// Cannot detach main / temp / temporary.
	if res := db.Exec("DETACH DATABASE main"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "cannot detach") {
		t.Errorf("detach main: want 'cannot detach', got %v", res.Error)
	}
	if res := db.Exec("DETACH DATABASE temp"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "cannot detach") {
		t.Errorf("detach temp: want 'cannot detach', got %v", res.Error)
	}
	if res := db.Exec("DETACH DATABASE temporary"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "cannot detach") {
		t.Errorf("detach temporary: want 'cannot detach', got %v", res.Error)
	}

	// Detach aux; afterwards aux.t2 is gone.
	if res := db.Exec("DETACH DATABASE aux"); res.Error != nil {
		t.Fatalf("detach aux: %v", res.Error)
	}
	if r := db.Query("SELECT * FROM aux.t2"); r.Error == nil {
		t.Errorf("aux.t2 after detach: want error, got none")
	}

	// Detaching a non-attached name errors.
	if res := db.Exec("DETACH DATABASE nosuch"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "no such database") {
		t.Errorf("detach nosuch: want 'no such database', got %v", res.Error)
	}
}

// TestP5AttachDatabaseList verifies PRAGMA database_list reports attached
// schemas in attachment order (main first, then temp, then attached).
func TestP5AttachDatabaseList(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	auxDB, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	auxDB.Exec(`CREATE TABLE t2(x)`)
	auxDB.Close()

	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}

	r := db.Query("PRAGMA database_list")
	if r.Error != nil {
		t.Fatalf("database_list: %v", r.Error)
	}
	if len(r.Columns) != 3 {
		t.Fatalf("database_list cols: got %v", r.Columns)
	}
	if len(r.Rows) < 2 {
		t.Fatalf("database_list rows: got %d want >= 2", len(r.Rows))
	}
	// Row 0 is main (seq 0, name main, file = mainPath).
	if !valuesEqual(r.Rows[0][0], int64(0)) || !valuesEqual(r.Rows[0][1], "main") {
		t.Errorf("database_list[0]: got %v want [0 main ...]", r.Rows[0])
	}
	// Find the aux row and confirm its name/file.
	found := false
	for _, row := range r.Rows {
		if valuesEqual(row[1], "aux") {
			found = true
			if !valuesEqual(row[0], int64(1)) {
				t.Errorf("aux seq: got %v want 1", row[0])
			}
			if !valuesEqual(row[2], auxPath) {
				t.Errorf("aux file: got %v want %v", row[2], auxPath)
			}
		}
	}
	if !found {
		t.Errorf("database_list missing aux row: %v", r.Rows)
	}
}

// TestP5AttachLimit verifies SQLITE_MAX_ATTACHED (10) is enforced.
func TestP5AttachLimit(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create 10 attachable files (main is not counted).
	var files []string
	for i := 0; i < 11; i++ {
		p := filepath.Join(dir, "aux"+string(rune('a'+i))+".db")
		files = append(files, p)
		auxDB, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		auxDB.Exec(`CREATE TABLE t(x)`)
		auxDB.Close()
	}

	// Attaching the 10th attached DB succeeds; the 11th fails with
	// "too many attached databases".
	for i := 0; i < 10; i++ {
		name := "aux" + string(rune('a'+i))
		if res := db.Exec("ATTACH '" + files[i] + "' AS " + name); res.Error != nil {
			t.Fatalf("attach %d (%s): %v", i, name, res.Error)
		}
	}
	res := db.Exec("ATTACH '" + files[10] + "' AS auxk")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "too many attached databases") {
		t.Errorf("11th attach: want 'too many attached databases', got %v", res.Error)
	}
}

// TestP5AttachCreateOnAttached verifies DDL against a schema-qualified name
// (CREATE TABLE aux.t1) and subsequent DML.
func TestP5AttachCreateOnAttached(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	auxDB, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	auxDB.Close()

	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}
	if res := db.Exec(`CREATE TABLE aux.t1(a INTEGER, b TEXT)`); res.Error != nil {
		t.Fatalf("create aux.t1: %v", res.Error)
	}
	if res := db.Exec(`INSERT INTO aux.t1 VALUES(1,'x'),(2,'y')`); res.Error != nil {
		t.Fatalf("insert aux.t1: %v", res.Error)
	}
	r := db.Query("SELECT a, b FROM aux.t1 ORDER BY a")
	if r.Error != nil {
		t.Fatalf("select aux.t1: %v", r.Error)
	}
	want := [][]interface{}{{int64(1), "x"}, {int64(2), "y"}}
	if len(r.Rows) != len(want) {
		t.Fatalf("aux.t1 rows: got %d want %d", len(r.Rows), len(want))
	}
	for i, row := range r.Rows {
		for j := range want[i] {
			if !valuesEqual(row[j], want[i][j]) {
				t.Errorf("aux.t1[%d][%d]: got %v want %v", i, j, row[j], want[i][j])
			}
		}
	}

	// The table persists on disk after detach (re-open aux file).
	if res := db.Exec("DETACH DATABASE aux"); res.Error != nil {
		t.Fatalf("detach: %v", res.Error)
	}
	reopen, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	r = reopen.Query("SELECT count(*) FROM t1")
	if r.Error != nil {
		t.Fatalf("reopened aux t1: %v", r.Error)
	}
	if !valuesEqual(r.Rows[0][0], int64(2)) {
		t.Errorf("reopened aux count: got %v want 2", r.Rows[0][0])
	}
}

// TestP5AttachDuplicateName verifies attaching the same name twice errors.
func TestP5AttachDuplicateName(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	auxDB, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	auxDB.Close()

	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}
	if res := db.Exec("ATTACH '" + auxPath + "' AS aux"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "already in use") {
		t.Errorf("duplicate attach: want 'already in use', got %v", res.Error)
	}
	// Case-insensitive: AUX is also in use.
	if res := db.Exec("ATTACH '" + auxPath + "' AS AUX"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "already in use") {
		t.Errorf("duplicate attach AUX: want 'already in use', got %v", res.Error)
	}
}
