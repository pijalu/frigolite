# regexp1 source-fixture reconciliation

## Evidence and result
- Exact isolated baseline `FRIGOLITE_TEST=regexp1 go test . -run '^TestSQLiteSuite$/^regexp1$' -count=1` had 27 leaves from missing t1 plus duplicate table at 8.6.
- Source `regexp1.test:18-29` loads extension and creates/populates t1 in first do_test. Engine already registers REGEXP/REGEXPI (`internal/function/function.go`).
- Regenerated only regexp1 from source; table/setup/order cascades disappeared.
- Exact isolated rerun now has 6 actual regex compatibility failures (regexp1-2.10, 2.13, 2.14, 2.15, 2.20, 2.21, 2.22 listed in output; seven leaves). Remaining mismatch classes include regex controls `\a`, `\f`, `\v`, escaped dollar and Unicode/hex escapes. No engine edit made pending source contract trace.

## Next
1. Inspect pinned SQLite test extension implementation for regexp syntax, not assume Go RE2 equivalence.
2. Add util regexp test-first cases for supported controls/escapes, validate against SQLite oracle.
3. Implement SQLite-compatible translation preserving character-class and escaped metacharacter behavior; rerun regexp1 exactly.
