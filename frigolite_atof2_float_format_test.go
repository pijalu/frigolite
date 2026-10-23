package frigolite

import (
	"reflect"
	"testing"
)

func TestAtof2AltTwoFormatsShortestRoundTrip(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	result := db.Query("SELECT format('%!.30f',ieee754_inc(100.0,-1)), format('%!.30f',ieee754_inc(100.0,-2))")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := [][]interface{}{{"99.9999999999999858", "99.9999999999999716"}}
	if !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("alternate-form-2 float output = %#v, want %#v", result.Rows, want)
	}
}
