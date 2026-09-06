// Frigolite serialization API: sqlite3_serialize / sqlite3_deserialize
// (memdb.c + tclsqlite.c DB_DESERIALIZE) over the engine's pager images.
package frigolite

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
)

// Serialize returns a contiguous copy of the named schema's database image
// (sqlite3_serialize via memdb.c: sz = page_count × pageSize; memdb1.test
// 100 asserts len == page_size × page_count).
func (db *DB) Serialize(schemaName string) ([]byte, error) {
	if db == nil || db.pager == nil || db.engine == nil {
		return nil, fmt.Errorf("frigolite: database not open")
	}
	if schemaName == "" || schemaName == "main" {
		return db.pager.Serialize(), nil
	}
	ctx := db.engine.GetDB(schemaName)
	if ctx == nil || ctx.Pager == nil {
		return nil, fmt.Errorf("unknown database %s", schemaName)
	}
	return ctx.Pager.Serialize(), nil
}

// DeserializeOptions carries the DB_DESERIALIZE flags (tclsqlite.c:
// -readonly BOOL, -maxsize N via SQLITE_FCNTL_SIZE_LIMIT).
type DeserializeOptions struct {
	ReadOnly bool
	MaxSize  int64
}

// Deserialize replaces the named schema's database image with img
// (sqlite3_deserialize via tclsqlite.c DB_DESERIALIZE: empty img resets to
// an empty database; bad magic defers "file is not a database" to the
// first schema read; maxSize overflow fails "database or disk is full").
// The TEMP schema cannot be deserialized (memdb1.test 650).
func (db *DB) Deserialize(schemaName string, img []byte, opts DeserializeOptions) error {
	if err := db.checkDeserializeAllowed(schemaName); err != nil {
		return err
	}
	pg, err := db.deserializeTarget(schemaName)
	if err != nil {
		return err
	}
	if err := pg.Deserialize(img, opts.MaxSize, opts.ReadOnly); err != nil {
		return err
	}
	db.refreshSchemaAfterDeserialize(pg)
	return nil
}

// checkDeserializeAllowed rejects schemas and states sqlite3_deserialize
// refuses: TEMP, an active read statement (db eval callback), or an active
// backup (memdb1.test 650/1010/1020 → "unable to set MEMDB content").
func (db *DB) checkDeserializeAllowed(schemaName string) error {
	if db == nil || db.pager == nil || db.engine == nil {
		return fmt.Errorf("frigolite: database not open")
	}
	if schemaName == "temp" {
		return fmt.Errorf("unable to set MEMDB content")
	}
	if db.engine.ActiveReadStatements() > 0 {
		return fmt.Errorf("unable to set MEMDB content")
	}
	if db.activeBackups > 0 {
		return fmt.Errorf("unable to set MEMDB content")
	}
	return nil
}

// deserializeTarget resolves the pager for the named schema (""/main → the
// main pager; otherwise the attached schema's pager).
func (db *DB) deserializeTarget(schemaName string) (*pager.Pager, error) {
	if schemaName == "" || schemaName == "main" {
		return db.pager, nil
	}
	ctx := db.engine.GetDB(schemaName)
	if ctx == nil || ctx.Pager == nil {
		return nil, fmt.Errorf("unknown database %s", schemaName)
	}
	return ctx.Pager, nil
}

// refreshSchemaAfterDeserialize re-syncs schema caches after an image swap:
// a fresh/empty image (0 pages) needs schema re-init (page 1 + default
// header) so the next CREATE works (memdb1.test 400-410: deserialize
// {} then CREATE TABLE t4). Non-empty images keep their schema.
func (db *DB) refreshSchemaAfterDeserialize(pg *pager.Pager) {
	if pg.NumPages() == 0 {
		db.schema.Init()
	} else {
		db.schema.InvalidateCache()
	}
	db.engine.InvalidateTableCaches()
}
