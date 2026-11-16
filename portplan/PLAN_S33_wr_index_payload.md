# PLAN S33 — the WITHOUT ROWID index payload format (C's [key..., PK tail...])

Unit S33: replace the synthetic-rowid tail of every WITHOUT ROWID (WR) secondary
index record with C's PRIMARY KEY tail, and enable the WR corrupt recheck
(where-25.5). Extends PLAN_S26_dml_corrupt.md's carded note.

## C contract (oracle 3.54, /tmp/s33/oracle hexdumps, 2026-11-14)

- Secondary index record on a WR table = [indexed cols..., PK cols...] — the PK
  columns in PK key order (build.c ~4269 "Append the table key"). Oracle wr.db:
  t(a,b,c,d PK(b,a)), i1 on c → records ('bar',3.5,'x')/('foo',1.5,'x')/('foo',2.5,'y').
- DUP ELISION: a PK column already among the index's KEY columns is NOT
  repeated in the tail (build.c isDupColumn → nColumn--). Oracle wr4.db:
  t(a,b,c PK(a,b)), i1 on (c,a) → records [c, a, b] (a elided).
- Tail collations = the PK columns' collations; tail sort flags = the PK's own
  sort flags (DESC honored). Oracle wr5.db: PK(b DESC), i1 on a → tails
  'z','y','x' (DESC order); wr2.db: PK(b COLLATE NOCASE) → 'aa' before 'ZZ'.
- The index's KeyInfo covers ALL fields (nKeyField == nColumn for WR) — the
  packed-vs-packed comparator (insert walk / delete descent) compares the tail
  under the PK collations.
- Probe seeks compare only the probe's fields (vdbeaux.c nField rule) — our
  btree.IndexRecordCompare already implements exactly this (probeFieldCount =
  len(probe.Values)); NO btree change needed (verified btree_keyinfo.go).
- Write-path recheck: the entry's tail seeks the WR table btree (PK-ordered,
  index-leaf cells, storage order PK-first) for the row; missing →
  "corrupt database" (where-25.5, oracle probe2.db). Reads stay tolerant
  (OP_NotFound skip; where-25.4 stays green — our WR SELECT paths never read
  secondary index payloads).

## Our divergence (probed 2026-11-14, /tmp/s33/probe)

- Every WR index write appends the insert-time rowid: /tmp/s33/probe/wr2.db
  page 3 = [1,1],[2,2] (i1 on t(a), t(a,b PK(b))). Nothing resolves that tail
  against our PK-keyed WR table btree → the 25.5 recheck cannot run (carded).
- DELETE leaves stale entries: probe wr2.db kept [1,1] after deleting k1 (the
  delete payload [1,0] ≠ the insert payload [1,1]).
- CREATE INDEX backfill writes [key..., 0] (probe wr1.db page 3) — a third
  tail convention; it also maps storage-order WR records by declared name
  (e.ctx.BuildRowMap does not remap) so non-identity WR layouts backfill WRONG
  key values (probe wr1.db: i1 on t(a) held the b values).
- integrity_check exempts WR tables from the entry-count and per-row presence
  passes (internal/exec/pragma_integrity_index.go) citing exactly this desync.

## Delivery

1. `internal/execdml/wr_index_tail.go` (new) — the C tail layout:
   - `wrIndexTailPlan(createSQL string, colDefs []sql.ColumnDef, indexCols []string) wrTailPlan`:
     PK columns (WRPKIndices order) minus those already among indexCols (plain
     column names only — expression keys never elide, C isDupColumn); per
     tail column: declared index, collation (column's declared collation,
     uppercased), DESC flag (table-level PRIMARY KEY(col DESC) or column-level
     PKDesc).
   - `(*DMLExecutor).wrIndexTailValues(plan, values)` — the tail values from a
     declared-order row value slice.
   - `extendIndexKeyInfoForWR(ki *btree.KeyInfo, plan) *btree.KeyInfo` — base
     KeyInfo + tail collations/flags appended (NKeyField covers the tail; the
     base memo's KeyInfo is copied, never mutated).
   - `(*DMLExecutor).wrRowExistsByPK(tableEntry, colDefs, pkKey []interface{}) (exists, seekable bool)`:
     the 25.5 recheck — SeekToKey lower bound over the WR table btree with the
     PK-only probe (the wrPKSeekConflict pattern, WRRecordComparator +
     wrValuesEqual slot verification); seekable=false when the PK layout is
     undecodable (caller keeps the scan verdict — wrPKSeekApplies' rule; a
     collated-equal PK row could land outside the binary window and a false
     corrupt on a healthy db is worse than no recheck).
2. Write sites (all append the tail instead of rowID for WR):
   - insert_core.go maintainIndexesOnInsert (~584): `append(indexValues, rowID)`
     → WR: tail values; writeIndexCell's comparator must be the extended one →
     writeIndexCell/`deleteIndexCell` gain the owning tableEntry (pass
     tableEntry through; rowid tables keep ikiOrder/installIndexOrder).
   - update.go collectUpdateIndexDeleteTarget (~412): the OLD row's payload gets
     the OLD tail. The targets map is keyed by rowID — WR cells all carry
     synthetic RowID 0 so per-row targets collide (pre-existing: only the last
     change's delete payload survived per index); key WR targets by the OLD
     PK tail's SQLiteValueString (unique per row).
   - update.go writeUpdateIndexEntriesFor (~447): NEW tail from the new values.
   - delete_index.go deleteRowFromIndexes (~73): tail from the deleted row's
     declared values (row.values / rowMapColumnValues — both declared order).
   - delete_index.go deleteIndexCellsBatch (~87): payload construction unchanged
     (values arrive full; indexStorageValues unwraps the tail too). The tree's
     comparator gets the extended KeyInfo (descent must match insert order).
   - reindex.go RebuildIndex (~209): tail from the scanned row (scanTableForMatch
     remaps WR records to declared order — verified).
   - execddl/ddl_index.go buildIndexValues/insertIndexKeyRow: WR → tail; ALSO
     remap the scan record (RemapWRRecordToDeclared) before BuildRowMap (fixes
     the wrong-key backfill); enforceIndexUnique slices the KEY prefix
     (len(keyExprs)), not len-1.
3. Comparator plumbing:
   - execdml/index_keyinfo.go: `installIndexOrderForTable` / memoized extended
     KeyInfo for WR (ikiKeyInfo stays the rowid-table memo; the WR extension
     derives from it per call — two slice copies).
   - internal/btree: NO changes. IndexRecordCompare probes len(probe.Values)
     fields (C's nField rule — already implemented); RecordPayloadCompare
     compares min(len) fields — one index tree's records all carry the same
     field count, and the extended KeyInfo now gives the tail its collations.
4. Recheck enablement (dml_corrupt.go + callers):
   - indexConflictEntry returns (entryRowID, pkTail, ok, errFallback): WR →
     pkTail = rec.Values[len(key):] (no int64 check — that is the rowid tail's
     rule), rowid tables unchanged.
   - insert_index_defs.go findRowByIndexCols: re-enable WR — probe → no entry:
     definitive no-hit; entry → rebuild the FULL PK (elided PK columns come
     from the probe key, the rest from the tail) → wrRowExistsByPK; missing →
     dmlCorruptError(true); present/unseekable → the existing scanTableForMatch
     verdict (exclude/NULL/collation semantics unchanged).
   - insert_core.go checkUniqueIndexExcluding (~631): same WR re-enable.
5. Reader sweep:
   - internal/exec/pragma_integrity_index.go: un-exempt WR — entry counts
     (all write paths now agree) and the per-row presence probe extended with
     the tail values (integrityIndexProbe gains the WR tail; the presence
     cursor's KeyInfo gets the WR extension). Watch for REAL drift reports —
     that is the point; a false positive would be a bug to diagnose.
   - internal/execquery: WR index scans are excluded today
     (select_index_seek.go:160, indexOrderedScanForOrderBy, loopOrderedEmission,
     joinInnerOrderPick's cost walk, planDMLSeek); the one leak —
     joinServingIndexCandidate can pick a secondary index for a WR operand and
     permuteJoinRightToIndexOrder then walks trailing rowids (synthetic today,
     PK-tail values tomorrow) — gets the WR guard (a WR operand's rows are
     already in PK order; no permutation).
6. Tests: /tmp/s33/probe main.go hexdumps (our pages must match C's record
   values + order for the same schema/data); pure-Go checks for where-25.5
   text and 25.4 tolerance; family sweeps (python leaf-name set diffs).

## Baselines (worktree HEAD 9a8391251, /tmp/s33/sweeps/before)

where=25.5 (+1 parent), where2=3 (2.4/2.5/2.5b), without_rowid=27,
temptable=7, vacuum=24, alter=20, quickcheck=0, index=101, insert=3,
delete=12, update=0, savepoint=15.

## Verify

- FRIGOLITE_TEST sweeps per family, python name-set diffs — no new leaves;
  where-25.5 green; 25.4 stays; without_rowid/vacuum/integrity surfaces
  leaf-identical or better.
- TestS22_/TestSOLID_ green; go build ./... clean.
- Full gate at the merge (coordinator).

## STATUS (2026-11-16, agent S33 resume)

- Items 1-5 DELIVERED at baa8dfebb: wr_index_tail.go complete (the plan's
  wrRowExistsByPK realized as wrSeekRowByPK + wrIndexEntryRowExists, with the
  wrPKSeekApplies BINARY-window gate), all six write sites, comparator
  plumbing (installIndexWriteOrder), the 25.5 recheck enabled over the
  entry's full PK, integrity un-exemption + joinServingIndexCandidate WR
  guard.
- Probe parity vs oracle 3.54 CONFIRMED (python dump_index.py, byte-equal
  record values + stored order): wr.db, wr2.db (NOCASE tail), wr4.db (dup
  elision), wr5.db (DESC tail), backfill.db; del.db stale entry gone;
  upd.db chngPk re-tail right; integrity ok on all.
- C update.c chngKey rule (update.c ~566: `chngKey || hasFK>1 || pIdx==pPk`)
  added to maintainedUpdateIndexes — a WR PK-column change maintains every
  index (the tail rides every record).
- TestSOLID_/TestS22_ green at commit; family sweeps PENDING (next).

## STATUS (2026-11-16, S33 COMPLETE)

- Items 1-6 DELIVERED: the C payload format on every write site, the 25.5
  recheck enabled, the reader sweep, probe parity, family sweeps clean.
- Commits: baa8dfebb (write paths + recheck + reader sweep), a5ea11125
  (WR table walks use the index-shaped btree — divider rows are real rows;
  the CREATE INDEX backfill/DML scans/integrity per-row walk lost one row
  per divider on multi-level WR tables).
- Probe parity (python dump_index.py vs /tmp/s33/oracle/*.db, byte-equal
  values + stored order): wr.db [c,b,a], wr2.db NOCASE tail, wr4.db dup
  elision, wr5.db DESC tail, backfill; del.db stale entry gone; upd.db
  chngPk re-tail; integrity ok everywhere.
- Family census (final, /tmp/s33/sweeps/final vs before): NEW=0 across all
  12 families. where 5->3 (where-25.5 GREEN — the recheck works; where2-2.4
  also absent this run = the documented ORDER BY random() flake flaking
  green, not a fix). where2 3, without_rowid 27, temptable 7, vacuum 24,
  alter 20, quickcheck 0, index 101, insert 3, delete 12, update 0,
  savepoint 15 — leaf-identical otherwise.
- Carded: temptable2-9.1.x (rowid temp table bulk-DELETE index drift) is a
  PRE-EXISTING random-sequence-dependent bug — fired 1/6 at the pre-S33
  baseline too; own slice needed (see lessons_learned flake watchlist).
