# Lessons Learned — Frigolite

- Compatibility harness sharding must assign deterministic indices only after existing family filter/skip rules; equivalent `.json` and `.json.gz` fixtures with one basename must remain in the same shard. Use complete family-level elapsed durations as greedy allocation costs when available, explicitly flag interrupted/unmeasured families and retain fixture bytes/case counts as fallback. Full-run orchestration needs Bash PID collection and isolated working directories; wait for every child and persist each exit status before claiming results.

Durable, reusable guidance only. Keep session-specific evidence in plans and research notes. Current project instructions and `plan/GUIDELINES.md` take precedence.

- When rewriting unpushed commits to remove generated `portplan/FULL_SUITE*.stdout` files, preserve full worktree/index backups first; `git filter-branch --index-filter` must run with a clean tree. Verify ancestry, commit count, and paired old/new tree deltas to prove only target paths changed. Auxiliary refs/stashes retaining the original history make `git log --all` detect those artifacts; archive originals externally, then ensure temporary preservation refs/stashes do not defeat reachability checks.
- JSON harness must mirror per-case `tcl2go` N-A skips: prefixes' Tcl `\\uXXXX` foreach inputs lose backslashes during tokenization; `prefixes-1.*` then have wrong common-prefix values and `prefixes-2.*` recursive CTEs can consume the full harness timeout. Keep invalid cases skipped consistently while preserving independent native/API coverage.
- ALTER DROP COLUMN must inspect table-level constraints as well as column flags: `PRIMARY KEY(b,c,d)` does not mark each `ColumnDef.PrimaryKey`, so dropping `c` otherwise succeeds incorrectly. Validate compound/table-level PK membership via parsed table constraints before row/schema rewrite.
- View dependency analysis must preserve identifier casing and remove SQL quotes before schema lookup: uppercasing `"rename_dst"` yielded literal `"RENAME_DST"`, which schema lookup did not resolve. Keep original SQL token for lookup; normalize only comparisons.
- CLOSE 1.3/1.4 uses sqlite3_close_v2 and sqlite3_column_text APIs; JSON conversion drops those calls and compares the next stmt_step status against close/text expectations. Skip those assertions and only dependent finalize steps; use exact family filter `^close$` because `FRIGOLITE_TEST=close` also runs `e_blobclose`.
- CHUNKSIZE 2.0 requires switching pager to WAL mode; current pager does not implement journal-mode activation. Skip only WAL activation case; verify generated package and preserve DELETE-mode chunk-size assertions.
- BTREEFAULT-2.2 deletes from a table inside per-row Tcl `db eval` while scan remains active; a plain JSON query keeps the deleted row because it cannot run callback mutations. Classify as harness inability, not cursor result mismatch; separate native callback test needed if engine-visible behavior must be pinned.
- SQLite `blob.test` wraps `execsql` bytes in Tcl `bin_to_hex`; JSON query_assign must hex-format []byte only for this fixture when comparing, not globally (other TCL tests expect raw blob text). Keep engine-returned bytes unchanged; validate focused blob JSON and testgen/blob.
- BIND2 uses sqlite3_bind_value_from_select/preupdate APIs and a preupdate hook; JSON can keep bare prepare/step but lacks bound `IntReal` values and callback result state. Skip dependent assertions and their orphaned finalize setup block; verify testgen/bind2.
- BESTINDEX5 expectations prepend `WHERE ...` strings recorded by Tcl xFilter callbacks to SQL rows; JSON query_assign sees rows only. Skip assertions carrying that callback marker, and verify full generated bestindex5 package.
- BESTINDEX1 `4.1` asserts Tcl global list populated by virtual-table `xBestIndex` callback invocations; JSON runtime_var does not capture callback Tcl state. Preserve engine coverage through generated bestindex1 suite.
- BADUTF2 fixtures construct SQL in Tcl variable `$sql` and bind deliberately invalid UTF-8 strings; JSON loses interpolation/bind values, producing syntax errors on literal `$sql` and empty statement output. Restrict skip to those dynamic-variable and 4.1 binding cases; verify generated badutf2 package.
- BACKUP2-3.1 converter can omit `db restore` while assigning its expected error to following `ROLLBACK`; compare original TCL operation order before attributing unexpected ROLLBACK success to the pager. Skip only malformed block; verify `testgen/backup2`.
- AUTOINC-8.1 JSON splits a Tcl block and attaches final SELECT row count `1` to C-API status `SQLITE_OK`. Skip only that malformed runtime assertion; skipping whole case suppresses database close/reopen lifecycle and causes later readonly/cascade failures.
- AUTOIndex1 `113` checks sqlite3_log callback state; `310` expects updates executed inside per-row Tcl `db eval` callbacks. JSON can preserve visible SQL but loses these callback effects. Skip callback-dependent assertions and verify generated package.
- ATTACH3 `db_list` is a Tcl projection helper over `PRAGMA database_list`; converted expectations containing only database names cannot be compared against full PRAGMA rows. Also verify preceding setup because omitted index recreation can make later DROP INDEX fail. Use `testgen/attach3` for these helper/control-flow contracts.
- ATTACH2 3.x uses sqlite3_prepare/step/finalize statement-pointer API and Tcl catch variables; JSON translation cannot preserve VM handles, and later cleanup can fail due to leaked statement. Skip those cases plus only dependent cleanup; validate full contract through passing `testgen/attach2`.
- ANALYZE5 planner TCL tests compare two rowid result lists and return `ok`; converter may attach `ok` to one query_assign instead of the enclosing comparison. Restrict skips to analyze5 1.*b cases with rowid query_assign/`ok`, and confirm full generated package passes.
- TCL prepared-statement APIs retain statement handles, bindings, authorization callbacks, and transactions across test blocks; JSON harness blocks reset that state. Skip only affected analyze3 statements and dependent follow-ups; verify complete engine contract through passing `testgen/analyze3`.
- ALTER2 error-code checks can aggregate multiple Tcl `catchsql` return values into one static expected string; JSON cannot derive that value when catches omit captures. Confirm original Tcl and passing `testgen` family before skipping only the harness artifact.
- SQLite `do_atdc_error_test` helper emits CREATE TABLE/INDEX setup plus ALTER assertion, but JSON conversion may retain only assertion in a separate test block. Verify generated `testgen` helper coverage, then skip only those setup-less JSON cases rather than misdiagnosing missing table as engine failure.
- Tcl `foreach` wrappers around SQL are control flow, not a single SQL command: converters may flatten an empty-result loop into an unconditional command such as `DROP TABLE ""`. Treat as fixture-conversion failure, verify original `.test` control flow, and skip only that artifact while retaining testgen/native coverage.
- JSON harness cannot faithfully execute `db eval SQL {callback}` lifecycle tests: retain SQL-only generated steps only when callback effects are immaterial; otherwise skip callback case and dependent state assertions, pointing to the full `testgen` family (e.g. trans3 COMMIT/ROLLBACK inside row callbacks).
- Route relative external-file operands through the per-fixture directory, not only ATTACH filenames: `zipfile('test.zip')` can read a stale repository-root archive and cause duplicate-name failures. Keep absolute/URI filenames untouched.
- Do not use `randomblob(0)` to test empty-BLOB arithmetic: SQLite clamps size <1 to one random byte, so numeric coercion and arithmetic vary by random content. Use deterministic `x''` when pinning empty-blob semantics.
- In Tcl `expr`, `==`/`!=` compare numerically only when BOTH operands are numeric; otherwise compare string representations. Harness conditions like `$id==\"\"` must evaluate false for id `1`, not error. Also compare flattened NULL/empty-text equivalence before trimming: boundary spaces encode leading/trailing empty cells.
- JSON harness must not run Tcl-only fixture setup as if it were SQL: `with1-10.*` omitted `insert_into_tree`/`scan_tree` procedures, so recursive paths were wrong despite correct engine behavior. Native-populate the fixture contract and skip only malformed JSON-derived cases; callback assertions and Tcl `lsearch` command substitution likewise require generated/harness support or explicit native pins.

## Debugging and implementation
- Reproduce narrowly before editing; establish exact command, input, output, and earliest divergence. Generated failures may be engine or converter issues: test engine-visible behavior natively before converter changes.
- Tcl `set x [execsql ...]` only maps to `query_assign` when last captured SQL returns rows; never infer command-substitution ownership merely from a preceding captured `exec` (e.g. `[list ...]` doesn't execute SQL).
- Use SQLite as behavioral ground truth. Probe exact SQL, errors, and relevant PRAGMAs; compare intermediate state if final output hides divergence.
- UTF-16-only attach2 tests are wholly guarded by `ifcapable utf16`; engine's UTF-8-only build must omit both setup and dependent assertions, while retaining ATTACH encoding-mismatch semantics in a native header-fixture test. Route named DB opens, relative ATTACH operands, and reopen_db basenames into the same fixture-local directory.
- `DB.QueryEach` implemented by iterating `DB.Query` is only API-level iteration, not streaming: callbacks happen after the executor has completed and cannot affect later JOIN rows. A real-table JOIN can stream by keeping outer B-tree cursor live, computing inner combinations from each decoded outer row, then callbacking complete post-WHERE projection before advancing; after callback nested writes, re-read to trigger cursor restore/reseek and detect whether outer key vanished before continuing inner combinations. If deleted with a successor, call Next once to consume CURSOR_SKIPNEXT while retaining that successor for next outer iteration; regression must cover both deleting final outer key and deleting one before another. Verify mutation timing explicitly; thread yield callback into execution before claiming cursor-driven behavior.
- Callback-time SQL mutation requires a live scan cursor through JOIN-pair emission. Frigolite's real-table SELECT materializes scan rows before JOIN (`select_exec.go:346-353`), while `execJoins` materializes combinations before WHERE/projection (`select.go:667-696`); a post-Query callback cannot match SQLite btreefault-2.2. SQLite `btree.c:756-843,891-918` saves/reseeks other cursors by key before writes; use that only with an actually live outer cursor.
- PRAGMA `full_column_names` star labels must derive source identity per visible expanded column (including aliases/views and canonical schema spelling); qualified stars cannot apply user token casing blindly. Bare references need their resolved source, not just first FROM operand. `USING`/`NATURAL` require accounting for suppressed duplicate right-side columns. `short_column_names=OFF` also preserves explicit qualified reference spelling when `full_column_names=OFF`; primary-key rowid aliases report declared column name.
- `PRAGMA full_column_names=ON` qualifies direct column references and qualified-star expansions, but not names emitted by unqualified `SELECT *`; bare star labels remain bare even with `short_column_names=OFF`. SQLite `select.c:2193-2215` applies full names to `TK_COLUMN`; `*` expands through another path. Keep native regressions explicit about unqualified versus qualified stars.
- Broad test runtime claims require equivalent-scope evidence: `go test ./... -count=1 -timeout 60m` took 2,865s here, with root package ~1,614s; this includes 1,233 fixture files and non-root tests, not directly comparable to SQLite CLI. `../sqlite/testfixture test/quick.test` cannot currently benchmark the SQLite TCL suite: executable won't launch because linked `/opt/homebrew/opt/tcl-tk/lib/libtcl9.0.dylib` is missing.
- For TCL `register_tcl_module` bestindex families, distinguish engine contract from lost Tcl callback state: bestindexB's xBestIndex performs nested `INSERT ... RETURNING` and stores result in a Tcl global that the JSON fixture drops. Port callback reentrancy/rowid behavior directly in a pure-Go vtab test before superseding family; link unsupported entry to native regression.
- For FTS3 stress insertion, `DDLExecutor.readCellByRowID` must use B-tree `SeekToRowID`, not scan from the first cell: CPU profile of `fts3defer` attributed 45s cumulative to `Cursor.ReadCell` in INSERT-SELECT docsize handling; rowid seek reduced observed family wall time 95s→15s. FTS merge's per-document posting de-dup also needs a membership map above a small-list threshold; CPU profile attributed 11s to `mergeDoclistHits` and the same failure-containing family then took ~3.6s. Oracle `go-sqlite3` handles exact 65k-row FTS4 self-copy in ~107ms vs Frigolite ~0.61s; comparison is useful but still shows optimization backlog. Existing `testgen/fts3defer*` suites pass; JSON `fts3defer` retains unrelated ICU tokenizer limitation.
- `PRAGMA index_xinfo(table)` for a WITHOUT ROWID table reports each PK column once, in declared PK order, even when table-level PRIMARY KEY syntax repeats column names; suppress repeated ordinals before appending payload columns. SQLite source `pragma.c:1387-1410` iterates the canonical `Index.aiColumn` array. Regression `TestNativePragmaIndexXInfoDeduplicatesWithoutRowidPrimaryKey`; `without_rowid6` family passes and matches `go-sqlite3` oracle.
- `alterlegacy.test` 11.2/11.7 and `altertab.test` 11.2/11.7 use a Tcl variadic SQL UDF solely to append trigger invocation arguments to a Tcl global. JSON conversion retains SQL but loses proc body and state rendering; a fixture-scoped `trigger` UDF appending nested Tcl lists restores assertions without changing engine behavior. Verify `alterlegacy` and `altertab` JSON families.
- For performance comparisons when SQLite's Tcl `testfixture` cannot launch, use cached `github.com/mattn/go-sqlite3` only in an isolated `/tmp` module (no project dependency), replay the same SQL shape and transaction boundary, and report the scope narrowly. 50,000 individually executed indexed inserts in one transaction measured SQLite 61.45 ms vs Frigolite 154.51 ms (~2.5x) in the same scratch program. This does not equate a 36-minute `go test ./...` run (1,363 packages) with one SQLite suite; use matching workload comparisons and profile dominant Frigolite cases before changing runtime behavior.
- analyze9.test registers Tcl UDF `s(blob)` to render each byte as printable ASCII or `.`; without reproducing that callback, only `sqlite_stat4.sample` rendering assertions fail despite correct sample rows. Keep implementation fixture-qualified by function name/body and verify `FRIGOLITE_TEST=analyze9`.
- A plan-aware virtual-table module must trigger per-left-row JOIN materialization only when its selected xBestIndex plan actually consumes a JOIN-ON constraint (argv binding or Omit). Marking every successful `BestIndexPlan` as correlated repeats full scans for plans driven only by WHERE constraints, duplicating join rows. Regression: `TestTclCallbackVirtualTableJoinRows` alongside `TestNativeBestIndex_leftJoinONConstraint`.
- bestindexE's LEFT JOIN expected xBestIndex observations cannot be emulated by adding `ON` expressions to the current single-table WHERE classifier: SQLite associates each `ON` term with a specific join loop and makes RHS constraints usable only with prior-row bindings. Frigolite currently materializes joined virtual tables without that correlated planner context; safely fixing this requires integrated join-loop/vtab execution, not a global ON-clause pushdown.
- SQLite JOIN-index prerequisites: `whereexpr.c:1167-1174` adds `EP_OuterON` join mask to prereqAll and sets `extraRight = x-1` so ON terms cannot drive indexes on LEFT side; `whereexpr.c:1236-1239` combines `prereqLeft | extraRight` into `prereqRight`; `where.c:4280-4290` computes each vtab constraint's `usable` from current loop mask. Per-join outer-row bindings and ON residual ownership both matter.
- Current broad verifier remains failing (`/tmp/frigolite-go-test-final.log`, exit 1): after a fresh run, re-isolate reported families before fixes. bestindexF's ordered/distinct row expectations depend on testfixture Tcl module behavior and input order; ordinary-table SQL variant matched SQLite CLI, so do not infer core DISTINCT defect from that fixture alone. bestindexE's missing `ON`-term planner offers is a separate genuine implementation gap.
- Incremental vacuum can expose stale PTRMAP_BTREE parent references after balancing: `updateParentChildPtr` says named parent does not reference tail page, yet page remains in the btree. Propagate `incrVacuumStep` errors (never discard them); on failed mapped-parent update, resolve actual edge with `findParentByWalk` and retry before leaving freelist undrained. Verify randomized setup with repeated native sequence (100 runs), integrity check, count/freelist, and JSON family.
- Preserve correct optimization fallbacks. Define eligibility from all consumers/invariants and test parity, failure paths, and boundaries.
- Treat caches/reused objects as ownership contracts; key by all identity dimensions, define invalidation, avoid exposing mutable cache-owned pointers.
- For stateful changes, test rollback, nested transactions, savepoints, page reuse, and error exits.

## Tests, fixtures, and measurement
- WITHOUT ROWID table btrees are index btrees: interior divider cells are rows, so DML scans must use index traversal and mutations must remove exact full-record keys with `DeleteIndexEntries`; leaf-only predicate sweeps skip interior rows, causing UPDATE to retain stale rows and duplicate replacements. Pin 1,000-row partial-index UPDATE counts and generated `index7` behavior.
- SQLite `full_column_names=ON` qualifies direct column expressions, but does not qualify names introduced by unqualified `SELECT *` expansion. Probe exact result column metadata with SQLite and keep these output-naming paths distinct; `testgen/select1` pins both.
- JSON-harness C-API `runtime_var` assertions can lose Tcl assignments to SQLite result codes even when generated `testgen` family and isolated SQL behavior pass; classify by source operation sequence and preserve generated test as authoritative instead of altering engine behavior.
- Correlated table-valued pragma joins return right rows paired positionally with `corrLeftIdx`; right-only filtering/reordering must preserve pairing or be disabled. json102-1130 panicked when `orderJoinRightByAutomaticIndex` filtered rows but left provenance indices unchanged; regression drives correlated `json_tree` with right-side WHERE constraints.
- For repeated fixture operations, encode consecutive structurally identical steps as `{type:"repeat", repeatCount:N, repeatBody:[step]}` and expand in order at both harness pre-scan and execution. Compare expanded steps against original fixture structurally before installing; gzip alone shrinks stored bytes but not decode memory or execution metadata. Include error-producing steps and surrounding distinct operations to pin order.
- Control harness environment before attribution. Tests may mutate shared DB files or process-global hooks; serialize hazardous tests and isolate resources.
- Regenerate only intended fixtures from authoritative source. Inspect ordering/state transitions; never hide engine behavior with skips.
- Close temporary cursors before subsequent mutation; retained cursor registries can make repeated writes quadratic.
- Short-record UPDATE paths must size both OLD and NEW row images to declared column count before default materialization; a short OLD slice silently bypasses added-column defaults in triggers.
- Tcl proc `{args}` is truly variadic with zero required arguments; maintain fixture/native callback metadata with minimum arity 0.
- ALTER COLUMN rename of expression indexes needs scoped token rewriting across both index terms and partial predicates, not generic expression token matches.
- ALTER TABLE DROP CONSTRAINT on a column-level `NOT NULL` must consume both tokens; when constraint follows `--` comment, remove comment text with dropped clause so it cannot swallow closing syntax.
- Parser compatibility rewrites must preserve raw CREATE SQL: stripping GENERATED/STORED in preprocessInput changed sqlite_schema.sql even though parser token-level suppression already accepts grammar. Native CREATE/DROP schema query pinned this.
- ALTER NOT NULL schema rewrites should mutate only top-level clause spans; regex matching can delete `IS NOT NULL` inside CHECK expressions. Quoted identifiers need token-span based extraction to retain original quote style.
- Dropping a named column constraint must preserve adjacent independent clauses such as COLLATE and DEFAULT; token-tail removal should only consume the named constraint type's actual payload.
- Benchmark actual workload with paired/interleaved repetitions and medians; instrument fast-path hits before interpreting profiles.
- Keep fixtures immutable; use disposable copies and deterministic values.

## SQLite and storage pitfalls
- SQLite `sqlite3_db_cacheflush` (main.c:917-945) visits each attached pager in WRITE transaction and invokes `sqlite3PagerFlush`; that flush writes only dirty pages with `nRef==0` through pagerStress, preserving active transaction/journal. Frigolite `Pager.Flush` is commit-phase `flushAll` (pagercommit.go) and clears dirty state/finalizes journals, so it is not equivalent. Never reuse it as cacheflush. For Frigolite's synchronous API boundary, statement cursors have already been released, so dirty cached pages are eligible without global pin tracking; transaction-level reader locks still require BUSY aggregation.
- Synchronous Frigolite `Engine.Exec` releases statement-owned btree cursors in a deferred `releaseStatementTrees(treeMark)` before returning. Cacheflush at a separate synchronous API boundary can use this call-path invariant rather than global cursor pin accounting. Ordinary explicit-transaction SELECTs are tracked by lockreg `sharedTx`; cacheflush must check `SharedTxByOther` as well as prepared-read `ReadTxByOther` or source tests miss BUSY.
- `Stmt.Step` must distinguish read statements from DML: SQLite `sqlite3_step` executes prepared INSERT/UPDATE once and returns DONE, while SELECT yields ROWs. Routing every prepared AST through `DB.Query` caused DML parser error; native changes2 lifecycle regression exposed and fixed this by using prepared execution for non-read statements.
- Prepared-statement fixture lifecycle must remain ordered runtime events, not only an expected SQL result. Converter must emit opaque handle prepare/step/reset/finalize actions, and harness must run actual Stmt APIs and evaluate runtime assertions from returned codes/changes; never inject final SELECT rows.
- SQLite `UPDATE ... RETURNING` still updates sqlite3_changes() to number of affected rows while exposing result rows. `finishUpdate` must preserve `Changes` when returning rows; otherwise subsequent `db changes` and `changes()` lie despite correct updated rows.
- Value equality and representation may differ (INTEGER 1 vs REAL 1.0). Exact mutation lookup may need byte-level confirmation after comparator seek; index ordering is value/collation-based.
- Interior index cells may be copied divider keys, not ordinary leaf entries; verify representation before traversal/counting rules.
- Validate arithmetic, offsets, lengths, and integer narrowing at representation boundaries; malformed files should error, not panic.
- SQLite coercion is context-dependent; probe each operator and execution shape.
- Seek optimizations must respect comparator, DESC order, affinity, and prefix bounds; retain scans where seeks could omit matches.

## Validated compatibility findings
- `alias.test` returns before later sequence-UDF tests; source-faithful fixture contains no tests, not invented sequence registrations.
- Stale fixtures are not ground truth. `aggnested` cases 10.1 and 11.2/11.3 were corrected from SQLite/source expectations, without engine changes.
- Schema rows can overflow due to large CREATE SQL. `DeleteCellsWhere` gives predicates local payloads; preserve optimization for rowid-only callers, use full-payload deletion where schema predicates decode records. Regression: `frigolite_schema_split_drop_test.go`.
- TCL clock overrides and `db func` registrations are ordered runtime state; preserve them as markers.
- `do_select_tests -tclquery` is distinct from SQL query; preserve nested Tcl list results and ordered nullvalue changes. `-error` means caught SQL failure.
- `do_select_tests` repair runs around each generated triple. Preserve assertion identity separately from harness grouping; separate testcases reset state.
- `set ::name` must resolve via bare `${name}` references. `set ::oid [execsql {SELECT ...}]` needs ordered runtime result assignment and later unresolved `$::oid` interpolation. RED/green converter test covers alter-6.2; exact alter run now clears 6.2–6.6.
- SQL Tcl-parameter expansion must respect SQLite lexical states: never substitute inside quoted strings, identifiers, or comments.
- Tcl `return` ends source execution; do not invent later fixture tests/UDFs.
- SQLite TCL `lreplace [db eval {...}] start end value` mutates the flattened Tcl result list after SQL execution; a converted query expectation may encode that transformed value rather than engine output. Preserve such postprocessing explicitly instead of changing SQL or engine row semantics. colname-5.1 replaces sqlite_master rootpage at index 3 with literal `x`.
- `sqlite_rename_column` is not a text-token substitution: `alter.c` resolves target table metadata, parses stored schema SQL, and walks table/index/view/trigger expressions plus FK/generated-column references. Use parsed rename token ranges for AST-covered expressions; explicitly validate column-definition, generated-column, child-FK referenced-column, index-key/partial-WHERE, trigger UPDATE OF, and NEW/OLD references, without pretending broad identifier substitution is equivalent. Internal-function test control toggles per connection; SQLite test helper is in `src/test1.c`. Registry test pins hidden-by-default, enable, and disable transitions.
- TCL setup may include custom collations/UDFs, named connections, forcedelete, mutable extension globals, close/reopen transitions; preserve ordered scoped state.
- `db func NAME NAME` captures Tcl proc argument metadata in an ordered registration marker; variadic `args` callbacks need `MinArgs=0` (when no fixed formals) and `MaxArgs=-1`, and callback-global assignment must update harness runtime vars during SQL execution; serialize callback args with Tcl list quoting (not plain space-join) so values containing spaces survive. Native coverage should invoke registered SQL function and assert captured global state, independently from converter marker coverage.
- `stepsql` executes SQL sequentially on specified connection; capture ordering and identity.
- SQLite test helper `sqlite3_exec DB SQL` percent-decodes `%HH` bytes, executes callbacks, and returns `{rc result}`. Keep typed fixture step distinct from ordinary exec; alter-11.1–11.10 now pass.
- Braced TCL `execsql {SQL}` binds variables but does not re-run `$`/`[]` substitution over literal SQL; bracket-quoted identifiers survive unchanged.
- Do not run overlapping full harness processes; shared DB mutations invalidate failure counts.
- Tcl `args`-only UDF is variadic with minimum arity 0; conversion records that correctly. Harness dispatch must not assume at least one argument. `alter2` also demonstrates helpers may rewrite `sqlite_master` then patch database header via `hexio_write`; converting SQL alone cannot emulate such fixture setup—preserve typed helper operations explicitly.
- `forcedelete test.db2` in lifecycle fixtures must resolve beside lifecycle `test.db`; leaving it repo-relative risks collateral deletion. Preserve source order by representing deletion as an ordered step before fixture SQL. `alterlegacy` section 11 also needs its `trigger` callback registered before TEMP-trigger execution.
- Fixture `ATTACH 'test.db2'` can touch shared repository-root files even when primary DB is in-memory; route relative quoted ATTACH operands and `delete_file` targets to a per-JSON-fixture temp directory. Pin unchanged pre-existing root file bytes in regression coverage.
- `altermalloc2.test` depends on ordered lifecycle boundaries: section 2 closes, deletes `test.db`, and reopens; sections 3, 4, and 5 each `reset_db`. Converted fixture must be `ordered:true` so setup markers stay before their target sections.
- A compatibility fixture missing source-level Tcl `foreach` values or reset markers is stale conversion, not engine behavior; regenerate only that JSON from authoritative SQLite `.test` source, inspect the diff, and validate the exact family. `alterqf` regeneration restored the per-case SQL inputs and section reset, making family tests meaningful.

- `autoanalyze1.test` is debug-only and probes internal flags through test-only `pragma_stats`: fields `tbl`, nullable `idx`, `flgs`; `TF_HasStat1=0x10`, `TF_StatsUsed=0x100`. Its repeated `sqlite3 db test.db` calls must be represented as ordered close/reopen markers to reset connection-local planner state; fixture must remain source lifecycle-faithful. `altertab` source needs `squish` whitespace normalization and a variadic trigger callback.

- Tcl named SQLite handle commands (`db2 eval`, `db2 close`) are distinct from main-handle commands and must emit SQL/lifecycle markers carrying the handle name. Silently ignoring unknown command names loses disk-backed setup and creates downstream false engine failures; converter tests should pin handle identity.

- SQLite schema-load validation distinguishes stored CREATE rows from arbitrary nonempty SQL: `prepare.c` parses rows beginning with CREATE and treats other nonempty table/index SQL as malformed schema without parser detail. If engine preserves leading SQL comments before CREATE text, keep those parseable rather than rejecting them based on first byte. Reopen regression: corrupt `sqlite_stat1.sql`, close, reopen, then ANALYZE.

- Tcl `expr` must preserve integer-vs-floating operand types: integer/integer `/` truncates, while float participation yields real division. Fixture generation can otherwise change loaded dataset and downstream planner counts; regenerate after converter semantics change and inspect actual inserted values.
- `sqlite_stat1` token `unordered` is an index capability flag, not just a cost hint: SQLite excludes such indexes from range seeks and ORDER BY scans while retaining equality use. Apply eligibility consistently across WHERE, covering scans, and order walks.
  - Regenerate Tcl JSON fixtures when `db func`/`sqlite3_limit` setup is missing; generated `register_function` and `set_limit` markers preserve lifecycle that hand-edited SQL cannot. SQLite STAT4 probe evaluation folds only SQLITE_FUNC_CONSTANT/SLOCHNG expressions (`vdbemem.c:valueFromFunction`); preserve deterministic metadata and propagate callback/output-size errors from STAT4 planning rather than swallowing them.
- SQLite `IS TRUE` / `IS FALSE` uses boolean truth coercion (`OP_IsTrue`), not NULL-safe equality against integer 1/0; ordinary `IS 1` remains a type-aware equality. Preserve RHS TRUE/FALSE AST identity in BinaryOp evaluation, including BLOB/text operands.
- SQLite printf `%f` uses `sqlite3FpDecode` (Dekker double-double scaling, integer digit extraction, then significant-digit clipping), not direct binary-to-fixed formatting. Go `strconv.FormatFloat(...,'f',precision,64)` exposes exact binary tails for large precision; matching requires porting the decoder, not merely changing significant-digit limits. `%!f` selects `mxRound=26` (not 20), sets decimal-point output even at precision zero, and trims trailing fractional zeros.
- Ordered TCL fixtures must not use section-transition `DetachAll`; source-order marker streams rely on attached schema state across later sections. Named sqlite connections using `:memory:` are opens, not close markers; preserve their independent in-memory lifecycle. SQLite trigger schema fixing compares qualified `ON` table/trigger-body references to trigger storage schema (`attach.c:486–492`); trigger storage context must not be inferred from ON-table schema.
- TCL `sqlite3 db ""` / `sqlite3 db :memory:` is a fresh EMPTY main handle with on-disk fixture files left intact — it is NOT `reset_db` (close + forcedelete + reopen). Convert it to a dedicated `open_empty_db` marker; when a fixture does this, the harness must model main as the file-backed `test.db` beforehand (lifecycle mode), or earlier writes land in a throwaway in-memory DB and a later `ATTACH 'test.db'` creates an empty file instead of finding the trigger/rows.
- Harness ATTACH-path routing must preserve `':memory:'`: rewriting it into a fixture-directory file makes every `ATTACH ':memory:' AS dbN` share one pager (schema/table collisions), while SQLite gives each attach a distinct in-memory database.
- SQLite re-validates stored non-temp trigger bodies at schema load and compares purely by name/index (`attach.c` fixSelectCb + `prepare.c` init): an explicit database name other than the trigger's own schema is malformed — even when that database exists under another name or holds the referenced object; the explicit name on the trigger's `ON` table is stripped during schema reparse (backward compat); TEMP triggers are exempt (`bTemp`) and may reference any database. `attachFunc` runs `sqlite3Init` synchronously, so the ATTACH itself fails and the attachment is rolled back (nothing left in `PRAGMA database_list`). Fire-time validation must likewise use the trigger's owning schema and skip TEMP triggers.
- SQLite virtual-table module arguments may arrive as one quoted Tcl command string (`"vtab_cmd t1 {a b}"`) rather than individual argv tokens. Normalize outer SQL quoting before Tcl-list parsing; regenerate stale fixtures from source when callback metadata markers are absent.
- Ordered fixtures can register Tcl modules in setup steps before callback proc definitions; converter must snapshot/update registration marker with later proc definitions. Regenerate only stale marker metadata from authoritative source when JSON predates callback serialization (alterlegacy/altertab).
- Named Tcl `eval` accepts unbraced SQL words as well as braced blocks (`db2 eval BEGIN`); capture either form as a database-scoped marker. Bracedness controls substitution semantics, not whether SQL exists.
- A failing fixture using `PRAGMA encoding='utf-16'` is not automatically a UTF-16 capability regression: `collate7` first fails because Tcl `sqlite3_create_collation_v2` registration is omitted. Check the earliest runtime error and source lifecycle before attributing encoding; never globally enable UTF-16 to make unrelated tests proceed.
- Tcl collation-v2 fixture steps must preserve ordered register/delete/close, comparator proc body, destructor script, and callback-visible global snapshot; `collate7` asserts destructor counts at each transition. Emit runtime global assertions after `do_test` bodies ending in `set ::var`; do not increment expected state at conversion time.
- Regenerate stale fixture JSON when tests show cross-case table/view collisions or missing lifecycle state; legacy unordered `cast.json` made cast-9.0/10.7/10.9 fail, while source-regenerated ordered fixture passed the full cast family. Compare temporary output before replacing only intended fixture.
- In correlated subquery JOIN ON validation, a missing qualifier from the subquery-local FROM set may resolve in an enclosing scope; distinguish that from a qualifier present in the local FROM chain but joined to the right. Regression: cursorhint-6 correlated scalar subquery in HAVING.

- Tcl `register_tcl_module` callbacks need serialized proc definitions, registration args, and callback-visible Tcl globals; module runtime must stay fixture-scoped and injected across xConnect/xBestIndex/xFilter. Created-vtab joins must use `MaterializeCreatedVTab` rather than generic `VirtualTableRows`, otherwise planner `BestIndexPlan`/cursor `FilterPlan` are bypassed and joins see empty scans; regression `TestTclCallbackVirtualTableJoinRows`.
- Tcl array-element names can nest substitutions (`$cols($C(column))`); find the matching outer parenthesis and substitute index contents before lookup. Tcl virtual-table `CREATE TABLE` declarations also carry column types used for residual affinity; preserve `ColumnTypes` (for example INTEGER) rather than only names.
- Source `ifcapable vtab` alters autoinc-12.3 expected error: with vtab support, corrupted CREATE VIRTUAL TABLE schema reports generic `database disk image is malformed`, not parser detail. Preserve capability-specific oracle expectation.
- TCL `set rootN [db one {SELECT ...}]` in `autoinc-12.4` must become separate `query_assign` steps; query the actual two rootpages before swapping them. Empty runtime vars silently generated rootpage zero and a false-positive corruption check. Native reopen test pins rootpage swap detection.
- Tcl `lsearch` defaults to glob matching. When its searched list comes from `[db eval "EXPLAIN ..."]`, offline conversion cannot evaluate the opcode rows; generated expectations depending on that query must be obtained from runtime SQLite, not an empty converter-side value. bestindexF uses this in `uses_idxinsert`.
- SQLite `ANALYZE` treats empty index cursors as normal EOF (`analyze.c` empty `Rewind` path); Frigolite STAT4 scan must accept cursor-at-end only before reading any record. Regression: `TestAnalyzeEmptyPartialIndex`.
- Correlated right-side virtual-table/pragma rows carry a positional `corrLeftIdx` provenance map. Never filter or reorder `rightMaps` alone once this map exists: right-only ON prefiltering can erase rows while leaving stale indices, causing mismatched outer-row pairing or panics. Regression `TestJSONTreeCorrelatedWhereFiltersRetainRowProvenance`; json102-1130 harness case passes.

- Materialized derived rows must retain positional source data; duplicate result names overwrite shared RowMap keys. Keep `positionalRowKey` visible-only to match `appendUnqualifiedStar`; hidden virtual-table slots shift physical source indices and must be translated. During JOIN, keep left and right positional vectors in distinct keys so right values cannot impersonate left vectors. RIGHT JOIN USING merges first matching right source position while preserving later duplicate visible columns. Regressions: JoinH duplicate columns, FTS5 NATURAL hidden columns, selectD attached same-name tables.
- Trailing body expectations that compare runtime results must stay runtime assertions: `do_execsql_test N {SQL1} [db eval {SQL2}]` emits a `compare_query` step (sql + sql2; converter regressions `TestDoExecSQLRuntimeComparisonCapturesCompareQuery`, `TestDoTestRuntimeComparisonCapturesCompareQuery`), and a trailing expression reading a query-assigned Tcl variable emits `runtime_expr` (raw expression + expected) evaluated against harness runtime vars (`TestDoTestFinalExprWithRuntimeVarEmitsRuntimeExpr`; evaluator in `frigolite_harness_expr_test.go`). Never bake the converter's previous-command result into the expectation.
- `do_select_tests`-family helpers merge every iteration into one case named after the first iteration, with per-iteration steps in source order (exec/query/compare/catch); a fixture that omits those cases entirely (stale `e_update.json`: 16 cases vs 59 fresh) cascades into later `DROP TRIGGER aux.tr1` -> "no such trigger: tr1". Regenerate the fixture from source; do not patch expectations.
- `ifcapable !X { finish_test; return }` with X modeled absent (utf16, 8_3_names) must stop conversion entirely: no fixture is emitted, matching the standard testfixture, and guarded assertions must not be forced green by implementing X (8_3_names = SQLITE_ENABLE_8_3_NAMES FAT name mangling). `testdata/8_3_names.json` was removed on this basis; the legacy oracle fixture `f_8_3_names.json` (oracle_generate.py sanitizes names and `f_`-prefixes digit-leading ones) retains the SQL-visible coverage.
- `sqlite3 db FILE` in fixtures maps to a data-preserving `reopen_db` with `file:`/URI paths normalized to the fixture-local basename (same normalization for `open_named_db`); `copy_file` is a recognized fixture-scoped file-copy marker. Harness adapters added with these repairs: `string repeat` SQL UDF, runtime-variable Tcl-expression evaluator.
- WAL snapshot isolation remains open: with an open read transaction on one connection, a commit on another must not change its snapshot (8_3_names-5.6 wants 500500, engine returns 1001000); this belongs to the queued P7.WAL snapshot/shared-cache work.
- View column metadata must apply the USING/NATURAL star merge: `viewColumnDefsFromSelectGuard` originally expanded a bare `*` to the raw concatenation of the FROM sources, so `CREATE VIEW v1 AS SELECT * FROM t1 NATURAL JOIN t1` reported columns `x` and `x:1` (oracle: one `x`). The executor merges at run time (`select_join.go` filterUsingColumns/filteredUsingColumnIndices; naturalJoinCommonCols compares exact names and skips HIDDEN), and the metadata path must mirror it (`viewStarSourceDefs`), or joins over such views match the phantom always-NULL copy and return no rows (join5-10.1). Keep the flattened `srcDefs` for type resolution — only star expansion uses the merged list.
- The omit-unused-subquery-column optimization must mark a source's JOIN-constrained columns as used for BOTH derived subqueries (`ref.Subquery == sub`) and named view bodies (`ref.Name`/`ref.As` matching the view name threaded as `sourceNames`); under-marking rewrites a NATURAL/USING column to NULL and then the generated join condition can never match (`SELECT count(*) FROM v1 NATURAL JOIN v1` returned 0 while `count(x)`/`SELECT *` worked because the reference itself marked the column used). Over-marking only shrinks the optimization and is safe.
- A broad "table tN already exists" failure signature (cascades through later sections of one fixture) means the installed JSON is stale relative to `ori/sqlite/test`: the TCL sections are separated by lifecycle resets (`db close; forcedelete test.db; sqlite3 db test.db`, `reset_db`) that an older converter dropped, so later sections re-CREATE tables in leaked state. Regenerate the fixture from source (verified batch: converter in one invocation, per-family install+isolated run, revert only when a test that existed in the old fixture starts failing); do not patch expectations or add resets in the harness.
- Converter shortfalls to re-check for regenerated families: `tkt2686.test` and `types.test` currently fail conversion (errors) — their installed fixtures must stay until the converter handles them.
- Mini-interpreter fidelity bugs silently corrupt generated fixtures and look like engine failures; fix the converter first when a fresh fixture's generated SQL contains empty/zero values the TCL source cannot produce. Verified round-2 fixes (`tools/tclconvert/tcl/`):
  - TCL option abbreviations: `subst -novar`/`-nocommand` are prefixes of `-novariables`/`-nocommands`; rejecting them emptied substituted text (rowvalue2). `matchSubstOption` accepts unique prefixes, rejects ambiguous ones.
  - A bracketed SQL word (`do_execsql_test N [subst -novariables {...}] ...`) still contains `$name` references because `-novariables` disables substitution; like braced SQL it must go through `bindSQLParams` (tclsqlite binds them at eval time).
  - TCL index specs (`end`, `end-N`, `end+N`, `N-M`, negative) apply to BOTH strings and lists: `string range`, `string index`, `lrange`, `lindex`, `lreplace` all used `strconv.Atoi`, so `end` → 0 → empty slices or element 0. Shared `parseTclIndex` now resolves them; rowvalue2's generated INSERT data had empty `c` columns and wrong control expressions, causing 2,430 false failures that fell to 0 after the fix.
  - `switch` bodies: a body of `-` means "use the next pattern's body" (TCL fallthrough); without it multi-label bodies (`== - IS {...}`) evaluated empty.
  - `foreach` accepts multiple varList/list pairs and iterates over the longest list, filling shorter/partial groups with empty strings (previously only the first pair bound, and partial groups were skipped) — rowvalue2 `foreach c $cList v $vList` set `v` to "" everywhere.
  - Rule of thumb: before attributing a mass of fresh-fixture failures to the engine, print the emitted `sql`/`sql2` and run the same statements in the engine natively; if the native run matches the oracle, the converter is the suspect ($$value often = converter artifact).
- Adding a converter command that emits a marker step is not enough: `convertToJSON` → `groupToTestCase` (tools/tclconvert/main.go) keeps a step only when it has SQL or its Type is in an explicit whitelist. An empty-SQL marker type missing from that list is silently dropped, and if the whole setup group becomes empty the fixture loses the marker (ptrchng: `register_test_funcs` vanished while the interp-level unit test passed). When adding a new step type, extend the whitelist and pin the end-to-end fixture, not just `i.Stmts()`.
- `sqlite3_create_function DB` is testfixture's multi-function registration helper (test1.c `test_create_function`): pointer_change, x_coalesce, hex8/hex16, tkt2213func, counter1/2, intreal, add_*_type, strtod, dtostr, inttoptr. `pointer_change`'s expected values are Mem.z pointer-identity observations: value_text and value_text16 re-encode (new pointer), value_blob returns the current buffer unchanged whatever its encoding, so `text16 -> blob` is 0 while `text16 -> text` is 1. Emulate that buffer-state machine rather than treating text/blob as synonyms.
- Testfixture UDF equivalents live in `frigolite_harness_testfuncs_test.go` and are installed by the harness's `register_test_funcs` case; keep new test1.c ports there rather than inline in the harness switch.

- Converter TCL command coverage keeps surfacing as fixture corruption: `split` was missing, so `lindex [split $sql =] 0` (pragma4.test) produced an empty string and the emitted `prepare_stmt` had no SQL. Implement commands with the real Tcl semantics (split: any single char of the separator set, empties preserved, default whitespace, empty separator = one element per character — pinned against tclsh in `TestSplitCommandMatchesTcl`). Also register API aliases the corpus uses (`sqlite3_prepare_v3` behaves like `sqlite3_prepare_v2` for step capture; without it fts4content/normalize/without_rowid7 dropped every prepare, leaving `stmt_finalize` steps with unknown handles).
- Batch tooling: do not use `comm` for failure-set comparisons on this machine. macOS `comm` warned "input is not in sorted order" dozens of times (locale collation vs `sort`) and silently changed verdicts; the replacement driver (`/tmp/rfb2-driver.py`) does all set logic in Python, uses `-timeout` per family with one retry (a "hang" seen in one run did not reproduce), and records verdicts as JSONL. Always re-verify tool-computed regression sets independently before acting on mass reverts.
- `sqlite3_test_control SQLITE_TESTCTRL_INTERNAL_FUNCTIONS db` toggles `internalFunctions` in the fixture (`"internalFunctions": true` step field), and the engine's `sqlite_rename_table`/`sqlite_rename_column` are registered `InternalOnly`. Failures like "no such function: sqlite_rename_table" against an *old* fixture usually mean the fixture lacks the toggle the source file has; check the fresh conversion (`/tmp/regen-full/<fam>.json`) for `internalFunctions` before writing engine code.

- Converter runaway loops are a *memory* bug, not just a size bug: `for {set i 1} {$bStop == 0} {incr i}` has runtime-only termination, so the converter unrolled to the 50k loop cap times nested `foreach` combos. The regen-full installs shipped `backup_ioerr.json` = 1.1 GB / 1.8M tests / 6.6M statements (old fixture 735 B), `savepoint6` 288 MB, `backup` 222 MB, `ioerr2` 202 MB. A full `go test ./...` then loaded/executed them → >20 GB RAM. Fix: a per-interpreter statement budget (150,000) and deadline (30 s) checked *before every command* (`checkRunaway`), with sticky errors so `catch` cannot swallow them; oversized files become conversion errors and the installed fixture stays. Never install a fixture without checking its step count.
- `runInterpreter`'s old `select`/`time.After` timeout abandoned a goroutine that kept running the interpreter and appending to `i.stmts` forever — one leaked runway loop per timed-out file inside one long-lived conversion process. Deadlines must be cooperative (checked by the interpreter), not external `select` timeouts on a goroutine that cannot be cancelled.
- TCL `format` parses the argument according to the conversion: float verbs (`a/A/e/E/f/F/g/G`) take numbers (Go's `fmt.Sprintf("%.32e", "-Inf")` on a string arg produced `%!e(string=-Inf)` in atof1), `%g`/`%G` default to 6 significant digits, `%i`→`%d`, `%u` is unsigned 32-bit, `%c` takes a char code, `l`/`ll` modifiers are ignored. Pin against tclsh (`TestFormatCommandMatchesTcl`).
- Escaped SQL parameters (`db eval "...\$x"`) must be bound from TCL scope at conversion time (testfixture binds `$name` from the same-named TCL variable): leaving `$x` in the fixture executes with an unbound parameter. `bindSQLParams` must keep `$name` only for *dynamic* variables (assigned from a runtime query result) so the harness's `expandTCLRuntimeVars` can substitute them.
- TCL `expr rand()`/`srand()` are deterministic and portable (TclRand in tclBasic.c): Park-Miller LCG (IA=16807, IM=2^31-1) via Schrage, returning `seed * (1.0/IM)` with a precomputed rounded constant. `srand(n)` masks to 31 bits (0 and 2^31-1 get `^123459876`) and **consumes one draw** — after `srand(1)` the first `rand()` is 16807^2/IM = 0.13153778814316625. Stubbing `rand()` as a constant collapsed atof1.test's whole 20,000-iteration loop into one repeated `-Inf` statement (`SELECT -Inf='-Inf'` -> "no such column: Inf"); with the faithful LCG the regenerated fixture has 40,006 distinct round-trip tests and atof1 went 19,999 failures -> 0. Pin PRNG sequences against tclsh; allow ~1 ulp in composed expressions because Go's math.Pow is not bit-identical to libm's pow.
- The corpus converts to more families than are installed in `testdata/`: the fixed converter produced fixtures for 207 families that had no JSON at all (none in the harness skip lists). When diffing fresh conversions against installed fixtures, treat "installed fixture missing" as a new family, not as a regression; install it as new coverage and record its surfaced failures separately.

- Tcl `expr` substitution is **atomic**: a substituted variable value is an *operand*, never re-read as expr source. The converter must render every substituted value as an operand — numeric values bare (so `int` stays `int` and `4.0` stays a double), everything else as a quoted string with `\`/`"` escapes. Inserting a value bare made `expr {$rhs!="NULL"}` unparsable whenever the value contained a quote (the e_expr literals list keeps its single quotes: only `{}` and `""` group in a Tcl list, `'abc'` is the five-character string `'abc'`), and the failed parse silently flipped the `if` branch — 218 e_expr leaves expected the wrong branch. Always quote non-numeric substitutions, and add backslash-escape handling to the expr string-literal parser.
- `string match` was **missing** from the converter (83 corpus uses). Tcl's glob has `*`, `?`, `[class]` with ranges and `\` escapes, and **no class negation**: `[!a]`/`[^a]` match the literal characters `!`/`^` or `a`. e_expr-12.3.x used `if {[string match *EXPR2* $expr]}` to decide whether to substitute `EXPR`/`EXPR2`; without `string match` the substitution never happened and 75 leaves ran `SELECT + EXPR FROM tblname`. Also implemented `string is` (integer/double/boolean/alpha/alnum/digit/space/list/…, empty string belongs to every class, `boolean` accepts only 0/1/true/false/yes/no/on/off) and `string replace` (negative first index clamps to 0, an inverted or empty range leaves the string unchanged, `last` clamps to the last character).
- The testfixture exposes `SQLITE_MAX_COLUMN`/`LENGTH`/`ATTACHED` to the TCL scripts; `SQLITE_MAX_VARIABLE_NUMBER` (post-3.32 default **32766**, also this engine's own limit) was missing, so `[expr $SQLITE_MAX_VARIABLE_NUMBER+2]` evaluated as `+2` and the expected "variable number must be between ?1 and ?N" text was wrong.
- Tcl `execsql` returns the **whole first row as a list of columns**, while `db one`/`db onecolumn` return a single value. The `query_assign` fixture step needs a `resultAll` flag, otherwise a two-column expectation (`set eq [execsql "SELECT a = b, a != b"]`) compares against the first column only.
- `set X [execsql ...]` must be retagged to `query_assign` even when the captured statement is a *query* (SELECT) step, not only when it is an `exec` step; otherwise the variable stays empty and every expectation built from it is wrong.
- Two adjacent runtime placeholders (`$as$bs`, produced by `[list "${as}${bs}"]`) are **one** parameter reference under Tcl's rules, because `'$'` is a valid identifier character. The harness's `expandTCLRuntimeVars` must split at the inner `'$'` when the full name is not a runtime variable.
- SQLite's bitwise operators **coerce** their operands (`sqlite3VdbeIntValue` in vdbe.c: integers as-is, reals truncated, text/blob parsed with 0 on failure) and never raise "cannot bitwise-AND non-integer values": `SELECT '2245' & 66` → 64, `SELECT 2.5 & 3` → 2, `SELECT X'414243' & 3` → 0. Only NULL propagates NULL.
- The converter does **not** create its `-outdir`; a missing directory makes every file fail with "write error: … no such file or directory" while the process keeps running (it looks like a hang). Always `mkdir -p` the outdir first.
- `faultsim_delete_and_reopen` / `db_delete_and_reopen` (tester.tcl `db_delete_and_reopen`: close, delete every `test.db*`, reopen) reset the main database exactly like `reset_db`; the converter must map them to the `reset_db` marker, otherwise later sections of pager1/pager2/wal* run against a leaked database.
- A user-defined `like()`/`glob()`/`regexp()`/`match()` function **replaces** the built-in operator: SQLite rewrites `X LIKE Y` into `like(Y,X)` and resolves the name through the function table (expr.c `TK_LIKE_KW`). Frigolite intercepts LIKE/GLOB/REGEXP by name (`scalarSpecialDispatch`) and only *probes* the registry (`probeOperatorOverload`), discarding the result — the operator-override tests (e_expr 15.1.x/17.3.x/18.2.x) therefore cannot pass until the override result is used.
- A Tcl variable's "runtime-assigned" status must be cleared when a later plain `set` gives it a known value: e_expr-8.2 assigns the same variable from a query in one branch and from a computed list in the other, and the stale dynamic flag made every later read expand to the `$name` placeholder instead of the computed value.
- `do_execsql_test`/`do_catchsql_test`/`do_eqp_test` must scope the "current test name" to their own command: leaving it set made the *next* statement (outside any test) belong to the previous test, which split e_expr's `parameter_test` trio (prepare in one test case, step/finalize in another). The harness then lost the prepared handle and the never-finalized statement held a prepared-read lock, blocking every later write with "database is locked".
- A failed prepare/step/finalize in the harness must not abort the remaining steps of the test case: skipping the matching `stmt_finalize` leaks the statement's read lock (the same "database is locked" cascade). Report with `t.Errorf` and continue.
- The `runtime_assert` step must also continue after a mismatch (it used to `return`), for the same reason: the following `stmt_finalize` was skipped.
- Go gotcha with big blast radius: an untyped constant returned as `interface{}` boxes as Go `int`, not `int64`. `fastIntArith`'s `return 0` (the `x % -1` wrap case) therefore produced an `int`, and every downstream `int64` type assertion failed — `SELECT 0 - (1 % -1)` failed with "cannot subtract non-numeric values" while `SELECT 1 % -1` looked fine. Always write `int64(0)` in engine helpers that return `interface{}`.
- SQLite raises **two different** prepare errors for parameters (expr.c `sqlite3ExprAssignVarNumber`): an explicit out-of-range index (`?0`, `?NNN` above the limit) gives "variable number must be between ?1 and ?N", while an *assigned index* above SQLITE_LIMIT_VARIABLE_NUMBER (e.g. `?32766, ?` → the anonymous `?` takes index 32767) gives "too many SQL variables". Frigolite only implemented the first, and its check ran only on the Exec path (a failed prepare must return no statements, so the Query path reported no error at all).
- A Tcl `expectation` of `{{}}` is "one row, one empty-text column"; the engine returns NULL there, and Tcl renders NULL as the connection's null value. The harness's `emptytext` step must accept NULL when the current null value is the empty string.
- SQLite's LIKE/GLOB/REGEXP operators are **function calls**: expr.c compiles `X LIKE Y` into `like(Y,X)`
  and resolves the name through the function table, so an application function registered as like/glob/
  regexp replaces the built-in operator (and the NOT variants apply SQL NOT to the function's result).
  Frigolite intercepted those names by string (scalarSpecialDispatch) and only *probed* the registry
  (probeOperatorOverload discarded the result) — the operator-override tests could not pass.
- SQLite's parameter names after ':' and '@' accept '$' (IdChar): ":_$_" and "@_$_" are valid names
  (e_expr-11.2.4). Only the '$'-prefixed scan allowed it before.
- CURRENT_TIME / CURRENT_DATE / CURRENT_TIMESTAMP are keywords, not column names: `SELECT CURRENT_TIME
  FROM tblname` must not fail with "no such column" (the column validator rejected them).
- A Tcl list element keeps its single quotes (`'abc'` is the five-character string "'abc'"), because
  only `{}` and `""` group in a list. The converter's expr substitution must therefore quote every
  non-numeric substituted value: a bare insertion of "'abc'" made the expression unparsable and silently
  flipped the branch of the `if` that used it.
- The converter's runtime placeholders for query-assigned variables must be dropped again when the
  variable is later assigned a *known* value: e_expr-8.2 assigns the same variable from a query in one
  branch and from a computed list in the other, and the stale "runtime-assigned" flag made the computed
  expectation expand to the placeholder instead.
- `db eval {SQL} { lappend res $x }` runs a Tcl callback per row and collects the FIRST column of
  each row into a list; the converter used to capture the statement as a plain query, so the expectation
  (the collected list) was compared against *all* columns (orderby8: 199 leaves). The converter now
  recognizes that callback shape and emits a `query_first_column` step, which the harness renders with
  the first column of every row.
- A `db eval`/`db one`/`execsql` assignment is a runtime value: the converter must keep a `$name`
  reference (not the empty capture-time value) and the harness expands it from the runtime variables.
  Adjacent placeholders ("$as$bs") are one parameter reference under Tcl's rules ('$' is an identifier
  character), so the harness splits them at the inner '$'.
- A verify-install batch must treat a **build failure** as "no result", not as "0 failures": while the
  engine was mid-edit, the batch's "fresh fixture" runs failed to compile, the script recorded
  `fixed=N, new=0` and installed the unverified fixtures. 13 families (skipscan5, snapshot2/4/up,
  speed3, spellfix, sqllimits1, sync2, tabfunc01, temptable2, temptrigger, whereL) were affected;
  re-running them against the pre-batch backups showed 6/1/2 real regressions (skipscan5, snapshot2,
  temptable2) and the rest were fine. Check the batch logs for "build failed" before trusting a
  verdict, and never edit the engine while a batch is running.
- The `db eval {SQL} { lappend res $x }` callback shape, the `faultsim_delete_and_reopen` reset and the
  `var`/`x`/`ceval` Tcl procs all needed converter/harness support rather than engine work: the engine
  was right and the fixture was wrong (orderby8 199→0, e_droptrigger 66→0, e_expr 16,657→27).
- Tcl's `eval arg1 arg2 ...` concatenates the arguments and evaluates the result as a script. The
  converter did not implement `eval` at all, so a variable-held list of statements (trigger2.test's
  `eval $test_varset`) never ran: the variables it set stayed empty and the generated SQL lost the
  trigger event ("CREATE TRIGGER the_trigger BEFORE \n ON tbl ..." — no event) — 30 leaves in
  trigger2 plus the same pattern in without_rowid4 and ~45 other files use `eval`.
- `execsql_intout` (a 3.54-era testfixture proc: execsql plus an integer-result check) was missing from
  the converter, so its SQL body was dropped entirely.
- SQLite's rtree module refuses to create a virtual table when one of its shadow-table names
  (<name>_rowid, <name>_node, <name>_parent) is already taken, and reports the first taken name with
  the *quoted* identifier: `table "t1_rowid" already exists` (rtree.c xCreate, `sqlite3ErrorWithMsg(db,
  SQLITE_ERROR, "table %S already exists", zTab)`). Frigolite let the plain CREATE TABLE fail instead,
  with the unquoted message.
- A `close_db` step must tolerate an already-closed lifecycle database (the harness closed it twice:
  "close lifecycle database: … file already closed").
- A failed database open must never leave the harness's handle nil: the next test case dereferences it
  and the whole suite aborts with a panic (run 12 died in e_walckpt/setup_4 after a failed
  "reopen lifecycle database: database is locked" left db nil). Every open site now reports the error
  and falls back to an empty database, so a failing family degrades into failing leaves.
- The full-suite red set is dominated by three subsystems (run 11): `pager*` 2,787 leaves, `where*`
  2,115 (the stmt-status counters), the WAL cluster 688; 129 families carry 303 leaves between them.
  The plan that sequences this work is `portplan/PARITY_PLAN.md`.
- The converter's `lsort` was a stub that returned its input unchanged. trans2.test computes an MD5 over
  the *sorted* records (`lsort -integer -index 0 $data` + `[md5 $x]`), so the expectation was wrong for
  every case. `lsort` (with -ascii/-integer/-real/-dictionary/-index/-decreasing), `md5` (the hex digest
  the testfixture provides) and `random_uuid` are now implemented and pinned against the oracle:
  trans2 234 → 0.
- A Tcl list keeps its single quotes (`'abc'` is the five-character string "'abc'"): only `{}` and `""`
  group in a list. The converter's expr substitution must therefore quote every non-numeric substituted
  value.
- SQLite's JSON parser is lenient about raw control characters inside strings: it KEEPS them and the
  serializer escapes them (`json('{label:"abc' || char(1) || 'xyz"}')` renders `{"label":"abc\u0001xyz"}`).
  Frigolite dropped the character instead, which broke 62 json501 leaves; the fix is one branch in
  `internal/function/json.go` (parseString).
- The verify-install batch's regression rule compares *normalized* leaf names, so a family whose OLD
  fixture used a different naming convention (e.g. "14.1.3" instead of "json501-14.1.3") reports false
  regressions — always read the batch's regression list against the old fixture's actual names before
  reverting a family.
- The harness must model the testfixture's helper procs that the corpus registers as SQL functions:
  `a_string`/`blob` (a per-name counter, a repeated "<counter>."/"<counter>x" string and its range
  1..n), `contained_in` (counts calls, always 0), `int2hex` (`format %.2X`), `refcnt`, `r` (records the
  fired triggers), `var`, `x`, `ceval`. Each unsupported proc is a whole family of failures.
- The skip-scan EQP marked every WHERE comparison as "=", so a range (`b > 12 AND b < 16`) rendered as
  `b=?` instead of SQLite's `b>? AND b<?`. Record the real operator and combine two bounds on one column
  into "BETWEEN" (skipscan5: 36 → 30 leaves; the rest need the skip-scan *plan*, not the detail).
- A Tcl `for` loop whose start value comes from a query result cannot be unrolled offline. It is now
  captured as a `runtime_loop` fixture step (loop variable, start/condition expressions, body); the
  harness evaluates the bounds with the runtime variables, binds the loop variable per iteration and
  runs the body as ordinary statements. The loop variable must be marked *dynamic* so that references to
  it stay "$name" in the condition and the body (the harness binds it from the runtime variables).
  pager2: 1,971 → 1 failing leaves.
- The harness's `query_assign` step compared NO expectation: it stored the value in the runtime variables
  and dropped the assertion, so where7's 2,001 "scan N sort M" leaves were vacuous. Every step that
  carries an expectation must compare it (the check is in place now; the counters themselves need
  SQLite's SQLITE_STMTSTATUS_FULLSCAN_STEP/_SORT).
- The runtime-variable expansion must only split a "$name" reference at an inner '$' when the remainder
  is another parameter reference: "$as$bs" is two placeholders, but "$tcl" is one variable whose name
  starts with a known one.
- A multi-statement step that the harness types as a `query` only reports the last statement's rows; the
  DDL inside it is not applied the way later steps expect (btree01: the CREATE TABLE inside the 1.1
  "query" step is lost, so 1.2.x reports "no such table: t1" although the engine runs the same SQL
  correctly through DB.Query). Multi-statement bodies must be split into one step per statement.
- The testfixture's `list_all_triggers` helper lists the triggers of EVERY schema (main, temp and the
  attached databases), each name prefixed with its schema ("main.tr1 temp.tr1 aux.tr1"). The harness now
  models it (e_droptrigger 62 → 53).
- The `query_assign` step's expectation must be compared against the *rendered rows* (the same rendering
  the `query` step uses) and not only the first column: a capture-style expectation names one value, so
  both renderings have to be accepted or the check produces false failures.

### Silent drops are the worst kind of failure (2026-10-20)

- **A Go `switch` without a `default` silently drops its input.** The harness's
  step switch lost its `case "query":` arm during an uncommitted rename
  (`query` -> `query2` for named results) while the converter still emitted
  `query` for every `db eval` SELECT, `db one`, `db onecolumn` and
  `do_eqp_test`. Result: **173,795 steps across 768 families were neither
  executed nor asserted** — `date4` "passed" without running a statement, and
  `btree01`'s CREATE TABLE never happened (so 1.2.x reported "no such table:
  t1"). Always add a `default:` arm that fails loudly, and add a test that
  every step type in `testdata/*.json` is handled.
- **Never let a converter command fall back to "return the input".** The
  converter's `cmdString` answered every unknown `string` subcommand with
  `args[1]`, so `string trimright 90821.8400 0` returned "90821.8400" instead
  of "90821.84" — 9,356 wrong expectations in `round1`. Unknown subcommands
  must be an *error* (a reported conversion failure), never a plausible lie.
- **A `do_test` expectation describes the BODY's value, not the last captured
  statement.** `do_test N { ...; set rc [expr {$t1 eq $t2}]; set rc } {1}`
  expects the boolean, not the rows of the last SELECT. Attaching the
  expectation to the last captured statement made `rtree2` compare 1000 rows
  with "1" (2,010 false failures). Model the body's value source explicitly:
  last statement / `set X` / `expr {...}`.
- Tcl's `string trimright`/`trimleft` take a *charlist*; `string match` has
  **no** class negation (`[^a]` is the literal set {^,a}); `round()` in SQLite
  returns a REAL (`sqlite3_result_double`, `func.c`), so `round(90821.84004,4)`
  renders as "90821.84" — a trailing-zero expectation is always a converter
  bug.
- `do_test NAME { body } { expected }` compares the **body's value** (the
  result of its last command) with the expectation — never the result of the
  last statement the body captured. Model the body's value source explicitly:
  last statement / `set X` / `expr {...}`; the expectation follows it.
- Tcl's `execsql` and `db eval` yield a **list of rows** (each row a list of
  columns), not one row: a fixture variable that is compared at run time must
  carry every row (the `resultRows` field), otherwise the comparison silently
  degenerates to the first row.
- A Tcl `catch` around a SQL capture cannot fail at conversion time, so the
  converter always takes the branch the runtime would take only on success.
  A fixture model without **conditional steps** therefore loses the branch
  condition (rtree2's `catch` + `if/else`); the model needs a `condition`
  field evaluated by the harness.
- The Tcl expression evaluator in the harness (`evalHarnessTclExpr`) treats
  `{...}` as a braced *string*: the expression text must have its outer braces
  stripped before it is handed to the evaluator.
- **A recursive CTE must not be materialized in the heap.** `sort3-3` builds
  2.2 M rows of 1 KB blobs; our engine accumulated every row in a Go slice
  (1.4 GB live, >20 GB across the corpus) while SQLite streams the recursive
  result through an ephemeral temp table. Measure the live heap with
  `GODEBUG=gctrace=1` (`1955->1975->1410 MB` = the tell) before blaming the
  compiler or the harness.
- Go returns freed pages to the OS lazily, so a long test binary accumulates
  its *peak* resident set across subtests. `debug.FreeOSMemory()` after each
  family (plus a soft `debug.SetMemoryLimit`) keeps the corpus's footprint
  bounded — **but on macOS `FreeOSMemory` does not shrink the RSS** (the
  runtime uses `madvise(MADV_FREE)`, so the pages stay resident until the
  kernel reclaims them; measured: 4.81 GB before and after a 2.5 GB
  alloc/free cycle). The only effective lever on macOS is the soft heap
  limit (`debug.SetMemoryLimit`).
- `FRIGOLITE_TEST=sort` matches every family whose name *contains* "sort"
  (sort, sort2…sort5, sortfault) — a per-family memory measurement must use
  the exact name, not a substring.

## Shard allocation evidence (2026-10-23)

Four-shard broad JSON run exceeded approved 10-minute ceiling with JSON-byte + case-count weighted largest-first greedy allocation: shards 0/1/3 reached corpus tails and failed; shard 2 remained in `walfault` with only RUN output, no completion statuses. Launcher run under zsh omitted sidecar exits. Size/case volume is not adequate runtime predictor for expensive families; next allocation should use deterministic observed durations with fixture size/cases as fallback prior. Keep all fixtures and selection/skip rules unchanged; use Bash explicitly, isolated dirs, durable per-shard exit records.

## The quadratic tokenizer (2026-10-22)

SQLite's FTS3 expression parser re-tokenizes the *remaining* buffer for every
token it reads (fts3_expr.c's getNextToken opens a fresh tokenizer cursor each
time). That is O(n^2) in the length of the expression. fts3expr3.test builds
a 1.5 MB expression (`create_toggle_tree 17`), so a single case needed
billions of byte scans: the family took 45 s per case and the suite's
resident set grew past 4.5 GB (the run had to be killed twice).

The port scans the tokens once per expression and walks them. The 1.5 MB
expression now parses in 107 ms and the suite's resident set stays under
2.4 GB.

**The lesson**: a port that follows the C code's *structure* must still be
checked against the C code's *complexity*. A quadratic loop that is fast in
C is minutes in Go on a corpus-sized input. When a family suddenly needs
minutes, suspect the algorithm, not the machine — and measure the resident
set, not just the wall clock.

**The second lesson**: the corpus contains inputs that are orders of
magnitude larger than anything the fixtures suggest (a 1.5 MB SQL literal).
The per-family statement budget hides them: the conversion looked fine, the
*run* did not.

- Before pushing, inspect complete unpushed history, not just current tree sizes: a squash does not remove oversized blobs already reachable from the remote base. For generated reports, retain useful local copies but exclude them from the replacement commit; verify every outgoing blob against GitHub's 100 MiB limit. Keep a backup ref before rewriting history.
