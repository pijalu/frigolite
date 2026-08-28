// Package main implements the tcl2go tool.
//
// This file implements the fts_kjv_genesis test-data loader: the TCL
// `source $testdir/genesis.tcl` script defines a proc that fills the table
// t1(docid, words) with the complete text of the Book of Genesis (1533
// INSERTs). The transpiler reads that script at generation time, extracts the
// SQL, and emits a package-level Go helper plus a call at each fts_kjv_genesis
// usage, so the generated test is self-contained.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// genesisProcRe matches the fts_kjv_genesis proc header and its `db eval {`
// opening brace. The body SQL follows until the first closing brace (SQL text
// never contains a brace).
var genesisProcRe = regexp.MustCompile(`(?s)proc\s+fts_kjv_genesis\s*\{\s*\}.*?db\s+eval\s*\{`)

// loadGenesisSQL reads genesis.tcl from the test directory and returns the SQL
// executed by the fts_kjv_genesis proc (the `db eval { ... }` body). Returns
// "" when the file or the proc is not found.
func loadGenesisSQL(testDir string) string {
	if testDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(testDir, "genesis.tcl"))
	if err != nil {
		return ""
	}
	m := genesisProcRe.FindIndex(data)
	if m == nil {
		return ""
	}
	rest := data[m[1]:]
	// The SQL block ends at the first '}' (the proc body has no nested
	// braces, and SQL text contains no braces).
	end := strings.IndexByte(string(rest), '}')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(string(rest[:end]))
}

// processFTSKJVGenesis handles `fts_kjv_genesis` — fills table t1(docid, words)
// with the Book of Genesis. It emits a call to a package-level helper that
// executes the INSERTs (built once per generated file).
func (tp *transpiler) processFTSKJVGenesis(args []tcl.RawWord) {
	sql := loadGenesisSQL(tp.testDir)
	if sql == "" {
		tp.emitLine("// fts_kjv_genesis: genesis.tcl not found")
		return
	}
	if tp.genesisPreamble == nil {
		tp.genesisPreamble = &strings.Builder{}
		tp.genesisPreamble.WriteString("// ftsKJVGenesis fills table t1(docid, words) with the text of the\n")
		tp.genesisPreamble.WriteString("// Book of Genesis (SQLite test/genesis.tcl).\n")
		tp.genesisPreamble.WriteString("func ftsKJVGenesis(t *testing.T, db *frigolite.DB) {\n")
		tp.genesisPreamble.WriteString("\tr := db.Exec(" + fmt.Sprintf("%q", sql) + ")\n")
		tp.genesisPreamble.WriteString("\tif r.Error != nil {\n")
		tp.genesisPreamble.WriteString("\t\tt.Errorf(\"fts_kjv_genesis: %v\", r.Error)\n")
		tp.genesisPreamble.WriteString("\t}\n")
		tp.genesisPreamble.WriteString("}\n")
	}
	tp.emitLine("ftsKJVGenesis(t, db)")
}
