package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// TestParseTableLevelConstraints verifies that the lemon parser (parse.ParseSQL)
// emits table-level constraints (PRIMARY KEY / UNIQUE / CHECK / FOREIGN KEY,
// with optional names, multi-constraint lists, and WITHOUT ROWID PK columns)
// onto sql.CreateTableStmt. That was the gap that forced engine call sites
// (parseColumnDefs, tableConstraints, etc.) to fall back to the hand-written
// parser via sql.NewParser.
func TestParseTableLevelConstraints(t *testing.T) {
	type want struct {
		cols     int
		cons     int
		rowid    bool
		strict   bool
		firstTyp string
		firstName string
	}
	cases := []struct {
		sql  string
		want want
	}{
		{"CREATE TABLE t(a INT, b INT, PRIMARY KEY(a))", want{cols: 2, cons: 1, firstTyp: "PRIMARY KEY"}},
		{"CREATE TABLE t(a INT, UNIQUE(a))", want{cols: 1, cons: 1, firstTyp: "UNIQUE"}},
		{"CREATE TABLE t(a INT, CHECK(a>0))", want{cols: 1, cons: 1, firstTyp: "CHECK"}},
		{"CREATE TABLE t(a INT, FOREIGN KEY(a) REFERENCES x(b))", want{cols: 1, cons: 1, firstTyp: "FOREIGN KEY"}},
		{"CREATE TABLE t(a INT, b INT, CONSTRAINT c UNIQUE(a,b))", want{cols: 2, cons: 1, firstTyp: "UNIQUE", firstName: "c"}},
		{"CREATE TABLE t(a INT, UNIQUE(a), CHECK(a>0))", want{cols: 1, cons: 2, firstTyp: "UNIQUE"}},
		{"CREATE TABLE t(a INT, b INT, PRIMARY KEY(a,b)) WITHOUT ROWID", want{cols: 2, cons: 1, rowid: true, firstTyp: "PRIMARY KEY"}},
		{"CREATE TABLE t(a INT, PRIMARY KEY(a,rowid,b)) WITHOUT ROWID", want{cols: 1, cons: 1, rowid: true, firstTyp: "PRIMARY KEY"}},
		{"CREATE TABLE t(a INT, b TEXT) STRICT", want{cols: 2, cons: 0, strict: true}},
	}
	for _, tc := range cases {
		stmts, err := parse.ParseSQL(tc.sql)
		if err != nil {
			t.Errorf("%s: parse error: %v", tc.sql, err)
			continue
		}
		if len(stmts) == 0 {
			t.Errorf("%s: no statements", tc.sql)
			continue
		}
		ct, ok := stmts[0].(*sql.CreateTableStmt)
		if !ok {
			t.Errorf("%s: got %T, want *sql.CreateTableStmt", tc.sql, stmts[0])
			continue
		}
		if len(ct.Columns) != tc.want.cols {
			t.Errorf("%s: cols=%d want %d", tc.sql, len(ct.Columns), tc.want.cols)
		}
		if len(ct.Constraints) != tc.want.cons {
			t.Errorf("%s: constraints=%d want %d", tc.sql, len(ct.Constraints), tc.want.cons)
		}
		if ct.WithoutRowid != tc.want.rowid {
			t.Errorf("%s: withoutRowid=%v want %v", tc.sql, ct.WithoutRowid, tc.want.rowid)
		}
		if ct.Strict != tc.want.strict {
			t.Errorf("%s: strict=%v want %v", tc.sql, ct.Strict, tc.want.strict)
		}
		if tc.want.cons > 0 && len(ct.Constraints) > 0 {
			first := ct.Constraints[0]
			if string(first.Type) != tc.want.firstTyp {
				t.Errorf("%s: first con type=%q want %q", tc.sql, first.Type, tc.want.firstTyp)
			}
			if first.Name != tc.want.firstName {
				t.Errorf("%s: first con name=%q want %q", tc.sql, first.Name, tc.want.firstName)
			}
		}
	}
}