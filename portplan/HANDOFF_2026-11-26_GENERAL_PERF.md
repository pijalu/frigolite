# Handoff 2026-11-26 — General engine optimization (goal kind.raven)

## Scope and guiding rule

User directive: general engine optimization only — no per-case hacks — and
minimize Go `interface{}` boxing/unboxing systemically. Recorded as binding
guidelines `plan/GUIDELINES.md §1j / §1j-a` (general-vs-per-case, boxing parity).

Three profile-backed units, all keyed to shared engine mechanisms (never to a
literal SQL shape, test, or family):

### Unit 1 — CREATE INDEX column-parse memo (query/planner side)

- `internal/exec/engine.go`: `tableCaches.idxColsMemo schemaMemo[[]string]`.
- `internal/exec/dml_forward.go`: `(*Engine).parseIndexColumnsCached(sqlStr)`
  uses the existing single-entry content-keyed `schemaMemo` (key `("", sqlStr)`;
  CREATE INDEX SQL is immutable, so no DDL invalidation; callers treat the
  slice read-only, as with the previous fresh parse).
- Callers migrated: `stat4.go:265`, `pragma_analyze.go` ×3, `select_context.go`
  (`ParseIndexColumns`, the `DatabaseContext` surface used by every
  execquery index-planner/EXPLAIN/skip-scan path).
- Profile evidence (`/tmp/phaseprof/indexed-select.mem.prof`):
  `internal/execdml.ParseIndexColumns` 100,032 alloc objects (17.98%) before,
  absent from the top after.

### Unit 2 — typed aggregate lane on the general table scan (boxing elimination)

- The unboxed `typedAggLane` already existed but only served covered rowid-range
  seeks (`rowid_range.go`). Wired it into the general scan:
  - `select_scan_row.go`: `scanState.typed/typedSerial/typedOffs`;
    `scanRow` dispatches to new `scanRowTyped` (parse header once into reusable
    buffers → `typed.stepDirect` straight from payload bytes; corrupt header
    falls back to the decoded path).
  - `select_scan.go:newScanState`: compile the lane only when `feed != nil`,
    no joins, no WHERE (`plan.whereExpr == nil`), and the grouped/non-typeable
    guards in `compileTypedAggLane` pass. Every other shape keeps the decoded
    feed. This is the same mechanism as the seek lane, not a query special case.
- Native pin written BEFORE the engine change: `frigolite_scan_agg_typed_test.go`
  `TestScanAggTypedFeed` (SUM/COUNT/AVG/TOTAL/MIN/MAX over int, REAL,
  numeric-text, NULL, ALTER default, rowid-alias; expected values verified
  against `/usr/bin/sqlite3` 3.54).
- Profile evidence (`/tmp/scanprof/scan.mem.prof`): before — 1,998,862 alloc
  objects, `decodeBigEndianInt` 1,625,621 (81.33%), `decodeValue` 361,701
  (18.10%); after — 11,404 objects, no decode/boxing in the top (statement-
  level compile only). The typed lane is definitively serving the rows.
- Discovered but NOT folded into this unit: SQLite's `sqlite3_value_numeric_type`
  coerces BLOB to numeric in SUM (`sum(x'31')` = 1.0); Frigolite's
  `sumNumericArg` leaves BLOB as 0.0. Separate correctness gap, recorded here
  and excluded from the pin fixture; needs its own unit.

### Unit 3 — DML index-def cache + precomputed key collations (indexed UPDATE)

- `internal/execdml/seek.go:seekIndexFor`: `indexDefsIn` → `indexDefsInCached`
  (existing fingerprint-guarded per-(db,table) cache). Only external caller.
- `indexDef` gained `KeyColls []string`, parsed once from immutable CREATE
  INDEX text in both constructors (`plainIndexDef`, `indexDefForEntry`).
  `dmlIndexProbeEligible` and `dmlIndexSeekProbes` read it instead of calling
  `seekKeyCollation` (removed) which re-split/re-parsed the key list per probe.
  Exact BINARY spelling preserved (`strings.ToUpper`).
- Profile evidence (`/tmp/phaseprof/indexed-update.mem.prof`): 504,752 → 374,374
  alloc objects (-26%); `parseIndexKeyCollations`/`parseIndexKeyCols` gone from
  top; `splitIndexCols` 40,004 → 15,003. Phase under profiling 4,119 → 5,959
  ops/s.

## Measurements (paired, interleaved, reps=3, 8 pairs)

Frozen baseline worktree `/tmp/frigo-gapbase` at `b788d1515` (pre-work HEAD),
binary `/tmp/gapbase/gapbase`; optimized `/tmp/perfprof/gapopt` (built from
`main.go` only — `fileshape.go` also declares `main`). Flags `-index
-point-ops 200`. Final table (median ops/s, paired min–max ratio):

| Phase | baseline | optimized | ratio | paired min–max |
|---|---:|---:|---:|---|
| `scan` | 19,341,660 | 37,457,458 | **1.937×** | 1.914–1.963 |
| `indexed-update` | 129,200 | 152,696 | **1.182×** | 1.033–1.254 |
| `indexed-select` | 171,816 | 196,668 | **1.145×** | 0.874–1.296 |
| `indexed-delete` | 141,278 | 170,894 | 1.210× | 1.139–1.297 |
| others | — | — | ~1.0 | straddle 1.0 |

`indexed-select`'s per-pair range straddles 1.0 (single-run GC noise at 200
point-ops); its causal evidence is the profile (17.98% alloc removed), like the
prior index-comparator unit. `scan` and `indexed-update` are unambiguous.

## Gates (run by the completing session)

- `go test -count=1 ./internal/exec/... ./internal/execquery/...
  ./internal/execdml/... ./internal/btree/... ./internal/storage/...` PASS.
- Native pins `TestScanAggTypedFeed|TestAggFeed.*|TestGroupByTypedKeyEquality`
  PASS.
- Full 12-shard harness: `bash tools/run_json_shards.sh /tmp/raven_testbin 12`
  → run_dir logged in `/tmp/raven_shards.out`; leaf/family FAIL sets must be
  NEW=0 vs `/tmp/t30_full_gate.log` and
  `/tmp/t30_baseline_leaves_clean.txt`.
- `go test -run TestSOLID_ ./...` and cross-engine matrix (verify command).

## Commit plan

1. `perf(query): memoize CREATE INDEX column parse on immutable SQL` (Unit 1).
2. `perf(scan): feed simple aggregates unboxed from record bytes` (Unit 2 +
   pin + §1j guidelines).
3. `perf(dml): cache index defs and precompute key collations for seeks` (Unit 3).
4. docs: TODO.md, `.agents/lessons_learned.md`, this handoff.
