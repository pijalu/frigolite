// cntNew packing for the index balance window. This mirrors btree.c:8488-8610:
// each page starts with its gathered cells plus the parent divider appended
// after it; an overfull page moves its last cell to the next page, then every
// page grows rightward while it fits; finally the right-to-left adjustment
// keeps the rightmost page from being starved. A consumed boundary cell is
// copied into the parent and is NOT stored on either sibling page, so it does
// not consume cell content space. Callers allow at most one extra page; the
// extra page is inserted immediately to the right of the splitting anchor and
// returned to the normal split-chain parent logic.

package btree

// packIndexBalance computes cntNew boundaries. cntOld[i] is the entry index
// just after page i's appended divider; used[i] is that page's byte footprint.
// A false result means even maxPages pages cannot hold the cell array.
func packIndexBalance(sizes, cntOld, used []int, usable, maxPages int, bulk bool) ([]int, bool) {
	nOld, n := len(cntOld), len(sizes)
	if nOld == 0 || maxPages < nOld || maxPages > nOld+2 {
		return nil, false
	}
	k := nOld
	cntNew := append([]int(nil), cntOld...)
	szNew := append([]int(nil), used...)
	for i := 0; i < k; i++ {
		for szNew[i] > usable {
			if i+1 == k && k >= maxPages {
				return nil, false
			}
			if i+1 == k {
				k++
				szNew = append(szNew, 0)
				cntNew = append(cntNew, n)
			}
			if cntNew[i] <= 0 {
				return nil, false
			}
			szNew[i] -= 2 + sizes[cntNew[i]-1]
			transfer := 0
			if cntNew[i] < n {
				transfer = 2 + sizes[cntNew[i]]
			}
			szNew[i+1] += transfer
			cntNew[i]--
		}
		for cntNew[i] < n {
			sz := 2 + sizes[cntNew[i]]
			if szNew[i]+sz > usable {
				break
			}
			szNew[i] += sz
			cntNew[i]++
			transfer := 0
			if cntNew[i] < n {
				transfer = 2 + sizes[cntNew[i]]
			}
			szNew[i+1] -= transfer
		}
		if i+1 == k && cntNew[i] < n {
			if k >= maxPages {
				return nil, false
			}
			k++
			szNew = append(szNew, 0)
			cntNew = append(cntNew, n)
		}
		prev := 0
		if i > 0 {
			prev = cntNew[i-1]
		}
		if cntNew[i] >= n {
			if i < k-1 {
				return nil, false
			}
			break
		}
		if cntNew[i] <= prev {
			return nil, false
		}
	}
	if !adjustIndexBalancePack(sizes, cntNew, szNew, k, n, bulk) {
		return nil, false
	}
	return cntNew, true
}

// adjustIndexBalancePack runs C's mandatory right-to-left balancing pass. The
// szRight==0 exception lets an empty right page be filled even when the size
// inequality would otherwise stop the move.
func adjustIndexBalancePack(sizes, cntNew, szNew []int, k, n int, bulk bool) bool {
	for i := k - 1; i > 0; i-- {
		szRight, szLeft := szNew[i], szNew[i-1]
		r, d := cntNew[i-1]-1, cntNew[i-1]
		if r < 0 || d >= n {
			return false
		}
		for r >= 0 {
			szR, szD := sizes[r], sizes[d]
			reserve := 0
			if i != k-1 {
				reserve = 2
			}
			if szRight != 0 && (bulk || szRight+szD+2 > szLeft-szR-reserve) {
				break
			}
			szRight += szD + 2
			szLeft -= szR + 2
			cntNew[i-1] = r
			r--
			d--
		}
		szNew[i], szNew[i-1] = szRight, szLeft
		prev := 0
		if i > 1 {
			prev = cntNew[i-2]
		}
		if cntNew[i-1] <= prev {
			return false
		}
	}
	return cntNew[0] > 0
}

// indexBalanceRun returns the half-open entry range stored on page p and the
// entry index copied up as that page's right divider (-1 for the last page).
func indexBalanceRun(cntNew []int, p, n int) (start, end, boundary int, ok bool) {
	if p == 0 {
		start = 0
	} else {
		start = cntNew[p-1] + 1
	}
	if p == len(cntNew)-1 {
		end, boundary = n, -1
	} else {
		end, boundary = cntNew[p], cntNew[p]
	}
	if start > end || boundary >= n || (boundary >= 0 && boundary < start) {
		return 0, 0, 0, false
	}
	return start, end, boundary, true
}

// indexBalanceUsed reports the byte footprint of a group of entries: encoded
// cell bytes plus the two-byte cell-pointer slot for each.
func indexBalanceUsed(sizes []int, start, end int) int {
	n, bytes := 0, 0
	for i := start; i < end; i++ {
		bytes += sizes[i]
		n++
	}
	return bytes + 2*n
}
