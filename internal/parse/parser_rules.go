// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// This file holds the per-rule grammar action handlers. Each rule maps to its
// own handler function via ruleHandlers (OCP: adding a rule = adding one map
// entry + one handler function). Rules without an explicit handler fall
// through to handleRuleFallback.

package parse

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// ruleHandlers dispatches each grammar rule to its action handler.
// Rules absent from the map fall through to handleRuleFallback (pass-through).
var ruleHandlers = map[int]ruleHandler{
	0:   ruleExplainPlain,
	1:   ruleExplainExplainQueryPlan,
	2:   ruleCmdxCmd,
	3:   ruleCmdBeginTranstypeTransOpt,
	4:   ruleTranstypeEmpty,
	8:   ruleCmdCommitEndTransOpt,
	9:   ruleCmdRollbackTransOpt,
	13:  ruleCreateTableCreatekwTempTableIfnotexistsNmDbnm,
	14:  rule14,
	15:  ruleIfnotexists,
	16:  ruleIfnotexistsIfNotExists,
	17:  ruleTempTemp,
	18:  ruleTemp,
	19:  ruleCreateTableArgsLpColumnlistConslistOptRpTableOptionSet,
	20:  ruleCreateTableArgsAsSelect,
	21:  ruleTableOptionSet,
	22:  rule22,
	23:  ruleTableOptionWithoutNm,
	24:  ruleTableOptionNm,
	25:  rule25,
	26:  ruleTypetoken,
	27:  ruleTypetokenTypenameLpSignedRp,
	28:  ruleTypetokenTypenameLpSignedCommaSignedRp,
	29:  ruleTypenameTypenameIdMultiWordTypeNames,
	32:  rule32,
	33:  ruleCconsDefaultScantokTerm,
	34:  ruleCconsDefaultLpExprRp,
	35:  ruleCconsDefaultPlusScantokTerm,
	36:  ruleCconsDefaultMinusScantokTerm,
	37:  ruleCconsDefaultScantokId,
	38:  ruleCconsNotNullOnconf,
	39:  ruleCconsPrimaryKeySortorderOnconfAutoinc,
	40:  rule40,
	41:  ruleCconsCheckLpExprRp,
	42:  ruleCconsReferencesNmEidlistOptRefargs,
	43:  ruleCconsDeferSubclause,
	44:  ruleCconsCollateIds,
	45:  ruleGeneratedLpExprRp,
	46:  ruleGeneratedLpExprRpId,
	47:  ruleAutoinc,
	48:  rule48,
	49:  ruleRefargsEmpty,
	50:  ruleRefargsRefargsRefarg,
	51:  rule51,
	52:  rule52,
	53:  rule53,
	54:  rule54,
	55:  rule55,
	56:  rule56,
	57:  rule57,
	58:  rule58,
	59:  rule59,
	61:  ruleDeferSubclauseDeferrableInitDeferredPredOpt,
	62:  ruleInitDeferredPredOptEmpty,
	63:  rule63,
	64:  ruleInitDeferredPredOptInitiallyImmediate,
	65:  ruleConslistOptEmpty,
	66:  ruleTconscommaComma,
	67:  ruleTconsConstraintNm,
	68:  ruleTconsPrimaryKeyLpSortlistAutoincRpOnconf,
	69:  ruleTconsUniqueLpSortlistRpOnconf,
	70:  rule70,
	71:  ruleTconsForeignKeyLpEidlistRpReferencesNmEidlistOptRefargsDeferSubclN71,
	72:  ruleDeferSubclauseOpt,
	73:  ruleOnconf,
	74:  ruleOnconfOnConflictOrconf,
	75:  ruleOrconf,
	76:  ruleOrconfOrResolvel,
	77:  ruleResolvelIgnore,
	78:  rule78,
	79:  ruleCmdDropTableIfexistsFullname,
	80:  ruleIfexistsIfExists,
	81:  ruleIfexists,
	82:  ruleCmdCreatekwTempViewIfnotexistsNmDbnmEidlistOptAsSelect,
	83:  ruleCmdDropViewIfexistsFullname,
	84:  ruleCmdSelect,
	85:  rule85,
	86:  ruleSelectWithRecursiveWqlistSelectnowith,
	87:  ruleSelectSelectnowith,
	88:  ruleSelectnowithSelectnowithMultiselectOpOneselect,
	89:  ruleMultiselectOpUnion,
	90:  ruleMultiselectOpUnionAll,
	91:  ruleMultiselectOpExceptIntersect,
	92:  ruleOneselectSelectDistinctSelcollistFromWhereOptGroupbyOptHavingOptON92,
	93:  rule93,
	94:  ruleValuesValuesLpNexprlistRp,
	95:  ruleOneselectMvalues,
	96:  ruleMvaluesValuesCommaLpNexprlistRp,
	97:  ruleMvaluesMvaluesCommaLpNexprlistRp,
	98:  ruleDistinctDistinct,
	99:  ruleDistinctAll,
	100: ruleDistinct,
	102: rule102,
	103: ruleSelcollistSclpScanptStar,
	104: ruleSelcollistSclpScanptNmDotStar,
	105: ruleAsAsNm,
	106: ruleAs,
	107: ruleFrom,
	108: ruleFromFromSeltablist,
	109: ruleStlPrefixSeltablistJoinop,
	110: rule110,
	111: ruleSeltablistStlPrefixNmDbnmAsOnUsing,
	112: ruleSeltablistStlPrefixNmDbnmAsIndexedByOnUsing,
	113: ruleSeltablistStlPrefixNmDbnmLpExprlistRpAsOnUsing,
	114: ruleSeltablistStlPrefixLpSelectRpAsOnUsing,
	115: ruleSeltablistStlPrefixLpSeltablistRpAsOnUsing,
	116: ruleDbnm,
	117: ruleDbnmDotNm,
	118: rule118,
	119: ruleFullnameNmDotNm,
	121: ruleXfullnameNmDotNmSchemaQualifiedTableNameUsedBy,
	122: ruleXfullnameNmAsNmTableAliasTheValueIsThe,
	123: ruleXfullnameNmDotNmAsNm,
	124: ruleJoinopCommaJoin,
	125: ruleJoinopJoinKwJoin,
	126: ruleJoinopJoinKwNmJoin,
	127: rule127,
	128: ruleJoinopJoinKwNmJoinN128,
	129: ruleOnUsingUsingLpIdlistRpTheUsingColumnList,
	130: ruleOnUsing,
	131: ruleOnUsingN131,
	132: ruleIndexedByIndexedByNm,
	133: ruleIndexedByNotIndexed,
	134: ruleOrderbyOpt,
	135: rule135,
	136: ruleSortlistSortlistCommaExprSortorderNulls,
	137: ruleSortlistExprSortorderNulls,
	138: ruleSortorderAsc,
	139: ruleSortorderDesc,
	140: ruleSortorder,
	141: ruleNullsNullsFirst,
	142: ruleNullsNullsLast,
	143: rule143,
	144: ruleGroupbyOpt,
	145: ruleGroupbyOptGroupByNexprlist,
	146: ruleHavingOpt,
	147: ruleHavingOptHavingExpr,
	148: ruleLimitOpt,
	149: ruleLimitOptLimitExpr,
	150: ruleLimitOptLimitExprOffsetExpr,
	151: rule151,
	152: ruleCmdWithDeleteFromXfullnameIndexedOptWhereOptRet,
	153: ruleWhereOpt,
	154: ruleWhereOptWhereExpr,
	155: ruleWhereOptRet,
	156: rule156,
	157: ruleWhereOptRetReturningSelcollist,
	158: ruleWhereOptRetWhereExprReturningSelcollist,
	159: ruleCmdWithUpdateOrconfXfullnameIndexedOptSetSetlistFromWhereOptRet,
	160: ruleSetlistSetlistCommaNmEqExpr,
	162: ruleSetlistNmEqExpr,
	164: ruleCmdWithInsertCmdIntoXfullnameIdlistOptSelectUpsert,
	165: ruleCmdWithInsertCmdIntoXfullnameIdlistOptDefaultValuesReturning,
	166: ruleUpsert,
	167: ruleUpsertReturningSelcollist,
	168: ruleUpsertOnConflictLpSortlistRpWhereOpt,
	169: ruleUpsertOnConflictLpSortlistRpWhereOptDoNothingUpsert,
	170: ruleUpsertOnConflictDoNothingReturning,
	171: ruleUpsertOnConflictDoUpdateSetSetlistWhereOptReturning,
	172: ruleReturningReturningSelcollist,
	173: rule173,
	174: ruleInsertCmdReplace,
	175: ruleIdlistOpt,
	176: rule176,
	177: ruleIdlistIdlistCommaNm,
	178: ruleIdlistNm,
	179: ruleExprLpExprRp,
	180: ruleExprIdIndexedJoinKwColumnReference,
	181: ruleExprNmDotNmSchemaTable,
	182: ruleExprNmDotNmDotNmSchemaTableColumn,
	183: ruleTermNullFloatBlob,
	184: rule184,
	185: ruleTermInteger,
	186: ruleExprVariable,
	187: ruleExprExprCollateIdString,
	188: ruleExprCastLpExprAsTypetokenRp,
	189: ruleExprIdIndexedJoinKwLpDistinctExprlistRpFunctionCall,
	190: ruleExprIdIndexedJoinKwLpDistinctExprlistOrderBySortlistRp,
	191: ruleExprIdIndexedJoinKwLpStarRpFunctionStar,
	192: rule192,
	193: ruleExprIdIndexedJoinKwLpDistinctExprlistOrderBySortlistRpFilterOver,
	194: ruleExprIdIndexedJoinKwLpStarRpFilterOverWindowFunction,
	196: ruleExprLpExprlistCommaExprRpRowValueVector,
	197: ruleExprExprAndExpr,
	198: ruleExprExprOrExpr,
	199: ruleExprExprLtGtGeLeExpr,
	200: ruleExprExprEqNeExpr,
	201: rule201,
	202: ruleExprExprPlusMinusExpr,
	203: ruleExprExprStarSlashRemExpr,
	204: ruleExprExprConcatExpr,
	205: ruleLikeopNotLikeKwMatchTheNegatedFormOfA,
	206: rule206,
	207: rule207,
	208: rule208,
	209: ruleExprExprNotLikeopExprNotLikeNotGlob,
	210: ruleExprExprIsExpr,
	211: ruleExprExprIsNotExpr,
	212: ruleExprExprIsNotDistinctFromExpr6RhsSymbols,
	213: ruleExprExprIsDistinctFromExpr5RhsSymbols,
	214: ruleExprNotExpr,
	215: ruleExprBitnotExpr,
	216: ruleExprPlusMinusExprUnary,
	217: ruleExprExprPtrExprTheSqLiteAndJson,
	220: rule220,
	221: ruleInOpIn,
	222: ruleInOpNotIn,
	223: ruleExprExprInOpLpExprlistRp,
	224: ruleExprLpSelectRp,
	225: ruleExprExprInOpLpSelectRp,
	226: ruleExprExprInOpNmDbnmParenExprlist,
	227: ruleExprExistsLpSelectRp,
	228: rule228,
	229: ruleCaseExprlistCaseExprlistWhenExprThenExpr,
	230: ruleCaseExprlistWhenExprThenExpr,
	231: ruleCaseElseElseExpr,
	232: ruleCaseElse,
	233: ruleCaseOperand,
	234: ruleExprlist,
	235: ruleNexprlistNexprlistCommaExpr,
	236: rule236,
	237: ruleParenExprlist,
	238: ruleParenExprlistLpExprlistRp,
	239: ruleCmdCreatekwUniqueflagIndexIfnotexistsNmDbnmOnNmLpSortlistRpWhereOpt,
	242: ruleEidlistOpt,
	243: ruleEidlistOptLpEidlistRp,
	244: ruleEidlistEidlistCommaNmCollateSortorder,
	245: ruleEidlistNmCollateSortorder,
	248: rule248,
	249: ruleCmdVacuumIntoOpt,
	250: ruleCmdVacuumNmVinto,
	251: ruleIntoOptIntoExprTheVacuumIntoTargetTheRawValue,
	253: ruleCmdPragmaNmDbnm,
	254: ruleCmdPragmaNmDbnmPragmaValue,
	255: ruleCmdPragmaNmDbnmLpPragmaValueRp,
	256: ruleCmdPragmaNmDbnmEqMinusNum,
	257: ruleCmdPragmaNmDbnmLpPragmaValueRp, // shared with rule 255 (identical action)
	259: ruleMinusNumMinusNumber,
	260: ruleCmdCreatekwTriggerDeclBeginTriggerCmdListEnd,
	261: rule261,
	270: ruleTriggerCmdListTriggerCmdListTriggerCmdSemi,
	271: ruleTriggerCmdListTriggerCmdSemi,
	274: ruleTriggerCmdUpdateOrconfNmIndexedOptSetSetlistFromWhereOpt,
	275: rule275,
	276: ruleTriggerCmdDeleteFromXfullnameTridxbyWhereOptScanpt,
	277: ruleTriggerCmdScanptSelectScanpt,
	278: ruleExprRaiseLpIgnoreRp,
	279: rule279,
	280: rule280,
	281: rule281,
	282: rule282,
	283: ruleCmdDropTriggerIfexistsFullname,
	284: ruleCmdAttachDatabaseKwOptExprAsExprKeyOpt,
	285: ruleCmdDetachDatabaseKwOptExpr,
	288: ruleCmdReindex,
	289: ruleCmdReindexNmDbnmReindexWithAnOptionalSchema,
	290: ruleCmdAnalyze,
	291: ruleCmdAnalyzeNmDbnm,
	292: ruleCmdAlterTableFullnameRenameToNm,
	293: ruleCmdAlterAddCarglist,
	294: ruleAlterAddAlterTableFullnameAddKwcolumnOptNmTypetoken,
	295: ruleCmdAlterTableFullnameDropKwcolumnOptNm,
	296: ruleCmdAlterTableFullnameRenameKwcolumnOptNmToNm,
	297: rule297,
	298: rule298,
	299: rule299,
	300: ruleCmdAlterTableFullnameAddConstraintNmCheckLpExprRpOnconf,
	301: ruleCmdAlterTableFullnameAddCheckLpExprRpOnconf,
	302: ruleCmdCreateVtab,
	303: ruleCmdCreateVtabLpVtabarglistRp,
	304: ruleCreateVtabCreatekwVirtualTableIfnotexistsNmDbnmUsingNm,
	305: rule305,
	306: ruleTokenIdASingleVirtualTableArgumentToken,
	309: ruleWithWithWqlist,
	310: ruleWithWithRecursiveWqlist,
	311: ruleWqasAs,
	314: ruleWqitemWithnmEidlistOptWqasLpSelectRp,
	315: ruleWithnmNm,
	316: ruleWqlistWqitem,
	317: rule317,
	318: ruleWindowdefnListWindowdefnListCommaWindowdefn,
	319: ruleWindowdefnNmAsLpWindowRp,
	320: ruleWindowPartitionByNexprlistOrderbyOptFrameOpt,
	321: ruleWindowNmPartitionByNexprlistOrderbyOptFrameOpt,
	322: ruleWindowOrderBySortlistFrameOpt,
	323: ruleWindowNmOrderBySortlistFrameOpt,
	324: rule324,
	325: ruleFrameOpt,
	326: ruleFrameOptRangeOrRowsFrameBoundSFrameExcludeOpt,
	327: ruleFrameOptRangeOrRowsBetweenFrameBoundSAndFrameBoundEFrameExcludeOpt,
	328: ruleRangeOrRowsRangeRowsGroups,
	329: ruleFrameBoundSFrameBound,
	330: ruleFrameBoundSUnboundedPreceding,
	331: ruleFrameBoundEFrameBound,
	332: rule332,
	333: ruleFrameBoundExprPrecedingFollowing,
	334: ruleFrameBoundCurrentRow,
	335: ruleFrameExcludeOpt,
	336: ruleFrameExcludeOptExcludeFrameExclude,
	337: ruleFrameExcludeNoOthers,
	338: ruleFrameExcludeCurrentRow,
	339: ruleFrameExcludeGroupTies,
	340: rule340,
	341: ruleFilterOverFilterClauseOverClause,
	342: ruleFilterOverOverClause,
	343: ruleFilterOverFilterClause,
	344: ruleOverClauseOverLpWindowRp,
	345: ruleOverClauseOverNm,
	346: ruleFilterClauseFilterLpWhereExprRp,
	348: ruleInputCmdlist,
	349: ruleCmdlistCmdlistEcmd,
	350: ruleCmdlistEcmd,
	351: rule351,
	352: ruleEcmdCmdxSemi,
	353: ruleEcmdExplainCmdxSemiExplain,
	354: ruleTransOpt,
	355: ruleTransOptTransaction,
	359: ruleCmdCreateTableCreateTableArgs,
	360: ruleTableOptionSetTableOption,
	361: ruleColumnlistColumnlistCommaColumnnameCarglist,
	362: rule362,
	363: ruleNmIdIndexedJoinKw,
	364: ruleNmString,
	365: ruleTypetokenTypename,
	366: ruleTypenameIdString,
	369: ruleCarglistCarglistCcons,
	370: ruleCarglist,
	371: ruleCconsAsGenerated,
	372: ruleCconsGeneratedAlwaysAsGenerated,
	373: ruleCconsAsGeneratedN373,
	374: ruleConslistOptCommaConslist,
	375: rule375,
	376: ruleConslistTcons,
	377: ruleTconscommaEmpty,
	379: ruleResolvelRollbackAbortFail,
	380: ruleSelectnowithOneselectAlreadyHandledButKeepForPassThrough,
	381: ruleOneselectValues,
	383: ruleAsIdString,
	385: rule385,
	386: ruleExprTerm,
	387: rule387,
	389: rule389,
	395: rulePlusNumIntegerFloat,
	403: ruleVtabarglistVtabarg,
	404: ruleVtabarglistVtabarglistCommaVtabarg,
	405: ruleVtabargVtabargToken,
	409: ruleWith,
	410: ruleWindowdefnListWindowdefn,
	411: rule411,
}

// ruleHandler implements the action for a single grammar rule.
type ruleHandler func(ruleNo int, p *Parser) interface{}

// handleRule implements the action code for each grammar rule.
// Returns the semantic value for the LHS symbol.
func handleRule(ruleNo int, p *Parser, lookahead int, lookaheadToken interface{}) interface{} {
	if h, ok := ruleHandlers[ruleNo]; ok {
		return h(ruleNo, p)
	}
	return handleRuleFallback(ruleNo, p)
}

func getRHS(p *Parser, ruleNo, n int) interface{} {
	t := p.tables
	size := -t.RuleInfoNRhs[ruleNo]
	return p.stack[p.pos-size+n].Minor
}

func handleRuleFallback(ruleNo int, p *Parser) interface{} {
	if p.pos >= 1 {
		t := p.tables
		if ruleNo < len(t.RuleInfoNRhs) && t.RuleInfoNRhs[ruleNo] != 0 {
			return getRHS(p, ruleNo, 1)
		}
	}
	return nil
}

// Rule 0: explain ::= EXPLAIN
func ruleExplainPlain(ruleNo int, p *Parser) interface{} {
	return false // plain EXPLAIN (opcode dump)

}

// Rule 1: explain ::= EXPLAIN QUERY PLAN
func ruleExplainExplainQueryPlan(ruleNo int, p *Parser) interface{} {
	return true // EXPLAIN QUERY PLAN (plan output)

}

// Rule 2: cmdx ::= cmd
func ruleCmdxCmd(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 3: cmd ::= BEGIN transtype trans_opt
func ruleCmdBeginTranstypeTransOpt(ruleNo int, p *Parser) interface{} {
	// transtype is RHS element 2 (BEGIN is 1). Its Minor is the DEFERRED /
	// IMMEDIATE / EXCLUSIVE token (fallback passthrough) or nil for the empty
	// rule (parse.y L179-187: sqlite3BeginTransaction(pParse, Y) with
	// Y = TK_DEFERRED/TK_IMMEDIATE/TK_EXCLUSIVE).
	typ := strings.ToUpper(getString(getRHS(p, ruleNo, 2)))
	return &sql.BeginStmt{Type: typ}

}

// Rule 4: transtype ::= (empty)
func ruleTranstypeEmpty(ruleNo int, p *Parser) interface{} {
	return nil

}
