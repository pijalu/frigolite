# perfbench — frigolite vs SQLite per-operation benchmark

Reproduces the numbers in `benchmarks/R11_RESEARCH.md`,
`benchmarks/R13_RESEARCH.md` and the `PERF.PARITY*` ledger entries. The two
programs run the same op shapes with the same flags, in the same literal mode:
one freshly built SQL string per operation, one `Exec`/`Query` per statement
(the only mode the public `Open/Exec/Query` API allows — no prepare/bind).

```sh
cd frigolite && go run . -reps 3            # canonical rowid CRUD shapes
cd frigolite && go run . -reps 1 -index -point-ops 200   # + secondary-index shapes
cd sqlite    && go run . -reps 3            # same shapes, system libsqlite3 (CGo)
cd sqlite    && go run . -reps 1 -index -point-ops 200
```

Flags (identical on both sides): `-rows` (default 50000), `-point-ops`
(20000), `-file-ops` (5000), `-scan-passes` (20), `-reps` (3, median
reported), `-index` (add `CREATE INDEX i1 ON t(b)` and the index-driven
SELECT/UPDATE/DELETE phases). Phase names are identical in both outputs, so
`grep ops/s` on the two runs pairs up directly.

The `sqlite` side links the **system** `libsqlite3` via CGo (`-lsqlite3`; on
macOS the SDK's 3.54 build, on Linux install `libsqlite3-dev`). Like
`tools/compare-benchmark`, it is a separate module so the engine itself stays
pure Go.

Reading the output: `insert`/`point-select`/`point-update`/`point-delete`/
`file-insert` are the ledger shapes; `scan`/`count-star`/`group-by` are
throughput (rows or passes per second, not statements); the `-index` phases
are the shapes documented in `R13_RESEARCH.md` §1 — they are 1000–5000x off
parity today and are the reason this harness is committed.
