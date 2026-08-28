# Grammar Completeness Map — SQLite LALR(1) Parser

> **Source**: `internal/parse/parser.go` handleRule() + `internal/parse/sql_tables.go`
> **Spec**: `/Users/muaddib/dev/sqlite/src/parse.y` (417 production rules)
> **Measurement**: `TestRuleInventory` over 35,653 SQL statements from testgen corpus

---

## 1. Summary

| Metric | Count |
|--------|-------|
| Total rules in LALR tables | 412 |
| Fired (reachable from corpus) | 282 |
| Unfired (not reached by corpus) | 130 |
| Passthrough (fired, no handler, non-empty RHS) | 10 |
| handleRule cases implemented | 261 |
| Cases returning nil (incl. legit empty rules) | 71 |

**Interpretation**: "Unfired" does NOT mean "broken" — it means the rule's specific
production wasn't exercised by the 35K-statement corpus. Many unfired rules are
alternative spellings of the same construct (e.g., 5 joinop variants for INNER/
LEFT/RIGHT/FULL/CROSS join — only the ones used in the corpus fired).

The real risk is the **10 passthrough rules** — these fire but produce no AST,
silently corrupting the parse for multi-symbol productions.

---

## 2. The 10 Passthrough Rules (HIGH PRIORITY — cause test failures)

These rules fire during real parses but have no `handleRule` case (or return nil),
causing the first RHS symbol to be silently passed through. For multi-symbol
rules, this drops all components after RHS #1.

| Rule | nrhs | Grammar Production | Feature Area | Impact | Sample Input |
|------|------|--------------------|-------------|--------|--------------|
| 2 | 1 | `cmdx ::= cmd` | top-level | LOW (single symbol, passthrough OK) | any statement |
| 14 | 1 | `createkw ::= CREATE` | DDL marker | LOW (single symbol) | CREATE TABLE/INDEX/VIEW |
| 133 | 2 | `seltablist ... NOT INDEXED` | FROM clause | MEDIUM — NOT INDEXED hint lost | `SELECT * FROM t1 NOT INDEXED` |
| 231 | 2 | `expr ::= CASE ... END + expr` | expressions | MEDIUM — CASE+arithmetic precedence | `SELECT case when 1 then 99 else ? end + ?` |
| 277 | 3 | `trigger_decl ::= temp TRIGGER ...` | triggers | HIGH — trigger declarations may lose structure | `CREATE TRIGGER ... WHEN ...` |
| 302 | 1 | `create_vtab ::= ... USING nm` | virtual tables | MEDIUM — vtab module name | `CREATE VIRTUAL TABLE ... USING fts4` |
| 348 | 1 | (cmd passthrough for INSERT...SELECT) | DML | LOW (single symbol) | `INSERT INTO t3 SELECT * FROM t2` |
| 349 | 2 | `cmdlist ::= cmdlist ecmd` | multi-statement | MEDIUM — statement chaining | `BEGIN; INSERT...; COMMIT` |
| 351 | 1 | `ecmd ::= SEMI` | empty statement | LOW (single symbol) | `; DELETE FROM parent` |
| 352 | 2 | `ecmd ::= cmdx SEMI` | statement terminator | LOW — already handled by rule 2 passthrough | multi-statement scripts |

### Passthrough priority order:
1. **Rule 277** (trigger_decl) — highest; triggers are widely tested (triggerA–G, fkey, view)
2. **Rule 133** (NOT INDEXED) — affects WHERE/EQP tests
3. **Rule 231** (CASE+expr) — affects expr/cse/select tests
4. **Rule 302** (create_vtab) — affects FTS/vtab tests
5. **Rules 349/352** (multi-statement chaining) — affects most multi-statement tests
6. **Rules 2, 14, 348, 351** (single-symbol) — low risk, likely fine as passthrough

---

## 3. Unfired Rules by Grammar Area

### 3a. Window Functions — MAJOR GAP (15+ unfired rules)

The entire window-function grammar is largely unexercised:

| Rule(s) | Nonterminal | Production | Notes |
|---------|------------|------------|-------|
| 161, 163 | windowdefn (271) | `nm AS LP window RP` variants | Named window definitions |
| 168 | frame_opt (274) | `range_or_rows frame_bound_s frame_exclude_opt` | RANGE/ROWS frame |
| 169 | frame_opt (274) | `range_or_rows BETWEEN ... AND ... frame_exclude_opt` | BETWEEN frame |
| 170 | frame_opt (274) | (5-symbol window variant) | |
| 171 | frame_opt (274) | (8-symbol window variant) | |
| 172 | filter_over (275) | `filter_clause over_clause` composition | FILTER + OVER |
| 266, 267 | over_clause (291) | `OVER LP window RP`, `OVER nm` | OVER clause |
| 270 | window_clause (289) | `WINDOW windowdefn_list` | Named window clause |
| 312, 313 | frame_bound variants (307) | frame boundary rules | |
| 321, 323, 324 | window/frame (311) | window assembly rules | |
| 326 | frame_exclude_opt (312) | `EXCLUDE frame_exclude` | EXCLUDE NO OTHERS etc |
| 337, 338, 339 | filter_clause (321) | `FILTER LP WHERE expr RP` | FILTER clause |
| 390, 391, 394 | frame_bound (287) | `expr PRECEDING/FOLLOWING`, `CURRENT ROW` | frame bounds |
| 411 | window (311) | additional window rule | |

**Status**: Window functions (P5.WINDOW goal) need full grammar + runtime. Currently
minimal `OVER()` support exists (cast-9.0 test).

### 3b. CREATE TABLE constraints — FK / generated columns (8 unfired rules)

| Rule(s) | Production | Notes |
|---------|------------|-------|
| 32, 34, 35 | `ccons ::= DEFAULT ...` variants | DEFAULT with +/- scantok |
| 37 | `ccons ::= CHECK LP expr RP` | column-level CHECK |
| 42 | `ccons ::= defer_subclause` | FK deferrable |
| 43 | `ccons ::= ...` | constraint variant |
| 371, 372 | `ccons ::= ...` | additional constraint rules |

These affect foreign-key, generated-column, and CHECK-constraint tests.

### 3c. JOIN variants (5 unfired rules)

| Rule(s) | Production | Notes |
|---------|------------|-------|
| 55–59 | `joinop ::= JOIN_KW nm JOIN` etc | LEFT/RIGHT/FULL/CROSS join operators |

Only basic INNER and LEFT joins fire. RIGHT/FULL/CROSS join grammar may be
untested. **Note**: The RD parser (`internal/sql/parser.go`) handles these at
runtime, but the LALR path may not.

### 3d. DDL commands (16 unfired rules for `cmd` nonterminal)

| Rule(s) | Feature | Notes |
|---------|---------|-------|
| 12 | `cmd ::= ... ALTER ...` | ALTER TABLE variants |
| 250, 256 | `cmd ::= ... CREATE INDEX ...` | Index creation variants |
| 285, 288–293, 295, 296 | `cmd ::= ... PRAGMA ...` | PRAGMA variants |
| 298–301 | `cmd ::= ... TRIGGER/DROP ...` | Trigger/Drop variants |

Most of these are handled by the RD parser fallback or the engine's PRAGMA/DDL
code paths, but the LALR handler may not produce AST for all of them.

### 3e. CTE / WITH clause (4 unfired rules)

| Rule(s) | Production | Notes |
|---------|------------|-------|
| 309, 310 | `wqlist ::= wqitem`, `wqlist ::= wqlist COMMA wqitem` | CTE list |
| 318, 410 | `wqitem` variants | CTE item with MATERIALIZED |

### 3f. Trigger commands (2 unfired rules for trigger_event)

| Rule(s) | Production | Notes |
|---------|------------|-------|
| 126 | `trigger_event ::= UPDATE OF idlist` | UPDATE OF trigger |
| 127 | `trigger_event` variant | |

### 3g. Virtual tables (3 unfired rules)

| Rule(s) | Production | Notes |
|---------|------------|-------|
| 406 | `vtabarg ::=` (empty) | vtab arg init |
| 407 | `vtabarg ::= vtabarg vtabargtoken` | vtab arg accumulation |
| 408 | `vtabargtoken ::= lp anylist RP` | vtab arg parenthesized |

---

## 4. Grammar → Test Failure Cross-Reference

Rules whose absence causes documented test failures:

| Rule(s) | Grammar Feature | Failing Tests | Fix Goal |
|---------|----------------|---------------|----------|
| 277 | trigger_decl structure | triggerA–G, fkey, view triggers | G3.TRIGGER |
| 133 | NOT INDEXED hint | whereE, whereH (EQP), whereD | G1.WHERE |
| 231 | CASE+expr precedence | expr, cse | G1.EXPR |
| 302 | CREATE VIRTUAL TABLE module | fts*, vtab* | G5.VTAB |
| 168–172, 266–267 | window functions | window* (all fail) | G5.WINDOW |
| 32–43 | column constraints (FK/CHECK/gen) | fkey*, check, gencol | G3.FK, G3.CONSTR |
| 55–59 | join operators | join*, joinF (RIGHT JOIN) | G2.JOIN |
| 309–310 | CTE list | with*, whereL | G5.CTE |
| 298–301 | PRAGMA variants | pragma*, pragmafault | G5.PRAGMA |

---

## 5. Handler Coverage Test

The grammar coverage test (`internal/parse/grammar_coverage_test.go`) asserts that
every multi-symbol rule that fires produces a real AST node (not nil passthrough).

To run:
```bash
go test ./internal/parse/ -run TestGrammarCoverage -count=1 -v
```

To regenerate the full inventory:
```bash
FRIGOLITE_INVENTORY=1 go test ./internal/parse/ -run TestRuleInventory -count=1 -timeout 120s
# Output: /tmp/rule_inventory.txt
```

---

## 6. Recommended Grammar Work Order

1. **G0.1** — Fix the 10 passthrough rules (rules 277, 133, 231, 302 first)
2. **G0.2** — Extend grammar coverage test corpus to exercise unfired rules
3. **Per-feature** — Implement grammar as each feature goal demands:
   - G2.JOIN needs rules 55–59 (join operators)
   - G3.TRIGGER needs rules 277, 126–127 (trigger decl/event)
   - G3.FK needs rules 32–43 (FK constraints)
   - G5.WINDOW needs rules 168–172, 266–267, 390–394 (full window grammar)
   - G5.CTE needs rules 309–310, 318, 410
   - G5.PRAGMA needs rules 285–301
   - G5.VTAB needs rules 302, 406–408
4. **G0.3** — Final grammar coverage: 100% of fired rules have handlers
