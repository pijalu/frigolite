package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// Compile-time probe: Engine satisfies the execpragma.EngineState capability
// interface. This is the Liskov substitution check — if Engine drifts from the
// interface the pragma handlers need, this file stops compiling.
var _ execpragma.EngineState = (*Engine)(nil)

// pragmaResult converts an engine result to the execpragma result type. A nil
// result stays nil (some pragma setters, e.g. a no-op encoding assignment,
// legitimately return nil).
func pragmaResult(r *Result) *execpragma.Result {
	if r == nil {
		return nil
	}
	return &execpragma.Result{Columns: r.Columns, Rows: r.Rows, Error: r.Error}
}

// --- Header-backed pragmas ---

// DataVersion implements PRAGMA data_version (schema-qualified).
func (e *Engine) DataVersion(schema string) *execpragma.Result {
	return pragmaResult(e.execPragmaDataVersion(e.pragmaDBCtx(schema)))
}

// FileDataVersion returns the database FILE's change counter (header offset
// 24), the SQLITE_FCNTL_DATA_VERSION equivalent. Unlike PRAGMA data_version
// (which stays fixed for the current connection's own commits), the file
// counter advances on every write transaction commit, including this
// connection's (dataversion1.test).
func (e *Engine) FileDataVersion(schema string) int64 {
	ctx := e.pragmaDBCtx(schema)
	dh := e.headerFor(ctx)
	if dh == nil {
		return 0
	}
	return int64(dh.FileChangeCount)
}

// DefaultCacheSize implements PRAGMA default_cache_size.
func (e *Engine) DefaultCacheSize(schema, value string) *execpragma.Result {
	return pragmaResult(e.execPragmaDefaultCacheSize(e.pragmaDBCtx(schema), value))
}

// UserVersion implements PRAGMA user_version.
func (e *Engine) UserVersion(schema, value string) *execpragma.Result {
	return pragmaResult(e.execPragmaUserVersion(e.pragmaDBCtx(schema), value))
}

// ApplicationID implements PRAGMA application_id.
func (e *Engine) ApplicationID(schema, value string) *execpragma.Result {
	return pragmaResult(e.execPragmaApplicationID(e.pragmaDBCtx(schema), value))
}

// SchemaVersion implements PRAGMA schema_version.
func (e *Engine) SchemaVersion(schema, value string) *execpragma.Result {
	return pragmaResult(e.execPragmaSchemaVersion(e.pragmaDBCtx(schema), value))
}

// PageSize implements PRAGMA page_size.
func (e *Engine) PageSize(schema, value string) *execpragma.Result {
	return pragmaResult(e.execPragmaPageSize(e.pragmaDBCtx(schema), value))
}

// JournalMode implements PRAGMA journal_mode (getter and setter). The setter
// enables the WAL write path when value is "wal"; for the legacy rollback-journal
// modes it records the mode so the getter reports it. A mode change requested
// while a transaction is open is deferred (pager.c pendingJournalMode) and only
// applied when the transaction ends, matching SQLite (test/jrnlmode3.c 3.3/3.5).
func (e *Engine) JournalMode(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		if value == "" {
			return &execpragma.Result{Rows: [][]interface{}{{"delete"}}}
		}
		return &execpragma.Result{Error: fmt.Errorf("no such database: %s", schema)}
	}
	if value != "" {
		return e.setJournalMode(ctx, schema, strings.ToLower(strings.TrimSpace(value)))
	}
	return journalModeResult(ctx.Pager.JournalMode())
}

// journalModeResult echoes a journal mode as the pragma's single-cell row
// (an empty pager mode renders as "delete", the rollback-journal default).
func journalModeResult(mode string) *execpragma.Result {
	if mode == "" {
		mode = "delete"
	}
	return &execpragma.Result{Rows: [][]interface{}{{mode}}}
}

// setJournalMode applies a journal_mode assignment to one database's pager.
func (e *Engine) setJournalMode(ctx *DatabaseContext, schema, m string) *execpragma.Result {
	// pager.c sqlite3PagerSetJournalMode: a WAL-involving mode change
	// (to or from WAL) opens/closes the WAL via the exclusive-lock path;
	// any other connection's lock blocks it. Rollback↔rollback changes
	// take no lock (tkt-fc62af4523.3).
	if m == "wal" || strings.EqualFold(ctx.Pager.JournalMode(), "wal") {
		if err := e.journalModeChangeLockError(schema); err != nil {
			return &execpragma.Result{Error: err}
		}
	}
	if e.InTransaction() && ctx.Pager.HasDirtyPages() {
		// Defer the switch until the transaction ends (pager.c
		// pendingJournalMode / btreeEndTransaction). When the pager
		// is in PAGER_WRITER_CACHEMOD (already wrote dirty pages
		// under the open transaction), sqlite3PagerOkToChangeJournalMode
		// (pager.c:7456) returns false and OP_JournalMode (vdbe.c:8021)
		// reports the CURRENT (active) mode — not the requested one
		// (test/jrnlmode3.c 3.3). A bare BEGIN IMMEDIATE with no writes
		// yet still allows the change (test/jrnlmode.c 8.21: the
		// setter echoes the new mode). The pending change is applied
		// by ApplyPendingJournalMode at COMMIT/ROLLBACK
		// (internal/exec/transaction.go).
		ctx.Pager.SetPendingJournalMode(m)
		return journalModeResult(ctx.Pager.JournalMode())
	}
	if err := ctx.Pager.SetJournalMode(m); err != nil {
		// WAL on an in-memory pager: SQLite's memdb keeps the prior
		// mode (memdb1.test 420 expects the WAL assignment to echo
		// "delete", the pre-existing mode — the request is a no-op
		// because :memory: has no WAL file). Echo the current mode.
		if strings.Contains(err.Error(), "in-memory") {
			return journalModeResult(ctx.Pager.JournalMode())
		}
		return &execpragma.Result{Error: err}
	}
	return journalModeResult(ctx.Pager.JournalMode())
}

// JournalSizeLimit implements PRAGMA journal_size_limit (getter and setter),
// the per-database cap applied to a PERSIST journal file after a commit. The
// value is stored verbatim so the getter echoes it (pragma.c journalSizeLimit).
func (e *Engine) JournalSizeLimit(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return &execpragma.Result{}
	}
	if value != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			ctx.Pager.SetJournalSizeLimit(n)
		}
	}
	return &execpragma.Result{Rows: [][]interface{}{{ctx.Pager.JournalSizeLimit()}}}
}

// lockingModeToken maps a PRAGMA locking_mode value to its pager.c mode
// constant: 0 = NORMAL, 1 = EXCLUSIVE, -1 = QUERY (pragma.c getLockingMode:
// an absent or unrecognised token is a query).
func lockingModeToken(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "normal":
		return 0
	case "exclusive":
		return 1
	default:
		return -1
	}
}

// dbEffectiveLockingMode reports a database context's pager locking mode
// (pager.c Pager.exclusiveMode): an explicitly set mode wins; otherwise temp
// and in-memory pagers are born EXCLUSIVE (pager.c:5052
// "pPager->exclusiveMode = (u8)tempFile") and file databases default to
// NORMAL.
func dbEffectiveLockingMode(ctx *DatabaseContext) string {
	if ctx.LockingMode != "" {
		return ctx.LockingMode
	}
	if ctx.IsTemp || ctx.IsMemory {
		return "exclusive"
	}
	return "normal"
}

// dbSetLockingMode applies a locking mode to one database context
// (sqlite3PagerLockingMode, pager.c:7324): the set is IGNORED on temp and
// in-memory pagers, which are always exclusive. The WAL parity flag follows
// the mode on pagers that accepted it (wal.c: in EXCLUSIVE mode the shm lock
// calls become no-ops).
func dbSetLockingMode(ctx *DatabaseContext, mode int) {
	if ctx == nil || mode < 0 || ctx.IsTemp || ctx.IsMemory {
		return
	}
	name := "exclusive"
	if mode == 0 {
		name = "normal"
	}
	ctx.LockingMode = name
	if ctx.Pager != nil {
		ctx.Pager.SetWALExclusiveMode(mode == 1)
	}
}

// LockingMode implements PRAGMA locking_mode (pragma.c PragTyp_LOCKING_MODE,
// src/pragma.c:687):
//
//   - "PRAGMA locking_mode" (unqualified query) reports the connection
//     default (db->dfltLockMode — the last unqualified set, else "normal").
//   - "PRAGMA locking_mode = N" (unqualified set) sets every database EXCEPT
//     temp (the pragma.c loop starts at aDb[2], skipping aDb[1]), sets main,
//     and updates the connection default so later ATTACHes inherit it.
//   - A schema-qualified form reads/writes only that database's pager; temp
//     and memory pagers report EXCLUSIVE and ignore sets
//     (pager.c:5052/7332).
func (e *Engine) LockingMode(schema, value string) *execpragma.Result {
	mode := lockingModeToken(value)
	if schema == "" && mode < 0 {
		// Simple "PRAGMA locking_mode" — the connection default.
		return &execpragma.Result{Rows: [][]interface{}{{e.currentLockingMode()}}}
	}
	if schema == "" {
		// Unqualified set: aux databases (pragma.c:710 loops from aDb[2]),
		// then main (pragma.c:716), then the connection default
		// (pragma.c:714 dfltLockMode). Temp is deliberately skipped.
		for _, dbCtx := range e.dbList {
			if dbCtx == nil || dbCtx == e.mainDB || dbCtx.IsTemp {
				continue
			}
			dbSetLockingMode(dbCtx, mode)
		}
		dbSetLockingMode(e.mainDB, mode)
		e.lockingMode = lockingModeName(mode)
		if mode == 0 {
			// Reverting to normal releases the never-unlocked SHARED locks
			// held in exclusive mode (pager.c drops back to
			// unlock-at-transaction-end).
			e.clearPersistentShared()
		}
		return &execpragma.Result{Rows: [][]interface{}{{lockingModeName(mode)}}}
	}
	// Qualified form: read (or set) only that database's pager.
	dbCtx := e.pragmaDBCtx(schema)
	if dbCtx == nil {
		return &execpragma.Result{Rows: [][]interface{}{{e.currentLockingMode()}}}
	}
	if mode >= 0 {
		dbSetLockingMode(dbCtx, mode)
		if mode == 0 {
			e.clearPersistentShared()
		}
	}
	return &execpragma.Result{Rows: [][]interface{}{{dbEffectiveLockingMode(dbCtx)}}}
}

// lockingModeName renders a pager.c locking-mode constant ("normal" or
// "exclusive").
func lockingModeName(mode int) string {
	if mode == 1 {
		return "exclusive"
	}
	return "normal"
}

// SoftHeapLimit implements PRAGMA soft_heap_limit (pragma.c
// PragTyp_SOFT_HEAP_LIMIT): any parseable value (the "=N" and "(N)" forms
// both arrive as value) calls sqlite3_soft_heap_limit64(N), where N < 0
// leaves the limit unchanged; the pragma always returns the current limit.
func (e *Engine) SoftHeapLimit(value string) *execpragma.Result {
	if value != "" {
		if n, err := parseDecOrHexInt64(value); err == nil {
			if n >= 0 {
				e.softHeapLimit = n
			}
		}
	}
	return &execpragma.Result{Rows: [][]interface{}{{e.softHeapLimit}}}
}

// parseDecOrHexInt64 parses a pragma value like sqlite3DecOrHexToI64:
// optional 0x hex prefix, otherwise decimal. Values that look numeric but
// overflow report an error (the caller then leaves the setting unchanged).
func parseDecOrHexInt64(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return strconv.ParseInt(s[2:], 16, 64)
	}
	return strconv.ParseInt(s, 10, 64)
}

// currentLockingMode returns the active locking mode (default "normal").
func (e *Engine) currentLockingMode() string {
	if e.lockingMode == "" {
		return "normal"
	}
	return e.lockingMode
}

// WalCheckpoint implements PRAGMA wal_checkpoint (PASSIVE|FULL|RESTART|
// TRUNCATE). The default mode is PASSIVE (the value-less form), which
// preserves the -wal file (sqlite/src/pragma.c PragTyp_WAL_CHECKPOINT).
// PASSIVE / FULL keep the committed frames on disk; RESTART / TRUNCATE
// fold the frames and reset the -wal to its header.
func (e *Engine) WalCheckpoint(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return &execpragma.Result{Rows: [][]interface{}{{0, 0, 0}}}
	}
	mode := pager.WalCkptPassive
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "full":
		mode = pager.WalCkptFull
	case "restart":
		mode = pager.WalCkptRestart
	case "truncate":
		mode = pager.WalCkptTruncate
	}
	busy, nLog, nCkpt, err := ctx.Pager.CheckpointMode(mode)
	if err != nil {
		return &execpragma.Result{Error: err}
	}
	return &execpragma.Result{Rows: [][]interface{}{{
		int64(busy), int64(nLog), int64(nCkpt),
	}}}
}

// PageCount implements PRAGMA page_count: the current number of pages in the
// named schema's database.
func (e *Engine) PageCount(schema string) int64 {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return 0
	}
	return int64(ctx.Pager.NumPages())
}

// MaxPageCount implements PRAGMA max_page_count (getter and setter). When
// value is "" the current cap is reported; when value parses to a non-
// negative integer the cap is updated. Mirrors pager.c::sqlite3PagerMaxPageCount:
// only the absolute value is stored; a value of 0 means unlimited.
//
// Per pragma.c (PragTyp_MAX_PAGE_COUNT): the new value is clamped to the
// current database size (sqlite3BtreeLastPage / sqlite3BtreeMaxPageCount)
// so max_page_count never shrinks below the actual file size. The
// clamping is performed here against ctx.Pager.NumPages() to mirror the
// VDBE OP_MaxPgcnt + sqlite3BtreeMaxPageCount chain.
//
// P8.PRAGMA: missing engine element implemented (2026-09).
func (e *Engine) MaxPageCount(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return &execpragma.Result{}
	}
	if value == "" {
		return &execpragma.Result{Rows: [][]interface{}{{int64(ctx.Pager.MaxPageCount())}}}
	}
	// Parse the value as a signed integer (sqlite3GetInt32). A non-numeric
	// or non-positive value leaves the cap unchanged and the pragma behaves
	// as a getter — OP_MaxPgcnt passes p3=0, so no new cap is applied
	// (oracle: PRAGMA max_page_count='abc' / =-5 / =0 all echo the current
	// value with no error).
	abs := int64(0)
	digits := strings.TrimPrefix(value, "-")
	negative := strings.HasPrefix(value, "-")
	for _, c := range digits {
		if c < '0' || c > '9' {
			return &execpragma.Result{Rows: [][]interface{}{{int64(ctx.Pager.MaxPageCount())}}}
		}
		abs = abs*10 + int64(c-'0')
	}
	if negative || abs <= 0 {
		return &execpragma.Result{Rows: [][]interface{}{{int64(ctx.Pager.MaxPageCount())}}}
	}
	// Mirror OP_MaxPgcnt's clamp to the current db size
	// (sqlite3BtreeLastPage): max_page_count never shrinks below the
	// number of pages already allocated.
	cur := int64(ctx.Pager.NumPages())
	if abs < cur {
		abs = cur
	}
	ctx.Pager.SetMaxPageCount(uint32(abs))
	return &execpragma.Result{Rows: [][]interface{}{{int64(ctx.Pager.MaxPageCount())}}}
}

// FreelistCount implements PRAGMA freelist_count: the number of free pages
// recorded in the on-disk database header (bytes 36-39), regardless of the
// in-memory p.freePages set. Mirrors btree.c sqlite3BtreeFreePageCount.
//
// P8.INCRVACUUM.phase7: prior to this, PRAGMA freelist_count returned a
// hard-coded 0, masking the gap between the chain's actual reach
// (chain-walked count) and the header's declared count. The hard-coded 0
// also hid ROLLBACK-fidelity gaps where the header was decremented but the
// chain still held stale references (and vice versa).
func (e *Engine) FreelistCount(schema string) int64 {
	ctx := e.pragmaDBCtx(schema)
	if ctx == nil || ctx.Pager == nil {
		return 0
	}
	return int64(ctx.Pager.FreelistCount())
}

// --- Cache pragmas ---

// CacheSize implements PRAGMA cache_size (getter and setter).
func (e *Engine) CacheSize(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if value != "" {
		e.setPragmaCacheSize(ctx, value)
		return &execpragma.Result{}
	}
	return &execpragma.Result{Rows: [][]interface{}{{e.pragmaCacheSizeFor(ctx)}}}
}

// CacheSpill implements PRAGMA cache_spill (getter and setter).
func (e *Engine) CacheSpill(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if value != "" {
		if res := e.setPragmaCacheSpill(value); res.Error != nil {
			return pragmaResult(res)
		}
		return &execpragma.Result{}
	}
	return &execpragma.Result{Rows: [][]interface{}{{e.pragmaCacheSpillFor(ctx)}}}
}

// MmapSize implements PRAGMA mmap_size (pragma.c PragTyp_MMAP_SIZE): the
// setter parses the limit (decimal, or hex with a 0x prefix —
// sqlite3DecOrHexToI64), stores it, and returns the effective value as a
// single integer row; a negative value selects the build default (0, the
// SQLITE_DEFAULT_MMAP_SIZE equivalent — the engine performs no real mmap,
// so the setting is a C-parity value store). The getter returns the
// current limit.
func (e *Engine) MmapSize(schema, value string) *execpragma.Result {
	if value != "" {
		v := strings.TrimSpace(value)
		base := 10
		if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
			base, v = 16, v[2:]
		}
		if n, err := strconv.ParseInt(v, base, 64); err == nil {
			if n < 0 {
				n = 0
			}
			e.settings.mmapSize = n
		}
		// An unparseable value is ignored for storage but the getter
		// still reports the current limit, matching sqlite3DecOrHexToI64
		// leaving sz unchanged on a parse failure.
	}
	return &execpragma.Result{Rows: [][]interface{}{{e.settings.mmapSize}}}
}

// AutoVacuum implements PRAGMA auto_vacuum (getter and setter) using
// SQLite's numbering (pragma.c getAutoVacuum): 0=NONE, 1=FULL,
// 2=INCREMENTAL. The mode is tracked per database in memory; actual
// incremental/full vacuuming is not performed (SQLite applies it at
// transaction commit).
func (e *Engine) AutoVacuum(schema, value string) *execpragma.Result {
	ctx := e.pragmaDBCtx(schema)
	if value != "" {
		return e.setAutoVacuum(ctx, value)
	}
	// Read path: report the EFFECTIVE mode (the pager's flag — restored
	// from header[52:56] at Open and applied at set-time only for empty
	// files), never a deferred request. SQLite reports
	// sqlite3BtreeGetAutoVacuum (the in-memory btree flag), which lags a
	// deferred auto_vacuum= assignment until VACUUM.
	return &execpragma.Result{Rows: [][]interface{}{{e.autoVacuumEffectiveMode(ctx)}}}
}

// setAutoVacuum applies a PRAGMA auto_vacuum assignment. Invalid values are
// silently ignored (SQLite logs but does not error — incrvacuum-1.4/1.7/2.1.x
// depend on the previous mode surviving).
func (e *Engine) setAutoVacuum(ctx *DatabaseContext, value string) *execpragma.Result {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "none", "off":
			n = 0
		case "full":
			n = 1
		case "incremental":
			n = 2
		default:
			// Invalid value: SQLite silently ignores it (logs a
			// warning via sqlite3_log, but does NOT return an error to
			// the caller). incrvacuum-1.4 / 1.7 / 2.1.x depend on
			// this — they set invalid values and expect the next
			// `pragma auto_vacuum` to return the previous mode.
			return &execpragma.Result{}
		}
	}
	if n < 0 || n > 2 {
		// Out-of-range number: same as above (silently ignored).
		return &execpragma.Result{}
	}
	if e.settings.autoVacuumModes == nil {
		e.settings.autoVacuumModes = make(map[string]int64)
	}
	e.settings.autoVacuumModes[e.autoVacuumSchemaName(ctx)] = int64(n)
	// Apply the mode to the pager — but only while the database file
	// is still empty (btree.c sqlite3BtreeSetAutoVacuum:3206: once
	// the file has content, BTS_PAGESIZE_FIXED makes a differing
	// mode change return SQLITE_READONLY, silently swallowed by
	// pragma.c, and the request is only recorded in db->nextAutovac
	// for the next VACUUM). For any non-empty file the persisted
	// header wins anyway (lockBtree:3419 re-reads the mode from
	// meta[4] at every open). Applying the mode immediately to a
	// non-empty database built without pointer-map pages mixes
	// geometries: the next commit drains/vacuum-moves pages of a
	// file whose page 2 is a data root, and corrupts it
	// (autovacuum-3.6/3.7).
	if ctx != nil && ctx.Pager != nil && ctx.Pager.NumPages() <= 1 {
		ctx.Pager.SetAutoVacuum(n > 0)
	}
	return &execpragma.Result{}
}

// autoVacuumSchemaName resolves the auto_vacuum settings key for the pragma
// target ("main" when the database is unresolved).
func (e *Engine) autoVacuumSchemaName(ctx *DatabaseContext) string {
	if ctx != nil && ctx.Name != "" {
		return ctx.Name
	}
	return "main"
}

// autoVacuumEffectiveMode reports the connection's effective auto_vacuum
// mode for the target: the pager's live flag (0/1) refined to FULL (1) vs
// INCREMENTAL (2) by the recorded setting.
func (e *Engine) autoVacuumEffectiveMode(ctx *DatabaseContext) int64 {
	mode := int64(0)
	if ctx == nil || ctx.Pager == nil || !ctx.Pager.AutoVacuum() {
		return mode
	}
	mode = 1
	if e.settings.autoVacuumModes != nil {
		if m, ok := e.settings.autoVacuumModes[e.autoVacuumSchemaName(ctx)]; ok && m != 0 {
			mode = m // FULL (1) vs INCREMENTAL (2)
		}
	}
	return mode
}

// --- Report pragmas ---

// LockStatus implements PRAGMA lock_status.
func (e *Engine) LockStatus() *execpragma.Result {
	return pragmaResult(e.execPragmaLockStatus())
}

// TableList implements the table-valued PRAGMA table_list materialization.
func (e *Engine) TableList() ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializeTableList(sql.TableRef{Name: "pragma_table_list"})
}

// Collations returns the names of the custom collations registered on the
// engine (built-ins BINARY/NOCASE/RTRIM are added by the handler).
func (e *Engine) Collations() []string {
	var names []string
	for c := range e.collations {
		names = append(names, c)
	}
	return names
}

// CompileOptions returns the compile-time options advertised by the engine
// (sqlite_compileoption_used/get and PRAGMA compile_options).
func (e *Engine) CompileOptions() []string {
	return function.CompileOptions
}

// DatabaseList implements PRAGMA database_list: one row per attached database
// in attachment order (main first), matching SQLite's output. The temp row
// (seq 1) appears only once the temp btree has been materialized.
func (e *Engine) DatabaseList() *execpragma.Result {
	var rows [][]interface{}
	// Main database first (seq 0). aDb[1] is the TEMP slot — reserved for
	// every connection even before the temp btree materializes — so the
	// first ATTACH lands at slot 2 (pragma.c PragTyp_DATABASE_LIST reports
	// each in-use aDb slot's index; misc8-4.1: "0 main ... 2 aux2 ...").
	rows = append(rows, []interface{}{int64(0), "main", e.mainDB.FilePath})
	// Temp row (slot 1) — listed only once its btree is materialized:
	// PragTyp_DATABASE_LIST skips aDb[i].pBt==0 entries, and the temp
	// btree opens lazily on first temp-schema use (attach4-1.2.1).
	if e.tempBtreeOpen {
		for _, ctx := range e.dbList {
			if u := strings.ToUpper(ctx.Name); u == "TEMP" || u == "TEMPORARY" {
				rows = append(rows, []interface{}{int64(1), "temp", ctx.FilePath})
				break
			}
		}
	}
	// Attached databases in ATTACH order starting at slot 2 (dbList
	// preserves attachment order; the databases map does not, so iterating
	// it would reorder rows non-deterministically).
	seq := int64(2)
	for _, ctx := range e.dbList {
		upper := strings.ToUpper(ctx.Name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		rows = append(rows, []interface{}{seq, ctx.Name, ctx.FilePath})
		seq++
	}
	return &execpragma.Result{Columns: []string{"seq", "name", "file"}, Rows: rows}
}

// --- Foreign key pragmas ---

// ForeignKeyCheck implements PRAGMA foreign_key_check, returning the
// violation rows (table, rowid, parent, fkid).
func (e *Engine) ForeignKeyCheck(table, schema string) ([][]interface{}, error) {
	viols, err := e.constraints.FindFKViolations(table, schema)
	if err != nil {
		return nil, err
	}
	rows := make([][]interface{}, 0, len(viols))
	for _, v := range viols {
		rows = append(rows, []interface{}{v.ChildTable, v.RowID, v.ParentTable, int64(v.FKID)})
	}
	return rows, nil
}

// ForeignKeyList implements PRAGMA foreign_key_list(table).
func (e *Engine) ForeignKeyList(table string) *execpragma.Result {
	return pragmaResult(e.execPragmaForeignKeyList(table))
}

// --- Index pragmas ---

// IndexInfo implements PRAGMA index_info / index_xinfo.
func (e *Engine) IndexInfo(name string, xinfo bool) *execpragma.Result {
	return pragmaResult(e.execPragmaIndexInfo(name, xinfo))
}

// IndexList implements PRAGMA index_list(table).
func (e *Engine) IndexList(table string) *execpragma.Result {
	return pragmaResult(e.execPragmaIndexList(table))
}

// --- Table pragmas ---

// TableInfo implements PRAGMA table_info / table_xinfo via the table-valued
// materialization path.
func (e *Engine) TableInfo(xinfo bool, table string) ([]sql.ColumnDef, [][]interface{}, error) {
	name := "pragma_table_info"
	if xinfo {
		name = "pragma_table_xinfo"
	}
	return e.materializeTableInfo(sql.TableRef{
		Name: name,
		Args: []sql.Expr{&sql.StringLit{Value: table}},
	})
}

// ColumnMetadata describes one column for sqlite3_table_column_metadata.
type ColumnMetadata struct {
	DeclType   string
	Collation  string
	NotNull    bool
	PrimaryKey bool
	AutoIncr   bool
	SchemaName string // owning schema (main / attached name)
}

// TableColumnMetadata implements sqlite3_table_column_metadata for a table
// column: resolves the schema-qualified table, finds the column (handling the
// rowid/oid/_rowid_ aliases), and reports its declared type, collation, NOT
// NULL, PRIMARY KEY, and AUTOINCREMENT flags (colmeta.test).
func (e *Engine) TableColumnMetadata(schemaName, table, column string) (*ColumnMetadata, error) {
	qualified := table
	if schemaName != "" && schemaName != "main" {
		qualified = schemaName + "." + table
	}
	entry, ctx, err := e.findTable(qualified)
	if err != nil {
		return nil, fmt.Errorf("no such table column: %s.%s", table, column)
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	// A column named rowid/oid/_rowid_ shadows the implicit alias.
	if md, ok := declaredColumnMetadata(ctx, colDefs, column); ok {
		return md, nil
	}
	// Implicit rowid alias: rowid/oid/_rowid_ on a rowid table reports as
	// INTEGER PRIMARY KEY (unless a WITHOUT ROWID table, which has no rowid).
	if execquery.IsRowIDName(column) {
		return rowidColumnMetadata(entry, ctx, colDefs, table, column)
	}
	return nil, fmt.Errorf("no such table column: %s.%s", table, column)
}

// declaredColumnMetadata reports one declared column's metadata ("BINARY"
// collation when the column declares none).
func declaredColumnMetadata(ctx *DatabaseContext, colDefs []sql.ColumnDef, column string) (*ColumnMetadata, bool) {
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, column) {
			coll := cd.Collate
			if coll == "" {
				coll = "BINARY"
			}
			return &ColumnMetadata{
				DeclType:   cd.Type,
				Collation:  coll,
				NotNull:    cd.NotNull,
				PrimaryKey: cd.PrimaryKey,
				AutoIncr:   cd.AutoInc,
				SchemaName: ctx.Name,
			}, true
		}
	}
	return nil, false
}

// rowidColumnMetadata reports the implicit rowid alias's metadata
// (INTEGER PRIMARY KEY). A WITHOUT ROWID table has no rowid; a table with an
// INTEGER PRIMARY KEY AUTOINCREMENT column reports autoincrement=1 for its
// rowid alias (colmeta.test 101/102).
func rowidColumnMetadata(entry *schema.Entry, ctx *DatabaseContext, colDefs []sql.ColumnDef, table, column string) (*ColumnMetadata, error) {
	if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return nil, fmt.Errorf("no such table column: %s.%s", table, column)
	}
	autoIncr := false
	for _, cd := range colDefs {
		if cd.AutoInc {
			autoIncr = true
			break
		}
	}
	return &ColumnMetadata{
		DeclType:   "INTEGER",
		Collation:  "BINARY",
		PrimaryKey: true,
		AutoIncr:   autoIncr,
		SchemaName: ctx.Name,
	}, nil
}

// --- Integrity pragmas ---

// QuickCheck implements PRAGMA quick_check / integrity_check.
func (e *Engine) QuickCheck(table string) *execpragma.Result {
	return pragmaResult(e.execQuickCheck(table))
}

// --- Encoding ---

// Encoding implements PRAGMA encoding: the setter persists the encoding to
// the header; the getter reports the engine's current encoding string.
func (e *Engine) Encoding(schema, value string) *execpragma.Result {
	if value != "" {
		return pragmaResult(e.assignPragmaEncoding(e.pragmaDBCtx(schema), value))
	}
	return &execpragma.Result{Rows: [][]interface{}{{e.encoding}}}
}

// --- Boolean engine flags ---

// LegacyAlterTable reports the legacy_alter_table flag.
func (e *Engine) LegacyAlterTable() bool { return e.settings.legacyAlterTable }

// SetLegacyAlterTable sets the legacy_alter_table flag.
func (e *Engine) SetLegacyAlterTable(b bool) { e.settings.legacyAlterTable = b }

// RecursiveTriggers reports the recursive_triggers flag.
func (e *Engine) RecursiveTriggers() bool { return e.settings.recursiveTriggers }

// SetRecursiveTriggers sets the recursive_triggers flag.
func (e *Engine) SetRecursiveTriggers(b bool) { e.settings.recursiveTriggers = b }

// IgnoreCheckConstraints reports the ignore_check_constraints flag.
func (e *Engine) IgnoreCheckConstraints() bool { return e.settings.ignoreCheckConstraints }

// SetIgnoreCheckConstraints sets the ignore_check_constraints flag.
func (e *Engine) SetIgnoreCheckConstraints(b bool) { e.settings.ignoreCheckConstraints = b }

// ForeignKeys reports the foreign_keys flag.
func (e *Engine) ForeignKeys() bool { return e.settings.foreignKeys }

// SetForeignKeys sets the foreign_keys flag.
func (e *Engine) SetForeignKeys(b bool) { e.settings.foreignKeys = b }

// ColumnLimit reports the SQLITE_LIMIT_COLUMN setting.
func (e *Engine) ColumnLimit() int { return e.settings.columnLimit }

// LengthLimit reports the SQLITE_LIMIT_LENGTH setting.
func (e *Engine) LengthLimit() int { return e.settings.lengthLimit }

// DeferForeignKeys reports the defer_foreign_keys flag.
func (e *Engine) DeferForeignKeys() bool { return e.settings.deferForeignKeys }

// SetDeferForeignKeys sets the defer_foreign_keys flag.
func (e *Engine) SetDeferForeignKeys(b bool) { e.settings.deferForeignKeys = b }

// InTransaction reports whether the engine is inside a transaction.
func (e *Engine) InTransaction() bool { return e.tx.inTransaction }

// WritableSchema reports the writable_schema flag.
func (e *Engine) WritableSchema() bool { return e.settings.writableSchema }

// SetWritableSchema sets the writable_schema flag.
func (e *Engine) SetWritableSchema(b bool) { e.settings.writableSchema = b }

// QueryOnly reports the query_only flag.
func (e *Engine) QueryOnly() bool { return e.settings.queryOnly }

// SetQueryOnly sets the query_only flag.
func (e *Engine) SetQueryOnly(b bool) { e.settings.queryOnly = b }

// ShortColumnNames reports the short_column_names flag.
func (e *Engine) ShortColumnNames() bool { return e.settings.shortColumnNames }

// SetShortColumnNames sets the short_column_names flag.
func (e *Engine) SetShortColumnNames(b bool) { e.settings.shortColumnNames = b }

// FullColumnNames reports the full_column_names flag.
func (e *Engine) FullColumnNames() bool { return e.settings.fullColumnNames }

// SetFullColumnNames sets the full_column_names flag.
func (e *Engine) SetFullColumnNames(b bool) { e.settings.fullColumnNames = b }

// ReverseUnorderedSelects reports the reverse_unordered_selects flag.
func (e *Engine) ReverseUnorderedSelects() bool { return e.settings.reverseUnordered }

// SetReverseUnorderedSelects sets the reverse_unordered_selects flag.
func (e *Engine) SetReverseUnorderedSelects(b bool) { e.settings.reverseUnordered = b }

// CountChanges reports the count_changes flag.
func (e *Engine) CountChanges() bool { return e.settings.countChanges }

// SetCountChanges sets the count_changes flag.
func (e *Engine) SetCountChanges(b bool) { e.settings.countChanges = b }

// TrustedSchema reports the trusted_schema flag.
func (e *Engine) TrustedSchema() bool { return e.settings.trustedSchema }

// SetTrustedSchema sets the trusted_schema flag.
func (e *Engine) SetTrustedSchema(b bool) { e.settings.trustedSchema = b }

// CaseSensitiveLike reports the case_sensitive_like flag.
func (e *Engine) CaseSensitiveLike() bool { return e.settings.caseSensitiveLike }

// SetCaseSensitiveLike sets the case_sensitive_like flag.
func (e *Engine) SetCaseSensitiveLike(b bool) { e.settings.caseSensitiveLike = b }

// SkipScanEnabled reports whether the skip-scan query optimization is on.
