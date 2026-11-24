# PLAN — S46 slice 1: misc3-6.11-utf8 (2026-11-24)

## State

`misc3-6.11-utf8` is the last S41 residual named in TODO. Its do_test
body captures `EXPLAIN SELECT ... ORDER BY +a, b DESC`, then builds a list
of four Tcl `regexp` probes over the captured text. The installed fixture
has one `query_assign` with `expect: "1 1 1 1"`, so the harness compares
the whole flattened EXPLAIN row stream against the list of booleans.

## C/Tcl contract

`misc3.test:289-297` computes:
```
set y [regexp { 123456789012 } $x]
lappend y [regexp { 4.5678 } $x]
lappend y [regexp { hello } $x]
lappend y [regexp {,-B} $x]
```
The body's value is the four regexp results. The EXPLAIN row stream is
only an input to those probes; none of its own values are asserted.

## Approach

- Converter: when a do_test body ends in `lappend` and contains `regexp`
  probes over a runtime variable, emit one `runtime_expr` step:
  `list [regexp {pattern1} $x] [regexp {pattern2} $x] ...` with the
  do_test expectation, instead of stamping it on the query_assign.
- Harness: add `regexp` to the runtime command head words and dispatch it
  with Go's `regexp` package (the four probe patterns are literal text).
- Engine EXPLAIN fidelity (the fixture's regexps observe real VDBE detail):
  sorter keyinfo renders descending terms as `-B` (`k(2,B,-B)`); result
  expressions emit `Int64`/`Real` constant-load rows; their `Column` rows
  carry the schema column's default value in p4 (`54321`, `hello`,
  `3.1415926`). Keep emission limited to literal defaults and large
  integer/float constants so existing opcode expectations are not perturbed.
- The fixture's `set x [execsql {...}]` capture is a FLAT list of EVERY
  row's columns, not the first row: the step must be `resultFlat`, else
  the regexps see only row 0 (no p4 text at all).
- Column defaults resolve through the SELECT's FROM tables (alias-aware),
  not by scanning all schemas for the first same-named column: an unrelated
  `t1(b)` shadowed `ex1.b` and returned an empty p4. All attached schemas
  must be searched in the fallback because a fixture table may live in
  temp, not main.
- Guard: a result `Column` row is suppressed when the column is GENERATED
  and the chosen scan index already yields it (`gencol1-23.3`: `a` is
  `x2`'s key, so C emits no `Column 0`; `b` is NOT in `x2`, so the row must
  stay). Without the guard the new result-row pass turns a passing leaf
  into a regression.

## Verification

- Converter unit test pins runtime_expr shape; pure-Go pin test checks the
  four EXPLAIN substrings directly.
- `FRIGOLITE_TEST=misc3` 0 FAIL; `go test ./internal/...` green;
  focused EXPLAIN-sensitive sweeps + full-suite failure-name-set gate;
  commit with TODO/lessons/handoff updates.
