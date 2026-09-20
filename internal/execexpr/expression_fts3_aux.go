// This file holds the FTS3/4 auxiliary function evaluation: matchinfo(),
// offsets(), snippet() and optimize() (fts3_snippet.c / fts3.c), including
// the matchinfo format interpreter and its argument validation. The fts5
// auxiliary functions live in fts5_aux.go and fts5_aux_testfuncs.go; the
// dispatch entry from evalEngineFunc is in expression_eval.go.
package execexpr

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// evalFTSAux dispatches the FTS3 auxiliary functions by name.
func (ev *Evaluator) evalFTSAux(name string, f *sql.FuncCall, row Row) (interface{}, error) {
	// fts3.c fts3FunctionArg L3690-3699: argv[0] must be the hidden fts3
	// cursor of the CURRENTLY MATCHING fts table; anything else fails with
	// "illegal first argument to <func>". Arity violations use SQLite's
	// generic wrong-num-args text (snippet >6 has its own message,
	// fts3.c L3724-3727).
	if err := ev.validateFTSAuxArgs(name, f); err != nil {
		return nil, err
	}
	switch strings.ToUpper(name) {
	case "MATCHINFO":
		return ev.evalMatchinfoFunc(f, row)
	case "OFFSETS":
		return ev.evalFTSSnippetAux("offsets", f, row)
	case "SNIPPET":
		return ev.evalFTSSnippetAux("snippet", f, row)
	case "OPTIMIZE":
		return ev.evalFTSOptimize(f)
	}
	return nil, nil
}

// ftsAuxTableName resolves an FTS3 auxiliary function's table: the current
// MATCH context, or the function's first argument when there is none.
func (ev *Evaluator) ftsAuxTableName(f *sql.FuncCall) string {
	tableName := ev.ctx.CurrentFTSMatch()
	if tableName == "" && len(f.Args) > 0 {
		if colRef, ok := f.Args[0].(*sql.ColumnRef); ok && colRef.Name != "" {
			tableName = colRef.Name
		}
	}
	return tableName
}

// validateFTSAuxArgs reproduces the argument checks SQLite applies before an
// FTS3 auxiliary function runs: arity limits and the first-argument-must-be-
// the-matching-fts-table rule.
func (ev *Evaluator) validateFTSAuxArgs(name string, f *sql.FuncCall) error {
	lower := strings.ToLower(name)
	switch strings.ToUpper(name) {
	case "MATCHINFO":
		if len(f.Args) < 1 || len(f.Args) > 2 {
			return fmt.Errorf("wrong number of arguments to function %s()", lower)
		}
	case "SNIPPET":
		if len(f.Args) > 6 {
			return fmt.Errorf("wrong number of arguments to function snippet()")
		}
		if len(f.Args) == 0 {
			// fts3.c fts3SnippetFunc checks argc<1 before it can identify a
			// table argument, so zero args report the missing cursor context
			// rather than an arity error (oracle-verified; e_fts3 2.1.7).
			return fmt.Errorf("unable to use function snippet in the requested context")
		}
	case "OFFSETS":
		if len(f.Args) != 1 {
			return fmt.Errorf("wrong number of arguments to function %s()", lower)
		}
	}
	// OPTIMIZE (and any other aux function reaching the first-argument
	// check) with no args at all reports the arity error.
	if len(f.Args) == 0 {
		return fmt.Errorf("wrong number of arguments to function %s()", lower)
	}
	colRef, ok := f.Args[0].(*sql.ColumnRef)
	if !ok || colRef.Name == "" {
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	return ev.validateFTSAuxTarget(colRef, lower)
}

// validateFTSAuxTarget checks the first-argument-must-be-the-matching-fts-
// table rule (fts3.c fts3FunctionArg L3690-3699: argv[0] must be the hidden
// fts3 cursor of the CURRENTLY MATCHING fts table; anything else fails with
// "illegal first argument to <func>").
func (ev *Evaluator) validateFTSAuxTarget(colRef *sql.ColumnRef, lower string) error {
	current := ev.ctx.CurrentFTSMatch()
	if current == "" {
		// No active MATCH-cursor context (JOIN/derived-table queries): the
		// legacy fallback resolves the first argument as an FTS table name;
		// unknown tables stay illegal.
		if _, ok := ev.ctx.FTSTables()[colRef.Name]; ok {
			return nil
		}
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	if !strings.EqualFold(colRef.Name, current) {
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	if _, ok := ev.ctx.FTSTables()[current]; !ok {
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	return nil
}

// evalMatchinfoFunc implements the FTS3 matchinfo() function (fts3_snippet.c
// fts3GetMatchinfo / fts3MatchinfoValues): it returns a blob of little-endian
// uint32 values in format-string order. The format letters are:
//
//	'p' number of phrases in the MATCH query (1 value)
//	'c' number of columns (1 value)
//	'n' number of documents (1 value, FTS4 only)
//	'a' average token count per column (nCol values, FTS4 only)
//	'l' per-column token count of the current row (nCol values, docsize)
//	'x' per phrase and column: local hits, global occurrences, global rows
//	'y' per phrase and column: local hit counts
//	'b' per phrase: bitmask of columns with local hits
//
// The phrase structure comes from the current FTS SELECT's MATCH constraint
// (SetFTSMatchInfo); without a MATCH the function returns an empty blob.
func (ev *Evaluator) evalMatchinfoFunc(f *sql.FuncCall, row Row) (interface{}, error) {
	// matchinfo(TABLE) — the first argument names the FTS table when there is
	// no MATCH context.
	tableName := ev.ftsAuxTableName(f)
	ftsTable, ok := ev.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return []byte{}, nil
	}
	format, err := ev.matchinfoFormat(f, row)
	if err != nil {
		return nil, err
	}
	// Validate the format string against the table's capabilities
	// (fts3_snippet.c fts3MatchinfoCheck): 'p'/'c' are always recognized;
	// 'n' and 'a' require an FTS4 table; 'l' requires the %_docsize table
	// (absent for FTS3 and for FTS4 created with matchinfo=fts3).
	if err := validateMatchinfoFormat(ftsTable, format); err != nil {
		return nil, err
	}

	// The matchinfo query context: the FTS table's MATCH phrases (set by
	// execFTSSelect). Without a MATCH constraint the blob is empty
	// (fts3matchinfo 7.2/7.3: typeof=blob, length=0).
	ctxTable, hasMatch, phrases := ev.ctx.FTSMatchInfo()
	if !strings.EqualFold(ctxTable, tableName) || !hasMatch {
		return []byte{}, nil
	}

	// Current row's docid (the FTS row map stores it under "rowid").
	docID := rowDocID(row)

	nCol := int64(len(ftsTable.ColumnNames()))
	nPhrase := int64(len(phrases))
	var out []byte
	if err := ev.writeMatchinfoFormat(ftsTable, format, phrases, docID, nCol, nPhrase, func(v uint32) {
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// matchinfoFormat evaluates the optional matchinfo(TABLE[, fmt]) format
// argument, defaulting to "pcx" (fts3_snippet.c fts3FunctionArg).
func (ev *Evaluator) matchinfoFormat(f *sql.FuncCall, row Row) (string, error) {
	format := "pcx"
	if len(f.Args) > 1 {
		v, err := ev.evalExpr(f.Args[1], row)
		if err != nil {
			return "", err
		}
		if s, ok := util.UnwrapColumnValue(v).(string); ok {
			format = s
		}
	}
	return format, nil
}

// validateMatchinfoFormat rejects format letters the table does not support
// (fts3_snippet.c fts3MatchinfoCheck): 'n'/'a' need an FTS4 table, 'l' needs
// the %_docsize table.
func validateMatchinfoFormat(ftsTable *fts.FTS3Table, format string) error {
	hasDocsize := ftsTable.IsFTS4() && !ftsTable.NoDocsize()
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case 'p', 'c', 'x', 'y', 'b':
			// always recognized
		case 'n', 'a':
			if !ftsTable.IsFTS4() {
				return fmt.Errorf("unrecognized matchinfo request: %c", format[i])
			}
		case 'l':
			if !hasDocsize {
				return fmt.Errorf("unrecognized matchinfo request: l")
			}
		default:
			// fts3_snippet.c fts3MatchinfoCheck L1005: any other letter
			// fails the statement ("unrecognized matchinfo request: d").
			return fmt.Errorf("unrecognized matchinfo request: %c", format[i])
		}
	}
	return nil
}

// rowDocID returns the FTS row map's docid (the row map stores it under
// "rowid").
func rowDocID(row Row) int64 {
	var docID int64
	if rv, ok := row.Get("rowid"); ok {
		if dv, ok := util.UnwrapColumnValue(rv).(int64); ok {
			docID = dv
		}
	}
	return docID
}

// matchinfoCtx binds one matchinfo format interpretation: the table, the
// MATCH phrases, the current docid, the column/phrase counts and the output
// sink.
type matchinfoCtx struct {
	ev      *Evaluator
	table   *fts.FTS3Table
	phrases []fts.MatchPhrase
	docID   int64
	nCol    int64
	nPhrase int64
	write   func(uint32)
}

// matchinfoFormatHandlers interprets one format letter (fts3_snippet.c
// fts3MatchinfoValues). Unknown letters are ignored by SQLite's matchinfo
// (fts3MatchinfoCheck errors, but the engine's compat suite does not exercise
// error paths here), so they have no entry.
var matchinfoFormatHandlers = map[byte]func(*matchinfoCtx) error{
	'p': func(m *matchinfoCtx) error {
		m.write(uint32(m.nPhrase))
		return nil
	},
	'c': func(m *matchinfoCtx) error {
		m.write(uint32(m.nCol))
		return nil
	},
	'n': func(m *matchinfoCtx) error {
		// FTS4 %_stat doctotal: the document count.
		n, err := m.ev.matchinfoDoctotal(m.table, m.nCol)
		if err != nil {
			return err
		}
		m.write(n)
		return nil
	},
	'a': func(m *matchinfoCtx) error {
		vals, err := m.ev.matchinfoAverages(m.table, m.nCol)
		if err != nil {
			return err
		}
		for _, v := range vals {
			m.write(v)
		}
		return nil
	},
	'l': func(m *matchinfoCtx) error {
		vals, err := m.ev.matchinfoLengths(m.table, m.nCol, m.docID)
		if err != nil {
			return err
		}
		for _, v := range vals {
			m.write(v)
		}
		return nil
	},
	'x': func(m *matchinfoCtx) error {
		writeMatchinfoHits(m.table, m.phrases, m.docID, func(mp fts.MatchPhrase) []uint32 {
			return m.table.MatchInfoX(mp.Node, mp.Scope, mp.Side, mp.Gate, m.docID)
		}, m.write)
		return nil
	},
	'y': func(m *matchinfoCtx) error {
		writeMatchinfoHits(m.table, m.phrases, m.docID, func(mp fts.MatchPhrase) []uint32 {
			return m.table.MatchInfoY(mp.Node, mp.Scope, mp.Side, mp.Gate, m.docID)
		}, m.write)
		return nil
	},
	'b': func(m *matchinfoCtx) error {
		// nPhrase * ceil(nCol/32) values: per phrase, a bitmask of
		// columns with at least one local hit.
		bmWords := (m.nCol + 31) / 32
		for _, mp := range m.phrases {
			y := m.table.MatchInfoY(mp.Node, mp.Scope, mp.Side, mp.Gate, m.docID)
			for w := int64(0); w < bmWords; w++ {
				m.write(matchinfoBitmask(y, m.nCol, w))
			}
		}
		return nil
	},
}

// writeMatchinfoFormat appends the matchinfo values for one format string to
// the output via writeU32 (fts3_snippet.c fts3MatchinfoValues). The 'n' and
// 'a' formats read the FTS4 %_stat doctotal blob and 'l' reads the %_docsize
// blob for the current row (sqlite3Fts3SelectDoctotal / fts3SelectDocsize); a
// corrupt or missing blob errors "database disk image is malformed"
// (FTS_CORRUPT_VTAB).
func (ev *Evaluator) writeMatchinfoFormat(ftsTable *fts.FTS3Table, format string, phrases []fts.MatchPhrase, docID int64, nCol, nPhrase int64, writeU32 func(uint32)) error {
	m := &matchinfoCtx{ev: ev, table: ftsTable, phrases: phrases, docID: docID, nCol: nCol, nPhrase: nPhrase, write: writeU32}
	for i := 0; i < len(format); i++ {
		h, ok := matchinfoFormatHandlers[format[i]]
		if !ok {
			continue
		}
		if err := h(m); err != nil {
			return err
		}
	}
	return nil
}

// matchinfoDoctotal reads the FTS4 %_stat doctotal blob and returns its first
// varint, the document count (fts3_snippet.c fts3MatchinfoSelectDoctotal:
// the first varint is nDoc; nDoc<=0 or an overrun is FTS_CORRUPT_VTAB).
func (ev *Evaluator) matchinfoDoctotal(ftsTable *fts.FTS3Table, nCol int64) (uint32, error) {
	blob, err := ev.ctx.FTSShadowBlob(ftsTable.Name(), "doctotal", 0)
	if err != nil {
		return 0, err
	}
	nDoc, consumed := fts.GetFTS3Varint(blob)
	if consumed == 0 || nDoc <= 0 {
		return 0, fmt.Errorf("database disk image is malformed")
	}
	return uint32(nDoc), nil
}

// matchinfoAverages computes the 'a' average-token-count values
// ((total + nDoc/2) / nDoc) from the %_stat doctotal blob (fts3_snippet.c
// FTS3_MATCHINFO_AVGLENGTH: the blob is nDoc followed by one per-column
// token-total varint; an overrun is FTS_CORRUPT_VTAB).
func (ev *Evaluator) matchinfoAverages(ftsTable *fts.FTS3Table, nCol int64) ([]uint32, error) {
	blob, err := ev.ctx.FTSShadowBlob(ftsTable.Name(), "doctotal", 0)
	if err != nil {
		return nil, err
	}
	nDoc, consumed := fts.GetFTS3Varint(blob)
	if consumed == 0 || nDoc <= 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	pos := consumed
	out := make([]uint32, nCol)
	for i := int64(0); i < nCol; i++ {
		total, n := fts.GetFTS3Varint(blob[pos:])
		if n == 0 || pos+n > len(blob) {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		pos += n
		out[i] = uint32((uint64(total) + nDoc/2) / nDoc)
	}
	return out, nil
}

// matchinfoLengths reads the %_docsize blob for the current row and decodes
// nCol per-column token-count varints (fts3_snippet.c FTS3_MATCHINFO_LENGTH:
// the blob is one FTS3 varint per column; an overrun is FTS_CORRUPT_VTAB).
func (ev *Evaluator) matchinfoLengths(ftsTable *fts.FTS3Table, nCol int64, docID int64) ([]uint32, error) {
	blob, err := ev.ctx.FTSShadowBlob(ftsTable.Name(), "docsize", docID)
	if err != nil {
		return nil, err
	}
	pos := 0
	out := make([]uint32, nCol)
	for i := int64(0); i < nCol; i++ {
		v, n := fts.GetFTS3Varint(blob[pos:])
		if n == 0 || pos+n > len(blob) {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		pos += n
		out[i] = uint32(v)
	}
	return out, nil
}

// writeMatchinfoHits writes one phrase's per-column values for the 'x' and
// 'y' formats (per phrase, per column, in phrase order).
func writeMatchinfoHits(ftsTable *fts.FTS3Table, phrases []fts.MatchPhrase, docID int64, values func(fts.MatchPhrase) []uint32, writeU32 func(uint32)) {
	for _, mp := range phrases {
		for _, v := range values(mp) {
			writeU32(v)
		}
	}
}

// matchinfoBitmask computes one 32-bit word of the 'b' format bitmap for a
// phrase's per-column hit counts: word w holds bits for columns w*32..w*32+31
// that have at least one local hit (fts3_snippet.c fts3ExprLHits).
func matchinfoBitmask(y []uint32, nCol, w int64) uint32 {
	var mask uint32
	for c := int64(0); c < 32; c++ {
		col := w*32 + c
		if col < nCol && y[col] > 0 {
			mask |= 1 << uint(c)
		}
	}
	return mask
}

// evalFTSSnippetAux evaluates the FTS3 offsets() and snippet() auxiliary
// functions. Both need the current FTS SELECT's MATCH phrases (from the
// matchinfo context) and the current row's docid; without a MATCH they
// return an empty string (fts3_snippet.c: pCsr->pExpr NULL → "").
func (ev *Evaluator) evalFTSSnippetAux(name string, f *sql.FuncCall, row Row) (interface{}, error) {
	tableName := ev.ftsAuxTableName(f)
	ftsTable, ok := ev.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return "", nil
	}
	ctxTable, hasMatch, phrases := ev.ctx.FTSMatchInfo()
	if !strings.EqualFold(ctxTable, tableName) || !hasMatch {
		return "", nil
	}
	docID := rowDocID(row)
	if name == "offsets" {
		s, err := ftsTable.Offsets(docID, phrases, ev.ftsContentOverride(ftsTable, row))
		if err != nil {
			return "", err
		}
		return s, nil
	}
	zStart, zEnd, zEllipsis, iCol, nToken := ev.snippetArgs(f, row)
	return ftsTable.Snippet(docID, phrases, zStart, zEnd, zEllipsis, iCol, nToken, ev.ftsContentOverride(ftsTable, row)), nil
}

// ftsContentOverride returns the content-row column values for an FTS4
// content=<table> table from the current row map (keyed by column name), or
// nil for a normal FTS table. SQLite reads the content table for
// offsets()/snippet() column text (fts3_snippet.c reads the row via the
// content table); the row map built by ftsContentTableRowMapsForDocIDs
// carries those values, so a document whose content row was updated after
// indexing shows the NEW text (fts4content 2.4.3/2.5.x).
func (ev *Evaluator) ftsContentOverride(ftsTable *fts.FTS3Table, row Row) []interface{} {
	if ftsTable.ContentTable() == "" {
		return nil
	}
	names := ftsTable.ColumnNames()
	out := make([]interface{}, len(names))
	for i, cn := range names {
		if v, ok := row.Get(cn); ok {
			out[i] = util.UnwrapColumnValue(v)
		}
	}
	return out
}

// snippetArgs evaluates the optional snippet(TABLE, zStart, zEnd, zEllipsis,
// iCol, nToken) arguments, returning the defaults when absent.
func (ev *Evaluator) snippetArgs(f *sql.FuncCall, row Row) (zStart, zEnd, zEllipsis string, iCol, nToken int) {
	zStart, zEnd, zEllipsis = "<b>", "</b>", "<b>...</b>"
	iCol, nToken = -1, 15
	if s, ok := ev.snippetStringArg(f, 1, row); ok {
		zStart = s
	}
	if s, ok := ev.snippetStringArg(f, 2, row); ok {
		zEnd = s
	}
	if s, ok := ev.snippetStringArg(f, 3, row); ok {
		zEllipsis = s
	}
	iCol = ev.snippetIntArg(f, 4, row, iCol)
	nToken = ev.snippetIntArg(f, 5, row, nToken)
	return zStart, zEnd, zEllipsis, iCol, nToken
}

// snippetStringArg evaluates snippet's optional string argument i, returning
// ok=false (keep the default) when absent, failed to evaluate, or not text.
func (ev *Evaluator) snippetStringArg(f *sql.FuncCall, i int, row Row) (string, bool) {
	if len(f.Args) <= i {
		return "", false
	}
	v, err := ev.evalExpr(f.Args[i], row)
	if err != nil {
		return "", false
	}
	s, ok := util.UnwrapColumnValue(v).(string)
	return s, ok
}

// snippetIntArg evaluates snippet's optional integer argument i, returning
// def when absent or failed to evaluate.
func (ev *Evaluator) snippetIntArg(f *sql.FuncCall, i int, row Row, def int) int {
	if len(f.Args) <= i {
		return def
	}
	v, err := ev.evalExpr(f.Args[i], row)
	if err != nil {
		return def
	}
	return int(ToIntValue(util.UnwrapColumnValue(v)))
}

// evalFTSOptimize implements the FTS3 optimize(TABLE) auxiliary function
// (fts3.c fts3OptimizeFunc): it merges the table's segments and returns
// "Index optimized" (or "Index already optimal" when no merge was needed).
func (ev *Evaluator) evalFTSOptimize(f *sql.FuncCall) (interface{}, error) {
	tableName := ev.ftsAuxTableName(f)
	ftsTable, ok := ev.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return "", nil
	}
	_ = ftsTable
	// fts3.c fts3DoOptimize: flush the pending terms FIRST (an unflushed
	// batch becomes its own segment), then merge. SQLITE_DONE ("Index
	// already optimal") is returned only when the post-flush index is a
	// single segment — i.e. nothing needed merging (fts3f 1.3: the first
	// optimize() flushes the open transaction's pending docs, creating a
	// second segment, so it reports "Index optimized"; every later call
	// sees one segment and reports "Index already optimal").
	if ev.ftsOptimizeAlreadyOptimal(tableName) {
		return "Index already optimal", nil
	}
	if _, exErr := ev.ctx.EvalExecSQL("INSERT INTO "+tableName+"("+tableName+") VALUES('optimize')", ""); exErr != nil {
		return nil, exErr
	}
	return "Index optimized", nil
}

// ftsOptimizeAlreadyOptimal reports whether the table's index is a single
// segment with no pending terms (nothing to merge).
func (ev *Evaluator) ftsOptimizeAlreadyOptimal(tableName string) bool {
	before, cerr := ev.ctx.EvalExecSQL("SELECT count(*) FROM "+tableName+"_segdir", "")
	hasPending := false
	if t, ok := ev.ctx.FTSTables()[tableName]; ok && t != nil {
		hasPending = len(t.PendingSnapshot()) > 0
	}
	if cerr == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(before)); perr == nil && n <= 1 && !hasPending {
			return true
		}
	}
	return false
}
