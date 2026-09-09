package fts4opt

import (
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func TestZZOptProbe(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(" CREATE TABLE t1(docid, words) "); r.Error != nil {
		t.Fatal(r.Error)
	}
	ftsKJVGenesis(t, db)
	if r := db.Exec(" CREATE VIRTUAL TABLE t2 USING fts4(words, prefix=\"1,2,3\") "); r.Error != nil {
		t.Fatal(r.Error)
	}
	rows := db.Query("SELECT docid, words FROM t1")
	if rows.Error != nil {
		t.Fatal(rows.Error)
	}
	for _, row := range rows.Rows {
		if r := db.Exec("INSERT INTO t2(docid, words) VALUES(" + sqlLiteral(row[0]) + ", " + sqlLiteral(row[1]) + ")"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := db.Exec(" INSERT INTO t2(t2) VALUES('merge=5,2') "); r.Error != nil {
		t.Fatalf("merge: %v", r.Error)
	}
}
