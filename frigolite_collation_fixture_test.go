package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

func runCollationDestructors(db *DB, destructors map[*DB]map[string]string, vars map[string]string) {
	for name := range destructors[db] {
		runOneCollationDestructor(db, name, destructors, vars)
	}
}

func runOneCollationDestructor(db *DB, name string, destructors map[*DB]map[string]string, vars map[string]string) {
	byName := destructors[db]
	key := strings.ToUpper(name)
	body, ok := byName[key]
	if !ok {
		return
	}
	delete(byName, key)
	fields := strings.Fields(strings.TrimSpace(body))
	if len(fields) != 2 || fields[0] != "incr" {
		return
	}
	variable := strings.TrimPrefix(fields[1], "::")
	value, _ := strconv.Atoi(vars[variable])
	vars[variable] = strconv.Itoa(value + 1)
}

func TestHarnessTclCollationComparatorAndDestructorLifecycle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vars := map[string]string{"caseless_del": "0"}
	destructors := map[*DB]map[string]string{db: {"CASELESS": "incr ::caseless_del"}}
	db.RegisterCollation("CASELESS", func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	if got := db.Query("SELECT 'AbC' = 'abc' COLLATE CASELESS"); got.Error != nil || len(got.Rows) != 1 || got.Rows[0][0] != int64(1) {
		t.Fatalf("case-insensitive collation query = %#v", got)
	}
	db.UnregisterCollation("CASELESS")
	runOneCollationDestructor(db, "CASELESS", destructors, vars)
	if got := vars["caseless_del"]; got != "1" {
		t.Fatalf("destructor count after deletion = %s, want 1", got)
	}
	if got := db.Query("SELECT 'AbC' = 'abc' COLLATE CASELESS"); got.Error == nil || !strings.Contains(got.Error.Error(), "no such collation sequence") {
		t.Fatalf("query with deleted collation error = %v", got.Error)
	}

	closingDB, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	closingDB.RegisterCollation("CASELESS", func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	closeDestructors := map[*DB]map[string]string{closingDB: {"CASELESS": "incr ::caseless_del"}}
	runCollationDestructors(closingDB, closeDestructors, vars)
	if err := closingDB.Close(); err != nil {
		t.Fatal(err)
	}
	if got := vars["caseless_del"]; got != "2" {
		t.Fatalf("destructor count after close = %s, want 2", got)
	}
}
