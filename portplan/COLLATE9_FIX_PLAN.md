# collate9 full-suite blocker repair plan

## Reproduction / source contract
- Full `go test . -run '^TestSQLiteSuite$' -count=1` fails first in collate9-1.1: missing `reverse sort` collation; later missing-table and REINDEX/integrity errors cascade.
- After collate9 repair, next family collateA isolated failure was stale `collateA-3.10` fixture SQL: it replayed CREATE TABLE/INSERT rather than source's one SELECT. Fresh converter output restores the source query; replacing only `testdata/collateA.json` made `FRIGOLITE_TEST=collateA go test . -run '^TestSQLiteSuite$' -count=1` pass.
- Isolated `FRIGOLITE_TEST=collate9 go test . -run '^TestSQLiteSuite$' -count=1` reproduces.
- `../sqlite/test/collate9.test:21-24` defines `reverse_sort` (`string compare $rhs $lhs`) and runs `db collate "reverse sort" reverse_sort` before use.
- Current generated `testdata/collate9.json` omits the registration marker. This is converter lifecycle loss, not engine sorting diagnosis.

## Planned slice
1. Add Tcl converter marker for `db collate NAME PROC`, preserving proc identity/body at source point.
2. Add fixture harness interpretation of comparator body for source-supported `string compare` order and reversed arguments; no guessed fallback for unknown comparator.
3. Add converter and native comparator marker regressions, regenerate only collate9 fixture from `../sqlite/test/collate9.test`.
4. Run focused tests and isolated collate9; document any remaining independent assertion failures.
5. Retain root full-suite evidence; do not alter attach2/UTF-8 capability behavior or unrelated workspace edits.
