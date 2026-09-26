package fts5

import (
	"errors"
	"fmt"
)

// Re-entrancy guards mirroring C's fts5 cursor/config locking:
//
//   - writeActive counts in-flight write operations (fts5UpdateMethod and
//     the fts5Storage* write helpers). A query plan arriving while it is
//     nonzero reads the index mid-write and fails with C's corrupt error
//     (fts5circref: a trigger on any shadow table selecting from the table
//     being written).
//
//   - scanGuard counts in-flight external-content reads of this table (C's
//     pConfig->bLock, held while the %_content read statements prepare and
//     step — fts5_storage.c sqlite3Fts5StorageStmt, fts5_main.c
//     fts5CsrStep). A query plan arriving while it is nonzero is a
//     content-table recursion and fails with C's "recursively defined fts5
//     content table" (fts5_main.c fts5BestIndexMethod's bLock check).

// beginWrite opens one write scope. The defer pairing is nesting-safe.
func (t *Table) beginWrite() func() {
	t.writeActive++
	return func() { t.writeActive-- }
}

// BeginWriteScope opens a write scope from outside the package — the
// statement-boundary sync (DML layer's flushFTS5Shadow) runs with the index
// write transaction still open in C (xSync), so its shadow writes fire
// shadow-table triggers mid-write just like in-statement writes do.
func (t *Table) BeginWriteScope() func() { return t.beginWrite() }

// beginContentScan opens one external-content read scope (C's bLock++).
func (t *Table) beginContentScan() func() {
	t.scanGuard++
	return func() { t.scanGuard-- }
}

// BeginQuery validates a query plan against the table (the xBestIndex
// analogue): a plan arriving mid-content-scan is a recursive content table;
// a plan arriving mid-write reads a transient index and is corrupt.
func (t *Table) BeginQuery() error {
	if t.scanGuard > 0 {
		return fmt.Errorf("recursively defined fts5 content table")
	}
	if t.writeActive > 0 {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}

// MissingContentRowError reports a content-table fetch for a rowid the index
// holds but the content table lacks (fts5CursorFetchContent's
// SQLITE_CORRUPT_VTAB class). The vtab layer surfaces the formatted message;
// the TCL API layer (fts5_tcl.c's rc-name convention) reports the code name
// SQLITE_CORRUPT_VTAB instead.
type MissingContentRowError struct {
	Rowid   int64
	Content string
}

// Error renders C's fts5SetVtabError text.
func (e *MissingContentRowError) Error() string {
	return fmt.Sprintf("fts5: missing row %d from content table %s", e.Rowid, e.Content)
}

// ReportCorrupt reports whether err is the missing-content-row class: the
// API layer maps it to the bare rc name.
func ReportCorrupt(err error) bool {
	var m *MissingContentRowError
	return errors.As(err, &m)
}
