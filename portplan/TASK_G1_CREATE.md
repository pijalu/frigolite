# TASK G1.CREATE — CREATE TABLE / DDL

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.CREATE.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR (parser handles all CREATE TABLE forms).

## Objective
All `CREATE TABLE` functionality works and matches SQLite exactly: column types,
type affinity, all column/table constraints, WITHOUT ROWID, STRICT, IF NOT
EXISTS, CREATE TABLE AS SELECT, AUTOINCREMENT, generated columns.

> **Status (2026-08-06): PARTIAL.** Session fix relevant to CREATE TABLE:
> `PRIMARY KEY('x' ASC, "y" ASC)` (single-quoted string keys + ASC in a
> composite table-level PK) previously collapsed to a UNIQUE on the last
> column; `indexedColumnName` now maps a bare string literal to a column
> identifier (SQLite `sqlite3StringToId`), matching the CREATE INDEX path.
> See `TestTriageCompositePKAsc`. `types`/`strict`/`without_rowid`/`tableopts`
> still FAIL — table-level constraint parsing/STRICT/ordering work remains.

## Scope — testgen packages
`select1` (shared), `types`, `strict`, `without_rowid`, `tableopts`.
(Several of these overlap with G1.TYPES; coordinate on shared fixes — see
"Coordination" below.)

## Pre-test file
`frigolite_p1_create_test.go` — test funcs `TestP1Create_*`. Compare each case
against `/usr/bin/sqlite3`. Cases:
- Column types: INTEGER, TEXT, REAL, BLOB, NUMERIC + affinity storage classes.
- Column constraints: PRIMARY KEY, NOT NULL, UNIQUE, DEFAULT (literal+expr),
  CHECK, REFERENCES, COLLATE.
- Table constraints: PRIMARY KEY(a,b), UNIQUE(a), CHECK(expr), FOREIGN KEY.
- WITHOUT ROWID (PK defines row order; no rowid alias).
- STRICT tables (reject wrong-type values with the exact SQLite error).
- IF NOT EXISTS (no-op when present).
- CREATE TABLE AS SELECT.
- AUTOINCREMENT (INTEGER PRIMARY KEY only; monotonic; no reuse after delete).
- Generated columns: `GENERATED ALWAYS AS (expr) [STORED|VIRTUAL]`.
- WITHOUT ROWID + AUTOINCREMENT is an error (exact message).
- TEMP / TEMPORARY table scoping.

## SQLite source references
- `src/build.c` — `sqlite3CreateTable`, constraint attachment, `TF_Strict`.
- `src/prepare.c` — STRICT enforcement on bind/insert.
- `parse.y` — create table / column-def / table-constraint rules.

## Coordination
`types`/`strict` also appear in G1.TYPES. If a fix is purely about affinity or
STRICT *value* enforcement, log it in both task files so the two goals don't
duplicate work. **Owner rule:** CREATE TABLE *syntax/structure* (parsing,
constraints attached, schema text stored verbatim) belongs to G1.CREATE; affinity
*arithmetic* and value coercion belongs to G1.TYPES.

## Steps
- [ ] **G1.CREATE.1** Write `frigolite_p1_create_test.go`; run it; record
  failures vs oracle. Commit: `G1.CREATE.1: CREATE TABLE pre-test suite`.
- [ ] **G1.CREATE.2** STRICT tables reject wrong-typed values with exact message
  (`src/build.c` TF_Strict). Fix `internal/exec/ddl.go` + insert path.
  Commit: `G1.CREATE.2: enforce STRICT type checking`.
- [ ] **G1.CREATE.3** Generated columns: parse `GENERATED ALWAYS AS (expr)`
  [STORED|VIRTUAL]; evaluate on read (VIRTUAL) / write (STORED); forbid writes
  to generated cols. Fix `internal/parse/` + `internal/exec/`.
  Commit: `G1.CREATE.3: implement generated columns`.
- [ ] **G1.CREATE.4** WITHOUT ROWID: PK defines physical order; autoindex for
  UNIQUE/PK; correct error for `rowid` references. Fix `internal/exec/ddl.go`.
  Commit: `G1.CREATE.4: fix WITHOUT ROWID PK ordering`.
- [ ] **G1.CREATE.5** AUTOINCREMENT: only INTEGER PRIMARY KEY; monotonic rowid;
  `sqlite_sequence` bookkeeping; no reuse after delete-max.
  Commit: `G1.CREATE.5: correct AUTOINCREMENT semantics`.
- [ ] **G1.CREATE.6** CREATE TABLE AS SELECT: column names/types/affinity derived
  from the SELECT; no constraints except the implicit rowid.
  Commit: `G1.CREATE.6: CREATE TABLE AS SELECT`.
- [ ] **G1.CREATE.7** Schema text stored **verbatim** (DQS, expr keys) — verify
  `sqlite_master.sql` round-trips through `.schema`.
  Commit: `G1.CREATE.7: verbatim schema text storage`.
- [ ] **G1.CREATE.8** Run testgen packages; fix any transpiler-specific issues
  uncovered (triage first). Commit: `G1.CREATE.8: all CREATE TABLE TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/select1/ ./testgen/types/ ./testgen/strict/ ./testgen/without_rowid/ ./testgen/tableopts/ && \
go test -run 'TestP1Create' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "All CREATE TABLE functionality matches SQLite: types, affinity, constraints, WITHOUT ROWID, STRICT, IF NOT EXISTS, CREATE TABLE AS SELECT, AUTOINCREMENT, generated columns. See portplan/TASK_G1_CREATE.md." \
  completionCriterion "testgen select1, types, strict, without_rowid, tableopts PASS and TestP1Create pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/select1/ ./testgen/types/ ./testgen/strict/ ./testgen/without_rowid/ ./testgen/tableopts/ && go test -run TestP1Create -count=1 . && go build ./..." \
  freshContext true \
  handover "<see file>"
```

## Handover note (template)
```
State: G1.CREATE. [done list with verify outputs]. Parser is lemon-LALR (internal/parse,
rule-numbered handleRule). Schema text must be stored verbatim (DQS-aware).
Decisions: CREATE structure here; affinity arithmetic in G1.TYPES.
Next: run TestP1Create, fix per steps, then testgen packages above.
Risks: STRICT + generated-column interactions; WITHOUT ROWID ordering edge cases.
Carried limits: verifyCommand above.
```
