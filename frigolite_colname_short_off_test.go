package frigolite

import (
	"reflect"
	"testing"
)

func TestColumnNameQualifiedRefPreservedWhenShortNamesOff(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE tabc(a,b,c); INSERT INTO tabc VALUES(1,2,3); PRAGMA short_column_names=OFF; PRAGMA full_column_names=OFF;"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Query("SELECT Tabc.a, tAbc.b, taBc.c FROM tabc")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := []string{"Tabc.a", "tAbc.b", "taBc.c"}
	if !reflect.DeepEqual(result.Columns, want) {
		t.Fatalf("column names = %#v, want %#v", result.Columns, want)
	}
}
