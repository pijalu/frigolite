package frigolite

import (
	"reflect"
	"testing"
)

func TestAtof1UTF16BlobSubstrAndEmptyBlobArithmetic(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	result := db.Query("PRAGMA encoding='UTF16be'; CREATE TABLE t(a,b); INSERT INTO t(rowid,a) VALUES(1,x'00'),(2,3); SELECT typeof(a), substr(a,',') is true FROM t ORDER BY rowid")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if got, want := result.Rows, [][]interface{}{{"blob", int64(0)}, {"integer", int64(1)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("substr truth rows = %#v, want %#v", got, want)
	}
	result = db.Query("SELECT 3 IS TRUE, 3 IS FALSE, 3 IS NOT TRUE, 3 IS NOT FALSE, NULL IS TRUE, NULL IS NOT TRUE, 3 IS 2")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if got, want := result.Rows, [][]interface{}{{int64(1), int64(0), int64(0), int64(1), int64(0), int64(1), int64(0)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IS truth literal rows = %#v, want %#v", got, want)
	}
	result = db.Query("SELECT x''-1")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if got, want := result.Rows, [][]interface{}{{int64(-1)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty blob arithmetic = %#v (%T), want %#v (%T)", got, got[0][0], want, want[0][0])
	}
}
