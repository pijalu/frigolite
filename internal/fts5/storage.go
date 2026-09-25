package fts5

import (
	"bytes"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"strings"
)

// init: gob registration for the %_data index blob payload.
func init() {
	gob.Register(indexBlob{})
}

// This file implements the fts5 shadow-table storage (fts5_storage.c +
// fts5_index.c). The shadow tables are REAL SQL tables with C's exact schemas
// and creation order — %_data, %_idx, %_content, %_docsize, %_config — so they
// can be read and written through SQL like SQLite's. The %_data block bytes
// use a Go-native encoding (a gob of the document token streams at id=11)
// instead of C's segment records: a documented divergence in the opaque block
// payload only; block ids 1 (averages) and 10 (structure) mirror C's seed
// records.

// indexBlob is the serialized payload of the %_data id=11 block.
type indexBlob struct {
	Docs []blobDoc
}

// blobDoc is one document's token streams in the blob.
type blobDoc struct {
	Rowid int64
	Cols  [][]string
}

// shadowSuffixes lists the shadow-table suffixes in creation order
// (fts5InitVtab: index tables first, then storage tables).
var shadowSuffixes = []string{"_data", "_idx", "_content", "_docsize", "_config"}

// qual renders the schema-qualified shadow table name: an unqualified
// "'t1_data'" in main (the stored sqlite_master SQL of the CLI oracle), a
// '"db".”t1_data”'" form elsewhere (C's %Q.'%q_%q').
func qual(dbName, name string) string {
	if dbName == "" || strings.EqualFold(dbName, "main") {
		return "'" + quoteSQL(name) + "'"
	}
	return `"` + strings.ReplaceAll(dbName, `"`, `""`) + `".'` + quoteSQL(name) + `'`
}

// quoteSQL escapes a single-quoted SQL string.
func quoteSQL(s string) string { return strings.ReplaceAll(s, "'", "''") }

// quoteIdent quotes a SQL identifier with double quotes (contents escaped by
// doubling) so names with spaces or punctuation stay one token.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// sqlLiteral renders a value as a SQL literal for shadow-table DML (the
// vtab.Database contract inlines argument values into the SQL text).
func sqlLiteral(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + quoteSQL(x) + "'"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	case int64:
		return fmt.Sprintf("%d", x)
	case int:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return "'" + quoteSQL(fmt.Sprintf("%v", x)) + "'"
	}
}

// createShadowTables creates the shadow family in C's order and seeds the
// index records (sqlite3Fts5IndexOpen + sqlite3Fts5StorageOpen with bCreate).
func (t *Table) createShadowTables() error {
	name := t.cfg.Name
	q := func(suffix string) string { return qual(t.dbName, name+suffix) }
	ddl := "CREATE TABLE " + q("_data") + "(id INTEGER PRIMARY KEY, block BLOB);" +
		"CREATE TABLE " + q("_idx") + "(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID;"
	if t.cfg.EContent == ContentNormal || t.cfg.EContent == ContentUnindexed {
		content := "CREATE TABLE " + q("_content") + "(id INTEGER PRIMARY KEY"
		for i := range t.cfg.Columns {
			if t.cfg.EContent == ContentUnindexed && !t.cfg.Unindexed[i] {
				continue // only unindexed columns are stored in that mode
			}
			content += fmt.Sprintf(", c%d", i)
		}
		ddl += content + ");"
	}
	if t.cfg.ColumnSize {
		docsize := "CREATE TABLE " + q("_docsize") + "(id INTEGER PRIMARY KEY, sz BLOB"
		if t.cfg.ContentlessDelete {
			docsize += ", origin INTEGER"
		}
		ddl += docsize + ");"
	}
	ddl += "CREATE TABLE " + q("_config") + "(k PRIMARY KEY, v) WITHOUT ROWID;"
	if _, err := t.db.ExecSQL(ddl); err != nil {
		return err
	}
	// Seed the config version row and the two seed blocks C writes
	// (fts5StorageConfigValue 'version', the empty averages record id=1 and
	// the initial structure record id=10 — the V2 form for
	// contentless_delete=1 tables, fts5IndexReinit's nOriginCntr=1).
	t.structRec = newStructRec(t.cfg.ContentlessDelete)
	if t.structRec.V2 {
		t.structRec.NOriginCntr = 1
	}
	t.nextSegid = 1
	seed := fmt.Sprintf("INSERT INTO %s(k, v) VALUES('version', 4);", q("_config")) +
		fmt.Sprintf("INSERT INTO %s(id, block) VALUES(1, X'');", q("_data")) +
		fmt.Sprintf("INSERT INTO %s(id, block) VALUES(10, X'%s');", q("_data"), hexEncode(t.structRec.encode()))
	_, err := t.db.ExecSQL(seed)
	return err
}

// storeConfigValue persists one %_config row (sqlite3Fts5StorageConfigValue).
func (t *Table) storeConfigValue(key string, v interface{}) error {
	qc := qual(t.dbName, t.cfg.Name+"_config")
	_, err := t.db.ExecSQL(fmt.Sprintf("INSERT OR REPLACE INTO %s(k, v) VALUES(%s, %s)",
		qc, sqlLiteral(key), sqlLiteral(v)))
	return err
}

// loadConfigValues reads the %_config rows into the configuration at
// connection time (fts5ConfigLoadSpecial: the rank function survives
// reopen).
func (t *Table) loadConfigValues() error {
	qc := qual(t.dbName, t.cfg.Name+"_config")
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT k, v FROM %s", qc))
	if err != nil {
		return nil // a missing/corrupt config table: defaults
	}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		key, _ := row[0].(string)
		t.applyLoadedConfigRow(strings.ToLower(key), row[1])
	}
	return nil
}

// applyLoadedConfigRow folds one %_config row into the configuration
// (fts5ConfigLoadSpecial: rank survives reopen; secure-delete drives the
// delete path; version records the format the shadow blobs were written
// with).
func (t *Table) applyLoadedConfigRow(key string, val interface{}) {
	switch key {
	case "rank":
		if spec, ok := val.(string); ok {
			if parsed, perr := ParseRankSpec(spec); perr == nil {
				t.cfg.Rank = *parsed
			}
		}
	case "secure-delete":
		if v, ok := asInt64(val); ok {
			t.cfg.SecureDelete = v != 0
		}
	case "version":
		if v, ok := asInt64(val); ok {
			t.cfg.FormatVersion = int(v)
		}
	case "pgsz":
		if v, ok := asInt64(val); ok {
			t.cfg.Pgsz = v
		}
	case "hashsize":
		if v, ok := asInt64(val); ok {
			t.cfg.HashSize = v
		}
	case "automerge":
		if v, ok := asInt64(val); ok {
			t.cfg.Automerge = v
		}
	case "usermerge":
		if v, ok := asInt64(val); ok {
			t.cfg.Usermerge = v
		}
	case "crisismerge":
		if v, ok := asInt64(val); ok {
			t.cfg.CrisisMerge = v
		}
	case "deletemerge":
		if v, ok := asInt64(val); ok {
			t.cfg.DeleteMerge = v
		}
	}
}

// resetIndexStructure drops every persisted segment/tombstone row and
// resets the in-memory structure to the initial (empty) record
// (fts5IndexReinit: a fully emptied index re-seeds averages + structure,
// with nOriginCntr=1 for contentless_delete tables).
func (t *Table) resetIndexStructure() error {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	if _, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id=11", qData)); err != nil {
		return err
	}
	if t.structRec != nil {
		for _, seg := range t.structRec.allSegments() {
			if err := t.removeSegmentRows(seg); err != nil {
				return err
			}
			if err := t.removeTombstoneRows(seg); err != nil {
				return err
			}
		}
	}
	t.pendingReset()
	t.nContentlessDelete = 0
	t.docOrigins = make(map[int64]uint64)
	t.structRec = newStructRec(t.cfg.ContentlessDelete)
	if t.structRec.V2 {
		t.structRec.NOriginCntr = 1
	}
	t.nextSegid = 1
	return t.structureWrite()
}

// detailCols reduces a document's token streams to the detail mode's
// persistence level: detail=none drops them entirely; detail=column keeps
// each column's distinct terms (positions are not persisted).
func detailCols(detail DetailMode, cols [][]string) [][]string {
	switch detail {
	case DetailNone:
		return nil
	case DetailColumns:
		out := make([][]string, len(cols))
		for c, tokens := range cols {
			seen := make(map[string]bool, len(tokens))
			for _, tok := range tokens {
				if seen[tok] {
					continue
				}
				seen[tok] = true
				out[c] = append(out[c], tok)
			}
		}
		return out
	}
	return cols
}

// loadFromShadow rebuilds the in-memory index from the shadow tables
// (fts5IndexOpen's xConnect path): the structure record drives which segment
// payloads restore, each segment's tombstone pages filtering its deleted
// documents. Missing/corrupt blocks yield an empty index; document values for
// normal-content tables come from %_content.
func (t *Table) loadFromShadow() error {
	t.ix = NewInvertedIndex(len(t.cfg.Columns))
	if err := t.loadConfigValues(); err != nil {
		return err
	}
	if t.cfg.EContent == ContentNormal {
		if err := t.loadContentValues(); err != nil {
			return err
		}
	}
	t.docOrigins = make(map[int64]uint64)
	sr, err := t.structureRead()
	if err != nil {
		// A missing/unreadable structure record: fall back to the legacy
		// single-blob payload (pre-segment databases), else start empty.
		t.structRec = newStructRec(t.cfg.ContentlessDelete)
		t.nextSegid = 1
		if blob, ok := t.readShadowIndexBlob(); ok {
			t.restoreShadowDocs(blob)
		}
		t.restoreMaxRowid()
		return nil
	}
	t.structRec = sr
	maxSegid := int64(0)
	for _, seg := range sr.allSegments() {
		if seg.Segid > maxSegid {
			maxSegid = seg.Segid
		}
		seg.Tombs = map[int64]bool{}
		docs, err := t.readSegmentBlob(seg.Segid)
		if err != nil {
			return nil
		}
		for ipg := int64(0); ipg < seg.NPgTombstone; ipg++ {
			pg, err := t.readTombstonePage(seg, ipg)
			if err != nil {
				return nil
			}
			if pg == nil {
				continue
			}
			nSlot := tombstoneNSlot(pg)
			for i := 0; i < nSlot; i++ {
				off := 8 + i*tombstoneKeySize(pg)
				var val uint64
				if tombstoneKeySize(pg) == 4 {
					val = uint64(beUint32(pg[off : off+4]))
				} else {
					val = beUint64(pg[off : off+8])
				}
				if val != 0 {
					seg.Tombs[int64(val)] = true
				}
			}
		}
		for _, bd := range docs {
			if seg.Tombs[bd.Rowid] {
				continue
			}
			t.restoreOneDoc(bd)
		}
	}
	t.nextSegid = maxSegid + 1
	t.restoreMaxRowid()
	// Restore the docsize origins of contentless_delete tables
	// (fts5StorageDelete reads them from %_docsize).
	if t.cfg.ContentlessDelete && t.cfg.ColumnSize {
		qd := qual(t.dbName, t.cfg.Name+"_docsize")
		if rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT id, origin FROM %s", qd)); err == nil {
			for _, row := range rows {
				if id, ok := asInt64(row[0]); ok {
					if origin, ok := asInt64(row[1]); ok {
						t.docOrigins[id] = uint64(origin)
					}
				}
			}
		}
	}
	return nil
}

// restoreMaxRowid seeds the auto-rowid watermark from the shadow tables
// (fts5StorageNewRowid reads max(id) from %_content/%_docsize, not the
// index).
func (t *Table) restoreMaxRowid() {
	q := func(suffix string) string { return qual(t.dbName, t.cfg.Name+suffix) }
	if t.cfg.EContent == ContentNormal || t.cfg.EContent == ContentUnindexed {
		if rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT max(id) FROM %s", q("_content"))); err == nil && len(rows) > 0 {
			if id, ok := asInt64(rows[0][0]); ok && id > t.maxRowid {
				t.maxRowid = id
			}
		}
	}
	if t.cfg.ColumnSize {
		if rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT max(id) FROM %s", q("_docsize"))); err == nil && len(rows) > 0 {
			if id, ok := asInt64(rows[0][0]); ok && id > t.maxRowid {
				t.maxRowid = id
			}
		}
	}
}

// beUint32/beUint64 read big-endian integers.
func beUint32(b []byte) uint32 { return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) }
func beUint64(b []byte) uint64 {
	return uint64(beUint32(b[0:4]))<<32 | uint64(beUint32(b[4:8]))
}

// readShadowIndexBlob decodes the persisted index payload (the id=11 row of
// %_data): ok=false for a missing, foreign or corrupt payload (an empty
// index).
func (t *Table) readShadowIndexBlob() (indexBlob, bool) {
	var blob indexBlob
	qData := qual(t.dbName, t.cfg.Name+"_data")
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT block FROM %s WHERE id=11", qData))
	if err != nil || len(rows) == 0 || rows[0][0] == nil {
		return blob, false // no persisted payload: an empty index
	}
	raw, ok := toBytes(rows[0][0])
	if !ok || len(raw) < 4 || !bytes.Equal(raw[:2], []byte("GF")) {
		return blob, false // foreign or empty payload: treat as empty index
	}
	if err := gob.NewDecoder(bytes.NewReader(raw[2:])).Decode(&blob); err != nil {
		return blob, false
	}
	return blob, true
}

// restoreShadowDocs rebuilds the in-memory index from a decoded legacy blob.
func (t *Table) restoreShadowDocs(blob indexBlob) {
	for _, bd := range blob.Docs {
		t.restoreOneDoc(bd)
	}
}

// restoreOneDoc restores one persisted document.
func (t *Table) restoreOneDoc(bd blobDoc) {
	var values []interface{}
	if stored, ok := t.contentValues[bd.Rowid]; ok {
		values = stored
	}
	cols := bd.Cols
	// A detail=none blob stores no token streams; a normal-content table
	// rebuilds them from %_content so single-term MATCH keeps working
	// after a reopen (C's detail=none segments keep the term rowids).
	if t.cfg.Detail == DetailNone && cols == nil && len(values) > 0 && t.tokErr == nil {
		cols, _ = t.tokenizeValues(values)
	}
	t.ix.AddDoc(bd.Rowid, values, cols)
	t.noteRowid(bd.Rowid)
}

// contentCols lists the column indexes stored in %_content (normal content
// stores every column; UNINDEXED content stores only the UNINDEXED ones).
func (t *Table) contentCols() []int {
	var out []int
	for i := range t.cfg.Columns {
		if t.cfg.EContent == ContentUnindexed && !t.cfg.Unindexed[i] {
			continue
		}
		out = append(out, i)
	}
	return out
}

// loadContentValues reads the %_content rows into the values mirror
// (fts5StorageColumn serves values from %_content; the mirror reads them once
// per connection).
func (t *Table) loadContentValues() error {
	t.contentValues = make(map[int64][]interface{})
	qc := qual(t.dbName, t.cfg.Name+"_content")
	colList := "id"
	for _, c := range t.contentCols() {
		colList += fmt.Sprintf(", c%d", c)
	}
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT %s FROM %s", colList, qc))
	if err != nil {
		return err
	}
	for _, row := range rows {
		id, ok := asInt64(row[0])
		if !ok {
			continue
		}
		// The stored columns are the contentCols subset in declared order
		// (UNINDEXED-content tables store only the UNINDEXED ones); expand
		// them back to full user-column positions.
		full := make([]interface{}, len(t.cfg.Columns))
		for j, c := range t.contentCols() {
			if j+1 < len(row) {
				full[c] = row[j+1]
			}
		}
		t.contentValues[id] = full
	}
	return nil
}

// toBytes extracts a blob value.
func toBytes(v interface{}) ([]byte, bool) {
	switch x := v.(type) {
	case []byte:
		return x, true
	case string:
		return []byte(x), true
	}
	return nil, false
}

// asInt64 coerces a stored value to int64.
func asInt64(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// updateUnindexedContent rewrites the stored (UNINDEXED) columns of one
// document without touching the index — fts5UpdateMethod's bContent branch
// for contentless_unindexed tables (sqlite3Fts5StorageContentInsert with
// bReplace=1 writes only the abUnindexed columns).
func (t *Table) UpdateUnindexedContent(rowid int64, values []interface{}) error {
	if err := t.deleteContentRow(rowid); err != nil {
		return err
	}
	if err := t.insertContentRow(rowid, values); err != nil {
		return err
	}
	stored := make([]interface{}, len(values))
	copy(stored, values)
	for i := range stored {
		if i < len(t.cfg.Unindexed) && !t.cfg.Unindexed[i] {
			stored[i] = nil
		}
	}
	t.contentValues[rowid] = stored
	return nil
}

// insertContentRow writes the %_content row of one document.
func (t *Table) insertContentRow(rowid int64, values []interface{}) error {
	if t.cfg.EContent != ContentNormal && t.cfg.EContent != ContentUnindexed {
		return nil
	}
	var cols strings.Builder
	var vals strings.Builder
	for _, i := range t.contentCols() {
		v := interface{}(nil)
		if i < len(values) {
			v = values[i]
		}
		cols.WriteString(fmt.Sprintf(", c%d", i))
		vals.WriteString(", " + sqlLiteral(v))
	}
	qc := qual(t.dbName, t.cfg.Name+"_content")
	_, err := t.db.ExecSQL(fmt.Sprintf("INSERT INTO %s(id%s) VALUES(%d%s)", qc, cols.String(), rowid, vals.String()))
	return err
}

// deleteContentRow removes a document's %_content row.
func (t *Table) deleteContentRow(rowid int64) error {
	if t.cfg.EContent != ContentNormal && t.cfg.EContent != ContentUnindexed {
		return nil
	}
	qc := qual(t.dbName, t.cfg.Name+"_content")
	_, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id=%d", qc, rowid))
	return err
}

// insertDocsizeRow writes the %_docsize row (per-column token-count varints,
// fts5StorageWriteDocsize).
func (t *Table) insertDocsizeRow(rowid int64) error {
	if !t.cfg.ColumnSize {
		return nil
	}
	doc := t.ix.Doc(rowid)
	var sz []byte
	for c := range t.cfg.Columns {
		n := 0
		if doc != nil && c < len(doc.cols) {
			n = len(doc.cols[c])
		}
		sz = putVarint(sz, uint64(n))
	}
	qd := qual(t.dbName, t.cfg.Name+"_docsize")
	if t.cfg.ContentlessDelete {
		// C's %_docsize carries an origin column for contentless_delete
		// tables (fts5StorageWriteDocsize).
		origin := t.docOrigins[rowid]
		_, err := t.db.ExecSQL(fmt.Sprintf("INSERT OR REPLACE INTO %s(id, sz, origin) VALUES(%d, X'%s', %d)",
			qd, rowid, hex.EncodeToString(sz), origin))
		return err
	}
	_, err := t.db.ExecSQL(fmt.Sprintf("INSERT OR REPLACE INTO %s(id, sz) VALUES(%d, X'%s')",
		qd, rowid, hex.EncodeToString(sz)))
	return err
}

// deleteDocsizeRow removes a document's %_docsize row.
func (t *Table) deleteDocsizeRow(rowid int64) error {
	if !t.cfg.ColumnSize {
		return nil
	}
	qd := qual(t.dbName, t.cfg.Name+"_docsize")
	_, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id=%d", qd, rowid))
	return err
}

// readExternalValues fetches one document's values from the external content
// table (fts5StorageRead's content=<table> path).
func (t *Table) readExternalValues(rowid int64) ([]interface{}, error) {
	// The content lookup steps a %_content read statement (C's bLock scope):
	// a nested query plan against this table while it runs is a content
	// recursion (fts5content 6.x "recursively defined fts5 content table").
	defer t.beginContentScan()()
	cols := strings.Join(quoteCols(t.cfg.Columns), ", ")
	sql := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s = %d",
		quoteIdent(t.cfg.ContentRowid), cols, quoteIdent(t.cfg.ContentTable),
		quoteIdent(t.cfg.ContentRowid), rowid)
	rows, err := t.db.ExecSQL(sql)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return append([]interface{}(nil), rows[0][1:]...), nil
}

// scanExternal reads the whole external content table in rowid order:
// (rowid, values...) pairs (fts5StorageScan's content-table walk).
func (t *Table) scanExternal() ([]int64, [][]interface{}, error) {
	// The content scan steps FTS5_STMT_SCAN_ASC (C's bLock scope): a nested
	// query plan against this table while it runs is a content recursion
	// (fts5_main.c fts5BestIndexMethod's bLock check).
	defer t.beginContentScan()()
	cols := strings.Join(quoteCols(t.cfg.Columns), ", ")
	sql := fmt.Sprintf("SELECT %s, %s FROM %s ORDER BY %s",
		quoteIdent(t.cfg.ContentRowid), cols, quoteIdent(t.cfg.ContentTable), quoteIdent(t.cfg.ContentRowid))
	rows, err := t.db.ExecSQL(sql)
	if err != nil {
		return nil, nil, err
	}
	rowids := make([]int64, 0, len(rows))
	values := make([][]interface{}, 0, len(rows))
	for _, row := range rows {
		id, ok := asInt64(row[0])
		if !ok {
			continue
		}
		rowids = append(rowids, id)
		values = append(values, append([]interface{}(nil), row[1:]...))
	}
	return rowids, values, nil
}

// quoteCols quotes every column name for a SELECT list.
func quoteCols(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c)
	}
	return out
}

// dropShadowTables removes the shadow family (fts5DestroyMethod; the DROP
// TABLE glue calls this after removing the schema entries). Each table is
// dropped IF EXISTS so a partially-created family cleans up cleanly.
func (t *Table) dropShadowTables() error {
	for _, suffix := range shadowSuffixes {
		if suffix == "_content" && t.cfg.Contentless() {
			continue
		}
		if suffix == "_docsize" && !t.cfg.ColumnSize {
			continue
		}
		if _, err := t.db.ExecSQL(fmt.Sprintf("DROP TABLE IF EXISTS %s", qual(t.dbName, t.cfg.Name+suffix))); err != nil {
			return err
		}
	}
	return nil
}

// renameShadowTables renames the existing family to the new name
// (fts5StorageRename; only tables that exist for this configuration move).
func (t *Table) renameShadowTables(newName string) error {
	rename := func(suffix string) error {
		_, err := t.db.ExecSQL(fmt.Sprintf("ALTER TABLE %s RENAME TO '%s';",
			qual(t.dbName, t.cfg.Name+suffix), quoteSQL(newName+suffix)))
		return err
	}
	if err := rename("_data"); err != nil {
		return err
	}
	if err := rename("_idx"); err != nil {
		return err
	}
	if err := rename("_config"); err != nil {
		return err
	}
	if t.cfg.ColumnSize {
		if err := rename("_docsize"); err != nil {
			return err
		}
	}
	if t.cfg.EContent == ContentNormal || t.cfg.EContent == ContentUnindexed {
		if err := rename("_content"); err != nil {
			return err
		}
	}
	t.cfg.Name = newName
	return nil
}
