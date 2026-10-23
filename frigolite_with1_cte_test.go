package frigolite

import (
	"reflect"
	"testing"
)

func TestWith1RecursiveTreeOrderingNative(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	setup := `CREATE TABLE tree(id INTEGER PRIMARY KEY, parentid, payload);
		INSERT INTO tree VALUES(1,NULL,'a'),(2,1,'a'),(3,2,'a'),
			(4,1,'b'),(5,4,'c'),(6,5,'d'),(7,4,'d')`
	if result := db.Exec(setup); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Query(`WITH flat(fid, p) AS (
		SELECT id, '/' || payload FROM tree WHERE parentid IS NULL
		UNION ALL
		SELECT id, p || '/' || payload FROM flat, tree WHERE parentid=fid
	) SELECT p FROM flat ORDER BY p`)
	want := [][]interface{}{{"/a"}, {"/a/a"}, {"/a/a/a"}, {"/a/b"}, {"/a/b/c"}, {"/a/b/c/d"}, {"/a/b/d"}}
	if result.Error != nil || !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("recursive paths = %#v, error = %v; want %#v", result.Rows, result.Error, want)
	}
}

func TestRecursiveCTEEmptyTextAnchorRemainsText(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if result := db.Exec("CREATE TABLE tree(i, p); INSERT INTO tree VALUES(1,NULL),(2,1),(3,1),(4,2),(5,4)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Query(`WITH t(id, path) AS (
		SELECT i, '' FROM tree WHERE p IS NULL
		UNION ALL
		SELECT i, path || '/' || i FROM tree, t WHERE p = id
	) SELECT path FROM t`)
	want := [][]interface{}{{""}, {"/2"}, {"/3"}, {"/2/4"}, {"/2/4/5"}}
	if result.Error != nil || !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("rows = %#v, error = %v; want %#v", result.Rows, result.Error, want)
	}
}
