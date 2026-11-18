# HANDOFF 2026-11-12 — the parallel-agent where phase

Restart index for the parallel-agent units. **TODO.md remains the live
queue**; this doc carries the fleet state and the merge protocol.

## Landed

- **S22 — 73a643fe4**: the seek/order cost model. Gate VERIFIED (33
  fixed/0 regressions). Where census 78 → 47.
- **S25 — 14e8ee855 (merge 3e4edb26b)**: wherelimit2 4 → 0; gate 5
  fixed/2 flakes/0 regressions → 1430.
- **S23.1+S23.2 — f3ec10698/7a3a28e6f (merge 510efa6b8)**: the join
  FROM-loop picks + INDEXED BY/NOT INDEXED; gate 8 fixed → 1422 (census
  36).
- **S26 — the write-path corrupt contract**: where-25.1/25.2 + 9 bonus
  DML fixes; **25.5 carded** (the WR index PK-tail format unit,
  portplan/PLAN_S26_dml_corrupt.md).
- **S24 — c3a0a7e9f (merge e746f48db)**: whereH 5 → 0 + where2-16.2
  (3.54's hasRightCrossJoin rule); gate 9 fixed/0 regressions → 1413.
- **S23-cont — a85d3cd9b…5436b135e (merge 0daa3073c)**: the join ORDER
  BY delivery (wherePathSatisfiesOrderBy port), the OR-arm execution
  order, the correlated-bound seek, where-19.0 + where2-12.1, where8 15
  → 0, where9 → 0. **The where census after the S23 merge: 3 leaves**
  (where-25.5 carded, where2-2.5/2.5b the random-EXPLAIN class).
- **S29 — 1cbe11121 (merge 62b1f8790)**: regsub -all + the fts3expr3
  regen (52 → 0, single .gz) + fts3 argv coercion + depth 12 +
  CAST/truthiness/var-limit (e_expr 9 fixed) + the A4 NUL harness fix.
  Its gate: 39 fixed, ONE regression (existsexpr-4.4.2 — a stale-collation
  seek) → FIXED by the **seek-collation gate** (96b624803,
  collectBinaryRef + C's sqlite3IndexCollSeq rule; verified by family
  replay + a pre/post bisect). The S32 gate then surfaced the
  **temptable2-9.2.x small-cache stress dither** (bisected PRE-EXISTING
  at 62b1f8790 — 5/5 fails there too; the four earlier gates drew clean
  samples; watchlist).
- **S30 — d0cd72e23 (merge 0692e6e27)**: pager1 U1-U4 (subst-foreach,
  __STEP_ column text, URI collapse + db_status, readonly surfaces) —
  pager1 99 → 89; two C-pinned engine deltas (the WAL heap-memory
  locking_mode refusal, the btree empty-root rule); U3's 4.5.6/4.7.2/
  4.8.x await U6/U7 (the journal substrate + tvfs snapshot).
  **The S30 merge gate is in flight** (/tmp/full_s30_gate.log; diff
  target /tmp/full_s32_gate_fails.txt, 1388 = 1375 - existsexpr + 14
  temptable2 stress dither).

## The fleet (2026-11-14)

| Unit | Worktree | Branch | Scope | State |
|------|----------|--------|-------|-------|
| S30 pager1 U1-U4 | /private/tmp/wt_pager | s30-pager1 | the converter/harness-only 14 leaves (subst-foreach, __STEP_, readonly, URI collapse) | running |
| S23-cont | — | s23-join | join machinery | **DONE + MERGED** |
| S29 expr | — | s29-expr-unit | expr slices E3/E1/E2+S1/S2 | **DONE + MERGED** |
| S24 choose | — | s24-choose | candidate choice | **DONE + MERGED** |

Carded queue: where-25.5 (the WR index PK-tail format unit), where2-2.5/
2.5b (the random()-EXPLAIN class), expr S5+A1+A8+D1 (11, converter
batch), the fts3* residue 54, pager1 B5/B4/B7 engine buckets (U5-U7:
61+13+11), where2-12.1 RESOLVED by S23-cont's P3c.

## The fleet (2026-11-12, all in dedicated worktrees — NEVER share a tree)

| Unit | Worktree | Branch | Scope | State |
|------|----------|--------|-------|-------|
| S23 JOIN | /private/tmp/wt_s23 | s23-join | join inner loops, FULLSCAN_STEP gating, per-loop ORDER BY | **S23.1+S23.2 MERGED** (510efa6b8; census 47→40 per the branch's frame); continuation agent running for where 6.15/14.x + residue (fresh agent — the pause expired the old task; /private/tmp was purged, uncommitted work lost, rebuild worktrees from branch refs) |
| S24 CHOOSE | /private/tmp/wt_s24 | s24-choose | whereH 5 + where2 12.1/16.2 — candidate choice | worktree rebuilt at 39d6bba9d; agent relaunch QUEUED (hold until the S23 merge gate finishes — ≤2 agents + gate) |
| S29 expr unit | /private/tmp/wt_expr | s29-expr-unit | TRIAGE_expr slices E3+E1+E2 (46) then S1+S2 (9) | running (fresh agent) |
| S25 LIMIT | — | s25-limit | wherelimit2 4 | **DONE + MERGED 3e4edb26b** (merge gate: 5 fixed incl. selectH-3.7 bonus, 2 flakes, 0 true regressions) |
| expr triage | — | s27-expr-triage | read-only census | **DONE + MERGED bc3f3d810** (expr truth = 3 leaves; 98-leaf substring table) |
| pager1 triage | — | s28-pager1-triage | read-only census | **DONE + MERGED 904efda02** (99 leaves, 7 buckets, slices U1→U7) |

S25 merge-gate baseline: /tmp/full_s25_merge_fails.txt (1430 names).
Flake watchlist (lessons): temptable2-4.1.1, where2-2.4 (probabilistic
random()-distinctness; 6/6 isolated green; chase only if it recurs).

Concurrency lesson: the user concurrency limit killed 4 agent runs at
peak (3 agents + gate + main). **Keep ≤3 agents while a gate runs, ≤4
without one; resume killed agents via SendMessage (their context
survives).**

## Merge protocol (order matters — file overlap is explain_plan.go/select_order_search.go)

1. ~~S25 LIMIT~~ **DONE** (merged 3e4edb26b; gate verified 5 fixed/2
   flakes/0 regressions vs /tmp/full_s22_gate3_fails.txt).
2. S23 JOIN merges next (`git merge s23-join` on feature/perf), then
   the FULL gate (-v -timeout 40m) + name-set diff vs
   /tmp/full_s25_merge_fails.txt (python sets, never comm).
3. S24 CHOOSE rebases over the S23 merge, then merges + one full gate.
4. S29 expr-unit merges last (disjoint territory; still gets a gate).
5. Each merge: append-only lessons consolidation, TODO.md census
   update, plan-file status lines.
6. S26 MISC (where 19.0 EQP autoindex names + 25.x corruption) runs in
   the MAIN tree AFTER the S23/S24 merges — its territory (explain EQP
   rendering) overlaps them.

## Baselines

- /tmp/full_s25_merge_fails.txt — the current full-suite failure name
  set (1430, TestSQLiteSuite/-prefixed) — DIFF TARGET for the next
  gates.
- /tmp/full_s22_gate3_fails.txt — the S22 gate set (1433).
- /tmp/full_s21_fails.txt — the S21 pre state (1466).
- /tmp/w_s22_f.txt — STALE (62 lines, a mid-session snapshot) — ignore.

## After the where classes close

expr 109 → pager1 101 (the triage docs guide the slices) → the WAL
residue classes (HANDOFF_2026-11-04_WAL_RESIDUE.md) → the carded
converter classes → the performance phase (F1c/F3 lanes in
GAP_CLOSURE_PLAN_2026-10-31.md).

## 2026-11-16 — the pager arc (S31/S33/S34) COMPLETE, gate verified

- **S31** merged 51a09c00b (S31.1→S31.8): pager1 **89 → 0** (957 leaves).
  S31.6 failed-COMMIT abort path; S31.7 hot-journal substrate (recovery at
  the per-statement shared-lock point, C's three-way classification,
  file-healing playback); S31.8 super-journal engine
  (internal/pager/superjournal.go) + faultsim/testvfs harness machinery.
  Also really fixed alter2 (13), analyze-5.99, fkey6 (2), index3-99.1,
  trigger7-99.1, main-2.0#01, tkt-f3e5abed55-2.5 (select_scan rewrite;
  verified fail-at-dd045611d / pass-at-HEAD across two gates).
- **The 1355 baseline was UNDERCOUNTED** (faultsim NUL corruption ate 75
  lines from a strings-style extraction): python raw byte scan of the same
  raw log gives 1430 = /tmp/full_s33_true_fails.txt. The first S31-merge
  diff looked like +310 NEW; bisect at dd045611d showed e_dropview's 14
  failing there too. Lesson recorded: raw-scan only + bisect suspicious
  GONE/NEW sets at the base commit.
- **S34** (s34-regress, merged): S31's ONE true regression —
  hotJournalProbe's nPage==0 guard read the in-memory page count
  (schema.Init materializes page 1) where C's pagerPagecount reads the
  FILE size. pragma2-5.1/5.3 + sharedA-1.1/1.2 fixed; sweep 46→42 name-set
  clean. Cards: rollback/close journal-file residue, close-mid-txn flush.
- **S33** (s33-wr-index, merged e8675d3de): the WR index payload format =
  C's [indexed..., PK tail...] at every write site (dup elision, tail
  collations, DESC), comparator extension, **where-25.5 recheck enabled
  and green** (where 5→3), integrity un-exemption + the join WR guard,
  probe parity byte-verified vs oracle 3.54. WR table trees are
  index-shaped — three scan sites skipped one row per divider (fixed).
- **Combined gate VERIFIED** (/tmp/full_s33s34_merge_gate.log): 1430 →
  **1317, NEW=0**, all 113 GONEs accounted (89 pager1 + 4 S34 + 18 scan
  fixes + atof1-2.40 the documented flake). New diff target:
  /tmp/full_s33s34_merge_fails.txt (1317).
- Next queue: the WAL residue classes → the carded converter classes →
  the performance phase.

## 2026-11-17: S37 + S41 landed (authorizer events + the EXPLAIN-row substrate)

**State: feature/perf @ afe2e21a6, full-suite truth 1268 failing leaves**
(/tmp/full_s41_merge_fails.txt — composed from the S41 gate raw scan 1281
plus three fresh family sweeps folding in the 13 distinctagg hand-pin
greens; vs the 1317 baseline: 61 GONE, 12 NEW, all residual cards).

- **S37** (merged): authorizer/update-hook event collection — the
  compile-time FK event walk (auth_events.go, gated on ForeignKeys &&
  HasAuthorizer), per-column SQLITE_READ + ROWID, schema-name arg3,
  harness authorizer global sync via the S25 push/pull pattern,
  converter update_hook branch + main.go whitelist + harness handler,
  hand-pinned runtime_var_reset fixture steps. 15 leaves: savepoint-9.x
  (3), without_rowid3-18.x (5), tkt-f7b4edec (2), fkey2-18.x (5 bonus).
- **S41** (merged): the EXPLAIN-row substrate, 4 stages — the engine row
  stream (explain_rows.go; UPDATE dispatch ported from update.c:564-585
  with indexColumnIsBeingUpdated), the explain() TVF (eponymous module +
  real-object shadow guard), the harness surfaces (index9 19→0
  hand-pinned with C's per-case $y restores; bestindexF 34→17; vacuum2
  4→2; indexexpr2 9→5; where2-2.5/2.5b GONE at Stage 1), distinctagg
  hand-pins (13). 61 GONE = 44 S41 + 17 riding the S37 merge (incl.
  tpch01-1.1b/1.1c). Residual cards: distinctagg ×5
  (DISTINCT-ordered-input), trigger7-2.x ×6 (trigger sub-program rows),
  misc3-6.11-utf8; bestindexF eDistinct/xBestIndex-globals → S43.
- S41 plan: portplan/PLAN_S41_explain_rows.md (STATUS per stage); S37
  plan: portplan/PLAN_S37_auth_events.md.
- **Restart point**: S35abc WIP on branch s35-wal-small @ 82854664a
  (worktree /private/tmp/wt_s35impl) — S35a lifecycle flip mid-
  verification (36 non-target families), then S35b/S35c per
  PLAN_S35abc.md; then S35d-g per TRIAGE_wal_residue_2026-11-16.md;
  then S42/S43 per TRIAGE_index_callback_2026-11-16.md.

## 2026-11-18: the S35a-d WAL small-slices arc complete (1317→1218 across the day)

**State: feature/perf @ 4bb2a2911, full-suite truth 1218 failing leaves**
(/tmp/full_s35_merge_gate.log; vs the 1268 baseline: 52 GONE — 50 real,
commit-attributed, +2 resetdb load flaps that re-run red — NEW=2, both
documented dithers: temptable2-4.1.1, walfault-15's flap).

- **S35a** the lifecycle-for-named-db pre-scan flip + the pager4
  rename_file pin (pager4 5→0; wal9/walcrash2 + 24 same-class artifacts).
- **S35b + S35c.1/.3** (15c7e8789): sync-flag levels (0x0A), the
  walmode-4.9 BUSY propagation, the SetPageSize double-refusal, the
  subjournal backup pin, the large-sector write rule + changecounter
  rider, the missing-main garbage rule. **31 adjudicated ride-alongs**
  across 11 non-wal families (exclusive/snapshot3/snapshot4/schema/
  schema2/parser1/e_changes/e_blobclose/multiplex/tkt-d82e3) — verified
  fixed at 15c7e8789 by pre-commit probes, mechanism = the page_size/
  journal-mode/exclusive-WAL surfaces.
- **S35c.2** (08897585f): the two-path wal-index header split (real-shm
  trusts; heap re-validates salts+capacity and rebuilds). Byte-identical
  wal-universe (74=74).
- **S35d** (15d293152/a9e1d2b3f): the NOOP checkpoint mode + the in-txn
  SQLITE_LOCKED veto + register_function's fixture-proc interpreter round
  trip + binary format c* + the converter's braced-body proc collection.
  walckptnoop 7→1 + tkt-f7772-1.2.
- **New cards**: the btree PAGE-DENSITY divergence (bulk-insert WAL
  mxFrame 451 vs C's 298 — own unit), the walfault-15 cross-case JOIN
  flap (isolation-pass/family-fail signature in lessons), walfault-11's
  trial-cap (647s/case), the resetdb load flap (watchlist).
- **Restart point**: S35e (no-shm WAL, B1) → S35f (wal-hook, B10) →
  S35g (error-injection + faultsim converter) → the density unit →
  S42/S43. Plans: PLAN_S35d_walckptnoop.md (STATUS), PLAN_S35abc.md
  (STATUS); lessons: the S35b/c/d records.

---

## 2026-11-18: S35e delivered — e_wal 10→0 (146a3c00e), the no-shm WAL class complete

The C choreography was pinned from source (wal.c/pager.c/btree.c/parse.c —
PLAN_S35e_noshm.md §UPDATE has the full chain): the WAL entry for a header-2/2
db runs at the first SCHEMA-NEEDING statement's parse (a bare PRAGMA never
calls sqlite3ReadSchema), so `PRAGMA locking_mode=EXCLUSIVE` in the same
execsql as the first SELECT opens the WAL with bNoShm=1 (heap index, no -shm).
Three engine pieces landed:

1. **The per-statement refusal gate** (execDBFileChecks →
   Pager.WALAttachRefusal, pinWAL-gated): the open-time refused attach must
   fail every btree-touching statement with CANTOPEN — the ReadPage hook alone
   missed it while the schema caches stayed warm.
2. **The heap conversion at the latch** (walConvertHeapEligibleLocked):
   swap the registry index for the heap form + remove the -shm + re-validate
   from the -wal file, windowed on `walDataTouched` — set at the first
   statement-level WAL entry (the gate's refresh / WALBeginWrite),
   with **SuppressAccessMark** bracketing the open's own schema load and the
   harness reopen prime (`SELECT name FROM sqlite_master LIMIT 1` —
   open-adjacent machinery C's driver never runs). The discriminator cost a
   second iteration: page-scope (page 1 vs >1) broke wal2-6.6.1 (its real
   page-1-only SELECT must CLOSE the window — the later locking_mode=NORMAL
   must work, else the exclusive file-lock mark wedges 6.6.4's db2).
3. **The EXCLUSIVE file-lock hold**: lockreg.walFileShared (the NORMAL-WAL
   persistent file-SHARED — pager.c:2158's unlock-skip quirk; blocks only
   EXCLUSIVE acquisitions) + e.walExclusiveFileHeld (the take at the
   conversion and at WAL+exclusive writes; released by clearPersistentShared;
   survives the journal_mode=DELETE exit per pager.c:7663).

Verify: e_wal 10→0; a 27-family leaf-set diff against a 6b7513599 worktree
run — every family identical; full gate **1207 leaves** with the durable
baseline committed at portplan/baselines/s35e_gate_fails.txt (/tmp baselines
proved ephemeral across sessions — the old 1218 file was lost). The census
block in TODO.md now carries the truthful per-leaf counts (the old grouped
counts mixed substring filters).

Restart point: **S35f** — the wal-hook surface (e_walhook, 10 leaves): the
db wal_hook converter + the commit-end hook with rc propagation +
wal_autocheckpoint overwrite (TestWalHookEngine's substrate). Then S35g
(error-injection + the faultsim converter + the walfault-15/11 cards), the
btree page-density unit (walckptnoop + resetdb-300's 13-vs-12 page_count),
S42/S43, carded converter classes, the perf phase.
