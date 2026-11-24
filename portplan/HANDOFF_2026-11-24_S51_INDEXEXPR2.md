# Session handoff — S51 slice 2: the last indexexpr2 leaves (2026-11-24, e3f3ed7ad)

Restart point for the goal "close all gaps with sqlite — functional first,
then performance". Working tree clean after the docs commit; the engine slice
is committed at `e3f3ed7ad`.

## Landed

- `e3f3ed7ad` — `indexexpr2` 3→0 (family green), `starschema1` 8→1:
  - Planner/executor expression-index WHERE seeks: `collectBinaryRef` and
    `indexSeekBinaryTerm` resolve non-column operands against index key
    expressions, compare canonical SQL, apply the index key's declared
    collation and the expression's affinity to the seek bound, and accept
    expression keys in the seek prefix. `indexexpr2-1.2` now returns C's
    index-key order (`threex`, `twox`), and `6.2.2`'s `CAST(b AS TEXT)=123`
    still finds both rows.
  - Expression-index ORDER BY: `orderTermCol` carries canonical expression
    text; `forEachOrderServingIndex` matches key expressions positionally
    under the term's effective collation. `3.4.5eqp` plans `SCAN t4 USING
    INDEX i4`; `3.4.6`'s BINARY term still declines the NOCASE index.
  - Correlated aggregate resolution: `aggFuncRefsOuter` now follows C's
    resolve.c `sqlite3ReferencesSrcList` rule — an aggregate whose argument
    references any column of the subquery's own FROM stays INNER even when it
    also references an outer column; an outer-only argument in WHERE is the
    misuse. `11.0` returns `44 -44`; `sum(-a0.a)` still errors.
  - CREATE INDEX key parsing is balanced/quote-aware in both
    `sql.ParseIndexColumnCollations` and `execdml.parseIndexColumns`, and
    query-side collation lookup handles expression keys with commas. The
    `execdml` parser alone fixes all seven `starschema1-1.3..1.9` GONEs
    (verified at the pre-unit commit with a scratch worktree, 3/3 repeats).
  - Pure-Go pin: `TestIndexexpr2EnginePins` covers the three leaf contracts.

## Gate evidence

- Focused `/tmp/t10_index_after.log`: `FRIGOLITE_TEST=indexexpr2` 0 FAIL.
- Focused `/tmp/t10_stars_after.log`: `starschema1` only `1.2` remains.
- `go test ./internal/...`: green (`/tmp/t10_internal_after.log`); pin test
  green; `git diff --check` clean.
- Full gate `/tmp/t10_full2.log` (1162.8s): **1351** failing leaves. Raw-byte
  leaf-set diff vs `/tmp/s50_full3.log` (1383): NEW=1
  (`temptable2-4.1.1`, the documented unseeded-randomblob flake — focused
  rerun has only `4.1.4`/`5.1.2`), GONE=33 (`indexexpr2` ×4 including parent,
  `starschema1-1.3..1.9` ×8 including parent, `temptable2-9.1.x` stress
  flakes, `vacuum6` + `4.0` load flake). The pre-refactor full gate
  `/tmp/t10_full.log` had NEW=0; after the complexity-only helper refactor
  the only delta is the documented flake. No engine regression.
- Complexity: no new gocognit/gocyclo findings above the pre-unit baseline in
  touched files (helpers extracted). Staticcheck and hard file-size failures
  are pre-existing at HEAD; tools-package unit failures also reproduce at
  HEAD.

## Restart point

Next unit: **expridx1** — its 10 leaves are engine-side expression-index
maintenance (stale entries after UPDATE/DELETE, integrity_check's
`row N missing from index` / imprecise-float diagnostics, NOT-INDEXED
existence probes) plus 2.3's 500-row result. Not a converter fossil. Then
A2/A9 engine slices and the `fts3*` residue (~53).

## Risks / working rules

- `canonicalExprSQL` and the balanced CREATE INDEX key parser are shared;
  any edit must keep `gencol1-23.3/23.4`, `collate1/9`, and `e_reindex` green.
- Expression affinity is part of the seek contract: `CAST(x AS TEXT)=123`
  needs the constant converted to TEXT before probing the stored key.
- Correlated aggregate misuse depends on whether the argument references the
  inner FROM, not merely on the presence of an outer reference.
- Compare full gates by raw-byte leaf-name sets only (`re.findall` on
  `--- FAIL: TestSQLiteSuite/`), never by counts or line greps.
- Keep full gates sequential and exclusive CPU; store logs under `/tmp`.
