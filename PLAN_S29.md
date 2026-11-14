# PLAN_S29 — expr slices (E3, E1, E2, then S1+S2)

Worktree /private/tmp/wt_expr, branch s29-expr-unit @ e1d2fc762.
Triage: portplan/TRIAGE_expr_2026-11-12.md. Oracle: /usr/bin/sqlite3 (3.54).
C source: /Users/muaddib/dev/sqlite (3.51.0).

## Baselines (recorded before any edit, from this worktree)

- FRIGOLITE_TEST=fts3expr3 → 52 leaf FAILs (26 unique ×2 — .json AND .json.gz
  both in testdata/, confirming triage signature 1).
- FRIGOLITE_TEST=fts3 → 106 leaf FAILs (the 52 fts3expr3 + e_fts3/fts3auto/
  fts3cov/fts3defer*/fts3expr-6.1/fts3first/fts3malloc/fts3query residue).
- FRIGOLITE_TEST=e_expr → 21 leaf FAILs (matches triage §A exactly).
- Leaf-name sets (python, from `--- FAIL:` lines) saved at
  /tmp/s29_base_sets.json; logs /tmp/s29_base_{fts3expr3,fts3,e_expr}.log.

## E3 — cmdRegsub options + fts3expr3 regen (32 subtests)

C-fact: TCL `regsub ?switches? pattern string subSpec varName`; switches are
leading dash-args. fts3expr3.test:120-121 uses `regsub -all AND $node OR node`
and `regsub -all xxx $tree $node tree`. The converter treats args[0]="-all" as
the pattern (matches literal "-all", no substitution) so the andor wants froze
as the raw `AND xxx xxx` template (triage signature 4).

Fix — tools/tclconvert/tcl/interp.go cmdRegsub (~1923):
1. Consume leading option args: `-all`, `-nocase` (and TCL's other no-arg
   switches `-expanded`, `-line`, `-linestop`, `-linestart` mapped to Go
   inline flags (?x)/(?m)/(?s)/(?s)/(?m); `-expanded` etc. rarely used);
   `--` ends options. Unknown dash option → UnsupportedCommandError (the
   S22 lsort lesson: never silently skip an unknown option).
2. Remaining args: pattern, str, repl, varName (4th → write back via setVar,
   preserving the existing proc-local write semantics).
3. `-nocase` → prefix "(?i)" to the pattern.
4. `-all` → re.ReplaceAllString(str, repl) (unchanged behavior for this path).
   WITHOUT `-all` → replace the FIRST match only (TCL semantics): find
   FindStringIndex, splice prefix + re.ExpandString(repl over loc) + suffix.
   (Current code replaces all even without -all — wrong per TCL; no committed
   fixture depends on the wrong shape: the only regsub-using converted family
   with non-`-all` regsubs is fts3expr3, whose non-all regsubs replace a
   single "xxx" placeholder per call, so single-vs-all coincides there.)
5. Compile error → UnsupportedCommandError (loud), never silent.

Regen (S22 lessons pattern):
- `go run ./tools/tclconvert/ -only fts3expr3 -testdir /Users/muaddib/dev/sqlite/test -outdir testdata`
- The converter writes ONE output (gz only when >1MB) and removes the stale
  alternate (main.go:237-253) — this resolves the .json+.json.gz duplicate:
  keep whichever it wrote, `git rm` must show the other gone; verify with
  `ls testdata/fts3expr3*` and git status.
- Determinism: the random queries come from the converter's TCL rand port;
  regsub changes no rand() call count, so the 1.N/2.N/3.N inputs must be
  byte-identical to the committed fixture — only the andor EXPECT strings
  (3.N#01 where i%4==2, i in 2..62 → 3.2,3.6,...,3.62) may change, from the
  raw "AND xxx xxx" template to substituted balanced trees.
- Diff old vs new fixture in python: test name-set equality (must be equal),
  list changed expect strings; count.
- If the regenerated expects still look wrong (e.g. unbalanced), diagnose
  against C's fts3ExprBalance before touching the engine (NO TRY/FAIL).

## E1 — fts3 exprtest argv coercion (4 subtests)

C-fact (fts3_expr.c:1233-1245): the C NEVER type-checks — zTokenizer,
zExpr and every azCol come from sqlite3_value_text(argv[i]) (any SQL value
coerces: integer 1 → "1"). Our internal/fts/exprtest.go ExprTestFunc rejects
non-text args ("fts3_exprtest: the expression must be text").

Fix — internal/fts/exprtest.go:
- Add fts3ValueText(v) mirroring sqlite3_value_text: nil→"" (C gets NULL and
  parses the empty buffer), string as-is, int64 strconv.FormatInt,
  float64 strconv.FormatFloat('g',-1,64) (the engine's REAL text convention,
  internal/value toString), []byte raw, default fmt.Sprintf("%v").
- ExprTestFunc: tokenizer = fts3ValueText(args[0]); expr = fts3ValueText(
  args[1]); columns = fts3ValueText(arg) each. Drop the three type-check
  errors ("the tokenizer name must be text", "the expression must be text",
  "the column names must be text" — none of these texts exist in C; the C
  surfaces only the Usage error and parse errors). Keep the <3-arg Usage
  error (fts3_expr.c:1224, byte-exact).

## E2 — depth limit 12 (10 subtests)

C-fact: SQLITE_FTS3_MAX_EXPR_DEPTH = 12 (fts3Int.h:66). exprtest maps ANY
parse-stage error to "Error parsing expression" (fts3_expr.c:1263-1265) —
the pzErr "FTS expression tree is too large (maximum depth %d)" text goes to
the MATCH path only, and exprtest discards it.

Fix — internal/fts/exprtest.go:
- fts3MaxExprDepth 100 → 12 (keep the C-name comment).
- ExprTest: on rebalance-stage failure (fts3ExprBalance or fts3ExprCheckDepth
  error) return the SAME "Error parsing expression" error the parse path
  produces (C fts3ExprTestCommon reports every error uniformly). The
  "FTS expression tree is too large" text inside CheckDepth/Balance stays for
  any non-exprtest caller (none today — exprtest.go is the only caller).
- Order already matches C: balance first, then depth-check the balanced tree
  (fts3_expr.c:1059-1066 vs ExprTest 121-127).
- 4.1.2 (4000-term AND, want [0] i.e. a balanced tree parses OK) pins the
  constant: balanced depth stays <= 12 so it must PASS; 3.65-3.69#01 and
  4.1.1/4.2 want [1 {Error parsing expression}].

## S1 — e_expr A4 + A5 (3 leaves) [if time]

evalCastExpr (internal/execexpr/expression_eval_tail.go:444) + its cast
helpers in expression_inlist.go:362-503:
- A4 (28.1.2): blob→text CAST truncates at the first NUL byte
  (sqlite3VdbeMemCast SQLITE_AFF_TEXT arm: the value stops at the NUL —
  oracle: CAST(X'670068006900' AS text) → typeof text, length 1, 'g').
  Unit: castToText's blob arm (or the blob→text path it uses).
- A5 (32.1.8, 32.1.9): CAST float-syntax TEXT to NUMERIC stays REAL —
  sqlite3VdbeMemNumerify: try sqlite3Atoi64 on the text; only integer-syntax
  text that fits becomes INTEGER; non-integer-syntax (contains ./e/E) or
  int64-overflow text (9000000000000000000) → REAL. No lossless-int collapse.
  Unit: castToNumeric.

## S2 — e_expr A3 + A6 + A7 (6 leaves) [if time]

- A3 (27.3.2, 27.3.3): route v.AsType through an sqlite3AffinityType port
  (build.c:1650 — rolling lowercase hash: CHAR/CLOB/TEXT→TEXT, BLOB→BLOB
  (only over NUMERIC/REAL), REAL/FLOA/DOUB→REAL (only over NUMERIC), any
  "int" window→INTEGER+stop, else NUMERIC) and switch the cast arms on the
  derived affinity, replacing the exact-name switch + isTextAffinityType.
  Check for an existing port first (grep AffinityType: none in internal/
  except pragma name mapping) — implement sqlite3AffinityType in execexpr
  (the CAST consumer) or value; execexpr, next to evalCastExpr.
- A6 (37.4, 37.5, 37.10): (a) CASE WHEN text truthiness uses the
  numeric-PREFIX rule ('1english' → 1 → true) via the value layer's
  applyNumericAffinity-shaped helper; (b) iif() in internal/function must
  call the SAME coercion (share one helper). Oracle: f/f/f/f/t/t.
- A7 (11.7.1): parameter slot assignment enforces
  SQLITE_MAX_VARIABLE_NUMBER=32766 (src/sqliteLimit.h:147; resolve.c
  parameter numbering) — error "too many SQL variables" when the assigned
  index exceeds it. Unit: internal/parse slot assignment (find the site that
  assigns ?NNN/auto-number slots).

## Verification & commit

- After E3+E1+E2: FRIGOLITE_TEST=fts3expr3 and FRIGOLITE_TEST=fts3 sweeps;
  python set-diff vs baseline (expect: exactly the 52 fts3expr3 leaves
  disappear from the fts3 sweep; no new leaves). Targeted pure-Go probes for
  the coercion/depth behaviors if a leaf misbehaves.
- After S1+S2: FRIGOLITE_TEST=e_expr sweep; set-diff (expect exactly A3-A7's
  9 leaves gone: 27.3.2 27.3.3 28.1.2 32.1.8 32.1.9 37.4 37.5 37.10 11.7.1;
  NO new leaves). go vet + go build + go test -run TestSOLID_ .
- Lessons: append S29 record to .agents/lessons_learned.md (append-only).
- Commit on s29-expr-unit (pre-commit hook: gocognit<=15, gocyclo<=12,
  staticcheck). Full-suite gate is the COORDINATOR's job.
