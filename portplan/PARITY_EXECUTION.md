# PARITY EXECUTION PLAN — the detailed work plan

> **Status**: ACTIVE (2026-10-20). Companion documents:
> `portplan/PARITY_PLAN.md` (the goals, the exit criteria and the binding roots),
> `PORTPLAN.md` (the feature roadmap), `plan/GUIDELINES.md` (the execution
> protocol), `handover.md` (session state), `.agents/lessons_learned.md`
> (durable findings).
>
> **This document is the "what / how" for every remaining failure.** It is
> written against the run-18 baseline (14,431 leaves / 305 families) and names,
> for each work item, the evidence, the root cause, the exact change, the
> verification command and the definition of done.

---

## 0. How to read this document

| section | content |
|---|---|
| §1 | the baseline and the three blocking findings (F1–F3) that gate everything else |
| §2 | the work streams W0–W8: goal, evidence, root cause, exact change, steps, verification, DoD, risk |
| §3 | the full register: every failing family → its work item |
| §4 | the execution order (phases), the batching procedure, the stop rules |
| §5 | the verification command list |
| §6 | the progress log |

**Binding rules** (from `PARITY_PLAN.md` §0 — they gate every item below):
all tests pass · a TCL test may be marked obsolete **only** when a named native
Go test covers the same functional aspect · no descoping (the skip lists do not
grow, expectations are never weakened, fixtures are never trimmed) · every
failure is owned · every item closes with evidence in §6.

---

## 1. Baseline and the three blocking findings

### 1.1 Baseline

| run | when | failing leaves | families | log |
|---|---|---|---|---|
| 16 | 2026-10-20 14:00 | 2,804 | 286 | `/tmp/run16-fails.txt` |
| 17 | 2026-10-20 15:40 | 14,745 | — | superseded (the first form of the `query_assign` check) |
| 18 | 2026-10-20 16:20 | 14,431 | 305 | `/tmp/frigolite-full-suite18.log` |
| 19 | 2026-10-20 17:40 | — | — | **invalid**: killed (`signal: terminated`) when the waiting tool call was interrupted, and earlier killed (`signal: killed`) by a concurrent converter run. Launch the suite detached (a new session) so an interrupted call cannot kill it. |
| 21 | 2026-10-21 08:00 | 10,120 | 329 | `/tmp/frigolite-full-suite21.log` — after **W0+W1+W2+W3** (the `query` arm restored, the `string` family implemented, the do_test body-value semantics) and with 12 fresh fixtures installed. 4,281 leaves fewer than run 18. |
| **23** | 2026-10-21 20:00 | **6,720** | **327** | `/tmp/frigolite-full-suite23.log` — after the runtime-conditional model, the Tcl authorizer, the `mode=memory` URIs, the runtime expectations and the statement-status counters. **3,400 leaves fewer than run 21.** |

The run-18 shape (families grouped by their dominant error):

| leaves | families | dominant error | work item |
|---|---|---|---|
| 11,950 | 26 | `query_assign mismatch` | **W2 + W3** (F2/F3) |
| 323 | 22 | `no such table: t1` | **W1** (F1) + W6.4 |
| 322 | 49 | `expected error but got success` | W6.2 / W6.3 |
| 293 | 5 | `runtime variable mismatch (got "")` | W6.4 / W7 |
| 225 | 2 | `database is locked` | W6.4 (pager1, notify3) |
| 220 | 32 | `error mismatch` | W6.1 |
| 109 | 2 | `collation destructor count` | W6.4 (multiplex) |
| 105 | 2 | `table t1 has N columns but M values` | W6.4 (wal2) |
| ~800 | ~140 | the long tail (1–30 leaves each) | W7 / W8 |

### 1.1b What run 21 shows

The restored assertions (W1) moved ~4,000 leaves from *vacuous* to *red* —
that is the truth being revealed, not a regression:

| family | run 18 | run 21 | why |
|---|---|---|---|
| `where7` | 20 | **2,021** | the `query` steps' expectations are the stmt-status counters (`scan N sort M`) — the engine does not produce them (W4.1) |
| `pager2` | 1 | **1,938** | the `runtime_loop` step's inner statements now execute and are asserted |
| `rtree2` | 2,010 | 1,514 | W3 turned the comparison into a real one; the remainder is the conditional-execution gap (W3.5) |
| `pager1` | 212 | **543** | the restored assertions in the fresh fixture reveal the reset-position defects (W6.4) |
| `fts3expr`, `fts3expr3`, `e_reindex` | — | 525 | **new** fixtures (new coverage → new red) |

So the *engineering* progress is 14,431 → 10,120 leaves, while the *visible*
number moves less because the suite is now honest about what it asserts.

### 1.2 F1 — the harness silently drops 173,795 `query` steps (768 families)

**Evidence**

* `git show HEAD:frigolite_harness_test.go | grep 'case "query"'` → the case
  existed at HEAD (line 1223). The current uncommitted tree has **no**
  `case "query"` in the main step switch (the only `case "query"` left is at
  line 3976, inside the runtime-loop body helper).
* The fixture census: **173,795** steps of type `query` across **768**
  families (`corruptI` 50,004 · `json106` 40,000 · `date4` 24,859 · `e_expr`
  12,181 · `func` 12,335 · …).
* A Go `switch` with no matching `case` and no `default` **falls through
  silently**: those steps are neither executed nor asserted.
* Consequence 1 (coverage hole): the assertions in those steps are vacuous —
  `date4` "passes" today without running a single statement. That violates
  root 3 (no descoping of functional coverage).
* Consequence 2 (red set): `btree01-1.1` is a `query` step that creates `t1`;
  `btree01-1.2.1` is an `exec` step that then reports **`no such table: t1`**
  (211 leaves). Same pattern for `temptable2` (75), `wal6`/`walbak`/`walvfs`
  (61), `pager4`, `resetdb`, `misc8`, …

**Root cause**: an incomplete rename in the uncommitted work (`query` →
`query2` for named results) — the harness kept `query2` and dropped `query`,
while the converter still emits `query` in three places
(`tools/tclconvert/tcl/interp_sql.go:103` `db onecolumn`, `:118` `db one`,
`:771` `do_eqp_test`) and `db eval` emits it for every SELECT.

**This is the single highest-leverage fix in the plan.** It restores ~174k
assertions *and* the side effects the red set is complaining about.

### 1.3 F2 — the converter's `string` command silently returns its first argument

**Evidence**

```go
// tools/tclconvert/tcl/interp.go:1229
func (i *Interp) cmdString(args []string) error {
    ...
    if fn, ok := stringHandlers[sub]; ok { return fn(i, args) }
    i.vars[""] = args[1]      // ← unknown subcommand: return the input unchanged
    return nil
}
```

`stringHandlers` implements `length tolower toupper trim range compare equal
first map repeat index match is replace` — but **not** `trimleft`/`trimright`.

`ori/sqlite/test/round1.test:28`:

```tcl
set r $x1.$x3                       ;# e.g. "90821.8400"
set ans [string trimright $r 0]    ;# Tcl: "90821.84"   converter: "90821.8400"
do_test $iTest/$n/${r}4=>$ans {
  set x [db one "SELECT round(${r}4,$n)"]
} $ans
```

The converter's `string trimright` returns the *input*, so the expectation
becomes `90821.8400` while SQLite returns the REAL `90821.84`
(`sqlite3_result_double`, `../sqlite/src/func.c:472`) → **9,356 false
failures** in `round1`, plus the same class elsewhere.

The same silent fallback hides every other unimplemented subcommand
(`string trimleft` 4 uses / 3 files, `string last`, `string cat`,
`string reverse`, `string wordstart`, `string wordend`).

### 1.4 F3 — a `do_test` expectation is attached to the wrong statement

**Evidence** — `ori/sqlite/test/rtree2.test:48`:

```tcl
do_test rtree2-$module.$nDim.2 {
  db transaction { for {...} { db eval "INSERT INTO t1 ..." } }
  set t1 [execsql {SELECT * FROM t1 ORDER BY ii}]
  set t2 [execsql {SELECT * FROM t2 ORDER BY ii}]
  set rc [expr {$t1 eq $t2}]
  if {$rc != 1} { puts $t1; puts $t2 }
  set rc
} {1}
```

The body's value is the value of the **last command** (`set rc` → "1"); the
expectation `{1}` describes *that*, not the rows of the last captured query.

`tools/tclconvert/tcl/interp_sql.go:735` attaches the expectation to the last
captured statement unconditionally:

```go
if expected != "" && len(i.stmts) > 0 {
    last := &i.stmts[len(i.stmts)-1]
    if last.TestName == name && last.Expected == "" { last.Expected = expected }
}
```

→ the harness compares the *rows* of `SELECT * FROM t2 ORDER BY ii` with `"1"`
→ **2,010 false failures** in `rtree2`, plus the same shape in `where` (118),
`conflict` (65), `conflict2` (64), `collate4` (51), `where9` (46), `memdb` (41),
`descidx1` (26), `analyze3` (26), `func2` (17), …

### 1.5 What F1+F2+F3 are worth

| finding | leaves | families | nature |
|---|---|---|---|
| F1 (harness drops `query` steps) | ~323 direct + the whole tail | 768 | coverage hole + missing side effects |
| F2 (`string` fallback) | 9,356 + part of the tail | ~30 | wrong expectations |
| F3 (`do_test` body semantics) | ~3,500 | ~26 | wrong expectations |
| **total** | **~13,200 of 14,431** | — | — |

---

## 2. Work streams

Each item states: **goal → evidence → root cause → exact change → steps →
verification → DoD → risk**. W0–W5 are *Phase A* (they unblock the corpus);
W6–W8 are *Phase B/C* (the remaining engine/harness work).

---

### W0 — Make silent drops impossible (guards)

**Goal**: no future step type, TCL command or converter shortcut can be dropped
without a loud failure.

**Root cause of the class**: the harness's step `switch` has no `default`, and
the converter's `cmdString` has an "unknown → return the input" fallback.

**Exact change**

1. `frigolite_harness_test.go` — add a `default:` arm to the step switch
   (near line 3458):

   ```go
   default:
       t.Errorf("unknown step type %q (the harness must implement every step type the converter emits)", step.Type)
       return
   ```

2. `tools/tclconvert/tcl/interp.go:1229` — replace the fallback:

   ```go
   if fn, ok := stringHandlers[sub]; ok {
       return fn(i, args)
   }
   return fmt.Errorf("unsupported string subcommand %q", sub)
   ```

   (The converter's per-file error handling turns this into a *reported
   conversion failure* instead of a silently wrong expectation.)

3. New test `frigolite_harness_steptypes_test.go`: read every `testdata/*.json`,
   collect the step types, and assert that every one of them is handled by the
   harness (the handled set is derived from the switch labels, or — simpler and
   more robust — from a `harnessKnownStepTypes` map that the switch consults).

**Verification**

```bash
go test . -run 'TestHarnessStepTypes|TestSOLID_' -count=1
go test ./tools/tclconvert/... -count=1
```

**DoD**: an unknown step type fails the run; an unknown `string` subcommand
fails the conversion of that file (and the family is reported, not silently
mangled).

**Risk**: low. The `default:` arm may expose other unknown types — that is the
point; each one becomes a work item.

---

### W1 — Restore the dropped `query` steps (F1)

**Goal**: every `query` step executes and asserts, as at HEAD.

**Exact change** — `frigolite_harness_test.go`, in the main step switch (next
to `case "query2":`, line ~3181):

```go
case "query":
    res := execDB.Query(step.SQL)
    if res.Error != nil {
        t.Errorf("query error: %v\n  sql: %s", res.Error, step.SQL)
        return
    }
    if step.Expect != "" {
        got := flattenResultNull(res, nullToken)
        want := cleanExpectedNull(step.Expect, nullToken)
        if msg := matchExpectation(got, want); msg != "" {
            t.Errorf("result mismatch\n  got:  [%s]\n  %s\n  sql: %s", got, msg, step.SQL)
        }
    }
```

Notes:
* use `execDB` (not `db`) so a step bound to an ATTACHed database runs on that
  handle — the HEAD version used `db`, which broke the named-DB families;
* use the harness-scope `nullToken` (the HEAD version used `td.NullToken`,
  which is per-file and wrong for the runtime `db nullvalue` override).

**Steps**
1. Add the case.
2. Re-run the six families that prove the side effects and the assertions are
   back: `btree01`, `temptable2`, `date4`, `json106`, `e_expr`, `corruptI`.
3. Re-measure the corpus baseline (the red set will *grow* where the restored
   assertions now fail — that is correct, not a regression; see §4.3).

**Verification**

```bash
FRIGOLITE_TEST=btree01   go test . -run '^TestSQLiteSuite$' -count=1
FRIGOLITE_TEST=date4     go test . -run '^TestSQLiteSuite$' -count=1
FRIGOLITE_TEST=e_expr    go test . -run '^TestSQLiteSuite$' -count=1
```

**DoD**: `btree01`'s `t1` exists for `1.2.1`; `date4` executes its 24,859
statements and asserts them (a deliberately wrong expectation in a scratch
fixture must fail — prove it once, then remove it).

**Risk**: **high but bounded** — this is expected to *increase* the red set
(every previously vacuous assertion now runs). Do not "fix" that by
re-dropping the step; triage the newly visible failures under W6/W7.

---

### W2 — The converter's `string` command (F2)

**Goal**: `string` is complete and never lies.

**Corpus census** (uses / files over `ori/sqlite/test/*.test`):

| subcommand | uses | files | state |
|---|---|---|---|
| `repeat` | 285 | 106 | implemented |
| `map` | 142 | 73 | implemented |
| `range` | 140 | 68 | implemented |
| `match` | 83 | 46 | implemented + oracle pin |
| `length` | 78 | 43 | implemented |
| `trim` | 69 | 31 | implemented — **audit the charlist semantics** |
| `compare` | 67 | 35 | implemented |
| `is` | 19 | 14 | implemented |
| `tolower`/`toupper` | 18/7 | 10/5 | implemented |
| `first` | 9 | 6 | implemented |
| `equal` | 5 | 4 | implemented |
| **`trimleft`** | **4** | **3** | **missing** |
| `index` | 3 | 2 | implemented |
| **`trimright`** | **2** | **1** | **missing** (round1) |
| `last`, `cat`, `reverse`, `wordstart`, `wordend` | 0 | 0 | implement anyway (cheap) |

**Exact change** — `tools/tclconvert/tcl/interp_string.go`:

```go
// stringTrimLeft implements `string trimleft string ?chars?`.
func stringTrimLeft(i *Interp, args []string) error {
    if len(args) < 2 { i.vars[""] = ""; return nil }
    cutset := " \t\n\r\v\f"
    if len(args) > 2 { cutset = args[2] }
    i.vars[""] = strings.TrimLeft(args[1], cutset)
    return nil
}

// stringTrimRight implements `string trimright string ?chars?`.
func stringTrimRight(i *Interp, args []string) error {
    if len(args) < 2 { i.vars[""] = ""; return nil }
    cutset := " \t\n\r\v\f"
    if len(args) > 2 { cutset = args[2] }
    i.vars[""] = strings.TrimRight(args[1], cutset)
    return nil
}
```

Register them in `stringHandlers`; add `last`, `cat`, `reverse`, `wordstart`,
`wordend` the same way; remove the silent fallback (W0.2).

**Oracle pins** — extend `tools/tclconvert/tcl/string_match_test.go` (or a new
`string_trim_test.go`) with a table-driven test whose expectations come from
`/opt/homebrew/bin/tclsh`:

```tcl
foreach c {{trimright 90821.8400 0} {trimleft "  x" " "} {trim "xxaxx" x} {last abc b} {cat a b}} {
    puts "[lindex $c 0] => [string [lindex $c 0]]"
}
```

**Verification**

```bash
go test ./tools/tclconvert/... -count=1
FRIGOLITE_TEST=round1 go test . -run '^TestSQLiteSuite$' -count=1   # expect 9,356 → 0
```

**DoD**: `round1` = 0 failures; the other `trimleft`/`trimright` families
(`analyze9`, `date5`, …) improve; no `string` subcommand is unimplemented.

**Risk**: low. The only risk is *revealing* new gaps (e.g. `string trim` with a
charlist) — each becomes a small follow-up.

---

### W3 — `do_test` body semantics (F3)

**Goal**: the `do_test` expectation describes the **body's value**, never a
statement that merely happened to be captured last.

**Model** (SQLite's testfixture, `../sqlite/test/tester.tcl`):

```
do_test NAME { body } { expected }
      └ the body's value (the last command's result) is compared with expected
```

The body's value is:

| body's last command | the value is | the converter must |
|---|---|---|
| `db eval {...}` / `execsql {...}` | that statement's result | keep the expectation on that step (current behaviour) |
| `set X` | the value of `X` | emit a `runtime_var` step for `X` |
| `set X [expr {...}]` | the expr over runtime values | emit a `runtime_expr` step for the expr |
| `expr {...}` | the expr | emit a `runtime_expr` step (already implemented) |
| `if`/`for`/`while`/`proc` | the last executed command's value | resolve recursively; if the value is runtime-dependent → `runtime_expr` |

**Exact change** — `tools/tclconvert/tcl/interp_sql.go`, `cmdDoTest`:

1. Parse the body's commands (the file already has `parseCommands`).
2. Compute the *body value source*:
   * `bodyValueIsLastStatement` — the last command is a captured SQL → keep
     today's behaviour (expectation on that step);
   * `bodyValueIsVar(name)` — the last command is `set name` → emit
     `Stmt{Type: "runtime_var", Variable: name, Expected: expected}` and
     **do not** attach the expectation to the last SQL step;
   * `bodyValueIsExpr(text)` — the last command is `expr {...}` (possibly
     assigned) → emit `Stmt{Type: "runtime_expr", SQL: baked(text), Expected: expected}`.
3. Remove the unconditional "attach the expectation to the last captured
   statement" for the cases in (2) — it is only correct for (1).
4. `set X [execsql {...}]` inside a body must become a **query_assign** step
   with `Variable: X` and `ResultAll: true` (the harness already renders that
   as a Tcl list) so that the runtime expr can compare `$t1 eq $t2`. Today
   `execsql` emits an `exec` step (`tools/tclconvert/tcl/interp_cmds.go:864`),
   which keeps the side effect but **discards the rows** — so `$t1`/`$t2` are
   never available at runtime. (`execsql2` is the one that emits `query2`.)

**Harness support** — `frigolite_harness_expr_test.go` (`evalHarnessTclExpr`):
the evaluator already covers `eq ne == != < > <= >= && || !` and the ternary;
add whatever the corpus needs (audit with a grep over `expr {` in
`ori/sqlite/test/*.test`).

**Verification**

```bash
FRIGOLITE_TEST=rtree2  go test . -run '^TestSQLiteSuite$' -count=1   # 2,010 → 0
FRIGOLITE_TEST=where   go test . -run '^TestSQLiteSuite$' -count=1
FRIGOLITE_TEST=conflict go test . -run '^TestSQLiteSuite$' -count=1
```

**DoD**: `rtree2` = 0; no fixture step compares a *row set* against a scalar
expectation (grep the fixtures for a `query`/`query_assign` step whose
expectation is a bare integer while the SQL is a `SELECT *`).

**Risk**: medium — the body-value classification can mis-fire; keep the
"expectation on the last statement" path for the plain case and make the new
paths *additive* (never remove an assertion that currently passes for a
non-scalar SQL result).

**Landed (2026-10-20)** — the converter now emits the right fixture shape and
the harness evaluates it:

| piece | change |
|---|---|
| `set X [expr {...}]` over runtime values | a `runtime_expr` step that stores its result in `X` (`tools/tclconvert/tcl/interp_cmds.go`) |
| the body's value (`set rc`) | a `runtime_var` step carrying the do_test expectation (`tools/tclconvert/tcl/interp_sql.go`) |
| the expectation | **no longer** attached to the last captured statement when the body's value was captured separately |
| the harness | `runtime_expr` stores its value in the named variable and only compares a non-empty expectation (`frigolite_harness_test.go`) |
| the row list | a new `resultRows` fixture field: the variable holds *every* row (a Tcl list of rows), not just the first one — Tcl's `execsql`/`db eval` yield a list of rows |

`rtree2` 2,010 → **1,513** with the assertion now real. The pin test is
`tools/tclconvert/tcl/dotest_body_value_test.go`.

**W3.5 — the remaining rtree2 gap: conditional execution (C1).** The TCL

```tcl
set rc [catch {db eval "INSERT INTO t1 VALUES($ii, $values)"}]
if {$rc} { incr ii -1 } else { db eval "INSERT INTO t2 VALUES($ii, $values)" }
```

runs the *second* insert only when the first one succeeded. The converter
evaluates `catch` at conversion time (where the capture cannot fail) and
therefore always takes the `else` branch → the shadow table gets rows the
rtree table does not → the comparison is false ✗.

The fix is a **conditional step** in the fixture model:

| piece | change |
|---|---|
| `catch { <sql> }` | emit `{"type":"catch","sql":...,"variable":"rc"}` — the harness stores the catch result in `rc` |
| `if {$rc} {A} else {B}` | when the condition reads a runtime variable, annotate the steps emitted for the branch with `{"condition":"$rc == 0"}` (the converter records the step index before executing the branch and stamps the new steps) |
| the harness | a step with a `condition` is executed only when the harness's expression evaluator (`evalHarnessTclExpr`) says the condition is true |

That model is general (it also covers `if {[catch ...]}`, `while` bodies and
`switch`), it is the only way to reproduce TCL's dynamic control flow in a
fixture, and it needs no engine change. It also fixes the same class in
`pagerfault` (the `table t1 already exists` cascade) and `temptable2`.

---

### W4 — One statement per step (multi-statement bodies)

**Goal**: a step is one SQL statement, so an expectation always names the
statement it describes.

**Why**: `btree01-1.1` is one `query` step with six statements; the engine
runs them all and returns the last result, so the expectation "ok" (the
`PRAGMA integrity_check`) is right — but any *other* statement's effect cannot
be asserted, and a failure inside the batch is reported against the whole
blob.

**Exact change** — the converter splits a multi-statement SQL at the
statement level (the tokenizer in `internal/sql` already knows where
statements end; the trigger-body `BEGIN…END;` case needs the same care as
the engine's own splitter — reuse `internal/sql`'s splitter if it is exported,
otherwise implement it in the converter with the same rules):

* the first N−1 statements become `exec` steps (side effects only),
* the last statement keeps the original type and the expectation.

**Verification**: `btree01`, `temptable2`, `wal2` (the "table t1 has 2 columns
but 1 values" family is a symptom of a lost INSERT), `pagerfault`.

**DoD**: no fixture step contains more than one statement unless it is inside
a trigger body.

**Risk**: medium — a naive splitter breaks trigger bodies and `BEGIN…END`
blocks. Implement against the engine's splitter, not from scratch.

---

### W5 — Regenerate and batch-verify the whole corpus

**Goal**: every fixture in `testdata/` comes from the current converter and is
verified by the batch procedure.

**State now**: 1,220 fixtures — 1,077 from the current converter, **41 stale**
(reverted by a batch: `aggnested alter2 altercons2 analyze3 analyze5 analyze9
attach2 autoinc bestindex1 collate1 e_fkey e_walhook func insert4 join json102
like pager1 pagerfault quote reindex rowid rtreecheck schema2 select1 …`),
**102 not regenerated** by the latest conversion (the converter's statement
budget skips them).

**Steps**

1. `mkdir -p /tmp/regen19 && go run ./tools/tclconvert/ -outdir /tmp/regen19`
   (the converter does **not** create the outdir).
2. Install the changed fixtures with the batch driver (`/tmp/regen3-batch.py`,
   env-parameterized: `REGEN_SRC REGEN_CHANGED REGEN_LOG REGEN_JSONL
   REGEN_REVERTED REGEN_BACKUP`), which **verifies then installs** and reverts
   any family whose leaf set regresses.
3. Raise the converter's statement budget so the 102 skipped families are
   converted too (or convert them with a per-file budget override).
4. Re-measure the corpus baseline.

**Verification**

```bash
python3 - <<'EOF'
import os, filecmp
src, inst = '/tmp/regen19', 'testdata'
stale = [f for f in os.listdir(inst) if f.endswith('.json')
         and (not os.path.exists(os.path.join(src, f))
              or not filecmp.cmp(os.path.join(src, f), os.path.join(inst, f), shallow=False))]
print(len(stale), 'stale fixtures'); print(stale[:40])
EOF
```

**DoD**: 0 stale fixtures; every family in `ori/sqlite/test` has a fixture.

**Amendment (2026-10-20, after batch #6)**: the batch's "revert on any
regression" rule is a *safety* mechanism for batch work, not a destination.
44 of the 60 families in batch #6 were reverted — most of them because the
fresh fixture *revealed* failures the stale fixture had hidden (e.g.
`analyze5`: old 0 → new 86; `json102`: old 0 → new 11). That is the C5 class
("a stale fixture hides a defect"). Keeping those stale fixtures installed
would be a permanent descoping of functional coverage (root 3), so:

* every reverted family is a **work item**: install the fresh fixture and fix
  the failures it reveals (the family's own triage item in §3);
* the batch may no longer be used to *avoid* a red family — it may only be
  used to avoid a *regression* while a fix is being prepared;
* the endgame is: 0 stale fixtures, 0 reverted families, 0 red leaves.

**Risk**: low, but the batch rule compares *normalized leaf names* — a family
whose old fixture used a different naming convention (`14.1.3` vs
`json501-14.1.3`) yields a false regression. Read the regression list against
the old fixture's actual names before reverting.

---

### W6 — Engine parity (the remaining red set, by cluster)

Each sub-item is: **symptom → SQLite's behaviour (with the source pointer) →
the change in `internal/` → verification**.

#### W6.1 ALTER TABLE — DROP/RENAME COLUMN (≈40 leaves)

*Symptoms*: `alterdropcol2` (9), `altercol` (8), `alterlegacy` (9),
`altertab` (8), `strict1` (20 — "error mismatch"), `e_dropview`,
`alterauth2`.

*SQLite's rules* (`../sqlite/src/alter.c`):
* `ALTER TABLE … DROP COLUMN` refuses when the column is a PRIMARY KEY, part
  of a UNIQUE constraint, indexed, referenced by a partial index, the target
  of a generated column, or when the table has a CHECK/FK dependency —
  `sqlite3AlterDropColumn` (alter.c) + the "cannot drop column" error text.
* `ALTER TABLE … RENAME COLUMN` must rewrite the column in every index, view,
  trigger and CHECK constraint, and must fail with
  `error in index <idx> after drop column: no such column: <old>` style
  messages when a dependent object cannot be rewritten.
* STRICT tables (`strict1`) reject the ALTER with
  `cannot ALTER TABLE ... in strict mode` / type violations.

*Change*: `internal/execddl/` (the ALTER implementation) — implement the
drop-column dependency analysis and the rename-column rewrite of indexes,
views, triggers and CHECK clauses; `internal/rename/` already handles the
table-rename dependency graph and is the model to follow.

*Verification*: `FRIGOLITE_TEST=alterdropcol2|altercol|alterlegacy|strict1`
(Go's `-run` takes one pattern — run them one at a time or use
`FRIGOLITE_TEST=alter`).

#### W6.2 Errors that must be raised (322 leaves / 49 families)

*Symptoms*: "expected error but got success" in `notify1` (122 — see W6.4),
`vtab1` (25), `quota` (19), `rtreeA` (13), `fkey2` (13), `savepoint` (11),
`with2` (8), `incrblob` (7), `view` (5), `uri2` (5), `journal2` (5),
`e_walhook` (5), `e_wal` (7) + 35 more.

The single most frequent shape in the whole log is:

```
frigolite_harness_test.go:3178: expected error but got success
  sql: ALTER TABLE x1 DROP COLUMN c
```

(286 leaves) — the engine accepts an `ALTER TABLE … DROP COLUMN` that SQLite
rejects. That is **W6.1** (the ALTER rules), not a separate item.

*Method*: for each family, take the failing SQL from the log
(`grep -a -A5 "FAIL: TestSQLiteSuite/<fam>/" /tmp/frigolite-full-suite18.log`),
run it through `sqlite3` (the oracle) and through frigolite, and diff the
error text. Then implement the missing rule in the responsible `internal/`
package. The families that share a rule are handled **once**:

| rule | families |
|---|---|
| the ALTER TABLE rules (DROP/RENAME COLUMN, STRICT) | alterdropcol2, altercol, alterlegacy, altertab, strict1, alterauth2, e_dropview |
| the constraint / error-text rules | check, conflict3, upsert1, fkey2, fkey4, fkey7, view, with2, savepoint, incrblob |
| the vtab rules | vtab1, vtab3, vtab7, vtabH, rtreeA, rtreedoc, rtreedoc2 |
| the WAL / lock rules | notify1, e_wal, e_walhook, journal2, uri2 |

*Change*: mostly `internal/exec*` (constraint evaluation order, `WITH`
recursion limits, savepoint semantics) and `internal/vtab` (`vtab1`).

*DoD*: each of the 49 families is green or has a native replacement.

#### W6.3 Result-set / error-text parity (220 leaves / 32 families)

*Symptoms*: "error mismatch" in `func` (26), `strict1` (20),
`rtreedoc2` (19), `without_rowid3` (16), `check` (13), `conflict3` (11),
`vtab3` (10), `upsert1` (8), `trustschema1` (8), `alterlegacy` (9),
`rtreefuzz001` (8), `queryonly` (7) + 18 more.

The most frequent shape is the ALTER family (205 leaves):

```
frigolite_harness_test.go:3178: error mismatch
  sql: ALTER TABLE x1 RENAME COLUMN t TO ttt;
```

*Method*: the error *text* must match SQLite's exactly (the harness compares
the message). Diff against the oracle, then fix the message in the producing
package. The rules cluster into: the ALTER family (W6.1), the STRICT-table
type rules (`strict1`: a STRICT table rejects a value whose type does not
match the column's declared type — `cannot store TEXT value in INTEGER
column`), the WITHOUT ROWID rules (`without_rowid3`), the CHECK-constraint
error text (`check`), the UPSERT conflict target (`upsert1`), the
`trusted_schema` rules (`trustschema1`) and the `PRAGMA query_only` rules
(`queryonly`).

#### W6.4 WAL, locking, shared cache, VFS (≈800 leaves)

| family | leaves | root cause | where |
|---|---|---|---|
| `e_walckpt` | 266 | the `testvfs` wrapper VFS is missing; the checkpoint counters are read through it | `internal/pager` (a VFS abstraction) + the harness's `register_test_funcs` |
| `pager1` | 212 | `faultsim_delete_and_reopen` resets the DB at the wrong point → "database is locked" | the harness's `reset_db` step + `internal/exec/locks.go` |
| `notify1` | 122 | shared cache + `sqlite3_unlock_notify` (the test expects a *failure* today) | `internal/lockreg` (the cross-connection lock registry) |
| `multiplex` | 108 | the multiplex VFS + `multiplex_control` | `internal/pager` |
| `wal2` | 99 | "table t1 has 2 columns but 1 values were supplied" → a lost INSERT (W4) + the WAL fixture lifecycle | the converter + `internal/pager` |
| `wal6`/`walbak`/`walvfs` | 61 | "named database db2 is not open" → the ATTACH lifecycle | the harness's `open_named_db`/`close_named_db` |
| `wal`/`walmode`/`malloc*`/`lookaside` | 49 | "collation destructor count = , want N" → the destructor counter | the harness's collation bookkeeping |
| `snapshot*` | 41 | "cannot commit - no transaction is active" / "database disk image is malformed" | `internal/pager` (the snapshot API) |
| `pagerfault` | 28 | "table t1 already exists" → a lost reset | the harness's reset step |
| `rtree1`/`rtreeJ`/`snapshot3` | 70 | "cannot start a transaction within a transaction" → a lost COMMIT/ROLLBACK | the converter's transaction modelling |

**Method for all of W6.4**: the WAL families are one family per feature
(`PORTPLAN.md` G7 lists them). Fix them in the order
`shared cache → unlock-notify → the WAL lifecycle → the VFS wrappers`, each
with a native test that pins the behaviour (the engine's own
`internal/pager` tests plus a `frigolite_wal_test.go`).

#### W6.9 The recursive CTE materializes every row (memory, ≈2.2 GB)

**Symptom**: the suite's resident set reaches tens of gigabytes; the peak
lives in the `sort3` family (and `sort`, `func` follow).

**Evidence** (`GODEBUG=gctrace=1` on the sort families):

```
gc 89  @7.1s  240->242->145 MB
...
gc 101 @14.7s 1955->1975->1410 MB      <- live heap grows to 1.4 GB
gc 102 @19.4s 2752->2753->95 MB        <- released when the CTE ends
```

The growth window is exactly `sort3-3` (`testdata/sort3.json`):

```sql
PRAGMA cache_size = 20000;
WITH r(x,y) AS (
  SELECT 1, randomblob(1000)
  UNION ALL
  SELECT x+1, randomblob(1000) FROM r
  LIMIT 2200000
)
SELECT count(*), sum(length(y)) FROM r GROUP BY (x%5);
```

**Root cause**: `internal/execquery/select_cte_exec.go:iterateRecursiveCTE`
accumulates every produced row in `allRows` and only then hands the whole
set to the outer query. 2.2 M rows × 1 KB blobs ≈ **2.2 GB live** (plus the
copies the planner makes → the multi-GB peaks). SQLite never materializes the
recursive result in the heap: the recursive table is an *ephemeral table*
(`OP_OpenEphemeral`, `sqlite3/src/select.c`) and the outer aggregate streams
it.

**Fix** (SQLite's approach, not a cap):

1. materialize the recursive CTE into a **temp table** (the pager's temp
   store) instead of a Go slice — the rows then live in the page cache with a
   bounded footprint, and the outer query scans the temp table;
2. the recursive *queue* stays in memory (SQLite's queue holds only the rows
   pending expansion — for a linear recursion that is one row ✓);
3. `LIMIT`/`OFFSET`/dedup keep working against the temp table.

**DoD**: `sort3-3` completes with a bounded resident set (target < 500 MB for
the family); the suite's peak RSS drops from >20 GB to < 3 GB.

**Interim mitigation (landed)**: the harness sets a soft memory limit
(`debug.SetMemoryLimit`, default 6 GB, `FRIGOLITE_MEM_LIMIT` in MiB) and calls
`debug.FreeOSMemory()` after every family, so a family's freed heap returns to
the OS immediately instead of accumulating across the corpus (the sort
families' peak dropped from 2.77 GB to 2.07 GB).

#### W6.5 C-API parity (≈45 leaves)

`schema` (27 — the result code), `columncount` (6), `close` (3), `bind2` (2),
`misc4` (4), `mutex2` (1), `notify2` (1), `rtree8` (1), `func5` (1).

The harness models the C API through step types (`prepare_stmt`, `stmt_step`,
`stmt_reset`, `stmt_finalize`, `sqlite3_exec`). The failures are in the
*result-code* and *column-name* reporting:

* `schema`: `want SQLITE_ERROR, got SQLITE_ROW` → `sqlite3_step` must return
  `SQLITE_ERROR` for a schema change mid-statement (the "schema cookie"
  re-prepare rule, `../sqlite/src/vdbe.c`).
* `columncount`: `want "1 a b c", got "stmt_1"` → the column *names* must come
  from the statement, not the handle.
* `close`: `want SQLITE_OK, got SQLITE_ROW` → the final `sqlite3_step` must
  return `SQLITE_DONE`.

---

### W7 — Harness model gaps (the harness must speak the testfixture's dialect)

| gap | families (leaves) | what to build |
|---|---|---|
| `list_all_views` | `e_dropview` (14) | mirror `list_all_triggers` (already implemented): list every view in every schema, prefixed |
| the collation destructors | `multiplex` (108), `wal` (32) | the destructor counter must be reported (`collation destructor count = , want SQLITE_OK`) |
| the TCL procs | `mit` (11), `swap_int32` (5), `rnode` (2), `force_rollback` (5), `myrandomblob` (2), `cksum` (1), `offrec` (1), `joinx cross` (1), `nondeter` (1), `external` (1), `sql_error` (1), `selectH_counter` (2), `::runsql` (2) | implement each proc in the harness's TCL-proc registry (`register_test_funcs` / the proc table), pinned by a native test |
| the named-DB lifecycle | "named database db2 is not open" (62 leaves over 8 families) | the `open_named_db`/`close_named_db` steps must open the *named* database and keep it open for the following steps |
| `PRAGMA lock_proxy_file` | 481 leaves (notify1) | implement the pragma (shared-cache proxy files) |
| the missing SQL functions | `half` (10), `row_number` (7), `test_auxdata`/`test_eval`/`test_extract` (9), `sql` (12), `f` (7), `val` (4), `hex8` (2), `uppercaseconversionfunctionwithaverylongname` (4) | implement in `internal/function` (or the extension surface they belong to) |
| the missing vtabs | `stat` (7), `schema` (10), `vars` (13) | `internal/vtab`: the `stat1`/`stat4`/`schema`/`pragma` vtabs |
| the collations | `text_collate` (1), `backwards_collate` (1), `nocase_collate` (1), `string compare` (3+1) | the harness's collation registration must support arbitrary TCL comparator bodies |
| the misc | `vacuum-into`/`vacuum_into` (11 — "output file already exists"), `e_reindex` (9), `errmsg` (7), `trace2` (12), `rowvalue4`/`rowvalue3` (10), `bestindex9` (6), `bestindexE` (6) | one item each |

**Method**: for each gap, add the harness/engine capability, pin it with a
native Go test in `frigolite_*_test.go`, then re-run the family.

---

### W8 — The long tail (129 families, ≤3 leaves each)

**Procedure** (per family — the discipline that has kept this work honest):

1. Extract the failure from the log:
   `grep -a -A5 "FAIL: TestSQLiteSuite/<fam>/" /tmp/frigolite-full-suite18.log`.
2. Write a **pure-Go** test that drives the engine directly and exercises the
   same behaviour (`frigolite_<fam>_test.go`). Run it.
3. If it fails → the engine is wrong → fix the engine → re-run.
4. If it passes → the fixture/converter is wrong → fix the converter → re-run
   the family.
5. Only if the behaviour is a *testfixture* artefact (not SQLite behaviour) may
   the TCL test be marked obsolete — and then only with a named native test
   that covers the same functional aspect (root 2).

**Throughput**: the tail is ~130 families × ~20 min = ~2 days of focused work.
It is the last phase and must not be rushed: a rushed tail is how coverage
gets descoped.

---

## 3. The full register (305 families → work item)

Machine-generated from the run-18 log; regenerate with:

```bash
python3 tools/status parity-register /tmp/frigolite-full-suite18.log
```

| family | leaves | dominant error | work item |
|---|---|---|---|
| round1 | 9,356 | query_assign mismatch | **W2** |
| rtree2 | 2,010 | query_assign mismatch | **W3** |
| e_walckpt | 266 | runtime variable mismatch | W6.4 |
| pager1 | 212 | database is locked | W6.4 |
| btree01 | 211 | no such table: t1 | **W1** (+W4) |
| notify1 | 122 | expected error but got success | W6.4 |
| where | 118 | query_assign mismatch | W3 |
| multiplex | 108 | collation destructor count | W6.4 / W7 |
| wal2 | 99 | column count mismatch | W4 / W6.4 |
| temptable2 | 75 | no such table: tx | **W1** (+W4) |
| conflict | 65 | query_assign mismatch | W3 |
| conflict2 | 64 | query_assign mismatch | W3 |
| rtree1 | 54 | cannot start a transaction | W6.4 / W4 |
| e_droptrigger | 53 | runtime variable mismatch | W7 (`list_all_triggers`) |
| collate4 | 51 | query_assign mismatch | W3 |
| where9 | 46 | query_assign mismatch | W3 |
| badutf2 | 41 | sqlite3_exec result mismatch | W8 (the `$sql` substitution) |
| memdb | 41 | query_assign mismatch | W3 |
| snapshot | 34 | cannot commit / malformed | W6.4 |
| wal | 32 | collation destructor count | W6.4 / W7 |
| pagerfault | 28 | table t1 already exists | W6.4 (reset) |
| schema | 27 | C-API result mismatch | W6.5 |
| analyze3 | 26 | query_assign mismatch | W3 |
| descidx1 | 26 | query_assign mismatch | W3 |
| func | 26 | error mismatch / no such function | W6.3 / W7 |
| vtab1 | 25 | expected error but got success | W6.2 |
| e_blobopen | 24 | runtime variable mismatch | W6.2 (blob I/O) |
| e_expr | 23 | query_assign mismatch | W3 |
| wal6 | 23 | no such table: t1 | **W1** / W6.4 |
| where7 | 20 | query_assign mismatch | W3 + the stmt-status counters |
| strict1 | 20 | error mismatch | W6.1 |
| triggerC | 20 | triggers nested too deep | W6.2 |
| bestindexF | 20 | runtime variable mismatch | W7 (the `bestindex` callback) |
| quota | 19 | expected error but got success | W6.2 / W7 |
| rtreedoc2 | 19 | error mismatch | W6.3 |
| descidx2 | 18 | query_assign mismatch | W3 |
| walbak | 21 | no such table: t1 | **W1** / W6.4 |
| func2 | 17 | query_assign mismatch | W3 |
| walvfs | 17 | no such table: t1 | **W1** / W6.4 |
| rtreedoc | 16 | runtime variable mismatch | W7 (`rnode` proc) |
| without_rowid3 | 16 | error mismatch | W6.3 |
| win32longpath | 15 | named database db3 is not open | W7 (named DBs) |
| e_dropview | 14 | unsupported Tcl query list_all_views | W7 |
| pragma | 14 | named result mismatch | W8 |
| bestindex5 | 13 | query_assign mismatch | W3 |
| e_fts3 / fts3auto / fts3first / fts3defer2 | 11 | unsupported TCL proc `mit` | W7 |
| vacuum-into / vacuum_into | 11 | output file already exists | W8 |
| loadext | 10 | no such function: half | W7 |
| vtab2 | 10 | no such table: schema | W7 (the `schema` vtab) |
| bestindexA | 9 | runtime variable mismatch | W7 |
| e_reindex | 9 | unable to identify the object | W8 |
| e_uri | 9 | runtime variable mismatch | W8 |
| exclusive / altercons2 | 13 | no such table: abc | W8 |
| vtabH | 13 | no such table: vars | W7 |
| trace2 | 12 | no such function: sql | W7 |
| wal3 | 8 | runtime variable mismatch | W6.4 |
| errmsg | 7 | step: near "messagee" | W8 |
| pushdown | 7 | no such function: f | W7 |
| rowvalue4 | 7 | index already exists | W4 / W8 |
| stat | 7 | no such table: stat | W7 |
| values | 7 | no such function: row_number | W7 |
| bestindex9 | 6 | declare_vtab: syntax error | W7 |
| bestindexE | 6 | invalid command name | W7 |
| columncount | 6 | C-API result mismatch | W6.5 |
| skipscan5 | 6 | no such collation sequence | W7 |
| eval | 5 | no such function: test_eval | W7 |
| fkey6 | 5 | malformed database schema | W8 |
| icu | 5 | wrong number of arguments to lower() | W8 |
| rowid | 5 | incomplete input | W8 |
| rtreecheck | 5 | unsupported proc swap_int32 | W7 |
| tkt-f777251dc7a | 5 | unsupported proc force_rollback | W7 |
| e_blobclose | 4 | no such function: val | W7 |
| fts3malloc | 4 | no such table: ft | W8 |
| misc4 | 4 | C-API result mismatch | W6.5 |
| selectC | 4 | no such function (long name) | W7 |
| sharedA | 4 | no such index: i1 | W6.4 |
| close | 3 | C-API result mismatch | W6.5 |
| dbstatus | 3 | trigger already exists | W8 |
| e_vacuum | 3 | btree relocate error | W8 |
| fkey4 | 3 | FOREIGN KEY constraint failed | W6.2 |
| fts3defer | 3 | unknown tokenizer: icu | W8 |
| indexedby | 3 | no such table: v2 | W8 |
| insert | 3 | runtime expression error | W3 (the evaluator) |
| insert4 | 3 | no such table: src | W8 |
| orderby5 | 3 | unsupported collation comparator | W7 |
| rowvalue3 | 3 | index already exists | W4 / W8 |
| schema2 | 3 | no such trigger: abc_trig | W8 |
| tkt-cbd054fa6b | 3 | no such function: test_extract | W7 |
| tkt1644 | 3 | no such table: temp_v1 | W8 |
| tpch01 | 3 | runtime variable mismatch (EQP) | W8 |
| vtab7 | 3 | no such table: log | W7 |
| bind2 | 2 | C-API result mismatch | W6.5 |
| e_select2 | 2 | unrecognized token | W8 |
| misc6 | 2 | no such function: hex8 | W7 |
| mutex1 | 2 | collation destructor count | W6.4 |
| nulls1 | 2 | no such table: t5 | W8 |
| quote | 2 | no such table: @abc | W8 |
| selectH | 2 | unsupported proc selectH_counter | W7 |
| temptrigger | 2 | no such trigger: tr1 | W8 |
| tkt-b72787b1 / tkt_b72787b1 | 4 | unsupported proc `::runsql` / no such function | W7 |
| tkt-f7b4edec | 2 | runtime variable mismatch | W7 |
| upfrom3 | 2 | UNIQUE constraint failed | W6.2 |
| upfrom4 | 2 | no such table: t5 | W8 |
| vacuum5 | 2 | unknown database x2 | W8 |
| walckptnoop | 2 | unsupported proc myrandomblob | W7 |
| zipfilefault | 2 | duplicate name | W8 |
| alterauth2 | 1 | error in view v1 | W8 |
| analyze9 | 1 | no such table: t3 | W8 |
| autoindex1 | 1 | runtime variable mismatch | W8 |
| collate1 | 1 | no such table: collate1t1 | W8 |
| collate2 | 1 | unsupported collation comparator | W7 |
| collate5 | 1 | unsupported collation comparator | W7 |
| collate6 | 1 | unsupported collation comparator | W7 |
| delete4 | 1 | UNIQUE constraint failed | W6.2 |
| exec | 1 | sqlite3_exec result mismatch | W8 |
| fkey7 | 1 | C-API result mismatch | W6.5 |
| fts-9fd058691 | 1 | no such table: fts | W8 |
| fts3cov | 1 | table xx already exists | W8 |
| fts3expr4 | 1 | no such function: fts3_exprtest | W8 |
| func5 | 1 | no such function: counter2 | W8 |
| func6 | 1 | unsupported proc offrec | W7 |
| func8 | 1 | unsupported proc joinx cross | W7 |
| having | 1 | unsupported proc nondeter | W7 |
| join | 1 | catch result mismatch | W8 |
| join2 | 1 | use DROP TABLE to delete table | W8 |
| mutex2 | 1 | C-API result mismatch | W6.5 |
| normalize | 1 | prepare: unrecognized token | W8 |
| notify2 | 1 | C-API result mismatch | W6.5 |
| pragma5 | 1 | unsupported proc external | W7 |
| quota2 | 1 | runtime variable mismatch | W8 |
| regexp2 | 1 | unsupported proc sql_error | W7 |
| rtree8 | 1 | C-API result mismatch | W6.5 |
| schema4 | 1 | view v1 already exists | W8 |
| shared2 | 1 | cannot rollback | W6.4 |
| sort3 | 1 | unsupported proc cksum | W7 |
| tkt-26ff0c2d1e | 1 | C-API result mismatch | W6.5 |
| tkt-80ba201079 | 1 | runtime expression error | W3 |
| tkt-f3e5abed55 | 1 | database aux already in use | W8 |
| tkt2686 | 1 | table filler already exists | W8 |
| tkt3838 | 1 | no such table: main.T1 | W8 |
| tkt_99378177930f87bd | 1 | no such index | W8 |
| tkt_cbd054fa6b | 1 | no such table: vvv | W8 |
| tkt_f973c7ac31 | 1 | table t already exists | W8 |
| tokenize | 1 | near "*": syntax error | W8 |
| update2 | 1 | query results differ | W8 |
| vacuum2 | 1 | no such collation sequence | W7 |
| vtab_alter | 1 | table t1_base already exists | W8 |
| walsetlk_recover | 1 | runtime variable mismatch | W6.4 |
| existsexpr / analyzeG | 2 | no such table: main.t2 | W8 |
| sort2 / minmax / cursorhint | 6 | no such table: main.t1 | W8 |
| fts3query | 6 | illegal first argument to matchinfo | W8 |
| misc1 | 6 | no such table: t8 | W8 |
| shell8 | 6 | write fixture file | W8 |
| rtreecheck | 5 | unsupported proc swap_int32 | W7 |

*(the register is machine-generated — the table above is the authoritative
mapping; regenerate it after every baseline run)*

---

## 4. Execution order, procedure and stop rules

### 4.1 The phases

```
Phase A (unblock the corpus)          Phase B (engine parity)        Phase C (tail)
──────────────────────────────────     ────────────────────────       ──────────────────
W0  guards                             W6.1 ALTER TABLE               W8  the 129-family
W1  restore the query steps            W6.2 missing errors                 tail, one family
W2  string command                     W6.3 error-text parity             at a time
W3  do_test body semantics             W6.4 WAL / locks / VFS
W4  one statement per step             W6.5 C-API parity              W7  (the harness-model
W5  regenerate + batch-verify          W7  harness model gaps              gaps fold into
                                                                          whichever phase
                                                                          they surface in)
```

Phase A is a *prerequisite*: W1 restores ~174k assertions, W2/W3 correct
~13k wrong expectations. Doing W6/W7 before Phase A means fixing failures
that do not exist once the expectations are right.

### 4.2 The batch procedure (unchanged, and binding)

1. Convert to a scratch dir: `mkdir -p /tmp/regenN && go run ./tools/tclconvert/ -outdir /tmp/regenN`.
2. Install with the batch driver — **verify first, install second, revert on
   regression**.
3. **A build failure counts as "no result", never as "0 failures".**
4. Never edit the engine while a batch runs.
5. Read the regression list against the *old fixture's* actual leaf names
   before reverting (the leaf-name normalization produces false regressions
   when the naming convention changed).

### 4.2b The per-item procedure (the seven steps every item follows)

```
1. NAME     pick the family from the register (§3) and write its item down
            (symptom, expected behaviour, the SQLite source pointer)
2. PROBE    write a pure-Go test that drives the engine directly
            (frigolite_<name>_test.go) and reproduces the behaviour
            - probe fails  -> the ENGINE is wrong  -> go to 3
            - probe passes -> the FIXTURE/CONVERTER is wrong -> go to 5
3. ENGINE   implement the rule in the responsible internal/ package,
            following SQLite's own source (../sqlite/src) — no simplification
4. PIN      the probe stays as a regression test; it must fail without the fix
5. CONVERT  regenerate the family's fixture
            (go run ./tools/tclconvert/ -testdir <dir> -outdir <dir>)
6. BATCH    install through the batch driver (verify -> install -> revert on
            regression) and read the regression list against the OLD fixture's
            leaf names before reverting anything
7. CLOSE    the family is green (or green with a named native replacement and a
            compliant obsolete record); record the evidence in §6
```

Rules that make the seven steps work:

* **The oracle is SQLite, not the fixture.** When the engine and the fixture
  disagree, run the same SQL through `sqlite3` (and the same Tcl through
  `tclsh`) before changing anything.
* **No try/fail loop.** If the second attempt does not work, stop and write a
  dedicated fix plan (GUIDELINES §1) — do not iterate blindly.
* **No simplification.** The fix must have the same fundamental approach as
  SQLite's; a shortcut that passes the test but diverges from SQLite is a new
  bug.
* **Never edit the engine while a batch or a suite run is in flight.**
* **A build failure is "no result", never "0 failures".**

### 4.3 Stop rules

* **Do not** respond to a growing red set after W1 by re-dropping the step —
  the growth is the *truth* being revealed. Triage it under W6/W7.
* **Do not** grow the skip lists. If a family must be excluded, root 2
  applies: a named native test must cover the same functional aspect.
* **Do not** weaken an expectation to match the engine. If the engine is
  wrong, fix the engine; if the *expectation* is wrong, fix the converter and
  regenerate the fixture.
* Every closed item lands in §6 with the run log that proves it.

### 4.4 Risk register

| risk | impact | mitigation |
|---|---|---|
| restoring the dropped `query` steps grows the red set (restored assertions now fail) | the baseline number gets worse before it gets better | expected and *not* a reason to re-drop the step; every newly visible failure becomes a W6/W7 item |
| a converter change alters many fixtures at once | a large diff, hard to attribute | the batch procedure (verify → install → revert on regression) keeps the corpus green; the pin tests keep the converter's behaviour attributable |
| the batch's revert rule keeps stale fixtures installed | a permanent coverage hole (C5) | the reverts are a *work queue*: each reverted family must end up installed-fresh with its failures fixed (W5 amendment) |
| the "regression" signal includes false positives (the leaf naming convention changed) | a good fixture is reverted | read the regression list against the old fixture's actual leaf names before reverting |
| the suite peaks at ~7.4 GB RSS | an OOM kill (`signal: killed`) loses the measurement | run the suite **alone** (never concurrently with a conversion); run 19 was lost this way |
| the fixture model has no conditional step | TCL's dynamic control flow is lost (rtree2's remainder) | W3.5 adds a `condition` field evaluated by the harness |
| a stale expectation looks like an engine bug | wasted days | the discipline: a pure-Go probe first, then the oracle (`sqlite3`/tclsh), then the engine |

### 4.5 Resource bounds

* The full suite is ≈55 min. Run it with `-p 1` and a 5400 s timeout, **alone**
  (a concurrent converter run once pushed the machine over its memory limit
  and the suite was killed with `signal: killed`).
* **Memory policy (landed 2026-10-21)**: the harness sets a soft memory limit
  (`debug.SetMemoryLimit`, default **3 GB**; `FRIGOLITE_MEM_LIMIT` in MiB, `0`
  disables it) and calls `debug.FreeOSMemory()` after every family.
  * The limit bounds the Go **heap**; the resident set can still exceed it,
    because on macOS the runtime releases freed pages with
    `madvise(MADV_FREE)` — the pages stay resident until the kernel reclaims
    them, so `debug.FreeOSMemory()` does **not** shrink the RSS (measured:
    4.81 GB before and after a 2.5 GB alloc/free cycle).
  * Measured effect of the 3 GB limit: the corpus-wide RSS peak fell from
    >8 GB (measured) to ≈4.4 GB and the growth became gradual instead of
    step-wise.
  * The remaining lever is the live heap of the heaviest family (W6.9).
* The remaining live-heap driver is **W6.9** (the recursive CTE materializes
  2.2 M rows of 1 KB blobs in `sort3-3` → ~1.4 GB live). Until it is fixed,
  the suite's peak is ≈4–5 GB.
* The converter's statement budget keeps a single file from ballooning; raise
  it per file rather than globally (see W5.3).
* Measure a family's memory with the *prebuilt* binary — `go test` compiles
  first, and the compiler's own footprint (1–3 GB) pollutes the measurement:

  ```bash
  go test -c -o /tmp/frigolite.test .
  FRIGOLITE_TEST=<family> /tmp/frigolite.test -test.run '^TestSQLiteSuite$' -test.count=1
  ```

  and use the exact family name: `FRIGOLITE_TEST=sort` matches every family
  whose name *contains* "sort" (sort, sort2…sort5, sortfault).

---

## 4b. The schedule (how the work is sequenced)

The work is ordered by *dependency*, not by size. A session takes one work
item to completion (fix → pin test → convert → batch → measure) and never
leaves a half-applied change behind.

| phase | items | dependency | exit signal |
|---|---|---|---|
| A1 | **W0** guards | — | an unknown step type / `string` subcommand fails loudly |
| A2 | **W1** restore `query` | W0 | `btree01` = 0, `date4`/`json106`/`corruptI` assert |
| A3 | **W2** the `string` family | W0 | `round1` = 0 |
| A4 | **W3 + W3.5** the body value + conditional steps | W1 | `rtree2` = 0, `where`/`conflict`/`collate4`/`memdb` = 0 |
| A5 | **W4** one statement per step | A4 | no multi-statement step outside a trigger body |
| A6 | **W5** regenerate + install the whole corpus | A1–A5 | 0 stale fixtures |
| B1 | **W6.2/6.3** the 71 error-parity families | A6 | each family green or natively covered |
| B2 | **W6.1** ALTER TABLE (DROP/RENAME COLUMN, STRICT) | B1 | alterdropcol2/altercol/alterlegacy/strict1 = 0 |
| B3 | **W6.4** WAL / locks / shared cache / VFS | B1 | e_walckpt/pager1/notify1/multiplex/wal2 = 0 |
| B4 | **W6.5** the C-API result codes | B1 | schema/columncount/close/bind2 = 0 |
| B5 | **W7** the harness model gaps | B1 | the missing procs/queries/pragmas/collations exist |
| C | **W8** the 129-family tail | B1–B5 | 0 red leaves |

**Effort shape** (from the measured data): A1–A3 ≈ 1 session (done), A4 ≈ 1–2
sessions, A5 ≈ 0.5, A6 ≈ 1 (mechanical, but each reverted family needs its own
triage), B1 ≈ 3–4 (71 families), B2 ≈ 1.5, B3 ≈ 3–4 (the WAL cluster is a
feature, not a patch), B4 ≈ 1, B5 ≈ 2, C ≈ 8–10 (one family ≈ 20 min).

**The rule that keeps this honest**: no item closes without a *green test* (or
a named native replacement + a compliant obsolete record). The corpus-wide
number is a symptom; the per-family green is the deliverable.

---

## 5. The verification command list

```bash
# a single family
FRIGOLITE_TEST=btree01 go test . -run '^TestSQLiteSuite$' -count=1

# the converter's own tests
go test ./tools/tclconvert/... -count=1

# the harness/engine unit + architecture tests
go test . -run 'TestSOLID_|TestHarnessStepTypes|TestExpandTCLRuntimeVars' -count=1

# the whole suite (the authoritative baseline)
go test -p 1 ./... -count=1 -timeout 3600s 2>&1 | tee /tmp/frigolite-full-suite<N>.log
grep -a -o "FAIL: TestSQLiteSuite/[^ ]*" /tmp/frigolite-full-suite<N>.log \
  | sed 's/ (.*//' | sort -u > /tmp/run<N>-fails.txt

# the quality gate (before every commit)
./tools/quality_gate.sh && go vet ./... && gofmt -l internal tools *.go
```

---

## 6. Progress log

| date | item | evidence |
|---|---|---|
| 2026-10-20 | the plan of record created (`PARITY_PLAN.md` §0 roots, §4b register) | `/tmp/run16-fails.txt` |
| 2026-10-20 | the run-18 baseline measured: 14,431 leaves / 305 families | `/tmp/frigolite-full-suite18.log` |
| 2026-10-20 | **F1 found**: the harness has no `case "query"` → 173,795 steps silently dropped across 768 families; `btree01`'s `t1` is never created | `git show HEAD:frigolite_harness_test.go` vs the working tree; the fixture census |
| 2026-10-20 | **F2 found**: `cmdString`'s unknown-subcommand fallback returns its first argument → `string trimright` returns the input → `round1`'s expectations are wrong (9,356) | `tools/tclconvert/tcl/interp.go:1229`; `ori/sqlite/test/round1.test:28` |
| 2026-10-20 | **F3 found**: `cmdDoTest` attaches the expectation to the last captured statement → the rows are compared with a scalar (`rtree2` 2,010) | `tools/tclconvert/tcl/interp_sql.go:735`; `ori/sqlite/test/rtree2.test:48` |
| 2026-10-20 | the per-family error clustering and the full register produced | this document, §1.1 and §3 |
| 2026-10-20 | **W0 landed**: the harness's step switch has a `default:` arm that fails loudly on an unknown step type; the audit proves `query` was the *only* unhandled type (173,795 steps / 768 families) | `frigolite_harness_test.go`; the step-type census |
| 2026-10-20 | **W1 landed**: `case "query":` restored (executes the statement and compares the rendered rows). `btree01` 211 → **0**, `temptable2` 75 → **3**, `date4`/`json106`/`corruptI` now actually assert their 24,859 / 40,000 / 50,004 statements | `FRIGOLITE_TEST=btree01 go test . -run '^TestSQLiteSuite$'` |
| 2026-10-20 | **W2 landed**: `string trimleft/trimright/last/cat/reverse/wordstart/wordend` implemented against the Tcl oracle; the unknown-subcommand fallback now raises `UnsupportedCommandError` (fatal for the file) and a failing command substitution aborts the conversion | `tools/tclconvert/tcl/interp_string.go`, `interp.go`; `string_trim_test.go` |
| 2026-10-20 | batch #6 (`/tmp/regen20`, 60 families): 14 kept · 44 reverted · 2 new-family; **round1 9,356 → 0** (kept, no regressions) | `/tmp/regen20-batch.jsonl` |
| 2026-10-20 | **W3 landed**: the do_test expectation now follows the *body's value*; `set X [expr {...}]` becomes a runtime expression that stores its value; the variable holds every row (`resultRows`). `rtree2` 2,010 → 1,513, the rest of the cluster (where/conflict/collate4/memdb/…) re-measured | `dotest_body_value_test.go`; `/tmp/regen21b-batch.jsonl` |
| 2026-10-20 | batch #7 (`/tmp/regen21`, 57 families, with the fixed harness): 12 kept · 45 reverted · 459 leaves fixed in the kept families | `/tmp/regen21b-batch.jsonl` |
| 2026-10-20 | **W3.5 identified**: the fixture model has no *conditional* step, so a TCL `catch` + `if/else` loses the branch condition (rtree2's remaining 1,513) | §W3.5 |
| 2026-10-21 | **N2.2–N2.4 landed**: the runtime-conditional step model — a `catch` around a captured statement is a runtime result (`catchResult` + the result code in a variable), an `if`/`else` on a runtime variable captures **both** branches with a `condition` guard, and the harness evaluates the condition. The branch's conversion-time side effects are rolled back (both scopes) so the loop unrolling still advances. **rtree2 1,513 → 102** | `conditional_step_test.go`; the rtree2 measurement |
| 2026-10-21 | **N1 landed**: SQLite's statement-status counters (`SQLITE_STMTSTATUS_FULLSCAN_STEP` / `_SORT`) are implemented (engine counters, the `db status step|sort` capture, the `stmtStatus` fixture flag) and pinned. The counters now match SQLite (a five-row full scan counts 4). where7's remaining 1,933 leaves differ **only** in the counters → where7 is the **planner's OR-optimization**, not the counters (new item P.1–P.4 in `portplan/TODOS.md`) | `frigolite_stmtstatus_test.go`; the where7 diff analysis |
| 2026-10-20 | **memory**: the suite's RSS reached >20 GB. Root cause found: `sort3-3` runs a 2.2M-row recursive CTE with 1 KB blobs and the engine materializes every row in the heap (1.4 GB live, `gctrace` evidence). Interim mitigation landed (a soft memory limit + `FreeOSMemory` per family); the real fix (an ephemeral temp table for the recursive CTE) is **W6.9** | `GODEBUG=gctrace=1`, the heap profile, §W6.9 |
