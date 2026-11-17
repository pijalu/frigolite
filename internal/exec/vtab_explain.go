package exec

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// engineExplainSource adapts this connection's EXPLAIN machinery to the vtab
// explain module's row-source contract (ext/misc/explain.c explainFilter's
// prepare("EXPLAIN %s") + step walk). The rows carry the eight EXPLAIN result
// columns; the module appends the raw `sql` column value itself.
type engineExplainSource struct {
	e *Engine
}

// ExplainRows implements vtab.ExplainRowSource: compile "EXPLAIN <sqlText>"
// the way sqlite3_prepare_v2 does (first statement only — explain.c ignores
// the pzTail) and render its opcode row stream through the engine's EXPLAIN
// path.
func (s engineExplainSource) ExplainRows(sqlText string) ([][]interface{}, error) {
	stmts, err := parse.ParseSQL("EXPLAIN " + sqlText)
	if err != nil {
		return nil, err
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("incomplete input")
	}
	ex, ok := stmts[0].(*sql.ExplainStmt)
	if !ok {
		return nil, fmt.Errorf("not an explainable statement")
	}
	res := s.e.execExplain(ex)
	if res.Error != nil {
		return nil, res.Error
	}
	return res.Rows, nil
}
