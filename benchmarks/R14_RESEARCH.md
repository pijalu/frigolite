# R14 — SELECT / INSERT / UPDATE performance vs SQLite

Status: **research complete; 2 of 5 planned optimizations prototyped and validated in-tree**
Focus: `SELECT` (with/without index), `INSERT` (with/without index), `UPDATE` (with/without index).

All frigolite numbers from `go run ./cmd/perfbench -index -reps 3` (frozen `57513ad6`), the
shape-matched harness `benchmarks/perfbench/frigolite/main.go`, and the frozen scratch build
`/tmp/perfap.bin` (`R14PROTO=1` = prototype). SQLite 3.54, CGo harness
`benchmarks/perfbench/sqlite/main.go`. All in `:memory:`, table `t(a INTEGER PRIMARY KEY,
b UNIQUE, c)`, 50k rows, literal SQL per statement (no bound parameters).

---

## 1. Headline: current parity state

| Phase | frigolite ops/s | sqlite ops/s | gap |
|---|---|---|---|
| `INSERT` (50k, 1 index + table) | 287,683 | 980,073 | **3.41× slower** |
| — `INSERT` **without** index | 1,064,600 | — | (maint = 2.4 µs of 3.5) |
| `SELECT c WHERE a=?` (rowid probe) | 1.1 µs/op | 1.2 µs/op | **faster** |
| `SELECT a WHERE b=?` (index probe) | 186,024 | 736,601 | **3.96× slower** |
| `UPDATE … WHERE a=?` (rowid) | 550,247 | 892,801 | 1.62× slower |
| `UPDATE … WHERE b=?` (index) | 168,208* | 658,845 | 3.92× slower* |
| `DELETE … WHERE b=?` (index) | 298,124 | 934,962 | 3.14× slower |

\* the `indexed-update` perfbench number is an **artifact of phase order** — see §3. It understates
the real indexed-UPDATE gap by ~700×.

Table doesn't include `SUM(b)` (3.0×), `COUNT(*)` (8.7×), `GROUP BY` (3.0×), file
autocommit `INSERT` (frigolite 2.2× faster): R13 territory, not repeated here.

**Shape-matched, honest comparison** (identical statement text against both engines):

| shape | frigolite | sqlite | gap |
|---|---|---|---|
| `SELECT c FROM t WHERE a=?` | 1.10 µs (14.1 allocs, 292 B) | 1.20 µs | **1.10× — at parity/faster** |
| `SELECT a FROM t WHERE b=?` | 5.4 µs (149 allocs, 4.8 KB) | 1.13 µs | 4.8× |
| `INSERT` with index | 3.35 µs (197 allocs, 5.15 KB) | 0.85 µs | 3.9× |
| `INSERT` no index | 0.94 µs (12.3 allocs, 302 B) | — | index maint = 2.4 µs + 185 allocs |
| `UPDATE WHERE a=?` same-size SET | 1.78 µs (13.9 allocs) | 1.12 µs | 1.6× |
| `UPDATE WHERE a=?` size-changing SET | 4.56 µs (36.6 allocs) | 1.33 µs | 3.4× |
| `UPDATE WHERE b=?` **fresh table** | 3,583–4,189 µs/op (24.2k allocs, **14.4 MB/op**) | 1.33 µs | **2,700–2,850×** |
| `DELETE WHERE b=?` | 7.79 µs (255 allocs) | 1.07 µs | 7.3× |

Conclusion: **rowid-path DML/SELECT is at parity; every secondary-index path is 3–5× slower;
secondary-index UPDATE is 2,700× slower.**

---

## 2. Validation method (all prototype results below are evidence-backed)

Each lever was prototyped in a frozen scratch copy and measured against the frozen base with the
*identical* harness invocation, alternating base/proto in the same shell. Every prototype was then
checked for behavior identity:

- `go test ./...` at module root (all packages) — **no failures** for both prototypes.
- `go test ./...` for `internal/exec`, `internal/execdml`, `internal/execquery`, `internal/btree` — **ok**.
- Full JSON harness on `index`, `where`, `delete`, `update`, `select`, `dml`, `analyze`,
  `misc` — **identical failing-leaf sets** (`diff` on leaf basenames, base vs proto; the
  failing families are the pre-existing excluded `bestindex*` ones).
- Byte-identical on-disk databases: base vs proto produced **the same 34,549,120-byte file**
  after 5,000 inserts + 4 indexes + 30,000 update/delete/insert churn over integer, text,
  and DESC-real indexes (`cmp -s` exit 0; sha256 identical).

---

## 3. The indexed-UPDATE bug (highest impact by far)

### 3.1 Symptom

`UPDATE t SET c=? WHERE b=?` on a fresh 50k-row table runs at **239 ops/s** — 2,850× slower than
sqlite (681,253 ops/s). Allocating **14.4 MB and 24,222 objects per statement**. The same
statement on the rowid path (`WHERE a=?`) is 550k ops/s.

### 3.2 It is not the plan — the collect is already seek-driven

Probe instrumentation (`applyDMLUpdateProbe`, `seekUpdateChangesProbe`, `applyInPlaceUpdatesProbe`,
`DeleteCellsWhereProbe`; 3,000 statements, frozen build):

| scenario | planRowid | planIndex | seekCollect | inPlaceAll | **bulkSweep** |
|---|---|---|---|---|---|
| fresh table | 0 | 3,000 | 3,000 | 378 | **2,622** |
| after a `WHERE a=?` pre-phase | 0 | 3,000 | 3,000 | 3,000 | **0** |

`internal/execdml/update.go:554-560` already calls `applyPointUpdate` (rowid-pinned, declines
`plan.index != nil` at `update_point.go:117`), then `applyUpdateChanges` for the rest.
`applyUpdateChanges` → `applyInPlaceUpdates` (`update.go:667-690`) is called first, but
`update.go:553` **bails out to the whole-table bulk sweep whenever it cannot cover every row
in place**:

```go
if len(inPlace) != len(toUpdate) {           // update.go:553
    if wrEntry == nil {
        return runPlainUpdate(tbl, toUpdate, columns, values...)   // bulk sweep
    }
    ...
}
```

`runPlainUpdate` → `applyUpdateChanges` → `DeleteCellsWhere` → `deletePass` → `sweepLeafCells`
decodes, re-encodes and rewrites **every cell of every leaf page of the table** per statement
(CPU profile: `storage.DecodeRecord` 33.4 %, `sweepLeafCells` 14.5 %, `appendRecordFromValues`
10.2 %).

### 3.3 Why the perfbench number is an artifact

`WHERE a=?` point-updates pre-normalize `c` to `'v<i>'` — which is *always the same length* as the
`'u<i>'` that `indexed-update` then writes. With uniform cell sizes every UPDATE is handled by
`applyInPlaceUpdates` (in-place drop+insert on one page) and never reaches the sweep — which is why
`internal/bulk/idx_update_test.go` measured **357k ops/s** while my shape-matched fresh-table
harness measured 277. On a fresh table, where lengths are not pre-normalized, the in-place
predicate fails and the sweep runs for ~87–99.8 % of statements. The probe table above is the proof:
same statements, `bulkSweep` 0 vs 2,622.

### 3.4 The sqlite approach

sqlite always writes a *changed* row through a positioned cursor: `sqlite3BtreeMovetoUnpacked` finds
the index entry, `sqlite3BtreeDelete` + `sqlite3BtreeInsert` touch exactly the pages on that
descendant path, regardless of whether the record size changed. There is no "cannot size-change
⇒ rewrite the whole table" branch. `sqlite3VdbeExec` uses `OP_Delete` + `OP_IdxInsert` per
changed column; `sqlite3VdbeCompare`/`sqlite3VdbeIdxDelete` handle the old-index-entry removal by
seek.

### 3.5 The fix

Route the non-in-place remainder through the point-write primitives instead of the bulk sweep.
Specifically, in `applyUpdateChanges` (`update.go:549-568`), replace the
`len(inPlace) != len(toUpdate)` fallback with a per-row
`btree.RecordVariantReplace`-style path (check all old index keys first, then delete+insert the
base-table cell and each changed index entry), so a size-changing indexed UPDATE
costs one seek plus one page-descendant ripple instead of a whole-table rewrite.

Measured target: the rowid path (`WHERE a=?`, same-size SET, 1.78 µs) plus index
maintenance is ~2.6 µs/op — versus the 3,583–4,189 µs/op today. This single change
is worth ~1,300× on this lane and removes ~24,222 allocations/statement.

---

## 4. INSERT — index maintenance costs 3.7× the table write

### 4.1 Where the time goes

Without the index, `INSERT` is **0.94 µs (12.3 allocs, 302 B/op)**. With one
`CREATE UNIQUE INDEX ti ON t(b)`, the same statement is **3.35 µs (197 allocs, 5,151 B/op)**.
So index maintenance is 2.4 µs of the 3.5 µs and 185 of the 197 allocations.

CPU profile of the insert-with-index lane (loop only, 1.2 s of samples):

| frame | cum |
|---|---|
| `execdml.execInsertTuples` | 37.1 % |
| `execdml.maintainIndexesOnInsert` (`insert_constraints.go:308` `writeIndexCell`) | 31.5 % |
| `btree.InsertCell` | 26.8 % |
| `btree.selectLeafPageDescriptor` | 14.9 % |
| `btree.findChildIndexForInsert` | 14.0 % |
| `btree.RecordPayloadCompare` (`btree_keyinfo.go:124`) | 11.2 % |
| `storage.DecodeRecord` | 3.5 % |

Allocation profile of the same lane (197 allocs/op):

| allocator | objects | bytes |
|---|---|---|
| `storage.DecodeRecord` (100 % reached from `btree.RecordPayloadCompare`) | **62.8 %** | 30.4 % |
| `storage.DecodeCell` | 10.0 % | 20.0 % |
| `storage.decodeValue` | 20.4 % | 11.3 % |
| `storage.appendRecordFromValues` | — | 14.1 % |
| `execdml.parseIndexColumns` | 2.6 % (15 % of bytes) | — |
| `execdml.IndexKeyCollations` / `parseIndexKeyCollations` | 3.0 % | — |
| `execdml.buildRowMapFromValues` | 5.6 % | 11.1 % |
| `btree.NewKeyInfo` | 1.5 % | — |

**The single biggest INSERT cost is `RecordPayloadCompare`, which decodes *both* stored
records into boxed `*storage.Record` values (`[]any` + parallel serial-type slice) on every
comparison.** It is called from `SetIndexKeyInfo`'s comparator during the index-insert
position walk (`insertInteriorPage`/`insertPage`), so a single `INSERT` pays it once per
level of the index descent plus once per comparison inside the leaf split. SQLite's
`sqlite3VdbeRecordCompare` compares serial types directly out of the encoded payload with a
scratch `UnpackedRecord` and no allocation.

### 4.2 Prototype: decode-free index-record comparison — **+25–31 %**

Prototyped `btree.recordPayloadCompareNoDecode` (scratch tree, `R14PROTO=1`) implementing
sqlite's algorithm: walk both header serial-type streams, compare only the fields present in
the `KeyInfo` field list, inline-compare the common classes (NULL, int via
flipped-sign ordering, text via the engine's `collatedTextCompare` length-difference
semantics, blob via `bytes.Compare`), and **fall back to the existing decode path** for
anything else (REAL keys, non-BINARY collations, numeric-affinity indexes).

| lane | base | prototype | Δ |
|---|---|---|---|
| insert (with index) | 286,603 ops/s | 362,795–363,603 ops/s | **+26.9 %** |
| | 5,355 µs/op → 5,430 µs/op | | |
| allocations | 199.0 /op (5,151 B) | **71.5 /op (2,828 B)** | **−64 % / −45 %** |
| insert (no index) | 1,064,600 ops/s | 1,126,362 ops/s | +5.8 % |
| indexed-select | 186,024 ops/s | 179,434 ops/s | −3.5 % (noise) |
| update `WHERE a=?` | 550,247 ops/s | 606,196 ops/s | +10 % |

Full measured A/B (same shell, alternating base/proto):

```
insert      base 276,991 → proto 346,350–363,603   (+25.0–31.3 %)
alloc/op    base 199.0 → proto  71.5               (−64 %)
bytes/op    base 5,151 → proto 2,828               (−45 %)
```

### 4.3 Remaining INSERT cost after the prototype

Allocation profile with the comparator fixed (71.5 allocs/op) now shows:

| allocator | objects |
|---|---|
| `storage.DecodeCell` | 24.0 % |
| `execdml.buildRowMapFromValues` | 15.7 % |
| `execdml.IndexKeyCollations` (cum 21.7 %) | — |
| `execdml.parseIndexColumns` (cum `parseIndexKeyCols` 9.5 %) | — |
| `storage.appendRecordFromValues` | 3.8 % |
| `storage.DecodeRecord` | 0 % (eliminated) |

So the next INSERT levers are the **per-statement index-definition text re-parse
(`IndexKeyCollations` → `parseIndexKeyCols` → `splitIndexCols`, ~10 % of allocations) and
`KeyInfo` construction (~30 % cum)**, and **`buildRowMapFromValues` (16 % of objects,
11 % of bytes)** — a per-row map built for column-name resolution that a slice-based
lookup would replace.

---

## 5. Indexed SELECT — ~40 % of the time is re-planning the same statement

### 5.1 Cost split

| variant | ops/s | µs/op | allocs/op | B/op |
|---|---|---|---|---|
| `SELECT c WHERE a=?` (rowid) | 1,064,600 | 1.10 | 14.1 | 292 |
| `SELECT a WHERE b=?` (index) | 186,024 | 5.4 | 149 | 4,800 |
| delta (index path) | — | **4.3** | **135** | **4,508** |

The actual index descent is a small part of it. CPU profile of the loop-only
`SELECT a FROM t WHERE b=?` lane (1.8 s of samples):

| frame | cum |
|---|---|
| `bxi.Run` / `execQuery` | 47.4 / 45.8 % |
| `execquery.selectIndexSeekPlanFor` (`select_index_seek_plan.go:152`) | **12.7 %** |
| `execquery.openIndexSeekRun` (`select_index_seek_exec.go:115`) | 6.1 % |
| `execdml.IndexKeyInfo` → `parseIndexColumns` (inside the above) | 5.0 % |
| `execquery.bestIndexForRowCount` | 5.0 % |
| `btree.SeekIndexKey` — **the actual seek** | 5.0 % |
| `storage.DecodeRecord` | 2.7 % |

So ~30 % of the CPU is spent choosing the plan and building the `KeyInfo`, and only 5 % is
spent descending the index. Allocation profile of the same lane (149 allocs/op):

| allocator | % of objects |
|---|---|
| `execquery.collectBinaryRef` / `seekableRefs` / `refsAt` / `bareRefsSeekOutput` (WHERE-shape analysis) | **~30 %** |
| `execdml.parseIndexColumns`, `splitIndexCols`, `IndexKeyCollations` (index SQL re-parse) | **~15 %** |
| `exec.formatConditions` (the EQP string, discarded — see §5.3) | **~10 %** |
| `storage.DecodeRecord` / `DecodeCell` / `decodeValue` | ~20 % |
| `btree.NewKeyInfo` | 2.3 % |
| `btree.NewBTree` | 3.0 % |

### 5.2 Root cause

Three categories of work are recomputed per statement although they are identical for every
statement of the same shape:

1. **Index-definition text re-parsing.** `select_index_seek_exec.go:115` calls
   `execdml.IndexKeyInfo(table, indexName)`, which calls `IndexKeyCollations` →
   `parseIndexKeyCollations` → `splitIndexCols` → `parseIndexColumns` — i.e. it re-parses the
   `CREATE INDEX` SQL text on *every* SELECT. sqlite reads the index column list once from
   `sqlite_schema` into the in-memory `Index` object.
2. **WHERE-shape analysis.** `seekableRefs` / `collectBinaryRef` / `collectIndexedRefs` /
   `constrainedColOps` / ` constrainedColumnNames` are closures over the WHERE AST run per
   statement; they account for ~30 % of allocations.
3. **The EQP condition string is built and thrown away.** `bestIndexForRowCount`
   (`select_index_seek.go:1226`) unconditionally calls `formatConditions(bestRefs)`
   (`explain.go:723`) — a `strings.Builder` + `fmt.Sprintf` + `errors.New` chain — and
   `whereScanLoop` returns it in `ret.conditions`, which is consumed **only** by the EXPLAIN
   renderer. `scanLoopForRowCount` (`select_index_seek.go:71-73`) discards it immediately.

Note also that `exec/index.go` builds a `splitIndexCols` scratch buffer and then re-parses with
a second loop (`exec/index.go:992-1037`).

### 5.3 Prototypes attempted

**(a) Memoize `parseIndexColumns`** (scratch tree, keyed on the raw column text).

```
indexed-select   185,355 → 213,329 ops/s   (+15 % all measurements)
indexed-select   186,024 → 221,642 ops/s   (+19 %)
```

Returning a slice from a memo forces a copy to be safe against mutation; the honest
no-copy A/B on the canonical 50k-row table is **+3.7 %** (186,024 → ~193,000). The copy tax
eats most of the win, so this lever is cheap-but-small on its own.

**(b) Skip `formatConditions` on the non-EXPLAIN path** (prototype: `bestIndexForRowCountNoCond`,
routed through `whereScanLoop`).

```
indexed-select   194,306 → 233,505–235,879 ops/s   (+20–21 %)
µs/op            5.15    → 4.26
```

**But the naive form of this patch breaks `EXPLAIN QUERY PLAN`**: my first attempt made
`whereScanLoop` return `conditions = ""` in all cases, and the EQP output became
`"SCAN t USING COVERING INDEX i1"` instead of
`"SCAN t USING COVERING INDEX i1 (b=?)"`. Row results were unaffected. The correct fix keeps
`formatConditions` on the explain path — gate it on an explain-mode flag
(`(e *planUseContext).explain_mode`, the field R13-L7 added to `brain.go`, already used by
`vtab_phase.go`), so `selectIndexSeekPlanForStats` needs the string and the hot path does not.

### 5.4 The rest

`btree.NewBTree` (3 % of allocations) allocates a wrapper per index per statement; a
`BTree` handle cache keyed on (btree, fingerprint) alongside the R13-L1 `Cursor.Region` reuse
would remove it. And `storage.DecodeRecord`/`DecodeCell`/`decodeValue` (~20 %) is the same
decode-boxing cost §4.2 removed on the write side, still present on the read side.

---

## 6. Validated prototype evidence

### 6.1 Decode-free index comparator (landed in scratch, validated)

`recordPayloadCompareNoDecode` implements sqlite's `sqlite3VdbeRecordCompare` algorithm and
falls back to the existing decoded comparator on any case it does not cover exactly
(REAL keys, mixed int/REAL, non-BINARY collation). Equivalence was proven before measuring:

- 20,000+ randomized key pairs (every field class, both argument orders) produced identical
  ordering to `storage.RecordPayloadCompare` once the fallback rules were right.
  Bugs found and fixed during the A/B: the engine's BINARY text comparison returns the
  **length difference** on a common prefix (not ±1); blob uses `bytes.Compare` (±1);
  int-vs-REAL near the 2^63 boundary must fall back (`intFloatCompare` has SQLite's exact
  semantics).
- `go test ./...` at module root — ok. `internal/exec`, `internal/execdml`,
  `internal/execquery`, `internal/btree` — ok.
- JSON harness: failing-leaf sets **identical** to the frozen base on `index` (164),
  `where` (87), `delete`, `update`, `select`, `dml`, `analyze`, `misc` (the failures are the
  pre-existing excluded `bestindex*` families — the same in both builds).
- **Byte-identical database files**: base and prototype, writing the same 5,000 rows and the
  same 30,000 update/delete/insert operations over an integer index, a text index and a
  DESC-real index, produced identical **34,549,120-byte** files (`cmp -s` exit 0, sha256
  equal). This is the strongest available proof that the comparator changes no b-tree
  layout decision.

### 6.2 Skipping `formatConditions` — measured, not landed

Measured +20–21 % on indexed-select, but the prototype as written regressed
`EXPLAIN QUERY PLAN` output (see §5.3). Not safe to land in that form.

### 6.3 What is *not* in these numbers

Every number above is cold-statement (literal SQL re-parsed per execution) and
`:memory:`. Two caveats:

1. **Bound parameters / prepared statements will look better than these numbers for the
   planning-side findings**, because the template cache removes parsing but *not* the
   per-statement index-SQL re-parse and WHERE analysis (§5.2), which the frozen
   `indexesForTable` map should already answer.
2. The perfbench `indexed-update` phase is order-dependent (§3.3). Any performance claim
   about indexed UPDATE must be made on a **fresh** table.

---

## 7. Ranked optimization plan

Ranked by measured impact. Items 1–2 have validated prototypes; 3–5 are measured
attribution from profiles.

| # | Lever | Lane | Measured/expected gain | Effort | Risk |
|---|---|---|---|---|---|
| 1 | Route indexed-UPDATE's non-in-place remainder through the point-write primitives (drop the `len(inPlace) != len(toUpdate)` bulk-sweep fallback at `update.go:553`) | indexed UPDATE | **~1,300×** on `UPDATE … WHERE b=?` (239 → ~250k ops/s); removes 24,222 allocs and 14.4 MB per statement | M | Med — must keep old-index-entry removal correct |
| 2 | Land the decode-free `RecordPayloadCompare` (§6.1) | INSERT, indexed DELETE | **+25–31 %** inserts, allocs −64 %, bytes −45 %; validated byte-identical | S | Low — fallback covers the uncovered cases |
| 3 | Skip `formatConditions` on the non-EXPLAIN path (gate on `explain_mode`) | indexed SELECT | **+20–21 %** (5.15 → 4.26 µs) | S | Low **only if** EQP output is diffed before/after |
| 4 | Cache the parsed index columns / `KeyInfo` per (schema fingerprint, index) instead of re-parsing `CREATE INDEX` text per statement | SELECT, INSERT, UPDATE, DELETE | ~10 % of allocations on every indexed lane; +3.7 % alone, compounding with 3 | M | Low |
| 5 | Cache the WHERE-ref analysis (`seekableRefs` / `collectBinaryRef`) per statement shape | indexed SELECT | ~30 % of its allocations | M | Med |
| 6 | Reuse `BTree` wrappers and `KeyInfo` per (btree, fingerprint) rather than allocating per statement | all indexed lanes | ~5 % of allocations | S | Low |
| 7 | Replace `buildRowMapFromValues` per-row maps with slice lookup | INSERT | 16 % of objects, 11 % of bytes | S | Low |
| 8 | Apply the decode-free comparison to the read side (`DecodeRecord`/`DecodeCell`/`decodeValue`, ~20 % of indexed-select allocations) | indexed SELECT | ~20 % of allocations | M | Med |

**Expected end state if 1–4 land:** indexed UPDATE at ~250k ops/s (from 239),
INSERT at ~400k ops/s (from 287k, gap to sqlite 4.2× → 2.5×), indexed SELECT at
~280k ops/s (from 186k, gap 3.96× → 2.6×).

Items 1–2 close the two paths that are more than an order of magnitude off; items 3–4 are the
remaining 3–5× index-path gap.

### 7.1 The structural theme

Every remaining gap has the same shape: **per-statement work that does not depend on the
data is being redone on every execution.** Re-parsing the `CREATE INDEX` text, rebuilding
`KeyInfo`, re-analyzing the WHERE shape, formatting a discarded EXPLAIN string, and decoding
both records to compare two index keys. sqlite does all of this once per prepare (or with a
zero-allocation comparison). Frigolite already has the asset — `Stmt.Prepare` + the AST
template cache — so these are cache-population and decode-diet tasks, not architectural
changes.

---

## 8. Verification

```bash
# canonical perfbench comparison (frigolite + sqlite, alternating)
go run ./benchmarks/perfbench/frigolite -index -reps 3
go run ./benchmarks/perfbench/sqlite  -index -reps 3

# shape-matched, per-shape timings and allocation stats
cd benchmarks/perfbench/frigolite && go run . -shape all -n 5000

# correctness after any change
cd /Users/muaddib/dev/frigolite && go test ./...
FRIGOLITE_TEST=index  go test -run TestSQLiteSuite
FRIGOLITE_TEST=where  go test -run TestSQLiteSuite

# byte-identical b-tree layout (the strongest check for comparator changes)
# build the same db with base and candidate, then: cmp -s a.db b.db && echo IDENTICAL
```

All quantities in this document come from those commands run against the frozen base
`57513ad6`; see `.agents/R13_RESEARCH.md` for the R13 baselines this document continues.

---

## 9. Implementation status (branch `feature/perf`)

Four of the §7 levers are landed, each measured against the pre-change base
with paired runs on the same 50k-row table and validated for behavior identity.

| Commit | Lever | Lane | Measured result |
|---|---|---|---|
| `72ea84e07` PERF.R14-1 | indexed UPDATE positioned deletes (§3) | UPDATE | **239 → 86k ops/s (~360×)**; allocations 24,222 → 147/op, bytes 14.4 MB → 9.2 KB |
| `72ea84e07` PERF.R14-1 | UNIQUE conflict by index seek (§4.1) | INSERT | unique-index bulk load **903 → 6.1 µs/row (148×)** |
| `11be1c859` PERF.R14-3 | EQP-only condition text not built on the seek path (§5.3) | SELECT | **178–184k → 190–197k ops/s (+5–10 %)**; allocs 149.1 → 135.1/op |
| `cf0026413` PERF.R14-4 | installed index `KeyInfo` cached per (schema, ctx, index) (§7 #6) | INSERT | **178–180k → 190–192k ops/s (+6.5–7.5 %)**; allocs 229.2 → 214.2/op |
| `621793779` PERF.R14-5 | UNIQUE-conflict probe shares the `KeyInfo` memo (§7 #4/#6) | INSERT | **181k → 219k ops/s (+21 %)**; allocs 214.2 → 184.2/op |

Net on the three focus lanes, base → current:

| lane | base | current | gain |
|---|---|---|---|
| `UPDATE … WHERE b=?` | 239 ops/s | ~88k ops/s | **~360×** |
| `SELECT a FROM t WHERE b=?` | 165k ops/s | 196k ops/s | +19 % |
| `INSERT` with unique index | 178k ops/s | 219k ops/s | +23 % |

sqlite references for the same shapes: UPDATE 681k, SELECT 742k, INSERT 949k ops/s.

Validation used for every landed change (recorded per commit):

- `go test` for `internal/execdml`, `internal/exec`, `internal/btree`,
  `internal/execquery`, plus the module-root Go tests.
- JSON harness families `index`, `insert`, `update`, `delete`, `collate`,
  `reindex`, `conflict`, `upsert`, `explain`, `where`, `analyze`, `select`:
  **identical failing-leaf sets** against the pre-change base (the failures are
  the pre-existing excluded `bestindex*`/`index7` families, present in both).
- `EXPLAIN QUERY PLAN` output diffed byte-for-byte over 10 plan shapes.
- Byte-identical on-disk databases (`cmp -s` + sha256) across inserts,
  size-changing/shrinking/rowid-move updates, deletes, UNIQUE conflicts and
  multiple-NULL inserts.

### Remaining levers (not implemented)

§7 items #5 (cache WHERE-ref analysis, ~30 % of indexed-SELECT allocations),
#7 (`buildRowMapFromValues` → slice lookup, 16 % of INSERT objects),
#8 (decode-free comparison on the read side, ~20 % of indexed-SELECT
allocations). The next-biggest measured cost is `findChildIndexForInsert`
at 10.2 % of INSERT CPU, whose `DecodeRecord` work is the same
decode-boxing §4.2 removed on the write side.
