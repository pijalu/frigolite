# Fleet State — clear snapshot at fleet stop (2026-09-23)

## T33 session START (2026-09-24, coordinator + 4 cluster agents) — ACTIVE

Objective: implement the remaining PORTPLAN work — drive the 17 actionable
testgen fails (of the 26-fail census) to green or evidence-adjudicated
close, then final census + PORTPLAN §2/§4/§5d close.

Baseline: main `9372fbb85`, census stamp 2026-09-24T18:06:08Z =
1037 pass / 26 fail / 283 skip. Build + vet green. Root `go test .`
baseline running (result → /tmp/t33_root_baseline.txt).

The 26 fails = 9 fts5 adjudicated architectural (stay) + 17 actionable:
misc2, misc3, misc5, misc7, misc8, having, where6, window8, selectH,
index, reindex, skipscan2, without_rowid4, permutations, fts3corrupt6,
rtree1, tpch01.

Fleet map (fresh worktrees, branches cut from main 9372fbb85):

| Branch | Worktree | Packages | Entry evidence |
|---|---|---|---|
| `fleet/t33-misc` | frigolite-wt-t33-misc | misc2 misc3 misc5 misc7 misc8 | resume from preserved WIP 380a22c5d (fleet/w5-tkt) — cherry-pick or re-derive; WIP touched execquery/execdml/execexpr/function/lexer/util + frigolite_w5_tkt_pin_test.go |
| `fleet/t33-query` | frigolite-wt-t33-query | having where6 window8 selectH | result mismatches (selectH do_test 1.3; window8 long row dump) |
| `fleet/t33-idx` | frigolite-wt-t33-idx | index reindex skipscan2 without_rowid4 permutations | reindex = documented planner sorter-omission tranche (2.6/2.7); permutations shows a nil-append PANIC; skipscan2/without_rowid4/index result mismatches |
| `fleet/t33-solo` | frigolite-wt-t33-solo | fts3corrupt6 rtree1 tpch01 | rtree1 "unable to id…" exec error; tpch01 EQP output (SEARCH supplier USING INDEX); fts3corrupt6 result mismatch |
| `fleet/t33-fts5` | frigolite-wt-t33-fts5 | fts5circref fts5content fts5contentless fts5contentless3 fts5contentless4 fts5hash fts5leftjoin fts5misc fts5unindexed | the 9 "architectural" adjudications were NOT NA_EVIDENCE-backed — implement the SQL-visible semantics (contentless_delete tombstones, external-content reads, unindexed cols, LEFT JOIN MATCH, re-entrancy guard) within the mirror-storage model; per-assertion NA_EVIDENCE only as last resort |

Protocol: engine-first (pure-Go probe before any edit), oracle
/usr/bin/sqlite3 ground truth, no `git stash` (patches only), agents
commit to their branch, coordinator merges + verifies + owns
FLEET-STATE.md. Ori corpus absent in fresh worktrees — symlink
/Users/muaddib/dev/frigolite/ori if regen needed. Serial package runs.

Coordinator-side baselines taken (main 9372fbb85):
- Root `go test .` (non-testgen): FAIL after 368s — failing test(s) TBD
  (full log rerunning to /tmp/t33_root_full.log).
- staticcheck ./...: 4 U1000 unused (btree_interior_page.go:81
  mustEncodeDividerCell, btree_tail.go:229 removeEmptyIndexLeaf,
  btree_tail.go:271 dropIndexLeafRefFromParent, pragma_analyze.go:133
  schemaPrefixOf). vet: 1 pre-existing (btree_interior_page.go:64
  unreachable).
- §5d legacy worklist (gate scope = non-test, non-third_party,
  non-testgen): over-1000-line files = exec/pragma_table.go 1098,
  execquery/select_agg_validate.go 1037, exec/pragma_analyze.go 1030,
  execquery/select_agg.go 1017, storage/storage.go 1005,
  execquery/select_columns.go 1004; gocognit>15 non-vendored = 17
  (resolveOrderByOrdinalTerms 44, compactExprText 34,
  primaryKeyOrdinalsFromSQL 33, reindexTargets 29, autoindexKeyColumns
  29, compareOrderByValues 27, evalMinMaxAggregate 22, equivalentGroupKey
  20, validateCompoundOrderByCollations 20, +3 below cutoff); gocyclo>12
  = 21. PLAN: dispatch the §5d refactor fleet AFTER all fix agents merge
  (same hot files — conflict avoidance), in 3 disjoint-file agents.
- SOLID green at baseline.

Merge log:
- `43c1504f0` ← fleet/t33-solo (fts3corrupt6/rtree1/tpch01 green; verified
  on main: 3 pkgs + SOLID green).
- `771abb374` ← fleet/t33-misc (misc2/3/5/7/8 green; verified on main:
  5 pkgs + SOLID + staticcheck 0 new).
- `748fdb03a` ← fleet/t33-idx (permutations/index/reindex/skipscan2/
  without_rowid4 green; verified on main: 5 pkgs + TestP5AnalyzeReindex +
  GlobRangePin + tkt2822 pins green).
- (next) ← fleet/t33-query (having/where6/window8/selectH; verified on
  main: 4 pkgs + SOLID green — merged with the push above).

§5d REFACTOR MERGES (all verified on main: build+SOLID+slice targets):
- `7cb9008d5` ← fleet/t33d-exec (10 findings cleared, pragma splits,
  schemaPrefixOf U1000 gone).
- `ab7008ddf` ← fleet/t33d-cmd (processcmdextra 1915→876, cmdexpr
  1537→743, gen 1075→470; 7 new family files; regen byte-identical).
- `482f787e0` ← fleet/t33d-set (processSetBracketValue 186/126→4/4,
  processNamespaceSet 142/76→8/7; 5 new family files).
- `5e0692f26` ← fleet/t33d-db (processdb 1826→split, processblob,
  processsqlite3).
- `c00f1f86d` ← fleet/t33d-q (select_columns/agg/agg_validate/subq_unused
  splits, disableUnusedSubqueryColumns 102→2, storage.go split, 3 btree
  U1000s + vet unreachable removed).
- staticcheck repo-wide now ZERO findings (was 4 U1000). §5d staticcheck
  + vet = CLOSED.

REGRESSION FOUND (post-merge sweep): testgen/window1 fails with 5
mismatches (1567/1579/2258/2270/3325) — attribution: fleet/t33-query's
positional-ORDER-BY change (window1 was green pre-merge; the q agent
documented the same 5 at ITS base, pre-regen lines 1551–3309; my
t33-query verification ran window8 but not window1 — gap in the merge
gate). REPAIR: fleet/t33-win (frigolite-wt-t33-win) dispatched with a
30-package sweep requirement.

Corpus regen note: the 4 tcl2go agents each carried an identical
full-corpus regen sync commit (2,181 files) — merged cleanly; the
committed corpus now matches the merged emitter. Known pre-existing
regen flicker: indexfault 2-line alias-order swap from a map-range in
emitTclProcAliasRegistrations (documented in lessons; fix must be its
own corpus-wide regen commit).

Still active: fleet/t33d-flow (final long-tail tranches), fleet/t33-win
(window1 repair, alive — active edits 2026-09-25 23:1x).
fleet/t33-fts5 AGENT DIED ~2026-09-24 23:12 (session interruption;
uncommitted WIP: structvtab.go new + structure/vocab/engine_register
edits). RESUMED per protocol: new agent (fleet/t33-fts5, same worktree,
merge main first, adjudicate WIP file-by-file).

LATE-BREAKING (2026-09-25 late evening):
- fleet/t33-win MERGED (fae5dec16): window1 regression fixed
  (omit-unused-subquery-column use-walk must mirror name resolution).
- fleet/t33d-flow MERGED (b73b9283a): the whole tcl2go long tail.
- Coordinator remainder §5d fixes pushed (ba9247849): skiptests2_part2
  1342→798+556 split + skipTestReason/resolveOrdinalOrderByTerm/
  indexOrderedScanForOrderBy gocyclo — gocognit/gocyclo/file-size/
  staticcheck/vet ALL ZERO repo-wide now.
- CORPUS COMPILE BREAKS from the regen sync FOUND AND FIXED
  (964efc700): varCount missing in 6 sub-transpiler literals (counter
  reset → 'no new variables' in window6/pager1/skipscan5);
  tclFpnumCompare(bool) type errors (interface{} wrapper); UDF $args
  list-rendering (emitPrefixFunction). Full corpus regen #2 landed;
  corpus vet = zero type errors. window6 GREEN through pure
  engine-side correctness (tclQuoteListElem double-bracing symmetric
  with flatten).
- NEW ENGINE REGRESSION under investigation → fleet/t33-idxfix
  (frigolite-wt-t33-idxfix): autoindex b-trees misordered at
  multi-page scale (misc5 t2 repro: insert-batch-grouped walk instead
  of value order; REINDEX does NOT fix; small trees correct).
  Bisected to the idx agent's autoindex-DML-maintenance feature
  (748fdb03a) — first code to exercise multi-page splits under KeyInfo
  comparators. Suspect: interior descent/split decisions using byte
  comparators instead of keyCompare. misc5 fell between the idx and
  exec agents' validation sets.
- fleet/t33-fts5 STILL ACTIVE in frigolite-wt-t33-fts5 (fts5hash +
  fts5unindexed + contentless3-2.x green at ef605a1ad; segment/structure
  persistence model landed; 4 dirty files mid-work).

Coordinator housekeeping: 46 stale pre-T33 worktrees removed (branches
preserved; fleet/w5-tkt WIP 380a22c5d remains on its branch).

§5d worklist after merges (gate scope, main tree):
- over-1000-line files (16): tools/tcl2go/{processcmdextra 1915,
  processdb 1826, cmdexpr 1537, processset_part2 1433, processloop 1355,
  skiptests2_part2 1342, dotest 1285, processset 1266, processblob 1115,
  gen 1075} + internal/exec/pragma_table 1098, exec/pragma_analyze 1026,
  execquery/select_agg_validate 1037, execquery/select_columns 1024,
  execquery/select_agg 1017, storage/storage 1005.
- gocognit>15 non-vendored ≈17, gocyclo>12 ≈21, staticcheck U1000 ×4
  (3 btree + schemaPrefixOf), vet ×1 pre-existing.
- Dispatch plan: 3 refactor agents NOW (tcl2go family-1; exec pkg;
  execquery+storage+btree+execdml), 4th agent (tcl2go family-2 incl.
  skiptests2_part2) AFTER t33-fts5 lands (skip-entry conflict
  avoidance).

§5d REFACTOR FLEET DISPATCHED (2026-09-24, 6 agents — full worklist was
much larger than old notes: ~110 tcl2go + ~30 engine complexity
findings):

| Branch | Worktree | Slice | Validation gate |
|---|---|---|---|
| fleet/t33d-set | frigolite-wt-33d-set | tcl2go processset+part2 (processSetBracketValue 186/126!, processNamespaceSet 142/76, file splits) | regen BYTE-DIFF (testgen/ unchanged) |
| fleet/t33d-db | frigolite-wt-33d-db | tcl2go processdb+part2+blob+sqlite3 | regen BYTE-DIFF |
| fleet/t33d-cmd | frigolite-wt-33d-cmd | tcl2go processcmdextra+cmdexpr+gen | regen BYTE-DIFF |
| fleet/t33d-flow | frigolite-wt-33d-flow | tcl2go dotest×2+processloop+foreach+command+collect+flow+expected+strings+stringexpr+vars+misc long tail | regen BYTE-DIFF |
| fleet/t33d-exec | frigolite-wt-33d-exec | internal/exec (pragma_table/pragma_analyze splits + schemaPrefixOf U1000) + execdml hot funcs | testgen validation set green |
| fleet/t33d-q | frigolite-wt-33d-q | execquery (select_columns/agg/agg_validate/subq_unused/order_emit splits + hot funcs) + storage.go split + btree (3 U1000 removals + hot funcs) | testgen validation set green |

HELD BACK for post-fts5: tools/tcl2go/skiptests.go + skiptests2_part2.go
function findings (fts5 agent may add skip entries there).

Mid-session progress (2026-09-24, ~30 min in):
- t33-idx: 2 commits — permutations GREEN (stale-regen artifact: package
  predated emitter fix 3bf26dc7d; nil tclListBuilder Append panic) +
  index/skipscan2 (seek constraints must form leading prefix —
  where.c whereLoopAddBtreeIndex; new execquery/bestindex.go extracted
  from 1000-line explain_plan.go).
- t33-misc: 2 commits — misc5 GREEN (lexer leading-dot literals + LIMIT
  subquery prepare error), misc8-1.6 btree saveAllCursors port (WIP).
- t33-query: WIP commit — positional ORDER BY compares output row at
  ordinal position (where6-3.1, window8-1.8.8).
- t33-solo: 1 commit — rtree1-17.1 REINDEX resolves zero-index/vtab
  targets (likely also fixes TestP5AnalyzeReindex root fail — to verify
  at merge).
- t33-fts5: still baselining the 9-package class.
- Root-suite rerun done: top-level fails = TestP5AnalyzeReindex (idx
  agent owns) + TestSQLiteSuite legacy JSON-harness drift (385 files,
  adjudicated T32-wip: superseded pipeline, testgen is the census
  currency; pre-squash drift documented §2 DRIFT ALERT).

---

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
   JOINED BY T32-wip findings: TestW5Tkt2822CompoundOrderByAlias (green at
   47772f421, red from merge 75451d1ed — compound ORDER BY QX,XX / t6b.x,QX /
   t6a.q,XX return pre-tkt2 order; oracle 3.54 wants verified) and
   TestP5AnalyzeReindex ("REINDEX main: unable to identify the object" —
   already failing at f5a2ea59a). Both are idx-coll engine-delta interaction;
   root-suite drift, not testgen.
2. autovacuum ("Page N never used"), backup, e_fkey residue, unionall
   570, tkt_78e04e52ea (empty-name index found-signal), randexpr1
   (nested correlated-agg — constraints in lessons).
3. w6-kernel branch preserves un-adopted WIP for: pragma-6.x PK ordinals,
   vacuum header metas, trigger3 RAISE scope, trigger6 UDF shadowing,
   alterlegacy/e_fkey legacy renames, csv01 declared types, lock
   read-marks — route to cluster owners.
   DONE (T32-wip): all six already adopted on kernel-wip via fleet/w6-misc
   (b603018db, merged 301800e18) and evolved; routed testgen packages
   pragma/trigger3/trigger6/alterlegacy/e_fkey/csv01/lock 7/7 green; 16/16
   TestW6MiscPin_* green; oracle 3.54 parity spot-checked. No further routing.
4. Then: final census + adjudication + PORTPLAN §2/§5d close.

Root-suite state at T32-wip (fleet/kernel-wip): 9 top-level fails at
babbd8c0d → 7 after two oracle-adjudicated stale-pin fixes
(TestSQLiteGlobRangePin want = index BINARY order "abd|acd";
TestWindowCGroupConcatBlobUTF16 null rendered "NULL" per flattenResult —
the pin had never passed since ca196b7a3). Remaining 7: tkt2822 +
P5AnalyzeReindex (RESUME-1, idx-coll follow-up), TestSQLiteSuite legacy
JSON-harness drift (identical on main; testgen corpus is the census
currency) and 4 missing-oracle-fixture infra fails (backupconformance,
walconformance, 2 regen fixtures needing the ori corpus).

## Session close 2 (2026-09-24, single-agent)

Landed: fleet/collate-res (merged 39ad1ee67) — collate1/collate5 GREEN
(the "merge interaction" premise was wrong: the green state lived on the
unmerged fleet/w5-tkt WIP; real gaps = format-UDF emitter stub,
numeric collation equality, INTERSECT/EXCEPT last-row survivor).
Fleet/kernel-wip merged: 2 stale root pins corrected (GlobRangePin
oracle-truth, WindowC null want); root fails 9→7.
P4Numeric randomblob pin corrected to oracle truth (n<1 → 1 byte).

NEW REGRESSION (top priority, introduced by fleet/collate-res 39ad1ee67):
**compound ORDER BY sorting is skipped entirely** — tkt2822 got
[1 8 9 2 1 7 7 2] (insertion order) for ALL FOUR ORDER BY variants
(PX/YX aliases, XX/QX, QX/XX, qualified t6b.x). TestW5Tkt2822CompoundOrderByAlias
red (was green at 8161628f4). Suspect: select_setop.go
intersectRows/exceptRows survivor rewrite or the new select_agg_group.go
key path dropping the compound sort call for UNION ALL. Fix = restore the
sort (or its call) while keeping the last-row survivor semantics for
INTERSECT/EXCEPT. tkt2822's own fix (select_validate_part2.go alias-first
+ compound exemption) must keep working.

Then: reindex 2.6/2.7 (planner sorter-omission tranche, documented),
randexpr1 + tkt_78e04e52ea (T32-deep agent — check its branch
fleet/tkt-deep for landed work), final census + PORTPLAN close.

## T32 close (census 2026-09-24+, post all T30/T31/T32 merges)

1037 pass / 26 fail / 280 skip / 17 suspects (same slow set, serially
adjudicated previously: 9 pass + 8 confirmed slow-class). The 17
non-adjudicated fails:
- 9 fts5 adjudicated architectural (stay).
- **8 fall-through-crack packages**: misc2, misc3, misc5, misc7, misc8
  (the w5-tkt WIP that fixed them was on commit 380a22c5d, preserved —
  `git log 380a22c5d` — when fleet/tkt2 was reset; resume by cherry-pick
  or re-derive), having, permutations, where6, window8, selectH, index,
  reindex (planner sorter-omission tranche), skipscan2, without_rowid4
  (4 residual), tpch01, rtree1.
- fts3corrupt6, e_fkey-class items: re-enumerate at next census.
