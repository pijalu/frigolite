package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
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
		m := strings.ToLower(strings.TrimSpace(value))
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
			cur := ctx.Pager.JournalMode()
			if cur == "" {
				cur = "delete"
			}
			return &execpragma.Result{Rows: [][]interface{}{{cur}}}
		}
		if err := ctx.Pager.SetJournalMode(m); err != nil {
			return &execpragma.Result{Error: err}
		}
		mode := ctx.Pager.JournalMode()
		if mode == "" {
			mode = "delete"
		}
		return &execpragma.Result{Rows: [][]interface{}{{mode}}}
	}
	mode := ctx.Pager.JournalMode()
	if mode == "" {
		mode = "delete"
	}
	return &execpragma.Result{Rows: [][]interface{}{{mode}}}
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

// LockingMode implements PRAGMA locking_mode (getter and setter). SQLite tracks
// it per database but the value is a connection-level lock model; the setter
// echoes the new mode as a result row (pragma.c PragTyp_LOCKING_MODE).
func (e *Engine) LockingMode(schema, value string) *execpragma.Result {
	if value != "" {
		m := strings.ToLower(strings.TrimSpace(value))
		switch m {
		case "normal", "exclusive":
			e.lockingMode = m
		default:
			// Unrecognised token: leave the current mode unchanged (no error),
			// matching SQLite's lenient handling of invalid pragma values.
		}
	}
	return &execpragma.Result{Rows: [][]interface{}{{e.currentLockingMode()}}}
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
	if err := ctx.Pager.CheckpointMode(mode); err != nil {
		return &execpragma.Result{Error: err}
	}
	return &execpragma.Result{Rows: [][]interface{}{{0, 0, 0}}}
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
		name := "main"
		if ctx != nil && ctx.Name != "" {
			name = ctx.Name
		}
		e.settings.autoVacuumModes[name] = int64(n)
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
	// Read path: report the EFFECTIVE mode (the pager's flag — restored
	// from header[52:56] at Open and applied at set-time only for empty
	// files), never a deferred request. SQLite reports
	// sqlite3BtreeGetAutoVacuum (the in-memory btree flag), which lags a
	// deferred auto_vacuum= assignment until VACUUM.
	name := "main"
	if ctx != nil && ctx.Name != "" {
		name = ctx.Name
	}
	mode := int64(0)
	if ctx != nil && ctx.Pager != nil && ctx.Pager.AutoVacuum() {
		mode = 1
		if e.settings.autoVacuumModes != nil {
			if m, ok := e.settings.autoVacuumModes[name]; ok && m != 0 {
				mode = m // FULL (1) vs INCREMENTAL (2)
			}
		}
	}
	return &execpragma.Result{Rows: [][]interface{}{{mode}}}
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
// in attachment order (main first), matching SQLite's output. SQLite always
// reserves seq 1 for the temp database (whether or not it has been opened),
// so attached databases start at seq 2.
func (e *Engine) DatabaseList() *execpragma.Result {
	var rows [][]interface{}
	seq := int64(0)
	// Main database first (seq 0), then attached databases in ATTACH
	// order (dbList preserves attachment order; the databases map does
	// not, so iterating it would reorder rows non-deterministically).
	rows = append(rows, []interface{}{seq, "main", e.mainDB.FilePath})
	seq++
	// Temp database at seq 1 — always present in SQLite's database_list.
	tempPath := ""
	for _, ctx := range e.dbList {
		upper := strings.ToUpper(ctx.Name)
		if upper == "TEMP" || upper == "TEMPORARY" {
			tempPath = ctx.FilePath
			break
		}
	}
	rows = append(rows, []interface{}{seq, "temp", tempPath})
	seq++
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
			}, nil
		}
	}
	// Implicit rowid alias: rowid/oid/_rowid_ on a rowid table reports as
	// INTEGER PRIMARY KEY (unless a WITHOUT ROWID table, which has no rowid).
	if execquery.IsRowIDName(column) {
		if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
			return nil, fmt.Errorf("no such table column: %s.%s", table, column)
		}
		// A table with an INTEGER PRIMARY KEY AUTOINCREMENT column reports
		// autoincrement=1 for its rowid alias (colmeta.test 101/102).
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
	return nil, fmt.Errorf("no such table column: %s.%s", table, column)
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
// Mirrors SQLite's SQLITE_SkipScan optimization_control bit. Toggle via
// PRAGMA skip_scan = 0|1; used by skip-scan tests to verify the alternative
// plan when the optimization is disabled.
func (e *Engine) SkipScanEnabled() bool { return e.settings.skipScanEnabled }

// SetSkipScanEnabled enables or disables the skip-scan query optimization.
func (e *Engine) SetSkipScanEnabled(b bool) { e.settings.skipScanEnabled = b }

// DefaultSecureDelete reports the connection-wide PRAGMA secure_delete
// default. New attached DBs inherit MAIN's current value at ATTACH time
// (execddl.execAttach) — this is the build-option equivalent of
// SQLITE_FAST_SECURE_DELETE (test/securedel.test DEFAULT_SECDEL=2).
func (e *Engine) DefaultSecureDelete() int64 { return e.settings.defaultSecureDelete }

// MainSecureDelete returns MAIN's current per-schema secure_delete value.
// Used by ATTACH to seed the new DB's value from MAIN's current state
// (mirrors src/attach.c:207-208 sqlite3BtreeSecureDelete inheritance from
// db->aDb[0].pBt).
func (e *Engine) MainSecureDelete() int64 {
	return e.settings.mainSecureDelete
}

// SetPerSchemaSecureDelete sets the per-schema secure_delete value used by
// PRAGMA [schema].secure_delete. execddl.execAttach calls this to record
// the inherited value for a newly attached DB.
func (e *Engine) SetPerSchemaSecureDelete(schemaUpper string, v int64) {
	e.settings.secureDeletes[strings.ToUpper(schemaUpper)] = v
}

// SecureDelete implements PRAGMA secure_delete (getter and setter). Mirrors
// src/pragma.c PragTyp_SECURE_DELETE and src/attach.c sqlite3BtreeSecureDelete
// inheritance from main (Btree-level tracking, per-DB).
//
//   - Setter with no schema: sets every attached DB to the value (pragma.c
//     pId2->n==0 && b>=0 branch), including MAIN.
//   - Setter with a schema: updates only that schema.
//   - Getter with no schema: returns MAIN's value (pragma.c returnSingleInt
//     reads pDb->pBt where pDb defaults to main when pId2 is empty).
//   - Getter with a schema: returns that schema's value, or MAIN's value when
//     the schema has no explicit entry (mirrors the Btree's per-DB tracking —
//     the new Btree inherits MAIN's value at attach time, so newly attached DBs
//     always have a value).
func (e *Engine) SecureDelete(schema, value string) *execpragma.Result {
	if value != "" {
		v, err := parseSecureDeleteValue(value)
		if err != nil {
			return &execpragma.Result{Error: err}
		}
		upper := strings.ToUpper(schema)
		if schema == "" {
			// No schema → set MAIN and every attached DB (pragma.c
			// pId2->n==0 branch).
			e.settings.mainSecureDelete = v
			for k := range e.settings.secureDeletes {
				e.settings.secureDeletes[k] = v
			}
		} else if upper == "MAIN" {
			e.settings.mainSecureDelete = v
		} else {
			e.settings.secureDeletes[upper] = v
		}
	}
	if schema == "" {
		// Getter with no schema: return MAIN's value (pragma.c returns the
		// pDb->pBt result; pDb is main when pId2 is empty).
		return &execpragma.Result{Rows: [][]interface{}{{e.settings.mainSecureDelete}}}
	}
	upper := strings.ToUpper(schema)
	if upper == "MAIN" {
		return &execpragma.Result{Rows: [][]interface{}{{e.settings.mainSecureDelete}}}
	}
	v, ok := e.settings.secureDeletes[upper]
	if !ok {
		// Schema was never explicitly set: inherit MAIN's value (mirrors the
		// Btree's per-DB tracking — the Btree was seeded with MAIN's setting
		// at attach time).
		v = e.settings.mainSecureDelete
	}
	return &execpragma.Result{Rows: [][]interface{}{{v}}}
}

// parseSecureDeleteValue converts a PRAGMA value string to the secure_delete
// int encoding: 0=OFF, 1=ON, 2=FAST. SQLite's "DEFAULT" maps to the current
// connection default; we collapse it to 0 (OFF).
func parseSecureDeleteValue(value string) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "no", "false", "0":
		return 0, nil
	case "on", "yes", "true", "1":
		return 1, nil
	case "fast", "2":
		return 2, nil
	case "default":
		return 0, nil
	}
	return 0, fmt.Errorf("unsupported secure_delete value: %q", value)
}

// --- Scalar settings ---

// RecursiveCTELimit reports the recursive CTE iteration limit.
func (e *Engine) RecursiveCTELimit() int { return e.settings.recursiveCTELimit }

// SetRecursiveCTELimit sets the recursive CTE iteration limit.
func (e *Engine) SetRecursiveCTELimit(n int) { e.settings.recursiveCTELimit = n }
