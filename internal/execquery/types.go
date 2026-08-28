// Package execquery implements SELECT query execution.
//
// This package owns the SELECT execution family: join execution, aggregate
// evaluation, clause validation, table scanning, query planning, and the
// orchestration (core) that composes them. The Engine in internal/exec
// delegates SELECT statements to the SelectEngine here.
//
// The SelectEngine depends on a minimal SelectContext capability interface
// (the Engine implements it) rather than on the concrete engine type:
// Dependency Inversion. The sub-concerns (JoinExecutor, AggregateEvaluator,
// SelectValidator, TableScanner, QueryPlanner) are composed by SelectEngine,
// isolating each concern (Single Responsibility).
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/util"
)

// Row provides column value lookup for expression evaluation.
// It aliases execexpr.Row so the query engine and the expression evaluation
// package share the same row abstraction without a dependency from
// evaluation back to the engine.
type Row = execexpr.Row

// RowMap implements Row for map-backed row stores.
type RowMap = execexpr.RowMap

// positionalRowKey is a reserved RowMap key holding the row's original
// positional value slice. It lets SELECT * output duplicate-named columns
// (e.g. a view with three columns aliased ”) in order, which a name-keyed
// map cannot distinguish. The NUL byte cannot appear in a column name.
const positionalRowKey = "\x00frigolite_positional"

// CollatedValue wraps a value with a collation name for COLLATE support.
type CollatedValue = execexpr.CollatedValue

// Result holds the result of executing a SQL statement.
type Result struct {
	Columns         []string        // column names
	Rows            [][]interface{} // data rows
	Changes         int64           // number of changed rows
	InsertedChanges int64           // rows written as new inserts (excludes upsert DO UPDATE / DO NOTHING)
	Error           error           // execution error
	LastInsertRowID int64           // rowid of the last inserted row
	Row             []interface{}   // final row written by the statement (used by upsert RETURNING)
	// rowMaps carries the per-row column→value maps for statements that
	// materialize joined results (derived tables). It is internal: the public
	// API ignores it, but execJoins uses it to preserve qualified column keys
	// (t4.a) when a derived table is joined again.
	rowMaps []RowMap
	// keepPriorRowsOnError marks a failed statement whose rows written before
	// the conflict survive (SQLite ON CONFLICT FAIL resolution, statement OR
	// or per-constraint). The statement-level rollback skips the restore.
	keepPriorRowsOnError bool
	// rollbackTxOnError marks a failed statement that must roll back the whole
	// transaction (SQLite ON CONFLICT ROLLBACK resolution, statement OR or
	// per-constraint). The statement-level rollback performs a full tx rollback.
	rollbackTxOnError bool

	// forceRollbackOnError marks a failed statement whose changes must be
	// rolled back even under ON CONFLICT FAIL (which otherwise keeps prior
	// rows). Foreign key violations are statement-level errors: SQLite rolls
	// back the whole statement for them regardless of the OR clause.
	forceRollbackOnError bool
}

// SetKeepPriorRowsOnError marks a failed statement whose rows written before
// the conflict survive (ON CONFLICT FAIL). Used internally by DML execution
// so the statement-level rollback does not undo them.
func (r *Result) SetKeepPriorRowsOnError() {
	if r != nil {
		r.keepPriorRowsOnError = true
	}
}

// KeepPriorRowsOnError reports whether the statement's pre-conflict rows must
// survive (ON CONFLICT FAIL resolution).
func (r *Result) KeepPriorRowsOnError() bool {
	return r != nil && r.keepPriorRowsOnError
}

// SetRollbackTxOnError marks a failed statement that must roll back the whole
// transaction (ON CONFLICT ROLLBACK). Used internally by DML execution.
func (r *Result) SetRollbackTxOnError() {
	if r != nil {
		r.rollbackTxOnError = true
	}
}

// SetForceRollbackOnError marks a failed statement whose changes must be
// rolled back even under ON CONFLICT FAIL (foreign key violations are
// statement-level errors).
func (r *Result) SetForceRollbackOnError() {
	if r != nil {
		r.forceRollbackOnError = true
	}
}

// ForceRollbackOnError reports whether the statement must be rolled back even
// under ON CONFLICT FAIL.
func (r *Result) ForceRollbackOnError() bool {
	return r != nil && r.forceRollbackOnError
}

// RollbackTxOnError reports whether the statement must roll back the whole
// transaction (ON CONFLICT ROLLBACK resolution).
func (r *Result) RollbackTxOnError() bool {
	return r != nil && r.rollbackTxOnError
}

// DatabaseContext holds all per-database state for a single database connection.
type DatabaseContext struct {
	Name     string          // schema name ("main", "temp", etc.)
	Pager    *pager.Pager    // file access for this database
	Schema   *schema.Manager // tables, indexes, views for this db
	FilePath string          // path to .db file
	IsMemory bool            // in-memory database
	IsTemp   bool            // temp database
}

// StructRow is an index-based Row that stores values in a slice
// with a shared column name→position index, avoiding per-row map allocation.
type StructRow struct {
	Values []interface{}
	Index  map[string]int // shared across rows with same schema
	RowID  int64
}

// Get implements Row.
func (r *StructRow) Get(name string) (interface{}, bool) {
	// Prefer a real column with the given name (a table or pragma output may
	// legitimately declare a column named rowid/_rowid_/oid). Only fall back
	// to the implicit rowid when no such column exists.
	if idx, ok := r.Index[name]; ok && idx < len(r.Values) {
		return r.Values[idx], true
	}
	if IsRowIDName(name) {
		// rowid/_rowid_/oid are implicit INTEGER-affinity columns. Wrap them so
		// comparisons apply SQLite affinity rules (e.g. rowid <= '0'
		// converts '0' to 0), matching how buildRowMap wraps real columns.
		return &util.ColumnValue{Value: r.RowID, Affinity: 'I'}, true
	}
	// SQLite column names are case-insensitive: a query may reference the
	// column in a different case than the CREATE TABLE definition (e.g.
	// "SELECT col0 FROM t" for a column declared "Col0"). Fall back to a
	// case-insensitive scan of the small index.
	if len(r.Index) > 0 {
		for k, idx := range r.Index {
			if strings.EqualFold(k, name) && idx < len(r.Values) {
				return r.Values[idx], true
			}
		}
	}
	return nil, false
}

// IsRowIDName reports whether name is one of the implicit rowid aliases
// (rowid, _rowid_, oid).
func IsRowIDName(name string) bool {
	return execexpr.IsRowIDName(name)
}

// isRowIDName is the unexported alias used throughout the moved SELECT code.
func isRowIDName(name string) bool {
	return execexpr.IsRowIDName(name)
}

// UniqDef describes one UNIQUE/PK constraint on a table, used by the query
// planner to decide whether an autoindex is needed.
type UniqDef struct {
	Cols []string
	IsPK bool
}

// UniqueIndexDef describes a UNIQUE index on a table.
type UniqueIndexDef struct {
	Name    string
	Cols    []string
	KeyColl []string // per-key explicit COLLATE ("" = none) for target matching
	Where   string   // partial index predicate ("" for full indexes)
}

// OrConstraint is one constant equality inside an OR-index plan branch.
type OrConstraint struct {
	Col           string
	Val           interface{}
	ApplyAffinity bool
}

// OrBranchPlan is one OR term of an OR-index plan: the index whose leading
// columns the term constrains, plus the constant prefix values.
type OrBranchPlan struct {
	IndexName string
	IndexCols []string
	Prefix    []OrConstraint
}

// IndexPragmaColumn describes one column of PRAGMA index_info/xinfo output.
type IndexPragmaColumn struct {
	Name  string
	Desc  bool
	Coll  string // resolved collation ("" for BINARY)
	Cid   int64  // table column ordinal (-1 for rowid)
	Key   int64  // 1 for key columns, 0 for payload columns
	Rowid bool   // synthetic rowid column
}
