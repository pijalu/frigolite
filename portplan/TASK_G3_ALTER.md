# TASK G3.ALTER — ALTER TABLE (RENAME, ADD/DROP/RENAME COLUMN, RENAME TO)

> **Phase**: G3 (schema & constraints).
> **Goal**: G3.ALTER.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G1.CREATE; G3.TRIGGER (dependency rewrite); G3.INDEX (index rebuild).
> **Current state: FAILING** — `alter` fails (much recent work per git log).

## Objective
ALTER TABLE matches SQLite: `RENAME TO` (rewrites table name + every dependent
schema object: views, triggers, indexes, FK references), `RENAME COLUMN old TO
new`, `ADD COLUMN` (with type/constraints/DEFAULT, no PK, no non-constant
default unless...), `DROP COLUMN` (rebuilds table; updates dependent indexes/
views/triggers), and the legacy `ALTER TABLE ... RENAME table`. All dependency
rewrites must be byte-accurate against `sqlite_master` text. Existing recent
work (see git log `fix(alter)…`) handled many cases — this task closes
remaining `alter`/`altercol`/`altertab`/`altertrig`/`alterdropcol` failures.

## Scope — testgen packages
`alter`, `altercol`, `altertab`, `altertrig`, `alterdropcol`, `altercons`,
`alterlegacy`, `alterqf`. (`alterauth` is auth-based → coordinate internal/auth;
`altercorrupt`/`alterfault`/`altermalloc` are fault/corrupt → N/A.)

## Pre-test file
`frigolite_p3_alter_test.go` — `TestP3Alter_*` (file already exists — extend it).
Cases vs oracle:
- RENAME TO: table name updated; views/triggers/indexes/FKs that reference it
  rewritten in `sqlite_master` (exact text).
- RENAME COLUMN a→b: all references in dependent schema objects rewritten;
  quoting preserved; DQS-off reparse validation.
- ADD COLUMN: type, NOT NULL with constant DEFAULT, DEFAULT expr, generated? (no
  — ADD cannot add generated except via special rules — match SQLite).
- DROP COLUMN: table rebuilt; indexes/views/triggers referencing the column
  error or are rebuilt per SQLite ("error in <type> <name> after drop column").
- Legacy RENAME table.

## SQLite source references
- `src/alter.c` — `sqlite3AlterRenameTable`, `sqlite3AlterRenameColumn`,
  `sqlite3AlterFinishAddColumn`, drop-column rebuild.
- `internal/rename/` — frigolite's dependency analysis (rename_test/quotefix funcs).

## Steps
- [ ] **G3.ALTER.1** Extend pre-test suite; record remaining failures.
  Commit: `G3.ALTER.1: extend ALTER pre-test suite`.
- [ ] **G3.ALTER.2** Run `alter` testgen; collect failures; triage each via
  pure-Go test (engine vs transpiler). Commit per fix:
  `G3.ALTER.2.<n>: <fix>`.
- [ ] **G3.ALTER.3** RENAME COLUMN dependency rewrite byte-accuracy (views,
  triggers, check-constraints, index expressions) — DQS-aware. Commit:
  `G3.ALTER.3: RENAME COLUMN dependency accuracy`.
- [ ] **G3.ALTER.4** ADD COLUMN edge cases (non-constant default, NOT NULL
  without default, type affinity of new column). Commit: `G3.ALTER.4: ADD COLUMN`.
- [ ] **G3.ALTER.5** DROP COLUMN rebuild + dependent-object errors.
  Commit: `G3.ALTER.5: DROP COLUMN rebuild`.
- [ ] **G3.ALTER.6** altercol/altertab/altertrig/alterdropcol/altercons/
  alterlegacy/alterqf green. Commit: `G3.ALTER.6: ALTER TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/alter/ ./testgen/altercol/ ./testgen/altertab/ ./testgen/altertrig/ ./testgen/alterdropcol/ ./testgen/altercons/ ./testgen/alterlegacy/ ./testgen/alterqf/ && \
go test -run 'TestP3Alter' -count=1 . && \
go build ./...
```

## Goal create command
```
goal create \
  objective "ALTER TABLE matches SQLite: RENAME TO (rewrite all dependent schema objects byte-accurate), RENAME COLUMN (DQS-aware dependency rewrite), ADD COLUMN (type/constraints/DEFAULT rules), DROP COLUMN (table rebuild + dependent errors), legacy RENAME. Much prior work exists (git log fix(alter)). Close remaining alter/altercol/altertab/altertrig/alterdropcol failures. alter currently FAILS. See portplan/TASK_G3_ALTER.md." \
  completionCriterion "testgen alter, altercol, altertab, altertrig, alterdropcol, altercons, alterlegacy, alterqf PASS and TestP3Alter pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/alter/ ./testgen/altercol/ ./testgen/altertab/ ./testgen/altertrig/ ./testgen/alterdropcol/ ./testgen/altercons/ ./testgen/alterlegacy/ ./testgen/alterqf/ && go test -run TestP3Alter -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G3.ALTER. alter FAILS but much prior work (git log fix(alter)*). Dependency analysis in
internal/rename/; schema ops in internal/exec/alter.go + internal/schema. Schema text stored verbatim.
Decisions: RENAME rewrites dependent objects in sqlite_master byte-accurate; DQS-off reparse on drop column.
Next: run alter testgen, triage each failure (pure-Go test first), fix per category.
Risks: byte-accuracy of rewritten schema text is brittle — diff against sqlite3 .schema.
Carried limits: verifyCommand above.
```
