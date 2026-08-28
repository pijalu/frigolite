package frigolite

// frigolite_vtabdistinct_test.go is the native Go port of SQLite's
// test/vtabdistinct.test. The TCL package testgen/vtabdistinct is superseded
// by this file (see AGENTS.md "Pure-Go supersession").
//
// The TCL test drives the qpvtab virtual table (ext/misc/qpvtab.c), a
// test/debug module whose xBestIndex echoes the raw C sqlite3_index_info it
// was handed and reports the value of sqlite3_vtab_distinct(). Its schema:
//
//	CREATE TABLE qpvtab(
//	  vn TEXT, ix INTEGER, cn TEXT, op INTEGER, ux BOOLEAN, rhs TEXT,
//	  a, b, c, d, e,
//	  flags INTEGER HIDDEN);
//
// The assertions pin sqlite3_vtab_distinct()'s return-set classification of
// the enclosing statement:
//
//	1.1  SELECT ix ...                    -> 0  (plain result set)
//	1.2  SELECT DISTINCT ix ...           -> 2  (DISTINCT)
//	1.3  qpvtab(3) ... IN (...) ORDER BY  -> 2  (0x002 flag: orderByConsumed)
//	1.4  ... GROUP BY vn HAVING ...       -> 1  (GROUP BY / sorted set)
//
// sqlite3_vtab_distinct() inspects C query-planner state (whether the
// statement's result set is already distinct/ordered) that frigolite's vtab
// contract does not expose: frigolite uses a constraint-sink BestIndex
// (SetHiddenConstraint / PushSpellfixConstraint / PushRTreeConstraint /
// SetMatchConstraint), not the mutable C sqlite3_index_info struct, and has
// no sqlite3_vtab_distinct()/sqlite3_vtab_rhs_value() planner hooks. qpvtab
// is a debugging mirror of those internals with no user-facing feature value.
//
// Per the t14 design decision (plan/goals/P6.VTAB.md Session 12), this port
// documents the superseded C contract and pins the engine-gap boundary:
// frigolite rejects the unregistered qpvtab module with "no such table"
// rather than emulating C query-planner return-set introspection.

import (
	"strings"
	"testing"
)

// TestVtabdistinct_ModuleNotRegistered pins the engine-gap boundary: qpvtab
// is a C query-planner introspection module (sqlite3_vtab_distinct) that
// frigolite does not implement, so referencing it resolves to "no such table"
// like any other unknown relation.
func TestVtabdistinct_ModuleNotRegistered(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	res := db.Query("SELECT ix FROM qpvtab WHERE vn='sqlite3_vtab_distinct'")
	if res.Error == nil {
		t.Fatalf("expected error querying qpvtab, got rows %v", res.Rows)
	}
	if !strings.Contains(res.Error.Error(), "qpvtab") {
		t.Errorf("error %q should name the qpvtab relation", res.Error)
	}

	// The engine's own DISTINCT/GROUP BY semantics (the feature qpvtab only
	// introspects in C) work natively; this is the real user-facing contract.
	if res := db.Exec("CREATE TABLE t(x)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(1),(1),(2)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	got := db.Query("SELECT DISTINCT x FROM t ORDER BY x")
	if got.Error != nil {
		t.Fatalf("distinct: %v", got.Error)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("distinct rows = %v, want 2", got.Rows)
	}
}
