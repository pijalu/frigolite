package parse

// Grammar coverage test.
//
// Frigolite ships the full SQLite LALR grammar tables (412 rules) but
// handleRule implements only a subset. Rules without an explicit handler fall
// through to a generic passthrough of the first RHS value, which silently
// corrupts the AST for multi-symbol productions (components after RHS #1 are
// dropped) and loses information for single-symbol productions.
//
// This test parses a corpus of SQL statements the engine is expected to
// support and asserts that every grammar rule that fires during those parses
// produces a real AST node: a multi-symbol rule must never fall through to
// the generic passthrough. Single-symbol passthroughs are accepted (they are
// type-alias productions, e.g. `expr ::= term`), and empty rules returning
// nil are accepted (they model optional clauses).

import (
	"strings"
	"testing"

	sql "github.com/pijalu/frigolite/internal/sql"
)

// grammarCoverageCorpus is a broad set of SQL statements spanning the areas
// the engine supports. The statements are intentionally simple so the test
// fails on grammar-handler gaps rather than on runtime execution limits.
var grammarCoverageCorpus = []string{
	// DML core
	"SELECT * FROM t1;",
	"SELECT a, b FROM t1 WHERE a > 1 ORDER BY b DESC LIMIT 5;",
	"SELECT DISTINCT a FROM t1;",
	"SELECT a AS x, b*2 AS y FROM t1 WHERE a BETWEEN 1 AND 10;",
	"SELECT CASE WHEN a > 0 THEN 'p' ELSE 'n' END FROM t1;",
	"SELECT a FROM t1 WHERE b LIKE 'x%' AND c IS NOT NULL;",
	"SELECT a FROM t1 WHERE b IN (1,2,3);",
	"SELECT COUNT(*), SUM(a), AVG(a) FROM t1 GROUP BY b HAVING COUNT(*) > 1;",
	"SELECT a FROM t1 UNION SELECT b FROM t2;",
	"SELECT t1.a FROM t1 JOIN t2 ON t1.a = t2.b;",
	"SELECT * FROM t1 LIMIT 2 OFFSET 1;",
	"INSERT INTO t1(a,b) VALUES(1,'x'),(2,'y');",
	"INSERT INTO t1 SELECT a,b FROM t2;",
	"INSERT INTO t1(a,b) VALUES(1,'x') ON CONFLICT DO NOTHING;",
	"INSERT INTO t1(a,b) VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET b=excluded.b;",
	"INSERT OR IGNORE INTO t1(a) VALUES(1);",
	"INSERT OR REPLACE INTO t1(a) VALUES(1);",
	"REPLACE INTO t1(a) VALUES(1);",
	"INSERT INTO t1 DEFAULT VALUES;",
	"UPDATE t1 SET a = a + 1, b = 'x' WHERE a > 0;",
	"UPDATE OR IGNORE t1 SET a = 1 WHERE a = 2;",
	"DELETE FROM t1 WHERE a = 1;",
	"DELETE FROM t1;",
	// DDL core
	"CREATE TABLE t1(a, b TEXT, c INTEGER DEFAULT 0);",
	"CREATE TABLE IF NOT EXISTS t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT NOT NULL DEFAULT 'x');",
	"CREATE TABLE t1(a, b, PRIMARY KEY(a,b)) WITHOUT ROWID;",
	"CREATE UNIQUE INDEX idx1 ON t1(a, b);",
	"CREATE INDEX IF NOT EXISTS idx1 ON t1(a);",
	"DROP TABLE IF EXISTS t1;",
	"DROP INDEX idx1;",
	"CREATE VIEW v1 AS SELECT a, b FROM t1;",
	"DROP VIEW IF EXISTS v1;",
	"CREATE TEMP TABLE t1(a);",
	// Transactions
	"BEGIN;",
	"BEGIN TRANSACTION;",
	"COMMIT;",
	"ROLLBACK;",
	"SAVEPOINT sp1;",
	"RELEASE sp1;",
	"ROLLBACK TO sp1;",
	// Misc
	"PRAGMA page_size = 4096;",
	"PRAGMA journal_mode = WAL;",
	"EXPLAIN SELECT * FROM t1;",
	"VACUUM;",
	"ANALYZE;",
	"ANALYZE t1;",
	"REINDEX;",
	// CTE
	"WITH x AS (SELECT a FROM t1) SELECT * FROM x;",
	"WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<10) SELECT * FROM c;",
	// Expressions
	"SELECT abs(a), length(b), substr(b,1,2), coalesce(c,0), ifnull(a,0), round(a,2) FROM t1;",
	"SELECT a FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.b = t1.b);",
	"SELECT a FROM t1 WHERE a IN (SELECT b FROM t2);",
	"SELECT CAST(a AS TEXT) FROM t1;",
	"SELECT a FROM t1 ORDER BY a COLLATE NOCASE;",
	"SELECT * FROM t1 AS x WHERE x.a = 1;",
	// Triggers
	"CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT 1; END;",
	"CREATE TRIGGER tr2 BEFORE UPDATE OF a ON t1 WHEN new.a > 0 BEGIN DELETE FROM t1 WHERE a = old.a; END;",
	"CREATE TRIGGER tr3 INSTEAD OF DELETE ON v1 FOR EACH ROW BEGIN INSERT INTO log VALUES(1); END;",
	"DROP TRIGGER IF EXISTS tr1;",
	// Virtual tables
	"CREATE VIRTUAL TABLE ft1 USING fts4(content, title);",
	"CREATE VIRTUAL TABLE ft2 USING fts5(a, b);",
	// NOT INDEXED / INDEXED BY
	"SELECT * FROM t1 NOT INDEXED;",
	"SELECT * FROM t1 INDEXED BY idx1 WHERE a = 1;",
	"DELETE FROM t1 NOT INDEXED WHERE a = 1;",
	// Window functions
	"SELECT sum(a) OVER (PARTITION BY b ORDER BY c) FROM t1;",
	"SELECT sum(a) OVER w, avg(b) OVER w FROM t1 WINDOW w AS (ORDER BY c);",
	"SELECT count(*) FILTER (WHERE a > 0) FROM t1;",
	"SELECT sum(a) OVER (ORDER BY c ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t1;",
	"SELECT sum(a) OVER (ORDER BY c RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t1;",
	// ALTER TABLE
	"ALTER TABLE t1 RENAME TO t2;",
	"ALTER TABLE t1 ADD COLUMN d TEXT DEFAULT 'x';",
	"ALTER TABLE t1 RENAME COLUMN a TO x;",
	"ALTER TABLE t1 DROP COLUMN b;",
	// Constraints (FK, CHECK, generated columns)
	"CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT REFERENCES t2(id) ON DELETE CASCADE);",
	"CREATE TABLE t1(a INTEGER, b TEXT CHECK(length(b) > 0));",
	"CREATE TABLE t1(a INTEGER, b TEXT GENERATED ALWAYS AS (a || 'x') VIRTUAL);",
	"CREATE TABLE t1(a INTEGER, b INTEGER CONSTRAINT ck1 CHECK(b >= 0));",
	"CREATE TABLE t1(a, b, FOREIGN KEY(b) REFERENCES t2(id));",
}

// TestGrammarCoverage parses the corpus and fails on any multi-symbol grammar
// rule that fires without an explicit handler (falling through to the generic
// passthrough). Single-symbol passthroughs are type-alias productions and are
// allowed; empty rules returning nil model optional clauses and are allowed.
func TestGrammarCoverage(t *testing.T) {
	fired := map[int]int{}      // ruleNo -> statement index
	passthrough := map[int]int{} // ruleNo -> statement index (nil result, size>0)

	for i, sqlStr := range grammarCoverageCorpus {
		p := NewParser(GetParseTables())
		p.OnReduce(func(ruleNo int, parser *Parser, lookahead int, lookaheadToken interface{}) {
			t := parser.tables
			size := -t.RuleInfoNRhs[ruleNo]
			result := handleRule(ruleNo, parser, lookahead, lookaheadToken)
			if result == nil && size > 0 {
				passthrough[ruleNo] = i
			}
			fired[ruleNo] = i
			lhsSlot := parser.pos
			if size > 0 {
				lhsSlot = parser.pos - size + 1
			}
			parser.stack[lhsSlot].Minor = result
		})
		tok := sql.NewTokenizer(sqlStr)
		var res ParseResult = ParseAccept
		for {
			tk := tok.Next()
			if tk.Type == 0 {
				res = p.Parse(0, nil)
				break
			}
			res = p.Parse(tokenCode(int(tk.Type), tk.Value), tk)
			if res != ParseAccept {
				break
			}
		}
		if res != ParseAccept {
			t.Errorf("corpus statement %d failed to parse: %q", i, strings.TrimSpace(sqlStr))
		}
	}

	var multiFailures []string
	for ruleNo, stmtIdx := range passthrough {
		size := -GetParseTables().RuleInfoNRhs[ruleNo]
		if size > 1 {
			multiFailures = append(multiFailures, ruleNoToString(ruleNo, size, stmtIdx))
		}
	}
	if len(multiFailures) > 0 {
		t.Errorf("multi-symbol grammar rules fired without a real handler:\n  %s",
			strings.Join(multiFailures, "\n  "))
	}
}

// ruleNoToString renders a rule number and the corpus statement that fired it.
func ruleNoToString(ruleNo, size, stmtIdx int) string {
	return formatRule(ruleNo, size) + " fired by: " + corpusStmt(stmtIdx)
}

// formatRule renders a rule as "rule N (size M)".
func formatRule(ruleNo, size int) string {
	return "rule " + itoa(ruleNo) + " (nrhs=" + itoa(size) + ")"
}

func corpusStmt(i int) string {
	if i >= 0 && i < len(grammarCoverageCorpus) {
		return strings.TrimSpace(grammarCoverageCorpus[i])
	}
	return "?"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
