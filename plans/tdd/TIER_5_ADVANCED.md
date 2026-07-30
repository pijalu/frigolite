# TDD Tier 5 — Advanced Features

> **Goal**: FTS, virtual tables, ATTACH/DETACH, ANALYZE, VACUUM,
> PRAGMA, backup all work (or are explicitly skipped).
> **Files**: ~100 files, ~4000 TODOs remaining.
> **Prerequisite**: Tiers 1-4 complete.

## Key Files

### Full-Text Search (FTS)
| File | TODOs | Notes |
|------|-------|-------|
| `fts1` | ~10 | basic FTS3 |
| `fts2` | ~15 | FTS3 queries |
| `fts3` | ~20 | FTS3 edge cases |
| `fts3aa` through `fts3am` | ~10-20 each | extensive FTS3 tests |
| `fts4aa` | ~15 | FTS4 |

**Strategy**: FTS is a large feature. Initially skip with a clean
`t.Skip("FTS3/4/5 not implemented yet")` and implement later.

### Virtual Tables
| File | TODOs | Notes |
|------|-------|-------|
| `vtab1` | ~5 | basic vtab |
| `vtab2` | ~8 | vtab edge cases |
| `generate_series` | ~3 | generate_series |

### ATTACH/DETACH
| File | TODOs | Notes |
|------|-------|-------|
| `attach1` | ~5 | basic ATTACH |
| `attach2` | ~8 | multiple databases |
| `attach3` | ~10 | ATTACH edge cases |

### ANALYZE
| File | TODOs | Notes |
|------|-------|-------|
| `analyze1` | ~5 | basic ANALYZE |
| `analyze2` | ~10 | ANALYZE with indexes |
| `analyze3` | ~15 | complex ANALYZE |

### VACUUM
| File | TODOs | Notes |
|------|-------|-------|
| `vacuum1` | ~3 | basic VACUUM |
| `vacuum2` | ~5 | VACUUM edge cases |

### PRAGMA
| File | TODOs | Notes |
|------|-------|-------|
| `pragma1` | ~5 | basic PRAGMA |
| `pragma2` | ~8 | PRAGMA edge cases |

### Backup
| File | TODOs | Notes |
|------|-------|-------|
| `backup1` | ~10 | backup API |

## Verification

```bash
go test ./testgen/attach1/... -v -count=1
go test ./testgen/pragma1/... -v -count=1
go build ./...
go test -run TestSOLID_ -count=1
```
