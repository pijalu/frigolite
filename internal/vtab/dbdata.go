package vtab

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unicode/utf16"
)

// DBDataModule implements the sqlite_dbdata and sqlite_dbptr eponymous-only
// virtual tables (src/ext/recover/dbdata.c, sqlite3DbdataRegister registers
// both names over one module distinguished by its pAux flag, ported here as
// the bPtr field). sqlite_dbdata extracts data directly from database b-tree
// pages and their overflow chains, bypassing the b-tree layer: one row per
// record field of each cell, plus a field=-1 row carrying the rowid key of
// int-key leaf cells. sqlite_dbptr reports one row per parent/child b-tree
// pointer. Both read raw pages through a PageSourceProvider (the C fetches
// them with "SELECT data FROM sqlite_dbpage(?) WHERE pgno=?") and resolve the
// scanned schema from the HIDDEN schema column (function argument or WHERE
// binding). A schema argument ending in "()" names a SQL function supplying
// page images instead (dbdata.c dbdataIsFunction, used by the recover
// extension) — that form resolves pages through the Database interface.
//
// Like the C module, dbdata never reports corruption errors: it extracts as
// much data as possible and skips what it cannot decode. Only schema/page
// source resolution failures (unknown database, missing page function) are
// errors. The module is eponymous-only: xCreate is NULL, so CREATE VIRTUAL
// TABLE ... USING sqlite_dbdata fails with "no such module" while the names
// are directly usable in a FROM clause.
type DBDataModule struct {
	provider PageSourceProvider
	db       Database
	bPtr     bool // true for sqlite_dbptr (the C pAux!=0 form)
}

// NewDBDataModule builds the sqlite_dbdata module over a schema resolver and
// the connection handle (the latter only serves the "func()" page-source form).
func NewDBDataModule(provider PageSourceProvider, db Database) *DBDataModule {
	return &DBDataModule{provider: provider, db: db}
}

// NewDBPtrModule builds the sqlite_dbptr module (dbdata.c registers the same
// module twice; the flag selects the (pgno, child, schema) schema).
func NewDBPtrModule(provider PageSourceProvider, db Database) *DBDataModule {
	return &DBDataModule{provider: provider, db: db, bPtr: true}
}

// EponymousOnly implements EponymousOnlyModule: dbdata.c's sqlite3_module has
// xCreate==0, so CREATE VIRTUAL TABLE fails and the names are FROM-usable.
func (m *DBDataModule) EponymousOnly() bool { return true }

// Create implements Module (xCreate is NULL in the C; the engine rejects
// CREATE VIRTUAL TABLE for EponymousOnly modules before reaching this).
func (m *DBDataModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module (xConnect).
func (m *DBDataModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

func (m *DBDataModule) connect(args []string) (VirtualTable, error) {
	v := &dbdataVTab{mod: m}
	if len(args) > 0 && args[0] != "" {
		// Hidden-column argument form: FROM sqlite_dbdata('main').
		v.schema = args[0]
		v.schemaSeen = true
	}
	return v, nil
}

// Column indexes of the two declared schemas (dbdata.c DBDATA_COLUMN_* /
// DBPTR_COLUMN_*).
const (
	dbdataColumnPGNO   = 0
	dbdataColumnCELL   = 1
	dbdataColumnFIELD  = 2
	dbdataColumnVALUE  = 3
	dbdataColumnSCHEMA = 4

	dbptrColumnPGNO   = 0
	dbptrColumnCHILD  = 1
	dbptrColumnSCHEMA = 2
)

const (
	// dbdataPaddingBytes ports DBDATA_PADDING_BYTES: every page and record
	// buffer carries 100 zero bytes past its logical end so varint/field
	// reads near a corrupt buffer's end behave exactly as in the C.
	dbdataPaddingBytes = 100
	// dbdataMxField ports DBDATA_MX_FIELD, the hard record-field limit
	// (verbatim from dbdata.c, including its 32676 spelling).
	dbdataMxField = 32676
)

// dbdataMxCell ports DBDATA_MX_CELL(pgsz): the maximum number of cells a
// page of the given size may hold (MX_CELL in the SQLite core).
func dbdataMxCell(pgsz int) int { return (pgsz - 8) / 6 }

// dbdataVTab is one instance. The schema is resolved per scan (xFilter), not
// at connect time, so the same instance serves any database.
type dbdataVTab struct {
	mod        *DBDataModule
	schema     string // hidden-column binding (argument or WHERE schema=?)
	schemaSeen bool
}

// Columns declares the schema (dbdata.c DBDATA_SCHEMA / DBPTR_SCHEMA).
func (v *dbdataVTab) Columns() []string {
	if v.mod.bPtr {
		return []string{"pgno", "child", "schema"}
	}
	return []string{"pgno", "cell", "field", "value", "schema"}
}

// HiddenColumns reports the HIDDEN schema column index.
func (v *dbdataVTab) HiddenColumns() map[int]bool {
	if v.mod.bPtr {
		return map[int]bool{dbptrColumnSCHEMA: true}
	}
	return map[int]bool{dbdataColumnSCHEMA: true}
}

// BestIndex accepts the default plan: the pgno= constraint of the C's
// xBestIndex has no pushdown channel in this engine (pgno is not a HIDDEN
// column), so scans are full and WHERE filtering is applied by the core —
// the produced row set is identical.
func (v *dbdataVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// SetHiddenConstraint implements HiddenConstraintSetter: a WHERE equality on
// the HIDDEN schema column names the scanned database (dbdata.c xFilter's
// idxNum 0x01 argv[0]).
func (v *dbdataVTab) SetHiddenConstraint(col string, val interface{}) error {
	if !strings.EqualFold(col, "schema") {
		return fmt.Errorf("no such column: %s", col)
	}
	switch t := val.(type) {
	case nil:
		v.schema = ""
	case string:
		v.schema = t
	case []byte:
		v.schema = string(t)
	default:
		v.schema = fmt.Sprintf("%v", val)
	}
	v.schemaSeen = true
	return nil
}

// Open implements VirtualTable: it ports dbdataFilter — resolve the page
// source (or page function), size the database, sniff the text encoding from
// page 1 and position on the first row.
func (v *dbdataVTab) Open() (Cursor, error) {
	c := &dbdataCursor{vtab: v}
	if err := c.filter(); err != nil {
		return nil, err
	}
	return c, nil
}

// dbdataCursor ports DbdataCursor. Pointer arithmetic of the C (pHdrPtr/pPtr
// into the record buffer) is carried as offsets; buffers keep the C's 100
// bytes of zero padding so near-end reads of corrupt data see zeros.
type dbdataCursor struct {
	vtab *dbdataVTab

	schemaFn string     // page-supplying SQL function for the "fn()" form
	src      PageSource // page source for the schema-name form
	szDb     int64      // page count (dbdataDbsize)
	nPage    int        // size of aPage in bytes (U in the C's payload math)
	enc      uint32     // database text encoding (dbdataGetEncoding)

	iPgno  int64  // current page number
	aPage  []byte // current page image incl. padding
	nCell  int    // number of cells on aPage (capped at MX_CELL)
	iCell  int    // current cell number (-1 = right-most pointer for dbptr)
	iRowid int64  // monotonic rowid (never reset by dbdataResetCursor)

	rec     []byte // record buffer incl. padding (DbdataBuffer)
	nRec    int    // logical record size
	nHdr    int    // record header size
	iField  int64  // current field number (-1 = intkey row)
	pHdr    int    // offset of the current field's type varint in rec
	pPtr    int    // offset of the current field's data in rec
	iIntkey int64  // integer key of the current leaf intkey cell

	started bool
	done    bool
}

// filter ports dbdataFilter: reset, resolve the schema to a page source or
// page function, size the database, read the encoding and step to row 1.
func (c *dbdataCursor) filter() error {
	c.reset()
	schema := "main"
	if c.vtab.schemaSeen {
		schema = c.vtab.schema
	}
	if err := c.resolvePageSource(schema); err != nil {
		return err
	}
	// dbdataGetEncoding: the text encoding is sniffed from page 1's header.
	aPg1, err := c.loadPage(1)
	if err != nil {
		return err
	}
	if len(aPg1) >= 60 {
		c.enc = binary.BigEndian.Uint32(aPg1[56:60])
	}
	return c.next()
}

// resolvePageSource ports dbdataDbsize plus dbdataFilter's statement setup:
// a schema ending in "()" names a SQL function supplying page images (its
// fn(0) result sizes the database); otherwise the schema must name a database
// of the connection, whose page count sizes the scan.
func (c *dbdataCursor) resolvePageSource(schema string) error {
	if nFunc := dbdataIsFunction(schema); nFunc > 0 {
		return c.resolvePageFunction(schema[:nFunc])
	}
	if c.vtab.mod.provider == nil {
		return fmt.Errorf("sqlite_dbdata: no database context")
	}
	src, ok := c.vtab.mod.provider.PageSourceFor(schema)
	if !ok {
		return fmt.Errorf("unknown database %s", schema)
	}
	c.src = src
	c.szDb = int64(src.PageCount())
	return nil
}

// resolvePageFunction binds the page-supplying SQL function and sizes the
// database from its fn(0) result (dbdata.c dbdataDbsize's function form).
func (c *dbdataCursor) resolvePageFunction(fn string) error {
	if c.vtab.mod.db == nil {
		return fmt.Errorf("no such database function: %s", fn)
	}
	rows, err := c.vtab.mod.db.ExecSQL(fmt.Sprintf("SELECT %s(0)", fn))
	if err != nil {
		return err
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		if n, ok := asInt64(rows[0][0]); ok {
			c.szDb = n
		}
	}
	c.schemaFn = fn
	return nil
}

// reset ports dbdataResetCursor (which leaves iRowid untouched).
func (c *dbdataCursor) reset() {
	c.iPgno = 1
	c.iCell = 0
	c.iField = 0
	c.aPage = nil
	c.nRec = 0
	c.rec = nil
}

// loadPage ports dbdataLoadPage: the raw bytes of pgno with the C's zero
// padding, or nil when the page does not exist (pgno<1 or beyond EOF —
// stepping the C's dbpage statement simply yields no row). A page-supplying
// function, when bound, replaces the page source.
func (c *dbdataCursor) loadPage(pgno int64) ([]byte, error) {
	if pgno <= 0 {
		return nil, nil
	}
	var raw []byte
	var err error
	if c.schemaFn != "" {
		raw, err = c.loadFunctionPage(pgno)
	} else {
		raw, err = c.loadSourcePage(pgno)
	}
	if err != nil || raw == nil {
		return nil, err
	}
	buf := make([]byte, len(raw)+dbdataPaddingBytes)
	copy(buf, raw)
	return buf, nil
}

// loadFunctionPage fetches one page image through the bound page function
// (dbdata.c's "SELECT %.*s(?2)" statement); no row or a non-blob result
// means the page does not exist.
func (c *dbdataCursor) loadFunctionPage(pgno int64) ([]byte, error) {
	rows, err := c.vtab.mod.db.ExecSQL(fmt.Sprintf("SELECT %s(%d)", c.schemaFn, pgno))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil, nil
	}
	switch b := rows[0][0].(type) {
	case []byte:
		return b, nil
	case string:
		return []byte(b), nil
	default:
		return nil, nil
	}
}

// loadSourcePage fetches one page image from the resolved database; pages
// beyond EOF yield no page (the C's dbpage statement steps to no row).
func (c *dbdataCursor) loadSourcePage(pgno int64) ([]byte, error) {
	if c.src == nil || uint64(pgno) > uint64(c.src.PageCount()) {
		return nil, nil
	}
	return c.src.ReadPage(uint32(pgno))
}

// next ports dbdataNext: advance to the next row (the first call positions).
// On a mid-scan page-read failure the scan ends (the cursor's Next() API
// cannot abort the statement); such failures are not reachable for in-memory
// pagers.
func (c *dbdataCursor) next() error {
	c.iRowid++
	for {
		again, err := c.step()
		if err != nil {
			return err
		}
		if !again {
			return nil
		}
	}
}

// step performs one dbdataNext loop iteration: acquiring the next page when
// none is loaded, then advancing the cell/field state. again reports that
// another iteration is needed; false means a row is ready (or the scan hit
// EOF, with done set).
func (c *dbdataCursor) step() (bool, error) {
	iOff := c.pageHeaderOffset()
	if c.aPage == nil {
		done, err := c.beginPage(iOff)
		if done || err != nil {
			c.done = done
			return false, err
		}
	}
	if c.vtab.mod.bPtr {
		return c.nextDbptrCell(iOff), nil
	}
	bNextPage, err := c.nextDbdataRow(iOff)
	if err != nil {
		return false, err
	}
	return c.resumeOrAdvance(bNextPage), nil
}

// resumeOrAdvance finishes one dbdata step (dbdataNext's tail): an exhausted
// page or record moves the scan on (true to continue), a decodable row
// reports false so the row is served.
func (c *dbdataCursor) resumeOrAdvance(bNextPage bool) bool {
	if bNextPage {
		c.aPage = nil
		c.nRec = 0
		c.iPgno++
		return true
	}
	if c.iField < 0 || c.pHdr < c.nHdr {
		return false
	}
	// No more fields in this record: advance to the next cell (the next
	// iteration loads its record).
	c.nRec = 0
	c.iCell++
	return true
}

// pageHeaderOffset reports the b-tree header offset on the current page
// (page 1 carries the 100-byte database header first).
func (c *dbdataCursor) pageHeaderOffset() int {
	if c.iPgno == 1 {
		return 100
	}
	return 0
}

// beginPage loads the page iPgno into the cursor and reads its cell count
// (dbdataNext's page-load loop, including the nPage>=256 usability floor and
// the MX_CELL cap). done reports that the scan is exhausted.
func (c *dbdataCursor) beginPage(iOff int) (bool, error) {
	for {
		if c.iPgno > c.szDb {
			return true, nil
		}
		pg, err := c.loadPage(c.iPgno)
		if err != nil {
			return false, err
		}
		if len(pg) >= 256 {
			c.aPage = pg
			c.nPage = len(pg) - dbdataPaddingBytes
			break
		}
		c.iPgno++
	}
	if c.vtab.mod.bPtr {
		c.iCell = -2
	} else {
		c.iCell = 0
	}
	c.nCell = int(binary.BigEndian.Uint16(c.aPage[iOff+3 : iOff+5]))
	if c.nCell > dbdataMxCell(c.nPage) {
		c.nCell = dbdataMxCell(c.nPage)
	}
	return false, nil
}

// nextDbptrCell advances the dbptr cursor within the current page: the first
// row of a 0x02/0x05 page is the header's right-most child pointer (iCell=-1),
// then one row per cell. exhausted reports that the page ran out and the
// scan moved to the next page number.
func (c *dbdataCursor) nextDbptrCell(iOff int) bool {
	if c.aPage[iOff] != 0x02 && c.aPage[iOff] != 0x05 {
		c.iCell = c.nCell
	}
	c.iCell++
	if c.iCell < c.nCell {
		return false
	}
	c.aPage = nil
	c.iPgno++
	return true
}

// nextDbdataRow produces the next dbdata row: loading the current cell's
// record when none is loaded, or advancing within it field by field.
// bNextPage reports that the page is exhausted.
func (c *dbdataCursor) nextDbdataRow(iOff int) (bool, error) {
	if c.nRec == 0 {
		return c.loadRecord(iOff)
	}
	c.iField++
	if c.iField > 0 && c.advanceField() {
		return true, nil
	}
	return false, nil
}

// loadRecord ports the record-decode block of dbdataNext (the pCsr->nRec==0
// branch): interpret the current cell, assemble its payload (following
// overflow pages) and walk to field 0 (or -1 for intkey leaves). bNextPage
// reports that the cell/page carries no decodable record.
func (c *dbdataCursor) loadRecord(iOff int) (bool, error) {
	bHasRowid, nPointer, ok := c.cellKind(iOff)
	if !ok {
		return true, nil
	}
	iOff, ok = c.cellPayloadOffset(iOff, nPointer)
	if !ok {
		return true, nil
	}
	nPayload, iOff := c.cellPayloadSize(iOff)
	// If this is a leaf intkey cell, load the rowid.
	if bHasRowid && iOff < c.nPage {
		w := dbdataGetVarint(c.aPage[iOff:], &c.iIntkey)
		iOff += w
	}
	nLocal := dbdataLocalPayload(c.nPage, nPayload, bHasRowid)
	if nLocal+iOff > c.nPage {
		return true, nil
	}
	if err := c.assemblePayload(iOff, nLocal, nPayload); err != nil {
		return false, err
	}
	c.beginRecord(nPayload, bHasRowid)
	return false, nil
}

// cellKind interprets the current page's b-tree type byte (dbdataNext's
// switch): interior index cells carry a 4-byte child pointer, leaf table
// cells carry a rowid. ok is false when the page is not a record-bearing
// b-tree page or its cells ran out.
func (c *dbdataCursor) cellKind(iOff int) (bHasRowid bool, nPointer int, ok bool) {
	switch c.aPage[iOff] {
	case 0x02: // interior index: cell = child pgno + record
		nPointer = 4
	case 0x0a: // leaf index: cell = record
	case 0x0d: // leaf table: cell = rowid varint + record
		bHasRowid = true
	default:
		// Not a b-tree page with records on it (e.g. interior table page,
		// freelist, overflow, bitmap): skip every cell of the page.
		c.iCell = c.nCell
		return false, 0, false
	}
	if c.iCell >= c.nCell {
		return false, 0, false
	}
	return bHasRowid, nPointer, true
}

// cellPayloadOffset resolves the current cell's payload start: the cell
// pointer, past the child-page number on interior index cells, validated
// against the cell pointer array (dbdataNext's iOff<=iCellPtr corruption
// guard).
func (c *dbdataCursor) cellPayloadOffset(iOff, nPointer int) (int, bool) {
	iCellPtr := iOff + 8 + nPointer + c.iCell*2
	if iCellPtr > c.nPage {
		return 0, false
	}
	iOff = int(binary.BigEndian.Uint16(c.aPage[iCellPtr:iCellPtr+2])) + nPointer
	if iOff > c.nPage || iOff <= iCellPtr {
		return 0, false
	}
	return iOff, true
}

// cellPayloadSize reads the "byte of payload including overflow" varint at
// iOff and applies dbdata.c's corruption clamps: sizes beyond 0x7fffff00 are
// masked to 14 bits and a zero size degenerates to 1. Returns the size and
// the offset just past the varint.
func (c *dbdataCursor) cellPayloadSize(iOff int) (int64, int) {
	var nPayload int64
	w := dbdataGetVarintU32(c.aPage[iOff:], &nPayload)
	iOff += w
	if nPayload > 0x7fffff00 {
		nPayload &= 0x3fff
	}
	if nPayload == 0 {
		nPayload = 1
	}
	return nPayload, iOff
}

// assemblePayload allocates the record buffer (with the C's zero padding),
// copies the local payload and follows the overflow chain. Bytes the chain
// fails to supply are simply absent: the record shrinks to what was loaded
// (dbdata.c: nPayload -= nRem).
func (c *dbdataCursor) assemblePayload(iOff, nLocal int, nPayload int64) error {
	c.rec = make([]byte, int(nPayload)+dbdataPaddingBytes)
	copy(c.rec, c.aPage[iOff:iOff+nLocal])
	if nPayload <= int64(nLocal) {
		return nil
	}
	nRem := nPayload - int64(nLocal)
	pgnoOvfl := int64(binary.BigEndian.Uint32(c.aPage[iOff+nLocal : iOff+nLocal+4]))
	for nRem > 0 {
		aOvfl, err := c.loadPage(pgnoOvfl)
		if err != nil {
			return err
		}
		if aOvfl == nil {
			break
		}
		nCopy := c.overflowCopy(aOvfl, nRem, int(nPayload)-int(nRem))
		nRem -= int64(nCopy)
		pgnoOvfl = int64(binary.BigEndian.Uint32(aOvfl[0:4]))
	}
	return nil
}

// overflowCopy copies one overflow page's contribution (its first 4 bytes
// hold the next-chain pointer) into rec[at:].
func (c *dbdataCursor) overflowCopy(aOvfl []byte, nRem int64, at int) int {
	nCopy := int64(c.nPage) - 4
	if nCopy > nRem {
		nCopy = nRem
	}
	if room := int64(len(aOvfl)) - 4; nCopy > room {
		nCopy = room
	}
	copy(c.rec[at:], aOvfl[4:4+nCopy])
	return int(nCopy)
}

// beginRecord parses the record header varint and positions the field walk:
// pHdr at the first field's serial type, pPtr at its data (dbdataNext's
// iHdr/nHdr/pHdrPtr/pPtr setup).
func (c *dbdataCursor) beginRecord(nPayload int64, bHasRowid bool) {
	var nHdr int64
	iHdr := dbdataGetVarintU32(c.rec, &nHdr)
	if nHdr > nPayload {
		nHdr = 0
	}
	c.nHdr = int(nHdr)
	c.pHdr = iHdr
	c.pPtr = int(nHdr)
	c.nRec = int(nPayload)
	if bHasRowid {
		c.iField = -1
	} else {
		c.iField = 0
	}
}

// dbdataLocalPayload computes how many payload bytes are stored locally on
// the page (dbdataNext's U/X/M/K arithmetic; U is the page size).
func dbdataLocalPayload(u int, nPayload int64, bHasRowid bool) int {
	var x int
	if bHasRowid {
		x = u - 35
	} else {
		x = ((u-12)*64)/255 - 23
	}
	if nPayload <= int64(x) {
		return int(nPayload)
	}
	m := ((u-12)*32)/255 - 23
	k := int64(m) + ((nPayload - int64(m)) % int64(u-4))
	if k <= int64(x) {
		return int(k)
	}
	return m
}

// advanceField ports dbdataNext's field-walk block (nRec!=0): step the header
// pointer to the next field's type varint and the data pointer past the
// current field's bytes. bNextPage reports record exhaustion.
func (c *dbdataCursor) advanceField() bool {
	if c.pHdr >= c.nRec || int(c.iField) >= dbdataMxField {
		return true
	}
	var iType int64
	w := dbdataGetVarintU32(c.rec[c.pHdr:], &iType)
	c.pHdr += w
	szField := dbdataValueBytes(iType)
	if c.nRec-c.pPtr < szField {
		c.pPtr = c.nRec
	} else {
		c.pPtr += szField
	}
	return false
}

// Column ports dbdataColumn.
func (c *dbdataCursor) Column(idx int) (interface{}, error) {
	if c.vtab.mod.bPtr {
		return c.dbptrColumn(idx)
	}
	return c.dbdataColumn(idx)
}

// dbptrColumn serves pgno and child (dbdataColumn's bPtr branch).
func (c *dbdataCursor) dbptrColumn(idx int) (interface{}, error) {
	switch idx {
	case dbptrColumnPGNO:
		return c.iPgno, nil
	case dbptrColumnCHILD:
		return c.dbptrChild(), nil
	}
	return nil, fmt.Errorf("sqlite_dbptr: invalid column index %d", idx)
}

// dbptrChild reads the child page number of the current dbptr row: iCell=-1
// addresses the page header's right-most child pointer, iCell>=0 the cell
// pointer array (dbdataColumn's DBPTR_COLUMN_CHILD case). An offset beyond
// the page reads NULL.
func (c *dbdataCursor) dbptrChild() interface{} {
	iOff := c.pageHeaderOffset()
	if c.iCell < 0 {
		iOff += 8
	} else {
		iOff += 12 + c.iCell*2
		if iOff > c.nPage {
			return nil
		}
		iOff = int(binary.BigEndian.Uint16(c.aPage[iOff : iOff+2]))
	}
	if iOff > c.nPage {
		return nil
	}
	return int64(binary.BigEndian.Uint32(c.aPage[iOff : iOff+4]))
}

// dbdataColumn serves pgno, cell, field, value and schema (dbdataColumn's
// non-bPtr branch).
func (c *dbdataCursor) dbdataColumn(idx int) (interface{}, error) {
	switch idx {
	case dbdataColumnPGNO:
		return c.iPgno, nil
	case dbdataColumnCELL:
		return int64(c.iCell), nil
	case dbdataColumnFIELD:
		return c.iField, nil
	case dbdataColumnVALUE:
		return c.dbdataValueColumn(), nil
	case dbdataColumnSCHEMA:
		// dbdataColumn has no case for the schema column: it reads NULL even
		// when a schema argument selected the database.
		return nil, nil
	}
	return nil, fmt.Errorf("sqlite_dbdata: invalid column index %d", idx)
}

// dbdataValueColumn serves the value column: the intkey for field=-1 rows,
// the decoded current record field otherwise, NULL past the record body
// (dbdataColumn's DBDATA_COLUMN_VALUE case).
func (c *dbdataCursor) dbdataValueColumn() interface{} {
	if c.iField < 0 {
		return c.iIntkey
	}
	if int64(c.pPtr) > int64(c.nRec) {
		return nil
	}
	var iType int64
	dbdataGetVarintU32(c.rec[c.pHdr:], &iType)
	return dbdataValue(c.enc, iType, c.rec[c.pPtr:], c.nRec-c.pPtr)
}

// Rowid implements RowidCursor (dbdataRowid: a private monotonic counter).
func (c *dbdataCursor) Rowid() int64 { return c.iRowid }

// Next implements Cursor: the first call reports the row positioned by Open,
// later calls advance (xFilter/xNext split of the materializer protocol).
func (c *dbdataCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return true
	}
	if err := c.next(); err != nil {
		c.done = true
		return false
	}
	return !c.done
}

// Close implements Cursor.
func (c *dbdataCursor) Close() error { return nil }

// dbdataIsFunction ports dbdataIsFunction: a schema value ending in "()"
// names a SQL function that supplies page images; the return value is the
// function-name length, or 0 when z is a plain schema name.
func dbdataIsFunction(z string) int {
	if len(z) > 2 && z[len(z)-2] == '(' && z[len(z)-1] == ')' {
		return len(z) - 2
	}
	return 0
}

// dbdataGetVarint ports dbdataGetVarint: decode a varint, reporting its byte
// width and value. Bytes past the buffer's end read as zero (the C relies on
// its buffers' padding for the same effect).
func dbdataGetVarint(z []byte, val *int64) int {
	var u uint64
	for i := 0; i < 8; i++ {
		var b byte
		if i < len(z) {
			b = z[i]
		}
		u = (u << 7) + uint64(b&0x7f)
		if b&0x80 == 0 {
			*val = int64(u)
			return i + 1
		}
	}
	var b byte
	if len(z) > 8 {
		b = z[8]
	}
	u = (u << 8) + uint64(b)
	*val = int64(u)
	return 9
}

// dbdataGetVarintU32 ports dbdataGetVarintU32: like dbdataGetVarint but the
// value saturates to 0 unless it fits in 32 bits (valid for every database
// varint except intkey rowids).
func dbdataGetVarintU32(z []byte, val *int64) int {
	n := dbdataGetVarint(z, val)
	if *val < 0 || *val > 0xFFFFFFFF {
		*val = 0
	}
	return n
}

// dbdataValueBytes ports dbdataValueBytes: the byte size of a record value
// of the given serial type.
func dbdataValueBytes(eType int64) int {
	switch eType {
	case 0, 8, 9, 10, 11:
		return 0
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return 3
	case 4:
		return 4
	case 5:
		return 6
	case 6, 7:
		return 8
	default:
		if eType > 0 {
			return int((eType - 12) / 2)
		}
		return 0
	}
}

// dbdataValue ports dbdataValue: decode one record field of serial type
// eType from data (nData bytes available) into an SQL value. A field whose
// declared size exceeds the available bytes decodes to the type's zero value
// — the C's corruption-tolerant fallback.
func dbdataValue(enc uint32, eType int64, data []byte, nData int) interface{} {
	if eType < 0 {
		return nil
	}
	if dbdataValueBytes(eType) <= nData {
		switch eType {
		case 0, 10, 11:
			return nil
		case 8:
			return int64(0)
		case 9:
			return int64(1)
		case 1, 2, 3, 4, 5, 6, 7:
			return dbdataIntOrFloat(eType, data)
		default:
			n := int((eType - 12) / 2)
			if eType%2 != 0 {
				return dbdataText(enc, data[:n])
			}
			out := make([]byte, n)
			copy(out, data[:n])
			return out
		}
	}
	switch {
	case eType == 7:
		return float64(0)
	case eType < 7:
		return int64(0)
	case eType%2 != 0:
		return ""
	default:
		return []byte{}
	}
}

// dbdataIntOrFloat decodes serial types 1..7 (the C's fallthrough shift
// chain): the first byte sign-extends, then type-specific little groups of
// big-endian bytes follow. Type 7 reinterprets the 8 bytes as an IEEE-754
// double.
func dbdataIntOrFloat(eType int64, data []byte) interface{} {
	v := uint64(int64(int8(data[0])))
	p := data[1:]
	var n2, n1 int
	switch eType {
	case 7, 6:
		n2, n1 = 2, 3
	case 5:
		n2, n1 = 1, 3
	case 4:
		n1 = 3
	case 3:
		n1 = 2
	case 2:
		n1 = 1
	}
	for i := 0; i < n2; i++ {
		v = (v << 16) | uint64(p[0])<<8 | uint64(p[1])
		p = p[2:]
	}
	for i := 0; i < n1; i++ {
		v = (v << 8) | uint64(p[0])
		p = p[1:]
	}
	if eType == 7 {
		return math.Float64frombits(v)
	}
	return int64(v)
}

// dbdataText decodes a text field honoring the database encoding sniffed
// from page 1 (dbdataValue's SQLITE_UTF16BE/LE/default branches).
func dbdataText(enc uint32, b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if enc == 2 || enc == 3 { // 2 = UTF-16LE, 3 = UTF-16BE
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if enc == 3 {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}
