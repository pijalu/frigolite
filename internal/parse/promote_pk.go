package parse

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// promoteTableLevelPrimaryKey applies build.c sqlite3AddPrimaryKey's rowid-
// alias rule for the TABLE-level PRIMARY KEY spelling: a PRIMARY KEY over
// exactly one column whose declared type is exactly INTEGER (case-insensitive,
// not DESC) makes that column an INTEGER PRIMARY KEY — the rowid alias. The
// table-level AUTOINCREMENT marker is promoted with it. Without the
// promotion, "CREATE TABLE t(x INTEGER, PRIMARY KEY(x AUTOINCREMENT))"
// (autoinc.test 7.x) parses with x as a plain column and INSERTs leave x NULL
// instead of assigning the rowid.
//
// The pass runs on every parse (fresh CREATE and stored-schema re-parse) so
// the live statement and PRAGMA table_info / column-def caches agree.
func promoteTableLevelPrimaryKey(stmts []sql.Stmt) {
	for _, st := range stmts {
		ct, ok := st.(*sql.CreateTableStmt)
		if !ok || ct == nil || ct.WithoutRowid {
			continue
		}
		promoteConstraints(ct)
	}
}

// promoteConstraints promotes the rowid-alias flags for every single-column
// INTEGER table-level PRIMARY KEY of one CREATE TABLE.
func promoteConstraints(ct *sql.CreateTableStmt) {
	for _, tc := range ct.Constraints {
		if tc.Type != sql.ConstraintPrimaryKey || len(tc.Columns) != 1 {
			continue
		}
		if tc.Columns[0].Name == "" || tc.Columns[0].Desc {
			continue
		}
		promoteColumn(ct, tc, tc.Columns[0].Name)
	}
}

// promoteColumn flags the named column as an INTEGER PRIMARY KEY when its
// declared type is exactly INTEGER (create.c: any other spelling, including
// "INT", is NOT a rowid alias).
func promoteColumn(ct *sql.CreateTableStmt, tc sql.TableConstraint, name string) {
	for i := range ct.Columns {
		cd := &ct.Columns[i]
		if !strings.EqualFold(cd.Name, name) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") {
			cd.PrimaryKey = true
			cd.PKDesc = false
			cd.PKPromoted = true
			cd.AutoInc = cd.AutoInc || tc.AutoInc
		}
		return
	}
}
