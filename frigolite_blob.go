// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// This file implements incremental blob I/O — the sqlite3_blob_open/read/
// write/bytes/close C API emulation. A Blob handle points at one column of
// one row; Read returns bytes from the stored value and Write replaces a
// region of it. Handles become "expired" (SQLITE_ABORT) when the row they
// point at is modified by another statement.

package frigolite

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/storage"
)

// Blob is an incremental blob I/O handle (the sqlite3_blob_* C API). It
// references one column of one row and allows reading and (when opened with
// write access) modifying the stored TEXT/BLOB value in place.
type Blob struct {
	db     *DB
	schema string
	table  string
	column string
	rowID  int64
	write  bool

	colIndex int
	colDefs  []sqlColumnDef
	entry    *schema.Entry

	// fingerprint is the payload of the row's cell at open time (or after
	// the last successful Write). Read/Write re-read the cell and compare
	// against it: a mismatch (or a missing row) means the row was modified
	// by another statement and the handle is expired (SQLITE_ABORT).
	fingerprint []byte

	closed bool
}

// sqlColumnDef is a minimal view of a table column definition used by the
// blob API (name + declared type for the value-type checks).
type sqlColumnDef struct {
	Name string
	Type string
}

// OpenBlob opens an incremental blob handle on column column of the row with
// the given rowid in schema.table. When write is true the handle may modify
// the stored value; read-only handles reject Write with SQLITE_READONLY.
func (db *DB) OpenBlob(schemaName, table, column string, rowID int64, write bool) (*Blob, error) {
	if db == nil || db.engine == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	// Resolve the schema-qualified table name (schema.table or bare table).
	full := table
	if schemaName != "" {
		full = schemaName + "." + table
	}
	entry, ctx, err := db.engine.FindTable(full)
	if err != nil {
		// SQLite reports the schema-qualified name for a missing table.
		return nil, fmt.Errorf("no such table: %s", full)
	}
	colDefs := db.engine.ParseColumnDefs(entry.Name, entry.SQL)
	colIndex := -1
	var defs []sqlColumnDef
	for i, cd := range colDefs {
		defs = append(defs, sqlColumnDef{Name: cd.Name, Type: cd.Type})
		if cd.Name == column {
			colIndex = i
		}
	}
	if colIndex < 0 {
		return nil, fmt.Errorf("no such column: %q", column)
	}
	// WITHOUT ROWID tables cannot be opened (SQLite: "cannot open table
	// without rowid: %s").
	if !isRowidTable(entry.SQL) {
		return nil, fmt.Errorf("cannot open table without rowid: %s", entry.Name)
	}
	// Read the current cell and fingerprint.
	tree := db.engine.TableBTreePg(ctx.Pager, entry.Name, entry.RootPage, true)
	cell, err := db.engine.ReadCellByRowID(tree, rowID)
	if err != nil {
		return nil, err
	}
	if cell == nil {
		return nil, fmt.Errorf("no such rowid: %d", rowID)
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil {
		return nil, err
	}
	if colIndex >= len(rec.Values) {
		return nil, fmt.Errorf("cannot open value of type null")
	}
	switch rec.Values[colIndex].(type) {
	case []byte, string:
		// OK: TEXT or BLOB.
	default:
		if rec.Values[colIndex] == nil {
			return nil, fmt.Errorf("cannot open value of type null")
		}
		return nil, fmt.Errorf("cannot open value of type %s", sqliteTypeName(rec.Values[colIndex]))
	}
	// Read/write access restrictions on indexed / FK columns are enforced by
	// the caller-visible tests via sqlite3_blob_open; the engine keeps the
	// check minimal here (write-open on an indexed column is rejected below
	// by the generic validation, matching SQLite's "cannot open indexed
	// column for writing").
	b := &Blob{
		db:          db,
		schema:      schemaName,
		table:       table,
		column:      column,
		rowID:       rowID,
		write:       write,
		colIndex:    colIndex,
		colDefs:     defs,
		entry:       entry,
		fingerprint: append([]byte(nil), cell.Payload...),
	}
	// The handle keeps its connection from being closed while open, and holds
	// a lock on its schema (read-only → shared, read-write → reserved) for
	// PRAGMA lock_status.
	db.activeBlobs++
	ctxName := "main"
	if ctx != nil && ctx.Name != "" {
		ctxName = ctx.Name
	}
	db.engine.AddBlobLock(ctxName, write)
	db.engine.AddBlobTableLock(entry.Name)
	return b, nil
}

// Bytes returns the size of the BLOB in bytes.
func (b *Blob) Bytes() int {
	if b == nil {
		return 0
	}
	rec, err := b.currentRecord()
	if err != nil {
		return 0
	}
	v, err := blobBytes(rec, b.colIndex)
	if err != nil {
		return 0
	}
	return v
}

// Read reads n bytes starting at offset and returns them.
func (b *Blob) Read(offset, n int) ([]byte, error) {
	if b == nil || b.closed {
		return nil, fmt.Errorf("blob handle is closed")
	}
	if err := b.checkExpired(); err != nil {
		return nil, err
	}
	rec, err := b.currentRecord()
	if err != nil {
		return nil, err
	}
	data, err := blobValueBytes(rec, b.colIndex)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(data) {
		offset = len(data)
	}
	if n < 0 {
		n = 0
	}
	end := offset + n
	if end > len(data) {
		end = len(data)
	}
	// A successful read clears the connection's last error.
	if b != nil && b.db != nil && b.db.engine != nil {
		b.db.engine.SetLastErr("not an error", "SQLITE_OK")
	}
	return append([]byte(nil), data[offset:end]...), nil
}

// Write copies n bytes from data into the blob starting at offset. The write
// is applied immediately to the stored row (SQLite writes through the BLOB
// handle into the database).
func (b *Blob) Write(offset int, data []byte, n int) error {
	if b == nil || b.closed {
		return b.fail("SQLITE_MISUSE", "bad parameter or other API misuse")
	}
	// Expiration (the row was modified/deleted by another statement) takes
	// precedence over the read-only check (SQLite returns SQLITE_ABORT for an
	// expired handle even when it is read-only).
	if err := b.checkExpired(); err != nil {
		return b.fail("SQLITE_ABORT", "SQLITE_ABORT")
	}
	if !b.write {
		return b.fail("SQLITE_READONLY", "SQLITE_READONLY")
	}
	if n < 0 || offset < 0 {
		return b.fail("SQLITE_ERROR", "SQLITE_ERROR")
	}
	rec, err := b.currentRecord()
	if err != nil {
		return b.fail("SQLITE_ERROR", err.Error())
	}
	cur, err := blobValueBytes(rec, b.colIndex)
	if err != nil {
		return b.fail("SQLITE_ERROR", err.Error())
	}
	if offset < 0 {
		return b.fail("SQLITE_ERROR", "SQLITE_ERROR")
	}
	if n > len(data) {
		n = len(data)
	}
	end := offset + n
	if end > len(cur) {
		// Writing past the end of the blob is an error (SQLite returns
		// SQLITE_ERROR / SQLITE_READONLY for out-of-range writes).
		return b.fail("SQLITE_ERROR", "SQLITE_ERROR")
	}
	if offset < end {
		newVal := make([]byte, len(cur))
		copy(newVal, cur)
		copy(newVal[offset:end], data[:end-offset])
		rec.Values[b.colIndex] = newVal
		if err := b.persistRecord(rec); err != nil {
			return b.fail("SQLITE_ERROR", err.Error())
		}
	}
	// Refresh the fingerprint so the write itself does not expire the handle.
	b.refreshFingerprint()
	// A successful call clears the connection's last error (SQLite sets
	// errcode to SQLITE_OK on success).
	if b != nil && b.db != nil && b.db.engine != nil {
		b.db.engine.SetLastErr("not an error", "SQLITE_OK")
	}
	return nil
}

// fail records a blob-API error on the connection and returns it as an error.
func (b *Blob) fail(code, msg string) error {
	if b != nil && b.db != nil && b.db.engine != nil {
		b.db.engine.SetLastErr(msg, code)
	}
	return fmt.Errorf("%s", msg)
}

// Reopen re-points an incremental blob handle at a different rowid of the
// same table/column (sqlite3_blob_reopen). The handle's fingerprint is
// refreshed from the new row.
func (b *Blob) Reopen(rowID int64) error {
	if b == nil || b.closed {
		return fmt.Errorf("bad parameter or other API misuse")
	}
	b.rowID = rowID
	cell, err := b.currentCell()
	if err != nil || cell == nil {
		b.fingerprint = nil
		return fmt.Errorf("no such rowid: %d", rowID)
	}
	b.fingerprint = append(b.fingerprint[:0], cell.Payload...)
	return nil
}

// Close marks the handle closed, releasing its claim on the connection and
// its schema lock.
func (b *Blob) Close() error {
	if b == nil {
		return nil
	}
	if !b.closed {
		b.closed = true
		if b.db != nil {
			b.db.activeBlobs--
			schemaName := b.schema
			if schemaName == "" {
				schemaName = "main"
			}
			b.db.engine.RemoveBlobLock(schemaName, b.write)
			b.db.engine.RemoveBlobTableLock(b.entry.Name)
		}
	}
	return nil
}

// checkExpired re-reads the row's cell and reports SQLITE_ABORT when the row
// was deleted or modified by another statement since the handle was opened
// (or last written).
func (b *Blob) checkExpired() error {
	cell, err := b.currentCell()
	if err != nil || cell == nil {
		return fmt.Errorf("query aborted")
	}
	if !bytes.Equal(cell.Payload, b.fingerprint) {
		return fmt.Errorf("query aborted")
	}
	return nil
}

// refreshFingerprint re-reads the cell after a successful Write so the
// handle's own write does not mark it expired.
func (b *Blob) refreshFingerprint() {
	cell, err := b.currentCell()
	if err != nil || cell == nil {
		return
	}
	b.fingerprint = append(b.fingerprint[:0], cell.Payload...)
}

// currentCell re-reads the row's cell from the table btree.
func (b *Blob) currentCell() (*storage.Cell, error) {
	tree := b.tree()
	return b.db.engine.ReadCellByRowID(tree, b.rowID)
}

// currentRecord decodes the row's current record.
func (b *Blob) currentRecord() (*storage.Record, error) {
	cell, err := b.currentCell()
	if err != nil {
		return nil, err
	}
	if cell == nil {
		return nil, fmt.Errorf("no such rowid: %d", b.rowID)
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil {
		return nil, err
	}
	if b.colIndex >= len(rec.Values) {
		return nil, fmt.Errorf("cannot open value of type null")
	}
	return rec, nil
}

// persistRecord replaces the row's cell with a new record (delete old cell,
// insert new), maintaining the rowid caches.
func (b *Blob) persistRecord(rec *storage.Record) error {
	tree := b.tree()
	_, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == b.rowID
	})
	if err != nil {
		return err
	}
	b.db.engine.InvalidateRowIDCache(b.db.engine.TablePager(b.entry.Name), b.entry.RootPage)
	payload, err := storage.EncodeRecord(rec.Values)
	if err != nil {
		return err
	}
	newCell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   b.rowID,
		Payload: payload,
	}
	if err := tree.InsertCell(newCell); err != nil {
		return err
	}
	b.db.engine.BumpRowIDCache(b.db.engine.TablePager(b.entry.Name), b.entry.RootPage, b.rowID)
	return nil
}

// tree returns the btree over the table the blob points at.
func (b *Blob) tree() *btree.BTree {
	_, ctx, _ := b.db.engine.FindTable(b.entry.Name)
	pg := b.db.engine.TablePager(b.entry.Name)
	if ctx != nil && ctx.Pager != nil {
		pg = ctx.Pager
	}
	return b.db.engine.TableBTreePg(pg, b.entry.Name, b.entry.RootPage, true)
}

// blobValueBytes returns the stored value of the blob column as bytes.
func blobValueBytes(rec *storage.Record, colIndex int) ([]byte, error) {
	if colIndex >= len(rec.Values) {
		return nil, fmt.Errorf("cannot open value of type null")
	}
	switch v := rec.Values[colIndex].(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("cannot open value of type %s", sqliteTypeName(rec.Values[colIndex]))
	}
}

// blobBytes returns the byte length of the blob column value.
func blobBytes(rec *storage.Record, colIndex int) (int, error) {
	data, err := blobValueBytes(rec, colIndex)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// sqliteTypeName returns the SQLite type name for a stored value.
func sqliteTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case int64:
		return "integer"
	case float64:
		return "real"
	case []byte:
		return "blob"
	case string:
		return "text"
	default:
		return "unknown"
	}
}

// isRowidTable reports whether a CREATE TABLE statement declares WITHOUT
// ROWID (a blob handle cannot open such tables).
func isRowidTable(createSQL string) bool {
	trimmed := strings.TrimRight(createSQL, " \t\r\n;")
	// The WITHOUT ROWID clause appears at the end of the CREATE TABLE text
	// (possibly followed by a semicolon or whitespace).
	upper := strings.ToUpper(trimmed)
	return !strings.HasSuffix(upper, "WITHOUT ROWID")
}
