---
name: dlv-debug
description: Debug Go engine code with Delve (dlv) — set breakpoints, step through btree/pager/exec code, inspect page bytes and btree state. Use when printf tracing is insufficient or for engine-side bugs in frigolite (autovacuum, btree rebalance, page relocation, free page leaks).
---

# Delve Debugging for Frigolite

Use `dlv` to set breakpoints and inspect engine state in `internal/btree/`, `internal/pager/`, `internal/exec/`. Printf works for shallow issues; dlv lets you inspect live page data, walk btree structures, and trace recursive calls without recompiling.

## Quick Start

```bash
# Build a test binary (don't `go test -c` — dlv needs the test packages imported)
cd /Users/muaddib/dev/frigolite
go test -tags testgen -c -o /tmp/av.test ./testgen/autovacuum/

# Launch dlv with the test binary
dlv exec /tmp/av.test -- -test.run "^Test_autovacuum$" -test.timeout 60s -test.v
```

Inside dlv:

```
(dlv) b btree/btree_vacuum.go:RelocatePage   # break at RelocatePage
(dlv) b pager/pager.go:FreePage              # break at FreePage
(dlv) conditions bp 1 pg.PageNum == 1083     # only break for specific page
(dlv) c                                       # continue to next breakpoint
(dlv) p pg.Data[:20]                          # hex dump first 20 bytes
(dlv) p toPg.PageNum                          # inspect variables
(dlv) n                                       # next line
(dlv) s                                       # step into
(dlv) goroutines                              # list goroutines
```

## Common Recipes

### 1. Inspect a page's content

```go
// Inside dlv:
p pg.Data[:100]   // first 100 bytes
// SQLite page header: 100 bytes for 1024-byte pages
// byte 0 = page type (0x0d = leaf table, 0x05 = interior table, 0x0a = leaf index, 0x02 = interior index)
```

### 2. Trace page relocation

```
(dlv) b btree/btree_vacuum.go:RelocatePage
(dlv) c
// At each hit, inspect:
//   from, to, parentPgno, parentType
//   *pager.Page — both source and destination
(dlv) p from
(dlv) p to
(dlv) p parentPgno
(dlv) p parentType
```

### 3. Catch malformed-btree bugs

```
(dlv) b storage/storage.go:ParsePage
(dlv) conditions bp 1 len(pageData) > 0 && pageData[0] == 0x05
(dlv) c
// Check parent page's child pointers for garbage
(dlv) p pageData[100:116]   // 16 bytes: 4 rightmost + 2*6 cell pointers
```

### 4. Trace freelist + autovac

```
(dlv) b pager/pager.go:FreePage
(dlv) b pager/pager.go:Truncate
(dlv) b btree/btree_vacuum.go:IncrVacuumStep
(dlv) c
// At each IncrVacuumStep:
//   (dvl) p lastPg
//   (dvl) p pager.IsPageOnFreelist(t.pager, lastPg)
```

## Tips

- **Don't rebuild** for dlv; just `dlv exec /path/to/test.bin` to start, then `r` to run.
- **Conditional breakpoints** beat step-by-step: `(dlv) conditions bp 1 ...`
- **Print variables** with `p varname`, not `print(...)`.
- **Call functions** with `call someFunc(args)`.
- **Goto line** with `g 123` to jump within the current frame.
- **Restart** with `r` from a stopped state.
- **Attach to a running process** with `dlv attach <pid>` for tests that loop or hang.

## When to Use

- Bug is in btree/pager/exec recursive logic and printf is too noisy
- Need to inspect raw page bytes during a failing operation
- Engine hangs (use `dlv exec`, set break, then `c`; the breakpoint tells you where it's stuck)
- Need to walk a real btree from a live database (e.g. inspect the parent→child chain of a suspect page)

## When NOT to Use

- Simple Go-level issues → use `fmt.Printf` + `go test`
- Compile-time errors → `go build` is enough
- Pure-orchestration bugs (e.g. transpiler issues) → no Go state to inspect
