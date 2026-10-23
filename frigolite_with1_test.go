// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

func TestWith1RecursiveTreeOrder(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE tree(id INTEGER PRIMARY KEY, parentid, payload)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, path := range []string{"/a/b", "/a/b/c", "/a/d", "/a/d/e", "/a/d/f", "/g/h"} {
		parent := "NULL"
		for _, segment := range splitTreePath(path) {
			q := db.Query(fmt.Sprintf("SELECT id FROM tree WHERE parentid IS %s AND payload = '%s'", parent, segment))
			if q.Error != nil {
				t.Fatal(q.Error)
			}
			if len(q.Rows) > 0 {
				parent = fmt.Sprint(q.Rows[0][0])
				continue
			}
			r := db.Exec(fmt.Sprintf("INSERT INTO tree(parentid,payload) VALUES(%s,'%s')", parent, segment))
			if r.Error != nil {
				t.Fatal(r.Error)
			}
			q = db.Query("SELECT last_insert_rowid()")
			if q.Error != nil {
				t.Fatal(q.Error)
			}
			parent = fmt.Sprint(q.Rows[0][0])
		}
	}
	r := db.Query(`WITH flat(fid, depth, p) AS (
 SELECT id, 1, '/' || payload FROM tree WHERE parentid IS NULL
 UNION ALL
 SELECT id, depth+1, p||'/'||payload FROM flat, tree WHERE parentid=fid
 ORDER BY 2 ASC, 3 ASC
) SELECT p FROM flat`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	want := []string{"/a", "/g", "/a/b", "/a/d", "/g/h", "/a/b/c", "/a/d/e", "/a/d/f"}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows = %#v, want %v", r.Rows, want)
	}
	for i, row := range r.Rows {
		if row[0] != want[i] {
			t.Fatalf("row %d = %v, want %q (all rows %#v)", i, row[0], want[i], r.Rows)
		}
	}
}

func TestWith1MandelbrotGroupedOutput(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`WITH RECURSIVE
		xaxis(x) AS (VALUES(-2.0) UNION ALL SELECT x+0.05 FROM xaxis WHERE x<1.2),
		yaxis(y) AS (VALUES(-1.0) UNION ALL SELECT y+0.1 FROM yaxis WHERE y<1.0),
		m(iter,cx,cy,x,y) AS (
			SELECT 0,x,y,0.0,0.0 FROM xaxis,yaxis
			UNION ALL
			SELECT iter+1,cx,cy,x*x-y*y+cx,2.0*x*y+cy FROM m
			WHERE (x*x+y*y)<4.0 AND iter<28
		),
		m2(iter,cx,cy) AS (SELECT max(iter),cx,cy FROM m GROUP BY cx,cy),
		a(t) AS (SELECT group_concat(substr(' .+*#',1+min(iter/7,4),1),'') FROM m2 GROUP BY cy)
	SELECT group_concat(rtrim(t),x'0a') FROM a`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("result = %#v, want one row", r.Rows)
	}
	got, ok := r.Rows[0][0].(string)
	if !ok {
		t.Fatalf("result type = %T, want string", r.Rows[0][0])
	}
	if !strings.HasPrefix(got, "                                    ....#") {
		t.Fatalf("result starts %q, want SQLite's 36-space-indented first row", got[:min(len(got), 50)])
	}
}

func TestWith1NestedNotMaterializedFromLimit(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`WITH RECURSIVE c AS NOT MATERIALIZED (
		WITH RECURSIVE c AS NOT MATERIALIZED (
			WITH RECURSIVE c AS NOT MATERIALIZED (
				WITH RECURSIVE c AS NOT MATERIALIZED (
					WITH c AS (VALUES(0)) SELECT 1 FROM c LEFT JOIN c ON ltrim(1)
				)
				SELECT 1 FROM c,c,c,c,c,c,c,c,c
			)
			SELECT 2 FROM c,c,c,c,c,c,c,c,c
		)
		SELECT 3 FROM c,c,c,c,c,c,c,c,c
	) SELECT 4 FROM c,c,c,c,c,c,c,c,c`)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "too many FROM clause terms, max: 200") {
		t.Fatalf("error = %v, want SQLite FROM-term limit", r.Error)
	}
}

func TestWith1NestedCTENaturalJoinExists(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`SELECT printf('', EXISTS (
		WITH RECURSIVE Table0 AS (
			WITH Table0 AS (SELECT DISTINCT 1)
			SELECT *, * FROM Table0 ORDER BY 1 DESC
		)
		SELECT * FROM Table0 NATURAL JOIN Table0
	))`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != "" {
		t.Fatalf("result = %#v, want one empty-text value", r.Rows)
	}
}

func TestWith1NestedCTECompoundColumnName(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`WITH x AS (
		WITH y(b) AS (SELECT 10)
		SELECT * FROM y UNION ALL SELECT * FROM y
	) SELECT * FROM x`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "b" {
		t.Fatalf("columns = %v, want [b]", r.Columns)
	}
}

func TestWith1RecursiveTreeOrderCollation(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE tree(id INTEGER PRIMARY KEY, parentid, payload)",
		"INSERT INTO tree VALUES(1,NULL,'a'),(2,NULL,'B'),(3,NULL,'c'),(4,1,'b'),(5,1,'C'),(6,1,'d'),(7,2,'e'),(8,2,'F'),(9,2,'g'),(10,3,'h'),(11,3,'I'),(12,3,'j')",
	} {
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	r := db.Query(`WITH flat(fid, depth, p) AS (
		SELECT id, 1, ('/' || payload) COLLATE nocase FROM tree WHERE parentid IS NULL
		UNION ALL
		SELECT id, depth+1, (p||'/'||payload) FROM flat, tree WHERE parentid=fid
		ORDER BY 2, 3
	) SELECT p FROM flat`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	want := []string{"/a", "/B", "/c", "/a/b", "/a/C", "/a/d", "/B/e", "/B/F", "/B/g", "/c/h", "/c/I", "/c/j"}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows = %#v, want %v", r.Rows, want)
	}
	for i, row := range r.Rows {
		if row[0] != want[i] {
			t.Fatalf("row %d = %v, want %q; all rows %#v", i, row[0], want[i], r.Rows)
		}
	}
}

func splitTreePath(path string) []string {
	var out []string
	segment := ""
	for _, r := range path {
		if r == '/' {
			if segment != "" {
				out = append(out, segment)
				segment = ""
			}
		} else {
			segment += string(r)
		}
	}
	if segment != "" {
		out = append(out, segment)
	}
	return out
}
