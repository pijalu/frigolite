// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Code in this file is ORGANIZED BY GRAMMAR FUNCTION, one file per family;
// rule numbers are go-lemon table indices (see parser_ruleids.go).
//
// Table-definition grammar shared by CREATE TABLE and ALTER TABLE ADD:
// column definitions, types, column constraints (ccons), table
// constraints (tcons), foreign-key refargs/refact, conflict resolution
// (onconf/orconf/resolvel), AUTOINCREMENT, generated columns, WITHOUT
// ROWID/STRICT table options, and eidlist (indexed-column lists).

package parse

import (
	"fmt"
	sql "github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// Rule 14: createkw ::= CREATE
func ruleCreatekw(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 15: ifnotexists ::=
func ruleIfnotexists(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 16: ifnotexists ::= IF NOT EXISTS
func ruleIfnotexistsIfNotExists(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 17: temp ::= TEMP
func ruleTempTemp(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 18: temp ::=
func ruleTemp(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 21: table_option_set ::=
func ruleTableOptionSet(ruleNo int, p *Parser) interface{} {
	return &createTableArgs{}

}

// Rule 22: table_option_set ::= table_option_set table_option
func ruleTableOptionSetList(ruleNo int, p *Parser) interface{} {
	acc := getTableOptions(getRHS(p, ruleNo, 1))
	opt := getTableOptions(getRHS(p, ruleNo, 3))
	acc.withoutRowid = acc.withoutRowid || opt.withoutRowid
	acc.strict = acc.strict || opt.strict
	return acc

}

// Rule 23: table_option ::= WITHOUT nm
func ruleTableOptionWithoutNm(ruleNo int, p *Parser) interface{} {
	// "WITHOUT ROWID" is the only valid WITHOUT option.
	return &createTableArgs{withoutRowid: true}

}

// Rule 24: table_option ::= nm
func ruleTableOptionNm(ruleNo int, p *Parser) interface{} {
	// A bare table option name: STRICT is the only one supported.
	opt := getString(getRHS(p, ruleNo, 1))
	return &createTableArgs{strict: strings.EqualFold(opt, "STRICT")}

}

// Rule 25: columnname ::= nm typetoken
func ruleColumnnameNmTypemod(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	typeName := getString(getRHS(p, ruleNo, 2))
	return sql.ColumnDef{Name: name, Type: typeName}

}

// Rule 26: typetoken ::=
func ruleTypetoken(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 27: typetoken ::= typename LP signed RP
// e.g., TEXT(50), VARCHAR(255), DECIMAL(10)
func ruleTypetokenTypenameLpSignedRp(ruleNo int, p *Parser) interface{} {
	typeName := getString(getRHS(p, ruleNo, 1))
	return fmt.Sprintf("%s(%s)", typeName, getString(getRHS(p, ruleNo, 3)))

}

// Rule 28: typetoken ::= typename LP signed COMMA signed RP
// e.g., DECIMAL(10,2)
func ruleTypetokenTypenameLpSignedCommaSignedRp(ruleNo int, p *Parser) interface{} {
	typeName := getString(getRHS(p, ruleNo, 1))
	return fmt.Sprintf("%s(%s, %s)", typeName,
		getString(getRHS(p, ruleNo, 3)), getString(getRHS(p, ruleNo, 5)))

}

// Rule 29: typename ::= typename ID — multi-word type names.
// SQLite permits multi-word type names (e.g. "NATIONAL CHARACTER",
// "LONG INTEGER", "DOUBLE PRECISION"). The recursive rule accumulates
// each additional identifier into the type name, joined by a space.
func ruleTypenameTypenameIdMultiWordTypeNames(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2))

}

// Rule 32: ccons ::= CONSTRAINT nm
func ruleCconsConstraintNm(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{ConstraintName: getString(getRHS(p, ruleNo, 2))}

}

// Rule 33: ccons ::= DEFAULT scantok term
func ruleCconsDefaultScantokTerm(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 3))}

}

// Rule 34: ccons ::= DEFAULT LP expr RP
func ruleCconsDefaultLpExprRp(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 3))}

}

// Rule 35: ccons ::= DEFAULT PLUS scantok term
// SQLite keeps the leading plus in the dflt_value text (PRAGMA
// table_info shows "+4.0" for DEFAULT +4.0), so wrap the operand in a
// unary-plus expression rather than dropping the sign.
func ruleCconsDefaultPlusScantokTerm(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Default: &sql.UnaryOp{Operand: getExpr(getRHS(p, ruleNo, 4)), Operator: "+"}}

}

// Rule 36: ccons ::= DEFAULT MINUS scantok term
func ruleCconsDefaultMinusScantokTerm(ruleNo int, p *Parser) interface{} {
	// Fold -9223372036854775808 into math.MinInt64 (SQLite special case),
	// mirroring the unary-minus handling in rule 216.
	if nl, ok := getExpr(getRHS(p, ruleNo, 4)).(*sql.NumericLit); ok && nl.Value == "9223372036854775808" {
		return sql.ColumnDef{Default: &sql.NumericLit{Value: "-9223372036854775808"}}
	}
	return sql.ColumnDef{Default: &sql.UnaryOp{Operand: getExpr(getRHS(p, ruleNo, 4)), Operator: "-"}}

}

// Rule 37: ccons ::= DEFAULT scantok ID
// SQLite's "DEFAULT ID" rule: an unquoted identifier becomes a string
// literal (sqlite3AddDefaultValue converts TK_ID to TK_STRING), except
// the unquoted keywords TRUE/FALSE which are boolean literals (1/0), and
// CURRENT_TIME / CURRENT_DATE / CURRENT_TIMESTAMP which are keyword
// literals evaluated at INSERT time (they become ColumnRefs so the
// expression evaluator's evalCurrentTimeKeyword returns the actual time).
func ruleCconsDefaultScantokId(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 3).(sql.Token); ok {
		if !tok.QuotedIdent && strings.EqualFold(tok.Value, "TRUE") {
			return sql.ColumnDef{Default: &sql.NumericLit{Value: "1"}}
		}
		if !tok.QuotedIdent && strings.EqualFold(tok.Value, "FALSE") {
			return sql.ColumnDef{Default: &sql.NumericLit{Value: "0"}}
		}
		if !tok.QuotedIdent && isCurrentTimeKeyword(tok.Value) {
			return sql.ColumnDef{Default: &sql.ColumnRef{Name: tok.Value}}
		}
		return sql.ColumnDef{Default: &sql.StringLit{Value: tok.Value}}
	}
	if s, ok := getRHS(p, ruleNo, 3).(string); ok {
		return sql.ColumnDef{Default: &sql.StringLit{Value: s}}
	}
	return sql.ColumnDef{}

}

// Rule 38: ccons ::= NOT NULL onconf
func ruleCconsNotNullOnconf(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{NotNull: true}
	cd.OnConflict = strings.ToUpper(getString(getRHS(p, ruleNo, 3)))
	return cd

}

// Rule 39: ccons ::= PRIMARY KEY sortorder onconf autoinc
func ruleCconsPrimaryKeySortorderOnconfAutoinc(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{PrimaryKey: true}
	cd.OnConflict = strings.ToUpper(getString(getRHS(p, ruleNo, 4)))
	// sortorder reduces to a string (rules 138-140: "DESC" for descending,
	// "ASC" or "" otherwise). A raw DESC token may appear in partial
	// parse states; either form means PRIMARY KEY DESC.
	switch so := getRHS(p, ruleNo, 3).(type) {
	case string:
		cd.PKDesc = so == "DESC"
	case sql.Token:
		cd.PKDesc = strings.EqualFold(so.Value, "DESC")
	}
	if getBool(getRHS(p, ruleNo, 5)) {
		cd.AutoInc = true
	}
	return cd

}

// Rule 40: ccons ::= UNIQUE onconf
func ruleCconsUniqueOnconf(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{Unique: true}
	cd.OnConflict = strings.ToUpper(getString(getRHS(p, ruleNo, 2)))
	return cd

}

// Rule 41: ccons ::= CHECK LP expr RP
func ruleCconsCheckLpExprRp(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Check: getExpr(getRHS(p, ruleNo, 3))}

}

// Rule 42: ccons ::= REFERENCES nm eidlist_opt refargs
func ruleCconsReferencesNmEidlistOptRefargs(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{References: getString(getRHS(p, ruleNo, 2))}
	if cols := getStringList(getRHS(p, ruleNo, 3)); len(cols) > 0 {
		cd.References += "(" + strings.Join(cols, ", ") + ")"
	}
	if ra := getString(getRHS(p, ruleNo, 4)); ra != "" {
		cd.References += " " + ra
	}
	return cd

}

// Rule 43: ccons ::= defer_subclause
// Produces a References marker carrying the DEFERRABLE clause so the
// merge can append it to a preceding REFERENCES constraint.
func ruleCconsDeferSubclause(ruleNo int, p *Parser) interface{} {
	if d, ok := getRHS(p, ruleNo, 1).(string); ok {
		return sql.ColumnDef{References: " " + d}
	}
	return sql.ColumnDef{}

}

// Rule 44: ccons ::= COLLATE ids
func ruleCconsCollateIds(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Collate: getString(getRHS(p, ruleNo, 2))}

}

// Rule 45: generated ::= LP expr RP
func ruleGeneratedLpExprRp(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 46: generated ::= LP expr RP ID
func ruleGeneratedLpExprRpId(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 47: autoinc ::=
func ruleAutoinc(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 48: autoinc ::= AUTOINCR
func ruleAutoincAutoincr(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 49: refargs ::= (empty)
func ruleRefargsEmpty(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 50: refargs ::= refargs refarg
// Accumulates FK reference actions as a space-separated string.
func ruleRefargsRefargsRefarg(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2))

}

// Rule 51: refarg ::= MATCH nm
// Rules 51-59: refarg — FK reference actions (ON DELETE CASCADE, etc.)
// Accumulated as strings into refargs.
func ruleRefargMatchNm(ruleNo int, p *Parser) interface{} {
	return "MATCH " + getString(getRHS(p, ruleNo, 2))
}

// Rule 52: refarg ::= ON INSERT refact
func ruleRefargOnInsertRefact(ruleNo int, p *Parser) interface{} {
	return "ON INSERT " + getString(getRHS(p, ruleNo, 3))
}

// Rule 53: refarg ::= ON DELETE refact
func ruleRefargOnDeleteRefact(ruleNo int, p *Parser) interface{} {
	return "ON DELETE " + getString(getRHS(p, ruleNo, 3))
}

// Rule 54: refarg ::= ON UPDATE refact
func ruleRefargOnUpdateRefact(ruleNo int, p *Parser) interface{} {
	return "ON UPDATE " + getString(getRHS(p, ruleNo, 3))
}

// Rule 55: refact ::= SET NULL
func ruleRefactSetNull(ruleNo int, p *Parser) interface{} {
	return "SET NULL"
}

// Rule 56: refact ::= SET DEFAULT
func ruleRefactSetDefault(ruleNo int, p *Parser) interface{} {
	return "SET DEFAULT"
}

// Rule 57: refact ::= CASCADE
func ruleRefactCascade(ruleNo int, p *Parser) interface{} {
	return "CASCADE"
}

// Rule 58: refact ::= RESTRICT
func ruleRefactRestrict(ruleNo int, p *Parser) interface{} {
	return "RESTRICT"
}

// Rule 59: refact ::= NO ACTION
func ruleRefactNoAction(ruleNo int, p *Parser) interface{} {
	return "NO ACTION"

}

// Rule 61: defer_subclause ::= DEFERRABLE init_deferred_pred_opt
func ruleDeferSubclauseDeferrableInitDeferredPredOpt(ruleNo int, p *Parser) interface{} {
	suffix := ""
	if d, ok := getRHS(p, ruleNo, 2).(string); ok {
		suffix = d
	}
	return "DEFERRABLE" + suffix

}

// Rule 62: init_deferred_pred_opt ::= (empty)
func ruleInitDeferredPredOptEmpty(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 63: init_deferred_pred_opt ::= INITIALLY DEFERRED
func ruleInitDeferredPredOptInitiallyDeferred(ruleNo int, p *Parser) interface{} {
	return " INITIALLY DEFERRED"

}

// Rule 64: init_deferred_pred_opt ::= INITIALLY IMMEDIATE
func ruleInitDeferredPredOptInitiallyImmediate(ruleNo int, p *Parser) interface{} {
	return " INITIALLY IMMEDIATE"

}

// Rule 65: conslist_opt ::= (empty)
func ruleConslistOptEmpty(ruleNo int, p *Parser) interface{} {
	return ([]sql.TableConstraint)(nil)

}

// Rule 66: tconscomma ::= COMMA
func ruleTconscommaComma(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 67: tcons ::= CONSTRAINT nm
func ruleTconsConstraintNm(ruleNo int, p *Parser) interface{} {
	return sql.TableConstraint{Type: "", Name: getString(getRHS(p, ruleNo, 2))}

}

// Rule 68: tcons ::= PRIMARY KEY LP sortlist autoinc RP onconf
func ruleTconsPrimaryKeyLpSortlistAutoincRpOnconf(ruleNo int, p *Parser) interface{} {
	sortlist := getOrderByList(getRHS(p, ruleNo, 4))
	if err := rejectNullsInSortlist(sortlist); err != nil {
		p.SemanticErr = err
		return nil
	}
	return sql.TableConstraint{
		Type:       sql.ConstraintPrimaryKey,
		Columns:    indexColumnsFromSortlist(getRHS(p, ruleNo, 4)),
		AutoInc:    getBool(getRHS(p, ruleNo, 5)),
		OnConflict: strings.ToUpper(getString(getRHS(p, ruleNo, 6))),
	}

}

// Rule 69: tcons ::= UNIQUE LP sortlist RP onconf
func ruleTconsUniqueLpSortlistRpOnconf(ruleNo int, p *Parser) interface{} {
	sortlist := getOrderByList(getRHS(p, ruleNo, 3))
	if err := rejectNullsInSortlist(sortlist); err != nil {
		p.SemanticErr = err
		return nil
	}
	return sql.TableConstraint{
		Type:       sql.ConstraintUnique,
		Columns:    indexColumnsFromSortlist(getRHS(p, ruleNo, 3)),
		OnConflict: strings.ToUpper(getString(getRHS(p, ruleNo, 5))),
	}

}

// Rule 70: tcons ::= CHECK LP expr RP onconf
func ruleTconsCheckOnconf(ruleNo int, p *Parser) interface{} {
	return sql.TableConstraint{
		Type:       sql.ConstraintCheck,
		Expr:       getExpr(getRHS(p, ruleNo, 3)),
		OnConflict: getString(getRHS(p, ruleNo, 5)),
	}

}

// Rule 71: tcons ::= FOREIGN KEY LP eidlist RP REFERENCES nm eidlist_opt refargs defer_subclause_opt
func ruleTconsForeignKey(ruleNo int, p *Parser) interface{} {
	refTable := getString(getRHS(p, ruleNo, 7))
	refCols := getStringList(getRHS(p, ruleNo, 8))
	refAction := ""
	if ra := getString(getRHS(p, ruleNo, 9)); ra != "" {
		refAction = strings.TrimSpace(ra)
	}
	deferred := false
	if d, ok := getRHS(p, ruleNo, 10).(string); ok {
		deferred = strings.Contains(strings.ToUpper(d), "DEFERRABLE") && strings.Contains(strings.ToUpper(d), "INITIALLY DEFERRED")
	}
	return sql.TableConstraint{
		Type:      sql.ConstraintForeignKey,
		Columns:   fkColumnsFromEidlist(getRHS(p, ruleNo, 4)),
		RefTable:  refTable,
		RefCols:   refCols,
		RefAction: refAction,
		Deferred:  deferred,
	}

}

// Rule 72: defer_subclause_opt ::=
func ruleDeferSubclauseOpt(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 73: onconf ::=
func ruleOnconf(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 74: onconf ::= ON CONFLICT orconf
func ruleOnconfOnConflictOrconf(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 3))

}

// Rule 75: orconf ::=
func ruleOrconf(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 76: orconf ::= OR resolvel
func ruleOrconfOrResolvel(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 77: resolvel ::= IGNORE
func ruleResolvelIgnore(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 78: resolvel ::= REPLACE
func ruleResolvelReplace(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 242: eidlist_opt ::=
func ruleEidlistOpt(ruleNo int, p *Parser) interface{} {
	return ([]string)(nil)

}

// Rule 243: eidlist_opt ::= LP eidlist RP
func ruleEidlistOptLpEidlistRp(ruleNo int, p *Parser) interface{} {
	return getStringList(getRHS(p, ruleNo, 2))

}

// Rule 244: eidlist ::= eidlist COMMA nm collate sortorder
func ruleEidlistEidlistCommaNmCollateSortorder(ruleNo int, p *Parser) interface{} {
	acc := getStringList(getRHS(p, ruleNo, 1))
	name := getString(getRHS(p, ruleNo, 3))
	// SQLite rejects a COLLATE clause or ASC/DESC in an identifier list
	// (eidlist), used by CREATE VIEW / CTE column lists and FK reference
	// lists (parserAddExprIdListTerm raises "syntax error after column
	// name"). The FIRST offending column wins (sqlite3ErrorMsg is only
	// honored while zErrMsg is still NULL), so do not overwrite an
	// already-set error. Schema reload (SchemaMode) skips the check: SQLite
	// accepts such lists when re-parsing stored sqlite_schema text.
	if getString(getRHS(p, ruleNo, 4)) != "" || getString(getRHS(p, ruleNo, 5)) != "" {
		if !p.SchemaMode && p.SemanticErr == nil {
			p.SemanticErr = fmt.Errorf("syntax error after column name %q", name)
		}
		return acc
	}
	return append(acc, name)

}

// Rule 245: eidlist ::= nm collate sortorder
func ruleEidlistNmCollateSortorder(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	if getString(getRHS(p, ruleNo, 2)) != "" || getString(getRHS(p, ruleNo, 3)) != "" {
		if !p.SchemaMode && p.SemanticErr == nil {
			p.SemanticErr = fmt.Errorf("syntax error after column name %q", name)
		}
		return []string{name}
	}
	return []string{name}

}

// Rule 359: cmd ::= create_table create_table_args
func ruleCmdCreateTableCreateTableArgs(ruleNo int, p *Parser) interface{} {
	ct, _ := getRHS(p, ruleNo, 1).(*sql.CreateTableStmt)
	args := getRHS(p, ruleNo, 2)
	if ct != nil {
		if cta, ok := args.(*createTableArgs); ok {
			ct.Columns = cta.columns
			ct.Constraints = cta.constraints
			ct.WithoutRowid = cta.withoutRowid
			ct.Strict = cta.strict
		} else if cols, ok := args.([]sql.ColumnDef); ok {
			ct.Columns = cols
		} else if ct2, ok := args.(*sql.CreateTableStmt); ok && ct2 != nil {
			ct.Columns = ct2.Columns
			ct.AsSelect = ct2.AsSelect
		}
	}
	if ct != nil {
		return ct
	}
	return getRHS(p, ruleNo, 1)

}

// Rule 360: table_option_set ::= table_option
func ruleTableOptionSetTableOption(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 361: columnlist ::= columnlist COMMA columnname carglist
func ruleColumnlistColumnlistCommaColumnnameCarglist(ruleNo int, p *Parser) interface{} {
	acc := getColumnList(getRHS(p, ruleNo, 1))
	col := getColumnDef(getRHS(p, ruleNo, 3))
	mergeColumnConstraints(&col, getColumnList(getRHS(p, ruleNo, 4)))
	return append(acc, col)

}

// Rule 362: columnlist ::= columnname carglist
func ruleColumnlistColumnnameCarglist(ruleNo int, p *Parser) interface{} {
	col := getColumnDef(getRHS(p, ruleNo, 1))
	mergeColumnConstraints(&col, getColumnList(getRHS(p, ruleNo, 2)))
	return []sql.ColumnDef{col}

}

// Rule 365: typetoken ::= typename
func ruleTypetokenTypename(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 366: typename ::= ID|STRING
func ruleTypenameIdString(ruleNo int, p *Parser) interface{} {
	if tok, ok := getRHS(p, ruleNo, 1).(sql.Token); ok {
		return tok.Value
	}
	return fmt.Sprintf("%v", getRHS(p, ruleNo, 1))

}

// Rule 369: carglist ::= carglist ccons
func ruleCarglistCarglistCcons(ruleNo int, p *Parser) interface{} {
	acc := getColumnList(getRHS(p, ruleNo, 1))
	if c, ok := getRHS(p, ruleNo, 2).(sql.ColumnDef); ok {
		acc = append(acc, c)
	}
	return acc

}

// Rule 370: carglist ::=
func ruleCarglist(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 371: ccons ::= AS generated
func ruleCconsAsGenerated(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 372: ccons ::= GENERATED ALWAYS AS generated
func ruleCconsGeneratedAlwaysAsGenerated(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 4))}

}

// Rule 373: ccons ::= AS generated
func ruleCconsAsGeneratedAlt(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Generated: getExpr(getRHS(p, ruleNo, 2))}

}

// Rule 374: conslist_opt ::= COMMA conslist
func ruleConslistOptCommaConslist(ruleNo int, p *Parser) interface{} {
	return getConstraintSlice(getRHS(p, ruleNo, 2))

}

// Rule 375: conslist ::= conslist tconscomma tcons
func ruleConslistListTconscommaTcons(ruleNo int, p *Parser) interface{} {
	acc := getConstraintSlice(getRHS(p, ruleNo, 1))
	tc, _ := getRHS(p, ruleNo, 3).(sql.TableConstraint)
	// Attach a preceding CONSTRAINT-name marker.
	if len(acc) > 0 && acc[len(acc)-1].Type == "" && tc.Type != "" {
		tc.Name = acc[len(acc)-1].Name
		acc = acc[:len(acc)-1]
	}
	if tc.Type != "" || tc.Name != "" {
		acc = append(acc, tc)
	}
	return acc

}

// Rule 376: conslist ::= tcons
func ruleConslistTcons(ruleNo int, p *Parser) interface{} {
	return getConstraintSlice(getRHS(p, ruleNo, 1))

}

// Rule 377: tconscomma ::= (empty)
func ruleTconscommaEmpty(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 379: resolvel ::= ROLLBACK|ABORT|FAIL
func ruleResolvelRollbackAbortFail(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}
