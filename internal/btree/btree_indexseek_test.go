package btree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// newIndexTree opens an in-memory INDEX b-tree rooted at page 2 (a user
// index: root pages start above page 1, which the schema tree owns).
func newIndexTree(t *testing.T) *BTree {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()           // page 1 — schema's root
	rootPg := pg.AllocatePage() // page 2 — the index root
	rootPg.Data[0] = storage.PageTypeLeafIndex
	binary.BigEndian.PutUint16(rootPg.Data[5:7], uint16(len(rootPg.Data)))
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage(root): %v", err)
	}
	return NewBTree(pg, 2, false)
}

// insertIndexRecord encodes values as an index record and inserts it.
func insertIndexRecord(t *testing.T, bt *BTree, values []interface{}) {
	t.Helper()
	rec, err := storage.EncodeRecord(values)
	if err != nil {
		t.Fatalf("EncodeRecord(%v): %v", values, err)
	}
	if err := bt.InsertCell(&storage.Cell{Type: storage.CellIndexLeaf, Payload: rec}); err != nil {
		t.Fatalf("InsertCell(%v): %v", values, err)
	}
}

// singleKeyInfo is a one-column BINARY KeyInfo (the DML point-lookup shape).
func singleKeyInfo() *KeyInfo {
	return NewKeyInfo(1, []string{""}, nil)
}

func mustCmp(t *testing.T, payload []byte, probe *UnpackedIndexKey) int {
	t.Helper()
	cmp, err := IndexRecordCompare(payload, probe)
	if err != nil {
		t.Fatalf("IndexRecordCompare: %v", err)
	}
	return cmp
}

func signOf(v int) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	}
	return 0
}

func TestIndexRecordCompare_FieldClasses(t *testing.T) {
	ki := singleKeyInfo()
	probeOf := func(v interface{}) *UnpackedIndexKey { return NewUnpackedIndexKey(ki, []interface{}{v}) }

	encode := func(t *testing.T, v interface{}) []byte {
		t.Helper()
		b, err := storage.EncodeRecord([]interface{}{v})
		if err != nil {
			t.Fatalf("EncodeRecord(%v): %v", v, err)
		}
		return b
	}

	// (stored, probe, expected sign of compare(stored, probe))
	cases := []struct {
		stored interface{}
		probe  interface{}
		want   int
	}{
		{int64(5), int64(5), 0},
		{int64(4), int64(5), -1},
		{int64(6), int64(5), 1},
		{int64(-3), int64(2), -1},
		{int64(300), int64(5), 1},   // 2-byte serial vs 1-byte probe value
		{float64(5.0), int64(5), 0}, // REAL 5.0 == INT 5 (encodings differ)
		{float64(5.5), int64(5), 1}, // REAL > INT 5
		{int64(5), float64(5.0), 0}, // INT branch vs REAL probe
		{float64(4.5), float64(5.5), -1},
		{"abc", "abc", 0},
		{"abd", "abc", 1},
		{"ab", "abc", -1}, // shorter prefix sorts first (BINARY)
		{"abc", int64(5), 1},
		{int64(5), "abc", -1},
		{nil, nil, 0},
		{nil, int64(1), -1},
		{int64(1), nil, 1},
		{nil, "abc", -1},
		{"abc", nil, 1},
	}
	for _, tc := range cases {
		payload := encode(t, tc.stored)
		if got := signOf(mustCmp(t, payload, probeOf(tc.probe))); got != tc.want {
			t.Errorf("stored %v probe %v: got %d want %d", tc.stored, tc.probe, got, tc.want)
		}
	}

	// Blobs: stored blob vs blob probe (bytes + length tiebreak), and the
	// type ordering text < blob.
	blob := []byte{1, 2, 3}
	if got := signOf(mustCmp(t, encode(t, blob), NewUnpackedIndexKey(ki, []interface{}{blob}))); got != 0 {
		t.Errorf("blob equality: got %d want 0", got)
	}
	if got := signOf(mustCmp(t, encode(t, blob), NewUnpackedIndexKey(ki, []interface{}{[]byte{1, 2, 4}}))); got != -1 {
		t.Errorf("blob ordering: got %d want -1", got)
	}
	if got := signOf(mustCmp(t, encode(t, "text"), NewUnpackedIndexKey(ki, []interface{}{blob}))); got != -1 {
		t.Errorf("text vs blob: got %d want -1 (text < blob)", got)
	}

	// Stored NaN REAL: sorts before every numeric probe (C serialGet7 parity).
	nan := encode(t, math.NaN())
	if got := signOf(mustCmp(t, nan, probeOf(int64(1)))); got != -1 {
		t.Errorf("NaN stored vs int probe: got %d want -1", got)
	}
	if got := signOf(mustCmp(t, nan, probeOf(1.5))); got != -1 {
		t.Errorf("NaN stored vs real probe: got %d want -1", got)
	}
	// ... and compares equal to a NULL probe (C null branch parity).
	if got := signOf(mustCmp(t, nan, probeOf(nil))); got != 0 {
		t.Errorf("NaN stored vs NULL probe: got %d want 0", got)
	}
}

func TestIndexRecordCompare_Collations(t *testing.T) {
	encode := func(t *testing.T, v interface{}) []byte {
		t.Helper()
		b, err := storage.EncodeRecord([]interface{}{v})
		if err != nil {
			t.Fatalf("EncodeRecord(%v): %v", v, err)
		}
		return b
	}
	cases := []struct {
		coll   string
		stored string
		probe  string
		want   int
	}{
		{"BINARY", "ABC", "abc", -1},
		{"NOCASE", "ABC", "abc", 0},
		{"NOCASE", "abcd", "ABC", 1},
		{"RTRIM", "abc  ", "abc", 0},
		{"RTRIM", "abc", "abc  ", 0},
		{"RTRIM", "abd", "abc ", 1},
		{"", "abc", "abc", 0},
	}
	for _, tc := range cases {
		ki := NewKeyInfo(1, []string{tc.coll}, nil)
		payload := encode(t, tc.stored)
		cmp, err := IndexRecordCompare(payload, NewUnpackedIndexKey(ki, []interface{}{tc.probe}))
		if err != nil {
			t.Fatalf("coll %q stored %q probe %q: %v", tc.coll, tc.stored, tc.probe, err)
		}
		if got := signOf(cmp); got != tc.want {
			t.Errorf("coll %q stored %q probe %q: got %d want %d", tc.coll, tc.stored, tc.probe, got, tc.want)
		}
	}
}

func TestIndexRecordCompare_SortFlags(t *testing.T) {
	encode := func(t *testing.T, v interface{}) []byte {
		t.Helper()
		b, err := storage.EncodeRecord([]interface{}{v})
		if err != nil {
			t.Fatalf("EncodeRecord(%v): %v", v, err)
		}
		return b
	}
	// DESC negates the first differing field.
	asc := NewKeyInfo(1, nil, nil)
	desc := NewKeyInfo(1, nil, []byte{KeyInfoOrderDesc})
	payload := encode(t, int64(3))
	probe5 := NewUnpackedIndexKey(asc, []interface{}{int64(5)})
	if got := mustCmp(t, payload, probe5); got != -1 {
		t.Errorf("ASC stored 3 probe 5: got %d want -1", got)
	}
	if got := mustCmp(t, payload, NewUnpackedIndexKey(desc, []interface{}{int64(5)})); got != 1 {
		t.Errorf("DESC stored 3 probe 5: got %d want 1", got)
	}
	// Equality is unaffected by DESC.
	if got := mustCmp(t, payload, NewUnpackedIndexKey(desc, []interface{}{int64(3)})); got != 0 {
		t.Errorf("DESC stored 3 probe 3: got %d want 0", got)
	}
	// BIGNULL (NULLS LAST on ASC): a stored value sorts BEFORE a NULL probe.
	bignull := NewKeyInfo(1, nil, []byte{KeyInfoOrderBigNull})
	nullPayload := encode(t, nil)
	if got := mustCmp(t, encode(t, int64(3)), NewUnpackedIndexKey(bignull, []interface{}{nil})); got != -1 {
		t.Errorf("BIGNULL stored 3 probe NULL: got %d want -1", got)
	}
	if got := mustCmp(t, nullPayload, NewUnpackedIndexKey(bignull, []interface{}{int64(3)})); got != 1 {
		t.Errorf("BIGNULL stored NULL probe 3: got %d want 1", got)
	}
}

func TestIndexRecordCompare_PrefixFields(t *testing.T) {
	// A one-field probe decides on the record's FIRST field only (nField
	// prefix comparison, default_rc=0): trailing fields never affect it.
	rec, err := storage.EncodeRecord([]interface{}{int64(5), "x", int64(99)})
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	ki := singleKeyInfo()
	if got := mustCmp(t, rec, NewUnpackedIndexKey(ki, []interface{}{int64(5)})); got != 0 {
		t.Errorf("prefix match: got %d want 0", got)
	}
	if got := mustCmp(t, rec, NewUnpackedIndexKey(ki, []interface{}{int64(4)})); got != 1 {
		t.Errorf("prefix greater (stored 5 > probe 4): got %d want 1", got)
	}
	// Two-field probe: the second field decides.
	ki2 := NewKeyInfo(2, nil, nil)
	if got := mustCmp(t, rec, NewUnpackedIndexKey(ki2, []interface{}{int64(5), "y"})); got != -1 {
		t.Errorf("2-field probe: got %d want -1", got)
	}
	if got := mustCmp(t, rec, NewUnpackedIndexKey(ki2, []interface{}{int64(5), "x"})); got != 0 {
		t.Errorf("2-field probe equal: got %d want 0", got)
	}
}

func TestIndexRecordCompare_Errors(t *testing.T) {
	ki := singleKeyInfo()
	probe := NewUnpackedIndexKey(ki, []interface{}{"hello"})

	// Truncated body: the serial type promises 5 text bytes, 2 are present.
	full, err := storage.EncodeRecord([]interface{}{"hello"})
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	if _, err := IndexRecordCompare(full[:len(full)-3], probe); !errors.Is(err, ErrIndexRecordTruncated) {
		t.Errorf("truncated body: got %v want ErrIndexRecordTruncated", err)
	}
	if got := mustCmp(t, full, probe); got != 0 {
		t.Errorf("full payload: got %d want 0", got)
	}

	// Header larger than the payload.
	if _, err := IndexRecordCompare([]byte{0x09, 0x01}, probe); !errors.Is(err, ErrIndexRecordTruncated) {
		t.Errorf("oversize header: got %v want ErrIndexRecordTruncated", err)
	}

	// Truncated varint (0x80 continuation with no following byte).
	if _, err := IndexRecordCompare([]byte{0x80}, probe); !errors.Is(err, ErrIndexRecordCorrupt) {
		t.Errorf("truncated varint: got %v want ErrIndexRecordCorrupt", err)
	}

	// Fewer stored fields than the probe (1-byte header, zero fields).
	if _, err := IndexRecordCompare([]byte{0x01}, probe); !errors.Is(err, ErrIndexRecordCorrupt) {
		t.Errorf("empty header: got %v want ErrIndexRecordCorrupt", err)
	}

	// Unsupported probe value class.
	if _, err := IndexRecordCompare(full, NewUnpackedIndexKey(ki, []interface{}{struct{}{}})); !errors.Is(err, ErrIndexRecordCorrupt) {
		t.Errorf("unsupported probe: got %v want ErrIndexRecordCorrupt", err)
	}

	// nil probe / nil KeyInfo.
	if _, err := IndexRecordCompare(full, nil); err == nil {
		t.Errorf("nil probe: want error")
	}
}

// TestIndexKeyRowIDs_ByteOrderInterleaved proves the seek finds value-equal
// entries that byte order SCATTERS: with probe 'a', the entries ('a',5) and
// ('a',300) sit at byte positions ('a',5) < ('aa',5) < ('b',5) < ('a',300)
// — a binary byte seek would find at most one of them.
func TestIndexKeyRowIDs_ByteOrderInterleaved(t *testing.T) {
	bt := newIndexTree(t)
	insertIndexRecord(t, bt, []interface{}{"a", int64(5)})
	insertIndexRecord(t, bt, []interface{}{"aa", int64(7)})
	insertIndexRecord(t, bt, []interface{}{"b", int64(9)})
	insertIndexRecord(t, bt, []interface{}{"a", int64(300)})
	insertIndexRecord(t, bt, []interface{}{"aardvark", int64(11)})

	got, err := bt.IndexKeyRowIDs(NewUnpackedIndexKey(singleKeyInfo(), []interface{}{"a"}))
	if err != nil {
		t.Fatalf("IndexKeyRowIDs: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []int64{5, 300}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("probe 'a': got %v want %v", got, want)
	}

	// int/real encoding split: probe int 5 must find stored REAL 5.0.
	bt2 := newIndexTree(t)
	insertIndexRecord(t, bt2, []interface{}{float64(5.0), int64(1)})
	insertIndexRecord(t, bt2, []interface{}{float64(5.5), int64(2)})
	insertIndexRecord(t, bt2, []interface{}{int64(500), int64(3)})
	got2, err := bt2.IndexKeyRowIDs(NewUnpackedIndexKey(singleKeyInfo(), []interface{}{int64(5)}))
	if err != nil {
		t.Fatalf("IndexKeyRowIDs(int 5): %v", err)
	}
	if len(got2) != 1 || got2[0] != 1 {
		t.Errorf("probe int 5: got %v want [1] (REAL 5.0 matches)", got2)
	}
}

// TestIndexKeyRowIDs_OverflowSpill exercises the truncated-local-payload
// path: long text values spill to overflow pages, so the local fragment can
// never decide the comparison on its own.
func TestIndexKeyRowIDs_OverflowSpill(t *testing.T) {
	bt := newIndexTree(t)
	long := make([]byte, 5000)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	target := string(long) + "-tail"
	insertIndexRecord(t, bt, []interface{}{target, int64(42)})
	insertIndexRecord(t, bt, []interface{}{string(long) + "-other", int64(43)})
	insertIndexRecord(t, bt, []interface{}{"short", int64(44)})

	got, err := bt.IndexKeyRowIDs(NewUnpackedIndexKey(singleKeyInfo(), []interface{}{target}))
	if err != nil {
		t.Fatalf("IndexKeyRowIDs: %v", err)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("spilled probe: got %v want [42]", got)
	}
}

// TestSeekIndexKey_PositionsAndContinues checks the cursor contract: the
// first equal entry is positioned (multi-level tree, so the path stack is
// exercised) and Next()+comparator continues through the remaining matches.
// Because byte order scatters value-equal entries (the design note's
// counter-example is live here: "target" sorts before "zz-late" sorts
// before "aa-early" by serial-type magnitude), the continuation re-compare
// skips non-matches and runs to end-of-tree.
func TestSeekIndexKey_PositionsAndContinues(t *testing.T) {
	bt := newIndexTree(t)
	want := []int64{}
	for i := int64(1); i <= 400; i++ {
		key := "aa-early"
		if i%3 == 0 {
			key = "target"
			want = append(want, i)
		} else if i%3 == 1 {
			key = "zz-late"
		}
		insertIndexRecord(t, bt, []interface{}{key, i})
	}
	probe := NewUnpackedIndexKey(singleKeyInfo(), []interface{}{"target"})
	cursor, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	found, err := cursor.SeekIndexKey(probe)
	if err != nil {
		t.Fatalf("SeekIndexKey: %v", err)
	}
	if !found {
		t.Fatalf("SeekIndexKey: no match found")
	}
	if payload, _, err := cursor.ReadCellData(); err != nil {
		t.Fatalf("ReadCellData at seek position: %v", err)
	} else if cmp, cerr := IndexRecordCompare(payload, probe); cerr != nil || cmp != 0 {
		t.Fatalf("seek position is not a match (cmp=%d err=%v)", cmp, cerr)
	}
	var got []int64
	for {
		payload, _, err := cursor.ReadCellData()
		if err != nil {
			t.Fatalf("ReadCellData: %v", err)
		}
		if cmp, cerr := IndexRecordCompare(payload, probe); cerr != nil {
			t.Fatalf("IndexRecordCompare: %v", cerr)
		} else if cmp == 0 {
			rid, rerr := indexRecordRowID(payload)
			if rerr != nil {
				t.Fatalf("indexRecordRowID: %v", rerr)
			}
			got = append(got, rid)
		}
		more, err := cursor.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !more {
			break
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != len(want) {
		t.Fatalf("continuation: got %d matches want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("match[%d]: got %d want %d", i, got[i], want[i])
		}
	}

	// A probe with no match reports false and does not corrupt the tree.
	missed, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	if ok, err := missed.SeekIndexKey(NewUnpackedIndexKey(singleKeyInfo(), []interface{}{"no-such-key"})); ok || err != nil {
		t.Errorf("SeekIndexKey(miss): got %v, %v want false, nil", ok, err)
	}
}

// TestIndexKeyRowIDs_RandomTreeOracle compares the seek against a
// brute-force decode+CompareValues scan over a randomized multi-value tree.
func TestIndexKeyRowIDs_RandomTreeOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(20260922))
	pool := []interface{}{
		int64(-5), int64(0), int64(7), int64(300), int64(1 << 40),
		float64(0.5), float64(-2.25), float64(7.0),
		"a", "aa", "ab", "b", "", "key",
		nil,
	}
	bt := newIndexTree(t)
	type row struct {
		key   interface{}
		rowid int64
	}
	rows := make([]row, 0, 300)
	for i := int64(1); i <= 300; i++ {
		key := pool[rng.Intn(len(pool))]
		rows = append(rows, row{key, i})
		insertIndexRecord(t, bt, []interface{}{key, i})
	}

	ki := singleKeyInfo()
	for _, p := range pool {
		probe := NewUnpackedIndexKey(ki, []interface{}{p})
		got, err := bt.IndexKeyRowIDs(probe)
		if err != nil {
			t.Fatalf("IndexKeyRowIDs(%v): %v", p, err)
		}
		var want []int64
		for _, r := range rows {
			if util.CompareValues(util.UnwrapColumnValue(r.key), p) == 0 {
				want = append(want, r.rowid)
			}
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if len(got) != len(want) {
			t.Fatalf("probe %v (%T): got %v want %v", p, p, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("probe %v: got[%d]=%d want %d", p, i, got[i], want[i])
			}
		}
	}
}

// TestIndexKeyRowIDs_MultiLevelTree keeps the oracle honest across splits
// (leaf + interior levels) with duplicate keys and larger records.
func TestIndexKeyRowIDs_MultiLevelTree(t *testing.T) {
	bt := newIndexTree(t)
	want := []int64{}
	for i := int64(1); i <= 800; i++ {
		key := fmt.Sprintf("row-%04d", i)
		if i%17 == 0 {
			want = append(want, i)
			key = "hit" // duplicate non-unique keys, scattered by payload bytes
		}
		insertIndexRecord(t, bt, []interface{}{key, fmt.Sprintf("payload-%d", i*i), i})
	}
	got, err := bt.IndexKeyRowIDs(NewUnpackedIndexKey(NewKeyInfo(1, nil, nil), []interface{}{"hit"}))
	if err != nil {
		t.Fatalf("IndexKeyRowIDs: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != len(want) {
		t.Fatalf("got %d hits want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hit[%d]: got %d want %d", i, got[i], want[i])
		}
	}
}
