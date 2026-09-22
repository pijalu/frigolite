# P9.PERF.T3 — Value-ordered index seek infrastructure (btree side)

> Status: IN PROGRESS (fleet agent Q5-BTREESEEK, branch `fleet/q5-btreeseek`)
> Binding directive: ENGINE CORRECTNESS FIRST — every increment gated by the
> full battery; zero on-disk format change unless oracle-verified.

## 1. How C stores index keys (the ground truth)

SQLite index (and WITHOUT ROWID) b-tree cells store **record-encoded keys**
(OP_MakeRecord images): `[header-size varint][serial-type varints][bodies]`,
key columns first, trailing rowid last. The b-tree order is **the record
comparison order**, not byte order:

- `sqlite3VdbeRecordCompare` (vdbeaux.c:4950, generic impl
  `sqlite3VdbeRecordCompareWithSkip` vdbeaux.c:4709) compares an unpacked
  probe (`UnpackedRecord`: `aMem[]` values + `nField` + `default_rc`) against
  a packed record **field by field**, decoding the stored field lazily from
  its serial type + body bytes (no full record decode):
  - probe INTEGER: stored serial ≥ 12 → stored is greater (text/blob);
    serial 0 (NULL) → stored is less; serial 7 → `sqlite3IntFloatCompare`
    against the decoded REAL (so INT 5 == REAL 5.0); serial 1-6/8/9 → signed
    big-endian int compare.
  - probe REAL: same shape with float compare; stored NaN sorts before.
  - probe TEXT/BLOB: stored numeric/NULL sorts before; same class compares
    with the **per-field collation** `KeyInfo.aColl[i]` (BINARY = memcmp +
    shorter-first), length tiebreak.
  - probe NULL: only stored NULL/serial-10 compares equal.
  - On the first differing field: apply `KeyInfo.aSortFlags[i]`
    (`KEYINFO_ORDER_DESC` 0x01 negates, `KEYINFO_ORDER_BIGNULL` 0x02);
    all compared fields equal → return `default_rc` (0 for exact seeks).
- `KeyInfo` (sqliteInt.h:2664): `nKeyField`, `nAllField`, per-field
  `aColl[]`, per-field `aSortFlags[]`. Built at prepare time from the index
  DDL (collation + ASC/DESC per key).
- `sqlite3BtreeIndexMoveto` (btree.c) binary-descends interior pages with
  this comparator: **the tree order IS the comparator order** because the
  insert path (sqlite3BtreeInsert → sqlite3BtreeMoveto) positions with the
  same comparator. INT 5 and REAL 5.0 are adjacent in the tree even though
  their encodings differ.

## 2. What frigolite's byte ordering actually is

- Index leaf/interior insertion (`internal/btree/btree_leaf_split.go`
  `findInsertPositionIndex`, `sortSplitCells`) binary-searches with
  `BTree.compareKey`, whose default is `util.CompareValues` over two raw
  payload `[]byte`s — `classifyValue([]byte)` = blob → pure `bytes.Compare`.
  So the stored order is the **raw byte order of whole payloads**.
- Byte order ≠ value order **even within one uniform index**. Concrete
  counter-example (2-column records `col1,rowid`, probe TEXT `'a'`):
  ```
  ('a',5)    = 03 0F 01 61 05
  ('aa',5)   = 03 0F 01 61 61 05
  ('b',5)    = 03 0F 01 62 05
  ('a',300)  = 03 0F 02 61 01 2C
  ```
  Byte order: `('a',5) < ('aa',5) < ('b',5) < ('a',300)`. The value-equal
  set `{('a',5), ('a',300)}` is split by `('aa',5)` and `('b',5)` because a
  longer TEXT serial-type varint (0F vs 11) outranks body bytes, and a
  2-byte int serial (02) outranks any 1-byte int body.
- Consequences (P9.PERF.T2 lessons, `.agents/lessons_learned.md`):
  `Cursor.SeekToKey` (byte binary search) is unusable for value probes;
  DML point lookups value-scan the whole index with a byte prefilter
  (`execdml/seek.go` `walkIndexForCandidates` → `dmlPayloadKeyMatches`);
  a prior seek-based `DeleteIndexEntry` surfaced "malformed" on trees whose
  stored order drifted — walk-based deletion stayed correct. **A binary
  value seek on today's trees is unsound, full stop.**
- Per-entry cost today: `Cursor.ReadCellData` on an index leaf falls back to
  `ReadCell` → `storage.DecodeCell` (Cell alloc) + `readOverflow` (follows
  the whole overflow chain, allocates) — for EVERY entry — before the
  2-varint + memcmp prefilter; matching entries additionally pay a full
  `storage.DecodeRecord` + `util.CompareValues`.

## 3. Options considered

**(b) true ordered-key storage** — write index entries in record-compare
order (install a KeyInfo comparator at insert, like `WRRecordComparator`
already does for WITHOUT ROWID trees) behind a schema-migration guard.
Rejected for this tranche: existing trees are byte-ordered, so the reader
would need a per-tree order flag (header/format change → NOT oracle-verified
round-trip safe without deep autovacuum/backup/vacuum analysis), and mixed
order trees would silently break every binary reader. It also cannot land
incrementally green.

**(a) KeyInfo-carrying comparator + record-compare seek** — CHOSEN.
Zero on-disk change; lands incrementally; the walk visits every entry (O(n)
page walks remain until (b) lands) but:
1. it compares record fields directly against the probe — no `DecodeCell`
   alloc, no overflow-chain read (only when a comparison is undecided beyond
   the local payload fragment), no `DecodeRecord` on non-matching entries;
2. it carries `KeyInfo`, so when (b) lands later, the SAME seek API flips
   from walk to binary descent with no interface change — this tranche lays
   exactly that seam;
3. it is order-agnostic by construction (never binary-searches), so it is
   correct on any tree regardless of stored order — the T25 corruption class
   cannot turn it into a false miss.

## 4. Design

### 4a. `internal/btree/btree_keyinfo.go` — the comparator

```go
// KeyInfo mirrors sqlite3's KeyInfo (sqliteInt.h:2664) for index b-trees.
type KeyInfo struct {
    NKeyField  int      // number of KEY columns (the trailing rowid is not a key)
    Collations []string // per key column; "" = BINARY
    SortFlags  []byte   // per key column; bit 0 = DESC (KEYINFO_ORDER_DESC)
}

// UnpackedIndexKey mirrors UnpackedRecord: decoded probe values limited to
// the first NKeyField fields (nField), default_rc = 0.
type UnpackedIndexKey struct {
    KeyInfo *KeyInfo
    Values  []interface{} // int64 | float64 | string | []byte | nil
}

// IndexRecordCompare ports sqlite3VdbeRecordCompareWithSkip: compares the
// packed index payload against the probe over len(probe.Values) fields.
// Returns <0/0/>0; error on a corrupt/truncated record (SQLite sets
// errCode=SQLITE_CORRUPT and returns 0 — callers here fall back).
func IndexRecordCompare(payload []byte, probe *UnpackedIndexKey) (int, error)
```

Faithful-port notes: serial 10/11 (reserved) keep C's branch behavior
(-1/+1 in numeric probes, NULL-ish in the NULL probe branch) and never reach
`SerialTypeLength`; int serials decode as big-endian two's complement
(`vdbeRecordDecodeInt`); text compares through `KeyInfo.Collations[i]`
(BINARY memcmp+length, NOCASE ASCII-fold, RTRIM — same semantics as
`util.stringCompareFn`); DESC negates the first differing field.

### 4b. `internal/btree/btree_indexseek.go` — the seek

```go
// IndexKeyRowIDs walks every index entry (leaf list via the existing
// collectLeafPages walk) and returns the trailing rowids of entries whose
// first len(probe.Values) fields compare equal to the probe.
func (t *BTree) IndexKeyRowIDs(probe *UnpackedIndexKey) ([]int64, error)

// SeekIndexKey positions the cursor at the FIRST entry whose key fields
// compare equal to the probe (SQLite's sqlite3BtreeIndexMoveto contract).
// On today's byte-ordered trees this is a stored-order walk; when the write
// path becomes value-ordered (option (b), later tranche) the same signature
// binary-descends. found=false leaves the cursor at end-of-tree.
func (c *Cursor) SeekIndexKey(probe *UnpackedIndexKey) (bool, error)
```

Per entry the walk parses the index-leaf cell header directly (payload-len
varint → `storage.LocalPayloadSize` local fragment, no `DecodeCell` alloc),
compares against the local fragment, and materializes the full payload via
`readOverflow` only when the comparison is undecided beyond the fragment.
A matching entry's rowid is decoded from the record's trailing element
(DecodeRecord on the full payload — match-time only).

### 4c. `internal/execdml/seek.go` wiring (candidate location only)

`scanIndexCandidates` builds one `UnpackedIndexKey` per probe candidate
(affinity-applied + raw; deduped when value-equal), unions `IndexKeyRowIDs`
results, dedupes and sorts ascending (unchanged trigger/preupdate order),
and keeps the existing fallback: comparator/corruption error → `ok=false` →
full table scan. Unchanged: `seekIndexFor`/`dmlIndexProbeEligible` gates
(BINARY-collated, non-partial, plain leading column), full WHERE
re-evaluation as the exact filter, rowid-seek path.

Correctness argument: the candidate rule is a superset-or-equal of today's.
Today: byte-encoding prefilter + `CompareValues` equality (an int/real
value-equal pair with different encodings is MISSED by the prefilter — a
latent gap). New: record-format equality — for the gated BINARY indexes it
matches exactly the `CompareValues` rule, and additionally catches the
int/real encoding split (SQLite parity). Superset is safe because the full
WHERE still evaluates on every candidate.

## 5. Verification per increment

1. `go test ./internal/btree/... -count=1` (new comparator/seek unit tests
   include an order-agnostic oracle: random trees, brute-force decode+compare).
2. `go test -tags testgen -p 2 ./testgen/btree01/ ./testgen/autovacuum/
   ./testgen/corrupt2/ ./testgen/index/ ./testgen/update/ ./testgen/intpkey/
   ./testgen/without_rowid3/ -count=1`
3. Final: `go test . -timeout 45m` baseline set not grown; quality gates on
   changed files; `BenchmarkPerfIndexSeek*` before/after numbers recorded.

## 6. Measured results (2026-09-22, branch fleet/q5-btreeseek)

`go test -run '^$' -bench BenchmarkPerfIndexSeek -benchtime 3x -count 3 .`
(500 point-lookup statements per op against a 20k-row t2 with index i2a(a);
medians of 3):

| Benchmark | before (prefilter walk) | after (record-compare seek) | delta |
|---|---|---|---|
| `PerfIndexSeekMiss` (pure candidate scan, no matches) | 390.3 ms/op | 210.9 ms/op | **1.85x** (~18 ns saved per index entry visited) |
| `PerfIndexSeekUpdate` (1 matching row per statement) | 1853.5 ms/op | 1805.3 ms/op | ~3% (within noise: dominated by the sibling index i2b(b)'s maintenance walk — the T2 batch-delete path, untouched here) |

Correctness: `go test ./internal/btree/... ./internal/execdml/...` green
(new unit tests include a randomized order-agnostic oracle against
decode+CompareValues); the 7-package testgen battery shows an IDENTICAL
failure-signature set vs base main (autovacuum/index/intpkey red at base —
pre-existing drift, execquery scan-class turf, reported to the coordinator);
full-battery and `go test . -timeout 45m` runs recorded in the tranche
report.

## 7. Next-tranche handoff (documented, not built here)

- Flip option (b): install a KeyInfo comparator on index trees at
  INSERT/CREATE INDEX (pattern: `WRRecordComparator` + `SetKeyCompare`),
  gate reads with a per-tree order marker, oracle-verify round-trip.
- Lift the BINARY-only gate in `seekIndexFor` (comparator already carries
  collations), and extend probes to multi-column equality / range bounds
  (`nField > 1`, `default_rc` sign for GE/LE scans).
- `execdml/or.go` OR-optimization reuses value-sorting of table rows; it can
  adopt `IndexKeyRowIDs` per OR arm once landed (w5-query owns execquery
  scan classes — not touched here).
