# alter.test runtime result propagation (`alter-6.2`)

## Reproduction and evidence
- Exact command: `FRIGOLITE_TEST=alter go test . -run '^TestSQLiteSuite$/^alter$' -count=1`; retained output `/tmp/alter-nameddb.log`. It fails first at `alter-6.2`: `SELECT sql FROM sqlite_master WHERE oid = ;` syntax error, followed by incomplete SQL in 6.3–6.5.
- Source `../sqlite/test/alter.test:595-603`: inside one `do_test`, it creates a table, assigns `set ::oid [execsql {SELECT max(oid) FROM sqlite_master}]`, then interpolates `$::oid` into another SQL statement. The fixture has the CREATE, a standalone query step `SELECT max(oid) FROM sqlite_master`, and a third SQL statement with the missing value.
- Thus the engine executes the max(oid) query, but converted fixture discards its runtime result-to-Tcl-variable assignment. This is converter/harness dataflow, not an ALTER defect. Similar post-query SQL result reads may exist; scope implementation to a traceable typed assignment rather than infer from adjacent query output.

## Ordered repair plan
1. Inspect `do_test` command capture, result storage/rendering, and harness step schema to identify a typed representation for assigning a query result to a Tcl global, preserving step order and connection.
2. Add a RED converter regression for `set ::oid [execsql {SELECT max(oid) FROM sqlite_master}]` between statements; assert the emitted representation explicitly expresses result assignment, not a redundant query.
3. Implement the smallest complete converter/harness result-assignment contract (including empty/null/scalar result behavior based on Tcl `execsql` semantics); add direct tests for converter output and runtime assignment.
4. Regenerate only `alter.json`; verify 6.2–6.6 and exact alter family. Then source-audit 9.x and 11.x independently; do not conflate failures.
5. Record validated durable lesson and run converter suite plus exact family.

## Constraints
- Preserve all existing workspace edits and fixture coverage; no skips or expected-output weakening.
- Do not touch engine ALTER code unless a pure-Go engine test isolates a source-proven engine discrepancy.
