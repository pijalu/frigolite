package exec

import (
	"hash/maphash"

	"github.com/pijalu/frigolite/internal/sql"
)

// Depth-indexed clone scratch.
//
// A template-cache or bind-mode substitution produces a transient AST clone
// whose lifetime is exactly one statement execution: tryTemplateCache hands
// the clone to Exec, Stmt.execute hands it to runSingleStmt. The clone used
// to be allocated fresh per statement (the walker's statement structs and
// out slice were the top engine-side allocation of the point-INSERT and
// point-SELECT floors).
//
// The engine keeps one cloneScratch per execDepth. One substitution at depth
// d builds SEVERAL live clones that must coexist (the InsertStmt plus its
// CTE bodies and its SELECT, a UNION chain's members), so a slot is not a
// single slot per node: every statement struct the walker clones during one
// substitution is RETIRED into the slot's retired list, and the next
// substitution at the same depth rotates that list into its free list.
// Statements at one depth are sequential within the engine's
// single-goroutine execution model (the same model every unsynchronized
// per-engine cache already relies on), so when the rotation happens the
// previous substitution's clones are fully dead; within one substitution a
// struct is handed out at most once (take pops it off the free list). No
// node is ever returned to a shared pool and no finalizer is involved.
//
// Reuse is field-complete: the walker assigns every field of a recycled
// statement struct (the InsertStmt assignments cover all fourteen fields;
// the SELECT walker does a full-struct copy of the template before
// overriding). A substitution the walker REFUSES mid-build leaves partially
// built structs in the retired list; rotation hands them out again and the
// full overwrite makes that safe — a refused substitution falls back to a
// full parse that never observes them.

// cloneScratch is one execDepth slot's recycled clone state.
type cloneScratch struct {
	stmts          []sql.Stmt        // out slice for the statement list
	freeSelects    []*sql.SelectStmt // dead SelectStmt clones, reusable now
	retiredSelects []*sql.SelectStmt // clones built by the substitution in flight
	freeInserts    []*sql.InsertStmt
	retiredInserts []*sql.InsertStmt
}

// rotate turns the previous substitution's retired clones into the free
// list. Called once per substitution, before the walker runs: the previous
// substitution's clones are dead by the per-depth sequentiality contract.
func (s *cloneScratch) rotate() {
	s.freeSelects = append(s.freeSelects[:0], s.retiredSelects...)
	s.retiredSelects = s.retiredSelects[:0]
	s.freeInserts = append(s.freeInserts[:0], s.retiredInserts...)
	s.retiredInserts = s.retiredInserts[:0]
}

// cloneScratchFor returns the scratch slot for the current exec depth,
// growing the slot table as nesting deepens.
func (e *Engine) cloneScratchFor() *cloneScratch {
	d := e.tx.execDepth
	if d >= len(e.cloneScratches) {
		e.cloneScratches = append(e.cloneScratches, make([]*cloneScratch, d+1-len(e.cloneScratches))...)
	}
	s := e.cloneScratches[d]
	if s == nil {
		s = new(cloneScratch)
		e.cloneScratches[d] = s
	}
	return s
}

// cloneStmtsValuesScratch is cloneStmtsValues on the engine's depth-indexed
// scratch (see the file header): the returned statement list is scratch and
// dies with the statement execution it feeds.
func (e *Engine) cloneStmtsValuesScratch(stmts []sql.Stmt, values []interface{}) ([]sql.Stmt, bool) {
	s := e.cloneScratchFor()
	s.rotate()
	if cap(s.stmts) < len(stmts) {
		s.stmts = make([]sql.Stmt, len(stmts))
	}
	out := s.stmts[:len(stmts)]
	c := exprClone{values: values, scratch: s}
	for i, stmt := range stmts {
		cloned, ok := c.stmt(stmt)
		if !ok {
			return nil, false
		}
		out[i] = cloned
	}
	if c.idx != len(values) {
		return nil, false
	}
	return out, true
}

// BindStmtValuesScratch substitutes bound values into a prepared statement
// list on the engine's depth-indexed scratch: the same substitution
// BindStmtValues performs, with the out slice and the cloned statement
// structs recycled per exec depth (see the file header). The returned
// statement list is scratch — it is consumed by the single runSingleStmt
// call that follows and must not be retained. ok=false falls back to the
// caller's text-rendering path (identical results).
func (e *Engine) BindStmtValuesScratch(stmts []sql.Stmt, plan *BindPlan, values []interface{}) ([]sql.Stmt, bool) {
	if plan == nil || len(values) != plan.count {
		return nil, false
	}
	if len(plan.occ) == 0 {
		return stmts, true
	}
	s := e.cloneScratchFor()
	s.rotate()
	if cap(s.stmts) < len(stmts) {
		s.stmts = make([]sql.Stmt, len(stmts))
	}
	out := s.stmts[:len(stmts)]
	c := exprClone{bind: plan, bindValues: values, scratch: s}
	for i, stmt := range stmts {
		cloned, ok := c.stmt(stmt)
		if !ok {
			return nil, false
		}
		out[i] = cloned
	}
	if c.bindOccI != len(plan.occ) {
		return nil, false
	}
	return out, true
}

// takeSelectStmt hands out a dead SelectStmt clone (or nil when the free
// list is empty). Within one substitution each struct is handed out at most
// once.
func (s *cloneScratch) takeSelectStmt() *sql.SelectStmt {
	if n := len(s.freeSelects); n > 0 {
		st := s.freeSelects[n-1]
		s.freeSelects = s.freeSelects[:n-1]
		return st
	}
	return nil
}

// retireSelect records a freshly built SelectStmt clone for the next
// substitution's free list.
func (s *cloneScratch) retireSelect(st *sql.SelectStmt) {
	s.retiredSelects = append(s.retiredSelects, st)
}

// giveBackSelect returns an unmodified borrowed tenant (the walks found
// nothing to substitute and the shared template node is returned instead).
func (s *cloneScratch) giveBackSelect(st *sql.SelectStmt) {
	s.freeSelects = append(s.freeSelects, st)
}

// takeInsertStmt hands out a dead InsertStmt clone (or nil).
func (s *cloneScratch) takeInsertStmt() *sql.InsertStmt {
	if n := len(s.freeInserts); n > 0 {
		st := s.freeInserts[n-1]
		s.freeInserts = s.freeInserts[:n-1]
		return st
	}
	return nil
}

// retireInsert records a freshly built InsertStmt clone for the next
// substitution's free list.
func (s *cloneScratch) retireInsert(st *sql.InsertStmt) {
	s.retiredInserts = append(s.retiredInserts, st)
}

// trySlotPathLive serves a template hit through the entry's precomputed
// slot paths: the values are gated against the slot classes (mirroring the
// COW walkers' gates — a refusal here falls back to the COW form, which
// refuses identically), then written into the entry's live clone for the

// current exec depth. The first hit at a depth builds that clone privately
// through the standalone COW walker (fresh allocations, exactly the
// scratchOK=false form — the per-depth scratch is NOT used: its tenants are
// recycled by the next substitution, while the live clone must persist).
func (e *Engine) trySlotPathLive(cached *sqlTemplateEntry, values []interface{}) ([]sql.Stmt, bool) {
	if cached.slots == nil || !cached.slots.validateValues(values) {
		return nil, false
	}
	depth := e.tx.execDepth
	for i := range cached.live {
		if lc := &cached.live[i]; lc.depth == depth {
			if !cached.slots.apply(lc.stmts[0], values) {
				// Unreachable for a collector-produced table; fall back to
				// the COW clone rather than serve a stale literal.
				return nil, false
			}
			return lc.stmts, true
		}
	}
	c := exprClone{values: values}
	stmts := make([]sql.Stmt, len(cached.ast))
	for i, stmt := range cached.ast {
		out, ok := c.stmt(stmt)
		if !ok {
			return nil, false
		}
		stmts[i] = out
	}
	if c.idx != len(values) {
		return nil, false
	}
	cached.live = append(cached.live, liveTemplateClone{depth: depth, stmts: stmts})
	return stmts, true
}

// templateCacheHash keys the template cache: a seeded AES hash of the
// normalized text. The seed is per-process; lookups verify the candidate
// entry's stored text against the normalized bytes before use, so a (never
// observed) collision degrades to a full parse, never to a wrong template.
var templateCacheHash = maphash.MakeSeed()

// tryTemplateCache attempts to reuse a cached AST template for structurally
// identical SQL (same after replacing literal values). normSQL is the
// recycled normalization scratch (nil when the statement held no literals).
// scratchOK clones onto the engine's per-exec-depth scratch — ONLY valid when
// the caller consumes the statements within the call; the retained form
// (scratchOK=false) clones onto fresh allocations. It returns (nil, false)
// when there is no usable template, falling through to a full parse.
func (e *Engine) tryTemplateCache(sqlStr string, normSQL []byte, values []interface{}, scratchOK bool) ([]sql.Stmt, bool) {
	if len(normSQL) == 0 || len(values) == 0 {
		return nil, false
	}
	cached, ok := e.caches.templateCache[maphash.Bytes(templateCacheHash, normSQL)]
	if !ok || cached.template != string(normSQL) {
		return nil, false
	}
	// Template cache hit — clone AST with new values. If the clone refuses
	// (unknown shape or value mismatch), fall through to re-parse.
	// The clone is NOT stored in the exact-text stmtCache: a structurally
	// identical statement with different literals has a different exact text,
	// so the store only paid a map insert + entry churn per statement (the
	// cache filled to its cap and was wholesale-dropped under unique-text
	// streams) while every exact-text repeat still re-clones from the same
	// template below. Results are identical either way: a substituted AST is
	// byte-for-byte what a fresh parse of the statement text produces, and
	// statement execution treats AST nodes as immutable.
	//
	// Slot-path form (immediate-consume callers, single-statement template):
	// rewrite the entry's live per-depth clone's literal leaves — no walk,
	// no allocation. Retained Prepare (scratchOK=false) never takes it: a
	// retained AST must not alias the live clone a later hit rewrites
	// (FIX.PREPARE-ALIAS).
	if scratchOK {
		if stmts, ok := e.trySlotPathLive(cached, values); ok {
			return stmts, true
		}
		cloned, okClone := e.cloneStmtsValuesScratch(cached.ast, values)
		if okClone {
			return cloned, true
		}
		return nil, false
	}
	cloned, okClone := cloneTemplateRetained(cached.ast, values)
	if !okClone {
		return nil, false
	}
	return cloned, true
}

// cloneTemplateRetained is the retained-Prepare clone form (scratchOK=false):
// a fresh private COW clone on every call, never the per-depth scratch and
// never the live slot-path clone (FIX.PREPARE-ALIAS).
func cloneTemplateRetained(ast []sql.Stmt, values []interface{}) ([]sql.Stmt, bool) {
	c := exprClone{values: values}
	cloned := make([]sql.Stmt, len(ast))
	for i, stmt := range ast {
		out, ok := c.stmt(stmt)
		if !ok {
			return nil, false
		}
		cloned[i] = out
	}
	if c.idx != len(values) {
		return nil, false
	}
	return cloned, true
}

// storeTemplateCache records a parsed statement list as a template for
// structurally identical SQL, bounded by maxTemplateCacheSize. normSQL is
// the normalization scratch; a fresh template materializes its normalized
// text once (the per-statement string the lookup path never pays).
func (e *Engine) storeTemplateCache(normSQL []byte, values []interface{}, stmts []sql.Stmt) {
	if len(normSQL) == 0 || len(values) == 0 || len(e.caches.templateCache) >= maxTemplateCacheSize {
		return
	}
	if e.caches.templateCache == nil {
		e.caches.templateCache = make(map[uint64]*sqlTemplateEntry)
	}
	key := maphash.Bytes(templateCacheHash, normSQL)
	if existing, ok := e.caches.templateCache[key]; ok && existing.template == string(normSQL) {
		return
	}
	e.caches.templateCache[key] = &sqlTemplateEntry{
		template: string(normSQL),
		ast:      stmts,
		slots:    collectTemplateSlots(stmts),
	}
}
