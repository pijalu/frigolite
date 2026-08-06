package frigolite

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// defaultNullToken is the string used to render SQL NULL in queryRows and
// oracleRows output. It matches the harness tcl_nullvalue default ("{}").
const defaultNullToken = "{}"

// runSQL executes each statement against db, failing the test on the first
// error.
func runSQL(t *testing.T, db *DB, stmts ...string) {
	t.Helper()
	for _, sql := range stmts {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("runSQL: exec %q: %v", sql, res.Error)
		}
	}
}

// queryRows runs sql against db and renders each result row as a []string,
// using nullToken (default "{}") for SQL NULL.
func queryRows(t *testing.T, db *DB, sql string, nullToken ...string) [][]string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("queryRows: query %q: %v", sql, res.Error)
	}
	tok := defaultNullToken
	if len(nullToken) > 0 {
		tok = nullToken[0]
	}
	var rows [][]string
	for _, row := range res.Rows {
		cols := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cols[i] = tok
			} else {
				cols[i] = formatSQLiteValue(v)
			}
		}
		rows = append(rows, cols)
	}
	return rows
}

// oracleRows pipes sql into the system sqlite3 CLI (:memory:) and parses its
// pipe-separated output into rows, using the same NULL-token convention as
// queryRows. It skips the test when no sqlite3 CLI is available, so CI without
// one still passes.
//
// Limitations of the pipe format: a "|" inside a value cannot be represented
// (fall back to a manual oracle comparison in that case), and an empty-string
// cell parses to "" while NULL parses to the NULL token.
func oracleRows(t *testing.T, sql string, nullToken ...string) [][]string {
	t.Helper()
	tok := defaultNullToken
	if len(nullToken) > 0 {
		tok = nullToken[0]
	}
	bin := oracleBin()
	if bin == "" {
		t.Skip("sqlite3 CLI not found; skipping oracle comparison")
	}
	cmd := exec.Command(bin, "-batch", "-noheader", "-separator", "|", "-nullvalue", tok, ":memory:")
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("oracleRows: sqlite3 %q: %v", sql, err)
	}
	lines := strings.Split(string(out), "\n")
	// The final row's trailing newline yields a trailing empty element.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var rows [][]string
	for _, line := range lines {
		rows = append(rows, strings.Split(line, "|"))
	}
	return rows
}

// oracleBin returns the path to a usable sqlite3 CLI, or "" if none is found.
func oracleBin() string {
	if bin, err := exec.LookPath("sqlite3"); err == nil {
		return bin
	}
	if _, err := os.Stat("/usr/bin/sqlite3"); err == nil {
		return "/usr/bin/sqlite3"
	}
	return ""
}

// TestOracleHelperSmoke proves the triage helpers compile and run end-to-end:
// runSQL executes setup, queryRows renders rows with the NULL-token convention,
// and oracleRows derives the same expectation from the system sqlite3 CLI.
func TestOracleHelperSmoke(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	runSQL(t, db,
		"CREATE TABLE t(a INTEGER, b TEXT);",
		"INSERT INTO t VALUES(1, 'one');",
		"INSERT INTO t VALUES(NULL, 'two');",
	)

	// queryRows renders NULL as the default "{}" token.
	got := queryRows(t, db, "SELECT a, b FROM t ORDER BY b")
	want := [][]string{{"1", "one"}, {"{}", "two"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queryRows = %#v, want %#v", got, want)
	}

	// A custom NULL token is honored.
	gotTok := queryRows(t, db, "SELECT a, b FROM t ORDER BY b", "NULL")
	wantTok := [][]string{{"1", "one"}, {"NULL", "two"}}
	if !reflect.DeepEqual(gotTok, wantTok) {
		t.Fatalf("queryRows(NULL token) = %#v, want %#v", gotTok, wantTok)
	}

	// oracleRows derives the same expectation from the system sqlite3 CLI.
	// When sqlite3 is unavailable this skips (t.Skip), which still passes CI.
	oracle := oracleRows(t, "SELECT 1, NULL UNION ALL SELECT 2, 'x'")
	wantOracle := [][]string{{"1", "{}"}, {"2", "x"}}
	if !reflect.DeepEqual(oracle, wantOracle) {
		t.Fatalf("oracleRows = %#v, want %#v", oracle, wantOracle)
	}

	// Empty results parse to no rows.
	if rows := oracleRows(t, "SELECT 1 WHERE 0"); rows != nil {
		t.Fatalf("oracleRows(empty) = %#v, want nil", rows)
	}
}
