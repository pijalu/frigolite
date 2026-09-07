package execdml

import (
	"reflect"
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

func wrColDefs() []sql.ColumnDef {
	return []sql.ColumnDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
}

// TestWROrder_PKFirst verifies the PK-first iField storage layout: oracle
// bytes for (1,2,3) with PK(b,c) are stored [2 3 1].
func TestWROrder_PKFirst(t *testing.T) {
	createSQL := "CREATE TABLE t1(a, b, c, PRIMARY KEY(b, c)) WITHOUT ROWID"
	order := withoutRowidStorageOrder(createSQL, wrColDefs())
	if !reflect.DeepEqual(order, []int{1, 2, 0}) {
		t.Fatalf("storage order = %v, want [1 2 0]", order)
	}
	declared := []interface{}{int64(1), int64(2), int64(3)}
	stored := reorderToStorage(declared, order)
	if !reflect.DeepEqual(stored, []interface{}{int64(2), int64(3), int64(1)}) {
		t.Fatalf("stored = %v, want [2 3 1]", stored)
	}
	back := reorderToDeclared(stored, order)
	if !reflect.DeepEqual(back, declared) {
		t.Fatalf("round-trip = %v, want %v", back, declared)
	}
}
