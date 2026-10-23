package parse

import (
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

func TestAlterRenameColumnPreservesQuotedTarget(t *testing.T) {
	for _, test := range []struct {
		sql    string
		quoted bool
	}{
		{sql: `ALTER TABLE x RENAME COLUMN a TO 'four'`, quoted: true},
		{sql: `ALTER TABLE x RENAME COLUMN a TO four`, quoted: false},
		{sql: `ALTER TABLE x RENAME COLUMN a TO "four"`, quoted: true},
	} {
		statements, err := ParseSQL(test.sql)
		if err != nil {
			t.Fatalf("ParseSQL(%q): %v", test.sql, err)
		}
		alter, ok := statements[0].(*sql.AlterTableStmt)
		if !ok || alter.NewName != "four" || alter.NewNameQuoted != test.quoted {
			t.Errorf("ParseSQL(%q) alter = %#v, want name four quoted=%t", test.sql, alter, test.quoted)
		}
	}
}
