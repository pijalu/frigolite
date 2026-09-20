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

// matchinfoSlotCount totals the u32 slots zArg needs (fts5MatchinfoFunc's
// size pass).
func matchinfoSlotCount(nCol, nPhrase int, zArg string) (int, error) {
	nInt := 0
	for i := 0; i < len(zArg); i++ {
		n := matchinfoFlagsize(nCol, nPhrase, zArg[i])
		if n < 0 {
			return 0, fmt.Errorf("unrecognized matchinfo flag: %c", zArg[i])
		}
		nInt += n
	}
	return nInt, nil
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
	nInt, err := matchinfoSlotCount(nCol, nPhrase, zArg)
	if err != nil {
		return nil, err
	}
	ret := make([]uint32, nInt)
	if err := aq.matchinfoGlobal(ret, nCol, nPhrase, zArg); err != nil {
		return nil, err
	}
	if err := aq.matchinfoLocal(rowid, ret, nCol, nPhrase, zArg); err != nil {
		return nil, err
	}
	out := make([]byte, 4*len(ret))
	for i, v := range ret {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out, nil
}

// matchinfoGlobal fills the global flag slots (fts5MatchinfoGlobalCb).
func (aq *AuxQuery) matchinfoGlobal(ret []uint32, nCol, nPhrase int, zArg string) error {
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
			if err := aq.matchinfoGlobalA(ret[at : at+nCol]); err != nil {
				return err
			}
		case 'x':
			if err := aq.matchinfoGlobalX(ret[at : at+3*nCol*nPhrase]); err != nil {
				return err
			}
		}
		at += matchinfoFlagsize(nCol, nPhrase, f)
	}
	return nil
}

// matchinfoGlobalA fills the per-column average-size slots (the global 'a'
// case): (2*size+rows)/(2*rows); an empty table leaves the zeros.
func (aq *AuxQuery) matchinfoGlobalA(out []uint32) error {
	nRow := aq.RowCount()
	if nRow == 0 {
		return nil // C stores zeros; the buffer is zero-initialized
	}
	for j := range out {
		nToken, err := aq.ColumnTotalSize(j)
		if err != nil {
			return err
		}
		out[j] = uint32((2*nToken + nRow) / (2 * nRow))
	}
	return nil
}

// matchinfoLocal fills the row-local flag slots (fts5MatchinfoLocalCb).
func (aq *AuxQuery) matchinfoLocal(rowid int64, ret []uint32, nCol, nPhrase int, zArg string) error {
	at := 0
	for i := 0; i < len(zArg); i++ {
		f := zArg[i]
		switch f {
		case 'b':
			aq.matchinfoLocalB(ret[at : at+matchinfoFlagsize(nCol, nPhrase, f)])
		case 'l':
			if err := aq.matchinfoLocalL(rowid, ret[at:at+nCol]); err != nil {
				return err
			}
		case 's':
			if err := aq.matchinfoLocalS(rowid, ret[at:at+nCol]); err != nil {
				return err
			}
		case 'x', 'y':
			nMul := 1
			if f == 'x' {
				nMul = 3
			}
			aq.matchinfoLocalXY(rowid, ret[at:], nMul, nCol, nPhrase)
		}
		at += matchinfoFlagsize(nCol, nPhrase, f)
	}
	return nil
}

// matchinfoLocalL fills the per-column token counts of the current row (the
// local 'l' case).
func (aq *AuxQuery) matchinfoLocalL(rowid int64, out []uint32) error {
	for j := range out {
		nToken, err := aq.ColumnSize(rowid, j)
		if err != nil {
			return err
		}
		out[j] = uint32(nToken)
	}
	return nil
}

// matchinfoLocalXY fills the x/y per-(phrase,column) instance counts: the
// slots zero, then one bump per row instance ('x' strides three slots per
// pair, 'y' one — only the first slot of each pair is used, like C).
func (aq *AuxQuery) matchinfoLocalXY(rowid int64, out []uint32, nMul, nCol, nPhrase int) {
	for j := 0; j < nCol*nPhrase; j++ {
		out[j*nMul] = 0
	}
	for _, inst := range aq.RowInstances(rowid) {
		out[nMul*(inst.Col+inst.Phrase*nCol)]++
	}
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
		nSeq, err := aq.ascendingChain(rowid, nInst, i, ip, ic, io)
		if err != nil {
			return err
		}
		if nSeq > out[ic] {
			out[ic] = nSeq
		}
	}
	return nil
}

// ascendingChain counts the run of consecutive phrase instances starting at
// instance i: same column, offsets ascending without gaps, phrase numbers
// increasing by one (fts5MatchinfoLocalCb's 's' inner loop).
func (aq *AuxQuery) ascendingChain(rowid int64, nInst, i, ip, ic, io int) (uint32, error) {
	iNextPhrase := ip + 1
	iNextOff := io + aq.PhraseSize(0)
	nSeq := uint32(1)
	for j := i + 1; j < nInst; j++ {
		jp, jc, jo, err := aq.instTriple(rowid, j)
		if err != nil {
			return nSeq, err
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
	return nSeq, nil
}

// instTriple returns instance i's (phrase, column, offset).
func (aq *AuxQuery) instTriple(rowid int64, i int) (int, int, int, error) {
	inst, err := aq.Inst(rowid, i)
	if err != nil {
		return 0, 0, 0, err
	}
	return inst.Phrase, inst.Col, inst.Offset, nil
}
