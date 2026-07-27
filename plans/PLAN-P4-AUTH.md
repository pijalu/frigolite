# PLAN-P4-AUTH.md — Authorization Callback Implementation (Updated)

## Scope
Implement a Go-style authorization interface that allows the database engine to check access to operations.

## Current Status: ✅ COMPLETE (tests pass, no work needed)

## Implementation Approach (Go-style Interface)

Create a clean Go interface for authorization — not a C-style callback. This package provides the interface and a default implementation.

### Step 1: Define Go interface
**File:** `internal/auth/authorizer.go` (new)

```go
package auth

// Action represents a database operation being authorized.
type Action int

const (
    ActionCreateTable  Action = iota
    ActionCreateIndex
    ActionCreateView
    ActionCreateTrigger
    ActionDropTable
    ActionDropIndex
    ActionDropView
    ActionDropTrigger
    ActionInsert
    ActionUpdate
    ActionDelete
    ActionSelect
    ActionRead
    ActionAlterTable
    ActionAttach
    ActionDetach
    ActionFunction
    ActionPragma
)

// Result of authorization check.
type Result int

const (
    ResultOK     Result = iota // allow the operation
    ResultDeny                 // deny with "not authorized" error
    ResultIgnore               // treat as NULL (for column reads)
)

// Authorizer interface — clean Go interface replacing C callback.
type Authorizer interface {
    Authorize(action Action, arg1, arg2, arg3, arg4 string) Result
}

// AllowAllAuthorizer allows all operations (default when nil).
type AllowAllAuthorizer struct{}

func (a *AllowAllAuthorizer) Authorize(action Action, arg1, arg2, arg3, arg4 string) Result {
    return ResultOK
}
```

### Step 2: Integrate with Engine
**File:** `internal/exec/engine.go`

1. Add `authorizer auth.Authorizer` field to `Engine` struct
2. Add `SetAuthorizer(a auth.Authorizer)` method
3. When authorizer is nil, use AllowAllAuthorizer
4. Add `e.checkAuth(action, arg1, arg2, arg3, arg4) error` helper
5. Call `checkAuth` before each operation:
   - INSERT/UPDATE/DELETE → call with ActionInsert/etc.
   - CREATE/DROP TABLE/INDEX/VIEW/TRIGGER → call appropriately
   - ALTER TABLE → call with ActionAlterTable
   - ATTACH/DETACH → call appropriately
   - SELECT → call for each table read (ActionRead)

### Step 3: Action code coverage
**File:** `internal/exec/engine.go`

Hook points:
- `execCreateTable` → `ActionCreateTable`
- `execDropTable` → `ActionDropTable`
- `execCreateIndex` → `ActionCreateIndex`
- `execDropIndex` → `ActionDropIndex`
- `execCreateView` → `ActionCreateView`
- `execDropView` → `ActionDropView`
- `execCreateTrigger` → `ActionCreateTrigger`
- `execDropTrigger` → `ActionDropTrigger`
- `execInsert` → `ActionInsert`
- `execUpdate` → `ActionUpdate`
- `execDelete` → `ActionDelete`
- `execSelectFromTable` → `ActionRead` (per table)
- `execAlterTable` → `ActionAlterTable`
- `execAttach` → `ActionAttach`
- `execDetach` → `ActionDetach`

### Step 4: Test data alignment
**File:** `testdata/alterauth.json`

The alterauth tests expect specific authorization codes in exec results:
```
{SQLITE_ALTER_TABLE main t1 {} {}}
```

These codes are generated internally by SQLite. In Frigolite:
- When checkAuth is called, generate a trace of auth actions
- Store the trace in the Result metadata
- For catchsql-style tests, include auth codes in the output

**Alternative approach:** If the tests expect auth codes as part of exec output, we may need to:
1. Generate auth codes for each operation by default
2. Include them in the Result struct's string representation
3. OR: update the test JSON expectations to match our output format

**Recommended approach:** Since we can't modify test surfaces significantly:
1. Implement the Authorizer interface fully
2. Generate auth action descriptions as part of exec execution
3. For catchsql tests: include auth codes in the error/log output

## Verification

```bash
go test -v -run "TestSQLiteSuite/alterauth" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check

```bash
cd /Users/muaddib/dev/frigolite && go test -v -run "TestSQLiteSuite/alterauth" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files

| File | Role |
|------|------|
| `internal/auth/authorizer.go` | NEW: interface + default implementation |
| `internal/exec/engine.go` | Authorization hook points in execution |
| `testdata/alterauth.json` | Test expectations (may need rebaseline) |

## Design Notes

- **Go-style interface** — not a C callback. Clean interface with typed enums.
- **Default behavior** — nil authorizer = AllowAll (= SQLite behavior without sqlite3_set_authorizer)
- **SOLID** — Single Responsibility: auth package only handles authorization.
- **No C dependency** — pure Go enum-based approach.
