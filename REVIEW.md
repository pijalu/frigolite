# Frigolite — Gap Analysis & Fix Plan

> Review scope: (1) bugs, incorrect Go paradigms, anti-patterns; (2) incorrect SQLite
> reimplementation / missing SQL dialect; (3) performance & algorithmic complexity.
>
> **Methodology**: every defect marked ✅ was reproduced against the live engine
> (`go run` against `github.com/pijalu/frigolite`). No claim is inferred from code reading alone.
> Test harnesses used to confirm each item are referenced inline.

Severity legend: 🔴 **CRITICAL** (engine-breaking / data loss / crash) · 🟠 **HIGH** (wrong results / core feature missing) · 🟡 **MEDIUM** (edge correctness / perf / idiom) · 🔵 **LOW** (polish).

---

## 0. Headline findings

| # | Finding | Severity | Verified |
|---|---------|----------|----------|
| 1 | B-tree has **no page splitting** → tables hold ~148 rows then INSERT fails | 🔴 | ✅ |
| 2 | **Lowercase/mixed-case SQL fails to parse** | 🔴 | ✅ |
| 3 | **Multi-row `INSERT … VALUES (..),(..)` silently drops rows** | 🔴 | ✅ |
| 4 | **GROUP BY parsed but never executed** | 🔴 | ✅ |
| 5 | `util.GetVarint` **panics** on truncated/malformed input | 🔴 | ✅ |
| 6 | Integer division wrong (`7/2`→`3.5`, SQLite→`3`) | 🟠 | ✅ |
| 7 | NULL three-valued logic broken | 🟠 | ✅ |
| 8 | No constraint enforcement (NOT NULL / UNIQUE / PK / CHECK / FK) | 🟠 | ✅ |
| 9 | `O(n²)` hot paths: insert-rowid-scan, ORDER BY, UPDATE, DELETE | 🟡 | ✅ |
| 10 | Indexes created but never populated/used | 🟠 | ✅ |

Internal packages `btree`, `exec`, `function`, `pager`, `schema`, `sql`, `vtab` have **zero unit tests**; only `util` and `storage` are tested. The 778-file "compat" suite only *executes* SQL and logs results — it does **not** assert correctness.

---

## Part A — Bugs & correctness defects

### A1. 🔴 B-tree never splits (data capacity capped at one page)

**Where**: `internal/btree/btree.go:296` `InsertCell`; `DeleteCell`/`DeleteCellsWhere` are leaf-only.

`InsertCell` returns `btree: page is full (need X, have Y)` when a leaf overflows. There is **no interior-page creation, no split, no sibling chaining**. `Cursor.Next()` (`btree.go:243`) explicitly comments *"for now we just mark as end"* — it cannot traverse beyond the first leaf.

**Repro**: insert loop fails at row 149; `COUNT(*)` returns 148.
```
INSERT 149 FAILED: btree: page is full (need 287, have 306)
```
**Impact**: the engine is unusable for any table beyond ~1 page. This single defect invalidates most of the "SQLite-compatible database" claim.

### A2. 🔴 Keywords are case-sensitive

**Where**: `internal/sql/lexer.go:347` — `if _, ok := keywords[word]; ok`. `keywords` only contains UPPER-CASE keys, and `readIdent` preserves the original case. So `select`, `Select`, `SELECT` → only the last is recognized; the others become `TokenIdentifier`.

Because `parser.parseKeywordStmt` switches on the raw `p.cur.Value`, **any non-uppercase keyword is a parse error**.
```
create table t(a integer) → parse error: unexpected token: 2 (create)
```
SQLite identifiers and keywords are case-insensitive by spec.

### A3. 🔴 Multi-row INSERT drops all but the first tuple

**Where**: `internal/sql/parser.go:459-466` `parseInsertSource`:
```go
for p.cur.Type == TokenComma {
    p.next()
    if p.cur.Type == TokenLParen {
        p.next()
        p.parseExprList()   // parsed then DISCARDED
        p.expect(TokenRParen)
    }
}
```
`INSERT INTO m VALUES (1),(2),(3)` → `Changes=1`, `COUNT(*)=1`. Every extra tuple is parsed and thrown away. (The AST `InsertStmt.Values` is a flat `[]Expr`, which also cannot represent multiple tuples.)

### A4. 🔴 GROUP BY is never executed

**Where**: `internal/exec/engine.go:733` `execSelect` never reads `s.GroupBy`; `evalAggregates` collapses **all** rows into a single group.
```
SELECT grp, SUM(val) FROM g GROUP BY grp   →  [[a 6]]   (all rows summed, no grouping)
```
`HAVING` is likewise ignored (parsed only).

### A5. 🔴 `util.GetVarint` panics on malformed input

**Where**: `internal/util/varint.go:56`. The loop indexes `buf[n]` with no bounds check and no 9-byte cap:
```go
func GetVarint(buf []byte) (uint64, int) {
    for { v = (v<<7)|uint64(buf[n]&0x7f); n++; if buf[n-1]&0x80==0 { break } }
}
```
**Repro** (`internal/util` test): `GetVarint([]byte{0x80})` → `panic: runtime error: index out of range [1] with length 1`.

Every cell/record/page decode calls this without pre-validation (`storage.DecodeCell`, `DecodeRecord`, `seekInPage`, …). A single truncated or corrupt byte → **process crash**. Also: SQLite varints are max 9 bytes; this loop is unbounded.

### A6. 🟠 Integer division returns REAL

**Where**: `internal/exec/engine.go:1838` `divValues` always promotes to `float64`. SQLite applies **integer division** when both operands are INTEGER (`7/2 == 3`, `7.0/2 == 3.5`). Confirmed `SELECT 7/2` → `3.5`.

### A7. 🟠 NULL / three-valued logic is broken

**Where**: `evalBinaryOpValues` (`engine.go:1594`) and `toBool`. Comparisons return a Go `bool`, losing the "unknown" state.
```
WHERE a = NULL     → returns [NULL]   (SQLite: empty — NULL never equals)
WHERE a <> 1       → [NULL, 3]        (SQLite: [3] — NULL excluded)
WHERE NOT (a = 1)  → [NULL, 3]        (SQLite: [3])
```
`||` concatenation with NULL returns `"a<nil>"` instead of NULL (`concatValues` uses `fmt.Sprintf("%v%v")`). SQLite: `NULL || x == NULL`.

### A8. 🟠 No constraint enforcement

**Where**: `execInsert` / `insertRow` never inspect `colDefs[].NotNull/Unique/PrimaryKey`. Verified:
```
INSERT NULL into NOT NULL col  → ok (should error)
duplicate UNIQUE insert        → ok, two rows stored
duplicate PRIMARY KEY          → ok
CHECK (a>0) violated           → ok
```
Only the **UPSERT** path (`findRowByUniqueCols`) checks UNIQUE, and only there. FOREIGN KEY references are parsed and discarded. Column-count mismatch on INSERT is silently padded with NULL.

### A9. 🟠 Indexes are inert

**Where**: `execCreateIndex` (`engine.go:161`) ends with `// TODO: populate index from existing table data`. The index b-tree root page is allocated but never written to; no insert path maintains it; `execSelect` never consults indexes. `SeekToRowID`/`SeekToKey` exist in the b-tree but the engine never calls them — every query is a full table scan.

**Related**: `parseDropIndex` (`parser.go:1039`) mis-builds a `DropTableStmt{Name:"index"}` then overwrites the name — semantically wrong type.

### A10. 🟠 GLOB operator & function broken

- Operator: `evalBinaryOpValues` has no `GLOB`/`REGEXP`/`MATCH` case → `WHERE s GLOB 'h*'` returns nothing.
- Function: `fnGLOB` uses `args[0]` as the string and `args[1]` as the pattern; SQLite's `GLOB(pattern, string)` is the reverse, and it returns `nil` (not `false`) for NULL input.

### A11. 🟠 Other correctness defects (all verified)

| Defect | Location | Symptom |
|--------|----------|---------|
| UNION ALL dedups; INTERSECT/EXCEPT behave like UNION | `mergeUnionRows` `engine.go:802` | `UnionAll` flag parsed, never used |
| ALTER TABLE is a no-op | `execAlterTable` returns `&Result{}` | RENAME/ADD/DROP do nothing |
| INTEGER PRIMARY KEY auto-value wrong after explicit ids | `findNextRowID` | after `id=5`, next auto = 3 (not 6) |
| Negative LIMIT → empty (SQLite = no limit) | `applyLimitOffset` | `LIMIT -1` → `[]` |
| JOIN doesn't merge right-table columns | `execSelect` ignores `s.Joins` for row map | `b.val` → `nil` |
| Subquery in FROM unsupported | parser | `SELECT * FROM (…)` → parse error |
| `PRAGMA table_info(t)` ignores argument | `pragmaHandlers` take no table arg | returns empty |
| `COUNT(DISTINCT x)` parse error | parser/aggregate | not supported |
| `random()` is deterministic | `fnRANDOM` `function.go:358` | returns a constant |

### A12. 🟡 Robustness / bounds (panic risk)

Beyond A5, the storage layer slices with unchecked offsets: `storage.CellPointer`, `decodeTableInteriorCell` (`data[off:off+4]`), `ParsePage`. A malformed page number, cell pointer, or record header crashes instead of returning an error.

### A13. 🟡 `frigodb` placeholder interpolation is unsafe w.r.t. literals

**Where**: `frigodb/driver.go:342` `interpolateArgs` scans the **raw query** char-by-char for `?`, including `?` **inside string literals and comments**. `INSERT INTO t VALUES ('a?b')` mis-substitutes the `?` inside the literal. (String *values* are escaped via doubled quotes, so classic injection is mitigated, but the literal scan is still wrong.) The lexer already emits `TokenParam` — this path ignores it entirely.

---

## Part B — Performance & algorithmic complexity

All measured on the live engine (Apple Silicon). Scaling factors confirm the asymptotics.

### B1. 🟡 `findNextRowID` → O(n²) inserts
**Where**: `engine.go:1752`, called on **every** insert (`insertRow`, `execInsertSelect`, `execInsertDefault`). It opens a cursor and scans the whole table to find `max(rowid)+1`.
```
30 inserts → 7.7 µs/insert ;  100 → 11.1 ;  140 → 14.1   (per-insert cost doubles with n)
```
**Fix direction**: maintain `sqlite_sequence`-style max-rowid in the schema/root page; or track per-table high-water in the table entry.

### B2. 🟡 `schema.nextRowID` → O(total rows in DB)
**Where**: `schema.go:251`. On **every** `AddEntry` it opens a cursor over **every table** and scans **all rows** to compute a global max rowid. Creating N schema objects is O(N · totalRows).

### B3. 🟡 ORDER BY uses bubble sort → O(n²)
**Where**: `engine.go:1135` `sortRowsWithMaps` (nested `for i… for j…`). Should use `sort.SliceStable`. (Measured ~0.5 ms even at 140 rows because the constant is tiny, but the asymptotic is n².)

### B4. 🟡 `DeleteCellsWhere` → O(n²) on a page
**Where**: `btree.go:422`. Loop: find **one** matching cell, delete it (shifts all later pointers), then **restart the scan from index 0**, re-parsing the page each iteration. `DELETE FROM t` (all rows) is O(n²).

### B5. 🟡 UPDATE is collect-then-delete+insert → O(m·n)
**Where**: `engine.go:1205` `collectUpdateChanges` (O(n) scan) + `applyUpdateChanges` which, **per changed row**, calls `DeleteCellsWhere` (full re-scan from start). UPDATE of m of n rows ≈ O(n + m·n).
```
UPDATE 30 rows → 113 µs ;  60 → 346 µs ;  100 → 896 µs   (~8× for 3.3× rows)
```

### B6. 🟡 Repeated re-parsing / no caching
- `parseColumnDefs` (`engine.go:1775`) **re-parses the CREATE TABLE SQL string on every query**. Column defs should be cached on the schema `Entry`.
- `Cursor.ReadCell`/`Next` **re-parse the page header** (`storage.ParsePage`) on every call.
- No prepared-statement cache; every `Exec`/`Query` re-lexes + re-parses.
- `execSelectView`/`virtualTableRows`/`fireTrigger` re-parse SQL via `strings` manipulation each time.

### B7. 🟡 Memory & allocation
- Pager `map[uint32]*Page` has **no eviction** — the whole DB stays resident.
- `buildRowMap` allocates a fresh `map[string]interface{}` per row per scan; `copyRowMap` clones it again for ORDER BY.
- DISTINCT/UNION dedup keys rows via `fmt.Sprintf("%v", row)` — O(len) per row, and formatting-sensitive (see B8).

### B8. 🟡 DISTINCT/UNION correctness-vs-perf
`distinctRows`/`mergeUnionRows` key on `fmt.Sprintf("%v", row)`. This is both slow and semantically fragile: `[int64(1)]` vs `[float64(1)]` format identically (`"1"`), so `1` and `1.0` can collapse (or not, depending on Go's formatting) inconsistently with SQLite's type-coercion rules. Needs a structural/hashable row key.

### B9. 🔵 Concurrency
`pager.Pager` is mutex-protected, but `exec.Engine`, `schema.Manager`, and the b-tree layer carry **no locks**. Two goroutines sharing one `*DB` race on the engine/schema maps and on in-place page mutation. The public API documents nothing about (thread-)safety.

---

## Part C — Go idioms & anti-patterns

| # | Issue | Where |
|---|-------|-------|
| C1 | Public API returns `*Result` (pointer) carrying `.Error` instead of `(Result, error)`. Callers must remember to check `.Error`; success/error are not type-distinguishable. | `frigolite.go` |
| C2 | `Exec` runs **all** statements in the string; `Query` runs only the **first**. Inconsistent, surprising. | `frigolite.go:106,135` |
| C3 | `Save()` always returns "not yet supported" for `:memory:`; `FileExists` is an unrelated exported helper — leaky surface API. | `frigolite.go:168,186` |
| C4 | Errors silently swallowed: `rowMatchesWhere` returns `false` on eval error; aggregate evals use `val, _ := e.evalExpr(...)`. | `engine.go:1268,948` |
| C5 | `internal/transaction/` is an **empty package** (no `.go` files) yet appears in the SOLID layer map. Dead weight. | repo |
| C6 | `fnRANDOM` returns a deterministic constant — misnamed, not random, no `math/rand` use. | `function.go:358` |
| C7 | `parseDropIndex` reuses `DropTableStmt` and overwrites `.Name` — wrong concrete type for downstream dispatch. | `parser.go:1039` |
| C8 | `EvalNumber` (LIMIT/OFFSET) parses via `ParseInt` then falls back to `ParseFloat`+`int64()` truncation; negative limit semantics wrong (A11). | `parser.go:1611` |
| C9 | String-typed dispatch for triggers: `fireTrigger` matches events with `strings.Contains(upper, " "+event+" ")` on the SQL text — fragile (table/column names containing event words misfire). | `engine.go:674` |
| C10 | `*BTree`/`*Engine` are recreated per-operation (`btree.NewBTree(...)` in many call sites) instead of cached; no cursor pooling. | throughout `exec` |
| C11 | TODO/FIXME left in shipped code (`// TODO: populate index…`, `// for now we just mark as end`). | `engine.go:209`, `btree.go:243` |
| C12 | Several `_ =` discarded values and commented-out branches (against the repo's own "Source Cleanup Guidelines"). | various |

---

## Part D — Missing SQLite SQL dialect

Verified-missing or broken (vs README's "Full SQL subset" claim):

| Feature | Status | Note |
|---------|--------|------|
| Case-insensitive keywords/identifiers | 🔴 broken | A2 |
| GROUP BY / HAVING execution | 🔴 missing | A4 |
| Multi-row VALUES | 🔴 broken | A3 |
| JOIN column resolution | 🟠 broken | A11 |
| Subquery in FROM | 🟠 missing | A11 |
| Constraints (NN/UQ/PK/CHECK/FK) | 🟠 missing | A8 |
| Integer division semantics | 🟠 wrong | A6 |
| NULL 3-valued logic | 🟠 wrong | A7 |
| GLOB / REGEXP / MATCH operators | 🟠 missing | A10 |
| `COUNT(DISTINCT …)` | 🟠 missing | A11 |
| Page splitting / multi-page tables | 🔴 missing | A1 |
| Indexes (create/populate/use) | 🟠 missing | A9 |
| COLLATE application | 🟡 parsed only | stored, never used in `CompareValues` |
| Type affinity on INSERT | 🟡 missing | values stored as-is; no TEXT↔INTEGER coercion |
| `WITH` (CTE, recursive) | 🟡 missing | documented as unsupported |
| Window functions (`OVER`/`PARTITION`) | 🟡 missing | keywords tokenize, no eval |
| `FILTER (WHERE …)` on aggregates | 🟡 missing | keyword tokenizes, no eval |
| Date/time fns (`date`,`time`,`datetime`,`strftime`,`julianday`) | 🟡 missing | — |
| `RETURNING` | 🟡 missing | — |
| Generated columns | 🟡 missing | — |
| UPSERT `WHERE` on conflict target | 🟡 missing | parser comment acknowledges it |
| `LIKE … ESCAPE` | 🟡 missing | — |
| `last_insert_rowid()` / `changes()` | 🟡 missing | no surface for last rowid |
| PRAGMA write side-effects / `table_info(target)` | 🟠 partial | handlers ignore arguments |
| Savepoints / nested transactions | 🔵 no-op | parsed, not implemented |
| ATTACH / VACUUM / REINDEX | 🔵 no-op | parsed, not implemented |

---

## Part E — Test & architecture gaps

- **E1.** `internal/{btree,exec,function,pager,schema,sql,vtab}` have **no unit tests**. Only `util` and `storage` do. The engine's most complex logic (parser, executor, b-tree) is tested only indirectly.
- **E2.** `frigolite_sqlite_compat_test.go` (778 tests, ~2 MB) only **executes** statements and logs; it asserts **nothing**. Passing green does not mean correct. README's "842 tests" overstates coverage.
- **E3.** No fuzzing on the lexer/parser/varint/record decoders — exactly the panic-prone surfaces (A5, A12).
- **E4.** `internal/transaction` empty package is wired into the SOLID layer map (C5).
- **E5.** `frigodb` driver round-trips via string interpolation rather than binding — bypasses the parser's `TokenParam`, so the two are untested against each other (A13).

---

## Part F — Fix plan (micro-steps)

Phased so each step is independently testable and mergeable. **Every step ends with "add/extend a test that fails before and passes after."** Order respects dependencies (e.g. varint hardening before b-tree split work that stresses it).

### Phase 0 — Stop-the-bleed (crashes & parse failures)  *(do first)*

**F0.1 Harden `util.GetVarint`** 🔴
- a. Add `len(buf)` bounds check + 9-byte cap to the decode loop; return `(0, n, err)` (change signature to return `error`, or return `(val, n)` with `n` capped and let callers validate).
- b. Audit **every** caller (`storage.DecodeCell`, `DecodeRecord`, `seekIn*`, `findInsertPosition*`) to handle short buffers.
- c. Fuzz test: `FuzzGetVarint` feeding random/truncated bytes — must never panic.
- *Verify*: the in-`util` test that currently panics now passes.

**F0.2 Case-insensitive keywords & identifiers** 🔴
- a. In `readIdent`, uppercase the word for the `keywords` map lookup **only**; keep original case as `Token.Value` for identifiers (SQLite preserves identifier case).
- b. Make parser keyword comparisons case-insensitive (add `isKeyword(tok, "SELECT")` helper that upper-cases), OR normalize keyword token values to UPPER at lex time.
- c. Add parser tests: `select`, `SeLeCt`, `SELECT`, `create table`, mixed-case identifiers.
- *Verify*: `/tmp/verify_bugs.go` BUG 1 now succeeds.

**F0.3 Bounds-check storage decoders** 🔴 (depends on F0.1)
- a. `ParsePage`, `CellPointer`, `decodeTableInteriorCell`, `decodeIndex*Cell`: validate offsets against `len(data)` before slicing; return `error` not panic.
- b. Fuzz `storage.DecodeRecord` and `DecodeCell`.

### Phase 1 — Core SQL correctness (HIGH, small diffs)

**F1.1 Integer division** 🟠
- a. In `divValues`: if both operands classify as INTEGER (`isInt`), perform integer division (`int64 / int64`); else float. Match SQLite: divide-by-zero → NULL.
- b. Test: `SELECT 7/2`→`3`, `7.0/2`→`3.5`, `7/0`→`NULL`.

**F1.2 NULL three-valued logic** 🟠
- a. Change comparison results from `bool` to a tri-state (`true/false/unknown`). Simplest: return `nil` for "unknown" and propagate; `toBool(nil)`→false; `AND/OR` use Kleene logic.
- b. `evalBinaryOpValues`: if either side NULL → unknown for `=,<,>,…`; `IS NULL`/`IS NOT NULL` stay boolean.
- c. `NOT`, `AND`, `OR` implement Kleene: `NULL AND false`=false, `NULL OR true`=true, else unknown.
- d. `concatValues`: if either nil → nil.
- e. Tests from A7 (expect empty/[3]/NULL).

**F1.3 Multi-row INSERT** 🔴
- a. Change `InsertStmt.Values []Expr` → `[][]Expr` (list of tuples).
- b. Fix `parseInsertSource` to collect every `(...)` tuple (remove the discard loop).
- c. Loop over tuples in `execInsert`/`insertRow`, incrementing `Changes`.
- d. Test: `VALUES (1),(2),(3)` → `Changes=3`, count 3.

**F1.4 Negative/zero LIMIT semantics** 🟡
- a. `applyLimitOffset`: `limit < 0` → no limit; `offset < 0` → 0; `limit==0` → empty.
- b. Tests for `-1`, `0`, offset > rows.

### Phase 2 — Constraints & rowid semantics

**F2.1 Constraint enforcement** 🟠
- a. After building the value vector in `insertRow`, validate against `colDefs`: NOT NULL (value nil → error), CHECK (eval expr, false → error), UNIQUE/PK (scan/lookup → error on dup). Return a `Result{Error}`.
- b. Map SQLite constraint names to error codes/text ("NOT NULL constraint failed: …").
- c. Tests: each constraint type, plus column-count mismatch error.

**F2.2 INTEGER PRIMARY KEY = rowid alias** 🟠
- a. Detect single `INTEGER PRIMARY KEY` column at CREATE TABLE; treat it as the rowid alias (do not store a duplicate column; `SELECT id` returns rowid).
- b. Replace `findNextRowID` full-scan with a per-table high-water mark stored in the schema entry / `sqlite_sequence`; bump on insert; honor `AUTOINCREMENT`.
- c. Tests: auto-assign, explicit-then-auto (A11 case → 6 not 3), AUTOINCREMENT monotonicity.

### Phase 3 — B-tree multi-page support (the big one) 🔴

This is the largest work item; decompose strictly.

**F3.1 Cursor sibling traversal**
- a. Implement `Cursor.Next()`/`Prev()` to follow leaf sibling pointers (right/left pointers in interior pages, or re-seek via parent stack).
- b. Maintain a parent-page stack in `Cursor` for navigation.
- c. Test: insert >1 page of rows, full scan returns all in order.

**F3.2 Leaf split**
- a. On `InsertCell` overflow: allocate a new leaf, move ~half the cells, insert the median key + left-child into the parent (interior) page; create a root interior page when splitting the root, updating the stored root page number.
- b. Handle page-1 (header offset) correctly in all new paths.
- c. Test: insert 10k rows, `COUNT(*)`==10000, ordered scan correct, file reopenable.

**F3.3 Interior delete / rebalance**
- a. `DeleteCell` on underflow: borrow from sibling or merge; update interior keys; handle root collapse.
- b. Test: insert 5k then delete all, `COUNT(*)`==0, page count shrinks.

**F3.4 Free-page list**
- a. On delete/merge, return freed pages to the header free list; `AllocatePage` reuses them.
- b. Update header `DatabaseSize`/`TotalFreelist`.
- c. Test: churn insert/delete, page count bounded.

### Phase 4 — Execution features (GROUP BY, JOIN, subqueries)

**F4.1 GROUP BY + HAVING** 🔴
- a. In `execSelect`, after the WHERE scan, partition `rowMaps` by the GROUP BY expression values (ordered key).
- b. Evaluate aggregates **per group**; apply HAVING as a post-group filter.
- c. Test: A4 case → `[[a 3][b 3]]`; HAVING filters groups.

**F4.2 JOIN execution** 🟠
- a. Build a combined row map keyed by `table.col` and unqualified `col`; nested-loop join over all `s.Joins` applying `ON`.
- b. Support INNER/LEFT (RIGHT/CROSS/NATURAL can follow).
- c. Resolve `SELECT a.name, b.val` and `*` expansion across joined tables.
- d. Tests: A11 JOIN case; LEFT JOIN NULLs.

**F4.3 Subquery in FROM** 🟠
- a. Extend `parseTableRef` to accept `(SELECT …) [AS alias]`; store as a subquery table source.
- b. `execSelect`: materialize the subquery rows, expose columns under the alias.
- c. Test: `SELECT * FROM (SELECT 1 AS x)`.

**F4.4 DISTINCT/UNION correctness + perf** 🟡
- a. Replace `fmt.Sprintf("%v")` keys with a structural hashable representation honoring `CompareValues` semantics (so `1` and `1.0` compare equal per SQLite affinity).
- b. Honor `UnionAll` (no dedup); implement INTERSECT/EXCEPT set semantics.
- c. Tests: UNION ALL keeps dups; INTERSECT/EXCEPT correct.

### Phase 5 — Indexes 🟠

**F5.1 Index population & maintenance**
- a. On `CREATE INDEX`, scan the table and insert index cells (encoded key + rowid) into the index b-tree.
- b. Keep indexes in sync on INSERT/UPDATE/DELETE (maintain a list of indexes per table in the schema).
- c. `DROP INDEX` via correct `DropIndexStmt` type (fix C7).

**F5.2 Index-based query plans**
- a. Nausea: for `WHERE col = ?`/`col IN …`/`ORDER BY col` where a matching index exists, use `SeekToKey` then `SeekToRowID` to fetch rows instead of full scan.
- b. Tests: query plan uses index; correctness vs full-scan; perf comparison.

### Phase 6 — Performance (after correctness) 🟡

**F6.1 Kill O(n²) hot paths**
- a. F2.2 already removes `findNextRowID` scan; also remove `schema.nextRowID` global scan (use a schema-rowid counter).
- b. Replace bubble sort with `sort.SliceStable` (F-ordering test must still pass).
- c. `DeleteCellsWhere`: collect matching indices in one pass, delete in descending order (single shift pass) → O(n).
- d. `applyUpdateChanges`: build a rowid→newValues map, single delete+rebuild pass.

**F6.2 Caching**
- a. Cache parsed column defs on `schema.Entry` (parse once at CREATE).
- b. Cache parsed page header in `Cursor`/`BTree` for the current page.
- c. Add a prepared-statement cache keyed by SQL text in `Engine`.
- d. LRU bound the pager page map.

**F6.3 Allocation reduction**
- a. Reuse row-map buffers across a scan; avoid `copyRowMap` unless needed (ORDER BY).
- b. Benchmark before/after (`benchmarks/`).

### Phase 7 — Idioms, API, dialect polish 🔵

**F7.1 API hygiene**
- a. Consider `(Result, error)` return or at least document the `*Result` contract; make `Exec`/`Query` consistent on multi-statement handling (C1, C2).
- b. Remove/stub `Save()` properly; move `FileExists` out of the public API or document it.
- c. Add a `LastInsertRowID()` accessor.

**F7.2 frigodb binding**
- a. Drive placeholder substitution through the lexer (`TokenParam`) + a bind map instead of raw-string scan (fixes A13). Optionally add real parameter binding to the executor.

**F7.3 Missing operators/functions (pick by priority)**
- a. GLOB/REGEXP/MATCH operators in `evalBinaryOpValues`; fix `fnGLOB` arg order.
- b. `COUNT(DISTINCT x)`; aggregate `FILTER`.
- c. Date/time functions.
- d. `COLLATE` application in `CompareValues`; type affinity on INSERT.
- e. `LIKE … ESCAPE`.

**F7.4 Cleanup**
- a. Remove `internal/transaction` empty package (or implement it); update SOLID map.
- b. Fix `parseDropIndex` type; replace string-based trigger matching with stored structured trigger metadata.
- c. Clear TODOs/FIXMEs or convert to tracked issues.
- d. Make `random()` actually random (`math/rand`).

### Phase 8 — Testing backbone

**F8.1 Unit tests for every internal package** (unblocks all above)
- a. `sql`: lexer (case, comments, blobs `x'…'`, params) + parser golden tables per statement type.
- b. `btree`: insert/split/delete/rebalance/seek with property tests.
- c. `exec`: per-statement executor tests with asserted rows (not just "no error").
- d. `function`: each scalar/aggregate.
- e. `pager`/`schema`: round-trip + concurrency.

**F8.2 Turn the compat suite into assertions**
- a. Add expected-result columns to a subset of `frigolite_sqlite_compat_test.go` (or a new curated file) and assert equality, so green means correct.

**F8.3 Fuzzing**
- a. `go test -fuzz` targets for varint, record, page, and the full lexer/parser.

---

## Appendix — Verification reproductions

Each was run against the current tree (`go1.22`, default page size):

```
# A1 page-split        : insert loop fails @149; COUNT=148
# A2 case              : create table t(a) → "unexpected token: 2 (create)"
# A3 multi-row INSERT  : VALUES (1),(2),(3) → Changes=1, COUNT=1
# A4 GROUP BY          : SELECT grp,SUM(val)…GROUP BY grp → [[a 6]]  (no grouping)
# A5 varint panic      : GetVarint([]byte{0x80}) → panic index OOR
# A6 int division      : SELECT 7/2 → 3.5
# A7 NULL logic        : WHERE a=NULL→[NULL]; WHERE a<>1→[NULL,3]
# A8 constraints       : NN/UQ/PK/CHECK all accepted-violations
# A10 GLOB             : WHERE s GLOB 'h*' → []
# A11 ALTER TABLE      : RENAME TO b; SELECT * FROM b → []
# B1/B5 scaling        : per-insert 7.7→14.1µs (30→140); UPDATE 113→896µs (30→100)
```
Harnesses: `/tmp/verify_bugs{,2,3,4,5}.go`, `/tmp/verify_perf.go`, `/tmp/verify_compat*.go`,
plus an in-package `internal/util` panic test.
