package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
)

// skipMaps holds the two skip maps parsed from tools/tcl2go/gen.go.
type skipMaps struct {
	// skipTestFiles maps TCL test file base names (e.g. "alter2") to the
	// reason the whole file is skipped. The transpiler emits a no-op test
	// function for these.
	skipTestFiles map[string]string
	// skipTests maps TCL test names (e.g. "alter-9.1") to the reason the
	// individual test is skipped. The transpiler emits a no-op block.
	skipTests map[string]string
}

// loadSkipMaps parses and merges the three skip-map source files in
// tools/tcl2go: skiptestfiles.go (whole-file skips) and skiptests.go +
// skiptests2.go (per-test skips).
func loadSkipMaps(repo string) (*skipMaps, error) {
	base := filepath.Join(repo, "tools", "tcl2go")
	out := &skipMaps{
		skipTestFiles: map[string]string{},
		skipTests:     map[string]string{},
	}
	for _, name := range []string{"skiptestfiles.go", "skiptests.go", "skiptests2.go"} {
		m, err := parseSkipMaps(filepath.Join(base, name))
		if err != nil {
			return nil, err
		}
		for k, v := range m.skipTestFiles {
			out.skipTestFiles[k] = v
		}
		for k, v := range m.skipTests {
			out.skipTests[k] = v
		}
	}
	return out, nil
}

// parseSkipMaps parses the skipTestFiles and skipTests map literals from
// a Go source file using go/parser, which is robust to comments, string
// escapes, and formatting (unlike a regex).
func parseSkipMaps(path string) (*skipMaps, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	out := &skipMaps{
		skipTestFiles: map[string]string{},
		skipTests:     map[string]string{},
	}

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			dst, ok := skipMapForSpec(spec, out)
			if !ok {
				continue
			}
			if err := fillSkipMap(dst, spec); err != nil {
				return nil, err
			}
		}
	}

	return out, nil
}

// skipMapForSpec returns the destination map for a ValueSpec whose name
// matches skipTestFiles or skipTests.
func skipMapForSpec(spec ast.Spec, out *skipMaps) (map[string]string, bool) {
	vs, ok := spec.(*ast.ValueSpec)
	if !ok || len(vs.Names) != 1 {
		return nil, false
	}
	switch vs.Names[0].Name {
	case "skipTestFiles":
		return out.skipTestFiles, true
	case "skipTests", "skipTestsMore":
		return out.skipTests, true
	default:
		return nil, false
	}
}

// fillSkipMap populates dst from the composite literal in spec.
func fillSkipMap(dst map[string]string, spec ast.Spec) error {
	vs := spec.(*ast.ValueSpec)
	if len(vs.Values) == 0 {
		return nil
	}
	cl, ok := vs.Values[0].(*ast.CompositeLit)
	if !ok {
		return nil
	}
	for _, elt := range cl.Elts {
		k, v, ok, err := skipMapEntry(elt)
		if err != nil {
			return err
		}
		if ok {
			dst[k] = v
		}
	}
	return nil
}

// skipMapEntry extracts a key/value pair from a map-literal element.
func skipMapEntry(elt ast.Expr) (string, string, bool, error) {
	kv, ok := elt.(*ast.KeyValueExpr)
	if !ok {
		return "", "", false, nil
	}
	key, ok := kv.Key.(*ast.BasicLit)
	if !ok || key.Kind != token.STRING {
		return "", "", false, nil
	}
	val, ok := kv.Value.(*ast.BasicLit)
	if !ok || val.Kind != token.STRING {
		return "", "", false, nil
	}
	k, err := strconv.Unquote(key.Value)
	if err != nil {
		return "", "", false, fmt.Errorf("bad map key %s: %w", key.Value, err)
	}
	v, err := strconv.Unquote(val.Value)
	if err != nil {
		return "", "", false, fmt.Errorf("bad map value for %q: %w", k, err)
	}
	return k, v, true, nil
}
