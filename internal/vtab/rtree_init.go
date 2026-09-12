package vtab

import (
	"fmt"
	"strings"
)

// This file holds the rtree module's xCreate/xConnect side (rtree.c rtreeInit,
// getNodeSize and rtreeSqlInit): module-argument parsing, declared-column
// naming, node sizing and the shadow-table family lifecycle.

// connect builds one bound instance from the module's USING arguments (both
// xCreate and xConnect route here; frigolite passes only the column arguments
// — the db/table name arrives via BindSchema). Column 0 is the INTEGER PRIMARY
// KEY (rowid); the remaining columns are coordinate pairs (min,max),
// optionally followed by '+'-prefixed auxiliary columns.
func (m *RtreeModule[T]) connect(args []string, isCreate bool) (VirtualTable, error) {
	columns := append([]string(nil), args...)
	for i := range columns {
		columns[i] = strings.TrimSpace(columns[i])
	}
	// SQLite parses the module-argument list as SQL identifiers; a bare
	// reserved keyword (rtree1-10.1: USING rtree(index, ...)) fails prepare
	// with the parser's message. The checked name is the argument's first
	// token (rtreeTokenLength), matching what declare_vtab would parse.
	for _, c := range columns {
		if tok := rtreeFirstToken(strings.TrimPrefix(c, "+")); rtreeReservedWord(tok) {
			return nil, fmt.Errorf("near %q: syntax error", tok)
		}
	}
	// Aux ('+') columns, rtree.c rtreeInit: the counting loop scans EVERY
	// argument after the rowid column with no break. The FIRST '+' argument
	// ends the coordinate block and every remaining argument must also be
	// '+'-prefixed (a non-aux column after an auxiliary one is aErrMsg[4],
	// "Auxiliary rtree columns must be last").
	coordEnd := len(columns)
	for i := 1; i < len(columns); i++ {
		if strings.HasPrefix(columns[i], "+") {
			coordEnd = i
			break
		}
	}
	nAux := 0
	for i := coordEnd; i < len(columns); i++ {
		if !strings.HasPrefix(columns[i], "+") {
			return nil, fmt.Errorf("Auxiliary rtree columns must be last")
		}
		nAux++
	}
	nDim2 := coordEnd - 1 // coordinate columns after the rowid column
	// Argument validation mirrors rtree.c rtreeInit and its exact messages:
	// at least the id + one min/max pair; at most RTREE_MAX_DIMENSIONS pairs;
	// an odd coordinate count leaves a dimension without a upper bound.
	if coordEnd < 3 {
		return nil, fmt.Errorf("Too few columns for an rtree table")
	}
	if nDim2 > RTREE_MAX_DIMENSIONS*2 {
		return nil, fmt.Errorf("Too many columns for an rtree table")
	}
	if nDim2%2 != 0 {
		return nil, fmt.Errorf("Wrong number of columns for an rtree table")
	}
	v := &rtreeVTab[T]{
		module:        m,
		columns:       columns,
		nDim:          nDim2 / 2,
		nDim2:         nDim2,
		nAux:          nAux,
		nBytesPerCell: 8 + nDim2*4,
		coordKind:     m.coordKind,
	}
	// declare_vtab parity (rtreeTokenLength): a declared column's SQL name is
	// the FIRST TOKEN of its module argument — "+c3 BLOB" declares "c3" (the
	// " BLOB" suffix is type text, not part of the name, and auxiliary
	// columns are declared without a type). Columns()/ColumnTypes() drive SQL
	// name resolution, PRAGMA table_info, INSERT's named-column matching and
	// the constraint-failure messages (rtree.c rtreeConstraintError reads the
	// declared names via sqlite3_column_name). The raw list stays in
	// v.columns for the nAux bookkeeping above.
	v.declared = make([]string, len(columns))
	for i, c := range columns {
		v.declared[i] = rtreeFirstToken(strings.TrimPrefix(c, "+"))
	}
	v.created = isCreate
	if err := v.queryStat1(); err != nil {
		return nil, err
	}
	return v, nil
}

// rtreeFirstToken extracts the first SQL token of one module argument,
// mirroring rtree.c rtreeTokenLength (sqlite3GetToken): a quoted identifier
// spans to its closing quote; an unquoted identifier is the run of identifier
// characters (alnum, '_', '$', bytes >= 0x80). Leading whitespace and SQL
// comments are skipped: SQLite's tokenizer drops comments before any token
// reaches rtreeInit, so a declaration like "id, -- comment\n minX" declares
// plain "minX".
func rtreeFirstToken(arg string) string {
	s := strings.TrimSpace(arg)
	for s != "" {
		if strings.HasPrefix(s, "--") {
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = strings.TrimSpace(s[nl+1:])
				continue
			}
			return ""
		}
		if strings.HasPrefix(s, "/*") {
			if e := strings.Index(s, "*/"); e >= 0 {
				s = strings.TrimSpace(s[e+2:])
				continue
			}
			return ""
		}
		break
	}
	if s == "" {
		return ""
	}
	switch s[0] {
	case '"', '`':
		if e := strings.IndexByte(s[1:], s[0]); e >= 0 {
			return s[:e+2]
		}
		return s
	case '[':
		if e := strings.IndexByte(s, ']'); e >= 0 {
			return s[:e+1]
		}
		return s
	}
	n := 0
	for n < len(s) {
		c := s[n]
		if c >= 0x80 || c == '_' || c == '$' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			n++
			continue
		}
		break
	}
	return s[:n]
}

// rtreeReservedWord reports whether the bare identifier is one of the SQL
// keywords SQLite's lexer refuses as an unquoted column name inside a module
// declaration (subset covering the common cases; quoted names pass through).
func rtreeReservedWord(word string) bool {
	switch strings.ToUpper(strings.TrimSpace(word)) {
	case "INDEX", "WHERE", "ORDER", "GROUP", "SELECT", "FROM", "TABLE",
		"CREATE", "VALUES", "LIMIT", "UNION", "HAVING", "SET", "PRIMARY",
		"UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT", "NOT", "NULL", "AND",
		"OR", "IN", "IS", "BETWEEN", "LIKE", "GLOB", "CASE", "WHEN", "THEN",
		"ELSE", "END", "JOIN", "LEFT", "RIGHT", "FULL", "INNER", "OUTER",
		"CROSS", "NATURAL", "USING", "ON", "AS", "BY", "DESC", "ASC",
		"DEFAULT", "COLLATE", "CURRENT", "MATCH", "REGEXP", "EXISTS":
		return true
	}
	return false
}

// queryStat1 mirrors rtree.c rtreeQueryStat1, which runs on every xCreate and
// xConnect. It estimates the row count from sqlite_stat1 (tbl='<name>_rowid'),
// falling back to RTREE_DEFAULT_ROWEST when sqlite_stat1 is absent or has no
// matching row. The probe SELECT is *prepared* against sqlite_stat1, so when a
// hostile schema shadows the reserved name with a table lacking the `stat`
// column (vtabK: an fts5 virtual table renamed onto sqlite_stat1 via
// writable_schema), prepare fails with "no such column: stat" and the CREATE
// or CONNECT aborts with that exact error.
//
// rtree.c uses sqlite3_table_column_metadata for the existence check, which
// returns SQLITE_ERROR for a virtual table; here the probe SELECT itself is the
// existence/column check, which is equivalent for the error path and cheaper.
func (v *rtreeVTab[T]) queryStat1() error {
	v.nRowEst = RTREE_DEFAULT_ROWEST
	// zFmt: SELECT stat FROM %Q.sqlite_stat1 WHERE tbl = '%q_rowid'
	db := v.dbName
	if db == "" {
		db = "main"
	}
	sql := fmt.Sprintf("SELECT stat FROM %s.sqlite_stat1 WHERE tbl = '%s_rowid'",
		dquoteIdent(db), strings.ReplaceAll(v.name, "'", "''"))
	rows, err := v.module.db.ExecSQL(sql)
	if err != nil {
		// No sqlite_stat1 table at all → default estimate, no error (the C
		// metadata call returns SQLITE_ERROR which rtreeQueryStat1 swallows).
		// Any other prepare/resolve failure (no such column: stat) propagates.
		if isNoSuchTable(err) {
			return nil
		}
		return err
	}
	nRow := int64(RTREE_MIN_ROWEST)
	if len(rows) > 0 && len(rows[0]) > 0 {
		if n := rtreeAsInt64(rows[0][0]); n > 0 {
			nRow = n
		}
	}
	if nRow < RTREE_MIN_ROWEST {
		nRow = RTREE_MIN_ROWEST
	}
	v.nRowEst = nRow
	return nil
}

// isNoSuchTable reports whether err is the "no such table" resolution failure
// (sqlite_stat1 simply absent), which rtreeQueryStat1 treats as
// RTREE_DEFAULT_ROWEST rather than an error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// dquoteIdent renders an SQL identifier (schema name) double-quoted for use in
// the stat1 probe, escaping embedded double quotes. Mirrors sqlite3_mprintf
// "%Q" applied to the %Q.sqlite_stat1 format. (vtab's existing quoteIdent only
// escapes; it does not add the surrounding quotes.)
func dquoteIdent(s string) string {
	return `"` + quoteIdent(s) + `"`
}

// BindSchema receives the resolved db + table name. The engine calls it after
// Create/Connect; rtree names its shadow tables after the table, so this is
// where the shadow family is (re)materialized and the node size settled.
//
// Fresh-family detection uses the %_node TABLE, not the vtab's own schema
// entry: SQLite writes the vtab row BEFORE xCreate runs, so the entry always
// exists at bind time — what distinguishes a first xCreate (shadows still
// absent) from a per-statement re-bind over an existing family is whether
// <name>_node is already in the schema.
//
//   - fresh:  size nodes from the current page (getNodeSize's xCreate branch)
//     and create the family with plain CREATE TABLE so a name taken by an
//     ordinary table fails the CREATE ("table ... already exists",
//     rtreeSqlInit parity), then seed the root node.
//   - existing family: xConnect sizing — infer the node size from the root
//     blob so a page-size change (VACUUM) cannot desynchronize the instance
//     from the stored nodes (rtree7-1.x), use IF NOT EXISTS (C runs no DDL on
//     xConnect; frigolite re-materializes per statement) and never re-seed —
//     re-seeding would silently heal a deleted/emptied root (rtree8-2.1.5).
func (v *rtreeVTab[T]) BindSchema(dbName, tableName string) error {
	v.dbName = dbName
	v.name = tableName
	exists := v.shadowNodeTableExists()
	if exists {
		if err := v.inferNodeSizeFromRoot(); err != nil {
			return err
		}
		if v.iNodeSize <= 0 {
			// Absent root (e.g. %_node emptied by corruption tests): fall back
			// to page-size sizing so statement-level instances materialize and
			// per-node-load guards report corruption like rtree.c does.
			v.computeNodeSize()
		}
	} else {
		v.computeNodeSize()
	}
	return createShadowDDL(v, v.created && !exists)
}

// shadowNodeTableExists reports whether the <name>_node shadow table is
// already in the schema (fresh-family discriminator; see BindSchema).
func (v *rtreeVTab[T]) shadowNodeTableExists() bool {
	rows, err := v.module.db.ExecSQL(
		fmt.Sprintf("SELECT name FROM sqlite_master WHERE name='%s'",
			strings.ReplaceAll(v.name+"_node", "'", "''")))
	return err == nil && len(rows) > 0
}

// inferNodeSizeFromRoot mirrors getNodeSize's connect branch (rtree.c
// 3586-3592): a root blob of at least 448 bytes (smallest page-derived node,
// 512-64) becomes the instance's node size. A fully EMPTIED root row (length
// 0) is the verbatim connect-time corruption `undersize RTree blobs in
// "<name>_node"` (rtreeA-7.110). A short-but-non-empty root stays tolerant —
// frigolite re-binds per statement, so the rtreedoc-2.4 corruption flow
// (short blob then SELECT on the same handle) must surface at cursor time as
// the generic malformed image error (nodeAcquire's blob-size mismatch),
// matching SQLite's same-connection behavior. A MISSING root row is equally
// tolerant (rtree8-2.x parity: statement-level instances materialize and the
// per-node-load guards report the corruption).
func (v *rtreeVTab[T]) inferNodeSizeFromRoot() error {
	sql := fmt.Sprintf("SELECT length(data) FROM \"%s\" WHERE nodeno=1",
		strings.ReplaceAll(v.name+"_node", `"`, `""`))
	rows, err := v.module.db.ExecSQL(sql)
	if err != nil {
		return nil // schema-level trouble surfaces on the real statement
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		// A ROW exists: rtree.c reads its byte length via
		// sqlite3_blob_bytes regardless of content. A fully emptied root
		// (length 0) is the verbatim connect-time corruption from getNodeSize
		// (rtreeA-7.110: UPDATE t1_node SET data=x''). A SHORT but non-empty
		// blob stays tolerant here: frigolite re-binds a fresh instance for
		// every statement (SQLite runs getNodeSize once per connection), and
		// the rtreedoc-2.4 flow ('UPDATE %_node SET data=...' then SELECT on
		// the same handle) requires the cursor-time generic malformed from
		// nodeAcquire's blob-size mismatch, not the connect-time message.
		size := int(rtreeAsInt64(rows[0][0]))
		if size == 0 {
			return errCapitalized{fmt.Sprintf("undersize RTree blobs in %q", v.name+"_node")}
		}
		if size >= 512-64 {
			v.iNodeSize = size
		}
	}
	return nil
}

// computeNodeSize mirrors rtree.c getNodeSize: page_size-64, capped at
// 4 + nBytesPerCell*RTREE_MAXCELLS so every node fits on one page.
func (v *rtreeVTab[T]) computeNodeSize() {
	ps := 4096
	if rows, err := v.module.db.ExecSQL("PRAGMA page_size"); err == nil && len(rows) > 0 && len(rows[0]) > 0 {
		if ps2 := rtreeAsInt64(rows[0][0]); ps2 > 0 {
			ps = int(ps2)
		}
	}
	size := ps - 64
	maxByCells := 4 + v.nBytesPerCell*RTREE_MAXCELLS
	if maxByCells < size {
		size = maxByCells
	}
	v.iNodeSize = size
}

// createShadowDDL creates the three backing tables in rtree.c rtreeSqlInit's
// order — <name>_rowid, <name>_node, <name>_parent (sqlite_master rowid parity
// with the CLI: rt=1, rt_rowid=2, rt_node=3, rt_parent=4) — and, on the fresh
// xCreate side only, seeds the root node (nodeno 1) as a zeroblob so every
// operation has a stable root to acquire. fresh also selects plain CREATE
// TABLE: a pre-existing table of the same name — including a hostile ordinary
// table — fails the CREATE exactly like rtreeSqlInit's non-IF-NOT-EXISTS
// statements. A re-bind uses IF NOT EXISTS (C runs no DDL on xConnect) and
// never re-seeds, so shadow-family corruption is not silently healed
// (rtree8-2.1.5).
func createShadowDDL[T coordType](v *rtreeVTab[T], fresh bool) error {
	q := func(s string) string { return strings.ReplaceAll(s, `"`, `""`) }
	create := "CREATE TABLE"
	if !fresh {
		create = "CREATE TABLE IF NOT EXISTS"
	}
	ddl := fmt.Sprintf(
		create+` "%[1]s_rowid"(rowid INTEGER PRIMARY KEY,nodeno%[2]s);`+
			create+` "%[1]s_node"(nodeno INTEGER PRIMARY KEY,data);`+
			create+` "%[1]s_parent"(nodeno INTEGER PRIMARY KEY,parentnode);`,
		q(v.name), v.auxColumnsSQL())
	if fresh {
		ddl += fmt.Sprintf(`INSERT OR IGNORE INTO "%s_node"(nodeno, data) VALUES(1, zeroblob(%d));`,
			q(v.name), v.iNodeSize)
	}
	_, err := v.module.db.ExecSQL(ddl)
	return err
}

// auxColumnsSQL renders the %_rowid auxiliary column list: one aN column per
// '+' argument (rtree.c rtreeSqlInit).
func (v *rtreeVTab[T]) auxColumnsSQL() string {
	if v.nAux == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < v.nAux; i++ {
		fmt.Fprintf(&b, ",a%d", i)
	}
	return b.String()
}
