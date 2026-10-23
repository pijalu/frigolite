package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
)

func TestSQLiteRenameColumnInternalFunction(t *testing.T) {
	engine := NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := engine.schema.Init(); err != nil {
		t.Fatal(err)
	}
	if result := execTestSQL(engine, t, "CREATE TABLE rename_probe(a, b)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := execTestSQL(engine, t, `SELECT sqlite_rename_column('CREATE TABLE rename_probe(a, b)', 'table', 'rename_probe', 'main', 'rename_probe', 1, 'renamed', 0, 0)`)
	if result.Error == nil {
		t.Fatal("internal rename function visible before test-control toggle")
	}
	engine.ToggleInternalFunctions()
	result = execTestSQL(engine, t, `SELECT sqlite_rename_column('CREATE TABLE rename_probe(a, b)', 'table', 'rename_probe', 'main', 'rename_probe', 1, 'renamed', 0, 0)`)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertRenameColumnOutput(t, result, "CREATE TABLE rename_probe(a, renamed)")
	result = execTestSQL(engine, t, `SELECT sqlite_rename_column('CREATE TABLE rename_probe(a, b DEFAULT ''b'' CHECK(b > 0), c AS (b + 1))', 'table', 'rename_probe', 'main', 'rename_probe', 1, 'renamed', 0, 0)`)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertRenameColumnOutput(t, result, `CREATE TABLE rename_probe(a, renamed DEFAULT 'b' CHECK(renamed > 0), c AS (renamed + 1))`)
	result = execTestSQL(engine, t, `SELECT sqlite_rename_column('CREATE TABLE child(x REFERENCES rename_probe(b), y)', 'table', 'rename_probe', 'main', 'rename_probe', 1, 'renamed', 0, 0)`)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertRenameColumnOutput(t, result, `CREATE TABLE child(x REFERENCES rename_probe(renamed), y)`)
	result = execTestSQL(engine, t, `SELECT sqlite_rename_column('CREATE TABLE rename_probe(a, b REFERENCES rename_probe(b))', 'table', 'rename_probe', 'main', 'rename_probe', 1, 'renamed', 0, 0)`)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertRenameColumnOutput(t, result, `CREATE TABLE rename_probe(a, renamed REFERENCES rename_probe(renamed))`)
	for _, tc := range []struct {
		source string
		want   string
	}{
		{"CREATE INDEX idx ON rename_probe(b, b) WHERE b > 0", "CREATE INDEX idx ON rename_probe(renamed, renamed) WHERE renamed > 0"},
	} {
		query := "SELECT sqlite_rename_column('" + tc.source + "', 'index', 'idx', 'main', 'rename_probe', 1, 'renamed', 0, 0)"
		result = execTestSQL(engine, t, query)
		if result.Error != nil {
			t.Fatalf("rename %q: %v", tc.source, result.Error)
		}
		assertRenameColumnOutput(t, result, tc.want)
	}
	query := `SELECT sqlite_rename_column('CREATE TRIGGER tr AFTER UPDATE OF b ON rename_probe BEGIN SELECT new.b; END', 'trigger', 'tr', 'main', 'rename_probe', 1, 'renamed', 0, 0)`
	result = execTestSQL(engine, t, query)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertRenameColumnOutput(t, result, "CREATE TRIGGER tr AFTER UPDATE OF renamed ON rename_probe BEGIN SELECT new.renamed; END")
	query = `SELECT sqlite_rename_column('CREATE TRIGGER b AFTER UPDATE OF b ON rename_probe BEGIN SELECT new.b, ''b''; END', 'trigger', 'b', 'main', 'rename_probe', 1, 'renamed', 0, 0)`
	result = execTestSQL(engine, t, query)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertRenameColumnOutput(t, result, "CREATE TRIGGER b AFTER UPDATE OF renamed ON rename_probe BEGIN SELECT new.renamed, 'b'; END")
	engine.ToggleInternalFunctions()
	result = execTestSQL(engine, t, `SELECT sqlite_rename_column('CREATE TABLE rename_probe(a, b)', 'table', 'rename_probe', 'main', 'rename_probe', 1, 'renamed', 0, 0)`)
	if result.Error == nil {
		t.Fatal("internal rename function visible after disabling test control")
	}
}

func TestSQLiteRenameTableInternalVisibility(t *testing.T) {
	engine := NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := engine.schema.Init(); err != nil {
		t.Fatal(err)
	}
	query := `SELECT sqlite_rename_table(0,0,0,0,0,0,0)`
	if result := execTestSQL(engine, t, query); result.Error == nil {
		t.Fatal("rename-table internal function visible before test-control toggle")
	}
	engine.ToggleInternalFunctions()
	for _, query := range []string{
		`SELECT sqlite_rename_table(0,0,0,0,0,0,0)`,
		`SELECT sqlite_rename_table(10,20,30,40,50,60,70)`,
		`SELECT sqlite_rename_table('foo','foo','foo','foo','foo','foo','foo')`,
	} {
		if result := execTestSQL(engine, t, query); result.Error == nil {
			t.Errorf("rename-table malformed call succeeded: %s", query)
		}
	}
	engine.ToggleInternalFunctions()
	if result := execTestSQL(engine, t, query); result.Error == nil {
		t.Fatal("rename-table internal function visible after test-control disabled")
	}
}

func assertRenameColumnOutput(t *testing.T, result *Result, want string) {
	t.Helper()
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || result.Rows[0][0] != want {
		t.Fatalf("rename result = %#v, want %q", result.Rows, want)
	}
}

func execTestSQL(engine *Engine, t *testing.T, source string) *Result {
	t.Helper()
	statements, err := parse.ParseSQL(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 {
		t.Fatalf("parsed %d statements, want one", len(statements))
	}
	return engine.Exec(statements[0])
}
