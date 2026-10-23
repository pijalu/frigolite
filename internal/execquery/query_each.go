package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// queryEachLiveJoin streams the supported real-table JOIN subset directly from
// the outer table cursor. Other statement shapes remain on the materialized
// SELECT path. The live cursor's existing save/reseek contract permits nested
// callback writes to update the outer table safely.
func (e *SelectEngine) queryEachLiveJoin(s *sql.SelectStmt, outer *schema.Entry, db *DatabaseContext, defs []sql.ColumnDef) (*Result, bool) {
	if !queryEachLiveJoinEligible(s, outer) || e.ctx.TableIsWithoutRowidEntry(outer) || (e.ctx.ReverseUnordered() && len(s.OrderBy) == 0) || e.hasAggregatesCachedStmt(s) || e.selectHasWindowFuncs(s.Columns) || selectHasSubqueryOperand(s) {
		return nil, false
	}
	right, _, rightErr := e.ctx.FindTable(s.Joins[0].Table.Name)
	if rightErr != nil || right == nil || right.RootPage == 0 {
		return nil, false
	}
	if len(s.OrderBy) > 0 && !e.queryEachOrderByOuterOnly(s, outer, db.Pager) {
		return nil, false
	}
	tree := e.ctx.TableBTreePg(db.Pager, outer.Name, outer.RootPage, !e.ctx.TableIsWithoutRowidEntry(outer))
	defer tree.Close()
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}, true
	}
	_, joinedDefs, joinErr := e.execJoins(s, nil, defs)
	if joinErr != nil {
		return &Result{Error: joinErr}, true
	}
	columns := e.buildColumnNames(s.Columns, joinedDefs, s)
	for {
		cursorAdvanced := false
		payload, rowID, readErr := cursor.ReadCellData()
		if readErr != nil {
			if more, nextErr := cursor.Next(); nextErr != nil {
				return &Result{Columns: columns, Error: nextErr}, true
			} else if more {
				return &Result{Columns: columns, Error: readErr}, true
			}
			break
		}
		record, decodeErr := storage.DecodeRecord(payload)
		if decodeErr != nil {
			return &Result{Columns: columns, Error: decodeErr}, true
		}
		outerMap := e.buildRowMap(record, defs, rowID)
		joined, joinedDefs, joinErr := e.execJoins(s, []RowMap{outerMap}, defs)
		if joinErr != nil {
			return &Result{Columns: columns, Error: joinErr}, true
		}
		for _, rowMap := range joined {
			pass, whereErr := e.rowPassesWhere(s.Where, rowMap, nil)
			if whereErr != nil {
				return &Result{Columns: columns, Error: whereErr}, true
			}
			if pass {
				row, outputErr := e.buildOutputRow(s.Columns, joinedDefs, rowMap)
				if outputErr != nil {
					return &Result{Columns: columns, Error: outputErr}, true
				}
				keepGoing, callbackErr := e.rowCallback(row)
				if callbackErr != nil {
					return &Result{Columns: columns, Error: callbackErr}, true
				}
				if !keepGoing {
					return &Result{Columns: columns}, true
				}
				// A nested write invalidates this cursor lazily. Reading again
				// restores its saved key; if deleted, the cursor is parked at the
				// next key and skipNext marks that key as the next scan row.
				_, restoredRowID, restoreErr := cursor.ReadCellData()
				if restoreErr != nil {
					if more, nextErr := cursor.Next(); nextErr != nil {
						return &Result{Columns: columns, Error: nextErr}, true
					} else if !more {
						return &Result{Columns: columns}, true
					}
					return &Result{Columns: columns, Error: restoreErr}, true
				}
				if restoredRowID != rowID {
					more, nextErr := cursor.Next()
					if nextErr != nil {
						return &Result{Columns: columns, Error: nextErr}, true
					}
					if !more {
						return &Result{Columns: columns}, true
					}
					cursorAdvanced = true
					break
				}
			}
		}
		if !cursorAdvanced {
			if more, nextErr := cursor.Next(); nextErr != nil {
				return &Result{Columns: columns, Error: nextErr}, true
			} else if !more {
				break
			}
		}
	}
	return &Result{Columns: columns}, true
}

func (e *SelectEngine) queryEachOrderByOuterOnly(s *sql.SelectStmt, outer *schema.Entry, pg *pager.Pager) bool {
	entry, _, err := e.ctx.FindTable(s.Joins[0].Table.Name)
	if err != nil || entry == nil {
		return false
	}
	outerDefs := e.ctx.ParseColumnDefs(outer.Name, outer.SQL)
	rightDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, term := range s.OrderBy {
		ref, ok := term.Expr.(*sql.ColumnRef)
		if !ok || (ref.Table != "" && !strings.EqualFold(ref.Table, s.From.Name) && !strings.EqualFold(ref.Table, s.From.As)) {
			return false
		}
		found := false
		for _, left := range outerDefs {
			if !strings.EqualFold(left.Name, ref.Name) {
				continue
			}
			for _, right := range rightDefs {
				if strings.EqualFold(right.Name, ref.Name) {
					return false
				}
			}
			found = true
		}
		if !found {
			return false
		}
	}
	// ORDER BY on an outer-only value is stable during inner iteration, but
	// cursor order is not necessarily sorted between multiple outer rows.
	return e.queryEachTableHasAtMostOneRow(outer, pg)
}

func (e *SelectEngine) queryEachTableHasAtMostOneRow(entry *schema.Entry, pg *pager.Pager) bool {
	tree := e.ctx.TableBTreePg(pg, entry.Name, entry.RootPage, !e.ctx.TableIsWithoutRowidEntry(entry))
	defer tree.Close()
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	if _, _, err := cursor.ReadCellData(); err != nil {
		return true
	}
	more, err := cursor.Next()
	return err == nil && !more
}

func queryEachLiveJoinEligible(s *sql.SelectStmt, outer *schema.Entry) bool {
	if outer == nil || outer.RootPage == 0 || s.From.Subquery != nil || len(s.Joins) != 1 ||
		s.Distinct || len(s.GroupBy) != 0 || s.Having != nil || s.Limit != nil || s.Offset != nil ||
		len(s.CTEs) != 0 || len(s.Windows) != 0 || s.Union != nil {
		return false
	}
	join := s.Joins[0]
	return join.Table.Subquery == nil && len(join.Table.Args) == 0 &&
		!joinTypeHas(join.JoinType, "LEFT") && !joinTypeHas(join.JoinType, "RIGHT") && !joinTypeHas(join.JoinType, "FULL")
}
