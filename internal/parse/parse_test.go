package parse

import (
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

func TestParseSimpleSelect(t *testing.T) {
	stmts, err := ParseSQL("SELECT a FROM t1;")
	if err != nil {
		t.Fatalf("ParseSQL failed: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("no statements returned")
	}
	t.Logf("Parsed %d statement(s)", len(stmts))
}

func TestParseSelectWithWhere(t *testing.T) {
	stmts, err := ParseSQL("SELECT a, b FROM t1 WHERE a > 1 ORDER BY b DESC;")
	if err != nil {
		t.Fatalf("ParseSQL failed: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("no statements returned")
	}
	t.Logf("Parsed %d statement(s)", len(stmts))
}

func TestParseCreateTable(t *testing.T) {
	stmts, err := ParseSQL("CREATE TABLE t1(a INTEGER, b TEXT);")
	if err != nil {
		t.Fatalf("ParseSQL failed: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("no statements returned")
	}
	t.Logf("Parsed %d statement(s)", len(stmts))
}

func TestParseInsert(t *testing.T) {
	stmts, err := ParseSQL("INSERT INTO t1 VALUES(1, 'hello');")
	if err != nil {
		t.Fatalf("ParseSQL failed: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("no statements returned")
	}
	t.Logf("Parsed %d statement(s)", len(stmts))
}

func TestParseMultipleStatements(t *testing.T) {
	sql := "CREATE TABLE t1(a INTEGER); INSERT INTO t1 VALUES(1); SELECT * FROM t1;"
	stmts, err := ParseSQL(sql)
	if err != nil {
		t.Fatalf("ParseSQL failed: %v", err)
	}
	t.Logf("Parsed %d statement(s)", len(stmts))
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(stmts))
	}
}

func TestParseSelectExcept(t *testing.T) {
	stmts, err := ParseSQL("SELECT a FROM t1 EXCEPT SELECT a FROM t2 ORDER BY a;")
	if err != nil {
		t.Fatalf("ParseSQL failed: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("no statements returned")
	}
	t.Logf("Parsed %d statement(s)", len(stmts))
}

func TestParseUpdateOrderLimit(t *testing.T) {
	cases := []struct {
		sql     string
		nOB     int
		hasLim  bool
		hasOff  bool
		wantErr bool
	}{
		{sql: "UPDATE t SET v='x' ORDER BY id LIMIT 2", nOB: 1, hasLim: true},
		{sql: "UPDATE t SET v='x' LIMIT 1", hasLim: true},
		{sql: "UPDATE t SET v='x' WHERE a>0 ORDER BY a DESC LIMIT 2 OFFSET 1", nOB: 1, hasLim: true, hasOff: true},
		// Subquery ORDER BY/LIMIT must not be treated as statement-level.
		{sql: "UPDATE t SET x=(SELECT y FROM u ORDER BY y LIMIT 1) WHERE a=1"},
		// Plain SELECT ORDER BY/LIMIT is unaffected.
		{sql: "SELECT * FROM u ORDER BY id LIMIT 2", wantErr: false},
	}
	for _, c := range cases {
		stmts, err := ParseSQL(c.sql)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got none", c.sql)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: ParseSQL failed: %v", c.sql, err)
			continue
		}
		if len(stmts) == 0 {
			t.Errorf("%q: no statements returned", c.sql)
			continue
		}
		upd, ok := stmts[0].(*sql.UpdateStmt)
		if !ok {
			// Not an UPDATE statement (e.g. SELECT); nothing to check.
			continue
		}
		if len(upd.OrderBy) != c.nOB {
			t.Errorf("%q: expected %d ORDER BY terms, got %d", c.sql, c.nOB, len(upd.OrderBy))
		}
		if (upd.Limit != nil) != c.hasLim {
			t.Errorf("%q: Limit present=%v, want %v", c.sql, upd.Limit != nil, c.hasLim)
		}
		if (upd.Offset != nil) != c.hasOff {
			t.Errorf("%q: Offset present=%v, want %v", c.sql, upd.Offset != nil, c.hasOff)
		}
	}
}

func TestParseUpdateOrderByWithoutLimit(t *testing.T) {
	// SQLite rejects ORDER BY without LIMIT on UPDATE; the parser must not
	// rewrite it into a valid-looking statement.
	_, err := ParseSQL("UPDATE t SET v='x' ORDER BY a")
	if err == nil {
		t.Fatal("expected error for ORDER BY without LIMIT on UPDATE")
	}
}
