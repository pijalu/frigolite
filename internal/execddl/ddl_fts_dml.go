// Package exec implements query execution.
//
// This file holds FTS virtual-table DML helpers extracted from ddl_core.go so
// each file stays within the repository's complexity budgets: the rowid
// equality fast-path predicate, the WHERE-less DELETE clear-all path, the
// per-document delete step, and the per-row UPDATE match decision.
package execddl

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
)

// ftsRowIDRef reports whether e is an unqualified rowid/docid column
// reference.
func ftsRowIDRef(e sql.Expr) bool {
	ref, ok := e.(*sql.ColumnRef)
	if !ok || ref.Table != "" {
		return false
	}
	return execquery.IsRowIDName(ref.Name) || strings.EqualFold(ref.Name, "docid")
}

// ftsConstIntLit evaluates a numeric literal (a unary-minus literal allowed)
// as an int64; a trailing ".0" decimal suffix is tolerated.
func ftsConstIntLit(e sql.Expr) (int64, bool) {
	if n, ok := e.(*sql.NumericLit); ok {
		return ftsParseIntLit(n.Value)
	}
	if u, ok := e.(*sql.UnaryOp); ok && u.Operator == "-" {
		if n, ok := u.Operand.(*sql.NumericLit); ok {
			if iv, ok := ftsParseIntLit(n.Value); ok {
				return -iv, true
			}
		}
	}
	return 0, false
}

// ftsParseIntLit parses a numeric literal's text as an int64.
func ftsParseIntLit(value string) (int64, bool) {
	iv, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(value, ".0")), 10, 64)
	return iv, err == nil
}

// ftsDeleteAllHandled handles the WHERE-less DELETE FROM <fts> (no
// WHERE/LIMIT/ORDER BY) by clearing the whole table at once — SQLite's
// fts3DeleteAll drops the segment directory and resets the in-memory index
// instead of deleting each document's postings one by one (per-doc removal is
// O(total postings), making the automerge test's between-scenario cleanup
// ~seconds for 500 40KB documents). For a content=<table> table SQLite does
// NOT use fts3DeleteAll: the DELETE goes through the per-doc xUpdate path,
// which skips docids whose content row is missing (the delete terms cannot be
// computed) — so a docid inserted into the index without a content row
// survives (fts4content 3.1.4: DELETE FROM ft3 leaves docid 21 MATCH-able).
// Reports whether the statement is complete.
func (e *DDLExecutor) ftsDeleteAllHandled(tableName string, ftsTable *fts.FTS3Table, s *sql.DeleteStmt) (bool, *Result) {
	if s.Where != nil || s.Limit != nil || len(s.OrderBy) > 0 {
		return false, nil
	}
	if ftsTable.ContentTable() != "" || ftsTable.Contentless() {
		return false, nil
	}
	changed := ftsTable.DocCount()
	ftsTable.Clear()
	e.clearFTSShadowIndex(tableName)
	e.clearFTSContent(tableName)
	e.writeFTSStat(tableName, ftsTable)
	return true, &Result{Changes: int64(changed)}
}

// deleteFTSDoc performs one matched document's delete: the content-row
// presence gate for content=<table> tables, the pending-batch flush, index
// removal and shadow-row cleanup. Reports whether the document was deleted.
func (e *DDLExecutor) deleteFTSDoc(tableName string, ftsTable *fts.FTS3Table, docID int64) (bool, *Result) {
	// An FTS4 content=<table> table's xDelete computes the delete terms
	// from the content row; a docid whose content row is missing cannot
	// be deleted and is skipped (fts3.c fts3DeleteTerms; fts4content
	// 3.1.4: DELETE FROM ft3 leaves docid 21 indexed until its content
	// row exists).
	if ct := ftsTable.ContentTable(); ct != "" && !e.ctx.ContentRowExists(ct, docID) {
		return false, nil
	}
	// C's fts3DeleteTerms reports bFound from the %_content row; only a
	// found row can leave the table empty below.
	hadContent := e.ctx.ContentRowExists(tableName+"_content", docID)
	// C's fts3PendingTermsDocid (bDelete=1) runs per deleted document: a
	// docid that restarts the pending sequence flushes the pending batch
	// BEFORE this document's delete terms pend (fts4onepass-4.0).
	if ftsTable.PendingDocidRestart(docID, true, ftsTable.DocLangID(docID)) && ftsTable.HasPendingOps() {
		if res := e.flushFTSPendingFlagged(tableName); res != nil {
			return false, res
		}
	}
	ftsTable.Delete(docID)
	// Remove the document row from the %_content shadow table so SELECT
	// FROM <name>_content reflects the deletion (fts3comp1 1.9: DELETE
	// FROM t1 WHERE docid=1 leaves only docids 3 and 4).
	e.deleteFTSContentRow(tableName, docID)
	e.deleteFTSDocsizeRow(tableName, docID)
	// fts3DeleteByRowid: a found row whose deletion empties the whole table
	// triggers fts3DeleteAll — the pending-terms hash is discarded and every
	// shadow table wiped — so the delete-marker flush writes nothing and the
	// next INSERT starts a fresh (level 0, idx 0) segment (fts3d 1.segments:
	// DELETE FROM t1 WHERE 1=1 leaves %_segdir empty; the re-INSERT lands at
	// idx 0, not idx 4).
	if hadContent && !e.ftsContentTableHasRows(tableName, ftsTable) {
		ftsTable.Clear()
		e.clearFTSShadowIndex(tableName)
		e.clearFTSContent(tableName)
	}
	return true, nil
}

// ftsContentTableHasRows reports whether the effective %_content table still
// holds any document row — fts3_write.c SQL_IS_EMPTY,
// "SELECT NOT EXISTS(SELECT docid FROM %Q.'%q_content' WHERE rowid!=?)", run
// by fts3DeleteByRowid after the deleted document's own entry is removed.
func (e *DDLExecutor) ftsContentTableHasRows(tableName string, ftsTable *fts.FTS3Table) bool {
	name := tableName + "_content"
	if ct := ftsTable.ContentTable(); ct != "" {
		name = ct
	}
	entry, _, err := e.ctx.FindTable(name)
	if err != nil || entry == nil || entry.RootPage == 0 {
		return false
	}
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	lastID, lerr := tree.LastRowID()
	return lerr == nil && lastID > 0
}

// ftsUpdateRowMatched decides whether one document participates in an FTS
// UPDATE: the WHERE clause must match (evaluated against the joined row map
// for a FROM-clause update) and, for a content=<table> table, the content row
// must exist. A non-nil error fails the whole statement.
func (e *DDLExecutor) ftsUpdateRowMatched(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt, docID int64) (bool, error) {
	if s.Where != nil {
		rowMap, matchedRow, jerr := e.ftsUpdateJoinedRowMap(ftsTable, colDefs, s, docID)
		if jerr != nil {
			// A missing FROM table fails the whole statement (fts4upfrom
			// 1.x: UPDATE ft SET c=v FROM changes → "no such table:
			// changes").
			return false, jerr
		}
		if !matchedRow {
			return false, nil
		}
		match, err := e.ctx.EvalBool(s.Where, rowMap)
		if err != nil || !match {
			return false, nil
		}
	}
	// A content=<table> table's xUpdate computes the delete terms from
	// the content row; a docid whose content row is missing cannot be
	// updated and is skipped (fts3.c fts3DeleteTerms; fts4content 3.1.4
	// / 3.3.x).
	if ct := ftsTable.ContentTable(); ct != "" && !e.ctx.ContentRowExists(ct, docID) {
		return false, nil
	}
	return true, nil
}
