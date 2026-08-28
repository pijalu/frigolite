package vtab

// Database is the subset of engine capability a virtual table module needs in
// order to manage its shadow/backing tables and register SQL functions. It is
// satisfied by *exec.Engine.
//
// Dependency Inversion: vtab defines this interface so concrete modules depend
// on the abstraction, never on the concrete engine. This avoids an import cycle
// (the exec package already imports vtab); the engine adapts itself to this
// interface (see internal/exec/vtab_db.go). rtree, fts5, dbdata and dbstat all
// obtain a Database handle via their module constructor (DI), exactly like
// dbpage obtains a PageSourceProvider.
type Database interface {
	// ExecSQL runs one or more SQL statements (DDL/DML/SELECT). It returns the
	// result rows of the final statement (nil for non-SELECT) or the first error
	// encountered. Argument binding is performed by the caller (values are
	// inlined into the SQL text), matching SQLite's sqlite3_exec/prepare style.
	ExecSQL(sql string, args ...interface{}) ([][]interface{}, error)
	// RegisterScalar registers a scalar SQL function.
	RegisterScalar(name string, minArgs, maxArgs int, fn func(args []interface{}) (interface{}, error))
	// RegisterAggregate registers an aggregate SQL function.
	RegisterAggregate(name string, minArgs, maxArgs int, newAgg func() Aggregator)
}

// Aggregator is the vtab-local aggregate contract. It mirrors function.Aggregator
// but is declared here so vtab does not import the function package; the engine
// adapts a vtab.Aggregator to a function.Aggregator.
type Aggregator interface {
	Step(args []interface{}) error
	Final() (interface{}, error)
}
