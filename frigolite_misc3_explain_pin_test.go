// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMisc3ExplainConstantAndDefaultRows pins misc3-6.11's engine-visible
// contract: the EXPLAIN row stream must carry the large integer and real
// literal loads, the column defaults in OP_Column p4, and DESC sorter
// keyinfo. The fixture observes those strings through Tcl regexp probes.
func TestMisc3ExplainConstantAndDefaultRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.Exec(`
		CREATE TABLE ex1(
			a INTEGER DEFAULT 54321,
			b TEXT DEFAULT "hello",
			c REAL DEFAULT 3.1415926
		);
		CREATE UNIQUE INDEX ex1i1 ON ex1(a);
	`)
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}
	res := db.Query("EXPLAIN SELECT a+123456789012, b*4.5678, c FROM ex1 ORDER BY +a, b DESC")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	var rows []string
	for _, row := range res.Rows {
		rows = append(rows, strings.Join([]string{
			stringValue(row[1]), stringValue(row[2]), stringValue(row[3]), stringValue(row[4]), stringValue(row[5]),
		}, "|"))
	}
	t.Logf("EXPLAIN rows: %s", strings.Join(rows, "\n"))
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"123456789012", "4.5678", "54321", "hello", "3.1415926", "k(2,B,-B)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("EXPLAIN missing %q", want)
		}
	}
}

// TestExplainGeneratedColumnIndexSupply pins gencol1-23.3/23.4: a generated
// column carried by the chosen index emits no OP_Column, while one the
// index does not carry still does.
func TestExplainGeneratedColumnIndexSupply(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.Exec(`
		CREATE TABLE t1(x, a INT AS (x) VIRTUAL, b BLOB AS (x) VIRTUAL);
		CREATE INDEX x2 ON t1(a);
	`)
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}
	res := db.Query("EXPLAIN SELECT a, b FROM t1 INDEXED BY x2")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	var out strings.Builder
	for _, row := range res.Rows {
		out.WriteString(stringValue(row[1]))
		out.WriteByte(' ')
		out.WriteString(stringValue(row[7]))
		out.WriteByte('\n')
	}
	got := out.String()
	if strings.Contains(got, "column a") {
		t.Errorf("index-supplied generated column emitted OP_Column:\n%s", got)
	}
	if !strings.Contains(got, "column b") {
		t.Errorf("generated column not in index missing OP_Column:\n%s", got)
	}
}
