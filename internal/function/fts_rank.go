package function

import (
	"encoding/binary"
	"fmt"
)

// FtsRankFunc implements the rank() function from SQLite's test harness
// (src/test_func.c rankfunc): the first argument must be the blob returned by
// the FTS matchinfo() function in its default 'pcx' format, followed by one
// numeric weight per FTS table column. The score is the sum over each phrase
// and column of (local hit count / global hit count) * column weight. The blob
// layout is validated: [0]=nPhrase, [1]=nCol, then per phrase per column the
// three 'x' values (local hits, global occurrences, global rows), so a blob
// whose size is not 2+3*nCol*nPhrase ints is rejected with "invalid matchinfo
// blob passed to function rank()" (fts3rank.test 1.3/1.5). The engine
// registers this function to mirror the SQLite test build; it is used by the
// fts3rank testgen package.
func FtsRankFunc(args []interface{}) (interface{}, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("wrong number of arguments to function rank()")
	}
	blob, ok := args[0].([]byte)
	if !ok {
		return nil, fmt.Errorf("invalid matchinfo blob passed to function rank()")
	}
	nInt := len(blob) / 4
	var nPhrase, nCol int
	if nInt >= 2 {
		nPhrase = int(binary.LittleEndian.Uint32(blob[0:4]))
		nCol = int(binary.LittleEndian.Uint32(blob[4:8]))
	}
	if nInt != 2+3*nCol*nPhrase {
		return nil, fmt.Errorf("invalid matchinfo blob passed to function rank()")
	}
	if len(args) != 1+nCol {
		return nil, fmt.Errorf("wrong number of arguments to function rank()")
	}
	score := 0.0
	pos := 2
	for iPhrase := 0; iPhrase < nPhrase; iPhrase++ {
		for iCol := 0; iCol < nCol; iCol++ {
			hit := int(binary.LittleEndian.Uint32(blob[pos*4 : pos*4+4]))
			global := int(binary.LittleEndian.Uint32(blob[(pos+1)*4 : (pos+1)*4+4]))
			pos += 3
			if hit > 0 {
				weight, _ := toFloat64(args[iCol+1])
				score += (float64(hit) / float64(global)) * weight
			}
		}
	}
	return score, nil
}
