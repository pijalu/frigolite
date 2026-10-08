# Lessons Learned — Frigolite

## R9.DELETE (2026-10-06) — point-DELETE statement diet (fleet/r9-delete)

- **Dead-on-arrival decode**: the point-DELETE fast path decoded every
  cell into a dmlRow snapshot although only index maintenance and the
  preupdate hook read the values (RETURNING/triggers/FK are excluded by
  eligibility). Gate the decode on `PreupdateNeeded() ||
  len(allTableIndexes())>0` — vdbe.c's OP_Delete likewise reads only the
  rowid when no index-key extraction is scheduled. The seek's exact hit
  pins the stored rowid, so the undecoded delete addresses the same cell.
  +7.6% alone.
- **Write-only journal scope**: execDeleteBulk opens a pager statement
  scope for its FK-failure rollback; the point path (FK-free by
  eligibility) NEVER replayed its scope — every failure exit defers to
  the ENGINE's statement scope (execSnapshotDML → undoFailedDML). The
  innermost open scope takes exactly one before-image either way, so
  dropping the redundant Begin/End round-trip is behavior-neutral and
  matches SQLite's can't-abort statement handling (no OP_Statement for a
  trigger-free FK-free rowid DELETE). ~4% wall.
- **Second parse of the same leaf**: pointDeleteTarget ran a full
  storage.ParsePage although the statement's seek had just memoized the
  leaf (nothing writes between). Serve from Page.ParsedBTree + hand
  dropCellFromLeafPage a BY-VALUE copy — dropCell keeps its parsed copy
  in step with its byte writes and the memo's struct is shared
  read-only; handing it the memo-owned pointer would trip the canary and
  force a self-heal re-parse (i.e. cost what the memo saved).
- **Path-stack nil drops**: saveCursorPosition/restoreIfNeeded set
  `c.path = nil`; the next resetFor re-made the slice per statement
  (point ops save their seek cursor every statement via saveAllCursors).
  Keep capacity with `c.path = c.path[:0]` — the stack is at most tree
  depth deep. Same discipline SeekToRowID already used.
- **Result-per-statement**: the fast path returned a fresh &Result per
  delete. One executor scratch slot (the encBuf pooling pattern) serves
  them all — the value is consumed synchronously (Engine.Exec funnel →
  frigolite boundary copy) and nothing retains the pointer across
  statements. Hook-produced results bypass the slot.
- **Memo generation contract is load-bearing**: TestParsedBTree*
  pin "a NEW pointer per generation" (invalidation + canary self-heal).
  In-place memo reuse (re-parse into the existing memo) FAILS four tests
  — the mutant struct must be abandoned, not repaired in place. Only the
  hdrSnap buffer's capacity is safely carryable (measured NEUTRAL —
  reverted; keep the fresh generation).
- **Noise discipline**: fleet machines are shared — wall-clock A/B
  batches drift >5% between minutes. Pair main-vs-worktree runs
  back-to-back and compare MEDIANS of paired ratios; for engine work
  prefer CPU-seconds/op (record()'s cpu field) — GC/sys time distorts
  wall. Statement-journal capture, Begin/End and ParsedBTree misses are
  all visible in the 300k-delete CPU profile (~20 samples/percent).
- **Remaining gap is out of scope from execdml**: after this round the
  point-DELETE statement spends most of its CPU in layers this fleet
  contract does not own — per-statement parse (scanNumericLiteral + AST,
  ~10%), the Engine.Exec statement funnel (withDMLCTEs, snapshot scopes),
  execResult/public-Result per Exec, and GC pressure driven by harness
  render() strings. The single biggest engine-side follow-up: extend
  Engine.dmlCanSkipSnapshot (internal/exec/engine_tail.go) to the
  can't-abort point DELETE/UPDATE shapes — that kills the engine-level
  per-statement scope AND its 4KB before-image capture per statement
  (sqlite pays ~zero there; we measured the execdml-level half at ~4%).

## R8.UPDATE (2026-10-06) — point-UPDATE collect diet + typed SET fast lane (fleet/r8-update)

- **The collect path's hidden no-op**: `RemapWRRecordToDeclared` re-parsed
  the CREATE TABLE text (ToUpper + PK-list scan) PER ROW to decide a
  permutation that point-UPDATE's own gates already exclude (WITHOUT ROWID
  tables never reach applyPointUpdate). Before deleting a per-row call, ask
  what its gates already guarantee — the same trick as the preupdate gate
  and CHECK-exists gating rounds. 6.25% of the update profile for free.
- **ReadCellData > ReadCell for single-row fetches**: ReadCell builds a
  full storage.Cell (DecodeCell copies the payload); ReadCellData hands the
  payload bytes + rowid straight off the cached leaf page. The delete path
  knew this; the update collect didn't. Same fusion applies to any new
  point-op family.
- **Typed SET fast lane rules** (update_setlane.go): compile the assignment
  list per statement into slot-indexed ops; evaluate ALL results before the
  FIRST store (the row map's all-RHS-see-the-original-row semantics —
  oracle-verified: `SET a=a+1, a=a*10` stores old*10, not (old+1)*10).
  Mirror execexpr exactly: addInt64/subInt64 overflow→REAL, the float64
  round-trip of * / % (bug-compatible above 2^53), x/0 and x%0 NULL, %
  divisor cast through int64 with minInt64%-1→0, NaN→NULL, target affinity
  after evaluation. Any other shape OR runtime operand type (text, blob)
  falls back BEFORE the first store — the fallback re-runs the generic path
  byte-identically. Encoded operand refs must have an explicit literal
  marker (laneRefLit = -3): a zero-valued ref collides with slot 0 and
  silently reads column 0 (burned once; the rollback-exactness pin caught
  it as "earlier statement's write lost" — a misleading symptom for a
  value-corruption bug).
- **Parity pin pattern**: force the generic pipeline with `WHERE id = N+0`
  (not a constant equality → planDMLSeek declines) and diff whole-table
  images between the point path and the scan path for every lane shape.
  Cheaper and more complete than per-shape oracle probes for the fallback
  surface; oracle probes then only pin the engaged numeric/affinity edges.
- **Memoize schema-shape predicates under fingerprint + entry identity**
  (the ciCache guard pattern): tableIsWithoutRowid was scanned 3-4x per
  point UPDATE statement (shape gate, conflict layout skip, constrained-def
  walk, CHECK walk). Same pattern now also de-boxed uniqueColValuesMatch's
  IPK rowid compare (both slots NULL → compare rowids directly, no int64
  boxing).
- **Update-phase walls after this round** (paired deltas only; the machine
  runs sibling fleet agents): the remaining CPU is btree SeekToRowID +
  OverwriteCellByRowIDAt (~22%, internal/btree — report-scope), exec-side
  per-statement glue execPreflight + updateCOW AST clone + execResult
  (~30% combined, internal/exec + frigolite glue — execPreflight and
  updateCOW are off-scope for execdml missions; an in-place per-Stmt
  COW clone would need an engine-side ownership audit), and GC from those
  same allocs. Pooling *Result changes the public API's lifetime contract —
  do not pool without a documented "valid until next Exec" rule.
- **-race on the full harness needs an explicit -timeout**: the suite under
  race exceeds go's default 10m on a loaded machine and the timeout panic
  mimics a failure. execdml + pager race clean; focused root pins race
  clean in seconds.
- **Numbers** (own scratch driver /tmp/perf/r8upd, 3-col table, prepared
  `UPDATE t SET c=c+1 WHERE id=?` in-txn, paired interleaved vs main
  @32253d08d): quiet-machine solo runs went 356k→514k ops/s (+44%) across
  the round; under fleet load the paired band was main 366k vs worktree
  449k (+22.7%) to 403k vs 492k (+21%) depending on sibling activity —
  only paired deltas mean anything (third repro of the loaded-machine
  lesson).

## PERF.JOURNAL (2026-10-05) — statement-journal capture moved to write intent (fleet/perf-journal)

- **Read-time before-image capture taxed every seek for pages that are
  never written.** The P5 journal captured at READ (stmtReadTouch in
  ReadPage): on a memory pager EVERY page read under an open statement
  scope was byte-copied (4KB), so a point UPDATE's root->interior->leaf
  descent paid 3 copies + 3 lock upgrades + 3 journal entries to mutate
  ONE page. Profiled: stmtReadTouch 15.5% of update CPU — the whole
  ReadPage cost inside seekTableLeafWithPath (8.8%) was journal capture,
  not the map lookup or descent itself. After the move: capture frames
  vanish from both update and delete profiles.
- **The fix is sqlite3PagerWrite's contract, not a new scheme**: btree
  mutators call `pager.PrepareWrite(pg)` BEFORE the first in-place edit;
  the journal captures there (pre-mutation, correct for every
  before-image kind). markDirtyLocked stays as capture point for the
  pages no PrepareWrite covers: fresh allocations (stmtEntAbsent), clean
  file pages (stmtEntFromFile), and pager-internal mutators that already
  dirty-before-edit (grabPageLocked, mirrorHeaderToPage1Locked,
  BumpSchemaCookie — their pre-mutation markDirty ordering was load-
  bearing; WritePage's page-1 header mirror needed an explicit capture
  added BEFORE the copy). NO mid-txn dirty spill exists (flushPage runs
  only in the commit path), so file-backed begin-dirty pages never needed
  read-time copies at all — eviction-restore reaches their txn-start disk
  image.
- **Proving exhaustiveness mechanically: the audit build.** ~30 btree
  functions mutate page bytes in place. Rather than trusting the
  enumeration, `go build -tags frigolite_journal_audit` turns the read
  path into a witness: every page handed out under an open statement
  scope is FNV-1a-64 hashed (package-level map keyed by pager, cleared
  when the outermost scope closes), and markDirtyLocked PANICS if a
  page's bytes differ from the read-time hash without a journal entry —
  mutation without write intent, i.e. a rollback-corruption bug. Full
  pager+btree+root suites pass under the tag; production builds inline
  the hooks to nothing (journal_noaudit.go). This pattern is reusable for
  any "every site covered" invariant.
- **Pins that hash the pager must not assume residency**: from-file
  journal entries restore by cache EVICTION, so after a statement
  rollback the previously-cached page set SHRINKS (correct — the disk
  image is the statement-start state). A whole-cache sha256 before/after
  "pin" then fails on RESIDENCY, not bytes (burned twice: digest diff
  with zero per-page byte diffs). Hash a STABLE universe — the before-map
  page numbers — re-reading each page after the rollback.
- **Pager-level journal unit tests are btree clients**: pagerstmt_test.go
  wrote page handles from ReadPage directly, which the new contract
  forbids; they now call PrepareWrite before mutating (the same discipline
  the btree layer follows). Test-helper mutation = production mutation.
- **Numbers** (paired interleaved vs main @91c5be1be, machine under fleet
  load, only deltas mean anything): update_xact +6.6% (401.7k vs 376.9k
  ops/s); delete +0.5-1% (delete is dominated by exec-layer gates —
  execPointDelete 26% cum — and scheduler noise on a 30k-op phase);
  insert/point/scan/group/file unchanged. Mission targets (update 620k,
  delete 850k on the quiet-machine scale) are NOT reachable from the
  journal path on current main: after this round the journal is ~1-2% of
  statement CPU. The walls moved to exec-layer per-statement gates
  (execPreflight, snapshotAllPagers 4%, normalizeScan, Exec entry) —
  sibling scope.
- **EndStatement diet: already at floor** — post-move profile shows
  ~1.5% flat (Lock/unlock + LIFO unlink + 1-2-entry discard loop);
  further pooling measured nothing beyond noise.
- **Update single-seek verified (read-only)**: applyPointUpdate's collect
  establishes cellPos{leaf,idx} ONCE; writePointUpdateRow re-addresses
  via OverwriteCellByRowIDAt with a stale-fallback full seek; delete uses
  DeleteCellByRowIDAt with hintParent. No second descent exists — nothing
  to fuse.
- **Fleet-load testgen timeouts reproduce on demand**: savepoint2 timed
  out at 600s when run concurrently with the full suite + race + audit
  runs, then passed SOLO in 8.1s. Never adjudicate a suite on a loaded
  machine (third reproduction of this lesson — PERF.P5, PERF.DMLCORE,
  now here).

## PERF.INSQUICK — insert append-cursor + staging (fleet/perf-insquick, 2026-10-03)

- **The b-tree root descent was NOT the insert hot cost.** The
  balance_quick-style saved-rightmost-leaf append cursor (btree.c
  BTCF_ValidNKey + BTREE_APPEND) works and engages (296k/300k hits on the
  harness workload, 0 verify fails), but InsertCell cum was only ~12% CPU
  and most of it is the leaf write itself (encode + allocateSpaceOnPage +
  WritePage), not the descent. Landing it bought a few percent; the profile
  truth was per-row execdml scaffolding.
- **The hidden second descent: ipkRowidAliasConflict.** Every explicit-rowid
  INSERT sought the tree (cursor.SeekToRowID) to check the PK value — a
  full root→leaf descent PLUS cursor-registry register/unregister churn per
  row, on top of the insert's own descent. The executor's largest-rowid
  cache is bump-only-grows and invalidated by every delete/rowid-changing
  update, so `probe rowid > cachedMax ⇒ no conflict` skips it soundly
  (btree.c BTREE_APPEND's moveto bias is the same idea). This was worth
  more than the append cursor.
- **Cross-statement quick-state must be keyed (pager, root), not kept on
  the wrapper**, and Close() is the invalidation workhorse: statement
  teardown after a DELETE/UPDATE/VACUUM closes the function-local wrapper,
  which clears the insert tree's slot at that statement boundary. A journal
  ROLLBACK rewrites page images WITHOUT running btree code, so the quick
  path re-verifies the saved leaf on every engagement (leaf type + last
  rowid == recorded max) — that check is what makes savepoint/txn rollback
  and DROP/CREATE page-reuse safe, and it cost 0 fails over the whole
  suite.
- **Reusable per-row/per-statement execdml Results are safe under trigger
  nesting** because trigger statements consume their Result fields strictly
  inside the outer statement's consumption window (stack discipline).
  What is NOT safe: pooling the PUBLIC frigolite.Result (callers may retain
  it across Execs) — the root execResult conversion (~32MB/300k inserts)
  stays; likewise the preupdate-hook `New` values copy (~13.5MB) is
  unavoidable without a DMLContext interface change (internal/exec is
  pinned by scope).
- **Fleet-box measurement discipline**: under load 4-17, sequential runs
  swing ±25% (same binary 414k vs 329k ops/s). Interleaved rounds with
  alternating first-runner keep the comparison fair (branch won 7/8
  rounds); quiet-machine deltas are the honest ones. zsh gotcha: `set --
  $ord` does not word-split — drive benchmark loops from a bash script.
- In-lane floor reached: remaining insert-phase cost is the exec-layer
  parse/clone/dispatch (~35%) + template literal substitution + root
  execResult — the template-floor sibling's lane, not btree/execdml.
## PERF.POINT3 — point-SELECT scaffolding, tranche 3 (fleet/perf-point3, 2026-10-03)

- **The seek path paid OpenCursor's leftmost-leaf descent for nothing.**
  fetchSeekStructRow + the loSet rowid-range seek now open through
  OpenCursorAtRoot (the point-UPDATE/DELETE shape): SeekToRowID clears the
  path stack and re-descends from the root, so descendToFirstLeaf was dead
  work per statement (+9% point ops/s alone). The scan cursor opens AFTER
  the seek verdict in execRealTableSelect — a seek-resolved statement never
  scans.
- **Memo keys on the copy-on-write clone: schema slices AND template-shared
  AST slices.** The template clone returns UNCHANGED slices (e.g. a bare
  column list without literal slots) to the template instance, so
  &s.Columns[0] is a stable cross-statement key exactly like &colDefs[0] —
  guarded by the schema fingerprint, capped (64 entries, flush on overflow),
  and copied on hit so per-statement caller-owned semantics stay identical.
  Anything containing literals (WHERE chains) is fresh per statement and can
  NEVER be a key.
- **One census walk replaces a dozen validator walks.** The SELECT prepare
  chain (aggregate misuse, star-arg arity, FILTER, DISTINCT arity, subquery
  resolution, window placement, row values, FTS MATCH, ORDER BY terms) all
  key on node KINDS; one WalkExprFull pass collecting {funcCall, aggFunc,
  distinct, filter, over, subquery, rowValue, matchOp, collateOp} lets each
  validator skip when its kinds are provably absent. Soundness hinges on
  WalkExprFull NOT descending into subquery bodies: any validator whose
  error paths inspect inside a subquery stays enabled while census saw a
  subquery at this level (its own execution validates its body).
- **Collation-free tables skip checkWhereCollations entirely**: the error
  only fires through the declared-collation lookup, so an empty
  collationMapFor means the WHERE walk can never fail.
- **The point-SELECT profile floor after all cuts**: btree descent
  (routeInteriorTable + seekInLeafTable ≈ 16%), template-clone literal
  substitution, output-row construction, and GC from the remaining
  per-statement allocations (NewBTree wrapper — deliberately NOT pooled per
  btree_pool.go's race note, affinity visitor closure, DecodeRecord). The
  next tranche needs btree write-path-adjacent work (out of scope here) or
  clone-machinery changes.
- **The shared-box full harness flakes in PARALLEL**: hundreds of files fail
  at ~0.01s each under fleet load (t.Parallel subtests + cleanupTestDBFiles
  racing), with MAIN failing the IDENTICAL 382-file set. Regression proof =
  per-FILE solo runs (deterministic) on both sides — 33 query-heavy files
  showed byte-identical failure counts pre/post change.
- **Method values still allocate per evaluation site** — caching
  `a.visitFn = a.visitNode` in the struct does not remove the closure alloc
  (it just moves it); only restructuring the walk to avoid the closure
  entirely would. Not worth it at ~190B/statement.
- **scanTableAffinityCols' map is statement-retained** (scanState/
  rangeSeekRow keep it for the scan duration), so per-engine scratch-map
  reuse is UNSAFE — a nested statement's collector would clear the outer
  statement's map mid-scan.

## PERF.DML2 — point UPDATE/DELETE at btree.c parity (fleet/perf-dml2, 2026-10-02)

- **dropCell+freeSpace+allocateSpace is the correct fundamental shape for point
  deletes; page repacking was the engine's invention.** Ported verbatim
  (btree.c:1918/7228/1743/1836/1613), the single-cell delete becomes O(1)
  freeblock-chain accounting and the insert path reuses/defragments that space
  on demand. Paired (contended-machine) delete_xact went +50% vs main. The
  engine's previous "compact on every delete" invariant is what made
  finishLeafDelete's missing freeblock-head clear safe — once ANY path creates
  freeblocks, every wholesale page rewrite (finishLeafDelete,
  compactLeafAfterDelete) must zero header bytes 1-2 or a stale chain head
  points into rewritten cell bytes.
- **allocateSpace allocates the range [top-nByte, top) — the overflow check is
  `top <= usableSize` (validated at entry), NOT `top+nByte <= usableSize`.**
  I initially conflated the two and every fresh-page insert split (or errored
  "cell too large"). SQLite's defrag condition is `gap+2+nByte > top`; the
  fast path is `gap+2 <= top`, identical to the engine's old leafHasRoom.
- **Fresh (zeroed) pages keep content=0 as a sentinel in the parsed header**
  — allocateSpace must re-apply the `top==0 → usableSize` convention AFTER a
  no-op defrag, or the first insert takes the split path with CellCount==0
  and fails "cell too large".
- **The single-candidate point UPDATE must keep the per-row map** (mirrors
  updateSeekMapPathMaxCandidates=2): the positional DMLRowPlan's fixed
  per-statement cost (affinity walk + colIndex build) exceeds one BuildRowMap.
  I measured the StructRow switch as a regression before reverting.
- **Seek-first cursors: OpenCursor's leftmost-leaf descent is pure waste when
  the next act is SeekToRowID/SeekToKey** (they reset the path and re-descend
  from the root). OpenCursorAtRoot + position accessors (PageNum/CellIdx/
  PathParent) + DeleteCellByRowIDAt/OverwriteCellByRowIDAt remove the second
  descent per point statement. The hinted primitives must re-validate the
  stored rowid (parity with the generic predicate delete on stale positions).
- **exec-level per-statement costs are the remaining wall** (out of fleet/
  perf-dml2's scope): findTable/detectExternalSchemaChanges ≈ 20-30% of each
  point statement (map iteration + EqualFold per statement),
  FirePreupdate→applyPreupdateAffinity→findTable PER ROW (both UPDATE and
  DELETE preupdate paths), VTabUpdaterInstance/EchoVTabSource probes per
  statement, CrossConnLockError. fixing findTable-per-row inside
  applyPreupdateAffinity (or gating FirePreupdate on hook presence with an
  exec accessor) is the next big delete/update win.
- **Fresh worktrees lack gitignored GENERATED fixtures** (testdata/
  walconformance, recoverconformance/*.input.db, internal/fts/testdata/
  ftsconformance/*.db) — full-suite failures there are environmental; copy
  them from the main checkout before judging. One worktree fts-x6-growth.db
  was a 0-byte placeholder; md5-compare against main exposes it.
- **Machine contention invalidates absolute ops/s**: sibling fleet agents
  share the box; main's own numbers moved ±20% between runs. All claims in
  this tranche are PAIRED same-minute main-vs-worktree runs, alternating.


> Consolidated 2026-09-26 (T33 close): dated per-session sections (P6.VTAB
> sessions through T32) moved verbatim to `.agents/lessons_archive_2026-09.md`.
> The durable rules, methodology, engine knowledge and process live below,
> followed by the current T33 session sections. Consult the archive for
> closed-goal specifics (also in plan/goals/*.md and portplan/NA_EVIDENCE.md).

## FIX.ROWID-AFFINITY — comparison text→numeric conversion is affinity-GATED, never prefix (fleet/fix-rowid-affinity, 2026-10-02)

- **SQLite's comparison conversion fires ONLY where the operator carries
  numeric comparison affinity**: a numeric-affinity COLUMN operand
  (wrapped `util.ColumnValue`), or the rowid SEEK-BOUND constant the
  optimizer codes. Unary `+` strips affinity (`Affinity: 0`), so
  `+rowid > ' 5000 '` and `+k = ' 50 '` must NOT convert (oracle 3.54:
  0 rows — the value stays TEXT and sorts above every number). Bare-vs-bare
  (literal vs literal) never converts (`1 = '1'` → 0) — util's
  `noAffinityPair` short-circuit encodes this; `value.CompareValuesCollate`
  (the ungated twin) is safe only because no exec consumer reaches it
  bare-vs-bare (rtree applies affinity first).
- **There is NO longest-prefix rule in comparisons.** sqlite3AtoF returns
  `rc<=0` when the whole (ws-trimmed) string isn't consumed and
  applyNumericAffinity bails; '5000abc'/'4999.5abc' stay TEXT. Prefix
  results live in DIFFERENT domains: CAST/arithmetic
  (sqlite3VdbeMemNumerify) and sqlite3_value_double (AtoF writes the prefix
  into pResult even when returning false). Task text claiming
  "+rowid > '5000abc' → oracle 15000" was transcription drift — ALWAYS
  re-probe the sqlite3 binary before implementing; the probe TSV is the
  only ground truth (127 cells → /tmp/rowidaff/oracle*.tsv, regenerable).
- **SQLite is internally inconsistent between seek and scan shapes**:
  `rowid > ' 4999.5 '` → 15001 (seek bound converts) while
  `+rowid > ' 4999.5 '` → 0 (scan, no affinity). Probe BOTH shapes; the
  consistency rule is one-directional — seek bounds must be SUPERSETS of
  eval acceptance because the WHERE re-check is the final filter
  (typedrow lesson reaffirmed: EXPLAIN QUERY PLAN first to prove which
  shape actually engaged).
- **rtree's xFilter domain is sqlite3_value_numeric_type (full-string)**:
  `rt WHERE x1 > ' 200 '` bounds at 200 (oracle 2,3) and
  `x0 <= '250abc'` matches ALL rows (text ≥ every coordinate). The old
  code classified with `ParseFloat(TrimSpace)` but converted with
  rtreeNumericPrefix, whose mantissa slice INCLUDED the leading space —
  `ParseFloat(' 200')` errors → compared against 0.0 → all rows leaked.
  value.NumericText now serves both classification and conversion; the
  CAST/blob-decode domains (geopoly args, geometry tokens, stored-cell
  decode) keep rtreeNumericPrefix deliberately.
- **Go's ParseFloat accepts NaN/Inf/'infinity'/hex-float spellings that
  sqlite3AtoF rejects**: storage affinity stored `integer:0`/`REAL NaN`
  for 'NaN' and `9e+999` for 'Inf' (oracle: TEXT stays TEXT), and NUMERIC
  saturated 2^63 to MaxInt64 (oracle: REAL). The six string arms of
  ApplyColumnAffinity (util + value twins) all convert through
  value.NumericText with the strict `> -2^63 && < 2^63` integer-affinity
  bounds (the exact ±2^63 doubles stay REAL: sqlite3Atoi64's
  smallest-int64 refusal). One scanner, every consumer.
- **worktree gotchas**: TestBackupConformance needs the UNTRACKED generated
  `testdata/backupconformance/*.db` + `*-backup.db` fixtures — copy them
  from the main worktree (read-only) instead of regenerating. zsh does not
  word-split `$VAR` in `go test $PKGS` — a newline-joined package list
  becomes ONE malformed import path (`[setup failed]` everywhere); use
  shell globs.

## PERF.STRUCT-fix — template-cache INSERT clone walk order = source order (fleet/perf-struct-fix, 2026-10-02)

- **The template cloner's walk order MUST match the statement's source
  order — for INSERT too, not only SELECT/UPDATE/DELETE.** The normalizer
  (normalizeSQLScratch) extracts literal values left-to-right over the raw
  text; the walker consumes them positionally (c.idx). insertStmtValues
  walked VALUES tuples → SELECT body → CTEs LAST, but an INSERT's WITH
  clause precedes the INSERT keyword, so its literals are extracted FIRST.
  With a same-shaped earlier statement in the template cache
  (`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 500)
  INSERT ... SELECT i, 'row' || i FROM s` then anchor-501/bound-5000), the
  clone cross-assigned: 501 into the 'row' literal, the anchor left at the
  template's 1, 5000 into the i+1 increment, and the STRING "row" into the
  guard's numeric slot. Value count still matched the slot count, so every
  guard passed and the statement silently executed the wrong AST: the
  recursive CTE's guard became an always-true integer<text comparison and
  ran to the 1M-row budget, inserting 1M garbage rows (count(*) 1000500 vs
  5000; the corrupted run takes ~10s). Fix: clone CTEs before the
  VALUES/SELECT body (frigolite_template_cte_order_test.go pins WR/rowid/
  VALUES/bind shapes).
- **Two symptoms were red herrings during forensics**: (1) the garbage rows
  look like btree payload aliasing (`a = bound*j`, `b = str(anchor) || a`)
  but integrity_check was "ok" and every value traceable to a wrongly
  substituted literal — walk the DATA LINEAGE of garbage values before
  suspecting storage; (2) the pre-existing btree pin
  (TestT32KernelPinIndexRootSplitLeafToInterior) failed on main for the
  same underlying reason (the repro's shape was already in its fixture).
- **A recursive CTE's WHERE filters INPUT rows, so output reaches the bound
  inclusively**: `... SELECT i+1 FROM s WHERE i < 10` yields 1..10 (oracle-
  verified). A always-true guard therefore produces exactly rowLimit rows
  (PRAGMA recursive_cte_limit, default 1000000) — a "magic" 1M row count in
  any CTE repro means the guard never terminated.
- **WITHOUT ROWID PK-uniqueness-on-INSERT gap — FIXED (fleet/fix-wr-duppk,
  2026-10-02; was repro'd 2026-10-02 on origin/main):** `CREATE TABLE w(a
  INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID; INSERT INTO w VALUES(1,'x');
  INSERT INTO w VALUES(1,'w')` used to store both rows (oracle: "UNIQUE
  constraint failed: w.a"). ROOT CAUSE: findRowByUniqueCols' single-unique-
  column fast path matched `isIPKRowidAliasCol` for ANY INTEGER PRIMARY KEY —
  including WR tables — and seeked the btree by CELL ROWID (SeekToRowID), but
  a WR table's btree is an INDEX btree keyed by the PK record whose cells
  carry synthetic rowids, so the seek missed and no other check ran. TEXT/
  REAL/BLOB/composite PKs skipped the fast path and were caught by the
  collation-aware scanForConflict full scan. FIX (conflict-scan layer):
  gate the IPK fast path on rowid tables; add wrPKSeekConflict — an O(log n)
  exact-key probe (cursor.SeekToKey over WRRecordComparator) whose PK-only
  probe record sorts strictly before equal-PK full rows (shorter-record-first
  tiebreak), so the landing cell IS the lower bound of the PK key; compare
  the landing cell's PK slots with wrValuesEqual. Applies only when every PK
  column is BINARY-collated (the btree's at-rest order is binary); a NOCASE
  PK keeps the collation-aware scan (oracle: 'ABC' vs 'abc' still conflicts).
  Side effect: TEXT-PK WR inserts went from per-row full scan to seek (20k-row
  build 81s → 0.4s). OR IGNORE/REPLACE, upsert, synthetic-rowid fallback and
  rowid tables unchanged. The WR duplicate-PK coexistence in the clone-order
  corruption can no longer occur from this path.

## PERF.PARITY-tplgate — template-cache same-kind substitution vs the parser's minus fold (fleet/perf-parity-tplgate, 2026-10-01)

- **One normalized key can serve statements whose parsed ASTs differ in
  shape; substitution must reproduce a fresh parse, not the stored
  template.** normalizeSQL replaces literals with '?' and extracts the
  UNSIGNED magnitude (the scan starts at the digits), so `f(-2^63)`,
  `f(-123)` and `f(-1.5)` all share the key `f(-?)` — but the parser FOLDS
  the unary minus of a 2^63-magnitude decimal literal into the literal
  itself (rule 216, expr.c sqlite3ExprCodeInteger: "-9223372036854775808"
  is one NumericLit, NOT UnaryOp{'-'}; hex folds too), while smaller
  magnitudes keep UnaryOp. Cloning the stored shape under a different
  value dropped or duplicated the sign (first attempt: -2147483648 became
  +2147483648, tointeger(-2^63) became NULL). The gate now refuses any
  slot text beginning '-' (folded), the exact double 2^63 (extractable
  from BOTH a folding integer spelling and a non-folding "....0" spelling
  — shape-ambiguous), hex slots ('x'/'X'; hex digits include 'E', so a
  naive exponent check admits "0xE8"), non-finite floats, and kind
  crossings; everything else substitutes same-kind (int -> digit-only
  slot, real -> '.'/'e' slot with '.0' restored, string -> any).
- **Oracle rendering drift: system sqlite3 3.54 prints shortest-round-trip
  doubles, frigolite mirrors classic %.15g.** Cell-parity tests must
  re-render the oracle's parsed value through util.FormatSQLiteReal and
  compare text, not ParseFloat both sides (15-digit strings need not
  round-trip to the same double).
- **Fresh worktrees fail TestSQLiteSuite en masse (~4.5k subtests,
  "no such table: t1") from harness shared-state/ordering, and
  TestBackupConformance needs generated testdata/backupconformance
  fixtures that are not committed.** A/B against a pristine origin/main
  worktree before attributing anything to a change; failure sets differ
  run-to-run at the 20-30-subtest level on identical code.
## PERF.STRUCT-scan — direct column reads replace the record-wide decode (fleet/perf-struct-scan, 2026-10-01)

- **The last scan-floor mass was the FULL-RECORD decode on no-WHERE shapes.**
  Lazy two-phase decode only armed when `s.Where != nil`, so the hottest
  feed shapes (`SELECT SUM(c) FROM t`, `SELECT COUNT(*) FROM t`) and bare
  projections (`SELECT c FROM t`) fell to `decodeRowFull`: every declared
  column boxed into an interface per row, ~9 of 10 values discarded. Fix:
  (a) feed mode arms the lazy two-phase pipeline even without a WHERE (its
  phase-1 set is the statement's referenced columns; the feed branch skips
  the refill), and (b) eligible scans decode exactly those slots via
  `storage.DecodeRecordColumn(s)` — serial-type varints are self-sized, so
  column k's data offset is the running size-sum of types 0..k-1
  (vdbe.c OP_Column). Same-probe A/B vs origin/main (wide 10-col table,
  300k rows): bare scan 294→143 ns/row (13.67→2.00 allocs/row, -43% B/row,
  +106% rows/s), COUNT(*) 213→80 ns (13.67→2.00, +168%), SUM 216→114
  (+90%), IPK-alias SUM 175→86 (+103%); star/where/groupby shapes
  byte-identical (they keep the historical decode by design). Post-fix
  profile: the floor is the header varint walk itself
  (decodeRecordColumnsInto ~38% cum) — the "precompute column-offset hints"
  follow-up lives there, not in the value decode.
- **`scanConsumedByAggPass` is true for EVERY aggregate SELECT — it must not
  gate a feed fast path.** The first wiring shared one exclusion list
  (needMaps/posAgg/aggConsumesRows) across feed and bare branches; feed
  scans were silently vetoed (aggConsumesRows = hasAggregates = true) and
  ran the map-based phase-1 decode, costing half the win while every test
  stayed green. The distinction: those flags only matter when ROWS OR MAPS
  materialize — feed mode builds neither (needMaps forced false, posAgg
  requires feed==nil, aggConsumesRows only suppresses per-row output
  building). Rule: a fast path's eligibility exclusions must be derived per
  branch from what that branch actually leaves readable, not shared because
  the shapes look similar. Debugging aid: a profile showing the OLD path's
  frames (decodeRecordValuesFromTypes + mapaccess2) while the new-path
  symbols never appear means the gate never fired — instrument the gate,
  not the fast path.
- **The record header includes its own size varint: value data starts at
  `hdrSize`, not at the varint's end.** First implementation anchored the
  running offset at the header-size varint's end and read every column one
  byte early (300 == 0x012C read as 515). DecodeRecord gets this for free by
  finishing the type walk at hdrEnd; a partial walker must anchor at hdrEnd
  explicitly.
- **The eligibility contract is "every consumer of the reused StructRow reads
  a direct slot"; undecoded slots keep the PREVIOUS row's values.** Feed mode
  reads compiled slots + WHERE refs (all in the plan's phase-1 index set, no
  rows/maps exist); bare output peels bareOutIdx only. Every whole-row
  consumer excludes the direct path: SELECT * (star output), needMaps
  (ORDER BY/DISTINCT/UNION rebuild from maps), posAgg clones, aggConsumesRows
  (bare branch only — see above), joins, subquery-WHERE (correlated refs read
  any column), WITHOUT ROWID (wrOrder permutes storage positions), dropped
  columns (storage/declared ordinal shift). Excluding any one of these
  silently serves stale slots.
- **A strict primitive over a lenient scan path needs a per-row fallback.**
  The scan's historical decode SILENTLY truncates on crafted payloads
  (DecodeRecordValuesFromTypes stops at the first bad type/short value; only
  a bad header size errors). DecodeRecordColumns validates strictly — so the
  scan falls back to the historical per-row decode on ANY primitive error,
  reproducing the silent-truncation semantics exactly (corrupt* suites are
  the canary; all 23 corrupt/fts3corrupt/incrcorrupt/mmapcorrupt/altercorrupt
  testgen packages stayed green before and after).
- **Empty decode sets are first-class**: COUNT(*) compiles zero storage
  slots; a non-nil empty directCols skips the record decode entirely (header
  walk only) — 2 allocs/row total. Gate with `!= nil`, never `len > 0`.
- **Walking the FULL header (not stopping at the last requested ordinal)
  keeps ALTER TABLE ADD COLUMN defaults exact**: the caller needs the
  record's true serial-type count to decide absent (apply DEFAULT) vs stored
  NULL (keep nil). The varint walk was never the cost — the value boxing was.
- **BSD xargs -I{} refuses long assembled command lines** ("command line
  cannot be assembled, too long") even at ~350 bytes — fleet sweep runners
  must delegate to an inner script taking the item as `$3` instead of
  inlining the whole command in the -I template.

## PERF.PARITY-memofix — pager-memo poisoning + index-decode tails (fleet/perf-parity-memofix, 2026-10-01)

- **A memo validated only against the BYTES it parsed cannot see Go-side
  mutation of the parsed struct.** ParsedBTree re-checked hdrSnap bytes but
  served &m.parsed forever while they matched; any consumer writing through
  the shared struct (the read-only contract is unenforceable at compile
  time) pinned a stale CellCount that walks the cell pointer array off the
  page. Fix: the canary — re-derive the fields FROM the snapshot on every
  hit (the parse is a pure function of that span, so 6 compares prove the
  struct pristine) and re-parse on mismatch. Rule: **when a cache hands out
  a pointer into itself, its revalidation must cover the exact bytes the
  value is derived from, including the Go-side struct, not just the source
  buffer.** Same check on the miss path closes the parse/snapshot straddle
  (a torn capture must be served unmemoized, never pinned).
- **Go slices panic where C pointer arithmetic stays in-bounds — the port
  must guard the tails SQLite gets for free.** SQLite reads interior cells
  through maskPage-masked offsets (in-page by construction) and bounds
  CellCount in int arithmetic at btreeInitPage (nCell > MX_CELL). The Go
  port's uint16 cellPtrEnd truncated (crafted CellCount 0xFFFF wrapped past
  the content-start check) and its raw Data[cellOff:cellOff+4] /
  Data[cellOff+n:] index-decode reads had no tail checks — every one a
  latent slice-bounds panic on a corrupt image. Guard at the read site with
  the standard "malformed" error; compute header bounds unwrapped.
- **A slice-bounds crash "in index decode" attributed to a memo is most
  likely reached through a CORRUPT IMAGE passing validation, not through
  memo staleness** — enumerate the actual panic sites (every raw
  Data[off:off+n] on the path) and fix each to error before theorizing
  about cache poisoning.
- **The merged-main full suite baseline (2026-10-01, 3 runs): NO crash,
  but ~4,500 TestSQLiteSuite subtest failures from the __RESET_DB__
  converter drift (reset markers land AFTER the cases they reset — cases
  then see "table t1 already exists"/"no such table"), plus a parallel
  windowcpin.db ENOENT flake (t.Chdir in parallel tests moves the process
  cwd).** Relative-path fixtures + t.Parallel + t.Chdir = cross-test cwd
  races; a "clean" fleet baseline means no crash and no NEW failures vs
  this drift, not zero failures.


- **NEVER SetFinalizer per registration on a pooled/recycled object.** The
  wrapper-pooling tranche set a registry finalizer on every OpenCursor and
  cleared it on every Close; on recycled cursors that set/clear pair races
  the GC sweep cycle (a special can outlive its object through the pool drop
  at poolCleanup) → fatal "runtime.SetFinalizer: finalizer already set" on
  the next registration (2 of 4 full-suite runs). Fix: the safety-net
  finalizer is installed EXACTLY ONCE at cursor allocation (acquireCursor's
  pool-New); registration only records regKey; Close/resetFor zero regKey so
  a queued finalizer unregisters nothing. SetFinalizer appears in exactly
  one place per pooled type, at construction.
- **A pooled WRAPPER is a crash multiplier — wrappers are no longer pooled.**
  Probe run 5 caught it live: execCreateIndex inserting into a wrapper whose
  Close ran from a DIFFERENT goroutine's statement teardown (Engine.Exec →
  releaseStatementTrees → Close → resetForPool). A pooled wrapper is
  re-armed for whoever Gets it next, so ANY close that races a statement
  still holding it — concurrent Exec frames on one engine (the funnel
  assumes strict nesting), or a segment mark mis-attributed across
  goroutines — turns "closed wrapper" into a live statement's pager pointer
  vanishing: the nil-pager SIGSEGV in Pager.ReadPage/WritePage. A closed
  wrapper that is NOT recycled degrades to the pre-pooling behavior (an
  object that merely becomes garbage). Cursor pooling (global cursorPool),
  the deterministic registry shrink at Close, and the normalize/lock-key
  scratch wins are all kept; the tradeoff is one small wrapper allocation
  per statement/tree (mission-blessed).
- **A Close that resets cursors must re-mark them released AFTER the reset.**
  releaseCursors→resetFor cleared the `released` flag immediately, so the
  "use of a closed cursor errors" contract (checkOpen/restoreIfNeeded) was
  dead code on the hot path and a closed-owner read silently served a stale
  page cache. Order: unregister → reset into free list → re-mark released;
  acquisition is the only place the marker clears. cachePage and InsertCell
  carry the same guard (error, not SIGSEGV).
- **Pointer-keyed probe maps go stale through heap address reuse** — a
  "last pooled at" stack captured for a DEAD wrapper at the same address
  taints the diagnosis (probe run 5's history vs the live panic). Debug
  state for object-lifetime bugs belongs ON the object (a closedBy field),
  never in a package-global map keyed by pointer.
- **Reproduce crashes in the FULL suite before theorizing.** Pre-fix runs
  crashed differently every time (SetFinalizer fatal ×2, ReadPage SIGSEGV,
  slice-bounds in index decode — the last one reproduces 2/2 on UNMODIFIED
  main and belongs to the sibling pager-memo tranche, not pooling). Isolated
  files never crash: the bug needs the global sync.Pool + GC churn only a
  1002-file parallel run produces. And the 10-min package timeout plus
  sibling-fleet load distort single measurements — baseline A/B on a second
  worktree is the decisive instrument.

## PERF.PARITY-wrap — BTree wrapper/cursor pooling + statement-path scratch (fleet/perf-parity-wrappers, 2026-09-29)

- **Pooling is safe exactly where the P6 lifecycle discipline holds.** BTree
  wrappers and cursors are poolable because Close is TERMINAL: the statement
  funnel closes wrappers at Exec end, and every function-local site uses
  `defer tree.Close()` with no post-Close use. The pool lives in the btree
  package (sync.Pool for wrappers, per-wrapper cursorFree list); Reset
  reinitializes every field and KEEPS buffers (cursors slice, cursorFree,
  path stack, page-header scratch) so a reused wrapper costs zero
  allocations. Watch the GC subtlety: sync.Pool pools survive until GC, and
  a pooled wrapper's backing arrays are GC-traced past slice len — released
  cursors must be reset (refs cleared) before the wrapper is Put.
- **The pooling change FOUND a latent registry leak**: BTree.schemaCursor
  opens a schema-keyed cursor on a user-tree wrapper while rootPage is
  temporarily 1; Close recomputed the registry key from the CURRENT
  rootPage, so those cursors were unregistered from the WRONG key and stayed
  in the registry forever. Harmless while wrappers were single-use (owner's
  pager stayed valid; the leaked scan cursor was at EOF and filtered), but
  pooling turns every latent use-after-reset into live corruption (double
  registration; save of a cursor whose owner was reset). Fix: unregister by
  the cursor's captured regKey (btree.c removes a cursor from the BtShared
  list it was OPENED on). Rule: registry membership keys must be captured at
  registration, never recomputed at teardown.
- **Debugging pooled-object corruption: instrument the CONTRACT, not the
  data.** Two asserts found it immediately: (1) registerTreeCursor rejects a
  cursor already present under the same key (double registration), (2)
  saveAllCursors rejects cursors whose tx.pager is nil (dead owner). Panic
  messages must carry the pointer + regKey + owner state. CAUTION: panicking
  while holding cursorRegMu deadlocks the recovered-panic traceback (mutex
  left locked) — copy what you need, unlock, then panic.
- **Per-statement scratch buffers MUST be truncated on reuse, not just the
  value slice.** The first normalizeSQLScratch reset `values` but not the
  byte buffer: every statement appended onto the previous text and
  string(buf) copied megabytes (2.8MB/stmt after 200k statements, 20-60x
  slowdown that looked like a planner fallback). Symptom signature: ns/op and
  B/op explode TOGETHER while allocs/op barely move; pprof -list points
  straight at the `string(buf)` line.
- **The full-suite "new failure" triage must compare ISOLATED runs, and the
  10-minute package timeout truncates -v output WITHOUT printing FAIL lines
  for in-flight files** — a base-vs-head set diff can falsely show head-only
  failures (here: where8/9/A, which fail identically at base in isolation).
  Interleaved per-file A/B (same file, both worktrees, N rounds) is the
  cheap decisive instrument.
- **Statement-path fixed-overhead budget after this tranche** (INSERT loop,
  per stmt): cloneInsertStmt+Value ~5.7 fresh AST nodes (required by the COW
  contract — literals must never be shared), nextLiteral/scan boxing ~2.5
  ([]interface{} API), splitSQLStatements ~1.3 (root-package tokenizer),
  execResult 1 (public Result). The remaining ~90 allocs/stmt on INSERT and
  ~125 on SELECT are execquery compile/validate + execdml row collection +
  storage/pager — sibling tranches' territory; the 30%-of-total target
  requires their reductions stacked on this one.

## PERF.PARITY-scan2 — scan/aggregate second pass (fleet/perf-parity-scan2, 2026-10-01)

- **The scan's affinity wrapper is 40-50% of alloc objects in EVERY full-scan
  shape, and a bare output column's wrapper never reaches a comparison.**
  buildOutputRow / appendScanStarValues / the aggregate arg paths all peel the
  wrappers (unwrap∘wrap == identity on the raw slot), so
  scanTableAffinityCols's SELECT-column collection existed only to allocate.
  The exemption (`skipBareSelectRef`) must keep the wrapper when the column
  declares a non-BINARY collation — the CollatedValue MARKER is the transport
  the GROUP BY key computation reads to merge 'abc'/'aBC' — and when the
  statement's output alias is referenced by a consuming clause (below).
- **An output ALIAS referenced by WHERE/ORDER BY/GROUP BY/HAVING resolves back
  to the SELECT expression at eval time through the alias stack; the
  collector only sees the alias NAME.** Exempting the underlying bare column
  then leaves it undecoded in lazy phase 1 AND unwrapped — the alias
  comparison reads an empty slot and filters every row (collate8-2.1/2.2,
  having, selectC). Rule: collect the consuming clauses FIRST; if any
  collected ref matches an output alias, cancel the exemption statement-wide.
  The same interplay is why the bare-output fast path requires
  `len(selectAliasMap(s))==0` (an alias can shadow a later column of the same
  name and change evalColumnRef's resolution order: SELECT a AS b, b).
- **The IPK rowid-alias fill RIDEs ON the affinity plan** (`plan.apply`), so a
  wrapper-free scan must still build a fill-only plan — a nil plan leaves
  "SELECT id FROM ipk" emitting the stored NULL. (The star shape hides this:
  the "*" ref marks the collector seen, which is also why stars must keep
  their historical collection — the seen flag drives
  appendScanStarValues's unwrap discipline, else the fill's wrapper leaks
  into star output.)
- **GROUP BY positional retention = one interface, two materialization
  points.** Rows flow as `[]Row` (*StructRow clones sharing the scan's
  colIndex; values carved from arena chunks = 1 small alloc/row) through
  partitionByGroupKey/evalAggregatesGroupBy/evalHaving/evalAggFuncCall.
  Name-keyed maps materialize ONLY where a consumer demands them: the window
  pass (per group, gated on selectHasWindowFuncs), the group's
  representative map for result ORDER BY, and the lazily-read engine sets
  (AggRowMaps()/OuterRows() materialize on first read from their positional
  source). A RowMap IS a Row, so map-fed sources (joins, CTEs, seeks) convert
  with zero per-row copies. The 45%-of-alloc-objects StructRowToMap mass and
  the per-row Get hashing both vanish for the single-scan GROUP BY shape.
- **Gating positional retention: copy the feed's exclusion list, then some.**
  The feed (aggFeedEvaluationEligible) excludes window functions and
  correlated-agg subquery columns for eval-order reasons; positional rows
  need the same list PLUS every consumer of allRowMaps between the scan and
  the aggregate pass: WITHOUT ROWID PK sort, schema-table filter,
  WHERE-driven index reorder, joins, enclosing outer contexts, and plain
  window scans (they fall through to execWindowPass(allRowMaps) — window
  WITHOUT GROUP BY must keep maps; window OVER GROUP BY is fine because the
  group passes materialize per-group maps). Missing one shows up as
  "empty result where rows are expected" — e.g. plain window scans silently
  emitted nothing until the gate caught them.
- **The scan's clone-retention discipline extends by construction**: decode
  fills REPLACE slot contents (never in-place mutation), fresh wrappers are
  exclusive per row, so a clone shares scalars/wrappers and deep-copies only
  []byte payloads — identical to rowMapValue. But NEVER compare interfaces
  with `!=` to detect the copy: `[]byte` dynamic types PANIC on interface
  comparison (cloneReuseSRow). Type-switch instead.
- **Under fleet load, CPU-time (getrusage) interleaved A/B with same-batch
  base exports is the only honest instrument**; wall-clock swings 3x run to
  run while allocs/row stay load-independent. The GROUP phase moved 9.8 ->
  5.8 allocs/row (523 -> 286 B/row) and +55..70% rows/cpu-s; bare scan 3.0
  -> 2.0 (+30..36%). Remaining known mass: exec.(*Engine).EnterAuxAggArg's
  per-arg restore closure (~19% of group-phase alloc objects, exec package =
  sibling scope) and int64 boxing in storage.decodeValue (unavoidable in
  interface{} rows).

## PERF.PARITY-pager — read-path memoization (2026-09-30/10-01, fleet/perf-parity-pager)

- **Validate, don't invalidate, for page-parse memos.** The memoized
  storage.ParsePage result on pager.Page carries a snapshot of the exact
  bytes a parse reads (header + rightmost ptr + CELL POINTER ARRAY) and
  re-compares on every access; no invalidation call sites to chase (writes,
  defrag, splits, rollback restore, external-change cache drops all change
  those bytes or the Page object). The snapshot MUST cover the pointer
  array: a delete+reinsert can net-restore every header FIELD while moving
  pointers — a header-only fingerprint would serve a stale parse.
- **Wire a parse memo only where hit-rate dominates.** Fill cost (snapshot
  copy ~2*CellCount bytes) exceeds a plain parse (48B). Cursor/seek/walk
  reads of stable pages (fresh BTree per statement re-reads the same
  root/leaf) win 40%; insertPage-style mutation-adjacent parses LOSE
  (miss on nearly every call) — leave those on plain ParsePage. Shared
  returned *BTreePage needs a read-only-contract audit: deleteCellOnPage
  mutates the parsed struct it was handed, so any site reaching it stays
  un-memoized.
- **dirtyMark=nil is a semantic no-op that re-arms a fast path.**
  clearDirtySetLocked allocated 2 maps per commit boundary — the pager's
  per-statement floor on read-only workloads (every autocommit SELECT
  flushes). Reuse dirty in place (clear(); trade in only above ~1024
  entries) and DROP dirtyMark: a missing stamp reads as 0, identical to an
  empty map everywhere it is consulted, and markDirtyLocked's fast path
  requires nil.
- **Contention indicts innocent code — twice in one night.** (1) A 40x
  "wall-clock collapse" was load-20+ (other fleet tranches); alloc counts
  stayed load-independent and interleaved GOMAXPROCS=2 A/B showed parity.
  (2) The full harness "hung" 9m48s in temptable2/5.1.3 (O(cells x
  targets) classifyIndexMatches during a 100k-row UPDATE) — pristine-main
  runs the SAME statement in 151-579s depending on contention, and
  isolated A/B = 171s vs 151s (both legacy-drift FAIL). P5 rule holds:
  isolated A/B before chasing any phantom regression; run final validation
  sequentially.
- **A live main checkout is not a baseline.** Sibling tranches hold
  in-flight edits there (its harness showed 4,487 FAIL lines vs 4,479 on
  a pristine export). Baseline = `git archive <commit> | tar -x` into /tmp;
  copy the gitignored fixture dirs (testdata/walconformance,
  testdata/backupconformance — a fresh worktree fails
  TestBackupConformance/walview tests without them).

## PERF.PARITY-rows — positional DML row collection (2026-10-01)

- **Point-loop allocs/op is binary-layout sensitive in BOTH engines; only
  same-main-file, same-build-batch A/B pairs are valid.** The base engine's
  point-UPDATE loop measured 175.6 or 324.1 allocs/op for the SAME commit
  depending on the probe main.go (binary layout shifts inlining/pool luck);
  GC-off does NOT remove it. Protocol: write ONE probe main, copy it to both
  replace-module dirs, build both in the same batch, alternate rounds, and
  distrust any delta smaller than the base engine's cross-build spread. The
  scan loops (allocs/stmt in the tens of thousands) are immune to this.
- **Full-suite (1002 files, one binary) failure SETS are parallelism/order
  dependent here — 4/22/35/669 "failing cases" for the same code.** The
  reliable regression instrument is the per-file isolation sweep
  (FRIGOLITE_TEST=<file> per JSON, xargs -P8, both worktrees, diff): base and
  head both 604 PASS / 409 FAIL, zero differences. Also: fts4merge4 needs
  -timeout 30m when anything else runs concurrently (default 10m truncates
  under contention; passes in ~360s idle on both trees).
- **The per-statement positional plan needs an amortization cutoff versus
  MAIN, not versus the branch.** Seek paths with <=2 (UPDATE) / <=4 (DELETE)
  candidates keep the map collect; measured cutoff=0 regression: point UPDATE
  324 -> 290 allocs/op, DELETE 131 -> 141. Re-measure the cutoff after every
  sibling tranche that touches pooling.
- **DELETE's two identity paths need only their own key material** (WRO:
  OLD-PK keys from declared values; rowid: rowid set membership) — building
  both per statement is waste. Preupdate Old/New slices must stay defensive
  copies: FirePreupdate applies column affinity IN PLACE unconditionally
  (event state is observable via exported accessors without any hook), so
  aliasing a change's values slices would corrupt the write path.
- **GROUP BY follow-up (measured, not done)**: partitionByGroupKey/evalAggregatesGroupBy
  retain `[]RowMap` per group; the GROUP-BY phase profile is StructRowToMap 58% flat +
  wrapPrecomputed 15% + appendRowOutput 8% (66% of phase allocs). Conversion requires
  threading a positional row type through aggRowMaps/outerRows/window passes/
  aggSteppingRows/evalHaving (~15 files in execquery's aggregate machinery) — too
  invasive for a zero-behavior-change tranche; needs its own goal.
- **The DML collect mass was per-row `BuildRowMap` (execdml) + `updateConstraintUnchanged`'s
  TWO `buildRowMapFromValues` per change + `computeGeneratedValues`' pass-map, NOT the
  SELECT scan's `StructRowToMap`** (that one is the GROUP-BY phase, a follow-up). A
  delta alloc profile (pprof `-base` between two `allocs.Lookup` snapshots around the
  phase) settled it in minutes; whole-process profiles pointed at the wrong functions
  because the table-build INSERT mass dominated. Always delta-profile the phase.
- **The SELECT scan's affinity model ports to DML unchanged**: wrap only the columns the
  evaluated expressions reference (WHERE ∪ SET ∪ ORDER BY via `affinityCollector`) +
  the unconditional IPK rowid-alias NULL→rowid fill. The evaluator (execexpr) is
  Row-interface-only — zero `.(RowMap)` assertions — so a `StructRow` evaluates
  identically; `CurrentScanTable`/`qualifiedUnqualifiedFallback` never type-asserts.
  A missing key vs a present-nil slot both evaluate to NULL (rowLookupUnqualified
  falls through to nil), so full-width StructRow slots are safe for pre-ALTER short
  records.
- **The cut line is the consumer contract, and it is discoverable by grep**: triggers
  receive `UnwrapRowMap` (RAW values — the collect-time wrapping was immediately
  discarded), RETURNING/FK/partial-index/expression-index-keys need name-keyed maps,
  everything else (WHERE, ORDER BY eval, PK sort, preupdate values, delete-identity
  keys, index maintenance keys) is positional. `rowMapColumnValues(map)` ==
  `DMLRowSnapshot` (unwrap∘wrap == identity on raw values; IPK substitution on both
  sides), so the positional snapshot is byte-equivalent to what the map path fed
  consumers.
- **IPK rowid-alias substitution is the invisible parity trap in index maintenance**:
  `buildRowMapFromValues` substitutes NULL-IPK → rowid, so index keys read the rowid;
  raw `updateChange.values` hold stored NULL. Any lazy-map conversion of an index-key
  path must re-apply the substitution (`ipkRowidSubstituted`) or indexed-IPK tables
  write mismatching delete/insert index payloads (stale entries).
- **Per-statement fixed costs need an amortization cutoff**: the positional plan
  (~9 allocs: collector, colIndex, wrap slices, StructRow) REGRESSED the 1-candidate
  point UPDATE/DELETE (+14 allocs/stmt on DELETE-by-rowid) before it saved anything.
  Seek paths with ≤2 (UPDATE) / ≤4 (DELETE) candidates keep the map path; scans and
  big candidate sets amortize. Measure the point-loop BEFORE claiming a win.
- **`computeGeneratedValues` ran its fixpoint pass (building a name-keyed map) even on
  tables with zero generated columns** — the early-out alone cut ~13% of the 50k
  INSERT-loop allocs. Same class: `checkConstraints` built its row map per row with
  no CHECK constraints anywhere (the row is read ONLY by CHECK expressions).
- **Benchmarks lie under parallel load** (confirmed again): baseline UPDATE-by-rowid
  read 6.1k ops/s under contention vs 12.5k idle — a phantom "2.1x speedup". Run the
  final A/B strictly sequentially on an idle machine, base and new back to back.

## PERF.P5 — statement journal replaces per-statement pager snapshots (2026-09-28)

- **The 9.4ms no-match DELETE was TWO O(database) costs stacked, not one.**
  The report attributed it all to `execDeleteBulk`'s per-statement
  `Pager.Snapshot()` (deep-copies every cached page). Removing that (statement
  journal, below) only bought ~1ms on this machine: the DELETE's seek plan
  (`seekDeleteRows`) fell back to `collectDeleteRows`' full table scan when
  `fetchSeekRow` found the rowid ABSENT — `!found` was treated as a lookup
  failure. A missing rowid is an exactly-empty candidate set (the equality
  conjunct pins candidates exactly), so `continue`, not fallback. Third stack
  layer: `deleteRowsByIdentity` swept every leaf via `DeleteCellsWhere` even
  for 1 rowid — `btree.DeleteCellByRowID` (already existed for trigger
  cascades) is O(log n) per rowid; use it for sparse sets (<=64), keep the
  single-pass sweep for mass deletes. Profile BEFORE assuming the named root
  cause is the only one: `collectDeleteRows`/`DeleteCellsWhere` in the profile
  named the real remaining costs in minutes.
- **Statement journal capture ordering** (pager.c sub-journal): frigolite's
  btree mutates `pg.Data` in place THEN calls `WritePage`, so capture at
  dirty-mark time is post-mutation. It is still correct for pages CLEAN at
  statement start (their statement-start image is the transaction-start image
  still on disk/WAL — pages reach the file only at commit — so the journal
  stores a from-file entry and rollback restores by cache EVICTION, no bytes).
  Only pages already dirty at statement begin (earlier statement of the same
  transaction; disk image stale) and memory-pager pages (no disk) need a
  pre-mutation MEMORY copy — the READ path (ReadPage fast path + end of
  readPageLocked) is the only point guaranteed before the caller mutates.
  Capture-at-read for every page would have kept the O(database) cost
  (scans touch every page); capturing only begin-dirty/memory pages keeps
  scans O(plan).
- **BeginStatement must be O(1)**: copying the dirty SET per statement is
  O(dirty count) → quadratic for per-row scopes inside big DML (the
  fts4merge4/sqllimits1-7.5 trap again). A monotonic clean→dirty stamp
  (`dirtyStamp`/`dirtyMark` map) answers "was this page already dirty when
  the scope began" with two map ops at capture time and zero work at scope
  open.
- **restoreFileImageLocked consumes s.fileSize**: it truncates the FILE to the
  snapshot size when `p.fileSize != s.fileSize` — pre-assigning `p.fileSize`
  before calling it (naive metadata restore) silently skips the both-direction
  truncate. Pass the metadata through the synthetic PagerState; don't assign
  first.
- **zsh gotcha for fleet runs**: unquoted `$VAR` does NOT word-split in zsh —
  `go test $DIRS` passed the whole list as ONE package ("file name too long").
  Use `xargs`.
- **The quota shim (test_quota.c port) is a flush-failure semantics mine**:
  its growth refusal fires PER PAGE WRITE at flush time (pager flushPage →
  quota.CheckDBFileGrowth), so a quota-failed statement dies with the file,
  the journal sidecar and the cache mutually stale — a state the lazy
  statement journal does not model (quota-2.4.x: a later insert then SKIPS
  its growth check and succeeds where SQLite reports disk-full). Any
  statement-atomicity machinery must keep the legacy whole-state snapshot
  under `quota.Active()`. Also: the quota callback's limit-raise contract
  (test harness sets `*limit = size` when quota_request_ok) only re-checks
  against the group limit inside CheckDBFileGrowth.
- **Wall-clock fleet suites under parallel load lie**: fts4merge4 grind
  (120s deadline) and savepoint2 (a ~90s suite at baseline too) failed with
  deadlines/timeouts ONLY when 3-4 go test processes ran concurrently;
  isolated A/B runs against a baseline worktree showed parity. Always do
  the isolated A/B before chasing a phantom regression — and run the final
  validation SEQUENTIALLY.

## T33-misc — misc2/3/5/7/8 driven green (2026-09-24)

- **WIP 380a22c5d adjudication**: of its ~30-file internal/ delta, only 3 hunks
  were needed for the misc packages (lexer leading-dot literals, evalLimitExpr
  subquery-error surfacing, readDotOp dot retention); everything else (collate
  min/max, setop survivor, compound ORDER BY) was ALREADY covered by main's
  evolved collate-res/idx-coll/tkt2822 merges — confirmed by adopting the WIP's
  13 w5_tkt_pin_test.go pins wholesale: all pass on main + my fixes with zero
  WIP engine grafts beyond the two seams above.
- **btree.c saveAllCursors port (misc8-1.6)**: a nested statement's write
  (eval UDF DELETE) while an outer scan cursor is positioned needs key-save +
  re-seek, because frigolite builds a FRESH BTree per statement over the shared
  (pager, rootPage) — that pair is the BtShared identity, so the cursor
  registry is keyed on it (internal/btree/btree_cursor_save.go). Restore
  semantics are subtle: btreeMoveto's skipnext means a MISSING saved key makes
  the restored next-larger row the Next RESULT (no extra advance); exact hits
  advance normally. Oracle-verified 3 shapes: delete-all mid-scan, delete each
  current row (all rows still emitted), delete a later row (skipped cleanly).
  Known divergence: frigolite decodes all columns BEFORE expression evaluation,
  so a mid-row delete shows the pre-delete value for later columns where
  SQLite's per-OP_Column restore yields NULL (misc8-1.6 row 3: c=9 vs NULL);
  corpus never asserts those bytes.
- **tcl2go 3-word `db eval {SQL} {arrayName} {body}`**: the emitter treated
  rest[1] (the ARRAY NAME) as the body — nested bodies emitted empty (misc2-7.2
  "transpiled-passing" but engine-perfect). Fix: body = LAST word when >=3.
- **fpnum_compare is the do_test contract**: SQLite's TCL suite compares
  string-first, then fpnum_compare (test1.c 6168) — trailing-zero/exponent-pad
  float text differences are EQUAL by design (misc3-2.5: 15-vs-13 digits after
  the point; the expectation file predates printf changes and passes upstream
  only via this fallback). Ported verbatim to the helpers template
  (tclFpnumCompare) and wired into ALL 13 got/want comparison emissions. The C
  comparator is STRICTER than its doc comment (1e-100 vs 1.0e-100 and 1e+5 vs
  1e5 are FALSE — dot/sign pairing breaks first); pinned in
  testgen/misc3/fpnum_pin_test.go.
- **Corpus regen drift**: regenerating a package with the current emitter also
  brings previously-landed emitter features (tclLRange negative-end,
  test_error/test_isolation auto-install) the checked-in corpus predates.
  Regen only the packages you own; diffs confirm identical-except-intended.
- **Skipped tests need FILE side effects too** (misc7-23.1): the later flow
  does `frigolite.Open("tst/test.db")` OUTSIDE any do_test, so a skipped test's
  file layout (mkdir tst, forcecopy) must still be emitted. Added
  fileSideEffectCmd emission (db close / forcedelete / file mkdir|delete|copy
  → os.RemoveAll / os.MkdirAll / tclFileCopy); deliberately NOT file
  attributes -permissions (enforcing readonly dirs is the N/A capability; a
  real chmod would break the engine's own opens).
- **Building the oracle with eval()**: /usr/bin/sqlite3 lacks eval(); compile
  sqlite3.c + ext/misc/eval.c + a tiny main (SQLITE_CORE, sqlite3_eval_init)
  for mid-scan-write ground truth (see /tmp/sqlite351build pattern).
- **quality_gate on tools/tcl2go**: dotest.go/processblob.go were already over
  the 1000-line hard max at base; extract NEW cohesive sections into new files
  (dotest_sideeffects.go, processdb_dbeval.go) rather than growing them —
  processdb_part2.go dropped below 1000 as a side effect.

## MANDATORY RULES (2026-09 update)

- **No skipping missing engine features.** If a testgen package fails because
  the engine lacks a behaviour, IMPLEMENT the behaviour in `internal/`. Do NOT
  classify the package N-A / G7 and supersede it with a native test that
  covers only the subset the engine already supports. The "pure-Go
  supersession" policy (2026-05) is RETIRED: it conflicted with this rule
  and let real engine gaps hide behind native ports. (Recorded 2026-09 during
  P7.WAL-E: the user clarified that "missing elements in the engine MUST be
  implemented, not skipped".) Native tests remain useful as a SUPPLEMENT
  to the testgen suite (oracle-driven regression coverage) but may not
  REPLACE a testgen package whose failure indicates a genuine engine gap.

- **Source-first, complete implementation.** Before any testgen failure, read
  the SQLite C source (`/Users/muaddib/dev/sqlite/src/pager.c` for journal
  machinery, `vdbe.c` for OP_JournalMode semantics, etc.) and port the
  behaviour faithfully. Do NOT simplify the fix. NO TRY/FAIL loops.

- **No "pre-existing" excuses for failures.** Any failing test in the repo is
  a defect that blocks the goal. The user clarified during P8.CORRUPT.C5:
  "on failure you should fix the issue - not find the culprit" — do not
  waste cycles git-stashing to prove the failure predates the current work;
  spend that time writing a §1c pure-Go discriminator and fixing the
  underlying defect. Strengthened in plan/GUIDELINES.md §1d (2026-09).

- **tcl2go file-channel seek: transpile-time vs runtime maps.** The
  `activeFileChannels` and `fileChannelSeek` maps in tools/tcl2go are
  package-level globals used to transpile `seek $fd N` and `puts $fd T`.
  `processSeek` stores the offset in the transpile-time map ONLY when
  the offset is foldable (literal int or `A+B`); for dynamic expressions
  like `seek $fd [expr 1024 + $iCelloffset]`, it emits a runtime
  `fileChannelSeek[%q] = int64(tclAtoi(%s))` line but leaves the
  transpile-time map empty. So `processPuts` MUST consult the runtime
  map (always emit `tclChannelAppendAt(dest, msg, fileChannelSeek[%q])`
  when the channel is in `activeFileChannels`) rather than gating on the
  transpile-time map. (Recorded 2026-09 during P8.CORRUPT.C5 corrupt2-5.1.)

- **tclExecSQL row-vs-cell separator: rows with `\n`, cells with ` `.**
  TCL's `db eval SQL` (no body) returns rows as a flat list; when
  stringified the outer-list elements (rows) are space-joined BUT a
  multi-row result with single-cell rows must match the test's expected
  braced multi-line string, which uses newlines between rows. The
  `tclExecSQL` helper in tools/tcl2go must therefore join ROWS with
  `\n` and CELLS within a row with a single space — not all with space.
  `tclExecSQL2` (column-name + value pairs) needs a separate variant.
  (Recorded 2026-09 during P8.CORRUPT.C5 corrupt2-5.1.)

- **SQLite incremental merge nLeafData charges each appended term's nSpace; new leaf includes height byte. Continuation loads cumulative value; first flush rewrites loaded leaf, then starts fresh leaf. WorkDone counts rewrite, LeavesFlushed counts only newly materialized leaves.**

Guideline: record general methodology and validated approaches here —
knowledge that transfers across tasks. Session-specific debug state belongs
in goal handovers / plan notes, not here. Review and summarize this file at
the start of each goal session to limit context impact; remove or
consolidate stale points.

## Debugging methodology

- **Verify disagreement claims with a direct UT before theorizing.** When two
  views of the same data appear to disagree (e.g., SQL SELECT vs raw btree
  cursor scan, cached vs uncached read), first write a small unit test that
  compares both views directly on the failing scenario. Data-path divergence
  is far more often in the caller's read logic than in storage staleness —
  don't chase "ghost row"/"stale cache" theories unverified.
- **Reproduce outside the slow suite before debugging inside it.** A focused
  scratch repro (pure Go test driving `frigolite.Open`/`Exec`/`Query`, no
  testgen tag) runs orders of magnitude faster than a generated TCL suite and
  pins expected behavior independently of transpiler artifacts. Root-package
  internal tests (`package frigolite`) can even reach unexported internals
  (`db.engine`) when storage-level observation is needed.
- **Re-verify handover claims against current HEAD.** A prior session's
  "X is green/exact" statement is a hypothesis until reproduced. Bisect a
  complex scenario to its EARLIEST failing checkpoint — debugging the last
  stage of a cascade whose earlier stages already diverge wastes sessions.
- **Diff against oracle call-by-call.** Use the sqlite3 CLI as ground truth
  with identical PRAGMA settings (page_size matters for FTS block layout);
  instrument the engine with a per-call debug print mirroring SQLite's own
  logging (e.g. fts3LogMerge / INCRMERGE stderr traces), then compare one
  invocation at a time rather than final states.

## Engine/SQLite knowledge

- **FTS5 test-only functions need a C oracle built with flags.** fts5_expr/fts5_expr_tcl/fts5_fold/
  fts5_isalnum/matchinfo exist only under SQLITE_TEST||SQLITE_FTS5_DEBUG, so /usr/bin/sqlite3 and
  python3 lack them. Build once: `gcc -DSQLITE_ENABLE_FTS5 -DSQLITE_FTS5_DEBUG -I<sqlite> main.c
  <sqlite>/sqlite3.c -o oracle` (main.c = sqlite3_exec over argv SQL). That binary answers every
  fts5_expr/matchinfo question at 3.51.0 exactly.
- **FTS5 expression EOF (zero-token phrase) semantics (fts5_expr.c sqlite3Fts5ParseImplicitAnd):**
  implicit-AND chains DROP a zero-token `""` operand (right EOF dropped; EOF left replaced by the
  right operand); explicit AND/OR/NOT keep it; `+` merges add no term. apPhrase loses the dropped
  phrase, shifting later phrase indices. MATCH 'one ""' == MATCH 'one' (verified vs oracle).
- **fts5 bareword set = alnum + '_' + 0x1A + >=0x80** (fts5_buffer.c aBareword table). 0x1B is NOT
  a bareword in 3.51 (older corpora allow it — drift). Query STRING token `""""` dequotes to one
  `"`, so with tokenchars '""' fts5_expr prints `""""` (2+len*2 quoting).
- **GLOB character classes were missing.** C patternCompare bracket branch: members, a-b ranges
  (prior_c rule), leading-^, `]` first = literal, unterminated class = no-match; GLOB is case
  sensitive byte/rune-wise. Ported in internal/function globMatchClass; oracle-verified incl.
  `[a-]`, `[]d]`, `[^]d]` edges.
- **fts5 full-scan (no MATCH) iterates %_content, not the index** (FTS5_PLAN_SCAN ->
  FTS5_STMT_SCAN_ASC "SELECT cols,rowid FROM %_content ORDER BY rowid"). So a doc whose content
  row was deleted directly disappears from scans while 'n' (index count) stays. frigolite
  ScanDocs now reads %_content for NORMAL/UNINDEXED content.
- **fts5StorageNewRowid: NONE/EXTERNAL content + implicit rowid + columnsize=0 -> SQLITE_MISMATCH
  ("datatype mismatch")**; with columnsize=1 the rowid comes from the %_docsize REPLACE. Also:
  the special 'delete' command is legal on ALL non-contentless_delete tables (fts5SpecialDelete
  uses the SUPPLIED values, no content read); contentless_unindexed promotes content= tables to
  CONTENT_UNINDEXED (config.c:690 chain) and UPDATE of unindexed columns is a content-only write.
- **FTS5 special queries ('*id'/'*reads') bypass expression parsing entirely** — xFilter sets
  FTS5_PLAN_SPECIAL; every aux call on such a cursor fails "no such cursor: <hidden value>"
  where the value is the cursor id ('*id' -> iCsrId, process-wide counter; '*reads' -> reads
  counter). Never parse '*...' in PrepareAux/MatchUniverse paths.
- **fts5 xColumnSize with columnsize=0:** content NONE (incl. content='') or UNINDEXED ->
  -1 per indexed column, 0 for unindexed (fts5ApiColumnSize REQUIRE_DOCSIZE branches); with
  real content C re-tokenizes the content row.
- **Tokenizer ctor args:** C's tokenize directive splits words via fts5ConfigSkipLiteral
  ('-words, '' doubling) or fts5ConfigSkipBareword, then fts5Dequote — NOT gobbleWord escaping.
  And ExprFunc (fts5_expr) must run trailing args through full config parsing so tokenize=
  applies; an empty arg is "parse error in \"\"" (SkipBareword of "" returns NULL).

- **Lazy file creation is global (pager.c)**: opening a 0-byte database must not
  materialize it — not at open, not at close, even though schema.Init builds an
  in-memory page 1. Pager.MarkClean drops dirty flags + re-baselines the change
  stamp without writing; Pager.OpenedEmpty flags the case. frigolite.Open flushes
  only non-empty files.
- **Pager.ResetToEmpty(pageSize)** = backup.c sqlite3BtreeNewDb/newDatabase:
  canonical DefaultHeader written BOTH to pager.header AND page-1 Data[:100]
  (on-disk image must be self-consistent), page 1 empty leaf, numPages=1, file
  truncated to exactly pageSize, then Flush IMMEDIATELY — leaving page 1 dirty
  lets the next per-statement external-file check read a zeroed image.
- **SetPageSize alone leaves the DB header stale** (bytes 16..17 inside page 1
  keep the old size): always pair it with updateDBHeaderField (PRAGMA path does;
  backup paths use ResetToEmpty instead).
- **INTEGER PRIMARY KEY columns are stored as NULL in records** (value = rowid,
  btree.c); readers must substitute the rowid for IPK columns on EVERY select
  path — buildRowMap and applyStructRowAffinity both do now, unconditioned on
  query references.
- **backup.c setDestPgsz scope**: empty dest adopts source page size (ResetToEmpty);
  populated MEMORY dest + mismatch → SQLITE_READONLY; populated FILE dests proceed
  leniently because frigolite's logical rebuild adapts page size during copy
  (documented delta vs verbatim page-copy until P8.STORAGE).
- **Oracle CLI (/usr/bin/sqlite3) writes header byte 20 (reserved space) = 12**,
  not 0: fixture pairs carry reserved bytes; readers must use usable =
  pageSize - reserved for payload math (P6 usable-size reader fix).

- **SQLite incr-merge semantics** (`fts3_write.c`): `merge=A,B` → A = leaf
  quota (`nMerge`), B = min segments (`nMin`, forced ≥2). FIND_MERGE_LEVEL
  picks lowest relative level with ≥ MAX(2,nMin) segments; %_stat id=1 hint
  entries continue partial merges; quota decreases by `1 + nWork` per
  iteration; chomp pushes leftover (level,nSeg) back onto the hint;
  promotion via fts3PromoteSegments only when input fully consumed (nSeg==0).
- **Chomp truncation must shrink sources (P6.FTS-F, fixed)**: chompFTSMerge
  now truncates EVERY surviving source segment — height-0 roots re-serialized
  from the reader's unmerged terms via writeFTSTruncatedSegment (SQLite
  fts3TruncateSegment / SQL_CHOMP_SEGDIR parity). Keeping the original root
  made continuation merges re-read already-merged terms and froze the level
  structure (fts4merge 1.3 stuck at L1:15+L2:4 instead of draining to
  "2 0 1 2 3", then "3 0" after merge=1,4 x100).
- **Never discard the re-serialized root**: the chomp fallback for
  "every entry merged" discarded the fresh single-leaf root and rewrote the
  OLD interior root with start=0 — the segment pointed at a dead block and
  every later read failed "database disk image is malformed", which the
  merge loop swallowed as a silent early-return (heap-priming reader error),
  freezing all subsequent incr-merges. Two rules: (a) when re-serialization
  fits one node, that leaf IS the new root; (b) silent returns on reader
  errors hide corruption — surface them (debug trace showed it in seconds).


## Process

- **GUIDELINE (mandatory): native test before TCL validation.** On any failure,
  first create a dedicated pure-Go "native" test that drives the engine directly
  (frigolite.Open/Exec/Query). Only when the engine passes natively may TCL/
  testgen validation proceed — otherwise the failure is an engine bug to fix
  first. This keeps transpiler hunts from hiding engine defects.
- **Bisect with a worktree + single-package oracle.** `git worktree add /tmp/wt
  <good-ref>`; `git bisect start <bad> <good>`; `git bisect run` a one-package
  test. Fastest way to attribute a regression to an exact commit.
- **Fixture hygiene**: tests that open committed .db fixtures MUTATE them if the
  engine writes at open. Keep fixtures read-only-by-construction and regenerate
  via the fixture tool (-check mode verifies determinism).
- **randblob/random() are nondeterministic** in oracle fixtures: use zeroblob or
  literal payloads so regeneration is byte-identical.

## T33-win (2026-09-25) — window1 regression repair: omit-unused-subquery-column use-walk holes (branch fleet/t33-win)

1. **Bisect inside the suspect merge, not across it.** The brief blamed
   t33-query's positional-ORDER-BY change (2421001cb~1 = 10d2f85d1) for the
   window1 breakage. Attribution runs proved window1 GREEN at 10d2f85d1 and
   RED at 2421001cb itself — the culprit was the SECOND commit of that branch
   (`disableUnusedSubqueryColumns` + view-outer scope), not the positional
   fix. When a merge contains multiple commits, diff each commit separately
   before believing a summary line.
2. **Name-based colUsed approximation must walk everything resolution walks.**
   SQLite sets a source's `colUsed` bit during name resolution (resolve.c),
   which descends into window definitions (OVER PARTITION BY / ORDER BY /
   frame bounds), aggregate ORDER BY terms, FILTER conditions, and
   expression-subquery bodies (correlated IN/EXISTS/scalar). The Go
   analyzer's `exprChildren` treats `Subquery` as a leaf and only descends
   FuncCall.Args — so a subquery column referenced ONLY through any of those
   constructs was wrongly NULLed out by the omit-unused optimization
   (window1-31.2/31.3/48.0/48.1/78.2, all wanting oracle-verified values,
   all rendering NULL/wrong).
3. **A nulled output column can rename itself.** For a compound FROM-subquery,
   rewriting member columns to `&sql.NullLit{}` also changes the derived
   output NAME (ExprString of a NULL literal), so an outer WINDOW clause over
   that column then fails with "no such column" — a downstream symptom that
   looks like a name-resolution bug but is the optimizer's omission. Fix the
   use-walk, not the resolver.
4. **Observable-pin design over single-row probes.** A column lost from a
   window ORDER BY is invisible on a 1-row input (any order sums the same);
   make the subquery multi-row (or use PARTITION BY) so the wrongness shows.
   Conversely, order-key loss on RANGE frames flips the frame content —
   single-row probes do catch that ('abc' vs NULL).
5. **NULL RANGE order-key frame semantics are NOT a bug here:** SQLite keeps
   NULL-keyed rows in RANGE `1 FOLLOWING AND 2 FOLLOWING` frames (verified:
   `... OVER (ORDER BY y RANGE BETWEEN 1 FOLLOWING AND 2 FOLLOWING)` over
   y=NULL returns the row) — frigolite's include-behavior matches the oracle;
   do not "fix" it.
6. **Pre-existing red is out of scope but must be pinned to main:** window6's
   generated test file fails to compile at main (`window6_test.go:499:11: no
   new variables on left side of :=` — tcl2go emitter bug, owned by the
   transpiler agent). Same verification command on main reproduces it before
   spending any time on it in a worktree.
7. **Full-JSON-harness baseline diffing:** a plain full `go test -run
   ^TestSQLiteSuite$` run is chronically red at main (~3.9k subtest failures
   over 387 files — the slowTestFiles/unsupportedTestFiles maps and the
   FRIGOLITE_TEST pattern exist for this; the gate is the testgen corpus).
   To prove an engine change adds no harness regression, diff the FAILING
   FILE SETS (worktree vs main): identical sets = no new impact, regardless
   of per-run subtest counts. Also: a fresh worktree CANNOT pass fixtures
   depending on gitignored artifacts (*.db under testdata/, tools/orafixture
   — oracle-generated runtime files that only exist in the long-lived main
   checkout); triage such failures as environmental before suspecting the
   engine.
8. **T33-win2 — view-declared column lists are POSITIONAL, name-matching is
   not enough:** `CREATE VIEW v(x,y) AS SELECT a,b FROM t1` + `SELECT x,y
   FROM v` NULLs every column under the omit-unused optimization when the
   use-analyzer matches only the body's output names (a,b): the outer query
   addresses the view's DECLARED names, which resolve to source iColumn
   positionally (resolve.c). Fix: pass `ViewDeclaredColumns(entry.SQL)` into
   disableUnusedSubqueryColumns and mark used[i] when an outer reference
   matches declaredNames[i] (length-guarded against the body's output arity).
   The same triage rule as T33-win applies: any NULL/empty result where a
   value is expected, on a query with a FROM-subquery or view, is a
   use-walk hole until proven otherwise — write the pure-Go probe first,
   oracle-verify the want, and enumerate EVERY construct the outer query
   can address a subquery/view column through (this class needed three
   passes: window defs + subquery bodies, then declared view names).

## T33-fts5-resume (2026-09-25)

- **TVF cursor protocol: Column() MUST error out-of-range.** execquery's
  readCursorRowsWithRowids drains columns `for i:=0;;i++` until the FIRST
  error. A vtab cursor whose Column(i) returns (nil, nil) for every i spins
  the query forever (fts5contentless/contentless3 "signal: killed"). Any new
  cursor must bound its column index with an error, like vocabCursor.
- **The transpiler's TCL-proc handling is a failure CLASS, not noise.** Three
  separate packages failed wholesale because the transpiler stubbed TCL
  procs: document() → NULL (fts5contentless4: no segment ever forms —
  oracle-verified C produces the identical empty structure for NULL docs),
  text_value → NULL (fts5content 8.3.x), and brace-guarded `ft($v)` bound
  parameters rendered as bare identifiers `ft(A)` (fts5contentless 4.x —
  oracle: `SELECT rowid FROM ft(A)` = "no such column: A"). Plus one
  INESCAPABLE generated loop: `break` parses the literal "([db total_changes]
  - $nChange)" via Atoi → never true (fts5contentless3 3.6 — the run can
  only time out; same class as fts5interrupt's always-interrupt stub).
  Adjudicate with the oracle before writing engine code: several "failures"
  were the engine matching C on the mangled input.
- **C's bLock is TWO observable guards.** fts5Config->bLock is held across
  %_content statement prepare AND step: (a) a nested query plan of a table
  whose content scan is in flight → "recursively defined fts5 content table"
  (fts5BestIndexMethod, mutual content=t2/content=t1 AND the TVF form —
  oracle-verified); (b) the mirror analogue for a query plan arriving while
  the table's WRITE transaction is open (triggers on any shadow table
  selecting from the table being written, incl. the statement-end xSync
  flush) → "database disk image is malformed" (fts5circref). Implement as
  two counters (scanGuard, writeActive) checked at one BeginQuery chokepoint.
- **C flushes a %_idx btree row per segment unconditionally.** Even a
  single-row INSERT writes one %_idx row (fts5WriteFlushBtree:
  (segid, X'', bFlag+(leaf<<1)) = (segid, X'', 2) for a single-leaf
  segment) and the sync runs with the write transaction open — shadow
  triggers fire mid-write. The mirror now writes the dlidx row and the DML
  layer wraps flushFTS5Shadow in the write scope.
- **C's option parser binds prefixes in a fixed order.** prefix, tokenize,
  content, contentless_delete, contentless_unindexed, content_rowid,
  columnsize, locale, detail, tokendata — first name that extends the user
  key case-insensitively wins ("c" → content; "contentless_delete" skips
  past "content" because the shorter name can't extend). Match with
  strings.EqualFold(name[:len(key)], key) over an ordered table.
- **NATURAL JOIN must skip HIDDEN vtab columns.** The fts5 operand's defs
  carry hidden table-name/rank columns; counting them as common columns
  turns t1(a,b,rank) NATURAL JOIN ft(a) into an a AND rank equality that
  can never match (rank NULL). Filter cd.Hidden in naturalJoinCommonCols/
  generateNaturalJoinOn. Explicit USING(hidden) is legal but requires C's
  argvIndex constraint consumption (parameterized inner scan) — fts5misc
  12.3 stays N-A.
- **Lazy content fetch (xColumn parity).** MATCH-driven universes must not
  fetch DocValues unless the statement reads a user column (star counts;
  `SELECT rowid FROM ft('two')` succeeds even when the content row is
  gone), while a column read errors "fts5: missing row N from content
  table 'db'.'tbl'"; the fts5_tcl.c API layer reports the rc NAME
  (SQLITE_CORRUPT_VTAB) not the vtab message. Full scans stay eager —
  fts5StorageScan walks the content table for the doc set.
- **Predecessor WIP adjudication.** Kept: structvtab.go (fts5_structure TVF),
  vocab decode validation, writeActive (wired), tombstonePageHas/
  tombstoneContains (removed — mirror reads seg.Tombs in memory). The
  predecessor's tombstone.go/flush.go violated gocognit/gocyclo AND
  fts5.go crossed the 1000-line hard cap only after my additions —
  quality_gate.sh + wc -l on every touched file BEFORE committing; the
  pre-commit hook was not installed in this worktree.
- **Re-merge main before finishing.** 33 commits (incl. T33-win2's view
  column-list fix) landed mid-tranche; join2's failure was main-fixed, not
  mine — check `git log HEAD..main | wc -l` before diagnosing cross-branch
  failures.

## T33-idxfix (2026-09-25) — autoindex b-trees mis-ordered at multi-page scale

**Symptom**: `select x from t2 order by x` over a 371-row unique-integer table
(misc5) returned batch-grouped runs, not value order. Small trees ordered
fine; REINDEX "re-fixed" nothing.

**Root cause (two layers)**:
1. `findChildPageForInsert` routed EVERY index-btree insert to the interior
   page's rightmost child (a T31 compromise: compact dividers carry no key,
   so descent was impossible). Each insert batch therefore landed in the
   rightmost leaf, sorted within the leaf by the KeyInfo comparator but not
   globally — batch-grouped order. Leaf-local sorts and single-leaf trees
   masked it; REINDEX re-inserted through the same walk.
2. Index interior dividers were the LEGACY COMPACT shape — (child,
   payload-length varint), no payload bytes — so no key-guided descent was
   even possible, and the interior-index seek paths (`seekInInteriorIndex`,
   `routeInteriorIndex`) read their cell-pointer array at the LEAF base
   (`CellPointer(pg.Data, coff, ...)` — interior arrays live at coff+12,
   pass `coff+cellPtrOffset(type)-8`), decoding garbage.

**The fix is the value-ordered storage tranche (SQLite's actual model)**:
- Dividers carry the FULL separator payload (btree.c:8820 parity): the right
  sibling's first key, duplicated (leaf keeps the entry). Spills to a FRESH
  overflow chain owned by the parent page exactly like a leaf cell
  (`storage.decodeIndexInteriorCell`/`encodeIndexInteriorCell` now honor
  LocalLen/Overflow; `MaxLocalPayload` is the same for index leaf/interior).
- Convention: left subtree < D, right subtree >= D — equal keys go RIGHT
  (the divider is a COPY; the equal entry lives in the right sibling). The
  SAME rule now drives insert descent (`findChildIndexForInsert`, binary
  search over divider payloads via `t.compareKey`), `seekInInteriorIndex`,
  and cursor-restore `routeInteriorIndex`. The pre-fix seeks went LEFT on
  equal — correct for SQLite's promote-don't-duplicate model, wrong here.
- `splitMedianKey` returns the real payload (`partitions[pi][0].key` —
  already a full-payload clone in readCellsForSplit; survives the page
  zeroing during the split rewrite).

**Chain lifecycle (every divider rewrite allocates or frees chains)**:
- `rekeyCarrierChainIndex`: each relocated divider frees its old chain
  (`abandonDividerCell`) and writes a fresh one; deadBytes counts
  on-page bytes incl. the 4-byte ovfl head. Safe because
  `childSplitsHaveRoom` is an EXACT precheck (dividerCellLen includes
  payload bytes) — the loop can never abort midway.
- `splitInteriorPage` frees ALL divider chains after cloning payloads into
  `entries`, before the halves rewrite (each half re-encodes fresh chains).
- `writeInteriorRootAt` must NOT free displaced chains: relocateRootSplit
  ROTATES the old root content VERBATIM into a child slot first — freeing
  there kills live chains (the T31 attempt's temptable2 1.3 corruption).
  Rotation instead re-parents via setChildPtrmaps.
- `setChildPtrmapsInterior` / `reparentPageOverflowChains` re-point divider
  chains (PtrmapOverflow1) when an interior index page moves;
  `createInteriorRootAtPage1` re-points the content moved off page 1.
- `removeInteriorCellRange` frees chains of dropped dividers (shallower
  unlink path); `FreeTable.walkInteriorPages` walks divider chains;
  `absorbChildCellSize` sizes index-interior cells correctly.
- Vacuum was ALREADY ready: `updateOvfl1ParentPtr` +
  `ovfl1CellTypeForPage/ovfl1CellSize` handle interior-owner chains
  (left in place by the T31 revert, unused until now).

**Integrity_check coverage**: `leafOverflows` decoded interior index cells
as index-LEAF cells (child pointer read as a payload-length varint → plen 0
→ Overflow always 0) and read interior pointer arrays at the leaf base
(coff+8) — spilled divider chains counted as "Page N: never used" in
autovacuum tests. Fixed with `chainCellTypeOf` (true cell type per page
kind; interior table owns no chains) + array base coff+12 for interior.

**Debugging wins**: tagging every `fmt.Errorf("database disk image is
malformed")` producer repo-wide with unique sentinels found the failing site
in ONE run (the message flows through verbatim). Counting chain
ALLOC/FREE/DIVALLOC events vs integrity results separated "chain leaked"
from "chain uncounted". The earlier panic-in-ReadPage trick identified a
pre-existing red herring: `freeSpaceWalk`/`appendInteriorChildren`
(schema_tail.go) misread interior pages (leaf array base) and swallow the
error — bogus but harmless; do not chase it from a corrupted-statement
stack alone.

**Balance-path scope**: `balanceNonroot` is table-leaf-only
(`collectBalanceCells` rejects non-table siblings; emptied INDEX leaves stay
in place), so index dividers never enter the DELETE rebalance paths — the
lifecycle sites above are the complete set.

## T33-fts5-resume2 (2026-09-26)

- **Merge of main's idxfix tranche was orthogonal to fts5.** The value-ordered
  index storage (divider payload descent) did not disturb %_idx/%_data mirror
  reads; the only "interaction" was the predecessor's WIP statement-
  granularity fix for fts5detail 5.2/5.3 (compare one flushed segment per
  table, not 2-vs-1). Verify oracle claims in resurrected WIP before
  trusting them — the corrupt-structure expectation re-verified on 3.54.0.
- **An early `return` in a linear generated test MASKS every later section.**
  fts5misc 14.0's `return` hid sections 20-27 for the whole T33 lifetime.
  When you make previously-unreachable assertions run for the first time,
  expect a fresh failure list and probe each (three were engine gaps, two
  were already-adjudicated N-As needing mechanical annotation).
- **MULTI-INDEX OR emission order is branch-major, not rowid-sorted.**
  where.c whereLoopAddOr runs one sub-plan per OR branch (each branch in
  rowid order, RowSet dedup at first emission): 'a' OR 'y' emits 1 4 3, 'y'
  OR 'a' emits 3 4 1. Frigolite's materialized universe sorted globally —
  fixed with Table.MatchOrBranchRowids (branch = pure MATCH conjunction;
  mixed shapes keep the scan fallback rather than guess C's plan).
- **fts5Init registers TWO module scalars beyond the aux family**: fts5(X)
  (API-pointer fetch; NULL for every SQL argument since no SQL value carries
  the fts5_api_ptr tag) and fts5_source_id() ("--FTS5-SOURCE-ID--", C's
  literal). Register arity-exact; the scalar namespace does not collide with
  the module name.
- **Instance APIs have a contentless cutoff**: fts5CsrPoslist returns an
  EMPTY poslist when contentless (content='' / contentless_unindexed) AND
  detail!=full — no content to re-derive positions from. detail=none with
  readable content still has instances (re-derived from content). Guarded at
  the AuxQuery.RowInstances chokepoint so every xInst-family consumer sees
  C's semantics at once.
- **A transpiled UDF stub can be PORTED FAITHFULLY instead of N-A'd**: the
  fts5content text_value proc is three lines (i==1 "one", i==2 "two", else
  "many"); a faithful port flipped 8.3.1/8.3.3/8.3.4 from adjudicated-N-A to
  genuinely green. Before adjudicating an N-A, read the TCL proc — if it is
  portable, port it; N-A is for the untranspilable (VFS fixtures, physical
  page counts), not for lazily-stubbed procs.
- **Debugging win**: when a fix "doesn't fire", print the AST node types at
  the boundary first — the bug was topOrBranches returning nil for leaves
  (append(nil, nil...)); split functions must return []Expr{leaf}, like the
  existing topAndConjuncts.

## T33r-order (2026-09-26) — ORDER/SCAN regression wave

- **A regression wave can hide THREE different root causes behind one
  symptom.** The "final census RED" set (values/distinct/nulls1/with3/
  selectC/unionall all showing NULLs where values belong) split into: (a)
  the omit-unused use-walk naming VALUES-chain outputs by rendered literal
  instead of column1..columnN (values-9.2); (b) the same walk not descending
  into CTE bodies, whose references resolve against enclosing scopes
  (with3-4.0); (c) a window pass leaking unprojected source columns through
  buildSubqueryRowMaps' rowMaps reuse into the enclosing join, shadowing a
  same-named output column of another FROM source (unionall-4.3). Fix (c) at
  the materialization boundary (projectSubqueryRowMaps: expose only output
  columns + dotted internal keys), not by trimming the window pass, whose
  merged source columns serve a legit intra-select ORDER BY.
- **ORDER-BY-via-index satisfaction needs three agreements, not two.**
  Direction matching (forward/backward) is not enough: each term's EFFECTIVE
  collation (term COLLATE > declared column collation > BINARY) must equal
  the index column's (a nocase index cannot provide a BINARY ordering —
  distinct-9.x), and explicit NULLS FIRST/LAST must agree with the scan's
  null placement (ASC index forward = NULLs first; backward flips; nulls1-
  4.3/5.2). Wire every consumer of the gate through ONE predicate —
  orderByIndexRowidTie kept its own stale copy and happily applied a rowid
  tie-break for an ordering the new gate refused.
- **sqlite_autoindex ordinal mapping must mirror the DDL's slot rules.**
  The DDL (createAutoIndexes) gives a rowid table's INTEGER PRIMARY KEY
  alias NO index and NO slot; the DML side prepended the PK to the candidate
  list, shifting every ordinal — sqlite_autoindex_t_1 got keyed on the
  rowid-alias column (payload (rowid,rowid), stored in insertion order,
  useless for seeks) instead of the first UNIQUE constraint's column
  (whereA-3.3, and REINDEX rebuilt the same wrong keys). When two modules
  independently derive the same numbering, diff their rules before
  debugging deeper.
- **Encode-then-check leaks chains.** encodeDividerCell writes a fresh
  overflow chain for a spilled payload; applyChildSplitsRightmost and
  addInteriorCell encoded FIRST and returned errInteriorFull AFTER — the
  caller's split-and-retry then orphaned the speculative chain (backup-3.x
  "Page N: never used", one page per full-parent split). Compute the exact
  encoded size (dividerCellLen) and precheck BEFORE allocating. Debugging
  win: instrument overflow alloc/free for the exact leaked page number and
  the alloc-site owner pinpoints the abandoned encode in one run.
- **usable size, not page size, for every cell decode.** The integrity
  structural walk decoded interior cells with pg.PageSize(); after another
  connection's VACUUM materialized reserved=8 the walk mis-split spilled
  dividers and mis-reported "malformed" even though the pager had adopted
  the new header (reservebytes-1.3.4). Any hardcoded PageSize() next to
  DecodeCell is a latent reserve-bytes bug.
- **state-dependent testgen failures: replay the exact statement sequence.**
  distinct 9.1.1 (no index) passed in isolation but failed under the corpus
  because the failing iteration was tn=4 (nocase index); whereA-3.3 needed
  the corpus's exact fixture (a INTEGER PRIMARY KEY, b UNIQUE) — plain t1
  probes passed. Bisect prefixes with a loop that replays Exec/Query exactly
  (mind Query-vs-Exec and the fixture schema), and never trust a "want"
  string copied from a different schema — two of my probe failures were my
  own wrong wants.
- **Emitter-owned failures to hand to the tcl2go agent** (all verified
  engine-correct or oracle-matching): selectC 1.12.2/1.13.2/1.14.2 (proc
  longname_toupper = string toupper, stubbed to nil — portable, port it);
  whereF 1.x (TCL regexp \y word boundary transpiled as literal y, making
  "SCAN t2\y" require a table named t2y); windowC 1.x (db eval SQL {body}
  per-row validation body dropped, want={} unreachable); e_fkey 4.2 (the
  skipped 4.1 dropped the CREATE TABLE p/c setup 4.2 depends on); pragma
  3.20 (the skipped 3.19 wrongly emitted os.RemoveAll(test.db) — the real
  block only hexio-patches the header); e_blobclose 2.3.3/2.3.5 (proc val
  executes SQL mid-scan; genuinely untranspilable without re-entrant UDF
  support — N-A candidate with evidence).
- **Pre-existing at base, triaged not fixed:** corrupt-7.3 (the INSERT that must
  trip balance-deeper's oversize-cell check doesn't: the engine's leaner
  leaf accounting keeps the root under-full where SQLite overflows — the
  ValidateCellSizeCheck site exists and is correct, it just never fires).
  (bigrow-2.2, the other half of this entry, RESOLVED by attribution in
  T34-bigrow — see that section: the engine was byte-exact all along.)
- **Bisect attribution pays in one hop:** the "Page N: never used" leak was
  absent at all four coordinator candidates but reproduced verbatim (same
  page number) at aa23922dd — the T33-idxfix merge — because divider chains
  did not exist before value-ordered index storage.
## T33r-kernel (2026-09-26) — kernel-singles + emitter wave: the red was a dropped skip map, not the engine (branch fleet/t33r-kernel)

- **Diff the skip-entry SETS across a file split, not just the diff hunks.**
  The 5d.close skiptests2_part2→part3 split (ba9247849, a complexity-refactor
  commit) silently dropped the whole `skipTestsMoreT30Kernel` map (11
  oracle-adjudicated evidence skips); the same commit also added e_fkey-4.1
  with a WRONG "(no-side-effects)" marker. The next full-corpus regen
  re-activated broken assertions and produced an 11-package red wave
  (avfs/mutex1/softheap1/shortread1/e_blobclose/btreefault/bigrow/sqllimits1/
  corrupt/e_fkey/pragma) that looked like engine regressions. Prove the
  corpus-emitter attribution by extracting
  `git grep -h -oE '"key":' <rev> -- tools/tcl2go/skiptests*.go | sort -u`
  before/after and diffing.
- **"got [literal TCL text]" = the emitter, not the engine.** softheap1-1.0's
  want was the literal string "sqlite3_soft_heap_limit -1": an untranspiled
  C fixture command baked into expected position. The fix was a real
  lowering, not a skip: test1.c test_soft_heap_limit (6436) shares the
  pragma.c PragTyp_SOFT_HEAP_LIMIT state, so the fixture lowers to
  `PRAGMA soft_heap_limit(N)` in all three positions (statement, do_test
  body via a new doTestBodyKindHandler, expected value via
  expectedStringExpr). +3 genuinely-running assertions, zero skips.
- **The caught message of a C-wrapper error is per-wrapper in test1.c.**
  test_bind_text/text16 Tcl_AppendResult(sqlite3ErrName(rc)) before
  returning TCL_ERROR (4167/4219) → `catch {...} res` yields "SQLITE_TOOBIG";
  test_bind_int/int64/double/null/blob return bare TCL_ERROR (3850...) →
  res = "". One emission rule per bind KIND (bind-10.8.1 wants {1 {}},
  sqllimits1-5.14.4 wants SQLITE_TOOBIG) — grep the amalgamation's test1.c,
  don't guess a single rule.
- **A skipped do_test must still replay what later tests OBSERVE** — and
  that includes `sqlite3 db FILE` re-binds, not just SQL batches and file
  ops. pragma-3.19 replayed `os.RemoveAll("test.db")` without the reopen,
  leaving db writing to an unlinked inode → the NEXT test failed with
  "attempt to write a readonly database" — two tests away from the actual
  emitter gap. e_fkey-4.1's marker suppressed the CREATE TABLE p/c that
  e_fkey-4.2 reuses → "no such table: c". When a skip breaks a LATER
  assertion, walk the skipped body line by line for un-replayed effects.
  BUT scope the replay: where the skip's N-A subject IS the reopen
  lifecycle (vtab1-1.1x "echo reopen-unregister (C test module)"), replaying
  the reopen re-activates the very seam the skip adjudicated (vtab1 4→8
  failures) — the reopenSideEffectCmd consults the reason and opts out.
  A full-regen diff sweep is how the extra touched packages (misc7/
  corruptB/vtab_shared, all neutral-or-green) were found before push.
- **Scope fixture lowerings with overrideFile(tp)** (the existing per-file
  override seam): sqlite3_soft_heap_limit appears in ~20 corpus files;
  gating to softheap1 kept the regen diff at exactly the targeted package
  (byte-diff discipline). Corpus-wide lowering is a clean follow-up.
- **Per-worktree environment triage order**: gitignored oracle fixtures
  (testdata/backupconformance etc.) first — a fresh worktree fails
  backup/conformance with "no oracle fixtures", and rsync
  --ignore-existing from the main checkout fixes it. Then the parallel-run
  census (600s timeouts are load artifacts — rerun serially before
  believing).
- **Pre-existing ≠ activated**: nulls1/distinct/values fail identically at
  the old-corpus worktree (748fdb03a) with MORE failures (3/2/2 → 2/1/1 now
  — main's engine work has been improving them). They are an active
  engine seam (index-order row emission), not the corpus wave; report with
  counts, don't absorb.
- **Root-package `go test .` needs -timeout beyond 10m** on this machine
  (the 1002-file JSON suite alone exceeds the default in serial runs);
  a bare "FAIL ... 600.365s" with no --- FAIL blocks is the timeout, not
  a regression.
## T33r-fts (2026-09-26) — FTS5 "regression wave" triage: mostly pre-existing transpiler artifacts, two genuine engine fixes

- **Census-compare BEFORE bisecting.** Every package in the mission's
  regression wave reproduced its failure at the census commit (5c2bfa675)
  too — fts5prefix, fts4merge, fts5optimize, fts5contentless2 all fail
  identically there. The "wave" was census-masking, not drift: fixing or
  re-running reaches sections that were previously behind early returns.
  Attribution protocol: run the failing package at the census SHA first;
  only identical-at-census ⇒ pre-existing, different ⇒ bisect.

- **Transpiler artifact classes found this session (all adjudicated in
  portplan/NA_EVIDENCE.md §FULL-SUITE-DRIFT.T33r-fts):**
  (a) do_execsql_test NAMES embedding brace groups (`3.3.$x.$tn.{$colset}`,
  `1.$iTest.$sz.{$s}`) — the name component is executed as the SQL
  statement and the real body is DROPPED (fts5prefix, fts5aj);
  (b) `foreach {a b} $list` two-variable destructuring skipped (variables
  stay empty → "near \",\"" downstream) — fts5prefix;
  (c) TCL double-quoted continuation strings keep their leading quote
  inside the SQL ("unrecognized token") — fts5prefix;
  (d) brace-quoted `t2('c1:x*')` — the `:` hallucinates a `$var`
  substitution (`c1<x>*`) — fts5prefix;
  (e) `while {[proc arg]}` proc conditions → tclBool bare-word fallback
  returns TRUE forever → infinite loop — fts5merge;
  (f) `[db total_changes]` inside a while-1 break expression survives as a
  literal in tclExprWith → break never fires — fts5optimize 2.tn.4,
  fts5merge 5.2;
  (g) `incr` past MaxInt64: TCL 9 bignums make the loop condition false
  (clean exit); the transpiled int64 add wraps to MinInt64 → 1.8e19
  iterations — fts5contentless2;
  (h) `set L 1852` never registered in the TCL var registry → `$L` binds
  NULL → LIMIT NULL = "datatype mismatch" (oracle-verified; LIMIT NULL is
  NOT a no-limit in SQLite 3.54.0) — fts4merge.
  Repairs land IN the generated file when the intended semantics are
  recoverable (literal substitution, overflow guard, dropped-quote removal,
  destructuring); whole-package supersession when the package's substance
  is wall-clock-bound (fts5aj 100k statements, fts5bigid 60k zero-assertion
  statements) — every supersession carries a native pin.

- **Oracle wins:** LIMIT NULL → "datatype mismatch" (3.54.0); a plain fts5
  INSERT moves total_changes by +7 (its shadow SQL counts!), 'merge=1' by
  exactly +1 (structure/blob writes are uncounted); redundant special-
  'delete' leaves MATCH 'one' working while MATCH 'two'/rank/full-scan
  error and integrity-check PASSES — C's doclist-duplicate violation is
  reader-path-dependent; the mirror flags all index reads (documented
  superset). A special-'delete' whose tokens underflow a column total
  errors immediately (2.1); fts5secure4 1.1's never-seen (term,rowid)
  delete stays a silent no-op — both live in the same specialDelete.

- **Engine fixes (this branch):** duplicate special-'delete' markers ⇒
  sticky index-read corruption (internal/fts5 specDelMarkers/idxCorrupt,
  re-insert consumes markers); vtab blob-tier shadow I/O (%_data/%_idx)
  must not move sqlite3_total_changes (vtab.UntrackedExecutor +
  engineVtabDB.ExecSQLUntracked — C counts only its shadow SQL);
  random-free-rowid probing via O(log n) b-tree seek instead of full-tree
  scans (rowIDExistsInTree → SeekToRowID).

- **PERF debt (documented, not fixed — pre-existing at census):** per-
  statement autocommit commits cost ~1-6ms each on darwin (journal
  before-image ReadAt, statement snapshots, GC). fts4merge4 = 700s serial;
  fts5bigid's 60k statements >45 min; profile showed 50%+ in
  Pager.WritePage/journaling, no app-level hotspot. Bulk-statement corpus
  tests should be budgeted as PERF-class, not correctness.

- **SIGQUIT-the-test process** (`kill -QUIT $(pgrep -f 'pkg.test')` after a
  20-40s hang) pinpoints the stuck goroutine instantly — faster than
  profiling for infinite-loop triage; CPU profiles are the complement for
  allocation-heavy slowness (fts5optimize: 110s of WritePage under
  structureWrite per INSERT).

## T33r-vtab (fleet/t33r-vtab, 2026-09-27)

- **Attribution (census 2026-09-24T18:06Z green → 2026-09-27 red, 7 pkgs):**
  - vtab3/vtab1/vtabH/vtab_shared/tabfunc01/tkt_ba7cbfaedc-generated-side:
    NOT the t33r-order merge (7efda8cdf is t33r-KERNEL; 10d05820f is order —
    both innocent). The corpus regen syncs (563dcf819..2bc2153be, §5d
    baseline) re-ran the transpiler over hand-patched generated files and
    CLOBBERED four T30-vtab/T30-kernel hand-patches whose emitter-side
    equivalents were missing or wrong:
    (1) vtab3: `incr ::auth_fail -1` was emitted as `tclIncrMod(&x,-1)` —
    tclIncrMod's arithmetic is ALWAYS +1 (its 2nd arg is the `[incr x] % n`
    condition modulus, introduced T24-vtab af9090613) → the authorizer deny
    counter never reached 0 → nothing was denied. Fix: faithful
    `tclIncrBy(x, N)` (processauth.go emitAuthorizerIncr).
    (2) vtabH: `fileChannelSeek` keyed by the TCL var NAME ("fd") instead of
    the channel's runtime path → x2.txt inherited x1.txt's end offset
    (written 143+153=296 bytes). Fix: channelSeekKey keys by the Go path
    expression for variable-path channels (processmisc/processblob); literal
    paths keep the historical key (byte-identical corpus).
    (3) vtab1: three patches — per-statement skip-side-effect replay (a
    combined Exec aborts at the first error and strands the rest;
    vtab-1.2152.4), the reopen-unregister skip must opt out of the HALF
    close replay too (the regen emitted a bare `db.Close()` and everything
    downstream ran on a closed connection), and empty-list expectations
    (`{}` / `[list]`) must render the harness 0-row form `"{}"` not `""`.
    (4) tabfunc01: wantOverride `tabfunc01:1370` (series.c step-zero
    normalization; oracle 3.54 returns "0" for generate_series(0,0,0)).
  - **Rule: a hand-patched generated file is a LOAN against the next regen.**
    When hand-patching is the T-answer, the same session must land the
    emitter-side equivalent (wantOverrides entry, shape fix, or keying fix)
    or the next corpus regen silently reverts it.
  - tkt_ba7cbfaedc (ENGINE): a9a61dd19 (t33-idx "ORDER BY emits the index
    b-tree's stored key order") — emitRowsInIndexOrder returned SUCCESS with
    an identity permutation for GROUP BY outputs (group representative
    rowMaps carry no `rowid`), leaking the ASC group-key order over all-DESC
    ORDER BY. Fix: decline when `scanRowidPositions(rowMaps)` is empty;
    the comparator sort owns grouped ORDER BY.
  - tkt_3a77c9714e (ENGINE): 2421001cb (t33-query omit-unused-subquery-
    column) — the correlation check treated every UNQUALIFIED reference as
    internal, so a body whose `WHERE Connected=SrcWord` reads the outer
    UNION-scan was "uncorrelated"; the pass nulled the un-referenced SrcWord
    output and the query lost all rows. Fix (resolve.c parity): unqualified
    references resolve scope-by-scope against each scope's source columns
    (named tables via schema; derived sources via expanded output names);
    a name no scope supplies = correlated = optimization declines. Bare `*`
    is a wildcard, never an outer reference; unresolvable scopes keep refs
    internal (only shrinks the optimization). selectH counter/star pins stay
    green — the optimization still applies when references resolve in scope.
- **Oracle wins:** GROUP BY x,y ORDER BY y DESC,x = y-DESC groups with x ASC
  inside (not the full reversal); generate_series(0,0,0) → row `0`.
- **Method:** engine-vs-emitter isolation = `git checkout <census> --
  testgen/<pkg>` into the HEAD worktree (old gen + new engine) vs the same
  package at the census commit; a package that flips with ONLY the generated
  files changed is emitter-side, one that fails both ways is engine-side.
  Native probes (frigolite.Open/Query) reproduce without the harness; the
  same probe binary dropped into throwaway worktrees bisects an engine
  regression in minutes.
- **Worktree ori trap:** `rm -rf ori && ln -s <main>/ori ori` deletes the two
  TRACKED files (ori/sqlite/test/genesis.tcl, rtree_util.tcl). Instead:
  `mkdir -p ori/sqlite/test` as a real dir, cp `*.test *.tcl` from main
  (~1221 files, *.test is gitignored), `git checkout -- ` the two tracked
  files. git status stays clean and the transpiler finds its inputs.

## T34-vacuum (fleet/t34-vacuum, 2026-09-27) — vacuum-test drift resolved; corrupt-7.3 engine contract landed

- **Stray autocommit COMMITs** in TestVacuumDoesNotCorruptBTree were written when
  execCommit had no "no transaction is active" guard (added later, oracle-verified
  for interrupt-3.x); the bare COMMITs became expectation drift. Oracle rule that
  keeps these from being "fixed" on the engine side: a bare COMMIT after autocommit
  statements MUST error — `/usr/bin/sqlite3` errors identically ("Error near line 4:
  cannot commit - no transaction is active"). When a test predates an error contract,
  diff the test against the oracle before touching the engine.
- **macOS /usr/bin/sqlite3 (3.54.0) has 12 reserved bytes per page** (header byte 20;
  usable = pageSize-12; verified via maxLocal overflow boundary: randomblob(980)
  overflows at page_size=1024 → maxLocal 977, not 989). Oracle byte-layout diffs are
  only comparable after normalizing reserved bytes; the testgen corpus targets the
  reference build (reserved=0), which frigolite's layout matches exactly.
- **corrupt-7.3 root cause was TWO engine divergences, not layout** (frigolite's
  24-byte cell layout already matched the reference build: rowid 10's blob at page
  offset 788, root leaf 2 bytes from full after 39 inserts):
  1. Same-size UPDATE went delete+reinsert (applyUpdateChanges bulk pass) — the
     delete pass compacts the page and rewrites the cell pointer array, DESTROYING
     the crafted corruption before the INSERT runs. SQLite (btree.c:9596-9614,
     sqlite3BtreeInsert loc==0): same-size + fully-local overwrite = memcpy at the
     SAME offset, pointer array untouched, no balance(). Implemented as
     btree.OverwriteCellByRowID (guard list: old fully local, szNew==szOld,
     !autovacuum||szNew<minLocal, new payload fully local by formula; bounds
     <coff+10 / >pageSize → ErrMalformedImage) wired into BOTH update appliers.
  2. An overfull ROOT leaf reconciled via split-then-relocateRootSplit unless the
     new cell could not fit an EMPTY root. btree.c balance() routes EVERY overfull
     root leaf through balance_deeper (src/btree.c:9115-9129) — copyNodeContent
     (RAW byte copy: content area verbatim + header/pointer array rebuilt at the
     child's header offset; cells are NOT decoded) then balance the child. The
     raw copy is load-bearing: the copied child's btreeInitPage (CellSizeCk →
     storage.ValidateCellSizeCheck) is the canonical corruption-detection site.
- **Page-allocation order flipped for root splits** (balance_deeper allocates the
  child FIRST, then the child's split allocates the sibling) — final 2-leaf layout
  is identical, but unit tests that pick leaves[0] and hand it to ptrmap machinery
  can collide with ptrmap page arithmetic (page 2 for 1KB pages) in harness pagers
  that are not in autovacuum mode. Real autovacuum allocators skip ptrmap pages;
  harness pagers don't — guard test sources like the targets.
- **Oracle cross-check gotcha (corrupt.test on macOS CLI):** the corrupt-7 sequence
  still produces "database disk image is malformed" on the INSERT even though the
  oracle's root split EARLIER (usable 1012 vs 1024): the corruption hits an interior
  root's rightmost pointer instead of a leaf's cellPtr[0]. Message-level oracle
  checks survive layout shifts; byte-offset reasoning does not transfer across
  reserved-byte differences.
- **SELECT after crafted-pointer corruption**: frigolite's scan silently returns no
  rows (count 0, no error) on the corrupted root; the oracle errors "malformed" on
  the same scan. Not part of the corrupt-7.x corpus contract (no assertion between
  7.2 and 7.3) — left as-is; revisit only if a corpus case pins scan-after-craft.
## T34-bigrow (fleet/t34-bigrow, 2026-09-27) — bigrow-2.2 residue: engine byte-exact, the "2-byte loss" was the emitter's want rendering

- **Dissect the printed got/want before believing a symptom summary.** The
  T33r-order residue note ("UPDATE swapping a ~65KB value returns it minus
  the first 2 bytes") was a mis-description of the emitted bigrow-2.2
  mismatch. Extracting the failure text from the base (01e0371e4) corpus run
  and diffing programmatically: got = big1 byte-exact (65520 chars, ends
  "9360 "), want = tclListFlatten(big1) = big1 minus its trailing space
  (65519 chars, ends "9360") — 65519-char common prefix, delta = ONE
  trailing space in the WANT. The engine never dropped anything; the
  emitter's list-flattening collapses the trailing space of the
  single-element [list $::big1] want (exactly what the emitted skip note
  says). tclQuoteListElem does not brace space-only values, so flatten()
  returns the value verbatim — the shapes differ only in that space.
- **Attribution protocol that settled it in one pass** (per the pure-Go
  supersession policy): (1) pure-Go probe replaying the exact corpus
  statement sequence (1.2→2.2, including the index and both swaps) compares
  byte-for-byte and PASSES at HEAD, at 01e0371e4 and at 9372fbb85 — pure-Go
  green + transpiled red = transpiler suspect; (2) oracle /usr/bin/sqlite3
  byte-exact on the same sequence (and on a size×page-size matrix);
  (3) run the BASE-emitted corpus package and diff its got/want strings to
  see the rendering delta with your own eyes instead of trusting prose.
- **Engine audit that closed it** (all SQLite-faithful, nothing changed):
  storage.LocalPayloadSize ports btreeParseCellPtr's surplus formula
  (minLocal + (payload-minLocal)%(usable-4), capped by maxLocal; table-leaf
  maxLocal = usable-35, index = (usable-12)*64/255-23); UPDATE has NO
  in-place cell rewrite — writeUpdateCell deletes the old cell and re-inserts
  through the same InsertCell/prepareCell path as INSERT; prepareCell spills
  c.Payload[local:] from offset 0 and readOverflow reassembles bounded by
  PayloadLen. No off-by-2 exists anywhere in the overflow encode/decode.
- **Pin:** frigolite_t34_bigrow_test.go (4 tests, ~170 subtests) — the exact
  corpus sequence, swap shapes × ps 512..65536 × index/no-index, a 24-size ×
  5-page-size UPDATE-rewrite sweep (256/257 and 65536/65537 transitions,
  minLocal/maxLocal boundaries), and a per-page-size SEAM sweep (partial-local
  branch + seam-CROSSING sizes where an UPDATE's +3-byte growth flips local
  maxLocal→minLocal and rebuilds the overflow chain, e.g. ps=4096
  n=8152→8155), each byte-exact in-session, after PRAGMA integrity_check,
  and after close/reopen; wants oracle-verified. The testgen/bigrow skip
  stays (emitter-owned; un-skipping needs the tcl2go want fix, not an
  engine change).
- **Parallel test subtests perturb state-sensitive root-leg tests.** The
  sweeps first ran with t.Parallel() (~100 concurrent open DBs) and the
  full-root run flipped TestP8IncrVacuum3OracleSequence (freelist_count=21
  vs 0) — a test that passes isolated and fails isolated at base too
  (pre-existing state dependence). Running the sweeps sequentially (~1s)
  removed the interference; new root-package tests that open many DBs must
  not parallelize.
- **Test-writing trap:** t.Fatalf arguments are evaluated even when the
  guard short-circuited — `len(res.Rows[0])` inside a Fatalf format list
  panics on a 0-row result. Capture into a local under the guard first.

## T34-x6 (fleet/t34-x6, 2026-09-27) — fts-x6 writer divergence was an ENGINE bug (NULL marker row), not fixture staleness

- **Verdict (b)**: TestWriterConformance/fts-x6-growth diverged because the
  ENGINE declined SQLite's append-to-existing-output continuation, not because
  the fixture predated 91e4296b5. The "diverges since 91e4296b5" attribution was
  wrong — the failure reproduces identically at 91e4296b5^ (bd8effd42) with the
  intact fixture. The reader-only position-bleed fix is irrelevant to writer
  bytes (the x6 scenario runs no queries). Re-verify residue attributions by
  checking out the blamed commit in a scratch worktree before acting on them.
- **Fixture hygiene trap**: the `ftsconformance/*.db` oracle fixtures are
  GITIGNORED (`*.db`) local artifacts, as is `tools/orafixture` itself. A fresh
  worktree runs the conformance test with MISSING or stale-local fixtures —
  TestSegviewOracleX6InteriorNodes "database disk image is malformed" was a
  0-byte `fts-x6-growth.db` (pager opens lazily, then btree on a missing page),
  not an engine regression. Copy the fixture set from the main worktree (or
  regenerate) before diagnosing; the canonical set lives at
  `/Users/muaddib/dev/frigolite/internal/fts/testdata/ftsconformance/`.
- **Oracle-version drift vs content comparison**: `/usr/bin/sqlite3` moved
  3.51.0 (ORACLE_VERSION at generation time) → 3.54.0. `orafixture -check`
  (whole-file byte compare) FAILS across that drift, but the FTS shadow-table
  CONTENT (the conformance comparison surface: segdir rows + segment block
  bytes) is byte-identical for all 5 scenarios. Compare CONTENT across oracle
  versions; whole-file equality is only meaningful within one oracle build.
- **Root cause (engine)**: `fts3IncrmergeWriter` pre-allocates the output's
  block range and writes a `(iEnd, NULL)` row in `%_segments`; `fts3IsAppendable`
  (fts3_write.c) detects the appendable segment by
  `SELECT 1 FROM %_segments WHERE blockid=? AND block IS NULL`. The marker row
  is DATA for the writer, not corruption. frigolite's
  `execddl.decodeSegmentBlock` returned "malformed [SEG16]" for a NULL block
  column, so the continuation's GEOMETRY fallback
  (`ftsMergeRun.loadGeometryFallback`) declined the append (`readFTSBlock`
  errored on the marker) and created a NEW output segment: fts4growth 7.4
  produced level-1 idx=1 where SQLite extends idx=0 in place (leaves_end
  744→769), and the level arithmetic cascaded (final segdir level 2 vs 1,
  636896 vs 635247 segment bytes).
- **The geometry fallback exists because the in-memory merge state dies**: the
  MergeCtx (FTS3Table.mergeCtx) is wiped by ANY direct SQL write to a shadow
  table (InvalidateSegmentCache — the scenario's 7.3 `UPDATE x6_segdir SET
  end_block=...` does exactly that). A continuation must then be decidable from
  persisted state alone: segdir geometry + the NULL marker row + the term-order
  check. Fix = `case nil: return nil, nil` in decodeSegmentBlock (C parity);
  consumers that need leaf content still fail at parse time (a NULL inside a
  leaf range → loadLeafBlock's height-varint read → "corrupt segment root"),
  and the integrity walk's checkSegdirLeafBlock flags a NULL below
  leaves_end_block (that carve-out was dead code until this fix).
- **Native pin method for corpus-dependent FTS contracts**: embed a COMPACT
  deterministic corpus (12 KJV-Genesis verses x 6 copies x 6 rounds — small
  enough for a test file, large enough that merge=25,4 stops at its quota),
  derive the expected segdir/geometry scalars from `/usr/bin/sqlite3` on THAT
  corpus, and hard-code them (TestT34X6_FTS4GrowthMergeContinuationPin). This
  covers the contract of the skipped testgen fts4growth 7.4-7.7 cases
  ("MergeFTS-continuation divergence") without the 269KB genesis_t1.sql and
  without the untracked fixture .dbs. Verify the pin corpus actually reproduces
  the dynamic (quota stop → chomp → append → drain) on the oracle BEFORE
  pinning: the first 10-verse attempt merged everything inside the quota and
  exercised nothing.

## T34r-btree-resume (fleet/t34r-btree, 2026-09-27) — 64KiB CellContent wrap + pad-4 leaf cells landed; fts4aa attributed

- **64KiB cell-content wrap was the tranche-wide root cause (btree01 + fts4aa).**
  On page_size=65536 an EMPTY page's content start = usableSize = 65536, which
  truncates to 0 in the 16-bit on-disk header field (C zeroPage writes
  (u16)usableSize; C never re-reads the field — insertCell works from MemPage
  in-memory offsets). frigolite re-parses page headers to drive cell-content
  arithmetic, so the wrapped 0 poisoned every subsequent `content - len(cell)`
  computation → `errInteriorFull` ("interior page full, cannot add child
  pointer") once the T34-vacuum tranche routed overfull ROOT leaves through
  balance_deeper (which re-parses/re-inits the copied child). Fix: CellContent
  widened uint16→int with a ParsePage normalization (0→65536 on 64KiB pages
  only) + call-site adaptation; validatePageHeader keeps accepting exactly the
  normalized shape. SAME root cause fixed testgen/btree01 and testgen/fts4aa
  (fts4aa builds its FTS4 corpus at PRAGMA page_size=65536, fts4aa_test.go:284).
- **pad-4 leaf cells is C parity, not a hack** (cellSizePtrTableLeaf /
  cellSizePtrIdxLeaf: "if( nSize<4 ) nSize = 4"): a fully-local leaf cell
  smaller than 4 bytes (all-NULL single-column record: 3 bytes) is ALLOCATED
  4 bytes with dead trailing bytes. Needed on BOTH sides: encode
  (padLeafCell in storage/cell.go) and size accounting (TableLeafCellSizeAt +
  cellsize_check walkers + btree_shallower absorbChildCellSize), else a tiny
  cell sits at usableSize-3 and trips the btreeCellSizeCheck bound
  (pc <= usableSize-4) after the next split. Fixed testgen/changes
  (5000-row recursive NULL insert).
- **fts4aa bisect attribution** (throwaway detached worktrees, removed after):
  PASS at f2433a886^ → FAIL at f2433a886 (T34-vacuum merge) → FAIL at
  8d589f1e8 → PASS with the CellContent-wrap fix. The vacuum tranche's
  unconditional balance_deeper for overfull root leaves EXPOSED the wrap; the
  regression was in the interaction, not balance_deeper itself.
- **reservebytes root cause (analysis gift to the owner session; NOT fixed
  here):** the vacuum copy-back lays rows out with usable=1016 (reserved=8
  honored by the btrees) but the final header byte 20 stays 0, so a FRESH
  reader decodes local=104 vs written 102 → every overflowing cell misread →
  integrity_check "Page N never used" (overflow pages unreferenced) + empty
  scans; the WRITER connection passes integrity_check only because its pager
  cache still holds the old image. C parity: vacuum.c:271 applies
  nRes = GetRequestedReserve(pMain) to the TEMP (vacuum_db) BEFORE the
  schema/row copy (SetPageSize(pTemp, mainPageSize, nRes, 0)), so the temp's
  header byte 20 = 8 and its layout already carry the reserve; the page-level
  copy-back (sqlite3BtreeCopyFile) then transfers the header verbatim, and
  vacuum.c:383-385 re-syncs main via SetPageSize(pMain, tempSize, nRes, 1).
  frigolite applies the reserve only to main mid-rebuild (vacuumResetDest)
  and its two LOGICAL backup copies never stamp byte 20 into the rebuilt
  image. Fix direction: apply reqReserve to the :memory: temp before the
  first copy (and/or stamp byte 20 from the pager's reserve wherever page 1's
  header is rebuilt after the copy-back).
- **Shared-worktree protocol (two live agents):** when another session owns
  adjacent files, commit with EXPLICIT paths only; split a co-edited file with
  `git apply --cached` on hand-built hunks (recompute hunk line offsets for
  the applied subset; include trailing blank context lines or the patch is
  "corrupt"); run gocognit/gocyclo/staticcheck on the STAGED versions
  (`git show :path`) — the working tree can carry the other agent's
  env-gated tracing that inflates complexity (ValidateCellSizeCheck hit 41
  cognitive in the working copy while the staged version stayed clean).

**T34r-reserve stand-down**: reservebytes fix ownership returned to the T34r-vacuum
owner session (its pagerconfig.go byte-20 stamping + usable-end re-anchoring is the
C-parity fix; my temp-side vacuum.c:271 analysis above is its reference). No merge
conflict risk maintained by zero edits — read-only diagnosis only, nothing staged.
## T34r-btree (reservebytes close, fleet/t34r-btree, 2026-09-27) — vacuum copy-back reserve propagation

- **Root cause chain (reservebytes 1.3.4/1.3.5/1.4.x):** the vacuum copy-back's
  logical rebuild FAILED with "database disk image is malformed" and the
  pre-existing restore-on-failure fallback (`vacuumRebuild`) masked it while
  re-copying with keepDestPageSize=false — a second ResetToEmpty stamped a
  DefaultHeader (byte 20 = 0) onto a pager whose p.reserved=8 still drove
  every btree layout decision. Result: cells laid out with usable=1016, the
  header claiming reserve=0 → a FRESH reader (or db2 after cache
  invalidation) decodes local=104 vs written 102, every overflowing cell's
  chain pointer misreads, integrity_check sweeps "Page N: never used" over
  all overflow pages, and byte-20 probes read 00 where the test wants
  08/10. The WRITER passed integrity_check only because its own pager state
  still agreed with the layout.
- **The primary rebuild's CSC failure was itself a reserve bug:** the copied
  root leaf's first cell sat at pageSize-108 (916 on a 1024 page) — packed
  from the PAGE end instead of the USABLE end. C's zeroPage anchors the
  empty-page content pointer at pBt->usableSize (put2byte(&data[hdr+5],
  pBt->usableSize), src/btree.c:2189); at reserve=0 the two coincide (every
  prior test), at reserve=8 the delta pushes cells into the reserved tail
  and btreeCellSizeCheck rejects the page. Fixed EVERY pageSize-anchored
  content-start writer, not just the one the test tripped: pager
  ResetToEmpty (which must also carry the materialized reserve into its
  fresh DefaultHeader byte 20), ApplyReservedBytes (re-anchors page 1's
  EMPTY schema-leaf content pointer to the new usable), execddl
  initIndexRootPage, btree_tail's empty-root rewrite,
  writeInteriorSplitLeft/Right, writeInteriorRootHeader and
  createInteriorRoot's page-1 branch.
- **ValidateCellSizeCheck takes the USABLE size, not pageSize** (btree.c
  btreeCellSizeCheck bounds: iCellLast = usableSize-4, pc+sz <= usableSize,
  xCellSize formulas usable-based). balance_deeper's caller passed
  t.pageSize — identical at reserve=0, wrong at reserve>0.
- **Reader-side stale header (db2):** schema.checkExternalMod's
  pager.InvalidateCache dropped the page cache but NOT the cached header, so
  the second connection kept walking with the pre-VACUUM usable (C's
  lockBtree re-reads page 1 on every new read transaction). InvalidateCache
  now re-reads the 100-byte header and adopts page size + reserve via
  adoptHeaderPageSizeLocked when it still parses; a non-parsing header
  keeps the headerCorrupt deferral (filefmt-1.2 contract).
- **Debugging protocol that cracked it:** trace the INVARIANT (header byte 20
  vs p.reserved) at the flush boundary instead of grepping mutation sites —
  the divergence print at flushPage(1) pinpointed the failing path in one
  run; panic-at-error-site localization (CSC-BAIL,
  PRIMARY-COPYBACK-FAILED) peeled the layers one at a time. Remove ALL
  instrumentation before committing — and on this shared worktree the
  branch was switched under the agent mid-session (fleet/t34r-reserve ↔
  fleet/t34r-btree): verify `git branch --show-current` before every commit
  and push with the EXPLICIT refspec `git push origin
  HEAD:refs/heads/fleet/t34r-btree` (the colon-less form followed
  upstream=main).

## T34-perf-resume (fleet/t34-perf, 2026-09-27)

Resumed a dead predecessor mid-tranche on P9.PERF hot-path work (base 5807a9c1e,
5-file WIP that did not compile). Adjudication + three tranches landed.

- **Resume protocol value proved immediately**: the WIP did not compile
  (`ipkAliasIndices` never written; two `fillStructRowFromTypes` call sites
  still on the old signature) and carried a LATENT BUG the first fence missed:
  the keyword length-switch used wrong lengths (CURRENT_TIME written as 11
  chars, actually 12; CURRENT_TIMESTAMP 16, actually 17), so
  `SELECT CURRENT_TIME/CURRENT_TIMESTAMP` returned empty. The mission fence
  (insert/update/select/index families) does NOT evaluate CURRENT_*; only the
  expr testgen package caught it. LESSON: any fence for expression-eval changes
  must include testgen/expr + testgen/collate*; length-switch tables must be
  derived with `len()` checks, not eyeballed.
- **Pointer-keyed memos on an engine-scoped Evaluator must be statement-scoped**:
  the WIP memoized exprCollation by AST node pointer on the connection-lifetime
  Evaluator and claimed "statement-scoped" — false. After GC of a freed
  statement's AST, a later statement can reuse addresses and serve stale
  results. Fix: clear in ResetStatementAux (already called per outermost
  statement via resetOuterStatementScopes). Statement scope also preserves the
  hit-rate (per-row reuse is within one statement anyway).
- **Static fast paths beat memos**: after memoization the collation map lookup
  itself was 30% of evalExprWithCollation. exprCollation resolves ("", false)
  WITHOUT consulting operands for every BinaryOp except COLLATE/|| and for
  every node type outside {BinaryOp, FuncCall, CaseExpr, UnaryOp} — so
  exprCanCarryExplicitCollation short-circuits before the memo. Same-window
  A/B vs base: Select1 −18%, Range5k −18% for this change alone.
- **op-keyed dispatch maps are per-row overhead**: binaryOpDispatch (string
  hash per comparison per row) → three switch helpers, ~−11% more on scan
  benches. Keep maps only when keys are dynamic. Complexity gates (gocognit 15
  / gocyclo 12) force splitting a 19-case switch into small helpers — split by
  operator family, and note `return f(), true` is illegal Go for multi-value f.
- **Affinity-plan gating subtlety**: fillStructRowFromTypes's old
  `if affinityCols != nil` gated BOTH the wrap AND the IPK rowid-alias refill;
  the plan-nil gate must reproduce that exactly (affinityPlan.apply handles
  both; nil plan skips both), while fillStructRowRemainingFromTypes's refill is
  UNCONDITIONAL — hence a separate always-computed ipkAliasIndices list.
- **Benchmarking on the shared fleet host**: sibling agents' test runs (one
  frigolite.test burned 576% CPU for ~40 min) make absolute ns/op meaningless
  and can fake regressions (Update3 read 10.1s→17.7s under load with NO code
  change). Mitigation: same-window interleaved A/B against a `git archive`
  export of base in /tmp (no stash, no extra worktree), compare relative
  deltas only; never trust single-window numbers.
- **Harness validation**: full TestSQLiteSuite needs -timeout 3600s (default
  10m panics mid-run with a goroutine dump). Pre-existing env failures on this
  host: 13 subtests of 8_3_names/f_8_3_names/walcrash2 (macOS shortname
  fixtures, crash-sim) — byte-identical failure set on base, so not
  regressions; TestBackupConformance fails for missing oracle fixtures, also
  on base.

## T34-planner (fleet/t34-planner, 2026-09-27) — SEARCH-plan ORDER BY consumption (wherePathSatisfiesOrderBy)

- **Probe first, on BOTH engines, with data.** The sqlite3-oracle EQP battery
  (t table + ASC/DESC/multi-col/unique indexes + ~30 SELECT shapes) mapped 10
  divergence classes before any edit. Trap: running the battery through
  `EXPLAIN QUERY PLAN <DDL>` executes NOTHING — schema/rows never exist and
  the plans are empty-table plans; Exec the DDL first, EQP only the probes.
  Second trap: a battery without INSERT produces different plan choices
  (unique-index preference disappears).
- **wherePathSatisfiesOrderBy model (single loop), now mirrored in
  execquery/select_order_search.go:** (1) pre-pass — every ORDER BY term
  whose column an AND conjunct fixes (col=const / col==const / col IS const /
  col IS NULL) is consumed without direction, collation must agree for
  value-carrying equalities (IS NULL skips it); (2) walk — index columns in
  key order, skipping the equality-bound prefix (nEq = equality-run only; a
  trailing range column is a FREE column and still orders); the FIRST
  unsatisfied term must name the column (one candidate per column — bOnce),
  under the column's collation, with ONE scan direction shared by all free
  matches (rev = idxDesc ^ termDesc); (3) mark-off — when the loop is
  order-distinct (unique index, walk reached past the last key column, free
  columns NOT NULL, no IS/ISNULL in the prefix), remaining table-column terms
  are consumed (distinct rows, no ties, term is a no-op).
- **The order-distinct/NOT NULL subtlety is load-bearing:** `WHERE a=1 AND
  d=2 ORDER BY c` over a NON-unique (a,d) index needs a sorter (rows can tie
  on (a,d)); over the UNIQUE index it does not (one row). Nullable free
  columns void distinctness (tag-20210426-1) — duplicates via NULL make the
  remaining term matter.
- **EQP and runtime emission must share ONE gate.** planSingleTable now
  computes a scanLoop once (scanLoopForQuery) and renders from it; the same
  struct feeds orderByConsumedByLoop (drops "USE TEMP B-TREE FOR ORDER BY")
  and loopOrderedEmission (permutes the result rows into the index b-tree
  order via emitRowsInIndexOrder, incl. backward rowid-desc ties). The
  legacy naive sortCoveredByIndex suppressed the sorter for the WRONG index
  (`a=1 ORDER BY c` found a sort-index i2 while the scan used i1) — corpus
  still green because no green test asserted those shapes, and the where.c
  gate fixed the direction (mixed-direction plain scans gained the sorter
  sqlite has).
- **Known remaining N-A (documented, do not "fix" blindly):** IS NULL seek
  refs (collectIndexedRefs has no IS NULL ref, so those plans stay SCAN+sorter
  where sqlite SEARCHes), IPK range rendering (rowid>3 renders SCAN, oracle
  SEARCH IPK), rowid-range vs full-index-scan cost choice, partial sorter
  text ("USE TEMP B-TREE FOR LAST TERM OF ORDER BY" — needs block-sort
  semantics, out of scope), skip-scan ORDER BY consumption.
- **Complexity gates bite refactors:** the walk decomposed into
  walkColumnStep/matchWalkColumn to stay under gocognit 15 / gocyclo 12;
  adding ONE if to a 12-gocyclo function (sortRowsWithMaps) trips the gate —
  run gocognit/gocyclo on every touched file before committing (the
  pre-commit hook was not installed in this worktree either).

## T34r-split (fleet/t34r-split, 2026-09-27) — vacuum6 "interior page has no cells to split" flake closed

- **The flake was NOT the reservebytes tranche.** Mission attribution said
  425857347; the seeded-probe bisect (fixed-seed randomblob content →
  deterministic repro) put the window at afcbc3397 (T33-idxfix,
  value-ordered index b-trees): full-payload index dividers made interior
  cells ~10-100x fatter, which both enabled the two defects below and made
  them reachable at scale. Harness vacuum6 4.0's deterministic
  `sum(length(b))=18018886` (+9886 = row 9886 counted twice) failed there
  too — same root cause, silent form. Always attribute a "flake" with a
  SEEDED repro before bisecting by commit: `rand.New(rand.NewSource(seed))`
  content in a loop beats whole-package reruns (13 package runs passed
  between the two catches; the probe caught failures at seeds 6 and 34
  within 40 seeds).
- **Defect 1 — non-atomic rightmost-path apply left a duplicated child
  pointer.** applyChildSplitsRightmost appended its divider cells one per
  iteration and checked room per cell; a chain whose FIRST cell fit but
  whose SECOND did not returned errInteriorFull with cell 0 already written
  and the rightmost pointer still unchanged → cell(leftChild=C) AND
  rightmost=C — the same subtree reachable twice. The invariant "a child is
  a cell's leftChild XOR the rightmost pointer" is what C's
  balance_nonroot never violates: it gathers pending cells and
  redistributes in one pass, never mutating until the fit is known. Fix:
  exact aggregate precheck (Σ dividerCellLen + pointer slots) before any
  mutation — the atomicity the re-key path already had (childSplitsHaveRoom)
  but the rightmost path never got. Symptom cascade worth remembering:
  the duplicated pointer made the parent's apply take the CELL path
  (findChildCellIndex found the dup cell) whose carrier arithmetic then
  never fit, the retry loop drained the left page 7→5→3→1 cells, and the
  <3 guard fired "has no cells to split" — the error text pointed at the
  GUARD, not at the corruption three levels earlier. Trace the INVARIANT
  violation (dump cell.leftChild == rightmost), not the error site.
- **Defect 2 — whole-chain apply starved the retry loop.** Applying a
  child's full separator chain per attempt needs Σ dividers + N carrier
  cells in ONE page; with fat index dividers that exceeds any page
  (1300 bytes of need on 1024-byte pages), so the tail-split retry loop
  drained the left page without ever fitting the chain. C inserts each
  split's divider with its OWN insertCell and balances around that single
  pending cell — the space need per balance is bounded by two cells. Fix:
  applyChildSplitChain applies the chain divider-by-divider (each divider's
  anchor is the previous divider's new sibling page), reusing the existing
  firstfit+tail-split retry per divider. The <3 guard is now unreachable
  for page sizes up to ~13KiB (2 max cells + 2 max divider cells always
  fit); it remains as the safety net for pathological huge-page shapes.
- **Reuse the fixture-gap lens for "new" failures:** TestSegviewOracleX6
  InteriorNodes / TestWriterConformance (internal/fts) and harness vacuum6
  1.2/3.0 ("table t1 already exists" = missing reset_db in the JSON
  conversion) all fail at pre-fix HEAD too — pre-existing, out of tranche.
  And `go test .` (root) without `-timeout 3600s` dies at the 600s default
  on TestSQLiteSuite — expected, not a hang.

## T34r-split-resume (2026-09-27) — resume validation of commit 18037545d

- **Resume race:** the dying predecessor's process committed AND pushed
  18037545d at 01:34:40, minutes after the resume snapshot was taken — the
  "zero commits" resume note was already stale on arrival. Adjudicate a
  resume by re-reading `git status`/`git log` at session start AND right
  before reverting anything: a clean tree + a matching `git show --stat`
  means the WIP is already shipped. (Backup-before-revert to /tmp saved
  the review baseline here.)
- **"Revert to HEAD" checks must target the BASE commit, not HEAD.** The
  first control run ("does seed 6 fail without the fix?") used
  `git checkout -- <files>` on an already-clean tree — a no-op that
  re-tested the fix against itself. The true control is
  `git checkout <base> -- <files>` (860543fc2 here): seed 6 → FAIL at
  i=9403 "interior page 1848 has no cells to split", seed 34 → FAIL at
  i=9364 "interior page 349", both PASS with the fix. Restore via
  `git checkout <fix-commit> -- <files>` afterwards.
- **Root-cause attribution of the vacuum6 flake confirmed operational, not
  re-bisected:** failure reproduces at base 860543fc2 (post-reservebytes,
  post-afcbc3397) and the atomic-precheck + divider-by-divider apply fixes
  it there; the predecessor's seeded-probe window (afcbc3397 fat index
  dividers enabling both defects) is consistent with the observed
  geometry (fat full-payload dividers are what make the whole-chain apply
  unfittable on 1KiB pages).
- **Engine randomblob is global math/rand (unseeded, fnRANDOMBLOB)** —
  testgen vacuum6 4.0 is content-nondeterministic across runs; seeded
  hex-literal probes are the only deterministic repro. A single green
  package run proves nothing about this flake; only the seeded probes do.

## Fleet perf-p1 — UPDATE uniqueness change-detection gate (2026-09-28)

- **UPDATE conflict scans were gated on "table HAS constraints", not "constrained
  value changed"**: checkUpdateConflicts ran a full table-btree walk per updated
  row whenever the table had any UNIQUE/PK column or unique index, even when the
  SET clause touched none of them (5.7ms per single-row UPDATE @20k rows). The
  gate (updateConstraintUnchanged, internal/execdml/update_constrained.go)
  compares a change's NEW vs OLD values on every constrained slot using the same
  comparators the scan uses (uniqueColValuesMatch; indexKeyValue+CompareValues
  for index defs; partial-index membership via evalIndexWhere) — skip iff all
  agree. Correctness: old values already coexisted with every other row and with
  every earlier change's old values, so an unchanged constrained value cannot
  conflict. Gate applied ONLY on the no-trigger paths (checkUpdateConflicts,
  runUpdateFail, perRowConflictError): BEFORE triggers can insert conflicting
  rows mid-statement, which breaks the "old values were valid" assumption on the
  trigger/OR IGNORE paths.
- **WITHOUT ROWID table-level PKs were invisible to UPDATE uniqueness checks**:
  frigolite creates no sqlite_autoindex schema row for WR tables (the table
  btree IS the PK index), so uniqueIndexColumns returned nothing and
  UPDATE...SET <pk-col> onto an existing key wrote DUPLICATE PKs (INSERT caught
  it via WRPKIndices, UPDATE didn't). Fix: updateConstrainedDefs synthesizes a
  uniqueIndexDef from WRPKIndices for WR tables not already covered — error text
  via uniqueIndexColsConflictError matches the sqlite3 oracle byte-for-byte
  ("UNIQUE constraint failed: t3.a, t3.b"). Oracle-verified.
- **The second O(N) in the same workload is the APPLY path, not the check**:
  after the gate, applyUpdateChanges still swept the whole table with
  DeleteCellsWhere even when every change had been written by the in-place
  same-size fast path (predicate excludes all in-place rowids → provably no-op).
  Skip the sweep when len(inPlace)==len(toUpdate). Combined: 5.72ms→79µs and
  14.15ms→159µs per op (>=70x).
- **btree index "seeks" are exhaustive leaf walks today**: IndexKeyRowIDs and
  SeekIndexKey (btree_indexseek.go) walk every index leaf in stored order
  because stored order is byte order, not value order — a "probe" through them
  is O(index), not O(log N). Do not swap a table scan for one expecting an
  asymptotic win; the seam for a true sqlite3BtreeIndexMoveto is the
  value-ordered-storage tranche. Cursor.SeekToRowID IS a real binary descent —
  rowIDExists/rowExists now use it (was a from-start walk; re-key-heavy UPDATEs
  were quadratic).
- **UPDATE-path unique comparisons apply neither affinity nor collation**
  (uniqueColsMatch/indexDefsMatch use raw util.CompareValues), so a TEXT COLLATE
  NOCASE UNIQUE column accepts SET s='ABC' when 'abc' exists (oracle: raises
  "UNIQUE constraint failed: tn.s"). Pre-existing gap, deliberately preserved
  by the gate (task contract: "as strict as today"); the INSERT path DOES apply
  affinity+collation (rowMatchesIndexKey). Follow-up candidate: thread column
  collation/affinity through the UPDATE conflict comparators.
- **rowid tables allow rowid 0 on explicit SET rowid=rowid-1** (scan order
  shifts 1..N down to 0..N-1, vacated slots free) — oracle-verified; do not
  "fix" rowid 0 as a conflict.

## fleet/perf-p24-rowid-seek — P2 IPK-alias equality seek + P4 rowid range seek (2026-09-28)

- **Seek paths need the b-tree path stack**: `SeekToRowID` descended without
  pushing `{pageNum, childIdx}` entries, so the FIRST `Next()` after seeking
  into a non-leftmost leaf continued from the stale leftmost-descent path and
  REPLAYED rows (BETWEEN 10000..10010 returned 13 rows: 10000,10001,10000,10001,...).
  Fix: SeekToRowID now clears `c.path` and routes through
  `seekTableLeafWithPath` (the same path-aware walk restoreIfNeeded already
  used; btree.c sqlite3BtreeTableMoveto keeps the cursor valid). The equality
  seek never noticed because it never calls Next().
- **INTEGER PRIMARY KEY alias is stored as NULL**: the record holds NULL for
  the IPK column and the scan substitutes the rowid (affinityPlan.apply +
  fillStructRowRemainingFromTypes). Any new row-source must replicate:
  affinity wrap loop must SKIP nils (wrapValueForRowMap(nil) produces a
  wrapper-around-nil that blocks the substitution), then fill nils with
  wrapAffinityCollated(colDef, rowid). Seek paths previously leaked the NULL
  into outputs (`SELECT * FROM t WHERE rowid=5` → id=NULL) and dropped rows
  whose WHERE re-check referenced the alias.
- **Bound-vs-eval consistency rule for range seeks**: the seek bounds must be
  SUPERSETS of what rowPassesWhere accepts — bounds wider than the engine's
  affinity conversion silently change results (planned lo=5001 + eval-reject
  = 0 rows while scan gives 15000). Frigolite's rowid-vs-text eval does NOT
  trim whitespace (pre-existing: `+rowid>' 5000 '` = 0 vs oracle 15000 on the
  SCAN path too), so text bounds parse with ParseFloat(text) — no TrimSpace —
  and non-numeric text/blob bounds classify as lower→never / upper→always
  (INTEGER < TEXT/BLOB always). Oracle-verified on 3.54.0: ranges render
  "(rowid>? AND rowid<?)" regardless of >=/<= spellings, lower bound first,
  eq dominates ranges, alias forms render the alias as display and "rowid" as
  the constraint name, `+rowid>5` (unary + on the COLUMN) scans.
- **reverse_unordered_selects applies to the rowid range walk too**
  (where.c WHERE_REVERSE): whereA-2.2 fails unless the range path mirrors
  select_scan's shouldReverse (ReverseUnordered && no ORDER BY &&
  selectDepth==1) by reversing the collected rows.
- **Lazy two-phase decode keeps the range loop at scan cost**: reuse
  scanLazyDecodeIndices + parseRecordSerialTypes +
  DecodeRecordValuesFromTypes (phase 1 = WHERE-referenced cols, phase 2
  refill after the WHERE passes, re-applying defaults + IPK fill); a full
  DecodeRecord per row made count(*) over a wide range 2x slower than scan.
## PERF.P6 cursor registry (2026-09-29, fleet/perf-p6-cursor-lifecycle)

- **btree cursor registry was a strong-ref leak with an unreachable finalizer.**
  `cursorRegistry[pager,rootPage][]*Cursor` kept every cursor reachable, so the
  `SetFinalizer` "cleanup" could never run — the registry only grew. Every
  mutation's `saveAllCursors` then walked + re-saved every cursor ever opened
  on that tree: O(n^2) insert growth, 94% of per-INSERT allocations at 50k
  rows (254KB/op at 60k). If a registry holds strong refs, a finalizer on the
  referenced objects is dead code — cleanup must be deterministic.
- **Fix = SQLite's own lifecycle, mirrored at Engine.Exec.** btree.c closes a
  statement's cursors when its VDBE halts (closeCursorsInFrame), so
  saveAllCursors only sees live cursors. frigolite equivalent: `BTree.Close()`
  unregisters+releases the wrapper's cursors (Cursor keeps `tx` as owner
  back-pointer; Close is idempotent and nil-safe); the
  tableBTree/tableBTreeForName/tableBTreePg funnels register wrappers in
  `Engine.stmtBtrees`, and Engine.Exec closes everything above its entry mark
  on return. Nested Exec frames (triggers, eval()) mark their own segment, so
  an inner statement never releases the enclosing statement's positioned scan
  cursors — the misc8-1.6 contract survives via ownership, and the registry
  stays as the cross-wrapper save/restore mechanism.
- **Per-row tree creation needs per-row release, not statement release.**
  Statement-scope alone still went quadratic WITHIN one multi-row statement
  (single INSERT...SELECT: 17→30→42 us/op at 20k→60k) because each row's
  dmlTableBTree/writeTableRow created a wrapper. Where a tree's use is
  provably function-local (verified per site), `defer tree.Close()` releases
  per row: after both layers the same probe is flat 4.66→4.00 us/op (~9x).
- **Harness verdict hygiene:** `go test` caching hides failures without
  `-count=1`; Go's default 10-min package timeout masquerades as "0 failing
  subtests" (a timeout panic prints no `--- FAIL` lines); the JSON harness
  runs files `t.Parallel()` and cascades within a file after a first failure
  ("table t1 already exists" / "no such table: t1" are cascade noise, find the
  file's FIRST failing case). Base b81c575d8 fails the same case sets
  (triggerB/joinH/trigger2/tkt2820/tkt3334/8_3_names identical base vs head)
  and `FRIGOLITE_TEST=<file>` pattern runs fail at base too — compare failing
  SETS, never single verdicts.

## PERF.P7-scan — simple-aggregate feed + range-loop buffer reuse (fleet/perf7-scan, 2026-09-29)

- **The aggregate row feed, not the loop, was the scan-throughput disease.**
  `SelectNeedsRowMaps` returns true for any aggregate query, so the
  range-seek/scan loops materialized a RowMap (~250B: hmap+bucket+cloned
  wrappers) plus a full output row for EVERY input row only to feed
  `agg.Step(evalAggCallArgs(...))` — and evalAggregates discards the rows.
  The fix (OP_AggStep parity): compile a statement-LOCAL `simpleAggFeed`
  (bare COUNT/SUM/AVG/TOTAL over a plain column ref or COUNT(*)) and step it
  from the loop's phase-1 decoded values; no rows, no maps, no refill, no
  per-row output rows. 15.4→2.6 allocs/row, 639→94B/row, range scan 2.5M→
  10.5M rows/s; plain-scan aggregates 1.6M→12M (7-8x).
- **The feed must be a LOCAL, not engine state.** First version parked the
  feed on SelectEngine (saved/restored like outerRows); a WHERE subquery
  executing another SELECT (e.g. `id BETWEEN 5 AND (SELECT MAX(id)...)`)
  ran its own execRealTableSelect, whose postscan CONSUMED the outer
  statement's feed (finishSimpleAggFeed is unconditional) — the outer query
  then returned empty-input values. Fix: pass the feed explicitly through
  selectRowidSeekRows/selectRowidRangeRows/TableScanner.ScanTable and finish
  it in execRealTableSelect; nested statements can neither see nor consume it.
- **The scan's IPK alias fill hides inside the affinity plan.**
  `fillStructRowFromTypes` performs the INTEGER PRIMARY KEY stored-NULL →
  rowid substitution via `affinityPlan.apply` — restricting the plan to
  WHERE-referenced columns (feed-mode raw-value optimization) silently
  dropped the alias fill on the no-WHERE (full-decode) path:
  `SELECT SUM(id) FROM t` stepped NULL. Feed-mode wrap columns must UNION
  the IPK alias columns (a VALUE fill, not a comparison wrapper). The
  range/eq seek paths are immune (fillSeekRowPhaseOne owns an independent
  ipkIdx loop). Caught by TestP1InsertOrIgnore, not by my own sweep —
  extend sweeps with no-WHERE IPK-aggregate shapes.
- **COUNT(*) parses as a ONE-ARG call** whose argument is the star
  ColumnRef (evaluated to the non-nil "*" marker); a feed keyed on
  "zero args = COUNT(*)" never fires. Treat `len(Args)==1 && arg is
  ColumnRef{Name:"*"}` as countStar.
- **WHERE fast paths: BETWEEN and AND chains mirror fastEvalComparison's
  discipline** (fastEvalBetween = operand >= low AND operand <= high via
  compareColumnToLiteral; fastEvalAndChain requires EVERY leaf to take a
  fast path, else the whole tree evaluates generically). Fast leaves are
  non-NULL definitive booleans, so 3-valued AND is exact. NumericLit
  caches are populated on first generic eval, so literal reads hit from
  row 2 of the same statement.
- **reverse_unordered_selects must disable the feed**: a compensated
  (Kahan-Babuška-Neumaier) float sum is order-sensitive in the last ulp,
  and the generic path feeds reversed rows. Same reasoning excluded
  index-scan-order reorders and GROUP BY's covering-index reorder.
- **Fresh-worktree fixture triage (recurring)**: `internal/fts`
  (ftsconformance), `internal/pager` (walconformance), `internal/recover`,
  and the `tools/orafixture`-dependent fixture-reference tests all fail on
  a fresh worktree with missing GITIGNORED fixtures; `rsync -a` (NOT
  --ignore-existing — stale 0-byte locals block it) from the main checkout
  fixes them. TestP8IncrVacuum3OracleSequence flakes ~1/5 runs isolated on
  BASE at the same rate as the branch (pre-existing state dependence).
- **The 1002-file JSON harness is red at base with a 382-file failing
  set**; regression triage = diff the failing FILE SET branch-vs-base
  (`grep -E '^    --- FAIL: TestSQLiteSuite/'`), not verdicts.

## PERF.P7-pipeline — per-statement prepare pipeline (2026-09-29)

- **The template cache never served SELECT/UPDATE/DELETE.** `cloneStmtsWithValues`
  substituted INSERT VALUES tuples only and errored "template cache: unused
  values" for any other shape, so every point SELECT with a varying literal
  re-ran the full lex+parse pipeline (~40% of its per-statement cost). The
  fix pattern generalizes: a copy-on-write cloner that shares every node
  except the statement root and the literal ancestor chain, refuses the
  clone (falling back to a full parse) on ANY doubt — unknown node kind,
  non-canonical literal text ("0x1F" normalizes to value 0), value/count
  mismatch — so a substituted AST is provably identical to a fresh parse.
- **The valIdx count check was the only thing keeping the old INSERT-only
  template cache correct.** normalizeSQL treated digits inside identifiers
  ("t1") as literals, merging unrelated statements into one normalized key
  with a phantom value; the count mismatch then forced a full parse (correct
  results by accident, zero cache benefit). normalizeSQL now classifies
  identifier/parameter continuations (digit after letter or after $ : @ #)
  via a 256-byte table; literal-free input returns unchanged (no copy).
- **Substitution must walk fields in SOURCE order** (WITH first, then select
  list, FROM, WHERE, ... ). Normalized values are consumed in text order; a
  field-order walk that visits the select list before the WITH body silently
  swaps values between matching-shaped statements (json501 caught it — the
  value COUNT still matched, so nothing bailed). Any new walker must mirror
  the grammar's textual order.
- **NewParser allocated a fresh ~3.2KB parser stack per statement (25.7% of
  INSERT-phase alloc_space) and GetParseTables rebuilt its wrapper struct per
  call.** A sync.Pool + Parser.reset() (zeroing stack slots so pooled parsers
  retain no AST garbage) and one init-built table set removed both. Pool +
  reset is the reusable pattern for per-statement engine objects.
- **Per-keyword strings.ToUpper ran twice per keyword token** (lexer keyword
  classification + parse.tokenCode). `util.LookupUpperASCII` uppercases into
  a stack buffer and keys the map with `m[string(buf)]` (compiler elides the
  allocation); non-ASCII words keep Unicode ToUpper semantics.
- **Derived-cache invalidation by schema fingerprint beats invalidation
  hooks.** `schema.Manager.SchemaFingerprint()` (header cookie folded with a
  mutation epoch; InvalidateCache bumps the epoch too) lets execdml cache
  index-maintenance def lists per (database, table) with airtight DDL
  sensitivity and zero call-site hooks. Content-keyed single-entry memos
  ((table, CREATE SQL text) → parse result) are self-validating the same way
  — no hooks needed, and they kept the hot path off a string-concat cache key.
- **ASTs are de-facto immutable during execution** (the exact-text stmtCache
  always re-executed one parsed AST; execquery/execdml mutators clone first).
  Verified by grepping for writes into statement/expr fields before shipping
  COW sharing — do this check again before sharing any NEW node type.
- **Probe hygiene:** sibling fleet agents share /tmp — a sibling overwrote
  /tmp/perf7probe mid-run. Use task-unique probe directories. Interleave
  baseline/branch binaries in one session for honest µs numbers (run-to-run
  machine noise was ±30%); allocs/stmt and B/stmt from MemStats are stable
  and profile-guided.
- **Harness flake:** TestSQLiteSuite/crashM can fail with "attempt to write
  a readonly database" when a leftover test2.db from an overlapping suite
  run sits in the package dir; passes 3/3 in isolation after cleanup.

## PERF.GC-rowmap — execquery row-output alloc cuts (fleet/perf-gc-rowmap, 2026-09-29)

- **A per-row map build cannot beat Go's swiss-map small-map floor.**
  StructRowToMap's ~350B/row (hmap + one group ≤8 entries) is irreducible
  while the aggregate/JOIN consumers take []RowMap; the win is everything
  AROUND the map: StructRowToMap can SHARE the row's freshly decoded values
  and freshly built affinity wrappers (fillStructRow* REPLACES slot contents,
  never mutates in place, and no code writes ColumnValue fields) — deep-copy
  only []byte payloads, through the CollatedValue/ColumnValue chain.
- **`make(map, N)` with N > 8 is a per-row PESSIMIZATION.** buildRowMap
  pre-sized len(colDefs)+4 (9 for a 5-col table): the swiss-map small
  representation holds one group only up to 8 entries; a hint above 8 forces
  the full-table structure EVERY row (+30% update-scan bytes). Pre-size
  len(colDefs)+1 and stay ≤8.
- **Skipping the per-row output-row build for aggregate statements needs TWO
  guard fixes, not one.** scanConsumedByAggPass (GROUP BY / aggregates /
  correlated-agg / window — a strict subset of execSelectPostScan's dispatch)
  leaves rows empty with maps populated; then (a) sortScanRowsIndexOrder's
  old `len(rows)<2 || len(maps)!=len(rows)` guard silently DECLINED, losing
  index-key order for order-sensitive aggregates (group_concat), and (b)
  permuteScanResults indexed rows[from] out of range. Rowsless map
  permutations are a first-class state: sort maps alone, permute rows only
  when len matches.
- **Scratch buffers shared across eval boundaries need a DEPTH-INDEXED pool,
  not one buffer.** evalAggCallArgs / computeGroupByKeyValues can re-enter
  through subqueries or eval() UDFs inside arguments; slot = nesting depth,
  grow on demand, and CLONE on retention (partitionByGroupKey keeps the key
  values for new groups). A single buffer corrupts the outer level's
  args[i] after the nested call returns.
- **The remaining UPDATE/DELETE alloc mass sits outside execquery**: the DML
  scan calls storage.DecodeRecord (~26%) + btree cell decode (~17%) +
  pager statement-journal copies (~15%) per visited row, and the retained
  RowMap + per-column affinity wrappers are the DML contract. In-scope
  ceiling measured: −2.4% B/stmt on update-scan; the 25% loop target needs
  the storage/pager/btree layers (or an execdml interface change), both out
  of the tranche's scope.

## PERF.GC2-decode (fleet/perf-gc2-decode, 2026-09-29) — update/delete decode-path alloc cuts

- **Statement-journal before-image pooling is safe only with the entry as the
  sole owner.** Get at capture (copyPageBytesLocked), Put only where the entry
  is DROPPED (EndStatement: parent already has the pgno / no parent / parent
  done; entry splice = ownership move, never Put), and rollback ADOPTS the
  captured buffer as the restored page's Data (ownership transfer, no copy);
  the evicted page object's bytes are never Pooled (ambiguous lifetime —
  ReadPage handles may outlive the scope). The once-only close (done flag)
  makes double-Put impossible. Do NOT pool *StmtJournal objects or their
  entries maps: the documented double-restore contract (a DML-level rollback
  followed by the engine-level rollback of the same scope) relies on object
  identity surviving both closes — a recycled scope object would let the late
  close kill an unrelated newer scope. Pinned by
  TestStmtJournalPool{RollbackExactness,NestedSpliceKeepsOldest,
  BufferNotShared} (note: uniform-byte pattern checks must use pages ≥2 —
  WritePage refreshes DB header fields inside page 1's bytes).
- **MemProfileRate=1 delta-heap profiling LIES on huge processes**: the
  runtime profile bucket set churns and cumulative totals DECREASE between
  dumps (observed 17.9M → 15.4M "cumulative" samples). MemStats Mallocs/TotalAlloc
  deltas per op are the deterministic ground truth; use default-rate pprof
  deltas only for site attribution, never for totals.
- **Cursor leaf-cache at descent is safe because save/restore clears it**:
  saveCursorPosition and restoreIfNeeded both clearPageCache, so caching the
  landing leaf (descendToFirstLeaf / descendToFirstLeafFromCurrent) cannot go
  stale across nested-statement writes. Escape analysis keeps the interior
  ParsePageInto scratch on the stack — verify with pprof -list that the
  intended lines show zero allocs.
- **DeleteCellByRowID's parent hint**: the seek's path stack names the leaf's
  parent; a verified hint (findLeafIndexInParent must confirm the leaf is
  still reachable from it NOW) spares the O(database) findParentByWalk per
  emptied leaf. To stay byte-compatible with the walk on crafted images, the
  hint path must replicate the walk's refusals: any-tree-root leaves and
  collectSchemaRoots failures route back to the walk (which answers
  "page is a root" / not-found → no rebalance).
- **The UPDATE grow-text shape (SET d = d || x, delete+reinsert per row) is
  O(N²) from execdml's full-table DeleteCellsWhere sweep per changed row** —
  658k allocs/stmt at base on a 100k table; the btree decode cuts dropped it
  7x but the sweep itself is execdml scope. In-place (same-size) updates skip
  DecodeRecord almost entirely — the mission profile's "DecodeRecord 26%"
  mass lives in the delete+reinsert shape, not the in-place one.
- **Registry empty-list retention**: unregisterTreeCursor keeping the emptied
  slice in cursorRegistry (spare capacity) kills the per-statement regrow;
  saveAllCursors treats empty lists as no-op. Static finalizer needs the key
  ON the cursor (c.regKey) — a capturing closure allocated per OpenCursor.

## PERF.PARITY-api — root public-API layer per-call overhead (2026-09-29)

- **The root layer's own per-call cost is tiny against the engine; profile it
  isolated before optimizing.** pprof `-show_from`/`-peek` over a
  /tmp replace-module probe (not `go test -bench` inside the repo) showed the
  root package contributes 2-3 allocs and ~300ns per Exec/Query call vs the
  engine's 80-130 allocs / 7-9µs. The two real root costs per call: (1)
  splitSQLStatements re-tokenized the whole SQL text on EVERY Exec/Query just
  to produce trace-hook texts; (2) Query append-copied the engine's row
  headers into a fresh backing array even for single statements.
- **Zero-semicolon fast path is byte-exact**: `strings.IndexByte(sql, ';') < 0`
  implies splitSQLStatements returns exactly `[sql]` (its EOF branch appends
  the untrimmed tail verbatim; the semicolon branch TrimLefts), so a nil
  texts slice + "use sql for statement 0" in stmtTextAt is trace-text
  identical. Any ';' byte anywhere (string/blob literal) falls back to the
  full tokenizer. ~260ns + 1 alloc/call saved.
- **Handing exec.Result.Rows to the caller verbatim is contract-safe**:
  execquery allocates Rows fresh per call (`make([][]interface{}, n)`) and
  never retains them; the Exec path ALREADY passed er.Rows through via
  execResult. Preserve the nil/empty distinction: `append(nil, empty...)`
  stays nil, so the single-statement branch must map len==0 → nil
  (execquery returns non-nil empties from `make(..., 0)`).
- **gocognit gate bites small changes**: folding two if-chains into
  (*DB).Query pushed it to 19 (limit 15) — extract a helper
  (foldQueryResult) rather than argue the gate.
- **Fleet-agent test hygiene**: root-suite tests use FIXED /tmp paths
  (e.g. /tmp/fts4merge_pin.db) and the harness flakily fails random subtests
  ("table t1 already exists") when sibling agents run suites concurrently —
  pre-existing on pristine origin/main, failing at DIFFERENT subtests per
  run. Validate failure SETS against a baseline worktree run, not greenness;
  re-run flakes serially on a quiet machine before believing them.
- **Benchmark measurement honesty**: fixed-iteration (-benchtime Nx) ns/op at
  200k iters is GC-dominated (~9µs for a 286ns MemStats-loop op); report
  MemStats mallocs/bytes per call + 0.5s-benchtime medians (count>=6,
  alternating A/B) and expect ±5% noise on 7µs ops. pprof
  `-sample_index=alloc_objects` with root-frame grep gives the cleanest
  root-layer alloc delta (405,713 → 107,677 over 200k calls, -73%).
## PERF.PARITY-expr — expression-evaluator fast paths (2026-10-01)

- **Fast paths must re-verify EVERY operand-class assumption at their own
  call site, not inherit the generic path's invariants.** The arithmetic
  fast path initially accepted "at least one float64 operand" and discarded
  `toFloat`'s ok flag — but it runs BEFORE `evalArithmeticOp`'s unwrap, so a
  ColumnValue-wrapped operand reached the arithmetic as 0 (`toFloat` of a
  wrapper is (0,false)): a REAL column op REAL literal returned the literal,
  and whereL's `c2/0.1` filter silently dropped its row. The generic path's
  "operands are unwrapped" invariant holds only AFTER `evalArithmeticOp`
  unwraps. Fix: assert BOTH sides bare (int64/float64 2x2 ladder), fall back
  otherwise. Testgen whereL caught it; a hand-rolled parity sweep had missed
  the (column op float-literal) class — sweep shapes must cover each operand
  wrapper-ness COMBINation per operator, not just each operator.
- **Dispatch fast paths entered from shared dispatchers must re-gate the
  operator.** `dispatchComparisonValues` is entered for every value-level
  operator; its generic switch is what routes LIKE/||/MATCH onward. An
  unfast-gated comparison fast path answered `x LIKE 'p%'` via its `>=`
  default arm ('XYZ' LIKE 'A%' → 1). Any default arm inside a fast helper is
  a lie unless the caller's routing is provably exclusive.
- **Fleet shared-infrastructure flakes**: `frigolite_fts4merge_pin_test.go`
  and several conformance tests use FIXED paths (`/tmp/fts4merge_pin.db`,
  gitignored `tools/orafixture`, `*-backup.db` fixtures) — concurrent fleet
  agents running the full suite collide ("attempt to write a readonly
  database") or hit missing fixtures. schema5/8_3_names harness failures
  ("no such table") reproduce on a pristine origin/main worktree (converter
  lost the CREATE setup) — always A/B against a baseline worktree before
  treating a suite failure as a regression. Wall-clock benchmarks are
  meaningless under 10+ sibling go-test processes: measure CPU time per op
  (getrusage RUSAGE_SELF) and take minima of interleaved A/B rounds; pprof
  shares are ±2-3% noisy run-to-run.
- **Group-phase CPU decomposition** (`SELECT k, COUNT(*), SUM(c) FROM g
  GROUP BY k`): execexpr's ~45% share of the group phase is ~90% row-map
  column lookup (evalColumnRef → RowMap.Get string hashing) — the row
  abstraction itself, not expression logic. Arithmetic/comparison fast paths
  cannot fire there; only a node-level column-resolution cache (needs a
  cross-package hook into execquery's StructRow schema identity) or keeping
  rows position-based end-to-end can move it further.

## PERF.STRUCT-dml — point-DML fast paths + single-cell leaf delete (fleet/perf-struct-dml, 2026-10-01)

- **Point-DML wall time on :memory: is parse + exec statement scope + pager
  header churn bound, not DML-pipeline bound.** After the point fast paths, a
  1-row point UPDATE costs 91 allocs vs SELECT 1's 15; the delta is
  dmlTableBTree (mission-blessed per-statement wrapper), BuildRowMap,
  EncodeCell, the btree write, and pager stmtReadTouch/BeginStatement/header
  Encode (~17%+14%+10% of the phase). The exec stmt cache makes REPEATED
  identical SQL parse-free — a probe printing varying literals per statement
  is the parse-realistic instrument (identical-SQL probes understate parse by
  ~40% and make DML cuts look negligible).
- **The suite-level failing-case COUNT is worthless as a regression signal;
  so is the per-file failing-case count.** The reliable instrument stays the
  per-file isolation sweep's FILE SET (same 409 failing files base and head),
  and even within a file the failing-case SET wobbles run-to-run on UNMODIFIED
  main (alter ±5, in ±4, e_delete flips 4↔7 identically on both trees).
  Compare sets across trees, rerun any count delta twice on both trees before
  believing it.
- **A single-cell leaf delete can byte-match the decode/re-encode compaction
  by staging survivors' RAW page spans through an arena in pointer order** —
  finishLeafDelete's layout (pack downward from usableSize, pointer array in
  that order, no fragmented bytes) is reproducible without decoding any
  survivor; TableLeafCellSizeAt gives each survivor's footprint. Decline to
  the generic predicate delete on EVERY anomaly (parse failure, duplicate
  rowid on the leaf — the cursor seek lands on the first match, so one next-
  pointer check covers it, overflow-free error) so corrupt-image behavior is
  bit-identical.
- **The 1-candidate point UPDATE was never allocation-fat** — the ≤2-candidate
  map path already kept it small; its real costs were the grow shape's
  O(table) DeleteCellsWhere sweep (99.4% of the phase — 1.7ms → 13.6µs when
  replaced by seek delete + re-insert) and the fixed exec/pager/parse scope.
  Cut lines for the point fast paths: single rowid-equality WHERE (skip WHERE
  eval — the seek's hit matches by construction), plain OR-less/triggerless/
  FK-off/generated-free target, SET against the collected row map (the
  pre-positional small-candidate evaluation, cheapest at n=1); the
  preCheckUpdate/checkUpdateConflicts gates run unchanged on the single
  change.

## PERF.TYPEDROW — page-batch scan + typed-row decode floor (fleet/perf-typedrow-scan, 2026-10-01)

- **The scan floor was ALLOCATION, not cursor machinery.** The premise (per-row
  cursor Next/cachePage/restore dominates) measured wrong: driving the batch
  walker alone moved bare scan ~3% (within noise) — the cursor's per-row cost
  is a handful of branches (cache hit + restore check). The real mass was 4
  allocs/row of which 3 were structural waste: an unread column's slot walk
  and the INTEGER PRIMARY KEY rowid-alias fill wrapper (capDirectCols's
  `len(cols)+2 > activeColCount` guard kept 1-of-2-column tables on the full
  decode), a fresh one-row output slice (nonStarRows make), and the feed's
  per-step [1]interface{} escape (Scratch through an interface method call
  always heaps). Alloc profile attribution at baseline: wrapPrecomputed 33%,
  decodeValue boxing ~21%, appendRowOutput 16.5%, feed step 16%.
- **A wrapper that is only ever unwrapped is identity — fold it.** The bare
  output peel (unwrapCollatedValue∘UnwrapColumnValue) strips ANY wrapper stack
  to the raw value, and the IPK fill peels to the rowid itself, so the pure
  bare scan (no WHERE/feed, output slots == direct slots in order) can decode
  straight into the flat result buffer's next window: no reuseSRow round
  trip, no wrap, no peel. Gates must include the flat output branch's own
  shape (no joins, no agg consumers) and slots EQUALITY (SELECT c,k keeps the
  historical path — the fold needs output order == decode order).
- **The prefix header walk is only legal when absence is detected by the
  CALLER.** DecodeRecordColumns walks all headers for the exact count (ALTER
  TABLE defaults key on it). The prefix variant returns
  min(count, max(cols)+1): a count below that ceiling means a requested slot
  is absent → re-read exact (defaults correct); at the ceiling every
  requested slot is present and slots beyond it are unread (the direct
  contract) so skipping defaults is invisible. The ceiling is
  max(cols)+1 — NOT len(cols): generated columns leave gaps
  (SELECT a,c over a,b GENERATED → cols=[0,2]). Empty slot sets (SELECT
  COUNT(*)) keep the exact walk. Trip hazard: `len(cols)-1` on a non-nil
  EMPTY slice panics — feedDecodeSlots deliberately returns non-nil empty.
- **Batch-scan save/restore fidelity: sync the cursor position per CELL, and
  route EVERY stop path through the save check.** saveAllCursors captures
  (page, cellIdx); Cell(i) must set c.cellIdx=i BEFORE decoding so a nested
  write (eval() UDF in the select list) saves the cell actually being
  consumed, and must check state BEFORE reading bytes the write may have
  defragmented (ErrScanSaved → consumer declines to the cursor loop). The
  resume is ONE Next() (restore re-seeks, skipNext returns the next-larger
  entry) then the cursor loop — the exact read/Next window of the pure
  cursor path. The t33misc shape caught it: a save discovered through the
  consumer's stop returned through the "fn stop" path as a clean walk end,
  the resume skipped the step-off Next, and the saved row was re-read
  (row 1 emitted twice). Every stop path must re-check state.
- **Full-suite failure-set regression proof needs the fixture dirs copied
  into the fresh worktree (testdata/backupconformance, testdata/walconformance)
  and a full pristine-BASE run, not just per-test sampling**: the base full
  run failed TestWindowCGroupConcatBlobUTF16 identically to head (shared-CWD
  fixture race: one test's os.Remove+Open of a .db races the parallel
  suite), which isolation runs never show (5/5 PASS). head ⊂ base failure
  sets is the acceptance criterion.
- **Scan floor after this tranche** (100k-row (k IPK, c) in-memory, 3
  interleaved rounds): bare SELECT c 11.9→6.2ms (1.91x, 16.1M rows/s; GOGC=off
  23.9M), SUM(c),COUNT(*) 8.3→5.0ms (1.65x, 19.8M), wide-table variants
  1.87-1.94x; allocs/row 4→1.01 (the remaining alloc IS the boxed value).
  Star scans and GROUP BY shapes are deliberately untouched (3.0/6.0
  allocs/row unchanged). Remaining known mass: interface{} boxing of decoded
  values (value model), the [][]interface{} result materialization (API),
  and the aggregate/GROUP BY passes (sibling scope).

## PERF.FLOOR — per-statement exec tax (fleet/perf-floor, 2026-10-02)

- **The per-statement floor is allocation, and the runtime noise it buys is
  ~60% of CPU.** Point-SELECT CPU profile: kevent 24% + usleep 13% + madvise
  12% + cond_wait 10% = scheduler/GC churn; engine work (runSQLText cum) is
  ~25%. Killing alloc frames moves throughput superlinearly through GC.
- **A stale pre-check rebuilt the DML memo's map per row**: the bare-unique
  gate (uniqueColIndicesWithPK) called buildColumnIndex although its caller
  already held the memoized colIndex. The memo existed — the last unreached
  call site didn't use it. Insert floor −9% allocs from a two-line change.
- **Storing template-cloned statements in the exact-text stmtCache is pure
  loss under unique-text streams**: the cache fills to cap and is
  wholesale-dropped; each store paid a map insert per statement and nothing
  ever hit. Clones now re-clone per exec (substituted AST ≡ fresh parse).
- **Template-cache keys don't need a per-statement string**: key on a seeded
  maphash of the normalization SCRATCH BYTES and verify the entry's stored
  text with `entry.template != string(bytes)` (compiles to memequal, no
  alloc). Collisions degrade to full parse, never a wrong template.
- **Transient clone pooling that is SAFE: per-execDepth retire/rotate.** One
  slot per execDepth on the engine; every statement struct a substitution
  clones is retired into the slot, and the NEXT substitution at that depth
  rotates retired→free. Within one substitution each struct is handed out at
  most once. Two traps found by tests: (1) borrow must happen BEFORE the
  field walks — walks recurse into nested selects/UNION members that borrow
  the same slot, and take-after-walk handed one struct to two levels (a
  self-referential Union chain the from-term counter walked forever);
  (2) one substitution builds SEVERAL coexisting clones (InsertStmt + CTE
  bodies + SELECT) — a single lastClone slot aliased the CTE body and the
  INSERT's SELECT ("circular reference: s"). Retire/rotate fixes both.
- **BTree WRAPPER pooling is structurally unsafe here — confirmed by
  experiment.** Close's idempotency contract (late second Close must no-op)
  breaks recycling: owner A closes → pool → owner B re-arms (closed=false)
  → A's late second Close now proceeds and frees B's wrapper mid-use. A
  GLOBAL pool makes it cross-engine; the engine's stmtBtrees funnel cannot
  express "this wrapper is dead" without ownership tokens. Wrappers stay
  unpooled (the file header was right); cursors remain pooled.
- **Pre-existing pooled-cursor race**: BTree.Close marked cursors released
  AFTER cursorPool.Put — the marker write raced the next acquirer's
  resetFor (4 DATA RACEs on pristine main with 4 concurrent connections).
  The marking now precedes the Put; resetFor still clears it on acquisition.
- **Fresh worktrees need the untracked fixtures**: testdata/{backup,hook,
  incrvacuum2,recover,stmtbind,wal}conformance and tools/orafixture are
  gitignored; their absence fails ~7 root tests that look like engine
  regressions. Copy them from the main checkout before judging failure sets.
- **TestRowidSeekRange fails on pristine main** (BETWEEN ' 10 ' AND 12 →
  3 rows, test wants 0) and **internal/fts TestSegviewOracleX6InteriorNodes/
  TestWriterConformance fail on pristine main** — pre-existing, not this
  branch.
- **PERF-scanagg tranche (fleet/perf-scanagg): the GROUP BY and range-scan
  gap closed by streaming the aggregates, not by micro-tuning the generic
  passes.** The generic GROUP BY path retained a positional StructRow clone
  per input row (cloneReuseSRow arena + header ≈ GBs over 100 queries),
  partitioned them under serialized string keys, and re-walked each group per
  output aggregate. The grouped feed (select_agg_groupfeed.go) moves the
  OP_AggStep/OP_AggFinal loop INTO the scan: per-group aggregator instances
  keyed by the group key, output rows built per GROUP at Final. select_group
  18→45 q/s (2.5x); select_scan 9.4M→21.5M rows/s (2.3x) via the batch leaf
  walk + covered-WHERE skip. Reuse beats re-tuning: the feed reuses
  collationGroupKey, resolveGroupKeyMiss's merge, aggFeedColumnArg,
  compileAggFeedCount, the registry Aggregators and the scan's phase-1 lazy
  decode — parity rides existing code.
- **Typed fast-path maps must engage ONLY on provable string-key equality.**
  The generic GROUP BY groups by serialized spellings (collationGroupKey):
  int64 5 and float64 5.0 share "5", but 1e15 spells "1e+15" ≠ int64's
  "1000000000000000" (sqlite3 groups them; frigolite's serialized-key
  semantics deliberately split — parity must mirror the ENGINE, not the
  oracle). typedIntGroupKey therefore engages only when FormatFloat('g',-1)
  == FormatInt, with ±0.0 special-cased to int 0's bucket ("0"); NaN,
  non-integrals and exponent spellings keep the string map. The map
  invariant "an integral-spelled group is only ever filed typed" removes all
  cross-map lookups. Pin with fast-vs-forced-generic parity (ORDER BY makes
  the generic route deterministic; ORDER BY also gates the feed off).
- **A seek that enforces the whole WHERE may skip the per-row re-check — but
  "enforces" needs the plan and the re-check to agree on EVERY bound.**
  Coverage requires pure-integer literal bounds only: text bounds stay
  inexact even when they spell an integer because value.NumericText trims
  whitespace (' 10 ') while the re-evaluation's affinity conversion does
  not (pre-existing plan/re-check divergence the re-check arbitrated);
  float ceil/floor bounds and int64-edge saturations admit boundary
  candidates that must fail the re-check. Distinguish three conjunct
  outcomes (folded / rowid-but-unresolvable / not-a-rowid-predicate) — the
  old two-state return conflated "folded" with "not a rowid conjunct" and
  silently claimed coverage for `c > 0`.
- **btree.LeafBatch starts at cell 0; a SEEked cursor must not replay.**
  ScanTableLeaves was built for full scans: the first page's loop must start
  at LeafBatch.StartCell() (the seeked cellIdx) or the range seek emits from
  the tree's first row (caught immediately by TestRowidSeekRange counts).
  Resume after a mid-walk save is still one Next() then the per-row cursor
  loop.
- **Planning overhead is scan overhead: one tableRowCount walk per query.**
  indexScanOrderIndex walked the whole table (300k Cursor.Next steps, ~8.5%
  of scan CPU) whenever the rowid-constraint check missed the conjunct
  shape — BETWEEN was simply not recognized (the comparison operators
  were). Teach the planner the AND-pair BETWEEN form; same final plan, the
  walk disappears.
- **Benchmark deltas on a shared fleet box need back-to-back same-binary
  rounds**: main itself swings 2x between rounds (insert 226k→118k ops/s).
  Run main and worktree benches interleaved and compare per-phase ratios;
  single absolute numbers are noise. Full-harness failure SETS are equally
  load-sensitive (main: 26 vs 173 failing files across runs) — regression
  proof must be per-FILE, each file run individually on both sides
  (single-file runs are deterministic).

## PERF.FLOOR2 — the exec-level per-statement wall, tranche 2 (fleet/perf-floor2, 2026-10-02)

- **The template cache changes what "memoize the AST" means.** db.Query with
  rendered literals normalizes to a template and CLONES per statement —
  copy-on-write: literal nodes are fresh, every other node is SHARED with the
  template. So memoizing collectors on `*SelectStmt` identity NEVER hits in
  the fresh-text bench; the stable memo keys are schema-owned state (colDefs
  slices, schema entry pointers) guarded by the schema fingerprint, exactly
  like seekColIndexFor. Entry-pointer identity is safe because schema entries
  are replaced, never edited, and any DDL moves the fingerprint (local
  cookie/mutation epoch; external commits drop the cache).
- **The biggest point-SELECT tax was speculative dispatch, not validation**:
  FROM dispatch probes eponymous-then-created vtab forms BEFORE knowing the
  name is a vtab, and each probe built vtabScanOptions — collectVtabRefCols
  walked the whole statement TWICE per real-table SELECT (~450B/stmt). Probe
  eligibility first (same predicates the materializers' own early-outs use),
  build options only for actual vtabs. DML pays the same shape:
  VTabUpdaterInstance + EchoVTabSource per statement — memoize the NEGATIVE
  (name → not-a-vtab) per fingerprint; clear on RegisterVtabModule because
  module registration moves neither schema nor fingerprint.
- **strings.ToUpper(createSQL) per statement is a silent 16% tax.**
  HasWithoutRowidKeyword(ToUpper(entry.SQL)) ran at ~10 call sites; the
  answer is a constant per schema entry. Memoize on entry identity
  (TableIsWithoutRowidEntry). Same disease: prevalidateSchemaFunctionSafety
  built collectSelectColumnRefs' map before checking the table even HAS
  generated columns; the agg-feed eligibility chain (incl. its own ToUpper)
  ran for statements with no aggregate and no GROUP BY. Gate walks behind
  cheap shape checks that decide the same outcome.
- **applyPreupdateAffinity re-resolved the table per ROW** (FirePreupdate →
  findTable with external-mod probe + trigger scope + cache lookup) for one
  table per statement. Engine-side single-entry memo (folded all-schemas
  fingerprint + name → entry); failed resolution drops the memo.
- **allSchemasFingerprint (fold over e.dbList) is the right key when a name
  can resolve in any attached schema** — a single-schema fingerprint can miss
  an attached-DB DDL that changes which entry a name resolves to.
- **lockreg marks are reference-counted per (path, connection) with empty-set
  cleanup, so Registry.AnyMarks (one len-sweep per map) is exact**: with zero
  marks, every "ByOther" check answers no, so CrossConnLockError can skip the
  per-statement key resolution (a findTable) and the busy loop. Keep the
  locking_mode=EXCLUSIVE SetPersistentShared side effect and the nolock style
  BEFORE the early-out — the original order sets the mark even under nolock.
- **The remaining update/delete floor is the sibling domain**: after all
  exec-level cuts, the update profile is ~33% pager stmtReadTouch/copyPage
  (statement-journal capture) + btree seek, ~8% template clone. execResult's
  per-statement wrapper (~130B) is not safely poolable (public Rows are
  caller-owned).
- **pprof -diff_base across phases LIES at MB scale**: heap profiles are
  cumulatively sampled; N=0 phases show hundreds of MB of phantom "delta".
  For phase-clean CPU attribution use a focused probe whose profiler starts
  AFTER setup; for allocs, reason from cumulative profiles of functions
  unique to the phase.
- **Quality gates were ALREADY red at base** (cognitive/cyclomatic offenders
  in fts5/exec pre-existing; engine_core.go over the 1000 hard cap). The
  tranche's obligation is to not ADD offenders: extraction helpers for new
  branching, move helpers out of near-cap files — engine_core ended UNDER
  the cap (991), strictly better than base.

## PERF.INSERT2 — insert write-path staging diet + finish adjudication (fleet/perf-insert2, 2026-10-03)

Landed: pooled leaf-split staging buffers (1), BTree.cursors inline array
(2), depth-indexed page-header scratch on the insert walk (3), insert-path
staging diet — reusable insCell/insRecBuf/insIPKVals + insertWriteTree seam
(4), one cached write tree per insert identity + Cursor.Close (5); dead
uniqueColIndicesWithPK helper dropped (4b).

**Every "this buffer is consumed synchronously inside window X" reuse claim
must be audited field-by-field, not struct-by-struct.** INSERT2-4's staging
diet carried two latent reuse bugs the pins missed; both fired only on
second-and-later rows/statement re-entry:

- **Reusable storage.Cell needs EVERY wire field reset per row** — writeTableRow
  set Type/RowID/Payload/LeftPtr/Overflow but left PayloadLen/LocalLen from a
  prior overflow-bearing row. cellPlen/localOrFull trust those when set, so
  the row after an overflow row encoded a wire cell with the PREVIOUS row's
  payload length and local split → "database disk image is malformed" on
  read (fts5prefix 4096-row doubling, %_data shadow tree). prepareCell cannot
  clear them (its already-prepared guard must keep the chain across
  balance_deeper re-entry) — the CELL PRODUCER owns the reset.
- **encodeValueInto's ZeroBlob case relied on a zero-filled buffer**
  (`make([]byte, n)`), but AppendEncodeRecord over a REUSED buffer keeps the
  previous record's bytes in exactly the tail zeroblob does not write.
  rtree xCreate seeds each new family's root node with
  `INSERT OR IGNORE INTO <v>_node VALUES(1, zeroblob(820))`; the seed row
  carried the PREVIOUS rtree's node blob, so the new tree decoded the old
  tree's coordinates (T30 rt1 rid=1 returned t6's float-bit row) and the
  IGNORE silently skipped the (existing-rowid) seed. Fix = `clear(buf)` —
  MEM_Zero-on-demand parity. Rule: any encoder contract that says "caller's
  buffer is zeroed" must be enforced by the encoder, not the allocator.
- **Debugging payoff**: a 30-line /tmp reproducer beating the exact pin
  shapes (12-round doubling corpus; two-rtree-family sequence) + a temporary
  BTREE_TRACE printf in InsertCell (root, rowid, plen, first payload bytes)
  found in minutes what profile-reading could not: the trace's payload
  prefix showed the stale bytes verbatim. `git bisect` on ONE deterministic
  failing test pinned the commit before any code was read.
- **Benchmarks (NROWS=300k full env, 8 interleaved main-vs-branch rounds,
  both run orders)**: insert_xact +11.2% (median 343,254 vs 308,694 ops/s;
  the branch is also far more stable across rounds — 330-368k vs main's
  228-332k), point +0.7%, scan +0.2%, update −0.1%, delete −1.1%, file
  autocommit −0.3% — read/write parity everywhere; group −2.2% (45 vs 46
  q/s) with identical CPU totals and no profile mechanism — treated as
  shared-machine noise, flagged for the next round's re-measure on a quiet
  machine. Heap at group time 15-19MB vs 51-80MB (the staging diet pays
  for itself in resident memory).
- **Order-bias control**: alternate binaries AND alternate which binary runs
  first; medians over 5 rounds; group-only PHASE=select_group short runs
  (early-return after the phase) give cheap statistical power when a
  0.7s-phase shows a consistent small delta.
- Process: the finish agent committed the predecessor's coherent dead-code
  WIP (uniqueColIndicesWithPK removal, zero callers) after build+race green —
  adjudicate by compile + call-site audit, then land separately so the fix
  commits stay attributable.
- **Finish-session gate reality**: TestSQLiteSuite under bare `go test ./...`
  carries ~4.7k subtest failures on MAIN and the branch ALIKE (4689/4689,
  case-level jitter ±12 from shared cwd file state) — the documented
  "legacy drift (adjudicated superseded)" state; testgen is authoritative.
  Gate = branch fails no MORE than main: after the two reuse fixes the
  branch's 12 INSERT2-caused regressions (fts4merge4/fts5 families) are
  green and the failure set is main-identical. Judging harness deltas
  requires per-FILE solo runs on both sides — full-run comparisons drown
  the signal in cwd-state jitter (vacuum-11.2, trigger1-10.x flip
  run-to-run on BOTH sides).

## FIX.INS2-VACUUM — insert write-tree cache vs in-place pager layout replacement (fleet/fix-ins2-vacuum, 2026-10-03)

INSERT2-5's executor-level cached write tree (one `btree.BTree` per
`(pager, root, kind)` insert identity) broke the second-and-later
`PRAGMA page_size=N; VACUUM` in a whole testgen census (vacuum, backup,
reservebytes, vacuum3, vacuum6): page size stayed at the old value.

- **Mechanism (exact)**: `btree.initFrom` SNAPSHOTSc `pageSize`/`usableSize`
  at wrapper build (btree_pool.go). `Pager.ResetToEmpty(newSize)` replaces
  the layout IN PLACE — same pager pointer — so `insertWriteTree`'s cache key
  (pointer, root, kind) still matches and the copy-back's re-INSERTs
  (frigolite_backup.go copyLocked runs DROP/CREATE/INSERT through dst.Exec)
  write the OLD geometry into the NEW layout → "database disk image is
  malformed" → vacuumRebuild's restore branch (`frigolite_vacuum.go`) silently
  re-copies with keepDestPageSize=false, which resets main to the TEMP db's
  OLD page size. The visible symptom (page_size stuck at 1024) is the restore
  path, not the write itself. SQLite parity: the page size lives in the
  file-shared BtShared, so a layout change is instantly visible to every
  cursor — a per-wrapper snapshot must be invalidated when the layout is
  replaced.
- **Fix (option "one hook")**: `Pager.SetLayoutHook` fires (outside p.mu)
  after SetPageSize / ResetToEmpty / ApplyReservedBytes; the Engine registers
  `e.dml.InvalidateWriteTree` for every database pager (main/temp at open,
  ATTACH at AppendDBList). Zero hot-path cost — the cache is untouched until
  a layout change. Pins (TestIns2Pin) and bench parity held.
- **Debugging payoff**: the vacuum restore branch SWALLOWS copy-back errors by
  design, so the failing PRAGMA showed no error anywhere — a one-line printf
  in the restore branch ("VACTRACE: copyback failed: ...") turned an invisible
  failure into the exact error text in one run. When a caller silently
  repairs on failure, trace the failure FIRST.
- **Bisect caveat**: the "6/6 good vs 0/6 bad" bisect verdict was wrong for
  capi2 — it fails IDENTICALLY (5 mismatches, capi2-6.7/6.9 row data
  `3 4 3 4 1 2` vs `2 3 3 4 1 2`) at the good commit 45d58936a. That is a
  pre-existing, unrelated data bug; a per-package failure-set diff at BOTH
  bisect endpoints is cheaper than trusting a single-package verdict.
- Minimal-repro caveat confirmed: a bare CREATE+INSERT+VACUUM probe PASSES
  even at the bad commit — the failure needs the copy-back to REUSE a cached
  wrapper across a root reallocation (enough prior schema/row state that the
  (pager, root, kind) key survives the reset). Reproduce from the harness
  shape first, minimize only after the mechanism is known.

## PERF.UPDDEL (2026-10-03) — point UPDATE/DELETE structural round

- **Slot-path template substitution** (template_slotpath*.go + clone_scratch
  trySlotPathLive): a template-cache hit re-walked and re-cloned the whole
  AST per statement (updateCOW 350MB/24% of the 500k-update alloc profile;
  deleteCOW 12.5% delete CPU). Each single-statement template entry now
  precomputes literal-slot POINTER PATHS at store time, mirroring the COW
  walkers' field order and value gates; a PrepareExec hit rewrites only the
  literal leaves of the entry's persistent per-execDepth live clone — zero
  walk, zero alloc. Red lines that made it safe: the template AST stays
  immutable (only COW-copied nodes are rewritten); retained Prepare never
  takes the live form (FIX.PREPARE-ALIAS); dual-slot LIMIT/OFFSET,
  folded-minInt64/hex slots, blob/RAISE, INSERT tuples with nested literals
  and multi-statement batches stay COW-only with identical refusals.
- **NumericLit.cached is hidden mutable state**: execution caches the parsed
  literal ON the AST node (SetCached) on first evaluation. An in-place
  literal rewrite MUST SetCached(nil) — a stale cache silently served the
  PREVIOUS statement's value (INSERT k=2 executed k=1 → UNIQUE violation).
  The COW form never hits this because its literal nodes are always fresh.
- **Slot-node kind follows the VALUE, not the slot's parse**: INSERT tuple
  slots accept int/float/string into either node kind (insertValue parity);
  writeSlot swaps the node when kinds diverge and rewrites in place when
  they match. writeSlot returning false is fail-closed (fall back to the
  COW clone), never a silent skip.
- **Cached point-op write trees** (updTree/delTree, the insertWriteTree
  pattern): one wrapper per (pager, resolved root), closed+replaced on
  identity change, invalidated by the pager layout hook, re-keyed+persisted
  after root moves (pointWriteTreeSync = persistTreeRootPage parity), and
  swept per statement with the new BTree.ReleaseIdleCursors — the btree
  write primitives (seekLeafRow, DeleteCellByRowID) LEAK their internal
  seek cursors onto the wrapper; statement-local trees swept them at Close,
  a persistent wrapper needs the explicit per-statement sweep.
- **Per-statement memo cluster** (execdml): schemaFingerprint memoized on a
  statement-sequence epoch (the memo guards — column index, column lookup,
  row plan, index defs — each paid a pager-header lock per read, several
  reads per statement); allTableIndexes one-slot memo (three resolutions
  per point UPDATE, each walking the databases map); loaded-trigger
  validation walk memoized on the fingerprint. Convention: the MAIN schema
  manager's fingerprint is THE DDL-invalidation token (matches existing
  ciCache/lookupCache guards).
- **Pooled point-update row map**: collectPointUpdateRow's SET-eval RowMap
  is cleared+refilled per statement. GATE: statements whose SET expressions
  contain subqueries must fall back to a fresh map — a correlated subquery's
  evaluation RETAINS the row as the engine's outer-row scope
  (SelectEngine.outerRow), and the next statement's clear() would corrupt
  it. Same class of trap as the clone scratch's retained-Prepare alias.
- **Paired-bench numbers** (NUPDATE=100k/NDELETE=30k, interleaved
  main-vs-worktree runs, quiet machine): update 296k→342k ops/s vs main
  ~266k (+28%), delete 411k→440k vs main ~364k (+20%); insert 394k→439k,
  point 496k→525k (slot-path helps the other literal-heavy phases too);
  scan/group/file-autocommit unchanged. Remaining walls (documented, out of
  this round's scope): btree SeekToRowID descent (~11%), pager
  stmtReadTouch statement-journal capture (~7%), Engine.Exec entry gates
  (CrossConnLockError, external-mod probe, SetStmtTime time.Now ~13% on
  delete), execPreflight's per-statement schema revalidation.
- **Harness instability is environmental**: TestSQLiteSuite's
  t.Parallel file-based cases collide nondeterministically under ANY
  concurrent load (main: 148–212 file failures; quiet wt run: 0). Judge
  root-suite failures only from a quiet machine, and only as a
  failure-SET diff vs a same-conditions main run; testgen packages are
  the authoritative correctness gate.
- **Fresh worktree fixtures** (repeat): copy testdata/*conformance,
  testdata/walconformance, testdata/recoverconformance,
  internal/fts/testdata/ftsconformance AND tools/orafixture from the main
  checkout before judging writer/segview/window failures.
## PERF.SCANBOX — scan/point read-path boxing diet (fleet/perf-scanbox, 2026-10-03)

Landed: typed aggregate feed lane (covered rowid-range plans step
SUM/AVG/TOTAL/COUNT straight off the record payload — `function.TypedSumStep`
unboxed surfaces StepInt64/StepFloat64 + countAgg.CountRow; text/blob and
ADD COLUMN defaults fall back to the boxed Step per value), covered eq-seek
skips the redundant WHERE re-eval, point fetch decodes only consumed columns
(seekDecodeCols), decode sets materialized by position as []bool
(DecodeRecordValuesFromTypesCols — map hash lookups out of the row loop),
single-bare-ref point output reads the slot directly. Interleaved medians (final, 5 rounds):
select_scan 21.33M→38.68M rows/s (1.81x); point +0.7%; insert/update/
delete/group/file parity (±0.7%) — zero regressions. Point's floor is
statement prep (render→lex/parse of 200k DISTINCT literal strings +
template misses + execSelectPrevalidate) — the sibling's lane;
fetchSeekStructRow was only 12.5% cum; the in-slice levers (covered eq
skips the WHERE re-eval, single-bare-ref output slot read) bought ~+2%.

- **The scan loop's boxing tax is three separate taxes**: (1) int64→interface{}
  boxing in decodeValue (24% cum, 175MB), (2) map-based decode-set tests
  (mapaccess2_fast64, ~6%), (3) the boxed feed step's unwrap/classify chain
  (unwrapCollatedValue + sumNumericArg + asserts, ~12%). Only (1) needs a
  semantically-typed lane; (2)/(3) are mechanical. Killing all three needs the
  lane to read the payload DIRECTLY — a typed decode into []interface{} still
  boxes; the values buffer must not exist.
- **Typed accumulator parity is exact when the typed step IS the boxed
  branch**: sumAgg.StepInt64/StepFloat64 reproduce Step's own branches
  (same count++, same stepExact/kahan routing), so mixed typed/boxed feeding
  is state-identical. Fallbacks preserve the rest: text/blob → boxed Step
  (numeric-text classification lives in sumNumericArg), stored NULL → skip,
  past-record-width → precomputed ADD COLUMN default (evaluated once per
  statement, ApplyColumnAffinity'd like applyColumnDefaults), IPK alias
  stored NULL → rowid. Oracle 3.54 pin: int64 overflow in SUM persists as
  "integer overflow" even after later REAL/integer inputs — do not
  "absorb" it.
- **On-disk slot mapping**: feed slots are colDefs indices; record positions
  are colDefs minus Dropped (shiftDroppedColumns's contract). The lane walks
  by precomputed rank. (The old map-based covered feed selected decode
  columns by colDefs index on the on-disk loop — a latent wrong-column
  decode for dropped-column tables; the rank walk fixes that class.)
- **Benchmarking under a loaded machine**: fleet siblings run parallel full
  suites (load 7+); a full-suite failure count is then meaningless (main
  3110 vs branch 19 for the SAME tree in different windows). Phase-focused
  binaries (PHASE=env early-return) + interleaved base/branch medians in one
  window are the only trustworthy signal; absolute numbers move ±15% with
  load, ratios hold.
- **Column-targeted decode has a fixed cost** (reference collector + bool
  set construction per fetch); a one-row point fetch on a narrow table
  amortizes none of it — measured a consistent −5% on the 2-column point
  benchmark until the set build was gated to tables with ≥4 columns (below
  that: full decode, which is the same boxes for less work). Gate per-shape
  fast paths by WHERE THE SAVINGS SCALE, not just by semantic eligibility.
- **Harness filtered runs need setup**: `go test -run
  TestSQLiteSuite/select1$` fails "no such table: test1" on MAIN and branch
  alike — filtered-mode skips the fixture setup step. Full-suite or
  per-FILE solo runs only.

## PERF.EXECENTRY — the exec-entry gate diet (fleet/perf-execentry, 2026-10-03)

- **The per-statement entry tax after upddel/insquick was still ~20-27% on
  DML**: two unconditional wall-clock reads in execPrepared (t0 +
  time.Since, feeding hooks nobody registered), the preupdate event build
  (Old/New value copies + the WITHOUT-ROWID rowid resolution + applyPreupdate
  Affinity's per-row allSchemasFingerprint probe) on every DML row, the
  cross-connection lock resolution (stmtLockKey → findTable → ValidateHeader
  ×2 + six registry lookups) whenever ANY marks existed — including this
  connection's OWN write-tx marks, which no *ByOther check can ever satisfy
  — and execPreflight's two full expression-tree walks (FROM-term counter +
  RAISE() checker) plus the FK-prepare FindTable per statement. All four
  gates are now zero-cost when their feature is absent, which is the
  default: StmtHooksActive, DMLContext.PreupdateNeeded,
  lockreg.ForeignMarks, and the pfAST*/pfDML* preflight memos.
- **ForeignMarks v1 walked the mark maps per statement — map iteration is
  the cost, not the mutex.** Iter.Init + Next + the iterator's chacha8
  reseed cost ~7% CPU on the update floor, more than the checks it
  replaced. The fix is counters: every Set* transition keeps a
  per-connection mark count (connMarks/markConns/onlyConn; backup/dotfile
  marks count as anonymous and stay conservative), so "any foreign marks?"
  answers from two integer compares. General lesson: a registry fast path
  must not itself iterate; maintain the summary incrementally.
- **execPreflight memoization is safe by AST identity, not by content
  hashing**: the template cache's per-depth live clones hand structurally
  identical statements the same pointer back, slot substitution rewrites
  LITERAL LEAVES only, and none of the memoized checks reads a literal
  (RAISE detection is function-name-shaped, FROM count / subquery arity are
  structural, FK resolution reads declarations). The memo slot RETAINS the
  statement reference, so the pointer key can never address a recycled AST
  — the single-entry memo doubles as the GC pin. Guards: folded
  all-schemas fingerprint + the foreign_keys setting for the DML half.
- **BTree wrapper reuse (lever 4) measured ZERO and was dropped.** A
  full engine-scoped rent/release slot (the insertWriteTree pattern:
  identity-keyed, Closed() drop-never-rearm, layout-hook invalidation,
  ReleaseIdleCursors at frame exit, execDepth-guarded release) was
  implemented, passed btree/exec/execdml suites, and then measured
  noise-equal to no-slot in three interleaved rounds. The 28.5MB of
  NewBTree allocs is cheap tiny-object churn; the real per-statement cost
  is the cursor lifecycle + registry work, which happens EITHER WAY
  (ReleaseIdleCursors ≈ Close's sweep, and the pooled cursor's resetFor
  runs on every acquisition). Patch preserved at /tmp/perf/execentry_
  treeslot.patch for whoever attacks the point path next — the pattern is
  proven safe but must buy something to keep.
- **Interleaved A/B or nothing**: in one window main measured insert
  523.8k/552.2k, update 376.0k/336.4k, delete 486.9k/449.0k across two
  rounds (±12% swings on MAIN itself); single absolute comparisons would
  have "proven" a 13% point regression that a denoised NSCAN=30 A/B showed
  to be ≤1%. Ratios from interleaved same-window rounds are the only
  signal.
- **Out-of-lane residue left on the floor** (for the next tranche):
  function.SetStmtTime's per-statement RWMutex pair + Now() (function pkg),
  noteReservedDbs' per-statement ToUpper + map assign inside open
  transactions (transaction.go), execSnapshotDML's pager BeginStatement +
  snapshot growslice (engine_tail.go + pagerstmt.go), and the point path's
  execquery allocators (affinityCollector 34MB, ParseRecordHeader 27.5MB,
  fetchSeekStructRow 99MB cum per 200k queries). The point target (700k)
  is unreachable from the entry-gate lane; it lives in execquery.
## PERF.STMTPREP (2026-10-04) — fused normalize+hash scan, statement-journal list, prevalidate memo (fleet/perf-stmtprep)

- **Adjudicating a dead agent's WIP by measuring, not reading**: the
  uncommitted diff looked destructive (−97 lines from select_exec.go, a
  pointer to a select_prevalidate_memo.go that did not exist) — discard
  material. It was in fact the QUALITY-GATE REMEDIATION for the tranche's own
  commits: 7ae2d7a3f pushed normalizeScan to gocognit 24 / gocyclo 14 and
  519409c1f pushed select_exec.go to 1013 lines (hard 1000 fail). Run
  `gocognit -over 15` on the BASE commit first: violations present at base are
  pre-existing (do not fix), violations absent at base are YOUR tranche's debt
  (must fix). With that lens the WIP is a coherent half-move: re-apply it and
  finish the missing file. Committing the remediation separately keeps the
  perf commits attributable.
- **Finish-session gate math**: quality_gate.sh on staged perf files fails
  even when your tranche is innocent — insertStmtValues (gocognit 22 /
  gocyclo 15) and execRealTableSelect (13) pre-exist on main. Gate = ZERO
  violations absent at base (`comm -23 branch_violations base_violations`
  after normalizing the ./ path prefixes gocognit/gocyclo print). This
  tranche ended with 0 new and 3 removed (normalizeScan, the memo fn, the
  1000-line file).
- **cachedPrevalidateChecks decomposition pattern** (gocognit 22 → ≤15 with
  semantics frozen): split by PHASE, not by condition — the runtime-dependent
  consulted-form gate (prevalidateRuntimeDependent), the schema-fingerprint
  read (prevalidateMemoGen), the registry-relevant bypass
  (colDefsTouchRegistries), memo hit/store (prevalidateMemoLookup /
  prevalidateMemoStore), and the direct-walk wrapper (prevalidateDirectly,
  the `err != nil → &Result{Error: err}}` shape that repeated three times).
  Each helper keeps its original comment block; the dispatcher reads as the
  original function's outline.
- **Statement-rollback exactness now has a whole-page pin**: page-hash digest
  (sha256 over header + page-number-sorted page bytes via db.pager.Pages(),
  reachable from root-package tests through the unexported field) around an
  INSERT OR FAIL that fails UNIQUE with an AFTER-INSERT trigger armed —
  the strongest shape because the failing statement dirties OTHER tables'
  pages before aborting. Gotcha: seed rows fire the trigger too — assert the
  side-table count is the SEED count, not zero.
- **Bench harness loss and reconstruction**: the paired-bench scratch
  (/tmp/perf/frigo + /tmp/frigo_main) does not survive host tmp cleanup.
  Absolute ops/s from a RECONSTRUCTED harness are not comparable to a prior
  session's reference numbers (phase shapes differ: my scan does
  count+sum+min+max → 4.4M rows/s vs their 39.7M lighter scan) — only the
  SAME-harness interleaved branch-vs-main ratios mean anything. Rebuild cost
  was ~80 lines; keep the harness under /tmp/perf/stmtprep and expect to
  rebuild per session.
- **Paired medians, full env, 3 interleaved rounds (alternate run order)**:
  point +6.2%, delete +7.1%, update +3.9%, insert +0.7%, scan +2.3%, group
  ±0, file −0.4% — wins exactly on the prep+journal lanes, no regression
  outside noise. Suite parity: branch 4079 vs main 4069 case-level failures
  in the full TestSQLiteSuite run, delta ±12 = documented shared-cwd jitter
  (all differing files pass SOLO on both sides via FRIGOLITE_TEST=^file.json$);
  per-FILE solo runs remain the only deterministic adjudication.
- **Fixture drift bites full suites**: worktrees lack gitignored
  oracle-generated fixtures (testdata/*conformance AND
  internal/fts/testdata/ftsconformance/*.db — the four WriterConformance
  .db files are NOT in git). Copy both trees from main before a full
  `go test ./...`, or btree/fts fail with "database disk image is
  malformed"/"oracle fixture missing" spuriously.

## PERF.DMLCORE (2026-10-04) — point-DML glue diet final tranche (fleet/perf-dmlcore)

Paired interleaved vs main @624667e5a (same harness, 3 rounds, machine under
fleet load — only deltas mean anything): insert +11-13%, delete +11-12%,
update +7.5%, point +2.5-3.5%, scan +0.5-2%, group ±0, file unchanged.
Mission stretch targets (insert 800k / update 600k / delete 800k / point
750k ops/s) NOT reached at this machine's load; all landed cuts are real and
stable. Commits: 453e0e1bf (glue diet), fccd1cea6 (+empty-name guard),
e27ed2d32 (per-exec glue), 2fca3240d (mayScan memo), 304ce1411 (touch lock
skip), 5adadcc37 (pool gate fix).

- **The statement glue tax is real and cheap to collect**: per-statement
  ToUpper over whole schema SQL (isStoragelessVirtualTable) was 22% of
  insert alloc_space via strings.Builder; EqualFold(TrimSpace(type),
  "INTEGER") per row (isIPKRowidAliasCol) ~80ns/row; ToUpper(ctx.Name) per
  statement (noteReservedDbs); ToUpper per locking-mode resolve. All now
  length/first-byte screened or pointer-keyed. Lesson: profile the FLAT top
  of the alloc profile per phase — the glue frames (execResult, ToUpper,
  EqualFold) outrank the "algorithmic" frames.
- **strings.ToUpper already returns the input unchanged for all-ASCII-uppercased
  strings** — the cost only shows on lowercase input ("main" → "MAIN"
  allocates). Cache at the context (DatabaseContext pointer keys) or screen
  before folding.
- **Pager.SchemaCookie is read several times per statement** (allSchemasFingerprint
  folds it for every memo gate); an atomic cookieCache (cookie+1, 0=unknown)
  with invalidation at EVERY p.header replacement/copy site killed the
  per-read RLock. Do NOT memoize the folded key at the schema.Manager level:
  `PRAGMA schema_version=N` moves the cookie WITHOUT a mutation epoch, so an
  epoch-keyed memo serves a stale key (GetEntries' cache key must track the
  real cookie). The atomic cache is exact because only BumpSchemaCookie writes
  [40:44] in place; every other path replaces the whole image and invalidates.
- **Journal skip under RLock**: ReadPage's stmt-touch probe upgraded to the
  exclusive lock per read inside a statement scope; checking
  stmtTop.stmtEntryFor under the READ lock (stable — captures hold p.mu)
  removes the upgrade for already-journalled pages (interior/root/schema
  re-reads). Neutral-to-positive; strictly fewer lock ops.
- **e.databases vs e.dbList**: per-statement loops must use dbList (ATTACH
  order slice); mapIterStart over the databases map was 50% of
  snapshotAllPagers' samples. Empty-registry scans (ftsTables/fts5Tables)
  skip too — map-iterator setup per statement on FTS-less workloads.
- **AST-pure per-exec queries memoize under a per-invocation generation**:
  selectHasWindowFuncs (asked up to 6× per exec) and scanTableAffinityCols
  (rowid-seek + range planners each ask). Key = execSelect-entry generation
  bump + argument identity — the generation is what makes recycled
  clone-scratch addresses safe (outer/subquery can never alias entries).
  Same single-slot + fingerprint pattern for schema-pure verdicts
  (MayScanCreatedVTab, echoVTabSource, notUpdaterVtabMemo).
- **VALUES-tuple pooling hazard (the round's only regression)**: pooling the
  per-row VALUES slice on the executor is only safe when NOTHING nests
  another statement while the row is in flight. BEFORE INSERT trigger
  bodies (tkt3832) and FK actions insert on the SAME executor — the nested
  evalTuple overwrote the scratch and the outer row wrote the nested row's
  values ("UNIQUE constraint failed" on insert 2). Gate: no triggers on the
  table AND foreign_keys off; RETURNING rows stay fresh (they escape).
  Pinned by TestInsertTuplePoolNestedInsertPin. Corollary: the existing
  insCell/insRecBuf pools are safe because their consumption window
  (encode+InsertCell) cannot interleave a nested statement.
- **Point-phase profiling on macOS is distortion-heavy**: idle Ps park in
  kevent and steal SIGPROF samples (69-93% of samples at kevent while
  /usr/bin/time shows user≈wall). Sample COUNTS per function are wrong;
  only relative order within frigolite frames is usable. Decompose with
  wall-clock variants instead: db.Query full vs prepared-Stmt bound vs
  Prepare-only (point: parse ≈580ns of ~1.6μs; engine exec dominates).
- **Page-parse memo for the rowid-seek descent: SKIPPED** — parsePageInto is
  O(1) (header fields + validation, no cell walk), so the per-level saving
  is ~50-80ns against a generation-counter invalidation surface across
  every pager write path (markDirty, statement rollback replay, Restore,
  serialize). Not worth the risk this round; SeekToRowID descent remains
  4-19% of point/update/delete.
- **Column-targeted decode for narrow tables stays gated OFF** (<4 cols,
  re-measured neutral-to-negative with the new surroundings).
- **Harness adjudication (confirmed prior precedent)**: full-suite
  TestSQLiteSuite failures in a worktree are dominated by shared-CWD
  attach-file jitter (test.db1/test.db2 survive cleanupTestDBFiles — it
  only globs *.db* not *.db1/*.db2 — and parallel file scheduling decides
  who pollutes whom). Adjudicate per-file SOLO (FRIGOLITE_TEST=^file.json$):
  all jitter files pass solo on both branches; the serial-mode failure sets
  are also nondeterministic (main isolated: 0-377 across runs). A branch's
  serial failure set being a SUBSET of main's is the clean bill; plus the
  per-file solo green.
- **isNonModifiableTable first-byte screen**: guard len==0 — schema entries
  with empty names exist (tkt_78e04e52ea panicked).

## FIX.INSREG (2026-10-05) — R4-R6 insert-tranche regressions: result-staging nesting + rowid-cache truth (fleet/fix-insreg)

Both testgen regressions from the perf tranches bisected to ONE commit —
2b3683c31 (PERF.INSQUICK-2: IPK-conflict append gate + reusable success
results) — via `git bisect run` with the testgen package as probe (verify
PASS at a pre-tranche commit first to confirm regression vs pre-existing).

- **Reusable executor-scratch Results are only safe at INSERT nesting depth
  1** (bug 1, fts5lastrowid): an INSERT statement's own SIDE WORK issues
  nested INSERTs through the same executor AFTER execInsertTuples staged
  the statement Result — the fts5 statement-end shadow flush (each
  autocommit INSERT into an fts5 table writes the %_data structure block
  via Engine.ExecSQLUntracked), trigger bodies, FK actions, sqlite_sequence
  upkeep. The nested statement restaged the shared insStmtRes with ITS
  change count / rowid; the engine's execTrackChanges (which runs only
  after the whole statement returns) then published the NESTED rowid —
  last_insert_rowid() read the %_data block id (10) instead of the fts5
  rowid (3 / explicit -22). The commit's own safety argument ("nested
  writes consume their fields strictly inside the outer consumption
  window") was wrong about the window: staging happens at statement build,
  consumption at statement END. Fix: execInsert tracks insDepth; scratch
  handed out only at depth 1, depth >= 2 builds fresh. Bulk INSERT — the
  hot path — stays on the scratch. Same shape as the PERF.DMLCORE
  VALUES-tuple pooling hazard: ANY executor-scratch result/staging must be
  depth-gated, not "consumed before next reuse" argued.
- **A bump-only-grows cache needs a PROVEN base** (bug 2, spellfix): the
  append-bias gate (skip the IPK uniqueness probe when rowid > cachedMax)
  trusted a cache that bumpRowIDCache seeded from nothing — an EMPTY entry
  was set to the just-inserted rowid, even when that rowid sat BELOW the
  tree's true max. Sequence: CREATE TABLE resets the cache mid-scenario →
  explicit insert of rowid 5 re-seeds cache=5 while the shadow table still
  holds 10/20/30 → rows 20/30 skip the probe → duplicate rowids accepted
  (spellfix 7.4.2 "constraint failed" lost). Fix is two-part: (1) engine
  invariant — bumpRowIDCache grows only over an EXISTING entry; entries
  are created solely by proven max scans (plainNextRowID) or the probe's
  re-arm; the AUTOINCREMENT sequence keeps its unconditional bump (largest
  EVER used, sqlite_sequence + max-scan fallback keeps it correct); (2)
  the IPK probe, on a clean miss with no cache entry, re-arms the gate by
  seeding the cache with the tree's TRUE maximum (scanMaxRowID — the same
  helper plainNextRowID uses), once per invalidation window; a bulk
  ascending load seeds at its second row (tree size 1) and skips probes
  from the third row on, matching the pre-fix skip pattern. Lesson: any
  "skip the check when X > cached" fast path needs its cache invariant
  enforced at EVERY writer, not just documented at the reader; "invalidate
  on delete" is not enough when other writers can seed low.
- **Cursor gotcha: Prev() from the seek-past-end position does NOT clear
  endOfBTree** — ReadCell then refuses with "btree: cursor at end". A
  "seek past the right edge, Prev, ReadCell" recipe silently never reads
  (establishment via that recipe seeded NOTHING, and every explicit-rowid
  INSERT re-probed: -13-15% on the insert phase). Use scanMaxRowID or seek
  to a key you know exists.
- **Bisect hygiene**: `git bisect start <bad-sha> <good-sha>` — branch
  names fail in worktrees ('main' used by the primary checkout). Probe =
  the failing testgen package; a grep for ^FAIL disambiguates build
  failures from test failures when old trees lack helpers.
- **Oracle discipline**: fts5lastrowid oracle-checked directly with the
  sqlite3 CLI (fts5 ships in the system binary: 3 then -22). spellfix1 is
  NOT in the system sqlite3 — the TCL expectations are the contract there.
- **Bench under fleet load is pair-only**: /tmp/perf was wiped; harness
  reconstructed (txn-insert/point/scan/group/update/delete/file-auto at
  the documented env sizes). Only fix-vs-main deltas on the SAME harness,
  interleaved on an idle machine, mean anything — absolute ops/s compared
  against FLEET-STATE numbers from an idle run are meaningless.
## FIX.MISCREG (2026-10-05) — two attached-schema memo regressions from PERF.UPDDEL (fleet/fix-miscreg)

Both regressions came from the SAME root pattern: PERF.UPDDEL memos keyed on
MAIN's schema fingerprint (DMLExecutor.schemaFingerprint) guarding inputs that
aggregate over ALL database contexts (e.ctx.Databases()). MAIN's fingerprint
stands still across ATTACH/DETACH and attached-schema DDL, so the memo served
verdicts computed before the attached schema existed or after it changed.
Commits: BUG1 = 661902335 (L2-L4 loaded-trigger walk memo), BUG2 = 76e92640a
(L5 allTableIndexes one-slot memo).

- **BUG1 triggerupfrom-2.4**: `ATTACH 'test.db' AS yyy; SELECT * FROM t1;`
  must report `malformed database schema (tr3)` (trigger in yyy references
  main.* objects unresolvable in the new main). The vlt memo skipped the
  loaded-trigger walk because MAIN's fp was unchanged by the ATTACH. Fix:
  walk memo keyed on databasesSchemaStamp — commutative fold (splitmix64
  finalizer + sum) of every context's fingerprint plus the context count.
  Sum (not xor-then-multiply chains) because the databases map iterates in
  random order; a fold whose result depends on order makes the stamp
  nondeterministic and the memo useless.
- **Validated-trigger marks are a schema-LOAD property**: first fix cleared
  the marks whenever the stamp moved — that re-runs body validation at DDL
  time and trigger2 fails with `no such table: rlog` (a trigger referencing a
  table the test recreates between sections errors at the wrong moment;
  SQLite validates a loaded body once, at schema load). Correct model: key
  the marks by the owning schema.Manager INSTANCE (pointer) + trigger name
  (exectrigger.ValidatedTriggerMark). Fresh ATTACH opens a fresh manager →
  re-validates; DDL keeps the manager → marks survive; DETACH/re-ATTACH of a
  different file under one schema name cannot inherit verdicts.
- **BUG2 backup-2.x**: backup into an ATTACHed POPULATED destination (rows>0)
  whose page size ≠ source. Dest populate-phase INSERTs filled the
  allTableIndexes slot with bak.i1's defs (old root page); the backup's DROP
  TABLE + CREATE TABLE moved only bak's fingerprint, so the copy-phase INSERTs
  reused the dropped index's root page and wrote index cells into freed pages
  → later reads: `database disk image is malformed`. pgsz==1024 and rows==0
  combos passed by luck (allocation patterns / cold memo). Same fix:
  databasesSchemaStamp.
- **Bisect note**: the "pre-R4 baseline" f42fa5da0 sits ON the upddel branch
  (post-L5 lessons commit) — it already contains the R4 work. The true
  pre-R4 base is the FIRST PARENT of the merge (688d2a149^1 = b8f6bae1c).
  Merge-commit tranches bisect on ^1, never on the branch tip's tail.
- **Generated-test debug trick**: transpiled testgen failures print only
  got/want; add a temporary DEBUG line into the generated _test.go (git
  checkout -- afterwards) to surface harness-local variables (which combo
  shape failed) plus engine-side detail like Backup.ErrMsg().
- **Bench harness**: /tmp/perf/frigo was lost to tmp cleanup again; rebuilt
  as /tmp/perf/miscreg (+ miscregmain with the replace flipped to main).
  Only interleaved same-harness branch-vs-main ratios are meaningful.

## PERF.ARENA — statement-scoped allocation elimination (fleet/perf-arena, 2026-10-05)

Mission: cut per-statement allocations via statement-scoped structure reuse.
Base 91c5be1be; branch fleet/perf-arena. All measurements paired interleaved
(main binary vs arena binary, same /tmp/perf harness script, 3 reps) —
absolute numbers drift with machine load from sibling agents; only the
paired ratio is signal.

- **Census first (alloc_space, not just cum CPU)**: point SELECT was ~1.46KB
  engine allocs/stmt, literal INSERT ~234B, point UPDATE ~1.46KB. Top
  classes: per-statement affinity name-map churn (~270B point), the internal
  control-flow &Result{} markers (2-3 obj/stmt update), the runSQLText
  single-statement result copy (124B, ALL shapes), the OR-index planner's
  pre-analysis on every WHERE'd SELECT (60B), the point-fetch decode buffer
  + StructRow + dead IPK rowid wrap (~140B point), the DML seek-plan struct
  + equality-side name set + UNIQUE/PK column scan (~110B update/delete).
- **No-aliasing discipline that worked**: per-selectDepth (execquery) /
  per-execDepth (execdml/exec) slot arrays; reset-on-acquire (clear(map)
  keeps buckets; *r = Result{} bulk-zero; decode buffer re-nils every
  element because the column-targeted decode writes only its slots);
  consumption-before-release (every converted site's value is dead before
  the next same-depth acquire — checked for Error and dropped, or copied
  out at the statement boundary); nested statements always take a deeper
  slot, so an enclosing statement's in-flight state can never be clobbered.
  Engine.ExecDepth exposed on DMLContext for the execdml slot index
  (execquery already had selectDepth). Pins: frigolite_arena_pins_test.go
  (six nesting pins, -race clean).
- **W-ladder (each measured before commit)**: W2 OR-gate whereHasOrConjunct
  (+4% point); W3 affinityCollector slots (+4% more point); W4/W5 seek
  scratch (values/StructRow/analysis/conjuncts) (+~1%); W6/W7 execdml
  result/plan/outerCols/uniqCols slots + pushUpdateSetColumns outermost-only
  buffer + snapBufs (+8% update, +3% delete); W8 single-statement result
  pass-through (+5% point, all shapes); W9 alloc-free exprHasSubquery walk +
  conjunct store-back + dead-IPK-fill gate (stable point, +2-3% scan).
  Cumulative: point 604k→681k ops/s (+13%), update 447k→490k (+10%), delete
  645k→672k (+4%), insert ~674k→684k (+1.5%), scan 38M→39.8M rows/s.
- **runSQLText copy subtlety**: the fold's single-statement branch passes
  Rows/Columns through verbatim, so `out := *last` was a field-for-field
  duplicate; returning `last` directly (with the zero-rows→nil Rows
  normalization done in place) preserves the public contract because the
  public boundary copy happens in execResult/DB.Query either way.
- **Full JSON suite is RED on main @ 91c5be1be (~4454 failing subtests with
  fixtures copied)**: types3-1.1 (`SELECT typeof(:V)`) etc. fail on main in
  isolation AND in the full run — pre-existing. The practical gate is
  failure-SET parity: comm both directions vs main_fail.txt; the flaky band
  (alter/temptrigger/trigger1-10.x/e_update/e_blobopen/incrblob/vacuum/
  pragma2, TestP8IncrVacuum3OracleSequence, TestRtreeStressChurn) shifts
  between runs on BOTH sides under machine load. Verify any suspicious
  subtest serially, then on the main checkout, before believing a
  regression.
- **Deferred (real cuts, out of scope or structural)**: btree.NewBTree
  wrapper per statement (129B point/stmt) — needs a btree-side Reset-on-
  Close; storage.ParseRecordHeader + DecodeRecord/decodeValue (~230B update,
  ~220B point) — storage pkg not in this tranche's scope; public *Result
  wrapper (78-210B/statement, the biggest remaining engine-side alloc) —
  needs an API change (value Result or pooled caller contract); INSERT
  literal triple-handling (scanNumericLiteral → int64Text template write →
  evalNumericLit box, ~67B/stmt) — needs a parsed-value cache on the
  template slot path; columnNamesMemoGet hit-path copy (19B) — public
  ownership.
- **Harness protocol**: /tmp/perf/arena (copy of frigo harness, go.mod
  replace → worktree, profile outputs renamed arena_*); always `go version
  -m <bin> | grep '=>'` before trusting a number; measure.sh runs main and
  arena interleaved per phase so ambient load cancels.

## PERF.BTREEUSE — ownership-token btree wrapper reuse (fleet/perf-btreeuse, 2026-10-05)

Mission: stop the per-statement BTree wrapper allocation (129B/stmt + init,
~0.95 wrappers per point SELECT, 190,083 of 5.89M alloc objects = 3.23% of
the point-phase census). Base main 8375084d3 (includes PERF.ARENA).

- **The first wrapper pooling failed for a STRUCTURAL reason; the fix is a
  token, not more discipline.** Zero-arg `Close` can never distinguish "my
  double close" from "the new owner's state": any scheme where re-arm
  clears an open/closed flag lets a stale second Close proceed against the
  live successor (this is exactly what removed the global sync.Pool
  attempt — PERF.FLOOR). The workable design: the ownership token lives
  with the OWNER. `BTree.gen` bumps on every Reinit; the tracker records
  `TreeLease{Tree, Gen}` at acquire; release re-checks gen BEFORE Close
  and no-ops on mismatch. Stale releases become provably dead code paths,
  not careful assumptions.
- **Scope the free list to ONE owner and ONE goroutine; Put only where
  ownership is provably dead.** The engine's statement funnel
  (releaseStatementTrees) and the DML executor's write-tree cache are the
  only Put sites — both single-goroutine, both putting wrappers they
  closed themselves in the same call. A wrapper that is Closed at funnel
  entry (function-local `defer tree.Close()` fired mid-statement) is
  NEVER Put — its late-Close hazard stays the pre-pooling story (closed
  forever, GC reclaims). Engine-scoped lists also mean wrappers never
  migrate across connections, unlike the old global pool.
- **Reinit must have fresh-NewBTree PARITY, not fresh-NewBTree+more.** The
  first draft invalidated the append-cursor slot on Reinit; a fresh
  wrapper never does (initFrom doesn't touch quickAppendReg) — the slot
  is keyed by (pager, root) identity, its trust re-derives from page
  truth on every engagement, and the extra invalidation cost a registry
  lock per acquire for nothing. Rule: a re-armed wrapper must be
  observably identical to a new one, including what it does NOT reset.
  Buffers with capacity (cursorsArr, insScratch, quickPageScratch) are
  kept — same discipline as the cursor pool's kept path/buffer capacity.
- **Close's two registry critical sections merged into one** (append-slot
  drop + owned-cursor unregister under a single cursorRegMu window): same
  semantics, one lock pair less per statement teardown. Also: removing a
  registry entry leaves the (now empty) slice in the map, so the next
  statement's register appends into existing capacity — no bucket churn.
- **Layout-replacement discipline**: the pager layout hook now purges BOTH
  free lists (engine's + DML's) in addition to the cached-write-tree
  drop. A pooled wrapper's snapshot geometry (pageSize/usableSize) is
  stale after an in-place layout change; Purge is cheaper than proving
  which pagers the change touched (Reinit re-snapshots anyway, so this is
  belt-and-suspenders with a hard cap: maxTreeFreeList=64).
- **Results (paired interleaved medians, 7 reps, loaded fleet box —
  absolute levels ~20% below quiet-box; only paired ratios are signal)**:
  point +2.7%, update +5.0%, delete +1.6%, scan +1.0%, insert/group ~0%
  (insert's cached insTree never misses identity in steady state — the
  free list only pays on identity churn, where it now also avoids the
  alloc). Alloc census: btree.NewBTree 190,083 objects → ZERO in the
  point-phase profile. vtabD-1.3/1.4/1.5/1.8 harness subtests fail
  IDENTICALLY on main@8375084d3 solo (pre-existing; failure-set parity
  vs main: worktree set is a strict subset — only the documented
  TestP8IncrVacuum3OracleSequence flake differs).
- **Gates passed**: full go test ./... failure-set parity vs main; testgen
  capi2/btree01/savepoint2/update + FRIGOLITE_TEST=btree01 solo;
  TestSOLID_ + TestSeekSaved; -race: TestPoolStressConcurrentOpenCloseDDL
  (4 conns x open/close x DDL x page-size VACUUM layout churn — the exact
  scenario that killed the first pooling) + the WAL/thread concurrency
  natives; generation-stale-Close pins (TestTreeFreeListGenerationLease,
  TestTreeFreeListPurge, TestReinitFullReset). engine.go shrank 1027→983
  (funnel moved to stmt_btree_funnel.go) — do not grow files already over
  the soft target.
- **Remaining point-SELECT wall (profile, out of scope)**: harness-side
  render/strconv (~40% of census), execquery bareRefSeekOutput/
  columnNamesMemoGet (~2+1.6 obj/stmt), pager ParsedBTree memo atomic
  loads, and the seek descent itself. btree-scope per-statement tax after
  this tranche: free-list Get+Reinit (no alloc, no lock), one registry
  lock pair at register + one merged pair at Close, cursor pool
  Get/resetFor. The 720k point target needs execquery/parse-side work.
## PERF.STORAGEDIET (2026-10-05, fleet/perf-storagediet @ 8375084d3) — per-row record-decode alloc cuts in internal/storage

- **ParseRecordHeader's stack buffer never was on the stack**: the returned
  `serialTypes` slice aliases the local `[16]uint64`, so escape analysis
  moves the array to the heap on EVERY call (`-gcflags -m`: `moved to heap:
  stackSerialTypes`) — 128B/point-seek. A returned slice can never keep its
  backing array on the caller's frame; "stack buffer + return" is always a
  heap alloc. The fix is a caller-owned buffer: `ParseRecordHeaderInto(data,
  buf)` appends into `buf` and ParseRecordHeader wraps it with nil.
- **`DecodeRecordValuesInto` fuses the point decode**: header varints parsed
  inline into a NON-escaping stack buffer (no return → stays on stack, ≤16
  cols zero heap; append spills beyond, parse identical) + the
  DecodeRecordValuesFromTypesCols fill verbatim, returning the stored-column
  count (the two-call form's `len(serialTypes)`) for phase-one row assembly.
  Value start = the post-header-loop pos, NOT `int(hdrSize)` — a final
  header varint can straddle hdrSize and land dataStart past hdrEnd
  (ParseRecordHeader's exact dataStart). Contract parity pinned by
  TestDecodeRecordValuesInto* (equivalence, selection, corrupt: oversized
  header errors, unknown type/truncated values stop early WITHOUT error
  with the full count — the two-call form's behavior, which fetchSeekStructRow's
  malformed-record fallback and partial-fill semantics depend on).
- **Point-seek plumbing**: fetchSeekStructRow now calls
  DecodeRecordValuesInto(payload, values, decodeCols) — per-point allocs
  drop ~145B/query (point profile: DB.Query cum allocs 134MB→105MB for
  200k point queries; ParseRecordHeader leaves the top alloc sites).
  Paired-interleaved point: +5.2%/+1.8% median across two 7-rep runs and
  +1.6% median / +4.0% mean (6/9 wins) in a point-only 9-rep run —
  17/23 paired wins overall; ambient fleet load makes single runs ±10%
  (observed main reps 435k-549k) and compresses the relative gain, so
  pairwise medians are the only trustworthy number.
- **Narrow-table decode gate re-measured post-arena, still loses**: dropping
  `len(colDefs) < 4` to `< 2` in seekDecodeCols adds a per-query
  `make([]bool, n)` for exactly the tables that amortize nothing (2-col
  point: the only other column is the IPK alias, whose stored NULL costs
  zero bytes). No win → reverted; the gate stays at 4.
- **indexRecordRowID** (index-seek walk) decoded the FULL record to read the
  trailing rowid; now parses the header into a non-escaping stack buffer,
  skips leading elements by length, DecodeSerialInt64's only the last
  (SerialZero/One spelled out — decodeValue boxes them as int64 and the old
  unwrap accepted them). Corrupt contract narrowed but equivalent at the
  rowid read: malformed header, unknown type, value overrun, empty list,
  non-integer tail (floats included) → ErrIndexRecordCorrupt.
- **readOverflow left alone (lever 4)**: its `make([]byte,0,PayloadLen)`
  result payload is RETAINED by callers (cells/rows escape), so cursor-scope
  buffer reuse would alias retained rows without a copy-on-retain audit of
  ~15 call sites; zero overflow traffic in the bench shapes. Not paid.
- **Deferred (still the biggest remaining engine-side allocs, all outside
  this tranche's scope)**: execdml collectPointUpdateRow's DecodeRecord
  (9MB/200k update+delete) — execdml frozen here; execResult row wrapper
  (32MB/200k point); bareRefSeekOutput 1-row slice (8MB); NewBTree
  per-statement handle (33MB) — needs btree-side Reset-on-Close;
  harness-side render/Sprintf excluded from engine accounting.
- **Updated deferred note**: the arena tranche's "storage
  ParseRecordHeader/DecodeRecord ~220B point" item is now PAID for the point
  seek (this tranche); the remaining decode mass sits in execdml's
  DecodeRecord call sites and the boxed-value API itself (unavoidable
  through []interface{}).
## PERF.LITCACHE — INSERT literal triple killed via slot-path value stash (fleet/perf-litcache, 2026-10-05)

Closes PERF.ARENA's deferred "INSERT literal triple" (scan→format→re-parse,
~67B/stmt): the template slot-path apply rewrote literal TEXT per hit and
execution re-parsed it (evalNumericLit box on a cache that writeSlot must
clear every statement).

- **Mechanism**: `templateSlots.tupSlot` records which slots are INSERT
  VALUES-tuple items (collector knows (tuple,item) via the sfInsTuple path
  terminal). apply() writes the parsed value into the live clone's
  `sql.InsertStmt.InsLitVals` (parallel to Values; nil for non-literal items
  like NULL/column refs) IN THE SAME PASS as the text rewrite — text and
  stash can never diverge. execdml's evalTuple/evalTuplePooled read non-nil
  stash entries instead of EvalExpr: re-parse, boxing AND the dispatch walk
  disappear. evalNumericLit vanished from the insert alloc profile (was #1).
- **Kind safety is inherited, not re-proven**: only slot values that passed
  validateValues (int64 digit-slot, float64 'g'+".0", string) are stashed,
  and those are exactly what EvalExpr would return for the rewritten node —
  the stash is the same truth, one copy earlier. Refusals (negative folded
  minus, hex, 2^63, blob, expressions, RETURNING literals) keep templates
  COW-only → full parse → no stash (parity corpus proves all of these).
- **First-build parity**: trySlotPathLive's COW-built clone gets the stash
  via slots.stashValues (no node rewrite — COW nodes already carry the
  values' text); a false there (unreachable) discards the clone → COW path.
  COW walker MUST reset InsLitVals=nil (recycled-tenant full-overwrite rule
  — insertStmtValues assigns every field).
- **Paired-diet levers that paid alongside** (all measured interleaved vs
  main): pkRowIDFromColumn int64 fast path (NUMERIC affinity never changes
  an int64 — skip ApplyColumnAffinity); applyColumnAffinities over memoized
  per-column affinity classes (BLOB/none class 0 skips the wrap; memo keyed
  fingerprint+colDefs-identity like columnIndexFor); fillIPKRowID via
  memoized IPK index; databasesSchemaStamp over a cached filtered db-list
  (refresh on map-length move — ATTACH/DETACH always moves len; a
  same-length context replacement needs DETACH+ATTACH in ONE statement,
  which the engine never runs). ~100ns/stmt of map iteration alone.
- **Results** (paired interleaved, mission env, this box): insert 749-762k
  vs main 632-639k ops/s (+18.5-19.3%; target ≥750k met), point +4-12%,
  update +6.5-10.4%, delete +5.7-9.1%, scan/group parity.
- **Pins**: TestInsLitParityCorpus (13 shapes × fast-engine slot path vs
  full-parse control — control uses round-odd UPPER-CASE table spellings:
  same table for SQLite, different template key bytes → control never
  template-hits while accumulating identically; single control engine per
  shape, not per-round, or accumulator shapes like upsert diverge) +
  white-box TestPinSlotPathInsertStash/MixedSlotsNoStash/StashValuesFirst
  Build; COW parity pin neutralizes InsLitVals before its deep-equal (the
  stash is slot-path-only by design).
- **Harness flake note (updated)**: TestSQLiteSuite at full GOMAXPROCS
  (14 cores) fails ~377 subtests with cross-file "already exists" pollution
  — IDENTICAL count on main; GOMAXPROCS=4 is green on both. Parallel file-
  backed ATTACH races (cleanupTestDBFiles vs t.Parallel), not engine bugs;
  gate the suite at GOMAXPROCS=4. Also: zsh does not word-split `env $E`
  — bench env vars must be passed explicitly.

## PERF.LITCACHE — finisher session: bind-path stash, oracle-parity fixes, adjudications (2026-10-06)

Completes the slot-path stash across the PREPARED-STMT (bind) path and fixes
two oracle gaps the new pins exposed. Commits 1ebf195ce/23ac19099/a24c338b6
(+ f1e4b86a1 lessons, edf6e06b0 bind stash, b7e101812 REPLACE triggers).

- **Bind-path stash**: `bindStashFor` readies the recycled INSERT tenant's
  `InsLitVals` (reuse when the shape matches — allocate-once like the
  template path's stashTarget), `insertValue` (3-value form) fills EVERY
  entry per substitution (nil for non-param items) — full overwrite, no
  stale. Kind gates mirror the slot path: int families/finite
  floats/strings stash verbatim (serve the caller's interface word —
  re-boxing costs an alloc/slot/exec); blob/NULL/NaN/uint64>MaxInt64 stay
  evaluated. TestStmtRepeatExecNoReparse: bound 336→204-209 B/op vs
  literal 261-266 — the prepared path now BEATS literal.
- **In-place literal rewrite is UNSAFE on the bind path** (tried, reverted):
  rewriteSameKindLiteral(prev) corrupted SHARED AST nodes — a recycled
  tenant's slot can hold a template/Stmt-shared literal kept by a previous
  non-param substitution, and a later same-kind bind rewrite mutates the
  shared node (observed: q5's literal `1` read as 100001 after qa reused
  the tenant). The template slot path is safe because its live clone is
  per-(entry,depth) and never shared; bind tenants rotate across shapes.
  Fresh nodes per bind stay mandatory.
- **fillIPKRowID returns ipkIndex -1 for an EXPLICIT IPK** — its return
  cannot distinguish explicit from no-IPK downstream. pkRowIDSource now
  returns (rowid, explicit, err) decided PRE-fill; fireInsertRowBefore-
  Triggers gates the post-BEFORE-trigger rowid re-allocation on !explicit
  (oracle 3.51: explicit ids are stored as given; a BEFORE trigger
  consuming the next rowid still pushes the AUTO row up).
- **INSERT OR REPLACE fires NO delete triggers with recursive_triggers OFF**
  (insert.c OE_Replace — the UPDATE OR REPLACE path already gated this;
  the INSERT path didn't). Oracle: REPLACE of a child-referenced parent
  SUCCEEDS (FK counter nets out in-statement) with side tables empty, and
  with recursive_triggers=ON the delete triggers fire exactly once.
  TestPerfStmtJournalReplaceFKTriggerRollback rewritten to those oracle
  end-states (it had pinned the accidental pre-fix behavior: explicit id
  re-alloc made the child FK check fail).
- **Gate adjudications**: TestSQLiteSuite full-suite fails ~4443-4450
  subtests at ANY parallelism on base AND branch (pristine git-archive
  export of 8375084d3 identical) — pre-existing rot amplified by
  cleanupTestDBFiles racing t.Parallel files; rotating solo-green flakes:
  TestWindowCGroupConcatBlobUTF16, TestP8IncrVacuum3OracleSequence,
  TestNativeThreadConcurrentWritersSerialize. FRIGOLITE_TEST is a
  Contains-match (probing `8_3_names` also runs `f_8_3_names`). The
  allocs/op pin measures BYTES (TotalAlloc), not objects. Live-main
  checkout re-confirmed as non-baseline; pristine export in
  /tmp/perf/basecheck is the reference. Quality-gate hard fails (engine.go
  1027 lines etc.) are pre-existing at base; branch files pass
  gocognit/gocyclo/staticcheck.
- **Bench**: paired interleaved 3 rounds (mission env) — see final report;
  run scripts /tmp/perf/litcache/paired3.sh. zsh: do not `env $E`
  (no word-splitting) — pass env inline.

## R8.INSERT (fleet/r8-insert, 2026-10-06)

- **The per-statement external-file validation was THE insert cost** —
  execEntry ran execDBFileChecks (pager fstat + 16-byte pread) on every
  outermost statement: 70% of insert_xact CPU and 38% of its allocs
  (os.File.Stat FileInfo boxes) at 600k rows. SQLite re-runs
  sqlite3PagerSharedLock + lockBtree ONCE per transaction: vdbe's
  OP_Transaction is a no-op when pBt->inTransaction already matches
  (btree.c sqlite3BtreeBeginTrans early-return). Fix: txState.fileChecksDone
  latches after the open transaction's first clean validation; cleared at
  BEGIN / implicit-SAVEPOINT tx start / COMMIT / ROLLBACK / implicit-release.
  A FAILED validation must NOT latch (next statement re-reports the
  corruption, like C's repeated OP_Transaction failure). Autocommit keeps
  per-statement checks (each statement is its own transaction). +38% insert.
- **Stashed bind slots must never build their literal node** — a VALUES
  tuple slot whose value passes bindStashValue is never evaluated (evalTuple
  reads the stash), yet bindLiteral still paid FormatInt + node alloc per
  slot per Exec (29% of insert allocs). bindParamTuple (VALUES tuples ONLY)
  returns a shared immutable placeholder (bindStashNode, a NullLit); ALL
  other parameter positions (upsert DO UPDATE SET, RETURNING, WHERE) keep
  bindParam's real node — the testgen corpus caught the first draft
  sentinel-ing the upsert-assignment position (fast=NULL, ctrl='U13').
- **Two execdml consumers evaluated VALUES nodes outside evalTuple** — the
  explicit-rowid scan (INSERT INTO t(rowid,...) / IPK column list, shared by
  the echo vtab pre-check) and the echo write-through rowid validation. Both
  now read the tuple stash when non-nil; kind parity pinned in
  TestInsBindRowidStashPin (int lands exactly, NULL auto-assigns, float/text
  raise 'datatype mismatch', echo vtab covered).
- **Per-txn schema external-mod reset** (execResetExternalChecks /
  checkExternalMod checkedThisStmt) still Preads the change counter once per
  statement via the first schema lookup — NOT cut in R8 (riskier: mid-txn
  same-process second-connection commits are observable without POSIX
  locks); candidate for a later tranche with lockreg-aware gating.
- **Remaining insert costs are OFF the insert-agent scope** (report items):
  (1) pager flushPage issues os.File.Truncate PER flushed page at COMMIT
  (24% of remaining CPU; C truncates once via bDoTruncate) — pager owner;
  (2) pager allocateExtendLocked 23% of remaining allocs; (3) btree
  splitLeafMulti/writeSplitPartitions/encodeCellScratch ~30MB/600k; (4)
  public *Result 128B/stmt (53% of remaining allocs) is caller-owned and
  off-limits by design; (5) runtime.madvise 26% of remaining CPU = GC
  pressure from pager/btree page caches.
- **Harness reconstruction**: /tmp/perf/frigo did not survive; rebuilt at
  /tmp/perf/r8ins (two module dirs, replace → main vs worktree, PHASE env,
  CPUOUT/MEMOUT pprof hooks, `go build -mod=mod`). Paired interleaved
  main-vs-worktree on ONE scratch binary pair is the protocol; positional
  args are NOT PHASE — pass PHASE=insert as env.
## R8.POINT — point-SELECT tranche (fleet/r8-point, 2026-10-06)

Paired interleaved vs main @32253d08d (own scratch harness /tmp/perf/r8pt,
3 rounds alternating run order, machine under sibling fleet load — only
deltas mean anything): **point +20.1%** (581,076 -> 697,595 medians ops/s);
insert +1.8%, update +2.5%, delete +2.6%, scan +0.7%, group ±0, file +2.9%
(all noise-or-positive; C5/C7 also serve scan/update/delete paths).
alloc_space per point statement −33% (412MB -> 275MB per 600k queries incl.
setup; engine-side ~500B -> ~330B/stmt). Commits: 7e066429e (C1 fusion),
32180dc3d (C2 scaffolding), 8ff2cd2ca (C3 collations), 635bc74d1 (C4 result
pool), 6880d90c6 (C5 parse memo), 9d61e22ad (C6 raw rowid), 88095aea1 (C7
agg-walk fast path).

- **Output-projection fusion is the big point win (+7.6%)**: the all-bare-ref
  projection evaluated every column through the expression walker although
  the value IS the StructRow slot. `bareRefsSeekOutput` + a
  (columns-slice, colDefs-slice, fingerprint) memo of resolved slots kills
  the walker per statement. Critical semantics: resolution must mirror
  StructRow.Get EXACTLY — exact declared-name first, then case-insensitive,
  and LAST same-named colDef wins (map-overwrite semantics: frigolite ACCEPTS
  duplicate column names at CREATE, unlike sqlite3's "duplicate column
  name"). IsRowIDName refs keep the generic route (Get answers them from
  StructRow.RowID, not a slot).
- **Dead-affinity gating (+9.9% with the IPK-fill diet)**: on the covered
  bare shape (seek bounds cover every WHERE conjunct, no row maps, no feed,
  every output col passes skipBareSelectRef) the affinity-reference walk
  CANNOT influence anything — the WHERE never re-evaluates and every output
  reader peels the wrapper. Gate it to nil instead of walking. The covered+
  bare IPK fill read the projection refs directly instead of rebuilding the
  recycled collector's name map; fill indices live in a per-selectDepth
  scratch consumed immediately by fillSeekRowPhaseOne.
- **Collations have exactly three consumers** (DISTINCT, compound merge,
  ORDER BY): finalizeSelectResult computed selectOutputCollations
  unconditionally; a statement with none passes nil (C3, ~+1.5% with C4).
- **Pooling the execquery.Result STRUCT per selectDepth (+5%)**: ~160B/stmt
  of the point path's allocs was the result struct. Contract that makes it
  safe: (a) every nested execution (compound member, subquery, view body,
  trigger stmt) runs through its own execSelect at a STRICTLY deeper
  selectDepth; (b) the public boundary (DB.Query/DB.Exec/Stmt) copies fields
  SYNCHRONOUSLY; (c) Rows/Columns ARRAYS stay fresh per statement (the
  scanbox pin). Trap found by the suite: execValuesGroup held the head
  member's result struct ACROSS later same-depth execSelect calls — carry
  rows/columns in locals and rebuild after the last member. Full-suite
  adjudication: the non-solo TestSQLiteSuite failure band (4463 cases on
  main vs 4455 on branch, identical file profile) is PRE-EXISTING
  shared-cwd drift; per-FILE solo (FRIGOLITE_TEST=^file.json$) passes on
  both branches and is the only deterministic gate.
- **The table seek was not taking the page-parse memo (+2.0%)**: the index
  seek's readTreePage used Page.ParsedBTree (validated byte-compare memo)
  but seekTableLeafWithPath re-parsed + re-validated each level with a fresh
  ParsePageInto. One-line switch; header is memo-owned read-only, which is
  all seekInLeafTable/routeInteriorTable do with it.
- **Wrapper-peeling argument legitimizes raw fills (+4.6%)**: on the
  targeted covered+bare fill, EVERY consumer peels (fused slot read,
  appendOutputExpr) and the covered plan compares nothing, so the alias
  slot takes the raw rowid; the every-alias fill (uncovered WHERE, row
  maps, non-bare projections) KEEPS wrapAffinityCollated (WHERE re-eval
  needs the affinity wrapper for comparisons like id > '4').
- **Gate fast paths that read like a walk can cost more than the walk**:
  hasSubqueryWithCorrelatedAgg's closure walk measured only ~13ns/call
  (5 asks/stmt); the bare-projection pre-check measured +0.2% (noise).
  Profile before assuming the closure is the cost — it usually isn't.
- **Measurement discipline on a contended machine**: main's own median
  swings ±15% between sessions (548k-658k). Session-over-session deltas of
  the SAME branch are meaningless; only same-session paired interleaved
  medians (alternate run order per round) decide. Keep a second scratch
  module + detached worktree of the PREVIOUS commit to A/B a candidate
  against its actual predecessor in one session.
- **Remaining point-path floors (report-only)**: public Result (~107B),
  fresh output row + rows slice (~73B), fresh Columns names copy (~27B),
  decode boxes (~28B) — all caller-owned. Parse (~16% CPU) and the literal
  substitution (exec.nextLiteral) are outside this tranche's scope
  (template/clone + parse owned elsewhere). Pager exposes no cheap dirty
  generation — the validation-based ParsedBTree memo makes one unnecessary.

## FIX-R8POINT — result-pool frame collision (fleet/fix-r8point, 2026-10-06)

The R8.POINT result-struct pool (pooledSelectResult, selectDepth-indexed)
broke rtree vtab content: INSERT INTO rt2 SELECT * FROM t2 iterates the
source SELECT's rows while every rtree xUpdate issues shadow SQL through
engineVtabDB.ExecSQL (d.e.Exec) — a nested engine.Exec that restarts
selectDepth at 1 and took the SAME pool slot, zeroing the source result
struct under the write loop. The rtree kept ~2 of 10001 rows; every later
rt2 read (plain scan AND MATCH breadthfirstsearch) served the truncated
content as rowid garbage ([448] = a stray rowid from the truncated walk).
Census: rtreeE 1071/2/290 vs 1073/0 at base. Fixed in 148fee27f by keying
the pool per statement FRAME (the engine's execDepth, SetResultFrame on
every Exec entry, re-tagged to the enclosing depth in execDepthLeave) —
nested executions land in their own frame; the enclosing statement keeps
its slot across the whole nested Exec.

- **Pooling rule, restated**: a pooled object may only be reacquired when
  every possible still-live consumer has copied what it keeps. selectDepth
  alone does NOT bound a result's lifetime — consumers may hold it across
  NESTED engine.Exec calls (insert-select source rows during vtab shadow
  writes; the same shape as any UDF-driven write loop). Key caches by
  (statement frame, nesting depth) when the object can outlive a nested
  execution.
- **DML Results are NOT pooled** (fresh &Result{} in execdml) — that is why
  fts-optimize's levelsRes-held-across-INSERT pattern never collided and
  why the fix needed no execdml changes.
- **Bisect discipline**: a single testgen run per SHA produced a WRONG
  boundary (flagged C1); two runs per SHA pinned it exactly at the pooling
  commit. Under sibling-fleet load, treat single-run verdicts as noise —
  the same flake band applies to tests, not just benches. A minimal
  standalone repro (file-backed db, PRAGMA page_size=512, bulk
  INSERT-SELECT into the rtree, then MATCH) turned a 11s/iteration testgen
  bisect into a 5s/iteration probe and made the corruption visible
  directly (plain scan saw 2 rows — no MATCH machinery involved at all).
- **Read the failure backwards**: got [448 0 0 0 0] for SELECT * on the
  MATCH query was the give-away — zeros for every coordinate mean the
  ROWS THEMSELVES were gone, i.e. a WRITE-path loss, not a query-path
  misprojection; "which fused path engaged on vtab" was the wrong question.
- **total(x) returns float64** (SQLite REAL aggregate) — a pin asserting
  int64 via type assertion reads 0; use sum(x) for integer totals in pins.

## R9.POINT — point-SELECT close-out tranche (fleet/r9-point, 2026-10-06)

Branch @ b3b3ef189 (commits 2a091c8eb shape-stable identity, 2cb4fd18b
flush/clock/shape-memo, 510aabe73 btree probe loops, b3b3ef189 trims).
Paired interleaved vs main @830dcd76b (binaries /tmp/perf/r9pt{,-main},
same session, alternating): **point ≈ 1.20× main** (main 754-768k vs
branch 864-922k ops/s medians; mission-day main baseline 775,664 at
1.39× sqlite3 1,082,134). Same-session single-branch bands swing ±15%
(thermal) — session-over-session absolute numbers are noise; ONLY the
paired interleaved ratio decides.

- **Template-clone identity has exactly two stable forms** — the
  prevalidate memo (R8) silently relied on one and could alias: the
  slot-path LIVE clone (one persistent SelectStmt per (template,
  execDepth), literal leaves rewritten IN PLACE → pointer stable) and the
  retained exact-text/fresh-parse forms (retained by the cache). The COW
  scratch clone (multi-statement templates, unsupported slot shapes)
  recycles per-depth structs ACROSS TEMPLATES — LIFO free-list rotation
  hands template B the struct template A was memoized under. Reproduced:
  two bad templates with different abort depths + a good one shuffle the
  free list so a valid `SELECT c FROM t WHERE id=1; SELECT 6` inherited
  "no such column: badx". Fix: Engine.setStmtShapeStable published by the
  producing prepare path (prepareCached entry=false; true on stmtCache
  hit / fresh parse / trySlotPathLive hit; BindStmtValuesScratch true only
  when nothing substituted); EVERY pointer-keyed verdict memo gates on it
  (prevalidateMemo, pfAST slot, pfDML slot — the pfDML gate was a latent
  second instance). Unstable statements walk directly and never enter a
  slot. Pin: TestR9PointShapeMemoStableIdentity.
- **Memoize the SHAPE, re-resolve the VALUE**: the rowid-seek plan's
  structure (single equality conjunct, which side is the rowid ref,
  covers) is template-constant; eqRowid/eqMatch/planned are literal-
  value-dependent (NULL and out-of-range reals match no rowid) and MUST
  re-resolve per statement through selectRowidLiteral. Store the literal
  NODE (stable pointer, rewritten in place), never the resolved int64.
  The census gate (no FuncCall / COLLATE / MATCH / subquery in the whole
  statement) is what makes ValidateExprs/hasAggregates verdicts registry-
  independent — RegisterFunction moves no schema fingerprint.
- **Runtime-dependent verdicts need the same gate at every memo**: the
  pre-dispatch walks consult outerRow/aliasStack/inCompoundMember/
  triggerDepth (correlated subqueries reach execSelect with outerRow
  set); validateSelectPreDispatchCached reuses prevalidateRuntime-
  Dependent, or a subquery memoizes its verdict and the outer statement's
  later hit serves the wrong context.
- **The autocommit flush was three avoidable passes per read statement**:
  bumpChangeCounters + autovacuumDrainIfDirty each walked the attach list
  taking the pager dirty lock (2 mutex round-trips), and
  flushAttachedPagers iterated the databases MAP with a strings.ToUpper
  ALLOCATION per database per statement. One dirty-scan pass (scratch
  slice) now feeds bump+drain; the drain KEEPS its own post-bump walk —
  updateFileChangeCounter dirties a header page, so a pre-bump dirty
  snapshot misses a db that owes the drain (P8 incr-vacuum oracle
  sequence caught it in the first attempt). Reads skip the drain walk
  entirely; flushAttachedPagers iterates dbList with EqualFold name
  checks and returns outright when nothing is attached.
- **SQLite's statement clock is LAZY** (sqlite3StmtCurrentTime computes
  on the first date/time function that needs it): function.BeginStmtTime
  opens the window, currentTime pins on first consumption,
  ClearStmtTime closes it — a statement that never touches 'now' stops
  paying two clock reads + mutex ops per statement. Nested-statement pin
  pollution (inner Exec clears mid-outer) is unchanged from the eager
  form — equally wrong before and after, single-connection exact.
- **The btree probe loops paid a function call per separator**:
  routeInteriorTable/seekInLeafTable addressed probes through
  storage.CellPointer (re-deriving contentOffset + pageSize conversion
  per probe). Hoisting the pointer-array base + page mask (one u16 load +
  mask per probe) plus 1-byte varint fast paths for the leaf payload
  length and interior separator rowid took the paired ratio from ~1.16×
  to ~1.20×. Semantics: corrupt pointer-array indexes now report
  malformed one layer lower instead of slicing out of bounds (strictly
  closer to SQLite than the panic). Watch the ±8 offset when inlining —
  CellPointer's contract is (arg + 8 + i*2); the first draft double-
  counted it and benching a knowingly-wrong build wastes a cycle.
- **Machine noise dwarfs micro-wins**: the SAME commit benched 835k,
  then 670k, then 780k across three sessions (thermal/sibling load).
  Keep TWO scratch modules (replace → worktree, replace → main), build
  both binaries once, interleave A/B/A/B and compare medians only.
- **Suite adjudication (unchanged from R8)**: the full TestSQLiteSuite
  has a PRE-EXISTING failing-file band (378 files identical on main and
  branch; zero branch-only), t.Parallel files race cleanupTestDBFiles
  (`*.db` glob) — TestWindowCGroupConcatBlobUTF16 and TestRtreeStress-
  Churn are full-suite-only flakes passing solo on both branches;
  TestP8IncrVacuum3OracleSequence is randomblob-nondeterministic and
  fails solo on main too. -race harness subtest expectations differ
  identically on main. Gates: solo per-FILE on both branches.

## FIX-R9INS — AUTOINCREMENT sequence writes lost (fleet/fix-r9ins, 2026-10-06)

- **A memo over a LAZY derived cache must not cache verdicts the cache
  cannot yet answer.** tableHasAutoIncrement's fingerprint memo stored the
  FALSE verdict of a colCache walk that found NO entry for the table — but
  colCache is populated lazily by ParseColumnDefs (CREATE TABLE DDL does
  not populate it, and invalidateTableCache/invalidateTableCaches wipe it
  wholesale after ANY DDL). The stored negative then served every later ask
  at the same schema state, and the insert statement-end-hook gate + 
  autoIncStatementSetup (both route through it) never fired the
  sqlite_sequence write: rows stayed empty ("SELECT * FROM sqlite_sequence"
  → [] instead of [t1 12]). Rule: a walk over a lazily-populated cache must
  distinguish "walked and answered" from "found nothing to ask" — only the
  first is cacheable.
- **A fast-path gate must ask a CONSERVATIVE form of the fact it gates.**
  Even with the negative-memo fixed, the first AUTOINCREMENT insert after
  any DDL still dropped its sequence write: the gate ran while the cache
  was wiped (verdict not derivable → false → hook-free fast path), while
  the authoritative check inside the body ran AFTER ParseColumnDefs
  (verdict true) — with no hook parked there was nothing to write through.
  Fix: TableMayHaveAutoIncrement (positive OR not-yet-derivable → hooked;
  known-negative stays memoized-cheap so the plain-table bulk load keeps
  its fast path). Gate asks may-be; the body's authoritative verdict
  decides the actual work. Generalizes: gate = cheap conservative
  predicate, body = authoritative check, and the gate's false must PROVE
  the body's false.
- Debug pattern that pinned it: R9DBG prints at the gate and at the write,
  run against the failing testgen suite — the failing statements showed
  "gate ai=false" while the map-bump path was healthy; the cache-wipe
  trigger was the intervening CREATE TEMP TABLE/DROP TABLE in the TCL
  sequence.
- btree -race full-package runs flake on TestCursorFinalizerSafetyNet
  (finalizer timing under race + 500s parallel load); it passes solo and
  on full-rerun — adjudicate per-test before hunting.

## R10.INSSCAN — INSERT + SCAN close-out tranches (fleet/r10-insscan, 2026-10-07, @ 0ebff88c9)

- **The typed scan lane's per-row residue was the slot WALK, not the
  parse.** After the typed aggregate lane engages, the profile showed
  GetVarint + resolveSlot + SerialTypeLength dominating: every aggregate
  call re-walked the record header's preceding serial types per row. Fix:
  parseRecordSerialTypesOffsetsInto — the header walk doubles as a slot
  span table (per-entry value span relative to dataStart, plus an end
  sentinel), so resolveSlotOffs is one array index. Corruption parity is
  the delicate part: a reserved serial type (10/11) has NO length, so its
  span END and every later span get poisoned (-1); a truncated body is
  not a parse error — prefix sums push the end past len(payload) and the
  per-slot bounds check fails exactly where the walk's pos+n check would.
  Pinned against resolveSlot case-by-case (typed_lane_span_test.go).
- **Boxed indirection per row is measurable even when each hop is
  trivial.** Giving the typed lane its own tight per-page loop in
  runBatch (processRowTyped IS the whole row step under a covered plan)
  plus inlining LocalPayloadSize's no-overflow branch at the cell header
  took scan from 41.8 to 46.7M rows/s — more than the span table itself.
  Rule: a lane that needs zero per-row services should not pay the
  general loop's fold layer.
- **Escape analysis loses on closure-call boundaries: a struct handed to
  a function-valued parameter escapes even when no callee retains it.**
  LeafBatch escaped once per visited leaf page (~344k allocs per
  benchmark) through `fn(&b)`; a per-Cursor batchScratch (assigned before
  each fn call, documented non-retention contract) removed the escape.
  Same pattern as the executor's insCell/insRecBuf reuse.
- **Insert shape memo: revalidation cost is per-CALL; derive cost is per
  MISS.** The insertShapeFor memo hit cheaply, but the VALUES row loop
  called it four times per row (insertOneTuple, insertRow's FTS gate,
  prepareInsertRowValues, writeTableRow) and the FTSTables() probe ran
  per row outside its gate. Threading the shape once per statement
  through insertOneTupleSh -> execInsertRowSh -> insertRowSh ->
  prepareInsertRowValuesSh -> writeTableRowSh (nil-shape wrappers keep
  the cold callers on the memo) was worth ~+5-8% end-to-end.
- **INSERT's remaining gap is floors outside a tranche's reach:**
  execResult's 109B/stmt is the public `Exec(sql) *Result` API shape
  (pooling is unsafe — callers may retain), main.render's 140B/stmt is
  the harness, scanNumericLiteral's 18B/stmt is internal/exec (sibling
  scope), and the exec-layer statement-cache validation
  (findTable/normalizeScan/allSchemasFingerprint ~80-100ns/stmt) is
  likewise off-scope. A DML-side FindTable memo was PROTOTYPED AND
  DROPPED: FindTable is the per-statement external-modification
  detection point (checkExternalMod's Pread), and a memo that skips it
  changes when an external commit becomes visible — SQLite's own
  granularity is per-transaction, frigolite's is per-statement; matching
  that safely needs the detection hoisted to statement entry (exec
  layer), not a caller-side cache. Also dropped: a wrapper-local
  quick-append slot mirror (cross-wrapper invalidation via the registry
  would be unseen — stale maxKey + dupAlreadyDropped could double-write a
  rowid); the registry lock pair (claim/update) stays.
- **A global registry gets a lock-free zero-check before its lock.**
  saveAllCursors locked + map-walked per write; openCursorCount (moved
  under cursorRegMu with the registry) lets cursor-free workloads (pure
  appends) skip the lock entirely. Zero means zero anywhere: conservative
  and correct, and the common case.
- **quality_gate.sh only gates STAGED files (pre-commit); run the full
  script before pushing.** The full run fails on main already (pager
  cookie_cache_test U1000, fts5 decodeStructRec gocognit 66, three exec
  files >1000 lines) — the tranche contract is delta-clean: no NEW
  offenders. R10's own crossings (insert_core.go 1007, btree.go 1038)
  were resolved by splitting the moved/added code into insert_row.go and
  the btree_scan.go cell-decode block instead of shrinking comments.
- **Benchmarking discipline that held:** shared fleet machine — always
  build per-binary (per the R9.INSERT lesson) AND interleave A/B pairs of
  the two BINARIES in one shell loop, take medians, discard spikes only
  when cpu-time (not wall) stayed normal. Baseline binary must be built
  from a SEPARATE scratch dir pointing at main (never rewrite
  /tmp/perf/frigo/go.mod).
- Results (interleaved A/B vs main @ acb9015b9, NROWS=300k): scan
  37.5M -> 47.6M rows/s median (+27%; 1.09x vs sqlite3, gate 45M MET).
  insert 1.016M -> ~1.05-1.09M ops/s (+5-8%; 1.40-1.45x vs sqlite3,
  gate 1.25M NOT met — floors above). point parity, no regressions.
## R10.DML — point UPDATE/DELETE tranche (fleet/r10-dml, 2026-10-07)

- **The statement journal's per-statement cost is the scope open/close, not
  the 4KB capture.** Killing BeginStatement/EndStatement + the before-image
  capture for point UPDATE/DELETE (dmlCanSkipSnapshot extension to
  can't-abort shapes) paid only ~+3-5%: the capture is a pooled-buffer
  4KB memcpy (~100ns) and the scope bookkeeping ~150ns, against a ~1.4µs
  statement budget. Real, but the mission-sized gap (300ns+) does not live
  in the journal. What the skip DOES also remove: markDirtyLocked's
  txn-time dirtyMark map write stays (load-bearing for later scopes'
  memory-vs-fromFile classification) — do not "optimize" it away.
- **Can't-abort = "WHERE contains a rowid-equality conjunct" + clause gates
  + target gates.** The rowid equality bounds matching rows to ≤1 under ANY
  evaluation (rowids unique), which is stronger than the executor's
  point-seek shape: RHS may be a subquery, extra AND conjuncts are fine,
  operand order/unary+/parens irrelevant. Must exclude: OR clauses,
  RETURNING (evaluated after write), ORDER BY/LIMIT/OFFSET, UPDATE...FROM,
  triggers, FK enforcement, FTS content, virtual tables, WITHOUT ROWID
  tables, and tables whose rowid/_rowid_/oid name is a DECLARED column
  (shadowing makes the term value-based, not rowid-pinned — multi-row).
  Decision matrix pinned in internal/exec/dmlabortskip_pin_test.go; the
  INSERT skip's semantics (interrupted row leaves writes visible) stay.
- **GC is NOT the point-op bottleneck — measure before hunting garbage.**
  GOGC=100 vs 300 moved nothing (±3% noise) despite madvise+mheap.alloc
  showing ~25% of CPU samples: that cost tracks the LIVE heap (in-mem DB
  pages + allocator span churn from page-mutating statements), not GC
  frequency. Halving per-statement garbage (execResult public boundary,
  120B/stmt) is expected to pay little; verify with GOGC first next time.
- **Template-cache verdict memos need the shape-stability bit.** The
  prepare-time WHERE/SET resolution walk (validateDMLExprs) now memoizes
  per (stmt pointer, schema fingerprint) behind DMLContext.StmtShapeStable
  (Engine.stmtShapeStable passthrough). Without the stability gate a
  recycled COW-clone address would serve another template's verdict (the
  pfASTStable rule, re-derived the hard way in FIX-R8POINT). Error verdicts
  memoize too (vExprErr); unstable statements recompute WITHOUT touching
  the slot.
- **Lazy argument materialization at gates**: any check with an early-out
  must not receive a freshly built expression slice ([]sql.Expr per
  statement — updateTargetExprs, the delete []sql.Expr{s.Where} literal).
  Build inside a closure/memo (validateDMLExprsVerdictMemo's build func) or
  behind the cheap predicate (validateUpdateAliasQualifier).
- **Paired A/B discipline held**: per-binary stable blocks (3 runs × 3
  blocks per binary, block medians) — single alternating runs on this
  machine swing ±4%. Paired-vs-main: update 643.6k→734.8k (+14.2%), delete
  800.0k→868.0k (+8.5%). Against the mission's stated baseline (different
  machine state): update 682.8k→734.8k, delete 805.7k→868.0k.
- **Where the remaining gap lives (next round's map)**: btree
  Cursor.SeekToRowID descent ~110-150ns/stmt (internal/btree — sibling
  scope); Page.ParsedBTree full re-parse + hdrSnap re-copy on the FIRST
  access after every page mutation (pager+btree cooperation: the mutating
  path keeps its own parse result and could refresh the memo — btree files
  out of r10 scope); the public *Result boundary (85-120B/stmt, one alloc,
  can't pool without caller-retention aliasing); execexpr evalNumericLit
  boxing the WHERE literal per substituted statement (execexpr scope);
  allocator span refill/madvise from page-mutating statements (runtime).
  SQLite-C per-statement floor for this shape is ~0.6-0.8µs; frigolite is
  at ~1.16-1.36µs.

## R11.BTREEMEMO — write-path parse-memo refresh (fleet/r11-btreememo, 2026-10-07, @ 4f0979191)

- **The memo's validation model already made the refresh a 30-line API —
  the hard part was the write paths' OWN contract.** Page.ParsedBTree is
  validation-based (fingerprint bytes on every access), so "refresh" only
  needs: canary the caller's struct against the LIVE bytes field-for-field
  (parsedMatchesData — the parsedMatchesSnapshot counterpart reading the
  page instead of the snapshot), run the same validatePageHeader a fresh
  parse would, then swap snapshot+struct in place (single goroutine per
  pager; buffers reused, zero steady-state allocs). But the btree write
  paths only HALF-kept their "parsed header kept in sync" contract:
  freeSpaceOnPage/freeSpaceExtendContent/freeSpaceLink rewrote
  FirstFree/CellContent/FragFree BYTES without touching the struct, and
  pageUseSlot/compactLeafAfterDelete/finishLeafDelete had the same holes.
  The canary caught every one (decline -> miss -> correct), which turned
  the bugs into measurable pins instead of corruption. Rule: an
  in-place-refreshed memo is exactly as honest as the struct-sync
  discipline of the paths that feed it; keep the canary non-negotiable.
- **R10.DML's #1-lever estimate was right about DELETE, wrong about
  UPDATE.** Point UPDATE (SET c=c+1) mostly takes the in-place same-size
  memcpy path (btree_update_inplace.go) — header bytes unchanged, memo
  stays valid, NO re-parse exists to kill: the update-phase profile has
  zero ParsedBTree/ParsePage samples and the update mem profile has no
  memo allocs. DELETE mutates the header every statement (dropCell), and
  the sequential sweep re-seeks the just-mutated leaf, so every statement
  paid miss+parse+hdrSnap-alloc: main alloc profile showed ParsedBTree at
  20MB/1M deletes. After the refresh: ParsedBTree alloc lines GONE,
  delete-phase heap 74.0 -> 60.2MB (-19%), paired delete +3.4%, insert
  +3.9% (freeblock-reuse inserts also stop declining), update/point/scan
  parity. Mission targets (update >=920k, delete >=1150k absolute) are
  NOT reachable from this lever alone — the remaining delete/update gap
  lives in the exec layer (deleteTableContext 13.5%, planDMLSeek 10.8%,
  findTableUncached 9.5%, madvise 10.8% — sibling scope) per the 1M-op
  CPU profiles.
- **INSERT's quick-append path never reads the memo** (verifyQuickLeaf
  direct-parses into quickPageScratch), so the refresh no-ops there (nil
  memo -> decline) and the insert bench pays nothing on fresh pages; the
  +3.9% insert gain comes from workloads where the memo exists (dup-drop
  insert, defrag inserts, post-scan rewrites).
- **Micro-bench warmup can lie by 7x**: 10-iteration benchtime reported
  2000ns/seek; at 200-300k iterations the same bench reads 230-280ns.
  Never report a descent/op number below ~100k iterations on this tree.
- **Benchmark adjudication this round**: interleaved per-binary stable
  blocks (3 rounds x 27 runs/side), medians compared; select_scan swung
  -3.6% on medians with mins 37M-48M — pure machine noise on a shared
  fleet box, and the scan path is byte-identical (read-only, no refresh
  call sites). TestCursorFinalizerSafetyNet -race flake reproduced 1/6 on
  MAIN solo — pre-existing finalizer timing, per R9.INSERT adjudication.
- Harness failing-file set: 161 files, IDENTICAL sets on main and branch
  (zero branch-only). Full-suite v-mode grep of
  "FAIL: TestSQLiteSuite/<file>/" into sorted-unique lists diffs cleanly.
## R11.LITBOX — literal-path tranche (fleet/r11-litbox, 2026-10-07, @ HEAD)

- **The slot-path apply's SetCached(nil) was a self-inflicted re-parse.**
  writeSlot rewrote the literal text in place and nilled the parsed-value
  cache, so every substituted statement re-parsed its own canonical
  rendering at first evaluation (evalNumericLit AND the Cached()-reading
  lanes: update_setlane, or.go, scan helpers). The substituted value IS the
  parse result — int64Text/floatSlotText round-trip exactly (all int64;
  finite float64 via shortest-'g') — so the cache is now written at
  substitution time. THE ONE EXCEPTION that must never be cached: a
  non-finite float. Its rendering ("NaN.0"/"+Inf.0") falls through
  evalNumericLit to the literal-STRING result; caching the float64 would
  change the typed result. NewFloatLit guards it; bindFloatLiteral's
  NaN→NULL / Inf-refusal gates were already correct and stay.
- **Uint64 binds above MaxInt64 have no int64 literal spelling** — keep
  their nodes text-only (FormatUint); NewIntLit(int64(n)) is only sound for
  n ≤ MaxInt64 (same text as FormatUint there).
- **A fully-fused single-pass numeric scan LOST to the two-pass walk+parse.**
  Fusing fastParseInt64's digit accumulation into advanceNumeric's walk
  (one pass instead of walk+containsExp+fastParseInt64) measured 2-6%
  SLOWER on insert 3/3 interleaved pairs: the per-digit overflow branch in
  the walk costs more than a tight substring re-parse saves. Rule: don't
  fuse a branch-heavy accumulator into a byte-walk when the second pass is
  branch-free over a short substring. Only the provably-dead third pass was
  removed: advanceNumeric sets hasDot on BOTH dot and exponent consumption,
  so `hasDot || containsExp(numStr)` was always `hasDot`.
- **Template last-entry memo: the span verification IS the lookup.** The
  hash+map in the template cache only LOCATE a candidate; correctness comes
  from templateMatchesSpans against the statement's own bytes. A
  single-entry memo (last hit or stored) skips the maphash finalization and
  the map probe on same-shape streams and cannot serve a wrong template —
  verification runs identically, and entries are never retracted so a
  superseded memo is still a valid template. Delete-phase profile:
  prepareCached cum 50ms→20ms of 250ms samples, Sum64/memHashAES gone.
- **Profile attribution on short phases is quantized (10ms samples on
  250ms = 4% quanta)** — a "20% function" may be 12-28%. The delete memo
  win PREDICTED ~8-12% wall from profile deltas but delivered +1.8%
  (interleaved 9-run block medians); update/insert/point landed inside the
  ±1% noise floor (select_scan, an untouched path, is the control).
  Per-binary stable blocks remain mandatory, and profile-derived ns
  estimates are upper bounds until wall-confirmed.
- **Pre-existing baseline failures (adjudicated per-FILE solo on clean
  d14cdb3ef AND the branch, identical sets): tokenize-2.2** (unterminated
  `/*` block comment at EOF: SQLite treats a comment as running to EOF,
  frigolite's lexer rejects — parse/lexer gap, untouched here), **tkt_fa7bf5ec**
  and **snapshot3** (WAL/snapshot shapes). Under parallel/load the SAME
  trees blow out to 370+ timing-sensitive files (avtrans/vacuum/wal*); run
  suspects solo before believing a regression.
- **Where the residue lives now (measured, for the next round):** execdml
  insert glue is down to btree InsertCell (~75% of writeTableRowSh —
  sibling scope) + lockreg registerWriteTx/SetWriteTx (~7.4% of insert
  Exec — off-scope); echoVTabSource→allSchemasFingerprint ~5% of
  update/delete Exec (off-scope); the public execResult boundary and
  main.render harness costs unchanged; normalizeScan's single text walk
  (~40-60ns/stmt) is the floor of the template path.
## R11.RESEARCH — C-vs-frigolite per-op work-diff (fleet/r11-research, 2026-10-07)

Full doc: benchmarks/R11_RESEARCH.md (this branch). Per-op step lists with
C line refs vs Go file:line, annotated MATCHES-C / FRIGOLITE-EXTRA /
FRIGOLITE-DIFFERENT. Ranked top-10 lever table there. The durable points:

- **C never re-parses a page; frigolite re-parses it after every mutation.**
  C's MemPage is parsed ONCE at cache load and nCell/nFree/content-start are
  maintained incrementally by insertCell/dropCell/allocateSpace; frigolite's
  ParsedBTree memo is validation-based, so every write invalidates it and the
  NEXT statement pays ParsePageInto + memo alloc + hdrSnap copy (~120-250ns).
  The R10 note's fix stands: mutating btree paths PUBLISH their post-write
  parse (patch memo bytes + fields) instead of invalidating. Top lever
  (#1), serves insert+update+delete.
- **C's append insert runs NO search** (`loc=-1` from USESEEKRESULT,
  `idx = ++pCur->ix`, btree.c:9612). frigolite's writeLeafCell calls
  findInsertPositionTable (binary walk, 2 GetVarint/probe) UNCONDITIONALLY —
  even on the proven-append quick path where claimQuickAppend already proved
  rowID > maxKey. Thread insertIdx through; quick path passes CellCount.
- **C grows the file implicitly; frigolite truncates per page.**
  pager_write_pagelist does ONE SIZE_HINT then pwrites a pre-built dirty
  linked list (no sort, no per-page truncate); db-file truncate happens ONCE
  iff bDoTruncate. frigolite's flushOrderLocked makes+sorts a slice per
  flush and flushPage file.Truncates before EVERY page beyond EOF (+quota
  check each). Insert-only lever, file-backed shapes (R8's 24% item).
- **Most surprising: frigolite answers C's O(log n) sqlite3BtreeLast
  (OP_NewRowid) with scanMaxRowID — a FULL TABLE SCAN decoding every cell —
  whenever the rowid cache misses, and every point UPDATE/DELETE
  Invalidates that cache first.** Zero cost on the current single-phase
  bench shapes; an O(n) cliff on any interleaved workload. Fix: rightmost-
  leaf descent (rightmostTableLeaf already implements the walk).
- **Point-UPDATE's overwrite re-parses what the seek just parsed:**
  OverwriteCellByRowIDAt does a fresh storage.ParsePage (bypassing the
  ParsedBTree memo) + TableLeafCellSizeAt re-walks the cell header that
  DecodeCellInto just read. C: xParseCell once + bare memcpy. Route through
  the memo and fuse size+overflow into one header parse.
- **Point-DELETE parity is close** (O(1) dropCell port + needValues decode
  gate faithful to OP_Delete); residue is the double header parse in
  pointDeleteTarget + next-cell probe + cursor/funnel churn.
- **Point-SELECT has no single lever left**: memo validation + cursor-churn
  mutex pairs (open+register, release+unregister) + funnel preamble ≈ the
  130ns gap. 1.14x is near the floor without registry redesign.
- Autocommit funnel (execEntry/preflight/flush walks) is frigolite-only
  structure (~40-90ns/stmt) — C rides opcode dispatch with two early-outs
  (OP_Transaction in-txn no-op, eState<CACHEMOD commit no-op). Collapse
  walks; don't chase below the C dispatch floor.

## R12.BTREE — btree-side levers #3/#5/#6/#8 (fleet/r12-btree, 2026-10-07, @ 3fa1bb904)

Branch: fleet/r12-btree @ main 5f9cdd051. Four levers from R11_RESEARCH.md,
all measured with per-binary stable blocks in /tmp/perf/r12bt (NROWS=300000
NPOINT=200000 NSCAN=3 NUPDATE=100000 NDELETE=30000 NAUTO=5000, quiet).

- **L1 (R11#3) — writeLeafCell takes the insert index; append runs no search.**
  2fd755567. Quick-append passes CellCount (claim proved rightmost);
  insertLeafPage reuses dropTableLeafDuplicateRowid's position when nothing
  was dropped and re-searches (-1) only after an actual drop; writeLeafCell's
  internal duplicate re-probe deleted — both callers already own dup handling.
  insert_xact 1115k → 1140k ops/s (+2%); point/update/delete flat.
- **L2 (R11#5) — scanMaxRowID = rightmost-leaf descent, not a full scan.**
  ee909610c. BTree.MaxRowID() (id, found) added on the LastRowID walk; the
  found flag (not "id > 0") is the emptiness signal — the old interior walk's
  non-positive heuristic misanswered all-negative-rowid trees. GOTCHA: the
  retired scan's accumulator started at 0, so empty AND all-negative trees
  answered 0; execdml scanMaxRowID keeps that exact floor (`id > 0`) so the
  replacement is byte-equal to the scan for EVERY input (pin:
  btree_maxrowid_test.go compares descent vs the retired scan oracle across
  empty/single/asc/desc/random/delete-max/delete-middle/drained/negative/
  index-tree shapes). execdml/rowid.go's scanMaxRowID body is the ONE line of
  this lever outside internal/btree (call sites unchanged). Bench: flat on
  single-phase shapes (the scan ran once per run) — the lever removes the
  O(n) cliff from insert-after-update/delete interleavings.
- **L3 (R11#6) — point-delete decodes cell+size in ONE header parse.**
  540da1d03. storage.DecodeTableLeafCellAndSize = C's xParseCell (CellInfo
  once, btree.c:9908): decodeTableLeafCellInto returns its end offset,
  TableLeafCellSizeAt delegates to the fused helper (all other callers
  unchanged), pointDeleteTarget hands the size to dropCell. The next-cell
  duplicate probe KEPT: it is the fast path's corrupt-image guard (generic
  predicate deletes ALL same-rowid cells on the leaf; the probe declines to
  it). delete_xact 962k → 998k ops/s (+3.7%); point flat.
- **L4 (R11#8) — append-cursor slot is atomic advisory state; hot path has
  ZERO cursorRegMu acquisitions.** 3fa1bb904. quickAppendSlot fields →
  atomics, registry → sync.Map (create-once/read-heavy), claim returns the
  slot pointer, park stores through it, the six simple invalidation sites
  drop their Lock/Unlock (Close keeps its critical section for the cursor
  registry). Correctness rests on verifyQuickLeaf's page-bytes gate: any
  torn/stale slot pair declines to the generic path, which re-establishes —
  no lock needed for advisory state. **Memo-consumption half DECLINED with
  measurement reasoning: verifyQuickLeaf's ParsePageInto (8 header fields +
  validate) beats a memo hit (bytes.Equal over 8+2*nCell span + canary) at
  leaf sizes, and the shared memo struct would need a defensive copy before
  writeLeafCell mutates it.** Bench: flat within noise (mutexes were
  uncontended; the theoretical ~70ns is inside the run-to-run band).
- **Method held**: per-binary stable A/B blocks (never interleaved
  single-runs); baselines re-run in the same session as B; flat results are
  reported flat, not claimed as wins. Ambient drift across a session can
  move untouched phases ~1% — adjudicate per-phase, protect select_point.
- **Finisher gates (same session)**: build/testgen(7)/btree01-solo/named
  pins(21 pkgs)/-race(btree 596s+pager+root stress incl. concurrent
  open/close/DDL + append-cursor pins)/SOLID/quality all GREEN;
  MaxRowID descent-vs-scan pin + memo pins GREEN. Full-suite band
  adjudicated base-identical: `go test ./...` fails the documented
  ~4443-4458-case shared-cwd band on BOTH 5f9cdd051 and the branch with
  IDENTICAL 379-file sets (0 files diff; subtests rotate) — per-FILE
  serial (FRIGOLITE_TEST=<name> -parallel=1) also base-identical
  (rowid family 68=68 fail events, byte-equal modulo timing). NOTE:
  these families fail solo on pristine main TODAY (env drifted from the
  earlier solo-green sessions; testdata unchanged since Sep 18) — the
  base itself is the control, not the lessons' historical green.
- **Paired bench (3 interleaved rounds, per-binary stable blocks,
  /tmp/perf/r12bt2, mission env)**: medians branch vs main —
  insert 1,155,311 vs 1,142,539 (+1.1%); point 965,965 vs 955,331
  (+1.1%, protected); scan 50.1M vs 50.2M rows/s (-0.3%, noise);
  group 53=53; update 805,443 vs 794,610 (+1.4%); delete 996,556 vs
  988,385 (+0.8%); file 11,450 vs 11,266 (+1.6%). No phase regressed;
  delete/insert carry the lever wins from their dev sessions; scan
  untouched (L2 pays only in insert-after-mutation interleavings).
