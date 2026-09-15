// Package main implements the tcl2go tool.
//
// This file emits the package-level bridge for the sqlite3_fts5_tokenize TCL
// command (ext/fts5/fts5_tcl.c f5tTokenize): a generated do_test body calling
// it runs the same request against the engine's own fts5 tokenizer registry
// (internal/fts5), which ports the four C tokenizers.
package main

import "strings"

// genFTS5TokenizePreamble holds the package-level fts5TclTokenize helper for
// the file currently being generated. It is package-level (like
// genFTSBuildPreamble) because a sqlite3_fts5_tokenize call may appear inside
// a do_test/foreach body whose sub-transpiler copy is discarded.
var genFTS5TokenizePreamble *strings.Builder

// useFTS5Tokenize marks the file being generated as needing the helper and
// lazily writes the helper source once.
func useFTS5Tokenize() {
	if genFTS5TokenizePreamble == nil {
		genFTS5TokenizePreamble = &strings.Builder{}
		genFTS5TokenizePreamble.WriteString(fts5TokenizePreambleText())
	}
}

// fts5TokenizePreambleText returns the fts5TclTokenize helper source.
func fts5TokenizePreambleText() string {
	var b strings.Builder
	b.WriteString("// fts5TclTokenize mirrors the sqlite3_fts5_tokenize TCL command\n")
	b.WriteString("// (ext/fts5/fts5_tcl.c f5tTokenize + xTokenizeCb2 without -subst): tokenize\n")
	b.WriteString("// input through the tokenizer named by spec (a TCL list of spec words) and\n")
	b.WriteString("// return the flat TCL list \"token start end ...\" with one triple per\n")
	b.WriteString("// token. A failed tokenizer lookup yields an empty string (the C command\n")
	b.WriteString("// raises a TCL error; corpora reaching this helper use valid specs).\n")
	b.WriteString("func fts5TclTokenize(db *frigolite.DB, spec, input string) string {\n")
	b.WriteString("\t_ = db\n")
	b.WriteString("\ttok, err := fts5.NewTokenizer(tclSplitList(spec))\n")
	b.WriteString("\tif err != nil {\n")
	b.WriteString("\t\treturn \"\"\n")
	b.WriteString("\t}\n")
	b.WriteString("\tvar b strings.Builder\n")
	b.WriteString("\tfor _, t := range tok.Tokenize(input) {\n")
	b.WriteString("\t\tif b.Len() > 0 {\n")
	b.WriteString("\t\t\tb.WriteByte(' ')\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\tb.WriteString(tclListElem(t.Term))\n")
	b.WriteString("\t\tb.WriteString(\" \" + strconv.Itoa(t.Start) + \" \" + strconv.Itoa(t.End))\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn b.String()\n")
	b.WriteString("}\n")
	return b.String()
}
