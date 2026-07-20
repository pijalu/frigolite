# Patched readline library

This is a local copy of `github.com/lmorg/readline/v4` v4.2.4 with a fix for a
crash when tab-completing then pressing Enter.

## Bug

`UnicodeT.Set()` updated the line value but did not reset the cursor position
(`rPos`). If the new line was shorter than the previous one, `rPos` could be
out of bounds, causing a slice bounds panic when `echoStr()` → `lineWrapCellPos()`
attempted `Runes()[:RunePos()]`.

## Fix

Added `rPos` bounds clamping in `Set()`:

```go
if u.rPos > len(u.value) {
    u.rPos = len(u.value)
}
```

## Tracking

- Upstream: `github.com/lmorg/readline/v4` v4.2.4
- Applied: 2025-07-20
- Revisit: check if upstream has fixed this in a newer release
