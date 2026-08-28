package exec

import (
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/vtab"
)

// engineVtabDB adapts *Engine to the vtab.Database interface, granting virtual
// table modules the ability to create/read shadow tables and register SQL
// functions. It lives in the exec package (which already imports vtab) so that
// vtab itself never imports exec — preserving the import boundary (SOLID / DIP).
type engineVtabDB struct{ e *Engine }

// ExecSQL parses and runs one or more SQL statements. It returns the result rows
// of the final statement (nil for non-SELECT) or the first error.
func (d engineVtabDB) ExecSQL(sql string, args ...interface{}) ([][]interface{}, error) {
	stmts, err := parse.ParseSQL(sql)
	if err != nil {
		return nil, err
	}
	var rows [][]interface{}
	for _, s := range stmts {
		res := d.e.Exec(s)
		if res.Error != nil {
			return nil, res.Error
		}
		rows = res.Rows
	}
	return rows, nil
}

// RegisterScalar delegates to the engine's scalar function registry.
func (d engineVtabDB) RegisterScalar(name string, minArgs, maxArgs int, fn func(args []interface{}) (interface{}, error)) {
	d.e.RegisterFunction(name, fn, minArgs, maxArgs)
}

// vtabAggAdapter bridges a vtab.Aggregator to function.Aggregator.
type vtabAggAdapter struct{ a vtab.Aggregator }

func (x vtabAggAdapter) Step(args []interface{}) error { return x.a.Step(args) }
func (x vtabAggAdapter) Final() (interface{}, error)   { return x.a.Final() }

// RegisterAggregate wraps a vtab.Aggregator in a function.Aggregator and
// registers it with the engine's aggregate registry.
func (d engineVtabDB) RegisterAggregate(name string, minArgs, maxArgs int, newAgg func() vtab.Aggregator) {
	d.e.Functions().RegisterAggregate(name, minArgs, maxArgs, func() function.Aggregator {
		return vtabAggAdapter{newAgg()}
	})
}

// Database returns the engine as a vtab.Database for module constructor DI.
func (e *Engine) Database() vtab.Database { return engineVtabDB{e: e} }
