package function

import "testing"

func TestInternalFunctionVisibilityToggle(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterInternal("internal_probe", func([]interface{}) (interface{}, error) {
		return "ok", nil
	}, 0, 0)
	if _, found := registry.Find("INTERNAL_PROBE"); found {
		t.Fatal("internal function visible before test-control toggle")
	}
	if enabled := registry.ToggleInternalFunctions(); !enabled {
		t.Fatal("internal function toggle did not enable functions")
	}
	if fn, found := registry.Find("internal_probe"); !found || fn.ScalarFn == nil {
		t.Fatal("internal function not visible after test-control toggle")
	}
	if enabled := registry.ToggleInternalFunctions(); enabled {
		t.Fatal("internal function toggle did not disable functions")
	}
	if _, found := registry.Find("internal_probe"); found {
		t.Fatal("internal function visible after test-control disabled")
	}
}
