// Package fts: generic segment-node truncation — a faithful port of
// SQLite's fts3TruncateNode (fts3_write.c). Used by the incremental-merge
// chomp (fts3TruncateSegment) to drop every term smaller than a bound from
// any node image (leaf or interior), keeping block ids stable.
package fts

import "strings"

// nodeEntry is one decoded entry of a segment node: its full term and, for
// leaf nodes, the raw doclist that follows it.
type nodeEntry struct {
	term    string
	doclist []byte // nil for interior-node entries
}

// parseNodeEntries decodes a node image into its height byte, first-child
// pointer (interior nodes only; 0 for leaves) and entries in order. It
// mirrors SQLite's nodeReaderInit/nodeReaderNext: interior entries carry no
// doclist, leaf entries are "<nPrefix><nSuffix><suffix><nDoclist><doclist>"
// with the prefix field omitted on the first entry.
func parseNodeEntries(aNode []byte) (height int, firstChild int64, entries []nodeEntry, corrupt bool) {
	if len(aNode) < 1 {
		return 0, 0, nil, true
	}
	height = int(aNode[0])
	pos := 1
	if height > 0 {
		fc, n := GetFTS3Varint(aNode[pos:])
		if n == 0 {
			return 0, 0, nil, true
		}
		firstChild = int64(fc)
		pos += n
	}
	var prev string
	for pos < len(aNode) {
		nPrefix := 0
		if len(entries) > 0 {
			v, n := GetFTS3Varint(aNode[pos:])
			if n == 0 || int(v) > len(prev) {
				return 0, 0, nil, true
			}
			nPrefix = int(v)
			pos += n
		}
		vs, ns := GetFTS3Varint(aNode[pos:])
		if ns == 0 || int(vs) == 0 || pos+ns+int(vs) > len(aNode) {
			return 0, 0, nil, true
		}
		nSuffix := int(vs)
		pos += ns
		term := prev[:nPrefix] + string(aNode[pos:pos+nSuffix])
		pos += nSuffix
		e := nodeEntry{term: term}
		if height == 0 {
			vd, nd := GetFTS3Varint(aNode[pos:])
			if nd == 0 || pos+nd+int(vd) > len(aNode) {
				return 0, 0, nil, true
			}
			pos += nd
			e.doclist = aNode[pos : pos+int(vd)]
			pos += int(vd)
		}
		entries = append(entries, e)
		prev = term
	}
	return height, firstChild, entries, false
}

// startNode writes a node header: the height byte followed by the left-hand
// child varint for interior nodes (SQLite's fts3StartNode).
func startNode(height int, firstChild int64) []byte {
	out := []byte{byte(height)}
	if firstChild != 0 {
		out = AppendFTS3Varint(out, uint64(firstChild))
	}
	return out
}

// appendToNode appends one entry (term + optional leaf doclist) to out,
// prefix-compressing term against prev and updating prev. Mirrors SQLite's
// fts3AppendToNode: the prefix length is omitted for the first entry.
func appendToNode(out []byte, prev *string, e nodeEntry) []byte {
	nPrefix := commonPrefixLen(*prev, e.term)
	bFirst := len(*prev) == 0
	if !bFirst {
		out = AppendFTS3Varint(out, uint64(nPrefix))
	}
	out = AppendFTS3Varint(out, uint64(len(e.term)-nPrefix))
	out = append(out, e.term[nPrefix:]...)
	if e.doclist != nil {
		out = AppendFTS3Varint(out, uint64(len(e.doclist)))
		out = append(out, e.doclist...)
	}
	*prev = e.term
	return out
}

// TruncateNode copies every entry whose term qualifies against zTerm from
// aNode into a fresh node image (a port of SQLite's fts3TruncateNode): leaf
// nodes keep terms >= zTerm, interior nodes keep terms > zTerm strictly.
//
// Returns the new node image and the child block the caller must descend
// into (SQLite's *piBlock): the child preceding the first kept entry (the
// child containing zTerm), or — when NO entry qualified — the LAST child,
// whose subtree still holds terms >= zTerm. Leaf nodes report 0 (no
// descent). A nil result means the node image was corrupt.
func TruncateNode(aNode []byte, zTerm string) ([]byte, int64) {
	height, firstChild, entries, corrupt := parseNodeEntries(aNode)
	if corrupt {
		return nil, 0
	}
	bLeaf := height == 0

	var out []byte
	var prev string
	iBlock := int64(0)
	for i := range entries {
		res := strings.Compare(entries[i].term, zTerm)
		if res < 0 || (!bLeaf && res == 0) {
			continue
		}
		if out == nil {
			// First qualifying entry. Interior nodes emit this entry's
			// preceding child pointer (firstChild+i) and report it as the
			// descent target; a LEAF has no child pointers (SQLite's
			// nodeReader never increments iChild for leaves), so its header
			// is just the height byte and there is no descent.
			child := int64(0)
			if !bLeaf {
				child = firstChild + int64(i)
				iBlock = child
			}
			out = startNode(height, child)
		}
		out = appendToNode(out, &prev, entries[i])
	}
	if out == nil {
		// Nothing qualified: keep an empty node of the same height pointing
		// at the LAST child (reader.iChild after consuming all entries).
		// A leaf has no child pointers (iChild stays 0): the emptied root
		// degenerates to its height byte and there is no descent.
		last := firstChild + int64(len(entries))
		if bLeaf {
			last = 0
		}
		out = startNode(height, last)
		iBlock = last
	}
	return out, iBlock
}
