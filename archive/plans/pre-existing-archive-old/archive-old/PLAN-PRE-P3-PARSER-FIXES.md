# PLAN-PRE-P3 — Parser Fixes for ALTER TABLE (ParenExpr + Window Specs)

> **Prerequisite**: None — this is a pre-P3 plan that fixes two parser-level root causes blocking P3.
> **Depends on**: Nothing (can run before P0/P1/P2).
> **Goal**: Fix `parseParenExpr` (drops ParenExpr from AST) and `parseWindowClause` (discards inline window specs) so that trigger/view bodies are correctly parsed during ALTER TABLE validation.
> **SQLite reference**: `/Users/muaddib/dev/sqlite/src/alter.c` (rename validation re-parses trigger SQL), `/Users/muaddib/dev/sqlite/src/parse.y` (window function grammar).

## Context

P3 (ALTER TABLE token-level rename) requires re-parsing trigger/view SQL during validation. Two parser bugs prevent correct AST construction:

1. **ParenExpr dropped** (`parser.go:3427`): `parseParenExpr` returns `expr` directly instead of `&ParenExpr{Expr: expr}`, silently dropping parentheses from the AST. This causes `exprToString` to emit `b IN ()` instead of `(b IN ())`, and walker functions to miss column references inside parentheses.

2. **Window specs discarded** (`parser.go:3915`): `parseWindowClause` calls `skipInlineWindowSpec()` for inline window specs like `OVER (ORDER BY d)`, which parses but discards all window spec contents. The `WindowDef` remains nil, so column references inside window specs are lost from the AST.

## Current State

### ParenExpr handlers already present (4 locations):
| Function | File:Line | Handler |
|---|---|---|
| `evalExpr` | `engine.go:7910` | `case *sql.ParenExpr: return e.evalExpr(v.Expr, row)` |
| `exprToString` | `engine.go:1967` | `case *sql.ParenExpr: return "(" + exprToString(v.Expr) + ")"` |
| `collectExprTriggerColRefs` | `engine.go:6491` | `case *sql.ParenExpr: collectExprTriggerColRefs(e.Expr, refs, inSubquery)` |
| `collectExprRange` | `rename.go:203` | `case *sql.ParenExpr: collectExprRange(e.Expr, ctx, ranges)` |

### ParenExpr handlers MISSING (4 locations — will regress if fix applied without these):
| Function | File:Line | Risk |
|---|---|---|
| `walkExpr` | `engine.go:1484` | Column refs inside parens not visited |
| `collectUsingColumns` | `engine.go:3514` | USING columns inside parens missed |
| `collectExprRefs` | `engine.go:7610` | Column refs inside parens missed |
| `extractConst` | `engine.go:1517` | Constants inside parens return nil |

### Window spec handling:
- `parseWindowClause` (parser.go:3915): For `OVER (spec)`, calls `skipInlineWindowSpec()` which discards all contents. `ov` remains nil.
- `parseOneWindowDef` (parser.go:584): Correctly parses `PARTITION BY` and `ORDER BY` inside parentheses for WINDOW clause definitions. Can be reused/adapted for inline specs.
- `skipInlineWindowSpec` (parser.go:4003): Parses but discards — should be replaced with a parse-and-store variant.

### normalizeSQL workaround (confirms ParenExpr bug):
`frigolite_harness_test.go:366-370` — regex converts `(b IN ())` to `b IN ()` to work around dropped parentheses. This workaround can be removed once the parser fix lands.

## SOLID Design Approach

### S — Single Responsibility
- **`UnwrapParenExpr(expr Expr) Expr`**: One function, one job — recursively unwrap `*ParenExpr` to the underlying expression. Lives in `internal/sql/expr_util.go`.
- **`parseInlineWindowSpec() *WindowDef`**: One function, one job — parse `(PARTITION BY ... ORDER BY ... frame_spec)` and return a populated `*WindowDef`. Lives in `internal/sql/parser.go`.

### O — Open/Closed
- **ParenExpr**: Rather than modifying every type switch's case logic, add `case *sql.ParenExpr` that forwards to the inner expression. This is additive — existing cases are untouched. For evaluation entry points, use `UnwrapParenExpr` as a preprocessing step (zero modification to switch bodies).
- **Window specs**: Replace `skipInlineWindowSpec()` with `parseInlineWindowSpec()` — additive change, no modification to existing parsing logic.

### L — Liskov Substitution
- `*ParenExpr` must produce identical results to its inner `Expr`. The forwarding cases guarantee this: `evalExpr(ParenExpr(e)) == evalExpr(e)`, `exprToString(ParenExpr(e)) == "(" + exprToString(e) + ")"`, etc.
- `*WindowDef` from `parseInlineWindowSpec` must be interchangeable with one from `parseOneWindowDef` — same struct, same fields.

### I — Interface Segregation
- `UnwrapParenExpr` takes and returns the minimal `Expr` interface — no concrete types.
- `parseInlineWindowSpec` returns `*WindowDef` — a focused struct with only window-spec fields.

### D — Dependency Inversion
- Walker/collector functions depend on the `Expr` interface, not concrete types. Adding `case *sql.ParenExpr` respects this — it's a new case on an existing interface, not a new dependency.
- `parseWindowClause` depends on `parseInlineWindowSpec` (a parser method), not on external packages.

## Implementation Steps

### Step 1: Create `internal/sql/expr_util.go` with `UnwrapParenExpr`

**File:** `internal/sql/expr_util.go` (NEW)

```go
package sql

// UnwrapParenExpr recursively unwraps *ParenExpr nodes to return the
// underlying expression. This allows ParenExpr to be a transparent
// wrapper that does not require explicit case handling in every
// type switch — callers can preprocess with UnwrapParenExpr at
// entry points.
func UnwrapParenExpr(expr Expr) Expr {
    for {
        if p, ok := expr.(*ParenExpr); ok {
            expr = p.Expr
        } else {
            return expr
        }
    }
}
```

**SOLID rationale:** Single Responsibility (one function, one job). Open/Closed (additive — doesn't modify existing code). Dependency Inversion (operates on `Expr` interface).

### Step 2: Fix `parseParenExpr` to return `&ParenExpr{Expr: expr}`

**File:** `internal/sql/parser.go`, line 3427

Change:
```go
p.expect(TokenRParen)
return expr
```
To:
```go
p.expect(TokenRParen)
return &ParenExpr{Expr: expr}
```

**Verify:** `ParenExpr` type already exists (`ast.go:219`) and implements `Expr` (`ast.go:223`).

### Step 3: Add `case *sql.ParenExpr` to missing type switches

**File:** `internal/exec/engine.go`

Add forwarding cases to these 4 functions:

**`walkExpr` (line 1489):**
```go
case *sql.ParenExpr:
    walkExpr(e.Expr, fn)
```

**`collectUsingColumns` (line 3514):**
```go
case *sql.ParenExpr:
    collectUsingColumns(e.Expr, cols)
```

**`collectExprRefs` (line 7610):**
```go
case *sql.ParenExpr:
    collectExprRefs(e.Expr, refs)
```

**`extractConst` (line 1517):**
```go
case *sql.ParenExpr:
    return extractConst(e.Expr)
```

**File:** `internal/exec/engine.go` — `evalComplexExpr` (line 7935):
Add for safety (though `evalExpr` already handles ParenExpr before reaching `evalComplexExpr`):
```go
case *sql.ParenExpr:
    return e.evalExpr(v.Expr, row)
```

### Step 4: Use `UnwrapParenExpr` at key evaluation entry points

**File:** `internal/exec/engine.go`

Add `expr = sql.UnwrapParenExpr(expr)` at the beginning of these functions (before the type switch), as a belt-and-suspenders approach:

- `evalExpr` (line 7897) — after the nil check, before the switch
- `exprToString` (line 1935) — after the nil check, before the switch
- `collectExprTriggerColRefs` (line 6466) — after the nil check, before the switch

**Note:** These functions already have `case *sql.ParenExpr` handlers, so `UnwrapParenExpr` is redundant here. However, it provides defense-in-depth: if a ParenExpr wraps another ParenExpr (e.g., `((expr))`), the recursive unwrap handles it in one step rather than relying on the case handler to recurse.

**Decision:** Use `UnwrapParenExpr` only at entry points that DON'T already have ParenExpr cases. For functions that already have the case, the recursive forwarding is sufficient. This avoids redundant code.

**Revised:** Skip Step 4. The existing `case *sql.ParenExpr` handlers in `evalExpr`, `exprToString`, `collectExprTriggerColRefs`, and `collectExprRange` (rename.go) are sufficient. The new cases added in Step 3 cover the remaining functions.

### Step 5: Fix `parseWindowClause` to parse and store inline window specs

**File:** `internal/sql/parser.go`

Replace the `skipInlineWindowSpec()` call in `parseWindowClause` (line 3928) with a new `parseInlineWindowSpec()` function that parses and stores the window spec.

**New function** (add near `skipInlineWindowSpec`, ~line 4003):

```go
// parseInlineWindowSpec parses an inline window specification
// (PARTITION BY ... ORDER BY ... frame_spec) inside parentheses
// and returns a populated *WindowDef. Unlike skipInlineWindowSpec,
// this function stores the parsed results rather than discarding them.
func (p *Parser) parseInlineWindowSpec() *WindowDef {
    wd := &WindowDef{}
    p.next() // skip (
    for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
        if p.cur.Type == TokenKeyword && p.cur.Value == "PARTITION" {
            p.next()
            if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
                p.next()
            }
            var partitions []Expr
            for p.cur.Type != TokenRParen && p.cur.Type != TokenEOF {
                if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
                    break
                }
                if p.cur.Type == TokenKeyword &&
                    (p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
                    break
                }
                expr := p.parseExpr()
                partitions = append(partitions, expr)
                if p.cur.Type == TokenComma {
                    p.next()
                } else {
                    break
                }
            }
            wd.Partitions = partitions
        } else if p.cur.Type == TokenKeyword && p.cur.Value == "ORDER" {
            p.next() // consume ORDER
            if p.cur.Type == TokenKeyword && p.cur.Value == "BY" {
                p.next() // consume BY
            }
            wd.OrderBy = p.parseOrderBy()
        } else if p.cur.Type == TokenKeyword &&
            (p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
            wd.FrameSpec = p.parseFrameSpecString()
        } else if p.cur.Type == TokenComma {
            p.next()
        } else {
            // Parse expressions (handles function calls, identifiers, etc.)
            p.parseExpr()
        }
    }
    if p.cur.Type == TokenRParen {
        p.next()
    }
    return wd
}
```

**Modify `parseWindowClause` (line 3926-3928):**

Change:
```go
} else if p.cur.Type == TokenLParen {
    // Inline window spec: skip tokens without storing
    p.skipInlineWindowSpec()
}
```
To:
```go
} else if p.cur.Type == TokenLParen {
    // Inline window spec: parse and store
    ov = p.parseInlineWindowSpec()
}
```

**Note on `parseFrameSpecString`:** This is a new helper that parses the frame spec and returns it as a string (since `WindowDef.FrameSpec` is a `string`). The existing `skipFrameSpec` (parser.go:4029) skips the frame spec without storing. We need a variant that captures it. The simplest approach: use the existing `skipFrameSpec` logic but capture the text between the frame keyword and the end of the frame spec.

**Alternative (simpler):** Since `WindowDef.FrameSpec` is only used for display purposes (not for execution — window functions are not fully implemented in frigolite), we can set it to a placeholder or use the existing skip logic. The critical fields are `Partitions` and `OrderBy`, which are used by `collectWindowDefTriggerColRefs` for column reference checking.

**Revised approach for frame spec:** Use the existing `skipFrameSpec()` for the frame spec (since it's not used for column ref checking), but parse and store `Partitions` and `OrderBy`:

```go
} else if p.cur.Type == TokenKeyword &&
    (p.cur.Value == "RANGE" || p.cur.Value == "ROWS" || p.cur.Value == "GROUPS") {
    p.skipFrameSpec()
}
```

This is sufficient because `collectWindowDefTriggerColRefs` (engine.go:6452) only walks `w.Partitions` and `w.OrderBy` — it does not use `w.FrameSpec`.

### Step 6: Remove the `normalizeSQL` workaround for ParenExpr

**File:** `frigolite_harness_test.go`, lines 365-370

Remove the regex that converts `(b IN ())` to `b IN ()`:
```go
// Remove these lines:
re = regexp.MustCompile(`(^|[^a-zA-Z0-9_])\((\w+)\s*IN\s*\(\)\)`)
normalized = re.ReplaceAllString(normalized, `${1}${2} IN()`)
```

**Rationale:** Once `parseParenExpr` returns `&ParenExpr{Expr: expr}`, the `exprToString` function (which already has `case *sql.ParenExpr: return "(" + exprToString(v.Expr) + ")"`) will correctly emit `(b IN ())`. The workaround is no longer needed.

**Caution:** Only remove this after verifying that all ParenExpr-related tests pass. The workaround may also be masking other issues.

### Step 7: Verify `checkTriggerColRefs` works with the fixes

**File:** `internal/exec/engine.go`, `checkTriggerColRefs` (line 6300)

After the fixes:
1. `parseParenExpr` returns `&ParenExpr{Expr: expr}` — `collectExprTriggerColRefs` already handles ParenExpr (line 6491)
2. `parseWindowClause` populates `WindowDef` with `Partitions` and `OrderBy` — `collectExprTriggerColRefs` already walks `FuncCall.Over` (line 6488-6489) via `collectWindowDefTriggerColRefs`

**No changes needed** to `checkTriggerColRefs` itself — it already has the correct traversal logic. The fixes in Steps 2 and 5 make the AST correct so that the existing traversal finds all column references.

### Step 8: Run tests and verify

```bash
cd /Users/muaddib/dev/frigolite

# 1. Compile check
go build ./...

# 2. Run parser-related tests
go test -v -count=1 -run 'TestParse|TestExpr|TestWindow|TestParen' . 2>&1 | tail -30

# 3. Run ALTER TABLE compat tests (should see improvement)
go test -v -count=1 -run '^TestSQLiteSuite/alter' . 2>&1 | grep -c "FAIL"

# 4. Run full compat test suite for regressions
go test -count=1 . 2>&1 | tail -5

# 5. Quality gates
make quality
go test -run TestSOLID_ ./...
```

## Files Modified

| File | Change |
|---|---|
| `internal/sql/expr_util.go` (NEW) | `UnwrapParenExpr` utility function |
| `internal/sql/parser.go` | Fix `parseParenExpr` (line 3427); add `parseInlineWindowSpec`; modify `parseWindowClause` |
| `internal/exec/engine.go` | Add `case *sql.ParenExpr` to `walkExpr`, `collectUsingColumns`, `collectExprRefs`, `extractConst`, `evalComplexExpr` |
| `frigolite_harness_test.go` | Remove `normalizeSQL` ParenExpr workaround (lines 365-370) |

## Risk Assessment

| Risk | Mitigation |
|---|---|
| ParenExpr fix causes regressions in walker functions | Step 3 adds cases to all 4 missing walkers; `evalExpr`/`exprToString`/`collectExprTriggerColRefs` already handle ParenExpr |
| Window spec fix changes behavior of existing window function tests | `parseInlineWindowSpec` produces the same `WindowDef` structure as `parseOneWindowDef`; existing tests that use `OVER (ORDER BY ...)` will now have populated `Over` field |
| `normalizeSQL` removal exposes other formatting mismatches | Remove only after all tests pass; keep as fallback if needed |
| `parseInlineWindowSpec` doesn't handle all window spec syntax | Reuse logic from `parseOneWindowDef` and `skipInlineWindowSpec` which already handle PARTITION BY, ORDER BY, and frame specs |

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite

# All ALTER TABLE suites — should show improvement
for suite in altertab3 alterlegacy altercons2 altertab2 alterdropcol altercons3 alterdropcol2 altermalloc2 altercorrupt alterauth; do
  echo "=== $suite ==="
  go test -v -count=1 -run "^TestSQLiteSuite/$suite" . 2>&1 | grep -c "FAIL"
done

# No regressions in full suite
go test -count=1 . 2>&1 | tail -5

# Quality
make quality
go test -run TestSOLID_ ./...
```

## Implementation Log

### Completed Changes

| Step | File | Change | Status |
|---|---|---|---|
| 1 | `internal/sql/expr_util.go` (NEW) | `UnwrapParenExpr` utility function | ✅ Done |
| 2 | `internal/sql/parser.go:3427` | `return &ParenExpr{Expr: expr}` (was `return expr`) | ✅ Done |
| 3 | `internal/exec/engine.go` | Added `case *sql.ParenExpr` to `walkExpr`, `extractConst`, `collectUsingColumns`, `collectExprRefs`, `evalComplexExpr` | ✅ Done |
| 4 | `internal/sql/parser.go:3928,4007` | Added `parseInlineWindowSpec()` function; modified `parseWindowClause` to use it | ✅ Done |
| 5 | `frigolite_harness_test.go` | Removed `normalizeSQL` ParenExpr workaround (lines 365-370) | ✅ Done |
| 6 | `internal/exec/engine.go:8305` | Fixed pre-existing `typesMatchForEquality` panic (int64 vs string type assertion) | ✅ Done |

### Verification Results

| Check | Result |
|---|---|
| `go build ./...` | ✅ Passes |
| `go vet ./...` | ✅ Clean |
| `go test -run TestSOLID_ ./...` | ✅ Passes |
| Hand-written tests (all) | ✅ All pass |
| altertab3 failures | 18 with fix vs 19 without (1 test fixed, 0 new regressions) |
| altertab2 failures | 8 with fix vs 8 without (0 new regressions) |
| Pre-existing panic (in4-4.15) | Fixed by `typesMatchForEquality` fix (pre-existing bug, not caused by ParenExpr change) |
| Pre-existing hang (where4-3.1) | Pre-existing parser hang, unrelated to changes |

### Notes

- The `typesMatchForEquality` fix (Step 6) was necessary because the pre-existing panic at in4-4.15 prevented the test suite from running to completion. This bug was in the pre-existing uncommitted changes, not introduced by this plan.
- The `make quality` failure is due to pre-existing unused functions in `internal/exec/rename.go` (P3 ALTER TABLE work in progress), not related to these changes.
- The ParenExpr fix actually reduces altertab3 failures by 1, confirming it improves rather than regresses ALTER TABLE behavior.
