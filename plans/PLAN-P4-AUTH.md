# PLAN-P4-AUTH.md — Authorization Callback Implementation

## Scope
Implement a Go-style authorization hook that allows the database engine to check and control access to database operations.

## Current Failures (5)
| Suite | Failures | Primary Issue |
|-------|----------|--------------|
| alterauth | 5 | ALTER TABLE operations trigger authorization callbacks that don't exist |

## Current State
No authorization mechanism exists.

## SQLite Authorization Model
SQLite's `sqlite3_set_authorizer()` registers a callback invoked before each SQL operation. The callback receives:
- **Action code** (e.g., SQLITE_ALTER_TABLE, SQLITE_INSERT, SQLITE_CREATE_INDEX)
- **Arguments** (varies by action: table name, column name, schema name, etc.)
- Returns: SQLITE_OK (allow), SQLITE_DENY (deny with error), SQLITE_IGNORE (treat as NULL)

### Action Codes Used in Tests
- SQLITE_ALTER_TABLE (main, table_name, {rename_action}, {})
- SQLITE_READ (table, column)
- SQLITE_INSERT (table, {})
- SQLITE_UPDATE (table, column)
- SQLITE_CREATE_TABLE (table_name, {})

## Implementation Approach (Go-style)

### Step 1: Define Go interface
Create `internal/auth/authorizer.go`:
```go
package auth

// Action represents a database operation being authorized.
type Action int

const (
    ActionCreateTable Action = iota
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
    // ... more as needed
)

// Result of authorization check.
type Result int

const (
    ResultOK     Result = iota // allow
    ResultDeny                 // deny with error
    ResultIgnore               // treat as NULL
)

// Authorizer is the interface for authorization callbacks.
type Authorizer interface {
    Authorize(action Action, arg1, arg2, arg3, arg4 string) Result
}
```

### Step 2: Integrate with Engine
1. Add `Authorizer` field to `Engine` struct
2. Add `SetAuthorizer(Authorizer)` method
3. Call `e.authorizer.Authorize(...)` before each operation:
   - INSERT/UPDATE/DELETE → call with appropriate action
   - CREATE/DROP TABLE/INDEX/VIEW/TRIGGER → call with appropriate action
   - ALTER TABLE → call with ActionAlterTable and action details
   - ATTACH/DETACH → call with appropriate action
   - SELECT/READ → call for each column read

### Step 3: Test behavior
1. When Authorizer is nil (default), all operations allowed (current behavior)
2. When Authorizer returns ResultDeny, operation fails with "not authorized" error
3. When Authorizer returns ResultIgnore, operation proceeds with NULL semantics

### Step 4: Handle the specific test expectations
The alterauth tests expect specific authorization codes to be generated:
- `SQLITE_ALTER_TABLE main t1 {} {}` → ALTER TABLE on t1 in main schema
- `1 {not authorized}` → authorization denied

**Fix:** Since the tests expect authorization codes to be generated (even if not denied), we need to:
1. When no authorizer is set, a default authorizer should return ResultOK for everything
2. The test harness may need to be adjusted to handle the authorization code format in exec results

### Step 5: Fix the test data
The JSON test data for alterauth contains expected values like:
```
"{SQLITE_ALTER_TABLE main t1 {} {}}"
```
This is the authorization CODE that SQLite generates. In Frigolite:
- Process: generate the authorization code → call authorizer → if denied, return error
- For tests that just expect the code without denial: the authorizer returns OK and the operation proceeds

**Issue:** The test expects the authorization code AS THE RESULT of the exec call. In SQLite, `catchsql` captures the auth code if the authorizer is set. In Frigolite, we need to decide:
1. Always generate auth codes and return them? 
2. Or only generate them when an authorizer is set?

**Approach:** Match SQLite behavior — auth codes are only generated when an authorizer is registered. Tests that expect auth codes must register an authorizer first. But the tests also expect the auth codes to appear in the result.

Since we can't modify the test data, we need to make the engine generate auth codes by default (when authorization is not explicitly set) AND allow them to be suppressed when no authorizer is set.

**Alternative approach:** Add a default authorizer that allows everything. The auth codes are generated as part of the internal authorization flow and returned as part of the exec result metadata.

## Verification
```bash
go test -v -run "TestSQLiteSuite/alterauth" . 2>&1 | grep -E "PASS|FAIL"
```

## Completion Check
```bash
go test -v -run "TestSQLiteSuite/alterauth" . 2>&1 | grep -c "FAIL" | xargs test 0 -eq
```

## Key Files
- `internal/auth/authorizer.go` — NEW: authorization interface and default implementation
- `internal/exec/engine.go` — authorization hook points
