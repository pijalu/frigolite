// Package pager — commit dirty-page ORDER selection (the write-order half
// of the commit path; the write loop itself lives in pagercommit.go).
package pager

import "sort"

// flushOrderLocked returns the dirty pages in commit-write order: all pages
// ascending, then page 1 LAST. sqlite3PagerCommitPhaseOne stamps page 1's
// change counter / in-header page count after the page-list write: a page
// allocated during this cycle extends the file mid-loop and
// growHeaderSizeLocked raises the in-header size and re-dirties page 1.
// Flushing page 1 first would write the pre-growth header (on-disk nPage 47
// with page 48 present — integrity_check then reports "invalid page number"
// on page_size=512 FTS4 builds) and the end-of-cycle dirty wipe would drop
// the re-dirty mark. The slice is reused across flushes (flushOrderScratch)
// and small orders sort by insertion (the per-commit dirty set is 2-3 pages;
// sort.Slice's reflect swapper + closure cost more than the sort itself).
// The caller holds p.mu.
func (p *Pager) flushOrderLocked() []uint32 {
	order := p.flushOrderScratch[:0]
	for pageNum := range p.dirty {
		if pageNum != 1 {
			order = append(order, pageNum)
		}
	}
	if len(order) > 16 {
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	} else {
		insertionSortPageNums(order)
	}
	if p.dirty[1] {
		order = append(order, 1)
	}
	p.flushOrderScratch = order
	return order
}

// insertionSortPageNums sorts a small ascending page-number list in place
// (insertion sort: near-sorted and tiny inputs — the per-commit dirty set —
// beat any general sort).
func insertionSortPageNums(a []uint32) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}
