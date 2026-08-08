// Package exec implements query execution.
package exec

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

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
	if e.dataVersion == 0 {
		e.dataVersion = 1
	}
	return &Result{Rows: [][]interface{}{{e.dataVersion}}}
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
		if e.defensive {
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
	if e.cacheSizes == nil {
		return 2000
	}
	key := strings.ToUpper(ctx.Name)
	if v, ok := e.cacheSizes[key]; ok {
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
	if e.cacheSizes == nil {
		e.cacheSizes = make(map[string]int64)
	}
	e.cacheSizes[strings.ToUpper(ctx.Name)] = n
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
		e.cacheSpillSize = size
	}
	enabled := sqliteGetBoolean(value, size != 0)
	e.cacheSpillEnabled = enabled
	// Boolean-true words reset the threshold (pragma2-4.8: cache_spill=ON
	// after a large numeric threshold still spills eagerly).
	if enabled && !isNumeric(value) {
		e.cacheSpillSize = 0
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
	if !e.cacheSpillEnabled {
		return 0
	}
	spill := e.cacheSpillSize
	if spill < 0 {
		szPage := int64(ctx.Pager.PageSize())
		spill = int((-1024 * int64(spill)) / (szPage + 152))
		println("ZZDEBUG spill convert:", e.cacheSpillSize, "szPage=", szPage, "->", spill)
	}
	cache := e.pragmaCacheSizeFor(ctx)
	if cache > int64(spill) {
		return cache
	}
	return int64(spill)
}

// readFileChangeCounter reads the change counter directly from the file for
// ctx, bypassing the pager cache (used by data_version to detect commits by
// other connections). Returns -1 when the counter cannot be read.
func readFileChangeCounter(pg *pager.Pager) int64 {
	if pg == nil {
		return -1
	}
	hdr := pg.Header()
	if hdr == nil {
		return -1
	}
	dh, err := storage.ParseHeader(hdr)
	if err != nil {
		return -1
	}
	return int64(dh.FileChangeCount)
}

// encodeCounterToHeader writes a change counter value into a header byte
// slice (used when the engine persists the counter).
func encodeCounterToHeader(hdr []byte, counter uint32) {
	if len(hdr) < 28 {
		return
	}
	binary.BigEndian.PutUint32(hdr[24:28], counter)
}

// lockStatusFor returns the PRAGMA lock_status state for a database. SQLite
// reports "unlocked", "shared", "reserved", "pending", or "exclusive"
// depending on the current transaction's lock. The engine tracks a write
// transaction as "reserved" (inside a transaction that may write) and an
// EXCLUSIVE/COMMIT as "exclusive"; otherwise "unlocked".
func (e *Engine) lockStatusFor(ctx *DatabaseContext) string {
	// A database whose pager has unflushed dirty pages was written by the
	// current transaction. Inside a transaction it holds at least a RESERVED
	// lock; with cache spilling enabled and a spill threshold that fits
	// within the cache the pager escalates to EXCLUSIVE.
	dirty := ctx != nil && ctx.Pager != nil && ctx.Pager.HasDirtyPages()
	if e.inTransaction && dirty {
		if e.cacheSpillEnabled && e.pragmaCacheSpillFor(ctx) <= e.pragmaCacheSizeFor(ctx) {
			return "exclusive"
		}
		return "reserved"
	}
	if !e.inTransaction && dirty {
		// A write outside an explicit transaction (autocommit) holds an
		// exclusive lock until the statement's implicit commit flushes.
		return "exclusive"
	}
	return "unlocked"
}
