# TASK G2 — Schema, Constraints & Transactions

> **Phase**: G2 (depends on G1 core goals green)
> **Goal IDs**: G2.ALTER, G2.INDEX, G2.TRIGGER, G2.FKEY-CONSTRAINTS, G2.COLLATE,
> G2.SAVEPOINT-ATTACH
> **Read first**: `PORTPLAN.md` §0, **`portplan/DESIGN.md` §D (schema designs:
> ALTER rebuild, max_page_count, sqlite_sequence, query read-locking)**,
> `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started

---

## Objective

Complete schema management, integrity constraints, triggers, foreign keys,
collations, savepoints, and ATTACH. These are the "slow" packages un-skipped in
G0 plus the constraint/fkey families. After G2, the schema layer matches SQLite.

**Triage rule**: pure-Go pre-test (`frigolite_p2_*.go`) + oracle first.

---

## Goal G2.ALTER — ALTER TABLE (rename/add/drop column, rename table)

**Scope**: `alter`, `alter2`, `altercol`, `altertab`, `altertab2`, `altertab3`,
`altertrig`, `alterdropcol`, `alterdropcol2`, `alterlegacy`, `altercons`,
`alterqf`.

**Key engine areas**: `internal/exec/alter.go`, `internal/rename/`,
`internal/schema/`. Reference SQLite `src/alter.c`, `src/build.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/alter/ ./testgen/altercol/ ./testgen/altertab/ ./testgen/altertrig/ \
  ./testgen/alterdropcol/ ./testgen/alterlegacy/ ./testgen/altercons/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP2Alter' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set for alter family.
2. RENAME TO/COLUMN: dependency rewrite (views, triggers, indexes, FKs, CHECK).
3. ADD COLUMN: default value, generated columns, NOT NULL, schema rebuild SQL.
4. DROP COLUMN: VIRTUAL/STORED slot removal, index cleanup, schema rewrite.
5. RENAME validation: reject if CHECK/index-WHERE references the table.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G2.INDEX — CREATE/DROP INDEX, expression indexes, covering scans

**Scope**: `index`, `index2`–`index9`, `expridx1`, `expridx2`, `coveridxscan`,
`indexedby`, `conflict`, `unique`, `partial` (WHERE-clause indexes).

**Key engine areas**: `internal/exec/ddl.go`, index population & the planner's
index usage (ties into G7.PLANNER — here: build/maintain real index b-trees so
covering scans & uniqueness are correct). Reference SQLite `src/build.c`
(`createIndex`), `src/insert.c` (uniqueness).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/index/ ./testgen/index2/ ./testgen/index3/ ./testgen/index4/ \
  ./testgen/index5/ ./testgen/index6/ ./testgen/index7/ ./testgen/index8/ \
  ./testgen/index9/ ./testgen/expridx1/ ./testgen/expridx2/ ./testgen/coveridxscan/ \
  ./testgen/unique/ ./testgen/conflict/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP2Index' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. Maintain real secondary-index b-trees on INSERT/UPDATE/DELETE (currently
   frigolite may not keep index b-trees — verify with a pure-Go repro).
3. UNIQUE enforcement via index; ON CONFLICT resolution (ABORT/FAIL/IGNORE/
   REPLACE/ROLLBACK); expression-index columns.
4. Covering-index scan order (coveridxscan family) — needs index-driven scan.
5. Partial indexes (WHERE clause); INDEXED BY / NOT INDEXED.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G2.TRIGGER — Triggers (before/after/instead-of, temp, recursive)

**Scope**: `trigger`, `triggerA`–`triggerG`, `temptrigger`, `triggerupfrom`,
`trigger2`.

**Key engine areas**: `internal/exec/ddl.go` (trigger storage), trigger firing
in insert/update/delete paths. Reference SQLite `src/trigger.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/trigger/ ./testgen/triggerA/ ./testgen/triggerB/ ./testgen/triggerC/ \
  ./testgen/triggerD/ ./testgen/triggerE/ ./testgen/triggerF/ ./testgen/triggerG/ \
  ./testgen/temptrigger/ ./testgen/trigger2/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP2Trigger' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. BEFORE/AFTER/INSTEAD OF; INSERT/UPDATE/DELETE events; FOR EACH ROW.
3. WHEN clause; NEW/OLD row references; recursive trigger depth limit.
4. TEMP triggers; trigger-program semantics (trigger2: prepare-time validation).
5. RAISE() function; trigger firing order; cascade interaction with FK.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G2.FKEY-CONSTRAINTS — Foreign keys, CHECK/NOT NULL, conflict, transactions

**Scope**: `fkey`, `fkey1`–`fkey_*`, `check`, `notnull`, `conflict`, `trans`,
`trans2`, `trans3`, `fkey2`–`fkey8`.

**Key engine areas**: `internal/exec/insert.go` (constraint checks),
`internal/exec/` (transaction/savepoint state). Reference SQLite `src/fkey.c`,
`src/insert.c` (`checkConstraint`), `src/vdbe` transaction opcodes.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/fkey/ ./testgen/check/ ./testgen/notnull/ ./testgen/conflict/ \
  ./testgen/trans/ ./testgen/trans2/ ./testgen/trans3/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP2FKey' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. FK enforcement (ON DELETE/UPDATE CASCADE/SET NULL/RESTRICT/NO ACTION);
   self-referential FK + INSERT OR REPLACE (carry G0.FIX-4-FAILS fkey fix).
3. CHECK constraint eval with BETWEEN + unary-plus affinity (carry check fix).
4. NOT NULL; conflict resolution across all algorithms.
5. Transactions: BEGIN/COMMIT/ROLLBACK; nested savepoints (overlap with
   G2.SAVEPOINT); statement-vs-transaction rollback.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G2.COLLATE — Collations, column naming pragmas

**Scope**: `collate`, `collateA`, `collateB`, `collate1`–`collate10`,
`colname`.

**Key engine areas**: `internal/util/compare.go`, `internal/value/`,
`internal/exec/expression.go`. Reference SQLite `src/func.c` (collation
registration), `src/select.c` (column naming: short vs full_column_names pragma).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/collate/ ./testgen/collateA/ ./testgen/collateB/ ./testgen/colname/ \
  2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP2Collate' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. BINARY/NOCASE/RTRIM collation semantics in compare & LIKE.
3. Collation precedence: column > COLLATE clause > comparison coercion.
4. `short_column_names` / `full_column_names` pragma column-naming rules.
5. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G2.SAVEPOINT-ATTACH — Savepoints, ATTACH/DETACH databases

**Scope**: `savepoint`, `savepoint2`–`savepoint7`, `attach`, `attach2`–`attach4`,
`attachmalloc`.

**Key engine areas**: `internal/pager/` (transaction nesting),
`internal/exec/` (ATTACH/DETACH, multi-DB schema). Reference SQLite `src/attach.c`,
`src/vdbe` savepoint opcodes.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 180s \
  ./testgen/savepoint/ ./testgen/savepoint2/ ./testgen/savepoint5/ \
  ./testgen/savepoint7/ ./testgen/attach/ ./testgen/attach2/ ./testgen/attach3/ \
  ./testgen/attach4/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP2Attach' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. SAVEPOINT/RELEASE/ROLLBACK TO; nested savepoint stack; interaction with FK.
3. ATTACH ':memory:'/file; schema in aux DB; DETACH (with active-query locking
   — see `tkt1873` future-feature note; implement query read-locks).
4. max_page_count enforcement (tkt2686 — was infinite loop; enforce SQLITE_FULL).
5. sqlite_sequence backed by real storage (tkt-d82e3).
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All six goals' verify commands pass; pre-tests pass; quality + SOLID pass; no
  G1 regression (`tools/status` G1 families still green).
- `PORTPLAN.md` §5 G2 rows → 🟢.
