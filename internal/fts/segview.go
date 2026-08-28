package fts

// Structure decoders for FTS3/4 segment files, ported from SQLite's
// ext/fts3/tool/fts3view.c (structure inspector). Each function carries a
// comment mapping it to its C origin. Decoders are pure blob→struct
// converters written from the C source (UCL rule U3); database access
// stays with the caller (see tools/ftsview). They exist so conformance
// tests can name the first divergent byte instead of a scalar mismatch.

import "fmt"

// SegmentEntry is one separator/posting entry of a decoded segment block.
type SegmentEntry struct {
	Term string

	// Child is valid only when the containing node has Height > 0:
	// the blockid of the right-hand child for this separator term.
	Child int64

	// DoclistSize and DoclistOffset are valid only when Height == 0:
	// the byte length of the term's doclist and its offset inside the
	// same blob (immediately after the size varint).
	DoclistSize   int64
	DoclistOffset int64
}

// SegmentNode is a decoded leaf or interior segment block.
//
// Layout (fts3_write.c): first varint is the height; when height > 0 the
// second varint is the left-most child blockid; then repeated entries of
// [shared-prefix varint (from 2nd entry on)] [suffix-length varint]
// [suffix bytes] and, on leaves only, [doclist-size varint][doclist].
type SegmentNode struct {
	Height    int64
	LeftChild int64 // valid when Height > 0
	Entries   []SegmentEntry
}

// DecodeSegmentBlock ports decodeSegment (fts3view.c:547-592) into a
// structured tree node instead of stdout text.
func DecodeSegmentBlock(aData []byte) (*SegmentNode, error) {
	node := &SegmentNode{}
	i := 0
	nHeight, n := getFTS3Varint(aData[i:])
	i += n
	node.Height = int64(nHeight)
	if node.Height > 0 {
		child, m := getFTS3Varint(aData[i:])
		i += m
		node.LeftChild = int64(child)
	}
	cnt := 0
	nextChild := node.LeftChild
	var prevTerm []byte
	for i < len(aData) {
		prefix := int64(0)
		if cnt > 0 {
			p, m := getFTS3Varint(aData[i:])
			i += m
			prefix = int64(p)
		}
		suffixLen, m := getFTS3Varint(aData[i:])
		i += m
		if i+int(suffixLen) > len(aData) {
			return nil, fmt.Errorf("segment block: term suffix overruns blob at offset %d", i)
		}
		// Shared-prefix encoding: keep prefix bytes of the previous term.
		if int(prefix) > len(prevTerm) {
			return nil, fmt.Errorf("segment block: prefix %d exceeds previous term length %d", prefix, len(prevTerm))
		}
		term := append(append([]byte{}, prevTerm[:prefix]...), aData[i:i+int(suffixLen)]...)
		i += int(suffixLen)
		entry := SegmentEntry{Term: string(term)}
		if node.Height == 0 {
			docsz, m := getFTS3Varint(aData[i:])
			i += m
			entry.DoclistSize = int64(docsz)
			entry.DoclistOffset = int64(i)
			i += int(docsz)
			if i > len(aData) {
				return nil, fmt.Errorf("segment block: doclist overruns blob after term %q", entry.Term)
			}
		} else {
			// Interior separator: children are numbered left-to-right starting
			// at LeftChild+1 (fts3view.c prints ++iChild).
			nextChild++
			entry.Child = nextChild
		}
		node.Entries = append(node.Entries, entry)
		prevTerm = term
		cnt++
	}
	return node, nil
}

// DoclistColumn holds the position list of one column within a document.
type DoclistColumn struct {
	Col       int64
	Positions []int64
}

// DoclistDoc holds all columns of one document rowid in a doclist.
type DoclistDoc struct {
	DocID   int64
	Columns []DoclistColumn
}

// DecodeDoclist ports decodeDoclist (fts3view.c:704-731) into structured
// rows. Docids are absolute deltas; positions are delta-coded with the
// col-switch sentinel 1 and end-of-document sentinel 0 (pos-2 accumulation
// mirrors the C exactly).
func DecodeDoclist(aData []byte) ([]DoclistDoc, error) {
	var docs []DoclistDoc
	iPrevDocid := int64(0)
	i := 0
	for i < len(aData) {
		v, m := getFTS3Varint(aData[i:])
		i += m
		docID := iPrevDocid + int64(v)
		iPrevDocid = docID
		doc := DoclistDoc{DocID: docID}
		curCol := int64(0)
		iPrevPos := int64(0)
		started := false
		for {
			if i >= len(aData) {
				return nil, fmt.Errorf("doclist: truncated before terminator for docid %d", docID)
			}
			v, m := getFTS3Varint(aData[i:])
			i += m
			iPos := int64(v)
			switch iPos {
			case 1: // column switch
				cv, m := getFTS3Varint(aData[i:])
				i += m
				curCol = int64(cv)
				doc.Columns = append(doc.Columns, DoclistColumn{Col: curCol})
				iPrevPos = 0
				started = true
			case 0: // end of document
				docs = append(docs, doc)
				goto nextDoc
			default:
				if !started {
					// Positions before any col switch belong to column 0.
					doc.Columns = append(doc.Columns, DoclistColumn{Col: curCol})
					started = true
				}
				iPrevPos += iPos - 2
				last := &doc.Columns[len(doc.Columns)-1]
				last.Positions = append(last.Positions, iPrevPos)
			}
		}
	nextDoc:
	}
	return docs, nil
}
