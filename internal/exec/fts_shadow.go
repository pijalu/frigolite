package exec

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/storage"
)

// FTSShadowBlob reads a value BLOB from an FTS4 shadow table for matchinfo's
// 'l' (per-document token counts) and 'a' (average token counts) formats,
// mirroring SQLite's sqlite3Fts3SelectDocsize / sqlite3Fts3SelectDoctotal
// (ext/fts3/fts3_write.c). A missing row or a non-BLOB value is reported as
// "database disk image is malformed" (SQLite returns FTS_CORRUPT_VTAB, which
// the FTS3 module maps to that message).
//
// kind is one of:
//
//	"docsize" — the %_docsize row for docID (SELECT size FROM t_docsize
//	            WHERE docid=docID; the column must be a BLOB)
//	"doctotal" — the %_stat row id=0 (SELECT value FROM t_stat WHERE id=0;
//	            FTS_STAT_DOCTOTAL; the column must be a BLOB)
func (e *Engine) FTSShadowBlob(tableName, kind string, docID int64) ([]byte, error) {
	var shadowName string
	switch kind {
	case "docsize":
		shadowName = tableName + "_docsize"
	case "doctotal":
		shadowName = tableName + "_stat"
	default:
		return nil, fmt.Errorf("unknown FTS shadow kind: %s", kind)
	}
	entry, dbCtx, err := e.FindTable(shadowName)
	if err != nil || dbCtx == nil || entry == nil {
		// A missing shadow table means the format is unsupported (matchinfo
		// 'l' on an FTS3 table); the caller's format validation rejects it
		// before reaching here. Treat as corrupt anyway (SQLite cannot read
		// the docsize of a MATCHed row without the table).
		return nil, fmt.Errorf("database disk image is malformed")
	}
	tree := e.TableBTreeForName(entry.Name, entry.RootPage, true)
	cell, cerr := e.ReadCellByRowID(tree, docID)
	if cerr != nil || cell == nil {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil || len(rec.Values) < 2 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	// The value column is the record's second column (the shadow tables are
	// (pk, value)); it must be a BLOB (sqlite3_column_type==SQLITE_BLOB).
	b, ok := rec.Values[1].([]byte)
	if !ok {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	return b, nil
}
