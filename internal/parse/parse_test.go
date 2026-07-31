package parse

import (
	"testing"
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
