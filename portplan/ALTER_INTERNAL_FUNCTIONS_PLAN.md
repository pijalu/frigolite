# alter.test internal rename SQL functions and test control

## Current evidence and ordered plan

- `../sqlite/test/alter.test:687-700` toggles `SQLITE_TESTCTRL_INTERNAL_FUNCTIONS` around alter-9.1/9.2 and disables before 9.3; helper in `../sqlite/src/test1.c:8002-8010` calls the connection's test control.
- Visibility infrastructure is already present: `internal/function/function.go` marks internal functions and `ToggleInternalFunctions` gates `Find`; `frigolite_harness_test.go` consumes ordered `internal_functions` markers. `sqlite_rename_table` is registered with `RegisterInternal` in `internal/exec/engine_register.go`. Do not duplicate or weaken this behavior.
- Exact suite reproduced `alter-9.1` failure: `no such function: SQLITE_RENAME_COLUMN` for all-zero args with internal functions enabled. There is no existing implementation/registration. `../sqlite/src/alter.c:1484-1659` shows real behavior: resolve table and indexed column, parse stored SQL, walk table/index/view/trigger expressions and FK/generated-column references, emit rewritten SQL; invalid/missing table returns SQL NULL. `internal/rename` token ranges help but do not by themselves cover CREATE TABLE declaration and all SQLite AST rewrite cases.
- Trigfunc callback is represented by ordered converter marker, harness supports `{args}` and `set ::TRIGGER $args`; native callback regression now verifies values containing spaces are serialized as a Tcl list. Converter marker regression separately validates variadic metadata.

### Next implementation steps
1. Trace Engine registry closure/schema lookup and parser token positions; document the per-object input/output contract against `alter.c` before coding.
2. Implement full `sqlite_rename_column` semantics using Engine schema metadata plus parsed SQL rewrite support (no all-inputs-null stub); register as internal and cover invalid-input SQL NULL plus actual table/index/trigger rewrite cases natively.
3. Run alter-9.1–9.3 and exact alter suite; preserve existing alter-6 runtime assignment and alter-11 `sqlite3_exec` checks.
4. Rerun converter/native tests and `git diff --check`; do not modify unsupported/skip lists.
