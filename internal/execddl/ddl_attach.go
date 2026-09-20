// Package exec implements query execution.
//
// This file holds ATTACH DATABASE execution helpers extracted from
// ddl_core.go so each file stays within the repository's complexity budgets:
// the preflight name guard, the shared-pager alias path, and the pager +
// schema bootstrap.
package execddl

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// attachNameGuard applies ATTACH's preflight name checks: the reserved
// "main"/"temp"/"temporary" names are always in use, a duplicate attachment
// is rejected, the attached-database limit is enforced, and sqlite_ schema
// names are reserved.
func (e *DDLExecutor) attachNameGuard(s *sql.AttachStmt, schemaUpper string) *Result {
	// Check reserved names: "main", "temp", "temporary" are always in use
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}
	// Check for duplicate attachment
	if _, ok := e.ctx.Databases()[schemaUpper]; ok {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}
	if e.attachedDBCount() >= MaxAttachedDatabases {
		return &Result{Error: fmt.Errorf("too many attached databases - max %d", MaxAttachedDatabases)}
	}
	if schemaUpper == "SQLITE_MASTER" || schemaUpper == "SQLITE_SCHEMA" {
		return &Result{Error: fmt.Errorf("reserved schema name: %s", s.Schema)}
	}
	return nil
}

// attachSharedPager reuses an existing pager when the ATTACH path is already
// open under another schema name: same FILE under a second schema name
// (attach-9.1: one file as aux1 and aux2) shares the existing schema's pager
// and schema manager so both names see the same data. Writes to both names in
// one transaction raise "database is locked" via the same-file write tracker
// (CheckSameFileWriteConflict); the shared pager is closed only by the
// connection's Close. Reports whether the attach completed through a shared
// pager.
func (e *DDLExecutor) attachSharedPager(s *sql.AttachStmt, path, schemaUpper string) (bool, *Result) {
	if mainPath := e.ctx.MainDB().FilePath; mainPath == path {
		ctx := &DatabaseContext{Name: s.Schema, Pager: e.ctx.MainDB().Pager,
			Schema: e.ctx.MainDB().Schema, FilePath: path, SharedPager: true}
		e.ctx.Databases()[schemaUpper] = ctx
		e.ctx.AppendDBList(ctx)
		return true, &Result{}
	}
	for _, other := range e.ctx.Databases() {
		if other != nil && other.FilePath == path {
			ctx := &DatabaseContext{Name: s.Schema, Pager: other.Pager,
				Schema: other.Schema, FilePath: path, SharedPager: true}
			e.ctx.Databases()[schemaUpper] = ctx
			e.ctx.AppendDBList(ctx)
			return true, &Result{}
		}
	}
	return false, nil
}

// openAttachSchema opens the attached database's pager and initializes and
// eagerly reads its schema. The eager read mirrors src/attach.c: ATTACH runs
// sqlite3InitOne on the new database, so a corrupt attached image must fail
// the ATTACH itself ("file is not a database", attach-8.1) and leave nothing
// registered. Deferred validation would register a dead attachment whose
// first later read fails mid-statement and poisons every following statement.
// Any failure closes the pager.
func openAttachSchema(path string, isMemory bool) (*schema.Manager, *pager.Pager, *Result) {
	pg, res := openAttachPager(path, isMemory)
	if res != nil {
		return nil, nil, res
	}
	// Initialize schema for the attached database
	sch := schema.NewManager(pg)
	if err := sch.Init(); err != nil {
		pg.Close()
		return nil, nil, &Result{Error: fmt.Errorf("cannot initialize schema for attached database: %w", err)}
	}
	if _, err := sch.GetEntries(schema.TypeTable); err != nil {
		pg.Close()
		return nil, nil, &Result{Error: err}
	}
	// Record the file state at attach time so later external writes (from
	// another connection) are detected and the schema re-read.
	sch.SetTrackExternalMod(true)
	sch.CaptureFileStamp()
	return sch, pg, nil
}
