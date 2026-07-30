# P0 Plan: Go TCL Interpreter + Converter for SQLite Test Data

> **⚠️ SUPERSEDED**: This plan describes the JSON-based converter approach (`tools/tclconvert/` → `testdata/*.json`). The project has since adopted the **tcl2go** approach (Go TCL interpreter → Go test files in `testgen/`). See [`P0_TCL2GO.md`](./P0_TCL2GO.md) and [`PLAN.md`](./PLAN.md) for the current strategy.
>
> The TCL interpreter infrastructure (`tools/tclconvert/tcl/`) built here is shared by tcl2go.
>
> **Objective**: Build a Go-based mini TCL interpreter that executes TCL test
> setup code (loops, variables, expressions) to capture real SQL statements.
> This fixes 411 test files missing CREATE TABLE/INSERT setup.
>
> **After completing all micro-tasks below, run**: `go run ./tools/tclconvert/`
> to regenerate all 1002 JSON files, then verify ≥300/869 file-level PASS.
>
> **Current state**: `interp.go` is partially written (command handlers exist
> but reference unimplemented functions). Tasks MT-1 through MT-4 implement
> the missing pieces. Each task is self-contained.

---

## Architecture

```
ori/sqlite/test/*.test  →  [Go TCL Interpreter]  →  testdata/*.json
                         tools/tclconvert/           (same JSON format as now)
```

**Components to implement**:

| File | Purpose | Status |
|------|---------|--------|
| `tools/tclconvert/tcl/parser.go` | TCL tokenizer, splits source into commands and words | MT-1 |
| `tools/tclconvert/tcl/interp.go` | TCL interpreter, executes commands, captures SQL | MT-2 (partial) |
| `tools/tclconvert/tcl/expr.go` | Expression evaluator (arithmetic, logic, comparison) | MT-3 |
| `tools/tclconvert/tcl/list.go` | TCL list helpers (splitList, tclList, lindex, etc.) | MT-4 |
| `tools/tclconvert/main.go` | Converter: .test → .json | MT-5 |

**The JSON output format stays identical** — the test harness needs no changes.

---

## MT-1: TCL Parser (`tools/tclconvert/tcl/parser.go`)

**Goal**: Parse TCL source text into commands (list of words).

### Types to define

```go
package tcl

// rawWord represents one word in a TCL command before substitution.
type rawWord struct {
    text   string // raw content (for braced words, this is literal; for
                  // others, it may contain $var or [cmd] needing substitution)
    braced bool   // true if word was { ... } quoted (literal, no substitution)
    quoted bool   // true if word was " ... " quoted (substitution applies)
}
```

### Function to implement

```go
// parseCommands splits TCL source into commands. Each command is a slice
// of rawWord. Commands are separated by newlines or semicolons (outside
// braces/brackets). Lines starting with # are comments (skipped).
func parseCommands(src string) [][]rawWord
```

### Parsing rules (with examples)

**Rule 1: Commands separated by newlines**
```
set x 5     ← command 1: [set, x, 5]
set y 10    ← command 2: [set, y, 10]
```

**Rule 2: Commands separated by semicolons (outside braces)**
```
set x 5; set y 10    ← two commands
```

**Rule 3: Brace words `{ ... }` — literal, nested braces balanced**
```
db eval { CREATE TABLE t1(a, b); }
  → words: [db, eval, {CREATE TABLE t1(a, b);}]
```
- Read from `{` to matching `}` (depth counter for nested `{` `}`)
- The text BETWEEN the outer braces is the word content
- `braced = true`, no substitution performed

**Rule 4: Quote words `" ... "` — substitution applies**
```
set msg "hello $name"
  → words: [set, msg, "hello $name"] (quoted=true)
```
- Read from `"` to matching `"` (no nesting)
- `quoted = true`, substitution IS performed

**Rule 5: Line continuation with backslash**
```
set x \
    5       ← single command: [set, x, 5]
```

**Rule 6: Comments `#` at start of a command**
```
# This is a comment    ← skipped entirely
set x 5                ← parsed normally
```
- Only `#` at the START of a command is a comment (not mid-line)

**Rule 7: Command substitution `[ ... ]` inside words**
```
set x [expr {1+2}]     ← words: [set, x, [expr {1+2}]]
```
- `[ ]` is handled at substitution time, not parse time
- But parser must track `[]` depth to avoid splitting commands inside `[...]`

### Algorithm

```
pos = 0
commands = []
currentWords = []
currentWord = ""
currentWordType = ""

for pos < len(src):
    ch = src[pos]

    if ch == '\n' or ch == ';':
        if currentWord != "" or currentWordType != "":
            currentWords.append(makeRawWord(currentWord, currentWordType))
        if len(currentWords) > 0:
            commands.append(currentWords)
        currentWords = []
        currentWord = ""
        currentWordType = ""
        pos++
        continue

    if ch == '#' and atStartOfCommand:
        # skip to end of line
        while pos < len(src) and src[pos] != '\n': pos++
        continue

    if ch == ' ' or ch == '\t':
        if currentWord != "" or currentWordType != "":
            currentWords.append(makeRawWord(currentWord, currentWordType))
        currentWord = ""
        currentWordType = ""
        pos++
        continue

    if ch == '{':
        # Brace word — read until matching }
        depth = 1
        start = pos + 1
        pos++
        while pos < len(src) and depth > 0:
            if src[pos] == '{': depth++
            elif src[pos] == '}': depth--
            if depth > 0: pos++
        word = src[start:pos]
        pos++ # skip closing }
        currentWords.append(rawWord{text: word, braced: true})

    elif ch == '"':
        # Quote word — read until matching "
        start = pos + 1
        pos++
        while pos < len(src) and src[pos] != '"':
            if src[pos] == '\\': pos++  # skip escaped char
            pos++
        word = src[start:pos]
        pos++ # skip closing "
        currentWords.append(rawWord{text: word, quoted: true})

    else:
        # Plain word — read until whitespace or special char
        start = pos
        trackDepth = 0  # track [ ] depth
        while pos < len(src):
            c = src[pos]
            if c == '[': trackDepth++
            elif c == ']': trackDepth--
            elif c == '{' and trackDepth == 0: break
            elif c == ' ' or c == '\t' or c == '\n' or c == ';':
                if trackDepth == 0: break
            pos++
        word = src[start:pos]
        currentWords.append(rawWord{text: word})
```

### Verification

```bash
cd /Users/muaddib/dev/frigolite
# After creating parser.go, test parsing:
cat > /tmp/test_parser.go << 'EOF'
package main
import (
    "fmt"
    "tools/tclconvert/tcl"
)
func main() {
    src := `set x 5
db eval { CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 2); }
do_execsql_test 3.0 { SELECT * FROM t1 } {1 2}`
    cmds := tcl.ParseCommands(src)
    for i, cmd := range cmds {
        fmt.Printf("Command %d: ", i)
        for _, w := range cmd {
            fmt.Printf("[%q braced=%v] ", w.Text, w.Braced)
        }
        fmt.Println()
    }
}
EOF
# Note: need to export types or provide public ParseCommands
```

### Expected output

```
Command 0: ["set" braced=false] ["x" braced=false] ["5" braced=false]
Command 1: ["db" braced=false] ["eval" braced=false] ["CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 2);" braced=true]
Command 2: ["do_execsql_test" braced=false] ["3.0" braced=false] ["SELECT * FROM t1" braced=true] ["1 2" braced=true]
```

### Commit

```
P0.MT-1: TCL parser - tokenizer and word parser
```

---

## MT-2: TCL Interpreter fixes (`tools/tclconvert/tcl/interp.go`)

**Goal**: Complete the interpreter by implementing the missing functions that
`interp.go` already references.

### Functions to implement in `interp.go`

#### `evalWord`

```go
// evalWord evaluates a single word, performing variable ($var) and command
// ([cmd]) substitution if needed. Braced words are returned as-is.
func (i *Interp) evalWord(rw rawWord, localVars map[string]string) (string, error) {
    if rw.braced {
        return rw.text, nil  // literal, no substitution
    }
    return i.substitute(rw.text, localVars)
}
```

#### `substitute`

```go
// substitute performs $var and [cmd] substitution in a string.
// $varname → variable value (or empty string if unset)
// ${varname} → variable value (braced name form)
// [cmd args] → result of executing command
func (i *Interp) substitute(s string, localVars map[string]string) string
```

**Substitution algorithm**:
- Scan string left to right
- When `$` found: read variable name (alphanumeric + underscore), look up value
- When `${` found: read until `}`, use as variable name
- When `[` found: parse balanced `[...]`, execute as TCL command, use result
- When `\` found: handle escape sequences (`\n` → newline, `\t` → tab, `\\` → `\`)
- Everything else: literal

**Example**:
```
Input:  "INSERT INTO t1 VALUES($a, $b, $i)"
With vars: a=0, b=0, i=0
Output: "INSERT INTO t1 VALUES(0, 0, 0)"

Input:  "expr {$i/10}"  (inside [expr ...])
Output: "0"  (result of $i/10 = 0/10 = 0)
```

#### `evalAllWords`

```go
// evalAllWords substitutes all words in a rawWord slice.
func (i *Interp) evalAllWords(words []rawWord, localVars map[string]string) ([]string, error) {
    result := make([]string, 0, len(words))
    for _, rw := range words {
        val, err := i.evalWord(rw, localVars)
        if err != nil {
            return nil, err
        }
        result = append(result, val)
    }
    return result, nil
}
```

### Changes to existing code in `interp.go`

1. **Export types** for use by `main.go`:
   - `ParseCommands` (capitalized) — or keep lowercase and keep everything in package
   - `RawWord` struct fields capitalized: `Text`, `Braced`, `Quoted`

2. **Fix the `cmdDoTest` function** to handle TCL bodies that contain
   `execsql { ... }` and `db eval { ... }` properly. Currently it executes the
   body, but the body may contain nested command substitution like:
   ```
   set v [catch {execsql {SELECT * FROM t1}} msg]
   lappend v $msg
   ```
   This requires the `catch` command to properly execute the inner `execsql`
   and the `lappend` to accumulate results.

3. **Handle the `db eval { SQL } SCRIPT` form** (db eval with per-row callback):
   ```tcl
   db eval { SELECT * FROM t1 } {
     lappend result $a $b
   }
   ```
   In this case, capture the SELECT SQL as a query statement, and skip the
   per-row script (we can't execute it without real DB results).

### Verification

```bash
cd /Users/muaddib/dev/frigolite
go build ./tools/tclconvert/... 2>&1
```

Must compile with zero errors. Then test with a small program:
```go
i := tcl.NewInterp()
i.Execute(`set x 5; set y 10; set z [expr {$x + $y}]`)
// i.vars["z"] should be "15"
```

### Commit

```
P0.MT-2: TCL interpreter - implement evalWord, substitute, evalAllWords
```

---

## MT-3: Expression evaluator (`tools/tclconvert/tcl/expr.go`)

**Goal**: Evaluate TCL arithmetic/logical expressions.

### Types to define

```go
package tcl

type exprParser struct {
    input string
    pos   int
}
```

### Functions to implement

```go
// parseExpr is the entry point. Returns a Go interface{} that can be
// float64, int64, bool, or string.
func (p *exprParser) parseExpr() (interface{}, error)

// Parse helpers for each precedence level:
func (p *exprParser) parseTernary() (interface{}, error)
func (p *exprParser) parseLogicalOr() (interface{}, error)
func (p *exprParser) parseLogicalAnd() (interface{}, error)
func (p *exprParser) parseBitOr() (interface{}, error)
func (p *exprParser) parseEquality() (interface{}, error)
func (p *exprParser) parseComparison() (interface{}, error)
func (p *exprParser) parseAdditive() (interface{}, error)
func (p *exprParser) parseMultiplicative() (interface{}, error)
func (p *exprParser) parseUnary() (interface{}, error)
func (p *exprParser) parsePrimary() (interface{}, error)
```

### Operators to support

| Operator | Example | Result |
|----------|---------|--------|
| `+` `-` `*` `/` `%` | `$i % 8` | modulo |
| `<` `>` `<=` `>=` | `$i < 1000` | bool |
| `==` `!=` | `$a == 5` | bool |
| `&&` `\|\|` `!` | `$a > 0 && $b < 10` | bool |
| `<<` `>>` `&` `\|` `^` `~` | `$i << 2` | bitwise |
| `eq` `ne` | `$s eq "hello"` | string compare |
| `? :` | `$x ? $a : $b` | ternary |
| `func(args)` | `int(log($i))` | function call |

### Functions to support

```go
// In parsePrimary, handle function calls:
// abs(x), double(x), int(x), round(x), floor(x), ceil(x),
// sqrt(x), pow(x,y), log(x), exp(x),
// min(x,y), max(x,y), rand(), srand(x), wide(x)
```

### Concrete example

Input from analyze8.test:
```tcl
expr {$i/10}         # i=5 → "0"
expr {$i%8}          # i=13 → "5"
expr {($i%8)*100}    # i=13 → "500"
expr {$c*$c*$c}      # c=2 → "8"
expr {int(log($i)/log(2))}  # i=8 → "3"
```

### Algorithm sketch

```
parsePrimary:
    skip whitespace
    if current char is digit:
        read number, return float64
    if current char is '"':
        read string literal, return string
    if current char is '(':
        consume '(', parseExpr, consume ')'
    if current char is letter:
        read identifier
        if next char is '(':
            parse function call: name(args)
        else:
            return as string (bare word)
    error

parseUnary:
    if peek is '-': consume, return negate(parseUnary)
    if peek is '!': consume, return not(parseUnary)
    if peek is '~': consume, return bitwise not(parseUnary)
    return parsePrimary

parseMultiplicative:
    left = parseUnary
    while peek is '*' or '/' or '%':
        op = consume
        right = parseUnary
        left = apply(op, left, right)
    return left

// ... similar pattern for each precedence level
```

### Verification

```bash
go build ./tools/tclconvert/...
# Test:
# EvalExpr("$i/10", interp_with_i=5) → "0"
# EvalExpr("$i%8*100", interp_with_i=13) → "500"
# EvalExpr("$c*$c*$c", interp_with_c=2) → "8"
```

### Commit

```
P0.MT-3: TCL expression evaluator
```

---

## MT-4: TCL list helpers (`tools/tclconvert/tcl/list.go`)

**Goal**: Implement TCL list operations used by test files.

### Functions to implement

```go
package tcl

// splitList parses a TCL list string into elements.
// TCL lists are whitespace-separated, with {} or "" grouping.
// Example: "a b {c d} e" → ["a", "b", "c d", "e"]
func splitList(s string) []string

// tclList converts a Go slice to a TCL list string.
// Example: ["a", "b c", "d"] → "a {b c} d"
func tclList(items []string) string

// tclLLength returns the number of elements in a TCL list.
func tclLLength(s string) int

// tclLIndex returns element at index (0-based).
func tclLIndex(s string, idx int) string

// tclLRange returns elements from start to end (inclusive).
func tclLRange(s string, start, end int) string
```

### splitList algorithm

```
result = []
pos = 0
while pos < len(s):
    skip whitespace
    if pos >= len(s): break

    if s[pos] == '{':
        # Braced element — read until matching }
        depth = 1, start = pos+1, pos++
        while depth > 0:
            if s[pos] == '{': depth++
            elif s[pos] == '}': depth--
            if depth > 0: pos++
        result.append(s[start:pos])
        pos++ # skip closing }

    elif s[pos] == '"':
        # Quoted element
        start = pos+1, pos++
        while s[pos] != '"': pos++
        result.append(s[start:pos])
        pos++

    else:
        # Plain element — read until whitespace
        start = pos
        while pos < len(s) and s[pos] not in ' \t\n': pos++
        result.append(s[start:pos])
return result
```

### Verification

```bash
go build ./tools/tclconvert/...
# splitList("a b c") → ["a","b","c"]
# splitList("a {b c} d") → ["a","b c","d"]
# tclList(["a","b c"]) → "a {b c}"
```

### Commit

```
P0.MT-4: TCL list helpers
```

---

## MT-5: Converter main (`tools/tclconvert/main.go`)

**Goal**: Read .test files, execute TCL, write JSON.

### Algorithm

```go
package main

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "tools/tclconvert/tcl"
)

func main() {
    testDir := flag.String("testdir", "ori/sqlite/test", "TCL test directory")
    outDir  := flag.String("outdir", "testdata", "JSON output directory")
    flag.Parse()

    files, _ := filepath.Glob(filepath.Join(*testDir, "*.test"))
    for _, testFile := range files {
        base := strings.TrimSuffix(filepath.Base(testFile), ".test")
        src, _ := os.ReadFile(testFile)
        interp := tcl.NewInterp()
        interp.Execute(string(src))
        stmts := interp.Stmts()
        json := convertToJSON(base, stmts)
        os.WriteFile(filepath.Join(*outDir, base+".json"), json, 0644)
    }
}
```

### JSON conversion logic

```go
// convertToJSON converts captured statements into the JSON test format.
// Statements are grouped by TestName. Statements with no TestName go into
// a "setup" group. Each group becomes a TestCase with Steps.
//
// JSON structure (MUST match existing format):
// {
//   "file": "analyze8",
//   "tests": [
//     {
//       "name": "1.0",
//       "steps": [
//         {"type": "exec", "sql": "CREATE TABLE t1(a,b,c,d)"},
//         {"type": "exec", "sql": "INSERT INTO t1 VALUES(0,0,0,0)"},
//         ...
//       ]
//     },
//     ...
//   ]
// }
func convertToJSON(base string, stmts []tcl.Stmt) []byte
```

**Key rules**:
1. Group statements by `TestName` preserving capture order
2. Statements with no TestName → group into `setup_0`, `setup_1`, etc.
3. Multi-statement SQL (containing `;`) → split into individual steps
4. `type` field: `"exec"` for DDL/DML, `"query"` for SELECT/PRAGMA/EXPLAIN
5. `expect` field: from `Expected` in Stmt, or omitted if empty
6. For `do_execsql_test` where SQL is a multi-statement string like
   `SELECT ...; SELECT ...`, keep as one step (the harness handles multi-stmt)

### Verification

```bash
cd /Users/muaddib/dev/frigolite
go run ./tools/tclconvert/ -testdir ori/sqlite/test -outdir /tmp/test_json
# Check analyze8.json has CREATE TABLE:
python3 -c "
import json
d = json.load(open('/tmp/test_json/analyze8.json'))
has_create = any('CREATE TABLE' in s.get('sql','').upper()
    for tc in d['tests'] for s in tc['steps'])
print(f'Has CREATE TABLE: {has_create}')
print(f'Tests: {len(d[\"tests\"])}')
"
# Expected: Has CREATE TABLE: True, Tests: >= 5
```

### Commit

```
P0.MT-5: Go TCL converter main entry point
```

---

## MT-6: Regenerate all test data

**Goal**: Run the converter on all 1192 .test files and regenerate JSON.

### Steps

1. **Backup current testdata**:
   ```bash
   cp -r testdata testdata_backup
   ```

2. **Run converter**:
   ```bash
   go run ./tools/tclconvert/ -testdir ori/sqlite/test -outdir testdata
   ```

3. **Count files with CREATE TABLE** (was 591, target 950+):
   ```bash
   python3 -c "
   import json, os
   count = 0
   for f in os.listdir('testdata'):
       if not f.endswith('.json'): continue
       d = json.load(open(f'testdata/{f}'))
       for tc in d.get('tests', []):
           for s in tc.get('steps', []):
               if 'CREATE TABLE' in s.get('sql','').upper():
                   count += 1
                   break
           else: continue
           break
   print(f'Files with CREATE TABLE: {count}')
   "
   ```

4. **Commit**:
   ```
   P0.MT-6: regenerate all 1002 JSON test files with TCL interpreter
   ```

---

## MT-7: Run full test suite and measure

**Goal**: Verify the regenerated test data improves test results.

### Steps

1. **Run full suite**:
   ```bash
   go test -run "^TestSQLiteSuite$" -count=1 -timeout 120s -v . 2>&1 | \
     grep -E "^    --- (PASS|FAIL): TestSQLiteSuite/[^/]+$" | \
     awk '/PASS/{p++} /FAIL/{f++} END{printf "PASS=%d FAIL=%d TOTAL=%d\n",p,f,p+f}'
   ```

2. **Check for regressions** (files that passed before but fail now):
   ```bash
   # Compare with baseline (215 passing files from git log)
   ```

3. **If regressions exist**: compare old vs new JSON for affected files:
   ```bash
   diff testdata_backup/FILENAME.json testdata/FILENAME.json
   ```

4. **Commit measurements**:
   ```
   P0.MT-7: measurement - PASS=X/869 (was 215)
   ```

---

## MT-8: Fix converter edge cases

**Goal**: Handle remaining patterns that the initial converter misses.

### Known edge cases to handle

1. **`sqlite3 db2 file.db`** (multi-connection): Skip these statements. Tables
   created via db2 are not accessible from db. Mark as no-op.

2. **`db eval { SQL } { script }`** (per-row callback): Capture the SELECT SQL
   as a query, skip the callback script.

3. **`subst` command**: `subst { $a + $b }` → substitute and return.

4. **`breakpoint` command**: No-op.

5. **`do_test` with `[catch {execsql {...}} msg]` pattern**: The `catch`
   command should execute the inner `execsql`, and `msg` should get the result.

6. **TCL `array set` / `array get`**: No-op (simplified).

7. **`format` command** (printf-like): `format "%d" $i` → return string.

8. **`string map`**: `string map {from to} $str` → simple replacement.

### Steps

1. Identify which edge cases cause test failures.
2. Implement fixes one at a time.
3. **Commit each fix**: `P0.MT-8: handle <edge case>`

---

## MT-9: Update PLAN.md and final commit

**Goal**: Record final measurements and update the master plan.

### Steps

1. Update `plans/PLAN.md` Live Metrics table with final PASS count.
2. Mark P0 as complete in the Progress Tracking table.
3. Update `plans/P0_DETAILED.md` with final status.
4. **Commit**: `P0.complete: TCL converter done, PASS=X/869`

---

## File Dependency Graph

```
MT-1 (parser.go)  ─────────────┐
MT-4 (list.go)    ─────────────┤
MT-3 (expr.go)    ───┐         │
                     ├──→ MT-2 (interp.go fixes)
                     │         │
                     │         ├──→ MT-5 (main.go)
                     │         │
                     │         ├──→ MT-6 (regenerate)
                     │         │
                     │         ├──→ MT-7 (measure)
                     │         │
                     │         └──→ MT-8 (edge cases)
                     │
                     └── MT-9 (update plan)
```

**Recommended order**: MT-1 → MT-4 → MT-3 → MT-2 → MT-5 → MT-6 → MT-7 → MT-8 → MT-9

MT-1 and MT-4 have no dependencies and can be done first. MT-3 depends on
understanding the interpreter. MT-2 depends on MT-1, MT-3, MT-4. MT-5
depends on MT-2.

---

## Current State (checkpoint)

| Component | File | Status | Lines |
|-----------|------|--------|-------|
| Parser | parser.go | ❌ Not started | — |
| Interpreter | interp.go | ⚠️ Partial | 1034 |
| Expr evaluator | expr.go | ❌ Not started | — |
| List helpers | list.go | ❌ Not started | — |
| Converter main | main.go | ❌ Not started | — |

**interp.go** has working command handlers for: set, incr, expr, if, for,
foreach, while, proc, return, break, continue, catch, uplevel, upvar,
list, lappend, llength, lindex, lrange, string, regexp, regsub,
execsql, catchsql, db (eval/onecolumn/transaction/close),
do_execsql_test, do_catchsql_test, do_test, do_eqp_test.

**interp.go** references unimplemented functions:
- `parseCommands()` — from parser.go (MT-1)
- `rawWord` type — from parser.go (MT-1)
- `evalWord()` — needs substitution logic (MT-2)
- `substitute()` — needs substitution logic (MT-2)
- `evalAllWords()` — needs substitution logic (MT-2)
- `splitList()`, `tclList()`, `tclLLength()`, `tclLIndex()`, `tclLRange()` — from list.go (MT-4)
- `exprParser`, `EvalExpr()` — from expr.go (MT-3)

---

## TCL Constructs in Test Files (reference data)

| Construct | Count (1192 files) | Handler |
|-----------|-------------------|---------|
| `do_test` | 31257 | `cmdDoTest` ✅ |
| `do_execsql_test` | 14058 | `cmdDoExecSQL` ✅ |
| `db eval` | 12463 | `cmdDB` ✅ |
| `execsql` | 11502 | `cmdSQL` ✅ |
| `set` | 10580 | `cmdSet` ✅ |
| `catchsql` | 2595 | `cmdSQL` ✅ |
| `if` | 2388 | `cmdIf` ✅ |
| `foreach` | 2009 | `cmdForeach` ✅ |
| `incr` | 1538 | `cmdIncr` ✅ |
| `do_catchsql_test` | 1432 | `cmdDoCatchSQL` ✅ |
| `proc` | 1409 | `cmdProc` ✅ |
| `expr` | 797 | Expr evaluator (MT-3) |
| `for` | 763 | `cmdFor` ✅ |
| `uplevel` | 158 | `cmdUplevel` ✅ |
| `db transaction` | 52 | `cmdDB` ✅ |
| `db onecolumn` | 7 | `cmdDB` ✅ |
