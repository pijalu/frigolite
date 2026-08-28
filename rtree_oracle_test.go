package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRtreeOracleReplay replays every recorded oracle transcript
// (.agents/rtree_oracle/*.sql) through frigolite and compares its output with
// the sqlite3 CLI reference output kept alongside as <name>.expected.
// Each statement runs standalone against a fresh in-memory database so DDL
// and query transcripts coexist. The test skips silently when the directory
// is absent (testgen-only environments).
func TestRtreeOracleReplay(t *testing.T) {
	const dir = ".agents/rtree_oracle"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no oracle transcripts")
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".sql" {
			continue
		}
		base := strings.TrimSuffix(name, ".sql")
		script, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		expected, err := os.ReadFile(filepath.Join(dir, base+".expected"))
		if err != nil {
			t.Fatalf("%s: missing .expected: %v", name, err)
		}
		t.Run(base, func(t *testing.T) {
			got, err := rtreeRunScript(string(script))
			if err != nil {
				t.Fatal(err)
			}
			want := string(expected)
			if got != want {
				t.Fatalf("output mismatch:\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

// rtreeRunScript executes each ;-terminated statement of script on its own
// fresh in-memory database (transcripts are written so every non-query
// statement starts a new section beginning at CREATE) and renders result rows
// in sqlite3 list mode ("v1|v2", floats formatted %.1f).
func rtreeRunScript(script string) (string, error) {
	var out strings.Builder
	stmts := strings.Split(script, ";\n")
	db, err := Open(":memory:")
	if err != nil {
		return "", err
	}
	defer db.Close()
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		res := db.Query(stmt + ";")
		if res.Error != nil {
			return "", fmt.Errorf("%q: %w", stmt, res.Error)
		}
		for _, row := range res.Rows {
			cells := make([]string, len(row))
			for i, v := range row {
				switch x := v.(type) {
				case float64:
					cells[i] = fmt.Sprintf("%.1f", x)
				case int64:
					cells[i] = fmt.Sprintf("%d", x)
				default:
					cells[i] = fmt.Sprintf("%v", v)
				}
			}
			out.WriteString(strings.Join(cells, "|") + "\n")
		}
	}
	return out.String(), nil
}
