# TASK G3.INDEX — CREATE/DROP INDEX, UNIQUE, expression & partial indexes

> **Phase**: G3 (schema & constraints).
> **Goal**: G3.INDEX.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.CREATE; G1.WHERE (partial-index WHERE).
> **Current state: PASSING** — `index`, `indexedby`, `indexexpr`, `unique` testgen packages and `TestP3Index` pre-tests all pass; verify command green.

## Objective
Indexes match SQLite: CREATE INDEX (single/multi-column, ASC/DESC, COLLATE per
column), UNIQUE indexes (enforce uniqueness + autoindex for PK/UNIQUE
constraints), expression indexes (`CREATE INDEX i ON t(a+b)`), partial indexes
(`WHERE <expr>`), DROP INDEX [IF EXISTS], `CREATE INDEX ... IF NOT EXISTS`,
index used for correctness (we don't need the planner to *prefer* indexes, but
results must be identical whether or not an index exists), and the
`sqlite_master.sql` stored verbatim including expression keys.

> **Note:** This task covers index *creation/maintenance/correctness*. The query
> planner *choosing* to use indexes for speed is G5.ANALYZE. A test that only
> fails due to EXPLAIN QUERY PLAN output differences belongs to G5.EXPLAIN, not
> here.

## Scope — testgen packages
`index`, `indexedby`, `indexexpr`, `conflict` (UNIQUE conflict overlap —
coordinate G3.CONSTRAINTS), `unique`. Plus P6c index-flavored packages
(`descidx`, `coveridxscan`, `skipscan`, `seekscan`, `expridx`, `numindex`,
`bloom`) — triage; correctness parts here, planner-only parts to G5.

## Pre-test file
`frigolite_p3_index_test.go` — `TestP3Index_*`. Cases vs oracle:
- CREATE INDEX single/multi-col; ASC/DESC; COLLATE per column.
- UNIQUE index enforces uniqueness (INSERT conflict → exact error).
- Expression index (`a+b`); partial index (`WHERE x>0`).
- DROP INDEX; IF EXISTS; IF NOT EXISTS.
- Results identical with/without a covering index (correctness, not speed).
- Index on WITHOUT ROWID table (PK index).

## SQLite source references
- `src/build.c` — `sqlite3CreateIndex`, `sqlite3CreateForeignKey`.
- `src/insert.c` — index uniqueness check on insert/update.
- `src/where*.c` — index usage (planner; relevant for correctness equivalence).

## Steps
- [x] **G3.INDEX.1** Pre-test suite. Commit: `G3.INDEX.1: index pre-test suite`.
- [x] **G3.INDEX.2** Triage `index` failure via pure-Go test. Commit per fix:
  `G3.INDEX.2.<n>: <fix>`.
- [x] **G3.INDEX.3** UNIQUE enforcement on INSERT/UPDATE with exact error.
  Commit: `G3.INDEX.3: UNIQUE enforcement`.
- [x] **G3.INDEX.4** Expression + partial indexes (maintain on write; WHERE
  predicate gates inclusion). Commit: `G3.INDEX.4: expr + partial indexes`.
- [x] **G3.INDEX.5** Index maintenance on UPDATE of an indexed column.
  Commit: `G3.INDEX.5: index maintenance on update`.
- [x] **G3.INDEX.6** DROP INDEX + IF variants; autoindex lifecycle for PK/UNIQUE.
  Commit: `G3.INDEX.6: DROP INDEX + autoindex`.
- [x] **G3.INDEX.7** indexedby/indexexpr/unique/conflict green; triage P6c index
  packages. Commit: `G3.INDEX.7: index TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/index/ ./testgen/indexedby/ ./testgen/indexexpr/ ./testgen/unique/ && \
go test -run 'TestP3Index' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "Indexes match SQLite: CREATE/DROP INDEX, single/multi-col ASC/DESC/COLLATE, UNIQUE enforcement (exact error), expression indexes, partial indexes (WHERE), IF EXISTS/IF NOT EXISTS, autoindex for PK/UNIQUE, index maintenance on update, results identical with/without index (correctness; planner speed is G5). index currently FAILS. See portplan/TASK_G3_INDEX.md." \
  completionCriterion "testgen index, indexedby, indexexpr, unique PASS and TestP3Index pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/index/ ./testgen/indexedby/ ./testgen/indexexpr/ ./testgen/unique/ && go test -run TestP3Index -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G3.INDEX. index FAILS. Index creation in internal/exec/ddl.go; maintenance on insert/update
in internal/exec/insert.go/update.go. UNIQUE check on conflict. EXPLAIN QUERY PLAN diffs belong to G5.EXPLAIN.
Decisions: correctness equivalence (with/without index) is the bar here; planner preference is G5.ANALYZE.
Next: pre-tests, triage index, then UNIQUE + expr/partial indexes.
Risks: partial-index WHERE re-evaluation + NULL logic; expression-index key storage.
Carried limits: verifyCommand above.
```
