# P0 Plan: Go TCL Interpreter + Converter for SQLite Test Data

> **Objective**: Replace the Python regex converter with a Go-based mini TCL
> interpreter that actually *executes* the TCL test setup code (loops,
> variables, expressions) to capture the real SQL statements with all
> variables substituted. This fixes 411 test files that are missing CREATE
> TABLE / INSERT setup because the old converter can't handle `db eval`,
> `for` loops, and `expr` expressions.

---

## Architecture Overview

```
ori/sqlite/test/*.test          (TCL source files — input)
        │
        ▼
┌──────────────────────────┐
│ tools/tclconvert/         │
│ ├── tcl/                  │
│ │   ├── parser.go         │ ← TCL tokenizer + word parser
│ │   ├── interp.go         │ ← TCL interpreter (command executor)
│ │   └── expr.go           │ ← Expression evaluator (arithmetic, logic)
│ │
│ └── main.go               │ ← Converter: .test → .json
│      For each .test file: │
│      1. Parse TCL         │
│      2. Execute TCL       │
│      3. Capture SQL stmts │
│      4. Write JSON        │
└──────────────────────────┘
        │
        ▼
testdata/*.json                (JSON test data — same format as now)
        │
        ▼
frigolite_harness_test.go     (test harness — UNCHANGED)
```

**Key principle**: The JSON output format stays identical. Only the content
improves (more SQL statements captured). The test harness needs NO changes.

---

## TCL Subset to Support

Based on analysis of all 1192 TCL test files, here are the constructs ranked
by usage frequency:

### Must-Have (cover 95%+ of test files)

| TCL Construct | Example | Why Needed |
|---------------|---------|------------|
| `db eval { SQL }` | `db eval {CREATE TABLE t1(a,b)}` | #1 way to run SQL in setup |
| `execsql { SQL }` | `execsql {SELECT * FROM t1}` | #2 way to run SQL |
| `catchsql { SQL }` | `catchsql {DROP TABLE t1}` | SQL expecting error |
| `do_execsql_test NAME { SQL } { EXPECT }` | — | Named test with result check |
| `do_catchsql_test NAME { SQL } { EXPECT }` | — | Named test expecting error |
| `do_test NAME { BODY } { EXPECT }` | — | General test (body may contain SQL) |
| `set VAR VALUE` | `set x 5` | Variable assignment |
| `expr { EXPR }` | `expr {$i * 10}` | Arithmetic/logical expressions |
| `for {INIT} {COND} {NEXT} {BODY}` | `for {set i 0} {$i<100} {incr i} {...}` | Data generation loops |
| `incr VAR [AMT]` | `incr i` or `incr x 2` | Loop increment |
| `$VAR` substitution | `INSERT INTO t1 VALUES($i)` | Variable in SQL string |
| `[CMD]` substitution | `set x [expr {1+2}]` | Command result in variable |
| `if {COND} {BODY} {ELSE}` | `if {$i%2==0} {set a $i} {set a 0}` | Conditionals |
| `proc NAME {ARGS} {BODY}` | `proc eqp {sql} {...}` | Procedure definition |
| `uplevel` | `uplevel execsql [list $sql]` | Call in caller scope |
| Comments `#` | `# This is a comment` | Skip lines |

### Important (cover remaining 5%)

| TCL Construct | Example | Notes |
|---------------|---------|-------|
| `foreach VAR LIST BODY` | `foreach x {1 2 3} {...}` | Iteration |
| `list ARGS...` | `list 1 2 3` | Create list |
| `lappend VAR ITEMS...` | `lappend result $val` | Accumulate results |
| `llength LIST` | — | List length |
| `lindex LIST IDX` | — | List element access |
| `catch {BODY} VAR` | `catch {execsql {...}} msg` | Error catching |
| `upvar` | — | Variable aliasing |
| `while {COND} {BODY}` | — | While loop |
| `string OP STR` | `string length $s` | String operations |
| `lrange LIST S E` | — | List slicing |

### Skip / No-Op (infrastructure, not needed for SQL capture)

| Construct | Action |
|-----------|--------|
| `source`, `sqlite3`, `finish_test` | Skip |
| `puts`, `output1`, `output2` | Skip |
| `ifcapable`, `ifnotcapable` | Skip body (we don't know capabilities) |
| `db close`, `db cache`, `db status` | No-op |
| `fix_testname`, `incr_ntest` | No-op |
| `sqlite3_memdebug_settitle` | No-op |

---

## Component Specification

### 1. `tcl/parser.go` — TCL Tokenizer and Word Parser

**Purpose**: Parse TCL source text into commands, where each command is a
list of words. Handle TCL quoting rules.

**TCL word types**:
- **Plain word**: `hello` — literal text, no quoting
- **Brace word**: `{ ... }` — literal, NO substitution, nested braces balanced
- **Quote word**: `" ... "` — substitution performed ($var, [cmd])
- **Variable ref**: `$var` or `${var}` — substituted inline
- **Command sub**: `[cmd args]` — result substituted inline

**Parsing algorithm**:
```
1. Split source into commands by:
   - Newlines (unless inside {} or [] or after \)
   - Semicolons (same rules)
2. For each command, split into words by whitespace
3. For each word, determine type:
   - Starts with { → brace word (read until matching })
   - Starts with " → quote word (read until matching ")
   - Contains $ or [ → needs substitution (mark for eval)
   - Otherwise → plain word
4. Return list of (text, braced bool) pairs per command
```

**Output type**:
```go
type rawWord struct {
    text   string // the raw word content
    braced bool   // true if { ... } quoted (no substitution needed)
    quoted bool   // true if " ... " quoted (substitution applied)
}
```

**Function signature**:
```go
func parseCommands(src string) [][]rawWord
```

### 2. `tcl/interp.go` — TCL Interpreter (command executor)

**Purpose**: Execute parsed commands, maintain variable scope, and capture
SQL statements.

**Core state**:
```go
type Interp struct {
    vars    map[string]string         // global variables
    procs   map[string]*Proc          // user-defined procedures
    stmts   []Stmt                    // captured SQL statements
    curTest string                    // current test name
    depth   int                       // call stack depth guard
}
```

**Word evaluation** (`evalWord`):
```go
func (i *Interp) evalWord(rw rawWord, localVars map[string]string) (string, error)
```
- If `rw.braced` → return text as-is (no substitution)
- Otherwise → perform `$var` and `[cmd]` substitution

**Variable substitution** (`substitute`):
- Replace `$varname` with variable value
- Replace `${varname}` with variable value (braced name)
- Replace `$varname(arr)` with array element (simplified)
- Leave unknown `$var` as-is (empty string in TCL)

**Command substitution**:
- Parse `[cmd args...]` and execute it
- Replace with result string

**Built-in commands** (see table above for full list):
- `set`, `unset`, `incr`, `expr`
- `if`, `for`, `foreach`, `while`
- `proc`, `return`, `break`, `continue`
- `catch`, `uplevel`, `upvar`, `global`
- `list`, `lappend`, `llength`, `lindex`, `lrange`, `concat`
- `string`, `regexp`, `regsub`
- `db eval`, `execsql`, `catchsql`
- `do_execsql_test`, `do_catchsql_test`, `do_test`, `do_eqp_test`
- Skip: `source`, `puts`, `finish_test`, `ifcapable`, etc.

### 3. `tcl/expr.go` — Expression Evaluator

**Purpose**: Evaluate TCL expressions used in `expr`, `if` conditions, `for`
conditions.

**Supported operators** (precedence high→low):
1. Unary: `-`, `+`, `!`, `~`
2. Multiplicative: `*`, `/`, `%`
3. Additive: `+`, `-`
4. Shift: `<<`, `>>`
5. Comparison: `<`, `>`, `<=`, `>=`
6. Equality: `==`, `!=`
7. Bitwise: `&`, `^`, `|`
8. Logical: `&&`, `||`
9. Ternary: `cond ? true : false`
10. String: `eq`, `ne`

**Built-in functions**: `abs`, `double`, `int`, `round`, `floor`, `ceil`,
`sqrt`, `pow`, `log`, `exp`, `min`, `max`, `rand`, `srand`, `hypot`, `wide`

**Implementation**: Recursive descent parser:
```
parseExpr  → parseTernary
parseTernary → parseLogicalOr [? parseExpr : parseExpr]
parseLogicalOr → parseLogicalAnd {|| parseLogicalAnd}
parseLogicalAnd → parseBitOr {&& parseBitOr}
parseBitOr → parseBitXor {| parseBitXor}
parseBitXor → parseBitAnd {^ parseBitAnd}
parseBitAnd → parseEquality {& parseEquality}
parseEquality → parseComparison {(==|!=|eq|ne) parseComparison}
parseComparison → parseShift {(<|>|<=|>=) parseShift}
parseShift → parseAdditive {(<<|>>) parseAdditive}
parseAdditive → parseMultiplicative {(+|-) parseMultiplicative}
parseMultiplicative → parseUnary {(*|/|%) parseUnary}
parseUnary → (-|!|~) parseUnary | parsePrimary
parsePrimary → NUMBER | STRING | (EXPR) | FUNC(ARGS)
```

### 4. `tools/tclconvert/main.go` — Converter

**Purpose**: Read all `.test` files, run them through the TCL interpreter,
and write JSON output.

**Algorithm**:
```go
func main() {
    testDir := "ori/sqlite/test"
    outDir := "testdata"
    
    for each *.test file:
        1. Read the TCL source
        2. Create a new Interp
        3. Execute the source
        4. Group captured Stmts by TestName
        5. Convert to TestCase JSON structure:
           {
             "file": "analyze8",
             "tests": [
               {
                 "name": "1.0",
                 "steps": [
                   {"type": "exec", "sql": "CREATE TABLE t1(...)"},
                   {"type": "exec", "sql": "INSERT INTO t1 VALUES(0,0,0,0)"},
                   {"type": "exec", "sql": "INSERT INTO t1 VALUES(100,0,0,1)"},
                   ...
                   {"type": "exec", "sql": "ANALYZE"}
                 ]
               },
               {
                 "name": "3.0",
                 "steps": [
                   {"type": "query", "sql": "SELECT ...", "expect": "50 376 32"}
                 ]
               }
             ]
           }
        6. Write JSON to testdata/FILENAME.json
}
```

**Key decisions**:
- Statements without a TestName go into the nearest preceding test or a
  `setup_N` group (matching current behavior)
- `execsql` → type "exec" or "query" (based on last SQL statement)
- `catchsql` → type "exec" with `expect` containing the error code
- `do_execsql_test` → named test, type "query"/"exec", with `expect`
- `do_catchsql_test` → named test, type "exec", with `expect`
- `do_test` → named test, body executed, SQL captured, `expect` attached
- If a `do_test` body generates multiple SQL stmts, they all go in the test
- `for` loops with 1000 iterations → 1000 INSERT steps (correct but large JSON)

---

## Execution Steps

### Step 1: Complete the TCL parser (`tcl/parser.go`)
- [ ] Implement `parseCommands(src string) [][]rawWord`
- [ ] Handle: brace words `{...}`, quote words `"..."`, plain words
- [ ] Handle: newlines, semicolons, line continuation `\`
- [ ] Handle: comments `#` at start of command
- [ ] Handle: nested braces, brackets
- [ ] **Verify**: parse `analyze8.test` and `select1.test` successfully
- [ ] **Commit**: `P0.1: TCL parser - tokenizer and word parser`

### Step 2: Complete the TCL interpreter (`tcl/interp.go`)
- [ ] Fix/complete `interp.go` (already has most command handlers)
- [ ] Implement `evalWord` (word evaluation with substitution)
- [ ] Implement `substitute` ($var and [cmd] substitution)
- [ ] Implement `evalAllWords` (substitute all words in a command)
- [ ] Add list helpers: `splitList`, `tclList`, `tclLLength`, `tclLIndex`, `tclLRange`
- [ ] **Verify**: Execute `analyze8.test` — should capture CREATE TABLE + 1000 INSERTs + ANALYZE
- [ ] **Commit**: `P0.2: TCL interpreter - command executor and substitution`

### Step 3: Complete the expression evaluator (`tcl/expr.go`)
- [ ] Implement recursive descent parser for TCL expressions
- [ ] Support all operators and built-in functions
- [ ] **Verify**: Evaluate `$i/10`, `$i%8`, `$c*$c*$c`, `int(log($i)/log(2))`
- [ ] **Commit**: `P0.3: TCL expression evaluator`

### Step 4: Build the converter (`tools/tclconvert/main.go`)
- [ ] Read `.test` files from `ori/sqlite/test/`
- [ ] Execute each with the interpreter
- [ ] Group captured statements into TestCase JSON
- [ ] Write JSON to `testdata/`
- [ ] **Verify**: Generate `testdata/analyze8.json` with CREATE TABLE present
- [ ] **Commit**: `P0.4: Go TCL converter - main entry point`

### Step 5: Regenerate all test data
- [ ] Run the converter on all 1002+ test files
- [ ] **Verify**: `python3 -c "..."` — count files with CREATE TABLE (was 591, target 1000+)
- [ ] **Commit**: `P0.5: regenerate all test data with TCL interpreter`

### Step 6: Run full test suite and measure
- [ ] Run: `go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v .`
- [ ] Record PASS count. Target: **≥300/869**
- [ ] Check for regressions (files that passed before but fail now)
- [ ] **Commit**: `P0.6: measurement + regression fixes`

### Step 7: Fix converter edge cases
- [ ] Handle multi-connection tests (`sqlite3 db2 file.db`)
- [ ] Handle `db eval { SQL } SCRIPT` (per-row callback — capture SQL only)
- [ ] Handle TCL `subst` command
- [ ] Handle `do_test` with complex bodies (nested execsql, variables)
- [ ] **Verify**: Run suite again, fix any regressions
- [ ] **Commit**: `P0.7: converter edge cases`

---

## Expected Impact

| Metric | Before P0 | After P0 (expected) |
|--------|-----------|---------------------|
| Files with CREATE TABLE | 591/1002 | 950+/1002 |
| "no such table" errors | 2644 | <200 |
| File-level PASS | 215/869 | 300+/869 |

The main impact comes from test files like `analyze8`, `where`, `select1`,
etc. that have setup code in TCL `for` loops using `db eval`. After P0,
these files will have complete setup SQL and the queries will find the tables.

---

## Risk Mitigation

1. **Large JSON files**: `for` loops with 1000 iterations produce 1000 INSERT
   statements. This increases JSON file size but is correct. If needed, can
   batch into multi-row INSERTs later.

2. **TCL features we can't handle**: Some test files use complex TCL (custom
   procs, upvar chains, `array` operations). These may not convert correctly.
   The interpreter silently skips unknown commands, so SQL still gets captured
   partially.

3. **Variable resolution failures**: If a TCL variable can't be resolved (set
   by external code or complex logic), the SQL will contain literal `$var`
   which will cause a parse error. We handle this by leaving unresolvable
   variables as-is.

4. **Regressions**: The regenerated JSON may differ from the old JSON in ways
   that break currently-passing tests. Mitigation: compare old vs new JSON for
   currently-passing files and investigate differences.

---

## Current State

- `tools/tclconvert/tcl/interp.go` — **PARTIALLY WRITTEN** (1034 lines, has
  command handlers but references unimplemented parser/expr/substitution functions)
- `tools/tclconvert/tcl/parser.go` — **NOT WRITTEN**
- `tools/tclconvert/tcl/expr.go` — **NOT WRITTEN**
- `tools/tclconvert/main.go` — **NOT WRITTEN**

The interpreter in `interp.go` has all command handlers but depends on:
- `parseCommands()` → from parser.go
- `rawWord` type → from parser.go
- `evalWord()` → needs substitution logic
- `substitute()` → needs substitution logic
- `evalAllWords()` → needs substitution logic
- `splitList()`, `tclList()`, `tclLLength()`, etc. → list helpers
- `exprParser` → from expr.go
- `EvalExpr()` → from expr.go
