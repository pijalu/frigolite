// Evaluation of LIKE/GLOB terms whose planner synthesized the prefix range
// (sql.LikeRangeOpt, see execquery/select_like_opt.go). This is the
// execution counterpart of whereexpr.c exprAnalyze's virtual
// x>='abc' AND x<'abd' constraints and wherecode.c's like()-call elision:
// the bounds are checked first; a row outside the range fails without ever
// invoking the matcher (the index seek never visits it in SQLite), and a
// complete pattern inside the range is decided by the bounds alone, so the
// like()/glob() invocation — and with it the sqlite3_like_count increment —
// is skipped exactly where SQLite's code generator omits it.

package execexpr

import (
	"bytes"

	"github.com/pijalu/frigolite/internal/sql"
)

// evalLikeRangeOp evaluates a LIKE/GLOB BinaryOp carrying LikeRangeOpt.
// Both operands are non-NULL (the generic NULL pre-check ran).
func (ev *Evaluator) evalLikeRangeOp(v *sql.BinaryOp, left, right interface{}) (interface{}, error) {
	opt := v.LikeRange
	// The bounds are plain strings; the row value compares as stored
	// (affinity conversions happened at storage time), so peel the
	// collation/affinity wrappers the column evaluation may have added.
	base := unwrapCollatedValue(left)
	if b, isBlob := base.([]byte); isBlob {
		// Second pass of the LIKE-optimization scan (wherecode.c): the
		// bounds are cast to BLOBs and compared byte-wise, so blob rows
		// whose bytes fall in the prefix range are still visited.
		if bytes.Compare(b, []byte(opt.Low)) < 0 || bytes.Compare(b, []byte(opt.High)) >= 0 {
			return int64(0), nil
		}
		if opt.IsComplete && !opt.NoCase {
			// The parent term is disabled on both passes (TERM_CODED).
			return int64(1), nil
		}
		return ev.evalLikeRangeFallback(v, left, right)
	}
	// Text pass: the bounds compare under the synthesized collation.
	if ev.ctx.CompareValuesCollate(base, opt.Low, opt.Collation) < 0 ||
		ev.ctx.CompareValuesCollate(base, opt.High, opt.Collation) >= 0 {
		return int64(0), nil
	}
	if opt.IsComplete {
		// The range provably decides the row: with BINARY bounds the parent
		// term is disabled on both passes (TERM_CODED); with folded NOCASE
		// bounds it is only skipped on this (text) pass (TERM_LIKECOND) —
		// either way the matcher never runs for a text row here.
		return int64(1), nil
	}
	// Incomplete pattern ('a_c', 'ab%d'): the range only narrowed the
	// candidates, the matcher still decides every visited row.
	return ev.evalLikeRangeFallback(v, left, right)
}

// evalLikeRangeFallback routes to the plain LIKE/GLOB evaluation for rows
// the range let through.
func (ev *Evaluator) evalLikeRangeFallback(v *sql.BinaryOp, left, right interface{}) (interface{}, error) {
	switch v.Operator {
	case "GLOB":
		bumpLikeCallCount()
		res := globValues(left, right)
		if res {
			ev.probeOperatorOverload("GLOB", right, left)
		}
		return boolToInt(res), nil
	default: // LIKE
		if v.Escape != "" || v.HasEscape {
			return ev.evalLikeWithEscape(v, left, right)
		}
		return ev.evalLikeOp(left, right, false), nil
	}
}
