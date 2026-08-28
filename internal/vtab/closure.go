package vtab

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

func debugClosureEnv() bool { return os.Getenv("CL_DBG") != "" }

// ClosureEdgeSource supplies the (child, parent) edge pairs of the base table
// backing a transitive_closure virtual table (closure.c reads T.X/T.P).
type ClosureEdgeSource interface {
	// ClosureEdges returns one pair per base-table row: (id, parent).
	// Rows whose parent is NULL are skipped by the caller.
	ClosureEdges(table, idCol, parentCol string) ([][2]int64, error)
}

// TransitiveClosureModule implements ext/misc/closure.c's transitive_closure
// virtual table: given a tree/DAG table, every scan enumerates the closure of
// one root node (id, depth). Without a hidden root= constraint the scan is
// empty (closure.c xFilter returns an empty set when idxNum bit 1 is clear).
//
// Columns: id, depth, root HIDDEN, tablename HIDDEN, idcolumn HIDDEN,
// parentcolumn HIDDEN.
type TransitiveClosureModule struct {
	src ClosureEdgeSource
}

// NewTransitiveClosureModule builds the module over an edge source.
func NewTransitiveClosureModule(src ClosureEdgeSource) *TransitiveClosureModule {
	return &TransitiveClosureModule{src: src}
}

// csv-free column indexes for the declared schema.
const (
	closureColID     = 0
	closureColDepth  = 1
	closureColRoot   = 2
	closureColTable  = 3
	closureColIDCol  = 4
	closureColParent = 5
)

// closureVTab is one configured instance.
type closureVTab struct {
	module           *TransitiveClosureModule
	table            string
	idColumn         string
	parentColumn     string
	tableOverride    string
	idColumnOverride string
	parentOverride   string
	root             int64
	rootSeen         bool
}

// Create implements Module.
func (m *TransitiveClosureModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module.
func (m *TransitiveClosureModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

func (m *TransitiveClosureModule) connect(args []string) (VirtualTable, error) {
	v := &closureVTab{module: m, rootSeen: false}
	for _, a := range args {
		eq := strings.IndexByte(a, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(a[:eq]))
		val := strings.TrimSpace(a[eq+1:])
		switch key {
		case "tablename":
			v.table = val
		case "idcolumn":
			v.idColumn = val
		case "parentcolumn":
			v.parentColumn = val
		case "root":
			if n, ok := asInt64(val); ok {
				v.root, v.rootSeen = n, true
			}
		}
	}
	// Missing configuration is legal (closure.c): it can be supplied later
	// through hidden-column constraints (tablename=/idcolumn=/parentcolumn=).
	return v, nil
}

// Columns implements ColumnInfo (closure.c declared schema).
func (v *closureVTab) Columns() []string {
	return []string{"id", "depth", "root", "tablename", "idcolumn", "parentcolumn"}
}

// HiddenColumns reports root/tablename/idcolumn/parentcolumn as HIDDEN.
func (v *closureVTab) HiddenColumns() map[int]bool {
	return map[int]bool{closureColRoot: true, closureColTable: true, closureColIDCol: true, closureColParent: true}
}

// BestIndex accepts the default plan.
func (v *closureVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// SetHiddenConstraint absorbs WHERE root=N bindings (and re-specifications of
// the hidden configuration columns).
func (v *closureVTab) SetHiddenConstraint(col string, val interface{}) error {
	n, ok := asInt64(util.UnwrapColumnValue(val))
	switch col {
	case "root":
		if !ok {
			return fmt.Errorf("transitive_closure: unusable root value")
		}
		v.root, v.rootSeen = n, true
	case "tablename":
		if s, isStr := util.UnwrapColumnValue(val).(string); isStr {
			v.tableOverride = s
		}
	case "idcolumn":
		if s, isStr := util.UnwrapColumnValue(val).(string); isStr {
			v.idColumnOverride = s
		}
	case "parentcolumn":
		if s, isStr := util.UnwrapColumnValue(val).(string); isStr {
			v.parentOverride = s
		}
	default:
		return fmt.Errorf("no such column: %s", col)
	}
	return nil
}

// Open computes the closure of the bound root (or yields no rows when no
// root was constrained — closure.c xFilter's idxNum&1==0 path).
func (v *closureVTab) Open() (Cursor, error) {
	c := &closureCursor{vt: v, tableName: v.table, idColumn: v.idColumn, parentColumn: v.parentColumn}
	if !v.rootSeen {
		c.done = true
		return c, nil
	}
	eTable, eID, eParent := v.table, v.idColumn, v.parentColumn
	if v.tableOverride != "" {
		eTable = v.tableOverride
	}
	if v.idColumnOverride != "" {
		eID = v.idColumnOverride
	}
	if v.parentOverride != "" {
		eParent = v.parentOverride
	}
	c.tableName, c.idColumn, c.parentColumn = eTable, eID, eParent
	if debugClosureEnv() {
		fmt.Fprintf(os.Stderr, "CLDBG open root=%d seen=%v edgesFromSrc\n", v.root, v.rootSeen)
	}
	edges, err := v.module.src.ClosureEdges(eTable, eID, eParent)
	if err != nil {
		return nil, err
	}
	// child lookup: parent -> children
	children := make(map[int64][]int64, len(edges))
	for _, e := range edges {
		id, parent := e[0], e[1]
		children[parent] = append(children[parent], id)
	}
	// BFS from root, tracking minimal depth and skipping repeats.
	type node struct {
		id    int64
		depth int
	}
	seen := map[int64]bool{v.root: true}
	queue := []node{{v.root, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		c.ids = append(c.ids, cur.id)
		c.depths = append(c.depths, cur.depth)
		for _, ch := range children[cur.id] {
			if !seen[ch] {
				seen[ch] = true
				queue = append(queue, node{ch, cur.depth + 1})
			}
		}
	}
	// closure.c keeps its scan queue sorted by id, so rows stream in
	// ascending id order; mirror that here.
	for i := 1; i < len(c.ids); i++ {
		id, dp := c.ids[i], c.depths[i]
		j := i - 1
		for j >= 0 && c.ids[j] > id {
			c.ids[j+1] = c.ids[j]
			c.depths[j+1] = c.depths[j]
			j--
		}
		c.ids[j+1] = id
		c.depths[j+1] = dp
	}
	return c, nil
}

// closureCursor walks the computed closure rows.
type closureCursor struct {
	vt           *closureVTab
	tableName    string
	idColumn     string
	parentColumn string
	ids          []int64
	depths       []int
	idx          int
	started      bool
	done         bool
}

// Next advances; the first call serves row 0.
func (c *closureCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return len(c.ids) > 0
	}
	c.idx++
	if c.idx >= len(c.ids) {
		c.done = true
		return false
	}
	return true
}

// Rowid returns the current node id.
func (c *closureCursor) Rowid() int64 { return c.ids[c.idx] }

// Column serves id/depth/root plus the echoed hidden configuration.
func (c *closureCursor) Column(idx int) (interface{}, error) {
	switch idx {
	case closureColID:
		return c.ids[c.idx], nil
	case closureColDepth:
		return int64(c.depths[c.idx]), nil
	case closureColRoot:
		// closure.c CLOSURE_COL_ROOT always yields NULL.
		return nil, nil
	case closureColTable:
		return c.tableName, nil
	case closureColIDCol:
		return c.idColumn, nil
	case closureColParent:
		return c.parentColumn, nil
	}
	return nil, fmt.Errorf("transitive_closure: invalid column index %d", idx)
}

// Close implements Cursor.
func (c *closureCursor) Close() error { return nil }

func stringsIndexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

func parseInt64(s string) int64 {
	var n int64
	neg := false
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int64(s[i]-'0')
	}
	if neg {
		return -n
	}
	return n
}
