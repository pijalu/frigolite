package fts

import "sort"

// Posting represents a single occurrence of a term in a document.
type Posting struct {
	DocID    int64
	Column   int
	Position int
}

// Document stores the original content for an FTS row.
type Document struct {
	DocID   int64
	Columns []string
}

// InvertedIndex is an in-memory inverted index for FTS.
type InvertedIndex struct {
	index  map[string][]Posting // term -> postings
	docs   map[int64]*Document  // docid -> document
	nextID int64
}

// NewInvertedIndex creates a new empty inverted index.
func NewInvertedIndex() *InvertedIndex {
	return &InvertedIndex{
		index:  make(map[string][]Posting),
		docs:   make(map[int64]*Document),
		nextID: 1,
	}
}

// Insert adds a document to the index. Returns the assigned docid.
func (idx *InvertedIndex) Insert(columns []string, tokenizer Tokenizer) int64 {
	docID := idx.nextID
	idx.nextID++
	doc := &Document{DocID: docID, Columns: make([]string, len(columns))}
	copy(doc.Columns, columns)
	idx.docs[docID] = doc
	for colNum, text := range columns {
		tokens := tokenizer.Tokenize(text)
		for _, tok := range tokens {
			key := tok.Term
			idx.index[key] = append(idx.index[key], Posting{
				DocID:    docID,
				Column:   colNum,
				Position: tok.Position,
			})
		}
	}
	return docID
}

// InsertWithID adds a document with a specific docid.
func (idx *InvertedIndex) InsertWithID(docID int64, columns []string, tokenizer Tokenizer) {
	if docID >= idx.nextID {
		idx.nextID = docID + 1
	}
	doc := &Document{DocID: docID, Columns: make([]string, len(columns))}
	copy(doc.Columns, columns)
	idx.docs[docID] = doc
	for colNum, text := range columns {
		tokens := tokenizer.Tokenize(text)
		for _, tok := range tokens {
			key := tok.Term
			idx.index[key] = append(idx.index[key], Posting{
				DocID:    docID,
				Column:   colNum,
				Position: tok.Position,
			})
		}
	}
}

// Delete removes a document from the index.
func (idx *InvertedIndex) Delete(docID int64) {
	delete(idx.docs, docID)
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
func (idx *InvertedIndex) Update(docID int64, columns []string, tokenizer Tokenizer) {
	idx.Delete(docID)
	idx.InsertWithID(docID, columns, tokenizer)
}

// GetDoc returns a document by docid.
func (idx *InvertedIndex) GetDoc(docID int64) *Document {
	return idx.docs[docID]
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
		if idx.phraseInDoc(docID, terms) {
			result = append(result, docID)
		}
	}
	return result
}

func (idx *InvertedIndex) phraseInDoc(docID int64, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	postings := idx.index[terms[0]]
	// Find the first term's positions in the right column
	var firstPostings []Posting
	for _, p := range postings {
		if p.DocID == docID {
			firstPostings = append(firstPostings, p)
		}
	}
	for _, fp := range firstPostings {
		// Check if subsequent terms appear at consecutive positions
		matched := true
		for offset, term := range terms[1:] {
			nextPos := fp.Position + offset + 1
			found := false
			for _, p := range idx.index[term] {
				if p.DocID == docID && p.Column == fp.Column && p.Position == nextPos {
					found = true
					break
				}
			}
			if !found {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
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
