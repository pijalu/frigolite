package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

func TestDropTablesFromSplitSchemaBTree(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 12; i++ {
		cols := make([]string, 500)
		for j := range cols {
			cols[j] = fmt.Sprintf("c%d", j)
		}
		query := fmt.Sprintf("CREATE TABLE t%d(%s)", i, strings.Join(cols, ","))
		if result := db.Exec(query); result.Error != nil {
			t.Fatalf("create t%d: %v", i, result.Error)
		}
	}
	for i := 11; i >= 0; i-- {
		if result := db.Exec(fmt.Sprintf("DROP TABLE t%d", i)); result.Error != nil {
			t.Fatalf("drop t%d: %v", i, result.Error)
		}
	}
	remaining := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if remaining.Error != nil {
		t.Fatalf("query remaining schema entries: %v", remaining.Error)
	}
	if len(remaining.Rows) != 0 {
		t.Fatalf("schema entries remain after DROP: %v", remaining.Rows)
	}
	if integrity := db.Query("PRAGMA integrity_check"); integrity.Error != nil || len(integrity.Rows) != 1 || integrity.Rows[0][0] != "ok" {
		t.Fatalf("integrity after DROP: rows=%v error=%v", integrity.Rows, integrity.Error)
	}
}
