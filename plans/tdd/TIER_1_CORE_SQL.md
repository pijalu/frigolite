# TDD Tier 1 — Core SQL

> **Goal**: All CREATE/INSERT/SELECT/DELETE/UPDATE/DROP tests pass.
> **Files**: ~50 files, ~800 TODOs remaining.
> **Workflow**: One file at a time, smallest fix possible, RED→GREEN→COMMIT.

## File Order (by TODO count, ascending)

### Sub-tier 1a: Zero-TODO files (already pass — verify first)
These files should already produce zero TODOs. Verify they compile and pass.

- `select1` — basic SELECT
- `insert1` — basic INSERT
- `delete1` — basic DELETE
- `update1` — basic UPDATE
- `create_table` — CREATE TABLE
- `drop_table` — DROP TABLE

### Sub-tier 1b: 1-5 TODOs (quick wins)
Each of these has very few missing features.

| File | TODOs | Likely Issue |
|------|-------|-------------|
| `aggerror` | 1 | C API call |
| `aggnested` | 1 | C API call |
| `aggorderby` | 1 | C API call |
| `alias` | 1 | C API call |
| `between` | 1 | edge case |
| `bitvec` | 1 | C API |
| `blob` | 2 | blob test |
| `case` | 2 | CASE expression edge case |
| `cast` | 1 | CAST |
| `collate` | 1 | collation |
| `default` | 2 | DEFAULT values |
| `distinct` | 4 | DISTINCT edge cases |
| `expr` | 4 | expression edge cases |
| `function` | 1 | function |
| `index` | 3 | index |
| `intpkey` | 2 | integer primary key |
| `join` | 2 | JOIN |
| `like` | 4 | LIKE |
| `limit` | 2 | LIMIT |
| `notfound` | 1 | not found |
| `notnull` | 2 | NOT NULL |
| `null` | 2 | NULL handling |
| `orderby` | 2 | ORDER BY |
| `primarykey` | 2 | PRIMARY KEY |
| `rowid` | 2 | rowid |
| `select2` | 2 | SELECT |
| `select3` | 2 | SELECT |
| `select4` | 3 | SELECT |
| `sort` | 2 | sort |
| `subquery` | 1 | subquery |
| `table` | 2 | table |
| `transitive` | 3 | transitive |
| `trigger` | 2 | trigger |
| `type` | 1 | type |
| `unique` | 2 | UNIQUE |
| `view` | 3 | view |
| `where` | 3 | WHERE |

*(The above are approximate — actual counts may vary)*

### Sub-tier 1c: 6-20 TODOs

| File | TODOs | Notes |
|------|-------|-------|
| `insert2` | ~10 | INSERT variants |
| `insert3` | ~8 | INSERT ... SELECT |
| `delete2` | ~8 | DELETE with JOIN |
| `delete3` | ~10 | DELETE with subqueries |
| `update2` | ~8 | UPDATE with JOIN |
| `select5` | ~12 | SELECT edge cases |
| `where2` | ~15 | WHERE with functions |
| `expr2` | ~18 | expression edge cases |
| `orderby2` | ~10 | ORDER BY edge cases |
| `type2` | ~15 | type affinity |

### Sub-tier 1d: 21-100 TODOs

| File | TODOs | Notes |
|------|-------|-------|
| `where3` | ~30 | complex WHERE |
| `where4` | ~40 | WHERE with subqueries |
| `select6` | ~25 | SELECT edge cases |

## TDD Workflow

```bash
# 1. Generate latest tests
go run ./tools/tcl2go/

# 2. Run a specific file
go test ./testgen/select1/... -v -count=1

# 3. See which TODOs remain
grep "TODO:" /tmp/testgen/*/select1_test.go

# 4. Fix the engine for the first failure
#    (edit internal/exec/engine.go, internal/sql/parser.go, etc.)

# 5. Re-run → verify fix
# 6. Check SOLID
go test -run TestSOLID_ -count=1

# 7. Commit
```

## Verification

```bash
# At end of Tier 1, must pass:
go test ./testgen/select1/... -v -count=1    # basic SELECT
go test ./testgen/insert1/... -v -count=1    # basic INSERT
go test ./testgen/delete1/... -v -count=1    # basic DELETE
go test ./testgen/update1/... -v -count=1    # basic UPDATE
go test ./testgen/create_table/... -v -count=1
go test ./testgen/drop_table/... -v -count=1
go build ./...
go test -run TestSOLID_ -count=1
```
