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
