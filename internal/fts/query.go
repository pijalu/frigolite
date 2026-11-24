package fts

import (
	"fmt"
	"strings"
)

// QueryNode is a node in the MATCH query AST.
type QueryNode interface {
	// MatchDoc returns true if the document matches this node.
	MatchDoc(idx *InvertedIndex, docID int64) bool
	// String returns the query string for debugging.
	String() string
}

// TermNode matches a single term.
type TermNode struct {
	Term string
	// First marks the FTS4 "^term" column-first operator: the term must be
	// the FIRST token of a column (position 0).
	First bool
	// Deferred marks an overflow-sized token selected by FTS4's deferred
	// evaluation rule (fts3.c fts3EvalSelectDeferred). Its positions come
	// from re-tokenizing the row content, not from the segment postings.
	Deferred bool
	// Tokenizer is the table tokenizer used for the content rescan.
	Tokenizer Tokenizer
}

func (n *TermNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return len(termPostingsForNode(idx, docID, n, -1)) > 0
}

func (n *TermNode) String() string { return n.Term }

// PhraseNode matches a phrase (consecutive terms). Prefixes marks terms that
// are prefix wildcards ("lin* app*" — a phrase of prefix terms).
type PhraseNode struct {
	Terms    []string
	Prefixes []bool // parallel to Terms: true when the term is a prefix wildcard
	// Deferred is parallel to Terms: true when that token is evaluated by a
	// content rescan because its segment doclist is overflow-sized.
	Deferred []bool
	// Tokenizer is the table tokenizer used for deferred content rescans.
	Tokenizer Tokenizer
	// First marks the "^"..." column-first operator: the phrase's first
	// token must be at position 0 of a column.
	First bool
	// FirstAt generalizes First for mid-phrase markers ('"K ^H"': token 1
	// would have to sit at position 0 while following K — impossible, so
	// the phrase never matches). -1 when no ^ marker applies.
	FirstAt int
}

func (n *PhraseNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	if n.FirstAt > 0 {
		// The marked token would have to sit at position 0 while preceding
		// tokens occupy earlier positions — impossible (fts3first '"K ^H"').
		return false
	}
	firstAt := -1
	if n.First || n.FirstAt == 0 {
		firstAt = 0
	}
	return len(phraseOccurrenceLastPositions(idx, docID, n, -1, firstAt)) > 0
}

func (n *PhraseNode) String() string {
	return "\"" + strings.Join(n.Terms, " ") + "\""
}

// PrefixNode matches terms with a given prefix.
type PrefixNode struct {
	Prefix string
	// First marks the "^pre*" column-first operator.
	First bool
	// Deferred marks an overflow-sized prefix token evaluated from content.
	Deferred bool
	// Tokenizer is the table tokenizer used for the content rescan.
	Tokenizer Tokenizer
}

func (n *PrefixNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return len(termPostingsForNode(idx, docID, n, -1)) > 0
}

// prefixMatches reports whether term starts with the given prefix.
func prefixMatches(prefix, term string) bool {
	return len(term) >= len(prefix) && term[:len(prefix)] == prefix
}

func (n *PrefixNode) String() string { return n.Prefix + "*" }

// TailDropNode is a neutral AND operand produced when a '(' opens at the end
// of the query with no closing ')' (fts3_expr.c: the recursive fts3ExprParse
// consumes to end-of-input and returns SQLITE_DONE with no expression, so
// everything after the unclosed '(' is dropped — the query is the phrases
// before it). Matching everything makes `x AND (unclosed` equivalent to `x`
// (fts3snippet2.test 3.1's binary blob).
type TailDropNode struct{}

func (n *TailDropNode) MatchDoc(idx *InvertedIndex, docID int64) bool { return true }

func (n *TailDropNode) String() string { return "(..." }

// AndNode matches documents matching both left and right.
type AndNode struct {
	Left, Right QueryNode
}

func (n *AndNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return n.Left.MatchDoc(idx, docID) && n.Right.MatchDoc(idx, docID)
}

func (n *AndNode) String() string {
	return fmt.Sprintf("(%s AND %s)", n.Left, n.Right)
}

// NearNode matches documents where the left and right phrases occur within
// Distance tokens of each other (SQLite's NEAR operator: `A NEAR/n B`). The
// default distance is 10 (SQLITE_FTS3_DEFAULT_NEAR_PARAM).
type NearNode struct {
	Left, Right QueryNode
	Distance    int
}

// MatchDoc implements QueryNode. SQLite's NEAR semantics (fts3.c
// fts3PoslistNearMerge / fts3EvalNearTrim): a phrase position is the offset of
// the phrase's LAST token, and positions in different columns never pair
// (fts3PoslistPhraseMerge only merges when iCol1==iCol2). A NEAR/n match
// requires a left position a and a right position b in the same column with
//
//	(b > a && b-a <= n+len(right))  (right occurs after left)
//	or
//	(a > b && a-b <= n+len(left))   (right occurs before left)
//
// The phrase-length terms account for the tokens inside each phrase: the gap
// between the phrase spans must be at most n tokens.
func (n *NearNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	leftPos, leftLen := n.phrasePositions(idx, docID, n.Left)
	if len(leftPos) == 0 {
		return false
	}
	rightPos, rightLen := n.phrasePositions(idx, docID, n.Right)
	if len(rightPos) == 0 {
		return false
	}
	for _, a := range leftPos {
		for _, b := range rightPos {
			if a.col != b.col {
				continue
			}
			if nearPairWithin(a, b, n.Distance, leftLen, rightLen) {
				return true
			}
		}
	}
	return false
}

// nearPairWithin reports whether one left occurrence and one right occurrence
// pair within the NEAR distance. fts3PoslistPhraseMerge pairs positions
// directionally and STRICTLY: right occurrences strictly after left within
// Distance+len(right), left occurrences strictly after right within
// Distance+len(left). The same offset never pairs with itself, so "four NEAR
// four" needs two distinct occurrences (fts3near 1.14: a single 'four' per
// document matches nothing).
// Distance+len(left) — fts3.c's clause `iPos2>iPos1 && iPos2<=iPos1+nToken`
// is strict for every nToken>=1 (the iPos2==iPos1+nToken exact clause is
// subsumed, nToken counting a phrase's own tokens is never 0), so the same
// offset never pairs with itself: "four NEAR four" needs two distinct
// occurrences (fts3near 1.14: a single 'four' per document matches nothing).
// This mirrors the already-strict pairing in nearPhrasePositions and
// markNearPairings — the filter, matchinfo and offsets paths must agree or
// offsets() emits NULL rows for docs the filter accepted (fts3near 2.6).
func nearPairWithin(a, b nearPos, distance, leftLen, rightLen int) bool {
	if b.pos > a.pos && b.pos-a.pos <= distance+rightLen {
		return true
	}
	return a.pos > b.pos && a.pos-b.pos <= distance+leftLen
}

// nearPos is a phrase occurrence position: the last token offset plus the
// column it occurs in (NEAR only pairs positions in the same column).
type nearPos struct {
	pos int
	col int
}

// phrasePositions collects the last-token positions of every occurrence of a
// phrase-like node (term, prefix, phrase, or a column-restricted version of
// those) in a document, plus the phrase's token count. Non-phrase nodes (AND,
// OR, NOT, NEAR) have no single position list; SQLite restricts NEAR operands
// to phrases, so such operands contribute no positions (no match).
func (n *NearNode) phrasePositions(idx *InvertedIndex, docID int64, node QueryNode) ([]nearPos, int) {
	switch v := node.(type) {
	case *TermNode:
		return termNearPositions(idx, docID, v, -1), 1
	case *PrefixNode:
		return termNearPositions(idx, docID, v, -1), 1
	case *PhraseNode:
		return phraseOccurrenceLastPositions(idx, docID, v, -1, -1), len(v.Terms)
	case *ColumnNode:
		return n.columnPhrasePositions(idx, docID, v)
	case *NearNode:
		return n.nearPhrasePositions(idx, docID, v)
	}
	return nil, 0
}

// columnPhrasePositions returns the positions of a column-restricted
// phrase-like node (SQL `col MATCH ...` restricts the phrase to one column).
func (n *NearNode) columnPhrasePositions(idx *InvertedIndex, docID int64, v *ColumnNode) ([]nearPos, int) {
	switch inner := v.Inner.(type) {
	case *TermNode:
		return termNearPositions(idx, docID, inner, v.Column), 1
	case *PrefixNode:
		return termNearPositions(idx, docID, inner, v.Column), 1
	case *PhraseNode:
		return phraseOccurrenceLastPositions(idx, docID, inner, v.Column, -1), len(inner.Terms)
	}
	return nil, 0
}

// nearPhrasePositions returns the position list of a chained NEAR expression.
// A chained NEAR (A NEAR B NEAR C) builds left-associatively: (A NEAR B) NEAR
// C. The inner NEAR's position list is its RIGHT operand's positions that are
// near its LEFT operand (fts3PoslistNearMerge writes the right list), and its
// token count is the right operand's. The outer NEAR then compares the inner's
// output positions against its own right operand.
func (n *NearNode) nearPhrasePositions(idx *InvertedIndex, docID int64, v *NearNode) ([]nearPos, int) {
	leftPos, leftLen := n.phrasePositions(idx, docID, v.Left)
	if len(leftPos) == 0 {
		return nil, 0
	}
	rightPos, rightLen := n.phrasePositions(idx, docID, v.Right)
	var out []nearPos
	for _, a := range leftPos {
		for _, b := range rightPos {
			if a.col == b.col && ((b.pos > a.pos && b.pos-a.pos <= v.Distance+rightLen) || (a.pos > b.pos && a.pos-b.pos <= v.Distance+leftLen)) {
				out = append(out, b)
			}
		}
	}
	if len(out) == 0 {
		return nil, 0
	}
	return out, rightLen
}

func (n *NearNode) String() string {
	return fmt.Sprintf("(%s NEAR/%d %s)", n.Left, n.Distance, n.Right)
}

// OrNode matches documents matching either left or right.
type OrNode struct {
	Left, Right QueryNode
}

func (n *OrNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return n.Left.MatchDoc(idx, docID) || n.Right.MatchDoc(idx, docID)
}

func (n *OrNode) String() string {
	return fmt.Sprintf("(%s OR %s)", n.Left, n.Right)
}

// NotNode matches documents NOT matching the inner node.
type NotNode struct {
	Inner QueryNode
}

func (n *NotNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	return !n.Inner.MatchDoc(idx, docID)
}

func (n *NotNode) String() string {
	return "-" + n.Inner.String()
}

// ColumnNode restricts matching to a specific column.
type ColumnNode struct {
	Column int
	Inner  QueryNode
}

func (n *ColumnNode) MatchDoc(idx *InvertedIndex, docID int64) bool {
	// Check if the inner node matches within the specified column
	switch inner := n.Inner.(type) {
	case *TermNode, *PrefixNode:
		return len(termPostingsForNode(idx, docID, n.Inner, n.Column)) > 0
	case *PhraseNode:
		firstAt := -1
		if inner.First || inner.FirstAt == 0 {
			firstAt = 0
		}
		return len(phraseOccurrenceLastPositions(idx, docID, inner, n.Column, firstAt)) > 0
	default:
		// Fall back to normal matching (may over-match for column filters)
		return n.Inner.MatchDoc(idx, docID)
	}
}

func (n *ColumnNode) String() string {
	return fmt.Sprintf("col%d:%s", n.Column, n.Inner)
}
