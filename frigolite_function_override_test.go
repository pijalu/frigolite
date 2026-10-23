package frigolite

import (
	"strings"
	"testing"
)

func TestApplicationFunctionOverridesBuiltin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE sample(name)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	registerTCLIdentityFunction(t, db, TestStep{
		FunctionName: "substr",
		FunctionProc: "failing_app_func",
		FunctionArgs: []string{"args"},
		FunctionBody: `error "bad function"`,
		MinArgs:      0,
		MaxArgs:      -1,
	}, make(map[string]string))
	result := db.Query("SELECT substr(name,1,3) FROM sqlite_master")
	if result.Error == nil || !strings.Contains(result.Error.Error(), "bad function") {
		t.Fatalf("overridden substr result = %v, error = %v; want bad function error", result.Rows, result.Error)
	}
}
