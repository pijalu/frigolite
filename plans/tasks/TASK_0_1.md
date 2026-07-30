# Task 0.1 — Refactor tcl2go to TCL Transpiler

> **Phase**: 0 — TCL to Go Test Pipeline
> **Status**: ✅ Complete
> **Files**: `tools/tcl2go/main.go`, `tools/tcl2go/gen.go`, `tools/tclconvert/tcl/parser.go`, `tools/tclconvert/tcl/interp.go`
> **Estimated**: 15 min

## Description

Rewrite tcl2go from a TCL interpreter-approach to a proper TCL transpiler.
Previously, tcl2go created a TCL interpreter, executed TCL files at generation
time (running loops, variables, expressions), and captured flat SQL statements.
Now it parses TCL commands and emits Go code directly — no TCL execution at
generation time. All control flow becomes native Go control flow.

## Changes

- **`tools/tcl2go/gen.go`**: Replaced flat Stmt-based generator with full
  transpiler that walks parsed TCL commands and emits Go code. Handles
  `foreach`, `for`, `while`, `if`, `set`, `incr`, `expr`, `execsql`,
  `catchsql`, `db eval`, `do_execsql_test`, `do_catchsql_test`, `do_test`,
  `do_eqp_test`, `reset_db`, and TCL string/command substitution.
- **`tools/tcl2go/main.go`**: Removed TCL interpreter dependency. Calls
  transpiler directly with TCL source text.
- **`tools/tclconvert/tcl/parser.go`**: Exported `ParseCommands()` and `RawWord`
  type for transpiler use. Internal types preserved via type alias.
- **`tools/tclconvert/tcl/interp.go`**: Updated field accesses to use exported
  names (`Text`, `Braced`, `Quoted`).

## Verification

```bash
go build ./tools/tcl2go/...
# Also verify old tools still compile
go build ./tools/tclconvert/...
```

## Session notes

- Started: 2024-07-30
- Completed: 2024-07-30
- Findings: Transpiler generates all 1002+ files in ~0.5s (>200x speedup vs
  old interpreter approach which timed out at 120s+). Interpreter code kept
  for backward compatibility with old `tclconvert` JSON converter tool.
