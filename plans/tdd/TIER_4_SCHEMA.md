# TDD Tier 4 — Schema & Constraints

> **Goal**: ALTER TABLE, constraints (UNIQUE, FOREIGN KEY, CHECK,
> NOT NULL, PRIMARY KEY), DEFAULT values, generated columns all work.
> **Files**: ~40 files, ~500 TODOs remaining.
> **Prerequisite**: Tiers 1-3 complete.

## Key Files

### ALTER TABLE
| File | TODOs | Notes |
|------|-------|-------|
| `alter1` | ~3 | ALTER TABLE RENAME |
| `alter2` | ~5 | ALTER TABLE ADD COLUMN |
| `alter3` | ~8 | ALTER TABLE DROP COLUMN |
| `altercol` | ~5 | ALTER COLUMN |
| `altercons` | ~8 | ALTER constraint changes |
| `altertab` | ~5 | complex ALTER TABLE |
| `altertrig` | ~3 | ALTER with triggers |
| `altercorrupt` | ~2 | ALTER corruption |

### Constraints
| File | TODOs | Notes |
|------|-------|-------|
| `unique1` | ~5 | UNIQUE constraint |
| `unique2` | ~8 | UNIQUE edge cases |
| `notnull1` | ~3 | NOT NULL |
| `notnull2` | ~5 | NOT NULL edge cases |
| `primarykey1` | ~5 | PRIMARY KEY |
| `foreignkey1` | ~8 | FOREIGN KEY |
| `foreignkey2` | ~15 | FK actions |
| `foreignkey3` | ~10 | FK edge cases |
| `check1` | ~3 | CHECK constraint |
| `check2` | ~5 | CHECK edge cases |
| `default1` | ~3 | DEFAULT values |
| `default2` | ~5 | DEFAULT expressions |

### Generated Columns
| File | TODOs | Notes |
|------|-------|-------|
| `gencol1` | ~5 | generated columns |

## Verification

```bash
go test ./testgen/alter1/... -v -count=1
go test ./testgen/unique1/... -v -count=1
go test ./testgen/notnull1/... -v -count=1
go test ./testgen/primarykey1/... -v -count=1
go test ./testgen/foreignkey1/... -v -count=1
go test ./testgen/check1/... -v -count=1
go test ./testgen/default1/... -v -count=1
go build ./...
go test -run TestSOLID_ -count=1
```
