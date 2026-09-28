// SPDX-License-Identifier: GPL-3.0-or-later
//
// # Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Rule IDs for every grammar rule with an explicit action handler.
// Numbers are go-lemon parse-table rule indices — the keys of the
// ruleHandlers map in parser_rules.go. They MUST match the shipped
// tables in sql_tables.go (yyRuleInfoLhs/yyRuleInfoNRhs); the trailing
// comments quote the grammar production (reconstructed where the
// original generator output was not preserved).
//
// Adding a rule: add one rid constant here, one handler function in
// the matching parser_rules_<family>.go file, and one map entry.
package parse

const (
	ridExplainPlain                                           = 0   // explain ::= EXPLAIN
	ridExplainExplainQueryPlan                                = 1   // explain ::= EXPLAIN QUERY PLAN
	ridCmdxCmd                                                = 2   // cmdx ::= cmd
	ridCmdBeginTranstypeTransOpt                              = 3   // cmd ::= BEGIN transtype trans_opt
	ridTranstypeEmpty                                         = 4   // transtype ::= (empty)
	ridCmdCommitEndTransOpt                                   = 8   // cmd ::= COMMIT|END trans_opt
	ridCmdRollbackTransOpt                                    = 9   // cmd ::= ROLLBACK trans_opt
	ridCreateTableCreatekwTempTableIfnotexistsNmDbnm          = 13  // create_table ::= createkw temp TABLE ifnotexists nm dbnm
	ridCreatekw                                               = 14  // createkw ::= CREATE
	ridIfnotexists                                            = 15  // ifnotexists ::=
	ridIfnotexistsIfNotExists                                 = 16  // ifnotexists ::= IF NOT EXISTS
	ridTempTemp                                               = 17  // temp ::= TEMP
	ridTemp                                                   = 18  // temp ::=
	ridCreateTableArgsLpColumnlistConslistOptRpTableOptionSet = 19  // create_table_args ::= LP columnlist conslist_opt RP table_option_set
	ridCreateTableArgsAsSelect                                = 20  // create_table_args ::= AS select
	ridTableOptionSet                                         = 21  // table_option_set ::=
	ridTableOptionSetList                                     = 22  // table_option_set ::= table_option_set table_option
	ridTableOptionWithoutNm                                   = 23  // table_option ::= WITHOUT nm
	ridTableOptionNm                                          = 24  // table_option ::= nm
	ridColumnnameNmTypemod                                    = 25  // columnname ::= nm typetoken
	ridTypetoken                                              = 26  // typetoken ::=
	ridTypetokenTypenameLpSignedRp                            = 27  // typetoken ::= typename LP signed RP
	ridTypetokenTypenameLpSignedCommaSignedRp                 = 28  // typetoken ::= typename LP signed COMMA signed RP
	ridTypenameTypenameIdMultiWordTypeNames                   = 29  // typename ::= typename ID — multi-word type names.
	ridCconsConstraintNm                                      = 32  // ccons ::= CONSTRAINT nm
	ridCconsDefaultScantokTerm                                = 33  // ccons ::= DEFAULT scantok term
	ridCconsDefaultLpExprRp                                   = 34  // ccons ::= DEFAULT LP expr RP
	ridCconsDefaultPlusScantokTerm                            = 35  // ccons ::= DEFAULT PLUS scantok term
	ridCconsDefaultMinusScantokTerm                           = 36  // ccons ::= DEFAULT MINUS scantok term
	ridCconsDefaultScantokId                                  = 37  // ccons ::= DEFAULT scantok ID
	ridCconsNotNullOnconf                                     = 38  // ccons ::= NOT NULL onconf
	ridCconsPrimaryKeySortorderOnconfAutoinc                  = 39  // ccons ::= PRIMARY KEY sortorder onconf autoinc
	ridCconsUniqueOnconf                                      = 40  // ccons ::= UNIQUE onconf
	ridCconsCheckLpExprRp                                     = 41  // ccons ::= CHECK LP expr RP
	ridCconsReferencesNmEidlistOptRefargs                     = 42  // ccons ::= REFERENCES nm eidlist_opt refargs
	ridCconsDeferSubclause                                    = 43  // ccons ::= defer_subclause
	ridCconsCollateIds                                        = 44  // ccons ::= COLLATE ids
	ridGeneratedLpExprRp                                      = 45  // generated ::= LP expr RP
	ridGeneratedLpExprRpId                                    = 46  // generated ::= LP expr RP ID
	ridAutoinc                                                = 47  // autoinc ::=
	ridAutoincAutoincr                                        = 48  // autoinc ::= AUTOINCR
	ridRefargsEmpty                                           = 49  // refargs ::= (empty)
	ridRefargsRefargsRefarg                                   = 50  // refargs ::= refargs refarg
	ridRefargMatchNm                                          = 51  // refarg ::= MATCH nm
	ridRefargOnInsertRefact                                   = 52  // refarg ::= ON INSERT refact
	ridRefargOnDeleteRefact                                   = 53  // refarg ::= ON DELETE refact
	ridRefargOnUpdateRefact                                   = 54  // refarg ::= ON UPDATE refact
	ridRefactSetNull                                          = 55  // refact ::= SET NULL
	ridRefactSetDefault                                       = 56  // refact ::= SET DEFAULT
	ridRefactCascade                                          = 57  // refact ::= CASCADE
	ridRefactRestrict                                         = 58  // refact ::= RESTRICT
	ridRefactNoAction                                         = 59  // refact ::= NO ACTION
	ridDeferSubclauseDeferrableInitDeferredPredOpt            = 61  // defer_subclause ::= DEFERRABLE init_deferred_pred_opt
	ridInitDeferredPredOptEmpty                               = 62  // init_deferred_pred_opt ::= (empty)
	ridInitDeferredPredOptInitiallyDeferred                   = 63  // init_deferred_pred_opt ::= INITIALLY DEFERRED
	ridInitDeferredPredOptInitiallyImmediate                  = 64  // init_deferred_pred_opt ::= INITIALLY IMMEDIATE
	ridConslistOptEmpty                                       = 65  // conslist_opt ::= (empty)
	ridTconscommaComma                                        = 66  // tconscomma ::= COMMA
	ridTconsConstraintNm                                      = 67  // tcons ::= CONSTRAINT nm
	ridTconsPrimaryKeyLpSortlistAutoincRpOnconf               = 68  // tcons ::= PRIMARY KEY LP sortlist autoinc RP onconf
	ridTconsUniqueLpSortlistRpOnconf                          = 69  // tcons ::= UNIQUE LP sortlist RP onconf
	ridTconsCheckOnconf                                       = 70  // tcons ::= CHECK LP expr RP onconf
	ridTconsForeignKey                                        = 71  // tcons ::= FOREIGN KEY LP eidlist RP REFERENCES nm eidlist_opt refargs defer_subclause_opt
	ridDeferSubclauseOpt                                      = 72  // defer_subclause_opt ::=
	ridOnconf                                                 = 73  // onconf ::=
	ridOnconfOnConflictOrconf                                 = 74  // onconf ::= ON CONFLICT orconf
	ridOrconf                                                 = 75  // orconf ::=
	ridOrconfOrResolvel                                       = 76  // orconf ::= OR resolvel
	ridResolvelIgnore                                         = 77  // resolvel ::= IGNORE
	ridResolvelReplace                                        = 78  // resolvel ::= REPLACE
	ridCmdDropTableIfexistsFullname                           = 79  // cmd ::= DROP TABLE ifexists fullname
	ridIfexistsIfExists                                       = 80  // ifexists ::= IF EXISTS
	ridIfexists                                               = 81  // ifexists ::=
	ridCmdCreateView                                          = 82  // cmd ::= createkw temp VIEW ifnotexists nm dbnm eidlist_opt AS select
	ridCmdDropViewIfexistsFullname                            = 83  // cmd ::= DROP VIEW ifexists fullname
	ridCmdSelect                                              = 84  // cmd ::= select
	ridSelectWithWqlistSelectnowith                           = 85  // select ::= WITH wqlist selectnowith
	ridSelectWithRecursiveWqlistSelectnowith                  = 86  // select ::= WITH RECURSIVE wqlist selectnowith
	ridSelectSelectnowith                                     = 87  // select ::= selectnowith
	ridSelectnowithSelectnowithMultiselectOpOneselect         = 88  // selectnowith ::= selectnowith multiselect_op oneselect
	ridMultiselectOpUnion                                     = 89  // multiselect_op ::= UNION
	ridMultiselectOpUnionAll                                  = 90  // multiselect_op ::= UNION ALL
	ridMultiselectOpExceptIntersect                           = 91  // multiselect_op ::= EXCEPT|INTERSECT
	ridOneselectCore                                          = 92  // oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt orderby_opt limit_opt
	ridOneselectCoreWindow                                    = 93  // oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt window_clause orderby_opt limit_opt
	ridValuesValuesLpNexprlistRp                              = 94  // values ::= VALUES LP nexprlist RP
	ridOneselectMvalues                                       = 95  // oneselect ::= mvalues
	ridMvaluesValuesCommaLpNexprlistRp                        = 96  // mvalues ::= values COMMA LP nexprlist RP
	ridMvaluesMvaluesCommaLpNexprlistRp                       = 97  // mvalues ::= mvalues COMMA LP nexprlist RP
	ridDistinctDistinct                                       = 98  // distinct ::= DISTINCT
	ridDistinctAll                                            = 99  // distinct ::= ALL
	ridDistinct                                               = 100 // distinct ::=
	ridSelcollistSclpCommaScanptExprAs                        = 102 // selcollist ::= sclp COMMA scanpt expr as
	ridSelcollistSclpScanptStar                               = 103 // selcollist ::= sclp scanpt STAR
	ridSelcollistSclpScanptNmDotStar                          = 104 // selcollist ::= sclp scanpt nm DOT STAR
	ridAsAsNm                                                 = 105 // as ::= AS nm
	ridAs                                                     = 106 // as ::=
	ridFrom                                                   = 107 // from ::=
	ridFromFromSeltablist                                     = 108 // from ::= FROM seltablist
	ridStlPrefixSeltablistJoinop                              = 109 // stl_prefix ::= seltablist joinop
	ridStlPrefixEmpty                                         = 110 // stl_prefix ::= (empty)
	ridSeltablistStlPrefixNmDbnmAsOnUsing                     = 111 // seltablist ::= stl_prefix nm dbnm as on_using
	ridSeltablistStlPrefixNmDbnmAsIndexedByOnUsing            = 112 // seltablist ::= stl_prefix nm dbnm as indexed_by on_using
	ridSeltablistStlPrefixNmDbnmLpExprlistRpAsOnUsing         = 113 // seltablist ::= stl_prefix nm dbnm LP exprlist RP as on_using
	ridSeltablistStlPrefixLpSelectRpAsOnUsing                 = 114 // seltablist ::= stl_prefix LP select RP as on_using
	ridSeltablistStlPrefixLpSeltablistRpAsOnUsing             = 115 // seltablist ::= stl_prefix LP seltablist RP as on_using
	ridDbnm                                                   = 116 // dbnm ::=
	ridDbnmDotNm                                              = 117 // dbnm ::= DOT nm
	ridFullnameNm                                             = 118 // fullname ::= nm
	ridFullnameNmDotNm                                        = 119 // fullname ::= nm DOT nm
	ridXfullnameNmDotNmSchemaQualifiedTableNameUsedBy         = 121 // xfullname ::= nm DOT nm (schema-qualified table name used by
	ridXfullnameNmAsNm                                        = 122 // xfullname ::= nm AS nm — table alias. The value is the
	ridXfullnameNmDotNmAsNm                                   = 123 // xfullname ::= nm DOT nm AS nm
	ridJoinopCommaJoin                                        = 124 // joinop ::= COMMA|JOIN
	ridJoinopJoinKwJoin                                       = 125 // joinop ::= JOIN_KW JOIN
	ridJoinopJoinKwNmJoin                                     = 126 // joinop ::= JOIN_KW nm JOIN
	ridJoinopJoinKwNmNmJoin                                   = 127 // joinop ::= JOIN_KW nm nm JOIN
	ridOnUsingOnExpr                                          = 128 // on_using ::= ON expr
	ridOnUsingUsing                                           = 129 // on_using ::= USING LP idlist RP — the USING column list.
	ridOnUsing                                                = 130 // on_using ::=
	ridScanpt                                                 = 131 // scanpt ::= (empty) — zero-width scan-position marker
	ridIndexedByIndexedByNm                                   = 132 // indexed_by ::= INDEXED BY nm
	ridIndexedByNotIndexed                                    = 133 // indexed_by ::= NOT INDEXED
	ridOrderbyOpt                                             = 134 // orderby_opt ::=
	ridOrderbyOptOrderBySortlist                              = 135 // orderby_opt ::= ORDER BY sortlist
	ridSortlistSortlistCommaExprSortorderNulls                = 136 // sortlist ::= sortlist COMMA expr sortorder nulls
	ridSortlistExprSortorderNulls                             = 137 // sortlist ::= expr sortorder nulls
	ridSortorderAsc                                           = 138 // sortorder ::= ASC
	ridSortorderDesc                                          = 139 // sortorder ::= DESC
	ridSortorder                                              = 140 // sortorder ::=
	ridNullsNullsFirst                                        = 141 // nulls ::= NULLS FIRST
	ridNullsNullsLast                                         = 142 // nulls ::= NULLS LAST
	ridNullsEmpty                                             = 143 // nulls ::= (empty)
	ridGroupbyOpt                                             = 144 // groupby_opt ::=
	ridGroupbyOptGroupByNexprlist                             = 145 // groupby_opt ::= GROUP BY nexprlist
	ridHavingOpt                                              = 146 // having_opt ::=
	ridHavingOptHavingExpr                                    = 147 // having_opt ::= HAVING expr
	ridLimitOpt                                               = 148 // limit_opt ::=
	ridLimitOptLimitExpr                                      = 149 // limit_opt ::= LIMIT expr
	ridLimitOptLimitExprOffsetExpr                            = 150 // limit_opt ::= LIMIT expr OFFSET expr
	ridLimitOptLimitExprCommaExpr                             = 151 // limit_opt ::= LIMIT expr COMMA expr
	ridCmdWithDeleteFromXfullnameIndexedOptWhereOptRet        = 152 // cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret
	ridWhereOpt                                               = 153 // where_opt ::=
	ridWhereOptWhereExpr                                      = 154 // where_opt ::= WHERE expr
	ridWhereOptRet                                            = 155 // where_opt_ret ::=
	ridWhereOptRetWhereExpr                                   = 156 // where_opt_ret ::= WHERE expr
	ridWhereOptRetReturningSelcollist                         = 157 // where_opt_ret ::= RETURNING selcollist
	ridWhereOptRetWhereExprReturningSelcollist                = 158 // where_opt_ret ::= WHERE expr RETURNING selcollist
	ridCmdUpdate                                              = 159 // cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt_ret
	ridSetlistSetlistCommaNmEqExpr                            = 160 // setlist ::= setlist COMMA nm EQ expr
	ridSetlistNmEqExpr                                        = 162 // setlist ::= nm EQ expr
	ridCmdWithInsertCmdIntoXfullnameIdlistOptSelectUpsert     = 164 // cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert
	ridCmdInsertDefaultValues                                 = 165 // cmd ::= with insert_cmd INTO xfullname idlist_opt DEFAULT VALUES returning
	ridUpsert                                                 = 166 // upsert ::=
	ridUpsertReturningSelcollist                              = 167 // upsert ::= RETURNING selcollist
	ridUpsertOnConflictLpSortlistRpWhereOpt                   = 168 // upsert ::= ON CONFLICT LP sortlist RP where_opt
	ridUpsertOnConflictLpSortlistRpWhereOptDoNothingUpsert    = 169 // upsert ::= ON CONFLICT LP sortlist RP where_opt DO NOTHING upsert
	ridUpsertOnConflictDoNothingReturning                     = 170 // upsert ::= ON CONFLICT DO NOTHING returning
	ridUpsertOnConflictDoUpdateSetSetlistWhereOptReturning    = 171 // upsert ::= ON CONFLICT DO UPDATE SET setlist where_opt returning
	ridReturningReturningSelcollist                           = 172 // returning ::= RETURNING selcollist
	ridInsertCmdOrResolvel                                    = 173 // insert_cmd ::= OR resolvel
	ridInsertCmdReplace                                       = 174 // insert_cmd ::= REPLACE
	ridIdlistOpt                                              = 175 // idlist_opt ::=
	ridIdlistOptLpIdlistRp                                    = 176 // idlist_opt ::= LP idlist RP
	ridIdlistIdlistCommaNm                                    = 177 // idlist ::= idlist COMMA nm
	ridIdlistNm                                               = 178 // idlist ::= nm
	ridExprLpExprRp                                           = 179 // expr ::= LP expr RP
	ridExprIdIndexedJoinKwColumnReference                     = 180 // expr ::= ID|INDEXED|JOIN_KW (column reference)
	ridExprNmDotNmSchemaTable                                 = 181 // expr ::= nm DOT nm (schema.table)
	ridExprNmDotNmDotNmSchemaTableColumn                      = 182 // expr ::= nm DOT nm DOT nm (schema.table.column)
	ridTermNullFloatBlob                                      = 183 // term ::= NULL|FLOAT|BLOB
	ridTermString                                             = 184 // term ::= STRING
	ridTermInteger                                            = 185 // term ::= INTEGER
	ridExprVariable                                           = 186 // expr ::= VARIABLE
	ridExprExprCollateIdString                                = 187 // expr ::= expr COLLATE ID|STRING
	ridExprCastLpExprAsTypetokenRp                            = 188 // expr ::= CAST LP expr AS typetoken RP
	ridExprFunc                                               = 189 // expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP (function call)
	ridExprFuncOrderBy                                        = 190 // expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist ORDER BY sortlist RP
	ridExprFuncStar                                           = 191 // expr ::= ID|INDEXED|JOIN_KW LP STAR RP (function(star))
	ridExprFuncFilterOver                                     = 192 // expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist RP filter_over
	ridExprFuncOrderByFilterOver                              = 193 // expr ::= ID|INDEXED|JOIN_KW LP distinct exprlist ORDER BY sortlist RP filter_over
	ridExprFuncStarFilterOver                                 = 194 // expr ::= ID|INDEXED|JOIN_KW LP STAR RP filter_over (window function)
	ridExprLpExprlistCommaExprRpRowValueVector                = 196 // expr ::= LP exprlist COMMA expr RP (row value / vector)
	ridExprExprAndExpr                                        = 197 // expr ::= expr AND expr
	ridExprExprOrExpr                                         = 198 // expr ::= expr OR expr
	ridExprExprLtGtGeLeExpr                                   = 199 // expr ::= expr LT|GT|GE|LE expr
	ridExprExprEqNeExpr                                       = 200 // expr ::= expr EQ|NE expr
	ridExprBitop                                              = 201 // expr ::= expr BITOP expr (& | << >>)
	ridExprExprPlusMinusExpr                                  = 202 // expr ::= expr PLUS|MINUS expr
	ridExprExprStarSlashRemExpr                               = 203 // expr ::= expr STAR|SLASH|REM expr
	ridExprExprConcatExpr                                     = 204 // expr ::= expr CONCAT expr
	ridLikeopNot                                              = 205 // likeop ::= NOT LIKE_KW|MATCH — the negated form of a
	ridExprLikeop                                             = 206 // expr ::= expr likeop expr (LIKE/GLOB/REGEXP/MATCH)
	ridExprLikeopEscape                                       = 207 // expr ::= expr likeop expr ESCAPE expr
	ridExprIsnullNotnull                                      = 208 // expr ::= expr ISNULL|NOTNULL
	ridExprExprNotLikeopExprNotLikeNotGlob                    = 209 // expr ::= expr NOT likeop expr (NOT LIKE / NOT GLOB /
	ridExprExprIsExpr                                         = 210 // expr ::= expr IS expr
	ridExprExprIsNotExpr                                      = 211 // expr ::= expr IS NOT expr
	ridExprExprIsNotDistinctFromExpr6RhsSymbols               = 212 // expr ::= expr IS NOT DISTINCT FROM expr (6 RHS symbols)
	ridExprExprIsDistinctFromExpr5RhsSymbols                  = 213 // expr ::= expr IS DISTINCT FROM expr (5 RHS symbols)
	ridExprNotExpr                                            = 214 // expr ::= NOT expr
	ridExprBitnotExpr                                         = 215 // expr ::= BITNOT expr
	ridExprPlusMinusExprUnary                                 = 216 // expr ::= PLUS|MINUS expr (unary)
	ridExprPtr                                                = 217 // expr ::= expr PTR expr — the SQLite '->' and '->>' JSON
	ridExprBetween                                            = 220 // expr ::= expr between_op expr AND expr
	ridInOpIn                                                 = 221 // in_op ::= IN
	ridInOpNotIn                                              = 222 // in_op ::= NOT IN
	ridExprExprInOpLpExprlistRp                               = 223 // expr ::= expr in_op LP exprlist RP
	ridExprLpSelectRp                                         = 224 // expr ::= LP select RP
	ridExprExprInOpLpSelectRp                                 = 225 // expr ::= expr in_op LP select RP
	ridExprExprInOpNmDbnmParenExprlist                        = 226 // expr ::= expr in_op nm dbnm paren_exprlist
	ridExprExistsLpSelectRp                                   = 227 // expr ::= EXISTS LP select RP
	ridExprCase                                               = 228 // expr ::= CASE case_operand case_exprlist case_else END
	ridCaseExprlistCaseExprlistWhenExprThenExpr               = 229 // case_exprlist ::= case_exprlist WHEN expr THEN expr
	ridCaseExprlistWhenExprThenExpr                           = 230 // case_exprlist ::= WHEN expr THEN expr
	ridCaseElseElseExpr                                       = 231 // case_else ::= ELSE expr
	ridCaseElse                                               = 232 // case_else ::=
	ridCaseOperand                                            = 233 // case_operand ::=
	ridExprlist                                               = 234 // exprlist ::=
	ridNexprlistNexprlistCommaExpr                            = 235 // nexprlist ::= nexprlist COMMA expr
	ridNexprlistExpr                                          = 236 // nexprlist ::= expr
	ridParenExprlist                                          = 237 // paren_exprlist ::=
	ridParenExprlistLpExprlistRp                              = 238 // paren_exprlist ::= LP exprlist RP
	ridCmdCreateIndex                                         = 239 // cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt
	ridEidlistOpt                                             = 242 // eidlist_opt ::=
	ridEidlistOptLpEidlistRp                                  = 243 // eidlist_opt ::= LP eidlist RP
	ridEidlistEidlistCommaNmCollateSortorder                  = 244 // eidlist ::= eidlist COMMA nm collate sortorder
	ridEidlistNmCollateSortorder                              = 245 // eidlist ::= nm collate sortorder
	ridCmdDropIndexIfexistsFullname                           = 248 // cmd ::= DROP INDEX ifexists fullname
	ridCmdVacuumIntoOpt                                       = 249 // cmd ::= VACUUM into_opt
	ridCmdVacuumNmVinto                                       = 250 // cmd ::= VACUUM nm vinto
	ridIntoOpt                                                = 251 // into_opt ::= INTO expr — the VACUUM INTO target. The raw value
	ridCmdPragmaNmDbnm                                        = 253 // cmd ::= PRAGMA nm dbnm
	ridCmdPragmaNmDbnmPragmaValue                             = 254 // cmd ::= PRAGMA nm dbnm = pragma_value
	ridCmdPragmaNmDbnmLpPragmaValueRp                         = 255 // cmd ::= PRAGMA nm dbnm LP minus_num RP
	ridCmdPragmaNmDbnmEqMinusNum                              = 256 // cmd ::= PRAGMA nm dbnm EQ minus_num
	ridCmdPragmaNmDbnmLpMinusNumRp                            = 257 // cmd ::= PRAGMA nm dbnm LP minus_num RP
	ridMinusNumMinusNumber                                    = 259 // minus_num ::= MINUS number
	ridCmdCreatekwTriggerDeclBeginTriggerCmdListEnd           = 260 // cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END
	ridTriggerDecl                                            = 261 // trigger_decl ::= TRIGGER ifnotexists nm dbnm trigger_time trigger_event ON nm ... when_clause (11 RHS symbols)
	ridTriggerCmdListTriggerCmdListTriggerCmdSemi             = 270 // trigger_cmd_list ::= trigger_cmd_list trigger_cmd SEMI
	ridTriggerCmdListTriggerCmdSemi                           = 271 // trigger_cmd_list ::= trigger_cmd SEMI
	ridTriggerCmdUpdate                                       = 274 // trigger_cmd ::= UPDATE orconf nm indexed_opt SET setlist from where_opt
	ridTriggerCmdInsert                                       = 275 // trigger_cmd ::= with insert_cmd INTO nm idlist_opt select upsert (INSERT inside a trigger body)
	ridTriggerCmdDeleteFromXfullnameTridxbyWhereOptScanpt     = 276 // trigger_cmd ::= DELETE FROM xfullname tridxby where_opt scanpt
	ridTriggerCmdScanptSelectScanpt                           = 277 // trigger_cmd ::= scanpt select scanpt
	ridExprRaiseLpIgnoreRp                                    = 278 // expr ::= RAISE LP IGNORE RP
	ridExprRaise                                              = 279 // expr ::= RAISE LP raisetype COMMA expr RP
	ridRaisetypeRollback                                      = 280 // raisetype ::= ROLLBACK
	ridRaisetypeAbort                                         = 281 // raisetype ::= ABORT
	ridRaisetypeFail                                          = 282 // raisetype ::= FAIL
	ridCmdDropTriggerIfexistsFullname                         = 283 // cmd ::= DROP TRIGGER ifexists fullname
	ridCmdAttachDatabaseKwOptExprAsExprKeyOpt                 = 284 // cmd ::= ATTACH database_kw_opt expr AS expr key_opt
	ridCmdDetachDatabaseKwOptExpr                             = 285 // cmd ::= DETACH database_kw_opt expr
	ridCmdReindex                                             = 288 // cmd ::= REINDEX
	ridCmdReindexNmDbnmReindexWithAnOptionalSchema            = 289 // cmd ::= REINDEX nm dbnm — REINDEX with an optional schema
	ridCmdAnalyze                                             = 290 // cmd ::= ANALYZE
	ridCmdAnalyzeNmDbnm                                       = 291 // cmd ::= ANALYZE nm dbnm
	ridCmdAlterTableFullnameRenameToNm                        = 292 // cmd ::= ALTER TABLE fullname RENAME TO nm
	ridCmdAlterAddCarglist                                    = 293 // cmd ::= alter_add carglist
	ridAlterAddAlterTableFullnameAddKwcolumnOptNmTypetoken    = 294 // alter_add ::= ALTER TABLE fullname ADD kwcolumn_opt nm typetoken
	ridCmdAlterTableFullnameDropKwcolumnOptNm                 = 295 // cmd ::= ALTER TABLE fullname DROP kwcolumn_opt nm
	ridCmdAlterTableFullnameRenameKwcolumnOptNmToNm           = 296 // cmd ::= ALTER TABLE fullname RENAME kwcolumn_opt nm TO nm
	ridCmdAlterDropConstraint                                 = 297 // cmd ::= ALTER TABLE fullname DROP CONSTRAINT nm
	ridCmdAlterColumnDropNotNull                              = 298 // cmd ::= ALTER TABLE fullname ALTER COLUMN nm DROP NOT NULL
	ridCmdAlterColumnSetNotNull                               = 299 // cmd ::= ALTER TABLE fullname ALTER COLUMN nm SET NOT NULL
	ridCmdAlterAddConstraintCheck                             = 300 // cmd ::= ALTER TABLE fullname ADD CONSTRAINT nm CHECK LP expr RP onconf
	ridCmdAlterTableFullnameAddCheckLpExprRpOnconf            = 301 // cmd ::= ALTER TABLE fullname ADD CHECK LP expr RP onconf
	ridCmdCreateVtab                                          = 302 // cmd ::= create_vtab
	ridCmdCreateVtabLpVtabarglistRp                           = 303 // cmd ::= create_vtab LP vtabarglist RP
	ridCreateVtab                                             = 304 // create_vtab ::= createkw VIRTUAL TABLE ifnotexists nm dbnm USING nm
	ridVtabargEmpty                                           = 305 // vtabarg ::= (empty)
	ridTokenIdASingleVirtualTableArgumentToken                = 306 // token ::= ID (a single virtual-table argument token)
	ridWithWithWqlist                                         = 309 // with ::= WITH wqlist
	ridWithWithRecursiveWqlist                                = 310 // with ::= WITH RECURSIVE wqlist
	ridWqasAs                                                 = 311 // wqas ::= AS
	ridWqitemWithnmEidlistOptWqasLpSelectRp                   = 314 // wqitem ::= withnm eidlist_opt wqas LP select RP
	ridWithnmNm                                               = 315 // withnm ::= nm
	ridWqlistWqitem                                           = 316 // wqlist ::= wqitem
	ridWqlistListCommaWqitem                                  = 317 // wqlist ::= wqlist COMMA wqitem
	ridWindowdefnListWindowdefnListCommaWindowdefn            = 318 // windowdefn_list ::= windowdefn_list COMMA windowdefn
	ridWindowdefnNmAsLpWindowRp                               = 319 // windowdefn ::= nm AS LP window RP
	ridWindowPartitionByNexprlistOrderbyOptFrameOpt           = 320 // window ::= PARTITION BY nexprlist orderby_opt frame_opt
	ridWindowNmPartitionByNexprlistOrderbyOptFrameOpt         = 321 // window ::= nm PARTITION BY nexprlist orderby_opt frame_opt
	ridWindowOrderBySortlistFrameOpt                          = 322 // window ::= ORDER BY sortlist frame_opt
	ridWindowNmOrderBySortlistFrameOpt                        = 323 // window ::= nm ORDER BY sortlist frame_opt
	ridWindowNmFrameOpt                                       = 324 // window ::= nm frame_opt
	ridFrameOpt                                               = 325 // frame_opt ::=
	ridFrameOptRangeOrRowsFrameBoundSFrameExcludeOpt          = 326 // frame_opt ::= range_or_rows frame_bound_s frame_exclude_opt
	ridFrameOptBetween                                        = 327 // frame_opt ::= range_or_rows BETWEEN frame_bound_s AND frame_bound_e frame_exclude_opt
	ridRangeOrRowsRangeRowsGroups                             = 328 // range_or_rows ::= RANGE|ROWS|GROUPS
	ridFrameBoundSFrameBound                                  = 329 // frame_bound_s ::= frame_bound
	ridFrameBoundSUnboundedPreceding                          = 330 // frame_bound_s ::= UNBOUNDED PRECEDING
	ridFrameBoundEFrameBound                                  = 331 // frame_bound_e ::= frame_bound
	ridFrameBoundEUnboundedFollowing                          = 332 // frame_bound_e ::= UNBOUNDED FOLLOWING
	ridFrameBoundExprPrecedingFollowing                       = 333 // frame_bound ::= expr PRECEDING|FOLLOWING
	ridFrameBoundCurrentRow                                   = 334 // frame_bound ::= CURRENT ROW
	ridFrameExcludeOpt                                        = 335 // frame_exclude_opt ::=
	ridFrameExcludeOptExcludeFrameExclude                     = 336 // frame_exclude_opt ::= EXCLUDE frame_exclude
	ridFrameExcludeNoOthers                                   = 337 // frame_exclude ::= NO OTHERS
	ridFrameExcludeCurrentRow                                 = 338 // frame_exclude ::= CURRENT ROW
	ridFrameExcludeGroupTies                                  = 339 // frame_exclude ::= GROUP|TIES
	ridWindowClause                                           = 340 // window_clause ::= WINDOW windowdefn_list
	ridFilterOverFilterClauseOverClause                       = 341 // filter_over ::= filter_clause over_clause
	ridFilterOverOverClause                                   = 342 // filter_over ::= over_clause
	ridFilterOverFilterClause                                 = 343 // filter_over ::= filter_clause
	ridOverClauseOverLpWindowRp                               = 344 // over_clause ::= OVER LP window RP
	ridOverClauseOverNm                                       = 345 // over_clause ::= OVER nm
	ridFilterClauseFilterLpWhereExprRp                        = 346 // filter_clause ::= FILTER LP WHERE expr RP
	ridInputCmdlist                                           = 348 // input ::= cmdlist
	ridCmdlistCmdlistEcmd                                     = 349 // cmdlist ::= cmdlist ecmd
	ridCmdlistEcmd                                            = 350 // cmdlist ::= ecmd
	ridEcmdSemi                                               = 351 // ecmd ::= SEMI
	ridEcmdCmdxSemi                                           = 352 // ecmd ::= cmdx SEMI
	ridEcmdExplainCmdxSemiExplain                             = 353 // ecmd ::= explain cmdx SEMI (EXPLAIN)
	ridTransOpt                                               = 354 // trans_opt ::=
	ridTransOptTransaction                                    = 355 // trans_opt ::= TRANSACTION
	ridCmdCreateTableCreateTableArgs                          = 359 // cmd ::= create_table create_table_args
	ridTableOptionSetTableOption                              = 360 // table_option_set ::= table_option
	ridColumnlistColumnlistCommaColumnnameCarglist            = 361 // columnlist ::= columnlist COMMA columnname carglist
	ridColumnlistColumnnameCarglist                           = 362 // columnlist ::= columnname carglist
	ridNmIdIndexedJoinKw                                      = 363 // nm ::= ID|INDEXED|JOIN_KW
	ridNmString                                               = 364 // nm ::= STRING
	ridTypetokenTypename                                      = 365 // typetoken ::= typename
	ridTypenameIdString                                       = 366 // typename ::= ID|STRING
	ridCarglistCarglistCcons                                  = 369 // carglist ::= carglist ccons
	ridCarglist                                               = 370 // carglist ::=
	ridCconsAsGenerated                                       = 371 // ccons ::= AS generated
	ridCconsGeneratedAlwaysAsGenerated                        = 372 // ccons ::= GENERATED ALWAYS AS generated
	ridCconsAsGeneratedAlt                                    = 373 // ccons ::= AS generated
	ridConslistOptCommaConslist                               = 374 // conslist_opt ::= COMMA conslist
	ridConslistListTconscommaTcons                            = 375 // conslist ::= conslist tconscomma tcons
	ridConslistTcons                                          = 376 // conslist ::= tcons
	ridTconscommaEmpty                                        = 377 // tconscomma ::= (empty)
	ridResolvelRollbackAbortFail                              = 379 // resolvel ::= ROLLBACK|ABORT|FAIL
	ridSelectnowithOneselect                                  = 380 // selectnowith ::= oneselect (already handled, but keep for pass-through)
	ridOneselectValues                                        = 381 // oneselect ::= values
	ridAsIdString                                             = 383 // as ::= ID|STRING
	ridReturningEmpty                                         = 385 // returning ::= (empty)
	ridExprTerm                                               = 386 // expr ::= term
	ridLikeopKeywords                                         = 387 // likeop ::= LIKE|GLOB|MATCH|REGEXP (single keyword form)
	ridExprlistExpr                                           = 389 // exprlist ::= expr
	ridPlusNumIntegerFloat                                    = 395 // plus_num ::= INTEGER|FLOAT
	ridVtabarglistVtabarg                                     = 403 // vtabarglist ::= vtabarg
	ridVtabarglistVtabarglistCommaVtabarg                     = 404 // vtabarglist ::= vtabarglist COMMA vtabarg
	ridVtabargVtabargToken                                    = 405 // vtabarg ::= vtabarg token
	ridWith                                                   = 409 // with ::=
	ridWindowdefnListWindowdefn                               = 410 // windowdefn_list ::= windowdefn
	ridWindowFrameOpt                                         = 411 // window ::= frame_opt
)
