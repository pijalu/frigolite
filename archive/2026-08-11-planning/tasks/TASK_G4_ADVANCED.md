# TASK G4 — Advanced SQL (CTE, Window Functions, UPSERT/RETURNING)

> **Phase**: G4 (depends on G3 core goals green)
> **Goal IDs**: G4.CTE, G4.WINDOW, G4.UPSERT-RETURNING
> **Read first**: `PORTPLAN.md` §0, **`portplan/DESIGN.md` §F (recursive CTE +
> window-function evaluation algorithms)**, `portplan/GUIDELINES.md`.
> **Status**: ⚪ not started (CTE parsed-only; window parsed/stubbed)

---

## Objective

Implement recursive CTEs, window functions (the 10 skipped `window*` packages),
and full UPSERT/RETURNING. CTE is currently parsed; window functions are parsed
but stubbed (return NULL/no-op) — **implement them fully**.

---

## Goal G4.CTE — WITH (common table expressions, recursive)

**Scope**: `with`, `withM`, `with1`–`with2`, recursive CTE packages, and the CTE
cases inside `select*`.

**Key engine areas**: `internal/exec/select.go` (CTE materialization + recursive
evaluation). Reference SQLite `src/select.c` (`sqlite3With`, recursive union),
`src/build.c` (`WithPush`).

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/with/ ./testgen/withM/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP4CTE' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set for `with*`/`withM`.
2. Non-recursive CTE: materialize the subquery, bind it as a named relation.
3. Recursive CTE: seed query → iterative `UNION`/`UNION ALL` until fixpoint;
   enforce the no-aggregate/no-distinct recursion restrictions.
4. MATERIALIZED / NOT MATERIALIZED hints; nested CTE; CTE in subquery.
5. Recursive-depth limit (`SetExprDepthLimit`-adjacent) to prevent runaway.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G4.WINDOW — Window functions (OVER/PARTITION/FRAME)

**Scope**: `window`, `window1`–`window9`, `windowA`–`windowE`, `windowerr`,
`windowpushd`. (10 whole-file-skipped packages — un-skip and implement.)

**Key engine areas**: `internal/exec/select.go` (window evaluation pass),
`internal/function/` (window aggregates + built-in window funcs).
Reference SQLite `src/window.c`, `src/func.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 200s \
  ./testgen/window/ ./testgen/window1/ ./testgen/window2/ ./testgen/window3/ \
  ./testgen/window4/ ./testgen/window5/ ./testgen/window6/ ./testgen/window7/ \
  ./testgen/window8/ ./testgen/window9/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP4Window' -count=1 . && go build ./... && make quality
```

**Todos**:
1. Un-skip the 10 `window*` packages from `skipTestFiles`; regenerate.
2. Parse (already stubbed) → evaluate: PARTITION BY + ORDER BY + FRAME
   (ROWS/RANGE/GROUPS, BETWEEN … AND …, UNBOUNDED/CURRENT ROW/expr PRECEDING/
   FOLLOWING).
3. Built-in window functions: `row_number`, `rank`, `dense_rank`, `ntile`,
   `lead`, `lag`, `first_value`, `last_value`, `nth_value`, `percent_rank`,
   `cume_dist`; aggregate window functions (sum/count/avg/min/max over a frame).
4. FILTER (WHERE) clause on window functions; EXCLUDE/INCLUDE frame options.
5. Window definitions in WINDOW clause; inline OVER specs.
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Goal G4.UPSERT-RETURNING — UPSERT (ON CONFLICT) and RETURNING

**Scope**: `upsert`, `upsert2`, `returning`, `returning2`, and upsert cases in
`insert*`. (The legacy plan noted "~200 upsert ON CONFLICT validation cases"
deferred — implement them.)

**Key engine areas**: `internal/exec/insert.go` (ON CONFLICT resolution),
`internal/exec/delete.go`/`update.go` (RETURNING). Reference SQLite
`src/upsert.c`, `src/insert.c`.

**Verify command**:
```bash
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/upsert/ ./testgen/returning/ 2>&1 | grep -cE '^FAIL' | grep -q '^0$' && \
go test -run 'TestP4Upsert' -count=1 . && go build ./... && make quality
```

**Todos**:
1. `tools/status` → fail set.
2. ON CONFLICT (col) DO UPDATE SET …; conflict-target WHERE (partial index);
   multi-column/expression targets; DO NOTHING.
3. UPSERT arity validation (the ~200 deferred cases) — match SQLite error messages.
4. RETURNING * / cols / exprs on INSERT/UPDATE/DELETE; aliases.
5. UPSERT on virtual tables: raise the correct error (not a crash).
6. Per fix: pre-test + oracle → fix → verify → commit.

---

## Definition of Done (this task)
- All three goals green; pre-tests pass; quality + SOLID pass; no G1–G3 regression.
- `PORTPLAN.md` §5 G4 rows → 🟢.
