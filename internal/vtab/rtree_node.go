package vtab

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/value"
)

// RTREE_MAXCELLS caps the number of cells per r-tree node (rtree.c
// RTREE_MAXCELLS). The node-size is capped so every node fits in one database
// page and holds at most this many cells.
const RTREE_MAXCELLS = 51

// RTREE_MAX_DEPTH bounds tree height (rtree.c RTREE_MAX_DEPTH).
const RTREE_MAX_DEPTH = 40

// RTREE_MAX_DIMENSIONS is the largest supported coordinate dimension count.
const RTREE_MAX_DIMENSIONS = 5

// RtreeCell is one deserialized node cell: the rowid (for leaf entries) or child
// node number (for internal entries) followed by its nDim2 bounding-box
// coordinates (min0,max0,min1,max1,...).
type RtreeCell[T coordType] struct {
	iRowid int64
	aCoord []T
}

// rtreeNode is an in-memory r-tree node: the on-disk blob (data) plus the
// bookkeeping SQLite keeps in RtreeNode (node number, dirty flag, parent link,
// reference count). byte 0..1 of data hold the node depth (root only); byte
// 2..3 hold the cell count (NCELL). Cells begin at byte 4.
type rtreeNode[T coordType] struct {
	iNode  int64
	dead   bool // removed from tree; destined for reinsertion, never flushed
	dirty  bool
	nRef   int
	data   []byte
	parent *rtreeNode[T] // best-known parent; %_parent table is authoritative
}

// nCell reads NCELL from data[2..3] (big-endian int16, matching SQLite's
// on-disk node layout).
func (n *rtreeNode[T]) nCell() int { return int(int16(binary.BigEndian.Uint16(n.data[2:4]))) }

// setNCell writes NCELL.
func (n *rtreeNode[T]) setNCell(c int) { binary.BigEndian.PutUint16(n.data[2:4], uint16(c)) }

// depth reads the node depth stored in data[0..1] (root only).
func (n *rtreeNode[T]) depth() int { return int(int16(binary.BigEndian.Uint16(n.data[0:2]))) }

// setDepth writes the node depth.
func (n *rtreeNode[T]) setDepth(d int) { binary.BigEndian.PutUint16(n.data[0:2], uint16(d)) }

// ---- coordinate serialization (generic over T ∈ {float32,int32}) ----

// rtreeReadCoord reads one 4-byte coordinate from p (big-endian, matching
// SQLite's on-disk rtree coordinate order for cross-engine blob parity).
func rtreeReadCoord[T coordType](p []byte) T {
	var z T
	switch any(z).(type) {
	case float32:
		return T(math.Float32frombits(binary.BigEndian.Uint32(p)))
	case int32:
		return T(int32(binary.BigEndian.Uint32(p)))
	}
	return z
}

// rtreeWriteCoord stores one coordinate v at p as a 4-byte big-endian value.
func rtreeWriteCoord[T coordType](p []byte, v T) {
	switch any(v).(type) {
	case float32:
		binary.BigEndian.PutUint32(p, math.Float32bits(float32(any(v).(float32))))
	case int32:
		binary.BigEndian.PutUint32(p, uint32(int32(any(v).(int32))))
	}
}

// asFloat64 widens a coordinate scalar to float64 for geometry math (matches
// SQLite's RtreeDValue computations, which are exact for integer coordinates).
func asFloat64[T coordType](v T) float64 {
	switch any(v).(type) {
	case float32:
		return float64(float32(any(v).(float32)))
	case int32:
		return float64(int32(any(v).(int32)))
	}
	return 0
}

// rtreeNumericPrefix parses a leading decimal number from s (SQLite's
// sqlite3AtoF/sqlite3Atoi64 semantics for coercing text into rtree id and
// coordinate columns): leading whitespace skipped, optional sign, digits with
// optional fraction/exponent. Returns 0 when no numeric prefix exists.
func rtreeNumericPrefix(s string) float64 {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 || i == start || (i == start+1 && (s[start] == '+' || s[start] == '-')) {
		return 0
	}
	man := s[:i]
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		expDigits := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			expDigits++
		}
		if expDigits > 0 {
			man = s[:j]
		}
	}
	v, err := strconv.ParseFloat(man, 64)
	if err != nil {
		return 0
	}
	return v
}

// toCoord narrows an arbitrary SQL value to coordinate scalar T. Integer and
// real literals both fold into the stored float32/int32 type; TEXT values use
// SQLite's numeric-prefix coercion ("52xyz" → 52, "one" → 0).
func toCoord[T coordType](v interface{}) T {
	var z T
	switch any(z).(type) {
	case float32:
		switch x := v.(type) {
		case float64:
			return T(float32(x))
		case int64:
			return T(float32(x))
		case string:
			return T(float32(rtreeNumericPrefix(x)))
		case nil:
			return T(float32(0))
		}
		return T(float32(0))
	case int32:
		switch x := v.(type) {
		case int64:
			return T(int32(x))
		case float64:
			return T(int32(x))
		case string:
			return T(int32(rtreeNumericPrefix(x)))
		case nil:
			return T(int32(0))
		}
		return T(int32(0))
	}
	return z
}

// ---- shadow-table name + raw IO (mirrors rtreeSqlInit's 8 statements) ----

// shadow returns the fully-qualified shadow-table name (e.g. "main"."r_node").
// Embedded double quotes are doubled so identifiers like raisara "one"' stay
// legal SQL text.
func (v *rtreeVTab[T]) shadow(suffix string) string {
	tbl := quoteName(v.name + "_" + suffix)
	if v.dbName != "" && v.dbName != "main" {
		return quoteName(v.dbName) + "." + tbl
	}
	return tbl
}

// asBytes normalizes a returned SQL column to a byte slice (blobs arrive as
// []byte; tolerate string; expand the lazy zeroblob wrapper like
// sqlite3_value_blob's MEM_Zero expansion).
func asBytes(v interface{}) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	case value.ZeroBlob:
		return x.Bytes()
	}
	return nil
}

// rtreeAsInt64 normalizes a returned SQL column to int64.
func rtreeAsInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int32:
		return int64(x)
	}
	return 0
}

// loadNodeBlob reads the on-disk node blob for nodeno.
func (v *rtreeVTab[T]) loadNodeBlob(nodeno int64) ([]byte, error) {
	rows, err := v.module.db.ExecSQL(fmt.Sprintf("SELECT data FROM %s WHERE nodeno=%d", v.shadow("node"), nodeno))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		// Missing node: rtree.c's sqlite3_blob_open fails with SQLITE_ERROR,
		// upgraded to SQLITE_CORRUPT_VTAB ("database disk image is malformed").
		return nil, errCapitalized{"database disk image is malformed"}
	}
	b := asBytes(rows[0][0])
	if b == nil {
		return nil, fmt.Errorf("rtree: node %d has no blob", nodeno)
	}
	return b, nil
}

// storeNodeBlob writes (INSERT OR REPLACE) the node blob for nodeno. A nodeno of
// 0 means "allocate a fresh number" (max+1 of existing nodes).
func (v *rtreeVTab[T]) storeNodeBlob(nodeno int64, data []byte) (int64, error) {
	if nodeno == 0 {
		mx, err := v.maxNodeNumber()
		if err != nil {
			return 0, err
		}
		nodeno = mx + 1
	}
	hexStr := fmt.Sprintf("%x", data)
	sql := fmt.Sprintf("INSERT OR REPLACE INTO %s(nodeno,data) VALUES(%d, X'%s')", v.shadow("node"), nodeno, hexStr)
	if _, err := v.module.db.ExecSQL(sql); err != nil {
		return 0, err
	}
	return nodeno, nil
}

// maxNodeNumber returns the largest existing node number (0 when empty).
func (v *rtreeVTab[T]) maxNodeNumber() (int64, error) {
	rows, err := v.module.db.ExecSQL(fmt.Sprintf("SELECT coalesce(max(nodeno),0) FROM %s", v.shadow("node")))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0, nil
	}
	return rtreeAsInt64(rows[0][0]), nil
}

// setRowidMapping records rowid -> nodeno (leaf entries) in %_rowid.
func (v *rtreeVTab[T]) setRowidMapping(rowid, nodeno int64) error {
	sql := fmt.Sprintf("INSERT OR REPLACE INTO %s(rowid,nodeno) VALUES(%d,%d)", v.shadow("rowid"), rowid, nodeno)
	_, err := v.module.db.ExecSQL(sql)
	return err
}

// getRowidNode returns the node number storing rowid, and whether it exists.
func (v *rtreeVTab[T]) getRowidNode(rowid int64) (int64, bool, error) {
	rows, err := v.module.db.ExecSQL(fmt.Sprintf("SELECT nodeno FROM %s WHERE rowid=%d", v.shadow("rowid"), rowid))
	if err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	return rtreeAsInt64(rows[0][0]), true, nil
}

// delRowidMapping deletes the %_rowid entry for rowid.
func (v *rtreeVTab[T]) delRowidMapping(rowid int64) error {
	_, err := v.module.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE rowid=%d", v.shadow("rowid"), rowid))
	return err
}

// setParent records nodeno -> parentnode in %_parent.
func (v *rtreeVTab[T]) setParent(nodeno, parent int64) error {
	sql := fmt.Sprintf("INSERT OR REPLACE INTO %s(nodeno,parentnode) VALUES(%d,%d)", v.shadow("parent"), nodeno, parent)
	_, err := v.module.db.ExecSQL(sql)
	return err
}

// getParent returns the parent node of nodeno, and whether it exists.
func (v *rtreeVTab[T]) getParent(nodeno int64) (int64, bool, error) {
	rows, err := v.module.db.ExecSQL(fmt.Sprintf("SELECT parentnode FROM %s WHERE nodeno=%d", v.shadow("parent"), nodeno))
	if err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	return rtreeAsInt64(rows[0][0]), true, nil
}

// delParent removes the %_parent entry for nodeno.
func (v *rtreeVTab[T]) delParent(nodeno int64) error {
	_, err := v.module.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE nodeno=%d", v.shadow("parent"), nodeno))
	return err
}

// maxRowid returns the largest existing entry rowid in %_rowid (0 when empty).
func (v *rtreeVTab[T]) maxRowid() (int64, error) {
	rows, err := v.module.db.ExecSQL(fmt.Sprintf("SELECT coalesce(max(rowid),0) FROM %s", v.shadow("rowid")))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0, nil
	}
	return rtreeAsInt64(rows[0][0]), nil
}

// ---- node cache + memory node management ----

// newNodeCache clears the per-operation in-memory node cache so every operation
// re-reads node blobs from the shadow tables (avoids cross-statement staleness).
func (v *rtreeVTab[T]) newNodeCache() { v.cache = make(map[int64]*rtreeNode[T]) }

// nodeAcquire returns the node numbered iNode, loading its blob from the shadow
// table on a cache miss. The returned node carries a borrowed reference.
func (v *rtreeVTab[T]) nodeAcquire(iNode int64) (*rtreeNode[T], error) {
	if n, ok := v.cache[iNode]; ok {
		n.nRef++
		return n, nil
	}
	blob, err := v.loadNodeBlob(iNode)
	if err != nil {
		return nil, err
	}
	// Undersize blobs cannot carry the 4-byte header; reject up front instead
	// of panicking on the first coordinate read. A blob whose size differs
	// from the instance's node size is equally unreadable: rtree.c's
	// nodeBlob only loads when sqlite3_blob_bytes == iNodeSize, leaving any
	// mismatch to surface as SQLITE_CORRUPT_VTAB ("database disk image is
	// malformed") — the corruption-test parity rtree8-2.x/3.x asserts.
	if len(blob) != v.iNodeSize {
		return nil, errCapitalized{"database disk image is malformed"}
	}
	n := &rtreeNode[T]{iNode: iNode, nRef: 1, data: append([]byte(nil), blob...)}
	v.cache[iNode] = n
	if iNode == 1 {
		v.iDepth = n.depth()
	}
	return n, nil
}

// nodeRelease drops one borrowed reference. Persistence is handled by the
// per-operation nodeFlush; release only tracks reference counting for clarity.
func (v *rtreeVTab[T]) nodeRelease(n *rtreeNode[T]) {
	if n == nil {
		return
	}
	if n.nRef > 0 {
		n.nRef--
	}
}

// nodeFlush writes every dirty cached node to the shadow table and empties the
// cache. Called at the end of each mutation so all modified nodes reach disk
// exactly once (if a parent references a child by number, the child is written
// before the parent's nodeWrite consumes it).
func (v *rtreeVTab[T]) nodeFlush() error {
	for _, n := range v.cache {
		if n.dirty && !n.dead {
			if _, err := v.storeNodeBlob(n.iNode, n.data); err != nil {
				return err
			}
			n.dirty = false
		}
	}
	v.cache = make(map[int64]*rtreeNode[T])
	return nil
}

// nodeWrite immediately persists node n (assigning a fresh number when iNode==0)
// and clears its dirty flag. Mirrors SQLite's nodeWrite at the points it is
// called (e.g. after a split allocates new nodes so their numbers are known).
func (v *rtreeVTab[T]) nodeWrite(n *rtreeNode[T]) error {
	nn, err := v.storeNodeBlob(n.iNode, n.data)
	if err != nil {
		return err
	}
	n.iNode = nn
	n.dirty = false
	return nil
}

// nodeZero clears all bytes after the depth/header (sets the node to empty).
func (v *rtreeVTab[T]) nodeZero(n *rtreeNode[T]) {
	for i := 2; i < len(n.data); i++ {
		n.data[i] = 0
	}
	n.dirty = true
}

// nodeNew allocates an in-memory node (no number yet).
func (v *rtreeVTab[T]) nodeNew() *rtreeNode[T] {
	return &rtreeNode[T]{iNode: 0, dirty: true, nRef: 1, data: make([]byte, v.iNodeSize)}
}

// rootAcquire acquires node 1, refreshing the cached tree depth.
func (v *rtreeVTab[T]) rootAcquire() (*rtreeNode[T], error) { return v.nodeAcquire(1) }

// ---- cell (de)serialization ----

// nodeGetRowid returns the rowid/child-node field of cell iCell.
func (v *rtreeVTab[T]) nodeGetRowid(n *rtreeNode[T], iCell int) int64 {
	off := 4 + v.nBytesPerCell*iCell
	return int64(binary.BigEndian.Uint64(n.data[off : off+8]))
}

// nodeGetCell deserializes cell iCell into a RtreeCell.
func (v *rtreeVTab[T]) nodeGetCell(n *rtreeNode[T], iCell int) RtreeCell[T] {
	c := RtreeCell[T]{aCoord: make([]T, v.nDim2)}
	c.iRowid = v.nodeGetRowid(n, iCell)
	off := 12 + v.nBytesPerCell*iCell
	for ii := 0; ii < v.nDim2; ii += 2 {
		c.aCoord[ii] = rtreeReadCoord[T](n.data[off : off+4])
		c.aCoord[ii+1] = rtreeReadCoord[T](n.data[off+4 : off+8])
		off += 8
	}
	return c
}

// nodeOverwriteCell writes cell at index iCell (cell 0 is the leftmost).
func (v *rtreeVTab[T]) nodeOverwriteCell(n *rtreeNode[T], cell *RtreeCell[T], iCell int) {
	off := 4 + v.nBytesPerCell*iCell
	binary.BigEndian.PutUint64(n.data[off:off+8], uint64(cell.iRowid))
	co := off + 8
	for ii := 0; ii < v.nDim2; ii++ {
		rtreeWriteCoord[T](n.data[co:co+4], cell.aCoord[ii])
		co += 4
	}
	n.dirty = true
}

// nodeInsertCell appends cell at the end of node n (caller must ensure room).
func (v *rtreeVTab[T]) nodeInsertCell(n *rtreeNode[T], cell *RtreeCell[T]) {
	v.nodeOverwriteCell(n, cell, n.nCell())
	n.setNCell(n.nCell() + 1)
}

// nodeDeleteCell removes cell iCell, shifting later cells down.
func (v *rtreeVTab[T]) nodeDeleteCell(n *rtreeNode[T], iCell int) {
	dst := 4 + v.nBytesPerCell*iCell
	src := dst + v.nBytesPerCell
	nByte := (n.nCell() - iCell - 1) * v.nBytesPerCell
	copy(n.data[dst:], n.data[src:src+nByte])
	n.setNCell(n.nCell() - 1)
	n.dirty = true
}

// maxCells returns the largest cell count a node of the current size can hold.
func (v *rtreeVTab[T]) maxCells() int { return (v.iNodeSize - 4) / v.nBytesPerCell }

// quoteName escapes an identifier for use inside SQL text.
func quoteName(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }
