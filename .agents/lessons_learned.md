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
