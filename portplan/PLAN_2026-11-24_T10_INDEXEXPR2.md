# Plan — S51 slice 2: the three indexexpr2 engine leaves (2026-11-24)

Goal: close `indexexpr2-1.2`, `indexexpr2-3.4.5eqp`, `indexexpr2-11.0`
without regressions in the family, `gencol1`, `count`, `window1`, or the
full-suite durable fail set. Baseline: `049a2a068`, focused family = 3
leaves (`/tmp/indexexpr2_t10_base.log`).

## Leaf 1.2 — expression-index WHERE seek

Observed: the engine scans the table and returns `[twox, threex]`; C seeks
`i1` on `b || 'x'` and returns `[threex, twox]` (oracle 3.54;
`SELECT ... INDEXED BY i1` confirms the stored index order). The planner
only resolves bare `ColumnRef` operands (`findColAndConstExpr`), and the
execution seek refuses expression keys (`isPlainIndexColumn`).

C parity: where.c's `whereLoopAddBtree` matches a TK_COLUMN-or-expression
term against `Index.aCol[].pExpr` with `sqlite3ExprCompare`; the seek probe
uses that expression's value under the index's KeyInfo collation.

Changes:

1. `internal/execquery/explain_helpers.go`: canonicalize indexed-expression
   SQL (case-fold, strip whitespace outside quoted literals) so `ExprString`
   and CREATE INDEX text compare. Add a balanced-paren-aware index key
   collation extractor (the existing naive `strings.Split` version cannot
   parse `Substr(a,-2) COLLATE nocase`).
2. `internal/execquery/explain_index.go`: `collectBinaryRef` gets an
   expression-operand path mirroring the column path (either orientation,
   operator flip, explicit/index collation agreement). `findIndexOnExpr`
   compares canonical key expressions after dropping a top-level COLLATE
   suffix. Keep BETWEEN's existing expression path working.
3. `internal/execquery/whereref_cache.go`: refuse memoization for shapes
   with expression comparison operands — their `colName` is rendered SQL
   and the structural replay is not set up for it (correctness first).
4. `internal/execquery/select_index_seek.go`: `indexSeekBinaryTerm` resolves
   expression operands; the seek prefix accepts expression keys. No table
   column affinity is applied to an expression key.
5. `internal/execquery/select_index_seek_exec.go`: KeyInfo collations come
   from the index's per-key explicit COLLATE (positional), so an expression
   key ordered NOCASE is probed NOCASE.

Verification: focused `FRIGOLITE_TEST=indexexpr2` still has only 3 (then 2)
leaves; pure-Go pin for row order; `go test ./internal/...`.

## Leaf 3.4.5eqp — expression-index ORDER BY

Observed: `SCAN t4` + temp b-tree, want `SCAN t4 USING INDEX i4` for
`ORDER BY Substr(a,-2) COLLATE nocase` over `CREATE INDEX i4 ON
t4(Substr(a,-2) COLLATE nocase)`. The planner's `orderByTermColls` only
represents bare columns, so expression-index ORDER BY is invisible.

C parity: where.c's `wherePathSatisfiesOrderBy` compares the ORDER BY term's
expression tree and effective collation with each index key expression and
its declared collation.

Changes:

1. `internal/execquery/explain.go`: `orderTermCol` carries an optional
   canonical expression; `orderByTermColls` renders non-column terms after
   stripping the top-level COLLATE.
2. `internal/execquery/explain_index.go`: `forEachOrderServingIndex` walks
   expression keys positionally, comparing canonical expression text and
   effective collations (term COLLATE else BINARY vs index key COLLATE else
   BINARY). Plain-column behavior and rowid-tail handling stay unchanged.
3. `internal/execquery/explain_plan.go`: no change expected — the resolver
   already consumes the new term representation.

Verification: focused family leaf 3.4.5eqp green, `3.4.6` (BINARY ORDER BY
must NOT use the NOCASE index) still green, pure-Go EQP pin.

## Leaf 11.0 — correlated aggregate resolution

Observed: engine errors `misuse of aggregate: sum()` on
`SELECT * FROM t3 AS a0 WHERE (SELECT sum(-a0.a=b) FROM t3 GROUP BY b)
GROUP BY b`; C returns `44 -44`. Oracle discriminators: `sum(-a0.a)` is a
misuse, `sum(1)` and `sum(-a0.a=b)` are legal.

C parity: resolve.c's function resolution sets `TK_AGG_FUNCTION` and walks
up name contexts while `sqlite3ReferencesSrcList` finds no reference to the
context's FROM list. An aggregate whose argument references an inner column
belongs to the inner context even when it also references an outer column;
an outer-only aggregate in a WHERE context is the misuse.

Changes:

1. `internal/execquery/select_agg_validate.go`: `aggFuncRefsOuter` returns
   an aggregate name only when the argument/FILTER references an outer
   column AND no column of the subquery's own FROM (the current code flags
   any outer reference). Add an `exprRefsInnerCol` probe that honors
   qualified table names.
2. Keep `whereSubqueryOuterAggRef` and the IN-subquery checks intact; they
   consume the corrected predicate.

Verification: focused family leaf 11.0 green; pure-Go pins for the three
oracle discriminators; `count` / `window1` family gates unchanged.

## Unit close-out

- Pure-Go pin tests beside the engine files changed.
- Focused gates: indexexpr2, gencol1, count, window1, index9, bestindexF.
- `go test ./internal/...` + `go test ./...` (default, no testgen tag).
- Full-suite gate with raw-byte leaf-set diff vs durable baseline; NEW must
  be zero except documented flakes.
- Commit code, then docs (TODO.md, lessons, dated handoff).
