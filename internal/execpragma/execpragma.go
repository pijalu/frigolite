// Package execpragma isolates PRAGMA statement dispatch from the query
// execution engine.
//
// The engine implements the EngineState capability interface (the minimal
// surface pragma handlers need) and registers pragma handlers in a Registry
// keyed by pragma name. The dispatch logic — finding the handler, converting
// the statement, returning the result — lives here so adding a pragma means
// adding a map entry, never editing dispatch code (Open/Closed).
package execpragma

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Result is the outcome of a pragma execution: column names, data rows, and
// an optional error. It is a deliberately minimal subset of the engine's
// result type so this package does not depend on internal/exec.
type Result struct {
	Columns []string
	Rows    [][]interface{}
	Error   error
}

// EngineState is the capability interface a PRAGMA statement may exercise.
// It is the minimal surface the pragma handlers need: header-backed pragmas
// (schema-qualified), cache/report pragmas, boolean engine flags, and a few
// scalar settings. Schema qualifiers are passed as strings ("", "main",
// "temp", or an attached database name); the implementing engine resolves
// them to its database contexts internally.
type EngineState interface {
	// Header-backed pragmas (getter when value is "", setter otherwise).
	DataVersion(schema string) *Result
	DefaultCacheSize(schema, value string) *Result
	UserVersion(schema, value string) *Result
	ApplicationID(schema, value string) *Result
	SchemaVersion(schema, value string) *Result
	PageSize(schema, value string) *Result

	// PageCount returns the current number of pages in the named schema's
	// database (PRAGMA page_count).
	PageCount(schema string) int64

	// Cache pragmas.
	CacheSize(schema, value string) *Result
	CacheSpill(schema, value string) *Result

	// AutoVacuum returns or sets the auto_vacuum mode (pragma.c
	// getAutoVacuum: 0=none, 1=full, 2=incremental) for a schema. The value
	// is tracked in memory; actual incremental vacuuming is not performed.
	AutoVacuum(schema, value string) *Result

	// IncrementalVacuum performs PRAGMA incremental_vacuum on a schema,
	// following btree.c sqlite3BtreeIncrVacuum (corruption guard on the
	// header freelist count, then the vacuum step work).
	IncrementalVacuum(schema string) *Result

	// Report pragmas.
	LockStatus() *Result
	TableList() ([]sql.ColumnDef, [][]interface{}, error)
	Collations() []string
	DatabaseList() *Result
	CompileOptions() []string

	// Foreign key pragmas.
	ForeignKeyCheck(table, schema string) ([][]interface{}, error)
	ForeignKeyList(table string) *Result

	// Index pragmas.
	IndexInfo(name string, xinfo bool) *Result
	IndexList(table string) *Result

	// Table pragmas.
	TableInfo(xinfo bool, table string) ([]sql.ColumnDef, [][]interface{}, error)

	// Integrity pragmas.
	QuickCheck(table string) *Result

	// Encoding.
	Encoding(schema, value string) *Result

	// Boolean engine flags.
	LegacyAlterTable() bool
	SetLegacyAlterTable(b bool)
	RecursiveTriggers() bool
	SetRecursiveTriggers(b bool)
	IgnoreCheckConstraints() bool
	SetIgnoreCheckConstraints(b bool)
	ForeignKeys() bool
	SetForeignKeys(b bool)
	DeferForeignKeys() bool
	SetDeferForeignKeys(b bool)
	InTransaction() bool
	WritableSchema() bool
	SetWritableSchema(b bool)
	QueryOnly() bool
	SetQueryOnly(b bool)
	ShortColumnNames() bool
	SetShortColumnNames(b bool)
	FullColumnNames() bool
	SetFullColumnNames(b bool)
	ReverseUnorderedSelects() bool
	SetReverseUnorderedSelects(b bool)
	CountChanges() bool
	SetCountChanges(b bool)
	CaseSensitiveLike() bool
	SetCaseSensitiveLike(b bool)
	TrustedSchema() bool
	SetTrustedSchema(b bool)

	// Scalar settings.
	RecursiveCTELimit() int
	SetRecursiveCTELimit(n int)
}

// Handler executes one PRAGMA statement against the engine state. Handlers
// are registered in the pragma handlers map keyed by the uppercased pragma
// name; each handler implements both the getter (no value) and setter (value)
// forms.
type Handler func(st EngineState, s *sql.PragmaStmt) *Result

// Registry dispatches PRAGMA statements to registered handlers.
type Registry struct {
	handlers map[string]Handler
}

// New creates a Registry with all supported pragma handlers registered.
func New() *Registry {
	r := &Registry{handlers: make(map[string]Handler, len(pragmaHandlers))}
	for name, h := range pragmaHandlers {
		r.handlers[name] = h
	}
	return r
}

// Register adds or replaces the handler for a pragma name (uppercased).
// It is the Open/Closed seam: adding a pragma means registering a handler,
// never editing Handle.
func (r *Registry) Register(name string, h Handler) {
	r.handlers[strings.ToUpper(name)] = h
}

// Handle dispatches a PRAGMA statement to its registered handler. Unknown
// pragma names return an empty result, matching SQLite's behavior for
// pragmas it does not recognize.
func (r *Registry) Handle(st EngineState, s *sql.PragmaStmt) *Result {
	if h, ok := r.handlers[strings.ToUpper(s.Name)]; ok {
		return h(st, s)
	}
	return &Result{}
}

// pragmaGetOnly wraps a getter-only pragma: a setter form (a value) is a no-op
// returning an empty result, matching the current engine behavior.
func pragmaGetOnly(get func(st EngineState) *Result) Handler {
	return func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			return &Result{}
		}
		return get(st)
	}
}

// pragmaBoolHandler builds a boolean engine-flag pragma: the getter reports
// the flag as 0/1 and the setter parses the value with boolPragma.
func pragmaBoolHandler(get func(st EngineState) bool, set func(st EngineState, b bool)) Handler {
	return func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			set(st, boolPragma(s.Value))
			return &Result{}
		}
		return &Result{Rows: [][]interface{}{{boolToInt(get(st))}}}
	}
}

// boolPragma reports whether a pragma value string is truthy.
func boolPragma(v string) bool {
	return v == "1" || strings.EqualFold(v, "ON") || strings.EqualFold(v, "TRUE")
}

// boolToInt converts a boolean to an integer (0 or 1).
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// pragmaHandlers maps every supported PRAGMA name to its handler. The map is
// the Open/Closed seam: adding a pragma means adding an entry, never editing
// the dispatch logic.
var pragmaHandlers = map[string]Handler{
	// --- Header-backed pragmas (getter + setter) ---
	"DATA_VERSION": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.DataVersion(s.Schema)
	},
	"DEFAULT_CACHE_SIZE": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.DefaultCacheSize(s.Schema, s.Value)
	},
	"USER_VERSION": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.UserVersion(s.Schema, s.Value)
	},
	"APPLICATION_ID": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.ApplicationID(s.Schema, s.Value)
	},
	"SCHEMA_VERSION": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.SchemaVersion(s.Schema, s.Value)
	},
	"PAGE_SIZE": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.PageSize(s.Schema, s.Value)
	},

	// --- Cache pragmas ---
	"CACHE_SIZE": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.CacheSize(s.Schema, s.Value)
	},
	"CACHE_SPILL": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.CacheSpill(s.Schema, s.Value)
	},

	// --- Report pragmas ---
	"LOCK_STATUS": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.LockStatus()
	},
	"TABLE_LIST": func(st EngineState, s *sql.PragmaStmt) *Result {
		cols, rows, err := st.TableList()
		if err != nil {
			return &Result{Error: err}
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return &Result{Columns: names, Rows: rows}
	},
	"COLLATION_LIST": func(st EngineState, s *sql.PragmaStmt) *Result {
		var rows [][]interface{}
		seq := int64(0)
		for _, c := range []string{"BINARY", "NOCASE", "RTRIM"} {
			rows = append(rows, []interface{}{seq, c})
			seq++
		}
		for _, c := range st.Collations() {
			rows = append(rows, []interface{}{seq, c})
			seq++
		}
		return &Result{Columns: []string{"seq", "name"}, Rows: rows}
	},

	// --- Foreign key pragmas (value is a table name / arg) ---
	"FOREIGN_KEY_CHECK": func(st EngineState, s *sql.PragmaStmt) *Result {
		rows, err := st.ForeignKeyCheck(s.Value, s.Schema)
		if err != nil {
			return &Result{Error: err}
		}
		return &Result{Columns: []string{"table", "rowid", "parent", "fkid"}, Rows: rows}
	},
	"FOREIGN_KEY_LIST": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.ForeignKeyList(s.Value)
	},

	// --- Index pragmas (value is an index/table name) ---
	"INDEX_INFO": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.IndexInfo(s.Value, false)
	},
	"INDEX_XINFO": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.IndexInfo(s.Value, true)
	},
	"INDEX_LIST": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.IndexList(s.Value)
	},

	// --- Table pragmas (value is a table name) ---
	"TABLE_INFO": func(st EngineState, s *sql.PragmaStmt) *Result {
		cols, rows, err := st.TableInfo(false, s.Value)
		if err != nil {
			return &Result{Error: err}
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return &Result{Columns: names, Rows: rows}
	},
	"TABLE_XINFO": func(st EngineState, s *sql.PragmaStmt) *Result {
		cols, rows, err := st.TableInfo(true, s.Value)
		if err != nil {
			return &Result{Error: err}
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return &Result{Columns: names, Rows: rows}
	},

	// --- Integrity pragmas (value is an optional arg) ---
	"QUICK_CHECK": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.QuickCheck(s.Value)
	},
	"INTEGRITY_CHECK": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.QuickCheck(s.Value)
	},

	// --- Boolean engine-flag pragmas ---
	"LEGACY_ALTER_TABLE": pragmaBoolHandler(
		func(st EngineState) bool { return st.LegacyAlterTable() },
		func(st EngineState, b bool) { st.SetLegacyAlterTable(b) },
	),
	"RECURSIVE_TRIGGERS": pragmaBoolHandler(
		func(st EngineState) bool { return st.RecursiveTriggers() },
		func(st EngineState, b bool) { st.SetRecursiveTriggers(b) },
	),
	"IGNORE_CHECK_CONSTRAINTS": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			st.SetIgnoreCheckConstraints(boolPragma(s.Value))
			return &Result{}
		}
		return &Result{}
	},
	"FOREIGN_KEYS": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			// SQLite: enabling/disabling foreign key constraints inside a
			// transaction has NO effect (R-46649-58537). The setter is only
			// honored outside a transaction (autocommit mode).
			if !st.InTransaction() {
				st.SetForeignKeys(boolPragma(s.Value))
			}
			return &Result{}
		}
		return &Result{Rows: [][]interface{}{{boolToInt(st.ForeignKeys())}}}
	},
	"DEFER_FOREIGN_KEYS": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			if st.DeferForeignKeys() && !st.InTransaction() {
				return &Result{Error: fmt.Errorf("defer_foreign_keys only supported inside a transaction")}
			}
			st.SetDeferForeignKeys(boolPragma(s.Value))
			return &Result{}
		}
		return &Result{Rows: [][]interface{}{{boolToInt(st.DeferForeignKeys())}}}
	},
	"WRITABLE_SCHEMA": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			st.SetWritableSchema(boolPragma(s.Value))
			return &Result{}
		}
		return &Result{}
	},
	"QUERY_ONLY": pragmaBoolHandler(
		func(st EngineState) bool { return st.QueryOnly() },
		func(st EngineState, b bool) { st.SetQueryOnly(b) },
	),
	"SHORT_COLUMN_NAMES": pragmaBoolHandler(
		func(st EngineState) bool { return st.ShortColumnNames() },
		func(st EngineState, b bool) { st.SetShortColumnNames(b) },
	),
	"FULL_COLUMN_NAMES": pragmaBoolHandler(
		func(st EngineState) bool { return st.FullColumnNames() },
		func(st EngineState, b bool) { st.SetFullColumnNames(b) },
	),
	"REVERSE_UNORDERED_SELECTS": pragmaBoolHandler(
		func(st EngineState) bool { return st.ReverseUnorderedSelects() },
		func(st EngineState, b bool) { st.SetReverseUnorderedSelects(b) },
	),
	"COUNT_CHANGES": pragmaBoolHandler(
		func(st EngineState) bool { return st.CountChanges() },
		func(st EngineState, b bool) { st.SetCountChanges(b) },
	),
	"CASE_SENSITIVE_LIKE": pragmaBoolHandler(
		func(st EngineState) bool { return st.CaseSensitiveLike() },
		func(st EngineState, b bool) { st.SetCaseSensitiveLike(b) },
	),
	"TRUSTED_SCHEMA": pragmaBoolHandler(
		func(st EngineState) bool { return st.TrustedSchema() },
		func(st EngineState, b bool) { st.SetTrustedSchema(b) },
	),

	// --- Encoding / journal mode / limits ---
	"ENCODING": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.Encoding(s.Schema, s.Value)
	},
	"JOURNAL_MODE": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			mode := strings.ToLower(s.Value)
			switch mode {
			case "delete", "truncate", "persist", "memory", "off", "wal", "wal2":
			default:
				return &Result{Error: fmt.Errorf("unsupported journal mode: %s", s.Value)}
			}
			return &Result{Rows: [][]interface{}{{mode}}}
		}
		return &Result{Rows: [][]interface{}{{"memory"}}}
	},
	"RECURSIVE_CTE_LIMIT": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value == "" {
			return &Result{}
		}
		if n, err := strconv.Atoi(s.Value); err == nil && n >= 0 {
			st.SetRecursiveCTELimit(n)
		}
		return &Result{Rows: [][]interface{}{{int64(st.RecursiveCTELimit())}}}
	},
	"MMAP_SIZE": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value == "" {
			return &Result{}
		}
		return &Result{Rows: [][]interface{}{{int64(0)}}}
	},

	// --- Simple getters (setter form is a no-op) ---
	"DATABASE_VERSION": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(1)}}}
	}),
	"PAGE_COUNT": func(st EngineState, s *sql.PragmaStmt) *Result {
		if s.Value != "" {
			return &Result{}
		}
		// SQLite names the result column "page_count" (pragma.c PragTyp_PAGE_COUNT).
		return &Result{Columns: []string{"page_count"}, Rows: [][]interface{}{{st.PageCount(s.Schema)}}}
	},
	"FREELIST_COUNT": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(0)}}}
	}),
	"AUTO_VACUUM": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.AutoVacuum(s.Schema, s.Value)
	},
	"INCREMENTAL_VACUUM": func(st EngineState, s *sql.PragmaStmt) *Result {
		return st.IncrementalVacuum(s.Schema)
	},
	"SYNCHRONOUS": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(1)}}}
	}),
	"TEMP_STORE": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(0)}}}
	}),
	"LOCKING_MODE": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{"normal"}}}
	}),
	"READ_UNCOMMITTED": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(0)}}}
	}),
	"SOFT_HEAP_LIMIT": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(0)}}}
	}),
	"THREADS": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Rows: [][]interface{}{{int64(1)}}}
	}),
	"DATABASE_LIST": pragmaGetOnly(func(st EngineState) *Result {
		return st.DatabaseList()
	}),
	"TABLE_X": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Columns: []string{"oid", "colX"}, Rows: [][]interface{}{{int64(0), ""}}}
	}),
	"SCHEMA_TABLE": pragmaGetOnly(func(st EngineState) *Result {
		return &Result{Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"}}
	}),
	"COMPILE_OPTIONS": pragmaGetOnly(func(st EngineState) *Result {
		opts := st.CompileOptions()
		rows := make([][]interface{}, 0, len(opts))
		for _, o := range opts {
			rows = append(rows, []interface{}{o})
		}
		return &Result{Columns: []string{"compile_options"}, Rows: rows}
	}),
}
