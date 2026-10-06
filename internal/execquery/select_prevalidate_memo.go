package execquery

import (
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file carries the memoized pre-scan validation verdict behind
// execSelectPrevalidate (split out of select_exec.go, which the pre-scan
// memo pushed past the 1000-line gate ceiling).
//
// prevalidateMemoEntry, prevalidateMemoCap, cachedPrevalidateChecks and the
// memo helpers live here.

// prevalidateMemoEntry is one memoized pre-scan validation verdict: the
// schema fingerprint it was produced under and the check error (nil = all
// checks passed).
type prevalidateMemoEntry struct {
	gen uint64
	err error
}

// SetShapeStable marks the statement about to execute as carrying (or not
// carrying) a template-stable AST pointer (the shapeStable field's identity
// contract in context.go). The exec engine's prepare paths call it per
// statement; every shape-verdict memo in this package gates on it.
func (e *SelectEngine) SetShapeStable(v bool) {
	e.shapeStable = v
}

// ShapeStable reports the current statement's AST-pointer stability verdict
// (SetShapeStable).
func (e *SelectEngine) ShapeStable() bool {
	return e.shapeStable
}

// prevalidateMemoCap bounds cachedPrevalidateChecks's memo. The keys retain
// their statement ASTs, so the cap keeps the worst-case retention bounded
// (a stream of full-parsed unique texts recycles the map wholesale).
const prevalidateMemoCap = 64

// cachedPrevalidateChecks serves prevalidateSelectChecks's verdict from the
// engine's memo when the statement qualifies. The checks (INDEXED BY, WHERE
// collations, WITHOUT-ROWID rowid refs, column references, RAISE placement,
// generated-column function safety) read only the statement's STRUCTURE and
// the SCHEMA (colDefs, index lists, WITHOUT ROWID, generated columns) —
// literal values and rows never participate — so the same statement
// structure revisited under the same schema fingerprint has the same
// verdict. That is exactly a template-cache hit's situation: the engine's
// per-depth live clone hands the SAME AST node back for every structurally
// identical statement, so the memo turns the repeated whole-statement
// column walks of a unique-literal SELECT stream into one walk per
// (structure, schema).
//
// The memo is consulted only in the runtime-independent form the checks'
// own branches expect: no outer row (correlation state the walks consult),
// no enclosing alias scopes (walkClause's alias fallback), no compound-member
// context (the ORDER BY walk's gate), no trigger depth (the RAISE check's
// gate and the NEW./OLD. reference handling). Every other form runs the
// walks directly — unmeasured statements (subqueries, views, trigger bodies)
// behave exactly as before.
func (e *SelectEngine) cachedPrevalidateChecks(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	// The memo keys on the statement pointer, which is only meaningful for a
	// template-STABLE AST (shapeStable): the COW scratch clone recycles
	// per-depth structs across TEMPLATES, so a recycled address would serve
	// a stale verdict for a different statement (the R9 point shape-memo
	// identity pin). Unstable statements always run the walks directly.
	if !e.shapeStable || e.prevalidateRuntimeDependent() {
		return e.prevalidateDirectly(s, tableEntry, colDefs)
	}
	gen := e.prevalidateMemoGen()
	// The generated-column function-safety check and the WHERE-collation
	// check consult the function/collation registries, which RegisterFunction
	// / RegisterCollation can extend at runtime WITHOUT moving the schema
	// fingerprint: statements whose table declares either keep running their
	// walks directly (their verdicts are registry-relevant, hence rare and
	// unmeasured). Bare tables — every steady-state shape — never reach those
	// registry lookups (colDefsHaveGenerated and collationMapFor both
	// early-out), so their verdicts are registry-independent.
	if colDefsTouchRegistries(colDefs) {
		return e.prevalidateDirectly(s, tableEntry, colDefs)
	}
	if res, hit := e.prevalidateMemoLookup(s, gen); hit {
		return res
	}
	err := e.prevalidateChecks(s, tableEntry, colDefs)
	e.prevalidateMemoStore(s, gen, err)
	if err != nil {
		return &Result{Error: err}
	}
	return nil
}

// prevalidateRuntimeDependent reports whether the engine is in a form the
// checks' own branches do not treat as runtime-independent: an outer row
// (correlation state the walks consult), enclosing alias scopes (walkClause's
// alias fallback), a compound-member context (the ORDER BY walk's gate), or a
// trigger depth (the RAISE check's gate and the NEW./OLD. reference
// handling). Such statements always run the walks directly.
func (e *SelectEngine) prevalidateRuntimeDependent() bool {
	return e.outerRow != nil || len(e.outerRowStack) != 0 || len(e.outerRows) != 0 ||
		len(e.aliasStack) != 0 || e.inCompoundMember || e.ctx.TriggerDepth() != 0
}

// prevalidateDirectly runs the pre-scan walks now and wraps the verdict in
// the execSelectPrevalidate form (non-nil Result = statement error).
func (e *SelectEngine) prevalidateDirectly(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if err := e.prevalidateChecks(s, tableEntry, colDefs); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// prevalidateMemoGen reads the schema fingerprint the memo keys its verdicts
// under (0 with no schema attached).
func (e *SelectEngine) prevalidateMemoGen() uint64 {
	if sm := e.ctx.Schema(); sm != nil {
		return sm.SchemaFingerprint()
	}
	return 0
}

// colDefsTouchRegistries reports whether any column declaration routes a
// check into the function/collation registries (a generated column's
// function-safety walk, a declared collation's WHERE-collation walk).
func colDefsTouchRegistries(colDefs []sql.ColumnDef) bool {
	for i := range colDefs {
		if colDefs[i].Generated != nil || colDefs[i].Collate != "" {
			return true
		}
	}
	return false
}

// prevalidateMemoLookup serves a memoized verdict: (the verdict, true) on a
// (statement structure, schema fingerprint) hit, (nil, false) on a miss.
func (e *SelectEngine) prevalidateMemoLookup(s *sql.SelectStmt, gen uint64) (*Result, bool) {
	if e.prevalidateMemo == nil {
		return nil, false
	}
	if ent, ok := e.prevalidateMemo[s]; ok && ent.gen == gen {
		if ent.err != nil {
			return &Result{Error: ent.err}, true
		}
		return nil, true
	}
	return nil, false
}

// prevalidateMemoStore records a freshly computed verdict, resetting the map
// when absent, schema-stale, or at capacity.
func (e *SelectEngine) prevalidateMemoStore(s *sql.SelectStmt, gen uint64, err error) {
	if e.prevalidateMemo == nil || e.prevalidateMemoFP != gen || len(e.prevalidateMemo) >= prevalidateMemoCap {
		e.prevalidateMemo = make(map[*sql.SelectStmt]prevalidateMemoEntry)
		e.prevalidateMemoFP = gen
	}
	e.prevalidateMemo[s] = prevalidateMemoEntry{gen: gen, err: err}
}

// prevalidateChecks runs prevalidateSelectChecks's walks, returning the
// bare error (the memo's stored form).
func (e *SelectEngine) prevalidateChecks(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	if err := e.prevalidateIndexCollation(s, tableEntry, colDefs); err != nil {
		return err
	}
	if err := e.prevalidateRowIDAndRefs(s, tableEntry, colDefs); err != nil {
		return err
	}
	return e.prevalidateSchemaFunctionSafety(s, colDefs)
}
