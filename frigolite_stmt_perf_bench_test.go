package frigolite_test

// Prepared-statement bind benchmarks: the bound loop (Prepare once, N x
// Exec with fresh arguments) against the literal-text loop (a distinct SQL
// string per call, the template cache path) — the per-statement parse/
// normalize floor the bind API removes.

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

func benchBindOpen(b *testing.B, schema string) *frigolite.DB {
	b.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	if r := db.Exec(schema); r.Error != nil {
		b.Fatal(r.Error)
	}
	return db
}

// benchInsertSchema is the plain (non-IPK) insert-cost table: values grow
// with b.N rescales, so the key must stay unique-free.
const benchInsertSchema = "CREATE TABLE ins(a INTEGER, b INTEGER, c TEXT)"

// BenchmarkStmtInsertLiteral measures one INSERT per call with a distinct
// literal text (values vary → exact-statement cache misses, template-cache
// clones).
func BenchmarkStmtInsertLiteral(b *testing.B) {
	db := benchBindOpen(b, benchInsertSchema)
	db.Exec("INSERT INTO ins VALUES(1, 2, 'x')")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO ins VALUES(%d, %d, 'x%d')", i, i*2, i)); r.Error != nil {
			b.Fatal(r.Error)
		}
	}
}

// BenchmarkStmtInsertBound measures the same INSERTs through a prepared
// statement with positional arguments (parse once, bind per call).
func BenchmarkStmtInsertBound(b *testing.B) {
	db := benchBindOpen(b, benchInsertSchema)
	st, err := db.Prepare("INSERT INTO ins VALUES(?, ?, ?)")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	if r := st.Exec(int64(1), int64(2), "x"); r.Error != nil {
		b.Fatal(r.Error)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := st.Exec(int64(i), int64(i*2), fmt.Sprintf("x%d", i)); r.Error != nil {
			b.Fatal(r.Error)
		}
	}
}

func benchBindLoad(b *testing.B, db *frigolite.DB, n int) {
	b.Helper()
	st, err := db.Prepare("INSERT INTO t VALUES(?, ?, ?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if r := st.Exec(int64(i+1), int64(i*2), "x"); r.Error != nil {
			b.Fatal(r.Error)
		}
	}
	st.Close()
	if r := db.Exec("CREATE INDEX ti ON t(b)"); r.Error != nil {
		b.Fatal(r.Error)
	}
}

// BenchmarkStmtSelectLiteral measures an INTEGER PRIMARY KEY point SELECT
// with a distinct literal text per call.
func BenchmarkStmtSelectLiteral(b *testing.B) {
	db := benchBindOpen(b, "CREATE TABLE t(id INTEGER PRIMARY KEY, b INTEGER, c TEXT)")
	const n = 20000
	benchBindLoad(b, db, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := db.Query(fmt.Sprintf("SELECT b FROM t WHERE id = %d", i%n+1))
		if r.Error != nil || len(r.Rows) != 1 {
			b.Fatal(r.Error)
		}
	}
}

// BenchmarkStmtSelectBound measures the same point SELECT through a prepared
// statement with a bound argument.
func BenchmarkStmtSelectBound(b *testing.B) {
	db := benchBindOpen(b, "CREATE TABLE t(id INTEGER PRIMARY KEY, b INTEGER, c TEXT)")
	const n = 20000
	benchBindLoad(b, db, n)
	st, err := db.Prepare("SELECT b FROM t WHERE id = ?")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := st.Query(int64(i%n + 1))
		if r.Error != nil || len(r.Rows) != 1 {
			b.Fatal(r.Error)
		}
	}
}
