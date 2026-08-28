# HANDOVER — Frigolite Tier 1 (session 2026-08-01, fresh context)

**Date**: 2026-08-01 (handover; goal paused for a fresh context)
**HEAD**: `037413ce` + one uncommitted batch (parenthesized-join-as-subquery parser, tcl2go `drop_all_tables` support + regenerated testgen, probe32 removal) — committed together with this doc.
**Master plan**: `plans/TDD_MASTER_V2.md`

---

## 0. FRESH MEASURED STATUS (this session, verify_tier.sh 1, 58 packages)

> Command: `bash scripts/verify_tier.sh 1` (each package `go test -tags testgen -count=1 -timeout 300s`; then SOLID).
> Result: **TIER 1 HAS FAILURES** — 27 PASS / 31 FAIL (full run log in `/tmp/verify_tier1_full.log`).

**PASS (27)**: insert, delete_, update, null, types, coalesce, literal,
select2, select3, select4, select5, select6, select8, select9, selectB,
selectE, selectF, **selectG** (parser panic GONE — see §2), whereA, whereB,
whereC, whereJ, whereK, whereN, delete2, delete4, valuesfault

**FAIL (31)** with failure signatures:

| pkg | signature |
|---|---|
| select1 | `CREATE VIEW v1a(z,y) AS SELECT x IS NULL, x FROM t2; SELECT ... LEFT JOIN v1a ON z=b` — VIEW+LEFT JOIN column resolution |
| affinity | `got [1 abc a 1 abc e 4 xyz e] want [4 xyz e]` — trailing duplicate rows (affinity/coercion) |
| expr | `got [{}] want [1]` — expr edge (probably `implies_nonnull_row` custom fn or similar) |
| cast | `got [44.0] want [44.0 55]` — regression vs old 28231f98 handover (was green); NOT from the pending parser change (fails at HEAD 037413ce too) |
| between | `exec error: unknown function: int` — `int(log($i)/log(2))` |
| istrue | `got [] want [1]` |
| numcast | `parse error: near "321.5"` — `SELECT CAST( Ġ 321.5 AS integer)` (unary + parsing) |
| subtype | `SELECT test_getsubtype('hello')` — custom test function missing |
| strict | `got [] want [{NULL value in t1.id}]` — STRICT table NULL error not raised |
| intpkey | `got [] want [max-int]` |
| intreal | `SELECT intreal(5)` — custom test function missing |
| nulls | ORDER mismatch: `got [X 3 X ... {} {} {} ...] want [X 3 X | X 4 X | X 5 X | X 7 X | X {} X | {} {} {} | Y {} {}]` |
| select7 | `CREATE VIEW v0 as SELECT x, y FROM t01 UNION SELECT x FROM t02; EXPLAIN QUERY PLAN SELECT * FROM v0 WHERE x='0' OR y` — UNION view + EQP |
| selectA | `got [] want [ABC]` |
| selectC | `got [1 {} 1 {} 1 {}] want [1 1 1 2 1 3]` — view/join expansion wrong row pairing |
| selectD | EQP: `got SCAN t41 / SCAN x2 want SEARCH x2 USING AUTOMATIC` |
| selectH | `got [&{... rtrim} ...] want [v1 t1]` — pointer rendering / view col names |
| where | 4-way join `1, 1, 1, 1` (see log) |
| whereD | `got [three one] want [one three]` — ORDER BY/LIMIT on compound |
| whereE | EQP order: `got SCAN t2 / SCAN t1 want .*SCAN t1.*SEARCH t2.*` |
| whereF | EXPLAIN missing `(Lt|Ge)` opcode — `got [0 Init ... 0 *sql.SelectStmt]` |
| whereG | `got [] want [NULL '0']` |
| whereH | EQP: `got SCAN t1 USING INDEX t1cd -- B-TREE FOR ORDER BY want TEMP B-TREE FOR ORDER BY` |
| whereI | `got [1.2 2.1 2.2] want [2.1 2.2 1.2]` — WITHOUT ROWID ordering |
| whereL | CTE + `LEFT JOIN t1 ON ((subQuery.col_0) == (false))` — CTE/join resolution |
| whereM | `got [0] want [1]` |
| delete3 | **panic** (goroutine stack trace in log) — investigate first |
| delete_pkg | `got [2 1 -20 2 2 {} 2 3 0 8 4 95] want [8 4 95]` — DELETE RETURNING aggregate |
| returning | `ON CONFLICT(a) DO UPDATE SET b=44 RETURNING *` — upsert RETURNING |
| values | `got [1 2 22 22 N N 1 2 44 44 N N ...] want [N N N N 3 4]` — VALUES/trigger RAISE |
| cse | `got [false] want [0]` — bool rendered `false`, SQLite wants `0` |

**SOLID / build / root-suite status**:
- `go build ./...` → OK
- `go test -run TestSOLID_ ./... -count=1` → passes (last verified on prior commit; re-run after next change)
- Root suite `go test .` → 10 FAIL, **byte-identical** between HEAD and working tree (no regression from the pending batch). Includes `TestSelectBetween` (pre-existing), FTS tests, etc.

---

## 1. DONE since the previous handover (commits T2.1–T2.5 + this batch)

**Prior 8 committed green packages** still green: types, select2, whereA, affinity (was green at old HEAD, see below), whereK, selectE, analyzeC, eqp, plus whereJ/whereN/whereC/whereB now green in this run.

Recent committed work (git log):
- `T2.5` FULL/RIGHT JOIN output + qualified-star expansion + VALUES views + empty-want
- `T2.4` NATURAL JOIN matching+merge, db nullvalue handler — join5+join6 green
- `T2.3` join2 fully green + join5-11.x — autoIndex fallback, unqualified ON cols, ORDER BY +1, comma-join USING
- `T2.2` IS/IS NOT unwrap ColumnValue in joins
- `T2.1` join2/join6 green — UTF-8 identifiers, ON-clause right-reference validation, TCL nullvalue
- earlier: window OVER() minimal support (cast-9.0), EQP multi-node emission, CTE merge, INSERT VALUES path fix, `\n;` comment terminator fix, EXPLAIN QUERY PLAN flag + regex expectations

**This session's uncommitted batch (committed WITH this doc)**:
1. `internal/sql/parser.go` — `parseParenTableRef`: parenthesized join `(t2 JOIN t3 USING(a))` now parses as a derived table (subquery) instead of skipping tokens; drives subquery/derived-table join support.
2. `tools/tcl2go/gen.go` — new `drop_all_tables` command transpilation (drops every user table, matching the TCL helper); **regenerated** the affected testgen packages (eqp, without_rowid3, fkey2/8, e_fkey, e_createtable, e_select, join, distinct, analyze3/9, dbstatus, rowvalue4, schema4, tkt_80ba, e_delete, e_select2).
3. `cmd/probe32/main.go` — removed (debug probe for FULL OUTER JOIN, no longer needed).

Verified for the batch: build clean; `go test -tags testgen ./testgen/eqp/` green; root failure list identical to HEAD (no regression). `without_rowid` still FAILs only on pre-existing collation `mysort` gaps (unrelated).

## 2. KEY STATE CHANGES vs the previous handover doc

- **selectG no longer panics** — the `parser.go:47` LALR crash is gone; `selectG` passes the full package run. (Old doc's §0 "PARSER PANIC" is obsolete.)
- **cast regressed** between old HEAD 28231f98 and current HEAD 037413ce (`got [44.0] want [44.0 55]`) — NOT caused by the pending batch; bisect T2.x commits if it matters.
- **affinity was green at old HEAD** (in the 8-package verify) but FAILs now in the full run (`got [... 4 xyz e] want [4 xyz e]`) — re-check: it may be a different subtest than the one verified before, or a T2.x regression.
- **intpkey/intreal** now FAIL with `want [max-int]` / custom fn `intreal` — old doc listed intpkey PASS; status changed since.
- `whereJ` still fully green (EQP multi-node + GROUP BY B-TREE + comment fix all hold).

## 3. TO DO — next Tier 1 targets (suggested order)

1. **delete3 panic** (crash — highest priority, like selectG was). Reproduce: `go test -tags testgen ./testgen/delete3/ -count=1 -timeout 300s`; find the goroutine panic in the log.
2. **cse bool formatting** (`false` → `0`): smallest, isolated — value rendering of Go bool in result rows.
3. **values / returning** — `VALUES multi-row + trigger RAISE` and `upsert ... RETURNING *` (column-count / RETURNING exec path).
4. **INSERT...SELECT column-count + LIMIT syntax** (select1 / objective note): `INSERT INTO t1 SELECT ...` with more columns than the table must be rejected.
5. **numcast unary parse** (`CAST( 321.5 AS integer)` — leading space + unary tokenization).
6. **strict** — STRICT table NULL constraint error message.
7. **EQP order fixes**: selectD, whereE, whereH (`TEMP B-TREE FOR ORDER BY` vs index).
8. **whereI WITHOUT ROWID ordering**, nulls order, whereD compound ORDER/LIMIT.
9. **Custom test functions** (between `int()`, subtype `test_getsubtype`, intreal `intreal`) — decide: implement in testgen harness or engine (engine has 60+ functions already; these are test-only helpers — prefer harness-side, like `implies_nonnull_row`).
10. **cast regression** — bisect T2.x to find the culprit; likely a type-coercion change.
11. **affinity** — re-verify which subtest regressed.
12. **view/join column resolution**: select1 (LEFT JOIN view), selectC, selectH, whereL (CTE + LEFT JOIN).

## 4. Verify loop (per fix)

```bash
# package under fix:
go test -tags testgen ./testgen/<pkg>/ -count=1
# regression neighbors:
go test -tags testgen ./testgen/{select1,insert,update,delete_,null,types,coalesce,literal,select2,select3,select4,select5,select6,select8,select9,selectB,selectE,selectF,selectG,whereA,whereB,whereC,whereJ,whereK,whereN,delete2,delete4,valuesfault}/ -count=1
# SOLID:
go test -run TestSOLID_ ./... -count=1
# full tier (after batch of fixes):
bash scripts/verify_tier.sh 1
```

Completion criterion (when all 31 FAIL are green): `bash scripts/verify_tier.sh 1` exits 0 AND `go test -run TestSOLID_ ./... -count=1` passes; all fixes committed; this doc updated with final measured status.

## 5. Repo conventions

- Generated testgen files carry `//go:build testgen` — run with `-tags testgen`. Regenerate with `go run ./tools/tcl2go/` (fast, ~1s).
- The `tcl2go` binary at repo root is a tracked build artifact; don't commit its changes (`git checkout -- tcl2go` if modified).
- Debug instrumentation: `fmt.Printf("ZZ...")`, remove before commit.
- Commit convention: `T1.x: <description>` per TDD inner loop (RED → root cause → smallest fix → GREEN → regression → commit).
