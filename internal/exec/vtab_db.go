package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execddl"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
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

// ExecSQLNoVtab runs one or more SQL statements under SQLite's
// SQLITE_PREPARE_NO_VTAB mode (rtree.c rtreeSqlInit prepares its shadow
// statements with PERSISTENT|NO_VTAB): while a statement runs in this mode,
// virtual tables resolve as absent — "no such table: <schema>.<name>" —
// including from trigger bodies fired by the statement (trigger.c:1286
// inherits prepFlags; build.c:454 hides virtual tables). The mode is scoped
// to these statements and never leaks into user statements.
func (d engineVtabDB) ExecSQLNoVtab(sql string, args ...interface{}) ([][]interface{}, error) {
	d.e.noVtabDepth++
	defer func() { d.e.noVtabDepth-- }()
	return d.ExecSQL(sql, args...)
}

// PrepareShadowStatements reproduces the connect-time effect of rtreeSqlInit's
// NO_VTAB shadow-statement preparation (rtree.c:3424): compiling a statement
// targeting one of tables also compiles the subprograms of every trigger
// defined on it (src/trigger.c:1286), and a trigger body referencing a
// virtual table fails that preparation with the schema-qualified
// "no such table" error (build.c:454). Nil when no trigger body references a
// virtual table.
func (d engineVtabDB) PrepareShadowStatements(schemaName string, tables []string) error {
	ctx := d.e.getDB(schemaName)
	if ctx == nil {
		ctx = d.e.mainDB
	}
	for _, table := range tables {
		triggers, err := ctx.Schema.FindTriggersForTable(table)
		if err != nil {
			continue
		}
		for _, trig := range triggers {
			if name, ref, hit := shadowTriggerRefsVtab(ctx, d.e.ddl, trig); hit {
				// sqlite3FixSrcList qualifies unqualified names with the
				// trigger's own database in the error text.
				if strings.Contains(ref, ".") {
					return fmt.Errorf("no such table: %s", strings.ToLower(ref))
				}
				return fmt.Errorf("no such table: %s.%s", strings.ToLower(ctx.Name), name)
			}
		}
	}
	return nil
}

// shadowTriggerRefsVtab scans one trigger body's table references for a
// virtual-table entry in ctx (the trigger's schema). Returns the referenced
// name (unqualified) and the raw reference text when found.
func shadowTriggerRefsVtab(ctx *DatabaseContext, ddl *execddl.DDLExecutor, trig *schema.Entry) (name, ref string, found bool) {
	if trig == nil {
		return "", "", false
	}
	for _, ref := range ddl.TriggerBodyTableRefs(trig.SQL) {
		name := ref
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			name = name[dot+1:]
		}
		if strings.EqualFold(name, "NEW") || strings.EqualFold(name, "OLD") ||
			strings.EqualFold(name, "SET") {
			continue
		}
		entry, err := ctx.Schema.FindTable(name)
		if err != nil || entry == nil {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(entry.SQL), "CREATE VIRTUAL TABLE") {
			return name, ref, true
		}
	}
	return "", "", false
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
