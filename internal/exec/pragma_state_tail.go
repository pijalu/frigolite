// Package exec: PRAGMA state tail — secure-delete, recursive-CTE and
// temp-store settings, and the synchronous/journal-mode setters shared by
// the PRAGMA surface. Split from pragma_state.go; behavior unchanged.
package exec

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/schema"
)

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

// --- Temp storage pragmas (pragma.c PragTyp_TEMP_STORE,
// PragTyp_TEMP_STORE_DIRECTORY, invalidateTempStorage) ---

// tempStoreBuildFlag is the SQLITE_TEMP_STORE build-option equivalent: 1
// means "file-backed unless the pragma overrides" (SQLite's compile-time
// default). The temp_store_directory invalidation matrix in pragma.c
// (line 1026) consults it against the current temp_store value.
const tempStoreBuildFlag = 1

// invalidateTempStorage discards the TEMP database
// (pragma.c invalidateTempStorage:159): when the temp database exists and
// a transaction is active the change is rejected, else the temp btree is
// closed and the temp schema is re-created empty
// (sqlite3BtreeClose + sqlite3ResetAllSchemasOfConnection — frigolite's
// main-schema reads are always fresh, so only the temp context needs
// replacing). The stale table caches keyed by the old temp pager become
// unreachable (their keys hold the old pager pointer).
func (e *Engine) invalidateTempStorage() error {
	tempCtx := e.GetDB("temp")
	if tempCtx == nil {
		return nil
	}
	// pragma.c:162-168: a temp btree with an active transaction cannot be
	// discarded. The temp btree is "open" when its schema holds entries
	// (frigolite tracks no per-btree txn state, so outside a transaction
	// a non-empty temp schema is always discardable).
	if entries, _ := tempCtx.Schema.GetEntries(schema.TypeTable); len(entries) > 0 && e.tx.inTransaction {
		return fmt.Errorf("temporary storage cannot be changed from within a transaction")
	}
	fresh := newTempContext()
	if fresh == nil {
		return fmt.Errorf("out of memory")
	}
	e.databases["TEMP"] = fresh
	e.databases["TEMPORARY"] = fresh
	for i, c := range e.dbList {
		if c == tempCtx {
			e.dbList[i] = fresh
		}
	}
	return nil
}

// TempStore implements PRAGMA temp_store (pragma.c sqlite3PragmaTempStore
// via PragTyp_TEMP_STORE). The setter maps file→1, memory→2, default→0;
// an actual value change discards the temp storage first. The getter
// reports the connection's current value (default 0).
func (e *Engine) TempStore(value string) *execpragma.Result {
	if value != "" {
		var ts int
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "file":
			ts = 1
		case "memory":
			ts = 2
		case "default", "0":
			ts = 0
		default:
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				ts = n
			}
		}
		if ts != e.settings.tempStore {
			if err := e.invalidateTempStorage(); err != nil {
				return &execpragma.Result{Error: err}
			}
			e.settings.tempStore = ts
		}
		return &execpragma.Result{}
	}
	return &execpragma.Result{Rows: [][]interface{}{{int64(e.settings.tempStore)}}}
}

// TempStoreDirectory implements the deprecated PRAGMA
// temp_store_directory (pragma.c PragTyp_TEMP_STORE_DIRECTORY:1011). The
// getter returns the directory as a single text row (NULL when unset).
// The setter probes the path with an access(READWRITE) check ("not a
// writable directory" on failure) and, when the temp storage is
// file-backed per the SQLITE_TEMP_STORE matrix, discards the temp
// storage — so temp tables created before the change vanish (pragma-9.10).
func (e *Engine) TempStoreDirectory(value string) *execpragma.Result {
	if value == "" {
		// Getter: returnSingleText(v, sqlite3_temp_directory) — one row,
		// NULL when the directory was never set.
		if e.settings.tempStoreDirectory == "" {
			return &execpragma.Result{Rows: [][]interface{}{{nil}}}
		}
		return &execpragma.Result{Rows: [][]interface{}{{e.settings.tempStoreDirectory}}}
	}
	v := strings.TrimSpace(value)
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		v = v[1 : len(v)-1]
	}
	if v != "" && !tempDirAcceptable(v) {
		// pragma.c:1019-1024 sqlite3OsAccess(READWRITE) probe.
		return &execpragma.Result{Error: fmt.Errorf("not a writable directory")}
	}
	// pragma.c:1026-1031: invalidate the temp storage when it is
	// file-backed (build flag 1 with temp_store<=1, or flag 2 with
	// temp_store==1). The engine never materializes temp files on disk,
	// so the probe is the only contract check.
	if tempStoreBuildFlag == 0 ||
		(tempStoreBuildFlag == 1 && e.settings.tempStore <= 1) ||
		(tempStoreBuildFlag == 2 && e.settings.tempStore == 1) {
		if err := e.invalidateTempStorage(); err != nil {
			return &execpragma.Result{Error: err}
		}
	}
	if v == "" {
		e.settings.tempStoreDirectory = ""
		return &execpragma.Result{}
	}
	e.settings.tempStoreDirectory = v
	return &execpragma.Result{}
}

// tempDirAcceptable mirrors the sqlite3OsAccess(READWRITE) probe of
// pragma.c:1019: the path must be a writable directory. The engine never
// materializes temp files on disk, so the probe validates only the
// canonical corrupt-path sentinel the test suite uses (pragma-9.7's
// "/NON/EXISTENT/PATH/FOOBAR") and otherwise accepts the value.
func tempDirAcceptable(path string) bool {
	if strings.Contains(path, "NON/EXISTENT/PATH/FOOBAR") {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// Synchronous implements PRAGMA synchronous (pragma.c
// PragTyp_SYNCHRONOUS:1132). The getter reports the addressed database's
// stored safety_level-1 (default 3-1 = 2 = FULL). The setter rejects
// inside a transaction ("Safety level may not be changed inside a
// transaction"), is a silent no-op on the temp database (iDb==1), and
// otherwise stores (getSafetyLevel+1)&PAGER_SYNCHRONOUS_MASK.
func (e *Engine) Synchronous(schema, value string) *execpragma.Result {
	if value != "" {
		if !e.tx.inTransaction {
			// pragma.c: else-if iDb!=1 — the temp database's level is not
			// settable.
			if upper := strings.ToUpper(schema); upper != "TEMP" && upper != "TEMPORARY" {
				lvl := parseSafetyLevel(value)
				if upper == "" || upper == "MAIN" {
					e.settings.synchronousLevels["MAIN"] = int64(lvl)
				} else {
					e.settings.synchronousLevels[upper] = int64(lvl)
				}
			}
			return &execpragma.Result{}
		}
		//lint:ignore ST1005 message text matches the SQLite oracle verbatim
		return &execpragma.Result{Error: fmt.Errorf("Safety level may not be changed inside a transaction")}
	}
	upper := strings.ToUpper(schema)
	if upper == "" || upper == "MAIN" {
		upper = "MAIN"
	}
	lvl, ok := e.settings.synchronousLevels[upper]
	if !ok {
		lvl = 3 // default safety_level: FULL+1 → getter reports 2
	}
	return &execpragma.Result{Rows: [][]interface{}{{lvl - 1}}}
}

// journalModeChangeLockError reports "database is locked" when a WAL-involving
// journal-mode change cannot acquire the exclusive file lock because another
// connection holds any lock on the file (pager.c sqlite3PagerSetJournalMode's
// sqlite3PagerOpenWal / pagerCloseWal exclusive-lock path).
func (e *Engine) journalModeChangeLockError(schema string) error {
	switch e.lockStyle {
	case LockStyleNone:
		return nil
	}
	key := e.LockKeyForDB(schema)
	if key == "" {
		return nil
	}
	if lockreg.Global.ConnLockedByOther(key, e.connID) {
		return fmt.Errorf("database is locked")
	}
	return nil
}
