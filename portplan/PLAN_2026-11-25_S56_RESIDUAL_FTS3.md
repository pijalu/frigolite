# Plan — S56 residual FTS3 auto/defer fixture closure

## Evidence

`/tmp/t20_full.log`:

- `fts3auto/setup_2`: `exec error: near ")": syntax error` on
  `INSERT INTO t1 () VALUES('aaaaaaa caaaaa')`.
- `fts3defer3/fts3defer3-1.7`: `expected error but got success` on
  `SELECT count(*) FROM t1 WHERE t1 MATCH 'b';`.

SQLite sources:

- `../sqlite/test/fts3auto.test:115-128`: `fts3_make_deferrable` obtains the
  column name with `sqlite3_column_name(db_prepare("SELECT * FROM $tbl"), 0)`
  and inserts into `($name)`.
- `../sqlite/test/fts3defer3.test:64-71`: test 1.4 corrupts the large `b`
  doclist with `set fd [db incrblob t1_segments block 2]`, writes 3204 NUL
  bytes, closes the handle; 1.7 then requires
  `database disk image is malformed` for `MATCH 'b'`.

Current converter gaps:

- `tools/tclconvert/tcl/interp_cmds.go` has `sqlite3_column_text` but no
  `sqlite3_column_name`, so `$name` becomes empty and the fixture emits `()`.
- `db incrblob` and `puts -nonewline $fd` on that handle are ignored, so the
  corruption step is absent from `testdata/fts3defer3.json`.

## Hypotheses

1. Engine FTS column naming is fine; only fixture regeneration with real
   `sqlite3_column_name` semantics is needed.
2. Engine corrupt-segment detection is fine; the fixture must materialize the
   incrblob NUL write as an SQL blob overwrite before 1.7. Pure-Go pins must
   validate both contracts before any converter edit.

## Revised diagnosis after pure-Go pins

Both pins failed before any converter work:

- `Stmt.Columns()` is empty before `Step`: `stmt.result` is only populated on
  execution, but SQLite's `sqlite3_column_name` reads VDBE column names fixed
  at prepare time. Fix: capture result names at `Prepare` via a new
  `exec.Engine.PrepareColumnNames` helper and keep them in a dedicated
  `Stmt.preparedColumns` field; `Columns()` falls back to them until a result
  exists.
- Zeroing segment block 2 applies cleanly, but `MATCH 'b'` returns 0 rows
  instead of SQLITE_CORRUPT. `readLeafChainTerm` accepts a first term whose
  length varint is 0; `fts3_write.c:1394-1399` rejects `nSuffix<=0` with
  `FTS_CORRUPT_VTAB`. Fix: treat a zero first-term length as a hard framing
  error in `readLeafChainTerm`, so the segment-load error is recorded and
  queries that read the damaged doclist fail.

The converter work remains necessary: without it, the fixtures never ask the
engine for the column name or never apply the corruption. Both layers must be
fixed.

## Ordered steps

1. Pure-Go pins (`frigolite_fts3auto_defer3_t21_pin_test.go`) were added and
   failed, localizing the two engine gaps above.

2. Engine: prepare-time column names
   - Add `exec.Engine.PrepareColumnNames(sql.Stmt) []string` next to
     `buildColumnNames`; use `SelectEngine.BuildColumnNames(sel.Columns, nil,
     sel)` for SELECT statements.
   - Add `Stmt.preparedColumns`, populate in `DB.Prepare`, and return it from
     `Stmt.Columns()` when no executed result exists.

3. Engine: zero-length first term in an FTS segment leaf is a hard corrupt
   framing error, mirroring `fts3_write.c`'s `nSuffix<=0`; add a focused
   internal/fts unit pin.

4. Converter: `sqlite3_column_name`
   - Add `ColumnIndex`/`AssignVariable` fields to `tcl.Stmt` and `main.TestStep`
     and `ColumnIndex`/`AssignVariable` to harness `TestStep`.
   - Add `commandColumnName` in `interp_stmt.go`, registered in
     `harnessRuntimeCommands`, emitting `stmt_column_name` with the prepared
     handle and assigning the name to the TCL variable marker.
   - Harness `prepare_stmt` neighbor case reads `stmt.Columns()[i]` and stores
     the name in `runtimeVars[assign]`.
   - Add converter unit test for `sqlite3_column_name $stmt 0`.

5. Converter: `db incrblob`
   - Extend `cmdDB` to recognize `incrblob TABLE COLUMN ROWID`; return a new
     handle and track it in an incrblob-write map.
   - Extend `commandPuts` for `puts -nonewline $fd <zeros>` on such a handle;
     accumulate data and validate it is all NUL bytes.
   - Extend `commandClose` to emit an `exec` step
     `UPDATE TABLE SET COLUMN = zeroblob(length(COLUMN)) WHERE rowid = ROWID`
     for a closed incrblob handle with all-zero content (SQLite's incrblob
     semantics: overwrite in place, preserve blob length).
   - Add converter unit test covering the fts3defer3 1.4 shape.

6. Regenerate `testdata/fts3auto.json` and `testdata/fts3defer3.json` with
   `go run ./tools/tclconvert/ -outdir testdata fts3auto.test fts3defer3.test`.
   Focused gates:
   `FRIGOLITE_TEST=fts3auto` and `FRIGOLITE_TEST=fts3defer3`.

7. Quality: `tools/quality_gate.sh`; compare normalized
   staticcheck/gocognit/gocyclo sets against `/tmp/t20_quality.log` (NEW=0).
   Converter test suite and `go test ./...` as applicable.

8. Full gate detached with `-timeout 30m`, raw-byte leaf fail-set diff versus
   `/tmp/t20_full.log` and `/tmp/t10_full2.log`. Both residuals must be GONE;
   NEW=0 except documented flakes confirmed by focused runs.

9. Commit code, then docs/handoff/lessons/TODO. Mark t21 done only after the
   full gate passes.

## Verification

- Pure-Go pins pass before converter edits.
- Focused family gates show zero leaves.
- Full gate raw fail-set loses exactly `fts3auto/setup_2` and
  `fts3defer3/fts3defer3-1.7`, gains no new leaf.
- Quality NEW=0; committed files include ignored converter subtree with
  `git add -f` and `git show --stat` check.

## Execution notes

- Pure-Go pins initially failed, revealing two engine gaps rather than a
  fixture-only defect: `Stmt.Columns()` was execution-only, and a zero-length
  first leaf term was accepted. Both were fixed before fixture edits.
- Corruption attribution needed a third engine change: a failed interior
  child records its boundary term as a corrupt doclist; queries that defer
  that token skip the check, matching `fts3.c:5195-5240`'s `nToken>1` rule.
- Full regeneration of `fts3auto.json` exposed unrelated converter gaps in
  proc-argument `lindex $args` capture. The durable fixture was retained and
  patched only for the real column list and the missing `COMMIT`; the new
  converter command is pinned by `interp_t21_test.go`.
- Code committed as `56b228df9` (converter files forced into the index).
