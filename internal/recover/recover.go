// Package recover implements SQLite's .recover command
// (ext/recover/sqlite3recover.c + dbdata.c): it reads an input database's
// pages directly and emits SQL that rebuilds the recovered schema and rows
// in a fresh output database, routing unreachable pages into a
// lost_and_found table.
package recover

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// Options mirrors sqlite3recover.h's configuration knobs.
type Options struct {
	// IgnoreFreelist skips pages recorded in the freelist when scanning for
	// recoverable content (the .recover -ignore-freelist option).
	IgnoreFreelist bool
	// LostAndFound is the name of the table receiving orphaned rows
	// (default "lost_and_found").
	LostAndFound string
}

// LostAndFoundDefault matches recover.c's LOST_AND_FOUND_TABLE default.
const LostAndFoundDefault = "lost_and_found"

// tableEntry is one recovered sqlite_master row.
type tableEntry struct {
	typ      string
	name     string
	tblName  string
	rootPage int64
	sql      string
	columns  []string
	ipkIndex int // index of INTEGER PRIMARY KEY column, -1 = none
	// autoInc marks tables whose SQL declares AUTOINCREMENT.
	autoInc      bool
	withoutRowid bool
}

// RecoverSQL produces the .recover-style SQL for the database held by pg,
// in the reference CLI's output order: .dbconfig line, BEGIN, PRAGMA
// preamble, schema CREATEs, per-table INSERT OR IGNORE rows, lost_and_found
// content, PRAGMA writable_schema = off, COMMIT.
func RecoverSQL(pg *pager.Pager, opts Options) (string, error) {
	if opts.LostAndFound == "" {
		opts.LostAndFound = LostAndFoundDefault
	}
	hdr := pg.Header()
	if len(hdr) < 100 {
		return "", fmt.Errorf("recover: database header missing")
	}
	enc := map[uint32]string{1: "UTF-8", 2: "UTF-16le", 3: "UTF-16be"}[binary.BigEndian.Uint32(hdr[56:60])]
	userVersion := binary.BigEndian.Uint32(hdr[60:64])
	appID := binary.BigEndian.Uint32(hdr[68:72])
	autoVacuum := binary.BigEndian.Uint32(hdr[52:56])

	entries, err := readSchema(pg)
	if err != nil {
		return "", err
	}

	// Build the recovery state and compute reachability so we can find
	// orphan pages after schema emission (lost_and_found rows).
	rs := &recoveryState{pg: pg, opts: opts}
	for _, e := range entries {
		if e.rootPage > 0 {
			rs.reachRoots = append(rs.reachRoots, uint32(e.rootPage))
		}
	}

	var sb strings.Builder
	sb.WriteString("BEGIN;\n")
	sb.WriteString("PRAGMA writable_schema = on;\n")
	sb.WriteString("PRAGMA foreign_keys = off;\n")
	fmt.Fprintf(&sb, "PRAGMA encoding = '%s';\n", enc)
	fmt.Fprintf(&sb, "PRAGMA page_size = '%d';\n", pg.PageSize())
	fmt.Fprintf(&sb, "PRAGMA auto_vacuum = '%d';\n", autoVacuum)
	fmt.Fprintf(&sb, "PRAGMA user_version = '%d';\n", userVersion)
	fmt.Fprintf(&sb, "PRAGMA application_id = '%d';\n", appID)

	// AUTOINCREMENT tables imply the output needs sqlite_sequence.
	hasAutoInc := false
	for _, e := range entries {
		if e.typ == "table" && e.autoInc {
			hasAutoInc = true
			break
		}
	}
	if hasAutoInc {
		sb.WriteString("CREATE TABLE sqlite_sequence(name,seq);\n")
	}

	// Schema CREATEs in schema order, then each table's rows.
	var sequenceRows []string
	for _, e := range entries {
		switch e.typ {
		case "table":
			if e.name == "sqlite_sequence" {
				// The canonical CREATE is emitted in the preamble block; the
				// input's own schema entry is not re-emitted. Its ROWS still
				// are (recoverTableRows routes to appendSequenceRows).
				if err := recoverTableRows(&sb, pg, e, &sequenceRows); err != nil {
					return "", err
				}
				continue
			}
			if e.sql != "" {
				sb.WriteString(e.sql)
				if !strings.HasSuffix(e.sql, ";") {
					sb.WriteString(";")
				}
				sb.WriteString("\n")
			}
			if err := recoverTableRows(&sb, pg, e, &sequenceRows); err != nil {
				return "", err
			}
		case "index", "view", "trigger":
			if e.sql != "" {
				sb.WriteString(e.sql)
				if !strings.HasSuffix(e.sql, ";") {
					sb.WriteString(";")
				}
				sb.WriteString("\n")
			}
		}
	}

	if hasAutoInc {
		sb.WriteString("DELETE FROM sqlite_sequence;\n")
		for _, r := range sequenceRows {
			sb.WriteString(r)
		}
	}

	// Orphaned-page recovery (lost_and_found): pages unreachable from any
	// schema tree and not on the freelist are decoded into the lost_and_found
	// table. The CREATE TABLE lost_and_found column count (c0..cN) is the
	// maximum field count observed across all orphans.
	orphans, maxFields, err := rs.collectOrphans()
	if err != nil {
		return "", err
	}
	if len(orphans) > 0 {
		// The output name must not collide with a table the schema
		// CREATEs already emitted (recoverLostAndFoundCreate probes
		// sqlite_schema: lost_and_found, lost_and_found_0, ...).
		lafName := opts.LostAndFound
		taken := map[string]bool{}
		for _, e := range entries {
			if e.typ == "table" {
				taken[e.name] = true
			}
		}
		if taken[lafName] {
			for i := 0; ; i++ {
				cand := opts.LostAndFound + "_" + strconv.Itoa(i)
				if !taken[cand] {
					lafName = cand
					break
				}
			}
		}
		fmt.Fprintf(&sb, "CREATE TABLE %s(rootpgno INTEGER, pgno INTEGER, nfield INTEGER, id INTEGER",
			lafName)
		for i := 0; i < maxFields; i++ {
			fmt.Fprintf(&sb, ", c%d", i)
		}
		sb.WriteString(");\n")
		for _, r := range orphans {
			parts := []string{
				strconv.FormatInt(int64(r.root), 10),
				strconv.FormatInt(int64(r.pgno), 10),
				strconv.FormatInt(r.nfield, 10),
			}
			if r.id == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, renderValue(r.id))
			}
			for _, v := range r.values {
				parts = append(parts, v)
			}
			fmt.Fprintf(&sb, "INSERT INTO %s VALUES(%s);\n",
				lafName, strings.Join(parts, ", "))
		}
	}

	sb.WriteString("PRAGMA writable_schema = off;\n")
	sb.WriteString("COMMIT;\n")
	return sb.String(), nil
}

// readSchema walks the sqlite_master btree (root page 1) and decodes its
// (type, name, tbl_name, rootpage, sql) rows.
func readSchema(pg *pager.Pager) ([]tableEntry, error) {
	tree := btree.NewBTree(pg, 1, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}
	var entries []tableEntry
	for {
		payload, _, err := cursor.ReadCellData()
		if err != nil {
			break
		}
		recR, derr := storage.DecodeRecord(payload)
		var rec []interface{}
		if derr == nil && recR != nil {
			rec = recR.Values
		}
		if len(rec) >= 5 {
			e := tableEntry{
				typ:      textOf(rec[0]),
				name:     textOf(rec[1]),
				tblName:  textOf(rec[2]),
				rootPage: int64(intOf(rec[3])),
				sql:      textOf(rec[4]),
			}
			if e.typ == "table" {
				parseTableColumns(e.sql, &e)
			}
			entries = append(entries, e)
		}
		if ok, aerr := cursor.Next(); aerr != nil || !ok {
			break
		}
	}
	return entries, nil
}

// recoverTableRows walks one table's btree and emits its recovered rows.
func recoverTableRows(sb *strings.Builder, pg *pager.Pager, e tableEntry, sequenceRows *[]string) error {
	if e.rootPage <= 0 || e.typ != "table" {
		return nil
	}
	if e.name == "sqlite_sequence" {
		// The sequence table's rows become the output's sequence state;
		// emitted after DELETE FROM sqlite_sequence by the caller.
		return appendSequenceRows(sb, pg, e, sequenceRows)
	}
	tree := btree.NewBTree(pg, uint32(e.rootPage), true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return err
	}
	// Page-type-aware WITHOUT ROWID mapping: oracle files store WR
	// tables as index btrees (0x0a/0x02, PK-first iField layout per
	// PRAGMA index_xinfo); frigolite's writer stores declared-column
	// order on 0x0d table-leaf pages, which need identity mapping.
	wrIdentity := isDeclaredOrderWR(pg, e)
	var ifield []int
	if e.withoutRowid && !wrIdentity {
		ifield = e.iField()
	}
	for {
		payload, rowID, err := cursor.ReadCellData()
		if err != nil {
			break
		}
		recR, derr := storage.DecodeRecord(payload)
		var rec []interface{}
		if derr == nil && recR != nil {
			rec = recR.Values
		}
		if derr != nil {
			if _, aerr := cursor.Next(); aerr != nil {
				break
			}
			continue
		}
		var values []string
		if e.withoutRowid {
			// wrIdentity (0x0d engine pages): declared order, map
			// by identity. Index pages: PK-first iField layout.
			values = make([]string, len(e.columns))
			for di := range e.columns {
				v := "NULL"
				si := di
				if !wrIdentity {
					si = ifield[di]
				}
				if si < len(rec) {
					v = renderValue(rec[si])
				}
				values[di] = v
			}
		} else {
			values = make([]string, len(e.columns))
			for i := range e.columns {
				v := "NULL"
				if i < len(rec) {
					v = renderValue(rec[i])
				}
				// An INTEGER PRIMARY KEY column is the rowid alias: its value
				// is the btree rowid (the record slot holds NULL).
				if i == e.ipkIndex {
					v = strconv.FormatInt(rowID, 10)
				}
				values[i] = v
			}
		}
		cols := quotedColumnList(e.columns, e.ipkIndex, e.withoutRowid)
		if e.ipkIndex < 0 && !e.withoutRowid {
			// A rowid table without an INTEGER PRIMARY KEY column stores the
			// rowid separately: expose it as _rowid_.
			values = append([]string{strconv.FormatInt(rowID, 10)}, values...)
		}
		fmt.Fprintf(sb, "INSERT OR IGNORE INTO '%s'(%s) VALUES (%s);\n",
			e.name, cols, strings.Join(values, ", "))
		if ok, aerr := cursor.Next(); aerr != nil || !ok {
			break
		}
	}
	return nil
}

// isDeclaredOrderWR reports whether a WITHOUT ROWID table's root page
// holds declared-column-order records on table-btree pages (0x0d/0x05,
// frigolite's writer layout) rather than PK-first index records
// (0x0a/0x02, oracle layout per PRAGMA index_xinfo iField).
func isDeclaredOrderWR(pg *pager.Pager, e tableEntry) bool {
	if !e.withoutRowid || e.rootPage <= 0 {
		return false
	}
	raw, rerr := pg.ReadPage(uint32(e.rootPage))
	if rerr != nil {
		return false
	}
	coff := contentOffset(uint32(e.rootPage))
	if coff >= len(raw.Data) {
		return false
	}
	pt := raw.Data[coff]
	return pt == 0x0d || pt == 0x05
}

// appendSequenceRows reads the input's sqlite_sequence rows and queues them
// for emission after DELETE FROM sqlite_sequence.
func appendSequenceRows(sb *strings.Builder, pg *pager.Pager, e tableEntry, sequenceRows *[]string) error {
	tree := btree.NewBTree(pg, uint32(e.rootPage), true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	rowID := int64(0)
	for {
		payload, rid, err := cursor.ReadCellData()
		if err != nil {
			break
		}
		rowID = rid
		recR, derr := storage.DecodeRecord(payload)
		var rec []interface{}
		if derr == nil && recR != nil {
			rec = recR.Values
		}
		if derr == nil && len(rec) >= 2 {
			*sequenceRows = append(*sequenceRows, fmt.Sprintf(
				"INSERT OR IGNORE INTO 'sqlite_sequence'(_rowid_, 'name', 'seq') VALUES (%d, %s, %s);\n",
				rowID, renderValue(rec[0]), renderValue(rec[1])))
		}
		if ok, aerr := cursor.Next(); aerr != nil || !ok {
			break
		}
	}
	return nil
}

// quotedColumnList renders the INSERT column list: _rowid_ first for rowid
// tables without an INTEGER PRIMARY KEY, then each column quoted.
func quotedColumnList(columns []string, ipkIndex int, withoutRowid bool) string {
	parts := make([]string, 0, len(columns)+1)
	if ipkIndex < 0 && !withoutRowid {
		parts = append(parts, "_rowid_")
	}
	for _, c := range columns {
		parts = append(parts, "'"+c+"'")
	}
	return strings.Join(parts, ", ")
}

// --- WITHOUT ROWID mapping (tranche 3) ---

// iField returns, for each declared column index i, the storage-field index
// in the WITHOUT ROWID index btree's record (sqlite3recover.c's
// RecoverColumn.iField, populated from PRAGMA index_xinfo: PK columns in key
// order first, then the remaining declared columns in declared order;
// oracle bytes for (1,2,3) with PK(b,c) are [2 3 1]).
func (e tableEntry) iField() []int {
	declared := e.columns
	pkCols := e.primaryKeyColumns()
	pkSet := map[string]bool{}
	ifield := make([]int, len(declared))
	storage := 0
	for _, pk := range pkCols {
		for di, c := range declared {
			if strings.EqualFold(c, pk) && !pkSet[strings.ToLower(c)] {
				pkSet[strings.ToLower(c)] = true
				ifield[di] = storage
				storage++
				break
			}
		}
	}
	for i := range declared {
		if !pkSet[strings.ToLower(declared[i])] {
			ifield[i] = storage
			storage++
		}
	}
	return ifield
}

// primaryKeyColumns extracts the PRIMARY KEY(...) column list from the
// schema SQL (table-constraint form).
func (e *tableEntry) primaryKeyColumns() []string {
	up := strings.ToUpper(e.sql)
	i := strings.Index(up, "PRIMARY KEY(")
	if i < 0 {
		i = strings.Index(up, "PRIMARY KEY (")
		if i < 0 {
			return nil
		}
	}
	open := strings.Index(up[i:], "(") + i
	depth, inStr, end := 0, byte(0), -1
	for j := open; j < len(e.sql); j++ {
		c := e.sql[j]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'':
			inStr = '\''
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = j
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	var cols []string
	for _, f := range splitTopLevel(e.sql[open+1 : end]) {
		cols = append(cols, firstIdentifier(f))
	}
	return cols
}

// --- lost_and_found (tranche 4) ---

// orphanRow is one recovered row from an unreachable page.
type orphanRow struct {
	root   uint32
	pgno   uint32
	nfield int64
	id     interface{} // rowid int64 or nil (index btree)
	values []string
}

// collectOrphans walks every page not reachable from the schema trees (and,
// when opts.IgnoreFreelist is true, not on the freelist) and decodes btree
// content into lost_and_found rows. Returns the rows and the maximum field
// count. The CLI .recover default treats the freelist as corrupt and recovers
// its pages as orphans — opts.IgnoreFreelist (=-ignore-freelist) opts in to
// honoring the freelist (sqlite3_recover_config SQLITE_RECOVER_FREELIST_CORRUPT).
func (r *recoveryState) collectOrphans() ([]orphanRow, int, error) {
	reachable := map[uint32]bool{1: true}
	for _, root := range r.reachRoots {
		r.markTree(root, reachable)
	}
	free := map[uint32]bool{}
	if r.opts.IgnoreFreelist {
		head := r.freelistHead()
		for h := head; h >= 2 && !free[h]; {
			free[h] = true
			pg, err := r.pg.ReadPage(h)
			if err != nil {
				break
			}
			next := binary.BigEndian.Uint32(pg.Data[0:4])
			k := binary.BigEndian.Uint32(pg.Data[4:8])
			for i := uint32(0); i < k && 8+4*i+4 <= uint32(len(pg.Data)); i++ {
				if leaf := binary.BigEndian.Uint32(pg.Data[8+4*i : 8+4*i+4]); leaf >= 2 {
					free[leaf] = true
				}
			}
			h = next
		}
	}
	var rows []orphanRow
	maxFields := 0
	visited := map[uint32]bool{}
	for pgno := uint32(2); pgno <= r.pg.NumPages(); pgno++ {
		if reachable[pgno] || free[pgno] || visited[pgno] {
			continue
		}
		pgd, err := r.pg.ReadPage(pgno)
		if err != nil {
			continue
		}
		ptype := pgd.Data[contentOffset(pgno)]
		if ptype != 0x0d && ptype != 0x0a && ptype != 0x05 && ptype != 0x02 {
			continue
		}
		// An unreached btree page starts a new orphan tree.
		orphanRows, err := r.walkOrphanTree(pgno, pgno, visited, &maxFields)
		if err != nil {
			continue
		}
		rows = append(rows, orphanRows...)
	}
	return rows, maxFields, nil
}

// walkOrphanTree decodes one orphan subtree, emitting its rows.
func (r *recoveryState) walkOrphanTree(root, pgno uint32, visited map[uint32]bool, maxFields *int) ([]orphanRow, error) {
	if visited[pgno] || pgno < 2 || pgno > r.pg.NumPages() {
		return nil, nil
	}
	visited[pgno] = true
	pgd, err := r.pg.ReadPage(pgno)
	if err != nil {
		return nil, nil
	}
	coff := contentOffset(pgno)
	ptype := pgd.Data[coff]
	var rows []orphanRow
	switch ptype {
	case 0x05, 0x02: // interior: recurse children (cells' left children + rightmost)
		page, perr := storage.ParsePage(pgd.Data, int(r.pg.PageSize()), coff)
		if perr != nil {
			return nil, nil
		}
		var children []uint32
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pgd.Data, coff+4, i, int(r.pg.PageSize())))
			if cellOff+4 <= len(pgd.Data) {
				children = append(children, binary.BigEndian.Uint32(pgd.Data[cellOff:cellOff+4]))
			}
		}
		children = append(children, binary.BigEndian.Uint32(pgd.Data[coff+8:coff+12]))
		for _, ch := range children {
			sub, err := r.walkOrphanTree(root, ch, visited, maxFields)
			if err != nil {
				return nil, err
			}
			rows = append(rows, sub...)
		}
	case 0x0d: // table leaf: rowid rows
		page, perr := storage.ParsePage(pgd.Data, int(r.pg.PageSize()), coff)
		if perr != nil {
			return nil, nil
		}
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pgd.Data, coff, i, int(r.pg.PageSize())))
			// Leaf table cells: CellPointer base is the page-content offset.
			payload, rowID, err := r.readLeafCellPayload(pgd.Data, cellOff, true)
			if err != nil {
				continue
			}
			rec, derr := storage.DecodeRecord(payload)
			if derr != nil || rec == nil {
				continue
			}
			vals := make([]string, len(rec.Values))
			n := 0
			for j, v := range rec.Values {
				vals[j] = renderValue(v)
				n++
			}
			if n > *maxFields {
				*maxFields = n
			}
			rows = append(rows, orphanRow{root: root, pgno: pgno, nfield: int64(n), id: rowID, values: vals})
		}
	case 0x0a: // index leaf: pure records, no rowid
		page, perr := storage.ParsePage(pgd.Data, int(r.pg.PageSize()), coff)
		if perr != nil {
			return nil, nil
		}
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pgd.Data, coff, i, int(r.pg.PageSize())))
			payload, _, err := r.readLeafCellPayload(pgd.Data, cellOff, false)
			if err != nil {
				continue
			}
			rec, derr := storage.DecodeRecord(payload)
			if derr != nil || rec == nil {
				continue
			}
			vals := make([]string, len(rec.Values))
			n := 0
			for j, v := range rec.Values {
				vals[j] = renderValue(v)
				n++
			}
			if n > *maxFields {
				*maxFields = n
			}
			rows = append(rows, orphanRow{root: root, pgno: pgno, nfield: int64(n), id: nil, values: vals})
		}
	}
	return rows, nil
}

// readLeafCellPayload decodes one leaf cell: payload length varint, optional
// rowid varint (for table leaves), local payload, overflow chain reassembly.
// hasRowid must be true for 0x0d (table leaf) cells.
func (r *recoveryState) readLeafCellPayload(pageData []byte, cellOff int, hasRowid bool) ([]byte, int64, error) {
	if cellOff < 0 || cellOff >= len(pageData) {
		return nil, 0, fmt.Errorf("cell offset out of range")
	}
	pos := cellOff
	plen64, n := varintAt(pageData, pos)
	plen := int(plen64)
	pos += n
	rowid := int64(0)
	if hasRowid {
		rowid64, n2 := varintAt(pageData, pos)
		rowid = int64(rowid64)
		pos += n2
	}
	usable := int(r.pg.UsableSize())
	maxLocal := usable - 35
	minLocal := ((usable - 12) * 32 / 255) - 23
	if !hasRowid {
		maxLocal = ((usable - 12) * 64 / 255) - 23
	}
	local := plen
	if plen > maxLocal {
		surplus := minLocal + (plen-minLocal)%(usable-4)
		local = surplus
		if local > maxLocal {
			local = minLocal
		}
	}
	if local < 0 || pos+local > len(pageData) {
		return nil, 0, fmt.Errorf("cell payload truncated")
	}
	out := make([]byte, 0, plen)
	out = append(out, pageData[pos:pos+local]...)
	if plen <= local {
		return out, rowid, nil
	}
	if pos+local+4 > len(pageData) {
		return nil, 0, fmt.Errorf("cell overflow pointer missing")
	}
	next := binary.BigEndian.Uint32(pageData[pos+local : pos+local+4])
	for next != 0 && next <= r.pg.NumPages() {
		pg, err := r.pg.ReadPage(next)
		if err != nil {
			break
		}
		chunk := usable - 4
		take := chunk
		if rem := plen - len(out); rem < take {
			take = rem
		}
		out = append(out, pg.Data[4:4+take]...)
		if len(out) >= plen {
			break
		}
		next = binary.BigEndian.Uint32(pg.Data[0:4])
	}
	return out, rowid, nil
}

// varintAt decodes one big-endian SQLite varint at data[pos].
func varintAt(data []byte, pos int) (uint64, int) {
	var v uint64
	for i := 0; i < 8 && pos+i < len(data); i++ {
		v = v<<7 | uint64(data[pos+i]&0x7f)
		if data[pos+i]&0x80 == 0 {
			return v, i + 1
		}
	}
	if pos+8 < len(data) {
		v = v<<8 | uint64(data[pos+8])
	}
	return v, 9
}

// contentOffset returns the btree header size for a page (100 for page 1).
func contentOffset(pgno uint32) int {
	if pgno == 1 {
		return 100
	}
	return 0
}
