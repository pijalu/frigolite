// SPDX-License-Identifier: GPL-3.0-or-later
// Package main implements the tcl2go tool: a TCL-to-Go transpiler that converts
// SQLite TCL test files (.test) into standalone Go test files (_test.go).
//
// This file contains the generated-file import detection: scanning the
// emitted Go body for package references (comment- and string-aware) and
// assembling the final import block.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// detectImports scans generated code for package references and returns only the needed imports.
var allStandardImports = []struct{ name, path string }{
	{"errors", "errors"},
	{"vtab", "github.com/pijalu/frigolite/internal/vtab"},
	{"fmt", "fmt"},
	{"os", "os"},
	{"filepath", "path/filepath"},
	{"regexp", "regexp"},
	{"sort", "sort"},
	{"strconv", "strconv"},
	{"strings", "strings"},
	{"time", "time"},
}

func detectImports(code string) []string {
	// Package references inside line comments (label comments carry Go
	// expression TEXT, e.g. "{ // "1." + tn + strconv.Itoa(...) }") must not
	// register imports — fts5phrase's label comment referenced strconv only
	// in comment text and produced an unused import (compile error).
	code = stripGoLineComments(code)
	needed := map[string]bool{
		"testing":                     true, // always needed
		"github.com/pijalu/frigolite": true, // always needed
	}

	// The date/time tests emit function.SetLocaltimeHook(...) for the TCL
	// harness's SQLITE_TESTCTRL_LOCALTIME_FAULT control.
	if hasPackageRef(code, "function") {
		needed["github.com/pijalu/frigolite/internal/function"] = true
	}
	// zeroblob tests emit storage.SetMaxBlobsize/MaxBlobsize for the TCL
	// harness's linked sqlite3_max_blobsize global.
	if hasPackageRef(code, "storage") {
		needed["github.com/pijalu/frigolite/internal/storage"] = true
	}
	// Authorizer tests emit auth.Authorizer types (db authorizer ::auth).
	if hasPackageRef(code, "auth") {
		needed["github.com/pijalu/frigolite/internal/auth"] = true
	}
	// The sqlite3_fts5_tokenize bridge (fts5TclTokenize helper) uses the
	// engine's fts5 tokenizer registry. Gated on the preamble so SQL text
	// that merely mentions fts5 does not pull the import in.
	if genFTS5TokenizePreamble != nil && hasPackageRef(code, "fts5") {
		needed["github.com/pijalu/frigolite/internal/fts5"] = true
	}

	for _, imp := range allStandardImports {
		// Check if the package name appears as a Go identifier reference
		// (preceded by a non-identifier character, followed by ".X" where X is uppercase)
		// This avoids false positives from package names appearing in SQL strings.
		if hasPackageRef(code, imp.name) {
			needed[imp.path] = true
		}
	}
	// Sort for deterministic output
	var result []string
	for p := range needed {
		result = append(result, p)
	}
	sort.Strings(result)
	return result
}

// hasPackageRef checks if pkgName appears as a Go package reference in code.
// It looks for patterns where pkgName is preceded by a non-identifier character
// and followed by ".Func" where Func starts uppercase.
func hasPackageRef(code, pkgName string) bool {
	search := pkgName + "."
	for {
		idx := strings.Index(code, search)
		if idx < 0 {
			return false
		}
		// Check word boundary before pkgName; skip when preceded by a
		// backslash (inside a Go string escape) or a Go identifier char.
		if !isPackageRefBoundary(code, idx) {
			code = code[idx+len(search):]
			continue
		}
		// Check next char after dot is uppercase (exported function)
		afterIdx := idx + len(search)
		if afterIdx < len(code) && code[afterIdx] >= 'A' && code[afterIdx] <= 'Z' {
			return true
		}
		code = code[idx+len(search):]
	}
}

// stripGoLineComments removes // line comments from emitted Go code, honoring
// string literals (a "//" inside a quoted or raw string is not a comment).
func stripGoLineComments(code string) string {
	var b strings.Builder
	for i := 0; i < len(code); {
		c := code[i]
		switch {
		case c == '"' || c == '`':
			i = writeGoStringLiteral(&b, code, i)
		case c == '\'' && i+2 < len(code) && code[i+1] != '\\':
			b.WriteString(code[i : i+3])
			i += 3
		case c == '/' && i+1 < len(code) && code[i+1] == '/':
			for i < len(code) && code[i] != '\n' {
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// writeGoStringLiteral copies a quoted (") or raw (`) string literal starting
// at code[i] to b and returns the offset just past it. A backslash escape
// inside an interpreted string is copied verbatim (so \" never ends the
// literal).
func writeGoStringLiteral(b *strings.Builder, code string, i int) int {
	quote := code[i]
	b.WriteByte(quote)
	i++
	for i < len(code) {
		if quote == '"' && code[i] == '\\' && i+1 < len(code) {
			b.WriteByte(code[i])
			b.WriteByte(code[i+1])
			i += 2
			continue
		}
		b.WriteByte(code[i])
		if code[i] == quote {
			i++
			break
		}
		i++
	}
	return i
}

// isPackageRefBoundary reports whether the character before position idx is a
// valid package-reference boundary (not a backslash escape and not part of a
// longer identifier).
func isPackageRefBoundary(code string, idx int) bool {
	if idx == 0 {
		return true
	}
	prev := code[idx-1]
	// Skip if preceded by backslash (inside a Go string escape)
	if prev == '\\' {
		return false
	}
	if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') ||
		(prev >= '0' && prev <= '9') || prev == '_' {
		return false
	}
	return true
}

// genFileHeader writes the generated-file banner: the DO NOT EDIT marker, the
// opt-in testgen build tag (generated test packages only compile when the
// testgen build tag is set, so 'go test ./...' — e.g. the SOLID verify
// command — builds only hand-written, non-generated code), the package
// clause, and the import block.
func genFileHeader(sb *strings.Builder, pkg string, imports []string) {
	sb.WriteString("// Code generated by tcl2go; DO NOT EDIT.\n")
	sb.WriteString("//go:build testgen\n")
	sb.WriteString("// +build testgen\n\n")
	sb.WriteString(fmt.Sprintf("package %s\n\n", pkg))
	sb.WriteString("import (\n")
	for _, imp := range imports {
		sb.WriteString(fmt.Sprintf("\"%s\"\n", imp))
	}
	sb.WriteString(")\n\n")
}
