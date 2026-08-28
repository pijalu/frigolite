// Package exec implements query execution.
package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// sqliteVersionNumber is the SQLite version written to header bytes
// 96..99 on every commit (pager.c pager_write_changecounter uses
// SQLITE_VERSION_NUMBER). 3045000 = 3.45.0.
const sqliteVersionNumber uint32 = 3045000

// updateDBHeaderField modifies one field of the database header of ctx and
// persists it by marking page 1 dirty (the header lives in the first 100 bytes
// of page 1). The in-memory pager header is kept in sync so later reads see
// the new value.
func (e *Engine) updateDBHeaderField(ctx *DatabaseContext, mutate func(*storage.DatabaseHeader)) error {
	if ctx == nil {
		ctx = e.mainDB
	}
	if ctx.Pager == nil {
		return nil
	}
	hdr := ctx.Pager.Header()
	var dh *storage.DatabaseHeader
	var err error
	if hdr == nil {
		dh = storage.DefaultHeader(ctx.Pager.PageSize())
	} else {
		dh, err = storage.ParseHeader(hdr)
		if err != nil {
			return err
		}
	}
	mutate(dh)
	ctx.Pager.SetHeader(dh.Encode())
	// Persist: page 1's Data must carry the header bytes and be marked dirty
	// so the next Flush writes it. If the pager already holds page 1, update
	// its Data; otherwise read it (creating a page-1 entry) then write.
	pg, err := ctx.Pager.ReadPage(1)
	if err != nil {
		// In-memory pagers and fresh databases always have page 1; a read
		// failure means the database has no page 1 yet (unusual), so just
		// keep the in-memory header.
		return nil
	}
	copy(pg.Data[:storage.HeaderSize], dh.Encode())
	return ctx.Pager.WritePage(pg)
}

// headerFor returns the parsed database header for ctx, or nil when the
// pager has no header (fresh in-memory database being created).
func (e *Engine) headerFor(ctx *DatabaseContext) *storage.DatabaseHeader {
	if ctx == nil {
		ctx = e.mainDB
	}
	hdr := ctx.Pager.Header()
	if hdr == nil {
		return nil
	}
	dh, err := storage.ParseHeader(hdr)
	if err != nil {
		return nil
	}
	return dh
}

// execPragmaDataVersion implements PRAGMA data_version. The value is a
// per-connection counter that starts at 1 and changes only when another
// connection commits to the database (observed via the external-modification
// check). Commits made by THIS connection do not change the reported version
// (SQLite: "unchanged for commits made on the same database connection").
func (e *Engine) execPragmaDataVersion(ctx *DatabaseContext) *Result {
	if e.settings.dataVersion == 0 {
		e.settings.dataVersion = 1
	}
	return &Result{Rows: [][]interface{}{{e.settings.dataVersion}}}
}

// updateFileChangeCounter increments the database file's change counter
// (header offset 24) and records the new data_version for THIS connection.
// SQLite increments the counter on every write transaction commit; a
// connection's own commits do not change its data_version view because it
// caches the pre-commit value. We therefore keep the pre-commit value: the
// file counter moves forward for other connections while this connection's
// reported data_version stays put.
func (e *Engine) updateFileChangeCounter(ctx *DatabaseContext) {
	if ctx == nil {
		ctx = e.mainDB
	}
	dh := e.headerFor(ctx)
	if dh == nil {
		return
	}
	dh.FileChangeCount++
	if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
		h.FileChangeCount = dh.FileChangeCount
		// pager.c pager_write_changecounter: bytes 92..95 carry the change
		// counter the version number is valid for, bytes 96..99 the SQLite
		// version that wrote the file.
		h.VersionValidFor = dh.FileChangeCount
		h.SQLiteVersionNum = sqliteVersionNumber
		// btree.c sqlite3BtreeBeginTrans keeps the in-header database size
		// (offset 28) current on every write transaction.
		if ctx != nil && ctx.Pager != nil {
			h.DatabaseSize = ctx.Pager.NumPages()
		}
	}); err != nil {
		return
	}
	// Record the counter this connection wrote so external-mod detection
	// does not invalidate the pager cache for our own commit.
	if ctx.Schema != nil {
		ctx.Schema.NoteOwnWrite(dh.FileChangeCount)
	}
}

// execPragmaDefaultCacheSize implements PRAGMA default_cache_size (with and
// without an argument). The value is stored in the database header at offset
// 48 (the "default page cache size" field) and read back as a signed 32-bit
// value whose absolute value is reported (SQLite negates negative values on
// read; writing a negative value stores its absolute value).
func (e *Engine) execPragmaDefaultCacheSize(ctx *DatabaseContext, value string) *Result {
	if ctx == nil {
		ctx = e.mainDB
	}
	if value != "" {
		n, err := parseInt64Value(value)
		if err != nil {
			return &Result{Error: fmt.Errorf("cannot store %s: not an integer", value)}
		}
		if n < 0 {
			n = -n
		}
		if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
			h.DefaultCacheSize = uint32(n)
		}); err != nil {
			return &Result{Error: err}
		}
		return &Result{}
	}
	n := int64(2000) // SQLite default when the header field is zero
	if dh := e.headerFor(ctx); dh != nil && dh.DefaultCacheSize != 0 {
		n = int64(int32(dh.DefaultCacheSize))
		if n < 0 {
			n = -n
		}
	}
	return &Result{Rows: [][]interface{}{{n}}}
}

// execPragmaSchemaVersion implements PRAGMA schema_version, stored in the
// header at offset 40 (the schema cookie). With SQLITE_DBCONFIG_DEFENSIVE
// enabled, setting it is ignored (SQLite keeps schema_version read-only in
// defensive mode).
func (e *Engine) execPragmaSchemaVersion(ctx *DatabaseContext, value string) *Result {
	if ctx == nil {
		ctx = e.mainDB
	}
	if value != "" {
		if e.settings.defensive {
			return &Result{}
		}
		n, err := parseInt64Value(value)
		if err != nil {
			return &Result{Error: fmt.Errorf("cannot store %s: not an integer", value)}
		}
		if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
			h.SchemaCookie = uint32(n)
		}); err != nil {
			return &Result{Error: err}
		}
		return &Result{}
	}
	n := int64(1)
	if dh := e.headerFor(ctx); dh != nil {
		n = int64(dh.SchemaCookie)
	}
	return &Result{Rows: [][]interface{}{{n}}}
}

// execPragmaUserVersion implements PRAGMA user_version, stored in the header
// at offset 60. Setting it also bumps the schema cookie (SQLite
// sqlite3BtreeUpdateMeta behavior is a plain header write; user_version does
// not affect the schema cookie).
func (e *Engine) execPragmaUserVersion(ctx *DatabaseContext, value string) *Result {
	if ctx == nil {
		ctx = e.mainDB
	}
	if value != "" {
		n, err := parseInt64Value(value)
		if err != nil {
			return &Result{Error: fmt.Errorf("cannot store %s: not an integer", value)}
		}
		if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
			h.UserVersion = uint32(n)
		}); err != nil {
			return &Result{Error: err}
		}
		return &Result{}
	}
	n := int64(0)
	if dh := e.headerFor(ctx); dh != nil {
		n = int64(dh.UserVersion)
	}
	return &Result{Rows: [][]interface{}{{n}}}
}

// parseInt64Value parses a pragma integer value, supporting decimal and 0x
// hex forms (SQLite sqlite3GetInt32 accepts 0x...).
func parseInt64Value(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if len(t) > 2 && (t[0:2] == "0x" || t[0:2] == "0X") {
		return strconv.ParseInt(t[2:], 16, 64)
	}
	return strconv.ParseInt(t, 10, 64)
}

// execPragmaApplicationID implements PRAGMA application_id, stored in the
// header at offset 72.
func (e *Engine) execPragmaApplicationID(ctx *DatabaseContext, value string) *Result {
	if ctx == nil {
		ctx = e.mainDB
	}
	if value != "" {
		n, err := parseInt64Value(value)
		if err != nil {
			return &Result{Error: fmt.Errorf("cannot store %s: not an integer", value)}
		}
		if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
			h.ApplicationID = uint32(n)
		}); err != nil {
			return &Result{Error: err}
		}
		return &Result{}
	}
	n := int64(0)
	if dh := e.headerFor(ctx); dh != nil {
		n = int64(dh.ApplicationID)
	}
	return &Result{Rows: [][]interface{}{{n}}}
}

// execPragmaPageSize implements PRAGMA page_size. Setting page_size on an
// empty database records the desired page size in the header; the engine uses
// a fixed page size at open (changing it after pages exist is a no-op, as
// SQLite only honors it before the first table is created).
func (e *Engine) execPragmaPageSize(ctx *DatabaseContext, value string) *Result {
	if ctx == nil {
		ctx = e.mainDB
	}
	if value != "" {
		n, err := parseInt64Value(value)
		if err != nil {
			return &Result{Error: fmt.Errorf("cannot store %s: not an integer", value)}
		}
		if n < 512 || n > 65536 || (n&(n-1)) != 0 {
			return &Result{Error: fmt.Errorf("out of range: %d", n)}
		}
		// Only honored before any table exists (SQLite errors with
		// "unsupported file format" only for a mismatch at open; setting
		// after creation is silently ignored).
		if e.schemaIsEmpty(ctx) {
			ctx.Pager.SetPageSize(uint32(n))
			if err := e.updateDBHeaderField(ctx, func(h *storage.DatabaseHeader) {
				h.PageSize = uint32(n)
			}); err != nil {
				return &Result{Error: err}
			}
		}
		return &Result{}
	}
	ps := ctx.Pager.PageSize()
	return &Result{Rows: [][]interface{}{{int64(ps)}}}
}

// encodingName converts a numeric text-encoding header value (offset 56)
// to the engine's encoding string: 1=UTF-8, 2=UTF-16le, 3=UTF-16be.
func encodingName(enc uint32) string {
	switch enc {
	case 2:
		return "UTF-16le"
	case 3:
		return "UTF-16be"
	default:
		return "UTF-8"
	}
}

// headerTextEncoding returns the text-encoding field (offset 56) of the
// pager's database header, or 1 (UTF-8) when the header is unavailable.
func headerTextEncoding(pg *pager.Pager) uint32 {
	hdr := pg.Header()
	if hdr == nil {
		return 1
	}
	dh, err := storage.ParseHeader(hdr)
	if err != nil {
		return 1
	}
	return dh.TextEncoding
}

// schemaIsEmpty reports whether ctx's schema contains no user objects
// (tables, indexes, views, triggers). The sqlite_schema entry itself and
// the sqlite_sequence autoincrement table are ignored.
func (e *Engine) schemaIsEmpty(ctx *DatabaseContext) bool {
	if ctx == nil {
		ctx = e.mainDB
	}
	entries, err := ctx.Schema.GetEntries("")
	if err != nil {
		return false
	}
	for _, ent := range entries {
		upper := strings.ToUpper(ent.Name)
		if upper == "SQLITE_SCHEMA" || upper == "SQLITE_MASTER" {
			continue
		}
		return false
	}
	return true
}

// pragmaDBCtx resolves the schema qualifier (PRAGMA main.cache_size, PRAGMA
// aux.user_version) to a database context, defaulting to main.
func (e *Engine) pragmaDBCtx(schemaName string) *DatabaseContext {
	if schemaName == "" {
		return e.mainDB
	}
	upper := strings.ToUpper(schemaName)
	if upper == "MAIN" {
		return e.mainDB
	}
	if upper == "TEMP" || upper == "TEMPORARY" {
		if ctx := e.getDB("TEMP"); ctx != nil {
			return ctx
		}
		return e.mainDB
	}
	if ctx := e.getDB(schemaName); ctx != nil {
		return ctx
	}
	return e.mainDB
}

// pragmaCacheSizeFor returns the effective cache size for ctx. SQLite's
// default cache size is 2000 pages (SQLITE_DEFAULT_CACHE_SIZE); a negative
// cache_size setting means kilobytes and is converted to pages. The engine
// does not actually size its cache, but reports the setting like SQLite.
func (e *Engine) pragmaCacheSizeFor(ctx *DatabaseContext) int64 {
	// Cache sizes are tracked per connection in the engine; the default is
	// 2000 pages.
	if e.settings.cacheSizes == nil {
		return 2000
	}
	key := strings.ToUpper(ctx.Name)
	if v, ok := e.settings.cacheSizes[key]; ok {
		return v
	}
	return 2000
}

// setPragmaCacheSize records the cache_size setting for ctx.
func (e *Engine) setPragmaCacheSize(ctx *DatabaseContext, value string) {
	n, err := parseInt64Value(value)
	if err != nil {
		return
	}
	if n < 0 {
		// Negative values are in KiB; convert to pages (SQLite rounds up).
		n = (-n + 1023) / 1024
		if n == 0 {
			n = 1
		}
	}
	if e.settings.cacheSizes == nil {
		e.settings.cacheSizes = make(map[string]int64)
	}
	e.settings.cacheSizes[strings.ToUpper(ctx.Name)] = n
}

// setPragmaCacheSpill implements the PRAGMA cache_spill=N setter. A numeric
// value sets the spill threshold (negative = KiB converted to pages using
// pageSize+152, matching sqlite3PcacheSetSpillsize). A boolean word
// (ON/OFF/YES/NO/TRUE/FALSE) toggles the spill-enabled flag; the SQLite test
// suite (pragma2.test) expects the boolean-true forms (ON/YES) to also reset
// the threshold so the effective spill equals the cache size (exclusive lock
// during a write transaction).
func (e *Engine) setPragmaCacheSpill(value string) *Result {
	size := 1
	if n, err := parseInt64Value(value); err == nil {
		size = int(n)
		e.settings.cacheSpillSize = size
	}
	enabled := sqliteGetBoolean(value, size != 0)
	e.settings.cacheSpillEnabled = enabled
	// Boolean-true words reset the threshold (pragma2-4.8: cache_spill=ON
	// after a large numeric threshold still spills eagerly).
	if enabled && !isNumeric(value) {
		e.settings.cacheSpillSize = 0
	}
	return &Result{}
}

// isNumeric reports whether s parses as an integer.
func isNumeric(s string) bool {
	_, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return err == nil
}

// sqliteGetBoolean mirrors SQLite's sqlite3GetBoolean: "1", "true", "yes",
// "on" (and their uppercase forms) are true; "0", "false", "no", "off" are
// false; anything else returns the default.
func sqliteGetBoolean(z string, dflt bool) bool {
	switch strings.ToLower(strings.TrimSpace(z)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return dflt
}

// pragmaCacheSpillFor implements the PRAGMA cache_spill getter, mirroring
// sqlite3PcacheSetSpillsize(p, 0) used by the pragma: returns 0 when spilling
// is disabled; otherwise max(cacheSize, spillSize) where spillSize has the
// KiB→pages conversion applied for negative values.
func (e *Engine) pragmaCacheSpillFor(ctx *DatabaseContext) int64 {
	if !e.settings.cacheSpillEnabled {
		return 0
	}
	spill := e.settings.cacheSpillSize
	if spill < 0 {
		szPage := int64(ctx.Pager.PageSize())
		spill = int((-1024 * int64(spill)) / (szPage + 152))
		println("ZZDEBUG spill convert:", e.settings.cacheSpillSize, "szPage=", szPage, "->", spill)
	}
	cache := e.pragmaCacheSizeFor(ctx)
	if cache > int64(spill) {
		return cache
	}
	return int64(spill)
}

// lockStatusFor returns the PRAGMA lock_status state for a database. SQLite
// reports "unlocked", "shared", "reserved", "pending", or "exclusive"
// depending on the current transaction's lock. The engine tracks a write
// transaction as "reserved" (inside a transaction that may write) and an
// EXCLUSIVE/COMMIT as "exclusive"; otherwise "unlocked".
func (e *Engine) lockStatusFor(ctx *DatabaseContext) string {
	// Open incremental-blob handles hold a lock on their database: a
	// read-write handle holds RESERVED, a read-only handle holds SHARED
	// (SQLite vdbe blob locks).
	if e.blobLocks != nil {
		name := strings.ToUpper(ctx.Name)
		if n := e.blobLocks[name].write; n > 0 {
			return "reserved"
		}
		if n := e.blobLocks[name].read; n > 0 {
			return "shared"
		}
	}
	// A database whose pager has unflushed dirty pages was written by the
	// current transaction. Inside a transaction it holds at least a RESERVED
	// lock; with cache spilling enabled and a spill threshold that fits
	// within the cache the pager escalates to EXCLUSIVE.
	dirty := ctx != nil && ctx.Pager != nil && ctx.Pager.HasDirtyPages()
	if e.tx.inTransaction && dirty {
		if e.settings.cacheSpillEnabled && e.pragmaCacheSpillFor(ctx) <= e.pragmaCacheSizeFor(ctx) {
			return "exclusive"
		}
		return "reserved"
	}
	if !e.tx.inTransaction && dirty {
		// A write outside an explicit transaction (autocommit) holds an
		// exclusive lock until the statement's implicit commit flushes.
		return "exclusive"
	}
	return "unlocked"
}

// blobLockCounts tracks open incremental-blob handles per schema for
// PRAGMA lock_status (read-only → shared, read-write → reserved).
type blobLockCounts struct {
	read  int
	write int
}

// AddBlobLock records an open blob handle on a schema.
func (e *Engine) AddBlobLock(schema string, write bool) {
	if e.blobLocks == nil {
		e.blobLocks = make(map[string]blobLockCounts)
	}
	c := e.blobLocks[strings.ToUpper(schema)]
	if write {
		c.write++
	} else {
		c.read++
	}
	e.blobLocks[strings.ToUpper(schema)] = c
}

// RemoveBlobLock records a closed blob handle on a schema.
func (e *Engine) RemoveBlobLock(schema string, write bool) {
	if e.blobLocks == nil {
		return
	}
	name := strings.ToUpper(schema)
	c := e.blobLocks[name]
	if write {
		if c.write > 0 {
			c.write--
		}
	} else {
		if c.read > 0 {
			c.read--
		}
	}
	if c.read == 0 && c.write == 0 {
		delete(e.blobLocks, name)
	} else {
		e.blobLocks[name] = c
	}
}

// AddBlobTableLock records an open blob handle on a table.
func (e *Engine) AddBlobTableLock(table string) {
	if e.blobTableLocks == nil {
		e.blobTableLocks = make(map[string]int)
	}
	e.blobTableLocks[strings.ToUpper(table)]++
}

// RemoveBlobTableLock records a closed blob handle on a table.
func (e *Engine) RemoveBlobTableLock(table string) {
	if e.blobTableLocks == nil {
		return
	}
	name := strings.ToUpper(table)
	if e.blobTableLocks[name] > 1 {
		e.blobTableLocks[name]--
	} else {
		delete(e.blobTableLocks, name)
	}
}

// HasOpenBlobsOnTable reports whether an incremental-blob handle is open on
// the table (DROP TABLE fails with "database table is locked" in that case).
func (e *Engine) HasOpenBlobsOnTable(table string) bool {
	return e.blobTableLocks != nil && e.blobTableLocks[strings.ToUpper(table)] > 0
}

// BeginActiveStatement marks the start of a harness-emulated active read
// statement (one upstream nVdbeRead unit).
func (e *Engine) BeginActiveStatement() {
	e.activeReadsMu.Lock()
	e.activeExternalReads++
	e.activeReadsMu.Unlock()
}

// EndActiveStatement marks the end of a harness-emulated active read
// statement.
func (e *Engine) EndActiveStatement() {
	e.activeReadsMu.Lock()
	if e.activeExternalReads > 0 {
		e.activeExternalReads--
	}
	e.activeReadsMu.Unlock()
}

// ActiveReadStatements returns the number of harness-emulated active read
// statements (the OP_Destroy interlock compares this against zero — any
// other running reader forbids table destruction: src/vdbe.c
// "db->nVdbeRead > db->nVDestroy+1" → SQLITE_LOCKED).
func (e *Engine) ActiveReadStatements() int {
	e.activeReadsMu.Lock()
	defer e.activeReadsMu.Unlock()
	return e.activeExternalReads
}

// ClearBlobLocks drops all blob-lock bookkeeping on the engine. Called when a
// connection is closed/reopened: the abandoned connection's blob handles no
// longer block DROP TABLE on the new connection (SQLite abandons handles when
// the connection is replaced).
func (e *Engine) ClearBlobLocks() {
	e.blobLocks = nil
	e.blobTableLocks = nil
}
