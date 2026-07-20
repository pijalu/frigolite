package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

func BenchmarkInsert(b *testing.B) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE bench (id INTEGER, val INTEGER, name TEXT)")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sql := fmt.Sprintf("INSERT INTO bench VALUES (%d, %d, 'name_%d')", i, i*2, i)
		res := db.Exec(sql)
		if res.Error != nil {
			b.Fatal(res.Error)
		}
	}
}

func BenchmarkSelect(b *testing.B) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE bench (id INTEGER, val INTEGER, name TEXT)")
	for i := 0; i < 1000; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO bench VALUES (%d, %d, 'name_%d')", i, i*2, i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := db.Query("SELECT * FROM bench")
		if res.Error != nil {
			b.Fatal(res.Error)
		}
	}
}

func BenchmarkSelectWhere(b *testing.B) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE bench (id INTEGER, val INTEGER, name TEXT)")
	for i := 0; i < 1000; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO bench VALUES (%d, %d, 'name_%d')", i, i*2, i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := db.Query("SELECT * FROM bench WHERE val > 500")
		if res.Error != nil {
			b.Fatal(res.Error)
		}
	}
}
