//go:build !frigolite_journal_audit

// Production counterparts of the statement-journal write-intent audit
// (journal_audit.go): empty bodies the compiler inlines away, so the read
// path and markDirty carry zero audit cost outside the
// frigolite_journal_audit build.

package pager

func (p *Pager) auditReadTouch(pg *Page) {}

// auditDirtyCheckLocked panics on mutation without write intent in the audit
// build; production no-op (called from markDirtyLocked under p.mu).
func (p *Pager) auditDirtyCheckLocked(pgno uint32) {}

// auditRecordReadLocked records a handed-out page's byte hash in the audit
// build (called from readPageLocked under p.mu); production no-op.
func (p *Pager) auditRecordReadLocked(pg *Page) {}

// auditClearLocked drops the audit read-hash map (no-op in production).
func (p *Pager) auditClearLocked() {}
