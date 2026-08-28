// Package main implements the tcl2go tool.
//
// This file implements the fts3_build_db_1 / fts3_build_db_2 test-data
// loaders from sqlite/test/fts3_common.tcl. The TCL procs build a sample FTS
// table (t1 or t2) with n rows of synthetic text. The transpiler emits a
// package-level Go helper plus a call at each usage, so the generated test is
// self-contained (matching the fts_kjv_genesis pattern in genesis.go).
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ftsBuildPreambleText returns the package-level helper source shared by
// fts3_build_db_1 and fts3_build_db_2. It is emitted once per generated file.
func ftsBuildPreambleText() string {
	return `// fts3BuildDB1 mirrors SQLite test/fts3_common.tcl's fts3_build_db_1:
// create virtual table t1 USING <module>(x, y) and insert n rows where x is
// four x-words (zero..ten) and y is four y-words (alpha..kappa) selected by
// the decimal digits of the docid. Each insert commits separately (SQLite's
// db eval per row), so every flush writes its own %_segdir segment.
func fts3BuildDB1(t *testing.T, db *frigolite.DB, module string, n int) {
	if rerr := db.Exec("CREATE VIRTUAL TABLE t1 USING " + module + " (x, y)").Error; rerr != nil {
		t.Errorf("fts3_build_db_1 create: %v", rerr)
		return
	}
	xwords := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	ywords := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	// Each insert commits separately (SQLite's fts3_build_db_1 uses db eval
	// per row), so every flush writes its own %_segdir segment and the
	// crisis-merge at MergeCount=16 produces the expected level structure.
	for i := 0; i < n; i++ {
		x := xwords[(i/1000)%10] + " " + xwords[(i/100)%10] + " " + xwords[(i/10)%10] + " " + xwords[i%10]
		y := ywords[(i/1000)%10] + " " + ywords[(i/100)%10] + " " + ywords[(i/10)%10] + " " + ywords[i%10]
		if rerr := db.Exec("INSERT INTO t1(docid, x, y) VALUES(" + strconv.Itoa(i) + ", '" + x + "', '" + y + "')").Error; rerr != nil {
			t.Errorf("fts3_build_db_1 insert: %v", rerr)
			return
		}
	}
}

// fts3BuildDB2 mirrors SQLite test/fts3_common.tcl's fts3_build_db_2: create
// virtual table t2 USING <module>(content[, extra]) and insert n rows whose
// single column is a 3-char word from the chars list (a..z, ""), selected by
// the base-27 digits of the docid. Each insert commits separately (SQLite's
// db eval per row), so every flush writes its own %_segdir segment.
func fts3BuildDB2(t *testing.T, db *frigolite.DB, module string, extra string, n int) {
	cols := "content"
	if extra != "" {
		cols += ", " + extra
	}
	if rerr := db.Exec("CREATE VIRTUAL TABLE t2 USING " + module + " (" + cols + ")").Error; rerr != nil {
		t.Errorf("fts3_build_db_2 create: %v", rerr)
		return
	}
	chars := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z", ""}
	nChar := len(chars)
	// Each insert commits separately (SQLite's fts3_build_db_2 uses db eval
	// per row) so every flush writes its own %_segdir segment.
	for i := 0; i < n; i++ {
		word := chars[(i/1)%nChar] + chars[(i/nChar)%nChar] + chars[(i/(nChar*nChar))%nChar]
		if rerr := db.Exec("INSERT INTO t2(docid, content) VALUES(" + strconv.Itoa(i) + ", '" + word + "')").Error; rerr != nil {
			t.Errorf("fts3_build_db_2 insert: %v", rerr)
			return
		}
	}
}

// buildMultilingualDB1 mirrors SQLite test/fts4langid.tcl's
// build_multilingual_db_1: create t2 USING fts4(x, y, languageid=l) and
// insert 1000 rows whose x/y are four words each selected by the decimal
// digits of the docid, with language id i%9. Then mirror the rows into a
// plain data(x, y, l) table for expected-result computation.
func buildMultilingualDB1(t *testing.T, db *frigolite.DB) {
	if rerr := db.Exec("CREATE VIRTUAL TABLE t2 USING fts4(x, y, languageid=l)").Error; rerr != nil {
		t.Errorf("build_multilingual_db_1 create: %v", rerr)
		return
	}
	xwords := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	ywords := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	for i := 0; i < 1000; i++ {
		x := xwords[(i/1000)%10] + " " + xwords[(i/100)%10] + " " + xwords[(i/10)%10] + " " + xwords[i%10]
		y := ywords[(i/1000)%10] + " " + ywords[(i/100)%10] + " " + ywords[(i/10)%10] + " " + ywords[i%10]
		if rerr := db.Exec("INSERT INTO t2(docid, x, y, l) VALUES(" + strconv.Itoa(i) + ", '" + x + "', '" + y + "', " + strconv.Itoa(i%9) + ")").Error; rerr != nil {
			t.Errorf("build_multilingual_db_1 insert: %v", rerr)
			return
		}
	}
	if rerr := db.Exec("CREATE TABLE data(x, y, l);").Error; rerr != nil {
		t.Errorf("build_multilingual_db_1 data create: %v", rerr)
		return
	}
	if rerr := db.Exec("INSERT INTO data(rowid, x, y, l) SELECT docid, x, y, l FROM t2;").Error; rerr != nil {
		t.Errorf("build_multilingual_db_1 data copy: %v", rerr)
	}
}

// buildMultilingualDB2 mirrors SQLite test/fts4langid.tcl's
// build_multilingual_db_2: create t4 USING fts4(tokenize=testtokenizer,
// languageid=lid) and insert 50 rows 'The Quick Brown Fox' with lid=i.
func buildMultilingualDB2(t *testing.T, db *frigolite.DB) {
	if rerr := db.Exec("CREATE VIRTUAL TABLE t4 USING fts4(tokenize=testtokenizer, languageid=lid)").Error; rerr != nil {
		t.Errorf("build_multilingual_db_2 create: %v", rerr)
		return
	}
	for i := 0; i < 50; i++ {
		if rerr := db.Exec("INSERT INTO t4(docid, content, lid) VALUES(" + strconv.Itoa(i) + ", 'The Quick Brown Fox', " + strconv.Itoa(i) + ")").Error; rerr != nil {
			t.Errorf("build_multilingual_db_2 insert: %v", rerr)
			return
		}
	}
}

// buildMultilingualDB3 mirrors SQLite test/fts4langid.tcl's
// build_multilingual_db_3: create t5 USING fts4(languageid=lid) and insert
// one row per language id in [0, 1, 2, 1<<30], docid = langid, content =
// 'My language is <langid>'.
func buildMultilingualDB3(t *testing.T, db *frigolite.DB) {
	if rerr := db.Exec("CREATE VIRTUAL TABLE t5 USING fts4(languageid=lid)").Error; rerr != nil {
		t.Errorf("build_multilingual_db_3 create: %v", rerr)
		return
	}
	languages := []int64{0, 1, 2, 1 << 30}
	for _, lid := range languages {
		if rerr := db.Exec("INSERT INTO t5(docid, content, lid) VALUES(" +
			strconv.FormatInt(lid, 10) + ", 'My language is " + strconv.FormatInt(lid, 10) + "', " +
			strconv.FormatInt(lid, 10) + ")").Error; rerr != nil {
			t.Errorf("build_multilingual_db_3 insert: %v", rerr)
			return
		}
	}
}
`
}

// genFTSBuildPreamble holds the package-level fts3BuildDB1/fts3BuildDB2
// helper source, built lazily on first use. It is a package-level var (like
// genBlobUsedChannels) so every bodyTP copy shares the same builder and no
// per-copy propagation is needed when a fts3_build_db_1/2 call appears inside
// a do_test/foreach body.
var genFTSBuildPreamble *strings.Builder

// ensureFTSBuildPreamble emits the package-level fts3BuildDB1/fts3BuildDB2
// helpers once per generated file.
func (tp *transpiler) ensureFTSBuildPreamble() {
	if genFTSBuildPreamble == nil {
		genFTSBuildPreamble = &strings.Builder{}
		genFTSBuildPreamble.WriteString(ftsBuildPreambleText())
	}
	tp.ftsBuildPreamble = genFTSBuildPreamble
}

// processFTS3BuildDB1 handles `fts3_build_db_1 ?switches? n` — build the
// sample FTS table t1 (see sqlite/test/fts3_common.tcl). Supported switches:
// -module NAME (default fts4). The final argument is the row count.
func (tp *transpiler) processFTS3BuildDB1(args []tcl.RawWord) {
	module, n, ok := tp.parseFTSBuildArgs(args)
	if !ok {
		tp.emitLine("// fts3_build_db_1 (unparseable args, not transpiled)")
		return
	}
	tp.ensureFTSBuildPreamble()
	tp.emitLine("fts3BuildDB1(t, db, %s, %s)", module, n)
}

// processFTS3BuildDB2 handles `fts3_build_db_2 ?switches? n` — build the
// sample FTS table t2 (see sqlite/test/fts3_common.tcl). Supported switches:
// -module NAME (default fts4) and -extra TEXT (appended to the column list).
// The final argument is the row count.
func (tp *transpiler) processFTS3BuildDB2(args []tcl.RawWord) {
	module, extra, n, ok := tp.parseFTSBuildArgs2(args)
	if !ok {
		tp.emitLine("// fts3_build_db_2 (unparseable args, not transpiled)")
		return
	}
	tp.ensureFTSBuildPreamble()
	tp.emitLine("fts3BuildDB2(t, db, %s, %s, %s)", module, extra, n)
}

// processBuildMultilingualDB1 handles `build_multilingual_db_1 db` — the
// fts4langid.tcl data loader: create t2 USING fts4(x, y, languageid=l) and
// insert 1000 rows whose l cycles 0..8 (fts4langid.test build_multilingual_db_1).
func (tp *transpiler) processBuildMultilingualDB1(args []tcl.RawWord) {
	tp.ensureFTSBuildPreamble()
	tp.emitLine("buildMultilingualDB1(t, db)")
}

// processBuildMultilingualDB2 handles `build_multilingual_db_2 db` — the
// fts4langid.tcl section-4 loader: create t4 USING
// fts4(tokenize=testtokenizer, languageid=lid) and insert 50 rows of 'The
// Quick Brown Fox' with lid=i.
func (tp *transpiler) processBuildMultilingualDB2(args []tcl.RawWord) {
	tp.ensureFTSBuildPreamble()
	tp.emitLine("buildMultilingualDB2(t, db)")
}

// processBuildMultilingualDB3 handles `build_multilingual_db_3 db` — the
// fts4langid.tcl section-5 loader: create t5 USING fts4(languageid=lid) and
// insert rows for languages 0, 1, 2 and 1<<30.
func (tp *transpiler) processBuildMultilingualDB3(args []tcl.RawWord) {
	tp.ensureFTSBuildPreamble()
	tp.emitLine("buildMultilingualDB3(t, db)")
}

// parseFTSBuildArgs parses fts3_build_db_1's argument list
// (-module EXPR [..] N). It returns Go expressions for the module and the row
// count. The row count may be a literal int or a $var expression.
func (tp *transpiler) parseFTSBuildArgs(args []tcl.RawWord) (moduleExpr, nExpr string, ok bool) {
	moduleExpr = `"fts4"`
	if len(args) == 0 {
		return "", "", false
	}
	i := 0
	for i < len(args)-1 {
		switch strings.TrimSpace(args[i].Text) {
		case "-module":
			if i+1 >= len(args)-1 {
				return "", "", false
			}
			moduleExpr = ftsBuildArgExpr(args[i+1])
			i += 2
		default:
			return "", "", false
		}
	}
	nExpr = ftsBuildArgExpr(args[len(args)-1])
	return moduleExpr, nExpr, true
}

// parseFTSBuildArgs2 parses fts3_build_db_2's argument list
// (-module EXPR / -extra EXPR [..] N). It returns Go expressions for the
// module, the extra column text, and the row count.
func (tp *transpiler) parseFTSBuildArgs2(args []tcl.RawWord) (moduleExpr, extraExpr, nExpr string, ok bool) {
	moduleExpr = `"fts4"`
	extraExpr = `""`
	if len(args) == 0 {
		return "", "", "", false
	}
	i := 0
	for i < len(args)-1 {
		switch strings.TrimSpace(args[i].Text) {
		case "-module":
			if i+1 >= len(args)-1 {
				return "", "", "", false
			}
			moduleExpr = ftsBuildArgExpr(args[i+1])
			i += 2
		case "-extra":
			if i+1 >= len(args)-1 {
				return "", "", "", false
			}
			extraExpr = ftsBuildArgExpr(args[i+1])
			i += 2
		default:
			return "", "", "", false
		}
	}
	nExpr = ftsBuildArgExpr(args[len(args)-1])
	return moduleExpr, extraExpr, nExpr, true
}

// ftsBuildArgExpr converts a raw word into a Go expression: $var references
// become the corresponding Go variable; everything else becomes a quoted Go
// string literal (the row count is emitted as a number literal when possible).
func ftsBuildArgExpr(w tcl.RawWord) string {
	text := strings.TrimSpace(w.Text)
	if strings.HasPrefix(text, "$") {
		return tclVarToGo(text)
	}
	if n, err := strconv.Atoi(text); err == nil {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%q", text)
}
