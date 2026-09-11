package execdml

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// TestZZT10Pipeline probes the T10 write pipeline for the repro schema
// CREATE TABLE t1(b UNIQUE, a INT PRIMARY KEY) WITHOUT ROWID:
// named-column insert (a='1', b=nil) vs full-tuple insert (b='x', a=3).
// TEMPORARY debug probe — deleted before commit.
func TestZZT10Pipeline(t *testing.T) {
	createSQL := "CREATE TABLE t1(b UNIQUE, a INT PRIMARY KEY) WITHOUT ROWID"
	up := strings.ToUpper(createSQL)
	colDefs := []sql.ColumnDef{
		{Name: "b", Unique: true},
		{Name: "a", Type: "INT", PrimaryKey: true},
	}
	withoutRowid := hasWithoutRowidKeyword(up)
	t.Logf("hasWithoutRowidKeyword=%v", withoutRowid)

	order := WithoutRowidStorageOrder(createSQL, colDefs)
	t.Logf("order=%v", order)
	npk := WRPKSlotCount(createSQL, colDefs)
	t.Logf("npk=%d", npk)

	cases := []struct {
		name   string
		decl   []interface{}
	}{
		{"named(a='1')", []interface{}{nil, "1"}},
		{"full('x',3)", []interface{}{"x", int64(3)}},
	}
	for _, tc := range cases {
		stored := ReorderToStorage(tc.decl, order)
		nulled := NullIPKAliasForWrite(colDefs, stored, withoutRowid)
		rec, err := storage.EncodeRecord(nulled)
		if err != nil {
			t.Fatalf("%s: EncodeRecord: %v", tc.name, err)
		}
		t.Logf("%s: declared=%v stored=%v nulled=%v record=%s",
			tc.name, tc.decl, stored, nulled, hex.EncodeToString(rec))
	}
}
