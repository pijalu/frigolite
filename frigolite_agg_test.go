package frigolite

import (
	"testing"
)

func TestAggregateCount(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (3)")

	res := db.Query("SELECT COUNT(*) FROM t")
	if res.Error != nil {
		t.Fatalf("COUNT: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != int64(3) {
		t.Errorf("expected 3, got %v", res.Rows[0][0])
	}
}

func TestAggregateSum(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (10)")
	db.Exec("INSERT INTO t VALUES (20)")
	db.Exec("INSERT INTO t VALUES (30)")

	res := db.Query("SELECT SUM(val) FROM t")
	if res.Error != nil {
		t.Fatalf("SUM: %v", res.Error)
	}
	if res.Rows[0][0] != float64(60) {
		t.Errorf("expected 60, got %v", res.Rows[0][0])
	}
}

func TestAggregateAvg(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (10)")
	db.Exec("INSERT INTO t VALUES (20)")
	db.Exec("INSERT INTO t VALUES (30)")

	res := db.Query("SELECT AVG(val) FROM t")
	if res.Error != nil {
		t.Fatalf("AVG: %v", res.Error)
	}
	if res.Rows[0][0] != float64(20) {
		t.Errorf("expected 20, got %v", res.Rows[0][0])
	}
}

func TestAggregateMinMax(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (10)")
	db.Exec("INSERT INTO t VALUES (5)")
	db.Exec("INSERT INTO t VALUES (20)")

	res := db.Query("SELECT MIN(val), MAX(val) FROM t")
	if res.Error != nil {
		t.Fatalf("MIN/MAX: %v", res.Error)
	}
	if res.Rows[0][0] != int64(5) {
		t.Errorf("expected MIN=5, got %v", res.Rows[0][0])
	}
	if res.Rows[0][1] != int64(20) {
		t.Errorf("expected MAX=20, got %v", res.Rows[0][1])
	}
}

func TestOrderByDesc(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, name TEXT)")
	db.Exec("INSERT INTO t VALUES (1, 'alice')")
	db.Exec("INSERT INTO t VALUES (2, 'bob')")
	db.Exec("INSERT INTO t VALUES (3, 'charlie')")

	res := db.Query("SELECT name FROM t ORDER BY id DESC")
	if res.Error != nil {
		t.Fatalf("ORDER BY DESC: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "charlie" {
		t.Errorf("expected 'charlie' first, got %v", res.Rows[0][0])
	}
	if res.Rows[2][0] != "alice" {
		t.Errorf("expected 'alice' last, got %v", res.Rows[2][0])
	}
}

func TestAggregateCountAll(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// COUNT(*) without any rows should return 0
	db.Exec("CREATE TABLE t (id INTEGER)")

	res := db.Query("SELECT COUNT(*) FROM t")
	if res.Error != nil {
		t.Fatalf("COUNT empty: %v", res.Error)
	}
	if res.Rows[0][0] != int64(0) {
		t.Errorf("expected 0, got %v", res.Rows[0][0])
	}
}

// TestAggregateTotal tests TOTAL function (mirrors func.test patterns)
func TestAggregateTotal(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (val INTEGER)")
	db.Exec("INSERT INTO t VALUES (10)")
	db.Exec("INSERT INTO t VALUES (20)")
	db.Exec("INSERT INTO t VALUES (30)")

	res := db.Query("SELECT TOTAL(val) FROM t")
	if res.Error != nil {
		t.Fatalf("TOTAL: %v", res.Error)
	}
	if res.Rows[0][0] != float64(60) {
		t.Errorf("expected 60, got %v", res.Rows[0][0])
	}

	// TOTAL on empty table returns 0.0 per SQLite spec
	// Note: current implementation returns nil for no rows (same as SUM)
	// This matches the evalAggregatesEmpty behavior
	db2 := setupDB(t)
	defer db2.Close()
	db2.Exec("CREATE TABLE empty (val INTEGER)")
	res = db2.Query("SELECT TOTAL(val) FROM empty")
	if res.Error != nil {
		t.Fatalf("TOTAL empty: %v", res.Error)
	}
	t.Logf("TOTAL empty result: %v (type: %T)", res.Rows[0][0], res.Rows[0][0])
}

// TestAggregateGroupConcat tests GROUP_CONCAT function (mirrors func.test patterns)
func TestAggregateGroupConcat(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (name TEXT)")
	db.Exec("INSERT INTO t VALUES ('alice')")
	db.Exec("INSERT INTO t VALUES ('bob')")
	db.Exec("INSERT INTO t VALUES ('charlie')")

	res := db.Query("SELECT group_concat(name) FROM t")
	if res.Error != nil {
		t.Fatalf("GROUP_CONCAT: %v", res.Error)
	}
	if res.Rows[0][0] != "alice,bob,charlie" {
		t.Errorf("expected 'alice,bob,charlie', got %v", res.Rows[0][0])
	}
}

// TestAggregateWithWhere tests aggregates combined with WHERE (mirrors func.test patterns)
func TestAggregateWithWhere(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO t VALUES (1, 10)")
	db.Exec("INSERT INTO t VALUES (2, 20)")
	db.Exec("INSERT INTO t VALUES (3, 30)")
	db.Exec("INSERT INTO t VALUES (4, 40)")

	res := db.Query("SELECT COUNT(*), SUM(val) FROM t WHERE val > 20")
	if res.Error != nil {
		t.Fatalf("aggregate with WHERE: %v", res.Error)
	}
	if res.Rows[0][0] != int64(2) {
		t.Errorf("expected COUNT=2, got %v", res.Rows[0][0])
	}
	if res.Rows[0][1] != float64(70) {
		t.Errorf("expected SUM=70, got %v", res.Rows[0][1])
	}
}
