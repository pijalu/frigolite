# Fleet State — clear snapshot at fleet stop (2026-09-23)

Main: `f2f0be9aa` — pushed. Build green; 18/18 wave-3 packages verified on main.

## Merged into main this session (30+ goals, all pushed)

T23 tkt_hash · T25 btree corruption (3 tranches) · T26 ×9 (alter, corrupt,
select/where/join, harness +2,922 subtests, singles, dml/index, fts3/4,
tkt, misc) · skip-audit · LIKE optimizer · automerge convergence · FTS
flush model (fts4merge4 green) · P9.PERF T1/T2/T3 · T29 engine4 + execqfix
(9 engine gap classes) · T28 regressions ×3 · T30-wal (10/11) ·
T30-fts3b (5 pkgs) · T30-vtab (7 pkgs) · T30-query (6 pkgs) — all
verified green on main at merge time.

## WIP branches (stopped mid-run — resume from these)

| Branch | State | Resume notes |
|---|---|---|
| `fleet/w5-fts3` | WIP commit 3ea152df1 | fts3aa/ac MATCH legacy-mode precedence (query_parse.go dual parser, fts3aa/ac 12/12 passing) + porter copy_stemmer done; fts3near/d/corrupt MOVED to w5-fts3b (merged). Remaining: verify no regression from the move; zz_debug_test.go is scratch (remove). |
| `fleet/w5-tkt` | WIP commit 380a22c5d | tkt×7 + misc×5 + collate×5 + minmax3/sort5/randexpr1/func_pkg triage in progress (frigolite_w5dbg_test.go is scratch — remove or finish). |
| `fleet/w6-kernel` | WIP commit 6eb865e53 | 18 kernel/pager singles (avfs, bigrow, btreefault, chunksize, mutex1, pager1, pagesize, prefixes, ptrchng, shortread1, softheap1, sqllimits1, rowhash, exclusive, zeroblob, e_blobclose, dbpage, corrupt). 16 files of engine work in the WIP commit — build state unverified. |
| `fleet/w6-misc` | WIP commit b603018db | 16 misc singles (alterlegacy, altertab, attach, autovacuum, backup, csv01, e_fkey, incrvacuum, pragma, pragma2, reindex, trigger3, trigger6, unionall, vacuum_into, vtab_shared). Pin test present; build state unverified. |

## Remaining fail list (113 at last census, pre-T30-wave; T30 merges closed
18 of them — expect ~95 + new-regen-exposure)

Full list in `tools/status/ledger.json` (fail states). Big classes:
- fts5 adjudicated architectural ×9 (circref/content/contentless×3/hash/
  leftjoin/misc/unindexed) — adjudicated, stay.
- Kernel singles (see w6-kernel branch).
- Newly-exposed query classes (see w5-query — merged, closed 6).
- tkt/misc/collate residue (see w5-tkt branch).

## Known infra issues for the next session

- The `git stash` cross-worktree hazard: banned in fleet protocol; use patches.
- Agent spawn mortality ~30% (rate limits/EPIPE): resume pattern = new agent,
  same worktree, `git merge main`, assess WIP.
- `go run ./tools/tcl2go/` requires the ori corpus (missing in fresh
  worktrees — regen checks are vacuous there; verify in main).
- Census parallelism can cross-contaminate: adjudicate suspicious flips
  serially per package.

## Verified-verdicts ledger for T30 agents (engine-first directive)

- w5-fts3(+b): 8/8 failures were ENGINE bugs (MATCH legacy precedence,
  offsets column-filter, porter copy_stemmer, NEAR strictness, segdir
  flush, CORRUPT_VTAB ×4) + 2 transpiler (fts3ab [set $lang], fts4unicode
  `array names`); legacy default-flag oracle built at /tmp/w5/sqlite3-legacy
  (volatile) — Apple /usr/bin/sqlite3 has ENABLE_FTS3_PARENTHESIS and is
  NOT ground truth for fts3 MATCH syntax.
- w5-vtab: 7 ENGINE bugs (INSERT..SELECT rowid leak into non-INTEGER PK,
  case-sensitive natural join, echo xBegin, NULL NOT IN vtab path,
  ORDER BY declared collation, MULTI-INDEX OR, series step=0) + 6
  transpiler attributions.
- w6-wal: 10/11 green (WAL close-checkpointing, walpersist 0-byte -wal
  contract, waloverwrite snapshot restore, per-pager locking_mode, commit
  veto rollback, interrupted COMMIT, lock shared-read contract,
  journal_size_limit −1 default); trans adjudicated: index-key-order row
  emission (planner goal, pairs with PERF.T4).

## Session continuation (single-agent, 2026-09-23)

Landed:
- fleet/w5-fts3 (merged 160952782): MATCH legacy precedence, porter
  copy_stemmer — 8/8 fts cluster green with the fts3b merge.
- fleet/w6-misc (merged 301800e18): pragma schema_version setter +
  VACUUM schema-cookie = pre+1 (pragma-8.2.4), FREELIST_COUNT
  schema-qualified handler, csv/vtab_shared emitter sync — 11/16 green.
- collate6-1.3 fixed: trigger NEW/OLD rows now carry declared column
  collations (wrapTriggerRowValue) — WHEN comparisons honor NOCASE;
  body writes stay raw (collate6/trigger1/temptrigger/fts5connect green).

Still open (resume order):
1. reindex + collate1/5/8 + minmax3 + index: ONE root cause — index-key
   MAINTENANCE ignores declared column collations (insert/REINDEX build
   binary-ordered keys), so ORDER BY over a declared-collation PK index
   returns binary order. Fix = KeyInfo-aware index compare using
   internal/btree/btree_keyinfo.go (PERF.T3 comparator — the seam exists,
   IndexRecordCompare already handles collations); wire into index insert
   + REINDEX with oracle-verified round-trip.
2. unionall (532/570: extra [2 2 0 {}] rows), backup (2.3s fail),
   e_fkey (residue), autovacuum (integrity_check "Page N never used"
   after VACUUM+autovacuum churn).
3. w5-tkt WIP branch: tkt/misc/collate triage in progress.
4. w6-kernel WIP branch: 16 files of kernel fixes, build unverified.
5. Planner goal: index-key-order row emission for SEARCH plans
   (trans-6.21..6.30, index(7)) — pairs with PERF.T4 value-ordered keys.

## Session close (2026-09-23, single-agent continuation)

Landed:
- fleet/kernel2 (merged): 18/18 kernel/pager singles green.
- fleet/tkt2 (merged 8161628f4): tkt2822 (compound positional sort — also
  fixed TestCompoundOrderPin), tkt3992 (UPDATE ADD COLUMN defaults),
  tkt4018 (second-conn lock emitter), tkt_38cb5df375 (tclLRange negative
  end), tkt_54844eea3f (derived-table outer-qual scoping), func_pkg
  135→0 (5 engine + 4 emitter fixes), sort5 evidence-skip + native pin.
- fleet/pinfix (merged f731f79bf): compound ORDER BY COLLATE preservation
  in the ordinal rewrite; P5ExplainEqpSubqueries pin corrected to the
  oracle contract (top-level correlated EXISTS has no SUBQUERY parent
  line); lock_status tx.readDbs branch restored alongside ReadTxHeld.
- P4Numeric randomblob pin corrected to oracle truth (n<1 → 1 byte).
- fleet/idx-coll (merged 75451d1ed): **index keys now collation-ordered**
  (RecordPayloadCompare under KeyInfo — previously raw payload bytes
  including serial-type varints); REINDEX physically rebuilds; query-side
  collation propagation (alias/positional/SELECT-*); oracle round-trips
  verified. reindex/collate8/minmax3/e_reindex green; index improved.
- collate6-1.3: trigger NEW rows carry declared column collations.

RESUME (in order):
1. collate1 (collate1_test.go:145 [{} {} {}] vs hex-function values) and
   collate5 + reindex-2.6/2.7: green at fleet/idx-coll tip f5a2ea59a, red
   on merged main — the merge interaction with main's select_columns
   declared-collation marker patch (line ~914) is the suspect; bisect the
   merged delta. idx-coll's own fixture registrations (hex collation +
   hex function) may also need porting.
2. autovacuum ("Page N never used"), backup, e_fkey residue, unionall
   570, tkt_78e04e52ea (empty-name index found-signal), randexpr1
   (nested correlated-agg — constraints in lessons).
3. w6-kernel branch preserves un-adopted WIP for: pragma-6.x PK ordinals,
   vacuum header metas, trigger3 RAISE scope, trigger6 UDF shadowing,
   alterlegacy/e_fkey legacy renames, csv01 declared types, lock
   read-marks — route to cluster owners.
4. Then: final census + adjudication + PORTPLAN §2/§5d close.
