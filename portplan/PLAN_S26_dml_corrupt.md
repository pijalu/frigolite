# PLAN S26 — the DML corrupt contract (where-25.1/25.2/25.5) + the deferred read-path note

## The contract (oracle 3.54 + where.test 25.x, verified 2026-11-14)

Schema corruption: `PRAGMA writable_schema` re-points index i1's rootpage at
i2's btree, so i1's CONTENT is foreign (entries whose table rows do not
exist). C's behavior — the write path trusts the INDEX:

- **25.1** `DELETE FROM t1 WHERE c='iii'` (rowid table, i1 corrupt):
  → error `database disk image is malformed`. C's plan (EXPLAIN'd on the
  oracle): SeekGE/IdxGT on the index → DeferredSeek → **FinishSeek**
  materializes the index→table seek; the missing rowid raises
  SQLITE_CORRUPT (vdbe FinishSeek; OP_NotExists p2==0 is the same family).
- **25.2** `INSERT ... ON CONFLICT(c) DO UPDATE` (rowid table):
  → `database disk image is malformed`. C: the uniqueness probe is
  OP_Found ON THE INDEX → the conflict resolves at the index entry → the
  DO UPDATE re-seeks the table row (OP_NotExists) → missing → corrupt.
- **25.5** same upsert on a WITHOUT ROWID table → `corrupt database`
  (distinct text, oracle-confirmed on 3.54).
- **25.4** (must KEEP passing) `SELECT * FROM t1 WHERE c='iii'` on the
  WITHOUT ROWID variant → `{0 {}}` NO error: the read loop uses
  OP_NotFound on the PK (missing row = skip, no error).
- Reads through a stale index on a ROWID table DO error in C (3.54
  oracle: the SELECT errors) — but that surface lives in the execquery
  scan machinery, which in-flight units S23/S24 are editing. DEFERRED:
  our tolerant read is a documented divergence until after those merges.

## Our divergence (probed 2026-11-14)

Uniqueness/conflict resolution never consults the index btree:
- `findRowByIndexCols` (upsert/replace index hits) → scanTableForMatch
  (table scan; the def's Cols used only for matching).
- `checkUniqueIndexExcluding` → `indexHasConflictingKey` (the btree probe)
  only as a fast-NEGATIVE pre-filter; a positive verdict falls back to the
  authoritative table scan → no row → no conflict → silent insert.
- `seekDeleteRows` (delete_seek.go) → `!found` on an index-derived
  candidate rowid `continue`s (the stale candidate is treated as no-match;
  comment cites PERF_REPORT 3.4's no-match point DELETE — that case is the
  ROWID-plan miss, which must STAY silent).

## Delivery

1. `internal/execdml/dml_corrupt.go` (new):
   - `dmlCorruptError(withoutRowid bool)` — "database disk image is
     malformed" / "corrupt database".
   - `indexConflictEntry(tableName, indexName string, colDefs, key)` —
     indexHasConflictingKey's skeleton extended to DECODE the found entry:
     rowid-table index → the entry record's last value is the rowid;
     WITHOUT ROWID index → the trailing PK columns. Returns
     (entryRowID, pkTail, ok, errFallback).
   - NULL rule: any nil key component → no conflict via this index (SQL
     UNIQUE: NULL never conflicts; C's OP_Found never matches NULL).
2. `findRowByIndexCols` (insert_index_defs.go): probe the index FIRST —
   no entry → no hit (definitive); entry → fetch the table row (rowid
   seek / PK seek): missing → dmlCorruptError; present → hit with the
   fetched row's values. Signature gains an error return; update callers
   (findOnConflictRow insert_constraints_tail.go, collectReplaceConflicts
   if it calls this).
3. `checkUniqueIndexExcluding` (insert_core.go): probe → entry → table
   row missing → dmlCorruptError; present → the existing
   scanTableForMatch (excludeCell semantics preserved — the row exists so
   the scan finds it; verdicts for healthy dbs unchanged).
4. `seekDeleteRows` (delete_seek.go): when the plan is INDEX-driven
   (plan.index != nil) and the candidate rowid fetch misses → corrupt
   error (new error return; the ROWID-plan miss keeps `continue`).
   Propagate through the delete.go call sites. Apply the same rule to the
   UPDATE seek path if it shares the machinery.
5. WITHOUT ROWID DELETE/UPDATE keep the keyed scan (planDMLSeek returns
   nil there) — OP_NotFound tolerance, matching 25.4.

## STATUS (2026-11-14, slice 1 LANDED)

- 25.1 + 25.2 fixed (rowid-table recheck); where census 10 → 8. BONUS:
  the probe-first restructure fixed 9 previously-failing leaves
  (insert 2→0, delete 6→0, delete2 1→0) — the probe's definitive
  no-entry verdict replaces scan paths that had drift.
- **25.5 CARDED**: a WITHOUT ROWID index record in our engine carries
  only the synthetic rowid (writeIndexCell appends it) — our WR table
  btree is PK-keyed index-leaf cells, so the entry's locator is
  unresolvable and the C recheck (seek the table by the entry's PK tail)
  cannot run. The C-faithful fix is the WR index payload format change
  (store the PK tail like C: index record = [key..., PK...]) touching
  insert/update/delete/reindex/CREATE-INDEX backfill + any WR index
  reader — a standalone unit; the battery re-run after the format change
  must include the WR family sweeps.
  → **S33 LANDED as portplan/PLAN_S33_wr_index_payload.md** (concrete
  plan: tail layout incl. dup elision + tail collations/DESC, all 7 write
  sites, comparator extension, the 25.5 recheck, integrity un-exemption).
- where-25.4 stays green (WR reads keep the OP_NotFound skip).

## Verify

- Oracle: /usr/bin/sqlite3 probes above (probe.db/probe2.db in
  /tmp/s26probe).
- The where family: the 3 leaves flip, 25.4/25.3 stay green, where
  census 36 → 33; python name-set diff, zero new anywhere.
- insert/update/delete family sweeps (python diffs) — the constraint
  paths are hot; watch for false corrupt positives (NULL keys, partial
  indexes, expression keys, collations all guarded by the probe's exact
  ikiKeyInfo comparator).
- Full gate at the merge (coordinator).
