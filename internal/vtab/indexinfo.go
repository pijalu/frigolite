package vtab

import (
	"errors"
	"math"
)

// IndexConstraintOp is the constraint-operator code passed to xBestIndex in
// sqlite3_index_info.aConstraint[].op. The numeric values mirror
// sqlite3.h's SQLITE_INDEX_CONSTRAINT_* constants exactly (where.c relies on
// WO_* == SQLITE_INDEX_CONSTRAINT_* identity).
type IndexConstraintOp int

// SQLITE_INDEX_CONSTRAINT_* operator codes (sqlite3.h:7799-7815).
const (
	IndexConstraintEq        IndexConstraintOp = 2
	IndexConstraintGt        IndexConstraintOp = 4
	IndexConstraintLe        IndexConstraintOp = 8
	IndexConstraintLt        IndexConstraintOp = 16
	IndexConstraintGe        IndexConstraintOp = 32
	IndexConstraintMatch     IndexConstraintOp = 64
	IndexConstraintLike      IndexConstraintOp = 65
	IndexConstraintGlob      IndexConstraintOp = 66
	IndexConstraintRegexp    IndexConstraintOp = 67
	IndexConstraintNe        IndexConstraintOp = 68
	IndexConstraintIsNot     IndexConstraintOp = 69
	IndexConstraintIsNotNull IndexConstraintOp = 70
	IndexConstraintIsNull    IndexConstraintOp = 71
	IndexConstraintIs        IndexConstraintOp = 72
	// IndexConstraintFunction is the first overloadable-function constraint
	// code: a module's xFindFunction returning a value >= this code makes
	// f(vtab_column, expr) an auxiliary vtab constraint (whereexpr.c
	// isAuxiliaryVtabOperator). LIMIT/OFFSET follow (whereexpr.c
	// sqlite3WhereAddLimit); isLimitTerm's
	// eMatchOp>=LIMIT && eMatchOp<=OFFSET range check requires exactly
	// these values (sqlite.h.in:7813-7814).
	IndexConstraintFunction IndexConstraintOp = 150
	IndexConstraintLimit    IndexConstraintOp = 73
	IndexConstraintOffset   IndexConstraintOp = 74
)

// SQLITE_INDEX_SCAN_* idxFlags bits (sqlite3.h).
const (
	// IndexScanUnique reports the scan visits at most one row
	// (SQLITE_INDEX_SCAN_UNIQUE).
	IndexScanUnique = 0x00000001
	// IndexScanHex asks EQP to render idxNum in hex (SQLITE_INDEX_SCAN_HEX).
	IndexScanHex = 0x00000002
)

// ErrVtabConstraint mirrors SQLITE_CONSTRAINT from xBestIndex: the offered
// constraint combination cannot form a viable plan and the planner must
// reject it without erroring the statement (where.c vtabBestIndex /
// whereLoopAddVirtualOne).
var ErrVtabConstraint = errors.New("vtab: constraint failed")

// IndexConstraint is one WHERE-clause term offered to xBestIndex
// (sqlite3_index_info.aConstraint[] entry).
type IndexConstraint struct {
	// Column is the constrained column's index in declared order, or -1 for
	// the rowid.
	Column int
	// Op is the constraint operator.
	Op IndexConstraintOp
	// Usable reports whether the constraint can be used in the current plan
	// (its right-hand side references no tables positioned earlier in the
	// join that are unavailable).
	Usable bool
	// IsIn marks a constraint that originated from an IN(...) list; the
	// planner maps IN to EQ (op==IndexConstraintEq) and the engine runs one
	// xFilter per list element.
	IsIn bool
	// TermOffset indexes the conjunct slice returned alongside the IndexInfo
	// by the planner, so the engine can recover the WHERE expression this
	// constraint came from (HiddenIndexInfo.aRhs / iTermOffset parity).
	TermOffset int
}

// IndexOrderBy is one ORDER BY term offered to xBestIndex
// (sqlite3_index_info.aOrderBy[] entry).
type IndexOrderBy struct {
	// Column is the ordered column's index in declared order, or -1 for the
	// rowid.
	Column int
	// Desc is true for a DESC term.
	Desc bool
}

// ConstraintUsage is the xBestIndex output for one constraint
// (sqlite3_index_info.aConstraintUsage[] entry).
type ConstraintUsage struct {
	// ArgvIndex is the 1-based position of this constraint's value in the
	// xFilter argv (0 = unused).
	ArgvIndex int
	// Omit tells the core the virtual table fully handles the constraint, so
	// it is not re-checked per row (sqlite3 omit).
	Omit bool
}

// IndexInfo is the Go port of sqlite3_index_info: the xBestIndex contract
// between the query planner and a virtual-table module.
type IndexInfo struct {
	// Constraints is the WHERE-clause term list (nConstraint).
	Constraints []IndexConstraint
	// OrderBy is the ORDER BY term list when every term references this
	// vtab (nOrderBy; nil otherwise).
	OrderBy []IndexOrderBy
	// Usage is the per-constraint xBestIndex output (aConstraintUsage[]).
	// It is zeroed before the call; ArgvIndex must form a contiguous
	// 1..N sequence or the plan is a malfunction.
	Usage []ConstraintUsage
	// Distinct is the sqlite3_vtab_distinct() hint: 1 = GROUP BY scan (any
	// output row subset is acceptable), 2 = DISTINCT (sorted output must
	// contain no duplicates), 3 = DISTINCT + ORDER BY (sorted, no
	// duplicates).
	Distinct int
	// IdxNum/IdxStr are the module's private plan identifier, echoed to
	// xFilter and rendered in EQP as "VIRTUAL TABLE INDEX <idxNum>:<idxStr>".
	IdxNum int
	IdxStr string
	// OrderByConsumed, when set, means the vtab outputs rows in OrderBy
	// order and the core skips sorting.
	OrderByConsumed bool
	// EstimatedCost is the per-row cost estimate (default
	// SQLITE_BIG_DBL/2, where.c:4300).
	EstimatedCost float64
	// EstimatedRows is the row-count estimate (default 25).
	EstimatedRows int64
	// IdxFlags carries the SQLITE_INDEX_SCAN_* bits.
	IdxFlags int
	// ColsUsed is a bitmask of vtab columns the statement references: bit
	// (1<<i) for column i, bit 63 for column 63 and higher. Unused columns
	// may be omitted from the declared schema.
	ColsUsed uint64
}

// NewIndexInfo returns an IndexInfo initialized with the planner defaults of
// whereLoopAddVirtualOne (where.c:4300-4304): estimatedCost =
// SQLITE_BIG_DBL/2 and estimatedRows = 25.
func NewIndexInfo() *IndexInfo {
	return &IndexInfo{
		EstimatedCost: math.MaxFloat64 / 2, // SQLITE_BIG_DBL/2
		EstimatedRows: 25,
	}
}

// SetColsUsed sets the ColsUsed bits for the given column indexes; indexes
// >= 63 all map to bit 63 (sqlite3 colUsed semantics).
func (ii *IndexInfo) SetColsUsed(cols ...int) {
	for _, c := range cols {
		if c > 62 {
			c = 62
		}
		ii.ColsUsed |= uint64(1) << uint(c)
	}
}

// PlanBestIndexer is the optional xBestIndex contract (sqlite3_module
// xBestIndex). Modules implementing it receive the planner's constraint set
// before a scan is materialized; returning ErrVtabConstraint rejects the
// plan. Instances that do not implement it keep the legacy behavior (full
// materialization, WHERE applied by the core) — Open/Closed: the 30+
// existing modules are untouched.
type PlanBestIndexer interface {
	BestIndexPlan(ii *IndexInfo) error
}

// PlanFilterer is the optional xFilter contract on a Cursor
// (sqlite3_module xFilter): before the first Next, the engine hands the
// cursor the plan chosen by BestIndexPlan plus the argv values bound in
// argvIndex order.
type PlanFilterer interface {
	FilterPlan(idxNum int, idxStr string, argv []interface{}) error
}

// FunctionOverloader is the optional xFindFunction contract
// (sqlite3_module xFindFunction): a return value >=
// IndexConstraintFunction makes calls of the named two-argument function
// whose FIRST argument is a column of this vtab an auxiliary vtab
// constraint (whereexpr.c isAuxiliaryVtabOperator).
type FunctionOverloader interface {
	FindFunction(name string, nArg int) int
}
