package btree

// TEMPORARY probes for the residual write-path crash (fleet/perf-parity-poolfix).
// Removed once fixed. Three contracts:
//  1. NewBTree/NewSchemaBTree are never called with a nil pager.
//  2. A pooled wrapper is never handed out while live (one Get per Put).
//  3. A mutation on a wrapper whose pager is nil panics loudly with the
//     wrapper's pool history instead of SIGSEGV deep in the pager.

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"

	"github.com/pijalu/frigolite/internal/pager"
)

var (
	probeMu   sync.Mutex
	probeLive = map[*BTree]string{}
)

func probeStack(skip int) string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	var out bytes.Buffer
	kept := 0
	for _, l := range bytes.Split(buf[:n], []byte("\n")) {
		if kept > 12 {
			break
		}
		out.Write(l)
		out.WriteByte('\n')
		kept++
	}
	return out.String()
}

// probeInit records arming; panics on a nil pager or a live re-handout.
func probeInit(t *BTree, pg *pager.Pager) {
	if pg == nil {
		panic("btree probe: wrapper armed with NIL pager at:\n" + probeStack(2))
	}
	t.closedBy = ""
	probeMu.Lock()
	if _, live := probeLive[t]; live {
		prev := probeLive[t]
		probeMu.Unlock()
		panic(fmt.Sprintf("btree probe: WRAPPER %p HANDED OUT WHILE LIVE\n--- at:\n%s--- now:\n%s", t, prev, probeStack(2)))
	}
	probeLive[t] = probeStack(2)
	probeMu.Unlock()
}

// probePool records pooling; panics on a wrapper that is not live.
func probePool(t *BTree) {
	probeMu.Lock()
	if _, live := probeLive[t]; live {
		delete(probeLive, t)
		probeMu.Unlock()
		return
	}
	probeMu.Unlock()
	panic(fmt.Sprintf("btree probe: POOLING NON-LIVE WRAPPER %p\n--- closed by:\n%s--- now:\n%s", t, t.closedBy, probeStack(2)))
}

// probeClose marks WHO closed the wrapper (taint-free: stored on the object,
// never in a pointer-keyed map that heap address reuse could stale).
func probeClose(t *BTree) {
	t.closedBy = probeStack(2)
}

// ProbeWrapperState renders a wrapper's probe history.
func ProbeWrapperState(t *BTree) string {
	probeMu.Lock()
	defer probeMu.Unlock()
	s := fmt.Sprintf("wrapper %p closed=%v", t, t.closed)
	if t.closedBy != "" {
		s += "\n--- closed by:\n" + t.closedBy
	}
	if prev, ok := probeLive[t]; ok {
		s += "\n--- live at:\n" + prev
	}
	return s
}
