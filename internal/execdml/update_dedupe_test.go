package execdml

import (
	"reflect"
	"testing"
)

// TestDedupeUpdateChanges covers applyUpdateChanges' defense against a
// pre-existing physical duplicate rowid (a corrupted or legacy database
// file may hold two cells with one rowid; the change scan then collects
// the row twice). SQLite overwrites per row in place, so the apply pass
// must write each distinct rowid exactly once — never re-create the
// duplicate (fts4merge4 2.2.x amplification: rowid 74 grew 3 -> 5 -> 9).
func TestDedupeUpdateChanges(t *testing.T) {
	changes := []updateChange{
		{rowID: 22, values: []interface{}{int64(1), "a"}},
		{rowID: 22, values: []interface{}{int64(1), "a2"}}, // physical duplicate
		{rowID: 23, values: []interface{}{int64(2), "b"}},
		{rowID: 23, values: []interface{}{int64(2), "b2"}}, // physical duplicate
		{rowID: 24, values: []interface{}{int64(3), "c"}},
	}
	got := dedupeUpdateChanges(changes)
	if len(got) != 3 {
		t.Fatalf("dedupeUpdateChanges returned %d changes, want 3", len(got))
	}
	wantIDs := []int64{22, 23, 24}
	gotIDs := []int64{got[0].rowID, got[1].rowID, got[2].rowID}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("rowIDs = %v, want %v", gotIDs, wantIDs)
	}
	// Last write wins (sequential overwrite semantics).
	if got[0].values[1] != "a2" || got[1].values[1] != "b2" {
		t.Errorf("dedupe kept stale values: %+v", got)
	}
}

func TestDedupeUpdateChangesNoDuplicates(t *testing.T) {
	changes := []updateChange{
		{rowID: 1, values: []interface{}{"x"}},
		{rowID: 2, values: []interface{}{"y"}},
	}
	got := dedupeUpdateChanges(changes)
	if len(got) != 2 || got[0].rowID != 1 || got[1].rowID != 2 {
		t.Errorf("clean input changed: %+v", got)
	}
}
