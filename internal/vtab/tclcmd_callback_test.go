package vtab

import "testing"

type declaredSchemaRuntime struct{}

func (declaredSchemaRuntime) HasProc(string) bool { return true }
func (declaredSchemaRuntime) Connect(string, []string) (string, error) {
	return "CREATE TABLE t4(x INTEGER, y TEXT, z)", nil
}
func (declaredSchemaRuntime) BestIndex(string, []string, *IndexInfo) (string, error) {
	return "", nil
}
func (declaredSchemaRuntime) Filter(string, []string, int, string, []interface{}) (string, error) {
	return "", nil
}
func (declaredSchemaRuntime) Query(string) ([][]interface{}, []int64, error) {
	return nil, nil, nil
}

func TestTclCommandModulePreservesDeclaredColumnTypes(t *testing.T) {
	module := NewTclCommandModuleWithRuntime(declaredSchemaRuntime{})
	instance, err := module.Connect([]string{"callback"})
	if err != nil {
		t.Fatal(err)
	}
	typed, ok := instance.(ColumnTypeInfo)
	if !ok {
		t.Fatal("Tcl vtab does not expose declared column types")
	}
	if got, want := typed.ColumnTypes(), []string{"INTEGER", "TEXT", ""}; !equalStrings(got, want) {
		t.Fatalf("column types = %#v, want %#v", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
