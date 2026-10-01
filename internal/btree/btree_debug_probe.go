package btree

// TEMPORARY debug probes for the pooled-wrapper use-after-close crash
// (fleet/perf-parity-poolfix). Removed once the root cause is fixed.
// Contract probed (btree.c ownership): a cursor object may hold at most one
// registry registration per lifetime; between two registrations it MUST pass
// through BTree.Close's clear (finalizer dropped, registry entry removed).
// A violation surfaces to users as "runtime.SetFinalizer: finalizer already
// set" (fatal) or as a cursor whose owner was reset under it (SIGSEGV).

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
)

var (
	debugProbeMu       sync.Mutex
	debugProbeRegStack = map[*Cursor]string{} // cursor -> stack at registration
	debugProbeLive     = map[*BTree]string{}  // wrapper -> stack at initFrom (live set)
)

// debugProbeCheckAcquire asserts a pooled wrapper being re-armed is not
// currently live (one Get per Put; a live wrapper must never be handed out).
func debugProbeCheckAcquire(t *BTree) {
	debugProbeMu.Lock()
	prev, live := debugProbeLive[t]
	if !live {
		debugProbeLive[t] = debugStack(3)
		debugProbeMu.Unlock()
		return
	}
	debugProbeMu.Unlock()
	panic(fmt.Sprintf("btree probe: WRAPPER %p HANDED OUT WHILE LIVE\n--- acquired at:\n%s--- re-acquired at:\n%s",
		t, prev, debugStack(3)))
}

// debugProbeCheckPool asserts a wrapper entering the pool was live exactly
// once (no double Close/Put, no pooling of a never-inited wrapper).
func debugProbeCheckPool(t *BTree) {
	debugProbeMu.Lock()
	_, live := debugProbeLive[t]
	if live {
		delete(debugProbeLive, t)
		debugProbeMu.Unlock()
		return
	}
	debugProbeMu.Unlock()
	panic(fmt.Sprintf("btree probe: POOLING A WRAPPER THAT IS NOT LIVE (double Close/Put) %p\n--- pooled at:\n%s", t, debugStack(3)))
}

// debugStack renders a compact goroutine stack for probe messages.
func debugStack(skip int) string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	buf = buf[:n]
	lines := bytes.Split(buf, []byte("\n"))
	var out bytes.Buffer
	kept := 0
	for _, l := range lines {
		if kept > 14 {
			break
		}
		out.Write(l)
		out.WriteByte('\n')
		kept++
	}
	return out.String()
}

// debugProbeCheckRegistration asserts c carries no live registration when it
// is being (re-)registered, and records the registering stack otherwise.
func debugProbeCheckRegistration(c *Cursor) {
	debugProbeMu.Lock()
	prev, live := debugProbeRegStack[c]
	if !live {
		debugProbeRegStack[c] = debugStack(3)
		debugProbeMu.Unlock()
		return
	}
	debugProbeMu.Unlock()
	panic(fmt.Sprintf("btree probe: DOUBLE REGISTRATION of cursor %p\n--- previous registration stack:\n%s--- now registered at:\n%s",
		c, prev, debugStack(3)))
}

// debugProbeCheckRelease asserts a cursor being recycled into a free list is
// not registered anymore, and drops its recorded stack.
func debugProbeCheckRelease(c *Cursor) {
	debugProbeMu.Lock()
	_, live := debugProbeRegStack[c]
	if !live {
		debugProbeMu.Unlock()
		return
	}
	prev := debugProbeRegStack[c]
	delete(debugProbeRegStack, c)
	debugProbeMu.Unlock()
	if c.released {
		// Cleared by Close's loop: legitimate.
		return
	}
	panic(fmt.Sprintf("btree probe: RECYCLE of REGISTERED cursor %p (released=false)\n--- registration stack:\n%s--- recycle at:\n%s",
		c, prev, debugStack(3)))
}
