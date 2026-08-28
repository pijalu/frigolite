package vtab

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

// RegisterRTreeSQLFunctions installs ext/rtree/rtree.c's global SQL functions
// on db (usable without any virtual-table instance):
//
//	rtreenode(nDim, blob)       → text rendering of node-content cells
//	rtreedepth(blob)            → depth stored in a root-node header
//	rtreecheck(name)            → integrity report ('ok' when clean)
//	rtreecheck(schema,name)     → same, with an explicit schema qualifier
func RegisterRTreeSQLFunctions(db Database) {
	db.RegisterScalar("rtreenode", 2, 2, rtreenodeFunc)
	db.RegisterScalar("rtreedepth", 1, 1, rtreedepthFunc)
	db.RegisterScalar("rtreecheck", 1, 2, func(args []interface{}) (interface{}, error) {
		table := args[len(args)-1]
		if len(args) == 2 {
			// rtreeCheckTable resolves its shadow tables against the schema
			// named by the first argument; only 'main' exists in frigolite
			// but the argument is still validated for type.
			schema, ok := util.UnwrapColumnValue(args[0]).(string)
			if !ok {
				return nil, fmt.Errorf("SQL logic error")
			}
			switch strings.ToLower(schema) {
			case "main", "temp":
				// supported qualifiers
			default:
				return nil, fmt.Errorf("unknown database %s", schema)
			}
		}
		return rtreecheckFunc(db, util.UnwrapColumnValue(table))
	})
}

// ---- rtreenode(nDim, blob) ----

// rtreenodeFunc renders each cell of a node-content blob as "{iRowid x0 x1
// ...}" (rtree.c's rtreenode). The blob is a FULL node image: 4-byte header
// with the cell count at [2:4], cells from offset 4. Contract parity:
//   - nDim outside 1..RTREE_MAX_DIMENSIONS → NULL result, no error;
//   - NULL/non-blob arg or blobs shorter than the header → NULL;
//   - a blob too small for its declared cell count → NULL;
//   - coordinates print as C printf "%g" (6 significant digits).
func rtreenodeFunc(args []interface{}) (interface{}, error) {
	nDim := int(rtreeAsInt64(util.UnwrapColumnValue(args[0])))
	if nDim < 1 || nDim > RTREE_MAX_DIMENSIONS {
		return nil, nil
	}
	blob := asBytes(util.UnwrapColumnValue(args[1]))
	if len(blob) < 4 {
		return nil, nil
	}
	// Mirror rtree.c's guard verbatim: nData < NCELL*nBytesPerCell (the +4
	// header offset is intentionally absent there).
	cellSize := 8 + 8*nDim
	nCell := int(binary.BigEndian.Uint16(blob[2:4]))
	if nCell > 0 && len(blob) < nCell*cellSize {
		return nil, nil
	}
	var b strings.Builder
	for i := 0; i < nCell; i++ {
		base := 4 + cellSize*i
		if base+cellSize > len(blob) {
			break
		}
		if i > 0 {
			b.WriteString(" ")
		}
		iRowid := int64(binary.BigEndian.Uint64(blob[base : base+8]))
		fmt.Fprintf(&b, "{%d", iRowid)
		for j := 0; j < 2*nDim; j++ {
			f := math.Float32frombits(binary.BigEndian.Uint32(blob[base+8+4*j:]))
			b.WriteString(" ")
			b.WriteString(rtreeFormatFloat(float64(f)))
		}
		b.WriteString("}")
	}
	return b.String(), nil
}

// rtreeFormatFloat renders one coordinate like sqlite3_str_appendf("%g", …):
// six significant digits, trailing zeros trimmed — Go's 'g' with precision 6
// produces byte-identical output to C printf %g on doubles.
func rtreeFormatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', 6, 64)
}

// ---- rtreedepth(blob) ----

// rtreedepthFunc reads the depth field from a node-content blob header
// (bytes 0..1, big-endian, readInt16 = UNSIGNED like the C source). The
// argument must be of BLOB type with at least 2 bytes; anything else raises
// the verbatim C error ("Invalid argument to rtreedepth()"). TEXT never
// coerces here (sqlite3_value_type must be SQLITE_BLOB); lazy zeroblobs
// count as blobs.
func rtreedepthFunc(args []interface{}) (interface{}, error) {
	var blob []byte
	switch x := util.UnwrapColumnValue(args[0]).(type) {
	case []byte:
		blob = x
	case value.ZeroBlob:
		blob = x.Bytes()
	}
	if len(blob) < 2 {
		return nil, errCapitalized{"Invalid argument to rtreedepth()"}
	}
	return int64(binary.BigEndian.Uint16(blob[0:2])), nil
}
