package parse

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

func TestCreateTableNameTokenOffsets(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{`CREATE TABLE ddd(a)`, `ddd`},
		{`CREATE TABLE "a b"(a)`, `"a b"`},
	} {
		stmts, err := ParseSQL(tc.input)
		if err != nil {
			t.Fatal(err)
		}
		got := stmts[0].(*sql.CreateTableStmt).NameTok
		start := strings.Index(tc.input, tc.want)
		if got.Start != start || got.End != start+len(tc.want) {
			t.Errorf("%q: NameTok=%+v, want [%d,%d)", tc.input, got, start, start+len(tc.want))
		}
	}
}
