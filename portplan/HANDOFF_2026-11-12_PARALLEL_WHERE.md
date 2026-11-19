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

---

## 2026-11-18 (later): S35f delivered — e_walhook 10→0 (cae6073ce), the wal-hook surface complete

C's doWalCallbacks position pinned from source: the sqlite3_wal_hook fires
at the committing statement's COMPLETION (vdbeapi.c:723-742, on
`rc==SQLITE_DONE && db->autoCommit`) — after the commit and its write-lock
release — per btree via the read-and-clear of the log's TOTAL frame count
(wal.c:4237 sets iCallback=iFrame; 4415 hands it over; R-18913-19355's
nEntry is not the per-commit delta). The hook's raw-code return fails the
statement (sqlite3ErrStr's canned text — 1/5/14) while the commit stands.
Three layers landed: the engine dispatch (fireWalCallbacks at Engine.Exec's
success tail + walHookErrStr + the C-shaped hook signature + the
wal_autocheckpoint replacement), the converter (the register_wal_hook step,
re-registration on hook-proc redefinitions — C's TCL late binding — the
top-level global literal-set emission), the harness (the hook interp's
sqlite3 opener for 2.1's in-hook db2 read, the runtime_var expect
evaluation with wal_frame_count, the lifecycle promotion scan for hook
procs opening test.db).

ALSO: latent internal/pager unit breaks fixed (the tests had not run since
the WAL-slice work — the harness gates alone let them rot): the WAL-mode
AllocatePage nil-header panic, TestWalCheckpoint's premise + the RESTART
observables, the pager-level checkpoint seam's read-mark deadlock, and the
new TestWALCallbackEntry (read-and-clear). LESSON: run `go test
./internal/...` in every unit's verify step.

Verify: e_walhook 10→0; the 13-family WAL universe leaf-identical; full
gate **1198 leaves** (NEW=1: where2-2.4 — the documented S41-era flap,
green in isolation); the durable baseline at
portplan/baselines/s35f_gate_fails.txt.

Restart point: **S35g** — error-injection surfaces + the faultsim
converter (sqlite3_interrupt/memdebug_fail/xShmMap scripts, the restart/exit
lock choreography, walfault2's do_faultsim_test emission; the walfault-15
cross-case JOIN card and walfault-11's trial-cap). Then the btree
page-density unit (walckptnoop + resetdb-300's 13-vs-12 page_count), S42/S43,
the carded converter classes, the perf phase.

---

## 2026-11-18 (latest): S35g slice 1 delivered (ae4ccd13ae) — walfault2 2→0, the walsetlk native port, the recovery restructure

Full-suite truth **1193 leaves** (the gate /tmp/s35g_full_gate2.log; the
durable baseline portplan/baselines/s35g_gate_fails.txt). The arc so far:
e_wal 10→0 (S35e) → e_walhook 10→0 (S35f) → walfault2 2→0 + the
walsetlk_recover native port (S35g slice 1). The census block in TODO.md
carries the per-family truth.

Key mechanisms this slice:
- The recovery SPLIT: the -wal file reads (the xRead sites) outside the
  wal-index writer section (C's private-buffer shape) — the section-
  enclosed form remains for the commit-path callers. The endianness bit:
  the WAL magic's checksum-order flag is the value's low byte (buf[3]).
- PRAGMA busy_timeout implemented (it had NO handler) + wired to the WAL
  layer; the busy sleeps emit xSleep; the recovery's reads emit xRead.
- The faultsim runner: close_db + the URI reopen (the shared-mem URIs
  route to the shared-cache machinery; everything else resolves inside
  the fixture dir — the first cut leaked to the process CWD and
  pagerfault-36 caught it in family context).
- walsetlk_recover superseded by TestWalSetlkRecoverEngine (the
  testfixture_nb class): the concurrent recovery holds the
  WRITER+RECOVER locks, the second connection times out into "database
  is locked" with xSleep count > 0, the post-recovery read succeeds.

Restart point: **the S35g remainder** — walfault-11 (the trial-cap, 789s
per run), -12 (the fixture-registry threading into runFaultsimStep: the
prep's register_testvfs/testvfs_config), -15 (the exclusive-INSERT lock
interplay with the trial state — passes in isolation, fails in family
context: the JOIN card). Then the btree page-density unit (walckptnoop +
resetdb-300's 13-vs-12 page_count — this session's resetdb-300 was green
once: the density divergence may be trial-sensitive), S42/S43, the carded
converter classes, the perf phase.

---

## HANDOVER 2026-11-18 (mid-flight): walfault-12 threading — the working tree holds UNCOMMITTED, COMPILING groundwork; the trial-runner wiring is half-applied and REVERTED to compile

**Branch feature/perf @ the S35g-slice-1 commit chain (ae4ccd13ae + docs
9b628d3b7). Full-suite truth 1193 leaves
(portplan/baselines/s35g_gate_fails.txt, gate /tmp/s35g_full_gate2.log).**

### The uncommitted working tree (all of it COMPILES: `go vet .` green,
`go vet ./tools/...` green):

1. **tools/tclconvert/tcl/interp.go** — decodeBackslashEscape gained TCL's
   OCTAL escapes (`\ddd` 1-3 digits → the raw byte): `\000` is a NUL
   (the old code dropped the backslash and kept the digits — "000").
   substituteExprAtoms' escape site now routes through the same decoder.
2. **tools/tclconvert/tcl/interp_stmt.go** — commandTestVFSConfig captures
   `tvfs shm FILE VALUE`'s VALUE into Stmt.ShmSeed (walfault-12's prep:
   `shmfault shm test.db [string repeat "\000" 40]` — 40 NUL bytes).
   A temporary TCLCONV_DBG trace print is STILL IN (remove before commit).
3. **tools/tclconvert/main.go** — the JSON TestStep carries ShmSeed
   (`json:"shmSeed,omitempty"`) + the Stmt→TestStep mapping.
4. **internal/pager/wallocks.go** — CorruptWalIndex generalized to
   CorruptWalIndexWith(seed []byte) (the seed writes into the registry
   index's page0 + persists to the -shm; CorruptWalIndex delegates with
   its "blah..." bytes).
5. **testdata/walfault.json** — REGENERATED with the seed: walfault-12's
   prep's testvfs_config step now carries `"shmSeed": "\u0000"×40`.
6. **frigolite_harness_test.go** — the TestStep struct gained ShmSeed.
   **A stray `namedDBList` helper was appended at the file tail** (used by
   the reverted runner patch — keep or remove with the wiring).

### The HALF-APPLIED piece (REVERTED to compile — the exact remaining work):

The trial-runner fixture threading. The intended patch (write it in one
python block whose asserts ALL pass before the write — the failed attempt
aborted before the write, leaving only the main-loop call line edited,
which I reverted):

1. `runFaultsimTest` signature += `fixtures map[string]*testvfsFixture,
   defaultVFS *string`; its prep/body `runFaultsimStep` calls pass them.
2. `runFaultsimStep` signature += the same two; new cases:
   - `register_testvfs`: create the fixture in `fixtures` (isDefault from
     step.SQL=="default", noShm from noshm/iversion, devcharBits -1); on
     default set `*defaultVFS`.
   - `testvfs_config`: `sql=="shm"` → resolve step.Variable against
     fixtureDBDir; for *dbp + namedDBList(namedDBs) whose .path matches,
     `CorruptWalIndexWith([]byte(step.ShmSeed))`; else
     `fx.configure(...)` + the default-script install.
   The existing cases (faultsim_*/reopen_db/reset_db/register_function/
   delete_*/open_*/close_named_db/prepare_stmt/stmt_step/exec) stay.
3. The main loop's `case "faultsim_test":` call passes
   `testvfsFixtures, &testvfsDefault` (the one-line change I reverted —
   re-apply it LAST so the tree compiles between steps).
4. Verify: `go vet .`; `FRIGOLITE_TEST=walfault-12` → the leaf must run
   the trial grid (the prep's shm seed zeroes the wal-index header — the
   pager's CorruptWalIndexWith — and the ioerr injection fires during the
   checkpoint's recovery); then `FRIGOLITE_TEST=walfault` (walfault-11 is
   the 789s trial-cap — it blocks the family sweep; budget ~15 min);
   the 15-family identity diff vs
   portplan/baselines/s35g_gate_fails.txt (walfault2/walsetlk_recover are
   GONE there; walfault-12 joining them is the win); `go test
   ./internal/...`; the full gate; commit.

### The carded-after state (unchanged):

walfault-11's trial-cap (789s/run — its trial grid burns the full budget),
walfault-15 (the exclusive-INSERT lock interplay — passes in isolation,
fails in family context: the JOIN card), then the btree page-density unit
(walckptnoop's 451-vs-298 frames; resetdb-300's 13-vs-12 page_count —
green once this session: possibly trial-sensitive), S42/S43, the carded
converter classes, the perf phase.

---

## 2026-11-19: S35g slice 2 delivered — walfault-12 GONE (walfault 3→2)

The four-step recipe completed, plus the two gaps the regenerated JSON
exposed:

- **The wiring** (per the recipe above): runFaultsimTest/runFaultsimStep
  carry `fixtures map[string]*testvfsFixture, defaultVFS *string`; the
  trial-scope `register_testvfs` (verbatim re-registration, isDefault →
  *defaultVFS) and `testvfs_config` cases (sql=="shm" → CorruptWalIndexWith
  on every open connection whose path matches the fixtureDBDir-resolved
  FILE; else fx.configure + the default-script install). The main loop's
  faultsim_test call passes `testvfsFixtures, &testvfsDefault`.
- **The regen's hidden emissions** (the step the recipe missed): the
  b0c131023 walfault.json regen carried the CURRENT converter's full
  vocabulary — walfault-10's prep gained `runtime_expr stmt_1 → stmt`
  (the OLD json predated the emission; the leaf died "unhandled prep
  step runtime_expr" although untouched). Fixes: the trial-scope
  `runtime_expr` case (a bare identifier naming a trial-prepared stmt
  binds the HANDLE TOKEN; else evalHarnessRuntimeExpr; empty = the reset)
  + `wal_checkpoint_rc`/`wal_checkpoint_v2` (test1.c:7609-7612: the
  simple command returns TCL_OK carrying t1ErrorName — the C body's `if
  {$rc != SQLITE_OK} {error [sqlite3_errmsg db]}` gives the pair {0 {}}
  on success (the `if`'s false-result is EMPTY) and {1 {disk I/O error}}
  on ioerr, matching faultsim's tolerated errlist) +
  evalFaultsimTestScript threads runtimeVars into the -test interp
  (walfault-10's `sqlite3_finalize $::stmt`), reserved testrc/
  testresult/testnfail set after (they win collisions).
- **The seed**: the committed regen had captured the STEP shape but an
  EMPTY seed (the capture chain predates the octal escapes) —
  regenerating with `-only walfault` after the escape fix produced the
  true 40-NUL shmSeed. Lesson: a regen must be re-run whenever ANY link
  in its capture chain changes, and the committed artifact re-diffed.
- **Verify**: go vet .; the walfault family sweep (/tmp/wf12f2.log):
  only walfault-11 + walfault-15 fail (both carded, baseline), walfault-12
  PASSES; `go test ./internal/...` EXIT=0; the identity diff vs
  portplan/baselines/s35g_gate_fails.txt — the FRIGOLITE_TEST=wal
  superset (21 families — "wal" matches every wal* file, a STRONGER net
  than the 13 separate runs) + journal2 + subjournal:
  **GONE=exactly walfault-12, NEW=0** (45 baseline leaves in run
  families → 44).
- **Note**: FRIGOLITE_TEST is a SINGLE substring (strings.Contains on the
  file base) — the "walfault-12" filter matches NO file; family truth
  comes from the case paths. walfault2 rides the "walfault" filter and
  stayed clean.

**The carded-after state**: walfault-11 (the trial-cap ~737-789s/run),
walfault-15 (the JOIN card), then the btree page-density unit
(walckptnoop's 451-vs-298 frames — the 5 baseline leaves re-confirmed in
this sweep), S42/S43, the carded converter classes, the perf phase.

---

## 2026-11-19 (later): S35g slice 3 delivered — walfault-15 GONE (walfault 2→1)

The JOIN card was a CROSS-LEAF LOCK RESIDUE, not an exclusive-INSERT
semantics gap:

- **The chain**: walfault-11's trials open db2 per trial (`sqlite3 db2
  test.db -vfs shmfault`); the leaf errors at the 512-trial cap with db2
  STILL OPEN (the harness's leaf transition manages only *dbp). db2's
  WAL-mode participation hold (lockreg WALFileShared, the S35e
  pager.c:2158 unlock-skip model) persists; walfault-15's prep then sets
  locking_mode=exclusive and BEGIN+INSERTs — the locks.go:319 gate sees
  WALFileSharedByOther(test.db, db2) → "database is locked" in trial 1's
  prep.
- **The C contract**: do_faultsim_test runs the fault-class lifecycle —
  the shmerr/fullerr/cantopen classes' -injectuninstall (malloc_common.tcl)
  runs `catch {db close}; catch {db2 close}; shmfault delete` at the leaf
  end; the ioerr classes define NO uninstall. C's db2 closes at -11's
  end; ours didn't.
- **The fix**: runFaultsimTest derives `uninstall` from the class names
  in step.SQL (shmerr/fullerr/cantopen) and, in a defer covering every
  exit, closes *dbp + every namedDBs entry + deletes the shmfault
  fixture. Gated: the ioerr-class files (walfault2, pagerfault's grids)
  skip the branch by construction.
- **The diagnosis trail** (for future lock-hunt): the native repro could
  NOT reproduce the family failure (the pure -14→-15 sequence passes) —
  the poison needed the -11 leaf. The pager busy-escape instrumentation
  (FRIGOLITE_BUSY_TRACE) exonerated ALL the WAL shm lock paths (0 hits in
  a 737s family run), pointing at the exec/locks.go lockreg layer. The
  C ground-truth chain: e_wal 3.2.5's persistent hold needs a successful
  WAL entry (a shmerr-blocked open unlocks normally — the hold needs the
  pre-WAL schema-load SHARED surviving the txn ends); the
  sqlite3PagerLockingMode set is just a FLAG (pager.c:7329) — the file
  EXCLUSIVE takes at the NEXT sqlite3PagerBegin (pager.c:5924-5933).
- **Verify**: the walfault family sweep → only walfault-11 (the carded
  trial-cap) fails; the pagerfault family (pagerfault-6.2 rides the
  cantopen class) 0-fail; go test ./internal/... EXIT=0.

**The carded-after state**: walfault-11 (the trial-cap ~737s/run — the
next unit: why its nfail never clears across 512 trials), then the btree
page-density unit (walckptnoop's 451-vs-298 frames), S42/S43, the carded
converter classes, the perf phase.

---

## 2026-11-19 (later still): S35g slice 4 delivered — walfault-11 GONE; the walfault family is COMPLETE

- **The trial-cap mechanism**: the shmerr class injects on the SHM
  METHODS, not file I/O. C's test_vfs ioerr group counts the fixture's
  FILTERED method calls at entry (per-VFS, script or no script), failing
  the nFail-th (persist: every call from it on) with SQLITE_IOERR. The
  harness ran the pager's file-I/O injector instead: -11's body
  (db2's checkpoint over the ~8000-frame setup WAL) does ≥512 I/O ops per
  trial — iFail 1..512 always landed inside the I/O stream, nfail never
  cleared, the grid hit the 512-trial cap (737s).
- **The fix**: the testvfsFixture gained the C-level counter
  (ioerrArmed/Persist/Fail/Count/Fired + ioerrStart/ioerrStop) checked at
  the hook's ENTRY (before the script dispatch — the shmerr fixture has
  no script); the shmerr leaf-start registers `shmfault` -default with
  the {xShmOpen,xShmMap,xShmLock} filter preset (C's
  shmerr_injectinstall — the -5/-6 preps' `shmfault filter xShmMap` then
  re-restricts via the normal testvfs_config path); the trial loop arms
  the fixture counter for the shmerr class (prep-before-arm — C's
  do_one_faultsim_test order: install once, then prep → injectstart →
  body → injectstop → -test) and keeps pager.IOErrInjectStart for every
  other class.
- **Verify**: the walfault family 0-FAIL (walfault-11 5.07s, was 725s);
  pagerfault 0-fail; go test ./internal/... EXIT=0. walfault 3→0 —
  **the S35g arc is COMPLETE** (e_wal 0, e_walhook 0, walfault2 0,
  walfault 0). Full-suite truth 1190.

**Next**: the btree page-density unit (walckptnoop 1.1-1.5, 5 leaves —
our bulk insert's WAL mxFrame 451 vs C's 298 on the walckptnoop workload;
resetdb-300's 13-vs-12 page_count), then S42/S43, the carded converter
classes, the perf phase.
