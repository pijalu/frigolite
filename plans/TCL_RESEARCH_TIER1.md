# Tier 1 Fix Plan — operational (rewritten 2026-08-01)

**Source of truth for TCL semantics**: `/Users/muaddib/dev/sqlite/test/`
**Package list**: `plans/PACKAGES_TIER1.txt` (58 packages)
**Measured this session** (`go test -tags testgen -count=1 -timeout 90s` per package, parallel sweep):
**27 PASS / 31 FAIL** — identical FAIL set to `plans/HANDOVER_TIER1.md`.

- **PASS (27)**: insert, delete_, update, null, types, coalesce, literal, select2,
  select3, select4, select5, select6, select8, select9, selectB, selectE, selectF,
  selectG*, whereA, whereB, whereC, whereJ, whereK, whereN, delete2, delete4, valuesfault
  (*selectG passes alone in ~145s but times out under parallel load — O(n²) rowid scan, task T1.2)
- **FAIL (31)**: affinity, between, cast, cse, delete3, delete_pkg, expr, intpkey,
  intreal, istrue, nulls, numcast, returning, select1, select7, selectA, selectC,
  selectD, selectH, strict, subtype, values, where, whereD, whereE, whereF, whereG,
  whereH, whereI, whereL, whereM

---

## §0. TASK QUEUE (sequential, small distinct tasks)

One goal per task. **Always update this plan's checkboxes as part of a task's work,
then commit and push.** Verify loop in §3. Root-cause details in §1.

| # | Task | Packages fixed | Kind | Verify command |
|---|---|---|---|---|
| T1.1 | delete3 hang — INTEGER PK conflict full-table scan | delete3 | crash | `go test -tags testgen ./testgen/delete3/ -count=1 -timeout 300s` |
| T1.2 | selectG O(n²) — findNextRowID full scan per insert | selectG | perf | `go test -tags testgen ./testgen/selectG/ -count=1 -timeout 60s` |
| T1.3 | cse bool→0/1 — value model never emits Go bool | cse | engine | `go test -tags testgen ./testgen/cse/ -count=1` |
| T1.4 | whereG braces — strip TCL list braces from expected | whereG | harness | `go test -tags testgen ./testgen/whereG/ -count=1` |
| T1.5 | tcl2go `$var`-in-SQL — bind/quote instead of raw text | numcast, values (partial), between (partial) | harness | `go test -tags testgen ./testgen/{numcast,values,between}/ -count=1` |
| T1.6 | multi-row VALUES compound — all rows, all contexts | values, cast, selectC (part), select1 (part) | engine | `go test -tags testgen ./testgen/{values,cast,selectC}/ -count=1` |
| T1.7 | RETURNING execution — INSERT/UPDATE/DELETE projection | returning | engine | `go test -tags testgen ./testgen/returning/ -count=1` |
| T1.8 | harness custom fns — test_getsubtype, intreal, udf, TCL int/log | subtype, intreal, selectC, between | harness | `go test -tags testgen ./testgen/{subtype,intreal,selectC,between}/ -count=1` |
| T1.9 | engine implies_nonnull_row (func.c INLINEFUNC) | expr | engine | `go test -tags testgen ./testgen/expr/ -count=1` |
| T1.10 | STRICT datatype enforcement + exact error msgs | strict | engine | `go test -tags testgen ./testgen/strict/ -count=1` |
| T1.11 | DQS off — double-quoted tokens in DML are identifiers | select7 | engine | `go test -tags testgen ./testgen/select7/ -count=1` |
| T1.12 | CAST REAL/NUMERIC numeric-prefix parse (sqlite3AtoF) | numcast | engine | `go test -tags testgen ./testgen/numcast/ -count=1` |
| T1.13 | EQP automatic index for inner join table | whereE | engine | `go test -tags testgen ./testgen/whereE/ -count=1` |
| T1.14 | EQP TEMP B-TREE FOR ORDER BY vs index scan | whereH | engine | `go test -tags testgen ./testgen/whereH/ -count=1` |
| T1.15 | EXPLAIN real opcode names (whereF (Lt|Ge) range) | whereF | engine | `go test -tags testgen ./testgen/whereF/ -count=1` |
| T1.16 | ATTACH ':memory:' AS name + schema qualification | selectD | engine | `go test -tags testgen ./testgen/selectD/ -count=1` |
| T1.17 | OR-optimization row order (rowid tables) | whereD | engine | `go test -tags testgen ./testgen/whereD/ -count=1` |
| T1.18 | OR-optimization row order (WITHOUT ROWID / PK order) | whereI | engine | `go test -tags testgen ./testgen/whereI/ -count=1` |
| T1.19 | NOCASE-collated OR join (AAA→aaa) | where | engine | `go test -tags testgen ./testgen/where/ -count=1` |
| T1.20 | column affinity on INSERT + comparisons | whereM | engine | `go test -tags testgen ./testgen/whereM/ -count=1` |
| T1.21 | UNION compound affinity (first SELECT, JOIN USING) | affinity | engine | `go test -tags testgen ./testgen/affinity/ -count=1` |
| T1.22 | collation-aware constant propagation + ON-clause CTE | whereL | engine | `go test -tags testgen ./testgen/whereL/ -count=1` |
| T1.23 | ON-clause view-column resolution (LEFT JOIN view) | select1 | engine | `go test -tags testgen ./testgen/select1/ -count=1` |
| T1.24 | intpkey rowid extremes (MinInt64/MaxInt64) | intpkey | engine | `go test -tags testgen ./testgen/intpkey/ -count=1` |
| T1.25 | DELETE WHERE scalar subquery ORDER BY LIMIT OFFSET | delete_pkg | engine | `go test -tags testgen ./testgen/delete_pkg/ -count=1` |
| T1.26 | istrue BOOLEAN DEFAULT + IS TRUE/FALSE/NOT | istrue | engine | `go test -tags testgen ./testgen/istrue/ -count=1` |
| T1.27 | nulls NULLS FIRST/LAST in ORDER BY | nulls | engine | `go test -tags testgen ./testgen/nulls/ -count=1` |
| T1.28 | selectA compound/CTE + f() helper + ORDER BY validation | selectA | engine+harness | `go test -tags testgen ./testgen/selectA/ -count=1` |
| T1.29 | selectH counter() helper + view-over-compound | selectH | engine+harness | `go test -tags testgen ./testgen/selectH/ -count=1` |

### Status
- [ ] T1.1
- [ ] T1.2
- [ ] T1.3
- [ ] T1.4
- [ ] T1.5
- [ ] T1.6
- [ ] T1.7
- [ ] T1.8
- [ ] T1.9
- [ ] T1.10
- [ ] T1.11
- [ ] T1.12
- [ ] T1.13
- [ ] T1.14
- [ ] T1.15
- [ ] T1.16
- [ ] T1.17
- [ ] T1.18
- [ ] T1.19
- [ ] T1.20
- [ ] T1.21
- [ ] T1.22
- [ ] T1.23
- [ ] T1.24
- [ ] T1.25
- [ ] T1.26
- [ ] T1.27
- [ ] T1.28
- [ ] T1.29

---

## §1. FAILURE → ROOT-CAUSE MAP (all 31 packages)

### A. HARNESS / TRANSPILER bugs (fix in tools/tcl2go or testgen helpers; engine is innocent)

1. **numcast** — `/Users/muaddib/dev/sqlite/test/numcast.test:27-43`
   `db eval {SELECT CAST($str AS real)}` — TCL `$str` is a **bound parameter**
   (string value), so SQLite evaluates `CAST(' 876xyz' AS real)` = `876.0`.
   tcl2go emitted `"SELECT CAST(" + str + " AS real)"` → raw SQL text
   `CAST( 876xyz AS real)` → parse error. **Fix (harness)**: transpile `$var`
   in SQL as a bound parameter or quote it as a string literal. AFTER that,
   the engine still needs REAL/NUMERIC CAST of strings to parse the numeric
   prefix (§1.C numcast second bug).

2. **values** — `/Users/muaddib/dev/sqlite/test/values.test:77-88` (1.2.5/1.2.6)
   `INSERT INTO x1 VALUES(4,4,$a),(5,5,$b),(6,6,$c)` with `set a 4` etc. —
   tcl2go left `$a/$b/$c` as literal text (the `set` statements were skipped),
   engine parses `$a` as a NULL variable → third column of rows 4-6 is `{}`.
   **Fix (harness)**: same `$var`-in-SQL transpilation issue as numcast.

3. **whereG** — `/Users/muaddib/dev/sqlite/test/whereG.test:76,91,106`
   `} {{Mass in B Minor, BWV 232}}` — TCL list rendering wraps the single
   element (contains comma/space) in braces. The Go want string kept the braces
   `{Mass in B Minor, BWV 232}` while `flatten` renders the actual value without
   them. Engine output is CORRECT. **Fix (harness)**: strip TCL list-rendering
   braces from expected values (brace-unwrapping step near
   `normalizeExpectedWord`).

4. **between** — `/Users/muaddib/dev/sqlite/test/between.test:29-33`
   `set x [expr {int(log($i)/log(2))}]` then `execsql {INSERT INTO t1
   VALUES($::w,$::x,$::y,$::z)}`. tcl2go's `tclExprWith` falls back to the raw
   expression when `evalSimpleArith` can't evaluate `int(log(1)/log(2))`, so
   `x` = literal `int(log($i)/log(2))` → SQL error `unknown function: int`.
   **Fix (harness)**: extend `evalSimpleArith`/`tclExprWith` to evaluate TCL
   math functions (`int`, `log`, `pow`, ...) or compute these specific
   expressions at transpile time.

5. **expr** — `/Users/muaddib/dev/sqlite/test/expr.test:1048-1074` (expr-16.100/101/102)
   `SELECT implies_nonnull_row(...)` — registered in the **main engine**
   `src/func.c:3325` (`TEST_FUNC(implies_nonnull_row, 2,
   INLINEFUNC_implies_nonnull_row, 0)`, available unless `SQLITE_UNTESTABLE`).
   The Go engine has no such function. **Fix (engine)**: implement
   `implies_nonnull_row(a,b)` (true iff a implies b, per SQLite expr.c
   `impliesNotNullRow` semantics) OR register it in the harness function table.

6. **subtype** — `/Users/muaddib/dev/sqlite/test/subtype1.test:19,22`
   `SELECT test_getsubtype('hello')` — test-only SQL function from
   `src/test_func.c:623` (test build). **Fix (harness)**: register
   `test_getsubtype`/`test_setsubtype` in the testgen helpers (subtype
   semantics: returns the subtype int, not needed by the engine itself).

7. **intreal** — `/Users/muaddib/dev/sqlite/test/intreal.test:19-42`
   `SELECT intreal(5)` — test-only SQL function from `src/test1.c:981`
   (returns arg as INTEGER type). **Fix (harness)**: register `intreal` in
   helpers.

8. **selectC** — `/Users/muaddib/dev/sqlite/test/selectC.test:215-217` (4.3)
   `proc udf {} { incr ::udf }` + `db function udf udf` — the `udf()` calls
   after a `SELECT DISTINCT a, b FROM t_distinct_bug` subquery. `proc` is not
   transpiled; the generated test lacks `udf()`. **Fix (harness)**: implement
   `udf()` (side-effect counter) in helpers. (selectC-4.2/4.2b `SELECT a FROM
   (SELECT DISTINCT a, b FROM t_distinct_bug)` may have a separate view-column
   issue — re-check after udf.)

9. **selectA (undocumented in original research)** — `selectA.test`
   - 3.98: WITH RECURSIVE over compound (INTERSECT/EXCEPT/UNION) with
     `ORDER BY y COLLATE NOCASE DESC,x,z` inside the subquery → `got [{}]`
     want `MAD MAD+ MAD++` (recursive CTE + compound subquery only returns 1 row).
   - 4.1.3 / 4.2.2: `WHERE f()==f()` / `SELECT c, f(d,c,d,c,d)` — `f()` is a
     test-only helper (always-true) missing from the engine → rows filtered out.
   - 5.4: `UNION ... ORDER BY a+b COLLATE NOCASE` must raise
     `1st ORDER BY term does not match any column in the result set` — engine
     returns no error (compound ORDER BY validation missing).
   - 6.1: `SELECT * FROM (SELECT a FROM t1 UNION SELECT b FROM t2) WHERE a=a`
     → `got []` want `[ABC]` — derived-table column resolution for compound.
   **Fix**: harness `f()` helper + engine compound ORDER-BY validation +
   derived-table/CTE compound column resolution.

10. **selectH (undocumented in original research)** — `selectH.test`
    `counter(1)` — test-only SQL function from `src/test1.c` (returns an
    incrementing counter). Missing helper → engine renders the function AST
    pointer (`&{0x... rtrim}`) instead of a value, and views over
    `UNION ALL ... counter(1)` collapse. **Fix (harness)**: register
    `counter()` in helpers; **Fix (engine)**: view-over-compound expansion
    with `*` + named extra columns (`SELECT c16 AS a, *, counter(1) AS x ...`).

### B. ENGINE bugs — schema / DDL / constraints

11. **strict** — `/Users/muaddib/dev/sqlite/test/strict1.test:21-36`
    `do_catchsql_test strict1-1.1 {CREATE TABLE t1(a) STRICT}` expects error
    `missing datatype for t1.a`; 1.4 expects `unknown datatype for t1.a:
    "BANJO"`. Engine returns no error → STRICT tables are accepted without
    datatype enforcement. **Fix (engine)**: in DDL, when `STRICT` is present,
    require each column to have a recognized datatype and reject unknown ones
    with the exact messages.

12. **select7** — `/Users/muaddib/dev/sqlite/test/select7.test:167-177` (6.6/6.7)
    `sqlite3_db_config db SQLITE_DBCONFIG_DQS_DML 0` then
    `INSERT INTO t1 VALUES (NULL,0,"") ...` must fail with `no such column: ""
    - should this be a string literal in single-quotes?`; count stays 0. The
    db_config line is not transpiled, and the engine treats `"..."` as a
    string literal (DQS=ON behavior). Modern SQLite defaults DQS off for DML.
    **Fix (engine)**: the SQL parser must treat double-quoted tokens in DML as
    column references (error when unresolved), not string literals.

13. **intpkey** — `/Users/muaddib/dev/sqlite/test/intpkey.test:646-668` (18.x)
    `INSERT INTO t1(rowid,x) VALUES(-9223372036854775808,'min-int'),(0,'zero'),
    (9223372036854775807,'max-int')` then `SELECT rowid,* ... ORDER BY rowid`
    and `WHERE rowid = -9223372036854775808`. Engine `got [] want [min-int]`
    and `got [1 min-int 2 zero 3 max-int]` (rowids 1,2,3 = wrong). The
    MinInt64/MaxInt64 rowid literal is not handled (probably parsed as a float
    or rejected; rowid storage of the extremes fails). **Fix (engine)**:
    parse `-9223372036854775808` as the MinInt64 rowid and store/compare it.

14. **istrue** — `/Users/muaddib/dev/sqlite/test/istrue.test:80-97,150-155` (500/510/700)
    `b BOOLEAN DEFAULT true` — engine renders `got [{} {} {} {} {}] want
    [1 1 1 0 0]`: BOOLEAN DEFAULT literals (`true`/`false`/`(not true)`) are
    not evaluated (NULL instead). Also `x IS TRUE` / `x IS FALSE` (istrue-600.x)
    return NULL `{}` instead of `1`/`0`. **Fix (engine)**: evaluate `true`/
    `false` as 1/0 in DEFAULT expressions; implement `IS TRUE`/`IS FALSE`/
    `IS NOT TRUE`/`IS NOT FALSE` operators (TRUE when x is 1/any non-zero,
    FALSE only for 0, NULL stays NULL).

### C. ENGINE bugs — expressions / values / CAST

15. **cse** — `/Users/muaddib/dev/sqlite/test/cse.test:257-300` (3.2-3.5)
    `SELECT 1000 IN (SELECT x FROM t2), 1000 = y FROM t3` — the engine returns
    Go `bool` `false`; SQLite renders `0`. **Fix (engine)**: all comparison /
    logical / IN operators must produce `int64(1)`/`int64(0)`, never Go `bool`
    (or fix `flatten` to render `bool` as 0/1 — but engine-side is correct per
    SQLite's value model).

16. **numcast (engine part)** — `/Users/muaddib/dev/sqlite/test/numcast.test:27-35`
    Once transpilation is fixed, `CAST(' 876xyz' AS real)` must be `876.0` and
    `CAST(' 456ķ89' AS real)` = `456.0` (SQLite parses the numeric prefix;
    `sqlite3AtoF` in src/util.c). `internal/exec/expression.go:248` REAL case
    uses `strconv.ParseFloat(whole)` which fails → 0.0. INTEGER case already
    parses the prefix manually. **Fix (engine)**: REAL/NUMERIC CAST of text
    should parse the leading numeric prefix (sign, digits, `.`, exponent),
    matching sqlite3AtoF; non-numeric-leading → 0.0.

17. **whereM** — `/Users/muaddib/dev/sqlite/test/whereM.test:19-105` (1.0-1.5)
    `INSERT INTO t1(a,b,c,d,e) VALUES(10.0,...)` with affinities
    (none/INTEGER/TEXT/REAL/BLOB) — engine renders column c (TEXT) as `10`
    instead of `10.0` (`got [10.0 10 10 10.0 10.0] want [10.0 10 10.0 10.0
    10.0]`): TEXT affinity is not applied (real 10.0 stored as int 10).
    Subsequent tests check `c=10`, `c=10.0`, `c='10.0'`, `c LIKE '10.0'`
    (affinity coercion in comparisons) — engine wrong on several. **Fix
    (engine)**: apply column affinities on INSERT (TEXT→string, INTEGER→int,
    REAL→float, BLOB→bytes) and in comparison/LIKE.

18. **affinity** — `/Users/muaddib/dev/sqlite/test/affinity3.test:97-118` (200-260)
    `data JOIN idmap USING(id)` where idmap is `map_integer UNION map_text` —
    engine returns 3 rows `[1 abc a 1 abc e 4 xyz e]`, SQLite returns 1 row
    `[4 xyz e]`. This is the 2017 ticket
    `https://sqlite.org/src/info/7ffd1ca1d2ad4ecf` — affinity coercion of the
    UNION-view column vs the join column. **Fix (engine)**: UNION compound
    column affinity must come from the first SELECT; JOIN USING must apply
    affinity conversion so `'1'` (TEXT, from data) does not match `1` (INT,
    from map_integer) the way the engine does.

### D. ENGINE bugs — multi-row VALUES / views / RETURNING

19. **values** — `/Users/muaddib/dev/sqlite/test/values.test` (engine part)
    After fixing `$var` transpilation, remaining engine issues:
    - 1.2.6 (multi-row VALUES): the engine's multi-row VALUES with 3+ columns
      drops the 3rd column of later rows (`[4 4 {} 5 5 {} 6 6 {}]`).
    - 2.x: `VALUES(1,2),(3,4),(row_number() OVER (),5)` — `row_number()` is
      an **unknown function** in VALUES (window functions in VALUES not
      supported; SQLite computes per-row).
    - 5.1: `CREATE VIEW v1 AS VALUES(1,2,3),(4,5,6),(7,8,9)` then
      `SELECT * FROM v1` → engine `[3 3 3]` (only last row) — VALUES-view
      materialization broken for multi-row.
    - 7.1: `WITH x1(a,b) AS (VALUES(1,2),('a','b')) SELECT * FROM x1 one, x1
      two` → engine `[1 2]` (CTE VALUES materialization broken).
    - 13.0: scalar `VALUES( (max(...)), (123), (456) )` in SELECT list →
      engine `[a]` want `[xyz]` — scalar VALUES with expressions.
    **Fix (engine)**: make multi-row VALUES (compound) produce all rows in all
    contexts (INSERT, CREATE VIEW AS, CTE, scalar subquery).

20. **cast** — `/Users/muaddib/dev/sqlite/test/cast.test:516-526` (10.1-10.4)
    `VALUES(CAST(44 AS REAL)),(55)` → `44.0 55`; engine `[44.0]`. Same
    multi-row VALUES compound bug (only first tuple returned) plus
    SQLITE_AFF_FLEXNUM (INT 55 must not be coerced to REAL in a VALUES
    compound — the second value stays `55` int). **Fix (engine)**: VALUES
    compound row preservation + flexnum affinity (no int↔real coercion in
    compound result columns).

21. **returning** — `/Users/muaddib/dev/sqlite/test/returning1.test:21-27` (1.0/1.1)
    `INSERT INTO t1(b) VALUES(10),('happy'),(NULL) RETURNING a,b,c;` →
    `1 10 pax 2 happy pax 3 {} pax`; engine returns `[]`. The RETURNING clause
    is parsed but not executed for multi-row INSERT...VALUES (and UPDATE
    RETURNING / DELETE RETURNING paths). **Fix (engine)**: execute RETURNING
    projection on INSERT/UPDATE/DELETE (the rows exist; the exec path just
    drops the RETURNING result).

22. **select1** — `/Users/muaddib/dev/sqlite/test/select1.test:1206-1212` (21.1)
    dbsqlfuzz: `FROM t1 LEFT JOIN v1a ON z=b` where `v1a(z,y)` is a view
    `SELECT x IS NULL, x FROM t2`. Engine errors `ON clause references tables
    to its right` — the ON-clause validation doesn't resolve view columns
    (`z` is a view column). Expected `{}` (empty). **Fix (engine)**: when the
    right side of a JOIN is a view, resolve its declared columns in ON-clause
    validation.

### E. ENGINE bugs — query planning / EQP / EXPLAIN

23. **selectD** — `/Users/muaddib/dev/sqlite/test/selectD.test:28-104`
    `ATTACH ':memory:' AS aux1` + cross-schema `main.t4 JOIN aux1.t4` —
    `no such table: main` → **ATTACH is not supported**. This blocks all of
    selectD (including the EQP `SEARCH x2 USING AUTOMATIC` expectation).
    **Fix (engine)**: implement ATTACH/DETACH for in-memory schemas (or at
    least `ATTACH ':memory:' AS name` + `main.`/`name.` table qualification).

24. **whereE** — `/Users/muaddib/dev/sqlite/test/whereE.test:63-68` (1.3/1.4)
    `ANALYZE; EXPLAIN QUERY PLAN SELECT x FROM t1, t2 WHERE a=z AND c=x`
    expects `SCAN t1` + `SEARCH t2` (automatic index). Engine emits
    `SCAN t1` + `SCAN t2` — the planner never builds an **automatic index**
    for the inner join table. **Fix (engine)**: EQP planner — when a join
    predicate references an unindexed column of the inner table, emit
    `SEARCH <t> USING AUTOMATIC COVERING INDEX` (and actually use the index
    for row lookup).

25. **whereH** — `/Users/muaddib/dev/sqlite/test/whereH.test` (lines ~74-122)
    EQP: `SCAN t1 USING INDEX t1abc -- B-TREE FOR ORDER BY` vs expected
    `TEMP B-TREE FOR ORDER BY` / `INDEX t1abcd`. The engine labels index
    scans that cannot satisfy the ORDER BY as `-- B-TREE FOR ORDER BY`
    instead of choosing a temp b-tree. **Fix (engine)**: planner must
    distinguish index-usable ORDER BY from temp-b-tree ORDER BY in EQP
    output (choose TEMP B-TREE when no index covers the sort).

26. **whereF** — `/Users/muaddib/dev/sqlite/test/whereF.test:303-310` (7.3)
    `EXPLAIN SELECT (SELECT COUNT(*) FROM t2 WHERE (t1.b GLOB 'a*z' AND
    t2.bb='xyz') OR (t2.bb=t1.b) OR (t2.aa=t1.a)) FROM t1` expects the
    `(Lt|Ge)` opcode in the bytecode. Engine EXPLAIN emits `0 Init ... 0
    *sql.SelectStmt` — EXPLAIN is a stub printing AST pointers, not VDBE
    opcodes. (Also earlier whereF EQP failures: `want pattern [a=. AND b=.]`.)
    **Fix (engine)**: either emit real opcode names in EXPLAIN or make the
    EQP path handle these patterns; the OR-optimization with `(Lt|Ge)` range
    opcodes is the semantic behind it.

27. **whereD** — `/Users/muaddib/dev/sqlite/test/whereD.test:34-52` (1.2-1.9)
    `SELECT k FROM t WHERE (i=1 AND j=1) OR (i=2 AND j=2)` expects rows in
    OR-term order `one two`; engine returns `two one`. **Fix (engine)**:
    OR-optimization row order — evaluate/union OR terms in source order and
    dedupe preserving first occurrence.

28. **whereI** — `/Users/muaddib/dev/sqlite/test/whereI.test:96` (3.0)
    WITHOUT ROWID table `t3(a,b,c,d, PRIMARY KEY(c,b))`; `SELECT c||'.'||b
    FROM t3 WHERE a='t' OR d='t'` expects `2.1 2.2 1.2`; engine returns
    `1.2 2.1 2.2`. **Fix (engine)**: OR-optimization order on WITHOUT ROWID
    tables (row order must follow the PK/index, not scan order).

29. **where** — `/Users/muaddib/dev/sqlite/test/where2.test:461-468` (6.14.1)
    `SELECT c FROM t614a, t614b WHERE a=c OR b=c` with `a,b TEXT COLLATE
    NOCASE` and `t614b.c TEXT` (BINARY) — engine `[]`, want `[aaa bbb]`.
    **Fix (engine)**: NOCASE-collated OR join — the engine must resolve
    columns with their collation and match `AAA`→`aaa` under NOCASE.

30. **whereL** — `/Users/muaddib/dev/sqlite/test/whereL.test:83-88` (200/201)
    `SELECT * FROM c3 WHERE x=y AND y=z AND z='abc'` with columns
    `x COLLATE binary, y COLLATE nocase, z COLLATE binary` — SQLite must NOT
    blindly propagate the constant `'abc'` into `x`/`y` comparisons (because
    of differing collations) and must return `ABC ABC abc`. Engine returns
    `[]` (or errors on later CTE+LEFT JOIN cases with `ON clause references
    tables to its right`, whereL-298 — same view/derived-table ON bug as
    select1). **Fix (engine)**: collation-aware constant propagation in the
    optimizer AND the ON-clause column-resolution fix.

### F. ENGINE bugs — DELETE / triggers / ordering

31. **delete3** — `/Users/muaddib/dev/sqlite/test/delete3.test:21-48` (1.1)
    `CREATE TABLE t1(x integer primary key); INSERT INTO t1 SELECT x+2 FROM
    t1;` repeated 20× doubling to 524288 rows. **NOT a panic — an infinite
    loop / hang** in `internal/exec/insert.go` — verified stack:
    `execInsert → insertRow → pkRowID → findNextRowID` (and the UNIQUE/PK
    conflict check `findRowByUniqueCols → scanForConflict → hasConflictAt`)
    does a **full table scan per inserted row**. With the table doubling to
    524288 rows this is O(n²) per statement → effectively a hang.
    **Fix (engine)**: for INTEGER PRIMARY KEY, check rowid conflict via
    direct rowid lookup (btree seek), and cache/derive max rowid without a
    full scan; ensure the loop terminates. (Same family as T1.2 selectG.)

32. **delete_pkg** — `/Users/muaddib/dev/sqlite/test/delete.test:436-442` (12.0)
    `DELETE FROM t0 WHERE NOT ((t0.vkey <= t0.c1) AND (t0.vkey <> (SELECT
    vkey FROM t0 ORDER BY vkey LIMIT 1 OFFSET 2)))` then `SELECT * FROM t0`
    expects `8 4 95` (only the row that survives). Engine keeps all 4 rows —
    the correlated subquery `ORDER BY vkey LIMIT 1 OFFSET 2` inside DELETE
    WHERE returns nothing/wrong, so `NOT(...)` is true for every row.
    **Fix (engine)**: evaluate LIMIT/OFFSET subqueries in DELETE WHERE
    (subquery with ORDER BY + LIMIT + OFFSET in a scalar context).

33. **values (trigger RAISE)** — `/Users/muaddib/dev/sqlite/test/values.test`
    (the `got [1 2 22 22 N N 1 2 44 44 N N ...] want [N N N N 3 4]` signature
    from the earlier handover corresponds to VALUES with a trigger doing
    RAISE(ABORT) — the trigger-fired VALUES rows are not being replaced by
    the RAISE result). **Fix (engine)**: trigger RAISE inside VALUES /
    INSERT path — the conflict/abort must stop the statement and leave the
    table unchanged (verify exact subtest after fixing the `$var` bug; the
    handover signature is stale relative to the 1.2.5 root cause above).

34. **nulls (undocumented in original research)** — `nulls1.test:14-45`
    `SELECT a FROM t3 ORDER BY a NULLS FIRST/LAST` (and DESC variants) with
    `LIMIT`; also index-assisted `ORDER BY b NULLS LAST` on
    `CREATE INDEX i2 ON t2(a,b)`. Engine renders `[{} {} 10 20 30]` for
    `NULLS LAST` — **NULLS FIRST/LAST is parsed but ignored**; the sort
    comparator always puts NULLs first. Also `ORDER BY nulls` (column named
    `nulls`, nulls1.test ~line 209) — parser treats the bare identifier
    `NULLS` as the keyword. **Fix (engine)**: honor NULLS FIRST/LAST in the
    ORDER BY comparator (NULLS FIRST default ASC, NULLS LAST default DESC);
    ensure bare `nulls`/`first`/`last` identifiers still work as column names
    in `ORDER BY <col>` (keyword context resolution).

---

## §2. CROSS-CUTTING FINDINGS (highest-leverage)

1. **`$var`-in-braced-SQL transpilation is broken** (numcast, values, between,
   and any test using `execsql {...$var...}`): tcl2go either leaves `$var`
   literal (values `$a`; between via `tclExprWith` fallback) or concatenates
   it as raw SQL text (numcast `$str`). In real TCL, `db eval`/`execsql`
   braced scripts substitute `$var` with the **value** — for `db eval` this is
   a bound parameter, for `execsql` it is string substitution. Fixing this one
   transpiler issue clears the numcast parse errors and the values 1.2.5/1.2.6
   NULL column.

2. **Custom test-only SQL functions**: `test_getsubtype` (test_func.c:623),
   `intreal` (test1.c:981), `udf` (proc in selectC.test), `counter`
   (test1.c, selectH), `f()` (selectA), TCL `int()/log()` in
   `evalSimpleArith` — all harness-side. `implies_nonnull_row` is the
   exception: it is in the main engine (func.c:3325, `INLINEFUNC`), so
   implementing it engine-side matches SQLite.

3. **Go `bool` rendering** (`false`/`true` instead of `0`/`1`) is the cse
   failure and probably affects many un-hit tests. Engine value model should
   never emit Go bools for SQL results.

4. **Multi-row VALUES compound** is broken in every context (INSERT, CREATE
   VIEW AS, CTE, scalar subquery, VALUES in SELECT list) — values, cast
   (10.1-10.4), and parts of select1/selectC/selectH. This is a single root
   cause: the engine's VALUES compound only keeps one tuple (first or last).

5. **EXPLAIN / EXPLAIN QUERY PLAN**: the planner emits SCAN-only plans and a
   stub EXPLAIN (`*sql.SelectStmt` AST pointers). whereE/whereH/whereF/selectD
   all need the planner to choose automatic indexes, temp b-trees, and real
   opcode names.

6. **ON-clause "references tables to its right" false positive** when the
   right side is a view/derived table (select1-21.1, whereL-298) — column
   resolution for view columns in JOIN ON.

7. **ATTACH not supported** (selectD) — blocks an entire package.

8. **NULLS FIRST/LAST ignored** (nulls) — sort comparator doesn't honor the
   modifier.

---

## §3. VERIFY LOOP + COMMIT DISCIPLINE (per task)

```bash
# 1. Reproduce (RED) — package under fix:
go test -tags testgen ./testgen/<pkg>/ -count=1

# 2. Fix (smallest change) → GREEN:
go test -tags testgen ./testgen/<pkg>/ -count=1

# 3. Regression neighbors (the 27 currently green):
go test -tags testgen ./testgen/{insert,delete_,update,null,types,coalesce,literal,select2,select3,select4,select5,select6,select8,select9,selectB,selectE,selectF,selectG,whereA,whereB,whereC,whereJ,whereK,whereN,delete2,delete4,valuesfault}/ -count=1

# 4. SOLID architecture:
go test -run TestSOLID_ ./... -count=1

# 5. Update THIS PLAN (checkbox §0 + measured status at top) + HANDOVER_TIER1.md,
#    commit (convention: "T1.N: <description>"), push:
git add -A && git commit -m "T1.N: ..." && git push
```

**Discipline**: every task updates the plan (§0 checkbox + status line) in the
same commit as the fix. Never commit a fix without its plan update. Never
leave the plan stale at session end — commit + push before stopping.

---

## §4. How databases are created / deleted (TCL vs generated Go)

Companion clarification (2026-08-01): the TCL test framework creates and
destroys SQLite connections and database files in well-defined ways; the
tcl2go transpiler maps each to a frigolite call. The testgen tree accumulates
`*.db` files at runtime — they are **gitignored artifacts** (`.gitignore`
has `*.db`; `git ls-files | grep '\.db$'` = 0 tracked), safe to delete, and
regenerated on the next test run. This section documents the full lifecycle.

### TCL side (tester.tcl)

| TCL | What it does |
|---|---|
| `sqlite3 db :memory:` | Open a new in-memory connection named `db` (wraps `sqlite_orig` via `proc sqlite3`, tester.tcl:116). |
| `sqlite3 db ./test.db` / `sqlite3 db test.db` | Open an on-disk connection (creates the file if absent). |
| `sqlite3 db2 <file>` (and db3, db4, ...) | Open secondary connections (multi-db tests). |
| `db close` | Close the `db` connection. |
| `forcedelete test.db` (and `-journal`/`-wal`) | Delete the file from disk; `proc forcedelete` → `do_delete_file true`, tester.tcl:272. |
| `reset_db` | `catch {db close}` + `forcedelete test.db test.db-journal test.db-wal` + `sqlite3 db ./test.db` (+ optional `$::SETUP_SQL`), tester.tcl:551. Called once at the top of every test. |
| `drop_all_tables {db}` | Drop every user table (disables foreign_keys during the drop), tester.tcl:2254. |
| `finish_test` | Closes the db and reports the pass/fail summary. |
| `ifcapable` / `sqlite3_db_config` / `sqlite3_limit` | Conditional/infrastructure commands (mostly no-op'd by tcl2go). |

### Generated Go side (tools/tcl2go/gen.go)

| TCL | Generated Go |
|---|---|
| `sqlite3 db :memory:` (or `""`) | `db, err = frigolite.Open("")` — main connection, always **in-memory** (gen.go `processSqlite3`). |
| `sqlite3 db ./test.db` (file) | `db, err = frigolite.Open("")` — **in-memory, not a file**; the compat suite deliberately keeps tests in-memory (gen.go comment: "Reopening a FILE database is a no-op"). |
| `forcedelete test.db; sqlite3 db test.db` | `os.Remove(...)` then the next `Open` uses `Open("")` (gen.go `pendingFileReset`). |
| `sqlite3 db2 <file>` (secondary) | `db2, err := frigolite.Open(<file>)` + `defer db2.Close()` — the ONLY path that keeps a filename, which is what creates stray `.db` files (e.g. `_dbtmp0, err := frigolite.Open("test.db")` in like_test.go:554). |
| `db close` | `db.Close()` (gen.go `processClose`). |
| `reset_db` | `db.Close()` + `db, err = frigolite.Open("")` (gen.go case `reset_db`). |
| `drop_all_tables` | `for _, _t := range db.Query("SELECT name FROM sqlite_master WHERE type='table'").Rows { db.Exec("DROP TABLE " + fmt.Sprint(_t[0])) }` (gen.go case `drop_all_tables`). |
| `finish_test` | No-op (deferred `db.Close()` from the initial open handles cleanup). |

### Consequences / notes for Tier 1 work

1. **Stray `*.db` files are runtime artifacts.** They appear because secondary
   connections keep the filename (like_test.go:554 `_dbtmp0,
   frigolite.Open("test.db")`). They are gitignored; delete freely, e.g.
   `find testgen -name '*.db' -delete` (214 files removed 2026-08-01).
   Verified: `go test -tags testgen ./testgen/like/` fails identically with
   and without `testgen/like/test.db` — pre-existing engine issues, not the
   cleanup.
2. **Main-connection semantics are already correct**: `sqlite3 db :memory:`
   and `sqlite3 db ./test.db` both map to `frigolite.Open("")`, so Tier 1
   tests exercise in-memory storage — matching the TCL intent for the compat
   suite.
3. **If a real on-disk test ever needs a file**, the harness currently has no
   reliable file-backed path for the MAIN connection (only secondary dbN vars
   keep filenames). Do not rely on file persistence in Tier 1 fixes.
4. `drop_all_tables` (the 2026-08-01 transpiler addition) drops user tables
   but not views/triggers/indexes; the TCL version drops all object types.
   If a future package needs full object cleanup, extend the generated loop
   to cover `sqlite_schema.type IN ('table','view','trigger','index')` with
   `DROP VIEW/TRIGGER/INDEX` as appropriate (or a single `DROP <type>` using
   the recorded type).

---

## §5. TCL test framework → generated Go test mapping

The generated tests come from `tools/tcl2go/gen.go` (a transpiler). Key
mappings (confirmed in the source and in generated code):

| TCL construct | Go output |
|---|---|
| `do_execsql_test NAME {SQL} {EXPECTED}` | `db.Query/Exec` + `flatten(r)` compare against `want` string |
| `do_catchsql_test NAME {SQL} {CODE {MSG}}` | `db.Exec` + error substring check (`strings.Contains`) |
| `do_test NAME {body} {EXPECTED}` | Go block with `t.Errorf` on mismatch |
| `db eval {SQL}` / `execsql {SQL}` | `db.Exec("SQL")` with error check |
| `foreach V L {BODY}` | Go `for _idx0 := 0; _idx0+1 <= len(_items0); _idx0++` loop |
| `for/while/if/set/incr` | native Go control flow + `tclExprWith`/`evalSimpleArith` |
| `reset_db` | `db.Close()` + `db, err = frigolite.Open("")` |
| `source tester.tcl`, `finish_test`, `proc`, `sqlite3_db_config`, `sqlite3_limit`, `catchsql` | **NO-OP / comment** — `// ... (unsupported command, not transpiled)` |
| `$var` inside a braced SQL script | **string-concatenated raw text** (BUG — see §2) |

The harness `flatten` renders: `nil`→`{}`, `int64`→decimal, `float64`→fixed
(`10.0` style), `string`→as-is, `bool`→Go `true`/`false` (**wrong** vs SQLite,
see cse). `tcl_nullvalue` = `"{}"`.

### Package name mapping (groupName in gen.go)
- `delete.test` → package `delete_pkg` (`delete` is a Go builtin → `_pkg` suffix)
- `delete_db.test` → `delete_`; `delete2/3/4.test` → `delete2/3/4`
- `affinity2.test`+`affinity3.test` → package `affinity`
- `subtype1.test` → `subtype`; `strict1/2.test` → `strict`; `nulls1/2.test` → `nulls`;
  `returning1.test` → `returning`
- every other package name == TCL base name (`select1.test` → `select1`)
