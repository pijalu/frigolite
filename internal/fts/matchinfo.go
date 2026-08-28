package fts

// This file implements the matchinfo() auxiliary data model: extracting the
// phrase structure of a MATCH query and computing per-phrase, per-column hit
// statistics from the inverted index (fts3_snippet.c fts3MatchinfoValues /
// fts3EvalGatherStats / fts3EvalUpdateCounts).
//
// SQLite numbers phrases by depth-first traversal of the query expression
// tree (fts3_expr.c sqlite3Fts3ExprIterate). Each phrase-like leaf (a term, a
// phrase, or a prefix; possibly column-restricted) is one "phrase" in the
// matchinfo sense. For every phrase the matchinfo 'x' array carries three
// values per column:
//
//	x[0] = occurrences of the phrase in the column of the current row
//	x[1] = occurrences of the phrase in the column across the phrase's
//	       matching rows (the phrase's own doclist, or the enclosing NEAR's
//	       result when the phrase sits inside a NEAR)
//	x[2] = number of those rows containing at least one occurrence
//
// The 'y' array is x[0] only (per phrase and column); 'b' is a per-phrase
// bitmask of columns with at least one local hit.

// MatchPhrase is one phrase-like leaf of a MATCH query. node is the phrase
// node itself; scope is the node whose matching rows bound the phrase's
// global statistics (the innermost enclosing NEAR, or the phrase itself).
// side is the operand position within the NEAR scope (0 = left, 1 = right);
// it selects which side's participating positions the phrase counts. gate is
// the innermost enclosing AND or NEAR node (nil when the phrase is not inside
// one): the phrase's LOCAL hit counts are only reported when the gate matches
// the current row (fts3_snippet.c fts3ExprLHitGather / fts3ExprLocalHitsCb:
// the phrase's evaluation must reach the row).
type MatchPhrase struct {
	Node  QueryNode
	Scope QueryNode
	Side  int
	Gate  QueryNode
}

// ExtractPhrases returns the phrase-like leaves of a query tree in
// depth-first (left-to-right) order. Each is paired with the scope that
// bounds its global matchinfo statistics: the innermost enclosing NEAR
// expression, or the phrase itself when not inside a NEAR (fts3.c
// fts3EvalGatherStats: pRoot walks up through FTSQUERY_NEAR parents).
func ExtractPhrases(node QueryNode) []MatchPhrase {
	var out []MatchPhrase
	extractPhrases(node, nil, 0, nil, &out)
	return out
}

func extractPhrases(node, scope QueryNode, side int, gate QueryNode, out *[]MatchPhrase) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *TermNode, *PhraseNode, *PrefixNode:
		sc := scope
		if sc == nil {
			sc = node
		}
		*out = append(*out, MatchPhrase{Node: node, Scope: sc, Side: side, Gate: gate})
	case *ColumnNode:
		// A column-restricted phrase (col:term) is one phrase whose
		// occurrences are counted only in that column: keep the ColumnNode
		// wrapper so PhraseColumnCounts applies the restriction.
		sc := scope
		if sc == nil {
			sc = n
		}
		*out = append(*out, MatchPhrase{Node: n, Scope: sc, Side: side, Gate: gate})
	case *ColumnRefNode:
		// Unresolved column ref: treat the inner as the phrase (the
		// resolved tree replaces this with a ColumnNode before matchinfo).
		extractPhrases(n.Inner, scope, side, gate, out)
	case *AndNode:
		extractPhrases(n.Left, scope, side, n, out)
		extractPhrases(n.Right, scope, side, n, out)
	case *OrNode:
		extractPhrases(n.Left, scope, side, gate, out)
		extractPhrases(n.Right, scope, side, gate, out)
	case *NotNode:
		extractPhrases(n.Inner, scope, side, gate, out)
	case *NearNode:
		extractPhrases(n.Left, n, 0, n, out)
		extractPhrases(n.Right, n, 1, n, out)
	}
}

// PhraseColumnCounts returns, for every column index [0, nCol), the number of
// occurrences of the phrase in docID's column (0 when the phrase is
// column-restricted to a different column). colFilter is the phrase's own
// column restriction (-1 for none).
func PhraseColumnCounts(idx *InvertedIndex, docID int64, phrase QueryNode, nCol int) []int {
	counts := make([]int, nCol)
	col := -1
	inner := phrase
	if cn, ok := phrase.(*ColumnNode); ok {
		col = cn.Column
		inner = cn.Inner
	}
	switch n := inner.(type) {
	case *TermNode:
		addPhraseCounts(idx, docID, n.Term, true, col, counts)
		// Column-first (^term): only the position-0 occurrence counts
		// (fts3first.test matchinfo semantics).
		if n.First {
			for c := range counts {
				counts[c] = 0
			}
			addFirstTermCount(idx, docID, n.Term, col, 0, counts)
		}
	case *PrefixNode:
		addPrefixCounts(idx, docID, n.Prefix, col, counts)
		if n.First {
			zeroCounts(counts)
			addFirstPrefixCount(idx, docID, n.Prefix, col, counts)
		}
	case *PhraseNode:
		addPhraseSeqCounts(idx, docID, n.Terms, n.Prefixes, col, counts)
		if n.FirstAt >= 0 {
			zeroCounts(counts)
			addPinnedSeqCounts(idx, docID, n.Terms, n.Prefixes, n.FirstAt, col, counts)
		}
	}
	return counts
}

func zeroCounts(counts []int) {
	for i := range counts {
		counts[i] = 0
	}
}

// addFirstTermCount counts position-at occurrences of term (the '^' pin).
func addFirstTermCount(idx *InvertedIndex, docID int64, term string, col, at int, counts []int) {
	for _, p := range idx.index[term] {
		if p.DocID != docID || p.Position != at {
			continue
		}
		if col >= 0 && p.Column != col {
			continue
		}
		if p.Column < len(counts) {
			counts[p.Column]++
		}
	}
}

// addFirstPrefixCount counts position-0 postings whose term starts with prefix.
func addFirstPrefixCount(idx *InvertedIndex, docID int64, prefix string, col int, counts []int) {
	for term, postings := range idx.index {
		if len(term) < len(prefix) || term[:len(prefix)] != prefix {
			continue
		}
		for _, p := range postings {
			if p.DocID == docID && p.Position == 0 && (col < 0 || p.Column == col) && p.Column < len(counts) {
				counts[p.Column]++
			}
		}
	}
}

// addPinnedSeqCounts counts phrase matches where token FirstAt sits at the
// column-start pin (^ inside a quoted phrase).
func addPinnedSeqCounts(idx *InvertedIndex, docID int64, terms []string, prefixes []bool, firstAt, col int, counts []int) {
	if len(terms) <= firstAt || firstAt < 0 {
		return
	}
	// Require match start s with s+firstAt == 0.
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
		if fp.DocID != docID || (col >= 0 && fp.Column != col) {
			continue
		}
		// A phrase match starting at s places token firstAt at s+firstAt;
		// require that to be the pinned absolute position 0.
		if fp.Position-firstAt != 0 {
			continue
		}
		if phraseMatchesAt(fp, terms, prefixes, idx) && fp.Column < len(counts) {
			counts[fp.Column]++
		}
	}
}

// addPhraseCounts adds one per-column count for every posting of term in the
// document (single term when seq is false, or the LAST token of each
// consecutive phrase match when seq is true via addPhraseSeqCounts).
func addPhraseCounts(idx *InvertedIndex, docID int64, term string, _ bool, col int, counts []int) {
	for _, p := range idx.index[term] {
		if p.DocID != docID {
			continue
		}
		if col >= 0 && p.Column != col {
			continue
		}
		if p.Column < len(counts) {
			counts[p.Column]++
		}
	}
}

// addPrefixCounts adds per-column counts for every posting whose term starts
// with the prefix in the document.
func addPrefixCounts(idx *InvertedIndex, docID int64, prefix string, col int, counts []int) {
	for term, postings := range idx.index {
		if len(term) < len(prefix) || term[:len(prefix)] != prefix {
			continue
		}
		for _, p := range postings {
			if p.DocID != docID {
				continue
			}
			if col >= 0 && p.Column != col {
				continue
			}
			if p.Column < len(counts) {
				counts[p.Column]++
			}
		}
	}
}

// addPhraseSeqCounts adds per-column counts for every occurrence of a
// consecutive-term phrase in the document (one count per phrase match).
func addPhraseSeqCounts(idx *InvertedIndex, docID int64, terms []string, prefixes []bool, col int, counts []int) {
	if len(terms) == 0 {
		return
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
	for _, fp := range firstPostings {
		if fp.DocID != docID || (col >= 0 && fp.Column != col) {
			continue
		}
		if phraseMatchesAt(fp, terms, prefixes, idx) {
			if fp.Column < len(counts) {
				counts[fp.Column]++
			}
		}
	}
}

// ScopeMatchesDoc reports whether a phrase's scope matches docID. A scope
// that is the phrase itself is equivalent to the phrase matching; a NEAR
// scope uses the NEAR operator's semantics.
func ScopeMatchesDoc(idx *InvertedIndex, docID int64, scope QueryNode) bool {
	return scope.MatchDoc(idx, docID)
}

// PhraseMatchesDoc reports whether the phrase itself matches docID.
func PhraseMatchesDoc(idx *InvertedIndex, docID int64, phrase QueryNode) bool {
	return phrase.MatchDoc(idx, docID)
}

// NearSideCounts returns per-column counts of a NEAR operand's positions that
// participate in the NEAR match for a document. SQLite trims each phrase's
// doclist to its own occurrences that are within the NEAR distance of an
// occurrence of the other side (fts3.c fts3EvalNearTrim /
// fts3PoslistNearMerge); the left operand counts its positions pairing with
// the right, the right its positions pairing with the left. Positions in
// different columns never pair. side is 0 for the left operand, 1 for the
// right.
func NearSideCounts(idx *InvertedIndex, docID int64, near *NearNode, side int, nCol int) []int {
	counts := make([]int, nCol)
	var n NearNode
	leftPos, leftLen := n.phrasePositions(idx, docID, near.Left)
	if len(leftPos) == 0 {
		return counts
	}
	rightPos, rightLen := n.phrasePositions(idx, docID, near.Right)
	if len(rightPos) == 0 {
		return counts
	}
	hit := markNearPairings(leftPos, rightPos, near.Distance, leftLen, rightLen)
	if side == 0 {
		countNearSide(counts, leftPos, hit)
	} else {
		countNearSide(counts, rightPos, hit[len(leftPos):])
	}
	return counts
}

// markNearPairings marks which left/right positions pair with an occurrence of
// the other side within the NEAR window (the hit slice has one entry per left
// position followed by one per right position).
func markNearPairings(leftPos, rightPos []nearPos, distance, leftLen, rightLen int) []bool {
	hit := make([]bool, len(leftPos)+len(rightPos))
	for i, a := range leftPos {
		for j, b := range rightPos {
			if a.col != b.col {
				continue
			}
			if (b.pos > a.pos && b.pos-a.pos <= distance+rightLen) || (a.pos > b.pos && a.pos-b.pos <= distance+leftLen) {
				hit[i] = true
				hit[len(leftPos)+j] = true
			}
		}
	}
	return hit
}

// countNearSide adds one per-column count for each participating position.
func countNearSide(counts []int, positions []nearPos, hit []bool) {
	for i, p := range positions {
		if hit[i] && p.col < len(counts) {
			counts[p.col]++
		}
	}
}

// phraseCountsFor returns the per-column hit counts of a phrase in a
// document. When the phrase sits inside a NEAR (scope is a NearNode), the
// counts come from the phrase's OWN positions that participate in the NEAR
// (fts3.c fts3EvalNearTrim); otherwise the phrase's own occurrences are
// counted. side is 0 when the phrase is the NEAR's left operand, 1 for the
// right.
func phraseCountsFor(idx *InvertedIndex, docID int64, phrase, scope QueryNode, side, nCol int) []int {
	if near, ok := scope.(*NearNode); ok {
		return NearSideCounts(idx, docID, near, side, nCol)
	}
	return PhraseColumnCounts(idx, docID, phrase, nCol)
}

// DocTokenCounts returns the number of tokens per column of a document (for
// the matchinfo 'l' format, fts3_snippet.c FTS3_MATCHINFO_LENGTH). A NULL
// column contributes 0.
func DocTokenCounts(doc *Document, tok Tokenizer, nCol int) []int {
	counts := make([]int, nCol)
	if doc == nil || tok == nil {
		return counts
	}
	for i := 0; i < nCol && i < len(doc.Columns); i++ {
		s, ok := doc.Columns[i].(string)
		if !ok {
			continue
		}
		counts[i] = len(tok.Tokenize(s))
	}
	return counts
}
