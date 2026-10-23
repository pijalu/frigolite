package frigolite

import "testing"

// TestJSONTreeCorrelatedWhereFiltersRetainRowProvenance covers json102-1130:
// filtering correlated virtual-table rows must preserve their outer-row index.
func TestJSONTreeCorrelatedWhereFiltersRetainRowProvenance(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`CREATE TABLE big(json);
		INSERT INTO big VALUES('{"id":123,"partlist":[{"uuid":"6fa5181e-5721-11e5-a04e-57f3d7b32808"}]}');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query(`SELECT DISTINCT json_extract(big.json,'$.id')
		FROM big, json_tree(big.json,'$.partlist')
		WHERE json_tree.key='uuid'
		AND json_tree.value='6fa5181e-5721-11e5-a04e-57f3d7b32808';`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != int64(123) {
		t.Fatalf("rows = %#v, want [[123]]", r.Rows)
	}
}
