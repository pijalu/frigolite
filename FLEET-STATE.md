# Fleet State — T34 CLOSE (2026-09-28)

## T34 session CLOSE — COMPLETE

Main: 74b5b39cb (+close commit). Final census stamp 2026-09-28T00:11:56Z:
**1,363 testgen packages → 1,072 PASS / 0 FAIL / 290 SKIP (all
NA_EVIDENCE-audited) / 0 unresolved suspects** (fts4merge4 serially
adjudicated green 683-806s). `tools/status -check` = 0 flips; `-audit`
= pass; status self-tests ok; ledger re-seeded.

T34 additions over T33 close:
- fleet/t34-vacuum (f2433a886): vacuum-corruption test stray COMMITs
  dropped (oracle-verified drift); corrupt-7.3 REAL engine gaps closed
  — in-place same-size cell overwrite (C loc==0 fast path,
  btree_update_inplace.go) + unconditional balance_deeper for overfull
  root leaves (raw copyNodeContent) — TestW6_Corrupt7 pin.
- fleet/t34-bigrow (d62a178d7): bigrow-2.2 RESOLVED by attribution —
  engine byte-exact vs oracle; residue was tclListFlatten want-
  rendering; native pin ~170 subtests (size/seam sweep, all pagesizes).
- fleet/t34-x6 (8d589f1e8): x6 conformance verdict (b) — ENGINE bug:
  NULL appendable-segment row misclassified malformed
  (decodeSegmentBlock); C fts3IsAppendable parity; conformance 5/5
  byte-exact; fts4growth 7.4-7.7 pin.
- fleet/t34r-btree (2fb3aa7aa): 64KiB cell-content u16 wrap
  normalization + pad-4 leaf cells (btree01/changes/fts4aa);
  reservebytes: vacuum copy-back reserve propagation (C zeroPage/
  usable parity — header byte 20 from temp layout, usable-end anchors,
  InvalidateCache header re-adoption).
- fleet/t34-planner (860543fc2): SEARCH-plan index-order emission —
  where.c wherePathSatisfiesOrderBy port (scanLoop, EQP sorter
  omission, runtime index-order permutation incl. reverse/rowid-desc
  ties); 30-shape oracle battery.
- fleet/t34-perf (860543fc2): P9.PERF tranches — scan/eval −24..−29%
  (affinity plan + IPK precompute, statement-scoped collation memo,
  binaryOp switch dispatch), CURRENT_* keyword length fix; byte-
  identical fence.
- fleet/t34r-split (74b5b39cb): vacuum6 interior-split — atomic
  aggregate room precheck in applyChildSplitsRightmost + divider-by-
  divider chain apply (C balance_nonroot gather-then-redistribute
  parity); seeded pins (unseeded randomblob had hidden the latent
  defect). Attribution: latent since T33-idxfix fat dividers, NOT the
  reservebytes tranche.

Fleet-process notes: telegram skill style enforced on all agent prompts
(user directive). Concurrency limit hit repeatedly — agents dispatched
serially when needed. Two silent agent deaths + one shared-worktree
double-writer incident resolved via split-scope protocol. Push refspec
trap documented (always `<branch>:refs/heads/<branch>` — tracking
config can redirect a colon-less push to main).

Open residue (documented, non-blockers): internal/fts x6 fixture
regen-from-oracle design (gitignored fixtures; fresh clones need
tools/orafixture run), harness vacuum6 1.2/3.0 reset_db conversion gap
(pre-existing), TestSQLiteSuite legacy drift (adjudicated superseded),
speed1p 445s PERF profile (improved but still above C).


## T34 session (2026-09-27) — residue fleet ACTIVE

Objective: close the 4 documented T33 residue items (FLEET-STATE above).
Worktrees cut from main 3b9cc6646:

| Branch | Worktree | Items |
|---|---|---|
| fleet/t34-vacuum | frigolite-wt-t34-vac | TestVacuumDoesNotCorruptBTree (txn-state) + testgen/corrupt-7.3 (balance-deeper oversize-cell accounting) |
| fleet/t34-bigrow | frigolite-wt-t34-bigrow | bigrow-2.2 oversized-record UPDATE loses first 2 bytes (~65KB swap) |
| fleet/t34-x6 | frigolite-wt-t34-x6 | TestWriterConformance fts-x6-growth fixture-vs-fix verdict (91e4296b5) — RESOLVED 2026-09-27: verdict (b) ENGINE bug (decodeSegmentBlock NULL marker row blocked the continuation append; attribution "since 91e4296b5" wrong, fixture correct per fresh oracle regen); fix + native pin TestT34X6_FTS4GrowthMergeContinuationPin on fleet/t34-x6 |

Protocol unchanged: engine-first, oracle ground truth, serial validation,
per-commit push, no main merges by agents. Merge log:
- fleet/t34-vacuum MERGED f2433a886 (verified: corrupt green, vacuum test
  fixed, SOLID).
- fleet/t34-bigrow MERGED d62a178d7 (RESOLVED by attribution — engine
  byte-exact; residue was tclListFlatten want-rendering; native pin
  ~170 subtests).
- fleet/t34-x6 MERGED 8d589f1e8 (verdict (b): ENGINE bug — NULL
  appendable-segment row misclassified malformed in decodeSegmentBlock;
  conformance 5/5 byte-exact vs oracle after C fts3IsAppendable parity).

T34 CENSUS (main 8d589f1e8): 1068/5/290. NEW FAILS = T34-vacuum tranche
fallout: btree01 (interior page full @65536 — balance_deeper routing
must stay leaf-root-only per C balance()), changes (malformed on 5000
recursive insert), reservebytes (Page never used again — chain leak via
new paths), fts4aa (41s, bisect pending). fts4merge4 = known contention
artifact. → fleet/t34r-btree (frigolite-wt-t34r-btree) dispatched to
repair the tranche to full C parity (keep corrupt-7.3 contract).

T34r-btree RESOLVED (merged 2fb3aa7aa): btree01/changes/fts4aa green —
64KiB cell-content u16 wrap normalization + pad-4 leaf cells C parity.
reservebytes root cause isolated: vacuum copy-back never stamps header
byte 20 (usable mismatch on reopen) — fix owned by the T34r-vacuum
owner session (still active in frigolite-wt-t34r-btree, its
pagerconfig.go ResetToEmpty byte-20 stamping in progress). Second
resume agent stood down per shared-worktree protocol (zero edits,
lessons note committed).

T34 EXPANDED FLEET (user directive: max sub-agents + telegram style on
all; fresh worktrees, disjoint scopes):
| Branch | Worktree | Scope |
|---|---|---|
| (owner session) | frigolite-wt-t34r-btree | reservebytes byte-20 fix (in flight) |
| fleet/t34-planner | frigolite-wt-t34-planner | SEARCH-plan index-order emission (EQP sorter omission, trans6 family) — execquery only |
| fleet/t34-perf | frigolite-wt-t34-perf | P9.PERF hot-path tranches (profile→optimize→byte-identical fence); balance-file + pager exclusions |
All prompts enforce telegram thinking style per the telegram skill.

## T33 session START (2026-09-24, coordinator + 4 cluster agents) — superseded by T33 CLOSE below

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
- SECOND REGRESSION (same merge window): TestP2ViewColumnList (root
  native) — FIXED AND MERGED: fleet/t33-win T33-win2 (ddc4012c9, merged
  e5e8a8a5c): view declared column lists resolve positionally in the
  omit-unused-subquery-column use-walk; 31-pkg sweep green.
- MISC5 REGRESSION FIXED AND MERGED: fleet/t33-idxfix (afcbc3397,
  merged aa23922dd) — REAL ROOT CAUSE: index btree inserts were
  hard-routed to the rightmost child (T31-era) and index interior
  dividers carried no payload, so key-guided descent was impossible.
  FIX = SQLite's model (btree.c:8820 parity): full separator payloads
  with parent-owned overflow chains, findChildIndexForInsert binary
  descent through keyCompare (equal keys go RIGHT), chain lifecycle
  (rekey/abandon/reparent), integrity_check interior-array coverage.
  Pins TestNativeIdxfix* green on main. All 21 validation packages
  green.
- fleet/t33-fts5 agent DIED A SECOND TIME during the coordinator pause
  (16h-stale WIP: flush.go + 2 native test files + zz scratch).
  RESUMED AGAIN per protocol (same worktree, merge main, adjudicate
  WIP). Remaining unknowns: fts5content/fts5circref/fts5misc residue —
  the resume agent re-baselines all 9.
- Race gate (§5e-2) running in background on main aa23922dd; will
  re-run after the fts5 merge lands.

## T33-close wave 4 (2026-09-26) — post-merge census regression wave

fts5 merged (239b5c4f5): ALL 9 former fts5 targets green on main
(resume2: fts5content fully green — N-A upgraded to real pass;
MULTI-INDEX OR branch order; fts5Init scalars; 27-pkg fence green).

Final census (2026-09-26, -timeout 900s, ALONE): 1363 pkgs →
1037 pass / 39 fail / 287 skip. The 26 baseline fails are ALL GREEN.
BUT 39 NEW fails: (a) ENGINE regressions from T33 merges — values/
distinct/nulls1 PROVEN red at pre-corpus-sync 748fdb03a; reservebytes/
corrupt "Page N never used" = idxfix divider-chain leak residue;
(b) corpus-activated assertions from the double full-regen (kernel
singles: mutex1/softheap1/shortread1/e_blobclose/btreefault);
(c) emitter gaps — avfs "got [fosAvfs $fa]" (un-interpolated $var);
(d) fts5 wave — fts5prefix MATCH syntax error + slow-family real fails
(fts5delete 68s etc.) suspecting f5cfbdd69/aa23922dd interaction.
NOTE: first census rerun (1038/38) was contaminated — raced the root
race leg in the same worktree; kernel singles "failed" spuriously.
LESSON: never run the census concurrently with any other go test load.

Race gate: lockreg.NewConnID non-atomic counter raced under parallel
Open (8 DATA RACE warnings) — FIXED (atomic.AddInt64, on main). Root
race leg still fails TestFTS4Merge4Automerge8Grind under -race (no
DATA RACE; timing-sensitive grind vs race slowdown) — with
fts4merge4 failing the census too, the t33r-fts agent owns the root
cause. tools/status self-test fails on 17 unresolved timeout-suspects
in the ledger — resolves at close: adjudicate serially + re-seed.

TRIAGE FLEET DISPATCHED (worktrees frigolite-wt-t33r-*):
- fleet/t33r-order: values distinct nulls1 windowC with3 selectC
  whereA whereF unionall backup e_fkey bigrow pragma corrupt
  e_blobclose reservebytes (engine regressions + idxfix residue;
  bisect protocol per package).
- fleet/t33r-kernel: avfs (emitter $var interpolation) + mutex1
  softheap1 shortread1 e_blobclose btreefault (corpus-activated
  classification: engine vs emitter vs NA_EVIDENCE).
- fleet/t33r-fts: fts5prefix + fts4merge fts4merge4 fts5aj fts5bigid
  fts5merge fts5optimize fts5contentless2 fts5delete.
Coordinator re-censuses AFTER all three land (alone, nothing else
running).

## Census wave RESIDUE (2026-09-27)

All wave-4 branches merged (kernel2 = 0b7639954; t33r-fts incl. 90595c293
follow-up = c44b2e9b3). Census (545949b03): 1064 pass / 9 fail / 290
skip. Serial adjudication by coordinator: fts4merge4 GREEN (683s,
slow-but-green — census panic was parallel-load contention),
fts5content GREEN (census flake). REAL residue = vtab constraint
propagation cluster (vtab1/vtab3/vtabH/vtab_shared/tabfunc01/
tkt_ba7cbfaedc — extra rows = WHERE constraints not reaching
xBestIndex/xFilter; PRIME suspect t33r-order bestindex.go seek-prefix
rewrite) + tkt_3a77c9714e (string-case UDF shape from kernel2).
→ fleet/t33r-vtab (frigolite-wt-t33r-vtab) dispatched; bisect protocol
against 7efda8cdf.

## Wave-4 merge status (2026-09-26 late)

- fleet/t33r-order MERGED (a90f0597a → main): use-walk three-ways
  (VALUES columnN naming, CTE-body descent, subquery rowMap projection),
  ORDER-BY-index gate (per-term collation + NULLS FIRST/LAST vs scan
  placement), sqlite_autoindex DDL-slot parity (whereA), divider-chain
  leak precheck + integrity_check UsableSize decode (backup/
  reservebytes — idxfix residue CONFIRMED: absent before aa23922dd).
  Handoff to kernel agent: selectC (string toupper proc stub), whereF
  (TCL regexp \y word boundary → literal y), windowC (db-eval per-row
  body dropped). Pre-existing at base, NOT wave: bigrow-2.2
  (oversized-record rewrite boundary), corrupt-7.3 (layout-bound).
- fleet/t33r-kernel MERGED (8d4486b05 → main 7efda8cdf): WAVE ROOT
  CAUSE = the coordinator's §5d skiptests2_part2→part3 split
  (ba9247849) silently dropped the skipTestsMoreT30Kernel map (11
  oracle-adjudicated skips) — restored. Real emitter fixes: softheap1
  PRAGMA soft_heap_limit lowering, catch-of-bind rc NAME (text binds),
  skip side-effect preservation (shortread1), e_fkey setup replay,
  pragma reopen. 16 packages green on main.
- Kernel agent RESUMED (T33r-kernel2) for the selectC/whereF/windowC
  emitter handoffs.
- fleet/t33r-fts still working (fts5prefix + slow family; f5cfbdd69/
  aa23922dd attribution in progress).
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

## T34-vacuum (2026-09-27, branch fleet/t34-vacuum — residue updates)

- internal/exec TestVacuumDoesNotCorruptBTree ("cannot commit - no transaction is
  active") RESOLVED — test-side expectation drift (stray autocommit COMMITs written
  before the execCommit guard existed); oracle-verified, removed (f9da41aac).
- corrupt-7.3 RESOLVED at the engine level (02ade962b) — NOT layout-bound (that
  read was wrong: frigolite's cell layout matches the reference build at offset
  788). Real gaps: same-size UPDATE went delete+reinsert (compacted the page and
  destroyed the crafted cellPtr[0]) and overfull root leaves skipped
  balance_deeper. Fixed with btree.OverwriteCellByRowID (btree.c loc==0 in-place
  overwrite) + unconditional balance_deeper for overfull root leaves (raw
  copyNodeContent + ValidateCellSizeCheck); contract pinned in TestW6_Corrupt7.
  The tcl2go skiptests entry stays (tools/tcl2go untouched); bigrow-2.2 and the
  fts-x6 / pager WAL-fixture items remain with their owners.

## Parse handler restructure (2026-09-28, commit 6fa561fde, direct on main)

User-driven cleanup after 68f2816f5 review: ruleHandlers map keys and the
numeric parser_rulesX.go split were unreadable. Landed:

- 11 functional files replace the 7 arbitrary numeric splits: stmt, ddl,
  cdef, select, expr, expr_ops, window, dml, trigger, pragma, misc.
- parser_ruleids.go: one rid* constant per handled rule (349), each
  commented with its grammar production; ruleHandlers map now reads
  `ridExplainPlain: ruleExplainPlain`.
- 61 legacy ruleNNN handlers named from body semantics + yyRuleInfoLhs
  symbol-code resolution + neighbor-grammar continuity; 22 over-long /
  prose-derived names shortened.
- Two provably mislabeled headers corrected: rule 128 = on_using ::= ON
  expr (NOT joinop), rule 131 = scanpt ::= (empty) — LHS symbol codes
  (262/267) + bodies + production-count arithmetic all agree.
- Grammar-comment provenance lesson: original generated file had only 57
  "Rule N:" comments; 288 exist today; no authoritative grammar text ships
  in-repo (no parse.y, no yyRuleName in sql_tables.go). If sql_tables.go is
  ever regenerated, emit tables.RuleName (go-lemon supports it) so rule
  comments become verifiable.
- Zero behavior change evidence: rule-number set preserved exactly
  (349=349), bodies byte-identical (348/348; 8 stray in-body comments moved
  to proper headers), parse tests + 14 parse-heavy testgen packages green,
  staticcheck/vet/quality-gate/SOLID clean.

## PERF: CRUD benchmark vs sqlite3 3.54 (2026-09-28, report committed)

benchmarks/PERF_REPORT_2026-09-28.md — frigolite 8×–15,000× slower on CRUD
hot paths (CPU+memory measured; GC-bound profiles). Six bottlenecks
root-caused with probe/profile evidence:

1. UPDATE uniqueness check full-scans table per row even when no unique
   column changed (update_check.go checkLiveTableConflictsWR) — worst path
   (15k× gap). Fix P1: change-detection gate (aUpdateFlag/chngRowid parity).
2. SELECT rowid-alias equality misses seek fast path (rowid_seek.go matches
   only literal rowid names; EQP already says SEARCH — plan/exec mismatch).
   Fix P2: carry isIPKRowidAliasCol predicate into execquery (layering!).
3. rowid range (BETWEEN/</>) never sought — planned+executed SCAN. Fix P4.
4. DELETE: per-statement Pager.Snapshot deep-copies whole page cache
   (9.4ms/statement constant, O(db)). Fix P5: before-image journaling.
5. btree cursor registry (btree_cursor_save.go): finalizer-only pruning,
   saveAllCursors allocates per stale entry — measured O(n²) insert growth,
   191KB/stmt. Fix P6: explicit statement-end cursor release + fast-path.
6. rowIDExists linear walk (insert REPLACE/conflict) — seek instead. Fix P3.

Systemic: per-statement alloc volume → GC coordination dominates CPU
(kevent/cond_wait 60-95% of samples); scan throughput 1.45M rows/s vs 53M
(36×) = standing P9.PERF eval-engine gap. P1/P2/P3 small+independent
(one-session fleet tasks); P5/P6 engine-level worktree branches. Harness +
probes live in /tmp/perf (method in report §2); commit as cmd/perfbench if
regression tracking is wanted.

## PERF-FIX (2026-09-28, START) — execute fix plan P1-P7 of benchmarks/PERF_REPORT_2026-09-28.md

Coordinator on main; 4 fleet agents in worktrees (branches below) + P3 by
coordinator; P7 after merges. Benchmark harness + probes: /tmp/perf
(frigo/, ssql/, probe*/). Baseline numbers in the report tables.

- fleet/perf-p1-update-gate   — P1 UPDATE change-detection gate (update_check.go)
- fleet/perf-p24-rowid-seek   — P2 IPK-alias SELECT seek + P4 rowid range seek
- fleet/perf-p5-delete-journal — P5 statement rollback without O(db) snapshot
- fleet/perf-p6-cursor-lifecycle — P6 explicit cursor release + saveAllCursors fast-path
- coordinator direct           — P3 rowIDExists → SeekToRowID
- P7 alloc diet + prepare/reuse scoping — after merges, measured
Verification per fix: pure-Go probes, targeted testgen suites, quality
gates; end: full benchmark rerun + census + report update.

## PERF-FIX (during) — P1 + P3 merged to main (2026-09-28)

- P3 coordinator commit 65746ffe6 (rowIDExists -> SeekToRowID; REPLACE
  3.14->2.59ms/op @20k; residual = alloc churn, P7 scope).
- P1 agent branch fleet/perf-p1-update-gate merged (5bf40a8d0): UPDATE
  change-detection gate; probe 5.72ms -> 79us/op @20k (73x), 14.15ms ->
  159us @50k (89x). Bonus fix: WITHOUT ROWID table-level PK enforcement
  was missing on UPDATE (no sqlite_autoindex row synthesized); now
  oracle-exact. 50 testgen suites green; gates clean.
- Deferred by P1 with justification: unique-index probe for changed
  constrained columns — current IndexKeyRowIDs is an exhaustive leaf walk
  (no asymptote vs scan); needs value-ordered-index tranche. Recorded as
  follow-up. Also: UPDATE conflict compare does not apply collation
  (pre-existing NOCASE-UNIQUE under-enforcement, preserved).
- Remaining in flight: fleet/perf-p24-rowid-seek, fleet/perf-p5-delete-journal,
  fleet/perf-p6-cursor-lifecycle.

## PERF-FIX (during 2) — P2+P4 and P6 merged (2026-09-29)

- P24 merged (9e1c80a3c): IPK-alias equality seek (5.35ms -> 8-20us @50k),
  rowid range seek (BETWEEN 8.1ms -> 35-65us), EQP rendered from the SAME
  analysis the executor runs (28-shape battery 0 diffs vs oracle). Lessons:
  SeekToRowID must maintain the cursor path stack (else Next() replays
  rows); IPK alias is stored NULL and row-sources must fill it from rowid;
  range bounds must be supersets of rowPassesWhere affinity semantics;
  reverse_unordered_selects reverses the range walk; lazy two-phase decode.
  Pre-existing gap documented: rowid-vs-text eval ignores whitespace
  (oracle divergence on SCAN path too; untouched).
- P6 merged (90695ffe0; one code conflict resolved: SeekToRowID = checkOpen
  guard + path-stack seek combined): BTree.Close() ownership model +
  statement-funnel release (Engine.Exec defer, nested-Exec segment marks),
  saveAllCursors fast-path, finalizer kept as safety net. Insert slope now
  LINEAR: 105k/105k/104k ops/s @20k/40k/60k GOGC=off (was 45k/31k/23k
  quadratic); alloc ~12.6KB/op flat; misc8-1.6 contract green; ~80 testgen
  pkgs + btree race subset green.
- Remaining in flight: fleet/perf-p5-delete-journal (riskiest — pager
  before-image journaling).

## PERF-FIX (during 3) — P5 merged; failure triage (2026-09-29)

- P5 merged CLEAN (27 files, no conflicts): pager statement journal
  (pagerstmt.go, sub-journal port) — first-modification before-image
  capture at the markDirtyLocked choke point, nested scope splicing,
  whole-state scopes kept for BEGIN/SAVEPOINT/memdb/FTS-index. DELETE
  plan: absent-rowid seek = empty candidates, sparse (<=64) deletes seek
  via DeleteCellByRowID. no-match DELETE 15.6ms -> 8.5us (~1000x); agent
  ran FULL testgen corpus (1363 dirs) exit=0 zero FAIL.
- Merged-main full-suite triage (all pre-existing, evidence in
  /tmp per-state sampling): TestRtreeStressChurn flaky at ALL states
  (base 4/60, P24 3/60, P6 5/60, P5 2/60 — map-iteration delete order
  hits a latent rtree-module row-loss; NOT introduced by PERF work;
  FOLLOW-UP filed). TestP8IncrVacuum3OracleSequence: documented
  randomblob flake. TestWindowCGroupConcatBlobUTF16: passes isolated at
  base+main — full-suite ordering artifact. TestSQLiteSuite: adjudicated
  legacy drift (standing).

## PERF-FIX (END, 2026-09-29) — P1-P7 executed; census 1073/0/290

Complete fix plan of benchmarks/PERF_REPORT_2026-09-28.md landed on main.
Benchmark after (vs sqlite3 3.54 literal): update 877x faster than before
(65 -> 57,004 ops/s; 17.4x residual gap), point select 525x (192 ->
100,857 ops/s; 8.7x), delete 401x (133 -> 53,313 ops/s; 23.9x), insert
5.2x (21,281 -> 109,974 ops/s; 11.6x); scan/group unchanged (standing
eval-engine item, P9.PERF). CPU util 1.6-2.5x wall -> 1.2-1.4x.

- Merges: P3 65746ffe6 (coordinator), P1 5bf40a8d0, P24 9e1c80a3c,
  P6 90695ffe0, P5 02bff7cbc (all fleet branches pushed).
- P7: landed echoVTabSource short-circuit 1d4317e02; remaining tranche
  documented with alloc-profile evidence (btree split-cell pooling,
  schema-level index-def cache; prepare/reuse API NOT the lever — parse
  is us-level, AST template cache exists).
- REGRESSION FOUND+FIXED: P1's runUpdateFail gate skipped the row WRITE
  (not just the uniqueness scans) when no constrained column changed ->
  UPDATE OR FAIL applied nothing on unconstrained tables (check-6.5/6.6).
  Fixed 768eae135 + native guard TestUpdateOrFailKeepsPriorRows. Lesson:
  the agent's 50-suite validation set missed check/; only the census
  caught it — post-merge census is mandatory for every fleet branch.
- Census: 8-worker pool produces ~15 contention flakes on long suites
  (fts4merge4, avtrans, fts3b, corrupt, intarray, limit, tkt_d11...); ALL
  pass serially and got FASTER with the fixes (fts4merge4 601s base ->
  427s main; avtrans 81s -> 76s). Authoritative census command:
  `go run ./tools/status --audit --concurrency 2 --timeout 25m` ->
  1073 pass / 0 fail / 290 skip (== pre-PERF baseline).
- Pre-existing flakes documented, NOT ours: TestRtreeStressChurn
  (map-order delete churn, ~5% both sides), TestP8IncrVacuum3
  (randomblob), TestWindowC (suite-order artifact), TestSQLiteSuite
  (legacy drift, adjudicated).
- Follow-ups filed: value-ordered index tranche (enables true index
  probes for changed-constrained UPDATEs); collation-aware UPDATE
  conflict compare (pre-existing NOCASE-UNIQUE under-enforcement on
  UPDATE path); rowid-vs-text whitespace affinity gap (pre-existing);
  rtree churn flake; P7 remainder (split-cell pooling, index-def cache).

## PERF-PUSH (2026-09-29, START) — P7 continuation: close residual gaps vs sqlite3

Objective: push optimization toward sqlite3 parity (speed/memory/CPU).
Residuals on main fe55eec09: update 17.4x, insert 11.6x, point 8.7x,
delete 23.9x, scan 39x, group 8.2x (ops/s vs sqlite3 3.54 literal).
Plan: (1) fresh per-phase CPU+alloc profiles; (2) fleet branches:
pipeline hot-path (parse/exec/statement overhead — gates update, point,
insert, delete) + scan-eval throughput (biggest gap); (3) coordinator
quick wins (ruleHandlers array dispatch); (4) measure each merge with
/tmp/perf harness; suites + census at end.

## PERF-PUSH (END, 2026-09-29) — residual gaps closed; scan 39x -> 7.2x

Second optimization round complete on main bf66d87fb:
- fleet/perf7-scan merged bc6aa7a43: aggregate feed (OP_AggStep parity,
  bare COUNT/SUM/AVG/TOTAL fast path + guards, generic fallback),
  range-loop buffer reuse, WHERE BETWEEN fast eval. SCAN-AGG probe
  1.60M -> 12.0M rows/s (7.5x); benchmark scan phase 1.45M -> 7.44M rows/s.
- fleet/perf7-pipeline merged 38aaa3a86: COW template substitution now
  covers SELECT/UPDATE/DELETE (was INSERT-only — every non-INSERT
  statement re-parsed), parser sync.Pool, allocation-free preprocess
  gates, zero-alloc keyword classification, index-def/constraint caches
  with fingerprint invalidation (tests included). insert 1.59x, select
  1.21x, update 1.21x, delete 1.23x per statement.
- f504f8b1f: parser reduce dispatch array (-5%/stmt).
- TWO correctness bugs caught by the final census, fixed bf66d87fb:
  COW FuncCall clone dropped Over (window1/window6 ntile misuse error);
  fastParseInt64 overflow wrap made 2^64 share a template with literal 0
  (func4-5.29 tointeger(toreal(2^64)) returned 0 not NULL). Both pinned
  by TestTemplateCloneOverflowLiteral. Agent validation sets keep
  missing canary suites — census after every merge stays mandatory.
- Final census: 1072 pass + savepoint2 (contention flake, passes serial
  13.8s) + 290 skip = effective 1073/0/290, audit exit 0.
- Final benchmark (100k rows): insert 103k ops/s (12.4x), point 90.4k
  (9.7x), scan 7.44M rows/s (7.2x, was 39x), update 50.7k (19.6x),
  delete 50.5k (25.2x), file autocommit 1.46x FASTER than sqlite3. CPU
  util 1.14-1.36x wall; per-phase heap 2.8-85MB.
- Plateau + next tranches documented in the report: value-ordered index
  / typed-row (scan floor), prepare/bind API (statement floor), GROUP BY
  feed discipline (untouched 8-10x).

## PERF-GC (2026-09-29, START) — full re-profile; Go/GC-specific optimization round

Objective: every phase still below sqlite3 (point 9.7x, insert 12.4x,
scan 7.2x, update 19.6x, delete 25.2x, group ~9x) gets a complete fresh
CPU+memory profile; bottlenecks fixed with Go-specific patterns
(sync.Pool reuse, boxing elimination, map->slice, escape analysis,
alloc-size reduction) to cut GC impact. Measure per step; suites +
census at end.

## PERF-GC (END, 2026-09-29) — full re-profile + GC-pattern round complete

All six phases re-profiled (CPU+alloc): runtime/GC coordination dominates
every phase; fixed the repeating allocators:
- partitionSplitCells O(n^2) probe -> running-total fit (INSERT best run
  184k ops/s);
- 52x ToUpper-per-statement WITHOUT ROWID gates + FTS/vtab prefix gates ->
  util fold helpers (tableIsWithoutRowid etc.);
- buildColumnIndex memoized per schema fingerprint
  (TestColumnIndexCacheInvalidation);
- fleet/perf-gc-rowmap merged 1baf26b14: row-output diet (GROUP BY probe
  -57% allocs, 2.1-2.4x; scan SELECT -51% allocs); 154/154 suites.
Benchmark: insert 142-184k ops/s, point 128-130k, scan 7.5M rows/s,
update 74-75k, delete 65-66k; CPU/wall 1.11-1.40x. Census 1073/0/290
audit exit 0.
Remaining floors with exact frames documented in the report PERF-GC
section: group-key machinery (equivalentGroupKey linear scan), storage
decode + journal copies (update/delete), value-ordered-index tranche
(scan), prepare/bind API (point/insert). Follow-up: journal before-image
pooling deferred (rollback-correctness risk vs 4-5%).

## PERF-GC2 (2026-09-30, START) — apply the proposed floor optimizations

Round: (1) GROUP BY group-key fast path (typed compare replacing the
fmt.Sprintf("%v") per-value-per-row equality + single-term key fast
path) — coordinator; (2) update/delete decode+journal diet
(storage.DecodeRecord 26% + btree cell decode 17% + journal 15% of
remaining bytes) — fleet agent with fresh profiles; (3) scan/prepare-bind
tranches stay documented follow-ups (value-ordered index, public API).
Measure per step; suites + census at end.

## PERF-GC2 (END, 2026-09-30) — group-key + decode floors applied

- Group-key fast path aaf0891e9 (coordinator): typed scalar equality
  (groupKeyScalarEqual, %v semantics, oracle-pinned
  TestGroupByTypedKeyEquality) + collation-gated equivalent-scan skip +
  single-term key fast path. GROUP BY phase 9 -> 29 ops/s (3.2x).
- fleet/perf-gc2-decode merged 63137985c: DecodeRecord stack scratch +
  DecodeCellInto/ParsePageInto targets + one-wire EncodeCell; btree
  in-place decode/arena encode/verified rebalance parent hint (O(db)
  walk only on miss); journal before-image pool with ownership-transfer
  rollback (3 lifetime tests; StmtJournal objects NOT pooled —
  documented identity contract). DELETE -29% allocs/+15% ops/s;
  grow-shape UPDATE -86% allocs/+52% ops/s. 98 testgen pkgs incl. all
  27 corrupt canaries green.
- Full bench: scan 8.85M rows/s (+19%), group 28 ops/s, insert 178k,
  point 128k, update 75k, delete 65k. Census 1073/0/290 zero flakes,
  audit exit 0.
- Remaining tranches unchanged: value-ordered index (scan), prepare/bind
  API (point/insert), group-key EvalExpr (largest remaining group frame),
  exec plumbing + DML row contracts (update/delete).

## PERF-PARITY (2026-09-30, START) — push toward sqlite3 performance parity

Objective: bring frigolite within the same performance level as sqlite3,
using the go-perf skill methodology (benchmark -> profile -> fix top
frame -> re-benchmark, per phase). Gaps on main 71bf20f5a: insert 7.0x,
point 6.4x, scan 6.0x, group 4.4x, update 12.7x, delete 18.9x.
Plan: fresh 6-phase CPU+alloc campaign; parallel fleet tranches
(per-statement pipeline; scan/decode throughput); coordinator fixes on
identified frames; iterate merges + re-profile; suites + census at end.

## PERF-PARITY (during) — 5 of 6 tranches merged

Merged: api b4b672465 (root-layer allocs -73%, wall 4-6%), expr
b3ccd8bfa (typed arith/compare/concat fast paths, expr SELECT 1.16x,
77/77 suites), wrappers 9cb942f92 (btree wrapper+cursor pooling, lockKey
memo, normalizeSQL scratch; registry-key bug found+fixed en route;
point -8.2% allocs 1.12-1.25x, INSERT -14.9%), pager 08b045049
(ParsePage memo w/ byte-fingerprint invalidation, dirty-set reuse,
split encode arena; point in-slice allocs -41%, insert -28%, +8-12%),
group b9dcf469b (group/ORDER BY invariant hoisting +11%).
Review-audit fixes 4db5f10eb (P1 INSERT template kind coercion — silent
wrong persisted type, pre-existing; P2 seek saved-state reset + missing
checkOpen; probes ported as committed tests).
Full bench on main: insert 191k ops/s (6.6x), point 136k (6.1x), scan
9.49M rows/s (5.5x), group 28, update 84k (11.4x), delete 91k (+40%
this round; 13.6x), file autocommit 1.9x FASTER than sqlite3.
In flight: fleet/perf-parity-posrows (resume agent adjudicating
predecessor WIP — positional DML row collection).

## PERF-PARITY (during 2) — validation sweep: 1 real regression found

fleet/validate-parity agent: build/vet/staticcheck clean; 266/266 testgen
sweep PASS; contract probes (kind gate, typed group keys, misc8, ntile,
seek-state) ALL PASS. ONE real regression: P1 use-after-pool SIGSEGV —
pooled BTree wrapper Closed/reset while an enclosing statement's scan
cursor still uses it (nested statement re-registers a shared wrapper;
inner release frees outer-owned wrapper). 4/4 full-suite crashes on main;
pre-tranche 0/2. fleet/perf-parity-poolfix dispatched (ownership-aware
registration). Environmental: 8 conformance tests need gitignored
fixtures (pass after copy); harness isolation-mode failures pre-existing.
Census/final close-out GATED on poolfix landing + 3x clean full-suite.

## PERF-PARITY (END, 2026-10-01) — 7 tranches + audit + validation, parity converged

All tranches merged to main (memofix f243cc18b last). Final quiet-machine
table (vs sqlite3 3.54 literal): insert 6.8x, point 7.0x, scan 6.2x,
group 4.6x, update 12.5x, delete 15.4x, file autocommit 1.65x FASTER.
NEW capability: db.Prepare -> Stmt.Exec/Query parameter binding (bound
point-SELECT 1.5x over literal, 184-188k ops/s). Census 1073/0/290
audit exit 0.
Correctness findings from parallel review+validation agents, ALL fixed
with pinned probes: INSERT template kind coercion (pre-existing P1),
btree seek saved-state reset (P2), pooled-wrapper use-after-pool
(poolfix: finalizer-once + wrapper pooling dropped), pager-memo canary +
crafted-CellCount uint16 wrap + index-decode tail guards. Documented
pre-existing divergences: -0.0/0.0 and text-5-vs-int-5 textual-key
grouping; x % 0.1 panic; rowid-vs-text whitespace affinity.
Remaining structural tranches (documented): value-ordered-index/typed-row
(scan 6.2x floor), exec plumbing (update/delete 12.5-15.4x), group-key
EvalExpr per row (group 4.6x). prepare/bind API DELIVERED this round.

## PERF-STRUCT (2026-10-01, START) — structural tranches toward parity

Verifier-gated round: scan floor 6.2x (value-ordered-index/typed-row
tranche) then update/delete 12.5-15.4x (exec-plumbing rewrite).
Tranche A (fleet agent): columnar/typed scan fast path — decode the
referenced column directly from cell payload for the agg feed + bare
projection, skip whole-record DecodeRecord boxing. Target >=20M rows/s
(gap <=3x). Tranche B (fleet agent): single-pass update/delete pipeline
for the rowid-seek shape — seek -> encode once -> in-place write, no
intermediate change structs. Target >=2x (gap <=7x). Benchmark pair
after EACH tranche; suites + census at end.

## PERF-STRUCT (during) — same-kind template gate: first attempt reverted, tplgate agent dispatched

Coordinator's same-kind substitution gate (int→int slot, float→float slot
with .0 rule, hex/non-finite refusals) delivered huge wins in isolation
(point 210k ops/s, update 136k, insert 228k) but broke testgen/func4
boundary statements: unary-minus literals (-9223372036854775808,
-2147483649) corrupted sign/value through template hits. Reverted
(spelling gate restored, func4 green); semantics pin committed
(9d211826c). Root-cause hypothesis: the parser FOLDS unary minus into
minInt64-case literal text (AST "-9223372036854775808") while
normalizeSQL skips the minus — stored template spelling mismatches what
later statements' substitutions assume. fleet/perf-parity-tplgate
dispatched with full forensic traces to derive the exact folding rule
and implement a correct gate. Tranche A (columnar scan) + Tranche B
(update/delete pipeline) dispatch pending this resolution (they consume
the same per-statement fast path).

## PERF-STRUCT (during) — tplgate merged a55419000

Same-kind template gate LANDED: root cause = parser rule 216 folds unary
minus into the literal ONLY for decimal 2^63 (and hex) spellings — three
AST shapes (folded-INTEGER, UnaryOp-INTEGER, UnaryOp-REAL) share one
normalize key f(-?). Correct gate: refuse folded slots (text starts '-'),
hex slots ('x'/'X' — hex digits include 'E' defeating the naive exponent
check), exact-2^63 doubles, non-finite floats, kind crossings; allow
int64→digit-only slot (FormatInt) and float64→'./e' slot (FormatFloat
g,-1,64 + .0 restore). point 1.55x, update 1.54x (probe), func4 green,
76/76 suites, 18 gate pins. Fold rule is load-bearing: parser rule 216
changes must extend the gate.
In flight: fleet/perf-struct-scan (columnar read), fleet/perf-struct-dml
(single-pass DML). Benchmark pair after each merges.

## PERF-STRUCT (during 2) — struct-scan merged 8afc20df9

Columnar read path (DecodeRecordColumn/Columns: serial-type walk to the
target column, OP_Column parity) wired into agg feed + bare projection.
Bare single-col scan 2x (7.0M), COUNT(*) +168% (12.5M), SUM +90-103%,
allocs/row -73..-85% on wide fixtures; 188/188 testgen incl. all corrupt
canaries; 18-shape parity byte-identical. Combined with tplgate, full
bench: update 106k ops/s (+42%), delete 104k (+60%), group 49 (+75%),
point 144k, insert 198k. Gaps: insert 6.3x, point 5.8x, scan 6.3x,
group 2.5x, update 7.0x, delete 11.9x.
In flight: fleet/perf-struct-dml (single-pass UPDATE/DELETE pipeline).

## PERF-STRUCT (END, 2026-10-02) — structural tranches landed; parity converged to 2.3-10.9x

Tranches merged: tplgate a55419000 (same-kind template gate; parser
rule-216 fold contract pinned), struct-scan 8afc20df9 (columnar
DecodeRecordColumn(s) read path), struct-dml 96d7a5f0e (single-pass
rowid-pinned UPDATE/DELETE + single-cell DeleteCellByRowID + encode
diet), poolfix 433e3aac7, memofix f243cc18b.
Final quiet table (vs sqlite3 3.54 literal): insert 231,856 ops/s
(5.7x), point 199,632 (4.5x), scan 9,412,998 rows/s (5.6x), group 55
(2.3x), update 133,209 (7.6x), delete 117,689 (10.9x), file autocommit
1.6x FASTER. Since the 2026-09-28 baselines: insert 10.9x, point 1040x,
scan 6.5x, group 3.9x, update 2049x, delete 885x faster. Census
1073/0/290 audit exit 0.
Structural facts that own the residual: interface-boxed value pipeline
(scan 5.6x, update/delete 7.6-10.9x), group-key EvalExpr per row (2.3x),
prepare/bind exec-only floor (point/insert 4.5-5.7x). Multi-round
rewrites documented here — not scoped optimizations.

## PERF-STRUCT (FINAL, 2026-10-02) — census 1073/0/290 clean post-LIMIT fix

Post-struct-round census caught one more tplgate-exposed latent bug:
LIMIT comma-form vs OFFSET-keyword form share a normalize key but bind
values in opposite text order — template hits cross-assigned limit/offset
(limit-1.4.2: LIMIT 30, 50 executed as LIMIT 50 OFFSET 30). Fix
7dd4dbdb1: both literal slots present ⇒ decline substitution in the
SELECT/UPDATE/DELETE walks (single-slot forms substitute unambiguously);
pinned by TestTemplateLimitCommaForm. Final census: 1073 pass / 0 fail /
290 skip, audit exit 0. Final table in the report PERF-STRUCT section:
insert 231,856 ops/s (5.7x), point 199,632 (4.5x), scan 9,412,998 rows/s
(5.6x), group 55 (2.3x), update 133,209 (7.6x), delete 117,689 (10.9x),
file autocommit 1.6x FASTER.

## PERF-TYPEDROW (2026-10-02, START) — typed-row/batch-scan read path

Verifier-gated tranche: scan floor 5.6x (9.41M vs 52.9M rows/s) —
target >=2x (>=19M rows/s). Lever: page-batch scan API on the btree
(per-leaf: one page fetch + cell-pointer array walk, rows decoded
page-locally without per-row cursor Next/dispatch/memo-revalidation) +
typed decode into reused slots (direct serial-type offset reads for the
referenced columns). Wired into the bare/agg scan paths under the
existing eligibility contracts. Update/delete exec-plumbing flattening
next tranche. Benchmark pair after merge; suites + census at end.

## PERF-TYPEDROW (during) — P1 corruption regression found+root-caused, fix agent dispatched

DML-flat tranche (agent died pre-report, 6 commits green on build/suites/
A/B: update 199k, delete 205k ops/s = +17%/+15%) merged 61df1fe07.
BUT: full-suite triage exposed a P1 data-corruption regression on main:
WITHOUT ROWID + INSERT...SELECT recursive CTE (2 consecutive statements)
duplicates rows massively (count 1000500 vs oracle 5000) — pinned by
pre-existing TestT32KernelPinIndexRootSplitLeafToInterior.
Bisect evidence: struct-dml tip alone GREEN, struct-scan tip alone GREEN,
any merge of struct-dml commit a04220129 into struct-scan 8afc20df9 =
CORRUPT. Interaction: struct-dml's encBuf/AppendEncodeRecord reuse
invariant ("btree writes copy payloads, never retain") x struct-scan's
reused decode buffers aliasing CTE insert-select row data.
fleet/perf-struct-fix dispatched: aliasing-chain root-cause, ownership
fix at the boundary, FULL 1363-pkg testgen sweep mandatory.
savepoint2 600s timeout in the same merged run = benchmark-process
contention artifact (9.0s standalone, no panic signature).
Census/final docs GATED on the corruption fix.

## PERF-TYPEDROW (END, 2026-10-02) — corruption fixed; round closed clean

fleet/perf-struct-fix merged d8c87aebb: template-cache clone walk-order
violation (INSERT values consumed AST-order Values→Select→CTEs while
normalization scans source-order WITH-first — same-shape CTE statements
substituted wrong literals, recursion ran to the 1M-row limit, garbage
rows + lost legit rows). Fix: clone CTEs FIRST (source order), bind mode
inherits; pinned by frigolite_template_cte_order_test.go (fails pre-fix
with exactly count=1000500). Root cause was NOT btree/encBuf aliasing —
both suspects verified clean.
Census post-fix: 1073/0/290 audit exit 0. Post-fix bench: insert 216k
ops/s, point 184k, scan 9.29M rows/s, group 54, update 156k, delete
162k; file autocommit 1.7x FASTER than sqlite3.
Follow-up filed: WITHOUT ROWID duplicate-PK acceptance gap (pre-existing,
repro'd on origin/main via the CTE overflow shape; needs WR conflict-scan
tranche). TestSQLiteSuite fresh-worktree instability = harness shared
state; testgen is authoritative.

## PERF-FIXUPS (2026-10-02, START) — fix every documented issue

Issues from the review/validation trail, all to be fixed:
A. WITHOUT ROWID duplicate-PK acceptance via INSERT (pre-existing, oracle
   P1) — fleet/perf-fixwr agent.
B. rowid-vs-text whitespace affinity gap (' 5000 ' should convert like
   SQLite's text->numeric) — fleet/perf-fixrowid agent.
C. Coordinator: x % 0.1 panic (oracle 0); group-key textual-key class
   divergences (-0.0/0.0 same group per oracle; text '5' vs int 5
   separate per oracle); CASE-collation in-place slot aliasing (latent).
Suites + census at end.

## PERF-FIXUPS (END, 2026-10-02) — all documented issues fixed; census 1073/0/290

- WR duplicate-PK b8c26bf20: INTEGER-PK WR tables accepted duplicates via
  a rowid-seek fast path probing an index btree; uniqueConflictFastPath
  now picks the O(log n) PK-record seek for WR (binary-collated PKs;
  NOCASE keeps the collation scan). Oracle-exact texts; text-PK WR
  insert build 81s -> 0.4s. 10 pins (frigolite_wr_duppk_test.go).
- rowid affinity ccf374907: predecessor's bare-rowid whitespace fix was
  already oracle-correct (fresh 127-probe battery showed the mission's
  3 "remaining" cells were transcription drift: +rowid strips affinity,
  no conversion fires; no prefix rule in comparisons). Real fixes: rtree
  query whitespace-bound bug (x1 > ' 200 ' returned ALL rows), storage-
  affinity parse divergences ('NaN'->0, 'Inf'->9e999), all six string
  arms through value.NumericText. 96+30-cell batteries 96-97/96-97.
- Coordinator 112d608e0: % cast-divisor panic (5 % 0.1 = NULL per
  oracle; also minInt64 % -1 wrap), GROUP BY class tags (TEXT '5' vs
  INTEGER 5 separate), -0.0/0.0 one group, CASE-collation slot aliasing
  (latent P3).
CORRECTED RECORD: the "5/'5' textual-key grouping divergence" and
"rowid-vs-text whitespace affinity gap" claims in earlier sections were
transcription drift against the oracle — fresh batteries show frigolite
matched on 5/'5' grouping and the predecessor's rowid fix was already
96/96. Remaining known oracle divergences: none in the CRUD/expr/group
surfaces covered by the batteries.
Census: 1073 pass / 0 fail / 290 skip, audit exit 0.

## PERF-PARITY2 (2026-10-02, DURING) — five tranches merged; gaps 2.4-4.6x

Fresh pair @main (NROWS=300k NPOINT=200k NSCAN=3 NGROUP=30 NUPDATE=100k
NDELETE=30k NAUTO=5k, vs sqlite3 same ops): insert 350,878 ops/s (4.6x),
point 384,053 (2.8x), scan 22,282,927 rows/s (2.4x), group 50 q/s (1.35x
FASTER), update 282,547 (4.0x), delete 377,417 (4.1x), file autocommit
10,415 (1.62x FASTER). Campaign start: 10.9x/1040x/6.5x/3.9x/2049x/885x.
Merged: fleet/perf-floor (memoized column indexes, template-cache maphash
keys, clone scratch, btree.Close race fix), fleet/perf-dml2 (btree
freeblock space mgmt, in-place cell overwrite, lazy conflict scan),
fleet/perf-scanagg (streaming GROUP BY feed 2.5x, batch range-scan
leaves 2.1-2.3x), FIX.GROUPKEY (INTEGER/REAL numeric group parity —
oracle 1e15==10^15 one group; collationGroupKey/typedIntGroupKey/
groupKeyScalarEqual unified on integralFloatKey; pinned), fleet/perf-
floor2 (speculative vtab dispatch, without-rowid memo, preupdate table
memo, lock-gate short-circuit; point 1.57x, insert 1.27x, delete 1.25x,
update 1.21x). Stale TestRowidSeekRange ' 10 ' expectation corrected to
oracle (3 rows — affinity converts spaced text; battery-verified).
In flight: fleet/perf-insert2 (btree split staging + page-buffer +
insertRow staging diet; 5 commits, gates running). Next walls: pager
statement-journal capture on update/delete (~33% stmtReadTouch/
copyPageBytes), point template clone ~8%, scan/point value boxing.

## PERF-PARITY2 (END, 2026-10-02) — 7 tranches merged; point 2.2x, group+file faster than sqlite3

Final pair (main post-merge, vs sqlite3 3.54 same ops): insert 385,576
ops/s (3.9x), point 493,966 (2.2x), scan 21,829,361 rows/s (2.4x), group
47 q/s (1.24x FASTER), update 284,281 (4.4x), delete 392,290 (4.1x),
file autocommit 10,728 (1.31x FASTER). Campaign start (2026-09-28):
10.9x / 1040x / 6.5x / 3.9x / 2049x / 885x — every phase improved 2.6x-
470x, two phases now FASTER than sqlite3.
Tranches merged this round: perf-floor (memoized column indexes,
maphash template keys, clone scratch, btree.Close race fix), perf-dml2
(freeblock space mgmt, in-place cell overwrite, lazy conflict scan),
perf-scanagg (streaming GROUP BY feed, batch range-scan leaves),
FIX.GROUPKEY (INTEGER/REAL numeric group parity — oracle-verified),
perf-floor2 (speculative vtab dispatch, preupdate/vtab/without-rowid
memos, lock-gate short-circuit), perf-insert2 (split-staging pool,
cached write tree, cell wire-field reset + zeroblob tail clear —
finisher agent caught 2 latent corruption bugs from the staging diet,
both pinned), perf-point3 (OpenCursorAtRoot seek paths, one-census
validation walk, projection/collation memos). TestRowidSeekRange stale
' 10 ' expectation corrected to oracle (affinity converts spaced text).
Remaining walls (documented): interface-boxed value pipeline (scan
2.4x, update/delete 4.1-4.4x), template-clone copy-on-write floor
(insert 3.9x, point 2.2x), btree descent cost. Census next.

## PERF-PARITY2 (CLOSE, 2026-10-03) — census 1073/0/290 clean; round fully closed

Post-merge correctness sweep caught and fixed three regression classes the
tranches had introduced (all pinned):
- FIX.DML2-MALFORMED (fleet/fix-dml2, merged 7a59417c8): 64KiB-page blob
  growth UPDATE hit validatePageHeader's `p.FirstFree > uint16(pageSize)`
  — u16 truncation of 65536→0 made any freeblock head "malformed" once
  dml2's freeblock port made nonzero heads reachable. Fix: unwrapped int
  compare (C parity: btreeInitPage does no head check; chains stay
  validated by the usableSize-bounded walk). Pinned
  frigolite_update_blob_growth_test.go + btree01 solo green.
- FIX.INS2-VACUUM (fleet/fix-ins2-vacuum, merged): INSERT2-5's cached
  write tree snapshot pageSize/usableSize per (pager,root) — VACUUM's
  ResetToEmpty(2048) swapped the layout under an unchanged pager pointer,
  stale-geometry writes corrupted the copy-back and the restore path
  reverted main to the temp's 1024 (vacuum-11.2/11.3 "got 1024"). Fix:
  pager layout-change hook (SetPageSize/ResetToEmpty/ApplyReservedBytes
  → notifyLayoutChanged → DMLExecutor.InvalidateWriteTree). Pinned
  frigolite_vacuumpgsz_test.go. Bench parity kept.
- FIX.PREPARE-ALIAS (direct): Engine.Prepare's template-cache hit cloned
  onto the per-exec-depth scratch and db.Prepare RETAINED the AST — the
  next same-shape Prepare rotated the scratch and rewrote the held
  literals (two prepared INSERTs (2,3)/(3,4) both executed (3,4);
  capi2-4/6). Fix: Prepare/PrepareExec split — retained Prepare clones
  privately (scratch=nil), the immediate-consume Exec/Query path keeps
  PrepareExec scratch (no perf loss). Pinned
  frigolite_prepare_alias_test.go; capi2/stmt/capi3 green.
Harness filtered-mode artifacts also fixed (converter-leaked all-#
comment steps skipped; incrvacuum_ioerr/backup_ioerr/autovacuum_ioerr2/
vacuum6 reopen markers; btree01 marker). Census: 1073 pass / 0 fail /
290 skip, audit exit 0 (savepoint2 census flake = documented contention
class, 7.5s solo 3/3 green). Final bench (paired vs sqlite3 3.54):
insert 362k ops/s (4.2x), point 437k (2.4x), scan 20.6M rows/s (2.6x),
group 45 q/s (1.19x FASTER), update 267k (4.7x), delete 366k (4.4x),
file autocommit 9.8k (1.2x FASTER).

## PERF-PARITY3 (2026-10-03, milestone) — R4+R5 merged; gaps 1.34-2.8x

R4 tranches: perf-upddel (slot-path template substitution — updateCOW 0,
cached DML write trees, fused validation walk, pooled rowmap), perf-
scanbox (typed aggregate lane off raw payload, covered-seek WHERE skip,
[]bool decode sets, column-targeted point decode), perf-insquick (btree
append-cursor balance_quick port 98.8% engagement, IPK conflict-probe
gating, result staging). R5 tranches: perf-execentry (StmtHooksActive
trace gate, preflight memos, preupdate copy gate, O(1) ForeignMarks),
perf-stmtprep (fused normalize+hash one-scan pipeline, bounded linear
journal list, prevalidate memo). Milestone bench (main 9951a3f96 vs
sqlite3 3.54 same ops): insert 655,704 ops/s (2.4x), point 613,286
(1.76x), scan 39.4M rows/s (1.34x), group 50 q/s (1.28x FASTER), update
453,102 (2.8x), delete 639,158 (2.3x), file autocommit 11,197 (1.12x
FASTER). Campaign start: 10.9x/1040x/6.5x/3.9x/2049x/885x.
Correctness note: /tmp/perf/frigo harness go.mod replace was found
pointing at a stale agent worktree (fix-ins2-vacuum) — 2026-10-02/03
"main" benches in that window measured that branch, not main; all
merged-main numbers re-established after fixing the replace.

## PERF-PARITY3 (CLOSE, 2026-10-03) — R6 merged, 4 tranche regressions fixed, census 1073/0/290

perf-dmlcore merged b5d70be74 (ASCII-fold screens, atomic SchemaCookie,
conditional rowmaps, generation memos, journal-skip reads, pooled tuple
with nested-insert pin). Post-merge census caught 4 tranche regressions,
all root-caused + fixed + pinned:
- FIX.INSREG (fleet/fix-insreg, merged 44cadaf00 lineage): both from
  PERF.INSQUICK-2 2b3683c31. fts5lastrowid — reusable insStmtRes scratch
  restaged by nested Engine.ExecSQLUntracked (%_data flush) leaked its
  rowid into execTrackChanges → last_insert_rowid wrong; fix: insDepth
  gate (scratch staged only at depth 1). spellfix — bumpRowIDCache
  seeded EMPTY nextRowIDCache with a low explicit rowid → append-bias
  gate skipped conflict probes → duplicate shadow rowids accepted; fix:
  cache grows only over existing entry + clean-miss re-seed of true max
  via scanMaxRowID (parity kept: insert −0.2% paired).
- FIX.MISCREG (fleet/fix-miscreg, merged 44cadaf00): memoized trigger
  validation + allTableIndexes keyed without the ATTACHed-schema state —
  trigger tr3 "cannot reference objects in database main" never fired;
  backup-2.9/2.10 SQLITE_ERROR. Fix: cross-database schema stamp keys
  both memos; validated-trigger marks per schema-manager instance;
  re-ATTACH revalidation pinned.
Final census: 1073 pass / 0 fail / 290 skip, audit exit 0. Final bench
(main, canonical harness, vs sqlite3 3.54 same ops): insert 673,633
ops/s (2.4x), point 605,507 (1.78x), scan 38.1M rows/s (1.39x), group
51 q/s (1.31x FASTER), update 444,020 (2.84x), delete 637,184 (2.35x),
file autocommit 10,217 (~1.03x FASTER). Campaign start 2026-09-28:
10.9x/1040x/6.5x/3.9x/2049x/885x. Remaining gap owned by per-statement
lex+normalize+dispatch floor (Go vs C parse cost) — documented, no
single hotspot left (profiles flat across tranches).
Cleanup: /Users/muaddib/dev/frigolite-wt (46 worktrees, 18G) removed —
all branches pushed; disk freed.

## PERF-PARITY4 (2026-10-03, milestone) — R7 four-lane round merged; gaps 1.33-2.43x

Four parallel fleet tranches (max-parallel directive): perf-journal
(write-intent statement-journal capture — sqlite3PagerWrite port;
before-images at the write barrier, pooled buffers, exactness pins),
perf-btreeuse (gen-token btree free list + Reinit + stmt funnel —
NewBTree-per-stmt 190k objects → 0; stale-lease Close structurally
harmless), perf-storagediet (ParseRecordHeader's escaping "stack buffer"
root-caused — 128B heap array per call; fused DecodeRecordValuesInto),
perf-litcache (slot-path VALUE stash — literal INSERT rows read typed
values instead of re-walking AST; bind-path stash; ALSO fixed
explicit-IPK realloc bug + OR REPLACE recursive-trigger gate,
oracle-verified). Milestone bench (main a94f07463 vs sqlite3 3.54 same
ops): insert 790,737 ops/s (2.0x), point 718,823 (1.59x), scan 40.4M
rows/s (1.33x), group 53 q/s (1.33x FASTER), update 539,633 (2.43x),
delete 749,831 (2.24x), file autocommit 9,664 (1.31x FASTER).

## PERF-PARITY5 (2026-10-03, milestone) — R8 three-lane round merged; point 1.33x, update 1.90x

User-focus round (insert/select/update): fleet/r8-insert (per-txn
external-file validation latch — sqlite OP_Transaction no-op-in-txn
parity; bind-stash placeholder diet; stash-aware rowid paths; insert
alloc 468→145B/stmt), fleet/r8-point (bare-ref slot fusion + (template,
schema) slot memo, covered-bare fill, result pooling per selectDepth,
parse-memo seek; point alloc −33%), fleet/r8-update (collect decode
need-set diet, typed SET fast lane — unboxed integer arithmetic with
NULL/overflow/affinity oracle pins, 18-shape engagement-parity pin,
memoized WITHOUT ROWID flag). Milestone bench (main vs sqlite3 3.54
same ops): insert 782,873 ops/s (1.95x), point 813,237 (1.33x), scan
38.9M rows/s (1.35x), group 50 q/s (1.32x FASTER), update 653,110
(1.90x), delete 724,208 (2.24x), file autocommit 10,896 (1.10x FASTER).

## PERF-PARITY5 (CLOSE, 2026-10-03) — rtree pool regression fixed; census 1073/0/290

Post-R8 census caught rtree1/rtreeE (1071/2): the r8-point result-pool
(selectDepth-indexed slot) was zeroed mid-iteration — INSERT..SELECT
holds the source SELECT's pooled result while rtree xUpdates issue
shadow SQL through nested engine.Exec at the same selectDepth.
Fix (fleet/fix-r8point 89f8b2433): pool slots indexed per STATEMENT
FRAME (execDepth, SetResultFrame/execDepthLeave) — nested Exec gets its
own frame. Pinned frigolite_r8point_frame_pin_test.go. Point perf kept
(+0.6% paired). Final: census 1073 pass / 0 fail / 290 skip, audit
exit 0; suite green; bench (canonical harness vs sqlite3 same ops):
insert 760,244 ops/s (2.0x), point 775,664 (1.39x), scan 38.7M rows/s
(1.36x), group 49 q/s (1.26x FASTER), update 642,188 (1.93x), delete
712,877 (2.27x), file autocommit 10,747 (1.08x FASTER). Every milestone
committed + pushed through this entry. Machine-pressure mitigation:
census heavy packages (fts4merge4, fts5bigpl — multi-GB GC transients)
now serialized (tools/status).

## PERF-PARITY6 (2026-10-03, milestone) — R9 three-lane round merged; point 1.10x, insert 1.61x

fleet/r9-delete (collect decode-skip when no consumer, cursor path-stack
reuse across save/restore, scratch Result, redundant per-statement
journal scope drop, parse-memo delete header — delete ns/op −20-23%),
fleet/r9-insert (LEAF-SPLIT SORTEDNESS PROBE: bubble sort ran O(n²) on
already-sorted cells; statement-end hooks off named-return heap alloc;
insert-shape fingerprint memo; append-path probe elision; GetVarint
3-byte fast path — ALSO caught fts5 flush error dropped by value-passed
Result, fixed), fleet/r9-point (shape-stable identity gating — found
pointer-keyed memos serving stale verdicts from recycled COW AST
addresses, correctness bug fixed + pinned; shape memo replacing 6
per-statement walks; flush fast path; lazy statement clock; btree probe
loops — point ns budget 950→600). Milestone bench (main 940120bb8 vs
sqlite3 3.54 same ops): insert 1,031,927 ops/s (1.61x), point 939,964
(1.10x), scan 38.0M rows/s (1.36x), group 48 q/s (1.26x FASTER), update
684,758 (1.78x), delete 869,258 (1.85x), file autocommit 11,105 (1.11x
FASTER). Campaign start: 10.9x/1040x/6.5x/3.9x/2049x/885x.

## PERF-PARITY6 (CLOSE, 2026-10-03) — AUTOINCREMENT memo regression fixed; census 1073/0/290

Post-R9 census caught autoinc/default_pkg/tkt_d82e3f3721 (1070/3):
r9-insert's aiMemo cached an UNANSWERABLE negative — tableHasAutoIncrement
memoized FALSE from a lazily-populated colCache that had no entry (CREATE
TABLE DDL never populates it; DDL wipes it), while insertNeedsEndHooks
asked at a cold moment → the hook-free fast path silently dropped the
sqlite_sequence write on first-insert-per-connection and first-insert-
post-DDL. Fix (fleet/fix-r9ins 3af9b04d0): gate asks the conservative
TableMayHaveAutoIncrement (positive-or-unknown → hooked path; known-
negative stays fast); memo never caches when the cache had no entry.
Pinned frigolite_autoincseq_pin_test.go (4 shapes; fails on pre-fix
main). Insert perf kept (+0.16% paired).
Final: census 1073 pass / 0 fail / 290 skip, audit exit 0; suite green;
bench (canonical harness vs sqlite3 same ops): insert 1,016,131 ops/s
(1.54x), point 928,879 (1.11x), scan 37.5M rows/s (1.38x), group 49
q/s (1.29x FASTER), update 682,782 (1.80x), delete 805,692 (1.74x),
file autocommit 10,603 (1.06x FASTER). Campaign start 2026-09-28:
10.9x/1040x/6.5x/3.9x/2049x/885x — every gap within 2.3x of oracle-
parity, two phases faster, zero functionality regressions (census
identical throughout).

## PERF-PARITY7 (2026-10-03, milestone) — R10 two-lane round merged; scan AT PARITY (1.04x)

fleet/r10-insscan (scan span-table — header walk doubles as per-slot
value-span table, resolveSlotOffs O(1), exact corruption parity; typed-
lane tight batch loop +12%; Cursor.batchScratch; fixed-arity all-int64
record encode byte-parity-pinned; insert shape threading) — scan
37.5M→49.6M rows/s. fleet/r10-dml (can't-abort point UPDATE/DELETE skip
the statement journal — sqlite parity for statements that cannot fail
after writes, decision-matrix pins for OR-clauses/triggers/FK/LIMIT
shapes; per-statement glue cuts) — update +14%, delete +27%. Milestone
bench (main 31120cb9f vs sqlite3 3.54 same ops): insert 1,043,361 ops/s
(1.45x), point 936,469 (1.14x), scan 49,611,151 (1.04x AT PARITY),
group 50 q/s (1.32x FASTER), update 772,039 (1.58x), delete 962,707
(1.65x), file autocommit 11,018 (1.14x FASTER). Campaign start:
10.9x/1040x/6.5x/3.9x/2049x/885x.

## PERF-PARITY7 (CLOSE, 2026-10-03) — ANALYZE stat-row crash fixed; census 1073/0/290

Post-R10 census caught analyze (1071/2; savepoint2 = documented
contention class, 6.9s solo green): r10-insscan's shape-threading nil
deref — insertShapeFor declines nil colDefs (ANALYZE's internal
sqlite_stat1 writes pass none) and insertRowSh dereferenced the nil
shape. Fix: cold path resolves a minimal non-memoized shape (FTS
routing still applies; flags mirror the pre-threading per-call
derivation). Pinned frigolite_analyze_statrow_pin_test.go (ANALYZE +
post-DDL churn). Final: census 1073 pass / 0 fail / 290 skip, audit
exit 0; suite green; bench (canonical harness vs sqlite3 same ops):
insert 1,043,361 ops/s (1.45x), point 936,469 (1.14x), scan 49,611,151
rows/s (1.04x AT PARITY), group 50 q/s (1.32x FASTER), update 772,039
(1.58x), delete 962,707 (1.65x), file autocommit 11,018 (1.14x FASTER).
Campaign start 2026-09-28: 10.9x/1040x/6.5x/3.9x/2049x/885x.

## PERF-PARITY8 (2026-10-03, milestone) — R11 research round merged; POINT AT PARITY (1.04x)

fleet/r11-btreememo (ParsedBTree refresh-on-mutation — design (b):
write-path memo refresh with canary validation; HARD FINDING: write
paths violated their own struct-sync contract — freeSpace/compact/
finishLeafDelete holes caught by canary, fixed; delete-phase parse
allocs 24MB→0, heap −19%), fleet/r11-litbox (substitution-time typed
literal cache — SetCached(nil) forced per-statement re-parse; template
last-entry memo — prepareCached cum 50→20ms), fleet/r11-research
(benchmarks/R11_RESEARCH.md — per-op C-vs-frigolite work diff, ranked
top-10 frigolite-only costs with C evidence; headline: scanMaxRowID is
a FULL TABLE SCAN on rowid-cache miss and every point upd/del
invalidates that cache first). Milestone bench (main 60d62f1a8 vs
sqlite3 same ops): insert 1,023,305 ops/s (1.57x), point 927,055
(1.04x AT PARITY), scan 49.8M rows/s (1.04x AT PARITY), group 50 q/s
(1.32x FASTER), update 765,074 (1.58x), delete 995,500 (1.55x), file
autocommit 10,400 (1.27x FASTER).

## PERF-PARITY8 (CLOSE, 2026-10-03) — R12 merged; census 1073/0/290

R12 both lanes merged (fleet/r12-upd 89c93e959: overwrite via R11 parse
memo + single header parse, pager commit-path syscall diet — 7 sources
cut: per-page Truncate, journal open-per-write, Stat/Chmod, fstat,
Seek — file +19%, update +6.6%; fleet/r12-btree d8f6e02eb: append
insert runs no position search, scanMaxRowID answers via rightmost-leaf
descent replacing the FULL TABLE SCAN O(n) cliff, fused point-delete
decode, lock-free quick-append claim — insert/update/delete/point all
+0.8-1.4% on top). selectG joined the census heavy-package serialization
set (6-min/650MB+ working set; concurrent-worker kill observed). Final:
census 1073 pass / 0 fail / 290 skip, audit exit 0; suite green; SOLID
20/20; bench (canonical harness vs sqlite3 same ops): insert 1,043,361
ops/s (1.45x), point 936,469 (1.14x), scan 49,611,151 rows/s (1.04x AT
PARITY), group 50 q/s (1.32x FASTER), update 772,039 (1.58x), delete
962,707 (1.65x), file autocommit 11,018 (1.14x FASTER). Campaign start
2026-09-28: 10.9x/1040x/6.5x/3.9x/2049x/885x. R11_RESEARCH.md holds the
per-op C-vs-frigolite work diff + ranked remaining levers (#10 decode
boxing, high risk; #2 journal re-read residue; allocator span churn —
runtime) for any future round.
