package frigolite

import (
	"errors"
	"testing"
)

func TestAnalyzeFStat4PlannerPropagatesDeterministicFunctionError(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE t(x INTEGER, y INTEGER); CREATE INDEX tx ON t(x); CREATE INDEX ty ON t(y); INSERT INTO t VALUES(1, 1),(2, 2),(3, 3); ANALYZE; DELETE FROM t;"); result.Error != nil {
		t.Fatal(result.Error)
	}
	want := errors.New("planner callback error")
	db.RegisterFunctionDeterministic("planner_error", func([]interface{}) (interface{}, error) { return nil, want }, 0, 0, true, false, false)
	result := db.Query("SELECT * FROM t WHERE x=planner_error() AND y=4")
	if result.Error == nil || result.Error.Error() != want.Error() {
		t.Fatalf("query error = %v, want %q", result.Error, want)
	}
}

func TestAnalyzeFStat4PlannerSkipsNondeterministicFunction(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE t(x INTEGER, y INTEGER); CREATE INDEX tx ON t(x); CREATE INDEX ty ON t(y); INSERT INTO t VALUES(1, 1),(2, 2),(3, 3); ANALYZE; DELETE FROM t;"); result.Error != nil {
		t.Fatal(result.Error)
	}
	called := false
	db.RegisterFunction("planner_nondeterministic", func([]interface{}) (interface{}, error) {
		called = true
		return nil, errors.New("must not run in planner")
	}, 0, 0)
	result := db.Query("SELECT * FROM t WHERE x=planner_nondeterministic() AND y=4")
	if result.Error != nil {
		t.Fatalf("query error = %v", result.Error)
	}
	if called {
		t.Fatal("non-deterministic function evaluated during STAT4 planning")
	}
}
