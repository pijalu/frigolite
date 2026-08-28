# TASK G1.SELECT — SELECT (projection, DISTINCT, ORDER BY, LIMIT, compound)

> **Phase**: G1 (CRUD core — critical path).
> **Goal**: G1.SELECT.
> **Read first**: `PORTPLAN.md`, `portplan/GUIDELINES.md`.
> **Depends on**: G0.GRAMMAR; G1.CREATE.

## Objective
SELECT projection, `*` / `t.*` star expansion, column/result aliases, DISTINCT,
ORDER BY (multi-key, ASC/DESC, NULLS FIRST/LAST, expression keys, COLLATE),
LIMIT/OFFSET, FROM-less SELECT (`SELECT 1 WHERE 0`), and **float formatting**
matching SQLite's `%!.15g` style exactly. Compound queries (UNION/INTERSECT/
EXCEPT) split into G2.SETOPS; basic single-SELECT projection+ordering lives here.

## Scope — testgen packages
`select2`, `select3`, `select4`, `select5`, `select6`, `select7`, `select8`,
`select9`, `selectA`–`selectH`. (The big `selectG`/`selectH` join-heavy suites
overlap G2.JOIN — coordinate.)

## Pre-test file
`frigolite_p1_select_test.go` — `TestP1Select_*`. Cases vs oracle:
- Projection of columns, literals, expressions; `*`; `t.*` for multi-table.
- Result-column aliases (`AS name`, bare alias); alias usable in ORDER BY/HAVING.
- DISTINCT vs ALL; DISTINCT with NULLs (NULLs are equal for DISTINCT).
- ORDER BY: by column, by expression, by output-column ordinal/alias,
  ASC/DESC, multiple keys, NULLS FIRST/LAST, COLLATE per key.
- LIMIT n; LIMIT n OFFSET m; LIMIT -1 (no limit).
- FROM-less SELECT; `SELECT 1 WHERE 0` → empty (already partly fixed).
- Float formatting: `1.0`, `0.1+0.2`, large/small exponents — match oracle byte-for-byte.
- Quoting of identifiers in output when needed; `||` concat projection.

## SQLite source references
- `src/select.c` — `sqlite3Select`, compute-column-names, DISTINCT, ORDER BY,
  compound query materialization.
- `src/printf.c` — float → text (`%!.15g`).

## Steps
- [x] **G1.SELECT.1** Pre-test suite (include float-format cases). Commit:
  `G1.SELECT.1: SELECT pre-test suite`.
- [x] **G1.SELECT.2** Star expansion `*` and `t.*` across multi-table FROM;
  correct column count + qualified names. Commit: `G1.SELECT.2: star expansion`.
- [x] **G1.SELECT.3** ORDER BY: alias/ordinal/expression keys; NULLS FIRST/LAST;
  COLLATE; stable secondary ordering. Commit: `G1.SELECT.3: ORDER BY semantics`.
- [x] **G1.SELECT.4** DISTINCT three-valued logic (NULLs equal). Commit:
  `G1.SELECT.4: DISTINCT NULL handling`.
- [x] **G1.SELECT.5** LIMIT/OFFSET incl. negative limit. Commit:
  `G1.SELECT.5: LIMIT/OFFSET edge cases`.
- [x] **G1.SELECT.6** Float formatting parity (harness tclRenderCell uses SQLite's
  %!.15g; pre-tests assert byte-for-byte). Commit: `G1.SELECT.6: float formatting parity`.
- [x] **G1.SELECT.7** Result-column name derivation (e.g. `SELECT a+b` → name
  `a+b`; alias precedence). Commit: `G1.SELECT.7: column name derivation`.
- [x] **G1.SELECT.8** testgen select2–selectF green (join-heavy parts coordinate
  with G2.JOIN; selectD join tests also green). Commit: `G1.SELECT.8: SELECT TCL green`.

## Verify command
```bash
go test -tags testgen -count=1 ./testgen/select2/ ./testgen/select3/ ./testgen/select4/ ./testgen/select5/ ./testgen/select6/ ./testgen/select7/ ./testgen/select8/ ./testgen/select9/ ./testgen/selectA/ ./testgen/selectB/ ./testgen/selectC/ ./testgen/selectD/ ./testgen/selectE/ ./testgen/selectF/ && \
go test -run 'TestP1Select' -count=1 . && \
go build ./...
```
(Defer selectG/selectH to G2.JOIN if they fail purely on joins.)

## Goal create command
```
goal create \
  objective "SELECT projection, star expansion, aliases, DISTINCT, ORDER BY (NULLS FIRST/LAST, COLLATE, expr keys), LIMIT/OFFSET, FROM-less SELECT, and exact float formatting. See portplan/TASK_G1_SELECT.md." \
  completionCriterion "testgen select2-selectF PASS and TestP1Select pre-tests PASS." \
  verifyCommand "go test -tags testgen -count=1 ./testgen/select2/ ./testgen/select3/ ./testgen/select4/ ./testgen/select5/ ./testgen/select6/ ./testgen/select7/ ./testgen/select8/ ./testgen/select9/ ./testgen/selectA/ ./testgen/selectB/ ./testgen/selectC/ ./testgen/selectD/ ./testgen/selectE/ ./testgen/selectF/ && go test -run TestP1Select -count=1 . && go build ./..." \
  freshContext true
```

## Handover note (template)
```
State: G1.SELECT. [done + outputs]. Float formatting must match SQLite %!.15g.
Star expansion + column-name derivation in internal/exec/select.go.
Decisions: compound queries (UNION/..) deferred to G2.SETOPS.
Next: pre-tests, then float-format + ORDER BY/DISTINCT, then select2–F.
Risks: join-heavy selectG/H depend on G2.JOIN.
Carried limits: verifyCommand above.
```
