package fts5

import (
	"os"
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
	// the empty structure record id=10).
	fmt.Fprintf(os.Stderr, "SEED-CONFIG db=%s\n", t.dbName)
	seed := fmt.Sprintf("INSERT INTO %s(k, v) VALUES('version', 4);", q("_config")) +
		fmt.Sprintf("INSERT INTO %s(id, block) VALUES(1, X'');", q("_data")) +
		fmt.Sprintf("INSERT INTO %s(id, block) VALUES(10, X'00000000000000');", q("_data"))
	_, err := t.db.ExecSQL(seed)
	return err
}

// storeConfigValue persists one %_config row (sqlite3Fts5StorageConfigValue).
func (t *Table) storeConfigValue(key string, v interface{}) error {
	qc := qual(t.dbName, t.cfg.Name+"_config")
	fmt.Fprintf(os.Stderr, "SCV %s=%s db=%s\n", key, v, t.dbName)
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
		switch strings.ToLower(key) {
		case "rank":
			if spec, ok := row[1].(string); ok {
				if parsed, perr := ParseRankSpec(spec); perr == nil {
					t.cfg.Rank = *parsed
				}
			}
		}
	}
	return nil
}

// flushShadowIndex rewrites the %_data id=11 block with the serialized token
// streams (sqlite3Fts5StorageSync's persistence point).
func (t *Table) flushShadowIndex() error {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	var payload bytes.Buffer
	payload.WriteString("GF") // magic checked by loadFromShadow
	blob := indexBlob{Docs: make([]blobDoc, 0)}
	for _, rowid := range t.ix.SortedRowids() {
		doc := t.ix.Doc(rowid)
		blob.Docs = append(blob.Docs, blobDoc{Rowid: rowid, Cols: doc.cols})
	}
	if err := gob.NewEncoder(&payload).Encode(blob); err != nil {
		return err
	}
	hexed := hex.EncodeToString(payload.Bytes())
	_, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id=11; INSERT INTO %s(id, block) VALUES(11, X'%s');",
		qData, qData, hexed))
	return err
}

// loadFromShadow rebuilds the in-memory index from the shadow tables
// (fts5IndexOpen's xConnect path). Missing/corrupt blocks yield an empty
// index; document values for normal-content tables come from %_content.
func (t *Table) loadFromShadow() error {
	t.ix = NewInvertedIndex(len(t.cfg.Columns))
	if err := t.loadConfigValues(); err != nil {
		fmt.Fprintf(os.Stderr, "LFS config err=%v\n", err)
		return err
	}
	if t.cfg.EContent == ContentNormal {
		if err := t.loadContentValues(); err != nil {
			return err
		}
	}
	qData := qual(t.dbName, t.cfg.Name+"_data")
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT block FROM %s WHERE id=11", qData))
	if err != nil || len(rows) == 0 || rows[0][0] == nil {
		return err // no persisted payload: an empty index
	}
	raw, ok := toBytes(rows[0][0])
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "LFS raw ok=%v len=%d head=%q\n", ok, len(raw), string(raw[:min(8, len(raw))]))
	}
	if !ok || len(raw) < 4 || !bytes.Equal(raw[:2], []byte("GF")) {
		return nil // foreign or empty payload: treat as empty index
	}
	var blob indexBlob
	if err := gob.NewDecoder(bytes.NewReader(raw[2:])).Decode(&blob); err != nil {
		if os.Getenv("CL_DBG") != "" {
			fmt.Fprintf(os.Stderr, "LFS gob err=%v\n", err)
		}
		return nil
	}
	for _, bd := range blob.Docs {
		var values []interface{}
		if stored, ok := t.contentValues[bd.Rowid]; ok {
			values = stored
		}
		t.ix.AddDoc(bd.Rowid, values, bd.Cols)
		t.noteRowid(bd.Rowid)
	}
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "LFS docs=%d cols0=%v\n", len(blob.Docs), blob.Docs[0].Cols)
	}
	return nil
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
		t.contentValues[id] = append([]interface{}(nil), row[1:]...)
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
	cols := strings.Join(t.cfg.Columns, ", ")
	sql := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s = %d",
		quoteSQL(t.cfg.ContentRowid), cols, quoteSQL(t.cfg.ContentTable),
		quoteSQL(t.cfg.ContentRowid), rowid)
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
	cols := strings.Join(quoteCols(t.cfg.Columns), ", ")
	sql := fmt.Sprintf("SELECT %s, %s FROM %s ORDER BY %s",
		quoteSQL(t.cfg.ContentRowid), cols, quoteSQL(t.cfg.ContentTable), quoteSQL(t.cfg.ContentRowid))
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
		out[i] = quoteSQL(c)
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
