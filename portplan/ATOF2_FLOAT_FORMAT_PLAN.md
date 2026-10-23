# atof2 floating format repair plan

## Evidence
- Final `go test -json ./... -count=1` after decoder refinements: exit 1, 380s; raw JSON, stderr, exit status, and elapsed seconds retained under `portplan/FULL_SUITE_FINAL_ATOF2.*`; rendered stdout omitted as redundant generated output. Earliest remaining test: `TestSQLiteSuite/attach/attach-1.3`, failing `no such table: two.t2` after ATTACH.
- Decoder validation: `go test ./internal/function -count=1`, native `TestAtof2AltTwoFormatsShortestRoundTrip`, and `FRIGOLITE_TEST=atof2 go test . -run '^TestSQLiteSuite$' -count=1` pass.
- Repaired family: `atof2`, tests 2.1/2.2. The earlier output exposed exact binary-decimal expansion instead of SQLite's shorter rounded value. Current `atof2` family passes after decoder port. Next earliest issue is `attach-1.3` in the retained full-suite run.

## Ordered steps
1. Reproduce exact family; inspect `../sqlite/test/atof2.test`, SQLite `printf.c` alternate-form-2 conversion, formatter implementation, and ieee754_inc implementation.
2. Add a native pure-Go regression for `%!.30f` on exact IEEE values and test nearby formats to preserve `!` semantics.
3. Port SQLite's source algorithm narrowly: its Dekker double-double scaling then integer extraction yields at most 19 digits from uint64 before significant-digit clipping; Go's fixed formatter exposes binary expansion instead. Keep source precision and rounding semantics, avoid hard-coded strings or modifying fixture expectations.
4. Run native and exact family checks; then rerun full `go test -json ./... -count=1`, retain raw JSON/text/stderr/status/elapsed, and proceed to next current failure.

## Risks
- `%!` alternate-form-2 renders shortest round-trippable decimal, not exact binary expansion; do not perturb ordinary `%f` precision behavior.
