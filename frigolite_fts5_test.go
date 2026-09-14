package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// This file validates the fts5 module (P6.FTS5 slices 1-3): module lifecycle
// and shadow tables, configuration option parsing with SQLite's error texts,
// the four built-in tokenizers, index maintenance (INSERT/UPDATE/DELETE incl.
// contentless variants) and MATCH evaluation (single-term, quoted phrase,
// column filter, prefix, NEAR). Expected values/error texts were validated
// against the sqlite3 fts5 oracle.

// --- Slice 1: module + storage ---

func TestFTS5CreateShadowTables(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a, b)"))

	// The shadow family exists with C's schemas and creation order
	// (fts5IndexOpen + fts5StorageOpen).
	checkQueryResult(t, db.Query("SELECT name FROM sqlite_master WHERE name LIKE 't1%' ORDER BY rowid"),
		"t1 t1_data t1_idx t1_content t1_docsize t1_config")
	checkQueryResult(t, db.Query("SELECT sql FROM sqlite_master WHERE name='t1_data'"),
		"CREATE TABLE 't1_data'(id INTEGER PRIMARY KEY, block BLOB)")
	checkQueryResult(t, db.Query("SELECT sql FROM sqlite_master WHERE name='t1_idx'"),
		"CREATE TABLE 't1_idx'(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID")
	checkQueryResult(t, db.Query("SELECT sql FROM sqlite_master WHERE name='t1_content'"),
		"CREATE TABLE 't1_content'(id INTEGER PRIMARY KEY, c0, c1)")
	checkQueryResult(t, db.Query("SELECT sql FROM sqlite_master WHERE name='t1_docsize'"),
		"CREATE TABLE 't1_docsize'(id INTEGER PRIMARY KEY, sz BLOB)")
	checkQueryResult(t, db.Query("SELECT sql FROM sqlite_master WHERE name='t1_config'"),
		"CREATE TABLE 't1_config'(k PRIMARY KEY, v) WITHOUT ROWID")

	// %_config carries the version row; %_data the two seed blocks.
	checkQueryResult(t, db.Query("SELECT k, v FROM t1_config"), "version 4")
	checkQueryResult(t, db.Query("SELECT id FROM t1_data ORDER BY id"), "1 10")

	// Hidden columns exist but are not projected by * or table_info.
	checkQueryResult(t, db.Query("SELECT count(*) FROM pragma_table_info('t1')"), "2")
	checkExecError(t, db.Exec("SELECT docid FROM t1"), "no such column: docid")
}

func TestFTS5ConfigErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// Error texts mirror fts5_config.c / fts5_tokenize.c.
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e1 USING fts5(a, bogus=1)"),
		`unrecognized option: "bogus"`)
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e2 USING fts5(a, tokenize=\"bogus\")"),
		"no such tokenizer: bogus")
	checkExecError(t, db.Exec(`CREATE VIRTUAL TABLE e3 USING fts5(a, tokenize="simple")`),
		"no such tokenizer: simple")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e4 USING fts5(rank)"),
		"reserved fts5 column name: rank")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e5 USING fts5(rowid)"),
		"reserved fts5 column name: rowid")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e6 USING fts5(a rowid)"),
		"unrecognized column option: rowid")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e7 USING fts5()"),
		"vtable constructor failed: e7")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e8 USING fts5(a, contentless_delete=1)"),
		"contentless_delete=1 requires a contentless table")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e9 USING fts5(a, content='', contentless_delete=2)"),
		"malformed contentless_delete=... directive")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e10 USING fts5(a, detail=bogus)"),
		"malformed detail=... directive")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e11 USING fts5(a, prefix='x')"),
		"malformed prefix=... directive")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE e12 USING fts5(a, content='', contentless_delete=1, columnsize=0)"),
		"contentless_delete=1 is incompatible with columnsize=0")
	checkExecError(t, db.Exec(`CREATE VIRTUAL TABLE e13 USING fts5(a, tokenize="unicode61 bogus 1")`),
		"error in tokenizer constructor")
	checkExecError(t, db.Exec(`CREATE VIRTUAL TABLE e14 USING fts5(a, tokenize="unicode61 categories 'X*'")`),
		"error in tokenizer constructor")

	// Valid option forms create cleanly.
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v1 USING fts5(a, b, tokenize="unicode61 categories 'L* N* Co'")`))
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v2 USING fts5(a, b, prefix='2 3')`))
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v3 USING fts5(a, b, prefix=2)`))
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v4 USING fts5(a UNINDEXED, b)`))
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v5 USING fts5(a, content='', detail=none, contentless_delete=1)`))
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v6 USING fts5(a, content='', columnsize=0)`))
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE v7 USING fts5(a, content='t1')`))
}

func TestFTS5InsertSelect(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a, b)"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a, b) VALUES(1, 'one two', 'three one')"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a, b) VALUES(2, 'four', 'five one')"))

	checkQueryResult(t, db.Query("SELECT rowid, a, b FROM t1"), "1 one two three one 2 four five one")
	checkQueryResult(t, db.Query("SELECT rowid, a, b FROM t1 ORDER BY rowid DESC"), "2 four five one 1 one two three one")
	checkQueryResult(t, db.Query("SELECT count(*) FROM t1"), "2")

	// Auto-assigned rowid = max+1.
	checkExecOK(t, db.Exec("INSERT INTO t1(a) VALUES('six')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 ORDER BY rowid"), "1 2 3")

	// %_docsize stores per-column token counts (varint blob).
	checkQueryResult(t, db.Query("SELECT id, hex(sz) FROM t1_docsize ORDER BY id"), "1 0202 2 0102 3 0100")

	// %_content stores the original values.
	checkQueryResult(t, db.Query("SELECT id, c0, c1 FROM t1_content ORDER BY id"), "1 one two three one 2 four five one 3 six NULL")

	// Duplicate explicit rowid fails like SQLite ("constraint failed").
	checkExecError(t, db.Exec("INSERT INTO t1(rowid, a) VALUES(1, 'x')"), "constraint failed")

	// The rank column is insertable and ignored.
	checkExecOK(t, db.Exec("INSERT INTO t1(a, rank) VALUES('seven', 5)"))
	checkQueryResult(t, db.Query("SELECT a FROM t1 WHERE rowid=4"), "seven")

	// rank reads as NULL without a MATCH; with one it is the bm25 value
	// (negated BM25, oracle-verified: -1.1282051282051283e-06 at full
	// precision, printed at SQLite's 15 significant digits).
	checkQueryResult(t, db.Query("SELECT rank FROM t1 WHERE rowid=1"), "NULL")
	checkQueryResult(t, db.Query("SELECT rowid, rank FROM t1 WHERE t1 MATCH 'one'"),
		"1 -1.12820512820513e-06 2 -8.8e-07")
}

// --- Slice 3: MATCH queries ---

func TestFTS5MatchBasics(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a, b)"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a, b) VALUES(1, 'one two', 'three one')"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a, b) VALUES(2, 'four', 'five one')"))

	// Single terms (implicit AND across the table).
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'three'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two three'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'nothing'"), "")

	// Whole-table MATCH via the hidden table-name column and bare LHS.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'four'"), "2")
	checkQueryResult(t, db.Query("SELECT * FROM t1 WHERE t1 MATCH 'four'"), "four five one")

	// Column filters.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE a MATCH 'one'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE b MATCH 'one'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1.a MATCH 'one'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'a:one'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'b:one'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '{a b} : four'"), "2")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'c:one'"), "no such column: c")

	// Quoted phrases require adjacency.
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"one two"'`), "1")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"two one"'`), "")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"three one"'`), "1")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH 'a : "one two"'`), "1")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH 'b : "one two"'`), "")

	// Prefix terms.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'on*'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'f*'"), "2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'a:tw*'"), "1")

	// Boolean operators.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one AND two'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one OR four'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one NOT two'"), "2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '(one two) OR four'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '^one'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '^three'"), "1")

	// NEAR (same-column windows; default N=10).
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t2 USING fts5(x)"))
	checkExecOK(t, db.Exec("INSERT INTO t2(rowid, x) VALUES(1, 'a b c d e f g h i')"))
	checkExecOK(t, db.Exec("INSERT INTO t2(rowid, x) VALUES(2, 'a z b z c z z z z z i')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t2 WHERE t2 MATCH 'NEAR(a c i)'"), "1 2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t2 WHERE t2 MATCH 'NEAR(a c, 2)'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t2 WHERE t2 MATCH 'NEAR(a c, 3)'"), "1 2")

	// Syntax errors fail the statement with C's text.
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH ''"), `fts5: syntax error near ""`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '(one'"), `fts5: syntax error near ""`)
}

// --- Slice 2: tokenizers ---

func TestFTS5Tokenizers(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// unicode61 (default): folds case, removes diacritics.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE u1 USING fts5(a)"))
	checkExecOK(t, db.Exec("INSERT INTO u1(rowid, a) VALUES(1, 'Café RUNNING')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM u1 WHERE u1 MATCH 'cafe'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM u1 WHERE u1 MATCH 'running'"), "1")
	// remove_diacritics 0 keeps the diacritic.
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE u2 USING fts5(a, tokenize="unicode61 remove_diacritics 0")`))
	checkExecOK(t, db.Exec("INSERT INTO u2(rowid, a) VALUES(1, 'Café')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM u2 WHERE u2 MATCH 'café'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM u2 WHERE u2 MATCH 'cafe'"), "")

	// tokenchars/separators exceptions.
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE u3 USING fts5(a, tokenize="unicode61 tokenchars '-'")`))
	checkExecOK(t, db.Exec("INSERT INTO u3(rowid, a) VALUES(1, 'full-text 42nd')"))
	// The query lexer is tokenizer-independent: an unquoted '-' is the
	// inverted-colset token, so 'full-text' resolves 'text' as a column
	// (oracle-verified); the quoted form tokenizes with the table tokenizer.
	checkExecError(t, db.Exec("SELECT rowid FROM u3 WHERE u3 MATCH 'full-text'"), "no such column: text")
	checkQueryResult(t, db.Query(`SELECT rowid FROM u3 WHERE u3 MATCH '"full-text"'`), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM u3 WHERE u3 MATCH 'full'"), "")
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE u4 USING fts5(a, tokenize="unicode61 separators 'x'")`))
	checkExecOK(t, db.Exec("INSERT INTO u4(rowid, a) VALUES(1, 'axb')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM u4 WHERE u4 MATCH 'ab'"), "")

	// ascii: underscore is a token character, only ASCII lowercases.
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE a1 USING fts5(a, tokenize="ascii")`))
	checkExecOK(t, db.Exec("INSERT INTO a1(rowid, a) VALUES(1, 'foo_bar BAZ')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM a1 WHERE a1 MATCH 'foo_bar'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM a1 WHERE a1 MATCH 'baz'"), "1")

	// porter: stems each token (base unicode61).
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE p1 USING fts5(a, tokenize="porter")`))
	checkExecOK(t, db.Exec("INSERT INTO p1(rowid, a) VALUES(1, 'running jumps')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM p1 WHERE p1 MATCH 'run'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM p1 WHERE p1 MATCH 'jump'"), "1")
	// Query terms go through the tokenizer: 'running' stems to 'run'.
	checkQueryResult(t, db.Query("SELECT rowid FROM p1 WHERE p1 MATCH 'running'"), "1")

	// porter with an explicit base.
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE p2 USING fts5(a, tokenize="porter ascii")`))
	checkExecOK(t, db.Exec("INSERT INTO p2(rowid, a) VALUES(1, 'running')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM p2 WHERE p2 MATCH 'run'"), "1")

	// trigram: every 3-character substring is indexed.
	checkExecOK(t, db.Exec(`CREATE VIRTUAL TABLE g1 USING fts5(a, tokenize="trigram")`))
	checkExecOK(t, db.Exec("INSERT INTO g1(rowid, a) VALUES(1, 'the quick brown fox')"))
	checkQueryResult(t, db.Query(`SELECT rowid FROM g1 WHERE g1 MATCH '"quick brown"'`), "1")
	checkQueryResult(t, db.Query(`SELECT rowid FROM g1 WHERE g1 MATCH '"ick bro"'`), "1")
	checkQueryResult(t, db.Query(`SELECT rowid FROM g1 WHERE g1 MATCH '"QUICK"'`), "1")
}

// --- content modes ---

func TestFTS5Contentless(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// content='': no %_content shadow; text columns read NULL; DELETE/UPDATE
	// with a WHERE are rejected.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE c1 USING fts5(a, content='')"))
	checkQueryResult(t, db.Query("SELECT name FROM sqlite_master WHERE name LIKE 'c1%' ORDER BY name"),
		"c1 c1_config c1_data c1_docsize c1_idx")
	checkExecOK(t, db.Exec("INSERT INTO c1(rowid, a) VALUES(1, 'hello world')"))
	checkQueryResult(t, db.Query("SELECT rowid, a FROM c1"), "1 NULL")
	checkQueryResult(t, db.Query("SELECT rowid FROM c1 WHERE c1 MATCH 'hello'"), "1")
	checkExecError(t, db.Exec("DELETE FROM c1 WHERE rowid=1"),
		"cannot DELETE from contentless fts5 table: c1")
	checkExecError(t, db.Exec("UPDATE c1 SET a='x' WHERE rowid=1"),
		"cannot UPDATE contentless fts5 table: c1")
	// A WHERE-less DELETE resets the table.
	checkExecOK(t, db.Exec("DELETE FROM c1"))
	checkQueryResult(t, db.Query("SELECT rowid FROM c1 WHERE c1 MATCH 'hello'"), "")
	checkQueryResult(t, db.Query("SELECT rowid FROM c1"), "")

	// contentless_delete=1: DELETE WHERE works and reindexes.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE c2 USING fts5(a, content='', contentless_delete=1)"))
	checkExecOK(t, db.Exec("INSERT INTO c2(rowid, a) VALUES(1, 'hello world')"))
	checkExecOK(t, db.Exec("DELETE FROM c2 WHERE rowid=1"))
	checkQueryResult(t, db.Query("SELECT rowid FROM c2 WHERE c2 MATCH 'hello'"), "")

	// contentless_delete=1 rejects partial-column UPDATEs.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE c3 USING fts5(a, b, content='', contentless_delete=1)"))
	checkExecOK(t, db.Exec("INSERT INTO c3(rowid, a, b) VALUES(1, 'x y', 'z')"))
	checkExecError(t, db.Exec("UPDATE c3 SET a='q' WHERE rowid=1"),
		"cannot UPDATE a subset of columns on fts5 contentless-delete table: c3")
	checkExecOK(t, db.Exec("UPDATE c3 SET a='q', b='r' WHERE rowid=1"))
	checkQueryResult(t, db.Query("SELECT rowid FROM c3 WHERE c3 MATCH 'q'"), "1")

	// Special insert directives.
	checkExecOK(t, db.Exec("INSERT INTO c1(c1, rank) VALUES('delete-all', NULL)"))
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE c4 USING fts5(a, content='')"))
	checkExecOK(t, db.Exec("INSERT INTO c4(rowid, a) VALUES(1, 'keep')"))
	checkExecOK(t, db.Exec("INSERT INTO c4(c4) VALUES('delete-all')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM c4 WHERE c4 MATCH 'keep'"), "")
}

func TestFTS5ExternalContent(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE TABLE src(id INTEGER PRIMARY KEY, body TEXT)"))
	checkExecOK(t, db.Exec("INSERT INTO src VALUES(5, 'alpha beta')"))
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE c3 USING fts5(body, content='src', content_rowid='id')"))

	// Values read from the content table.
	checkQueryResult(t, db.Query("SELECT rowid, body FROM c3"), "5 alpha beta")
	// The index is empty until maintained.
	checkQueryResult(t, db.Query("SELECT rowid FROM c3 WHERE c3 MATCH 'alpha'"), "")
	checkExecOK(t, db.Exec("INSERT INTO c3(rowid, body) VALUES(5, 'alpha beta')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM c3 WHERE c3 MATCH 'alpha'"), "5")

	// Index-only rows are MATCHable.
	checkExecOK(t, db.Exec("INSERT INTO c3(rowid, body) VALUES(6, 'gamma')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM c3 WHERE c3 MATCH 'gamma'"), "6")
	checkQueryResult(t, db.Query("SELECT rowid FROM c3('gamma')"), "6")
	checkQueryResult(t, db.Query("SELECT rowid FROM c3(7)"), "")
}

func TestFTS5DetailModes(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// detail=none: no column or phrase queries.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE d1 USING fts5(a, b, detail=none)"))
	checkExecOK(t, db.Exec("INSERT INTO d1(rowid, a, b) VALUES(1, 'x y', 'z')"))
	// detail=none still matches single terms (positions are the only thing
	// dropped) — oracle-verified.
	checkQueryResult(t, db.Query("SELECT rowid FROM d1 WHERE d1 MATCH 'x'"), "1")
	checkExecError(t, db.Exec(`SELECT rowid FROM d1 WHERE d1 MATCH '"x y"'`),
		"fts5: phrase queries are not supported (detail!=full)")
	checkExecError(t, db.Exec("SELECT rowid FROM d1 WHERE d1 MATCH 'a:x'"),
		"fts5: column queries are not supported (detail=none)")

	// detail=columns: column filters work, phrases do not.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE d2 USING fts5(a, b, detail=columns)"))
	checkExecOK(t, db.Exec("INSERT INTO d2(rowid, a, b) VALUES(1, 'x y', 'z')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM d2 WHERE d2 MATCH 'a:x'"), "1")
	checkExecError(t, db.Exec(`SELECT rowid FROM d2 WHERE d2 MATCH '"x y"'`),
		"fts5: phrase queries are not supported (detail!=full)")
}

// --- maintenance ---

func TestFTS5UpdateDeleteReindex(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a, b)"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a, b) VALUES(1, 'one two', 'three')"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a, b) VALUES(2, 'four', 'five')"))

	// UPDATE reindexes the changed document.
	checkExecOK(t, db.Exec("UPDATE t1 SET a='nine ten' WHERE rowid=1"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one'"), "")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'nine'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid, a, b FROM t1 WHERE rowid=1"), "1 nine ten three")

	// rowid UPDATE moves the document.
	checkExecOK(t, db.Exec("UPDATE t1 SET rowid=7 WHERE rowid=1"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'nine'"), "7")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 ORDER BY rowid"), "2 7")

	// DELETE removes the index entries.
	checkExecOK(t, db.Exec("DELETE FROM t1 WHERE rowid=7"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'nine'"), "")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1"), "2")
	// %_content/%_docsize stay consistent.
	checkQueryResult(t, db.Query("SELECT count(*) FROM t1_content"), "1")
	checkQueryResult(t, db.Query("SELECT count(*) FROM t1_docsize"), "1")

	// DELETE with a MATCH predicate.
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a) VALUES(9, 'target here')"))
	checkExecOK(t, db.Exec("DELETE FROM t1 WHERE t1 MATCH 'target'"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1"), "2")

	// INSERT ... SELECT.
	checkExecOK(t, db.Exec("CREATE TABLE src(x)"))
	checkExecOK(t, db.Exec("INSERT INTO src VALUES('bulk one')"))
	checkExecOK(t, db.Exec("INSERT INTO src VALUES('bulk two')"))
	checkExecOK(t, db.Exec("INSERT INTO t1(a) SELECT x FROM src"))
	// Auto rowids continue from max(rowid)=9.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'bulk'"), "10 11")
}

func TestFTS5DropRename(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a)"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a) VALUES(1, 'hello')"))

	// DROP removes the whole shadow family.
	checkExecOK(t, db.Exec("DROP TABLE t1"))
	checkQueryResult(t, db.Query("SELECT name FROM sqlite_master WHERE name LIKE 't1%'"), "")

	// Recreating with the same name starts fresh.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a)"))
	checkExecOK(t, db.Exec("INSERT INTO t1(rowid, a) VALUES(1, 'fresh doc')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'fresh'"), "1")

	// ALTER TABLE RENAME moves the family.
	checkExecOK(t, db.Exec("ALTER TABLE t1 RENAME TO t2"))
	checkQueryResult(t, db.Query("SELECT name FROM sqlite_master WHERE name LIKE 't1_%'"), "")
	checkQueryResult(t, db.Query("SELECT rowid FROM t2 WHERE t2 MATCH 'fresh'"), "1")
	checkQueryResult(t, db.Query("SELECT name FROM sqlite_master WHERE name='t2_data'"), "t2_data")
}

func TestFTS5ColumnsizeZero(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// columnsize=0 contentless: no %_docsize and no scan support.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE x1 USING fts5(a, content='', columnsize=0)"))
	checkQueryResult(t, db.Query("SELECT name FROM sqlite_master WHERE name LIKE 'x1%' ORDER BY name"),
		"x1 x1_config x1_data x1_idx")
	checkExecOK(t, db.Exec("INSERT INTO x1(rowid, a) VALUES(1, 'hello')"))
	checkQueryResult(t, db.Query("SELECT rowid FROM x1 WHERE x1 MATCH 'hello'"), "1")
	res := db.Query("SELECT rowid, a FROM x1")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "table does not support scanning") {
		t.Errorf("expected scan failure, got %v / %v", res.Error, res.Rows)
	}
}

// checkExecError asserts one statement fails with the expected text.
func checkExecError(t *testing.T, res *Result, expected string) {
	t.Helper()
	if res.Error == nil {
		t.Errorf("expected error %q, got success", expected)
		return
	}
	if !strings.Contains(res.Error.Error(), expected) {
		t.Errorf("expected error containing %q, got %q", expected, res.Error.Error())
	}
}

// --- Slice 4: full query language (fts5_expr.c/fts5parse.y) ---

// fts5QLSetup builds the shared query-language corpus (oracle-transcribed).
func fts5QLSetup(t *testing.T) *DB {
	t.Helper()
	db := setupDB(t)
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a, b)"))
	type row struct {
		rowid int
		a, b  string
	}
	rows := []row{
		{1, "one two three", "three four"},
		{2, "two three", "five"},
		{3, "three", "one two"},
		{4, "four five six", "seven"},
	}
	for _, r := range rows {
		checkExecOK(t, db.Exec(fmt.Sprintf(
			"INSERT INTO t1(rowid, a, b) VALUES(%d, '%s', '%s')", r.rowid, r.a, r.b)))
	}
	return db
}

func TestFTS5QueryLanguage(t *testing.T) {
	db := fts5QLSetup(t)
	defer db.Close()

	// Operator precedence: OR < AND < NOT < implicit AND.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one AND two OR five'"), "1 2 3 4")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one OR two AND five'"), "1 2 3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two NOT one AND five'"), "2")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one two NOT four'"), "3")
	// NOT's right operand absorbs implicit ANDs.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one NOT four two'"), "3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two NOT one five'"), "1 2 3")
	// Left-associative NOT.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one NOT four NOT two'"), "")

	// Implicit AND joins cnearsets only; a parenthesized expression cannot
	// join a chain (oracle: 'two (four)' parses as a NEAR call named "two").
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'two (four)'"), `fts5: syntax error near "two"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'two (four OR five)'"), `fts5: syntax error near "OR"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '(one) (two)'"), `fts5: syntax error near "("`)
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '(one two) OR five'"), "1 2 3 4")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two NEAR(one three)'"), "1")

	// Keywords are case-sensitive: lowercase and/or/not are plain terms.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'one and two'"), "")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'NOT one'"), `fts5: syntax error near "NOT"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'AND one'"), `fts5: syntax error near "AND"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'OR one'"), `fts5: syntax error near "OR"`)

	// NEAR: the call word is case-sensitive; one phrase is legal (the window
	// is inert); the distance is a raw digit string.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(one three)'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(one three, 0)'"), "")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'near(a b)'"), `fts5: syntax error near "near"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'foo(a b)'"), `fts5: syntax error near "foo"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(a b, x)'"), `expected integer, got "x"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(a b, )'"), `fts5: syntax error near ")"`)
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(two, 5)'"), "1 2 3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two NEAR(one three) five'"), "")

	// Column filters: braces (space-separated, no commas), exclusion with a
	// leading minus, quoted names, and parse-time merging.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '{a b} : two'"), "1 2 3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '-{a} : two'"), "3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '-a : two'"), "3")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"b" : one'`), "3")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '{"a" b} : one'`), "1 3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '{a a} : one'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'a : one OR five'"), "1 2 4")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'a : (one OR five)'"), "1 4")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'a : one + two'"), "1")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '{a} : ^three'"), "3")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'zz : a'"), "no such column: zz")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '{zz} : a'"), "no such column: zz")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '- one'"), "no such column: one")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '{} : one'"), `fts5: syntax error near "}"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '{a , b} : one'"), `fts5: syntax error near ","`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '{a} b'"), `fts5: syntax error near "b"`)
	// Nested filters merge; an empty merge matches nothing (no error).
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'a : (b : one)'"), "")

	// Phrases: "+" continuation, per-term stars, caret anchoring.
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH 'two + "three four"'`), "")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"one two" + three'`), "1")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"one two"*'`), "1 3")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'one + + two'"), `fts5: syntax error near "+"`)
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '^one + two'"), "1 3")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '^(one two)'"), `fts5: syntax error near "("`)

	// A zero-token phrase is an EOF node: it matches nothing on its own and
	// is dropped from implicit-AND chains (sqlite3Fts5ParseImplicitAnd —
	// verified against the 3.51.0 oracle: MATCH 'one ""' == MATCH 'one').
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '""'`), "")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH 'one ""'`), "1 3")
	checkQueryResult(t, db.Query(`SELECT rowid FROM t1 WHERE t1 MATCH '"" + one'`), "1 3")
	checkExecError(t, db.Exec(`SELECT rowid FROM t1 WHERE t1 MATCH '"abc'`), "unterminated string")

	// Lexer: double quotes only; barewords are digits/letters/_/high bytes.
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'one & two'"), `fts5: syntax error near "&"`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH \"'one'\""), `fts5: syntax error near "'"`)
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'ONE'"), "1 3")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH ''"), `fts5: syntax error near ""`)
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH ''"), `fts5: syntax error near ""`)

	// NEAR windows never span columns (column-major absolute positions).
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(two three four)'"), "")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(one three, 0)'"), "")

	// Special queries (fts5SpecialMatch).
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '*bogus'"), "unknown special query: bogus")
	checkExecError(t, db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH '*'"), "unknown special query: ")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH '*reads'"), "0")
}

func TestFTS5QueryLanguageDetailErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE d1 USING fts5(x, y, detail=none)"))
	checkExecOK(t, db.Exec("INSERT INTO d1(rowid, x, y) VALUES(1, 'one two', 'three')"))
	checkExecError(t, db.Exec("SELECT rowid FROM d1 WHERE d1 MATCH 'NEAR(x y)'"),
		"fts5: NEAR queries are not supported (detail!=full)")
	checkExecError(t, db.Exec("SELECT rowid FROM d1 WHERE d1 MATCH 'x : y'"),
		"fts5: column queries are not supported (detail=none)")
	checkQueryResult(t, db.Query("SELECT rowid FROM d1 WHERE d1 MATCH 'x'"), "")

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE d2 USING fts5(x, y, detail=columns)"))
	checkExecOK(t, db.Exec("INSERT INTO d2(rowid, x, y) VALUES(1, 'one two', 'three')"))
	checkExecError(t, db.Exec(`SELECT rowid FROM d2 WHERE d2 MATCH '"one two"'`),
		"fts5: phrase queries are not supported (detail!=full)")
	checkExecError(t, db.Exec("SELECT rowid FROM d2 WHERE d2 MATCH '^one'"),
		"fts5: phrase queries are not supported (detail!=full)")
	checkExecError(t, db.Exec("SELECT rowid FROM d2 WHERE d2 MATCH 'NEAR(one two)'"),
		"fts5: NEAR queries are not supported (detail!=full)")
	checkQueryResult(t, db.Query("SELECT rowid FROM d2 WHERE d2 MATCH 'y : three'"), "1")
}

// --- Slice 5: rank/bm25 and highlight/snippet ---

func TestFTS5AuxInWhere(t *testing.T) {
	db := fts5QLSetup(t)
	defer db.Close()

	// Auxiliary functions are usable in any expression context.
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two' AND bm25(t1) < 0"), "1 2 3")
	checkQueryResult(t, db.Query("SELECT rowid FROM t1 WHERE t1 MATCH 'two' AND highlight(t1, 0, '<', '>') LIKE '%<two>%'"), "1 2")
}
