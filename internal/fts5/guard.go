package fts5

import "fmt"

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
