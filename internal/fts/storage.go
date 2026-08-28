package fts

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Posting represents a single occurrence of a term in a document.
type Posting struct {
	DocID    int64
	Column   int
	Position int
	// Delete marks a position-less delete-marker entry (fts3DeleteTerms):
	// buildDoclist emits [docid][posEnd] for it. Column is -1 so
	// sortPostings places it before the same document's real postings.
	Delete bool
}

// Document stores the original content for an FTS row. Column values are
// interface{} so NULL (nil) is preserved distinct from an empty string
// (fts3DeclareVtab stores NULL columns verbatim; SELECT col IS NULL works).
type Document struct {
	DocID   int64
	Columns []interface{}
	// LangID is the document's language id (the FTS4 languageid=<col>
	// option). 0 is the default language. The hidden langid column exposes
	// it; MATCH with a lang_id constraint filters to that language.
	LangID int64
}

// InvertedIndex is an in-memory inverted index for FTS.
type InvertedIndex struct {
	index  map[string][]Posting // term -> postings
	docs   map[int64]*Document  // docid -> document
	nextID int64
	// nCol is the table's column count, used by the doclist loader to detect
	// column markers beyond the table (0 disables the check).
	nCol int
	// auxCorrupt marks a loaded segment carrying an out-of-range column
	// marker: ordinary reads skip it, fts4aux fails (fts3corrupt7 4.4).
	auxCorrupt bool
	// docSizes caches each document's per-column token counts and total text
	// bytes, computed once at insert (the FTS4 %_stat/%_docsize writers call
	// DocSize/TokenStats at every segment flush; re-tokenizing every document
	// per flush is O(n²) for large tables — fts4check builds 30k rows).
	docSizes map[int64]*DocSizeInfo
	// skipColumns holds column numbers whose text is NOT indexed (the FTS4
	// notindexed=<col> option). Those columns' values are stored in the
	// Document but produce no postings (fts3.c fts3InsertTerms checks
	// fts3ColumnIsIndexed before tokenizing; fts4noti 2.x).
	skipColumns map[int]bool
	// corruptTerms records terms whose doclist failed to load from a real
	// SQLite segment (valid framing, corrupt content). A MATCH query that
	// reads one of these terms must fail with "database disk image is
	// malformed" (fts3corrupt4 11.1/19.1), while queries on other terms
	// succeed (fts3corrupt4 13.1/24.2).
	corruptTerms map[string]bool
}

// DocSizeInfo is a document's cached per-column token counts and total text
// bytes (fts3.c fts3PendingTermsAdd aSz[]).
type DocSizeInfo struct {
	Counts     []int
	TotalBytes int64
}

// NewInvertedIndex creates a new empty inverted index.
// SetColumnCount records the table's column count so the doclist loader can
// reject column markers beyond it (fts3corrupt7 4.x: a crafted column marker
// of 0x0FFFFFFF makes segment reads corrupt).
func (idx *InvertedIndex) SetColumnCount(n int) {
	idx.nCol = n
}

func NewInvertedIndex() *InvertedIndex {
	return &InvertedIndex{
		index:        make(map[string][]Posting),
		docs:         make(map[int64]*Document),
		docSizes:     make(map[int64]*DocSizeInfo),
		corruptTerms: make(map[string]bool),
		nextID:       1,
	}
}

// SetSkipColumns marks column numbers whose text is not indexed (the FTS4
// notindexed=<col> option). Called once at table creation.
func (idx *InvertedIndex) SetSkipColumns(cols map[int]bool) {
	idx.skipColumns = cols
}

// Snapshot returns a deep copy of the index state so a transaction,
// savepoint, or failed statement can restore it on rollback. The FTS store
// is in-memory, so the engine's pager snapshots (which cover only the btree
// pages) do not undo FTS changes; these copies give FTS the same
// rollback capability (fts3conf 4.x savepoint/statement atomicity).
func (idx *InvertedIndex) Snapshot() *InvertedIndex {
	s := &InvertedIndex{
		index:        make(map[string][]Posting, len(idx.index)),
		docs:         make(map[int64]*Document, len(idx.docs)),
		docSizes:     make(map[int64]*DocSizeInfo, len(idx.docSizes)),
		corruptTerms: make(map[string]bool, len(idx.corruptTerms)),
		nextID:       idx.nextID,
	}
	if idx.skipColumns != nil {
		s.skipColumns = make(map[int]bool, len(idx.skipColumns))
		for k, v := range idx.skipColumns {
			s.skipColumns[k] = v
		}
	}
	for term, postings := range idx.index {
		cp := make([]Posting, len(postings))
		copy(cp, postings)
		s.index[term] = cp
	}
	for id, doc := range idx.docs {
		dcp := &Document{DocID: doc.DocID, Columns: make([]interface{}, len(doc.Columns)), LangID: doc.LangID}
		copy(dcp.Columns, doc.Columns)
		s.docs[id] = dcp
	}
	for id, ds := range idx.docSizes {
		cp := &DocSizeInfo{Counts: make([]int, len(ds.Counts)), TotalBytes: ds.TotalBytes}
		copy(cp.Counts, ds.Counts)
		s.docSizes[id] = cp
	}
	for term := range idx.corruptTerms {
		s.corruptTerms[term] = true
	}
	return s
}

// Restore replaces the index contents with a snapshot taken earlier.
func (idx *InvertedIndex) Restore(s *InvertedIndex) {
	idx.index = s.index
	idx.docs = s.docs
	idx.docSizes = s.docSizes
	idx.nextID = s.nextID
}

// Insert adds a document to the index. Returns the assigned docid.
func (idx *InvertedIndex) Insert(columns []interface{}, tokenizer Tokenizer) int64 {
	docID := idx.nextID
	idx.nextID++
	idx.insertDoc(docID, columns, tokenizer, 0)
	return docID
}

// InsertWithID adds a document with a specific docid.
func (idx *InvertedIndex) InsertWithID(docID int64, columns []interface{}, tokenizer Tokenizer) {
	if docID >= idx.nextID {
		idx.nextID = docID + 1
	}
	idx.insertDoc(docID, columns, tokenizer, 0)
}

// InsertWithLangID adds a document with a specific language id (the FTS4
// languageid=<col> option).
func (idx *InvertedIndex) InsertWithLangID(columns []interface{}, tokenizer Tokenizer, langID int64) int64 {
	docID := idx.nextID
	idx.nextID++
	idx.insertDoc(docID, columns, tokenizer, langID)
	return docID
}

// InsertWithIDLangID adds a document with a specific docid and language id.
func (idx *InvertedIndex) InsertWithIDLangID(docID int64, columns []interface{}, tokenizer Tokenizer, langID int64) {
	if docID >= idx.nextID {
		idx.nextID = docID + 1
	}
	idx.insertDoc(docID, columns, tokenizer, langID)
}

// insertDoc stores a document in the index, its postings, and its cached
// per-column sizes.
func (idx *InvertedIndex) insertDoc(docID int64, columns []interface{}, tokenizer Tokenizer, langID int64) {
	doc := &Document{DocID: docID, Columns: make([]interface{}, len(columns)), LangID: langID}
	copy(doc.Columns, columns)
	// A language-aware tokenizer is bound to the document's language id
	// before tokenizing (fts3.c passes iLangid to xLanguageid per cursor —
	// fts4langid 4.x: the test tokenizer lowercases only for even langids).
	if la, ok := tokenizer.(LangidAware); ok {
		tokenizer = la.WithLangid(langID)
	}
	idx.docs[docID] = doc
	counts := make([]int, len(columns))
	var totalBytes int64
	for colNum, v := range columns {
		s := ftsColumnString(v)
		totalBytes += int64(len(s))
		if idx.skipColumns[colNum] {
			// A notindexed column contributes no tokens or postings; its
			// per-column token count is 0 (fts3.c fts3InsertTerms skips the
			// column before fts3PendingTermsAdd — aSz for the column stays
			// 0; the total BYTES still counts the stored text).
			counts[colNum] = 0
			continue
		}
		tokens := tokenizer.Tokenize(s)
		counts[colNum] = len(tokens)
		for _, tok := range tokens {
			key := tok.Term
			idx.index[key] = append(idx.index[key], Posting{
				DocID:    docID,
				Column:   colNum,
				Position: tok.Position,
			})
		}
	}
	idx.docSizes[docID] = &DocSizeInfo{Counts: counts, TotalBytes: totalBytes}
}

// ftsColumnString renders a stored FTS column value for tokenization: NULL
// becomes the empty string (no tokens); a []byte blob is tokenized as its raw
// bytes (SQLite tokenizes the blob's bytes directly — fts3snippet2.test inserts
// binary documents and MATCHes them); any other value uses its string form.
// The result is truncated at the first NUL byte: SQLite passes the column text
// to the tokenizer with length -1 (fts3_write.c fts3PendingTermsAdd:
// sqlite3Fts3OpenTokenizer(..., zText, -1, ...)), so the tokenizer uses strlen
// and the document ends at the first \x00 byte (fts3snippet2.test 3.1).
func ftsColumnString(v interface{}) string {
	if v == nil {
		return ""
	}
	var s string
	if b, ok := v.([]byte); ok {
		s = string(b)
	} else {
		s = fmt.Sprintf("%v", v)
	}
	if idx := strings.IndexByte(s, 0); idx >= 0 {
		s = s[:idx]
	}
	return s
}

// Delete removes a document from the index.
func (idx *InvertedIndex) Delete(docID int64) {
	delete(idx.docs, docID)
	delete(idx.docSizes, docID)
	for term, postings := range idx.index {
		filtered := postings[:0]
		for _, p := range postings {
			if p.DocID != docID {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			delete(idx.index, term)
		} else {
			idx.index[term] = filtered
		}
	}
}

// Update replaces a document's content in the index.
func (idx *InvertedIndex) Update(docID int64, columns []interface{}, tokenizer Tokenizer) {
	idx.Delete(docID)
	idx.InsertWithID(docID, columns, tokenizer)
}

// GetDoc returns a document by docid.
func (idx *InvertedIndex) GetDoc(docID int64) *Document {
	return idx.docs[docID]
}

// DocSizeInfo returns a document's cached per-column token counts and total
// text bytes (nil when the doc is absent).
func (idx *InvertedIndex) DocSizeInfo(docID int64) *DocSizeInfo {
	return idx.docSizes[docID]
}

// NextDocID returns the next auto-assigned docid (idx.nextID).
func (idx *InvertedIndex) NextDocID() int64 {
	return idx.nextID
}

// SearchTerm returns all docids that contain the given term.
func (idx *InvertedIndex) SearchTerm(term string) []int64 {
	postings := idx.index[term]
	if len(postings) == 0 {
		return nil
	}
	seen := make(map[int64]bool)
	var result []int64
	for _, p := range postings {
		if !seen[p.DocID] {
			seen[p.DocID] = true
			result = append(result, p.DocID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// SearchPhrase returns docids where all terms appear in sequence.
func (idx *InvertedIndex) SearchPhrase(terms []string) []int64 {
	if len(terms) == 0 {
		return nil
	}
	// Start with docids matching the first term
	candidates := idx.SearchTerm(terms[0])
	if len(candidates) == 0 {
		return nil
	}
	// For each candidate, check if all terms appear in sequence
	var result []int64
	for _, docID := range candidates {
		if idx.phraseInDoc(docID, terms, nil) {
			result = append(result, docID)
		}
	}
	return result
}

// phraseInDoc reports whether all terms appear in sequence in a document.
// prefixes (parallel to terms) marks prefix-wildcard terms.
func (idx *InvertedIndex) phraseInDoc(docID int64, terms []string, prefixes []bool) bool {
	if len(terms) == 0 {
		return true
	}
	var firstPostings []Posting
	if len(prefixes) > 0 && prefixes[0] {
		// First term is a prefix wildcard: consider every posting whose
		// term starts with the prefix.
		for term, postings := range idx.index {
			if len(term) >= len(terms[0]) && term[:len(terms[0])] == terms[0] {
				firstPostings = append(firstPostings, postings...)
			}
		}
	} else {
		firstPostings = collectDocPostings(idx.index[terms[0]], docID)
	}
	for _, fp := range firstPostings {
		if fp.DocID != docID {
			continue
		}
		if phraseMatchesAt(fp, terms, prefixes, idx) {
			return true
		}
	}
	return false
}

// phraseInDocFirst reports whether the phrase occurs with its FIRST token at
// position 0 of some column (the FTS4 "^"phrase"" column-first operator,
// fts3first.test).
func (idx *InvertedIndex) phraseInDocFirst(docID int64, terms []string, prefixes []bool) bool {
	if len(terms) == 0 {
		return true
	}
	var firstPostings []Posting
	if len(prefixes) > 0 && prefixes[0] {
		for term, postings := range idx.index {
			if len(term) >= len(terms[0]) && term[:len(terms[0])] == terms[0] {
				firstPostings = append(firstPostings, postings...)
			}
		}
	} else {
		firstPostings = collectDocPostings(idx.index[terms[0]], docID)
	}
	for _, fp := range firstPostings {
		if fp.DocID != docID || fp.Position != 0 {
			continue
		}
		if phraseMatchesAt(fp, terms, prefixes, idx) {
			return true
		}
	}
	return false
}

// phraseInDocColumn reports whether all terms appear in sequence within a
// specific column of a document (SQL `col MATCH '"a b"'` / col:phrase).
func (idx *InvertedIndex) phraseInDocColumn(docID int64, terms []string, prefixes []bool, colNum int) bool {
	if len(terms) == 0 {
		return true
	}
	var firstPostings []Posting
	if len(prefixes) > 0 && prefixes[0] {
		for term, postings := range idx.index {
			if len(term) >= len(terms[0]) && term[:len(terms[0])] == terms[0] {
				firstPostings = append(firstPostings, postings...)
			}
		}
	} else {
		firstPostings = idx.index[terms[0]]
	}
	for _, p := range firstPostings {
		if p.DocID == docID && p.Column == colNum {
			if phraseMatchesAt(p, terms, prefixes, idx) {
				return true
			}
		}
	}
	return false
}

// prefixInDocColumn reports whether a document contains a term with the given
// prefix within a specific column.
func (idx *InvertedIndex) prefixInDocColumn(docID int64, prefix string, colNum int) bool {
	for term, postings := range idx.index {
		if len(term) >= len(prefix) && term[:len(prefix)] == prefix {
			for _, p := range postings {
				if p.DocID == docID && p.Column == colNum {
					return true
				}
			}
		}
	}
	return false
}

// collectDocPostings returns the postings for a doc within a posting list.
func collectDocPostings(postings []Posting, docID int64) []Posting {
	var result []Posting
	for _, p := range postings {
		if p.DocID == docID {
			result = append(result, p)
		}
	}
	return result
}

// phraseMatchesAt reports whether the remaining phrase terms appear at
// consecutive positions following fp. A term marked as a prefix in prefixes
// matches any indexed term starting with that prefix.
func phraseMatchesAt(fp Posting, terms []string, prefixes []bool, idx *InvertedIndex) bool {
	for offset := 1; offset < len(terms); offset++ {
		term := terms[offset]
		nextPos := fp.Position + offset
		if offset-1 < len(prefixes) && prefixes[offset] {
			if !prefixPostingAt(idx, fp, nextPos, term) {
				return false
			}
		} else if !postingAt(idx.index[term], fp, nextPos) {
			return false
		}
	}
	return true
}

// prefixPostingAt reports whether the document has a posting at the given
// position whose term starts with the prefix, in the same column as fp.
func prefixPostingAt(idx *InvertedIndex, fp Posting, nextPos int, prefix string) bool {
	for term, postings := range idx.index {
		if len(term) >= len(prefix) && term[:len(prefix)] == prefix {
			for _, p := range postings {
				if p.DocID == fp.DocID && p.Column == fp.Column && p.Position == nextPos {
					return true
				}
			}
		}
	}
	return false
}

// postingAt reports whether the posting list contains a match for fp's doc
// and column at the given position.
func postingAt(postings []Posting, fp Posting, nextPos int) bool {
	for _, p := range postings {
		if p.DocID == fp.DocID && p.Column == fp.Column && p.Position == nextPos {
			return true
		}
	}
	return false
}

// collectTermNearPositions returns the (position, column) of every occurrence
// of term in a document (within one column when colNum >= 0). Used by NEAR
// matching: a term's position is its own token offset.
func collectTermNearPositions(idx *InvertedIndex, docID int64, term string, colNum int) []nearPos {
	var positions []nearPos
	for _, p := range idx.index[term] {
		if p.DocID == docID && (colNum < 0 || p.Column == colNum) {
			positions = append(positions, nearPos{pos: p.Position, col: p.Column})
		}
	}
	return positions
}

// collectPrefixNearPositions returns the (position, column) of every indexed
// term starting with the prefix in a document (within one column when colNum
// >= 0). Each matching posting contributes its own position.
func collectPrefixNearPositions(idx *InvertedIndex, docID int64, prefix string, colNum int) []nearPos {
	var positions []nearPos
	for term, postings := range idx.index {
		if len(term) < len(prefix) || term[:len(prefix)] != prefix {
			continue
		}
		for _, p := range postings {
			if p.DocID == docID && (colNum < 0 || p.Column == colNum) {
				positions = append(positions, nearPos{pos: p.Position, col: p.Column})
			}
		}
	}
	return positions
}

// collectPhraseNearPositions returns the LAST-token (position, column) of every
// occurrence of a phrase in a document (within one column when colNum >= 0).
// SQLite's NEAR operator uses each phrase's last token as its position, and the
// phrase length offsets the distance window (fts3.c fts3PoslistPhraseMerge).
func collectPhraseNearPositions(idx *InvertedIndex, docID int64, terms []string, prefixes []bool, colNum int) []nearPos {
	if len(terms) == 0 {
		return nil
	}
	var positions []nearPos
	var firstPostings []Posting
	if len(prefixes) > 0 && prefixes[0] {
		for term, postings := range idx.index {
			if len(term) >= len(terms[0]) && term[:len(terms[0])] == terms[0] {
				firstPostings = append(firstPostings, postings...)
			}
		}
	} else {
		firstPostings = idx.index[terms[0]]
	}
	for _, fp := range firstPostings {
		if fp.DocID != docID || (colNum >= 0 && fp.Column != colNum) {
			continue
		}
		if phraseMatchesAt(fp, terms, prefixes, idx) {
			positions = append(positions, nearPos{pos: fp.Position + len(terms) - 1, col: fp.Column})
		}
	}
	return positions
}

// SearchPrefix returns docids containing terms with the given prefix.
func (idx *InvertedIndex) SearchPrefix(prefix string) []int64 {
	seen := make(map[int64]bool)
	var result []int64
	for term, postings := range idx.index {
		if len(term) >= len(prefix) && term[:len(prefix)] == prefix {
			for _, p := range postings {
				if !seen[p.DocID] {
					seen[p.DocID] = true
					result = append(result, p.DocID)
				}
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// AllDocIDs returns all document IDs in the index, sorted.
func (idx *InvertedIndex) AllDocIDs() []int64 {
	var ids []int64
	for docID := range idx.docs {
		ids = append(ids, docID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// DocCount returns the number of documents in the index.
func (idx *InvertedIndex) DocCount() int {
	return len(idx.docs)
}

// SearchPostings returns all postings for a term (for offsets/snippet).
func (idx *InvertedIndex) SearchPostings(term string) []Posting {
	return idx.index[term]
}

// AuxTerm is one row of the fts4aux virtual table: a term, the column it
// appears in (or "*" for the all-columns aggregate), the number of documents
// containing it, and the total number of occurrences.
type AuxTerm struct {
	Term        string
	Column      string
	Documents   int64
	Occurrences int64
}

// AuxTerms returns the per-term-per-column statistics for the fts4aux virtual
// table (fts3_aux.c Fts3auxCursor: one row per (term, column) with the
// documents/occurrences counts, plus a per-term "*" row aggregating across
// all columns). Terms and columns are sorted so the cursor yields the same
// order SQLite produces.
// HasCorruptTerms reports whether any term's doclist failed to load (the
// term was recorded as corrupt during segment loading).
func (t *FTS3Table) HasCorruptTerms() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.index.corruptTerms) > 0
}

// HasOutOfRangePostings reports whether any posting references a column
// outside the table's column range (a crafted doclist's column marker beyond
// nCol makes fts4aux reads corrupt, fts3corrupt7 4.4).
func (t *FTS3Table) HasOutOfRangePostings() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.index.auxCorrupt {
		return true
	}
	nCol := len(t.columnNames)
	for _, postings := range t.index.index {
		for _, p := range postings {
			if p.Column < 0 || p.Column >= nCol {
				return true
			}
		}
	}
	return false
}

func (t *FTS3Table) AuxTerms() []AuxTerm {
	t.mu.Lock()
	defer t.mu.Unlock()
	terms := make([]string, 0, len(t.index.index))
	for term := range t.index.index {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	nCol := len(t.columnNames)
	var out []AuxTerm
	for _, term := range terms {
		postings := t.index.index[term]
		type colStat struct {
			docs int64
			occ  int64
		}
		perCol := make([]colStat, nCol)
		// Distinct documents per column (docid+column pair key).
		seen := make(map[int64]bool)
		for _, p := range postings {
			if p.Column >= 0 && p.Column < nCol {
				perCol[p.Column].occ++
				key := p.DocID*int64(nCol+1) + int64(p.Column)
				if !seen[key] {
					seen[key] = true
					perCol[p.Column].docs++
				}
			}
		}
		// Aggregate totals across columns.
		allCols := make(map[int64]bool)
		totalOcc := int64(0)
		for _, p := range postings {
			totalOcc++
			allCols[p.DocID] = true
		}
		totalDocs := int64(len(allCols))
		// SQLite's fts4aux emits the per-term "*" aggregate row FIRST, then one
		// row per column with the column INDEX as its name (fts3_aux.c
		// fts3AuxNext: col "-1" becomes "*", else the column number).
		out = append(out, AuxTerm{Term: term, Column: "*", Documents: totalDocs, Occurrences: totalOcc})
		for i := 0; i < nCol; i++ {
			out = append(out, AuxTerm{Term: term, Column: strconv.Itoa(i), Documents: perCol[i].docs, Occurrences: perCol[i].occ})
		}
	}
	return out
}
