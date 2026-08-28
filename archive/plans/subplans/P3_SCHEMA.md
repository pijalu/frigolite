# Sub-Plan: P3 — Schema & Constraints (6 sub-goals)

> **Prerequisite**: P1, P2 complete.
> **Packages**: 47

---

## G3.ALTER — ALTER TABLE

### Goal
```
Objective: ALTER TABLE RENAME (table/column), ADD COLUMN, DROP COLUMN, ADD/DROP
CONSTRAINT, SET NOT NULL.
Completion criterion: testgen alter–altertrig all PASS.
Verify: go test -tags testgen ./testgen/alter*/ -count=1 && go test -run TestP3Alter -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p3_alter_test.go`
- ALTER TABLE t RENAME TO t2
- ALTER TABLE t ADD COLUMN c TYPE
- ALTER TABLE t DROP COLUMN c
- ALTER TABLE t RENAME COLUMN c TO c2
- ALTER TABLE t RENAME c TO c2 (without COLUMN keyword)
- ALTER TABLE t DROP CONSTRAINT name
- ALTER TABLE t ADD CONSTRAINT name CHECK(expr)
- ALTER TABLE t ALTER c DROP NOT NULL
- ALTER TABLE t ALTER c SET NOT NULL
- Dependency tracking (rename updates views/triggers/FKs that reference the table)

### Steps
1. **Write pre-test**. Commit: `G3.ALTER.1: add ALTER TABLE pre-test`
2. **Implement DROP COLUMN** — SQLite ref: `src/alter.c` (sqlite3AlterDropColumn).
   Commit: `G3.ALTER.2: implement ALTER TABLE DROP COLUMN`
3. **Implement RENAME COLUMN** — dependency tracking for renamed columns.
   Commit: `G3.ALTER.3: implement ALTER TABLE RENAME COLUMN`
4. **Implement ADD/DROP CONSTRAINT**.
   Commit: `G3.ALTER.4: implement ALTER TABLE constraint operations`
5. **Run TCL tests**. Commit: `G3.ALTER.N: ALTER TABLE TCL tests green`

---

## G3.INDEX — Indexes

### Goal
```
Objective: CREATE INDEX (single/multi/expr/partial), DROP INDEX, UNIQUE index,
IF NOT EXISTS, indexed-by hints, collation on index.
Completion criterion: testgen index, indexfault, indexedby, indexexpr PASS.
Verify: go test -tags testgen ./testgen/index/ ./testgen/indexfault/ ./testgen/indexedby/ ./testgen/indexexpr/ -count=1 && go test -run TestP3Index -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p3_index_test.go`
- CREATE INDEX on single column
- CREATE INDEX on multiple columns
- CREATE UNIQUE INDEX
- CREATE INDEX IF NOT EXISTS
- CREATE INDEX with expression (indexexpr)
- CREATE INDEX with partial WHERE clause
- DROP INDEX
- INDEXED BY hint
- NOT INDEXED hint
- Collation in index columns

### Steps
1. **Write pre-test**. Commit: `G3.INDEX.1: add index pre-test`
2. **Fix expression indexes** — index on expression, not just column.
   Commit: `G3.INDEX.2: implement expression indexes`
3. **Fix partial indexes** — CREATE INDEX ... WHERE condition.
   Commit: `G3.INDEX.3: implement partial indexes`
4. **Run TCL tests**. Commit: `G3.INDEX.N: index TCL tests green`

---

## G3.TRIGGER — Triggers

### Goal
```
Objective: CREATE/DROP TRIGGER, BEFORE/AFTER/INSTEAD OF, INSERT/UPDATE/DELETE
triggers, WHEN clause, UPDATE OF, trigger body (multiple statements), RAISE().
Completion criterion: testgen trigger–triggerG, temptrigger PASS.
Verify: go test -tags testgen ./testgen/trigger*/ ./testgen/temptrigger/ -count=1 && go test -run TestP3Trigger -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p3_trigger_test.go`
- CREATE TRIGGER ... AFTER INSERT ON t BEGIN ... END
- BEFORE UPDATE trigger with NEW/OLD references
- AFTER DELETE trigger
- INSTEAD OF trigger (on view)
- UPDATE OF (col1, col2) trigger
- WHEN clause in trigger
- Multiple statements in trigger body
- RAISE(IGNORE), RAISE(ROLLBACK, msg), RAISE(ABORT, msg), RAISE(FAIL, msg)
- DROP TRIGGER
- Recursive trigger firing (if recursive_triggers pragma on)

### Steps
1. **Write pre-test**. Commit: `G3.TRIGGER.1: add trigger pre-test`
2. **Fix trigger grammar** — rule 277 (trigger_decl) + rules 126–127 (trigger_event).
   Commit: `G3.TRIGGER.2: implement trigger grammar handlers`
3. **Implement trigger firing** — BEFORE/AFTER execution with NEW/OLD row context.
   SQLite ref: `src/trigger.c`.
   Commit: `G3.TRIGGER.3: implement trigger firing (BEFORE/AFTER)`
4. **Implement INSTEAD OF triggers** — on views.
   Commit: `G3.TRIGGER.4: implement INSTEAD OF triggers on views`
5. **Run TCL tests**. Commit: `G3.TRIGGER.N: trigger TCL tests green`

---

## G3.FK — Foreign Keys

### Goal
```
Objective: FOREIGN KEY constraints, REFERENCES, ON DELETE/UPDATE actions
(SET NULL/DEFAULT/CASCADE/RESTRICT/NO ACTION), DEFERRABLE.
Completion criterion: testgen fkey, fkey_ PASS.
Verify: go test -tags testgen ./testgen/fkey/ ./testgen/fkey_/ -count=1 && go test -run TestP3FK -count=1 .
Fresh context: true
```

### Pre-test file: `frigolite_p3_fk_test.go`
- Simple FK: child REFERENCES parent(id)
- ON DELETE CASCADE
- ON DELETE SET NULL
- ON DELETE SET DEFAULT
- ON DELETE RESTRICT
- ON DELETE NO ACTION
- ON UPDATE CASCADE / SET NULL / RESTRICT
- Composite FK (multiple columns)
- DEFERRABLE / INITIALLY DEFERRED
- FK enforcement toggle (PRAGMA foreign_keys)

### Steps
1. **Write pre-test**. Commit: `G3.FK.1: add foreign key pre-test`
2. **Implement FK constraint grammar** — rules 32–43 (ccons REFERENCES).
   Commit: `G3.FK.2: implement FK constraint grammar`
3. **Implement FK enforcement** — ON DELETE/UPDATE actions.
   SQLite ref: `src/fkey.c`.
   Commit: `G3.FK.3: implement FK enforcement and cascade actions`
4. **Run TCL tests**. Commit: `G3.FK.N: FK TCL tests green`

---

## G3.CONSTR — Constraints (UNIQUE/CHECK/NOT NULL)

### Goal
```
Objective: UNIQUE, CHECK, NOT NULL constraint enforcement, ON CONFLICT resolution.
Completion criterion: testgen conflict, notnull, check, upsert, trans, transitive PASS.
Verify: go test -tags testgen ./testgen/conflict/ ./testgen/notnull/ ./testgen/check/ ./testgen/upsert/ ./testgen/trans/ ./testgen/transitive/ -count=1 && go test -run TestP3Constr -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p3_constr_test.go`.
   Commit: `G3.CONSTR.1: add constraint pre-test`
2. **Fix UNIQUE constraint** — single and composite, NULL handling (multiple NULLs allowed).
   Commit: `G3.CONSTR.2: fix UNIQUE constraint enforcement`
3. **Fix CHECK constraint** — expression evaluation, error messages.
   Commit: `G3.CONSTR.3: fix CHECK constraint enforcement`
4. **Fix ON CONFLICT resolution** — ABORT/FAIL/IGNORE/REPLACE/ROLLBACK.
   Commit: `G3.CONSTR.4: implement ON CONFLICT resolution`
5. **Run TCL tests**. Commit: `G3.CONSTR.N: constraint TCL tests green`

---

## G3.COLLATE — Collation

### Goal
```
Objective: BINARY, NOCASE, RTRIM collations; custom collations; COLLATE in
ORDER BY, WHERE, index, column definition.
Completion criterion: testgen collate, collateA, collateB, without_rowid PASS.
Verify: go test -tags testgen ./testgen/collate/ ./testgen/collateA/ ./testgen/collateB/ ./testgen/without_rowid/ -count=1 && go test -run TestP3Collate -count=1 .
Fresh context: true
```

### Steps
1. **Write pre-test** `frigolite_p3_collate_test.go`.
   Commit: `G3.COLLATE.1: add collation pre-test`
2. **Implement BINARY/NOCASE/RTRIM** — comparison functions.
   Commit: `G3.COLLATE.2: implement built-in collations`
3. **Fix COLLATE propagation** — column-def → comparison → ORDER BY → index.
   Commit: `G3.COLLATE.3: fix COLLATE propagation through query`
4. **Run TCL tests**. Commit: `G3.COLLATE.N: collation TCL tests green`
