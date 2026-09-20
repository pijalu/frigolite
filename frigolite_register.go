// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// Per-connection registration API: virtual-table modules, r-tree geometry
// callbacks, scalar/aggregate SQL functions and collation sequences.
import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/vtab"
)

// RegisterEchoModule registers the echo test module (SQLite src/test8.c) on
// this connection — the register_echo_module TCL harness command /
// sqlite3_create_module(db, "echo", ...) parity. CREATE VIRTUAL TABLE ...
// USING echo fails with "no such module: echo" until this is called, and a
// fresh connection starts unregistered.
func (db *DB) RegisterEchoModule() {
	if db != nil && db.engine != nil {
		db.engine.RegisterEchoModule()
	}
}

// RegisterRtreeGeometry installs a harness-style r-tree geometry callback
// under its SQL function name: "cube" and "circle" from SQLite's
// src/test_rtree.c (the TCL procs register_cube_geom/register_circle_geom).
// The function returns an opaque geometry marker usable only as the right
// operand of `col MATCH name(...)` against rtree-family virtual tables.
func (db *DB) RegisterRtreeGeometry(name string) error {
	if db == nil || db.engine == nil {
		return fmt.Errorf("no database connection")
	}
	switch strings.ToLower(name) {
	case "cube":
		vtab.RegisterRTreeCubeGeometry(db.engine.Database())
	case "circle":
		vtab.RegisterRTreeCircleGeometry(db.engine.Database())
	default:
		return fmt.Errorf("no such geometry: %s", name)
	}
	return nil
}

// RegisterFunction registers a scalar SQL function for this database
// connection. It is used by the test harness to reproduce SQLite's
// TCL-defined test functions (e.g. `db func f f` where f returns a constant).
func (db *DB) RegisterFunction(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	if db != nil && db.engine != nil {
		db.engine.RegisterFunction(name, fn, minArgs, maxArgs)
	}
}

// UnregisterVTabModulesExcept drops every virtual table module except the
// named ones; later CREATE VIRTUAL TABLE statements on a dropped module fail
// with "no such module: <name>" (SQLite's sqlite3_drop_modules test command;
// fts3dropmod.test).
func (db *DB) UnregisterVTabModulesExcept(keep []string) {
	if db != nil && db.engine != nil {
		db.engine.UnregisterVTabModulesExcept(keep)
	}
}

// RegisterFunctionFlags registers a scalar SQL function with SQLite
// function-safety flags (innocuous / directonly) controlling its use in
// schema objects under PRAGMA trusted_schema.
func (db *DB) RegisterFunctionFlags(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int, innocuous, directOnly bool) {
	if db != nil && db.engine != nil {
		db.engine.RegisterFunctionFlags(name, fn, minArgs, maxArgs, innocuous, directOnly)
	}
}

// AggregateFunction is a user-defined SQL aggregate: Step accumulates one
// row's arguments, Final computes the aggregate result from the accumulated
// state (SQLite's sqlite3_create_function with xStep/xFinal).
type AggregateFunction interface {
	Step(args []interface{}) error
	Final() (interface{}, error)
}

// AggregateFuncs adapts plain step/final closures to AggregateFunction (a
// convenience for RegisterAggregate call sites).
type AggregateFuncs struct {
	StepFn  func(args []interface{}) error
	FinalFn func() (interface{}, error)
}

// Step implements AggregateFunction.
func (a *AggregateFuncs) Step(args []interface{}) error { return a.StepFn(args) }

// Final implements AggregateFunction.
func (a *AggregateFuncs) Final() (interface{}, error) { return a.FinalFn() }

// RegisterAggregate registers a user-defined aggregate function. newAgg
// returns a fresh accumulator per query; Step sees each input row's arguments
// and Final produces the result (mirrors RegisterFunction for scalars).
func (db *DB) RegisterAggregate(name string, newAgg func() AggregateFunction, minArgs, maxArgs int) {
	db.engine.Functions().RegisterAggregate(name, minArgs, maxArgs, func() function.Aggregator {
		return newAgg()
	})
}

// RegisterCollation registers a custom collation sequence for this database
// connection (sqlite3_create_collation). The function compares two strings
// and returns -1/0/1. Collation names are case-insensitive; registering a
// name that shadows a built-in (BINARY/NOCASE/RTRIM) replaces the built-in
// for this connection, matching SQLite.
func (db *DB) RegisterCollation(name string, fn func(a, b string) int) {
	if db != nil && db.engine != nil {
		db.engine.RegisterCollation(name, fn)
	}
}

// RegisterCollationNeeded sets the collation-needed callback for this
// database connection (sqlite3_collation_needed). It is invoked whenever a
// statement references a collation sequence that is not registered; the
// callback typically registers the missing collation via RegisterCollation,
// after which the referencing statement succeeds. A nil fn clears the hook.
func (db *DB) RegisterCollationNeeded(fn func(name string)) {
	if db != nil && db.engine != nil {
		db.engine.RegisterCollationNeeded(fn)
	}
}

// UnregisterCollation removes a registered custom collation sequence
// (sqlite_delete_collation). It reports whether a collation was removed.
func (db *DB) UnregisterCollation(name string) bool {
	if db != nil && db.engine != nil {
		return db.engine.UnregisterCollation(name)
	}
	return false
}
