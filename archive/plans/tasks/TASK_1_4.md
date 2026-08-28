# Task 1.4 — Fix parse/syntax errors

> **Phase**: 1 — Fix Engine Bugs
> **Status**: 🔲 Not started
> **Files**: `internal/sql/lexer.go`, `internal/sql/parser.go`, `internal/sql/ast.go`
> **SQLite ref**: `src/tokenize.c`, `src/parse.y`
> **Estimated**: 2-3 sessions

## Description

Fix parser gaps causing ~300 parse/syntax errors. Add support for FILTER,
OVER, CTE, window frames, and other missing SQL constructs.

## Steps

- [ ] Collect all parse/syntax errors: `grep -r "syntax error\|parse error" testgen/`
- [ ] Fix top-10 parser gaps (in priority order):
  - [ ] `FILTER (WHERE ...)` clause on aggregates
        AST: add `Filter Expr` field to `FunctionCall` struct.
  - [ ] `OVER (PARTITION BY ... ORDER BY ...)` clause
        AST: add `OverClause` struct with Partition, OrderBy, Frame fields.
  - [ ] Window frame: `ROWS BETWEEN ... PRECEDING AND ... FOLLOWING`
        AST: add `WindowFrame` struct with Mode, Start, End, StartExpr, EndExpr.
  - [ ] `RETURNING` clause on INSERT/UPDATE/DELETE
  - [ ] `WITH` clause (CTE — `WITH name AS (SELECT ...)`)
  - [ ] `WINDOW win AS (...)` clause in SELECT
  - [ ] `NULLS FIRST` / `NULLS LAST` in ORDER BY
  - [ ] `GENERATED ALWAYS AS (expr) STORED` column syntax
  - [ ] `ON CONFLICT (col) DO UPDATE SET ...` upsert syntax
  - [ ] `TABLE` keyword in `SELECT * FROM TABLE(func(args))` (table-valued functions)
- [ ] Verify: parse/syntax errors drop by ≥80%
- [ ] **Commit** with message: `P1.4: fix parser gaps — FILTER, OVER, CTE, window specs`

## Verification

```bash
go test ./testgen/... -count=1 2>&1 | grep -c "parse error\|syntax error"
# Compare count before vs after
```

## Session notes

- Started:
- Completed:
- Gaps fixed:
- Parse error count before:
- Parse error count after:

## Protocol

Before fixing: reproduce → investigate → read SQLite source → fix → verify.
After completing: update status, `go build ./...`, SOLID check, commit, update PLAN.md.
