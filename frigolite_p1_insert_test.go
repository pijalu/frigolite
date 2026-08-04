package frigolite

import (
	"strings"
	"testing"
)

// TestP1InsertValues covers basic INSERT with VALUES lists: single row,
// explicit column lists, and partial column lists (missing columns use
// their DEFAULTs).
func TestP1InsertValues(t *testing.T) {
	t.Run("single row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT, c REAL)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'one',1.5)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT a, b, c FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 3 {
			t.Fatalf("expected 1x3 result, got %v", r.Rows)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[0][1] != "one" || r.Rows[0][2] != 1.5 {
			t.Errorf("unexpected row: %v", r.Rows[0])
		}
	})

	t.Run("explicit column list", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT, c REAL)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t(b, a) VALUES('x', 7)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT a, b, c FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		// c has no default -> NULL
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(7) || r.Rows[0][1] != "x" || r.Rows[0][2] != nil {
			t.Errorf("unexpected row: %v", r.Rows[0])
		}
	})

	t.Run("column count mismatch", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		// Too few values (no column list) -> SQLite errors.
		res := db.Exec("INSERT INTO t VALUES(1)")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "has 2 columns but 1 values were supplied") {
			t.Errorf("expected column count error, got: %v", res.Error)
		}
		// Too many values -> SQLite errors.
		res = db.Exec("INSERT INTO t VALUES(1,'a',3)")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "has 3 values for 2 columns") {
			t.Errorf("expected too-many error, got: %v", res.Error)
		}
		// Column list with wrong value count -> SQLite errors.
		res = db.Exec("INSERT INTO t(a,b) VALUES(1,2,3)")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "has 3 values for 2 columns") {
			t.Errorf("expected column-list error, got: %v", res.Error)
		}
	})

	t.Run("NULL values", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(NULL, NULL)"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT a IS NULL, b IS NULL FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != int64(1) {
			t.Errorf("expected NULLs, got %v", r.Rows)
		}
	})
}

// TestP1InsertMultiRow covers multi-row INSERT ... VALUES (a),(b),(c).
func TestP1InsertMultiRow(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER, b TEXT)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c')"); res.Error != nil {
		t.Fatalf("multi-row insert: %v", res.Error)
	}
	r := db.Query("SELECT count(*), sum(a) FROM t")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if r.Rows[0][0] != int64(3) || r.Rows[0][1] != int64(6) {
		t.Errorf("expected count=3 sum=6, got %v", r.Rows[0])
	}
}

// TestP1InsertSelect covers INSERT ... SELECT, including expressions and
// LIMIT/OFFSET in the source query.
func TestP1InsertSelect(t *testing.T) {
	t.Run("copy rows", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE src(a INTEGER, b TEXT)"); res.Error != nil {
			t.Fatalf("create src: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')"); res.Error != nil {
			t.Fatalf("seed src: %v", res.Error)
		}
		if res := db.Exec("CREATE TABLE dst(a INTEGER, b TEXT)"); res.Error != nil {
			t.Fatalf("create dst: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO dst SELECT * FROM src"); res.Error != nil {
			t.Fatalf("insert select: %v", res.Error)
		}
		r := db.Query("SELECT count(*), sum(a) FROM dst")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(3) || r.Rows[0][1] != int64(6) {
			t.Errorf("expected count=3 sum=6, got %v", r.Rows[0])
		}
	})

	t.Run("expression with limit", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE dst(a INTEGER, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO dst SELECT i*10, 'row'||i FROM (SELECT 1 i UNION ALL SELECT 2 UNION ALL SELECT 3) LIMIT 2"); res.Error != nil {
			t.Fatalf("insert select expr: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM dst ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 {
			t.Fatalf("expected 2 rows, got %v", r.Rows)
		}
		if r.Rows[0][0] != int64(10) || r.Rows[0][1] != "row1" || r.Rows[1][0] != int64(20) || r.Rows[1][1] != "row2" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})
}

// TestP1InsertDefaultValues covers INSERT DEFAULT VALUES and column-level
// DEFAULTs (literals, expressions, CURRENT_* keywords).
func TestP1InsertDefaultValues(t *testing.T) {
	t.Run("default values row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER DEFAULT 42, b TEXT DEFAULT 'hi', c REAL DEFAULT 1.5)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t DEFAULT VALUES"); res.Error != nil {
			t.Fatalf("insert default values: %v", res.Error)
		}
		r := db.Query("SELECT a, b, c FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(42) || r.Rows[0][1] != "hi" || r.Rows[0][2] != 1.5 {
			t.Errorf("unexpected row: %v", r.Rows[0])
		}
	})

	t.Run("default expression", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER DEFAULT (1+2*3))"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t DEFAULT VALUES"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT a FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(7) {
			t.Errorf("expected 7, got %v", r.Rows[0][0])
		}
	})

	t.Run("implicit default via column list", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER DEFAULT 9, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t(b) VALUES('only-b')"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(9) || r.Rows[0][1] != "only-b" {
			t.Errorf("unexpected row: %v", r.Rows[0])
		}
	})

	t.Run("default parenthesized expression", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER DEFAULT (5+5), b TEXT DEFAULT ('pre'||'fix'))"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t DEFAULT VALUES"); res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(10) || r.Rows[0][1] != "prefix" {
			t.Errorf("unexpected row: %v", r.Rows)
		}
	})
}

// TestP1InsertUpsert covers UPSERT (ON CONFLICT ... DO UPDATE / DO NOTHING).
func TestP1InsertUpsert(t *testing.T) {
	t.Run("do nothing", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x')"); res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO NOTHING"); res.Error != nil {
			t.Fatalf("upsert do nothing: %v", res.Error)
		}
		r := db.Query("SELECT b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != "x" {
			t.Errorf("expected row preserved, got %v", r.Rows)
		}
	})

	t.Run("do update", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x')"); res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b=excluded.b"); res.Error != nil {
			t.Fatalf("upsert do update: %v", res.Error)
		}
		r := db.Query("SELECT b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != "y" {
			t.Errorf("expected updated row, got %v", r.Rows)
		}
	})

	t.Run("do update multi-row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x'),(2,'y')"); res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'new1'),(3,'new3') ON CONFLICT(a) DO UPDATE SET b=excluded.b"); res.Error != nil {
			t.Fatalf("upsert: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 3 || r.Rows[0][1] != "new1" || r.Rows[1][1] != "y" || r.Rows[2][1] != "new3" {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})

	t.Run("do nothing without target", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER UNIQUE, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'x')"); res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'y') ON CONFLICT DO NOTHING"); res.Error != nil {
			t.Fatalf("upsert bare: %v", res.Error)
		}
		r := db.Query("SELECT count(*) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(1) {
			t.Errorf("expected 1 row, got %v", r.Rows[0][0])
		}
	})
}

// TestP1InsertOrIgnore covers INSERT OR IGNORE (and ON CONFLICT DO NOTHING
// semantics for constraint violations).
func TestP1InsertOrIgnore(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT OR IGNORE INTO t VALUES(1,'x')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	// Same PK -> ignored, no error
	if res := db.Exec("INSERT OR IGNORE INTO t VALUES(1,'y')"); res.Error != nil {
		t.Fatalf("insert or ignore conflict: %v", res.Error)
	}
	// Same UNIQUE b -> ignored
	if res := db.Exec("INSERT OR IGNORE INTO t VALUES(2,'x')"); res.Error != nil {
		t.Fatalf("insert or ignore unique: %v", res.Error)
	}
	r := db.Query("SELECT count(*), sum(a) FROM t")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if r.Rows[0][0] != int64(1) || r.Rows[0][1] != int64(1) {
		t.Errorf("expected count=1 sum=1, got %v", r.Rows[0])
	}
}

// TestP1InsertOrReplace covers INSERT OR REPLACE semantics: on conflict the
// conflicting rows are deleted and the new row is inserted.
func TestP1InsertOrReplace(t *testing.T) {
	t.Run("replace on PK", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'old')"); res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
		if res := db.Exec("INSERT OR REPLACE INTO t VALUES(1,'new')"); res.Error != nil {
			t.Fatalf("replace: %v", res.Error)
		}
		r := db.Query("SELECT count(*), b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if r.Rows[0][0] != int64(1) || r.Rows[0][1] != "new" {
			t.Errorf("expected 1 row with 'new', got %v", r.Rows[0])
		}
	})

	t.Run("replace deletes conflicting unique rows", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()
		if res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE)"); res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		if res := db.Exec("INSERT INTO t VALUES(1,'dup'),(2,'keep')"); res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
		// Replacing a=3 with b='dup' conflicts with row a=1 on UNIQUE(b):
		// that row is deleted, new row inserted.
		if res := db.Exec("INSERT OR REPLACE INTO t VALUES(3,'dup')"); res.Error != nil {
			t.Fatalf("replace unique: %v", res.Error)
		}
		r := db.Query("SELECT a, b FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(3) {
			t.Errorf("unexpected rows: %v", r.Rows)
		}
	})
}
