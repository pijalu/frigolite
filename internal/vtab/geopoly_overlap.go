package vtab

// Faithful port of geopoly.c's geopolyOverlap: a Bentley-Ottmann-style sweep
// over the segments of two polygons. Events (segment endpoints) are sorted by
// X with a bottom-up merge (geopolySortEventsByX); between distinct event Xs
// the active segment list is re-sorted by current Y (then slope C,
// geopolySortSegmentsByYAndC) and scanned pairwise: an adjacent pair whose
// sides differ and whose Y order inverts between columns proves the polygon
// boundaries CROSS (result 1). Otherwise the parity masks aOverlap[] record
// which side-interior regions exist between adjacent active segments, and
// the final classification derives from them:
//
//	aOverlap[3]==0              → 0  (disjoint)
//	aOverlap[1] && !aOverlap[2] → 3  (p2 completely inside p1)
//	aOverlap[2] && !aOverlap[1] → 2  (p1 completely inside p2)
//	!aOverlap[1] && !aOverlap[2]→ 4  (identical polygons)
//	otherwise                   → 1  (partial overlap)
//
// The linked-list merges preserve C's exact tie order (a later-arriving list
// goes first on equal keys), which keeps results deterministic for
// degenerate/shared-boundary inputs.

// geoSegment is one non-vertical polygon edge in y = C*x + B form (GeoSegment).
type geoSegment struct {
	c, b float64 // line parameters
	y    float64 // current y at the sweep position
	y0   float32 // initial y (left endpoint)
	side byte    // 1 for p1, 2 for p2
	idx  uint32  // segment index within its polygon
}

// geoEvent is one segment-endpoint event (GeoEvent): eType 0 = add the
// segment at x, 1 = remove it.
type geoEvent struct {
	x     float64
	eType int
	seg   int // index into geoOverlap.seg
}

// geoOverlap holds the event/segment arrays plus the per-element next links
// used by the merge sorts (C's pNext pointers).
type geoOverlap struct {
	ev   []geoEvent
	seg  []geoSegment
	nExt []int // next links over ev
	nSeg []int // next links over seg
}

// geopolyAddOneSegment appends one edge and its two events, skipping
// vertical edges and normalizing the direction so x0 < x1.
func (ov *geoOverlap) addOneSegment(p *geoPoly, x0i, y0i, x1i, y1i int, side byte, idx uint32) {
	x0, y0 := float64(p.a[x0i]), float64(p.a[y0i])
	x1, y1 := float64(p.a[x1i]), float64(p.a[y1i])
	if x0 == x1 {
		return // ignore vertical segments
	}
	if x0 > x1 {
		x0, x1 = x1, x0
		y0, y1 = y1, y0
	}
	si := len(ov.seg)
	ov.seg = append(ov.seg, geoSegment{
		c:    (y1 - y0) / (x1 - x0),
		b:    y1 - x1*((y1-y0)/(x1-x0)),
		y0:   float32(y0),
		side: side,
		idx:  idx,
	})
	ov.nSeg = append(ov.nSeg, -1)
	ov.ev = append(ov.ev, geoEvent{x: x0, eType: 0, seg: si})
	ov.nExt = append(ov.nExt, -1)
	ov.ev = append(ov.ev, geoEvent{x: x1, eType: 1, seg: si})
	ov.nExt = append(ov.nExt, -1)
}

// geopolyAddSegments appends every edge of pPoly (the closing edge included)
// tagged with side.
func (ov *geoOverlap) addSegments(p *geoPoly, side byte) {
	for i := 0; i < p.nVertex-1; i++ {
		ov.addOneSegment(p, 2*i, 2*i+1, 2*i+2, 2*i+3, side, uint32(i))
	}
	i := p.nVertex - 1
	ov.addOneSegment(p, 2*i, 2*i+1, 0, 1, side, uint32(i))
}

// newGeoOverlap allocates the sweep state and fills it with both polygons'
// segments and events (geopolyOverlap's setup).
func newGeoOverlap(p1, p2 *geoPoly) *geoOverlap {
	nVertex := p1.nVertex + p2.nVertex + 2
	ov := &geoOverlap{
		ev:   make([]geoEvent, 0, nVertex*2),
		seg:  make([]geoSegment, 0, nVertex),
		nExt: nil,
		nSeg: nil,
	}
	ov.addSegments(p1, 1)
	ov.addSegments(p2, 2)
	return ov
}

// geoEventMerge merges two X-sorted event lists; on equal X the right list's
// element goes first (pRight->x <= pLeft->x).
func (ov *geoOverlap) eventMerge(l, r int) int {
	head, last := -1, -1
	for l != -1 && r != -1 {
		nxt := l
		if ov.ev[r].x <= ov.ev[l].x {
			nxt = r
		}
		if last == -1 {
			head = nxt
		} else {
			ov.nExt[last] = nxt
		}
		last = nxt
		if nxt == r {
			r = ov.nExt[r]
		} else {
			l = ov.nExt[l]
		}
	}
	tail := l
	if tail == -1 {
		tail = r
	}
	if last == -1 {
		head = tail
	} else {
		ov.nExt[last] = tail
	}
	return head
}

// sortEventsByX ports geopolySortEventsByX: a bottom-up merge over the event
// array in insertion order.
func (ov *geoOverlap) sortEventsByX() int {
	var a [50]int
	mx := 0
	for i := range ov.ev {
		ov.nExt[i] = -1
		p := i
		j := 0
		for ; j < mx && a[j] != -1; j++ {
			p = ov.eventMerge(a[j], p)
			a[j] = -1
		}
		a[j] = p
		if j >= mx {
			mx = j + 1
		}
	}
	p := -1
	for i := 0; i < mx; i++ {
		p = ov.eventMerge(a[i], p)
	}
	return p
}

// segmentMerge merges two Y-then-C sorted segment lists; on equal Y the
// slope decides, and on equal slope the right list goes first.
func (ov *geoOverlap) segmentMerge(l, r int) int {
	head, last := -1, -1
	for l != -1 && r != -1 {
		diff := ov.seg[r].y - ov.seg[l].y
		if diff == 0.0 {
			diff = ov.seg[r].c - ov.seg[l].c
		}
		nxt := l
		if diff < 0.0 {
			nxt = r
		}
		if last == -1 {
			head = nxt
		} else {
			ov.nSeg[last] = nxt
		}
		last = nxt
		if nxt == r {
			r = ov.nSeg[r]
		} else {
			l = ov.nSeg[l]
		}
	}
	tail := l
	if tail == -1 {
		tail = r
	}
	if last == -1 {
		head = tail
	} else {
		ov.nSeg[last] = tail
	}
	return head
}

// sortSegmentsByYAndC ports geopolySortSegmentsByYAndC over the active list.
func (ov *geoOverlap) sortSegmentsByYAndC(list int) int {
	var a [50]int
	for i := range a {
		a[i] = -1
	}
	mx := 0
	for s := list; s != -1; {
		p := s
		s = ov.nSeg[s]
		ov.nSeg[p] = -1
		i := 0
		for ; i < mx && a[i] != -1; i++ {
			p = ov.segmentMerge(a[i], p)
			a[i] = -1
		}
		a[i] = p
		if i >= mx {
			mx = i + 1
		}
	}
	p := -1
	for i := 0; i < mx; i++ {
		p = ov.segmentMerge(a[i], p)
	}
	return p
}

// geopolyOverlap determines the geometric relationship between two polygons
// (returns -1 on the C code's allocation failure path, which cannot occur
// here since the state is Go-allocated).
func geopolyOverlap(p1, p2 *geoPoly) int {
	ov := newGeoOverlap(p1, p2)
	thisEvent := ov.sortEventsByX()
	rX := 0.0
	if thisEvent != -1 && ov.ev[thisEvent].x == 0.0 {
		rX = -1.0
	}
	var aOverlap [4]byte
	active := -1
	needSort := false
	for ; thisEvent != -1; thisEvent = ov.nExt[thisEvent] {
		if ov.ev[thisEvent].x != rX {
			rX = ov.ev[thisEvent].x
			if needSort {
				active = ov.sortSegmentsByYAndC(active)
				needSort = false
			}
			crossing := ov.sweepColumn(&aOverlap, active, rX)
			if crossing {
				return 1 // boundaries cross: partial overlap
			}
		}
		active = ov.applyEvent(thisEvent, active, &needSort)
	}
	return overlapClassOf(aOverlap)
}

// sweepColumn ports geopolyOverlap's two per-column scans over the sorted
// active list: the first records interior masks from the previous column's Y
// values, the second refreshes each segment's Y at the new X, records masks
// again and reports a boundary crossing between segments of different sides.
func (ov *geoOverlap) sweepColumn(aOverlap *[4]byte, active int, rX float64) bool {
	iMask := ov.maskColumn(aOverlap, active)
	for s := active; s != -1; s = ov.nSeg[s] {
		y := ov.seg[s].c*rX + ov.seg[s].b
		ov.seg[s].y = y
	}
	return ov.crossingColumn(aOverlap, active, iMask)
}

// maskColumn is the first per-column loop: adjacent segments with differing
// Y record aOverlap[iMask] (iMask = running XOR of the sides seen so far).
func (ov *geoOverlap) maskColumn(aOverlap *[4]byte, active int) int {
	prev := -1
	iMask := 0
	for s := active; s != -1; s = ov.nSeg[s] {
		if prev != -1 && ov.seg[prev].y != ov.seg[s].y {
			aOverlap[iMask] = 1
		}
		iMask ^= int(ov.seg[s].side)
		prev = s
	}
	return iMask
}

// crossingColumn is the second per-column loop: adjacent segments of
// different sides whose Y order inverted prove a boundary crossing;
// differing adjacent Y values record masks.
func (ov *geoOverlap) crossingColumn(aOverlap *[4]byte, active, iMask int) bool {
	prev := -1
	for s := active; s != -1; s = ov.nSeg[s] {
		if prev != -1 {
			if ov.seg[prev].y > ov.seg[s].y && ov.seg[prev].side != ov.seg[s].side {
				return true
			} else if ov.seg[prev].y != ov.seg[s].y {
				aOverlap[iMask] = 1
			}
		}
		iMask ^= int(ov.seg[s].side)
		prev = s
	}
	return false
}

// applyEvent ports geopolyOverlap's add/remove dispatch: ADD places the
// segment at the head of the active list (resetting its Y to the left
// endpoint), REMOVE unlinks it.
func (ov *geoOverlap) applyEvent(e int, active int, needSort *bool) int {
	si := ov.ev[e].seg
	if ov.ev[e].eType == 0 {
		ov.seg[si].y = float64(ov.seg[si].y0)
		ov.nSeg[si] = active
		*needSort = true
		return si
	}
	if active == si {
		return ov.nSeg[active]
	}
	for s := active; s != -1; s = ov.nSeg[s] {
		if ov.nSeg[s] == si {
			ov.nSeg[s] = ov.nSeg[si]
			break
		}
	}
	return active
}

// overlapClassOf ports the aOverlap[] final classification.
func overlapClassOf(aOverlap [4]byte) int {
	switch {
	case aOverlap[3] == 0:
		return 0
	case aOverlap[1] != 0 && aOverlap[2] == 0:
		return 3
	case aOverlap[1] == 0 && aOverlap[2] != 0:
		return 2
	case aOverlap[1] == 0 && aOverlap[2] == 0:
		return 4
	default:
		return 1
	}
}
