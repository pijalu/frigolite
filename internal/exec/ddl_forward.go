package exec

import (
	"encoding/binary"
	"fmt"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// This file forwards DDL-family helpers that moved to internal/execddl. The
// execution engine keeps thin same-named wrappers so existing call sites
// (DML, SELECT, PRAGMA, FK checks) stay unchanged while the implementation
// lives in the DDL sub-package.

func (e *Engine) echoVTabSource(name string) (string, bool) {
	return e.ddl.EchoVTabSource(name)
}

func (e *Engine) rewriteEchoInsert(s *sql.InsertStmt, srcName string) {
	e.ddl.RewriteEchoInsert(s, srcName)
}

func (e *Engine) execFTSDelete(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	return e.ddl.ExecFTSDelete(tableName, ftsTable, colDefs, s)
}

// FlushFTSSegments flushes pending FTS3 segments to %_segdir (at COMMIT).
func (e *Engine) FlushFTSSegments() *Result {
	return e.ddl.FlushFTSSegments()
}

// ValidateFTSSegments checks an FTS table's %_segdir roots for corruption.
func (e *Engine) ValidateFTSSegments(tableName string, checkBlocks bool) *Result {
	return e.ddl.ValidateFTSSegments(tableName, checkBlocks)
}

// RunFTSIntegrityCheck verifies an FTS table's in-memory index against its
// content rows (the FTS3 'integrity-check' special command and PRAGMA
// integrity_check(<fts-table>)).
func (e *Engine) RunFTSIntegrityCheck(tableName string) *Result {
	return e.ddl.RunFTSIntegrityCheck(tableName)
}

// WriteFTSStat rewrites the FTS4 %_stat doctotal row from the live index
// (SQLite's fts3UpdateDocTotals runs inside xUpdate, so every INSERT/UPDATE/
// DELETE refreshes it — fts3conf 3.1: matchinfo 'na' after REPLACE INTO
// t3(docid, ...) reads the up-to-date totals).
func (e *Engine) WriteFTSStat(tableName string) {
	e.ddl.WriteFTSStatPublic(tableName)
}

// ValidateFTSShadowRoots checks an FTS table's shadow btrees for structural
// corruption (fts3corrupt4 24.1).
func (e *Engine) ValidateFTSShadowRoots(tableName string) *Result {
	return e.ddl.ValidateFTSShadowRoots(tableName)
}

// MergeFTS runs the FTS 'merge=N[,M]' special command (N = max leaf pages,
// M = min segments per level).
func (e *Engine) MergeFTS(tableName string, nMerge, nMin int) {
	// The merge's internal %_segdir/%_segments/%_stat writes (creating the
	// output, truncating sources) are a single logical operation: they skip
	// the per-write pager snapshot (inFTSFlush) so a large merge does not
	// re-snapshot the growing pager per shadow write (fts4merge 1.4/5.x:
	// the truncation updateFTSShadowRow snapshotted the whole pager on every
	// segment, O(n^2) over the merge sequence). Save/restore so a merge
	// nested inside the flush-time automerge keeps the outer flush's flag.
	wasFlush := e.tx.inFTSFlush
	e.tx.inFTSFlush = true
	e.ddl.MergeFTS(tableName, nMerge, nMin)
	e.tx.inFTSFlush = wasFlush
}

// WriteFTSShadowRow inserts one %_segdir row (FTS optimize support).
func (e *Engine) WriteFTSShadowRow(tableName string, level, idx int, blocks []fts.SegmentBlock, root []byte) {
	e.ddl.WriteFTSShadowRow(tableName, level, idx, blocks, root)
}

// NextFTSBlockID returns the next %_segments block ID (FTS optimize support).
func (e *Engine) NextFTSBlockID(tableName string) int {
	return e.ddl.NextFTSBlockID(tableName)
}

// ContentRowExists reports whether a content=<table> FTS table's external
// content table has a row with the given rowid. A virtual-table content
// source (fts4content 9.x: content=e1 with e1 an echo module) is checked
// through its materialized rows.
func (e *Engine) ContentRowExists(tableName string, rowID int64) bool {
	entry, _, err := e.FindTable(tableName)
	if err != nil || entry == nil {
		return false
	}
	// A vtab content source has no b-tree: its rows are materialized, and the
	// rowid is the 1-based materialization index (the echo module mirrors the
	// source table's rowids).
	if entry.RootPage == 0 {
		rows, rerr := e.ddl.VirtualTableRows(entry, 0, "", false)
		if rerr != nil || rows == nil {
			return false
		}
		return rowID >= 1 && int(rowID) <= len(rows)
	}
	tree := e.TableBTreeForName(entry.Name, entry.RootPage, true)
	cell, cerr := e.ReadCellByRowID(tree, rowID)
	return cerr == nil && cell != nil
}

func (e *Engine) execFTSUpdate(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result {
	return e.ddl.ExecFTSUpdate(tableName, ftsTable, colDefs, s)
}

func (e *Engine) execFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	return e.ddl.ExecFTSSelect(s, tableEntry, ftsTable, colDefs)
}

func (e *Engine) validateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error {
	return e.ddl.ValidateIndexedBy(tableEntry, indexName, s)
}

func (e *Engine) tableHasColumn(tableName, colName string) bool {
	return e.ddl.TableHasColumn(tableName, colName)
}

func (e *Engine) virtualTableRows(entry *schema.Entry, bound int64, input string, hasInput bool) ([][]interface{}, error) {
	return e.ddl.VirtualTableRows(entry, bound, input, hasInput)
}

func (e *Engine) ensureFTSForTable(entry *schema.Entry) {
	e.ddl.EnsureFTSForTable(entry)
}

func (e *Engine) isVirtualTable(entry *schema.Entry) bool {
	return e.ddl.IsVirtualTable(entry)
}

func (e *Engine) readCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error) {
	return e.ddl.ReadCellByRowID(tree, rowID)
}

// Close closes every database pager, flushing buffered writes to disk.
func (e *Engine) Close() error {
	// Release this connection's cross-connection lock marks first
	// (sqlite3_close drops the connection's file locks; a connection closed
	// mid-transaction must not keep blocking others on the same file).
	e.ReleaseAllLocks()
	// Disconnect cached unionvtab/swarmvtab instances first (unionDisconnect
	// fires the openclose UDF per still-open source database), then release
	// any remaining swarmvtab file-source handles.
	e.DisconnectUnionVtabs()
	e.closeUnionFileDBs()
	return e.ddl.Close()
}

// DetachAll detaches all attached databases except main/temp.
func (e *Engine) DetachAll() {
	e.ddl.DetachAll()
}

// ValidateAllTableRoots walks every table's root-page btree, reporting
// structural corruption as "database disk image is malformed" (fts3corrupt4
// 24.1: an INSERT ... SELECT fails when any table's btree is corrupt).
func (e *Engine) ValidateAllTableRoots() error {
	return e.schema.ValidateAllTableRoots()
}

// EstimateFreeSpace returns the approximate free bytes across the database's
// btree pages (used to decide whether an INSERT ... SELECT's write grows the
// file, matching SQLite's allocation behavior).
func (e *Engine) EstimateFreeSpace() int64 {
	return e.schema.EstimateFreeSpace()
}

// RebuildFTSIndex rebuilds an FTS table's index from %_content (the FTS3
// 'rebuild' special command), validating the shadow btrees first.
func (e *Engine) RebuildFTSIndex(tableName string) *Result {
	return e.ddl.RebuildFTSIndex(tableName)
}

// ReloadFTSIndex drops an FTS table's in-memory index and reloads it from
// the persisted %_segdir/%_segments rows (see DMLContext.ReloadFTSIndex).
func (e *Engine) ReloadFTSIndex(tableName string) *Result {
	return e.ddl.ReloadFTSIndex(tableName)
}

// ValidateFreelistForGrowth validates the database freelist when an
// allocating write (REBUILD, a large INSERT) is about to run. A freelist
// whose trunk page is corrupt (a crash-written page) fails with "database
// disk image is malformed" (fts3corrupt4 24.7: REBUILD allocates new
// segments and hits the corrupt freelist; the oracle fails).
func (e *Engine) ValidateFreelistForGrowth() error {
	hdr := e.pager.Header()
	if len(hdr) < 40 {
		return nil
	}
	trunk := binary.BigEndian.Uint32(hdr[32:36])
	count := binary.BigEndian.Uint32(hdr[36:40])
	if trunk == 0 || count == 0 {
		return nil
	}
	numPages := e.pager.NumPages()
	if trunk > numPages {
		return fmt.Errorf("database disk image is malformed")
	}
	pg, err := e.pager.ReadPage(trunk)
	if err != nil {
		return fmt.Errorf("database disk image is malformed")
	}
	data := pg.Data
	if len(data) < 8 {
		return fmt.Errorf("database disk image is malformed")
	}
	allZero := true
	for _, b := range data[:8] {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}
