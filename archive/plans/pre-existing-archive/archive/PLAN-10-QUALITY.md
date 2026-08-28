# PLAN-P10 — Quality Gates & Final Verification

> **⚠️ DEPRECATED APPROACH**: This plan references the old Python converter (`convert_compat_json.py`) and JSON harness (`testdata/*.json`). The project now uses the **tcl2go** pipeline (Go TCL interpreter → Go test files). See [`PLAN.md`](./PLAN.md) for the current strategy.
>
> **Prerequisite**: All prior phases (P0–P9).
> **Goal**: Ensure quality gates pass, SOLID architecture is maintained, and the
> full test suite is green.

## Scope

1. `gocognit` — 24 functions with cognitive complexity > 30.
2. SOLID architecture checks.
3. `go vet`, `staticcheck`.
4. Full test suite — zero FAIL.
5. Documentation cleanup.

## Implementation Steps

### Step 1: Fix gocognit violations

**Current state:** 24 functions over complexity 30 (pre-existing tech debt).

**Run:**
```bash
cd /Users/muaddib/dev/frigolite
gocognit -over 30 . 2>&1 | sort -rn | head -30
```

**Strategy:** For each function over 30:
1. Identify the complexity source (deeply nested loops, many branches).
2. Extract helper functions.
3. Simplify conditional logic.
4. Re-run `gocognit` to verify the function is under 30.

**Key functions likely to be complex** (based on prior knowledge):
- `internal/exec/engine.go` — SELECT execution (nested loops for JOINs).
- `internal/exec/engine.go` — expression evaluation.
- `internal/sql/parser.go` — SELECT parsing.
- `frigolite_harness_test.go` — `cleanExpected` (nested brace matching).

**Approach:** Extract sub-functions, use early returns, simplify boolean logic.
Do NOT change behaviour — this is pure refactoring (Mode: refactor).

**Verify:**
```bash
gocognit -over 30 . 2>&1 | wc -l | xargs test 0 -eq
```

### Step 2: Run staticcheck

```bash
cd /Users/muaddib/dev/frigolite
staticcheck ./... 2>&1
# Should be 0 issues
```

If issues appear (from new code added in P1–P9):
- Fix each issue.
- Use `//lint:ignore` only for intentionally unused planned-feature code.

### Step 3: Run go vet

```bash
go vet ./...
```

### Step 4: Run SOLID tests

```bash
go test -v -run TestSOLID_ ./...
```

**If new packages were added** (internal/fts, internal/vtab/amatch, etc.):
1. Add them to `internalLayers` in `frigolite_solid_test.go`.
2. Verify import boundaries:
   - `fts` can import `vtab` but not `exec`.
   - `amatch` can import `vtab` but not `exec`.
   - No circular imports.

### Step 5: Full test suite

```bash
cd /Users/muaddib/dev/frigolite

# Run everything
go test -count=1 -v ./... 2>&1 | tee /tmp/frigolite_final.log

# Count FAILs
grep -c "^    --- FAIL" /tmp/frigolite_final.log
# Must be 0

# Check no panics
grep -c "panic:" /tmp/frigolite_final.log
# Must be 0
```

### Step 6: Regenerate test data (final)

If infrastructure was updated in P0, regenerate all tests:
```bash
cd /Users/muaddib/dev/frigolite
go run ./tools/tcl2go/              # Generate all Go test files from TCL
python3 tools/oracle_generate.py    # If oracle generation is needed
```

Then run the full suite again to confirm:
```bash
go test ./testgen/... -count=1
```

### Step 7: Documentation cleanup

- Ensure all exported symbols have GoDoc comments.
- Remove unused imports.
- Remove commented-out code.
- Remove `_ =` patterns (use `var _ =` or remove).
- Update AGENTS.md if architecture changed (new packages, etc.).

### Step 8: Final `make quality`

```bash
make quality
```

This runs:
- `go vet`
- `staticcheck`
- `gocognit`
- `gocyclo`
- `gofmt`

All must pass.

## Completion Check (THE FINAL GATE)

```bash
cd /Users/muaddib/dev/frigolite

echo "=== 1. Full test suite ==="
go test -count=1 ./... 2>&1 | tee /tmp/final.log
! grep -q "FAIL" /tmp/final.log && echo "PASS" || echo "FAIL"

echo "=== 2. Quality gates ==="
make quality && echo "PASS" || echo "FAIL"

echo "=== 3. SOLID ==="
go test -run TestSOLID_ ./... && echo "PASS" || echo "FAIL"

echo "=== 4. Sub-test FAIL count ==="
go test -v -count=1 . 2>&1 | grep -c "^    --- FAIL"
# Must be 0

echo "=== 5. gocognit ==="
gocognit -over 30 . 2>&1 | wc -l
# Must be 0
```

All five checks must pass.
