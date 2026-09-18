// Schema-load row validation, mirroring SQLite's sqlite3InitCallback
// (src/prepare.c:95) plus build.c's init.busy duplicate-rootpage check
// (src/build.c:4383-4393). SQLite validates every sqlite_schema row while
// loading the schema: the rootpage must be within the database page count,
// the stored CREATE text must parse, and an index must not share its root
// page with a sibling index of the same table. Failures report
// "malformed database schema (NAME) - <detail>" — or, when PRAGMA
// writable_schema is ON, a generic SQLITE_CORRUPT ("database disk image is
// malformed") without the object name (corruptSchema's SQLITE_WriteSchema
// branch, src/prepare.c:44-45).
package exec

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// validateLoadedSchema runs the sqlite3InitCallback row checks over every
// attached database's schema. Called from statement preflight, mirroring
// SQLite validating the schema at prepare time.
func (e *Engine) validateLoadedSchema(stmt sql.Stmt) error {
	// PRAGMA statements do not read the schema at prepare in SQLite
	// (sqlite3_prepare of "PRAGMA writable_schema=ON" never runs
	// sqlite3Init): the flag takes effect before the following statement's
	// prepare, so a corrupt schema reports the WriteSchema generic there
	// (corruptN-3.1), and PRAGMA batches never abort on the PRAGMA itself.
	if _, isPragma := stmt.(*sql.PragmaStmt); isPragma {
		return nil
	}
	// Inside an open transaction the in-memory schema is authoritative:
	// SQLite never reloads (and never re-parses stored rows) while a
	// transaction is active — the file schema cookie is compared only on
	// prepares AFTER COMMIT/ROLLBACK restores or settles the header.
	// (misc1-23.1: a writable_schema edit of sqlite_master followed by
	// BEGIN/CREATE/ROLLBACK must not fail — the ROLLBACK statement's
	// preflight must not re-parse the edited row from the in-flight
	// header state.)
	if e.tx.inTransaction {
		return nil
	}
	e.schemaParseMu.Lock()
	defer e.schemaParseMu.Unlock()
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil || ctx.Schema == nil {
			continue
		}
		// SQLite validates rows once per schema LOAD, not per statement:
		// writable_schema edits do not change the schema cookie and C does
		// not reload the schema for them (misc4-7.1 corrupts a stored row
		// mid-connection and later statements keep working off the stale
		// in-memory schema). Gate on the (schema cookie, page count) state
		// so an unchanged schema skips re-validation on this connection.
		stamp := e.schemaStateStamp(ctx)
		if e.schemaValidated == nil {
			e.schemaValidated = map[*DatabaseContext]uint64{}
		}
		if e.schemaValidated[ctx] == stamp {
			continue
		}
		if err := e.validateLoadedSchemaCtx(ctx); err != nil {
			return err
		}
		// Cache only success: a connection whose schema fails to load keeps
		// reporting on every statement (SQLite aborts each prepare that
		// needs the schema until the schema becomes readable again).
		e.schemaValidated[ctx] = stamp
	}
	return nil
}

// schemaStateStamp fingerprints the persisted schema state that
// validateLoadedSchema examines: the header schema cookie (bumped by DDL
// and PRAGMA schema_version) combined with the page count. A matching
// stamp means the rows were validated on this connection already.
func (e *Engine) schemaStateStamp(ctx *DatabaseContext) uint64 {
	hdr := ctx.Pager.Header()
	cookie := uint64(0)
	if len(hdr) >= 44 {
		cookie = uint64(binary.BigEndian.Uint32(hdr[40:44]))
	}
	return cookie<<32 | uint64(ctx.Pager.NumPages())
}

// validateLoadedSchemaCtx validates one database context's schema rows.
func (e *Engine) validateLoadedSchemaCtx(ctx *DatabaseContext) error {
	entries, err := ctx.Schema.GetEntries("")
	if err != nil {
		// The schema btree itself is unreadable: GetEntries already
		// reports "database disk image is malformed" for structural
		// damage (fts3corrupt4 14.2 precedent).
		return err
	}
	mxPage := ctx.Pager.NumPages()
	// Rootpage -> index name per table, for the duplicate check
	// (sqlite3IndexHasDuplicateRootPage is scoped to sibling indexes
	// of one table; different tables may legally share nothing, but
	// the C check would still be table-scoped — mirror that).
	idxRoots := map[string]map[uint32]string{}
	registerIndex := func(tblName, name string, root uint32) {
		if root == 0 {
			return
		}
		m, ok := idxRoots[tblName]
		if !ok {
			m = map[uint32]string{}
			idxRoots[tblName] = m
		}
		if _, dup := m[root]; !dup {
			m[root] = name
			return
		}
	}
	dupIndex := func(tblName string, root uint32, name string) bool {
		m := idxRoots[tblName]
		other, ok := m[root]
		return ok && other != name
	}
	// Pass 1: register every index rootpage (explicit + autoindex rows).
	for _, ent := range entries {
		if ent.Type == schema.TypeIndex {
			registerIndex(ent.TblName, ent.Name, ent.RootPage)
		}
	}
	// Pass 2: validate each row (sqlite3InitCallback).
	for _, ent := range entries {
		// Triggers and views carry rootpage 0 legitimately (argv[3]==0
		// with SQL text is the trigger/view shape). The argv[3]==0
		// generic-corrupt branch applies only to rows that should have
		// a rootpage; frigolite represents vtab entries with rootpage 0
		// too, so a zero rootpage is never by itself an error here.
		if ent.RootPage == 0 {
			continue
		}
		// Rootpage beyond the database page count (src/prepare.c:134-140:
		// db->init.newTnum > pData->mxPage when mxPage>0).
		if mxPage > 0 && ent.RootPage > mxPage {
			return e.schemaCorrupt(ent.Name, "invalid rootpage")
		}
		trimmed := strings.TrimLeft(ent.SQL, " \t\r\n\f")
		kind := ""
		if len(trimmed) >= 2 {
			up := strings.ToUpper(trimmed[:2])
			if up == "CR" {
				kind = "create"
			}
		}
		switch {
		case kind == "create":
			// The stored CREATE text must parse (src/prepare.c:144-158:
			// sqlite3Prepare on argv[4]; a parse error corrupts the
			// schema with the parser's message). Parse results are
			// memoized per SQL text.
			if perr, ok := e.schemaParseOK[ent.SQL]; !ok {
				_, perr = parse.ParseSQLSchema(ent.SQL)
				if e.schemaParseOK == nil {
					e.schemaParseOK = map[string]error{}
				}
				e.schemaParseOK[ent.SQL] = perr
			}
			if perr := e.schemaParseOK[ent.SQL]; perr != nil {
				return e.schemaCorrupt(ent.Name, perr.Error())
			}
			if ent.Type == schema.TypeIndex && dupIndex(ent.TblName, ent.RootPage, ent.Name) {
				// build.c:4388-4392: a CREATE INDEX row whose tnum equals
				// a sibling index of the same table is "invalid rootpage".
				return e.schemaCorrupt(ent.Name, "invalid rootpage")
			}
		case ent.SQL == "" && ent.Type == schema.TypeIndex:
			// Autoindex row (SQL column blank): src/prepare.c:177-184 —
			// tnum < 2 or duplicate rootpage is corrupt. (> mxPage was
			// already checked above.) Oracle-verified (corruptN-4.2 with
			// `sqlite3 -bail`): /usr/bin/sqlite3 3.51 reports the generic
			// WriteSchema corrupt even with writable_schema ON — the
			// apparently-successful REPLACE in default CLI mode was the
			// continue-on-error behavior masking the first statement's
			// failure.
			if ent.RootPage < 2 || dupIndex(ent.TblName, ent.RootPage, ent.Name) {
				return e.schemaCorrupt(ent.Name, "invalid rootpage")
			}
		}
	}
	return nil
}

// schemaCorrupt builds the schema-load corruption error. With PRAGMA
// writable_schema ON, corruptSchema (src/prepare.c:44-45) reports a bare
// SQLITE_CORRUPT — "database disk image is malformed" — instead of the
// named message.
func (e *Engine) schemaCorrupt(name, detail string) error {
	if e.settings.writableSchema {
		return fmt.Errorf("database disk image is malformed")
	}
	if detail == "" {
		return fmt.Errorf("malformed database schema (%s)", name)
	}
	return fmt.Errorf("malformed database schema (%s) - %s", name, detail)
}
