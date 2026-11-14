package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns the WHERE-ref analysis memo (R14 §7 #5): the
// collectIndexedRefs + seekableRefs pair that bestIndexForRowCount and
// indexSeekRefSet re-run on every execution of the same statement shape,
// ~30% of an indexed SELECT's allocations (alloc profile: collectBinaryRef /
// seekableRefs / refsAt plus findIndexOnColumn's schema walk and
// ParseIndexColumns underneath).
//
// The analysis splits into a SHAPE part — which index each predicate
// resolves to (findIndexOnColumn's schema walk, the LIKE collation lookup)
// and the seekable-prefix filter — and a VALUE part (the constants the
// predicates compare). Only the shape part memoizes. Every serve re-derives
// the value part from the statement's CURRENT literal leaves, so a slot-path
// live clone (one persistent clone per (template, exec depth) whose literal
// leaves are rewritten in place per execution) is served with this
// execution's values, byte-identically to an uncached run: the downstream
// consumers (refEstimate's estimateSelectivity, stat1/stat4 probes,
// formatConditions) read the same fresh constants they always did.
//
// Identity contract (the prevalidateMemo/shapeMemo pattern): entries are
// keyed by the WHERE expression pointer plus the analysis table name (a
// join's shared WHERE is analyzed once per table), and every consult gates
// on shapeStable (context.go), which the exec engine's prepare paths set per
// statement — the COW scratch clone recycles per-exec-depth structs across
// templates, so a recycled address would alias a stale entry. The schema
// fingerprint guards DDL. The memo lives on the QueryPlanner sub-executor
// (core.go) because SelectEngine's field set is owned by context.go; the
// state is created once per engine through the engine's own &e.planner
// field and dies with it, so there is no package-level state and no
// cross-engine sharing.
//
// Refusal contract (correctness first): a shape is not memoized — a nil
// tombstone is stored so the census is not re-run per execution — when
// - the WHERE carries FuncCall (constantExpr depends on the function
//   registry, which no fingerprint moves), COLLATE wrappers, Subquery or
//   ParameterExpr nodes,
// - the analyzed table has any partial index (partialIndexImplied renders
//   the WHERE conjuncts, whose literal text a slot-path reuse changes),
// - a BETWEEN operand is an expression (findIndexOnExpr resolves the
//   operand's rendered text, which literal rewrites can change).
// LIKE sites keep their per-execution re-derivation even when memoized
// (prefix existence and the case_sensitive_like verdict are value-/pragma-
// dependent); a LIKE-bearing shape therefore gets no precomputed seekable
// outcome and replays seekableRefs live over the rebuilt refs.

// whererefMemoCap bounds the memo between DDLs (the colNamesMemoCap
// pattern): keys retain their statement ASTs, so the cap keeps retention
// bounded; reaching the cap drops the whole epoch (the next statement
// rebuilds from one entry).
const whererefMemoCap = 64

// whererefKey identifies one memoized analysis: the WHERE expression's
// template-stable pointer plus the table name the analysis ran against.
type whererefKey struct {
	where sql.Expr
	table string
}

// whererefSiteKind names the predicate-node class a site was recorded from;
// the class picks the re-derivation whererefSiteRef mirrors.
type whererefSiteKind uint8

const (
	whererefSitePlain   whererefSiteKind = iota // column-to-constant BinaryOp
	whererefSiteLike                            // LIKE BinaryOp (constant string pattern side)
	whererefSiteBetween                         // BETWEEN whose operand is a bare column reference
)

// whererefSite is one predicate node's recorded resolution: the structural
// fields the collectors resolve through schema walks, kept for the
// per-execution rebuild to hang fresh values on.
type whererefSite struct {
	node    sql.Expr // the *sql.BinaryOp or *sql.Between (template-stable)
	kind    whererefSiteKind
	idxName string // resolved planner index token ("" = the site never yields a ref)
	colName string // column the predicate constrains ("" when none was resolved)
	op      string // the BinaryOp operator (plain sites; verified on rebuild)
	coll    string // LIKE: the resolved index column collation
	leftCol bool   // plain: the column operand is the BinaryOp's Left side
}

// whererefSkeleton is one statement shape's memoized analysis: the
// predicate sites in collectIndexedRefs's walk order plus, for the
// all-plain shape, the seekableRefs outcome recorded as ref/site slots.
type whererefSkeleton struct {
	sites []whererefSite

	// likeFree marks the all-plain/between shape: ref existence is then
	// structural, so the collect-order ref list and the seekableRefs outcome
	// are fixed across executions and precomputed below.
	likeFree bool
	// refCount is the size of the collect-order ref list the sites produce
	// (likeFree only): bestIndexForRowCount compares its post-eligibility
	// list against it to decide whether the recorded outcome still applies.
	refCount int
	// seekRefs is the seekableRefs outcome as collect-order ref indices, in
	// output order (likeFree only; nil when no refs survive the prefix rule).
	seekRefs []int32
	// seekSites names the site that produced each seekRefs slot, so
	// indexSeekRefSet's per-token allowed sets can be published once.
	seekSites []int32
	// seekSets lazily caches indexSeekRefSet's per-token (column, operator)
	// sets. Each set is built once from the recorded outcome and then only
	// read (the seek executor looks constraints up, never mutates).
	seekSets map[string]map[string]bool
}

// whererefState is the per-engine memo: one epoch of entries per schema
// fingerprint (the fingerprint guards rebuild the map on DDL).
type whererefState struct {
	fp      uint64
	entries map[whererefKey]*whererefSkeleton
}

// whererefMemo returns the engine's WHERE-ref memo, creating it on first
// use through the engine's own planner field.
func (e *SelectEngine) whererefMemo() *whererefState {
	if e.planner.whereref == nil {
		e.planner.whereref = &whererefState{}
	}
	return e.planner.whereref
}

// whererefCensusOK reports whether every expression node the ref collectors
// visit is a kind whose resolution is a pure function of structure and
// schema. FuncCall (constantExpr consults the function registry), COLLATE
// wrappers, Subquery and ParameterExpr (runtime values) are refused and the
// caller computes those shapes fresh. The walk mirrors walkExpr's descent
// exactly — the collectors see the same nodes — so node kinds they cannot
// reach (InList items, CASE arms, EXISTS bodies) need no verdict here.
func whererefCensusOK(where sql.Expr) bool {
	ok := true
	walkExpr(where, func(n sql.Expr) {
		switch v := n.(type) {
		case *sql.FuncCall, *sql.Subquery, *sql.ParameterExpr:
			ok = false
		case *sql.BinaryOp:
			if v.Operator == "COLLATE" {
				ok = false
			}
		}
	})
	return ok
}

// whererefTableHasPartialIndex reports whether any index on the table
// carries a partial predicate. partialIndexImplied renders the query WHERE's
// conjunct text, whose literals a slot-path reuse rewrites per execution —
// a shape analyzed under such an index must not be memoized.
func (e *SelectEngine) whererefTableHasPartialIndex(tableName string) bool {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return true // unresolved schema: refuse to cache
	}
	for _, entry := range entries {
		if entry.Type != "index" || entry.TblName != tableName {
			continue
		}
		if entry.SQL != "" && indexWhereRe.MatchString(entry.SQL) {
			return true
		}
	}
	return false
}

// whererefCollect serves collectIndexedRefs(where, tableName) from the shape
// memo: on a hit the refs are rebuilt in collect order with the CURRENT
// literal values. ok=false sends the caller down the uncached pair
// (unstable AST pointer, a refused node kind, a partial index on the table,
// a full memo, or a structural drift on rebuild). The returned slice is
// fresh per call and caller-owned — only the recomputation disappears.
func (e *SelectEngine) whererefCollect(where sql.Expr, tableName string) ([]indexedRef, *whererefSkeleton, bool) {
	if !e.shapeStable || where == nil {
		return nil, nil, false
	}
	fp := e.schemaFingerprint()
	st := e.whererefMemo()
	if st.fp != fp || st.entries == nil {
		st.fp = fp
		st.entries = make(map[whererefKey]*whererefSkeleton)
	}
	key := whererefKey{where: where, table: tableName}
	sk, ok := st.entries[key]
	if !ok {
		if len(st.entries) >= whererefMemoCap {
			st.entries = make(map[whererefKey]*whererefSkeleton)
		}
		sk = e.whererefBuild(where, tableName)
		st.entries[key] = sk // nil = tombstone: the shape is refused, don't re-run the census
	}
	if sk == nil {
		return nil, nil, false
	}
	refs, valid := e.whererefServe(sk)
	if !valid {
		return nil, nil, false
	}
	return refs, sk, true
}

// whererefBuild records one shape's predicate resolutions with the uncached
// collectors (so the recorded build refs are exactly what
// collectIndexedRefs would have produced) and precomputes the seekableRefs
// outcome for the all-plain shape. nil marks a refused shape.
func (e *SelectEngine) whererefBuild(where sql.Expr, tableName string) *whererefSkeleton {
	if !whererefCensusOK(where) {
		return nil
	}
	if e.whererefTableHasPartialIndex(tableName) {
		return nil
	}
	sk := &whererefSkeleton{}
	var refs []indexedRef
	var refSite []int32
	refuse := false
	walkExpr(where, func(n sql.Expr) {
		if binop, isOp := n.(*sql.BinaryOp); isOp {
			sk.recordBinary(binop, tableName, e, where, &refs, &refSite)
		}
	})
	walkExpr(where, func(n sql.Expr) {
		if bt, isBt := n.(*sql.Between); isBt {
			if _, isCol := bt.Operand.(*sql.ColumnRef); !isCol {
				// findIndexOnExpr resolves the operand's rendered text,
				// which a slot-path literal rewrite can change.
				refuse = true
				return
			}
			sk.recordBetween(bt, tableName, e, where, &refs, &refSite)
		}
	})
	for _, ref := range refs {
		if _, isRef := ref.constantExpr.(*sql.ColumnRef); isRef {
			// A correlated bound: the value comes from the enclosing outer
			// rows at execution, not from a literal leaf the slot-path
			// replay can re-read — refuse the memo (the uncached walk runs
			// fresh per execution).
			refuse = true
			break
		}
	}
	if !refuse {
		// A comparison whose bound resolves through the enclosing outer
		// rows (the correlated-bound class): the skeleton's recordBinary
		// extraction cannot reproduce it — refuse the memo.
		walkExpr(where, func(n sql.Expr) {
			if refuse {
				return
			}
			if binop, isOp := n.(*sql.BinaryOp); isOp && binop.Operator != "LIKE" {
				if colRef, constVal, constExpr := findColAndConstExpr(binop, e); colRef == nil || (constVal == nil && constExpr == nil) {
					if cr, bound, _ := e.seekRefSides(binop, tableName); cr != nil && bound != nil {
						refuse = true
					}
				}
			}
		})
	}
	if refuse {
		return nil
	}
	sk.likeFree = true
	for i := range sk.sites {
		if sk.sites[i].kind == whererefSiteLike {
			sk.likeFree = false
			break
		}
	}
	if sk.likeFree {
		sk.refCount = len(refs)
		order := e.seekableRefOrder(refs, tableName)
		if len(order) > 0 {
			sk.seekRefs = make([]int32, len(order))
			sk.seekSites = make([]int32, len(order))
			for k, ri := range order {
				sk.seekRefs[k] = ri
				sk.seekSites[k] = refSite[ri]
			}
		}
	}
	return sk
}

// recordBinary resolves one BinaryOp predicate with the uncached collector's
// algorithm and records the outcome as a site: the structural fields the
// per-execution rebuild re-derives values around.
func (sk *whererefSkeleton) recordBinary(binop *sql.BinaryOp, tableName string, e *SelectEngine, where sql.Expr, refs *[]indexedRef, refSite *[]int32) {
	siteIdx := int32(len(sk.sites))
	site := whererefSite{node: binop, op: binop.Operator}
	switch {
	case binop.Operator == "LIKE":
		site.kind = whererefSiteLike
		if colRef, isCol := binop.Left.(*sql.ColumnRef); isCol {
			site.colName = colRef.Name
			if _, isStr := likePatternConst(binop.Right); isStr {
				site.idxName = e.findIndexOnColumn(tableName, colRef.Name, where)
				if site.idxName != "" {
					site.coll = e.indexColumnCollation(tableName, site.idxName, colRef.Name)
				}
			}
		}
		if produced := collectLikeRef(binop, tableName, e, where); len(produced) > 0 {
			*refs = append(*refs, produced[0])
			*refSite = append(*refSite, siteIdx)
		}
	default:
		site.kind = whererefSitePlain
		if colRef, constVal, constExpr := findColAndConstExpr(binop, e); colRef != nil && (constVal != nil || constExpr != nil) {
			_, site.leftCol = binop.Left.(*sql.ColumnRef)
			site.colName = colRef.Name
			site.idxName = e.findIndexOnColumn(tableName, colRef.Name, where)
			if site.idxName != "" {
				// collectBinaryRef's ref, mirrored field for field (the
				// census guarantees constExpr is nil for these shapes).
				*refs = append(*refs, indexedRef{
					indexName: site.idxName, colName: colRef.Name,
					constant: constVal, constantExpr: constExpr, op: binop.Operator,
				})
				*refSite = append(*refSite, siteIdx)
			}
		}
	}
	sk.sites = append(sk.sites, site)
}

// recordBetween resolves one BETWEEN predicate with a bare column operand
// (collectBetweenRef's column branch) and records the outcome as a site.
func (sk *whererefSkeleton) recordBetween(bt *sql.Between, tableName string, e *SelectEngine, where sql.Expr, refs *[]indexedRef, refSite *[]int32) {
	siteIdx := int32(len(sk.sites))
	site := whererefSite{node: bt, kind: whererefSiteBetween}
	colRef := bt.Operand.(*sql.ColumnRef)
	site.colName = colRef.Name
	site.idxName = e.findIndexOnColumn(tableName, colRef.Name, where)
	if site.idxName != "" {
		low, lowErr := e.ctx.EvalExpr(bt.Low, nil)
		high, highErr := e.ctx.EvalExpr(bt.High, nil)
		if lowErr != nil || highErr != nil {
			low, high = nil, nil
		}
		*refs = append(*refs, indexedRef{
			indexName: site.idxName, colName: colRef.Name,
			constant: float64(0), op: "BETWEEN",
			selectivity: computeBetweenSelectivity(bt), rangeLow: low, rangeHigh: high,
		})
		*refSite = append(*refSite, siteIdx)
	}
	sk.sites = append(sk.sites, site)
}

// whererefServe rebuilds the shape's collect-order ref list from the
// recorded sites, hanging THIS execution's literal values on the recorded
// resolutions. valid=false marks structural drift — the caller falls back
// to the uncached pair.
func (e *SelectEngine) whererefServe(sk *whererefSkeleton) ([]indexedRef, bool) {
	refs := make([]indexedRef, 0, len(sk.sites))
	for i := range sk.sites {
		ref, produced, valid := e.whererefSiteRef(&sk.sites[i])
		if !valid {
			return nil, false
		}
		if produced {
			refs = append(refs, ref)
		}
	}
	return refs, true
}

// whererefSiteRef re-derives one site's ref from the statement's current
// literal leaves, mirroring the collector the site was recorded from.
// produced=false marks a site yielding no ref under the current values (a
// LIKE pattern whose prefix vanished); valid=false marks structural drift.
// A site with no resolved index is a schema-guarded dead site: within one
// fingerprint epoch it can never yield a ref, whatever the values do.
func (e *SelectEngine) whererefSiteRef(site *whererefSite) (ref indexedRef, produced, valid bool) {
	switch node := site.node.(type) {
	case *sql.BinaryOp:
		if site.kind == whererefSiteLike {
			return e.whererefLikeRef(site, node)
		}
		return e.whererefPlainRef(site, node)
	case *sql.Between:
		return e.whererefBetweenRef(site, node)
	}
	return indexedRef{}, false, false
}

// whererefPlainRef mirrors collectBinaryRef's comparison branch over a
// recorded plain site.
func (e *SelectEngine) whererefPlainRef(site *whererefSite, node *sql.BinaryOp) (indexedRef, bool, bool) {
	if site.idxName == "" {
		return indexedRef{}, false, true
	}
	colRef, constVal, constExpr := findColAndConstExpr(node, e)
	_, leftIsCol := node.Left.(*sql.ColumnRef)
	if colRef == nil || (constVal == nil && constExpr == nil) || constExpr != nil ||
		leftIsCol != site.leftCol || node.Operator != site.op ||
		!strings.EqualFold(colRef.Name, site.colName) {
		return indexedRef{}, false, false
	}
	return indexedRef{
		indexName: site.idxName, colName: colRef.Name,
		constant: constVal, constantExpr: constExpr, op: node.Operator,
	}, true, true
}

// whererefLikeRef mirrors collectLikeRef over a recorded LIKE site: the
// prefix existence and the case_sensitive_like verdict are value-/pragma-
// dependent and re-read per execution; only the index resolution and the
// index column collation are served from the site.
func (e *SelectEngine) whererefLikeRef(site *whererefSite, node *sql.BinaryOp) (indexedRef, bool, bool) {
	if site.idxName == "" {
		return indexedRef{}, false, true
	}
	colRef, ok := node.Left.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(colRef.Name, site.colName) {
		return indexedRef{}, false, false
	}
	pattern, ok := likePatternConst(node.Right)
	if !ok {
		return indexedRef{}, false, false
	}
	prefix, ok := likePrefix(pattern, node.Escape, node.HasEscape)
	if !ok || prefix == "" {
		return indexedRef{}, false, true
	}
	if !e.likeIndexCompatible(site.coll) {
		return indexedRef{}, false, true
	}
	return indexedRef{
		indexName: site.idxName, colName: colRef.Name,
		constant: prefix, op: "LIKE", selectivity: estimateLikePrefixSelectivity(prefix),
	}, true, true
}

// whererefBetweenRef mirrors collectBetweenRef's column-operand branch over
// a recorded BETWEEN site; the bounds are evaluated per execution.
func (e *SelectEngine) whererefBetweenRef(site *whererefSite, node *sql.Between) (indexedRef, bool, bool) {
	if site.idxName == "" {
		return indexedRef{}, false, true
	}
	colRef, ok := node.Operand.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(colRef.Name, site.colName) {
		return indexedRef{}, false, false
	}
	low, lowErr := e.ctx.EvalExpr(node.Low, nil)
	high, highErr := e.ctx.EvalExpr(node.High, nil)
	if lowErr != nil || highErr != nil {
		low, high = nil, nil
	}
	return indexedRef{
		indexName: site.idxName, colName: colRef.Name,
		constant: float64(0), op: "BETWEEN",
		selectivity: computeBetweenSelectivity(node), rangeLow: low, rangeHigh: high,
	}, true, true
}

// whererefSeekOrdered replays the skeleton's recorded seekableRefs outcome
// over the freshly rebuilt collect-order refs (all-plain shape, eligibility
// having dropped nothing). The pick is by ref index, so the output order is
// byte-identical to a live seekableRefs call on the same list.
func whererefSeekOrdered(refs []indexedRef, sk *whererefSkeleton) []indexedRef {
	out := make([]indexedRef, 0, len(sk.seekRefs))
	for _, ri := range sk.seekRefs {
		out = append(out, refs[ri])
	}
	return out
}

// seekSet returns indexSeekRefSet's allowed set for token, building it once
// from the recorded seekable outcome. The set is published read-only:
// buildIndexSeekPlan and its helpers only look constraints up.
func (sk *whererefSkeleton) seekSet(token string) map[string]bool {
	if set, ok := sk.seekSets[token]; ok {
		return set
	}
	if sk.seekSets == nil {
		sk.seekSets = make(map[string]map[string]bool)
	}
	set := map[string]bool{}
	for _, si := range sk.seekSites {
		if site := &sk.sites[si]; site.idxName == token {
			set[indexSeekRefKey(site.colName, site.op)] = true
		}
	}
	sk.seekSets[token] = set
	return set
}
