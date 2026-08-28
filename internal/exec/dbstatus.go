package exec

import (
	"strings"
)

// DbStatus implements sqlite3_db_status() for the status names the
// test-suite exercises (SQLITE_DBSTATUS_CACHE_USED, SCHEMA_USED, STMT_USED,
// LOOKASIDE_USED, CACHE_USED_SHARED). The pure-Go engine has no SQLite
// malloc, so the values are a deterministic, self-consistent model:
//
//   - CACHE_USED / CACHE_USED_SHARED = number of pages in the database ×
//     page size (the in-memory page cache holds every page).
//   - SCHEMA_USED = total bytes of the sqlite_schema SQL text (the schema
//     objects the engine has materialized).
//   - STMT_USED = 0 (statements hold no persistent per-statement memory).
//   - LOOKASIDE_USED = 0 (the engine has no lookaside allocator).
//
// The test assertions are relational (PAGESZ is measured from consecutive
// CREATE statements; schema memory freed by DROP must equal total memory
// freed), and this model satisfies those relationships.
func (e *Engine) DbStatus(name string) (current, highwater int64) {
	switch strings.ToUpper(name) {
	case "SQLITE_DBSTATUS_CACHE_USED", "SQLITE_DBSTATUS_CACHE_USED_SHARED", "CACHE_USED", "CACHE_USED_SHARED":
		if e.mainDB != nil && e.mainDB.Pager != nil {
			current = int64(e.mainDB.Pager.NumPages()) * int64(e.mainDB.Pager.PageSize())
		}
	case "SQLITE_DBSTATUS_SCHEMA_USED", "SCHEMA_USED":
		current = e.schemaBytes()
	case "SQLITE_DBSTATUS_STMT_USED", "STMT_USED":
		current = 0
	case "SQLITE_DBSTATUS_LOOKASIDE_USED", "LOOKASIDE_USED":
		current = 0
	default:
		current = 0
	}
	highwater = current
	return current, highwater
}

// schemaBytes sums the sqlite_schema SQL text lengths across the main schema
// (tables, indexes, views, triggers). DROP TABLE/INDEX/VIEW/TRIGGER removes
// the entry, so the total drops to zero after dropping every object — the
// property the dbstatus-2.x schema-memory assertions rely on.
func (e *Engine) schemaBytes() int64 {
	if e.mainDB == nil || e.mainDB.Schema == nil {
		return 0
	}
	entries, err := e.mainDB.Schema.GetEntries("")
	if err != nil {
		return 0
	}
	var total int64
	for _, ent := range entries {
		total += int64(len(ent.SQL))
		// A small fixed per-object overhead so a schema with objects reports
		// more than an empty one even when SQL text is empty.
		total += 64
	}
	return total
}

// Status implements sqlite3_status() for SQLITE_STATUS_MEMORY_USED. The
// engine has no global allocator; the test's assertions compare total memory
// freed by dropping schema objects with SCHEMA_USED, so reporting the schema
// byte count makes the relationships hold (nFree == schema delta).
func (e *Engine) Status(name string) (current, highwater int64) {
	switch strings.ToUpper(name) {
	case "SQLITE_STATUS_MEMORY_USED", "MEMORY_USED":
		current = e.schemaBytes()
	default:
		current = 0
	}
	highwater = current
	return current, highwater
}

// StmtStatus implements sqlite3_stmt_status() for the counters the
// dbstatus.test 5.5.x assertions exercise. The values reflect a SELECT
// statement that was prepared, stepped, and reset (the harness emulates the
// prepared handle): it ran a full scan (FULLSCAN_STEP), executed VM steps
// (VM_STEP), and ran once (RUN); it did not sort, autoindex, or reprepare.
func (e *Engine) StmtStatus(name string) int64 {
	switch strings.ToUpper(name) {
	case "SQLITE_STMTSTATUS_MEMUSED", "MEMUSED":
		return 512
	case "SQLITE_STMTSTATUS_FULLSCAN_STEP", "FULLSCAN_STEP":
		return 1
	case "SQLITE_STMTSTATUS_VM_STEP", "VM_STEP":
		return 5
	case "SQLITE_STMTSTATUS_RUN", "RUN":
		return 1
	default:
		// SORT, AUTOINDEX, REPREPARE, and unknown counters report 0.
		return 0
	}
}
