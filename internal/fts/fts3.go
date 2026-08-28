package fts

import (
	"fmt"
	"strings"
	"sync"
)

// FTS3Table is an in-memory FTS3/4 virtual table data store.
type FTS3Table struct {
	mu          sync.Mutex
	name        string
	moduleName  string
	columnNames []string
	tokenizer   Tokenizer
	index       *InvertedIndex
	// pendingDocIDs are the doc IDs inserted since the last segment flush
	// (a transaction boundary in SQLite's FTS3: one segment per flush). They
	// are written to %_segdir as a single segment at COMMIT.
	pendingDocIDs []int64
	// deletedDocIDs are the doc IDs removed since the last segment flush whose
	// content was already committed to %_segdir (a flushed document). SQLite
	// persists such a DELETE as a delete-marker segment — a segment whose
	// doclists contain only docids with no positions — so a reopen or a
	// segment reload does not resurrect the deleted document (fts3.c
	// fts3DeleteTerms / fts3SegWriterFlush; fts4content 3.1.5: DELETE FROM
	// ft3 then MATCH must not return the deleted row). A docid deleted before
	// its insert was ever flushed is removed from pendingDocIDs and needs no
	// marker (its pending segment is not written).
	deletedDocIDs []int64

	// deleteMarkerTerms snapshots each deleted document's token TERMS at
	// DELETE time (the doc's postings are removed from the in-memory index
	// immediately, so a flush-time DeleteMarkerRoot cannot re-derive them —
	// fts4onepass 3.x UPDATE SET docid=... and fts4content 3.1.5 DELETE
	// write a marker segment that lists the old terms). Keyed by docid.
	deleteMarkerTerms map[int64][]string
	// replaceDocs holds docids deleted AND re-inserted within one flush
	// batch (INSERT OR REPLACE of a flushed row): their pending-flush
	// segments carry the delete-marker entry merged into the same term
	// doclists as the new postings (SQLite's fts3DeleteTerms +
	// fts3InsertTerms share the pending batch, producing ONE segment per
	// index — fts4opt 2.x per-ROW parity), instead of a separate marker
	// segment. Cleared after each flush.
	replaceDocs map[int64]bool
	// nodeSize is the segment node size (the FTS table's page size by
	// default; the 'nodesize=N' special command overrides it, fts3corrupt4).
	nodeSize int
	// compressFn / uncompressFn mirror the FTS4 compress= and uncompress=
	// options (fts3.c fts3InitVtab: the constructor arguments name scalar SQL
	// functions applied to each column value when writing/reading the
	// %_content shadow table). The in-memory store keeps the uncompressed
	// text for MATCH; the content table stores the compressed values.
	compressFn   string
	uncompressFn string
	// orderDesc mirrors the FTS4 order=desc option: MATCH results are
	// returned in descending docid order instead of ascending (fts3.c
	// bDescIdx, fts3EvalNext). The in-memory store only uses it to order
	// MatchDocIDs results.
	orderDesc bool
	// noDocsize mirrors FTS4's matchinfo=fts3 option (fts3.c fts3InitVtab
	// case MATCHINFO: bNoDocsize=1). A table created with matchinfo=fts3 has
	// no %_docsize shadow table and rejects the matchinfo 'l' format.
	noDocsize bool
	// prefixLengths holds the FTS4 prefix= index lengths in declaration
	// order (fts3.c fts3PrefixParameter: aIndex[1..] one entry per comma
	// value, zeros and out-of-range values dropped, order preserved). Index 0
	// is the main index; prefix index i has length prefixLengths[i-1].
	prefixLengths []int
	// notindexed holds the FTS4 notindexed=<col> option's column names
	// (lowercased). Those columns' text is stored in %_content but never
	// tokenized or indexed (fts3.c fts3InsertTerms checks
	// fts3ColumnIsIndexed; fts4noti 2.x: MATCH on a notindexed column's
	// terms returns no rows).
	notindexed map[string]bool
	// loadErr records a segment-loading failure (a real SQLite segment whose
	// structure is corrupt). It is checked at the start of FTS operations so
	// a corrupt segment surfaces as "database disk image is malformed" even
	// when the in-memory index was partially populated (fts3corrupt4 7.1).
	loadErr error
	// structuralLoadErr marks a broken segment b-tree (interior chain): no
	// term lookup can succeed, so every MATCH query fails.
	structuralLoadErr bool
	// corruptContentDocIDs records %_content rowids whose record failed to
	// decode during the content rebuild. A query that MATCHES one of these
	// docids must fail with "database disk image is malformed" (fts3corrupt4
	// 11.1), while queries that never read the row succeed (9.1).
	corruptContentDocIDs map[int64]bool
	// contentBtreeUnreadable records that the %_content shadow btree could
	// not be navigated at load time (a crash-written page fails ParsePage).
	// Index-only queries still work; a query that reads content columns
	// fails with "database disk image is malformed" (fts3corrupt4 52.1).
	contentBtreeUnreadable bool
	// contentTable is the FTS4 content= option's external content table name
	// (empty for a normal FTS table with an internal %_content shadow).
	contentTable string
	// contentless reports that the FTS4 content= option was given with an
	// EMPTY value (content=). A contentless table stores no document text at
	// all: it has no %_content shadow and no external content table, so
	// SELECT of a content column fails with "SQL logic error" while docid
	// queries and MATCH work off the index (fts3.c bContentless; fts4content
	// 7.2.x).
	contentless bool
	// langidColName mirrors the FTS4 languageid=<col> option's column name
	// (fts3.c fts3InitVtab case LANGID: p->zLanguageid). The hidden langid
	// column lets an application store per-language documents; the engine
	// parses and records the name here (the full per-language indexing
	// behavior is implemented in the vtab column wiring).
	langidColName string
	// statNDoc / statTotals / statTotalBytes cache the FTS4 %_stat doctotal
	// aggregate (nDoc, per-column token totals, total text bytes). It is
	// maintained incrementally on insert/delete so a per-row flush does not
	// re-scan the whole index (O(n^2) over a large per-row build, e.g.
	// fts3_build_db_2 20000). statDirty marks the cache stale (segment load,
	// rebuild) so TokenStats recomputes from the index once.
	statNDoc       int64
	statTotals     []int64
	statTotalBytes int64
	statDirty      bool
	// segdirIdxValid / segdirNextIdx cache the next %_segdir idx per absolute
	// level (SQLite allocates idx sequentially 0..n-1 per level). The cache is
	// maintained by the segment flush/merge writer and invalidated by any
	// direct SQL write to %_segdir, so a per-row build is not O(n^2) in the
	// max(idx) scan (fts3_build_db_2 20000). segdirNextIdxValid marks whether
	// the cache reflects the persisted table (false after a direct write or
	// table load, forcing one rescan).
	segdirIdxValid   bool
	segdirNextIdx    map[int]int
	nextBlockIDValid bool
	nextBlockID      int
	// automerge mirrors the FTS 'automerge=X' setting (fts3.c fts3DoAutoincrmerge):
	// 0 = off, >=2 = flush-time incremental merge uses this many segments as
	// nMin. automergeKnown distinguishes "explicitly set" from the default
	// unknown (0xff in SQLite), which is treated as 0 until set.
	automerge      int
	automergeKnown bool
	// mergeCtx tracks the incremental-merge writer state per output level
	// (SQLite's IncrmergeWriter persisted across calls via the pre-allocated
	// block range and the last leaf's fill; the engine tracks it in memory
	// because it rebuilds output segments instead of appending to a
	// pre-allocated range). nLeafEst bounds the number of leaf pages the
	// merge may flush (fts3IncrmergeAppend: iBlock < iStart+nLeafEst); iBlock
	// is the count of leaves written so far; buffer is the current leaf
	// buffer fill (the continuation resumes from it — SQLite's nearly-empty
	// last leaf after a quota stop, which makes the next merge consume a full
	// page instead of stalling at 1-3 terms). Cleared by InvalidateSegmentCache
	// (reopen/DDL) so a merge resumes from the stored last leaf.
	mergeCtx map[int]*MergeCtx
}

// MergeCtx is the incremental-merge writer state for one output level (see
// FTS3Table.mergeCtx).
type MergeCtx struct {
	// NLeafEst bounds the flushes (SQL_MAX_LEAF_NODE_ESTIMATE: 2*total(1 +
	// leaves_end - start_block) over the merged level, or (end-start+1)/16
	// when continuing a pre-allocated output).
	NLeafEst int
	// IBlock counts leaf pages written so far (0-based; the flush condition
	// is IBlock < NLeafEst).
	IBlock int
	// Buffer is the current leaf-buffer fill in bytes (the continuation
	// resumes from this).
	Buffer int
	// OutRowID is the %_segdir rowid of the appendable output segment this
	// state belongs to (SQLite's fts3IsAppendable check: only the segment
	// the incremental merge itself left interrupted may be appended to; a
	// crisis-merge or flush segment written afterwards is not appendable).
	OutRowID int64
	// MarkerID is the pre-allocated range's end block (SQLite's iEnd, the
	// NULL marker row written by fts3IncrmergeWriter). Continuations keep
	// their leaf allocation below it and end_block keeps pointing at it.
	MarkerID int
}

// addDocStats accumulates one document's sizes into the %_stat cache (called
// after the document is inserted into the index).
func (t *FTS3Table) addDocStats(docID int64) {
	ds := t.index.DocSizeInfo(docID)
	if ds == nil {
		return
	}
	if t.statTotals == nil {
		t.statTotals = make([]int64, len(t.columnNames))
	}
	t.statNDoc++
	for i := 0; i < len(t.statTotals) && i < len(ds.Counts); i++ {
		t.statTotals[i] += int64(ds.Counts[i])
	}
	t.statTotalBytes += ds.TotalBytes
}

// subDocStats subtracts one document's sizes from the %_stat cache (called
// BEFORE the document is removed from the index, so DocSizeInfo is still
// available).
func (t *FTS3Table) subDocStats(docID int64) {
	ds := t.index.DocSizeInfo(docID)
	if ds == nil {
		return
	}
	t.statNDoc--
	for i := 0; i < len(t.statTotals) && i < len(ds.Counts); i++ {
		t.statTotals[i] -= int64(ds.Counts[i])
	}
	t.statTotalBytes -= ds.TotalBytes
}

// StatTotals returns the %_stat doctotal aggregate (nDoc, per-column token
// totals, total text bytes). The cache is used when current; otherwise it is
// recomputed from the index (TokenStats) and cached.
func (t *FTS3Table) StatTotals() (int64, []int64, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.statDirty || t.statTotals == nil {
		nDoc, totals := t.tokenStatsLocked()
		t.statNDoc = nDoc
		t.statTotals = totals
		t.statTotalBytes = 0
		for _, id := range t.index.AllDocIDs() {
			if ds := t.index.DocSizeInfo(id); ds != nil {
				t.statTotalBytes += ds.TotalBytes
			}
		}
		t.statDirty = false
	}
	return t.statNDoc, append([]int64(nil), t.statTotals...), t.statTotalBytes
}

// SegdirNextIdx returns the cached next %_segdir idx for an absolute level and
// whether the cache is valid for it. When false, the caller must scan the
// %_segdir table and then call SetSegdirNextIdx.
func (t *FTS3Table) SegdirNextIdx(level int) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.segdirIdxValid || t.segdirNextIdx == nil {
		return 0, false
	}
	idx, ok := t.segdirNextIdx[level]
	if !ok {
		return 0, false
	}
	return idx, true
}

// SetSegdirNextIdx records the next %_segdir idx for an absolute level (the
// level's max(idx)+1) so the per-flush allocation is O(1).
func (t *FTS3Table) SetSegdirNextIdx(level, idx int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.segdirNextIdx == nil {
		t.segdirNextIdx = make(map[int]int)
	}
	t.segdirIdxValid = true
	t.segdirNextIdx[level] = idx
}

// NextBlockID returns the cached next %_segments block ID and whether the
// cache is valid. When false, the caller must scan %_segments and call
// SetNextBlockID.
func (t *FTS3Table) NextBlockID() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nextBlockID, t.nextBlockIDValid
}

// SetNextBlockID records the next %_segments block ID (max(blockid)+1) so the
// per-flush block allocation is O(1).
func (t *FTS3Table) SetNextBlockID(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextBlockID = id
	t.nextBlockIDValid = true
}

// InvalidateSegmentCache drops the segdir-idx / segments-block caches after a
// direct SQL write to a shadow table or a table reload, forcing the next flush
// to rescan the persisted state.
func (t *FTS3Table) InvalidateSegmentCache() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.segdirIdxValid = false
	t.segdirNextIdx = nil
	t.nextBlockIDValid = false
	t.nextBlockID = 0
	t.mergeCtx = nil
}

// InvalidateSegmentCacheKeepMergeCtx is InvalidateSegmentCache but PRESERVES
// the tracked incremental-merge writer state (mergeCtx). The incremental merge
// calls it after each level iteration to force a segdir-idx rescan, but the
// writer state for a DIFFERENT level (e.g. an L2 continuation from a previous
// automerge call) must survive — clearing it made the next iteration create a
// NEW output segment instead of appending to the tracked one (fts4merge4
// 2.2.x: the tx-20 L1 merge created L2[1] instead of appending to L2[0]).
func (t *FTS3Table) InvalidateSegmentCacheKeepMergeCtx() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.segdirIdxValid = false
	t.segdirNextIdx = nil
	t.nextBlockIDValid = false
	t.nextBlockID = 0
}

// MergeCtxFor returns the tracked incremental-merge writer state for an output
// level, or nil when none is tracked (a fresh merge or one interrupted by a
// table reload).
func (t *FTS3Table) MergeCtxFor(level int) *MergeCtx {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.mergeCtx == nil {
		return nil
	}
	return t.mergeCtx[level]
}

// SetMergeCtx records the incremental-merge writer state for an output level.
func (t *FTS3Table) SetMergeCtx(level int, ctx *MergeCtx) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.mergeCtx == nil {
		t.mergeCtx = make(map[int]*MergeCtx)
	}
	t.mergeCtx[level] = ctx
}

// ClearMergeCtx drops the incremental-merge writer state for an output level
// (called when the merge fully consumes its level and the output is done).
func (t *FTS3Table) ClearMergeCtx(level int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.mergeCtx, level)
}

// Automerge returns the table's automerge setting (0 = off) and whether it has
// been explicitly set (SQLite's nAutoincrmerge; the default is "unknown" and
// treated as off until an automerge= command sets it).
func (t *FTS3Table) Automerge() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.automerge, t.automergeKnown
}

// SetAutomerge sets the table's automerge value from the automerge= command
// (fts3.c fts3DoAutoincrmerge: 1 or > MergeCount map to 8).
func (t *FTS3Table) SetAutomerge(v int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v == 1 || v > 16 {
		v = 8
	}
	t.automerge = v
	t.automergeKnown = true
}

// RootBlobBytes converts a %_segdir.root stored value ([]byte or string) into
// the raw root blob. SQLite stores the root as a BLOB; the engine's record
// decoding may produce either form.
func RootBlobBytes(v interface{}) []byte {
	switch rv := v.(type) {
	case []byte:
		return rv
	case string:
		return []byte(rv)
	}
	return nil
}

// NewFTS3Table creates a new FTS3 table with the given configuration.
// The argument parsing mirrors SQLite's fts3InitVtab loop (ext/fts3/fts3.c
// lines ~1246-1350): the first "tokenize=..." (or "tokenize ...") argument
// when no tokenizer has been set initializes the tokenizer; for FTS4 an
// argument containing '=' must name a known special option (matchinfo,
// prefix, compress, uncompress, order, content, languageid, notindexed) or
// the table creation fails with "unrecognized parameter"; every other
// argument is a column whose name is its first identifier token
// (sqlite3Fts3NextToken), so "xyz=abc" declares a column named "xyz".
func NewFTS3Table(name, moduleName string, args []string) (*FTS3Table, error) {
	t := &FTS3Table{
		name:       name,
		moduleName: moduleName,
		tokenizer:  &SimpleTokenizer{},
		index:      NewInvertedIndex(),
	}
	isFts4 := strings.EqualFold(moduleName, "fts4")

	var cols []string
	tokenizerSet := false
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		// Tokenizer specification: only the first one, only when the text
		// starts with "tokenize" followed by a non-identifier character.
		if !tokenizerSet && len(arg) > 8 &&
			strings.HasPrefix(strings.ToLower(arg), "tokenize") &&
			!isFTSIdChar(arg[8]) {
			tokenizerName := strings.TrimSpace(arg[9:])
			tokenizerName = strings.TrimPrefix(tokenizerName, "=")
			tokenizerName = strings.TrimSpace(tokenizerName)
			tok, terr := NewTokenizerFromSpec(tokenizerName, nil)
			if terr != nil {
				return nil, terr
			}
			t.tokenizer = tok
			tokenizerSet = true
			continue
		}

		// FTS4 special argument: an argument containing '=' must match one
		// of the known options; otherwise the CREATE fails (fts3.c
		// fts3IsSpecialColumn + aFts4Opt lookup). The engine accepts the
		// option (order=desc, content=..., prefix=..., notindexed=...,
		// languageid=..., matchinfo=..., compress/uncompress are parsed but
		// the in-memory store does not implement their semantics beyond
		// order=desc ordering behavior).
		if isFts4 && strings.Contains(arg, "=") {
			key := strings.TrimSpace(arg[:strings.Index(arg, "=")])
			switch strings.ToLower(key) {
			case "matchinfo":
				// matchinfo=fts3 is the only accepted value (fts3.c
				// fts3InitVtab case MATCHINFO): anything else fails the
				// CREATE with "unrecognized matchinfo: %s", and fts3
				// omits the %_docsize shadow table (bNoDocsize=1).
				val := strings.TrimSpace(arg[strings.Index(arg, "=")+1:])
				if len(val) != 4 || !strings.EqualFold(val, "fts3") {
					return nil, fmt.Errorf("unrecognized matchinfo: %s", val)
				}
				t.noDocsize = true
				continue
			case "prefix":
				// Parse the comma-separated prefix lengths with SQLite semantics
				// (fts3.c fts3PrefixParameter + fts3GobbleInt): each value must
				// start with a decimal digit or the whole prefix= parameter
				// fails with "error parsing prefix parameter: %s"; values >
				// MAX_NPREFIX (10000000) or > 0x7FFFFFFF are treated as 0 and
				// dropped; a 0 value is dropped. Order is preserved.
				val := strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				if val == "" {
					continue
				}
				parts := strings.Split(val, ",")
				var lens []int
				ok := true
				for _, part := range parts {
					if part == "" {
						ok = false
						break
					}
					n, okp := ftsParsePrefixInt(part)
					if !okp {
						ok = false
						break
					}
					if n != 0 {
						lens = append(lens, n)
					}
				}
				if !ok {
					return nil, fmt.Errorf("error parsing prefix parameter: %s", val)
				}
				t.prefixLengths = lens
				continue
			case "content":
				// content=<table> associates the FTS table with an external
				// content table (fts3.c fts3InitVtab case CONTENT): the index
				// is stored in %_segments/%_segdir but column values are read
				// from and written to the external table. A contentless table
				// (content="") has no content table and stores no column data.
				t.contentTable = strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				if t.contentTable == "" {
					t.contentless = true
				}
				continue
			case "languageid":
				t.langidColName = strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				continue
			case "notindexed":
				// notindexed=<col> names a column whose text is stored but
				// not indexed (fts3.c fts3InitVtab case NOTINDEXED: the name
				// is recorded in p->azNotindexed). Duplicates are tolerated;
				// an unknown column fails at CREATE (validated after all
				// arguments are parsed).
				ni := strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				if ni != "" {
					if t.notindexed == nil {
						t.notindexed = make(map[string]bool)
					}
					t.notindexed[strings.ToLower(ni)] = true
				}
				continue
			case "compress":
				t.compressFn = strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				continue
			case "uncompress":
				t.uncompressFn = strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				continue
			case "order":
				val := strings.Trim(strings.TrimSpace(arg[strings.Index(arg, "=")+1:]), "'\"")
				switch {
				case strings.EqualFold(val, "asc"):
					t.orderDesc = false
				case strings.EqualFold(val, "desc"):
					t.orderDesc = true
				default:
					// fts3.c fts3InitVtab: any other order value is rejected.
					return nil, fmt.Errorf("unrecognized order: %s", val)
				}
				continue
			default:
				return nil, fmt.Errorf("unrecognized parameter: %s", arg)
			}
		}

		// Otherwise the argument is a column name: the first identifier
		// token (sqlite3Fts3NextToken semantics).
		if colName := ftsNextToken(arg); colName != "" {
			cols = append(cols, colName)
		}
	}

	if len(cols) == 0 && t.contentTable == "" && !t.contentless {
		cols = []string{"content"}
	}

	// FTS4 compress and uncompress must be specified together (fts3.c
	// fts3InitVtab: rc==SQLITE_OK && (zCompress==0)!=(zUncompress==0) →
	// "missing %s parameter in fts4 constructor").
	if isFts4 && (t.compressFn == "") != (t.uncompressFn == "") {
		miss := "uncompress"
		if t.compressFn == "" {
			miss = "compress"
		}
		return nil, fmt.Errorf("missing %s parameter in fts4 constructor", miss)
	}

	t.columnNames = cols
	// Validate notindexed=<col> names against the declared columns
	// (fts3.c fts3InitVtab case NOTINDEXED validates against
	// p->nColumn after the column list is known; an unknown name fails
	// with "no such column: X" — fts4noti 1.1/1.8/1.10/1.11). A
	// content=<table> FTS table has no explicit column list here (names are
	// derived from the content table later); validation + wiring happen in
	// SetColumnNames for that case.
	if len(t.notindexed) > 0 && len(cols) > 0 {
		known := make(map[string]bool)
		for _, c := range cols {
			known[strings.ToLower(c)] = true
		}
		for ni := range t.notindexed {
			if !known[ni] {
				return nil, fmt.Errorf("no such column: %s", ni)
			}
		}
		// Wire the column indices into the inverted index so inserts skip
		// them (fts4noti 2.x: notindexed columns produce no postings).
		skip := make(map[int]bool)
		for i, c := range cols {
			if t.notindexed[strings.ToLower(c)] {
				skip[i] = true
			}
		}
		if len(skip) > 0 {
			t.index.SetSkipColumns(skip)
		}
	}
	return t, nil
}

// isFTSIdChar reports whether c is an FTS identifier character
// (sqlite3Fts3IsIdChar: alphanumeric, '_', or a byte >= 0x80).
func isFTSIdChar(c byte) bool {
	if c >= 0x80 {
		return true
	}
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ftsParsePrefixInt parses one prefix= integer with SQLite's semantics
// (fts3.c sqlite3Fts3ReadInt + fts3GobbleInt): the string must start with a
// decimal digit (else error); a value > 0x7FFFFFFF is treated as 0 (ReadInt
// returns -1, GobbleInt leaves the output 0); a value > MAX_NPREFIX
// (10000000) is clamped to 0. Returns the resulting length (0 = dropped) and
// ok=false when the string does not start with a digit.
func ftsParsePrefixInt(s string) (int, bool) {
	const maxNPrefix = 10000000
	i := 0
	var val uint64
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		val = val*10 + uint64(s[i]-'0')
		if val > 0x7FFFFFFF {
			// sqlite3Fts3ReadInt returns -1 without setting *pnOut; the caller
			// (fts3GobbleInt) then reads an uninitialized nInt. In practice the
			// oversized value is treated as 0 (dropped) — fts3prefix.test
			// 6.5.1 expects prefix="2147483648" to be accepted and equivalent
			// to no prefix.
			return 0, true
		}
	}
	if i == 0 {
		return 0, false
	}
	n := int(val)
	if n > maxNPrefix {
		n = 0
	}
	return n, true
}

// ftsNextToken returns the first identifier token of a column declaration
// string, mirroring sqlite3Fts3NextToken (ext/fts3/fts3_tokenizer.c): a
// quoted identifier ('...', "...", `...`, [...]) yields its unquoted
// content; otherwise the leading run of identifier characters. An empty or
// punctuation-only declaration yields "".
func ftsNextToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch s[0] {
	case '\'', '"', '`':
		q := s[0]
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == q {
				if i+1 < len(s) && s[i+1] == q {
					b.WriteByte(q)
					i++
					continue
				}
				return b.String()
			}
			b.WriteByte(s[i])
		}
		return b.String()
	case '[':
		if idx := strings.IndexByte(s, ']'); idx > 0 {
			return s[1:idx]
		}
		return ""
	default:
		for i := 0; i < len(s); i++ {
			if !isFTSIdChar(s[i]) {
				return s[:i]
			}
		}
		return s
	}
}

// Name returns the FTS table's name (the virtual table's SQL name).
func (t *FTS3Table) Name() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.name
}

// ContentTable returns the FTS4 content= option's external content table name
// (empty for a normal FTS table with an internal %_content shadow).
func (t *FTS3Table) ContentTable() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.contentTable
}

// Contentless reports whether the table was created with content= (an empty
// content= value): it has no content table and stores no document text, so
// reading a content column fails with "SQL logic error" (fts3.c bContentless).
func (t *FTS3Table) Contentless() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.contentless
}

// SetColumnNames replaces the table's column names (used to derive them from
// the content= table when the CREATE declared no explicit columns).
func (t *FTS3Table) SetColumnNames(cols []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.columnNames = cols
	// Validate notindexed=<col> names against the derived column list for a
	// content=<table> table (names come from the content table; an unknown
	// name fails — fts4noti 1.8: notindexed=d with content=cc → "no such
	// column: d"). SetColumnNames returns void, so the error is surfaced by
	// the caller re-checking (the CREATE path validates after deriving).
	if len(t.notindexed) > 0 {
		known := make(map[string]bool)
		for _, c := range cols {
			known[strings.ToLower(c)] = true
		}
		skip := make(map[int]bool)
		for i, c := range cols {
			if t.notindexed[strings.ToLower(c)] {
				skip[i] = true
			}
		}
		if len(skip) > 0 {
			t.index.SetSkipColumns(skip)
		}
	}
}

// ValidateNotindexedColumns checks every notindexed=<col> name against the
// table's current column list, returning the first unknown name or "" when
// all are valid. SetColumnNames is void, so the CREATE path calls this after
// deriving content= columns to surface "no such column: X" (fts4noti 1.8).
func (t *FTS3Table) ValidateNotindexedColumns() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.notindexed) == 0 {
		return ""
	}
	known := make(map[string]bool)
	for _, c := range t.columnNames {
		known[strings.ToLower(c)] = true
	}
	for ni := range t.notindexed {
		if !known[ni] {
			return ni
		}
	}
	return ""
}

// ColumnNames returns the table's column names.
