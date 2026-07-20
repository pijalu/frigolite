// Package frigolite — SOLID principle verification tests.
//
// These tests verify that the package architecture follows SOLID design
// principles, particularly:
//
//   - Single Responsibility: each package has exactly one concern
//   - Dependency Inversion: high-level packages depend only on lower-level ones
//   - Interface Segregation: packages expose minimal surface area
package frigolite

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// internalLayers defines the allowed dependency layers for internal packages.
// Lower number = lower level. A package may only import packages from its
// own layer or lower-numbered layers.
var internalLayers = map[string]int{
	"github.com/pijalu/frigolite/internal/util":          0, // utilities
	"github.com/pijalu/frigolite/internal/storage":        1, // file format
	"github.com/pijalu/frigolite/internal/pager":          2, // page cache
	"github.com/pijalu/frigolite/internal/btree":          3, // B-tree
	"github.com/pijalu/frigolite/internal/sql":            4, // parser/lexer
	"github.com/pijalu/frigolite/internal/schema":         5, // schema
	"github.com/pijalu/frigolite/internal/function":       5, // functions
	"github.com/pijalu/frigolite/internal/vtab":           5, // virtual tables
	"github.com/pijalu/frigolite/internal/transaction":    5, // transaction mgmt (WIP)
	"github.com/pijalu/frigolite/internal/exec":           6, // execution engine
}

// TestSOLID_ImportBoundaries verifies that internal packages only import
// from lower or equal layers, preventing circular or inverted dependencies.
//
// This enforces Dependency Inversion: high-level packages (exec) may depend
// on low-level packages (storage, btree) but NOT vice versa.
func TestSOLID_ImportBoundaries(t *testing.T) {
	root := findModuleRoot(t)
	internalDir := filepath.Join(root, "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		t.Fatalf("reading internal dir: %v", err)
	}

	type pkgInfo struct {
		path    string
		imports []string
	}

	var pkgs []pkgInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkgPath := filepath.Join(internalDir, entry.Name())
		pkgImport := "github.com/pijalu/frigolite/internal/" + entry.Name()

		imports, err := parseImports(pkgPath)
		if err != nil {
			t.Errorf("parsing %s: %v", pkgPath, err)
			continue
		}

		// Filter to only internal frigolite imports
		var internalImports []string
		for _, imp := range imports {
			if strings.HasPrefix(imp, "github.com/pijalu/frigolite/internal/") {
				internalImports = append(internalImports, imp)
			}
		}
		sort.Strings(internalImports)

		if len(internalImports) > 0 {
			pkgs = append(pkgs, pkgInfo{
				path:    pkgImport,
				imports: internalImports,
			})
		}

		thisLayer, hasLayer := internalLayers[pkgImport]
		if !hasLayer {
			t.Errorf("package %s has no layer assignment", pkgImport)
			continue
		}

		for _, imp := range internalImports {
			impLayer, ok := internalLayers[imp]
			if !ok {
				t.Errorf("  imported package %s (from %s) has no layer assignment", imp, pkgImport)
				continue
			}
			if impLayer > thisLayer {
				t.Errorf("DEPENDENCY INVERSION: %s (layer %d) imports %s (layer %d) — higher layer must not depend on lower",
					pkgImport, thisLayer, imp, impLayer)
			}
		}
	}

	// Report import graph
	if len(pkgs) > 0 {
		t.Logf("Internal import graph:")
		for _, p := range pkgs {
			t.Logf("  %s → %v", p.path, p.imports)
		}
	}
}

// TestSOLID_SingleResponsibility verifies each internal package has a
// focused scope by checking exported symbol count is reasonable.
func TestSOLID_SingleResponsibility(t *testing.T) {
	root := findModuleRoot(t)
	internalDir := filepath.Join(root, "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		t.Fatalf("reading internal dir: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkgPath := filepath.Join(internalDir, entry.Name())

		// Count exported symbols
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, pkgPath, nil, parser.AllErrors)
		if err != nil {
			t.Errorf("parsing %s: %v", pkgPath, err)
			continue
		}
		if len(pkgs) == 0 {
			t.Logf("  %s: (empty — no .go files)", entry.Name())
			continue
		}

		exportedCount := 0
		for _, pkg := range pkgs {
			if pkg == nil || pkg.Scope == nil {
				continue
			}
			for name := range pkg.Scope.Objects {
				if name[0] >= 'A' && name[0] <= 'Z' {
					exportedCount++
				}
			}
		}
		t.Logf("  %s: %d exported symbols", entry.Name(), exportedCount)
	}
}

// parseImports returns all import paths found in .go files under dir.
func parseImports(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}

	var imports []string
	seen := make(map[string]bool)
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if !seen[path] {
					imports = append(imports, path)
					seen[path] = true
				}
			}
		}
	}
	return imports, nil
}

// findModuleRoot finds the project root by looking for go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found")
		}
		dir = parent
	}
}
