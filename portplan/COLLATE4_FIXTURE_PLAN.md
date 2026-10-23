# collate4 custom collation fixture

## Evidence
- Exact isolated `FRIGOLITE_TEST=collate4 go test . -run '^TestSQLiteSuite$/^collate4$' -count=1` shows cascades beginning with `no such collation sequence: TEXT`.
- Source registers `TEXT` at `collate4.test:20-22` as bytewise Tcl string compare and `numeric` at `:558-562` as Tcl numeric comparison.
- Harness has a per-file collation fixture seam, but collate4 is not registered there.

## Work completed
1. Added per-file collate4 callbacks matching source TEXT lexical compare and numeric Tcl expr comparison.
2. Regenerated only collate4 from source; this also restores the missing DROP/order boundary before collate4-6.0.
3. `FRIGOLITE_TEST=collate4 go test . -run '^TestSQLiteSuite$/^collate4$' -count=1` passes; no expectation edits or skips.
