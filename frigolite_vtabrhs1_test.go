package frigolite

// frigolite_vtabrhs1_test.go is the native Go port of SQLite's
// test/vtabrhs1.test. The TCL package testgen/vtabrhs1 is superseded by this
// file (see AGENTS.md "Pure-Go supersession").
//
// The TCL test drives the qpvtab virtual table (ext/misc/qpvtab.c) and pins
// sqlite3_vtab_rhs_value(): the C xBestIndex-time hook that extracts the
// right-hand-side value of a usable constraint when — and only when — that
// RHS is a constant usable at plan time. The assertions pin:
//
//	1.1  a=12345           -> rhs 12345      (integer literal)
//	1.2  a<>4.5            -> rhs 4.5        (real literal)
//	1.3  'quokka' < a      -> rhs 'quokka'   (text literal, commuted)
//	1.4  a IS NULL         -> rhs NULL       (no usable RHS value)
//	1.5  a GLOB x'0123'    -> rhs x'0123'    (blob literal)
//	2.1  a=format('abc')   -> typeof NULL    (function RHS: SQLITE_NOTFOUND)
//	2.2  a=?2              -> typeof NULL    (?N parameter RHS: SQLITE_NOTFOUND)
//
// sqlite3_vtab_rhs_value() reads C query-planner constraint state (the
// pre-computed constant RHS attached to each usable sqlite3_index_info
// constraint) that frigolite's vtab contract does not expose: frigolite
// passes constraints to modules via a constraint-sink BestIndex
// (SetHiddenConstraint / PushSpellfixConstraint / PushRTreeConstraint /
// SetMatchConstraint) after the plan is fixed, not via the mutable C
// sqlite3_index_info struct, and has no sqlite3_vtab_rhs_value() hook. qpvtab
// is a debugging mirror of that planner surface with no user-facing value.
//
// Per the t14 design decision (plan/goals/P6.VTAB.md Session 12), this port
// documents the superseded C contract and pins the engine-gap boundary:
// frigolite rejects the unregistered qpvtab module with "no such table"
// rather than emulating C query-planner RHS extraction.

import (
	"strings"
	"testing"
)

// TestVtabrhs1_ModuleNotRegistered pins the engine-gap boundary: qpvtab is a
// C query-planner introspection module (sqlite3_vtab_rhs_value) that
// frigolite does not implement, so referencing it resolves to "no such table"
// like any other unknown relation.
func TestVtabrhs1_ModuleNotRegistered(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	res := db.Query("SELECT rhs FROM qpvtab WHERE cn='a' AND a=12345")
	if res.Error == nil {
		t.Fatalf("expected error querying qpvtab, got rows %v", res.Rows)
	}
	if !strings.Contains(res.Error.Error(), "qpvtab") {
		t.Errorf("error %q should name the qpvtab relation", res.Error)
	}

	// The engine's own constant-RHS comparison semantics (the feature qpvtab
	// only introspects in C) work natively; this is the user-facing contract.
	if res := db.Exec("CREATE TABLE t(a)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t VALUES(12345),(4.5),('quokka')"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	got := db.Query("SELECT a FROM t WHERE a=12345")
	if got.Error != nil {
		t.Fatalf("select: %v", got.Error)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %v, want 1", got.Rows)
	}
}
