package vtab

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// This file ports src/dbstat.c — the "dbstat" virtual table reporting one row
// per b-tree page (name, path, pageno, pagetype, ncell, payload, unused,
// mx_payload, pgoffset, pgsize) plus per-btree aggregates through the hidden
// "aggregate" column. Pages come from a PageSource (the enginePageSources
// provider, the sqlite_dbpage mechanism); the b-tree set is enumerated from
// sqlite_schema through the Database handle (dbstat.c statFilter builds the
// same "SELECT ... FROM sqlite_schema WHERE rootpage!=0" statement).

// DBStatPagePaddingBytes mirrors DBSTAT_PAGE_PADDING_BYTES: decoding reads a
// few bytes past declared cell offsets on corrupt pages, so every page buffer
// carries zero padding (dbstat.c statGetPage memsets the tail).
const DBStatPagePaddingBytes = 100

// DBStatModule provides the eponymous "dbstat" table (xCreate == xConnect ==
// statConnect, dbstat.c:874-905 — usable both as CREATE VIRTUAL TABLE and as
// a FROM-clause implicit table).
type DBStatModule struct {
	provider PageSourceProvider
	db       Database
}

// NewDBStatModule builds the module over a page-source provider and a
// Database handle for sqlite_schema enumeration.
func NewDBStatModule(provider PageSourceProvider, db Database) *DBStatModule {
	return &DBStatModule{provider: provider, db: db}
}

// Create implements Module (statConnect parity).
func (m *DBStatModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

// Connect implements Module (statConnect parity).
func (m *DBStatModule) Connect(args []string) (VirtualTable, error) {
	return &dbStatVTab{mod: m}, nil
}

// Eponymous implements EponymousModule: FROM dbstat resolves without CREATE.
func (m *DBStatModule) Eponymous() bool { return true }

type dbStatVTab struct {
	mod       *DBStatModule
	schema    string // hidden schema=? binding ("" = module default main)
	aggregate bool   // hidden aggregate=? binding
}

// Columns implements ColumnInfo (dbstat.c zDbstatSchema).
func (v *dbStatVTab) Columns() []string {
	return []string{"name", "path", "pageno", "pagetype", "ncell", "payload",
		"unused", "mx_payload", "pgoffset", "pgsize", "schema", "aggregate"}
}

// HiddenColumns implements HiddenColumnInfo: schema (10) and aggregate (11).
func (v *dbStatVTab) HiddenColumns() map[int]bool {
	return map[int]bool{10: true, 11: true}
}

// SetHiddenConstraint implements HiddenConstraintSetter: WHERE equality
// bindings on the hidden columns (dbstat.c statFilter's idxNum 0x01/0x04).
// name=? stays visible and filters in the residual WHERE (identical rows).
func (v *dbStatVTab) SetHiddenConstraint(col string, val interface{}) error {
	switch strings.ToLower(col) {
	case "schema":
		if s, ok := val.(string); ok {
			v.schema = s
		} else {
			v.schema = fmt.Sprintf("%v", val)
		}
	case "aggregate":
		v.aggregate = truthyVtabValue(val)
	}
	return nil
}

func truthyVtabValue(val interface{}) bool {
	switch n := val.(type) {
	case int64:
		return n != 0
	case float64:
		return n != 0
	case string:
		return n != "" && n != "0"
	case []byte:
		return len(n) > 0
	case bool:
		return n
	}
	return val != nil
}

// BestIndex accepts the default plan (statBestIndex offers the hidden
// constraints to the core, which re-resolves them through the bindings above).
func (v *dbStatVTab) BestIndex([]byte) ([]byte, error) { return nil, nil }

// Open materializes the scan: the b-tree walk (statDecodePage/statNext) runs
// eagerly into rows — frigolite's vtab materialization model.
func (v *dbStatVTab) Open() (Cursor, error) {
	schema := v.schema
	if schema == "" {
		schema = "main"
	}
	src, ok := v.mod.provider.PageSourceFor(schema)
	if !ok {
		// Unknown database: C sets isEof (zero rows).
		return &dbStatCursor{}, nil
	}
	rows, rowids, err := v.mod.walkAll(src, schema, v.aggregate)
	if err != nil {
		return nil, err
	}
	return &dbStatCursor{rows: rows, rowids: rowids}, nil
}

// dbStatCursor serves the materialized rows; rowid = pageno (statRowid —
// aggregate rows report the page count, matching statColumn's pageno case).
type dbStatCursor struct {
	rows   [][]interface{}
	rowids []int64
	idx    int
}

// Next positions the cursor at the next row (vtab Cursor contract: Next
// positions, Column reads — idx 0 is the NEXT row to serve, so the current
// row is rows[idx-1] after a true Next, matching the other vtab cursors).
func (c *dbStatCursor) Next() bool { c.idx++; return c.idx-1 < len(c.rows) }
func (c *dbStatCursor) Column(idx int) (interface{}, error) {
	if c.idx-1 < 0 || c.idx-1 >= len(c.rows) || idx < 0 || idx >= len(c.rows[c.idx-1]) {
		return nil, fmt.Errorf("dbstat: invalid column index %d", idx)
	}
	return c.rows[c.idx-1][idx], nil
}
func (c *dbStatCursor) Rowid() int64 {
	if c.idx-1 >= 0 && c.idx-1 < len(c.rowids) {
		return c.rowids[c.idx-1]
	}
	return 0
}
func (c *dbStatCursor) Close() error { return nil }

// dbStatCell is one decoded cell (dbstat.c StatCell).
type dbStatCell struct {
	nLocal   int
	childPg  uint32
	ovfl     []uint32
	nOvfl    int
	lastOvfl int
}

// dbStatPage is one decoded page (dbstat.c StatPage).
type dbStatPage struct {
	pgno      uint32
	data      []byte
	flags     byte
	nCell     int
	unused    int
	mxPayload int
	cells     []dbStatCell
	rightPg   uint32
	path      string
}

// statGetPage reads one page with zero padding (statGetPage parity).
func statGetPage(src PageSource, pgno uint32) (*dbStatPage, error) {
	data, err := src.ReadPage(pgno)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, len(data)+DBStatPagePaddingBytes)
	copy(buf, data)
	return &dbStatPage{pgno: pgno, data: buf}, nil
}

// statUsableBytes computes pageSize - reserved (byte 20 of the file header).
func statUsableBytes(src PageSource, page1 []byte) int {
	reserved := 0
	if len(page1) > 20 {
		reserved = int(page1[20])
	}
	return int(src.PageSize()) - reserved
}

// statGetLocalPayload ports dbstat.c getLocalPayload verbatim.
func statGetLocalPayload(nUsable int, flags byte, nTotal int) int {
	var nMinLocal, nMaxLocal int
	if flags == 0x0D {
		nMinLocal = (nUsable-12)*32/255 - 23
		nMaxLocal = nUsable - 35
	} else {
		nMinLocal = (nUsable-12)*32/255 - 23
		nMaxLocal = (nUsable-12)*64/255 - 23
	}
	nLocal := nMinLocal + (nTotal-nMinLocal)%(nUsable-4)
	if nLocal > nMaxLocal {
		nLocal = nMinLocal
	}
	return nLocal
}

// statDecodePage ports dbstat.c statDecodePage. A corrupt page leaves flags=0
// (pagetype "corrupted") with the cells cleared instead of erroring.
func statDecodePage(p *dbStatPage, nUsable int) {
	if !statDecodeHeader(p) {
		return
	}
	statDecodeCells(p, nUsable)
}

// statDecodeHeader reads the b-tree page header into p (flags, cell count,
// freeblock-chained unused bytes, right-most child). ok=false marks the page
// corrupt (statPageIsCorrupt).
func statDecodeHeader(p *dbStatPage) bool {
	aData := p.data
	hdrOff := 0
	if p.pgno == 1 {
		hdrOff = 100
	}
	if hdrOff+12 > len(aData) {
		p.flags = 0
		return false
	}
	aHdr := aData[hdrOff:]
	p.flags = aHdr[0]
	isLeaf := false
	nHdr := 0
	switch p.flags {
	case 0x0A, 0x0D:
		isLeaf, nHdr = true, 8
	case 0x05, 0x02:
		isLeaf, nHdr = false, 12
	default:
		p.flags = 0
		return false
	}
	if p.pgno == 1 {
		nHdr += 100
	}
	p.nCell = int(binary.BigEndian.Uint16(aHdr[3:5]))
	p.mxPayload = 0
	szPage := len(aData) - DBStatPagePaddingBytes
	if nHdr+2*p.nCell > szPage {
		p.flags = 0
		return false
	}
	nUnused := int(binary.BigEndian.Uint16(aHdr[5:7])) - nHdr - 2*p.nCell
	nUnused += int(aHdr[7])
	if !statAccumFreeblocks(aData, int(binary.BigEndian.Uint16(aHdr[1:3])), szPage, &nUnused) {
		p.flags = 0
		return false
	}
	p.unused = nUnused
	if !isLeaf {
		p.rightPg = binary.BigEndian.Uint32(aHdr[8:12])
	}
	return true
}

// statAccumFreeblocks walks the page's freeblock chain adding each block's
// size to nUnused (statDecodePage's iOff loop).
func statAccumFreeblocks(aData []byte, iOff, szPage int, nUnused *int) bool {
	for iOff != 0 {
		if iOff < 0 || iOff+4 > szPage {
			return false
		}
		*nUnused += int(binary.BigEndian.Uint16(aData[iOff+2 : iOff+4]))
		iNext := int(binary.BigEndian.Uint16(aData[iOff : iOff+2]))
		if iNext > 0 && iNext < iOff+4 {
			return false
		}
		iOff = iNext
	}
	return true
}

// statDecodeCells parses every cell of a decoded page (statDecodePage's cell
// loop). ok=false marks the page corrupt with the cells cleared.
func statDecodeCells(p *dbStatPage, nUsable int) bool {
	if p.nCell == 0 {
		return true
	}
	aData := p.data
	hdrOff := 0
	if p.pgno == 1 {
		hdrOff = 100
	}
	aHdr := aData[hdrOff:]
	nHdr := 8
	if !isStatLeafFlags(p.flags) {
		nHdr = 12
	}
	if p.pgno == 1 {
		nHdr += 100
	}
	szPage := len(aData) - DBStatPagePaddingBytes
	p.cells = make([]dbStatCell, 0, p.nCell)
	for i := 0; i < p.nCell; i++ {
		cellOff := int(binary.BigEndian.Uint16(aHdr[nHdr+i*2 : nHdr+i*2+2]))
		if cellOff < nHdr || cellOff >= szPage {
			p.flags = 0
			p.cells = nil
			return false
		}
		cell := dbStatCell{}
		off := cellOff
		if !isStatLeafFlags(p.flags) {
			cell.childPg = binary.BigEndian.Uint32(aData[off : off+4])
			off += 4
		}
		if p.flags != 0x05 && !statDecodeCellPayload(&cell, p, aData, off, nUsable) {
			p.flags = 0
			p.cells = nil
			return false
		}
		p.cells = append(p.cells, cell)
	}
	return true
}

func isStatLeafFlags(flags byte) bool { return flags == 0x0A || flags == 0x0D }

// statDecodeCellPayload parses one cell's payload fields (local size, overflow
// chain head) into cell. ok=false marks the page corrupt.
func statDecodeCellPayload(cell *dbStatCell, p *dbStatPage, aData []byte, off, nUsable int) bool {
	nPayload, n := statVarint32(aData[off:])
	off += n
	if p.flags == 0x0D {
		_, n2 := statVarint32(aData[off:])
		off += n2
	}
	if int(nPayload) > p.mxPayload {
		p.mxPayload = int(nPayload)
	}
	nLocal := statGetLocalPayload(nUsable, p.flags, int(nPayload))
	cell.nLocal = nLocal
	if int(nPayload) <= nLocal {
		return true
	}
	nOvfl := (int(nPayload) - nLocal + nUsable - 4 - 1) / (nUsable - 4)
	if off+nLocal+4 > nUsable || nPayload > 0x7fffffff {
		return false
	}
	cell.lastOvfl = int(nPayload) - nLocal - (nOvfl-1)*(nUsable-4)
	cell.nOvfl = nOvfl
	// First overflow pointer sits after the local payload; the chain is
	// followed lazily (statReadOvflChain).
	cell.ovfl = []uint32{binary.BigEndian.Uint32(aData[off+nLocal : off+nLocal+4])}
	return true
}

// statVarint32 decodes a 1-4 byte big-endian 7-bit varint (the u32 form
// getVarint32 produces for the payload lengths dbstat observes).
func statVarint32(b []byte) (uint32, int) {
	if len(b) == 0 {
		return 0, 0
	}
	if b[0] <= 0x7f {
		return uint32(b[0]), 1
	}
	if len(b) >= 2 && b[1] <= 0x7f {
		return uint32(b[0]&0x7f)<<7 | uint32(b[1]), 2
	}
	if len(b) >= 3 && b[2] <= 0x7f {
		return uint32(b[0]&0x7f)<<14 | uint32(b[1]&0x7f)<<7 | uint32(b[2]), 3
	}
	if len(b) >= 4 {
		return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 |
			uint32(b[2]&0x7f)<<7 | uint32(b[3]), 4
	}
	return 0, len(b)
}

// statReadOvflChain follows one cell's overflow chain to its full declared
// length (statDecodePage's PagerGet loop): each page's first 4 bytes point to
// the next. Returns the pointers read before any failure.
func statReadOvflChain(src PageSource, first uint32, nOvfl int) []uint32 {
	chain := []uint32{first}
	for len(chain) < nOvfl {
		data, err := src.ReadPage(chain[len(chain)-1])
		if err != nil || len(data) < 4 {
			break
		}
		next := binary.BigEndian.Uint32(data[0:4])
		if next == 0 {
			break
		}
		chain = append(chain, next)
	}
	return chain
}

// dbStatBtreeAccum accumulates one b-tree's aggregate row (statNext's
// isAgg counters: pageno=nPage, ncell/nPayload/nUnused summed, pgsize=sum).
type dbStatBtreeAccum struct {
	nPage     int64
	nCell     int64
	nPayload  int64
	nUnused   int64
	mxPayload int64
	szPage    int64
}

// walkAll materializes every dbstat row for one schema (statFilter+statNext).
func (m *DBStatModule) walkAll(src PageSource, schema string, aggregate bool) ([][]interface{}, []int64, error) {
	// statFilter's b-tree set: the schema btree itself is seeded first
	// (it is not listed in sqlite_schema), then every rootpage!=0 entry.
	entries, err := m.db.ExecSQL(fmt.Sprintf(
		"SELECT name, rootpage FROM (SELECT 'sqlite_schema' AS name, 1 AS rootpage"+
			" UNION ALL SELECT name, rootpage FROM \"%s\".sqlite_schema WHERE rootpage!=0)",
		schema))
	if err != nil {
		return nil, nil, err
	}
	page1, err := src.ReadPage(1)
	if err != nil {
		return nil, nil, err
	}
	nUsable := statUsableBytes(src, page1)
	pageSize := int(src.PageSize())
	var rows [][]interface{}
	var rowids []int64
	for _, e := range entries {
		name, _ := e[0].(string)
		root, _ := e[1].(int64)
		if root <= 0 {
			continue
		}
		acc := &dbStatBtreeAccum{}
		aborted := m.walkBtree(src, schema, name, uint32(root), nUsable, pageSize, aggregate, acc, &rows, &rowids)
		if aborted {
			break
		}
		if aggregate {
			row := make([]interface{}, 12)
			row[0] = name
			row[1] = nil // path NULL for aggregates
			row[2] = acc.nPage
			row[3] = nil // pagetype NULL for aggregates
			row[4] = acc.nCell
			row[5] = acc.nPayload
			row[6] = acc.nUnused
			row[7] = acc.mxPayload
			row[8] = nil // pgoffset NULL for aggregates
			row[9] = acc.szPage
			row[10] = schema
			row[11] = int64(1)
			rows = append(rows, row)
			rowids = append(rowids, acc.nPage)
		}
	}
	return rows, rowids, nil
}

// walkBtree walks one b-tree depth-first in statNext's order: for each cell,
// its overflow rows appear first, then that cell's child page (full subtree),
// and the right-most child descends after the last cell. Overflow paths
// ("/1c2/000+") sort before their child ("/1c2/") under BINARY, matching the
// C's documented sort property. Returns true when a page read failed and the
// whole scan must end (C's PagerGet error propagation from statNext).
func (m *DBStatModule) walkBtree(src PageSource, schema, name string, root uint32, nUsable, pageSize int, aggregate bool, acc *dbStatBtreeAccum, rows *[][]interface{}, rowids *[]int64) bool {
	p, err := statGetPage(src, root)
	if err != nil {
		return true
	}
	acc.nPage++
	acc.szPage += int64(pageSize)
	if p.path == "" {
		p.path = "/" // the b-tree root (children arrive pre-pathed)
	}
	statDecodePage(p, nUsable)
	m.accumulatePage(acc, p)
	if !aggregate {
		m.emitPageRow(rows, rowids, schema, name, p, statPagetypeOf(p.flags), pageSize)
	}
	if p.flags == 0 {
		return false
	}
	// Overflow rows + subtree descent, cell by cell (statNext's loop order:
	// each cell's overflow rows, then that cell's child subtree).
	ctx := &dbStatWalk{m: m, src: src, schema: schema, name: name, nUsable: nUsable,
		pageSize: pageSize, aggregate: aggregate, acc: acc, rows: rows, rowids: rowids}
	return ctx.walkCells(p)
}

// statPagetypeOf names a decoded page's type ("corrupted" for flags 0).
func statPagetypeOf(flags byte) string {
	switch flags {
	case 0x05, 0x02:
		return "internal"
	case 0x0A, 0x0D:
		return "leaf"
	}
	return "corrupted"
}

// walkCells emits each cell's overflow rows and descends into its child
// subtree, then the right-most child (statNext's loop order). aborted=true
// ends the scan.
func (w *dbStatWalk) walkCells(p *dbStatPage) bool {
	for i := range p.cells {
		cell := &p.cells[i]
		if len(cell.ovfl) > 0 {
			w.emitCellOverflowRows(p, i, cell)
		}
		if cell.childPg != 0 && w.descend(p.path, i, cell.childPg) {
			return true
		}
	}
	if p.rightPg != 0 {
		return w.descend(p.path, p.nCell, p.rightPg)
	}
	return false
}

// dbStatWalk carries the shared per-scan state of the recursive b-tree walk.
type dbStatWalk struct {
	m         *DBStatModule
	src       PageSource
	schema    string
	name      string
	nUsable   int
	pageSize  int
	aggregate bool
	acc       *dbStatBtreeAccum
	rows      *[][]interface{}
	rowids    *[]int64
}

// emitCellOverflowRows emits (and accumulates) one cell's overflow chain rows.
func (w *dbStatWalk) emitCellOverflowRows(p *dbStatPage, cellIdx int, cell *dbStatCell) {
	chain := statReadOvflChain(w.src, cell.ovfl[0], cell.nOvfl)
	for j, opg := range chain {
		last := j == len(chain)-1
		w.acc.nPage++
		w.acc.szPage += int64(w.pageSize)
		if last {
			w.acc.nPayload += int64(cell.lastOvfl)
			w.acc.nUnused += int64(w.nUsable - 4 - cell.lastOvfl)
		} else {
			w.acc.nPayload += int64(w.nUsable - 4)
		}
		if !w.aggregate {
			w.m.emitOverflowRow(w.rows, w.rowids, w.schema, w.name, p,
				cellIdx, j, opg, cell.lastOvfl, last, w.nUsable, w.pageSize)
		}
	}
}

// descend loads one child page and recurses. aborted=true ends the scan.
func (w *dbStatWalk) descend(parentPath string, cellIdx int, pgno uint32) bool {
	child, err := statGetPage(w.src, pgno)
	if err != nil {
		return true
	}
	child.path = fmt.Sprintf("%s%.3x/", parentPath, cellIdx)
	return w.m.walkBtree(w.src, w.schema, w.name, pgno, w.nUsable, w.pageSize, w.aggregate, w.acc, w.rows, w.rowids)
}

// accumulatePage folds one decoded page's counters into the b-tree aggregate
// (statNext's per-page accumulation; local payload only — overflow pages
// contribute through their own rows).
func (m *DBStatModule) accumulatePage(acc *dbStatBtreeAccum, p *dbStatPage) {
	acc.nCell += int64(p.nCell)
	acc.nUnused += int64(p.unused)
	if int64(p.mxPayload) > acc.mxPayload {
		acc.mxPayload = int64(p.mxPayload)
	}
	for i := range p.cells {
		acc.nPayload += int64(p.cells[i].nLocal)
	}
}

// emitPageRow appends one page row (statColumn's non-aggregate values).
func (m *DBStatModule) emitPageRow(rows *[][]interface{}, rowids *[]int64, schema, name string, p *dbStatPage, pagetype string, pageSize int) {
	nPayload := int64(0)
	for i := range p.cells {
		nPayload += int64(p.cells[i].nLocal)
	}
	row := make([]interface{}, 12)
	row[0] = name
	row[1] = p.path
	row[2] = int64(p.pgno)
	row[3] = pagetype
	row[4] = int64(p.nCell)
	row[5] = nPayload
	row[6] = int64(p.unused)
	row[7] = int64(p.mxPayload)
	row[8] = int64(pageSize) * (int64(p.pgno) - 1)
	row[9] = int64(pageSize)
	row[10] = schema
	row[11] = int64(0)
	*rows = append(*rows, row)
	*rowids = append(*rowids, int64(p.pgno))
}

// emitOverflowRow appends one overflow-page row ('<cellpath>%.3x+%.6x' path;
// payload usable-4 except the chain's final page, which carries the remainder
// plus the leftover unused bytes).
func (m *DBStatModule) emitOverflowRow(rows *[][]interface{}, rowids *[]int64, schema, name string, p *dbStatPage, cellIdx, ovflIdx int, opg uint32, lastOvfl int, last bool, nUsable, pageSize int) {
	row := make([]interface{}, 12)
	row[0] = name
	row[1] = fmt.Sprintf("%s%.3x+%.6x", p.path, cellIdx, ovflIdx)
	row[2] = int64(opg)
	row[3] = "overflow"
	row[4] = int64(0)
	if last {
		row[5] = int64(lastOvfl)
		row[6] = int64(nUsable - 4 - lastOvfl)
	} else {
		row[5] = int64(nUsable - 4)
		row[6] = int64(0)
	}
	row[7] = int64(0)
	row[8] = int64(pageSize) * (int64(opg) - 1)
	row[9] = int64(pageSize)
	row[10] = schema
	row[11] = int64(0)
	*rows = append(*rows, row)
	*rowids = append(*rowids, int64(opg))
}
