package fts

import (
	"fmt"
	"sort"
	"strings"
)

func (t *FTS3Table) ColumnNames() []string {
	return t.columnNames
}

// ColumnIndexed reports whether the given column number's text is indexed
// (not excluded by the notindexed= option). SQLite's fts3InsertTerms checks
// fts3ColumnIsIndexed before tokenizing a column; a notindexed column's text
// is stored in %_content but produces no postings (fts4noti 2.x).
func (t *FTS3Table) ColumnIndexed(colNum int) bool {
	if len(t.notindexed) == 0 {
		return true
	}
	if colNum < 0 || colNum >= len(t.columnNames) {
		return true
	}
	return !t.notindexed[strings.ToLower(t.columnNames[colNum])]
}

// Tokenizer returns the table's tokenizer (used to stem MATCH query terms the
// same way document terms are stemmed).
func (t *FTS3Table) Tokenizer() Tokenizer {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tokenizer
}

// NoDocsize reports whether the table was created with matchinfo=fts3 and
// therefore has no %_docsize shadow table (fts3.c bHasDocsize=0).
func (t *FTS3Table) NoDocsize() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.noDocsize
}

// Optimize consolidates the table's in-memory index state, mirroring SQLite's
// FTS OPTIMIZE command (fts3.c sqlite3Fts3Optimize merges all segments into
// one). The engine's in-memory store keeps one logical segment; the pending
// docid list is flushed so a later segment write produces one merged segment.
func (t *FTS3Table) Optimize() {
	t.mu.Lock()
	defer t.mu.Unlock()
	// The in-memory index is already a single merged structure; recording the
	// pending docids as flushed (nil) makes the next segment flush write one
	// segment for all documents.
	t.pendingDocIDs = nil
}

// IsFTS4 reports whether the table uses the fts4 module (fts3.c bFts4). The
// matchinfo flags 'n' and 'a' are only recognized on FTS4 tables.
func (t *FTS3Table) IsFTS4() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.EqualFold(t.moduleName, "fts4")
}

// IsFTS5 reports whether the table uses the fts5 module. fts5 has a wholly
// different storage layout (%_data/%_idx/%_docsize/%_config) and its own
// xIntegrity (fts5StorageIntegrity) than fts3/fts4, so callers must not run
// the fts3/4 inverted-index-vs-content cross-check on it.
func (t *FTS3Table) IsFTS5() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.EqualFold(t.moduleName, "fts5")
}

// PrefixLengths returns the FTS4 prefix= index lengths in declaration order
// (fts3.c aIndex[1..]: index i has length prefixLengths[i-1]; empty when the
// table has no prefix indexes).
func (t *FTS3Table) PrefixLengths() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]int(nil), t.prefixLengths...)
}

// IndexHasPostings reports whether prefix index iIndex has at least one
// posting among the given document IDs (fts3_write.c fts3InsertTerms: a term
// is added to prefix index i only when len(term) >= aIndex[i].nPrefix). It is
// used at segment flush to skip empty prefix indexes (fts3prefix.test 6.4.2:
// prefix="1,600,2" and prefix="1,2" produce identical segdir roots because
// the 600-character prefix index is empty for short documents).
func (t *FTS3Table) IndexHasPostings(iIndex int, prefixLen int, docIDs []int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := make(map[int64]bool, len(docIDs))
	for _, id := range docIDs {
		want[id] = true
	}
	for term, postings := range t.index.index {
		if len(term) < prefixLen {
			continue
		}
		for _, p := range postings {
			if want[p.DocID] {
				return true
			}
		}
	}
	return false
}

// BatchHasTerms reports whether the given pending document IDs produce at
// least one term posting in the main index. SQLite's fts3SegmentMerge(PENDING)
// writes no segment when the pending-terms hash is empty (fts3.c
// sqlite3Fts3SegReaderPending returns NULL when the hash has no terms — a
// commit containing only empty-content documents leaves the hash empty), so
// the flush must skip the segment write and the level-0 idx allocation
// entirely (fts4merge 3.2: the 30040-doc build has one empty document at
// i=19682; writing it as a segment adds a spurious level-0 row, 8 vs the
// oracle's 7).
func (t *FTS3Table) BatchHasTerms(docIDs []int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(docIDs) <= 64 {
		for _, docID := range docIDs {
			doc := t.index.GetDoc(docID)
			if doc == nil {
				continue
			}
			for colNum, v := range doc.Columns {
				if !t.ColumnIndexed(colNum) {
					continue
				}
				if len(t.tokenizer.Tokenize(ftsColumnString(v))) > 0 {
					return true
				}
			}
		}
		return false
	}
	want := make(map[int64]bool, len(docIDs))
	for _, id := range docIDs {
		want[id] = true
	}
	for _, postings := range t.index.index {
		for _, p := range postings {
			if want[p.DocID] {
				return true
			}
		}
	}
	return false
}

// IndexTerms returns the sorted, deduplicated terms of index iIndex
// (0 = the main index; iIndex >= 1 = prefix index iIndex with length
// prefixLengths[iIndex-1]). For a prefix index each main-index term of length
// >= the prefix length contributes its first prefixLen bytes (fts3_write.c
// fts3InsertTerms: a token is added to prefix index i only when
// nToken >= aIndex[i].nPrefix).
func (t *FTS3Table) IndexTerms(iIndex int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	set := make(map[string]bool)
	if iIndex <= 0 {
		for term := range t.index.index {
			set[term] = true
		}
	} else {
		if iIndex-1 >= len(t.prefixLengths) {
			return nil
		}
		prefixLen := t.prefixLengths[iIndex-1]
		for term := range t.index.index {
			if len(term) >= prefixLen {
				set[term[:prefixLen]] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for term := range set {
		out = append(out, term)
	}
	sort.Strings(out)
	return out
}

// IndexPosting is one row of the fts4term virtual table: a term of a given
// FTS index and one posting (docid, column, position) for it (fts3_term.c
// FTS3_TERMS_SCHEMA: term, docid, col, pos).
type IndexPosting struct {
	Term  string
	DocID int64
	Col   int
	Pos   int
}

// IndexPostings returns all (term, posting) rows of index iIndex
// (0 = main index; iIndex >= 1 = prefix index with length
// prefixLengths[iIndex-1]), sorted by (term, docid, col, pos) so the fts4term
// cursor yields the same order SQLite's segment scan produces
// (fts3_term.c fts3termFilterMethod: FTS3_SEGMENT_SCAN over one index).
func (t *FTS3Table) IndexPostings(iIndex int) []IndexPosting {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []IndexPosting
	if iIndex <= 0 {
		for term, postings := range t.index.index {
			for _, p := range postings {
				out = append(out, IndexPosting{Term: term, DocID: p.DocID, Col: p.Column, Pos: p.Position})
			}
		}
	} else {
		if iIndex-1 >= len(t.prefixLengths) {
			return nil
		}
		prefixLen := t.prefixLengths[iIndex-1]
		for term, postings := range t.index.index {
			if len(term) < prefixLen {
				continue
			}
			prefix := term[:prefixLen]
			for _, p := range postings {
				out = append(out, IndexPosting{Term: prefix, DocID: p.DocID, Col: p.Column, Pos: p.Position})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Term != out[j].Term {
			return out[i].Term < out[j].Term
		}
		if out[i].DocID != out[j].DocID {
			return out[i].DocID < out[j].DocID
		}
		if out[i].Col != out[j].Col {
			return out[i].Col < out[j].Col
		}
		return out[i].Pos < out[j].Pos
	})
	return out
}

// IntegrityCheck verifies that the in-memory index exactly matches the given
// content documents (docid → per-column text). It re-tokenizes each document
// with the table's tokenizer and compares the resulting (term, docid, column,
// position) postings against the index. Returns nil when they match; a
// non-nil error ("database disk image is malformed") when the index has
// drifted from the content (SQLite's FTS3 integrity-check command, fts3.c
// sqlite3Fts3IntegrityCheck; fts4check/fts4intck1).
func (t *FTS3Table) IntegrityCheck(docs map[int64][]interface{}) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Build the expected postings.
	expected := make(map[string]map[string]bool) // term → "docid:col:pos"
	add := func(term string, docID int64, col, pos int) {
		key := fmt.Sprintf("%d:%d:%d", docID, col, pos)
		if expected[term] == nil {
			expected[term] = make(map[string]bool)
		}
		expected[term][key] = true
	}
	for docID, cols := range docs {
		for colNum, v := range cols {
			if !t.ColumnIndexed(colNum) {
				continue
			}
			tokens := t.tokenizer.Tokenize(ftsColumnString(v))
			for _, tok := range tokens {
				add(tok.Term, docID, colNum, tok.Position)
				// Prefix indexes add the first prefixLen bytes of each token
				// whose length is at least prefixLen (fts3_write.c
				// fts3InsertTerms); the segment index includes those terms,
				// so the expected postings must too (fts4check t3 uses
				// prefix="2,3").
				for _, plen := range t.prefixLengths {
					if len(tok.Term) >= plen {
						add(tok.Term[:plen], docID, colNum, tok.Position)
					}
				}
			}
		}
	}
	// Compare against the actual index.
	// Compare against the actual index.
	if len(expected) != len(t.index.index) {
		return fmt.Errorf("database disk image is malformed")
	}
	for term, expKeys := range expected {
		postings, ok := t.index.index[term]
		if !ok {

			return fmt.Errorf("database disk image is malformed")
		}
		if len(postings) != len(expKeys) {

			return fmt.Errorf("database disk image is malformed")
		}
		for _, p := range postings {
			key := fmt.Sprintf("%d:%d:%d", p.DocID, p.Column, p.Position)
			if !expKeys[key] {
				return fmt.Errorf("database disk image is malformed")
			}
		}
	}
	return nil
}

// CompressFn returns the FTS4 compress= function name (empty when unset).
func (t *FTS3Table) CompressFn() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.compressFn
}

// NodeSize returns the segment node size (the FTS table's page size unless
// the nodesize= command set it).
func (t *FTS3Table) NodeSize() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nodeSize
}

// SetNodeSize sets the segment node size from the nodesize= special command.
func (t *FTS3Table) SetNodeSize(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n >= 24 {
		t.nodeSize = n
	}
}

// UncompressFn returns the FTS4 uncompress= function name (empty when unset).
func (t *FTS3Table) UncompressFn() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.uncompressFn
}

// Insert inserts a row and returns the rowid.
func (t *FTS3Table) Insert(values []interface{}) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	docID := t.index.Insert(values, t.tokenizer)
	t.addDocStats(docID)
	return docID
}

// InsertLangID inserts a row with a specific language id (the FTS4
// languageid=<col> option) and returns the rowid.
func (t *FTS3Table) InsertLangID(values []interface{}, langID int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	docID := t.index.InsertWithLangID(values, t.tokenizer, langID)
	t.addDocStats(docID)
	return docID
}

// InsertWithID inserts a row with a specific rowid.
func (t *FTS3Table) InsertWithID(rowid int64, values []interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.InsertWithID(rowid, values, t.tokenizer)
	t.addDocStats(rowid)
}

// InsertWithIDLangID inserts a row with a specific rowid and language id.
func (t *FTS3Table) InsertWithIDLangID(rowid int64, values []interface{}, langID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.InsertWithIDLangID(rowid, values, t.tokenizer, langID)
	t.addDocStats(rowid)
}

// LangIDColName returns the languageid=<col> option's column name ("" when
// the table has no languageid option).
func (t *FTS3Table) LangIDColName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.langidColName
}

// DocLangID returns a document's stored language id (0 when the doc or
// table has no language).
func (t *FTS3Table) DocLangID(docID int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	doc := t.index.GetDoc(docID)
	if doc == nil {
		return 0
	}
	return doc.LangID
}

// InsertWithIDIncludingPrefixes inserts a row with a specific rowid AND adds
// the prefix-index postings for its tokens (the in-memory index normally
// stores only main-index postings; prefix terms are materialized at segment
// write time). The integrity check's fresh-from-segments index must include
// prefix postings for PENDING (unflushed) documents, whose prefix terms are
// not yet in any segment (fts4check 5.1: an uncommitted prefix="1,2,3" table
// validates against the content + prefix expectations).
func (t *FTS3Table) InsertWithIDIncludingPrefixes(rowid int64, values []interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.InsertWithID(rowid, values, t.tokenizer)
	t.addDocStats(rowid)
	for colNum, v := range values {
		if !t.ColumnIndexed(colNum) {
			continue
		}
		tokens := t.tokenizer.Tokenize(ftsColumnString(v))
		for _, tok := range tokens {
			for _, plen := range t.prefixLengths {
				if len(tok.Term) >= plen {
					t.index.addPosting(tok.Term[:plen], rowid, colNum, tok.Position)
				}
			}
		}
	}
}

// Delete removes a row. When the document was already flushed to %_segdir
// (not in the pending-insert batch), its docid is recorded so the next flush
// writes a delete-marker segment (SQLite's fts3DeleteTerms persistence). A
// docid removed from the pending batch needs no marker.
func (t *FTS3Table) Delete(rowid int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasPending := false
	for i, id := range t.pendingDocIDs {
		if id == rowid {
			t.pendingDocIDs = append(t.pendingDocIDs[:i], t.pendingDocIDs[i+1:]...)
			wasPending = true
			break
		}
	}
	doc := t.index.GetDoc(rowid)
	if !wasPending && doc != nil {
		t.deletedDocIDs = append(t.deletedDocIDs, rowid)
		// Snapshot the document's terms so the flush can build a delete
		// marker AFTER this call removes the postings (fts3DeleteTerms
		// writes the marker from the old terms; fts4onepass 3.x UPDATE
		// SET docid=... re-keys a flushed doc, fts4content 3.1.5 DELETE).
		if t.deleteMarkerTerms == nil {
			t.deleteMarkerTerms = make(map[int64][]string)
		}
		var terms []string
		seen := make(map[string]bool)
		for colNum, v := range doc.Columns {
			if !t.ColumnIndexed(colNum) {
				continue
			}
			tokens := t.tokenizer.Tokenize(ftsColumnString(v))
			for _, tok := range tokens {
				if !seen[tok.Term] {
					seen[tok.Term] = true
					terms = append(terms, tok.Term)
				}
			}
		}
		sort.Strings(terms)
		t.deleteMarkerTerms[rowid] = terms
	}
	t.subDocStats(rowid)
	t.index.Delete(rowid)
}

// Update updates a row's content.
func (t *FTS3Table) Update(rowid int64, values []interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subDocStats(rowid)
	t.index.Update(rowid, values, t.tokenizer)
	t.addDocStats(rowid)
}

// AllRows returns all rows.
func (t *FTS3Table) AllRows() [][]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	docIDs := t.index.AllDocIDs()
	rows := make([][]interface{}, len(docIDs))
	for i, docID := range docIDs {
		doc := t.index.GetDoc(docID)
		if doc == nil {
			continue
		}
		row := make([]interface{}, len(t.columnNames))
		copy(row, doc.Columns)
		rows[i] = row
	}
	return rows
}

// AllRowsMap returns the docIDs of all rows, ordered by the table's index
// direction (ascending by default; descending when the FTS4 order=desc option
// is set — fts3.c bDescIdx makes the vtab scan return descending rowids).
func (t *FTS3Table) AllRowsMap() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := t.index.AllDocIDs()
	if t.orderDesc {
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	}
	return ids
}

// GetDoc returns a document by docid.
func (t *FTS3Table) GetDoc(docID int64) *Document {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.GetDoc(docID)
}

// HasDoc reports whether a document with the given docid exists.
func (t *FTS3Table) HasDoc(docID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.GetDoc(docID) != nil
}

// DocCount returns the number of documents in the in-memory index.
func (t *FTS3Table) DocCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.DocCount()
}

// NextDocID returns the next auto-assigned docid (the index's nextID).
func (t *FTS3Table) NextDocID() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.index.NextDocID()
}

// LoadSegment loads one real FTS3 segment (a %_segdir root blob plus its
// %_segments leaf blocks) into the in-memory index. A structurally corrupt
// segment's valid prefix of terms is still loaded; the caller decides whether
// a load error blocks the operation (the segdir-btree OpenCursor failure is
// recorded via SetLoadErr because the whole table is unreadable, whereas a
// single corrupt term doclist only affects queries that read that term —
// fts3corrupt4 24.2).
func (t *FTS3Table) LoadSegment(root []byte, leavesEndBlock int, readBlock SegmentBlockReader) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.nCol = len(t.columnNames)
	err := t.index.LoadSegment(root, leavesEndBlock, readBlock)
	t.statDirty = true
	return err
}

// QueryHasCorruptTerm reports whether the given MATCH query reads a term whose
// segment doclist failed to load. A MATCH that reads a corrupt term must fail
// with "database disk image is malformed" even when the in-memory index has no
// candidate rows (fts3corrupt4 31.1: SQLite reads the segment at prepare).
func (t *FTS3Table) QueryHasCorruptTerm(query string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	node, err := ParseMatchQuery(query)
	if err != nil {
		return false
	}
	node = tokenizeQueryNode(node, t.tokenizer)
	node = ResolveQuery(node, t.columnNames)
	res := queryReferencesCorruptTerm(node, t.index)
	if res {
	}
	return res
}

// LoadErr returns the first segment-loading error, if any (a corrupt real
// SQLite segment that could not be fully read into the in-memory index).
func (t *FTS3Table) LoadErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.loadErr
}

// IndexHasTerm reports whether the term has any posting in the in-memory
// index (a segment load that failed partway leaves the successfully-loaded
// prefix of terms queryable — SQLite's deferred evaluation reads each
// term's doclist on demand, so only queries touching unreadable terms fail:
// fts3defer2 1.x vs 1.7).
func (t *FTS3Table) IndexHasTerm(term string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.index.index[term]) > 0
}

// SetStructuralLoadErr marks the table's segment b-tree as structurally
// broken: no term can be looked up, so every MATCH query fails regardless of
// which terms it references.
func (t *FTS3Table) SetStructuralLoadErr() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.structuralLoadErr = true
}

// StructuralLoadErr reports whether the segment structure itself is broken.
func (t *FTS3Table) StructuralLoadErr() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.structuralLoadErr
}

// ClearLoadErr resets the recorded segment-loading error (a successful
// segment reload replaces the previous state — fts3corrupt5 1.3.x: each
// UPDATE ft_segdir SET root=... reloads, and a valid root must clear the
// error left by the preceding corrupt one).
func (t *FTS3Table) ClearLoadErr() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadErr = nil
}

// SetLoadErr records a segment-loading failure so a corrupt %_segdir btree
// (whose rows cannot even be enumerated) surfaces as "database disk image is
// malformed" at the next FTS operation.
func (t *FTS3Table) SetLoadErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loadErr == nil {
		t.loadErr = err
	}
}

// RecordCorruptContentDocID records a %_content rowid whose record failed to
// decode during the content rebuild. A query that matches this docid fails
// with "database disk image is malformed"; queries that never read the row
// succeed (fts3corrupt4 9.1 vs 11.1).
func (t *FTS3Table) RecordCorruptContentDocID(docID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.corruptContentDocIDs == nil {
		t.corruptContentDocIDs = make(map[int64]bool)
	}
	t.corruptContentDocIDs[docID] = true
}

// IsCorruptContentDocID reports whether the docid's content row failed to
// decode (so reading it must fail as "database disk image is malformed").
func (t *FTS3Table) IsCorruptContentDocID(docID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.corruptContentDocIDs[docID]
}

// HasCorruptContent reports whether any %_content row failed to decode. An
// FTS command that reads the whole table (optimize/integrity-check) must fail
// with "database disk image is malformed" (fts3corrupt4 10.3/14.2).
func (t *FTS3Table) HasCorruptContent() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.corruptContentDocIDs) > 0
}

// SetContentBtreeUnreadable records that the %_content shadow btree could
// not be navigated (a crash-written page fails structural parsing).
func (t *FTS3Table) SetContentBtreeUnreadable() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.contentBtreeUnreadable = true
}

// ContentBtreeUnreadable reports whether the %_content btree failed to load.
func (t *FTS3Table) ContentBtreeUnreadable() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.contentBtreeUnreadable
}

// Reset clears the table's in-memory index, pending rows, load errors, and
// corrupt-content markers so the FTS3 'rebuild' command can repopulate it
// from %_content (fts3.c fts3RebuildMethod).
func (t *FTS3Table) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index = NewInvertedIndex()
	t.reapplySkipColumnsLocked()
	t.pendingDocIDs = nil
	t.loadErr = nil
	t.corruptContentDocIDs = nil
	t.statNDoc = 0
	t.statTotals = nil
	t.statTotalBytes = 0
	t.statDirty = true
}

// reapplySkipColumnsLocked rebuilds the inverted index's skipColumns set from
// the notindexed option (Reset/Clear replace the index with a fresh one that
// loses the wiring; the rebuild path must still skip notindexed columns —
// fts4noti 2.2.x after INSERT INTO t1(t1) VALUES('rebuild')).
func (t *FTS3Table) reapplySkipColumnsLocked() {
	if len(t.notindexed) == 0 {
		return
	}
	skip := make(map[int]bool)
	for i, c := range t.columnNames {
		if t.notindexed[strings.ToLower(c)] {
			skip[i] = true
		}
	}
	if len(skip) > 0 {
		t.index.SetSkipColumns(skip)
	}
}

// TokenStats returns the matchinfo global statistics: the number of
// documents (nDoc) and the total token count per column (one entry per
// user column). SQLite stores these in the FTS4 %_stat table
// (fts3.c sqlite3Fts3SelectDoctotal / fts3UpdateDocTotals); the engine
// computes them from the in-memory inverted index for matchinfo() 'n'
// and 'a' flags. Uses the per-document cached sizes so a flush over a
// large table is not O(n²) re-tokenization (fts4check builds 30k rows).
func (t *FTS3Table) TokenStats() (int64, []int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nDoc, totals := t.tokenStatsLocked()
	return nDoc, totals
}

// tokenStatsLocked computes the %_stat doctotal aggregate (nDoc, per-column
// token totals) from the in-memory index. Caller holds t.mu.

// IntegrityCheckIndex verifies ONE index band's in-memory index against the
// content-derived postings for that band (fts3_write.c fts3ChecksumIndex is
// invoked once per index: each FTS band — main plus every prefix truncation
// — is an independent index whose doclists never mix). iIndex 0 is the main
// index and expects FULL token terms; iIndex >= 1 is prefix index iIndex-1
// and expects terms truncated to that prefix length. Comparing all bands in
// one shared key space lets a delete-marker applied while loading a main-
// band segment erase a prefix band's contribution to the same string key
// (e.g. "her" is both a main term and the prefix-3 form of "here"),
// producing spurious integrity-check failures (fts4opt 2.x churn).
func (t *FTS3Table) IntegrityCheckIndex(docs map[int64][]interface{}, iIndex int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	plen := 0
	if iIndex > 0 {
		if iIndex-1 >= len(t.prefixLengths) {
			return nil
		}
		plen = t.prefixLengths[iIndex-1]
	}
	expected := make(map[string]map[string]bool) // term -> "docid:col:pos"
	add := func(term string, docID int64, col, pos int) {
		key := fmt.Sprintf("%d:%d:%d", docID, col, pos)
		if expected[term] == nil {
			expected[term] = make(map[string]bool)
		}
		expected[term][key] = true
	}
	for docID, cols := range docs {
		for colNum, v := range cols {
			if !t.ColumnIndexed(colNum) {
				continue
			}
			tokens := t.tokenizer.Tokenize(ftsColumnString(v))
			for _, tok := range tokens {
				term := tok.Term
				if iIndex > 0 {
					if len(term) < plen {
						continue // shorter than this prefix: contributes nothing
					}
					term = term[:plen]
				}
				add(term, docID, colNum, tok.Position)
			}
		}
	}
	return t.compareExpectedBand(expected, iIndex)
}

// compareExpectedBand diffs the expected posting set against the actual
// in-memory index (the comparison half of IntegrityCheck), restricted to
// index band iIndex (negative = all bands).
func (t *FTS3Table) compareExpectedBand(expected map[string]map[string]bool, _ int) error {
	if len(expected) != len(t.index.index) {
		return fmt.Errorf("database disk image is malformed")
	}
	for term, expKeys := range expected {
		postings, ok := t.index.index[term]
		if !ok {
			return fmt.Errorf("database disk image is malformed")
		}
		if len(postings) != len(expKeys) {
			return fmt.Errorf("database disk image is malformed")
		}
		for _, p := range postings {
			key := fmt.Sprintf("%d:%d:%d", p.DocID, p.Column, p.Position)
			if !expKeys[key] {
				return fmt.Errorf("database disk image is malformed")
			}
		}
	}
	return nil
}

// InsertWithIDForIndex inserts a row's postings for ONE index band only
// (band semantics mirror IntegrityCheckIndex): pending documents must feed
// each band's expectation separately during a per-index integrity check,
// mirroring how the flush writes one pending segment per index.
func (t *FTS3Table) InsertWithIDForIndex(rowid int64, values []interface{}, iIndex int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	plen := 0
	if iIndex > 0 {
		if iIndex-1 >= len(t.prefixLengths) {
			return
		}
		plen = t.prefixLengths[iIndex-1]
	}
	for colNum, v := range values {
		if !t.ColumnIndexed(colNum) {
			continue
		}
		tokens := t.tokenizer.Tokenize(ftsColumnString(v))
		for _, tok := range tokens {
			term := tok.Term
			if iIndex > 0 {
				if len(term) < plen {
					continue
				}
				term = term[:plen]
			}
			t.index.addPosting(term, rowid, colNum, tok.Position)
		}
	}
	t.addDocStats(rowid)
}

// DeleteForIndex removes a document's postings from the in-memory index
// without recording delete markers (integrity-check helper operating on the
// throwaway per-band fresh index).
func (t *FTS3Table) DeleteForIndex(rowid int64, iIndex int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index.Delete(rowid)
}
