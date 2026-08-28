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

// Module implementation.

func (m *RtreeModule[T]) Create(args []string) (VirtualTable, error) {
	return m.connect(args, true)
}

func (m *RtreeModule[T]) Connect(args []string) (VirtualTable, error) {
	return m.connect(args, false)
}

func (m *RtreeModule[T]) connect(args []string, isCreate bool) (VirtualTable, error) {
	// frigolite passes only the USING arguments (the coordinate/aux column
	// names) to Create/Connect; the db/table name arrives via BindSchema.
	// Column 0 is the INTEGER PRIMARY KEY (rowid); the remaining columns are
	// coordinate pairs (min,max), optionally followed by '+'-prefixed aux cols.
	columns := append([]string(nil), args...)
	for i := range columns {
		columns[i] = strings.TrimSpace(columns[i])
	}
	// SQLite parses the module-argument list as SQL identifiers; a bare
	// reserved keyword (rtree1-10.1: USING rtree(index, ...)) fails prepare
	// with the parser's message.
	for _, c := range columns {
		if rtreeReservedWord(c) {
			return nil, fmt.Errorf("near %q: syntax error", c)
		}
	}
	// Count aux ('+') columns (declared after the coordinate block).
	nAux := 0
	coordEnd := len(columns)
	for i := 1; i < len(columns); i++ {
		if strings.HasPrefix(columns[i], "+") {
			nAux++
			coordEnd = i
			break
		}
	}
	for i := coordEnd + 1; i < len(columns); i++ {
		if !strings.HasPrefix(columns[i], "+") {
			// rtree.c rtreeInit: a regular column after an auxiliary one.
			return nil, fmt.Errorf("Auxiliary rtree columns must be last")
		}
	}
	nDim2 := coordEnd - 1 // coordinate columns after the rowid column
	// Argument validation mirrors rtree.c rtreeInit and its exact messages:
	// at least the id + one min/max pair; at most RTREE_MAX_DIMENSIONS pairs;
	// an odd coordinate count leaves a dimension without a upper bound.
	if coordEnd < 3 {
		return nil, fmt.Errorf("Too few columns for an rtree table")
	}
	if nDim2 > RTREE_MAX_DIMENSIONS*2 {
		return nil, fmt.Errorf("Too many columns for an rtree table")
	}
	if nDim2%2 != 0 {
		return nil, fmt.Errorf("Wrong number of columns for an rtree table")
	}
	v := &rtreeVTab[T]{
		module:        m,
		columns:       columns,
		nDim:          nDim2 / 2,
		nDim2:         nDim2,
		nAux:          nAux,
		nBytesPerCell: 8 + nDim2*4,
		coordKind:     m.coordKind,
	}
	// declare_vtab parity: auxiliary ('+') columns are reported to the core
	// under their bare name (the '+' is module-argument syntax only). Keeping
	// the raw list in v.columns preserves the nAux bookkeeping above while
	// Columns()/ColumnTypes() drive SQL name resolution, PRAGMA table_info and
	// INSERT's named-column matching (rtree1-10.x aux flows).
	v.declared = make([]string, len(columns))
	for i, c := range columns {
		v.declared[i] = strings.TrimPrefix(c, "+")
	}
	v.created = isCreate
	if err := v.queryStat1(); err != nil {
		return nil, err
	}
	return v, nil
}

// queryStat1 mirrors rtree.c rtreeQueryStat1, which runs on every xCreate and
// xConnect. It estimates the row count from sqlite_stat1 (tbl='<name>_rowid'),
// falling back to RTREE_DEFAULT_ROWEST when sqlite_stat1 is absent or has no
// matching row. The probe SELECT is *prepared* against sqlite_stat1, so when a
// hostile schema shadows the reserved name with a table lacking the `stat`
// column (vtabK: an fts5 virtual table renamed onto sqlite_stat1 via
// writable_schema), prepare fails with "no such column: stat" and the CREATE
// or CONNECT aborts with that exact error.
//
// rtree.c uses sqlite3_table_column_metadata for the existence check, which
// returns SQLITE_ERROR for a virtual table; here the probe SELECT itself is the
// existence/column check, which is equivalent for the error path and cheaper.
func (v *rtreeVTab[T]) queryStat1() error {
	v.nRowEst = RTREE_DEFAULT_ROWEST
	// zFmt: SELECT stat FROM %Q.sqlite_stat1 WHERE tbl = '%q_rowid'
	db := v.dbName
	if db == "" {
		db = "main"
	}
	sql := fmt.Sprintf("SELECT stat FROM %s.sqlite_stat1 WHERE tbl = '%s_rowid'",
		dquoteIdent(db), strings.ReplaceAll(v.name, "'", "''"))
	rows, err := v.module.db.ExecSQL(sql)
	if err != nil {
		// No sqlite_stat1 table at all → default estimate, no error (the C
		// metadata call returns SQLITE_ERROR which rtreeQueryStat1 swallows).
		// Any other prepare/resolve failure (no such column: stat) propagates.
		if isNoSuchTable(err) {
			return nil
		}
		return err
	}
	nRow := int64(RTREE_MIN_ROWEST)
	if len(rows) > 0 && len(rows[0]) > 0 {
		if n := rtreeAsInt64(rows[0][0]); n > 0 {
			nRow = n
		}
	}
	if nRow < RTREE_MIN_ROWEST {
		nRow = RTREE_MIN_ROWEST
	}
	v.nRowEst = nRow
	return nil
}

// isNoSuchTable reports whether err is the "no such table" resolution failure
// (sqlite_stat1 simply absent), which rtreeQueryStat1 treats as
// RTREE_DEFAULT_ROWEST rather than an error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// dquoteIdent renders an SQL identifier (schema name) double-quoted for use in
// the stat1 probe, escaping embedded double quotes. Mirrors sqlite3_mprintf
// "%Q" applied to the %Q.sqlite_stat1 format. (vtab's existing quoteIdent only
// escapes; it does not add the surrounding quotes.)
func dquoteIdent(s string) string {
	return `"` + quoteIdent(s) + `"`
}

// BindSchema receives the resolved db + table name. The engine calls it after
// Create/Connect. rtree names its shadow tables after the table, so it creates
// them here (idempotently) once the name is known, then sizes the nodes.
// On the Connect side (existing table) the node size is INFERRED from the
// root blob length — getNodeSize parity: a blob shorter than 448 bytes (the
// smallest legal page-derived node) yields the verbatim corrupt error.
func (v *rtreeVTab[T]) BindSchema(dbName, tableName string) error {
	v.dbName = dbName
	v.name = tableName
	if !v.created {
		if err := v.inferNodeSizeFromRoot(); err != nil {
			return err
		}
		if v.iNodeSize <= 0 {
			// Absent root (e.g. %_node emptied by corruption tests): fall back
			// to page-size sizing so statement-level instances materialize and
			// per-node-load guards report corruption like rtree.c does.
			v.computeNodeSize()
		}
		return v.createShadowTables()
	}
	v.computeNodeSize()
	return v.createShadowTables()
}

// inferNodeSizeFromRoot mirrors getNodeSize's connect branch for HEALTHY
// tables: a root blob of at least 448 bytes becomes the instance's node
// size. Shorter or missing roots are NOT fatal here — SQLite runs
// getNodeSize once per connection before any corruption, and later
// statement-level instances must still materialize so the query path can
// report the per-cursor corruption ("database disk image is malformed"
// via the short-blob guard in nodeAcquire).
func (v *rtreeVTab[T]) inferNodeSizeFromRoot() error {
	sql := fmt.Sprintf("SELECT length(data) FROM \"%s\" WHERE nodeno=1",
		strings.ReplaceAll(v.name+"_node", `"`, `""`))
	rows, err := v.module.db.ExecSQL(sql)
	if err != nil {
		return nil // schema-level trouble surfaces on the real statement
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		// A ROW exists: rtree.c reads its byte length via
		// sqlite3_blob_bytes regardless of content. An EMPTY or short blob
		// (<448 = min page_size minus reserve) is the verbatim connect-time
		// corruption from getNodeSize.
		size := int(rtreeAsInt64(rows[0][0]))
		if size < 512-64 {
			// Keep connection successful; cursor acquisition reports corruption
			// using SQLite's generic malformed-image error.
			v.iNodeSize = 0
			return nil
		}
		v.iNodeSize = size
		return nil
	}
	return nil // no root row at all: caller falls back to page-size sizing
}

// ColumnTypes implements ColumnTypeInfo: the id column is INTEGER PRIMARY
// KEY, coordinate columns are REAL (module rtree) or INTEGER (rtree_i32) and
// auxiliary columns have no declared type. Declaring these drives SQLite's
// comparison affinity in the core (rtree1-18.0: `c1 > '-1'` compares as
// REAL vs converted -1).
func (v *rtreeVTab[T]) ColumnTypes() []string {
	out := make([]string, len(v.columns))
	coord := "REAL"
	if v.coordKind == RTREE_COORD_INT32 {
		coord = "INTEGER"
	}
	for i := range out {
		switch {
		case i == 0:
			out[i] = "INTEGER"
		case i <= v.nDim2:
			out[i] = coord
		default:
			out[i] = ""
		}
	}
	return out
}

// computeNodeSize mirrors rtree.c getNodeSize: page_size-64, capped at
// 4 + nBytesPerCell*RTREE_MAXCELLS so every node fits on one page.
func (v *rtreeVTab[T]) computeNodeSize() {
	ps := 4096
	if rows, err := v.module.db.ExecSQL("PRAGMA page_size"); err == nil && len(rows) > 0 && len(rows[0]) > 0 {
		if ps2 := rtreeAsInt64(rows[0][0]); ps2 > 0 {
			ps = int(ps2)
		}
	}
	size := ps - 64
	maxByCells := 4 + v.nBytesPerCell*RTREE_MAXCELLS
	if maxByCells < size {
		size = maxByCells
	}
	v.iNodeSize = size
}

// createShadowTables builds the three backing tables used by rtree.c:
// <name>_node (one node blob per nodeno), <name>_rowid (entry rowid -> node),
// <name>_parent (node -> parent node). The root node (nodeno 1) is seeded as a
// zeroblob so every operation has a stable root to acquire.
func (v *rtreeVTab[T]) createShadowTables() error {
	// rtree.c rtreeSqlInit aborts the CREATE when any shadow name is already
	// taken by an ordinary table, quoting the name (sqlite3 CLI parity:
	// `table "t1_rowid" already exists`). The check applies only while the
	// virtual table itself is not yet in the schema (first xCreate); later
	// binds must tolerate their own shadow rows (idempotent re-bind).
	for _, suffix := range []string{"node", "rowid", "parent"} {
		nm := v.name + "_" + suffix
		rows, err := v.module.db.ExecSQL(
			fmt.Sprintf("SELECT name FROM sqlite_master WHERE name IN ('%s','%s')",
				strings.ReplaceAll(v.name, "'", "''"),
				strings.ReplaceAll(nm, "'", "''")))
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}
		// If the virtual table's own entry exists we are re-binding over an
		// intact shadow family; anything else is a hostile collision.
		if !schemaHasOwnVTab(rows, v.name) {
			return fmt.Errorf("table %q already exists", nm)
		}
	}
	return createShadowDDL(v)
}

// schemaHasOwnVTab reports whether one of the queried sqlite_master rows is
// the virtual table itself (name matches v.name).
func schemaHasOwnVTab(rows [][]interface{}, vname string) bool {
	for _, row := range rows {
		if len(row) > 0 && nameOf(row[0]) == vname {
			return true
		}
	}
	return false
}

// createShadowDDL emits and runs the three CREATE TABLE IF NOT EXISTS
// statements for the rtree shadow family. The root node (nodeno=1) is seeded
// ONLY on the xCreate side: ext/rtree's rtreeSqlInit creates it together with
// the tables; later binds must tolerate a missing root (rtree8-2.x deletes
// %_node outright and the next SELECT must report corruption, not resurrect
// an empty tree).
func createShadowDDL[T coordType](v *rtreeVTab[T]) error {
	q := func(s string) string { return strings.ReplaceAll(s, `"`, `""`) }
	ddl := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS "%[1]s_node"(nodeno INTEGER PRIMARY KEY, data);`+
			`CREATE TABLE IF NOT EXISTS "%[1]s_rowid"(rowid INTEGER PRIMARY KEY, nodeno%[2]s);`+
			`CREATE TABLE IF NOT EXISTS "%[1]s_parent"(nodeno INTEGER PRIMARY KEY, parentnode);`,
		q(v.name), v.auxColumnsSQL())
	if v.created {
		ddl += fmt.Sprintf(`INSERT OR IGNORE INTO "%s_node"(nodeno, data) VALUES(1, zeroblob(%d));`,
			q(v.name), v.iNodeSize)
	}
	_, err := v.module.db.ExecSQL(ddl)
	return err
}

// nameOf coerces a sqlite_master.name cell to string.
func nameOf(v interface{}) string {
	s, _ := v.(string)
	return s
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

// rtreeReservedWord reports whether the bare identifier is one of the SQL
// keywords SQLite's lexer refuses as an unquoted column name inside a module
// declaration (subset covering the common cases; quoted names pass through).
func rtreeReservedWord(word string) bool {
	switch strings.ToUpper(strings.TrimSpace(word)) {
	case "INDEX", "WHERE", "ORDER", "GROUP", "SELECT", "FROM", "TABLE",
		"CREATE", "VALUES", "LIMIT", "UNION", "HAVING", "SET", "PRIMARY",
		"UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT", "NOT", "NULL", "AND",
		"OR", "IN", "IS", "BETWEEN", "LIKE", "GLOB", "CASE", "WHEN", "THEN",
		"ELSE", "END", "JOIN", "LEFT", "RIGHT", "FULL", "INNER", "OUTER",
		"CROSS", "NATURAL", "USING", "ON", "AS", "BY", "DESC", "ASC",
		"DEFAULT", "COLLATE", "CURRENT", "MATCH", "REGEXP", "EXISTS":
		return true
	}
	return false
}
func (v *rtreeVTab[T]) auxColumnsSQL() string {
	if v.nAux == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < v.nAux; i++ {
		fmt.Fprintf(&b, ",a%d", i)
	}
	return b.String()
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
	// when declared.
	if v.nAux > 0 {
		if err := v.attachAuxColumns(rows); err != nil {
			return nil, err
		}
	}
	return &rtreeCursor[T]{rows: rows, idx: -1}, nil
}

// attachAuxColumns reads each scanned entry's auxiliary column values from
// its %_rowid row and appends them to the data columns.
func (v *rtreeVTab[T]) attachAuxColumns(rows [][]interface{}) error {
	for i, row := range rows {
		id, ok := row[0].(int64)
		if !ok {
			continue
		}
		cols := []string{"rowid"}
		for a := 0; a < v.nAux; a++ {
			cols = append(cols, fmt.Sprintf("a%d", a))
		}
		q := `SELECT ` + strings.Join(cols, ",") + ` FROM %s WHERE rowid=%d`
		out, err := v.module.db.ExecSQL(fmt.Sprintf(q, v.shadow("rowid"), id))
		if err != nil || len(out) == 0 {
			continue // detached mapping: surface NULLs like sqlite3 does
		}
		vals := out[0][1:]
		rows[i] = append(row, vals...)
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

func (c *rtreeCursor[T]) Close() error { return nil }

// compile-time assertion that rtreeVTab satisfies the required interfaces.
var (
	_ VirtualTable    = (*rtreeVTab[float32])(nil)
	_ VirtualTable    = (*rtreeVTab[int32])(nil)
	_ RowUpdater      = (*rtreeVTab[float32])(nil)
	_ RowUpdater      = (*rtreeVTab[int32])(nil)
	_ SchemaBoundVTab = (*rtreeVTab[float32])(nil)
	_ SchemaBoundVTab = (*rtreeVTab[int32])(nil)
)
