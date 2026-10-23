# View NATURAL/USING star-merge and unused-column marking plan

Status: **landed** (2026-10-19). Both fixes implemented and verified; `join5` green
(fixture also regenerated from `ori/sqlite/test` — the old JSON was missing tests
1.1–2.12). Evidence: run8 log `/tmp/frigolite-full-suite8.log` (`join5-10.1`).

## Root causes (oracle-verified with sqlite3 3.54)

### A. View column defs do not merge USING/NATURAL star columns
`internal/exec/pragma_table_views.go:viewStarDefs` expands a bare `*` to the raw
concatenation of all FROM source defs (`viewSourceDefs`), without applying the
using/natural right-side merge that the executor applies at run time
(`select_join.go` `filterUsingColumns` / `filteredUsingColumnIndices`).

- Engine: `CREATE VIEW v1 AS SELECT * FROM t1 NATURAL JOIN t1; PRAGMA table_info(v1)`
  reports two columns `x`, `x:1` (uniquified duplicate).
- Oracle: one column `x`.
- Same for USING: `SELECT * FROM ta JOIN tb USING(x)` → oracle `[x, y, z]`,
  engine `[x, y, x:1, z]`.
- Consequences: phantom duplicate columns in view-based joins (`select_join.go:411`
  uses `ctx.ViewColumnDefs`), generated natural/using conditions including the
  empty `x:1 = v1.x:1` equality (NULL on both sides → no rows), `join5-10.1`.

### B. Unused-column optimization ignores named-view join sources
`disableUnusedSubqueryColumns`'s `markJoinSourceColumns` recognizes only
derived-subquery sources (`ref.Subquery == sub`). A **named view** body whose
outer statement NATURAL/USING-joins it gets no used-column marking; the join
condition generated later references the column, but the view body's column was
already rewritten to NULL.

- Engine: `SELECT count(*) FROM v1 NATURAL JOIN v1` (v1 = `SELECT * FROM t1`,
  2 rows) returns 0; `count(x)` and `SELECT *` return 2 / rows (reference marks
  the column used).
- Oracle: 2. Same shape with the view on either side of a base-table join.

## Fix plan

### Fix A (package internal/exec)
Add `viewStarSourceDefs(sel, resolving)`: recompute the FROM sources in order,
and per join drop the right side's first occurrence of every USING clause name
and every NATURAL-common name (exact-name matching, mirroring the executor's
`naturalJoinCommonCols`: skip Hidden columns; keep the left copy). Use it for
bare-`*` expansion in `viewStarDefs`; qualified `t.*` keeps the operand's own
columns (unchanged). `srcDefs` stays the flattened list for type resolution
(changing it would alter `viewColumnType` results).

### Fix B (package internal/execquery)
Thread `sourceNames []string` (the names this body is referenced under; the view
name for view bodies, nil for derived subqueries) through
`disableUnusedSubqueryColumns` → `markJoinSourceColumns`. A source matches when
`ref.Subquery == sub` (existing) or when `ref.Name`/`ref.As` equals a source
name (EqualFold). Over-matching only shrinks the optimization (safe); it must
never under-match.

## Verification
1. Native probe: `PRAGMA table_info` of the NATURAL and USING views matches the
   oracle; `count(*) FROM v1 NATURAL JOIN v1` = 2; `count(x)` = 2; join5-10.1
   SQL returns `[0]`.
2. `FRIGOLITE_TEST=join5 go test . -run '^TestSQLiteSuite$' -count=1` green.
3. Converter tests, `go test ./internal/... -count=1`, `git diff --check`.
4. Regression spot-check: related view/join families (`view`, `join1`, `using`,
   `selectA`, `distinct2`) vs run8 failing sets.

## Out of scope (recorded)
- `naturalJoinCommonCols` matches column names case-sensitively; SQLite matches
  case-insensitively. Fixing it globally is a separate change; this plan mirrors
  current executor behavior so view defs and execution stay consistent.
