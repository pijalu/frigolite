package parse

import (
	"path/filepath"
	"testing"
)

func TestGlobCheck(t *testing.T) {
	dirs, err := filepath.Glob("../../testgen/*/")
	t.Logf("dirs=%v err=%v", len(dirs), err)
	files, err := filepath.Glob("../../testgen/select1/*_test.go")
	t.Logf("files=%v err=%v", files, err)
}
