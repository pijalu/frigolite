// Command ftsview is a thin CLI over the internal/fts segview decoders,
// mirroring SQLite's ext/fts3/tool/fts3view.c inspection commands.
//
// Usage:
//
//	ftsview segment <db> <table> <id> [--raw]   id = blockid, or r<rowid> for a segdir root
//	ftsview doclist <db> <table> <id> <offset> <size>
//	ftsview segdir  <db> <table>                segment map from %_segdir geometry
//	ftsview stat    <db> <table>                %_stat rows with blob varints decoded
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	frigolite "github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/fts"
)

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "ftsview: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	raw := false
	var kept []string
	for _, a := range args {
		if a == "--raw" {
			raw = true
			continue
		}
		kept = append(kept, a)
	}
	if len(kept) < 3 {
		usage()
	}
	cmd, dbPath, table := kept[0], kept[1], kept[2]
	db, err := frigolite.Open(dbPath)
	if err != nil {
		fatal("open %s: %v", dbPath, err)
	}
	defer db.Close()

	switch cmd {
	case "segment":
		requireArgs(kept, 4)
		showSegment(db, table, kept[3], raw)
	case "doclist":
		requireArgs(kept, 6)
		showDoclist(db, table, kept[3], kept[4], kept[5])
	case "segdir":
		showSegdirMap(db, table)
	case "stat":
		showStat(db, table)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ftsview segment <db> <table> <id> [--raw]
  ftsview doclist <db> <table> <id> <offset> <size>
  ftsview segdir  <db> <table>
  ftsview stat    <db> <table>`)
	os.Exit(2)
}

func requireArgs(args []string, n int) {
	if len(args) < n {
		usage()
	}
}

// fetchBlob mirrors prepareToGetSegment (fts3view.c:654): id "rN" selects
// the segdir root blob at rowid N, otherwise %_segments.block at blockid N.
func fetchBlob(db *frigolite.DB, table, id string) ([]byte, error) {
	var query string
	if strings.HasPrefix(id, "r") {
		rowid, err := strconv.ParseInt(id[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad segdir rowid %q", id)
		}
		query = fmt.Sprintf("SELECT root FROM '%s_segdir' WHERE rowid=%d", table, rowid)
	} else {
		blockid, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad blockid %q", id)
		}
		query = fmt.Sprintf("SELECT block FROM '%s_segments' WHERE blockid=%d", table, blockid)
	}
	res := db.Query(query)
	if res.Error != nil {
		return nil, res.Error
	}
	rows := res.Rows
	if len(rows) == 0 || len(rows[0]) == 0 || rows[0][0] == nil {
		return nil, fmt.Errorf("no such segment %s", id)
	}
	switch v := rows[0][0].(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("segment %s: unexpected column type %T", id, rows[0][0])
	}
}

// showSegment mirrors showSegment (fts3view.c:680).
func showSegment(db *frigolite.DB, table, id string, raw bool) {
	blob, err := fetchBlob(db, table, id)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("Segment %s of size %d bytes:\n", id, len(blob))
	if raw {
		printBlob(blob)
		return
	}
	node, err := fts.DecodeSegmentBlock(blob)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("height: %d\n", node.Height)
	if node.Height > 0 {
		fmt.Printf("left-child: %d\n", node.LeftChild)
	}
	for _, e := range node.Entries {
		if node.Height == 0 {
			fmt.Printf("term: %-25s doclist %7d bytes offset %d\n", e.Term, e.DoclistSize, e.DoclistOffset)
		} else {
			fmt.Printf("term: %-25s child %d\n", e.Term, e.Child)
		}
	}
}

// showDoclist mirrors showDoclist (fts3view.c:750).
func showDoclist(db *frigolite.DB, table, id, offsetArg, sizeArg string) {
	offset, err := strconv.Atoi(offsetArg)
	if err != nil {
		fatal("bad offset %q", offsetArg)
	}
	size, err := strconv.Atoi(sizeArg)
	if err != nil {
		fatal("bad size %q", sizeArg)
	}
	blob, err := fetchBlob(db, table, id)
	if err != nil {
		fatal("%v", err)
	}
	if offset+size > len(blob) {
		fatal("doclist range [%d,%d) exceeds blob size %d", offset, offset+size, len(blob))
	}
	fmt.Printf("Doclist at %s offset %d of size %d bytes:\n", id, offset, size)
	docs, err := fts.DecodeDoclist(blob[offset : offset+size])
	if err != nil {
		fatal("%v", err)
	}
	for _, d := range docs {
		fmt.Printf("docid %d col0", d.DocID)
		for _, c := range d.Columns {
			if c.Col != 0 {
				fmt.Printf(" col%d", c.Col)
			}
			for _, p := range c.Positions {
				fmt.Printf(" %d", p)
			}
		}
		fmt.Println()
	}
}

// printTreeLine mirrors printTreeLine (fts3view.c:431).
func printTreeLine(lower, upper int64) {
	fmt.Printf("                 tree   %9d", lower)
	if upper > lower {
		fmt.Printf(" thru %9d  (%d blocks)", upper, upper-lower+1)
	}
	fmt.Println()
}

// segdirRow is one %_segdir record used by the segment map.
type segdirRow struct {
	level, idx     int64
	startBlock     int64
	leavesEndBlock int64
	endBlock       int64
	rowid          int64
}

// querySegdirRows reads all %_segdir rows for one index band, ordered like
// the C query (level DESC, idx).
func querySegdirRows(db *frigolite.DB, table string, index int) ([]segdirRow, error) {
	query := fmt.Sprintf(
		"SELECT level, idx, start_block, leaves_end_block, end_block, rowid FROM '%s_segdir' WHERE level/1024=%d ORDER BY level DESC, idx",
		table, index)
	res := db.Query(query)
	if res.Error != nil {
		return nil, res.Error
	}
	var rows []segdirRow
	for _, r := range res.Rows {
		get := func(i int) int64 { return asInt(r[i]) }
		rows = append(rows, segdirRow{
			level: get(0), idx: get(1),
			startBlock: get(2), leavesEndBlock: get(3), endBlock: get(4),
			rowid: get(5),
		})
	}
	return rows, nil
}

// showSegdirMap mirrors showSegdirMap (fts3view.c:458): per index band, the
// leaf/tree block ranges implied by each segdir's geometry.
func showSegdirMap(db *frigolite.DB, table string) {
	maxIndexRes := db.Query(fmt.Sprintf("SELECT max(level/1024) FROM '%s_segdir'", table))
	if maxIndexRes.Error != nil {
		fatal("%v", maxIndexRes.Error)
	}
	mxIndex := 0
	if len(maxIndexRes.Rows) > 0 && maxIndexRes.Rows[0][0] != nil {
		mxIndex = int(asInt(maxIndexRes.Rows[0][0]))
	}
	fmt.Printf("Number of inverted indices............... %3d\n", mxIndex+1)
	for iIndex := 0; iIndex <= mxIndex; iIndex++ {
		rows, err := querySegdirRows(db, table, iIndex)
		if err != nil {
			fatal("%v", err)
		}
		prevLevel := int64(-1)
		for _, r := range rows {
			iLevel := r.level % 1024
			if iLevel != prevLevel {
				fmt.Printf("level %2d idx %2d", iLevel, r.idx)
				prevLevel = iLevel
			} else {
				fmt.Printf("         idx %2d", r.idx)
			}
			fmt.Printf("  root   %9s\n", fmt.Sprintf("r%d", r.rowid))
			if r.leavesEndBlock > r.startBlock {
				printTreeLinesForRange(db, table, r.leavesEndBlock+1, r.endBlock)
				fmt.Printf("                 leaves %9d thru %9d  (%d blocks)\n",
					r.startBlock, r.leavesEndBlock, r.leavesEndBlock-r.startBlock+1)
			}
		}
	}
}

// printTreeLinesForRange prints contiguous blockid runs in [lo,hi],
// collapsing gaps into separate tree lines (the pStmt2 loop of
// showSegdirMap). The C null-segment special case only matters for empty
// trailing blocks and is reported as a plain tree line here.
func printTreeLinesForRange(db *frigolite.DB, table string, lo, hi int64) {
	if lo > hi {
		return
	}
	res := db.Query(fmt.Sprintf(
		"SELECT blockid FROM '%s_segments' WHERE blockid BETWEEN %d AND %d ORDER BY blockid",
		table, lo, hi))
	if res.Error != nil {
		fatal("%v", res.Error)
	}
	lower, prev := int64(-1), int64(-1)
	for _, r := range res.Rows {
		x := asInt(r[0])
		if lower < 0 {
			lower, prev = x, x
		} else if x == prev+1 {
			prev = x
		} else {
			printTreeLine(lower, prev)
			lower, prev = x, x
		}
	}
	if lower >= 0 {
		printTreeLine(lower, prev)
	}
}

// showStat mirrors showStat (fts3view.c:152): %_stat rows with blob
// payloads decoded as varint lists.
func showStat(db *frigolite.DB, table string) {
	res := db.Query(fmt.Sprintf("SELECT id, value FROM '%s_stat'", table))
	if res.Error != nil {
		fatal("%v", res.Error)
	}
	for _, r := range res.Rows {
		id := asInt(r[0])
		switch v := r[1].(type) {
		case []byte:
			fmt.Printf("stat[%d] =", id)
			i := 0
			for i < len(v) {
				val, n := fts.GetFTS3Varint(v[i:])
				i += n
				fmt.Printf(" %d", val)
			}
			fmt.Println()
		case string:
			blob, decErr := hex.DecodeString(strings.TrimPrefix(v, "x'"))
			if decErr != nil {
				fmt.Printf("stat[%d] = %q\n", id, v)
				continue
			}
			fmt.Printf("stat[%d] =", id)
			i := 0
			for i < len(blob) {
				val, n := fts.GetFTS3Varint(blob[i:])
				i += n
				fmt.Printf(" %d", val)
			}
			fmt.Println()
		default:
			fmt.Printf("stat[%d] = %v\n", id, r[1])
		}
	}
}

func asInt(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// printBlob mirrors printBlob (fts3view.c:596): hex + ascii dump.
func printBlob(aData []byte) {
	const perLine = 16
	for i := 0; i < len(aData); i += perLine {
		end := i + perLine
		if end > len(aData) {
			end = len(aData)
		}
		fmt.Printf(" %04x: ", i)
		for j := i; j < i+perLine; j++ {
			if j >= len(aData) {
				fmt.Print("   ")
				continue
			}
			fmt.Printf("%02x ", aData[j])
		}
		line := aData[i:end]
		for _, b := range line {
			if b >= 0x20 && b < 0x7f {
				fmt.Printf("%c", b)
			} else {
				fmt.Print(".")
			}
		}
		fmt.Println()
	}
}
