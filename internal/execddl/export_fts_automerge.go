// Package execddl: FTS auto-incr-merge %_stat persistence (SQLite's
// FTS_STAT_AUTOINCRMERGE row). Split from export_fts_flush.go for the
// 1000-line gate; behavior unchanged.
package execddl

import (
	"fmt"

	"strconv"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// WriteFTSAutomergeStat persists the auto-incr-merge setting as the %_stat
// id=2 row (exported for the DML layer's automerge= special insert).
func (e *DDLExecutor) WriteFTSAutomergeStat(tableName string, v int) {
	e.writeFTSAutomergeStat(tableName, v)
}

// writeFTSAutomergeStat persists the auto-incr-merge setting as the %_stat
// id=2 row with an INTEGER value (fts3_write.c fts3DoAutoincrmerge:
// SQL_REPLACE_STAT binds FTS_STAT_AUTOINCRMERGE and the value as ints). The
// %_stat table is created on demand (fts3.c sqlite3Fts3CreateStatTable), so
// an fts3 table's automerge= persists too.
func (e *DDLExecutor) writeFTSAutomergeStat(tableName string, v int) {
	stat := tableName + "_stat"
	if _, _, err := e.ctx.FindTable(stat); err != nil {
		if res := e.createShadowTableSQL(stat, []sql.ColumnDef{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "value", Type: "BLOB"},
		}); res.Error != nil {
			return
		}
	}
	_ = e.ctx.Exec(&sql.InsertStmt{
		Table:     stat,
		Columns:   []string{"id", "value"},
		IsReplace: true,
		Values: [][]sql.Expr{
			{
				&sql.NumericLit{Value: fmt.Sprintf("%d", 2)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", v)},
			},
		},
	})
}

// readFTSAutomergeStat reads the %_stat id=2 auto-incr-merge value
// (fts3_write.c sqlite3Fts3PendingTermsFlush's SQL_SELECT_STAT restore).
// Returns (v, true) when the row exists and holds an integer (the record
// slot decodes as int64, or as text/bytes a numeric string written by a
// foreign tool); (0, false) when the row or table is absent.
func (e *DDLExecutor) readFTSAutomergeStat(tableName string) (int, bool) {
	stat := tableName + "_stat"
	segEntry, _, err := e.ctx.FindTable(stat)
	if err != nil || segEntry == nil {
		return 0, false
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return 0, false
	}
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) < 2 {
			break
		}
		if cell.RowID == int64(2) {
			switch v := rec.Values[1].(type) {
			case int64:
				return int(v), true
			case string:
				if n, perr := strconv.Atoi(v); perr == nil {
					return n, true
				}
				return 0, false
			case []byte:
				if n, perr := strconv.Atoi(string(v)); perr == nil {
					return n, true
				}
				return 0, false
			default:
				return 0, false
			}
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return 0, false
}
