# PLAN S12 — the ORDER BY collation-aware index matcher (e_reindex + index EQP)

## C contract (where.c's ORDER BY index satisfaction)

The planner may satisfy `ORDER BY <term>` with an index when the term is a
column reference whose EFFECTIVE collation equals the index column's
DECLARED collation by NAME (expr.c's collating-function resolution):
- the term's effective collation = an explicit `COLLATE x` on the term,
  else the table column's declared COLLATE, else BINARY;
- the index column's declared collation = the index definition's explicit
  `COLLATE x`, else the table column's declared COLLATE, else BINARY.
Name equality — the CURRENT REGISTRATION of the collation is irrelevant:
a stale stored order (an index last rebuilt under a different
registration) is still used (e_reindex-2.3.3's contract — REINDEX collA
leaves the collB indexes in their earlier order and the planner returns
that stored order through the index).

Today's matcher (explain_index.go findIndexOnColsForQuery) compares column
NAMES only and refuses COLLATE-wrapped ORDER BY terms outright:
- `ORDER BY x COLLATE c` never uses an index (temp b-tree) even when the
  index declares the same collation — e_reindex's 19 remaining failures;
- a bare `ORDER BY x` against an index declared `x COLLATE nocase` uses
  the index and skips the sorter even though C temp-sorts — a latent
  wrong-order bug (verified by the S12 probes).

## Changes

1. **execdml**: move `parseIndexColumnCollations` from exec/pragma_index.go
   to execdml/helpers.go (same-layer seam; exec stays a caller via the
   exported wrapper `ParseIndexColumnCollations` in export.go).
2. **execquery/explain.go**: `orderTermCol{col, coll}` +
   `orderByTermColls(s)` — per ORDER BY term the column name and the
   explicit COLLATE (splitCollateOperand); nil when a term is not a
   (possibly COLLATE-wrapped) bare column.
3. **execquery/explain_index.go**: `findIndexForOrderTerms(tableName,
   terms, where)` — per candidate index, per column: declared = the index
   SQL's explicit COLLATE (ParseIndexColumnCollations; autoindexes fall
   back to the table column's declared collation), else the table column's
   declared COLLATE (ParseColumnDefs), else BINARY; the term's effective
   collation matches by EqualFold ("" normalizes to BINARY).
4. **execquery/explain_plan.go** orderByIndexToken: prefer the collation
   -aware matcher; keep the name-only matcher for terms it cannot
   represent (expressions) — behavior preserved.

## Verification

- Scratch probes become the native pin `TestOrderByCollationIndexReuse`:
  the same-name index is used for an explicit-COLLATE ORDER BY (stale
  stored order wins — the e_reindex contract); a mismatched-collation
  index is refused (temp b-tree, correct row order).
- e_reindex family failures 19 -> ~0; the index-sweep (index/index9/.../
  bestindex*) failure-name-set diff vs /tmp/index_fails.txt shows only the
  intended movement; then the full-suite check for 0 new.
