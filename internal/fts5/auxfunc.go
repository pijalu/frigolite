package fts5

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file exposes the per-row aux machinery as the fts5 test-support API
// surface (fts5_aux.c's Fts5ExtensionApi, plus the fts5_aux_test_functions
// of ext/fts5/test/fts5_common.tcl and the ad-hoc registrations of
// fts5aux.test, compiled into SQLite's test builds only). The engine
// registers them unconditionally — a documented deviation from C's
// SQLITE_TEST gating.
//
// Every xXxx method mirrors the C entry point of the same name. Out-of-range
// indices fail with ErrSQLITERange — the TCL wrapper surfaces rc as
// sqlite3ErrName(rc), so the SQL-visible error text is exactly "SQLITE_RANGE".

// ErrSQLITERange is the out-of-range failure of the index/phrase accessors.
var ErrSQLITERange = errors.New("SQLITE_RANGE")

// InstCount returns the row's phrase-instance count (xInstCount).
func (aq *AuxQuery) InstCount(rowid int64) int {
	return len(aq.RowInstances(rowid))
}

// Inst returns instance i of the row in xInst order (xInst).
func (aq *AuxQuery) Inst(rowid int64, i int) (Inst, error) {
	insts := aq.RowInstances(rowid)
	if i < 0 || i >= len(insts) {
		return Inst{}, ErrSQLITERange
	}
	return insts[i], nil
}

// ColumnSize returns the row's token count in column iCol, or the whole
// document's count for -1 (xColumnSize).
func (aq *AuxQuery) ColumnSize(rowid int64, iCol int) (int64, error) {
	t := aq.t
	switch {
	case iCol == -1:
		return int64(t.docTokenCount(rowid)), nil
	case iCol < 0 || iCol >= len(t.cfg.Columns):
		return 0, ErrSQLITERange
	}
	return int64(t.columnTokenCount(rowid, iCol)), nil
}

// ColumnTotalSize returns the table's total token count in column iCol, or
// every column's total for -1 (xColumnTotalSize).
func (aq *AuxQuery) ColumnTotalSize(iCol int) (int64, error) {
	nCol := len(aq.t.cfg.Columns)
	switch {
	case iCol == -1:
		return aq.t.totalTokens(), nil
	case iCol < 0 || iCol >= nCol:
		return 0, ErrSQLITERange
	}
	return aq.t.ix.nTokensPerCol[iCol], nil
}

// ColumnText returns the row-column's stored text (xColumnText). A NULL
// column yields the empty string; a bad index fails with ErrSQLITERange.
func (aq *AuxQuery) ColumnText(rowid int64, iCol int) (string, error) {
	text, ok, err := aq.t.columnText(rowid, iCol)
	if err != nil {
		var rangeErr *ColumnRangeError
		if errors.As(err, &rangeErr) {
			return "", ErrSQLITERange
		}
		return "", err
	}
	if !ok {
		return "", nil
	}
	return text, nil
}

// RowCount returns the indexed document count (xRowCount).
func (aq *AuxQuery) RowCount() int64 { return int64(aq.t.ix.NumDocs()) }

// TokenizeText tokenizes text with the table's tokenizer, returning the
// terms (xTokenize).
func (aq *AuxQuery) TokenizeText(text string) []string {
	var out []string
	for _, tok := range aq.t.tok.Tokenize(text) {
		out = append(out, tok.Term)
	}
	return out
}

// QueryPhraseRowids visits the rows phrase iPhrase alone matches, in rowid
// order, collecting their rowids (xQueryPhrase). stopCode mirrors the TCL
// callback protocol of fts5aux.test's phrasequery(): when the visited rowid
// equals outerRowid the callback returns stopCode — SQLITE_DONE ends the
// scan with the rows collected so far, any other non-empty code fails with
// that text (SQLITE_OK and the empty string continue).
func (aq *AuxQuery) QueryPhraseRowids(iPhrase int, stopCode string, outerRowid int64) ([]int64, error) {
	ph, err := aq.queryPhrase(iPhrase)
	if err != nil {
		return nil, err
	}
	set, err := aq.t.evalPhrase(ph)
	if err != nil {
		return nil, err
	}
	rowids := aq.t.SortedMatchRowids(set)
	var out []int64
	for _, rowid := range rowids {
		out = append(out, rowid)
		if rowid == outerRowid && stopCode != "" && stopCode != "SQLITE_OK" {
			if stopCode == "SQLITE_DONE" {
				return out, nil
			}
			return out, errors.New(stopCode)
		}
	}
	return out, nil
}

// QueryPhraseColumnHits returns, per column, the number of rows with at
// least one instance of phrase iPhrase in that column (xQueryPhrase with
// fts5_common.tcl's test_queryphrase_cb counter).
func (aq *AuxQuery) QueryPhraseColumnHits(iPhrase int) ([]int64, error) {
	ph, err := aq.queryPhrase(iPhrase)
	if err != nil {
		return nil, err
	}
	set, err := aq.t.evalPhrase(ph)
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(aq.t.cfg.Columns))
	for _, rowid := range aq.t.SortedMatchRowids(set) {
		seen := map[int]bool{}
		for _, in := range aq.t.phraseInstances(ph, aq.t.ix.Doc(rowid)) {
			if !seen[in.Col] {
				seen[in.Col] = true
				out[in.Col]++
			}
		}
	}
	return out, nil
}

// PhraseHitCount returns the total number of phrase iPhrase instances across
// every row it appears in (fts5aux.test's fts5_hitcount: xQueryPhrase
// summing xInstCount per visited row).
func (aq *AuxQuery) PhraseHitCount(iPhrase int) (int, error) {
	ph, err := aq.queryPhrase(iPhrase)
	if err != nil {
		return 0, err
	}
	set, err := aq.t.evalPhrase(ph)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rowid := range aq.t.SortedMatchRowids(set) {
		n += len(aq.t.phraseInstances(ph, aq.t.ix.Doc(rowid)))
	}
	return n, nil
}

// queryPhrase resolves and range-checks a phrase index.
func (aq *AuxQuery) queryPhrase(iPhrase int) (*phraseNode, error) {
	if iPhrase < 0 || iPhrase >= len(aq.phrases) {
		return nil, ErrSQLITERange
	}
	return aq.phrases[iPhrase], nil
}

// PhraseCollist returns the distinct columns the row's phrase-iPhrase
// instances appear in, ascending (xPhraseColumnForeach).
func (aq *AuxQuery) PhraseCollist(rowid int64, iPhrase int) ([]int, error) {
	ph, err := aq.queryPhrase(iPhrase)
	if err != nil {
		return nil, err
	}
	doc := aq.t.ix.Doc(rowid)
	if doc == nil {
		return nil, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, in := range aq.t.phraseInstances(ph, doc) {
		if !seen[in.Col] {
			seen[in.Col] = true
			out = append(out, in.Col)
		}
	}
	sort.Ints(out)
	return out, nil
}

// AuxDataInt reads one integer auxdata slot (xGetAuxdataInt with clear=0).
// The slots are statement-scoped and keyed by the registered function name,
// mirroring C's per-function-instance auxdata.
func (aq *AuxQuery) AuxDataInt(name string) int64 {
	if aq.auxdata == nil {
		return 0
	}
	return aq.auxdata[name]
}

// AuxDataSetInt stores one integer auxdata slot (xSetAuxdataInt).
func (aq *AuxQuery) AuxDataSetInt(name string, v int64) {
	if aq.auxdata == nil {
		aq.auxdata = make(map[string]int64)
	}
	aq.auxdata[name] = v
}

// TestAll renders fts5_common.tcl's fts5_test_all as the flattened text the
// harness compares: "columnsize ... columntext ... columntotalsize ...
// poslist ... tokenize ... rowcount N", with empty lists rendered "{}".
func (aq *AuxQuery) TestAll(rowid int64) (string, error) {
	nCol := len(aq.t.cfg.Columns)
	colsize := make([]string, nCol)
	for i := 0; i < nCol; i++ {
		n, err := aq.ColumnSize(rowid, i)
		if err != nil {
			return "", err
		}
		colsize[i] = strconv.FormatInt(n, 10)
	}
	columntext := make([]string, nCol)
	for i := 0; i < nCol; i++ {
		text, err := aq.ColumnText(rowid, i)
		if err != nil {
			return "", err
		}
		columntext[i] = text
	}
	totalsize := make([]string, nCol)
	for i := 0; i < nCol; i++ {
		n, err := aq.ColumnTotalSize(i)
		if err != nil {
			return "", err
		}
		totalsize[i] = strconv.FormatInt(n, 10)
	}
	insts := aq.RowInstances(rowid)
	poslist := make([]string, len(insts))
	for i, in := range insts {
		poslist[i] = fmt.Sprintf("%d.%d.%d", in.Phrase, in.Col, in.Offset)
	}
	var tokenize []string
	for i := 0; i < nCol; i++ {
		text, err := aq.ColumnText(rowid, i)
		if err != nil {
			return "", err
		}
		tokenize = append(tokenize, aq.TokenizeText(text)...)
	}
	return "columnsize " + flatOrEmpty(colsize) +
		" columntext " + flatOrEmpty(columntext) +
		" columntotalsize " + flatOrEmpty(totalsize) +
		" poslist " + flatOrEmpty(poslist) +
		" tokenize " + flatOrEmpty(tokenize) +
		" rowcount " + strconv.FormatInt(aq.RowCount(), 10), nil
}

// flatOrEmpty space-joins a flattened list, rendering the empty list as
// "{}" (TCL's empty-list rendering).
func flatOrEmpty(elems []string) string {
	if len(elems) == 0 {
		return "{}"
	}
	return strings.Join(elems, " ")
}
