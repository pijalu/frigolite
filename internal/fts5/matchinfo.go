package fts5

import (
	"encoding/binary"
	"fmt"
)

// This file ports the fts5 matchinfo() test-support function
// (ext/fts5/fts5_test_mi.c, SQLITE_TEST-only in C): matchinfo(t, zArg)
// returns a blob of little-endian u32 values, one or more per flag character
// of zArg. Flag sizes (fts5MatchinfoFlagsize): p/c/n emit 1 value, a/l/s one
// per column, x three per (phrase, column) pair, y one per pair and b one per
// 32 columns per phrase.

// matchinfoFlagsize returns the number of u32 slots flag f contributes for a
// query with nPhrase phrases over nCol columns, or -1 for an unknown flag
// (fts5_test_mi.c fts5MatchinfoFlagsize).
func matchinfoFlagsize(nCol, nPhrase int, f byte) int {
	switch f {
	case 'p', 'c', 'n':
		return 1
	case 'x':
		return 3 * nCol * nPhrase
	case 'y':
		return nCol * nPhrase
	case 'b':
		return ((nCol + 31) / 32) * nPhrase
	case 'a', 'l', 's':
		return nCol
	}
	return -1
}

// Matchinfo renders the matchinfo blob for one row (fts5MatchinfoFunc). The
// global flags (p c n a x) are computed once per call like C's auxdata-
// cached fts5MatchinfoGlobalCb pass; the row-local flags (b l s x y) then
// fill/overwrite their slots (fts5MatchinfoLocalCb).
func (aq *AuxQuery) Matchinfo(rowid int64, zArg string) ([]byte, error) {
	nCol := len(aq.t.cfg.Columns)
	nPhrase := aq.PhraseCount()
	if zArg == "" {
		// C: nVal==0 renders the default format "pcx".
		zArg = "pcx"
	}
	nInt := 0
	for i := 0; i < len(zArg); i++ {
		n := matchinfoFlagsize(nCol, nPhrase, zArg[i])
		if n < 0 {
			return nil, fmt.Errorf("unrecognized matchinfo flag: %c", zArg[i])
		}
		nInt += n
	}
	ret := make([]uint32, nInt)

	// Pass 1: global flags (fts5MatchinfoGlobalCb).
	at := 0
	for i := 0; i < len(zArg); i++ {
		f := zArg[i]
		switch f {
		case 'p':
			ret[at] = uint32(nPhrase)
		case 'c':
			ret[at] = uint32(nCol)
		case 'n':
			ret[at] = uint32(aq.RowCount())
		case 'a':
			nRow := aq.RowCount()
			if nRow == 0 {
				for j := 0; j < nCol; j++ {
					ret[at+j] = 0
				}
			} else {
				for j := 0; j < nCol; j++ {
					nToken, err := aq.ColumnTotalSize(j)
					if err != nil {
						return nil, err
					}
					ret[at+j] = uint32((2*nToken + nRow) / (2 * nRow))
				}
			}
		case 'x':
			if err := aq.matchinfoGlobalX(ret[at : at+3*nCol*nPhrase]); err != nil {
				return nil, err
			}
		}
		at += matchinfoFlagsize(nCol, nPhrase, f)
	}

	// Pass 2: row-local flags (fts5MatchinfoLocalCb).
	at = 0
	for i := 0; i < len(zArg); i++ {
		f := zArg[i]
		switch f {
		case 'b':
			aq.matchinfoLocalB(ret[at : at+matchinfoFlagsize(nCol, nPhrase, f)])
		case 'l':
			for j := 0; j < nCol; j++ {
				nToken, err := aq.ColumnSize(rowid, j)
				if err != nil {
					return nil, err
				}
				ret[at+j] = uint32(nToken)
			}
		case 's':
			if err := aq.matchinfoLocalS(rowid, ret[at : at+nCol]); err != nil {
				return nil, err
			}
		case 'x', 'y':
			nMul := 1
			if f == 'x' {
				nMul = 3
			}
			span := nCol * nPhrase
			for j := 0; j < span; j++ {
				ret[at+j*nMul] = 0
			}
			for _, inst := range aq.RowInstances(rowid) {
				ret[at+nMul*(inst.Col+inst.Phrase*nCol)]++
			}
		}
		at += matchinfoFlagsize(nCol, nPhrase, f)
	}

	out := make([]byte, 4*len(ret))
	for i, v := range ret {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out, nil
}

// matchinfoGlobalX fills the global x slots (fts5MatchinfoXCb via
// xQueryPhrase): for each phrase, every instance in (rowid, column, offset)
// order bumps the column's instance slot (+1) and, when the column differs
// from the previous instance's, its distinct-group slot (+2).
func (aq *AuxQuery) matchinfoGlobalX(out []uint32) error {
	nCol := len(aq.t.cfg.Columns)
	for iPhrase := 0; iPhrase < aq.PhraseCount(); iPhrase++ {
		base := iPhrase * nCol * 3
		iPrev := -1
		for _, rowid := range aq.rowidsInRowidOrder() {
			for _, inst := range aq.RowInstances(rowid) {
				if inst.Phrase != iPhrase {
					continue
				}
				out[base+inst.Col*3+1]++
				if inst.Col != iPrev {
					out[base+inst.Col*3+2]++
				}
				iPrev = inst.Col
			}
		}
	}
	return nil
}

// rowidsInRowidOrder lists the indexed documents in ascending rowid order.
func (aq *AuxQuery) rowidsInRowidOrder() []int64 {
	return aq.t.ix.SortedRowids()
}

// matchinfoLocalB fills the per-phrase column bitmaps (fts5MatchinfoLocalCb's
// 'b' case): bit (iCol%32) of word iCol/32 of phrase iPhrase's bitmap is set
// when the phrase matches in that column.
func (aq *AuxQuery) matchinfoLocalB(out []uint32) {
	nCol := len(aq.t.cfg.Columns)
	for i := range out {
		out[i] = 0
	}
	for iPhrase := 0; iPhrase < aq.PhraseCount(); iPhrase++ {
		cols, err := aq.PhraseCollist(0, iPhrase)
		if err != nil {
			continue
		}
		for _, iCol := range cols {
			out[iPhrase*((nCol+31)/32)+iCol/32] |= 1 << (uint(iCol) % 32)
		}
	}
}

// matchinfoLocalS fills the per-column longest-ascending-phrase-chain counts
// (fts5MatchinfoLocalCb's 's' case).
func (aq *AuxQuery) matchinfoLocalS(rowid int64, out []uint32) error {
	for i := range out {
		out[i] = 0
	}
	nInst := aq.InstCount(rowid)
	for i := 0; i < nInst; i++ {
		ip, ic, io, err := aq.instTriple(rowid, i)
		if err != nil {
			return err
		}
		iNextPhrase := ip + 1
		iNextOff := io + aq.PhraseSize(0)
		nSeq := uint32(1)
		for j := i + 1; j < nInst; j++ {
			jp, jc, jo, err := aq.instTriple(rowid, j)
			if err != nil {
				return err
			}
			if jc != ic || jo > iNextOff {
				break
			}
			if jp == iNextPhrase && jo == iNextOff {
				nSeq++
				iNextPhrase++
				iNextOff = jo + aq.PhraseSize(jp)
			}
		}
		if nSeq > out[ic] {
			out[ic] = nSeq
		}
	}
	return nil
}

// instTriple returns instance i's (phrase, column, offset).
func (aq *AuxQuery) instTriple(rowid int64, i int) (int, int, int, error) {
	inst, err := aq.Inst(rowid, i)
	if err != nil {
		return 0, 0, 0, err
	}
	return inst.Phrase, inst.Col, inst.Offset, nil
}
