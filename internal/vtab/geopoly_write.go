package vtab

import (
	"fmt"
)

// xUpdate implementation for the geopoly module (geopoly.c geopolyUpdate).
// The declared values arrive in geopoly column order: argv-analog values[0]
// is _shape, values[1..] the user columns; the rowid travels out of band
// (RowidConflictWriter for INSERT INTO ... (rowid, ...) / UPDATE ... SET
// rowid, exactly the engine's argv[1] analog).
//
// The cell inserted into the r-tree carries the polygon's bounding box as
// its four coordinates (geopolyBBox(0, aData[2], cell.aCoord, &rc)); a _shape
// of an unusable TYPE (NULL etc.) fails with "_shape does not contain a
// valid polygon", while a TEXT/BLOB value that merely fails to PARSE yields
// an all-zero cell and is stored verbatim (the rc==SQLITE_OK branch of
// geopolyFuncParam).

// errGeopolyShape is the xUpdate cell-derivation failure text.
var errGeopolyShape = fmt.Errorf("_shape does not contain a valid polygon")

// geopolyShapeValue returns the declared _shape value from a row-image.
func geopolyShapeValue(values []interface{}) interface{} {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

// buildGeopolyCell derives the r-tree cell (bbox coordinates) for a row
// image. ok=false with rc==geoOK reproduces the zero-cell path.
func buildGeopolyCell(shape interface{}) (RtreeCell[float32], int, error) {
	poly, rc := geopolyFuncParam(shape)
	if rc == geoError {
		return RtreeCell[float32]{}, rc, errGeopolyShape
	}
	bbox := geopolyBBoxOf(poly)
	cell := RtreeCell[float32]{
		aCoord: []float32{bbox[0], bbox[1], bbox[2], bbox[3]},
	}
	return cell, rc, nil
}

// InsertRow implements vtab.RowUpdater (plain INSERT; rowid auto-assigned).
func (v *geopolyVTab) InsertRow(values []interface{}) (int64, error) {
	return v.insertRow(values, 0, false, "")
}

// InsertRowWithRowid implements RowidConflictWriter (xUpdate argv[0]=NULL,
// argv[1]=explicit rowid).
func (v *geopolyVTab) InsertRowWithRowid(values []interface{}, rowid int64, resolve string) (int64, error) {
	return v.insertRow(values, rowid, true, resolve)
}

// UpdateRow implements vtab.RowUpdater (generic path; the engine routes
// geopoly UPDATEs through UpdateRowWithRowid because the instance is a
// RowidConflictWriter, so this stays a defensive error).
func (v *geopolyVTab) UpdateRow(oldValues, newValues []interface{}) error {
	return fmt.Errorf("rtree: cannot update entry without rowid")
}

// UpdateRowWithRowid implements RowidConflictWriter (xUpdate argv[0]=old
// rowid, argv[1]=new rowid): delete + reinsert when the cell must change,
// then refresh the auxiliary columns.
func (v *geopolyVTab) UpdateRowWithRowid(oldValues []interface{}, oldRowid int64, newValues []interface{}, newRowid int64, resolve string) (bool, bool, error) {
	cell, _, err := buildGeopolyCell(geopolyShapeValue(newValues))
	if err != nil {
		return false, false, err
	}
	// A reassigned rowid that already exists is a constraint failure unless
	// the statement resolves it with REPLACE (geopolyUpdate's pReadRowid
	// probe + sqlite3_vtab_on_conflict check).
	if err := v.resolveRowidMove(oldRowid, newRowid, resolve); err != nil {
		return false, false, err
	}
	v.rtree.newNodeCache()
	v.rtree.deleted = nil
	if err := v.rtreeDeleteRowid(oldRowid); err != nil {
		return false, false, err
	}
	if err := v.insertCellAux(cell, newRowid, newValues); err != nil {
		return false, false, err
	}
	return true, false, v.rtree.nodeFlush()
}

// resolveRowidMove rejects (or deletes under REPLACE) an existing occupant
// of newRowid when the row moves onto it.
func (v *geopolyVTab) resolveRowidMove(oldRowid, newRowid int64, resolve string) error {
	if newRowid == oldRowid {
		return nil
	}
	if _, exists, gerr := v.rtree.getRowidNode(newRowid); gerr == nil && exists {
		if resolve == "REPLACE" {
			return v.deleteRowid(newRowid)
		}
		return &UniqueConstraintError{Table: v.rtree.name, Column: v.declared[0], RowID: newRowid}
	}
	return nil
}

// insertCellAux inserts cell at its leaf (with the pending aux row) and
// persists the auxiliary columns.
func (v *geopolyVTab) insertCellAux(cell RtreeCell[float32], rowid int64, values []interface{}) error {
	cell.iRowid = rowid
	leaf, err := v.rtree.ChooseLeaf(&cell, 0)
	if err != nil {
		return err
	}
	v.rtree.pendingAux = geopolyAuxCopy(values, v.rtree.nAux)
	if err := v.rtree.rtreeInsertCell(leaf, &cell, 0); err != nil {
		v.rtree.nodeRelease(leaf)
		return err
	}
	v.rtree.nodeRelease(leaf)
	return v.rtree.storeAuxColumns(rowid)
}

// DeleteRow implements vtab.RowUpdater (generic path; see UpdateRow).
func (v *geopolyVTab) DeleteRow(oldValues []interface{}) error {
	return fmt.Errorf("rtree: cannot delete entry without rowid")
}

// DeleteRowWithRowid implements RowidConflictWriter (xUpdate argv[0]=rowid).
func (v *geopolyVTab) DeleteRowWithRowid(oldValues []interface{}, rowid int64) error {
	return v.deleteRowid(rowid)
}

// deleteRowid removes one entry and flushes the tree.
func (v *geopolyVTab) deleteRowid(rowid int64) error {
	v.rtree.newNodeCache()
	v.rtree.deleted = nil
	if err := v.rtree.rtreeDeleteRowid(rowid); err != nil {
		return err
	}
	return v.rtree.nodeFlush()
}

// insertRow ports the INSERT arm of geopolyUpdate: derive the cell from
// _shape, resolve the rowid (explicit, or the next free one), reject a taken
// rowid per the conflict resolution, insert into the tree and persist the
// auxiliary columns (_shape included, JSON text normalized to its blob).
func (v *geopolyVTab) insertRow(values []interface{}, rowid int64, hasRowid bool, resolve string) (int64, error) {
	cell, _, err := buildGeopolyCell(geopolyShapeValue(values))
	if err != nil {
		return 0, err
	}
	v.rtree.newNodeCache()
	v.rtree.deleted = nil
	if hasRowid {
		if err := v.resolveInsertRowid(rowid, resolve); err != nil {
			return 0, err
		}
		cell.iRowid = rowid
	} else {
		rowid, err = v.rtree.rtreeNewRowid()
		if err != nil {
			return 0, err
		}
		cell.iRowid = rowid
	}
	if err := v.insertCellAux(cell, rowid, values); err != nil {
		return 0, err
	}
	return rowid, v.rtree.nodeFlush()
}

// resolveInsertRowid rejects (or deletes under REPLACE) an existing occupant
// of an explicitly inserted rowid.
func (v *geopolyVTab) resolveInsertRowid(rowid int64, resolve string) error {
	if _, exists, gerr := v.rtree.getRowidNode(rowid); gerr == nil && exists {
		if resolve == "REPLACE" {
			return v.rtreeDeleteRowid(rowid)
		}
		// The engine maps UNIQUE-constraint failures to the OR IGNORE
		// skip / OR FAIL keep-prior contract (rtreeConstraintError).
		return &UniqueConstraintError{Table: v.rtree.name, Column: v.declared[0], RowID: rowid}
	}
	return nil
}

// geopolyAuxCopy copies the first nAux values as the pending auxiliary row.
// A TEXT _shape is normalized to its blob form exactly like geopolyUpdate's
// pWriteAux binding (sqlite3_bind_blob for TEXT input); every other value is
// bound verbatim.
func geopolyAuxCopy(values []interface{}, nAux int) []interface{} {
	aux := make([]interface{}, nAux)
	for i := 0; i < nAux && i < len(values); i++ {
		aux[i] = values[i]
	}
	if len(aux) > 0 {
		if s, ok := aux[0].(string); ok {
			if poly, rc := geopolyFuncParam(s); rc == geoOK && poly != nil {
				aux[0] = poly.encodeBlob()
			}
		}
	}
	return aux
}
