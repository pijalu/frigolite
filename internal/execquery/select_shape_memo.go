package execquery

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns the per-template SELECT shape memo: the statement-level
// verdicts whose value is a pure function of (statement structure, schema)
// and whose dominant consumers are the point-SELECT paths. One memo entry
// replaces up to six whole-statement walks per execution (the pre-dispatch
// expression validation, the row-map requirement, the aggregate-presence
// walk, the bare-reference projection test, and the rowid-seek equality
// resolution).
//
// Identity contract (the R9.POINT shape-memo pin, shared with the prevalidate
// memo): entries are keyed by the *sql.SelectStmt pointer, which is only
// meaningful for a template-STABLE AST — the slot-path live clone (one
// persistent clone per (template, exec depth), literal leaves rewritten in
// place), the exact-text statement cache, or a retained/fresh parse. The COW
// scratch clone recycles per-exec-depth structs across TEMPLATES, so a
// recycled address would alias a stale verdict for a different statement;
// every consult gates on shapeStable (context.go), which the exec engine's
// prepare paths set per statement (Engine.setStmtShapeStable).
//
// Registry contract: a statement's expression census (select_census.go)
// proves the absence of FuncCall / COLLATE / MATCH / subquery nodes; the
// remaining validators' verdicts consult nothing but node kinds, column
// definitions, and the schema fingerprint — no function/collation registry
// lookup can change them (RegisterFunction / RegisterCollation move no
// fingerprint). Statements carrying those node kinds stay unmemoized and run
// their walks directly, exactly as before.

// shapeMemoCap bounds the memo between DDLs. Keys retain their statement
// ASTs, so the cap keeps retention bounded; reaching the cap drops the whole
// epoch (the same flush-on-change policy the fingerprint applies).
const shapeMemoCap = 64

// selectShapeEntry is one template's memoized shape verdicts. A field's zero
// value means "not decided" — each verdict records its own done bit so a
// partially-built entry (the seek decision stores only the single-equality
// shape) never implies an uncomputed verdict.
type selectShapeEntry struct {
	gen uint64 // schema fingerprint the verdicts were computed under

	bareRefs bool // projectionIsBareRefs (pure AST shape)

	preDispatchDone bool  // validateSelectPreDispatch verdict recorded
	preDispatchErr  error // nil = all pre-dispatch checks passed

	needsRowMapsDone bool // SelectNeedsRowMaps verdict recorded
	needsRowMaps     bool // SelectNeedsRowMaps
	hasAggDone       bool // hasAggregates verdict recorded
	hasAgg           bool // hasAggregates(s.Columns)

	// seekDone records the rowid-seek shape for the SINGLE-EQUALITY form:
	// the WHERE is exactly one conjunct, a rowid-pinned equality whose
	// literal side is seekLitExpr. The analysis then reconstructs per
	// statement as {eq, covers, planned} with the literal VALUE re-resolved
	// (eqMatch/planned are literal-value-dependent: NULL and out-of-range
	// reals match no rowid). Any other WHERE shape stays seekDone=false and
	// walks analyzeRowidSeekInto every statement, exactly as before.
	seekDone    bool
	seekEq      bool
	seekLitExpr sql.Expr
}

// shapeEntryFor returns the statement's shape-memo entry, building it on
// first sight, or nil when the statement cannot be memoized (unstable AST
// pointer, registry-relevant node kinds, or the cap just recycled). The
// build's census walk is the same one validateSelectExprs runs — on the
// memo-eligible path it is paid once per (template, schema) instead of once
// per statement.
func (e *SelectEngine) shapeEntryFor(s *sql.SelectStmt) *selectShapeEntry {
	if !e.shapeStable {
		return nil
	}
	fp := e.schemaFingerprint()
	if ent, ok := e.shapeMemo[s]; ok && ent.gen == fp {
		return ent
	}
	c := censusSelectStmt(e, s)
	if c.funcCall || c.collateOp || c.matchOp || c.subquery {
		return nil
	}
	if e.shapeMemoFP != fp || e.shapeMemo == nil || len(e.shapeMemo) >= shapeMemoCap {
		e.shapeMemo = make(map[*sql.SelectStmt]*selectShapeEntry)
		e.shapeMemoFP = fp
	}
	ent := &selectShapeEntry{gen: fp, bareRefs: projectionIsBareRefs(s)}
	e.shapeMemo[s] = ent
	return ent
}

// cachedShapeEntry returns the statement's entry without building one (the
// deep-path consults — affinity, decode, output slots — run after the
// statement-level memo decision and never need to force a build).
func (e *SelectEngine) cachedShapeEntry(s *sql.SelectStmt) *selectShapeEntry {
	if !e.shapeStable || e.shapeMemo == nil {
		return nil
	}
	if ent, ok := e.shapeMemo[s]; ok && ent.gen == e.shapeMemoFP {
		return ent
	}
	return nil
}

// projectionIsBareRefsCached serves projectionIsBareRefs from the shape memo
// when the statement has one; the entry's bareRefs bit is decided at build.
func (e *SelectEngine) projectionIsBareRefsCached(s *sql.SelectStmt) bool {
	if ent := e.cachedShapeEntry(s); ent != nil {
		return ent.bareRefs
	}
	return projectionIsBareRefs(s)
}

// selectNeedsRowMapsCached serves SelectNeedsRowMaps from the shape memo.
// The verdict is a pure function of statement structure and the FROM table's
// class (schema-table check included); the registry-relevant aggregate
// classification only fires on FuncCall nodes, which memo-eligible
// statements cannot carry.
func (e *SelectEngine) selectNeedsRowMapsCached(s *sql.SelectStmt, tableName string) bool {
	if ent := e.shapeEntryFor(s); ent != nil {
		if !ent.needsRowMapsDone {
			ent.needsRowMaps = SelectNeedsRowMaps(e, s, tableName)
			ent.needsRowMapsDone = true
		}
		return ent.needsRowMaps
	}
	return SelectNeedsRowMaps(e, s, tableName)
}

// hasAggregatesCachedStmt serves hasAggregates from the shape memo. On the
// memo-eligible census (no FuncCall anywhere) the walk can only confirm the
// false verdict; memo-eligible statements with calls are not memoized.
func (e *SelectEngine) hasAggregatesCachedStmt(s *sql.SelectStmt) bool {
	if ent := e.cachedShapeEntry(s); ent != nil {
		if !ent.hasAggDone {
			ent.hasAgg = e.hasAggregates(s.Columns)
			ent.hasAggDone = true
		}
		return ent.hasAgg
	}
	return e.hasAggregates(s.Columns)
}

// validateSelectPreDispatchCached serves validateSelectPreDispatch's verdict
// from the shape memo. The checks read only statement structure, colDefs,
// and schema resolution (the fingerprint's domain): a repeated structure
// under the same schema has the same verdict. The runtime-dependent forms
// (an outer row, enclosing alias scopes, a compound-member context, a
// trigger depth — the same gate the prevalidate memo applies, because the
// walks' own branches consult that state) always run directly.
func (e *SelectEngine) validateSelectPreDispatchCached(s *sql.SelectStmt) error {
	if !e.shapeStable || e.prevalidateRuntimeDependent() {
		return e.validateSelectPreDispatch(s)
	}
	if ent := e.shapeEntryFor(s); ent != nil {
		if !ent.preDispatchDone {
			ent.preDispatchErr = e.validateSelectPreDispatch(s)
			ent.preDispatchDone = true
		}
		return ent.preDispatchErr
	}
	return e.validateSelectPreDispatch(s)
}
