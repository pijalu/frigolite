package vtab

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

// errSchemaCorrupt mirrors ext/rtree/rtree.c's verbatim "Schema corrupt or
// not an rtree" report. A custom type (not fmt.Errorf) keeps the required
// capital letter without tripping Go error-string linting.
var errSchemaCorrupt = errCapitalized{"Schema corrupt or not an rtree"}

type errCapitalized struct{ msg string }

func (e errCapitalized) Error() string { return e.msg }

// rtreeCheckInstance runs the sqlite3_rtreeintegritycheck checks over one
// rtree-family virtual table's shadow tables. Message texts mirror
// ext/rtree/rtree.c's check callback verbatim (asserted by rtreecheck.test).
type rtreeCheckInstance struct {
	db       Database
	name     string
	nDim     int
	i32      bool
	bppCell  int
	nodeSize int
	problems []string

	// expectedRowidNode accumulates the leaf-declared rowid→node mappings;
	// populated during the node walk and compared against %_rowid afterwards.
	expectedRowidNode map[int64]int64
	// expectedParentNode accumulates child→parent mappings discovered while
	// descending interior cells; compared against %_parent after the walk.
	expectedParentNode map[int64]int64
	// nLeaf / nNonLeaf mirror RtreeCheck.nLeaf/nNonLeaf cell counters.
	nLeaf, nNonLeaf int
}

// run executes every check. A returned error means the report itself could
// not be produced ("Schema corrupt or not an rtree" for non-rtree names).
func (c *rtreeCheckInstance) run() error {
	rows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT sql FROM sqlite_master WHERE lower(name)=lower('%s') AND type='table'",
		strings.ReplaceAll(c.name, "'", "''")))
	if err != nil || len(rows) == 0 {
		return errSchemaCorrupt
	}
	sqlStr, ok := rows[0][0].(string)
	if !ok || !RTreeFamilyModuleOf(sqlStr) {
		return errSchemaCorrupt
	}
	c.i32 = strings.Contains(strings.ToLower(sqlStr), "using rtree_i32")
	if err := c.loadDims(); err != nil {
		return err
	}
	c.bppCell = 8 + 8*c.nDim
	c.nodeSize = c.inferNodeSize()
	c.expectedRowidNode = map[int64]int64{}
	c.expectedParentNode = map[int64]int64{}

	depth := c.rootDepth()
	const rtreeMaxDepth = 40 // RTREE_MAX_DEPTH (ext/rtree/rtree.c)
	if depth > rtreeMaxDepth {
		c.problemf("Rtree depth out of range (%d)", depth)
	} else {
		c.walkNodes(1, depth, nil)
	}
	c.checkRowidTable()
	c.checkParentTable()
	return nil
}

// loadDims derives nDim from the declared column list of the CREATE SQL.
func (c *rtreeCheckInstance) loadDims() error {
	dimRows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT sql FROM sqlite_master WHERE lower(name)=lower('%s')",
		strings.ReplaceAll(c.name, "'", "''")))
	if err != nil || len(dimRows) == 0 {
		return errSchemaCorrupt
	}
	sqlStr, _ := dimRows[0][0].(string)
	open := strings.Index(sqlStr, "(")
	closing := strings.LastIndex(sqlStr, ")")
	if open < 0 || closing < open {
		return errSchemaCorrupt
	}
	coords := 0
	for _, a := range splitTopLevelCommas(sqlStr[open+1 : closing]) {
		a = strings.TrimSpace(a)
		if a == "" || strings.HasPrefix(a, "+") {
			continue
		}
		coords++
	}
	coords-- // id column
	if coords < 2 || coords%2 != 0 {
		return errSchemaCorrupt
	}
	c.nDim = coords / 2
	return nil
}

// splitTopLevelCommas splits s on top-level commas (quotes and parens aware).
func splitTopLevelCommas(s string) []string {
	var parts []string
	cur := strings.Builder{}
	var quote byte
	depth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			cur.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
			cur.WriteByte(ch)
		case ch == '(':
			depth++
			cur.WriteByte(ch)
		case ch == ')':
			depth--
			cur.WriteByte(ch)
		case ch == ',' && depth == 0:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// inferNodeSize derives the node blob size from the largest stored node.
func (c *rtreeCheckInstance) inferNodeSize() int {
	rows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT coalesce(max(length(data)),0) FROM \"%s_node\"", quoteIdent(c.name)))
	if err != nil || len(rows) == 0 {
		return 960
	}
	n := int(rtreeAsInt64(rows[0][0]))
	if n <= 4 {
		return 960
	}
	return n
}

func quoteIdent(s string) string { return strings.ReplaceAll(s, `"`, `""`) }

// rootDepth reads the raw depth stored in the root-node header (bytes 0..1);
// validation against RTREE_MAX_DEPTH happens at the call site so a corrupt
// value produces SQLite's "Rtree depth out of range" problem line instead of
// being silently clamped. The field is UNSIGNED (get2byteAligned parity): a
// signed cast would turn e.g. 0xC800 into -14336 and slip past the range
// check into an unbounded walk.
func (c *rtreeCheckInstance) rootDepth() int {
	rows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT data FROM \"%s_node\" WHERE nodeno=1", quoteIdent(c.name)))
	if err != nil || len(rows) == 0 {
		return 0
	}
	blob := asBytes(rows[0][0])
	if len(blob) < 4 {
		return 0
	}
	return int(binary.BigEndian.Uint16(blob[0:2]))
}

// nodeBlob fetches one node blob.
func (c *rtreeCheckInstance) nodeBlob(nodeno int64) ([]byte, bool) {
	rows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT data FROM \"%s_node\" WHERE nodeno=%d", quoteIdent(c.name), nodeno))
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	b := asBytes(rows[0][0])
	return b, b != nil
}

// problemf appends one formatted problem line.
func (c *rtreeCheckInstance) problemf(format string, args ...interface{}) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// walkNodes recursively validates nodes starting at nodeno. depth==0 marks a
// leaf level (cells carry entry rowids rather than child pointers).
// parentCoords carries the bounding cell of nodeno stored on its parent page
// (nil for the root), mirroring rtreeCheckNode's aParent argument: every cell
// is checked for internal consistency AND containment inside parentCoords.
func (c *rtreeCheckInstance) walkNodes(nodeno int64, depth int, parentCoords []byte) {
	blob, ok := c.nodeBlob(nodeno)
	if !ok {
		c.problemf("Node %d missing from database", nodeno)
		return
	}
	if len(blob) < 4 {
		// Faithful: rtreeCheckNode rejects <4-byte blobs before parsing the
		// cell-count header ("Node %lld is too small (%d bytes)").
		c.problemf("Node %d is too small (%d bytes)", nodeno, len(blob))
		return
	}
	nCell := int(int16(binary.BigEndian.Uint16(blob[2:4])))
	if nCell > 0 && len(blob) < 4+c.bppCell*nCell {
		c.problemf("Node %d is too small for cell count of %d (%d bytes)", nodeno, nCell, len(blob))
		return
	}
	isLeaf := depth == 0
	for i := 0; i < nCell && offOK(off(4, c.bppCell, i), len(blob), c.bppCell); i++ {
		base := 4 + c.bppCell*i
		rowidOrChild := int64(binary.BigEndian.Uint64(blob[base : base+8]))
		coords := blob[base+8 : base+c.bppCell]
		c.checkCellCoords(nodeno, i, coords, parentCoords)
		if isLeaf {
			c.expectedRowidNode[rowidOrChild] = nodeno
			c.nLeaf++
			continue
		}
		c.expectedParentNode[rowidOrChild] = nodeno
		c.nNonLeaf++
		c.walkNodes(rowidOrChild, depth-1, coords)
	}
}

// checkCellCoords validates one cell's coordinates against the internal
// min<=max invariant and — on non-root pages — containment inside the parent
// bounding pair, emitting rtreeCheckCellCoord's message forms per dimension.
func (c *rtreeCheckInstance) checkCellCoords(nodeno int64, iCell int, coords, parentCoords []byte) {
	for d := 0; d < c.nDim; d++ {
		pair := coords[8*d:]
		if coordPairBad(pair, c.i32) {
			c.problemf("Dimension %d of cell %d on node %d is corrupt", d, iCell, nodeno)
		}
		if parentCoords != nil && coordOutsideParent(pair, parentCoords[8*d:], c.i32) {
			c.problemf("Dimension %d of cell %d on node %d is corrupt relative to parent", d, iCell, nodeno)
		}
	}
}

func off(base, step, i int) int       { return base + step*i }
func offOK(pos, total, span int) bool { return pos >= 0 && pos+span <= total }

// coordPairBad reports whether the (min,max) raw coordinate pair violates the
// min<=max invariant under the table's coordinate flavor.
func coordPairBad(p []byte, i32 bool) bool {
	if len(p) < 8 {
		return true
	}
	if i32 {
		lo := int32(binary.BigEndian.Uint32(p[0:4]))
		hi := int32(binary.BigEndian.Uint32(p[4:8]))
		return lo > hi
	}
	loF := math.Float32frombits(binary.BigEndian.Uint32(p[0:4]))
	hiF := math.Float32frombits(binary.BigEndian.Uint32(p[4:8]))
	// NaN pairs are always corrupt.
	return math.IsNaN(float64(loF)) || math.IsNaN(float64(hiF)) || loF > hiF
}

// coordOutsideParent mirrors rtreeCheckCellCoord's pParent branch: the child
// cell must lie inside the parent's bounding pair per dimension
// (c1 >= p1 && c2 <= p2); a violation yields "corrupt relative to parent".
func coordOutsideParent(child, parent []byte, i32 bool) bool {
	if len(parent) < 8 {
		return false
	}
	if i32 {
		c1 := int32(binary.BigEndian.Uint32(child[0:4]))
		c2 := int32(binary.BigEndian.Uint32(child[4:8]))
		p1 := int32(binary.BigEndian.Uint32(parent[0:4]))
		p2 := int32(binary.BigEndian.Uint32(parent[4:8]))
		return c1 < p1 || c2 > p2
	}
	c1 := math.Float32frombits(binary.BigEndian.Uint32(child[0:4]))
	c2 := math.Float32frombits(binary.BigEndian.Uint32(child[4:8]))
	p1 := math.Float32frombits(binary.BigEndian.Uint32(parent[0:4]))
	p2 := math.Float32frombits(binary.BigEndian.Uint32(parent[4:8]))
	return c1 < p1 || c2 > p2
}

// checkRowidTable compares %_rowid against the tree the walk reconstructed:
// missing mappings and misdirected mappings first (ascending rowid), then the
// entry-count discrepancy.
func (c *rtreeCheckInstance) checkRowidTable() {
	rows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT rowid, nodeno FROM \"%s_rowid\"", quoteIdent(c.name)))
	if err != nil {
		c.problemf("error reading \"%%_rowid\" table")
		return
	}
	got := map[int64]int64{}
	for _, r := range rows {
		got[rtreeAsInt64(r[0])] = rtreeAsInt64(r[1])
	}
	wantIDs := make([]int64, 0, len(c.expectedRowidNode))
	for id := range c.expectedRowidNode {
		wantIDs = append(wantIDs, id)
	}
	sort.Slice(wantIDs, func(i, j int) bool { return wantIDs[i] < wantIDs[j] })

	for _, id := range wantIDs {
		wantNode := c.expectedRowidNode[id]
		gotNode, present := got[id]
		switch {
		case !present:
			c.problemf("Mapping (%d -> %d) missing from %%_rowid table", id, wantNode)
		case gotNode != wantNode:
			c.problemf("Found (%d -> %d) in %%_rowid table, expected (%d -> %d)",
				id, gotNode, id, wantNode)
		}
	}
	if len(c.expectedRowidNode) != len(rows) {
		c.problemf("Wrong number of entries in %%_rowid table - expected %d, actual %d",
			len(c.expectedRowidNode), len(rows))
	}
}

// checkParentTable compares %_parent against the child→parent mappings the
// walk discovered (ascending nodeno), then the entry-count discrepancy.
func (c *rtreeCheckInstance) checkParentTable() {
	rows, err := c.db.ExecSQL(fmt.Sprintf(
		"SELECT nodeno, parentnode FROM \"%s_parent\"", quoteIdent(c.name)))
	if err != nil {
		c.problemf("error reading \"%%_parent\" table")
		return
	}
	got := map[int64]int64{}
	for _, r := range rows {
		got[rtreeAsInt64(r[0])] = rtreeAsInt64(r[1])
	}
	wantIDs := make([]int64, 0, len(c.expectedParentNode))
	for id := range c.expectedParentNode {
		wantIDs = append(wantIDs, id)
	}
	sort.Slice(wantIDs, func(i, j int) bool { return wantIDs[i] < wantIDs[j] })
	for _, id := range wantIDs {
		wantParent := c.expectedParentNode[id]
		gotParent, present := got[id]
		switch {
		case !present:
			c.problemf("Mapping (%d -> %d) missing from %%_parent table", id, wantParent)
		case gotParent != wantParent:
			c.problemf("Found (%d -> %d) in %%_parent table, expected (%d -> %d)",
				id, gotParent, id, wantParent)
		}
	}
	if len(c.expectedParentNode) != len(rows) {
		c.problemf("Wrong number of entries in %%_parent table - expected %d, actual %d",
			len(c.expectedParentNode), len(rows))
	}
}

// ---- SQL function plumbing ----

// rtreecheckFunc performs the integrity checks over one virtual table and
// returns the report string ("ok" when no problem was found).
func rtreecheckFunc(db Database, arg interface{}) (interface{}, error) {
	name, ok := arg.(string)
	if !ok {
		return nil, fmt.Errorf("SQL logic error")
	}
	rows, err := db.ExecSQL(fmt.Sprintf(
		"SELECT name FROM sqlite_master WHERE lower(name)=lower('%s')",
		strings.ReplaceAll(name, "'", "''")))
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("SQL logic error")
	}
	c := &rtreeCheckInstance{db: db, name: name}
	if err := c.run(); err != nil {
		return err.Error(), nil
	}
	if len(c.problems) == 0 {
		return "ok", nil
	}
	return strings.Join(c.problems, "\n"), nil
}

// RTreeIntegrityReport renders the PRAGMA integrity_check contribution for
// one rtree table ("In RTree main.<name>:" + problems), or "" when clean.
func RTreeIntegrityReport(db Database, name string) string {
	c := &rtreeCheckInstance{db: db, name: name}
	if err := c.run(); err != nil {
		return ""
	}
	if len(c.problems) == 0 {
		return ""
	}
	return "In RTree main." + name + ":\n" + strings.Join(c.problems, "\n")
}
