package frigolite

import (
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

func TestZZParseProbe(t *testing.T) {
	sel, _ := parse.ParseSQL("VALUES (1,1,1), (2,2,2,2), (3,3,3)")
	if s, ok := sel[0].(*sql.SelectStmt); ok {
		for cur, i := s, 0; cur != nil; cur, i = cur.Union, i+1 {
			t.Logf("member %d: ValuesChain=%v SetOp=%v UnionAll=%v cols=%d", i, cur.ValuesChain, cur.SetOp, cur.UnionAll, len(cur.Columns))
		}
	} else {
		t.Logf("stmt type %T", sel[0])
	}
	sel2, _ := parse.ParseSQL("VALUES (2) UNION SELECT 3,4")
	if s, ok := sel2[0].(*sql.SelectStmt); ok {
		for cur, i := s, 0; cur != nil; cur, i = cur.Union, i+1 {
			t.Logf("mixed member %d: ValuesChain=%v SetOp=%v UnionAll=%v cols=%d", i, cur.ValuesChain, cur.SetOp, cur.UnionAll, len(cur.Columns))
		}
	} else {
		t.Logf("mixed stmt type %T", sel2[0])
	}
}
