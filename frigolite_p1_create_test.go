package frigolite

import (
	"testing"
)

// TestP1CreateColumnTypes verifies CREATE TABLE with all SQLite column types.
func TestP1CreateColumnTypes(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	tests := []struct {
		name string
		ddl  string
	}{
		{"INTEGER", "CREATE TABLE t1(a INTEGER)"},
		{"INT", "CREATE TABLE t2(a INT)"},
		{"TEXT", "CREATE TABLE t3(a TEXT)"},
		{"REAL", "CREATE TABLE t4(a REAL)"},
		{"BLOB", "CREATE TABLE t5(a BLOB)"},
		{"NUMERIC", "CREATE TABLE t6(a NUMERIC)"},
		{"VARCHAR", "CREATE TABLE t7(a VARCHAR(255))"},
		{"CHARACTER", "CREATE TABLE t8(a CHARACTER(10))"},
		{"DOUBLE", "CREATE TABLE t9(a DOUBLE PRECISION)"},
		{"no type", "CREATE TABLE t10(a)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := db.Exec(tt.ddl)
			if res.Error != nil {
				t.Errorf("CREATE TABLE %s: %v", tt.name, res.Error)
			}
		})
	}
}

// TestP1CreateColumnConstraints verifies column-level constraints.
func TestP1CreateColumnConstraints(t *testing.T) {
	tests := []struct {
		name string
		ddl  string
	}{
		{"PRIMARY KEY", "CREATE TABLE t(a INTEGER PRIMARY KEY)"},
		{"NOT NULL", "CREATE TABLE t(a TEXT NOT NULL)"},
		{"UNIQUE", "CREATE TABLE t(a TEXT UNIQUE)"},
		{"DEFAULT literal", "CREATE TABLE t(a INTEGER DEFAULT 0)"},
		{"DEFAULT string", "CREATE TABLE t(a TEXT DEFAULT 'hello')"},
		{"DEFAULT expr", "CREATE TABLE t(a INTEGER DEFAULT (1+2))"},
		{"DEFAULT CURRENT_TIME", "CREATE TABLE t(a TEXT DEFAULT CURRENT_TIME)"},
		{"CHECK", "CREATE TABLE t(a INTEGER CHECK(a > 0))"},
		{"REFERENCES", "CREATE TABLE p(a INTEGER PRIMARY KEY); CREATE TABLE c(a INTEGER REFERENCES p(a))"},
		{"combined", "CREATE TABLE t(a INTEGER PRIMARY KEY NOT NULL)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupDB(t)
			defer db.Close()
			res := db.Exec(tt.ddl)
			if res.Error != nil {
				t.Errorf("DDL error: %v\n  sql: %s", res.Error, tt.ddl)
			}
		})
	}
}

// TestP1CreateTableConstraints verifies table-level constraints.
func TestP1CreateTableConstraints(t *testing.T) {
	tests := []struct {
		name string
		ddl  string
	}{
		{"composite PK", "CREATE TABLE t(a INTEGER, b INTEGER, PRIMARY KEY(a,b))"},
		{"table UNIQUE", "CREATE TABLE t(a INTEGER, b INTEGER, UNIQUE(a))"},
		{"table CHECK", "CREATE TABLE t(a INTEGER, CHECK(a > 0))"},
		{"table FK", "CREATE TABLE p(a INTEGER PRIMARY KEY); CREATE TABLE c(a INTEGER, FOREIGN KEY(a) REFERENCES p)"},
		{"multi PK", "CREATE TABLE t(a INTEGER, b TEXT, c REAL, PRIMARY KEY(a,b,c))"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupDB(t)
			defer db.Close()
			res := db.Exec(tt.ddl)
			if res.Error != nil {
				t.Errorf("DDL error: %v\n  sql: %s", res.Error, tt.ddl)
			}
		})
	}
}

// TestP1CreateWithoutRowid tests WITHOUT ROWID tables.
func TestP1CreateWithoutRowid(t *testing.T) {
	t.Run("basic create + insert", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res = db.Exec("INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c')")
		if res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT * FROM t ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 3 {
			t.Errorf("expected 3 rows, got %d: %v", len(r.Rows), r.Rows)
		}
	})

	t.Run("PK uniqueness enforced", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a TEXT, b TEXT, PRIMARY KEY(a)) WITHOUT ROWID")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		db.Exec("INSERT INTO t VALUES('abc','def')")
		res = db.Exec("INSERT INTO t VALUES('abc','ghi')")
		if res.Error == nil {
			t.Errorf("expected UNIQUE constraint failure for duplicate PK in WITHOUT ROWID table")
		}
	})

	t.Run("composite PK uniqueness", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a TEXT, b TEXT, c TEXT, d TEXT, PRIMARY KEY(c,a)) WITHOUT ROWID")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		db.Exec("INSERT INTO t VALUES('journal','sherman','ammonia','helena')")
		// Same (c,a) = (ammonia,journal) → should conflict
		res = db.Exec("INSERT INTO t VALUES('journal','jones','ammonia','x')")
		if res.Error == nil {
			t.Errorf("expected UNIQUE constraint failure for duplicate composite PK")
		}
		// Different (c,a) = (ammonia,arctic) → should succeed
		res = db.Exec("INSERT INTO t VALUES('arctic','sleep','ammonia','helena')")
		if res.Error != nil {
			t.Errorf("expected success for distinct composite PK: %v", res.Error)
		}
	})

	t.Run("REPLACE INTO replaces conflicting row", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a TEXT, b TEXT, PRIMARY KEY(a)) WITHOUT ROWID")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		db.Exec("INSERT INTO t VALUES('key','old')")
		res = db.Exec("REPLACE INTO t VALUES('key','new')")
		if res.Error != nil {
			t.Fatalf("REPLACE: %v", res.Error)
		}
		r := db.Query("SELECT b FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 {
			t.Errorf("expected 1 row after REPLACE, got %d: %v", len(r.Rows), r.Rows)
		}
		if len(r.Rows) > 0 && fmtVal(r.Rows[0][0]) != "new" {
			t.Errorf("expected 'new', got %v", r.Rows[0][0])
		}
	})
}

// TestP1CreateStrict tests STRICT tables.
func TestP1CreateStrict(t *testing.T) {
	t.Run("missing datatype rejected", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a) STRICT")
		if res.Error == nil {
			t.Errorf("expected error for STRICT table column without datatype")
		}
	})

	t.Run("unknown datatype rejected", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a BANJO) STRICT")
		if res.Error == nil {
			t.Errorf("expected error for STRICT table column with unknown datatype")
		}
	})

	t.Run("valid strict types accepted", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a INT, b INTEGER, c TEXT, d REAL, e BLOB) STRICT")
		if res.Error != nil {
			t.Errorf("valid STRICT table: %v", res.Error)
		}
	})

	t.Run("wrong type rejected on INSERT", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a TEXT) STRICT")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		// Inserting INTEGER into TEXT column should fail in STRICT mode
		res = db.Exec("INSERT INTO t VALUES(123)")
		if res.Error == nil {
			t.Errorf("expected error inserting INTEGER into STRICT TEXT column")
		}
		// Correct type should work
		res = db.Exec("INSERT INTO t VALUES('hello')")
		if res.Error != nil {
			t.Errorf("valid text insert: %v", res.Error)
		}
	})

	t.Run("INTEGER strict rejects text", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a INTEGER) STRICT")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res = db.Exec("INSERT INTO t VALUES('not a number')")
		if res.Error == nil {
			t.Errorf("expected error inserting text into STRICT INTEGER column")
		}
		// Text that looks like integer should be accepted
		res = db.Exec("INSERT INTO t VALUES('42')")
		if res.Error != nil {
			t.Errorf("numeric-looking text into STRICT INT should work: %v", res.Error)
		}
	})

	t.Run("REAL strict rejects text", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a REAL) STRICT")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res = db.Exec("INSERT INTO t VALUES('not a number')")
		if res.Error == nil {
			t.Errorf("expected error inserting text into STRICT REAL column")
		}
	})
}

// TestP1CreateIfNotExists tests IF NOT EXISTS.
func TestP1CreateIfNotExists(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// First CREATE succeeds
	res := db.Exec("CREATE TABLE t(a INTEGER)")
	if res.Error != nil {
		t.Fatalf("first CREATE: %v", res.Error)
	}

	// Second CREATE without IF NOT EXISTS should fail
	res = db.Exec("CREATE TABLE t(a INTEGER)")
	if res.Error == nil {
		t.Errorf("expected error for duplicate CREATE TABLE")
	}

	// CREATE IF NOT EXISTS should be a no-op
	res = db.Exec("CREATE TABLE IF NOT EXISTS t(a INTEGER)")
	if res.Error != nil {
		t.Errorf("CREATE IF NOT EXISTS should succeed: %v", res.Error)
	}
}

// TestP1CreateTableAsSelect tests CREATE TABLE AS SELECT.
func TestP1CreateTableAsSelect(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE src(a INTEGER, b TEXT)")
	db.Exec("INSERT INTO src VALUES(1,'hello'),(2,'world')")

	res := db.Exec("CREATE TABLE dst AS SELECT * FROM src")
	if res.Error != nil {
		t.Fatalf("CTAS: %v", res.Error)
	}

	r := db.Query("SELECT * FROM dst ORDER BY a")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if len(r.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(r.Rows))
	}
	// Verify column names and types
	if len(r.Columns) != 2 {
		t.Errorf("expected 2 columns, got %d: %v", len(r.Columns), r.Columns)
	}
}

// TestP1CreateAutoincrement tests AUTOINCREMENT.
func TestP1CreateAutoincrement(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	res := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT)")
	if res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	db.Exec("INSERT INTO t(b) VALUES('x')")
	db.Exec("INSERT INTO t(b) VALUES('y')")
	db.Exec("INSERT INTO t(b) VALUES('z')")

	r := db.Query("SELECT * FROM t ORDER BY a")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(r.Rows))
	}
	// Verify auto-incrementing IDs
	for i, row := range r.Rows {
		expected := int64(i + 1)
		if fmtVal(row[0]) != fmtVal(expected) {
			t.Errorf("row %d: expected id=%d, got %v", i, expected, row[0])
		}
	}
}

// TestP1CreateGeneratedColumns tests GENERATED ALWAYS AS columns.
func TestP1CreateGeneratedColumns(t *testing.T) {
	t.Run("VIRTUAL generated column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a INTEGER, b INTEGER GENERATED ALWAYS AS (a*2) VIRTUAL)")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res = db.Exec("INSERT INTO t(a) VALUES(5)")
		if res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT * FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(r.Rows))
		}
		if fmtVal(r.Rows[0][1]) != "10" {
			t.Errorf("expected b=10, got %v", r.Rows[0][1])
		}
	})

	t.Run("STORED generated column", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a INTEGER, b INTEGER GENERATED ALWAYS AS (a*2) STORED)")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		res = db.Exec("INSERT INTO t(a) VALUES(7)")
		if res.Error != nil {
			t.Fatalf("insert: %v", res.Error)
		}
		r := db.Query("SELECT * FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(r.Rows))
		}
		if fmtVal(r.Rows[0][1]) != "14" {
			t.Errorf("expected b=14, got %v", r.Rows[0][1])
		}
	})

	t.Run("generated column in WHERE", func(t *testing.T) {
		db := setupDB(t)
		defer db.Close()

		res := db.Exec("CREATE TABLE t(a INTEGER, b INTEGER GENERATED ALWAYS AS (a+10) VIRTUAL)")
		if res.Error != nil {
			t.Fatalf("create: %v", res.Error)
		}
		db.Exec("INSERT INTO t(a) VALUES(1),(2),(3)")
		r := db.Query("SELECT a FROM t WHERE b > 11 ORDER BY a")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) != 2 {
			t.Errorf("expected 2 rows where b>11, got %d: %v", len(r.Rows), r.Rows)
		}
	})
}

// TestP1CreateTypeAffinity tests SQLite type affinity rules.
func TestP1CreateTypeAffinity(t *testing.T) {
	tests := []struct {
		name       string
		ddl        string
		insert     string
		wantType   string // expected typeof()
		wantValue  string
	}{
		// TEXT affinity: INT stored as TEXT
		{"TEXT affinity from VARCHAR", "CREATE TABLE t(a VARCHAR(10))", "INSERT INTO t VALUES(123)", "text", "123"},
		// INTEGER affinity: '123' stored as INTEGER
		{"INTEGER affinity", "CREATE TABLE t(a INTEGER)", "INSERT INTO t VALUES('123')", "integer", "123"},
		// REAL affinity
		{"REAL affinity", "CREATE TABLE t(a REAL)", "INSERT INTO t VALUES(44)", "real", "44.0"},
		// BLOB affinity (no conversion)
		{"BLOB affinity", "CREATE TABLE t(a BLOB)", "INSERT INTO t VALUES('text')", "text", "text"},
		// NUMERIC affinity
		{"NUMERIC affinity", "CREATE TABLE t(a NUMERIC)", "INSERT INTO t VALUES(3.0)", "integer", "3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupDB(t)
			defer db.Close()

			res := db.Exec(tt.ddl)
			if res.Error != nil {
				t.Fatalf("create: %v", res.Error)
			}
			res = db.Exec(tt.insert)
			if res.Error != nil {
				t.Fatalf("insert: %v", res.Error)
			}
			r := db.Query("SELECT typeof(a), a FROM t")
			if r.Error != nil {
				t.Fatalf("select: %v", r.Error)
			}
			if len(r.Rows) != 1 {
				t.Fatalf("expected 1 row, got %d", len(r.Rows))
			}
			if got := fmtVal(r.Rows[0][0]); got != tt.wantType {
				t.Errorf("typeof: got %s, want %s", got, tt.wantType)
			}
			if got := fmtVal(r.Rows[0][1]); got != tt.wantValue {
				t.Errorf("value: got %s, want %s", got, tt.wantValue)
			}
		})
	}
}

// TestP1CreateMultiPKOrdering tests that WITHOUT ROWID tables are ordered by PK.
func TestP1CreateMultiPKOrdering(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// PK(c,a) means rows should be ordered by (c,a)
	res := db.Exec("CREATE TABLE t(a TEXT, b TEXT, c TEXT, PRIMARY KEY(c,a)) WITHOUT ROWID")
	if res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	// Insert out of order
	db.Exec("INSERT INTO t VALUES('journal','sherman','ammonia','helena')")
	db.Exec("INSERT INTO t VALUES('dynamic','juliet','flipper','command')")
	db.Exec("INSERT INTO t VALUES('journal','sherman','gamma','patriot')")
	db.Exec("INSERT INTO t VALUES('arctic','sleep','ammonia','helena')")

	// SELECT without ORDER BY should return in PK order: (ammonia,arctic), (ammonia,journal), (flipper,dynamic), (gamma,journal)
	r := db.Query("SELECT a FROM t")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}

	want := []string{"arctic", "journal", "dynamic", "journal"}
	if len(r.Rows) != len(want) {
		t.Fatalf("expected %d rows, got %d: %v", len(want), len(r.Rows), r.Rows)
	}
	for i, row := range r.Rows {
		if got := fmtVal(row[0]); got != want[i] {
			t.Errorf("row %d: got %s, want %s (full: %v)", i, got, want[i], r.Rows)
		}
	}
}

// fmtVal converts a value to its string representation for comparison.
func fmtVal(v interface{}) string {
	if v == nil {
		return ""
	}
	return formatSQLiteValue(v)
}
