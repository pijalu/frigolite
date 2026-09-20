package fts5

import (
	"sort"
)

// The inverted index lives doc-centric: every document records its token
// stream per column (docEntry.cols), and a derived postings map
// (term -> rowid -> column -> positions) serves term/phrase lookups. This is
// a Go-idiomatic storage layout INSIDE the shadow tables: the %_data blocks
// serialize the doc token streams instead of C's segment b-trees
// (fts5_index.c). The SQL-observable contract — %_data/%_content/%_docsize/
// %_config exist with C's schemas, are readable/writable through SQL, and
// MATCH behaves like SQLite — is preserved; the block byte format is not
// (documented divergence, see storage.go).

// docEntry is one indexed document.
type docEntry struct {
	rowid int64
	// values holds the stored column values for normal-content tables
	// (mirrored from %_content). Contentless tables keep nil.
	values []interface{}
	// cols[c] is the token stream of column c in position order (unindexed
	// columns are nil).
	cols [][]string
}

// docPostings is one document's per-column position lists for a term.
type docPostings struct {
	cols map[int][]int
}

// InvertedIndex is the fts5 table's in-memory index.
type InvertedIndex struct {
	// docs is keyed by rowid; iteration order comes from sortedRowids.
	docs map[int64]*docEntry
	// postings maps a term to per-document per-column position lists.
	postings map[string]map[int64]*docPostings
	// nTokensPerCol totals the token counts per column (bm25 averages).
	nTokensPerCol []int64
}

// NewInvertedIndex creates an empty index for nCol columns.
func NewInvertedIndex(nCol int) *InvertedIndex {
	return &InvertedIndex{
		docs:          make(map[int64]*docEntry),
		postings:      make(map[string]map[int64]*docPostings),
		nTokensPerCol: make([]int64, nCol),
	}
}

// addPosting records one (term, rowid, col, pos) instance.
func (ix *InvertedIndex) addPosting(term string, rowid int64, col, pos int) {
	docs, ok := ix.postings[term]
	if !ok {
		docs = make(map[int64]*docPostings)
		ix.postings[term] = docs
	}
	dp, ok := docs[rowid]
	if !ok {
		dp = &docPostings{cols: make(map[int][]int)}
		docs[rowid] = dp
	}
	dp.cols[col] = append(dp.cols[col], pos)
}

// removeDocPostings drops a document's postings, recomputing them from its
// token streams.
func (ix *InvertedIndex) removeDocPostings(doc *docEntry) {
	for _, tokens := range doc.cols {
		seen := make(map[string]bool, len(tokens))
		for _, tok := range tokens {
			if seen[tok] {
				continue // each distinct term's positions are dropped once
			}
			seen[tok] = true
			if docs, ok := ix.postings[tok]; ok {
				delete(docs, doc.rowid)
				if len(docs) == 0 {
					delete(ix.postings, tok)
				}
			}
		}
	}
}

// AddDoc inserts (or replaces) a document built from tokenized columns.
// columns[c] is the token stream of column c (nil for unindexed columns);
// values are the stored values (nil for contentless tables).
func (ix *InvertedIndex) AddDoc(rowid int64, values []interface{}, columns [][]string) {
	if old, ok := ix.docs[rowid]; ok {
		ix.removeDoc(rowid, old)
	}
	doc := &docEntry{rowid: rowid, values: values, cols: columns}
	ix.docs[rowid] = doc
	for c, tokens := range columns {
		ix.nTokensPerCol[c] += int64(len(tokens))
		for pos, tok := range tokens {
			ix.addPosting(tok, rowid, c, pos)
		}
	}
}

// RemoveDoc deletes a document's index entries. It reports whether the rowid
// existed.
func (ix *InvertedIndex) RemoveDoc(rowid int64) bool {
	doc, ok := ix.docs[rowid]
	if !ok {
		return false
	}
	ix.removeDoc(rowid, doc)
	return true
}

// removeDoc unlinks a document (postings + totals).
func (ix *InvertedIndex) removeDoc(rowid int64, doc *docEntry) {
	ix.removeDocPostings(doc)
	for c, tokens := range doc.cols {
		ix.nTokensPerCol[c] -= int64(len(tokens))
	}
	delete(ix.docs, rowid)
}

// HasDoc reports whether a rowid is indexed.
func (ix *InvertedIndex) HasDoc(rowid int64) bool {
	_, ok := ix.docs[rowid]
	return ok
}

// Doc returns a document's token streams (nil when absent).
func (ix *InvertedIndex) Doc(rowid int64) *docEntry {
	return ix.docs[rowid]
}

// sortRowids sorts a rowid slice ascending.
func sortRowids(out []int64) {
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
}

// SortedRowids returns every indexed rowid in ascending order.
func (ix *InvertedIndex) SortedRowids() []int64 {
	out := make([]int64, 0, len(ix.docs))
	for rowid := range ix.docs {
		out = append(out, rowid)
	}
	sortRowids(out)
	return out
}

// termRowids returns the rowids containing term, restricted to col (-1 for
// any column).
func (ix *InvertedIndex) termRowids(term string, col int) map[int64]bool {
	docs, ok := ix.postings[term]
	if !ok {
		return nil
	}
	out := make(map[int64]bool, len(docs))
	for rowid, dp := range docs {
		if col >= 0 {
			if _, ok := dp.cols[col]; ok {
				out[rowid] = true
			}
			continue
		}
		out[rowid] = true
	}
	return out
}

// prefixTerms lists every indexed term with the given prefix, in ascending
// term order (the prefix-index lookup of fts5_index.c, served from the main
// postings map).
func (ix *InvertedIndex) prefixTerms(prefix string) []string {
	var out []string
	for term := range ix.postings {
		if len(term) >= len(prefix) && term[:len(prefix)] == prefix {
			out = append(out, term)
		}
	}
	sort.Strings(out)
	return out
}

// NumDocs returns the indexed document count.
func (ix *InvertedIndex) NumDocs() int { return len(ix.docs) }

// ColTotal returns the total token count of column col across all documents
// (p->aTotalSize[col] analog).
func (ix *InvertedIndex) ColTotal(col int) int64 {
	if col < len(ix.nTokensPerCol) {
		return ix.nTokensPerCol[col]
	}
	return 0
}

// Snapshot returns a deep copy of the index for statement rollback.
func (ix *InvertedIndex) Snapshot() *InvertedIndex {
	cp := NewInvertedIndex(len(ix.nTokensPerCol))
	copy(cp.nTokensPerCol, ix.nTokensPerCol)
	for rowid, doc := range ix.docs {
		nd := &docEntry{rowid: rowid}
		if doc.values != nil {
			nd.values = append([]interface{}(nil), doc.values...)
		}
		nd.cols = make([][]string, len(doc.cols))
		for c, tokens := range doc.cols {
			nd.cols[c] = append([]string(nil), tokens...)
		}
		cp.docs[rowid] = nd
	}
	for term, docs := range ix.postings {
		nd := make(map[int64]*docPostings, len(docs))
		for rowid, dp := range docs {
			ndp := &docPostings{cols: make(map[int][]int, len(dp.cols))}
			for c, pos := range dp.cols {
				ndp.cols[c] = append([]int(nil), pos...)
			}
			nd[rowid] = ndp
		}
		cp.postings[term] = nd
	}
	return cp
}

// Restore replaces the index contents with a snapshot taken by Snapshot.
func (ix *InvertedIndex) Restore(snap *InvertedIndex) {
	ix.docs = snap.docs
	ix.postings = snap.postings
	ix.nTokensPerCol = snap.nTokensPerCol
}

// putVarint appends a SQLite-format varint (fts5_buffer.c
// sqlite3Fts5PutVarint; used for the %_docsize sz blobs so their shape
// matches C's for small counts).
func putVarint(b []byte, v uint64) []byte {
	if v <= 0x7F {
		return append(b, byte(v))
	}
	if v <= 0x3FFF {
		return append(b, byte((v>>7)&0x7F)|0x80, byte(v&0x7F))
	}
	var tmp [9]byte
	n := 0
	for v > 0 {
		tmp[n] = byte(v & 0x7F)
		v >>= 7
		n++
	}
	for i := n - 1; i >= 0; i-- {
		bb := tmp[i]
		if i != 0 {
			bb |= 0x80
		}
		b = append(b, bb)
	}
	return b
}
