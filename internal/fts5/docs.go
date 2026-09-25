package fts5

// Document lifecycle beyond the plain insert/delete path: the 'rebuild'
// command, full scans (ScanDocs), stored content reads (DocValues) and the
// statement-level snapshot/restore used by savepoint rollback.

import "fmt"

// rebuild re-indexes every external content row (fts5StorageRebuild).
func (t *Table) rebuild() error {
	defer t.bumpVersion()
	docs, err := t.rebuildDocs()
	if err != nil {
		return err
	}
	t.ix = NewInvertedIndex(len(t.cfg.Columns))
	t.maxRowid = 0
	// Rebuild reinitializes the index at the current file format
	// (fts5StorageRebuild: REPLACE 'version'=FTS5_CURRENT_VERSION), so a
	// secure-delete-upgraded table rebuilds back to version 4
	// (fts5version 1.11 second block).
	if t.cfg.FormatVersion != 4 {
		if err := t.storeConfigValue("version", 4); err != nil {
			return err
		}
		t.cfg.FormatVersion = 4
		t.pendingSecureUpgrade = false
	}
	// The prior index state is discarded and every document re-enters
	// through the pending hash (fts5StorageRebuild's per-row
	// sqlite3Fts5IndexWrite), flushing as ordinary segments at the sync
	// point.
	if err := t.resetIndexStructure(); err != nil {
		return err
	}
	for _, d := range docs {
		if err := t.rebuildAddDoc(d.rowid, d.values); err != nil {
			return err
		}
	}
	return t.FlushShadowIfDirty()
}

// rebuildDoc is one document collected for a rebuild.
type rebuildDoc struct {
	rowid  int64
	values []interface{}
}

// rebuildDocs collects the documents a 'rebuild' re-indexes: an external
// content scan, or the stored %_content mirror
// (fts5StorageRebuild scans %_content for content= tables).
func (t *Table) rebuildDocs() ([]rebuildDoc, error) {
	if t.cfg.EContent != ContentExternal {
		var docs []rebuildDoc
		for _, rowid := range t.ix.SortedRowids() {
			docs = append(docs, rebuildDoc{rowid: rowid, values: t.contentValues[rowid]})
		}
		return docs, nil
	}
	rowids, values, err := t.scanExternal()
	if err != nil {
		return nil, err
	}
	docs := make([]rebuildDoc, 0, len(rowids))
	for i, rowid := range rowids {
		docs = append(docs, rebuildDoc{rowid: rowid, values: values[i]})
	}
	return docs, nil
}

// rebuildAddDoc re-enters one document through the pending hash
// (fts5StorageRebuild's per-row sqlite3Fts5IndexWrite).
func (t *Table) rebuildAddDoc(rowid int64, values []interface{}) error {
	cols, err := t.tokenizeValues(values)
	if err != nil {
		return err
	}
	t.ix.AddDoc(rowid, nil, cols)
	t.noteRowid(rowid)
	return t.AddPendingRow(rowid, cols)
}

// ScanDocs returns the documents a full scan visits in ascending rowid order
// with their stored values (fts5StorageScan). A contentless table without
// columnsize has no scan source and fails like C. Normal and unindexed
// content tables scan %_content itself (C's FTS5_PLAN_SCAN runs
// FTS5_STMT_SCAN_ASC — "SELECT <cols>, rowid FROM %_content ORDER BY rowid"),
// so a document whose content row is missing does not appear even if the
// index still holds it (fts5matchinfo 15.2/15.3).
func (t *Table) ScanDocs() ([]int64, [][]interface{}, error) {
	if t.cfg.EContent == ContentExternal {
		return t.scanExternal()
	}
	if t.cfg.Contentless() && !t.cfg.ColumnSize {
		return nil, nil, fmt.Errorf("%s: table does not support scanning", t.cfg.Name)
	}
	if t.cfg.EContent == ContentNormal || t.cfg.EContent == ContentUnindexed {
		return t.scanContentTable()
	}
	rowids := t.ix.SortedRowids()
	values := make([][]interface{}, len(rowids))
	for i, rowid := range rowids {
		if t.cfg.EContent == ContentNormal {
			values[i] = t.contentValues[rowid]
		}
	}
	return rowids, values, nil
}

// scanContentTable scans %_content itself (C's FTS5_PLAN_SCAN runs
// FTS5_STMT_SCAN_ASC — "SELECT <cols>, rowid FROM %_content ORDER BY rowid"),
// so a document whose content row is missing does not appear even if the
// index still holds it (fts5matchinfo 15.2/15.3).
func (t *Table) scanContentTable() ([]int64, [][]interface{}, error) {
	qc := qual(t.dbName, t.cfg.Name+"_content")
	colList := "id"
	for _, c := range t.contentCols() {
		colList += fmt.Sprintf(", c%d", c)
	}
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT %s FROM %s ORDER BY id ASC", colList, qc))
	if err != nil {
		return nil, nil, err
	}
	rowids := make([]int64, 0, len(rows))
	values := make([][]interface{}, 0, len(rows))
	stored := t.contentCols()
	for _, row := range rows {
		id, ok := asInt64(row[0])
		if !ok {
			continue
		}
		rowids = append(rowids, id)
		values = append(values, t.storedRowValues(row, stored))
	}
	return rowids, values, nil
}

// storedRowValues spreads a %_content row over the user-column slots.
func (t *Table) storedRowValues(row []interface{}, stored []int) []interface{} {
	full := make([]interface{}, len(t.cfg.Columns))
	for j, c := range stored {
		if j+1 < len(row) {
			full[c] = row[j+1]
		}
	}
	return full
}

// DocValues returns one document's stored values in user-column order
// (fts5StorageColumn): the %_content mirror for normal content, a live read
// of the external content table, or NULLs for a contentless table.
func (t *Table) DocValues(rowid int64) ([]interface{}, error) {
	switch t.cfg.EContent {
	case ContentNormal, ContentUnindexed:
		// ContentNormal stores every column; UNINDEXED content stores only
		// the UNINDEXED ones (the mirror already expands them to user
		// positions). Indexed columns of a contentless_unindexed table read
		// as NULL (fts5StorageColumn's content-only paths).
		if v, ok := t.contentValues[rowid]; ok {
			return v, nil
		}
		return make([]interface{}, len(t.cfg.Columns)), nil
	case ContentExternal:
		v, err := t.readExternalValues(rowid)
		if err != nil {
			return nil, err
		}
		if v == nil {
			// C's content fetch fails when the row is absent from the
			// content table (fts5_main.c fts5CursorFetchContent's
			// fts5SetVtabError "fts5: missing row %lld from content table
			// %s"; zContent renders as 'db'.'table' — fts5content 9.5).
			return nil, &MissingContentRowError{
				Rowid:   rowid,
				Content: fmt.Sprintf("'%s'.'%s'", t.dbName, t.cfg.ContentTable),
			}
		}
		return v, nil
	default:
		return nil, nil
	}
}

// SortedMatchRowids returns the union of rowids in the given set, ascending.
func (t *Table) SortedMatchRowids(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for rowid := range set {
		out = append(out, rowid)
	}
	sortRowids(out)
	return out
}

// Rename renames the table and its shadow family (fts5StorageRename).
func (t *Table) Rename(newName string) error {
	return t.renameShadowTables(newName)
}

// Drop removes the shadow family (fts5DestroyMethod).
func (t *Table) Drop() error { return t.dropShadowTables() }

// TableState is a statement-rollback snapshot of the table's in-memory state
// (the shadow tables themselves are covered by the pager snapshot).
type TableState struct {
	ix                 *InvertedIndex
	content            map[int64][]interface{}
	maxRowid           int64
	structRec          *StructRec
	nextSegid          int64
	pendingRowids      []int64
	pendingBytes       int64
	pendingTermState   map[string]*pendingTerm
	nContentlessDelete int64
	docOrigins         map[int64]uint64
}

// Snapshot captures the in-memory state.
func (t *Table) Snapshot() *TableState {
	return &TableState{
		ix:                 t.ix.Snapshot(),
		content:            snapshotContent(t.contentValues),
		maxRowid:           t.maxRowid,
		structRec:          snapshotStructRec(t.structRec),
		nextSegid:          t.nextSegid,
		pendingRowids:      append([]int64(nil), t.pendingRowids...),
		pendingBytes:       t.pendingBytes,
		pendingTermState:   snapshotTermState(t.pendingTermState),
		nContentlessDelete: t.nContentlessDelete,
		docOrigins:         snapshotOrigins(t.docOrigins),
	}
}

// snapshotContent deep-copies the values mirror.
func snapshotContent(src map[int64][]interface{}) map[int64][]interface{} {
	out := make(map[int64][]interface{}, len(src))
	for k, v := range src {
		out[k] = append([]interface{}(nil), v...)
	}
	return out
}

// Restore rolls the in-memory state back to a snapshot.
func (t *Table) Restore(s *TableState) {
	t.ix = s.ix
	t.contentValues = s.content
	t.maxRowid = s.maxRowid
	t.structRec = s.structRec
	t.nextSegid = s.nextSegid
	t.pendingRowids = s.pendingRowids
	t.pendingBytes = s.pendingBytes
	t.pendingTermState = s.pendingTermState
	t.nContentlessDelete = s.nContentlessDelete
	t.docOrigins = s.docOrigins
	t.dirtySegments = nil
	t.bumpVersion()
}

// snapshotStructRec deep-copies a structure record.
func snapshotStructRec(sr *StructRec) *StructRec {
	if sr == nil {
		return nil
	}
	out := &StructRec{V2: sr.V2, NWriteCounter: sr.NWriteCounter, NOriginCntr: sr.NOriginCntr}
	for _, lvl := range sr.Levels {
		var cp []*Segment
		for _, seg := range lvl {
			s2 := *seg
			s2.Rowids = append([]int64(nil), seg.Rowids...)
			s2.Tombs = make(map[int64]bool, len(seg.Tombs))
			for k, v := range seg.Tombs {
				s2.Tombs[k] = v
			}
			cp = append(cp, &s2)
		}
		out.Levels = append(out.Levels, cp)
	}
	return out
}

// snapshotTermState deep-copies the pending-term accounting state.
func snapshotTermState(src map[string]*pendingTerm) map[string]*pendingTerm {
	out := make(map[string]*pendingTerm, len(src))
	for k, v := range src {
		p := *v
		out[k] = &p
	}
	return out
}

// snapshotOrigins deep-copies the docsize origin mirror.
func snapshotOrigins(src map[int64]uint64) map[int64]uint64 {
	out := make(map[int64]uint64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
