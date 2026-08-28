# COMPLEXITY + SOLID REFACTOR — Consolidated Master Plan

> **Status**: AUTHORITATIVE for the complexity + architecture campaign.
> Supersedes all prior complexity/quality-gate task files and checkpoints.
> **Prerequisite read**: `PORTPLAN.md` §0, `portplan/GUIDELINES.md`.
> **Created**: 2025-08-09. **Baseline**: HEAD `e783d997d`, tree clean.

---

## 1. Why We're Stuck — Root Cause

The quality gate (`tools/quality_gate.sh`: gocognit ≤15, gocyclo ≤12,
file-size ≤1000) reports **397 gocognit >15** + **367 gocyclo >12**
violations across **18 files >1000 lines**. The gate cannot pass, blocking
the pre-commit hook and all downstream PORTPLAN feature work (G1–G8).

**Root cause is architectural, not cosmetic:**

| Metric | Value | Problem |
|--------|-------|---------|
| `internal/exec/` total lines | **36,622** | God-package: everything is a method on `*Engine` |
| Methods on `*Engine` | **639** | No responsibility isolation |
| Fields on `Engine` struct | **84** | God-object: 14+ orthogonal concerns share state |
| `select.go` alone | **9,668 lines, 89 violations** | One file = entire SELECT subsystem |
| `parser.go` handleRule | **464 case clauses** | Switch-dispatch instead of map-dispatch |

Previous attempts failed by bundling entire god-files (e.g. 9668-line
select.go) into single goals → context explosion → agent loops with no
progress. **Mechanical helper-extraction alone won't fix this** — it just
spreads the mess across more files without reducing coupling.

---

## 2. Target Architecture — SOLID North Star

### 2.1 The Engine God-Object Must Shrink

The `Engine` struct (84 fields, 639 methods) handles 14+ orthogonal
responsibilities: connection management, schema/catalog, DDL, DML, query
execution, PRAGMAs, transactions, triggers, FK enforcement, virtual tables,
FTS, statistics/planning, expression evaluation, row scanning.

**Target**: `Engine` becomes a **thin coordinator** (~200 lines) that owns
connection-level state (pager, schema, config) and **delegates** to focused
sub-executors via small interfaces.

### 2.2 Sub-Package Extraction (the Go way to enforce SRP)

New packages under `internal/`, each with **one responsibility** and a
**focused public API**. Each defines **capability interfaces** (ISP/DIP)
for the Engine services it needs — sub-packages never import `internal/exec`.

```
internal/
├── exec/              ← Layer 6: thin coordinator (Engine shell + dispatch)
│                         imports all sub-executors; owns connection state
├── execquery/         ← Layer 5: SELECT execution
│   ├── select_core.go     execSelect orchestration
│   ├── join.go            JOIN execution (Executor struct)
│   ├── aggregate.go       GROUP BY / aggregate evaluation
│   ├── validate.go        SELECT clause validation
│   ├── scan.go            table scanning + row materialization
│   └── columns.go         column-name resolution + output building
├── execdml/           ← Layer 5: INSERT/UPDATE/DELETE execution
├── execddl/           ← Layer 5: CREATE/DROP/ALTER execution
├── execexpr/          ← Layer 5: expression evaluation
├── execpragma/        ← Layer 5: PRAGMA handling (map dispatch)
├── execconstraint/    ← Layer 5: FK/CHECK/UNIQUE enforcement
├── exectrigger/       ← Layer 5: trigger management + firing
└── (existing: parse, schema, function, btree, pager, etc. unchanged)
```

**Layer assignment**: all new `internal/exec*/` packages are **Layer 5**
(same as schema/function/vtab/fts). `internal/exec` stays **Layer 6**. This
preserves the existing import-direction invariant (high → low).

### 2.3 Capability Interface Pattern (ISP + DIP)

Each sub-package defines the **minimal** interface for what it needs from
the engine. Engine satisfies these implicitly (Go structural typing).
Compile-time probe enforces substitutability (LSP):

```go
// In the sub-package:
package join

// ExecutionContext is the minimal engine capability JOIN needs.
type ExecutionContext interface {
    ReadAllRows(tableName string) ([]RowMap, error)
    FindIndex(tableName string, cols []string) (*Index, bool)
    EvalExpr(expr sql.Expr, row RowMap) (value.Value, error)
}
var _ ExecutionContext = (*exec.Engine)(nil) // LSP probe (in exec package)

type Executor struct { ctx ExecutionContext; ... }
func NewExecutor(ctx ExecutionContext) *Executor { return &Executor{ctx: ctx} }
func (x *Executor) Execute(joins []JoinSpec) error { ... }
```

### 2.4 Dispatch Maps (OCP)

Replace giant `switch` chains with `map[Key]Handler` dispatch:

| Current | Current size | Target |
|---------|-------------|--------|
| `execPragma` switch | 33 cases | `pragmaHandlers map[string]Handler` |
| `handleRule` switch | 464 cases | `ruleHandlers map[int]Handler` |
| `Exec` dispatch | (check) | `stmtHandlers map[StmtType]Handler` |

Adding a handler = adding a map entry, not editing a growing switch.

### 2.5 State Extraction (SRP)

Group the 84 Engine fields into focused sub-structs owned by their
respective sub-executors:

| Field group | Target owner | ~Fields |
|-------------|-------------|---------|
| trigger state | `exectrigger.Manager` | triggerDepth, triggerTables, triggerNewRow, triggerOldRow, ... |
| scan/query state | `execquery.Engine` | outerRow, outerRows, aliasStack, cteScopes, currentScanTable, ... |
| transaction state | (stays in exec or `execdb`) | inTransaction, ddlBuffer, txSnapshots, savepointStack, ... |
| DML context | `execdml.Engine` | currentDMLTable, currentDMLCtx, updateSetColumns, ... |
| pragma state | `execpragma.Registry` | (pragma flags) |

Engine retains only connection-level fields (pager, schema, dbs, config).

---

## 3. Strategy — Two-Track Micro-Task Split

### Track 1: File SRP + Complexity Reduction (CX-NN)

**Goal**: get the quality gate passing by splitting god-files into
responsibility files (same package) and reducing each function ≤15/≤12.

Each micro-task:
- Targets **one concern** (5–15 functions) in one source file
- Extracts into a **responsibility-named file** (e.g., `select_join.go`)
- Reduces each function to ≤15 gocognit / ≤12 gocyclo
- Uses dispatch maps where the concern has a switch (parser, pragma)
- **Names files to match future sub-packages** so Track 2 is mechanical

### Track 2: SOLID Sub-Package Extraction (SOLID-NN)

**Goal**: extract responsibility files into sub-packages with capability
interfaces, shrinking the Engine god-object.

Each micro-task:
- Creates one sub-package (e.g., `internal/execquery`)
- Defines capability interfaces
- Moves the Track-1 responsibility file into the sub-package
- Introduces a focused Executor type with methods
- Wires Engine to delegate; adds LSP compile-time probe
- Updates the SOLID layer map in `frigolite_solid_test.go`

**Risk acceptance**: the user has approved architectural risk — "code in
current state has limited use." Aggressive restructuring is expected.

---

## 4. Anti-Loop Protocol (MANDATORY — every goal)

This protocol prevents the context-explosion loops that stalled previous work:

1. **Closed function list**: the goal scope is a FIXED list of function
   names. Do NOT add functions. Do NOT explore beyond the list.
2. **One function at a time**: extract helper(s) → check complexity →
   run scoped regression → **checkpoint** (commit). Then next function.
3. **Skip-and-note**: if a function resists after **2 extraction attempts**
   (still >15 gocognit), SKIP it. Note in handover as
   `RESISTANT: <fn> (<reason>)`. Move on. **Do NOT loop.**
4. **Budget**: default **25 turns**. If exceeded, `goal update paused` with
   a handover listing completed/remaining functions. The next goal resumes.
5. **Done**: goal completes when ALL listed functions ≤15/≤12 OR noted
   resistant. Resistant functions are collected in a follow-up goal.
6. **No re-reading**: read each target function ONCE (use line anchors from
   the scope). Never re-read the whole file every turn.
7. **CHECKPOINT every batch**: after every 3–5 functions (or at minimum
   every completed concern), run the verify, then:
   ```
   git add -A && git commit -m "CX-NN.<step>: <what was done>"
   ```
   This is **non-negotiable** — committed work is resumable work.

---

## 5. Regression Net (run after every function batch)

```bash
# Standard regression net (fast, ~30s):
go build ./... && \
go test -count=1 -timeout 60s ./internal/exec/ ./internal/parse/ && \
go test -tags testgen -count=1 -timeout 120s \
  ./testgen/check/ ./testgen/fkey/ ./testgen/subquery/ ./testgen/rowvalue/

# For parser/DDL changes, also run broader testgen:
go test -tags testgen -count=1 -timeout 300s ./testgen/select/ ./testgen/where/

# For tools changes (generators), prove byte-identical output:
go run ./tools/tcl2go/ && git diff --exit-code -- testgen/

# For Track 2 (sub-package extraction), also:
go test -run TestSOLID_ -count=1 .
```

**Committing**: `git commit --no-verify` is ALLOWED during the campaign
(the pre-commit hook runs only build + SOLID, both of which pass). The
strict quality gate is enforced at goal completion via verifyCommand.

---

## 6. Track 1 Catalog — Complexity + File SRP

> Files named to match future sub-packages (§2.2). Ordered by priority
> (biggest blocker first). Each entry is one goal.

### Tier 1A — select.go (89 violations → 8 goals)

| ID | Target file | Key functions (gocognit) | Σ | SOLID note |
|----|-------------|--------------------------|---|------------|
| CX-01 | `select_join.go` | execJoins(72) processJoinRowTrackingRight(51) buildRightJoinUnmatched(27) columnIndexed(25) emptyJoinShortCircuit(19) materializeSubqueryJoin(24) materializeViewJoin(24) materializeTableJoin(22) extractEquiJoinCols(21) collectUsingColumns(17) collectJoinMergedColumns(35) collectJoinTableCols(16) | 353 | SRP: all JOIN execution in one file. Future: `execquery/join.go` |
| CX-02 | `select_join_validate.go` | validateJoinOnClauses(64) checkUsingAmbiguity(22) validateOnRefs(16) validateOnSubqueries(53) validateOnColumnRefs(16) walkJoinOnExpr(41) validateAmbiguousColumnRefs(68) addSubqueryFromCols(19) | 299 | SRP: JOIN/ON/USING validation. Future: `execquery/join_validate.go` |
| CX-03 | `select_agg.go` | evalGroupByNoAggs(27) evalAggregates(29) evalAggregatesEmpty(26) evalAggregatesGroupBy(55) evalAggFuncCall(44) evalDistinctAggregate(49) evalAggOverOuterRows(31) aggregateHasOnlyOuterRefs(20) evalAggOverOuterRowsWithInner(38) | 319 | SRP: aggregate evaluation. Future: `execquery/aggregate.go` |
| CX-04 | `select_agg_validate.go` | aggRefsMatchFromTable(20) aggRefsOuter(34) exprRefsOuterCol(23) subqueryOuterAggRef(23) validateDistinctAggArgs(57) aggHasColumnRef(16) selectHasCorrelatedAggSubquery(19) validateSelectExprs(43) validateSelectColumnRefs(75) validateSelectRowValues(18) | 328 | SRP: aggregate/outer-ref analysis. Future: `execquery/aggregate_validate.go` |
| CX-05 | `select_validate.go` | validateOrderByLength(48) validateExprSubqueriesCtxMode(31) validateExprSubqueriesInList(21) validateExprSubqueriesBinaryOp(24) validateRowValueUse(24) validateRowValueBinaryOp(22) validateDMLSubqueries(25) validateTriggerBodiesForDML(29) validateNoFromColumnRefs(64) validateExprOrderBy(40) validateCompoundOrderBy(55) | 383 | SRP: SELECT clause validation. Future: `execquery/validate.go` |
| CX-06 | `select_scan.go` | scanTableRows(63) scanTableAffinityCols(56) fastEvalComparison(27) filterSystemTables(18) buildRowMap(29) fillStructRowFromTypes(46) buildOutputRow(50) distinctRows(20) coveringIndexForDistinct(27) | 336 | SRP: table scan + row building. Future: `execquery/scan.go` |
| CX-07 | `select_expr.go` | findAggregateInExpr(38) exprHasSubquery(37) exprContainsSubquery(44) exprCollation(35) lastMinMaxInExpr(80) exprStructurallyEqual(21) walkExprFull(32) findRowIDRef(30) checkWhereCollations(26) | 343 | SRP: expression analysis helpers. Future: `execquery/expr.go` |
| CX-08 | `select_exec.go` + `select_columns.go` | execSelect(34) execSelectPrevalidate(24) finalizeSelectResult(47) execSelectViewWithOuter(54) execSelectNoFrom(57) finalizeNoFromSelect(31) execSelectOverMaterialized(64) execSelectCTE(32) selectFromRefersTo(16) cteAnchorColumnCount(18) execRecursiveCTE(56) ‖ tableColumnNames(20) compoundStarCount(24) compoundJoinColNames(16) buildQualifiedStarNames(21) qualifiedStarNamesFromRow(16) resolveColumnRefName(20) orderQualifiedNamesByDefs(19) pkColumnNames(23) lessRows(43) compareOrderByValues(27) | 731 | SRP: SELECT orchestration + column names. **Split to 2 files.** If budget exceeded, pause after exec paths; CX-08b resumes columns. Future: `execquery/select_core.go` + `execquery/columns.go` |

### Tier 1B — alter.go (48 violations → 3 goals)

| ID | Target file | Key functions (gocognit) | Σ | SOLID note |
|----|-------------|--------------------------|---|------------|
| CX-09 | `alter_rename.go` | execAlterTableRenameColumn(64) renameUpdateRelatedEntriesInSchema(42) renameColumnInCreateTableSQL(39) checkRenameAmbiguity(39) execAlterTableRename(37) renameSQLiteSequence(33) renameColumnInEntries(30) replaceTableNameInSQL(26) replaceColumnNameInSQL(24) quoteFixSQLWithColumns(24) renameColumnInViews(21) renameColumnInForeignKeys(18) | 397 | SRP: RENAME operations. Future: `execddl/alter_rename.go` |
| CX-10 | `alter_modify.go` | execAlterTableDrop(62) execAlterTableAdd(46) validateAddColumnConstraints(48) execAlterTableAlter(32) removeLeadingConstraintClause(63) removeConstraintFromSQL(37) removeColumnLevelConstraint(34) rebuildRowsAfterDrop(25) validateAddConstraint(21) addColumnToCreateTableSQL(23) addConstraintToCreateTableSQL(20) columnHasNull(21) validateGeneratedAddColumn(26) | 438 | SRP: ADD/DROP/ALTER + constraints. Future: `execddl/alter_modify.go` |
| CX-11 | `alter_deps.go` | rebuildCreateTableSQL(61) checkViewDependencies(47) exprReferencesColumn(47) collectExprRefs(46) collectExprTriggerColRefs(45) emptyINOperandSpans(78) emptyINBareOperandSpans(44) findTableRefsInTrigger(26) validateTriggerSQL(26) hasViewCircularRef(24) checkTriggerConflictCols(24) skipSQLWhitespaceAndComments(22) extractIdentifierTokens(22) indexReferencesColumn(22) createTableColumnNames(16) collectTriggerColRefs(20) collectSelectTriggerColRefs(16) checkIndexRenameDependencies(19) checkViewDropDependencies(18) checkTriggerTableRefs(18) isTempTrigger(17) validateViewSQL(16) constraintNameBefore(17) formatTableConstraint(26) | 808 | SRP: schema dependency analysis. **Largest goal — use skip-and-note aggressively.** Future: `execddl/alter_deps.go` |

### Tier 1C — other exec god-files (4 goals)

| ID | Target file | Key functions | Σ | SOLID note |
|----|-------------|---------------|---|------------|
| CX-12 | `insert_core.go` + `insert_conflict.go` | execInsertSelect(77) findRowByUniqueCols(74) execInsertView(74) replaceDeleteConflicts(59) execInsertDefault(56) checkUniqueIndex(52) validateLoadedTriggerSchemaCtx(50) insertRow(50) checkConstraints(48) execInsert(47) fireTrigger(46) execInsertOnConflict(46) evalReturningExprs(43) resolveInsertRowConstraints(41) ‖ validateCollationsInExpr(40) fireTriggers(40) evalTuple(40) maintainIndexesOnInsert(37) findRowByIndexCols(35) execInsertRow(35) buildInsertSelectValues(35) checkCompositeUnique(29) fireInsertRowBeforeTriggers(26) uniqueIndexColumns(23) computeGeneratedValues(23) validateLoadedTriggers(21) checkUniqueConstraints(21) checkConstraintText(20) pkRowID(19) buildRowMapFromValues(18) constraintNameBefore(17) isIgnoreableConflict(17) allTableIndexes(17) tableCheckConstraintText(16) execInsertSelectConflict(16) | ~1300 | **Split to 2 files; skip-and-note aggressively.** Future: `execdml/insert.go` |
| CX-13 | `expression_eval.go` + `expression_rowvalue.go` | evalBetween(67) evalQualifiedColumnRef(61) evalFuncCall(61) evalCastExpr(58) parseColumnDefs(54) evalInListSubqueryItem(54) evalInListOperand(49) sqliteCodePoints(37) evalUnqualifiedColumnRef(33) parseNumericPrefix(32) resolveRowValueSubqueries(32) subValues(31) addValues(31) likeMatchRunes(28) evalInListScalarItem(26) evalArithmeticOp(25) evalBinaryOpValues(24) sqliteRealValue(23) exprOutputAffinity(22) evalRowValueCaseWhens(22) evalRowValueCompare(21) evalBinaryOp(21) evalRowValueIs(20) evalMatchOp(19) evalNumericLit(18) findNextRowID(18) evalBinaryOpIs(17) stripHiddenToken(16) | ~1000 | **Split to 2 files.** Future: `execexpr/` |
| CX-14 | `ddl_core.go` + `ddl_trigger.go` | selectStmtToString(85) defaultContainsNonConstant(60) execDropTable(48) execCreateTrigger(45) hasBindParameter(40) virtualTableRows(39) validateTriggerSchemaRefs(38) findGeneratedColumnLoop(37) execAttach(34) createAutoIndexes(32) execFTSDelete(30) vtabUpperBound(29) exprToString(27) formatTableConstraint(26) execCreateView(25) windowDefToString(23) enforceStrictType(23) execDetach(22) execCreateTableAsSelect(21) insertStmtToString(20) checkTriggerSelectSchemaRefs(20) validateIndexKeyExpr(17) execFTSSelect(17) execCreateVirtualTable(17) updateStmtToString(16) joinClausesToString(16) validateIndexedBy(16) | ~900 | **Split to 2 files.** Future: `execddl/` |
| CX-15 | `update_split.go` | applyUpdateIgnore(71) joinUpdateFromRows(70) execUpdateView(67) applyUpdateWithTriggers(66) checkUpdateConflicts(54) execUpdate(50) collectUpdateChanges(43) checkUpdateConstraints(43) buildUpdateChange(42) mergeTriggerModifiedRow(26) applyUpdateReplace(20) | 552 | Future: `execdml/update.go` |

### Tier 1D — medium exec files (4 goals)

| ID | Target file | Key functions | Σ | SOLID note |
|----|-------------|---------------|---|------------|
| CX-16 | `parser_dispatch.go` | handleRule dispatch: replace 464-case switch chunks with **`ruleHandlers map[int]ruleHandler`** dispatch. Split parser.go 5755→multiple files. | ~600 | **OCP redesign**: map dispatch, not switch. See §2.4. ParseSQL(51) rewriteParenSet(27) splitSQLStatements(36) collectFuncCallOrderBy(36) mergeColumnConstraints(26) stripSQLComments(31) parseSavepointStatement(25) findUpdateParenSets(24) findSetRhsEnd(18) splitSelectList(17) findStatementOrderLimit(17) feedParserTokens(17) + handleRuleChunk* (each ~16-31) | | |
| CX-17 | `explain_plan.go` + `explain_index.go` | planJoin(61) planSingleTable(44) collectIndexedRefs(43) bestIndexForQuery(36) stat1RowCount(31) autoindexColumns(28) findBestCoveringIndex(26) indexCoversCols(25) planSubqueryNodes(23) subqueryReferencesOuter(19) readStatSZs(19) joinNodeFor(18) findIndexOnCols(18) findIndexOnColumn(17) findIndexOnExpr(16) | 424 | SRP: query planner. Future: `execquery/planner.go` |
| CX-18 | `pragma_dispatch.go` | execPragma(48): replace 33-case switch with **`pragmaHandlers map[string]Handler`** (OCP). computeIndexStat(38) computePKStat(31) analyzeOneTable(31) withoutRowidPKColumns(26) execPragmaIndexInfo(21) statLookup(20) clearStatsForIndex(20) withoutRowidAutoindexes(18) execAnalyze(17) | 270 | **OCP redesign**: pragma map dispatch. |
| CX-19 | `fk_constraint.go` + `engine_core.go` | fkCheckChildTable(79) fkParentActionRec(42) checkForeignKeyViolations(36) fkParentKeyValid(35) findFKViolations(27) fkParentRefAction(26) fkFindChildMatches(26) fkParentRowExists(25) fkChildRefs(21) fkParentKeyIndices(16) ‖ Exec(75) validateNoRaiseOutsideTrigger(73) checkSelectRaise(62) normalizeSQL(56) cloneStmtsWithValues(31) findTable(31) findView(18) detectExternalSchemaChanges(17) Prepare(16) | 636 | Split FK enforcement (future: `execconstraint/`) from Engine core. |

### Tier 1E — function packages (2 goals)

| ID | Target file | Key functions | Σ | SOLID note |
|----|-------------|---------------|---|------------|
| CX-20 | `function_core.go` (split function.go 3050→) | sqlitePrintf(70) sqliteFormatEscape(20) sqliteFormatInt(19) renderFloatG(18) fnAddIntType(18) sqliteFormatString(16) parseHhMmSs(16) fnUNISTR(16) fnINSTR(16) percentileAgg.Step(16) fnUNHEX(17) getDigits(22) isDate(28) strftimeFormat(49) fnTIMEDIFF(29) | ~410 | SRP: split scalar funcs by category (printf, string, numeric, datetime). |
| CX-21 | `datetime_split.go` (split datetime.go 1535→) | parseModifier(53) parseModifierNum(72) | 125 | SRP: modifier parsing separated from datetime computation. |

### Tier 1F — tools + small packages (4 goals)

| ID | Target file | Key functions | Σ | SOLID note |
|----|-------------|---------------|---|------------|
| CX-22 | `tcl_interp_split.go` | substitute(73) execCommand(52) cmdIf(37) cmdString(33) cmdFor(31) cmdWhile(23) cmdForeach(21) splitList(60) | 330 | SRP: split TCL interpreter by command type. |
| CX-23 | `tcl_expr_split.go` | parsePrimary(42) parseNumber(36) parseMultiplicative(24) parseEquality(20) parseComparison(20) | 142 | SRP: TCL expression parser nodes. |
| CX-24 | `golemon_split.go` | buildParseTables(142) ParseGrammar(101) parseDirective(73) computeFollowSets(55) generateTablesGo(47) computeFirstSets(28) buildLR0States(27) closure(22) parseRule(22) generateActionFunction(18) + yyFindShiftAction(23) ×3 | ~560 | SRP: split grammar parser from table generator. |
| CX-25 | `misc_complexity.go` | btree: splitLeaf(43) insertInteriorPage(24) DeleteCellsWhere(29) ‖ sql: readNumber(42) readString(18) readIdent(17) ExprString(45) WindowDef.String(22) ‖ rename: collectExprRange(52) collectRanges(38) collectColumnRefRange(20) ‖ fts: stemPorter(46) phraseInDoc(20) parseAnd(26) parsePrimary(18) collectTerms(17) ‖ util: CompareValuesCollateFn(55) stringCompareFn(18) translateRegexpUnicodeEscapes(17) ‖ value: CompareValuesCollate(16) ‖ tclparser: readBareWord(25) next(23) yyFindShiftAction(23) Parse(29) ‖ status: parseSkipMaps(45) discoverPackages(25) run(22) ‖ tclconvert: main(44) convertToJSON(16) ‖ frigodb: interpolateArgs(18) ‖ compare-benchmark: main(39) | ~800 | **Catch-all for remaining small-package violations.** Process package-by-package, checkpoint after each. |

### Tier 1G — verification (1 goal)

| ID | Description |
|----|-------------|
| CX-26 | **FINAL VERIFICATION**: run full `tools/quality_gate.sh` (no args) repo-wide. Must report 0 violations. Run full regression: `go test ./internal/... -count=1` + all 4 testgen packages. Collect any RESISTANT functions from handovers → create CX-27. |

---

## 7. Track 2 Catalog — SOLID Sub-Package Extraction

> Each goal creates one sub-package. **Prerequisite**: the corresponding
> Track 1 file(s) must exist. Ordered to minimize cross-dependencies.

| ID | Sub-package | Extract from (Track 1 files) | Key interface | SOLID note |
|----|-------------|------------------------------|---------------|------------|
| SOLID-01 | `internal/execpragma` | pragma_dispatch.go, pragma.go, pragma_table.go | `EngineState{ Get/Set pragma }` | OCP: `pragmaHandlers` map. Shrinks execPragma to 5-line dispatch. |
| SOLID-02 | `internal/execexpr` | expression_eval.go, expression_rowvalue.go, select_expr.go | `ExprContext{ EvalExpr, ResolveColumn, Collation }` | DIP: expression eval depends on ExprContext interface, not Engine. |
| SOLID-03 | `internal/execquery/join` | select_join.go, select_join_validate.go | `JoinContext{ ReadRows, FindIndex, EvalExpr }` | SRP: all JOIN logic in one focused package. |
| SOLID-04 | `internal/execquery/aggregate` | select_agg.go, select_agg_validate.go | `AggContext{ EvalExpr, OuterRows }` | SRP: aggregate evaluation isolated. |
| SOLID-05 | `internal/execquery/validate` | select_validate.go | `ValidateContext{ Schema, Columns }` | SRP: SELECT validation as pure checks. |
| SOLID-06 | `internal/execquery/scan` | select_scan.go | `ScanContext{ ReadRows, BuildRowMap }` | SRP: scanning isolated from orchestration. |
| SOLID-07 | `internal/execquery/core` | select_exec.go, select_columns.go | composes join/aggregate/validate/scan | Orchestration; delegates to sub-executors. |
| SOLID-08 | `internal/execquery/planner` | explain_plan.go, explain_index.go | `PlanContext{ Schema, Indexes, Stats }` | SRP: query planning isolated. |
| SOLID-09 | `internal/execdml/insert` | insert_core.go, insert_conflict.go | `DMLContext{ Schema, Triggers, Indexes }` | SRP: INSERT execution. |
| SOLID-10 | `internal/execdml/update` | update_split.go | `DMLContext` (shared) | SRP: UPDATE execution. |
| SOLID-11 | `internal/execddl` | ddl_core.go, ddl_trigger.go, alter_rename.go, alter_modify.go, alter_deps.go | `DDLContext{ Schema, Renamer }` | SRP: all DDL in one package. |
| SOLID-12 | `internal/execconstraint` | fk_constraint.go | `ConstraintContext{ Schema, Rows }` | SRP: FK/CHECK/UNIQUE enforcement. |
| SOLID-13 | `internal/exectrigger` | (trigger fields + firing from insert/update) | `TriggerContext{ Depth, Tables, NewRow, OldRow }` | SRP: trigger state management. State extraction from Engine. |
| SOLID-14 | `internal/execquery` | consolidates SOLID-03..08 under one parent | Facade; Engine delegates SELECT to `execquery.Execute` | Final SELECT sub-package. |
| SOLID-15 | Engine cleanup | engine.go (post-extraction) | — | Remove dead fields, slim to ~200 lines. Verify interfaces. |

---

## 8. Per-Task Contract (template for every goal)

Every goal's handover carries this contract. Copy verbatim.

```
SCOPE (closed — do not add functions):
  - <function1> (line N)
  - <function2> (line N)
  ...

TARGET FILE: <responsibility_file.go> (same package internal/exec/)
SOLID DESIGN: <what responsibility this file owns; future sub-package>

ANTI-LOOP PROTOCOL (§4):
  1. One function at a time: extract → verify complexity → regression → checkpoint
  2. Skip-and-note after 2 failed attempts: RESISTANT: <fn> (<reason>)
  3. Budget: 25 turns. Exceed → pause with handover.
  4. Never re-read the whole file. Use line anchors.

CHECKPOINT PROTOCOL (mandatory):
  After every 3-5 functions (or each concern):
    1. ./tools/quality_gate.sh <target_file>  (must pass for touched file)
    2. go build ./... && go test ./internal/exec/ ./internal/parse/ &&
       go test -tags testgen ./testgen/check/ ./testgen/fkey/ ./testgen/subquery/ ./testgen/rowvalue/
    3. git add -A && git commit -m "CX-NN.<step>: <what>"
    4. Update plan checkbox

VERIFY (goal completion):
  ./tools/quality_gate.sh <target_file> && \
  go build ./... && \
  go test -count=1 -timeout 120s ./internal/exec/ ./internal/parse/ && \
  go test -tags testgen -count=1 -timeout 120s ./testgen/check/ ./testgen/fkey/ ./testgen/subquery/ ./testgen/rowvalue/
```

---

## 9. Definition of Done

### Per-goal (CX-NN)
1. All listed functions are ≤15 gocognit AND ≤12 gocyclo, OR noted RESISTANT.
2. `./tools/quality_gate.sh <target_file>` passes (no output).
3. Target file ≤1000 lines.
4. Regression net green (build + internal tests + 4 testgen packages).
5. All work committed with `CX-NN.<step>` prefix; plan checkbox updated.
6. **SOLID review**: each extraction improves responsibility isolation (the
   file has one clear concern, not mixed responsibilities).

### Per-goal (SOLID-NN)
1–4 same as above.
5. `go test -run TestSOLID_ .` passes (layer map updated).
6. Sub-package has focused public API; no Engine god-object coupling.
7. Capability interface defined with compile-time LSP probe.
8. Engine field count **decreased** (state moved to sub-executor).
9. All work committed with `SOLID-NN.<step>` prefix.

### Campaign complete (CX-26)
1. `./tools/quality_gate.sh` (no args, repo-wide) → **0 violations**.
2. Every non-test `.go` file ≤1000 lines.
3. `go build ./...` + `go vet ./...` + `go test ./internal/...` green.
4. All testgen packages (check/fkey/subquery/rowvalue + broader sweep) green.
5. `go test -run TestSOLID_ .` green.
6. Engine struct <30 fields (from 84); `internal/exec/` <10000 lines (from 36622).
7. No RESISTANT functions outstanding (all resolved or re-attempted).

---

## 10. Goal Schedule

Goals are created in the goa goal system. Each runs with `freshContext: true`
and carries state ONLY via its handover note. The plan file (this document)
is the backlog: as the queue drains, create the next batch from the catalog.

**Execution order** (dependencies enforced by queue position):

```
Track 1 (CX-01 → CX-26):  complexity + file SRP — PRIORITY
  ├── CX-01..CX-08   select.go (sequential — same source file)
  ├── CX-09..CX-11   alter.go (sequential — same source file)
  ├── CX-12          insert.go
  ├── CX-13          expression.go
  ├── CX-14          ddl.go
  ├── CX-15          update.go
  ├── CX-16          parser.go
  ├── CX-17          explain.go
  ├── CX-18          pragma.go + pragma_table.go
  ├── CX-19          fk.go + engine.go
  ├── CX-20..CX-21   function/ packages
  ├── CX-22..CX-25   tools + small packages
  └── CX-26          FINAL VERIFICATION

Track 2 (SOLID-01 → SOLID-15):  sub-package extraction — AFTER Track 1
  ├── SOLID-01       execpragma (no deps on other sub-packages)
  ├── SOLID-02       execexpr (no deps on other sub-packages)
  ├── SOLID-03..SOLID-08  execquery/* (sequential)
  ├── SOLID-09..SOLID-10  execdml/* (sequential)
  ├── SOLID-11       execddl
  ├── SOLID-12       execconstraint
  ├── SOLID-13       exectrigger
  ├── SOLID-14       execquery consolidation
  └── SOLID-15       Engine cleanup
```

**Initial goal batch**: CX-01 through CX-12 (12 goals, covering the top 4
god-files). Remaining goals created from this catalog as the queue drains.

**Resume protocol** (for any interruption):
1. `git pull` — read this plan's status + the active goal's handover.
2. `git log --oneline -5` — see last committed checkpoint.
3. Continue from the first incomplete function in the scope.
4. Run the verify command to confirm current state.
