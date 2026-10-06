//go:build frigolite_journal_audit

// Statement-journal write-intent AUDIT (build tag frigolite_journal_audit).
//
// The write-intent contract — every in-place page edit is preceded by
// PrepareWrite (or a markDirty that itself precedes the edit) — is what
// keeps statement rollback byte-exact. This build proves the contract holds
// across the whole test corpus:
//
//   - auditReadTouch records a 64-bit FNV-1a hash of every page the read
//     path hands out while a statement scope is open (keyed by pager).
//   - auditDirtyCheckLocked runs at the top of markDirtyLocked: if the page
//     has NO statement-journal entry yet but its bytes differ from the
//     recorded read hash, some code path mutated the page without
//     announcing write intent — the rollback would restore corrupted bytes.
//     That is a bug: panic loudly.
//
// Run: go test -tags frigolite_journal_audit ./...
package pager

import (
	"sync"
)

// auditHashes maps each audited pager to its per-transaction read hashes.
// Package-level (not a Pager field) so the production build carries no
// extra state; tests construct many pagers, so entries are keyed and
// dropped with the pager's scope lifecycle.
var auditState struct {
	mu     sync.Mutex
	hashes map[*Pager]map[uint32]uint64
}

func init() {
	auditState.hashes = make(map[*Pager]map[uint32]uint64)
}

// pageHashFnv is FNV-1a 64 over the page bytes.
func pageHashFnv(data []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, b := range data {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

// auditRecordReadLocked records pg's byte hash for the audit (caller holds
// p.mu; the scope-open check happened under the same lock).
func (p *Pager) auditRecordReadLocked(pg *Page) {
	if p.stmtTop == nil {
		return
	}
	auditState.mu.Lock()
	m := auditState.hashes[p]
	if m == nil {
		m = make(map[uint32]uint64)
		auditState.hashes[p] = m
	}
	m[pg.PageNum] = pageHashFnv(pg.Data)
	auditState.mu.Unlock()
}

// auditReadTouch records the handed-out page's hash. Production no-op.
func (p *Pager) auditReadTouch(pg *Page) {
	if pg == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.auditRecordReadLocked(pg)
}

// auditDirtyCheckLocked panics when pgno's bytes were mutated since their
// last read without a PrepareWrite (caller holds p.mu; called from
// markDirtyLocked BEFORE the dirty flag / journal entry is written).
func (p *Pager) auditDirtyCheckLocked(pgno uint32) {
	if p.stmtTop == nil {
		return
	}
	if _, ok := p.stmtTop.stmtEntryFor(pgno); ok {
		return // write intent (or an earlier write) already journalled
	}
	auditState.mu.Lock()
	var recorded uint64
	var known bool
	if m := auditState.hashes[p]; m != nil {
		recorded, known = m[pgno]
	}
	auditState.mu.Unlock()
	if !known {
		return // never read under a scope: fresh allocation or internal page
	}
	pg, ok := p.pages[pgno]
	if !ok || pg == nil {
		return
	}
	if cur := pageHashFnv(pg.Data); cur != recorded {
		panic("journal audit: page mutated without PrepareWrite (pgno " +
			itoa(int(pgno)) + ")")
	}
}

// auditClearLocked drops the pager's read hashes when its outermost
// statement scope closes (the transaction's write phase ended).
func (p *Pager) auditClearLocked() {
	if p.stmtTop != nil {
		return
	}
	auditState.mu.Lock()
	delete(auditState.hashes, p)
	auditState.mu.Unlock()
}

// itoa avoids a strconv import in a panic path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
