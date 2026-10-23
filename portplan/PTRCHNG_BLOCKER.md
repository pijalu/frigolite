# ptrchng compatibility blocker

## Evidence
- Exact isolated `FRIGOLITE_TEST=ptrchng go test . -run '^TestSQLiteSuite$/^ptrchng$' -count=1` fails 29 leaves, all `no such function: pointer_change`.
- `../sqlite/test/ptrchng.test:13-22` defines contract as pointer changes caused by `sqlite3_value_text/text16/blob/bytes/bytes16`; line 36 registers `sqlite3_create_function db`, implemented by C test harness (`test1.c`).
- `tools/tcl2go/skiptestfiles.go:641-646` already classifies `ptrchng` as N/A because it requires C-only Mem pointer representation/mutation; generated testgen package is an explicit N/A stub.
- Frigolite is pure Go/no CGO. Returning canned values for `pointer_change` would hard-code test assertions, not implement equivalent SQL-visible engine semantics; a Go value-conversion test cannot prove C pointer address stability.

## Decision needed
Strict full-suite objective forbids skips, but this test's contract is C allocator/pointer identity absent from pure-Go engine. Need user direction: approve existing N/A classification for ptrchng (including JSON harness exclusion), or broaden scope to emulate SQLite C Mem pointer semantics in Go with a concrete behavioral contract. Until then, no code change can satisfy both constraints honestly.
