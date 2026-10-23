# `full_column_names` star expansion repair plan

## Reproduction/source contract
- Latest full run `/tmp/frigolite-full-after-fts5joinfix.log` exits 1 (root 367.457s, suite 155.52s). First non-random harness family failure: `colname-4.1` onward.
- `FRIGOLITE_TEST=colname go test . -run '^TestSQLiteSuite$' -count=1` reproduces. Source `../sqlite/test/colname.test:174-188` sets `short_column_names=OFF; full_column_names=ON`, then expects `SELECT * FROM tabc` result names `tabc.a`, `tabc.b`, `tabc.c`. Engine currently returns `a,b,c`.
- `resolveColumnRefName` honors FullColumnNames (`internal/execquery/select_columns.go:356+`), but `starColumnNamesFor` calls `expandStarColNames(colDefs)` which strips prefixes and has no SELECT operand context (`select_colnames.go:264-272,378-391`). This bypasses pragma for bare-star outputs.

## Plan before edits
1. Add native test for PRAGMA full_column_names on a base-table bare star and qualified star; ensure short/full precedence and default unaffected.
2. Implement star-name generation using current SELECT operand ownership/name from `sel`, preserving explicit alias and schema/source casing behavior; for views, SQLite's full names use view name (`colname 4.8+`). Avoid changing row values or output definitions.
3. For full names, use schema-resolved source identity per star column; display explicit alias when present, else canonical schema table name. Qualified stars use alias as output qualifier while resolving columns against underlying source. Add mixed-case table and alias assertions plus multi-table collision coverage.
4. Verify native test, exact colname harness family and sibling cases 4.1–4.13, join/select tests, attach2, `git diff --check`.

## Implemented / verified
- `fullNamesForBareStar` now emits per-source qualifiers, uses explicit aliases and canonical `schema.Entry.Name` for unaliased tables; qualified stars report aliases rather than base table names. Existing `columnOwnerOperand` supports explicit qualified references.
- Native regression exercises bare/qualified stars, mixed casing, aliases/views, multiple sources and explicit refs. `TestFullColumnNamesExpandsBareAndQualifiedStars` passes.
- `FRIGOLITE_TEST=colname go test . -run '^TestSQLiteSuite$' -count=1`: PASS.
- `FRIGOLITE_TEST=attach2 go test . -run '^TestSQLiteSuite$' -count=1`: PASS.
- `go test . -run 'TestJoinH|TestFullColumnNamesExpandsBareAndQualifiedStars' -count=1`: PASS. `git diff --check`: PASS.
- `go test ./...` again exceeded the hard 300-second shell-tool timeout; broad suite remains unverified, limitation is tool runtime cap.
