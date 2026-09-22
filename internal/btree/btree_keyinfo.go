// Index-key comparison infrastructure: a KeyInfo-carrying record comparator
// that ports sqlite3VdbeRecordCompare (src/vdbeaux.c) to frigolite's encoded
// index payloads. P9.PERF.T3.
//
// The engine's index b-trees are stored in RAW PAYLOAD BYTE order
// (findInsertPositionIndex binary-searches with bytes.Compare), which is NOT
// value order: value-equal entries are not byte-contiguous, so no binary
// value seek can be sound on such trees. This file provides the VALUE-order
// comparison (what SQLite orders its index trees by) as the comparison
// primitive for the seek walk in btree_indexseek.go — and as the seam a
// future value-ordered storage tranche flips from walk to binary descent.
//
// C ground truth (vdbeaux.c):
//   - sqlite3VdbeRecordCompare / sqlite3VdbeRecordCompareWithSkip: field-wise
//     comparison of an unpacked probe against a packed record, decoding the
//     stored field lazily from its serial type + body bytes.
//   - KeyInfo (sqliteInt.h:2664): nKeyField, per-field aColl[], per-field
//     aSortFlags[] (KEYINFO_ORDER_DESC 0x01, KEYINFO_ORDER_BIGNULL 0x02).
//   - Fields that appear in both keys equal → default_rc (0 for exact seeks).

package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"

	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// Sort-order flag bits per KeyInfo key column (sqliteInt.h KEYINFO_ORDER_*).
const (
	// KeyInfoOrderDesc marks a DESC sort key (KEYINFO_ORDER_DESC).
	KeyInfoOrderDesc = 0x01
	// KeyInfoOrderBigNull marks a "NULL is larger than any other value"
	// sort key (KEYINFO_ORDER_BIGNULL).
	KeyInfoOrderBigNull = 0x02
)

// ErrIndexRecordCorrupt reports a record the comparator cannot parse (a
// header that does not fit the payload, a truncated serial-type varint, a
// body shorter than its serial type demands, a reserved serial type, or
// fewer stored fields than the probe). SQLite's comparator sets
// errCode=SQLITE_CORRUPT and returns 0; frigolite's seek callers surface
// this to their own corrupt/fallback handling.
var ErrIndexRecordCorrupt = errors.New("btree: corrupt index record")

// ErrIndexRecordTruncated reports that the comparison needs payload bytes
// beyond the end of the slice it was handed — the caller passed a local
// fragment of a record whose declared body spills further (overflow pages
// or a clipped cell). The seek walk resolves it by re-comparing against the
// reassembled payload; for an already-complete payload it means corruption.
var ErrIndexRecordTruncated = errors.New("btree: index record payload truncated")

// KeyInfo carries the comparison configuration of an index b-tree's keys:
// the mirror of sqlite3's KeyInfo (sqliteInt.h:2664) for the engine's index
// trees. NKeyField counts the KEY columns of the index (an index record's
// trailing rowid element is not a key); Collations holds one name per key
// column ("" = BINARY); SortFlags holds one flag byte per key column
// (KeyInfoOrderDesc / KeyInfoOrderBigNull).
type KeyInfo struct {
	NKeyField  int
	Collations []string
	SortFlags  []byte
}

// NewKeyInfo builds a KeyInfo for nKey key columns with the given per-column
// collations ("" entries mean BINARY) and sort flags (nil = all ASC).
func NewKeyInfo(nKey int, collations []string, sortFlags []byte) *KeyInfo {
	ki := &KeyInfo{NKeyField: nKey, Collations: make([]string, nKey), SortFlags: make([]byte, nKey)}
	for i := 0; i < nKey; i++ {
		if i < len(collations) {
			ki.Collations[i] = collations[i]
		}
		if i < len(sortFlags) {
			ki.SortFlags[i] = sortFlags[i]
		}
	}
	return ki
}

// UnpackedIndexKey is a decoded probe key: the mirror of sqlite3's
// UnpackedRecord restricted to what an index seek needs — the probe values
// for the FIRST len(Values) fields of the stored records (nField), compared
// under the KeyInfo's per-column collations and sort orders. Equality over
// the probed prefix (default_rc = 0) is the seek's match condition.
type UnpackedIndexKey struct {
	KeyInfo *KeyInfo
	Values  []interface{} // per field: int64 | float64 | string | []byte | nil
}

// NewUnpackedIndexKey builds a probe over values with a shared KeyInfo.
func NewUnpackedIndexKey(ki *KeyInfo, values []interface{}) *UnpackedIndexKey {
	return &UnpackedIndexKey{KeyInfo: ki, Values: values}
}

// collationOf returns the collation name of key column i ("" = BINARY).
func (ki *KeyInfo) collationOf(i int) string {
	if i < len(ki.Collations) {
		return ki.Collations[i]
	}
	return ""
}

// sortFlagOf returns the sort flags of key column i.
func (ki *KeyInfo) sortFlagOf(i int) byte {
	if i < len(ki.SortFlags) {
		return ki.SortFlags[i]
	}
	return 0
}

// IndexRecordCompare compares the packed record payload against the unpacked
// probe over len(probe.Values) fields, sqlite3VdbeRecordCompareWithSkip
// semantics (vdbeaux.c:4709, bSkip=0, default_rc=0). Returns a negative
// value when the stored record's probed prefix sorts before the probe, zero
// when they are equal (the seek match condition), positive when after.
//
// The stored fields are decoded lazily from their serial types + body bytes
// (no full record decode); the probe's class drives each field's branch:
// NULL < INTEGER/REAL < TEXT < BLOB, INTEGER vs REAL compared numerically
// (so int 5 == real 5.0 — encodings differ, values do not), TEXT under the
// field's collation, blobs by bytes with a length tiebreak.
//
// Errors: ErrIndexRecordTruncated when the probed fields' bytes extend past
// len(payload) (callers holding a local fragment re-compare with the full
// payload — for an already-complete payload this means corruption);
// ErrIndexRecordCorrupt for malformed records.
func IndexRecordCompare(payload []byte, probe *UnpackedIndexKey) (int, error) {
	nField, err := probeFieldCount(probe)
	if err != nil {
		return 0, err
	}
	if nField == 0 {
		return 0, nil // no probed fields: default_rc (0)
	}
	hdrSize, idx, herr := indexRecordHeader(payload)
	if herr != nil {
		return 0, herr
	}
	d := hdrSize // offset of the next body byte (C: d1)

	for i := 0; i < nField; i++ {
		st, serr := nextRecordSerialType(payload, hdrSize, &idx)
		if serr != nil {
			return 0, serr
		}
		rc, isNaNStored, cerr := compareRecordField(st, payload[d:], probe.Values[i], probe.KeyInfo.collationOf(i))
		if cerr != nil {
			return 0, cerr
		}
		if rc != 0 {
			return applySortFlags(rc, st, probe, i, isNaNStored), nil
		}
		// Advance past this field (C: d1 += serialTypeLen; the idx1 extent
		// check happens at the next loop head inside nextRecordSerialType).
		stLen, aerr := storage.SerialTypeLength(st)
		if aerr != nil {
			return 0, ErrIndexRecordCorrupt
		}
		d += int(stLen)
		if d > len(payload) {
			break // stored record ran out of fields: default_rc (0)
		}
	}
	return 0, nil
}

// probeFieldCount validates a seek probe and returns its field count.
func probeFieldCount(probe *UnpackedIndexKey) (int, error) {
	if probe == nil || probe.KeyInfo == nil {
		return 0, ErrIndexRecordCorrupt
	}
	return len(probe.Values), nil
}

// nextRecordSerialType reads the serial-type varint at *idx, bounds-checked
// against the header extent hdrSize (C: idx1 >= szHdr1 → corrupt), and
// advances *idx past it.
func nextRecordSerialType(payload []byte, hdrSize int, idx *int) (uint64, error) {
	if *idx >= hdrSize || *idx >= len(payload) {
		return 0, ErrIndexRecordCorrupt // fewer stored fields than probed
	}
	st, n := util.GetVarint(payload[*idx:])
	if n == 0 {
		return 0, ErrIndexRecordCorrupt
	}
	*idx += n
	return st, nil
}

// indexRecordHeader parses a record's header-size varint and returns the
// header extent and the offset of the first serial-type varint. See
// IndexRecordCompare for the error semantics.
func indexRecordHeader(payload []byte) (hdrSize, idx int, err error) {
	szHdr, n := util.GetVarint(payload)
	if n == 0 || szHdr == 0 {
		// A truncated size varint decodes to (0, n>0); n==0 is the empty
		// buffer. Either way no legal record header is here.
		return 0, 0, ErrIndexRecordCorrupt
	}
	hdrSize = int(szHdr)
	if hdrSize > len(payload) {
		// The declared header does not fit the bytes at hand: unresolved
		// against a local fragment, corrupt against a complete payload.
		return 0, 0, ErrIndexRecordTruncated
	}
	if hdrSize < n {
		return 0, 0, ErrIndexRecordCorrupt // smaller than its own size varint
	}
	return hdrSize, n, nil
}

// applySortFlags mirrors vdbeaux.c's sortFlags block: a DESC flag negates
// the first differing field's result; KEYINFO_ORDER_BIGNULL keeps NULL
// larger than every other value (negation is skipped when the column's
// DESC-ness equals the null-ness of the compared pair — C compares
// "(sortFlags & DESC) != (serial_type==0 || pRhs is NULL)").
func applySortFlags(rc int, st uint64, probe *UnpackedIndexKey, i int, _ bool) int {
	flags := probe.KeyInfo.sortFlagOf(i)
	if flags == 0 {
		return rc
	}
	storedNull := st == 0
	probeNull := probe.Values[i] == nil
	if flags&KeyInfoOrderBigNull == 0 || (flags&KeyInfoOrderDesc != 0) != (storedNull || probeNull) {
		rc = -rc
	}
	return rc
}

// compareRecordField compares one stored field (serial type st, body bytes)
// against one probe value, driven by the probe's class exactly as
// sqlite3VdbeRecordCompareWithSkip's per-Mem-flag branches are. The second
// return reports a stored NaN REAL (serial 7); the caller folds it into the
// BIGNULL sort handling.
func compareRecordField(st uint64, body []byte, probe interface{}, collation string) (int, bool, error) {
	switch v := probe.(type) {
	case nil:
		return nullProbeVsStored(st, body)
	case int64:
		return intProbeVsStored(st, body, v)
	case float64:
		return realProbeVsStored(st, body, v)
	case string:
		return compareStoredText(st, body, v, collation)
	case []byte:
		return compareStoredBlob(st, body, v)
	default:
		return 0, false, ErrIndexRecordCorrupt
	}
}

// nullProbeVsStored is the RHS null branch: stored NULL, reserved 10, or a
// NaN REAL compare equal; everything else sorts after the NULL probe.
func nullProbeVsStored(st uint64, body []byte) (int, bool, error) {
	if st == 0 || st == 10 {
		return 0, false, nil
	}
	if st == 7 {
		r, ok := storedFloat(body)
		if !ok {
			return 0, false, ErrIndexRecordTruncated
		}
		if math.IsNaN(r) {
			return 0, true, nil
		}
	}
	return 1, false, nil
}

// numericRankVsStored is the shared head of the numeric probe branches: a
// stored TEXT/BLOB (serial ≥ 12) sorts after, reserved 10 before, reserved
// 11 after, stored NULL before. done=false leaves the numeric comparison to
// the caller.
func numericRankVsStored(st uint64) (rc int, done bool) {
	if st >= 10 {
		if st == 10 {
			return -1, true
		}
		return 1, true
	}
	if st == 0 {
		return -1, true
	}
	return 0, false
}

// intProbeVsStored is the RHS integer branch (vdbeaux.c MEM_Int): a stored
// REAL compares through sqlite3IntFloatCompare (INT 5 == REAL 5.0), stored
// integers directly.
func intProbeVsStored(st uint64, body []byte, probe int64) (int, bool, error) {
	if rc, done := numericRankVsStored(st); done {
		return rc, false, nil
	}
	if st == 7 {
		r, ok := storedFloat(body)
		if !ok {
			return 0, false, ErrIndexRecordTruncated
		}
		return -intFloatCompare(probe, r), math.IsNaN(r), nil
	}
	lhs, ok, err := storedInt(st, body)
	if err != nil || !ok {
		return 0, false, err
	}
	switch {
	case lhs < probe:
		return -1, false, nil
	case lhs > probe:
		return 1, false, nil
	}
	return 0, false, nil
}

// realProbeVsStored is the RHS real branch (vdbeaux.c MEM_Real): a stored
// NaN sorts before every real probe; stored integers compare through
// sqlite3IntFloatCompare.
func realProbeVsStored(st uint64, body []byte, probe float64) (int, bool, error) {
	if rc, done := numericRankVsStored(st); done {
		return rc, false, nil
	}
	if st == 7 {
		r, ok := storedFloat(body)
		if !ok {
			return 0, false, ErrIndexRecordTruncated
		}
		if math.IsNaN(r) {
			return -1, true, nil
		}
		switch {
		case r < probe:
			return -1, false, nil
		case r > probe:
			return 1, false, nil
		}
		return 0, false, nil
	}
	lhs, ok, err := storedInt(st, body)
	if err != nil || !ok {
		return 0, false, err
	}
	return intFloatCompare(lhs, probe), false, nil
}

// compareStoredText is the RHS string branch: stored numerics/NULL sort
// before text, stored blobs after; stored text compares under the field's
// collation with a length tiebreak.
func compareStoredText(st uint64, body []byte, probe string, collation string) (int, bool, error) {
	if st < 12 {
		return -1, false, nil
	}
	if st%2 == 0 {
		return 1, false, nil
	}
	nStr := int((st - 12) / 2)
	if nStr > len(body) {
		return 0, false, ErrIndexRecordTruncated
	}
	stored := body[:nStr]
	switch collation {
	case "NOCASE":
		return nocaseCompare(stored, probe), false, nil
	case "RTRIM":
		return rtrimCompare(stored, probe), false, nil
	default:
		return binaryTextCompare(stored, probe), false, nil
	}
}

// compareStoredBlob is the RHS blob branch: stored numerics/NULL/text sort
// before blobs; stored blobs compare by bytes then length.
func compareStoredBlob(st uint64, body []byte, probe []byte) (int, bool, error) {
	if st < 12 || st%2 != 0 {
		return -1, false, nil
	}
	nStr := int((st - 12) / 2)
	if nStr > len(body) {
		return 0, false, ErrIndexRecordTruncated
	}
	nCmp := nStr
	if len(probe) < nCmp {
		nCmp = len(probe)
	}
	if c := bytes.Compare(body[:nCmp], probe[:nCmp]); c != 0 {
		return c, false, nil
	}
	return nStr - len(probe), false, nil
}

// binaryTextCompare is memcmp over the common prefix then shorter-first
// (vdbeaux.c: rc = memcmp(...); if( rc==0 ) rc = nStr - pRhs->n).
func binaryTextCompare(stored []byte, probe string) int {
	nCmp := len(stored)
	if len(probe) < nCmp {
		nCmp = len(probe)
	}
	for i := 0; i < nCmp; i++ {
		if stored[i] != probe[i] {
			if stored[i] < probe[i] {
				return -1
			}
			return 1
		}
	}
	return len(stored) - len(probe)
}

// nocaseCompare folds ASCII 'A'-'Z' down on both sides (sqlite3UpperToLower
// via value.SQLiteAsciiToLower semantics) and compares BINARY.
func nocaseCompare(stored []byte, probe string) int {
	nCmp := len(stored)
	if len(probe) < nCmp {
		nCmp = len(probe)
	}
	for i := 0; i < nCmp; i++ {
		a, b := stored[i], probe[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return len(stored) - len(probe)
}

// rtrimCompare ignores trailing spaces on both sides (RTRIM collation).
func rtrimCompare(stored []byte, probe string) int {
	for len(stored) > 0 && stored[len(stored)-1] == ' ' {
		stored = stored[:len(stored)-1]
	}
	s := probe
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return binaryTextCompare(stored, s)
}

// storedInt decodes an integer serial type's big-endian two's complement
// body (vdbeRecordDecodeInt: serials 1-6 sign-extended, 8 → 0, 9 → 1).
// ok=false with ErrIndexRecordCorrupt for a non-integer serial type;
// ok=false with ErrIndexRecordTruncated for a short body.
func storedInt(st uint64, body []byte) (int64, bool, error) {
	switch st {
	case 8:
		return 0, true, nil
	case 9:
		return 1, true, nil
	}
	if st < 1 || st > 6 {
		return 0, false, ErrIndexRecordCorrupt
	}
	n := int(st)
	if st == 5 {
		n = 6
	} else if st == 6 {
		n = 8
	}
	if len(body) < n {
		return 0, false, ErrIndexRecordTruncated
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(body[i])
	}
	// Sign-extend from the serial type's width.
	shift := uint(8 * (8 - n))
	return int64(v<<shift) >> shift, true, nil
}

// storedFloat decodes a REAL serial-7 body (big-endian IEEE 754 double).
// ok=false when truncated or when the serial type is not 7.
func storedFloat(body []byte) (float64, bool) {
	if len(body) < 8 {
		return 0, false
	}
	return math.Float64frombits(binary.BigEndian.Uint64(body)), true
}

// intFloatCompare is sqlite3IntFloatCompare (src/util.c): exact int64 vs
// float64 comparison without precision loss; NaN sorts after every int64
// and the C integer branch negates the result (rc = -compare), which this
// file's integer branch already accounts for.
func intFloatCompare(i int64, r float64) int {
	if math.IsNaN(r) {
		return 1
	}
	if r < -9223372036854775808.0 {
		return 1 // r < i
	}
	if r == -9223372036854775808.0 {
		if i == math.MinInt64 {
			return 0
		}
		return 1
	}
	if r > 9223372036854775808.0 {
		return -1 // r > i
	}
	if r == 9223372036854775808.0 {
		return -1
	}
	y := int64(r)
	if i < y {
		return -1
	}
	if i > y {
		return 1
	}
	if float64(i) < r {
		return -1
	}
	if float64(i) > r {
		return 1
	}
	return 0
}
