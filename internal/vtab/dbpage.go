package vtab

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/value"
)

// PageSource abstracts raw database-page access for the sqlite_dbpage virtual
// table (src/dbpage.c reads/writes whole pages through the pager).
type PageSource interface {
	// PageCount reports the number of pages in the database file.
	PageCount() uint32
	// PageSize reports the page size in bytes.
	PageSize() uint32
	// ReadPage returns a COPY of the raw bytes of page pgno (1-based).
	ReadPage(pgno uint32) ([]byte, error)
	// WritePage replaces the raw bytes of page pgno; data must be exactly
	// PageSize long.
	WritePage(pgno uint32, data []byte) error
	// TruncatePages drops all pages after n.
	TruncatePages(n uint32) error
}

// PageSourceProvider resolves a schema name ("main", "temp", an ATTACHed
// database name) to that database's page source.
type PageSourceProvider interface {
	// PageSourceFor returns the page source for schema; ok=false when the
	// schema is unknown.
	PageSourceFor(schema string) (src PageSource, ok bool)
	// AllPageSources lists every database in attachment order (main first).
	// The unqualified "FROM sqlite_dbpage" form scans all of them
	// (src/dbpage.c SQLITE_VTAB_USES_ALL_SCHEMAS).
	AllPageSources() []NamedPageSource
}

// NamedPageSource pairs a page source with its schema name.
type NamedPageSource struct {
	Schema string
	Src    PageSource
}

// DBPageModule implements the sqlite_dbpage virtual table: one row per page
// of a database file exposing pgno, raw data BLOB and the HIDDEN schema name.
// It is a full-eponymous module (xCreate == xConnect) so the implicit
// instance accepts a schema argument: FROM sqlite_dbpage('aux1').
type DBPageModule struct {
	provider PageSourceProvider
}

// NewDBPageModule builds the sqlite_dbpage module over a schema resolver.
func NewDBPageModule(provider PageSourceProvider) *DBPageModule {
	return &DBPageModule{provider: provider}
}

// Eponymous implements EponymousModule: src/dbpage.c registers xCreate ==
// xConnect, so the module is usable directly in a FROM clause.
func (m *DBPageModule) Eponymous() bool { return true }

// DirectOnly implements DirectOnlyModule: dbpageConnect configures
// SQLITE_VTAB_DIRECTONLY, forbidding references from trigger bodies and
// views (dbpagefault 3.x).
func (m *DBPageModule) DirectOnly() bool { return true }

// dbpageColumn indexes of the declared schema.
const (
	dbpageColumnPGNO   = 0
	dbpageColumnDATA   = 1
	dbpageColumnSCHEMA = 2
)

// dbpageVTab is one instance bound to one or more schemas' pages: a single
// schema when the module was created with an explicit argument (or a hidden
// schema= binding arrives), every attached database otherwise.
type dbpageVTab struct {
	sources []NamedPageSource
}

// Create implements Module (xCreate): CREATE VIRTUAL TABLE ... USING
// sqlite_dbpage(<schema>).
func (m *DBPageModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module (xConnect).
func (m *DBPageModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

func (m *DBPageModule) connect(args []string) (VirtualTable, error) {
	schema := ""
	if len(args) > 0 && args[0] != "" {
		schema = args[0]
	}
	if m.provider == nil {
		return nil, fmt.Errorf("sqlite_dbpage: no database context")
	}
	v := &dbpageVTab{}
	if schema != "" {
		src, ok := m.provider.PageSourceFor(schema)
		if !ok {
			return nil, fmt.Errorf("no such schema: %s", schema)
		}
		v.sources = []NamedPageSource{{Schema: schema, Src: src}}
	} else {
		// No schema argument: the scan covers every attached database.
		v.sources = m.provider.AllPageSources()
	}
	return v, nil
}

// Columns declares the dbpage schema (dbpage.c dbpageConnect):
// pgno INTEGER PRIMARY KEY, data BLOB, schema HIDDEN.
func (v *dbpageVTab) Columns() []string {
	return []string{"pgno", "data", "schema"}
}

// HiddenColumns reports the HIDDEN schema column index.
func (v *dbpageVTab) HiddenColumns() map[int]bool {
	return map[int]bool{dbpageColumnSCHEMA: true}
}

// BestIndex accepts the default plan: pgno/schema filtering is re-checked at
// run time and ORDER BY pgno matches the natural scan order.
func (v *dbpageVTab) BestIndex(input []byte) ([]byte, error) {
	return nil, nil
}

// Open positions the cursor on the first page of the first non-empty bound
// source (the materializer's first Next() serves that row).
func (v *dbpageVTab) Open() (Cursor, error) {
	c := &dbpageCursor{sources: v.sources}
	c.startNextSource()
	return c, nil
}

// dbpageCursor walks each bound source's page range [1, mxPgno] in turn.
type dbpageCursor struct {
	sources []NamedPageSource
	idx     int    // current source index
	pgno    uint32 // current page number in sources[idx]
	mxPgno  uint32 // last page number in sources[idx]
	started bool   // the positioned first row has been served yet
	done    bool
}

// startNextSource positions the cursor on page 1 of the next non-empty
// source; done is set once no source remains.
func (c *dbpageCursor) startNextSource() {
	for {
		if c.idx >= len(c.sources) {
			c.done = true
			return
		}
		n := c.sources[c.idx].Src.PageCount()
		if n > 0 {
			c.mxPgno = n
			c.pgno = 1
			return
		}
		c.idx++
	}
}

// Next advances: the first call confirms the positioned first row, later
// calls step to the next page of the current source and then to the next
// source (dbpageNext: pgno==mxPgno moves on).
func (c *dbpageCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return true
	}
	if c.pgno < c.mxPgno {
		c.pgno++
		return true
	}
	c.idx++
	c.startNextSource()
	return !c.done
}

// Rowid returns the current page number (dbpageRowid).
func (c *dbpageCursor) Rowid() int64 { return int64(c.pgno) }

// Column serves pgno, data and schema (dbpageColumn). The data BLOB is a
// fresh copy so callers cannot alias pager memory.
func (c *dbpageCursor) Column(idx int) (interface{}, error) {
	switch idx {
	case dbpageColumnPGNO:
		return int64(c.pgno), nil
	case dbpageColumnDATA:
		data, err := c.sources[c.idx].Src.ReadPage(c.pgno)
		if err != nil {
			return nil, fmt.Errorf("sqlite_dbpage: read page %d: %w", c.pgno, err)
		}
		return data, nil
	case dbpageColumnSCHEMA:
		return c.sources[c.idx].Schema, nil
	}
	return nil, fmt.Errorf("sqlite_dbpage: invalid column index %d", idx)
}

// UpdateRow implements RowUpdater (dbpageUpdate's UPDATE path): pgno and
// schema identify the page, data carries the new bytes. The new data must be
// exactly one page.
func (v *dbpageVTab) UpdateRow(oldValues, values []interface{}) error {
	if len(values) <= dbpageColumnDATA {
		return fmt.Errorf("sqlite_dbpage: update needs pgno and data")
	}
	pgno, ok := asInt64(values[dbpageColumnPGNO])
	if !ok || pgno < 1 {
		return fmt.Errorf("sqlite_dbpage: invalid pgno")
	}
	data, err := pageBlobOf(values[dbpageColumnDATA])
	if err != nil {
		return err
	}
	return v.writePage(pgno, data)
}

// InsertRow implements RowUpdater (dbpageUpdate's INSERT path, which acts as
// REPLACE): writing a page replaces it; NULL data truncates the database to
// pgno pages (src/dbpage.c: "INSERT to page N with NULL data causes the N-th
// page and all subsequent pages to be deleted").
func (v *dbpageVTab) InsertRow(values []interface{}) (int64, error) {
	if len(values) <= dbpageColumnPGNO {
		return 0, fmt.Errorf("sqlite_dbpage: insert needs a pgno")
	}
	pgno, ok := asInt64(values[dbpageColumnPGNO])
	if !ok || pgno < 1 {
		return 0, fmt.Errorf("sqlite_dbpage: invalid pgno")
	}
	if values[dbpageColumnDATA] == nil {
		// Truncate every source down to the target page count; only the
		// source owning pgno keeps that many pages (single-source instances
		// are unaffected by other schemas).
		for _, ns := range v.sources {
			n := ns.Src.PageCount()
			if uint64(n) > uint64(pgno) {
				if err := ns.Src.TruncatePages(uint32(pgno)); err != nil {
					return 0, err
				}
			}
		}
		return pgno, nil
	}
	blob, err := pageBlobOf(values[dbpageColumnDATA])
	if err != nil {
		return 0, err
	}
	if err := v.writePage(pgno, blob); err != nil {
		return 0, err
	}
	return pgno, nil
}

// DeleteRow implements RowUpdater: dbpage rejects deletes ("cannot delete").
func (v *dbpageVTab) DeleteRow(oldValues []interface{}) error {
	return fmt.Errorf("sqlite_dbpage: cannot delete")
}

// writePage locates the source owning the row's schema (default: the first
// bound source) and writes the page, extending the file when pgno exceeds
// the current page count.
func (v *dbpageVTab) writePage(pgno int64, data []byte) error {
	if len(v.sources) == 0 {
		return fmt.Errorf("sqlite_dbpage: no database")
	}
	src := v.sources[0].Src
	if uint64(pgno) > uint64(src.PageCount()) {
		return fmt.Errorf("sqlite_dbpage: no such page: %d", pgno)
	}
	return src.WritePage(uint32(pgno), data)
}

// pageBlobOf coerces an SQL value (BLOB/TEXT/zeroblob) into raw page bytes.
// The Go byte-slice string form "[1 2 3]" (tclStr's rendering of a BLOB
// cell through the db-eval row-callback harness) is decoded back to bytes so
// pages copied via the callback round-trip losslessly.
func pageBlobOf(val interface{}) ([]byte, error) {
	switch v := val.(type) {
	case []byte:
		return v, nil
	case value.ZeroBlob:
		return v.Bytes(), nil
	case string:
		if b, ok := parseGoByteSliceString(v); ok {
			return b, nil
		}
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("sqlite_dbpage: data must be a blob")
	}
}

// parseGoByteSliceString decodes fmt.Sprint's "[83 81 76]" rendering of a
// []byte back into bytes. ok is false unless every field parses as a byte.
func parseGoByteSliceString(s string) ([]byte, bool) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, false
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []byte{}, true
	}
	fields := strings.Fields(inner)
	out := make([]byte, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || n > 255 {
			return nil, false
		}
		out = append(out, byte(n))
	}
	return out, true
}

// Close implements Cursor.
func (c *dbpageCursor) Close() error { return nil }
