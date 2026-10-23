# Function-family full-suite triage

## Evidence
- Fresh retained full profile `/tmp/frigolite-current-profile.jsonl`: `func` family has 64 failed leaves, one of largest root-cause families.
- Isolated `FRIGOLITE_TEST=func go test . -run '^TestSQLiteSuite$' -count=1` failed in 14.68s; selector also includes func2/func4/... due substring matching.
- Failures cluster into fixture-contract gaps (`legacy_count`, `test_isolation`, `test_frombind`, `testdirectonly`, `counter1`, `sqlite_offset`/`offrec`, plus func8 bare identifiers miscaptured as function calls) and engine semantics/formatting (`char()`, SUBSTR empty-string normalization, toreal rendering, JSON arrows, concat_ws NULL).
- SQLite TCL source `../sqlite/test/func.test` registers multiple test-only functions through C test extensions; generic SQL-only fixture cannot reproduce them without explicit harness behavior.
- Fixed `func-20` conversion and engine behavior: source uses loop-bound `$name` inside braced `execsql`; regenerated `testdata/func.json` now quotes each name as a Tcl-bound SQL literal. Exact source/oracle `soundex` optional function was also absent; implemented SQLite `func.c:1762-1816` algorithm and registered SOUNDEX. Full `func-20` subtests pass; direct function vector tests pass. Latest full profile `/tmp/frigolite-after-soundex.jsonl` reduces func leaves 62→46. Remaining func family gaps include C test-only UDFs and independent engine contracts.
- Fixed `func-4.39`: converter captures `string tolower` as `resultTransform: tolower` on the query step; harness applies transform to actual flattened output. Focused converter tests and exact fixture test pass. `func-38.100` remains separate.

## Ordered work plan
1. Read source for failed leaf names and classify whether each is a test-only C UDF, unsupported API, or engine-visible built-in SQL behavior. Preserve full output `/var/folders/gw/p4cdspjx49x5113yh4hy322m0000gn/T/goa-output-1110858670.txt`.
2. Select one coherent fixture-backed group (e.g. `counter1`/custom funcs); trace registration from TCL/C test source and add faithful harness registration before implementing. Do not silently mock expected output.
3. For built-in behavior, write native tests first and use SQLite oracle for exact result/error semantics; fix one behavior at a time.
4. Run focused native/converter and `FRIGOLITE_TEST=func...` checks, then retain current failure count.
5. Continue next highest-impact profile family only after current slice is verified.
