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
	t.Logf("levels before: %v", db.Query("SELECT level, count(*) FROM t2_segdir GROUP BY level").Rows)
	if r := db.Exec(" INSERT INTO t2(t2) VALUES('merge=5,2') "); r.Error != nil {
		t.Fatalf("merge: %v", r.Error)
	}
	got := db.Query("SELECT level, count(*) FROM t2_segdir GROUP BY level").Rows
	t.Logf("levels after: %v", got)
	want := "0 13 1 15 2 5 1024 13 1025 15 1026 5 2048 13 2049 15 2050 5 3072 13 3073 15 3074 5"
	if norm(got) != want {
		t.Errorf("MISMATCH: got %q want %q", norm(got), want)
	}
	if r := db.Exec(" INSERT INTO t2(t2) VALUES('integrity-check') "); r.Error != nil {
		t.Fatalf("integrity-check: %v", r.Error)
	}
	t.Log("integrity-check ok")
}

func norm(rows [][]interface{}) string {
	out := ""
	for _, r := range rows {
		for _, c := range r {
			out += fmtSprintCell(c) + " "
		}
	}
	return trimSp(out)
}

func fmtSprintCell(v interface{}) string { return toStr(v) }
func toStr(v interface{}) string         { return strOf(v) }
func strOf(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return intStr(v)
}

func intStr(v interface{}) string {
	if i, ok := v.(int64); ok {
		d := 0
		x := i
		neg := x < 0
		if neg {
			x = -x
		}
		if x == 0 {
			return "0"
		}
		digits := []byte{}
		for x > 0 {
			digits = append(digits, byte('0'+x%10))
			x /= 10
		}
		if neg {
			digits = append(digits, '-')
		}
		for j := len(digits) - 1; j >= 0; j-- {
			out := append([]byte{}, digits[:j+1]...)
			d = 0
			_ = d
			_ = out
			break
		}
		rev := []byte{}
		for j := len(digits) - 1; j >= 0; j-- {
			rev = append(rev, digits[j])
		}
		return string(rev)
	}
	return "?"
}

func trimSp(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
