package vtab

import (
	"fmt"
	"strings"
)

// Coordinate-kind discriminator for the rtree module family.
const (
	RTREE_COORD_REAL32 = iota // float32 coordinates (module "rtree")
	RTREE_COORD_INT32         // int32 coordinates   (module "rtree_i32")
)

// Row-estimate bounds for the query planner (rtree.c RTREE_DEFAULT_ROWEST /
// RTREE_MIN_ROWEST): the default when no sqlite_stat1 row exists, and the hard
// floor applied to any estimate.
const (
	RTREE_DEFAULT_ROWEST = 1048576
	RTREE_MIN_ROWEST     = 100
)

// coordType is the generic constraint for rtree coordinate scalars. float32
// matches SQLite's RTREE_COORD_REAL32 (4-byte float, stored exactly — not a
// double) and int32 matches RTREE_COORD_INT32.
type coordType interface{ float32 | int32 }

// coordToOut widens a coordinate scalar to the SQL value the cursor surfaces:
// float32 rtree columns are REAL (float64), rtree_i32 columns are INTEGER (int64).
func coordToOut[T coordType](c T) interface{} {
	var z T
	switch any(z).(type) {
	case float32:
		return float64(float32(any(c).(float32)))
	case int32:
		return int64(int32(any(c).(int32)))
	}
	return nil
}

// RtreeModule implements the SQLite rtree / rtree_i32 virtual table modules
// (ext/rtree/rtree.c), parameterized by the coordinate scalar type T. The R-tree
// algorithm (B+tree over shadow tables, MBR queries, SQL functions) is shared;
// only coordinate serialization differs (coordCodec, added in later slices).
type RtreeModule[T coordType] struct {
	db        Database
	coordKind int
}

// NewRtreeModule builds an rtree module bound to the engine's Database handle
// (constructor DI, mirroring NewDBPageModule). T selects the coordinate type.
func NewRtreeModule[T coordType](db Database) *RtreeModule[T] {
	var zero T
	kind := RTREE_COORD_REAL32
	if _, ok := any(zero).(int32); ok {
		kind = RTREE_COORD_INT32
	}
	return &RtreeModule[T]{db: db, coordKind: kind}
}

// Module implementation (xCreate/xConnect bodies live in rtree_init.go).

func (m *RtreeModule[T]) Create(args []string) (VirtualTable, error) {
	return m.connect(args, true)
}

func (m *RtreeModule[T]) Connect(args []string) (VirtualTable, error) {
	return m.connect(args, false)
}

// ColumnTypes implements ColumnTypeInfo: the declared types SQLite's
// declare_vtab receives from rtreeInit — `%.*s INT` for the id column, then
// `,%.*s REAL` (module rtree) or `,%.*s INT` (rtree_i32) per coordinate and
// no type for auxiliary columns. "INT" carries INTEGER affinity (value.Affinity
// prefix rules), which drives the core's comparison affinity (rtree1-18.0:
// `c1 > '-1'` compares as REAL vs converted -1) and PRAGMA table_info output.
func (v *rtreeVTab[T]) ColumnTypes() []string {
	out := make([]string, len(v.columns))
	coord := "REAL"
	if v.coordKind == RTREE_COORD_INT32 {
		coord = "INT"
	}
	for i := range out {
		switch {
		case i == 0:
			out[i] = "INT"
		case i <= v.nDim2:
			out[i] = coord
		default:
			out[i] = ""
		}
	}
	return out
}

// RTreeFamilyModuleOf reports whether sqlStr creates an rtree/rtree_i32
// virtual table ("CREATE VIRTUAL TABLE <name> USING rtree...").
func RTreeFamilyModuleOf(sqlStr string) bool {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(sqlStr[idx+len(" USING "):])
	end := strings.IndexAny(rest, "( \t\n\r,")
	name := strings.ToLower(rest)
	if end > 0 {
		name = strings.ToLower(rest[:end])
	}
	return name == "rtree" || name == "rtree_i32"
}

// VirtualTable implementation.

func (v *rtreeVTab[T]) BestIndex(input []byte) ([]byte, error) { return nil, nil }

func (v *rtreeVTab[T]) Open() (Cursor, error) {
	return v.openCursor()
}

// Columns reports the declared schema: the rowid column followed by the
// coordinate (and optional auxiliary) columns — matches SQLite's declare_vtab,
// which receives aux columns under their bare names (no '+' prefix).
func (v *rtreeVTab[T]) Columns() []string { return v.declared }

// rtreeVTab is one bound rtree instance. Its coordinate source and tree state are
// held in the shadow tables created at xCreate time; an in-memory node cache is
// rebuilt per operation to avoid cross-statement staleness.
type rtreeVTab[T coordType] struct {
	module        *RtreeModule[T]
	dbName        string
	name          string
	columns       []string
	nDim          int
	nDim2         int
	nAux          int
	nBytesPerCell int
	iNodeSize     int
	iDepth        int
	coordKind     int
	cache         map[int64]*rtreeNode[T]
	deleted       *rtreeNode[T]
	pending       []rtreeConstraint[T] // pushed coordinate/rowid predicates (t4)
	pendingRowids rtreeRowidSet        // pushed `id IN (...)` membership
	pendingMatch  *RtreeGeometry       // pushed MATCH geometry callback (t5)
	matchErr      error                // non-geometry MATCH argument error
	declared      []string             // Columns() view (aux names sans '+')
	created       bool                 // xCreate (true) vs xConnect side of this instance
	pendingAux    []interface{}        // auxiliary values of the row being written
	nRowEst       int64                // planner row estimate (rtreeQueryStat1)
}

// ---- cursor (full scan; constraint-filtered search added in slice 4) ----

// rtreeCursor is the row scanner: it precomputes the data rows by walking the
// tree depth-first (depthLeft==0 nodes hold leaf entries), matching SQLite's
// full-scan enumeration order.
type rtreeCursor[T coordType] struct {
	rows [][]interface{}
	idx  int
}

// openCursor scans the r-tree and returns a cursor over its entries. When the
// engine pushed constraints down (see constraintSink), the scan filters at
// the r-tree level with numeric semantics and skips re-evaluating those
// conjuncts as SQL.
func (v *rtreeVTab[T]) openCursor() (Cursor, error) {
	v.newNodeCache()
	defer func() { _ = v.nodeFlush() }()
	constraints, rowids, match, matchErr := v.resetPending()
	if matchErr != nil {
		return nil, matchErr
	}
	rows, err := v.collectDataRows(constraints, rowids, match)
	if err != nil {
		return nil, err
	}
	// Aux columns come from %_rowid.aN and are joined onto every scanned row
	// when declared; constraints pushed on aux columns are re-checked against
	// the attached values (they have no coordinates).
	if v.nAux > 0 {
		if err := v.attachAuxColumns(rows); err != nil {
			return nil, err
		}
		rows = v.filterAuxConstraints(rows, constraints)
	}
	return &rtreeCursor[T]{rows: rows, idx: -1}, nil
}

// attachAuxColumns reads each scanned entry's auxiliary column values from
// its %_rowid row and appends them to the data columns. A detached mapping
// (no %_rowid row) surfaces NULLs like sqlite3 does — every row keeps
// exactly 1+nDim2+nAux cells.
func (v *rtreeVTab[T]) attachAuxColumns(rows [][]interface{}) error {
	for i, row := range rows {
		attached := make([]interface{}, v.nAux)
		if id, ok := row[0].(int64); ok {
			cols := []string{"rowid"}
			for a := 0; a < v.nAux; a++ {
				cols = append(cols, fmt.Sprintf("a%d", a))
			}
			q := `SELECT ` + strings.Join(cols, ",") + ` FROM %s WHERE rowid=%d`
			out, err := v.module.db.ExecSQL(fmt.Sprintf(q, v.shadow("rowid"), id))
			if err == nil && len(out) > 0 && len(out[0]) > 0 {
				copy(attached, out[0][1:])
			}
		}
		rows[i] = append(row, attached...)
	}
	return nil
}

func (c *rtreeCursor[T]) Next() bool {
	c.idx++
	return c.idx < len(c.rows)
}

func (c *rtreeCursor[T]) Column(idx int) (interface{}, error) {
	if c.idx < 0 || c.idx >= len(c.rows) {
		return nil, nil
	}
	if idx < 0 || idx >= len(c.rows[c.idx]) {
		return nil, fmt.Errorf("rtree: column index %d out of range", idx)
	}
	return c.rows[c.idx][idx], nil
}

// Rowid implements vtab.RowidCursor: the current entry's id column (xRowid
// parity). execdml's vtab UPDATE/DELETE row maps inject rowid/_rowid_/oid
// from it, so `DELETE FROM t1 WHERE rowid>1` filters on the real ids instead
// of a NULL (rtreeJ-1.9).
func (c *rtreeCursor[T]) Rowid() int64 {
	if c.idx < 0 || c.idx >= len(c.rows) {
		return 0
	}
	if id, ok := c.rows[c.idx][0].(int64); ok {
		return id
	}
	return 0
}

func (c *rtreeCursor[T]) Close() error { return nil }

// compile-time assertion that rtreeVTab satisfies the required interfaces.
var (
	_ VirtualTable    = (*rtreeVTab[float32])(nil)
	_ VirtualTable    = (*rtreeVTab[int32])(nil)
	_ RowUpdater      = (*rtreeVTab[float32])(nil)
	_ RowUpdater      = (*rtreeVTab[int32])(nil)
	_ SchemaBoundVTab = (*rtreeVTab[float32])(nil)
	_ SchemaBoundVTab = (*rtreeVTab[int32])(nil)
	_ RowidCursor     = (*rtreeCursor[float32])(nil)
	_ RowidCursor     = (*rtreeCursor[int32])(nil)
)
