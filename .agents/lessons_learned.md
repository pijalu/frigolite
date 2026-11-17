# Lessons Learned — Frigolite

Durable, reusable guidance only. Session-specific evidence lives in
`portplan/TESTVFS_P_C1_PLAN.md`, `portplan/HANDOFF_*.md`, and the run logs.
`plan/GUIDELINES.md` and AGENTS.md take precedence. Review and consolidate
this file at the start of every session; remove stale points instead of
appending.

## Process & verification discipline

- **Verify by failure-NAME-SET diffs, never counts**: counts swing ±35
  run-to-run under load (pure HEAD reproduces it — the wal-18.2.x
  busy/interrupt trials and setup_78-86 reopens are timing-sensitive).
  `grep -ao "TestSQLiteSuite/<fam>/[^ ]+" | sort -u` + comm both directions.
  Count FAMILY-level FAIL lines too: an unresolved runtime variable makes
  the harness `return` mid-family and the leaf grep shows 0 (select2/
  select7/conflict2/ioerr4/select5 hid behind this).
- **Never build baselines via stash roundtrips** — a pop takes stash@{0},
  which after any push is not your baseline; pops conflict and reorder.
  Commit each verified unit immediately; use a worktree for HEAD baselines.
  Uncommitted work has also been wiped by another agent's checkout — back
  up in-flight files to /tmp before long validation runs.
- **`go build ./...` does NOT compile _test.go files** — a "0 failures"
  grep can be a build-failure artifact. `go vet .` before trusting zeros;
  verify instrumented binaries with `strings <bin> | grep <trace-tag>` and
  run one known-hit unconditional trace as a control before trusting
  silence elsewhere.
- **An empty failure set can also mean the run was KILLED** (go test's
  default 10m timeout, or a shell `timeout N` expiring mid-family):
  non-verbose failure summaries print only at the END, so a killed run
  emits no FAIL lines. Bit four times in one session (e_walckpt 0/0/0/0
  that were 25/24 + hang). Check the run COMPLETED: `tail` shows
  "ok ... /FAIL" plus a duration, never "panic: test timed out"; pass
  `-timeout 25m` for long suites and run full-suite checks SEQUENTIALLY
  (two concurrent `go test` invocations in one module race each other's
  builds — both produced garbage).
- **Sweep-grep traps**: `grep -v "FAIL: TestSQLiteSuite"` strips leaf lines
  too (every leaf CONTAINS that string) — write go test output to a FILE
  and grep `-a -- '--- FAIL: TestSQLiteSuite/.*/'` (a `/` after the suite
  name = leaf lines only); verify the pipeline catches a known failure
  before trusting a 0. macOS `comm` warns "not in sorted order" and
  silently changes verdicts — do set logic in Python. `grep > same-file`
  self-truncates.
- **FRIGOLITE_TEST matches by SUBSTRING** (strings.Contains on the family
  name): "wal" runs the whole wal* group; "index1" matches bestindex1;
  "func/" matches nothing. Per-family truth = the
  `TestSQLiteSuite/<family>/<case>` paths in a run log.
- **The oracle**: `DYLD_LIBRARY_PATH=/tmp/tclshim ../sqlite/testfixture`
  (the shim dir holds libtcl9.0.dylib → the 9.1 symlink; homebrew's tcl-tk
  upgrade removed 9.0). ../sqlite is 3.51.0, /usr/bin/sqlite3 is 3.54 —
  version-tag every observation. For perf comparisons use a cached
  go-sqlite3 scratch module in /tmp (never a project dependency), replay
  the same SQL shape and transaction boundary, report scope narrowly, and
  benchmark with paired/interleaved repetitions + medians.
- Live-cursor callback mutation (btreefault-2.2 class): SQLite saves and
  reseeks other cursors by key around writes (btree.c:756-918); a
  post-Query callback cannot match it because the executor materializes
  scan rows before JOIN (select_exec.go:346) and combinations before
  WHERE (select.go:667) — real streaming needs a live outer B-tree cursor.
- Reproduce narrowly before editing (exact command, input, output,
  earliest divergence). Generated failures may be engine OR converter:
  establish engine-visible behavior natively before converter changes.
  Use SQLite as behavioral ground truth; probe intermediate state when
  final output hides divergence.
- Parallel agents + long validation runs: never edit the engine while a
  batch/suite is running; a verify-install batch must treat a BUILD
  FAILURE as "no result", never "0 failures" (it once installed 13
  unverified fixtures).
- Go gotchas with blast radius: an untyped constant returned as
  `interface{}` boxes as `int`, not `int64` — always `int64(0)` in engine
  helpers returning `interface{}`. `os.SameFile` type-asserts `*os.fileStat`
  — a custom FileInfo wrapper fails it; pair custom FileInfos with a raw
  stat path. `go build ./tools/tclconvert/` drops a binary in the repo
  root — `-o /tmp/...` or delete before committing. `git stash` skips
  untracked files without `-u`. zsh does not word-split `$VAR` in loops.
  Go returns freed pages to the OS lazily — `debug.SetMemoryLimit` is the
  only effective resident-set lever on macOS (`FreeOSMemory` does not
  shrink RSS: madvise(MADV_FREE) keeps pages resident until the kernel
  reclaims them); measure live heap with `GODEBUG=gctrace=1`.

## Fixture & converter discipline

- **Stale-fixture signature**: got [""] runtime-var mismatches (the VFS was
  never registered — no callback ever fired) + cascading "file is not a
  database" reopens from a corrupted predecessor flow, or a broad "table tN
  already exists" cascade (dropped lifecycle resets), or a family whose
  case count is far below the source's (stale e_update: 16 vs 59). Regenerate
  FIRST: `mkdir -p /tmp/x && go run ./tools/tclconvert -testdir
  ../sqlite/test -outdir /tmp/x -only <fam> && cp /tmp/x/<fam>.json
  testdata/` — wal3 was 107/112 stale-fixture artifacts. Never install a
  fixture without checking its step count; a full-corpus regen is its OWN
  unit with its own battery (never folded into a feature commit; converter
  drift once touched ~663 files).
- The corpus converts to more families than are installed — "installed
  fixture missing" is NEW COVERAGE, not a regression. tkt2686.test and
  types.test currently FAIL conversion; their installed fixtures stay.
- **The main executor must implement every step type the converter emits** —
  the unknown-type arm fails loudly on purpose (a missing `case "query":`
  once left 173,795 steps across 768 families unexecuted AND unasserted).
  Adding a new step type also requires the `groupToTestCase` whitelist in
  tools/tclconvert/main.go: an empty-SQL marker type missing from it is
  silently dropped (runtime_list was dropped corpus-wide until found).
- **A do_test expectation describes the BODY's value** (its last command's
  result), never the last captured statement — model the value source
  explicitly (last statement / `set X` / `expr {...}` / a marked runtime
  step). When a NEW step type can be a body's last statement, gate BOTH
  expectation paths: the specific promotion (direct trailing call) and the
  catch-all (never stamp the new type) — otherwise a trailing helper proc's
  value migrates onto the step and fabricates mismatches.
- **A converter-dropped user-proc call fails silently**: the call vanishes,
  the expectation migrates to the last emitted step, and the failure
  surfaces far from the cause (e_walckpt's `checkpoint db passive` dropped
  the whole checkpoint). When a fixture defines procs via `eval $script`
  from a foreach, verify every proc body's commands are known to the
  converter; an unknown tail command silently truncates the proc's effect.
- **commandCatch executes the first argument as a script whether braced or
  quoted** (a quoted body is substituted first — e_walckpt-4.x's
  `catch "wal_checkpoint_v2 db $mode" msg`); non-braced bodies used to do
  nothing. `catch` around a SQL capture is runtime-conditional — fixtures
  need conditional steps, not baked branches. A catch-list element's rc
  reference must resolve to the catch-result carrier the body actually
  stamped (a nested wrapper's named rc beats the deterministic
  `__catchrc_<msg>`).
- dispatchCommand passes args WITHOUT the command name — write the
  arg-index comment FIRST when adding a handler (the same arg-index bug
  struck commandBinary and commandBindStmt).
- Runtime markers never reach fixtures (stripRuntimeValuesDeep): never emit
  a marker into a Stmt field expecting it to survive — promote the
  statement to a compared step instead. Converter runtime placeholders must
  be dropped again when a later plain `set` assigns a known value (stale
  dynamic flags made computed expectations expand to placeholders).
- TCL semantics the converter must implement faithfully (each omission once
  corrupted a corpus family): `expr` substitution is ATOMIC — render every
  substituted value as an operand (numerics bare, everything else a quoted
  escaped string; a bare insertion silently flipped if-branches); `==`/`!=`
  compare numerically only when BOTH operands are numeric; integer/integer
  `/` truncates; `rand()/srand()` are Tcl's own Park-Miller LCG (srand
  consumes one draw; stubbing rand collapsed atof1's 20k-iteration loop);
  `string match` has no class negation and `trimright/trimleft` take a
  charlist; index specs (`end`, `end-N`, `N-M`, negative) apply to strings
  AND lists; `foreach` iterates multiple varList/list pairs over the
  longest, filling partial groups with ""; a `switch` body of `-` means the
  next pattern's body; option abbreviations (`-novar`) are accepted by
  unique-prefix matching; `split` follows real Tcl semantics; `lsort`
  (with -integer/-index/...), `md5`, `random_uuid`, `string is/replace`,
  `format` (float verbs take numbers; %i→%d; %u unsigned 32-bit), `eval
  arg1...` (concatenates + evaluates), `global` (writes through to the
  global slot inside procs), `file tail/dirname/extension`, and the
  testfixture commands (`sqlite3_wal_checkpoint{,_v2}`,
  `sqlite3_exec` percent-decoding, `sqlite3_prepare_v3`, `lsort`/`md5`).
  Unknown `string`/`format` subcommands must ERROR, never return the input
  (cmdString once answered everything with args[1] — 9,356 wrong round1
  expectations).
- TCL value-model traps: a list element keeps its single quotes
  (`'abc'` is the 5-char string); `execsql`/`db eval` return a LIST OF ROWS
  (a runtime-compared variable carries `resultRows`, else comparisons
  silently degrade to the first row); `db eval {SQL} {lappend res $x}`
  collects the FIRST column per row (a `query_first_column` step);
  adjacent placeholders (`$as$bs`) are ONE reference ('$' is an identifier
  char) — split at the inner '$' only when the remainder is another
  reference; escaped `\$x` in `db eval "..."` binds from TCL scope at
  conversion (bindSQLParams keeps `$name` only for dynamic variables);
  `$name` in braced SQL is a BOUND PARAMETER rendered as a SQL literal at
  runtime (identifier-shaped values and numbers stay bare); `set ::x`
  writes must land in the GLOBAL slot even inside procs.
- Converter scope/state: `do_execsql_test`/`do_catchsql_test`/`do_eqp_test`
  must scope `curTest` to their own command (a leaked name split prepared
  handle trios and leaked read locks); a Tcl variable's dynamic flag, a
  family's process-global flags, and `lastCollectVar` all reset per test/
  family boundary; `foreach`-over-glob cleanup (`foreach f [glob
  -nocomplain test.db*] { forcedelete $f }`) is a recognized `delete_glob`
  marker (also implemented on the main path); `faultsim_delete_and_reopen`
  maps to reset_db; `Tcl return` ends source execution (never invent later
  tests); `forcedelete test.db2` resolves beside the lifecycle test.db;
  `sqlite3 db ""` / `:memory:` is `open_empty_db`, NOT reset_db; `sqlite3
  tmp ""` (empty filename on a NAMED handle) is a private-temp OPEN.
- Converter runaway loops are a MEMORY bug: a `for` with runtime-only
  termination unrolls to the loop cap — the per-interpreter statement
  budget (150,000) + deadline (30s) checked before every command
  (checkRunaway, sticky errors catch cannot swallow) makes oversized files
  conversion errors. Deadlines must be cooperative (interpreter-checked),
  not `select` timeouts that leak a still-running goroutine. Never install
  a fixture without checking its step count (one shipped at 1.1 GB /
  1.8M tests and >20 GB RAM across the corpus).
- Mini-interpreter fidelity bugs look like engine failures: print the
  emitted SQL and run it natively — if the native run matches the oracle,
  the converter is the suspect ($$value is usually a converter artifact).
  When adding a converter command, pin the end-to-end FIXTURE, not just
  the interp-level unit test.

## Engine contracts pinned against C

**WAL**:
- WAL commits never bump the file-header change counter (pager.c:6496's
  early return); the frame page numbers (walview.go, the byte-exact frame
  decoder) are the diagnostic.
- Sync events: WAL_SYNC_FLAGS(x)=x&3 (commit — FULL only),
  CKPT_SYNC_FLAGS(x)=(x>>2)&3 (header/checkpoint — NORMAL+FULL). The WAL
  header sync fires when the first frame of a restarted log is written,
  gated on IOCAP_SEQUENTIAL and synchronous=OFF. The writer pins
  READ_LOCK(0) for walFrames' walRestartLog gate (wal.c:4056, via
  pagerSharedLock) — but the pin must gate on
  the LIVE shared header (nBackfill==mxFrame): a vacuous 0==0 against a
  not-yet-adopted zero cached header pins "ignore the log" beside a
  non-empty log and the writer reads zeroed WAL-only pages ("unknown page
  type: 0x00").
- A commit's page-1 frame: C's allocateBtreePage append branch
  (btree.c:6737+6761) makes page 1 writable and writes the new nPage into
  its image at EVERY allocation-from-end. The stamp is WAL-mode only (C's
  allocateBtreePage never runs outside a write transaction; the pager's
  open-time init allocates page 1 on a 0-byte db — a stamp there leads the
  physical file and HeaderBeyondFile reports truncation) and
  size-bytes-only (a full-header mirror clobbers fresher in-progress meta
  bytes).
- Second connection on a 0-byte main file: three pieces together —
  pagerOpenWalIfPresent (pager.c:3337) on EVERY shared lock (attach when a
  -wal exists beside a non-empty main file; DELETE it beside an empty one);
  OP_JournalMode's sqlite3BtreeSetVersion(pBt, 2) (vdbe.c:8069;
  btree.c:11465 writes aData[18]/[19]) stamping the version bytes into
  page 1 through a ROLLBACK-mode flush BEFORE the -wal opens
  (the main file must become non-empty for the attach check to fire); and
  the live-gated READ_LOCK(0) pin above.
- vfs_set_readmark (test_vfs.c:1637) reads/writes the wal-index read
  marks (aShm[25+slot], slots 0..4 = aReadMark[0..4]) and fires
  xShmMap(0, 32*1024, 0) first; the converter's statement form stores
  WriteValue (args exclude the command name), the `list [vfs_set_readmark
  ...]` body form emits per-slot collector steps + a runtime_list.
- WalCheckpoint (main.c): an unknown zDb → SQLITE_ERROR "unknown database:
  <name>" (pragmaDBCtx's main fallback masks it — resolve the context
  explicitly); a NULL zDb checkpoints EVERY attached database, the triple
  reports the FIRST checkpointed db (the C nulls its out-params after the
  first iteration), BUSY accumulates, non-WAL leaves the initial -1s; the
  pragma's nLog/nCkpt initializers stay -1 when walCheckpoint never ran (a
  WRITER/CKPT lock veto precedes it). The testfixture command rejects a
  MODE outside passive/full/restart/truncate with SQLITE_MISUSE ("not an
  error").
- xShmLock vetoes propagate verbatim through the lock protocol (BUSY drives
  busy handling; READONLY_* flows to the caller's mapping;
  SQLITE_READONLY_CANTINIT/RECOVERY/ROLLBACK/DBMOVED/DIRECTORY share the
  base errstr "attempt to write a readonly database", main.c:1547).
  C maps the -shm LAZILY at the first transaction; our pager attaches
  eagerly — SetVFSHook arms a pending xShmMap check the first post-install
  WAL access consumes.
- walvfs-5.3's 24 frames: `tvfs devchar sequential` (walvfs.test:61, never
  reset) DROPS POWERSAFE_OVERWRITE, so walFrames' padToSectorBoundary
  repeats the last commit frame to the 4096-byte sector boundary at
  synchronous=FULL (22 pages + 2 padding). The CLI oracle (PSO on) cannot
  reproduce it; the reference testfixture can.
- Pager short reads: readFilePageLocked zero-fills like C's readDbPage
  (SQLITE_IOERR_SHORT_READ swallowed) — a 0-byte main file under WAL mode
  reads page 1 as zeros and the SCHEMA layer decides. A wal-index adopted
  from a restored -shm must also verify the WAL file can hold MxFrame
  frames (a snapshot pair can pair a wal-index with a restarted WAL); a
  zeroed page 1 with numPages>0 is genuinely corrupt.

**B-trees / storage**:
- WITHOUT ROWID table btrees are INDEX btrees: DML scans use index
  traversal and mutations remove exact full-record keys with
  DeleteIndexEntries; leaf-only sweeps skip interior rows. WR secondary
  indexes currently have TAILS THAT DISAGREE ACROSS WRITE PATHS (INSERT
  appends the insert-time rowid; DELETE/DDL address [key..., 0]) —
  integrity_check exempts WR secondary indexes from the count comparison
  until the tail is unified (the SQLite-faithful layout is
  [key..., PK columns]); a pre-existing WR DELETE leak (deleteOneConflictRow in
  update_orreplace.go:124 skips maintainIndexesOnDeleteRowMaps; the INSERT
  side's deleteReplaceConflictRow at insert_replace.go:380 shows the
  pattern) is carded, and xferOptimization's counter is plan-time
  (insert.c:3235).
- Engine.databases registers TEMP under BOTH "TEMP" and "TEMPORARY"
  (engine.go ~973) — aggregates walking Databases() (insert_index_defs.go,
  uniqueIndexColumns) yield each temp index TWICE; dedupe by pointer.
- `PRAGMA integrity_check`'s "row N missing from index X" number is the
  1-based SCAN ORDINAL (pragma.c reg[7]), not the rowid; index report order
  is REVERSE creation (pIndex prepend). Probe trees need SetIndexKeyInfo or
  SeekIndexKey falls back to the O(n²) stored-order walk (46s → 95ms on
  50k rows). quick_check skips the per-row presence pass; the count rule is
  `leaf || !intKey` (= btree.CountEntries).
- `PRAGMA index_xinfo` on a WITHOUT ROWID table reports each PK column
  once in declared PK order even when table-level PRIMARY KEY syntax
  repeats names (pragma.c:1387-1410 iterates Index.aiColumn).
- Interior index cells may be copied divider keys — verify representation
  before traversal/counting rules. Value equality and representation differ
  (INTEGER 1 vs REAL 1.0) — exact mutation lookups may need byte-level
  confirmation after comparator seek. Validate arithmetic/offsets/lengths
  at representation boundaries; malformed files error, never panic.
- Incremental vacuum can expose stale PTRMAP_BTREE parent references after
  balancing — propagate incrVacuumStep errors, resolve the actual edge with
  findParentByWalk, retry before leaving the freelist undrained.
- FTS3: the C expression parser re-tokenizes the remaining buffer per token
  (O(n²)) — our port scans once (a 1.5 MB expression: 45s → 107ms); a port
  that follows C's structure must still be checked against C's complexity.
  readCellByRowID must SeekToRowID, not scan (fts3defer 95s → 15s).

**Values / names / semantics**:
- The testfixture renders REALs through TCL's shortest-round-trip print
  (fixed notation for exponents −4..16 with ".0" when integral, scientific
  otherwise) — the HARNESS uses the TCL rules while the ENGINE's
  value→text keeps SQLite's %!.15g. tester.tcl's do_test falls back to
  fpnum_compare (test1.c:6168): trailing-zero tolerance only when the
  other string ENDED; once 15 fractional digits match, the rest are
  skipped. printf %f uses sqlite3FpDecode (Dekker double-double) — Go
  strconv is not equivalent; %!f selects mxRound=26. round() returns a
  REAL, so trailing-zero expectations are converter bugs.
- LIKE/GLOB/REGEXP are FUNCTION CALLS (expr.c TK_LIKE_KW → like(Y,X)); a
  user registration replaces the operator — Frigolite's dispatch must USE
  the registry result, not just probe it (the vtab OverloadProbe gate does
  not apply to `->`, which parse.y compiles to the plain function "->").
- Parameters: `:name`/`@name` accept `$` (IdChar); CURRENT_TIME/DATE/
  TIMESTAMP are keywords, not columns; two distinct prepare errors exist
  (an explicit out-of-range `?NNN` → "variable number must be between ?1
  and ?N"; an ASSIGNED index above the limit → "too many SQL variables",
  and the check must run on the Query path too). SQLITE_MAX_VARIABLE_NUMBER
  (32766 post-3.32) must be exposed to fixtures.
- Bitwise operators coerce (int as-is, reals truncated, text/blob parsed
  with 0 on failure); only NULL propagates NULL. `IS TRUE/FALSE` uses
  boolean truth coercion (OP_IsTrue), not equality against 1/0. SQLite's
  JSON parser KEEPS raw control characters inside strings and the
  serializer escapes them.
- Function auxdata (sqlite3_get/set_auxdata) lives per (call site, argument
  index) for one statement execution: constant-argument slots persist
  across rows, non-constant slots are fresh per call, get-after-set within
  one call sees the value, statement teardown clears everything.
- Column naming (select.c, oracle-verified): full_column_names names direct
  references `REAL-TABLE.COLUMN` (never the alias); short=OFF/full=OFF
  names the expression's original span text; default short=ON names the
  bare column; star expansion fixes `zTab.column` only under
  full=ON&&short=OFF. Derived tables/CTEs/views dedup case-insensitive
  column collisions with `:N` (top-level result sets never dedup). View
  metadata must apply the USING/NATURAL star merge, and the
  omit-unused-subquery-column optimization must mark JOIN-constrained
  columns used for derived subqueries AND named views.
- SQLITE_TEST counter calibration (where7/where8): sqlite3_search_count is
  +1 per OP_SeekLT/LE/GE/GT execution (vdbe.c:4973, unconditional), +1 per
  successful OP_Next/Prev (next_tail), +1 per deferred moveto
  (FinishMoveto); Rewind/Last/SeekRowid not counted; OP_Sort is a bare −1
  with NO recovering +1 (a sorter of n rows nets n−2). The MULTI-INDEX OR
  eq loop's final Next steps onto the boundary successor and SUCCEEDS (the
  beyond[] model in execdml/or.go). Index walks: 1 seek + n successful
  Nexts (the boundary-successor step counts) + n movetos (covering: none).
  IN-valued OR arms plan as per-value eq branches (+n−1 ephemeral Nexts per
  arm; unary + suppresses). The OR-single-range rule is whereexpr.c CASE 2
  (whereCombineDisjuncts; the prereq mask plumbing is whereexpr.c:1167-1239
  and the vtab usable mask where.c:4280). The planner/executor disconnect class: gate on
  the RESOLVED PLAN's provenance, never a blanket query-shape gate (the
  LIKE pre-gate once declined every LIKE-bearing WHERE).
- insert.c's xferOptimization compares column references BY ORDINAL, is
  plan-time (later constraint failures do not undo the counter), and is
  gated by PRAGMA foreign_keys; the converter's `set ::sqlite3_xferopt_count`
  interception must fire only on the two-arg WRITE form.
- Other pinned contracts: `sqlite3_db_cacheflush` (main.c:917-945) flushes only dirty pages
  with nRef==0 through pagerStress (our commit-phase flushAll is NOT
  equivalent; the synchronous boundary also checks SharedTxByOther);
  the shared-cache contract decomposes as table locks (btree.c
  querySharedCacheTableLock — one writer, BTS_PENDING), the begin rule
  (btree.c:3638 — a second write txn fails with the BARE "database table
  is locked"), a failed OP_TableLock ends only the STATEMENT, the
  pending-lock rule blocks NEW transactions until the conflicting ones
  end, and notify.c's blocked graph fires one grouped callback per
  connection — the page-cache sharing itself is observable-irrelevant;
  `Stmt.Step` must route DML to prepared execution (Query-ing every AST
  broke DML); `UPDATE ... RETURNING` still updates sqlite3_changes();
  rtree refuses CREATE when a shadow-table name is taken, with a QUOTED
  identifier in the message; schema rows can overflow (DeleteCellsWhere
  with full-payload deletion for predicates); prepare.c parses stored
  schema rows beginning with CREATE and treats other nonempty SQL as
  malformed (keep leading comments parseable); SQLite re-validates stored
  trigger bodies by owning schema at load AND fire time (TEMP triggers
  exempt); vtab xFilter argv must carry RAW values (unwrap
  util.ColumnValue); row-value EQ splits fully implement the comparison
  while the ORDERING split is a first-difference prefix (the residual
  KEEPS the full comparison).

## Known carded issues (do not re-diagnose — pick up the unit)

- `TestWalCheckpoint` (internal/pager/wal_test.go:347) is TEST DRIFT: it
  drives AllocatePage/WritePage/Flush directly, a path that no longer
  appends the WAL commit frame; the real commit path is green in the WAL
  families. Verify which path a unit test drives before attributing a
  main-file-size assertion to the checkpoint code.
- The harness `exec` case NEVER compares rows: exec+expect SELECT steps are
  vacuous corpus-wide (~21,107 steps; func.json alone 2,202). Carded as a
  major unit (promote SELECT to query in the converter, or compare in the
  harness) — expect churn when 21k assertions go live.
- WAL snapshot isolation is open (8_3_names-5.6: an open read txn must not
  observe another connection's commit) — queued P7.WAL snapshot/shared-cache
  work. bestindexE's ON-term planner is a genuine gap requiring integrated
  join-loop/vtab execution. The temp pager cannot express file-backed
  cache_size spill (temptable2-4.x) — carded as the temp-spill pager unit.
- WR secondary-index tails + the two integrity-check leaks are carded with
  exact fix locations in the B-trees section above.
- e_walckpt remainder (129): the mode-sweep choreography (1.full/restart/
  truncate.x no-such-table/COMMIT — checkpoint-busy vs a second connection)
  and the busy-handler/xWrite/sync callback counters (1.passive.1.6-8,
  1.3.2.2). See portplan/HANDOFF_2026-11-04_WAL_RESIDUE.md for the restart
  point and the full unit queue.

## Fixture & skip policies

- The harness cannot faithfully execute Tcl callback-state tests (`db eval
  {SQL} {callback}` mutations, `db func` UDF side effects, Tcl globals
  populated by xBestIndex/xFilter, prepared-handle lifecycles across
  blocks). Policy: skip ONLY the affected assertions and their dependent
  cleanup, keep the fixture's lifecycle steps, and point to the generated
  `testgen/<fam>` suite as the authoritative engine contract. This covers:
  analyze3/analyze5(analyzeE green via regen), alter2 (aggregated catchsql
  expectations), alterlegacy/altertab (the variadic `trigger` UDF is
  fixture-scoped and restores them), attach2/attach3 (VM handles; db_list
  is a projection helper), autoinc-8.1/12.x (query_assign rootpages, C-API
  status), autoindex1 (sqlite3_log), backup2-3.1 (omitted db restore),
  badutf2 ($sql interpolation), bestindex1/5 (callback globals),
  bind2 (preupdate), blob (bin_to_hex), close 1.3/1.4 (close_v2 — filter
  `^close$`, "close" substring-matches e_blobclose), e_droptrigger
  (list_all_triggers lists EVERY schema prefixed), orderby8 (first-column
  collection), trans3 (COMMIT/ROLLBACK inside row callbacks), with1-10
  (missing Tcl tree procs), analyze9 (the s(blob) UDF), a_string/blob
  counter UDFs (walbig).
- `ifcapable !X { finish_test; return }` with X modeled absent (utf16,
  8_3_names) stops conversion ENTIRELY — no fixture, matching testfixture;
  never force guarded assertions green by implementing X (8_3_names is
  SQLITE_ENABLE_8_3_NAMES FAT mangling; the legacy f_8_3_names.json keeps
  the SQL-visible coverage). A failing `PRAGMA encoding='utf-16'` fixture
  is not an encoding regression — check the earliest error (collate7 was
  the missing collation-v2 registration).
- Fixture-lifecycle fidelity: `ordered:true` when sections reset state
  (altermalloc2); named-handle commands (db2 eval/close/unlock_notify)
  carry the handle name through BOTH the handler-map and named-connection
  paths; Tcl module registrations are fixture-scoped with later proc
  definitions attached (a registration post-pass); vtab module arguments
  arrive as one quoted command string (normalize before list parsing);
  array-element names nest substitutions (`$cols($C(column))`); ATTACH
  ':memory:' keeps a DISTINCT in-memory database per attach (never rewrite
  to a shared file); relative ATTACH operands and delete_file targets route
  through the fixture directory; `sqlite3_create_function DB` ports live in
  frigolite_harness_testfuncs_test.go (register_test_funcs), including
  pointer_change's buffer-state machine.
- Correlated table-valued pragma/function rows carry a positional
  corrLeftIdx provenance map — never filter/reorder rightMaps alone
  (json102-1130 panic class). Materialized derived rows keep positional
  source data with left/right vectors in distinct keys. Correlated
  subquery qualifier resolution may resolve in an ENCLOSING scope —
  distinguish that from a locally-joined qualifier.
- `do_select_tests` helpers merge every iteration into ONE case named
  after the first iteration, with per-iteration steps in source order;
  `-tclquery` is distinct from SQL query (nested Tcl lists, nullvalue
  ordering); `-error` means a caught SQL failure; the repair script runs
  around each generated triple. `do_test` bodies ending in `set ::var`
  emit runtime global assertions AFTER the steps (never increment expected
  state at conversion time). `lreplace [db eval {...}] start end value`
  mutates the FLATTENED result — preserve as postprocessing
  (ResultTransform), never change the SQL.
- Stale fixture JSON: regenerate only the intended family from source,
  inspect the diff, validate the exact family, and never hide engine
  behavior with skips. Naming-convention changes between old/new fixtures
  produce false regression lists — read the batch's list against the old
  fixture's actual names before reverting.

## Callback interps (harness fixture callbacks)

- **callbackCommands dispatch must precede the converter branches**: a
  callback interp's `db2 eval commit` must drive the LIVE connection —
  dispatchCommand checks callbackCommands FIRST (the converter never
  registers any, so conversion time is unchanged), then
  RegisterDefaultCallbackCommand resolves connection names against the
  live set per call (adapters bound at interp build time go stale when a
  scenario reopens connections: e_walckpt's tvfs_callback db5/db6, wal3's
  xShmLock driver).
- **Fixture-VFS masks are test_vfs.c semantics**: unset filter = ALL
  methods fire (p->mask = TESTVFS_ALL_MASK), `filter X` restricts, `filter
  {}` silences. A script bound AFTER a connection opened still governs it
  (the testvfs object holds the script live) — the tvfs interp builds
  lazily at the first callback then.
- **The busy handler is a counting session per statement** (main.c
  sqlite3InvokeBusyHandler): the handler receives the current count
  starting 0, the count advances only on a retry answer, one session spans
  the whole checkpoint (writer lock + read marks + restart blocking).
  TCL `db busy` script result 0 = retry, nonzero = give up (DbBusyHandler
  inverts into sqlite's nonzero-retries convention).
- **Hooks and handler scripts re-enter the engine**: NEVER fire a VFS
  hook or busy-handler script while holding the wal-index mutex (the
  script drives other connections' commits which need that mutex —
  wal3-6.2.4/e_walckpt copyback self-deadlocks; C's backfill holds no
  wal-index critical section, its locks are shm bytes). Go mutex stacks
  from the `panic: test timed out` dump identify the holder in one look.
- **test1.c's wal_checkpoint_v2 parses MODE as an integer FIRST** (0..3
  valid, other integers reach the API's SQLITE_MISUSE "not an error");
  a checkpoint on a non-WAL pager reports {0 -1 -1} (pPager->pWal==0
  leaves the pragma's initializers standing).
- **A combined do_test body `list [catchsql A dbX] [catchsql B dbY]`**
  emits expect-less catch steps plus a final catch step whose Expect is
  the BRACED pair list — the harness concatenates the last N catch
  results rendered with tcl.TclList (brace style); distinguish from a
  pragma triple ("0 -1 -1") by the pair's inner message braces.

## Backup: the sqlite3_backup command object (S7g)

- **test_backup.c's object contract**: `sqlite3_backup CMD DEST DESTDB SRC
  SRCDB` (init) copies NOTHING; methods `step N` / `finish` /
  `remaining` / `pagecount`; `finish` DELETES the command and ROLLS BACK
  the destination (backup.c:599-603 — finish never copies), returning the
  last step's code (DONE→OK). A BUSY step (source write txn / other-conn
  lock) is resumable, not terminal. frigolite: `Backup.Step` exists in
  frigolite_backup_step.go (page budget + setDestPgsz + restart rule) —
  don't re-add a duplicate; Finish's copy-remaining path needs the BUSY
  guard (added, backup.c-faithful) so a finish after a BUSY step doesn't
  read the source's uncommitted state.
- **Converter named-object fallthrough**: the backup handle dispatches
  like `i.testvfsNames[cmd]` (before `i.procs[cmd]`); `step N` emits one
  `backup_db` step (the harness runs NewBackup+Step(n)+Finish per
  emission), `finish` emits nothing and deletes the handle. The harness
  expect compare renders srcRC alone (single-code expect, walbak-2.6's
  SQLITE_BUSY) or `srcRC+" "+dstRC` (a `list [B step N] [B finish]`
  body).
- **Regenerating a fixture after teaching the converter a construct
  converts vacuous-green tests into real assertions** (walbak-2.x gained
  expects incl. SQLITE_BUSY) — the failure-name-set diff stays the
  arbiter, and recovered steps may fix previously-failing tests for free
  (walbak 9 → 0).
- **WAL-source backup page-1-image parity**: a WAL source leaves a
  file-backed rollback destination reporting WAL (the copied page 1
  carries 2/2 — walbak-3.x.7); mirror with SetJournalMode("wal") on the
  live dest, skipped for IsMemory() destinations (memory dbs have no
  file to carry version bytes).

## Harness connection lifecycle (the wal-12.2 refs=2 leak)

- **C's reopen_db (tester.tcl) runs `catch {db close}` first**, and a
  `sqlite3 <name> ...` replacement deletes the old command (closing its
  connection). The harness's reopen_db/open_named_db handlers kept the
  replaced engine alive: its wal-index reference kept RefCount at 2, the
  next close skipped the close-time checkpoint, and the copied main file
  lost the schema pages (wal-12.2/12.4 class). Fix: run the collation
  destructors, unregister the echo module and Close the replaced engine
  in BOTH handlers (wal sweep 156 → 147).
- **The close-time checkpoints make the wal group slower**: every reopen
  now checkpoints+fsyncs at the next close, so the wal sweep needs
  -test.timeout 25m (~1400s wall). faultsim families pay a bigger price:
  walfault-11's trial loop now hits the 512-trial cap (713.77s) — the
  per-reopen close-time checkpoints multiply the fault-injection
  schedule. Same residue class as the wal-3 xShmLock hook-callback
  surplus; carded, not a functional gap.

## The checkpoint copyback interrupt check (S8)

- **wal.c:2289-2292 is a per-page check INSIDE the copyback loop**: after
  each main-db page write, `db->u1.isInterrupted` is consulted and the
  loop breaks with SQLITE_INTERRUPT — the szDb truncate and the nBackfill
  advance are skipped (the SQLITE_OK gate at wal.c:2308), so the
  interrupted attempt leaves the log's frames standing for the next
  attempt. Without the check the copyback drains the WAL despite the
  interrupt, and every later interrupt-during-checkpoint trial becomes a
  no-op (interrupt2's delay=10/15/20 trials saw success).
- **Plumbing**: `Pager.interruptProbe func() bool` (set once, read
  without the mutex like vfsHook — no engine reference in the pager
  layer) bound at NewEngine + AppendDBList via
  `Engine.bindInterruptProbe(pg.SetInterruptProbe(e.IsInterrupted))`.
  The sentinel `pager.ErrCheckpointInterrupted` maps to the pragma's
  "interrupted" Result error; the harness's wal_checkpoint_v2 composition
  renders "SQLITE_INTERRUPT - interrupted" (test1.c's
  sqlite3ErrName(rc) " - " errmsg).
- **Passive-checkpoint triples after a full backfill stay {0 36 36}** —
  C keeps the frames in the log until a RESET/TRUNCATE (don't pin
  {0 0 0} after a PASSIVE checkpoint; only TRUNCATE zeroes the log).
- **tester.tcl's snapshot pair aliases the faultsim pair**
  (malloc_common.tcl:169-172: `faultsim_save` IS `db_save`): the
  converter maps db_save/db_save_and_close/db_restore/
  db_restore_and_reopen to the faultsim step handlers — one line each.
- **interrupt2-2.1 residue**: a per-row `db eval` callback that arms an
  interrupt and interleaves a checkpoint MID-SCAN needs an interleaved
  row-callback step surface (the body was dropped; the query degenerates
  to a full-row render).

## S9-S12 records (2026-11-07/08)

- **The census's per-family counts can be substring artifacts**: "index
  200" was FRIGOLITE_TEST=index sweeping bestindex*/index9/e_reindex/...
  — the TRUE index family had 2 failures. Re-derive family truth from
  the `TestSQLiteSuite/<family>/<case>` paths before scoping a unit.
- **An Open failure after a WAL attach must release the wal-index ref**
  (pageropen.go openPager's error path now closes the WAL): a leaked ref
  keeps the registry entry live forever, the DMS first-attach truncate
  never runs, and every later open of the same path ADOPTS the stale
  shared header (matching salts make the staleness check blind) instead
  of recovering — the "isolated passes, sequence fails" signature.
- **wal-18.2's label was stale** ("interrupt choreography"): the source
  is WAL-recovery page-size validation, and the failures were the
  converter dropping the `binary format`+file-write crafting body. The
  binary-format class spans 43 ../sqlite test files (carded). The
  engine's recovery gate (walrecover.go) already matched C exactly.
- **IOCAP bit values**: SEQUENTIAL=0x400, POWERSAFE_OVERWRITE=0x1000
  (sqlite.h.in:671-686) — the old engine/harness pair (0x100/0x800) was
  consistently wrong with itself. testvfs iDevchar semantics: -1 =
  passthrough (the unix default PSO stands), a name list REPLACES the
  mask, an argument-less `devchar` is the C read (no-op).
- **SQLITE_DEFAULT_SYNCHRONOUS=2 is PRAGMA-numbering (2=FULL)**; the
  pager's WAL sync-flag default must be FULL (commit sync bits set) —
  the old NORMAL default disagreed with the SQL layer's FULL and quietly
  disabled walFrames' padding gate.
- **DeviceCharacteristics' RLock self-deadlocks inside the commit path**
  (the commit holds p.mu) — read the field directly in locked sections
  (the vfsHook/walSyncFlags style).
- **padToSectorBoundary (wal.c:4172-4190)**: pad when the commit sync
  bits are non-zero AND the device lacks PSO; the padding frames are
  real frames (mxFrame/aFrameCksum/iCallback include them); the
  observable is walvfs-5.3's read mark {24 100 100 100}.
- **where.c's ORDER BY index rule**: a term's EFFECTIVE collation
  (explicit COLLATE, else the column's declared, else BINARY) must equal
  the index column's DECLARED collation (index SQL's explicit COLLATE,
  else the column's declared, else BINARY) by NAME — the current
  registration is irrelevant (a stale stored order still delivers,
  e_reindex 2.3.x). The executor has TWO delivery gates: gate1
  (indexOrderedScanForOrderBy) STRIPS the COLLATE before its matcher —
  pass the terms (orderByTermColls) through instead; gate2
  (loopOrderedEmission) requires a WHERE clause by design. Both gates
  and the b-tree read must resolve the FROM table's OWNING schema
  (aux.t1's index is a different b-tree over a different pager).
- **REINDEX target resolution (build.c sqlite3Reindex)**: resolve the
  MODE once, per target — collation FIRST (R-35892-30289: the collation
  match takes precedence over index/table names), then table (all its
  indexes), then index; a table/index target rebuilds the ONE object
  LocateTable/FindIndex resolve (temp, main, then attached — never a
  same-named shadow in another schema). The old per-entry switch
  conflated the modes (a table target rebuilt collation-matching indexes
  elsewhere; a collation target rebuilt same-named tables' indexes).
- **Hand-pinning fixture assertions** (index.json 2.2/13.2): when the
  converter migrates a do_test body's value onto a non-value step (the
  lappend composition), hand-edit the JSON to express the C contract and
  cite the shape in the commit — a regen would re-emit the defect.

## S13-S16 records (2026-11-08, the tkt census slice)

- **Old-generation duplicate fixtures inflate the census**: 48 tkt
  underscore-named fixtures (plus fts_9fd058691/fuzz_oss1/vacuum_into)
  were stale-converter duplicates of dash-named siblings — truncated
  content (missing CREATE TABLE setups, missing UDF registrations,
  missing set_fts3_parens steps) double-running every source under two
  family names. Scan testdata for `name`/`name-with-dashes` pairs;
  verify the dash one against a fresh regeneration; delete the stale
  one. After S14/S15 the tkt census 124 -> 55 with ZERO new failures.
- **The set_fts3_parens machinery is end-to-end** (converter
  special-case -> set_fts3_parens step -> harness TclVarSet ->
  fts3ParenthesesEnabled): a family whose MATCH expectations need the
  paren grammar but whose fixture lacks the step is a STALE fixture —
  regenerate before diagnosing the engine (tkt-bdc6bbbb38's 16 got []
  were exactly this).
- **A `catch {<probe query>}` whose query errors IS the captured
  result**: the harness query arm stamps the catch carriers (rc=1 +
  message) and continues when the step carries catchResult — it
  previously failed the case before the capture (fts3expr5 2.2's
  "Error parsing expression").
- **UNIQUE-index NULL semantics void the order-distinct mark-off**
  (where.c isOrderDistinct): an `X IS NULL` equality over a nullable
  unique-prefix column matches several rows sharing the visible key
  (NULLs are distinct entries in a UNIQUE index) — the remaining ORDER
  BY term still needs its sorter (tkt3824-2.x returned index-ordered
  rows). `c IS NULL` parses as IS over a NULL literal — both conjunct
  shapes must be checked.
- **Sort-marker-only helpers are not counter helpers**: tkt3824's
  execsql_status appends only "sort"/"nosort" (where7's count_steps is
  the labeled-counter variant, where8's execsql_status2 the bare one) —
  attributing the counters to the wrong helper fabricates "scan N sort
  M" tails the oracle never emitted. `lsort [execsql_status ...]`
  postprocesses are result transforms, not expectations.

## S17 record (2026-11-08)

- **A dropped per-row foreach cascades into misleading validator errors**:
  tkt-31338-3.1's `foreach x [db eval {SELECT name FROM sqlite_master...}]
  { db eval "DROP TABLE $x" }` emitted a bare `DROP TABLE ` ("incomplete
  input"), left every table in place, and the following section's
  LEFT-JOIN queries ran against the PREVIOUS section's same-named tables
  — the ON-clause validator's "references tables to its right" was a
  cascade artifact, not an engine gap (the real shape passes natively).
  When a validator error looks wrong, check the fixture's SETUP state
  first (the stale-fixture lesson generalized to mid-file state).
- At a deterministic-schema point, a dropped foreach over sqlite_master
  names hand-pins as explicit DROP TABLE statements (S17).

## S21 record (2026-11-11) — the OR-plan's per-arm mechanics

- **C's deferred-seek alt-map is per-arm** (vdbe.c:3022): a Column on a
  deferred table cursor whose `aAltMap[1+col]` is nonzero is SERVED FROM
  THE INDEX CURSOR (`pAltCursor`) without finishing the moveto — only an
  uncovered column read fires FinishMoveto. So the moveto is per-ROW and
  keyed on the row's OWNING arm's index, never per-plan ("covered" vs
  "uncovered" queries): whereD-6.3.2's x1a arm lacks c (1 moveto) while
  its x1bc arm covers it. `SELECT rowid` never fetches (OP_IdxRowid
  reads the index entry's trailing rowid); `SELECT *` reads everything.
  This replaced orPlanCovered's all-or-nothing model.
- **A rowid-equality OR arm is OP_SeekRowid** (C renders "SEARCH t USING
  INTEGER PRIMARY KEY (rowid=?)"): no seek count, NO Next loop, no
  moveto — the table cursor is positioned directly. The pseudo-aliases
  rowid/_rowid_/oid work on any rowid table; a declared column named
  rowid shadows them; a rowid RANGE arm is a counted seek + Nexts on the
  table b-tree with no movetos.
- **A subquery bound as a branch constraint runs ONCE** — the 3.4.2
  leak was never the binding (evalConstExpr already evaluates
  uncorrelated Subqueries) but the fetch filter re-running the FULL
  WHERE per candidate. C compiles consumed terms into the seek; the
  residual is only what is left. The consumed/residual split is the
  reusable fix shape (branch constraints + orBranchPrefix's
  consumed-const set -> OrBranchPlan.Residual -> orRowAfterResidual).
  Oracle pins for the same leak in the PLAIN seek path (carded):
  `a=1 AND b=(SELECT y FROM t4 WHERE x='a')` = 3, with the noncovering
  output = 4; the seek path consumes eq terms as VALUES and re-runs
  s.Where per row at select_index_seek_exec.go:430 /
  select_index_seek_covered.go:186 (seekConstValue does not evaluate
  subqueries, so the term never binds).
- **Oracle byte-reading trap**: `db eval {EXPLAIN ...} v {...}` — the
  v array's keys are addr/opcode/p1..p5/comment; printing by POSITION
  ([lindex $r 0..4]) shifts every field one slot (addr first). The
  addr=... p1=... form reads unambiguously.
- A TCL probe helper that interpolates `db eval` twice (once for the
  result, once inside the return) DOUBLES sqlite3_search_count — the
  whereD oracle numbers only reconcile with a single eval per probe.

- **The residual fold must be exhaustive per constraint class**: folding
  only unpinned EQUALITY subs drops unconsumed RANGE/LIKE/GLOB conjuncts
  from the fetch filter — where7-2.2.1 planned `g=... AND f GLOB ...` on
  the g index and leaked every GLOB reject (+3078 leaves at the gate).
  Consumed = the prefix's branch-sourced eq cols + the driving range's
  col; everything else is residual.
- **C's per-arm RowSetTest fires AFTER the arm's residual terms**
  (wherecode.c's OR loop: residual -> IdxRowid -> RowSetTest -> Gosub):
  dedup-before-residual LOSES rows (a row arm i rejects must stay
  available to arm j — where7-2.4.1's b=407 row behind the g+f GLOB
  arm). The OR walk dedups by rowid only after the owning branch's
  residual accepts. whereD's fully-consumed arms cannot expose either
  bug — the GLOB-arm shapes needed the full-suite gate.
- **Fixture `resultTransform: lsort:integer` makes the row SET the
  contract** — where7's count_steps_sort bodies sort before comparing,
  so OR-plan arm ORDER is unobservable there; the oracle pins the order
  only for unsorted shapes (whereD 6.5.4's [9 5]).
- The 3.51.0 oracle can DISAGREE with a fixture's baked expectation when
  the source predates a planner change — where7-2.4.1's fixture want
  [33 37 49 51] is the old planner's full-scan order; the modern
  testfixture returns 49 51 33 37 (the transform reconciles them). Tag
  such observations to the oracle version before modeling.

- **A NULL-constant comparison never binds as an index term**:
  `col = NULL`/`col == NULL` is SQL-NULL (never true) — the OR prefix
  matcher's nil Val means "col IS NULL" (different semantics!), so
  extractEquality/extractRange must REJECT nil bounds and leave the term
  residual, where its evaluation rejects every row
  (rowvalue2-4.IDX2.4, oracle-verified: both the row-value form and the
  expanded OR return nothing for the NULL row). HEAD masked the whole
  class by re-checking the full WHERE per candidate.
- **C's deferred row fetch is two-phase, at most once per candidate**:
  phase 1 = the arm's residual reads (BEFORE the RowSetTest — rejected
  and duplicate candidates count; where8-1.10's rejected row 10), phase
  2 = the output block (AFTER the RowSetTest — emitted rows only;
  1.4a's duplicate row 10 in the GLOB arm does not count). A branch's
  index covering a phase's columns serves that phase without the fetch
  (aAltMap); a rowid branch's cursor is positioned directly (never
  fetches). Oracle decompositions are the arbiter: 1.10=7, 1.4a=10,
  whereD-6.3.2=5, 3.5.2=3. The per-EMITTED moveto model undercounts
  exactly the residual-rejected candidates.
- **Probe schemas must be copied from the SOURCE test, not reinvented**:
  where8's t1(a, b TEXT, c) has NO rowid alias — a probe with
  `a INTEGER PRIMARY KEY` routed the arm to the rowid path and measured
  5 vs the true 7, costing a false "oracle disagrees" conclusion. The
  modern testfixture passes its own where8.test (0 errors / 2052) — the
  fixture expectations are modern-C-correct there.

## S22 record (2026-11-11) — the single-table ORDER BY delivery slice

- **The seek threshold has no 10% cliff in C.** With stat1 present, where.c
  compares total path costs and the seek wins at ANY selectivity below a
  full scan (probed 3.54: SEARCH at ~12% over stat1 "100 8 1", at 50% over
  "1000 500 1", and seek+TEMP-B-TREE when the index does not deliver the
  ORDER BY). A tie (est == nRow, stat1 "1000 1000 1") goes to the SCAN.
  The old `bestEstimate < nRow*0.10` rule made the executor's
  scanLoopForQuery return kind=loopNone for where-7.x while the EQP
  renderer printed the right SEARCH — the planner/executor disconnect
  class again, surfaced by the harnessSortMarker.
- **The ORDER-BY index resolution stack now walks the rowid tail**
  (where.c's nColumn = nKeyCol+1): a FINAL rowid-named ORDER BY term
  (pseudo-name or IPK alias) rides the index entry's trailing rowid —
  where-12.2's `ORDER BY b, a` over (b) — no collation, direction follows
  the scan. findIndexForOrderTerms is refactored into
  forEachOrderServingIndex (ALL serving candidates; where.c enumerates
  every candidate and lets the cost pick).
- **A covering ORDER-BY-serving walk beats a seek when the sorter's priced
  cost makes it so** (where-6.7.x: `WHERE b>0 ORDER BY a` over the
  analyzed t3 plans SCAN t3 USING COVERING INDEX t3acb). The port is C's
  wherePathSolver comparison in wherecost.go: LogEst/LogEstAdd/estLog
  verbatim from util.c, the seek rRun = LogEstAdd(rLogSize, nOut+1+
  (15*szIdxRow)/szTabRow) (+ the table-lookup term when not covering),
  the sorter rSort = LogEst(nRowEst) + LogEst((nResultCols+59)/30) +
  estLog(nRow) with C's MIXED-DOMAIN LIMIT cap (the plain count compared
  against the LogEst, where.c:5455), seek +3, walk -2. Row widths:
  szEst 5 for the BLOB/TEXT-affinity standard typenames, else 1
  (build.c:1537), +1 for the rowid; szTabRow adds +1 when no IPK. The
  model reproduces both probed boundaries (walk wins the 100-row shapes;
  the seek wins at 100k) — the boundary between is unprobed, carded.
- **The `db status` counters reset per statement** in the engine
  (engine_core.go ResetStmtStatus) — the harness's SortMarker PEEKS the
  sort counter, so a "sort" verdict is always THIS statement's; there is
  no leftover-carrier trap.
- **Tcl option ABBREVIATIONS in the converter**: `lsort -dec` hit the
  unknown-option skip and silently baked ASCENDING expectations (whereC's
  10 `.3` cases: the source sorts [lsort -integer -dec $res] = [5 4], the
  fixture wanted [4 5]). The engine was RIGHT. Fix:
  lsortOption() unique-prefix matching over lsort's option set
  (-dec→-decreasing, -int→-integer, -incr→-increasing). The stale-
  fixture signature extends to "expectation baked from the WRONG helper
  shape": where2-7.2/7.3 carried `stmtStatus` (scan N sort M) where the
  source's cksort emits the sort marker (nosort|sort), and whereF-5.x
  carried `stmtStatus` the source never emits. Hand-pin those steps in
  place — a full where2 regen DROPS 19 passing hand-pinned cases
  (6.14-6.17, 16.1, 17.x), so never install it wholesale.
- **The vmstep surface is unexpressible**: SQLITE_STMTSTATUS_VM_STEP
  counts real VDBE opcodes; the tree-walking executor has no opcode
  stream to count. whereF's 5.2/5.4/5.6 (`expr [db status vmstep]<200`)
  are removed from the fixture (the queries themselves run in 5.1/5.3/
  5.5) — same card as e_walauto's -shm byte introspection.
- The where census after the S22 slice: 78 -> 47 (where 26->10, whereC
  10->0, whereF 3->0, where2 6->4). Remaining classes: the JOIN loop
  machinery (where8 16 fullscan-step counter = the inner loop must be
  index-driven + covering-preferred; where9 8 scan counter = the same
  covering inner loop; where 6.15 + 14.x = per-loop ORDER BY delivery +
  the order-distinct mark-off), whereH 5 + where2 12.1/16.2 (the
  candidate-choice class: longer equality prefix wins, join order),
  wherelimit2 4 (LIMIT-on-DML + INSTEAD OF trigger + log UDF), where
  19.0/25.x (EQP autoindex names, corruption).

## Gate health — never block on a possibly-stuck test (2026-11-12, directive)

- **Every full gate MUST carry `-timeout`** (40m; the carded walfault-11
  512-trial cap legitimately eats ~12 min). The timeout is the backstop:
  on wedge, go test panics with a goroutine dump that names the stuck
  test — never launch a gate bare.
- **Prefer `-v` on full gates.** Non-verbose gates buffer all output
  until exit, so a hung run shows a 0-byte log and you cannot tell which
  family is in flight. With `-v`, `tail` the log for the last `=== RUN`
  line — instant stuck diagnosis. (Grep `--- FAIL:` lines only for
  diffs; RUN lines pollute substring greps.)
- **Before waiting on a background gate, prove it's alive**: two `ps`
  samples ~20s apart — the accumulated TIME must advance (real work) and
  STAT should be R. A 0-byte log alone proves nothing (buffering).
- **Bounded waits only**: poll with short sleeps; never sit in an
  unbounded block on a gate task. If wedged: TaskStop, then re-run the
  suspicious family in isolation before blaming the engine.
## S25 record (2026-11-12) — wherelimit2 4 -> 0 (the UDF-global round trip + the CTE-header UDL strip)

- **A general-path harnessTclFunction UDF writes INTERP globals, not
  runtime vars** — the fixture's `db func log log` proc
  (`lappend ::log {*}$args`, fired from INSTEAD OF trigger bodies)
  evaluated correctly in its own tcl.Interp, but `::log` never reached
  the runtimeVars map the runtime_var step reads (got [""]). The fix is
  the SAME round trip the rtgeom syncVars / unlock-notify eval_script
  syncs use: collect the write targets (`tcl.NotifyScriptVarTargets`
  over the proc body + callback procs), PUSH present runtime values into
  the interp before each call, PULL the written values back after.
  Pure-Go probe first proved the ENGINE side (trigger firing + UDF args)
  was already correct — the got-[""] signature was a harness gap, NOT an
  engine gap and NOT a stale fixture (setup_0 already carried the
  register_function).
- **Push-before-invoke is REQUIRED, not just pull-after**: the
  regenerated fixture's `runtime_var_reset log` (the source's
  `set ::log {}`) would otherwise leave the interp's ::log holding 5.4's
  list and 5.5 would append onto it. The reset propagates into the
  function interp only if the shared-global mirror runs before every
  call. Push only vars PRESENT in runtimeVars (a reset makes the key
  present-and-empty — push that); absent vars are not the test's shared
  state and stay untouched.
- **The UDL pre-parse strip's CTE-header walk must know the
  materialization hints**: isTopLevelStmt allowed only
  WITH/AS/RECURSIVE/, as depth-0 keywords before the DML keyword, so
  `WITH t2 AS MATERIALIZED (VALUES(5)) DELETE ... ORDER BY ... LIMIT 2`
  (wherelimit2-6.1) was left un-stripped and the LALR tables rejected
  ORDER. Adding MATERIALIZED/NOT completes the vocabulary; no other TCL
  source combines a materialization hint with DELETE/UPDATE, so the
  where/wherelimit sweeps were the only exposure. Oracle (/usr/bin/
  sqlite3 3.54, tagged): the CTE does NOT shadow the DELETE target — the
  statement deletes rows 1,2 from the REAL table (result 3 5 8 13),
  matching the fixture; frigolite's collect->sort->limit delete executor
  already evaluates `ORDER BY rank() OVER ()` per row correctly.
- **Plain SELECT `ORDER BY rank()OVER()` errors in frigolite**
  ("wrong number of arguments to function rank()" — the window context
  is lost when an ORDER BY term is evaluated outside the select list;
  `SELECT x, rank()OVER() ... ORDER BY 2` works). The DELETE-with-limit
  path does NOT hit it. Carded as a window-function execution residue
  (G4), untouched by this unit.
- Fixture regen discipline held: tclconvert -only wherelimit2 diff vs
  committed = name-set equal, 31 cases byte-identical, only
  __RESET_DB__ steps null->[] and setup_0's converter drift (callbackProcs
  + newer callbackVars keys) besides the wanted runtime_var_reset steps.
S25 final counts: wherelimit2 4 -> 0; wherelimit sweep 5 -> 0; where
sweep 86 -> 81 (fixed = exactly the wherelimit2 five); regressions = []
everywhere; go vet + go build clean.

## Flake watchlist (2026-11-12)

- **temptable2-4.1.1** — the documented timing flake (S21 record).
- **where2-2.4** — NEW: three `SELECT * FROM x1, x2 WHERE x=1 ORDER BY
  random()` runs must each reshuffle 50 rows (C: P(collide) ~ 1/50!;
  fnRANDOM = global rand.Int63 — consecutive draws cannot repeat).
  Failed once in the S25 merge gate (`$out1!=$out2 && $out2!=$out3` got
  [0]), passed gates 1-3 and 6/6 isolated reruns + 2 family runs.
  Suspect: shared harness runtime-variable state under full-suite load,
  NOT the engine. If it recurs in the next gate, dig the harness
  query_assign/runtime_expr path with dlv; until then treat as flake
  (do not chase single occurrences).
## S24 record (2026-11-14) — the proper-superset candidate choice + the CROSS-join join order

- **The whereH "proper superset" choice is decided by whereLoopInsert's
  subset-cost adjustment, not by the default ladder alone**
  (where.c whereLoopCheaperProperSubset + whereLoopAdjustCost +
  whereLoopFindLesser): a loop whose constraint set is a proper superset of
  an existing loop's is forced strictly cheaper (rRun=min, nOut=min(
  existing−1)); a proper subset strictly costlier. whereH-1's trace proves
  it load-bearing: t1abc and t1bc's raw ladder arithmetic TIES at nOut 12
  and the subset rule breaks the tie to 11. Probe order = the pIndex
  prepend chain = REVERSE creation order (adjustments are insertion-order
  dependent; both probe orders still elect t1abc, but by different
  adjustment directions).
- **The verbatim no-stat1 pricing chain**: default ladder build.c
  sqlite3DefaultRowEst aVal={33,32,30,28,26} then 23, a[0]=max(nRowLogEst,
  99) with nRowLogEst=200; nOut(eq prefix)=aiRowLogEst[nEq]; one range
  bound: whereRangeScanEst −20 per bound, −20 more for both bounds, −1 per
  bound, floor 10 — **the −20 comes from WhereTerm.truthProb DEFAULTING TO
  1 (whereexpr.c:83), not from a TUNING constant**; then
  whereLoopOutputAdjust (only when single table AND an ORDER BY exists —
  C's early-out) −1 per unconsumed term with the iReduce cap (10 for an
  unused `col = int in [-1,1]`, else 20) against aiRowLogEst[0]; rRun =
  LogEstAdd(estLog(aiRowLogEst[0]), nOut+1+(15*szIdxRow)/szTabRow) plus
  LogEstAdd(..., nOut+16) when not covering — priced from the PRE-adjust
  nOut. szIdxRow/szTabRow are LogEst(w*4) values and the (15*x)/y ratio
  consumes LogEst inputs directly.
- **Debug-shell oracle recipe**: gcc -O1 -DSQLITE_DEBUG=1
  -DSQLITE_ENABLE_WHERETRACE -o /tmp/.../sqlite3dbg shell.c sqlite3.c
  (never build inside the shared ../sqlite tree); `.wheretrace 0x44ff`
  dumps every candidate loop ("add:/skip:/replace:" + the final list);
  whereLoopPrint's cost tuple is (rSetup, rRun, nOut); estLog(x) =
  sqlite3LogEst(x)−33. The 3.54 where.c source fetch: sqlite.org/src/raw/
  <artifact-hash> with the hash read from the tag's MANIFEST (the raw URL
  by tag name returns the manifest).
- **3.54 changed the CROSS-join prereq rule** (where.c:4960-5017,
  hasRightCrossJoin): a CROSS edge |= mPrior at its term AND latches, so
  the per-term `mPrereq = 0` reset stops for ALL later FROM terms — a
  comma-joined term after a CROSS edge inherits the CROSS term's prereq
  (probes: `t1, t2 CROSS JOIN t3, t4` gives t3 AND t4 prereq {t1,t2};
  `t1 CROSS JOIN t2, t3, t4` leaves t2..t4 at {t1}; LEFT edges still
  reset). 3.51's `hasRightJoin` (LTORJ only) does not latch on CROSS —
  the 3.51 tree alone MISDIAGNOSES 16.2's contract; validate against the
  3.54 CLI (/usr/bin/sqlite3) whenever the shape involves CROSS.
- **Our parser normalizes every join operator through joinTypeOf, whose
  default returns "CROSS"** — a comma join carries JoinType "CROSS" PLUS
  CommaJoin=true. Any jointype-driven logic (prereqs, reorder rules) must
  consult CommaJoin; keying on the JoinType string alone turns every comma
  join into a CROSS edge (it regressed `t1, t2 WHERE t2.c=1` to t1-first
  before the flag check landed).
- **Landing**: select_order_loopcost.go prices the ORDER-BY-delivering
  seek candidates per the chain above and runs the subset adjustment
  (terms = conjunct indexes = C's WhereTerm identity); the elevation lives
  in preferOrderByServingIndex's chosen-delivers path (the chosen-not-
  delivering tranche path is untouched); both EQP and the seek executor
  flow through scanLoopForRowCount so plan and execution stay coherent.
  The join EQP grew joinPrereqs (the 3.54 rule) with driver/next gating in
  explain_join_order.go (moved out of explain_plan.go for the size
  budget). whereH 5 -> 0, where2-16.2 fixed; where* name-set diff vs the
  base commit: exactly those 6 fixed, zero new; orderby/index/analyze/
  intpkey/select/join/limit/like all 0/0; TestS22_/TestSOLID_ green.
- **where2-12.1 is CARDED** (the OR-loop-in-join planner unit): C's plan
  is the MULTI-INDEX OR as table b's JOIN loop (whereLoopAddOr inside a
  join: arms b.x=rowid-seek + b.y=t12y-seek, prereq 0; a searched inner
  via t12y(y=b.x)) — needs OR loops as join-loop candidates + the
  MULTI-INDEX OR EQP block; overlaps S23's active join machinery
  (select_join_scan.go) — schedule after the S23 merge settles. The
  fixture's single-table OR machinery (S21) does not reach joins.

## S23 record (2026-11-14) — the join-loop machinery (P-C1, the where census 40 -> 15)

- **The join ORDER BY delivery is a WHOLE-PATH analysis, not a per-table
  one** (where.c wherePathSatisfiesOrderBy over the loop path): the
  per-loop walk (the eq pre-pass over conjuncts whose other side is
  already-positioned, the index-column walk from the seek's eq prefix with
  the rowid tail, the nullable-unconstrained order-distinct void and the
  rowid-match restore) is the single-table shape per loop, but the
  obSat/obDone/mask state is CROSS-LOOP: the mark-off (isOrderDistinct ->
  orderDistinctMask |= the loop) retires every term whose operand usage
  the mask covers — so a UNIQUE base index can retire the whole ORDER BY
  before the inner loop is ever examined (where-14.7.2's x.a||x.b), and a
  rowid match restores distinctness even for a voided non-unique loop.
- **The loops must EMERGE in the claimed order**: the base pick is
  recorded from actual execution (rowid seek / index seek / walk / plain
  scan / OR loop), and each real-table inner operand materializes into its
  pick's stored index order (C's covering walk) — the permutation by the
  index b-tree's rowid sequence reproduces C's constrained-inner seek
  order too (the filtered subset preserves the walk's relative order).
  The claim declines when the base direction is backward (the executor's
  base runs forward only — carded) and at LEFT/RIGHT/FULL joins.
- **A delivered claim owns the loop order**: the comma-join swap
  (indexed-side-inner) must be declined when the claim held — C kept the
  ORDER-BY-delivering loop outer (where-6.15's t3 outer via its a-walk).
- **C's OR-arm execution order is NOT the written order** (three oracle
  probes + the source): sqlite3WhereExprAnalyze walks the clause
  BACKWARD, every two-column comparison gets a commuted virtual copy
  (exprAnalyze's copy+commute), and wherecode.c Case 5 runs only terms
  whose normalized leftCursor is the OR table — so operand-column-left
  arms (and AND-spine arms, literal-left arms) run in written order, then
  the commuted copies of outer-column-left arms run in REVERSE written
  order (where8-3.11's (a=d OR b=e) emits the e-arm first; (b=e OR a=d)
  emits the d-arm first). The CASE-1 same-column shape (every arm a
  single eq on one operand column) rewrites to one IN seek — the arms
  stay in written order (where2-6.14.1).
- **The OR plan's every-arm rule is the no-OR-loop rule**: an arm without
  an indexed operand column voids the whole OR plan (where8-3.9's c='IX'
  arm — C scans t1 instead; where9-2.5/2.6's all-indexed arms form the
  MULTI-INDEX OR). The join BASE routes through the S21 branch machinery
  on the FROM-only conjunct view; the INNER loop runs a per-outer-row
  arm-order pass over the materialized right maps (the arm exprs evaluated
  on the combined maps, dedup by right-row identity after the arm
  accepts, the ON's remaining conjuncts still filtering). Gated off
  RIGHT/FULL joins (their unmatched tracking needs the full sweep —
  join7-1.116) and correlated rights. The ORDER BY claim marks an
  OR-pass operand non-deliverable (C's OR loop carries no index).
- **A correlated comparison bound is a runtime constant** (where8-3.14's
  `e = b` seeks i4 per outer row; 3.15's `t2.d > inner.d` ranges i3): the
  planner's collectBinaryRef gains the orientation-aware seekRefSides
  (the OWN-table column side pairs with a literal/param/deterministic/
  correlated bound; the operator FLIPS when the column is the
  written-right — the executor's term extraction flips the same way, so
  the (column, operator) pairs must agree), the executor's
  seekOuterBoundValue binds through OuterRowsForResolution (innermost
  first, Get(qual) then Get(name)), and the whereref memo REFUSES the
  correlated-bound shapes (the slot-path replay re-reads literal leaves
  only). The correlated-bound check consults the statement's FROM operand
  scope: a JOIN SIBLING's column is never a bound (its value is only
  computable at that operand's own loop level — whereE-1.1's SCAN t1
  stays); the scope is set per statement in execRealTableSelect and
  explainQueryPlanSelect.
- **bestIndexForRowCount must resolve the REAL table name**: the aliased
  FROM term (t2 AS inner) rendered as the alias and every schema lookup
  (findIndexOnColumn) failed — an aliased single-table seek planned SCAN
  pre-P5 (`FROM t2 AS inner WHERE d=4` planned SCAN, C plans SEARCH inner
  USING COVERING INDEX i3). The whereref memo key stays correct: the
  where-AST pointer is unique per statement instance, so alias variants
  never collide.
- **Stale fixture (converter artifact)**: where8-5.3's do_execsql_test
  result list "1 {}" was baked onto an exec step — the harness reads
  "1 {}" as rc-1 + braced-message = an ERROR expectation, and the query
  passed only while the engine ERRORED. The S23.2 loop picks made the
  LEFT-JOIN-OR query succeed (C-correct: oracle "1|") and unmasked it.
  Hand-pinned to a query step. The general signature: an exec step whose
  expect is "N {...}" for a do_execsql_test whose LAST statement is a
  SELECT.
- **The join EQP's inner OR loop**: one "INDEX n" sub-plan per arm in
  execution order, each arm's SEARCH naming its driving index (C's
  ExplainQueryPlan in Case 5; where-19.0's t191 via
  sqlite_autoindex_t191_1/2). The single-table seek planner must resolve
  the REAL table name for the same reason as above.
- **Sweep discipline**: the baseline for regression diffs must come from
  a DEDICATED baseline worktree (git stash without -u leaves untracked
  new files and breaks the baseline build — the "0 failures" baseline
  was a build failure, and the resulting diff invented 28 regressions);
  grep the harness logs with -a (the logs contain binary bytes).

## S29 record (2026-11-14) — the expr slices: E3 (regsub -all + fts3expr3 regen), E1/E2 (fts3 exprtest), S1/S2 (CAST/truthiness/var-limit)

- **TCL `regsub` option args are leading dash-args** (`regsub ?switches?
  pattern string subSpec varName`): the converter took args[0] ("-all") as the
  pattern, so `regsub -all AND $node OR node` matched the literal "-all" and
  NO substitution happened — fts3expr3's andor wants froze as the raw
  "AND xxx xxx" template (the E3 class). Fix: skip/apply the switch set
  (-all → ReplaceAllString; -nocase → (?i); unknown dash-arg →
  UnsupportedCommandError, never silently skipped — the lsort -decreasing
  lesson), non-all → FIRST match only (TCL semantics; ExpandString for the
  $-expansion). The SAME regsub class also froze the fixture's INPUTS: the
  old ReplaceAllString-without--all numbered every placeholder with the same
  i, the correct first-match-per-iteration loop numbers them 1..n — so a
  regen changes SQL and expects together; both were self-consistently wrong
  before, which is why only the andor leaves failed.
- **A conversion-time error raised AFTER a statement was captured must not
  become the catch's message**: fts3expr3's catch bodies emit the query, then
  the strip_phrase_data recursion over the runtime placeholder hits the
  proc-depth limiter — the old cmdCatch branch order overwrote the dynamic
  $msg with the literal "proc call depth exceeded". Statement capture is now
  authoritative (checked before err). The seven depth-limit leaves emit the
  correct [$__catchrc_msg, $msg] runtime_list with expect
  "1 {Error parsing expression}".
- **A bare `catch { ... }` do_test body value is the catch rc**: stamp a
  deterministic __catchrc variable (commandCatch) and emit runtime_list
  [$__catchrc] (cmdDoTest) — leaving expect "0" on the query step compared
  the tree RENDER against the rc (fts3expr3-4.1.2).
- **The exprtest surface reports every parse-stage error uniformly**
  (fts3_expr.c:1263-1265): the "FTS expression tree is too large (maximum
  depth %d)" pzErr goes to the MATCH path only; fts3ExprTestCommon prints
  "Error parsing expression" for balance/depth failures too. And the C
  NEVER type-checks exprtest argv: zTokenizer/zExpr/azCol all come from
  sqlite3_value_text (fts3_expr.c:1233-1245) — integer 1 parses as "1".
  SQLITE_FTS3_MAX_EXPR_DEPTH is 12 (fts3Int.h:66), not 100; 4.1.2's
  4000-term AND chain pins it (balanced depth ≤ 12 parses OK).
- **The converter writes ONE output and removes the stale alternate**
  (main.go:237-253): the fts3expr3 .json+.json.gz double-family duplicate
  resolves itself on regen — the >1MB fixture stays .gz, the stale .json is
  deleted by the converter. Verify with `git status testdata/`.
- **The oracle keeps NUL bytes IN the value; the stop is the TCL adapter**:
  hex(CAST(X'670068006900' AS text)) = 670068006900 (3.54 oracle) — the
  triage's "truncate in evalCastExpr" was wrong. testfixture renders row
  values with Tcl_NewStringObj(z, -1) (tclsqlite.c:1866): the harness's
  formatSQLiteValue truncates TEXT at the first NUL. length()/quote()'s
  NUL rules are separate engine surfaces (carded).
- **CAST's arms switch on the declared type's AFFINITY, not the name**:
  sqlite3ExprAffinity's TK_CAST case derives it via sqlite3AffinityType's
  substring rules (build.c:1650) — util.Affinity IS the engine's faithful
  derivation (DDL uses it; verified equivalent for CHAR/CLOB/TEXT/BLOB/
  REAL/FLOA/DOUB/INT-window priority), so evalCastExpr switches on
  util.Affinity(v.AsType) and the exact-name switch + isTextAffinityType
  died. CAST('def' AS shobblob_x) is blob, 'abbLOb10' blob, 'shint_x'
  integer, 'clob' text.
- **CAST-NUMERIC keeps REAL-syntax text REAL** (sqlite3VdbeMemNumerify,
  vdbemem.c:781): integer-SYNTAX text (AtoF rc 0/1) goes through
  sqlite3Atoi64 (return ≤ 1 → INTEGER; trailing junk/spaces tolerated,
  2^63 fits negative only); REAL-syntax text folds only when
  sqlite3RealSameAsInt — bit-equal to its int64 AND |i| < 2^51
  (vdbemem.c:756). The old lossless-int collapse at ANY magnitude made
  '9.223372036e15'/'9.0e+18' INTEGER. e_expr-32.2.8's `||''` rendering of
  9223372036854775808.0 wants the 17-digit shortest-round-trip form — the
  A9 harness-real-rendering layer stays carded.
- **CASE WHEN truthiness is the REAL-prefix rule with AtoF's dangling-
  exponent ignore**: C's AtoF treats 'e' without [sign]digits as consumed-
  but-invalid (eValid=0) and the VALUE is the significand — the old
  scanRealPrefix kept the 'e' in the prefix ('1english' → '1e' → ParseFloat
  error → 0.0 → false). The shared port is value.RealValuePrefix; iif() is
  INLINEFUNC_iif in C (compiled to CASE), so fnIfIIf's isTruthyValue calls
  the SAME helper ('1engl' → true, 'engligh'/'0' → false, oracle f/f/f/t).
- **A prepare-time parameter error is VALUE-INDEPENDENT**: C prepares the
  UNEXPANDED text, then binds $refs (tclsqlite.c) — the converter's
  bindSQLParams and the harness's expandTCLRuntimeVarsSQL both emulated
  binding by INLINING the value, which deleted the parameter and hid the
  limit error (e_expr-11.7.1: `SELECT ?32766, \$::a` — the named parameter
  takes slot 32767 → "too many SQL variables"). Fixes on both layers:
  bindSQLParams keeps ::-prefixed (global-namespace) references as
  parameters (globals bind AFTER prepare), and expandTCLRuntimeVarsSQL
  keeps the original text when the unexpanded SQL already breaches a
  parameter limit (exec.CollectParameterNames, guarded on '$' presence so
  $-free SQL pays nothing). The engine's exec/params.go
  (MaxVariableNumber=32766) was already faithful — the triage's "fix
  resolve slot assignment" pointer was stale.
- **The e_expr fixture was NOT wholesale-regenerated**: the regen drifted on
  7024/32210 cases ($a2 → {} expect freeze — the current converter's
  runtime-var machinery bakes different text than the committed one), so
  leaf e_expr-11.7.1's first step is HAND-PINNED instead ("SELECT ?32766,
  22" → "SELECT ?32766, $::a", one string replacement, JSON re-validated).
  The S22 lesson (never wholesale-install drifted regens) held; the e_expr
  converter drift is a carded item for a future converter-alignment slice.
- Family census after the slice: fts3expr3 52 → 0; fts3 sweep 106 → 54
  (exactly the 52 fts3expr3 leaves + the pre-existing fts3* residue);
  e_expr 21 → 12 (A3×2, A4, A5×2, A6×3, A7 gone; A1×8/A2/A8×2 are
  converter slices S3/S5; A9 32.2.8 needs the real-rendering layer).

## Gate exclusivity + corrupted faultsim logs (2026-11-14)

- **A full gate needs the box's CPU**: a concurrent agent's family sweep
  (walfault!) starved the carded walfault-11 stretch into the 40m
  timeout (2400s gate). Either run gates when agents are idle or budget
  -timeout 60m when sharing.
- **Faultsim runs corrupt the gate log in place** (binary junk interleaves
  with the -v stream — awk chokes on multibyte). A FAIL extraction from
  such a log can silently show "0 failures in 347k tests" — the
  empty-failure-set trap's third face. Verify a suspiciously-clean gate
  log: `grep -c "=== RUN"` sanity, no-binary-junk check, and targeted
  family sweeps for the known-failing set.
- **The single-leaf harness filter runs the leaf WITHOUT its family's
  setup** (FRIGOLITE_TEST=where-25.5 alone skips setup_2's corruption →
  the case trivially passes). Verify in FAMILY context, never by
  leaf-name filter, when the case depends on setup steps.
- **The seek-collation gate** (collectBinaryRef): C's sqlite3IndexCollSeq
  rule — a comparison serves as an index term only when its effective
  collation (explicit COLLATE on either operand, else the column-operand
  side's declared) equals the index column's (the DDL's COLLATE, else the
  column's declared). A recreated same-name index with a different
  collation (existsexpr-4.3's DROP+CREATE) is the trap shape.
- **temptable2-9.2.x (+4.1.1, 5.1.2 when they dither)** — the
  small-cache stress class (cache_size=15 + auto_vacuum + randomblob
  churn + exact page-count assertions): dithers 13→3→2 across back-to-back
  family runs, identical profile at the pre- and post-collation commits
  (bisected via a worktree). The four historical gates drew clean
  samples; its true baseline is "sometimes ~14". Class unit: the
  small-cache temp-db churn (spill/latch timing) — separate from the
  timing flake temptable2-4.1.1.
## S30 record (2026-11-14) — pager1 slices U1-U4 (subst-foreach, __STEP_ column text, readonly surfaces, URI + CACHE_MISS)

- **An unknown command inside a `[subst {...}]` list silently deleted its
  element and shifted every foreach column after it** (the U1 class):
  pager1-7.1's `foreach {tn sql res js ws} [subst {...}]` lost one element
  per unconverted `[wal_file_size ...]` (wal_common.tcl is `source`d, and
  `source` is a converter noop), collapsing rows 6-10 into the 3 garbage
  cases (`sql: "exclusive"`/`sql: "-1"`). The substituteCmd policy comment
  ("a failing command substitution must abort") never fired because
  dispatchCommand's unknown-command tail returns nil — the error was only
  sticky on the evalWord path. Fixes: (a) implement the sourced helper proc
  the file needs (wal_file_size = `32 + (pgsz+24)*nFrame`); (b) commandSubst
  now propagates the sticky substErr so a subst-context unknown command
  aborts the conversion loudly instead of baking shifted lists.
- **Reproduce a converter bug with a minimal interp Execute before
  modeling**: the minimal foreach+subst repro (with/without the proc
  defined) proved the binding-shift mechanism in one run — the
  element-count model in my head was wrong twice before that.
- **Hand-pin, never wholesale-regen, when the converter drifts**: the full
  pager1 regen DROPS pager1-16.1.* x25 and adds setup_249-308 x60. All
  fixture edits were hand-pins of exactly the affected cases; the fixed
  converter's regen output only VALIDATED the pinned shapes (7.1.6-10, 39.x
  markers, 4.7.2/4.8.x/37.1/43.x steps).
- **sqlite3_column_text is a value carrier, not a status carrier**: the
  do_test body `sqlite3_step $S; sqlite3_column_text $S 0` evaluates to the
  CURRENT ROW's column 0. The converter now emits `__COL_<handle>_<idx>__`
  (commandColumnText) and the harness records the formatted current row per
  handle at stmt_step (main handler) and expands the markers at
  runtime_assert. The __STEP_ STATUS carriers stay untouched — every LIVE
  __STEP_ user (analyze3, blob, misc6, tkt2820) expects a status name; the
  value-expect families (trace3, capi2) are skip-listed and inert.
- **The engine's prepared statement already survives same-connection DDL
  mid-cursor** (step 2 returns the SECOND row after CREATE TABLE) — the
  pager1-39.2 contract needed zero engine work; probe first.
- **C's locking_mode SET form reports the PAGER's mode, the bare QUERY form
  reports db->dfltLockMode** (pragma.c:687: eMode =
  sqlite3PagerLockingMode(pPager, eMode) for the set, `eMode =
  db->dfltLockMode` for the query). And the SET is REFUSED while
  sqlite3WalHeapMemory holds: a WAL opened while the pager was exclusive
  keeps a heap wal-index (wal.c sqlite3WalOpen's bNoShm ← pager.c:7596
  pPager->exclusiveMode) — pager1-7.1.6.1's `PRAGMA locking_mode = NORMAL`
  must return "exclusive". Port: Pager.walExclusiveLatched (latched by
  SetWALExclusiveRequested even before a WAL exists) → walWriter.heapMemory
  at openWal → Pager.WALHeapMemory; the exec LockingMode gate skips
  heap-WAL pagers and splits the set/query result. Verified: after
  journal_mode=TRUNCATE tears the WAL down, the same NORMAL set takes
  effect.
- **C's sqlite3BtreeCursor folds a schema-root cursor (iTable<=1) on a
  ZERO-PAGE database to iTable=0 — an empty cursor** (btree.c:4693): a
  readonly connection cannot bootstrap page 1, so the empty-file schema
  read yields NO ROWS, never "malformed". Our OpenCursor/OpenCursorAtRoot
  now return an endOfBTree cursor for rootPage<=1 && NumPages()==0 — the
  read-only fallback open of a wiped 0-byte file (pager1-4.8.x's state
  before the restore lands) scans empty like C. The writable path was
  already correct (schema Init bootstraps the header in cache).
- **frigolite.Open already implements the URI semantics the pager1-37.x
  test needs** (sharedMemURI routes file:?mode=memory&cache=shared to a
  shared in-memory store) — the converter was the only gap: commandSQLite3
  collapsed the URI to filepath.Clean("") = ".". URIs now pass through
  INTACT (the harness's reopen_db detects the "file:" prefix and opens
  verbatim), EXCEPT the deletedFiles lifecycle branch, which keeps the
  bare basename (8_3_names's forcedelete→file:./test.db?8_3_names=1
  models the harness's lifecycle file; pinned by
  TestSQLite3ReopenAfterForcedeleteURI).
- **sqlite3_db_status CACHE_MISS counts a content-wanted acquire that
  reads the page image from the db file** (pager.c:5615, before readDbPage;
  WAL-resolved reads included; the noContent zero-fill and open-time direct
  header reads excluded). The counter sits in Pager.ReadPage's two paths
  (hit fast path, miss slow path under the write-lock re-check), NOT in
  readPageLocked — pageropen.go/external.go read page 1 through
  readPageLocked directly at open (C's direct sqlite3OsRead, no counter).
  Reset zeroes after reading (sqlite3PagerCacheStat). 43.1/43.2/43.3 = 2/3/1.
- **The U3 readonly surfaces**: `file attributes PATH -permissions X`
  parses TCL's rwxrwxrwx string to chmod bits, `-readonly N` toggles the
  write bits on the file's current mode (tclUnixFile.c); a missing file is
  a no-op (the fixture wraps everything in catch{}). `sqlite3 db test.db
  -readonly 1` → reopen_db readonly → OpenReadOnly. sqlite3_db_readonly →
  capi_db_readonly (1/0 via pager.ReadOnly, -1 unknown name). The engine
  already fell back to read-only on an EACCES open (openDatabaseFile) —
  probed before touching anything.
- **U3's 4.5.6/4.7.2/4.8.1/4.8.3 need the B4/B5 substrate**: C's readonly
  SELECT fails because a readonly connection cannot roll back a HOT
  JOURNAL restored by the tvfs xDelete→faultsim_save snapshot (4.7.1's
  commit). Our engine writes no on-disk journal (U6) and the fixture has
  NO tv machinery (U7) — the pinned restore wipes test.db, so 4.8.1's
  readonly EXPECT reads a missing (writable, empty) file. Fixtures now
  carry the faithful steps; the leaves go green when U6/U7 land.
  4.8.2 (`sqlite3_db_readonly db xyz` → -1) is a NEW leaf, green.
- **Quality gate delta**: commandFile's attributes branch pushed it to
  gocyclo 13 — extracted commandFileAttributes. Check the gate on changed
  non-test files BEFORE committing: several target files already exceed
  the 1000-line hard cap at HEAD (interp_cmds 2019, interp.go 2113,
  pragma_state 1059, wal 1249) — pre-existing, but a new function-level
  violation blocks.
- Family diffs (failure NAME sets, pristine-vs-changed): pager1 100→90
  (10 fixed: the 3 U1 garbage names + 39.1/39.2 + 37.1/37.1.2/43.1-3; new
  green leaves 7.1.6.1-10.1 + 4.8.2), pager family sweep identical except
  pager1, wal 97→97 identical, wal2 26→26, wal3 6→6, jrnlmode*/lock*
  0→0, tkt2820/analyze3 identical-ok, blob/misc6 pre-existing failures
  only (misc6-1.1, blob-3.2, e_blobclose-1.2/3.3 fail at the pristine
  commit too). wal4/walbak/walfault diffs recorded in the S30 sweep logs
  (/tmp/s30_cleanbase_* vs /tmp/s30_after_*).

## S32 record (2026-11-14) — the expr converter batch: A1 (foreach var-init) + A8 ($as$bs expects) + D1 (malloc-test reset)

- **A `foreach {v…} {list} break` binds its variables BEFORE the body's
  break runs** (Tcl binds the first chunk, then runs the body): the
  var-init idiom's globals survive the loop. The converter's cmdForeach
  bound them only in its own interp (conversion-time `$` substitution), so
  the harness's `var()` UDF (registered via setup_15714's register_function,
  reading runtimeVars) saw stale values — runtimeVars["b"]="23" was seeded
  earlier by setup_15700's register_function callbackVars, and every CASE
  WHEN var(...) test picked 'B'. Fix: cmdForeach detects the break
  idiom (bare `break` body or braced body whose FIRST command is break),
  emits one runtime_expr step per variable with the first-chunk value
  (numeric bare / braced literal / empty for past-list-end = the harness's
  empty-reset), and binds only iteration 0.
- **The runtime value's TCL OBJECT TYPE must cross the UDF boundary**:
  e_expr-22.1.1's `CASE var('a') WHEN 3` compares TEXT vs INTEGER with NO
  affinity (oracle 3.54: `CASE '3' WHEN 3` → NULL, `'3' = 3` → 0), so a
  TEXT '3' from the harness never matches. The 3.51.0 testfixture pins the
  object duality: braced-literal foreach items are TEXT (e_expr-25.1.3's
  `CASE '0' WHEN var('a')` REQUIRES text), while `[list [expr 3] ...]`
  items are INTEGER (22.1.1 REQUIRES integer). Fix: the converter marks
  bracket-evaluated lists' canonical-numeric elements with
  `valueType: "integer"/"real"` (braced/bare list words stay untyped); the
  harness stores the type on a NUL-prefixed side key
  (`\x00rttype:<name>` in runtimeVars — no Tcl name contains NUL, nothing
  iterates the map wholesale) and the var() UDF returns int64/float64 for
  typed values; runtime_expr/runtime_var_reset clear the marker on
  re-assignment. A strings-only runtimeVars map is an approximation: any
  NEW UDF that reads a global into an SQL comparison must check the type
  key first.
- **The A8 split condition was an off-by-one that never fired**: the
  frozen expect "$as$bs" is ONE parameter reference to scanTCLParamRef
  ('$' is a name byte), and the split-at-inner-'$' branch tested
  `key[idx+1] == '$'` — but the inner '$' sits AT idx, so the remainder
  never starts with '$' and the branch was dead (a scratch probe of
  expandTCLVarRefs with both values present reproduced it in one run).
  Fix: split when `values[key[:idx]]` exists, resume the scan at
  i+1+idx (the inner '$'), so chains ("$a$b$c") split iteratively; a
  whole-key hit still wins; "$tcl" (no '$' in the key) never enters the
  branch. The e_expr-5.1/5.2 fixture needed NO change.
- **do_malloc_test resets the database per trial** (malloc_common.tcl
  forcedelete test.db/-journal/-wal + open empty): the converter dropped
  the whole command, so fts3expr-4.1's t1(a,b,c) survived into 6.1 and its
  `CREATE VIRTUAL TABLE t1 USING fts3(a)` failed "table t1 already
  exists". Fix: commandDoMallocTest = reset_db marker + -tclprep/-sqlprep/
  -tclbody/-sqlbody/-cleanup in order under the malloc test's name; the
  SQL body is a plain query/exec step (an engine error fails the test —
  the trial's do_test asserts {1 1}, i.e. the body succeeds on the no-fault
  trial); unknown options → UnsupportedCommandError. -start/-end/-testdb
  are not needed by the corpus (fail-trial side effects are erased by the
  next trial's reset, so the observable contract is reset+success).
- **Hand-pin the pins in source order, keep the name list in sync**: the
  first e_expr.json re-pin dropped the `names.insert` bookkeeping and all
  five new setup groups stacked at the SAME stale index — the family ran
  all four groups before 21.2.1 and three leaves regressed. The debug
  trail was the interleaved `=== RUN` + value-set log, not the leaf list.
  Also: JSON rewrites of the fixture must reproduce Go's MarshalIndent
  byte-for-byte (indent "  ", HTML-escaped `< > &` as \u003c/…, no
  trailing newline) or the diff explodes to 780k lines.
- **Regen-vs-hand-pin verdicts**: fts3expr.json regenerated (trial regen
  with the fixed converter was byte-identical to committed before the D1
  change — the installed diff is exactly the two malloc groups + resets).
  e_expr.json hand-pinned (the regen drift re-measured at 7024/32210
  cases / 7021 drifting steps: register_function callbackVars bake a flood
  of runtime-var placeholders the committed fixture lacks) — but the
  pinned region was cross-checked against the fixed converter's trial
  regen: SAME steps at the SAME positions, only the sequential setup
  group NAMES shift (15715a… vs the regen's renumbering); content-equal.
- Quality gate: isCanonicalFloat started at gocognit 19/gocyclo 24 and
  commandDoMallocTest at gocyclo 14 — rewritten (charset scan + ParseFloat;
  parseMallocTestArgs/execMallocScript/emitMallocSQLStep split). The
  converter files' gate failures are grandfathered (commandSet 69/61,
  interp.go/interp_cmds.go > 1000 lines at dd045611d too); staticcheck
  diff vs baseline = line-number shifts only.
- Family diffs: e_expr 12 → 2 (closed 21.1.1-21.1.4, 21.2.3, 21.3.1,
  21.3.2, 22.1.1, 5.1, 5.2; remaining 26.1.6 A2 + 32.2.8 A9 are engine
  slices); fts3expr 1 → 0 (6.1 closed; fts3expr-malloc-1/-2 are NEW green
  subtests); fts3 substring sweep 54 → 53 leaves (only fts3expr-6.1 gone,
  zero new); expr substring net 98 → 26 (triage residue minus this unit's
  and S29's closures: e_expr 2, expr 3, expridx1 10, indexexpr2 9,
  indexexpr3 2).
- **atof1-2.40** — `SELECT randomblob(0) - 1` (randomblob(0) yields a
  1-RANDOM-BYTE blob; the arithmetic result varies per run: got [4] want
  [-1] in the S32 gate, 0/3 family reruns). Same probabilistic class as
  where2-2.4 — never chase single occurrences.
## S31 record (2026-11-16) — pager1 U5 complete: the failed-COMMIT abort path, rowid-alias aggregates, lockout pins

- **A failed COMMIT must still roll the transaction back even though the
  commit teardown already cleared the transaction flags** (vdbeaux.c
  sqlite3VdbeHalt's abort path): `commitClearTxState` set
  inTransaction=false AND nilled txSnapshots BEFORE the pager flush, so the
  flush-failure `execRollback()` (a) bailed at the "cannot rollback - no
  transaction is active" guard and (b) found no snapshots — the connection
  kept the mid-txn pager state (grown header nPage + fileSize cache vs the
  un-extended file) and the next read failed "database disk image is
  malformed" (pager1-11.5). Fix: `rollbackOpenTx()` = the rollback body
  without the guard; execRollback = guard + rollbackOpenTx; the
  commitFlushAllPagers failure path calls rollbackOpenTx directly; the BEGIN
  snapshots stay alive until AFTER commitFlushAllPagers succeeds.
- **flushPage's xWrite gate runs after the cached fileSize / header nPage
  bump** — a VETOED page write still grows p.fileSize and
  growHeaderSizeLocked's in-header count in memory. That is survivable ONLY
  because the failed commit now restores the whole BEGIN image; without the
  restore the vetoed write leaves header claims leading the file.
- **Statement-boundary cache spill (CacheFlush) is the engine's mid-txn
  file-writer** (cache_size=10 + a 32-row UPDATE): the probe must replicate
  EVERY fixture ingredient (cache_size, row widths) or the failure will not
  reproduce — the plain probe COMMIT had nothing to flush and never failed.
- **The previous instance's `[open hdr]` / `[pgno>N]` debug tags in
  user-visible error text would have corrupted exact-match catchsql
  comparisons** (matchExpectation is exact; only the harness's regex/glob
  wrapper forms relax it). Reverted before S31.6; error-text probes must be
  removed before commit, not landed.
- **U5 is COMPLETE (11 leaves)**: 44.1/44.3 (S31.1), 38.1#01 (S31.2),
  14.1.4/14.1.6 (S31.3), 42.1 (S31.4), 21.1/21.2 (S31.5), 11.1/11.2/11.5
  (WIP + S31.6), 8.2.2 (WIP). pager1 90 → 79.
- The C 11.x lockout pins: `tv script lockout` + `tv filter {xWrite
  xTruncate xSync}` are set BETWEEN 11.1 and 11.2 (they veto 11.2's COMMIT
  only); `tv script {}` clears at the END of 11.2 (C's line between the
  catchsql and 11.3) — db2's 11.3 open + journal_mode=TRUNCATE must NOT be
  vetoed; the filter (without a script) stays a no-op through 11.5.
- Sensitive-family sweep at S31.6 (failure NAME sets, pristine vs changed,
  two directions): trans/jrnlmode/incrvacuum/lock_/lock3/pagerfault/pager2/
  pager3/trigger2/fts4merge4 all 0; savepoint 15 and pager4 6 identical to
  pristine (pre-existing). staticcheck U1000 joinOperandColumnSide
  (select_join_or.go) is pre-existing at the WIP commit.

## S31 record (2026-11-16, cont.) — U6 hot-journal substrate (S31.7)

- **Hot-journal recovery runs at the per-statement shared-lock point, NOT at
  Open** (pager.c sqlite3PagerSharedLock → hasHotJournal → pager_playback):
  C's sqlite3PagerOpen only reads page 1 raw. The faultsim fixtures restore a
  hot journal onto a CLOSED snapshot and expect the sidecar to survive the
  reopen until the first statement (pager1-4.5.4's hexio_write hits the
  journal AFTER a restore_reopen) — an Open-time replay consumed it. Port:
  `Pager.RecoverHotJournalIfNeeded` called from exec's checkDBFileCtx before
  CheckExternalFileErr; the old recoverJournalAtOpen is gone.
- **C's hot classification is three-way, not two-way** (hasHotJournal):
  nPage==0 → DELETE the journal (journal1-1.2's remnant); first byte == 0
  (PERSIST's zeroJournalHdr, or a never-synced header) → NOT hot, file LEFT
  on disk; first byte nonzero but header unparsable (bad magic / sector field
  out of [32,0x10000] / page field out of [512,65536], zero page field =
  pre-3.5.8 compat → the pager's own size) → DELETE (playback's
  readJournalHdr returned DONE); a valid header → play records FORWARD
  (pgno==0 stops, pgno>dbSize skips, the pager_cksum sample-byte checksum
  STOPS the walk — 4.5.5's partial playback), truncate the db file to the
  header's dbOrigSize, write every before-image to the FILE (the heal must be
  visible to later connections — a cache-only replay left the crashed image
  on disk), re-seed the header from page 1's before-image, pager_reset the
  cache, mxPgno adjusts upward, then unlink.
- **PERSIST finalize must zero the journal header** (pager.c zeroJournalHdr):
  28 zero bytes at offset 0 (or a 0-truncate when journal_size_limit==0 or
  the super-journal path), then the size-limit truncation. Without it the
  leftover PERSIST journal still carries a valid magic + the last txn's
  records, and the new hot-journal probe plays it back over the NEXT
  database at that path — pager1-10.x's reopen failed "database disk image
  is malformed" (30 leaves).
- **The commit's db-file xSync runs BEFORE the journal finalize** (C's
  CommitPhaseOne tail vs pager_end_transaction): a faultsim save keyed on
  the db xSync (pager1-4.3.1's xSyncCallback) must capture the journal still
  on disk. Likewise the journal xClose/xDelete events fire BEFORE the actual
  os.Remove (C's testvfs intercepts xDelete INSTEAD of performing it;
  pager1-4.5.1's copy_on_journal_delete faults in on the pre-delete state).
- **InstallVFSHook now wires the journal sidecar events into the fixture
  hook** (xOpen/xClose/xDelete via SetJournalFileOpHook): C's testvfs
  intercepts every method of every file it backs; pager1-4.5.1's save keys
  on the journal xDelete. journal2's own SetDefaultJournalFileOpHook path is
  untouched (per-pager hook precedence) and stays green.
- **A nil *testing.T in faultsimFileOp panicked at t.Helper()** — the
  fixture-callback faultsim_save registration passes nil; nil-safe now
  (log.Printf instead of Errorf). A family-run PANIC truncates the run: a
  "fixed" name-set computed from a panicked run silently covers only the
  leaves BEFORE the panic point — diff by failure name set only after a
  run that COMPLETED (grep for "panic" first).
- **pager1 census after S31.7: 79 → 63** (16 fixed: 4.3.2-10, setup_11/12,
  4.5.3/4.5.5/4.5.6, and 4.8.1/4.8.3 as a side effect of the recovery
  timing + readonly-hot-journal refusal). The remaining 63 = the U7
  super-journal bucket exactly (4.4.x 54, 4.6.x 4, 4.7.2 — their failure
  shapes: ::mj_delete_cnt got [""] (no super-journal exists → no mj
  deletes), the 4.6.x multi-file restore cascade (t1.2 vs t1.1), 4.7.2's
  readonly-journal open).
- Sweeps (failure NAME sets, pristine-vs-changed): journal1/journal3/
  jrnlmode*/corrupt*/pagerfault/incrvacuum/trans/pager2/pager3/lock*/quick
  all 0; journal2 5, savepoint 15, pager4 6, walfault 6 identical to
  pristine.

## S31 record (2026-11-16, cont. 2) — U7 super-journal + faultsim (S31.8): pager1 100% green

- **The multi-file commit needs the super-journal to be a COMMIT POINT, not
  decoration** (vdbeaux.c sqlite3VdbeCommit): nTrans counts write-transaction
  databases whose journal mode carries an mj (aMJNeeded: DELETE/PERSIST/
  TRUNCATE), whose safety_level != OFF, and that are not memory — nTrans>1
  routes zSuper through every pager's CommitPhaseOne. The engine flow:
  create `<main>-mj%06X9%02X` (antepenultimate '9') + write each
  participating journal's NAME (dbPath+"-journal") NUL-terminated + sync →
  flush every pager (each appends its trailing POINTER block:
  [SJ-pgno sentinel][name][len][cksum=sum(name bytes)][journal magic] — the
  sentinel pgno stops the hot-journal record walk) → delete the mj (the
  xDelete event routes through the main pager's fixture hook — the
  copy_on_mj_delete save + ::mj_delete_cnt fire HERE, journals still on
  disk) → CommitPhaseTwo per pager (the journal finalize). A pager whose
  journalMode is "" participates: the empty mode IS the DELETE default
  (a pager attached after `PRAGMA journal_mode=DELETE` never sees the
  pragma).
- **Each journal's trailing pointer gates its own playback** (pager.c
  readSuperJournal, read from the FILE TAIL: len@szJ-16, cksum@szJ-12,
  magic@szJ-8, name@szJ-16-len): a journal naming an EXISTING super-journal
  belongs to an UNCOMMITTED multi-file txn (roll back); naming a MISSING one
  means committed (delete, no rollback — 4.4.x.7's "no master-journal ==
  no rollback"). pager_delsuper after a playback: read the mj's journal-name
  list; delete the mj only when no OTHER journal still points at it
  (4.6.x's per-database deletion cascade — the last journal's playback
  removes the mj).
- **PERSIST/TRUNCATE journal reuse must RE-INITIALIZE the epoch** (fresh
  header via writeJournalHdr): the sidecar stays open across COMMITs, and
  the post-finalize epoch (TRUNCATE zeroes the file, PERSIST zeroes the 28
  header bytes) must not be mistaken for a live one — without a
  journalEpochLive gate the next txn appended records into a ZEROED journal
  (no header → the hot probe saw first-byte==0 → LEAVE → no playback →
  4.4.1.p1/t1.6's aux never rolled back).
- **tester.tcl's db_save/db_restore are hardcoded to the test.db\* glob**
  regardless of which file the connection was opened on (pager1-4.6.8's
  `sqlite3 db test.db2` leaves db.path=test.db2 while the snapshot pair
  still walks test.db\*) — faultsimMainPath now returns the test.db
  lifecycle path unconditionally; the REOPEN target is C's $dbfile argument
  (faultsim_restore_reopen's optional step.SQL; reset_db likewise carries
  faultsim_delete_and_reopen's $prefix — 4.4.2's padded prefix IS the main
  database file, and its `.7` delete_glob must use the padded prefix for
  the -mj files).
- **The hot-journal playback reopens the journal READWRITE** (pager.c:5329):
  a journal without WRITE permission (4.7.2's chmod r-------- journal)
  fails SQLITE_CANTOPEN "unable to open database file" and stays in place.
  The readonly-CONNECTION refusal (4.5.6) and the readonly-JOURNAL refusal
  (4.7.2) are separate checks in RecoverHotJournalIfNeeded.
- **Converter-dropped fixture steps must be re-derived from the C source,
  not invented**: 4.7.3's chmod+delete_file+file-exists steps were dropped
  by the converter (the leaf carried only close_db), which left the 4.7.2
  journal alive into 4.8.1's window and broke the readonly-fallback leaves
  that had already passed. The file_attr/delete_file step shapes come from
  the S30 pins (4.7.2/4.8.x).
- **pager1 census: 79 → 0** (S31.6 -1: 11.5; S31.7 -16: 4.3.2-10,
  setup_11/12, 4.5.3/4.5.5/4.5.6, 4.8.1/4.8.3; S31.8 -62: the 4.4.x
  super-journal bucket (23+34), 4.6.x (4), 4.7.2 (1)). The family runs 957
  leaves, all green, exit 0.
- U7 sweeps (failure NAME sets vs pristine): trans/trans2/trans3, attach,
  attach2, lock_/3/4, jrnlmode/2/3, corrupt/2, journal1/3, pagerfault,
  walfault, incrvacuum, pager2/3/4, quick, e_walckpt, e_walptr, auto_vacuum,
  trigger2 — all 0 or identical-baseline; journal2 5 / savepoint 15 /
  pager4 6 / walfault 6 / wal 97 / wal2 26 / trigger 25 all identical to
  pristine (two-direction name-set diffs).

## The 1355 baseline was UNDERCOUNTED (2026-11-16)

- **/tmp/full_s33_gate_fails.txt (1355) lost 75 leaf lines** to the s33 gate
  log's faultsim NUL corruption — the extraction used a line-based method
  (`strings`-style) that collapses NUL-heavy regions and DROPS the FAIL lines
  inside them. Robust re-extraction of the SAME raw log
  (/tmp/full_s33_gate.log, still on disk) with the python raw byte scan
  (`re.findall(rb'--- FAIL: TestSQLiteSuite/([^\s(]+)')`, leaf-only = names
  containing '/') yields **1430 = /tmp/full_s33_true_fails.txt**.
- **Rule: extract from corrupted faultsim logs with the python raw byte scan
  ONLY.** `strings(1)`/grep-line methods silently undercount (my first
  strings extraction found ONE FAIL line in a log with 1556). Validate any
  extraction: leaf-only count + spot-check a family known-failing.
- **Validate suspicious GONE sets the same way**: e_dropview's 14 leaves
  "passed" at baseline only because the lines were eaten — bisected at
  dd045611d (pre-S31) reproducing the identical failure set (a
  wt_basecheck worktree run). A "fix" that no commit explains = extraction
  loss, not engine improvement.
- The true S31-merge delta vs 1430: **4 NEW (pragma2-5.1, pragma2-5.3,
  sharedA-1.1, sharedA-1.2 — all reproduce at HEAD, 0/3 at dd045611d) /
  112 GONE (89 pager1 + alter2 13 + analyze-5.99, fkey6 2, index3-99.1,
  main-2.0#01, tkt-f3e5abed55-2.5, trigger7-99.1 — the non-pager1 GONEs
  need per-leaf verification before crediting S31)**.
- Agent sweep lists need the FULL family universe: S31's 31-family sweep
  missed pragma2/sharedA (the regressions) AND e_dropview/skipscan5/tkt-*
  (the phantom news). The full-suite gate is the only complete net.

## S34: hot-journal nPage classification must be FILE-SIZE-derived (2026-11-16)

- **C's hasHotJournal (pager.c) classifies via pagerPagecount, and
  pagerPagecount's nPage at the OPEN state is the DATABASE FILE SIZE via
  xFileSize, ceil-divided by the page size — NEVER the pager's in-memory
  page count.** nPage==0 ⇒ the journal is "a remnant from a prior database"
  and is DELETED without playback, before any header read.
- **Frigolite's schema.Init() materializes page 1 in memory for an empty
  file, so a freshly-reopened empty db reports pager.numPages==1.** Any
  probe keyed on `numPages==0` therefore NEVER fires for the exact state
  C's nPage==0 branch exists for (db file deleted, stale journal left) —
  the stale journal classified HOT, playback truncated the fresh 0-byte
  file to the journal header's dbOrigSize and wrote skipped-range
  before-images: old schema resurrected ("table t1 already exists",
  pragma2-5.1) or header-over-zeros image ("database disk image is
  malformed", sharedA-1.1/1.2). Fix: Pager.hotProbeDBPageCount —
  p.file.Stat(), ceil(size/pageSize).
- **Diagnostic pattern that nailed it in ~30min:** (1) per-step fixture-dir
  probes in the harness (log file sizes before each step, print only on
  CHANGE) showed test.db-journal surviving ROLLBACK/COMMIT/close while the
  attached db's journal was deleted; (2) an S34DBG-gated print + the call
  site in RecoverHotJournalIfNeeded pinned WHO ran the playback and with
  WHICH numPages (1) — the wrong signal was visible in one line.
- **engine residue vs classification: fix the classification, card the
  residue.** The DELETE-mode journal left on disk after a snapshot-based
  ROLLBACK (Restore never ends the journal epoch) and after a close with
  zero dirty pages is a real divergence from C (rollback/close remove it),
  but with the classification fixed the next open handles the residue
  C-style (delete, no playback) — no currently-green fixture observes the
  residue, so re-plumbing rollback through journal playback is carded, not
  done (PLAN_S34_regress.md §Carded).
- pragma2-5.3's cache_spill was PURELY downstream of 5.1 (aborted batch
  never ran cache_size=2; the max(cache,spill) read then reports 2000).
  Check what a failed earlier statement in the SAME exec batch means for
  the observed pragma state before treating a pragma read as an
  independent bug.
- **S33 WR index payload format (baa8dfebb)**: C stores a WITHOUT ROWID
  secondary index record as [indexed cols..., PK cols in PK key order...]
  (build.c ~4269 "Append the table key"), with isDupColumn eliding PK
  columns the index keys already carry, and the tail ordered under the PK
  columns' collations + DESC flags. The synthetic-rowid tail we wrote
  before was unresolvable against the PK-keyed WR table btree, which made
  the where-25.5 corrupt recheck impossible and left stale entries after
  DELETE (delete payload [key..., rowID] != insert payload [key..., rowID]
  because the delete's rowID read the WR cell's synthetic 0).
- **WR tail plumbing patterns**: (a) one plan resolution per index —
  wrTailPlanMemo (schema-stamp keyed, the ikiStamp pattern); (b) the tree
  comparator must cover key + tail — installIndexWriteOrder extends the
  memoized base KeyInfo per call (two slice copies, never mutates the
  memo); (c) seek probes carry only the probe's fields (C's nField rule —
  btree.IndexRecordCompare already probes len(probe.Values)); (d) the
  seek's lower-bound landing is conclusive ONLY under BINARY PK collations
  (wrPKSeekApplies gate) — a NOCASE PK row can sort outside the binary
  window and a false "corrupt database" on a healthy db is worse than no
  recheck.
- **WR UPDATE targets collide on the synthetic rowid**: every WR index
  cell carries RowID 0, so an update-side delete map keyed by rowID
  collapsed all changes into one target (only the last delete payload
  survived). Key by the OLD PK tail's value string (unique per row). Also
  C update.c's chngKey rule (~566): a WR PK-column change must maintain
  EVERY index (the tail rides every record) even when no index KEY column
  changed.
- **CREATE INDEX backfill on WR must remap before BuildRowMap**: the
  stored record is PK-first; the positional row map read storage slot 0
  as declared column 0 and stored wrong key values on non-identity
  layouts. RemapWRRecordToDeclared (now on DDLContext) before the row map.
- **Probe verification workflow that worked**: engine-built dbs in
  /tmp/s33/probe/go + python3 -I dump_index.py (raw btree leaf parser,
  record decode, stored order) diffed against /tmp/s33/oracle/*.db dumps
  — byte-equal values AND order on all five format contracts in one shot.
- **Flake watchlist addition (2026-11-16, S33)**: temptable2-9.1.x — a
  bulk `DELETE FROM tx WHERE (random()%3)==0` on a ROWID temp table
  (attached db, auto_vacuum=1, cache_size=15, two blob-keyed indexes)
  sometimes leaves surviving rows' entries missing from index i1
  (integrity_check: "wrong # of entries in index i1" + "row N missing").
  PRE-EXISTING: fired 1/6 family-sweep runs at the pre-S33 baseline
  9a8391251 (base fam run 6) as well as on S33 code (~1/3). NOT an S33
  regression (tx is a rowid table; its integrity passes were never
  exempted). Needs its own slice: capture a firing seed, then bisect the
  bulk-DELETE index-maintenance funnel (delete.go maintainIndexesOnDelete
  vs the scan that collects deleted rows) under auto_vacuum + tiny cache.
- **WR table walks must use isTable=false**: a WITHOUT ROWID table's btree
  is index-shaped — interior divider cells ARE rows (R13 moved one cell per
  split boundary up). Any walk that opens a WR table tree with
  isTable=true silently skips every divider row (CREATE INDEX backfill
  short counts, scanTableForMatch misses rows). The flag must follow the
  TABLE SHAPE resolved in the statement's OWN schema (temp/attached
  shadowing!), not a hardcoded true. Fixed in a5ea11125 for the DDL
  backfill, the DML uniqueScanTree funnels and integrity's per-row walk;
  audit the execconstraint/alter sites when they next surface.
- **sweep.sh gotcha**: grep treats some failure logs as BINARY (blob
  values in got/want lines) and drops the leaf extraction — use `grep -a`.

## S37 record (2026-11-16) — the authorizer/update-hook event collection slice

- **The authorizer's event stream is a COMPILE-TIME (codegen) event, not a
  runtime one.** Every SQLITE_READ the FK machinery issues fires from
  sqlite3FkCheck's codegen walk (fkey.c:977-990 parent-key permission in the
  FIRST loop; resolve.c:842 via fkScanChildren's scan-WHERE child-key
  resolution in the SECOND loop — the parent side of the scan WHERE is a
  TK_REGISTER and reads no auth) and from sqlite3FkActions' action-trigger
  compilation (fkey.c:1416 → fkActionTrigger → codeRowTrigger, trigger.c
  1272-1348). update.c calls FkCheck TWICE per UPDATE (update.c:1049 regOld +
  1093 regNew) → child-side parent-key reads and parent-side scan reads
  fire ×2 for UPDATE, ×1 for INSERT/DELETE. The trigger subprogram compiles
  ONCE per statement (getRowTrigger cache) and is anonymous (malloc-zeroed
  zName) → its events carry arg4="". Resolution order inside the trigger:
  WHEN(old.pk IS new.pk — 2 parent-col reads, UPDATE only) → SET resolve
  (1 parent-col read for CASCADE-UPDATE) → per-column stmt auth (update.c
  :508) → WHERE resolve (parent col then child col — the OLD. term is the
  LHS) → the child's own FkCheck first-loop reads (×2 UPDATE / ×1 DELETE)
  → the child's FkActions (recursion). without_rowid3-18.5's 11-event
  sequence is the calibration target — match it event for event.
- **The second-loop scan skip is INSERT-specific** (fkey.c:1025-1036 — its
  own comment: "inserting a single row into a parent table cannot cause or
  fix an immediate violation"; the condition's assert documents
  regOld==0 && regNew!=0). UPDATE/DELETE pass isMultiWrite-equivalent (the
  row loop is a multi-write-capable statement — update.c:758/1319 set the
  flag for any non-single-pass shape). Model: skip immediate scans only for
  a single-row INSERT parent, keyed off the same singleRowInsert rule
  validateDMLFKPrepare already computes.
- **UPDATE statement auth shape**: update.c:508 fires SQLITE_UPDATE ONCE PER
  CHANGED COLUMN in SET order with the column name as arg2 ("ROWID" when the
  rowid pseudo-alias is assigned and the table declares no such column);
  each target column resolves BEFORE its auth request (C interleaves lookup
  + auth, so "no such column" precedes "not authorized"); the table lookup
  precedes everything. INSERT (insert.c:969) and DELETE (delete.c:393) pass
  arg3 = the target's schema name — the old engine shape passed "" for both
  arg2 (UPDATE) and arg3 (INSERT/DELETE).
- **The harness authorizer interp needed the S25 push/pull round trip**
  (frigolite_tcl_authorizer_test.go): recording procs append to fixture
  globals (`lappend ::authdata [lrange $args 0 4]`, savepoint.test:564) in a
  PRIVATE tcl.Interp; NotifyScriptVarTargets over the proc bodies names the
  sync targets; push present runtime values before EVERY invoke (a
  runtime_var_reset between leaves must land in the interp) and pull after.
  auth/auth2/auth3/hook are `unsupportedTestFiles` — the harness regression
  surface for authorizer changes is the auth_proc-USING fixtures:
  alterauth/alterauth2/altercons2/analyze3/fkey2/fkey7/fts4aa/savepoint/
  schema/schema2/tclsqlite/without_rowid3.
- **tkt-f7b4edec is an UPDATE-HOOK test, not an authorizer test** (`db1
  update_hook update_hook`, args = INSERT/main/t1/rowid) — the triage's
  bucket-F label was loose. The converter silently dropped the named-handle
  command; the engine surface already existed (FirePreupdate → updateHook).
  The named-handle branch (interp_cmds.go) emits an `update_hook` step
  (whitelist it in tclconvert main.go — empty-SQL marker type) and the
  harness handler registers SetUpdateHook with the same push/pull.
- **The reset steps were the second half of every collection fix**: the
  section-level `set authargs {}` / in-body `set ::authdata [list]` resets
  had been dropped by the older converter, so events accumulated across
  leaves even with sync working (fkey2-18.3's got began with 18.2's events).
  The current converter emits them (regen-verified) — but the regens pull in
  unrelated converter drift (savepoint's delete_file/repeat/close_db shapes;
  without_rowid3's 10.2.x SQL corrections; tkt-f7b4edec's setup_1/2 resets
  became runtime_expr with an EMPTY expr that errors). Hand-pin the reset
  steps instead, copying Go-marshaled blocks from the regen output for
  byte-exact format; the `tests[]` anchor sits INSIDE an already-opened
  object — inserting a `    {` group before the `"name":` line duplicates
  the opening brace (hit twice; repair by replacing the damaged block).
- **Fixture-lifecycle nuance**: savepoint-9.4-9.6 pass vacuously — the catch
  arm's comparison for a non-pair-list expect only asserts res.Error != nil
  (harness:5370-5393), so the baked authdata prefix in their catch expects
  is inert. Adding collection cannot break them; still pin their resets for
  C faithfulness.
- Census after the slice: savepoint 8→5 (bucket-K residue), without_rowid3
  21→16 (bucket-J residue + 18.8/18.11 IGNORE), fkey2 22→17,
  tkt-f7b4edec 2→0. 15 leaves closed (10 planned + 5 fkey2 bonus).
