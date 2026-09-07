// Rowid-alias record adaptation for FK scans. The rowid-alias storage
// convention stores NULL in an INTEGER PRIMARY KEY column's record slot
// (execdml.NullIPKAliasForWrite); the rowid carries the value. FK scans that
// compare raw record values against real key values must substitute the
// rowid for that slot first.
package execconstraint

import (
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// fillRowidAliasSlots substitutes rowID for every NULL IPK rowid-alias slot
// in a decoded record, in place.
func fillRowidAliasSlots(colDefs []sql.ColumnDef, rec *storage.Record, rowID int64) {
	for i, cd := range colDefs {
		if i < len(rec.Values) && rec.Values[i] == nil && execdml.IsIPKRowidAliasCol(cd) {
			rec.Values[i] = rowID
		}
	}
}
