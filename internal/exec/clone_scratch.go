package exec

import (
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
