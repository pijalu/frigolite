package fts5

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// This file ports the fts5_rowid() scalar function (fts5_index.c
// fts5RowidFunction, compiled under SQLITE_TEST / SQLITE_FTS5_DEBUG). Like
// the other test-support surfaces the engine registers it unconditionally
// (documented deviation from C's SQLITE_TEST gating).
//
// fts5_rowid maps between %_data rowids and their (segment, page) parts:
// the rowid of a segment leaf is segid<<37 | pgno (fts5_index.c's
// fts5_dri macro with dlidx=0 and height=0 — FTS5_DATA_PAGE_B=31,
// FTS5_DATA_HEIGHT_B=5, FTS5_DATA_DLI_B=1).

// errRowidSubject is the zero-argument failure text.
var errRowidSubject = errors.New("should be: fts5_rowid(subject, ....)")

// errRowidSegmentArg is the wrong-argument-count failure for the 'segment'
// subject (the doubled closing paren is C's pinned text).
var errRowidSegmentArg = errors.New("should be: fts5_rowid('segment', segid, pgno))")

// errRowidFirstArg is the unknown-subject failure.
var errRowidFirstArg = errors.New("first arg to fts5_rowid() must be 'segment'")

// SegmentRowid returns the %_data rowid of a segment leaf page
// (FTS5_SEGMENT_ROWID: fts5_dri(segid, 0, 0, pgno)).
func SegmentRowid(segid, pgno int64) int64 {
	return segid<<(31+5+1) + pgno
}

// RowidFunc implements the SQL scalar fts5_rowid(subject, ...). The subject
// must be the (case-insensitive) text 'segment', followed by the segment id
// and the leaf page number; every other shape fails with C's error texts.
func RowidFunc(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, errRowidSubject
	}
	if strings.EqualFold(valueText(args[0]), "segment") {
		if len(args) != 3 {
			return nil, errRowidSegmentArg
		}
		segid, _ := asInt64(args[1])
		pgno, _ := asInt64(args[2])
		return SegmentRowid(segid, pgno), nil
	}
	return nil, errRowidFirstArg
}

// DecodeFunc implements the SQL scalar fts5_decode(rowid, block)
// (fts5_index.c fts5DecodeFunction, SQLITE_TEST builds). The engine's
// %_data blocks use the Go-native encoding (storage.go), so the rendering
// mirrors that format rather than C's segment records: a GF-prefixed block
// decodes to an index summary, anything else reports "corrupt" — the
// SQL-visible contract the tests pin (a non-NULL rendering per stored block
// and "corrupt" for undecodable bytes).
func DecodeFunc(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, errors.New("should be: fts5_decode(rowid, block)")
	}
	raw, ok := toBytes(util.UnwrapColumnValue(args[1]))
	if !ok || len(raw) < 2 || raw[0] != 'G' || raw[1] != 'F' {
		return "corrupt", nil
	}
	var blob indexBlob
	if err := gob.NewDecoder(bytes.NewReader(raw[2:])).Decode(&blob); err != nil {
		return "corrupt", nil
	}
	return fmt.Sprintf("nDocs=%d", len(blob.Docs)), nil
}

// DecodeNoneFunc implements fts5_decode_none(rowid, block): the detail=none
// decoding form. The engine's payload encoding is detail-independent, so it
// shares DecodeFunc's rendering.
func DecodeNoneFunc(args []interface{}) (interface{}, error) {
	return DecodeFunc(args)
}

// valueText renders a function-argument value as text (sqlite3_value_text).
func valueText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return ""
	}
}
