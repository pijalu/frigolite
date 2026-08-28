package fts

import (
	"testing"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
)

// Hand-built blobs below follow the block layout documented in
// fts3_write.c / decoded by fts3view.c decodeSegment: [height varint]
// [left-child varint when height>0] then repeated
// [prefix varint][suffix varint][suffix bytes] plus a doclist-size varint
// on leaves.

func buildLeafBlob(t *testing.T, terms []string, doclists [][]byte) []byte {
	t.Helper()
	buf := AppendFTS3Varint(nil, 0) // height 0
	for i, term := range terms {
		if i > 0 {
			// Shared prefix with previous term (exercise prefix coding). The
			// FIRST entry carries no prefix varint (fts3view.c decodeSegment:
			// "if( (cnt++)>0 )").
			prefix := 0
			prev := terms[i-1]
			for prefix < len(prev) && prefix < len(term) && prev[prefix] == term[prefix] {
				prefix++
			}
			buf = AppendFTS3Varint(buf, uint64(prefix))
			term = term[prefix:]
		}
		buf = AppendFTS3Varint(buf, uint64(len(term)))
		buf = append(buf, term...)
		buf = AppendFTS3Varint(buf, uint64(len(doclists[i])))
		buf = append(buf, doclists[i]...)
	}
	return buf
}

func TestSegviewDecodeLeafBlock(t *testing.T) {
	dl1 := []byte{0x01}
	dl2 := []byte{0x02, 0x03}
	blob := buildLeafBlob(t, []string{"apple", "apply", "banana"}, [][]byte{dl1, dl2, dl2})
	node, err := DecodeSegmentBlock(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if node.Height != 0 || len(node.Entries) != 3 {
		t.Fatalf("got height=%d entries=%d", node.Height, len(node.Entries))
	}
	wantTerms := []string{"apple", "apply", "banana"}
	sizes := []int64{int64(len(dl1)), int64(len(dl2)), int64(len(dl2))}
	for i, e := range node.Entries {
		if e.Term != wantTerms[i] {
			t.Errorf("entry %d term=%q want %q", i, e.Term, wantTerms[i])
		}
		if e.DoclistSize != sizes[i] {
			t.Errorf("entry %d doclist size=%d want %d", i, e.DoclistSize, sizes[i])
		}
		if e.DoclistOffset <= 0 || int(e.DoclistOffset)+int(e.DoclistSize) > len(blob) {
			t.Errorf("entry %d doclist range [%d,%d) outside blob (%d bytes)",
				i, e.DoclistOffset, e.DoclistOffset+e.DoclistSize, len(blob))
		}
	}
}

func TestSegviewDecodeInteriorBlock(t *testing.T) {
	buf := AppendFTS3Varint(nil, 1) // height 1
	buf = AppendFTS3Varint(buf, 42) // left child
	for i, sep := range []string{"ab", "abc", "m"} {
		if i > 0 {
			buf = AppendFTS3Varint(buf, 0) // shared-prefix varint (omitted on first entry)
		}
		buf = AppendFTS3Varint(buf, uint64(len(sep)))
		buf = append(buf, sep...)
	}
	node, err := DecodeSegmentBlock(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if node.Height != 1 || node.LeftChild != 42 {
		t.Fatalf("height=%d left=%d", node.Height, node.LeftChild)
	}
	want := []string{"ab", "abc", "m"}
	for i, e := range node.Entries {
		if e.Term != want[i] {
			t.Errorf("entry %d term=%q want %q", i, e.Term, want[i])
		}
		if e.Child != int64(43+i) {
			t.Errorf("entry %d child=%d want %d", i, e.Child, 43+i)
		}
	}
}

func TestSegviewDecodeDoclist(t *testing.T) {
	// Two documents; second one uses a column switch. Positions are
	// delta-coded with pos-2 accumulation (fts3view.c decodeDoclist).
	var dl []byte
	dl = AppendFTS3Varint(dl, 10) // first docid (absolute delta from 0)
	dl = AppendFTS3Varint(dl, 2)  // position 0 (2-2)
	dl = AppendFTS3Varint(dl, 4)  // position 2 ((4-2)+0)
	dl = AppendFTS3Varint(dl, 0)  // end of document
	dl = AppendFTS3Varint(dl, 5)  // docid delta: 10+5=15
	dl = AppendFTS3Varint(dl, 1)  // column switch sentinel
	dl = AppendFTS3Varint(dl, 2)  // column 2
	dl = AppendFTS3Varint(dl, 6)  // position 4
	dl = AppendFTS3Varint(dl, 0)  // end of document

	docs, err := DecodeDoclist(dl)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs=%d want 2", len(docs))
	}
	if docs[0].DocID != 10 || docs[1].DocID != 15 {
		t.Errorf("docids %d,%d want 10,15", docs[0].DocID, docs[1].DocID)
	}
	if p := docs[0].Columns[0].Positions; len(p) != 2 || p[0] != 0 || p[1] != 2 {
		t.Errorf("doc0 positions=%v want [0 2]", p)
	}
	cols := docs[1].Columns
	if len(cols) != 1 || cols[0].Col != 2 || cols[0].Positions[0] != 4 {
		t.Errorf("doc1 columns=%+v want col 2 pos [4]", cols)
	}
}

func TestSegviewDecodeTruncatedErrors(t *testing.T) {
	if _, err := DecodeSegmentBlock([]byte{0x00, 0x05}); err == nil {
		t.Error("truncated suffix accepted")
	}
	if _, err := DecodeDoclist([]byte{0x0a, 0x02}); err == nil {
		t.Error("truncated doclist accepted")
	}
}

// TestSegviewOracleX6InteriorNodes validates the decoders against the
// committed oracle fixture (UCL rule U1): the x6 growth scenario must show
// interior nodes at blockids 2155 (986 B) and 2156 (754 B), both height 1,
// per /usr/bin/sqlite3 3.51.0 output recorded at fixture generation time.
func TestSegviewOracleX6InteriorNodes(t *testing.T) {
	const dbPath = "testdata/ftsconformance/fts-x6-growth.db"
	pg, err := pager.Open(dbPath, 1024)
	if err != nil {
		t.Skipf("oracle fixture not available: %v", err)
	}
	defer pg.Close()
	tree := btree.NewBTree(pg, 238, true) // x6_segments root page in the committed fixture
	cur, err := tree.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	blocks := map[int64][]byte{}
	for {
		payload, rowID, derr := cur.ReadCellData()
		if derr != nil {
			t.Fatalf("scan row %d: %v", len(blocks)+1, derr)
		}
		rec := payload
		// Each %_segments row is a single-blob record; strip the record
		// header (serial-type varints precede the blob data) by decoding it.
		blob := decodeRecordFirstColumn(rec)
		blocks[rowID] = blob
		ok, nerr := cur.Next()
		if nerr != nil {
			t.Fatal(nerr)
		}
		if !ok {
			break
		}
	}
	for _, id := range []int64{2155, 2156} {
		blob, ok := blocks[id]
		if !ok {
			t.Fatalf("blockid %d missing from fixture scan (%d blocks)", id, len(blocks))
		}
		wantLen := map[int64]int{2155: 986, 2156: 754}[id]
		if len(blob) != wantLen {
			t.Errorf("block %d: len=%d want %d", id, len(blob), wantLen)
		}
		node, derr := DecodeSegmentBlock(blob)
		if derr != nil {
			t.Fatalf("block %d: decode: %v", id, derr)
		}
		if node.Height != 1 {
			t.Errorf("block %d: height=%d want 1", id, node.Height)
		}
		if len(node.Entries) == 0 {
			t.Errorf("block %d: no separator entries", id)
		}
	}
}

// decodeRecordFirstColumn extracts the first column value (raw bytes) from
// a single-column record payload: the record header (header-size varint +
// serial-type varints) precedes the body, which for a one-blob row is the
// blob itself.
func decodeRecordFirstColumn(rec []byte) []byte {
	hdrSize, _ := getFTS3Varint(rec)
	return rec[hdrSize:]
}
