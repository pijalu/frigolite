// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar.
//
// Continuation of the per-rule grammar action handlers (split from
// parser_rules*.go to keep file sizes manageable).

package parse

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Rule 8: cmd ::= COMMIT|END trans_opt
func rule8(ruleNo int, p *Parser) interface{} {
	return &sql.CommitStmt{}

}

// Rule 9: cmd ::= ROLLBACK trans_opt
func rule9(ruleNo int, p *Parser) interface{} {
	return &sql.RollbackStmt{}

}

// Rule 13: create_table ::= createkw temp TABLE ifnotexists nm dbnm
func rule13(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
	if schema != "" {
		name = name + "." + schema
	}
	return &sql.CreateTableStmt{
		Name:        name,
		IfNotExists: getBool(getRHS(p, ruleNo, 4)),
		Temporary:   getBool(getRHS(p, ruleNo, 2)),
		Columns:     nil, // will be filled by create_table_args
	}

}

func rule14(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 15: ifnotexists ::=
func rule15(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 16: ifnotexists ::= IF NOT EXISTS
func rule16(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 17: temp ::= TEMP
func rule17(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 18: temp ::=
func rule18(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 19: create_table_args ::= LP columnlist conslist_opt RP table_option_set
func rule19(ruleNo int, p *Parser) interface{} {
	// This rule produces columns from a column definition list plus
	// table-level constraints (conslist_opt) and table options
	// (table_option_set). The create_table value isn't available here;
	// rule 359 combines them into the CreateTableStmt.
	cols := getColumnList(getRHS(p, ruleNo, 2))
	cons := getTableConstraints(getRHS(p, ruleNo, 3))
	opts := getTableOptions(getRHS(p, ruleNo, 5))
	return &createTableArgs{
		columns:      cols,
		constraints:  cons,
		withoutRowid: opts.withoutRowid,
		strict:       opts.strict,
	}

}

// Rule 20: create_table_args ::= AS select
func rule20(ruleNo int, p *Parser) interface{} {
	sel := getSelectStmt(getRHS(p, ruleNo, 2))
	if sel != nil {
		// Wrap in CreateTableStmt with AS SELECT
		createStmt := &sql.CreateTableStmt{
			AsSelect: sel,
		}
		return createStmt
	}
	return nil

}

// Rule 21: table_option_set ::=
func rule21(ruleNo int, p *Parser) interface{} {
	return &createTableArgs{}

}

func rule22(ruleNo int, p *Parser) interface{} {
	acc := getTableOptions(getRHS(p, ruleNo, 1))
	opt := getTableOptions(getRHS(p, ruleNo, 3))
	acc.withoutRowid = acc.withoutRowid || opt.withoutRowid
	acc.strict = acc.strict || opt.strict
	return acc

}

// Rule 23: table_option ::= WITHOUT nm
func rule23(ruleNo int, p *Parser) interface{} {
	// "WITHOUT ROWID" is the only valid WITHOUT option.
	return &createTableArgs{withoutRowid: true}

}

// Rule 24: table_option ::= nm
func rule24(ruleNo int, p *Parser) interface{} {
	// A bare table option name: STRICT is the only one supported.
	opt := getString(getRHS(p, ruleNo, 1))
	return &createTableArgs{strict: strings.EqualFold(opt, "STRICT")}

}

// Rule 25: columnname :: nm typemod
func rule25(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 1))
	typeName := getString(getRHS(p, ruleNo, 2))
	return sql.ColumnDef{Name: name, Type: typeName}

}

// Rule 26: typetoken ::=
func rule26(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 27: typetoken ::= typename LP signed RP
// e.g., TEXT(50), VARCHAR(255), DECIMAL(10)
func rule27(ruleNo int, p *Parser) interface{} {
	typeName := getString(getRHS(p, ruleNo, 1))
	return fmt.Sprintf("%s(%s)", typeName, getString(getRHS(p, ruleNo, 3)))

}

// Rule 28: typetoken ::= typename LP signed COMMA signed RP
// e.g., DECIMAL(10,2)
func rule28(ruleNo int, p *Parser) interface{} {
	typeName := getString(getRHS(p, ruleNo, 1))
	return fmt.Sprintf("%s(%s, %s)", typeName,
		getString(getRHS(p, ruleNo, 3)), getString(getRHS(p, ruleNo, 5)))

}

// Rule 29: typename ::= typename ID — multi-word type names.
// SQLite permits multi-word type names (e.g. "NATIONAL CHARACTER",
// "LONG INTEGER", "DOUBLE PRECISION"). The recursive rule accumulates
// each additional identifier into the type name, joined by a space.
func rule29(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2))

}

func rule32(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{ConstraintName: getString(getRHS(p, ruleNo, 2))}

}

// Rule 33: ccons ::= DEFAULT scantok term
func rule33(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 3))}

}

// Rule 34: ccons ::= DEFAULT LP expr RP
func rule34(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Default: getExpr(getRHS(p, ruleNo, 3))}

}

// Rule 35: ccons ::= DEFAULT PLUS scantok term
// SQLite keeps the leading plus in the dflt_value text (PRAGMA
// table_info shows "+4.0" for DEFAULT +4.0), so wrap the operand in a
// unary-plus expression rather than dropping the sign.
func rule35(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Default: &sql.UnaryOp{Operand: getExpr(getRHS(p, ruleNo, 4)), Operator: "+"}}

}

// Rule 36: ccons ::= DEFAULT MINUS scantok term
func rule36(ruleNo int, p *Parser) interface{} {
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
func rule37(ruleNo int, p *Parser) interface{} {
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

// isCurrentTimeKeyword reports whether name is CURRENT_TIME, CURRENT_DATE, or
// CURRENT_TIMESTAMP (case-insensitive).
func isCurrentTimeKeyword(name string) bool {
	switch strings.ToUpper(name) {
	case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
		return true
	}
	return false
}

// Rule 38: ccons ::= NOT NULL onconf
func rule38(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{NotNull: true}
	cd.OnConflict = strings.ToUpper(getString(getRHS(p, ruleNo, 3)))
	return cd

}

// Rule 39: ccons ::= PRIMARY KEY sortorder onconf autoinc
func rule39(ruleNo int, p *Parser) interface{} {
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

func rule40(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{Unique: true}
	cd.OnConflict = strings.ToUpper(getString(getRHS(p, ruleNo, 2)))
	return cd

}

// Rule 41: ccons ::= CHECK LP expr RP
func rule41(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Check: getExpr(getRHS(p, ruleNo, 3))}

}

// Rule 42: ccons ::= REFERENCES nm eidlist_opt refargs
func rule42(ruleNo int, p *Parser) interface{} {
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
func rule43(ruleNo int, p *Parser) interface{} {
	if d, ok := getRHS(p, ruleNo, 1).(string); ok {
		return sql.ColumnDef{References: " " + d}
	}
	return sql.ColumnDef{}

}

// Rule 44: ccons ::= COLLATE ids
func rule44(ruleNo int, p *Parser) interface{} {
	return sql.ColumnDef{Collate: getString(getRHS(p, ruleNo, 2))}

}

// Rule 45: generated ::= LP expr RP
func rule45(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 46: generated ::= LP expr RP ID
func rule46(ruleNo int, p *Parser) interface{} {
	return getExpr(getRHS(p, ruleNo, 2))

}

// Rule 47: autoinc ::=
func rule47(ruleNo int, p *Parser) interface{} {
	return false

}

func rule48(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 49: refargs ::= (empty)
func rule49(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 50: refargs ::= refargs refarg
// Accumulates FK reference actions as a space-separated string.
func rule50(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1)) + " " + getString(getRHS(p, ruleNo, 2))

}

// Rules 51-59: refarg — FK reference actions (ON DELETE CASCADE, etc.)
// Accumulated as strings into refargs.
func rule51(ruleNo int, p *Parser) interface{} {
	return "MATCH " + getString(getRHS(p, ruleNo, 2))
}

func rule52(ruleNo int, p *Parser) interface{} {
	return "ON INSERT " + getString(getRHS(p, ruleNo, 3))
}

func rule53(ruleNo int, p *Parser) interface{} {
	return "ON DELETE " + getString(getRHS(p, ruleNo, 3))
}

func rule54(ruleNo int, p *Parser) interface{} {
	return "ON UPDATE " + getString(getRHS(p, ruleNo, 3))
}

func rule55(ruleNo int, p *Parser) interface{} {
	return "SET NULL"
}

func rule56(ruleNo int, p *Parser) interface{} {
	return "SET DEFAULT"
}

func rule57(ruleNo int, p *Parser) interface{} {
	return "CASCADE"
}

func rule58(ruleNo int, p *Parser) interface{} {
	return "RESTRICT"
}

func rule59(ruleNo int, p *Parser) interface{} {
	return "NO ACTION"

}

// Rule 61: defer_subclause ::= DEFERRABLE init_deferred_pred_opt
func rule61(ruleNo int, p *Parser) interface{} {
	suffix := ""
	if d, ok := getRHS(p, ruleNo, 2).(string); ok {
		suffix = d
	}
	return "DEFERRABLE" + suffix

}

// Rule 62: init_deferred_pred_opt ::= (empty)
func rule62(ruleNo int, p *Parser) interface{} {
	return ""

}

func rule63(ruleNo int, p *Parser) interface{} {
	return " INITIALLY DEFERRED"

}

// Rule 64: init_deferred_pred_opt ::= INITIALLY IMMEDIATE
func rule64(ruleNo int, p *Parser) interface{} {
	return " INITIALLY IMMEDIATE"

}

// Rule 65: conslist_opt ::= (empty)
func rule65(ruleNo int, p *Parser) interface{} {
	return ([]sql.TableConstraint)(nil)

}

// Rule 66: tconscomma ::= COMMA
func rule66(ruleNo int, p *Parser) interface{} {
	return nil

}

// Rule 67: tcons ::= CONSTRAINT nm
func rule67(ruleNo int, p *Parser) interface{} {
	return sql.TableConstraint{Type: "", Name: getString(getRHS(p, ruleNo, 2))}

}

// Rule 68: tcons ::= PRIMARY KEY LP sortlist autoinc RP onconf
func rule68(ruleNo int, p *Parser) interface{} {
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
func rule69(ruleNo int, p *Parser) interface{} {
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

func rule70(ruleNo int, p *Parser) interface{} {
	return sql.TableConstraint{
		Type:       sql.ConstraintCheck,
		Expr:       getExpr(getRHS(p, ruleNo, 3)),
		OnConflict: getString(getRHS(p, ruleNo, 5)),
	}

}

// Rule 71: tcons ::= FOREIGN KEY LP eidlist RP REFERENCES nm eidlist_opt refargs defer_subclause_opt
func rule71(ruleNo int, p *Parser) interface{} {
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
func rule72(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 73: onconf ::=
func rule73(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 74: onconf ::= ON CONFLICT orconf
func rule74(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 3))

}

// Rule 75: orconf ::=
func rule75(ruleNo int, p *Parser) interface{} {
	return ""

}

// Rule 76: orconf ::= OR resolvel
func rule76(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 2))

}

// Rule 77: resolvel ::= IGNORE
func rule77(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

func rule78(ruleNo int, p *Parser) interface{} {
	return getString(getRHS(p, ruleNo, 1))

}

// Rule 79: cmd ::= DROP TABLE ifexists fullname
func rule79(ruleNo int, p *Parser) interface{} {
	ifExists := getBool(getRHS(p, ruleNo, 3))
	name := getString(getRHS(p, ruleNo, 4))
	return &sql.DropTableStmt{Name: name, IfExists: ifExists}

}

// Rule 80: ifexists ::= IF EXISTS
func rule80(ruleNo int, p *Parser) interface{} {
	return true

}

// Rule 81: ifexists ::=
func rule81(ruleNo int, p *Parser) interface{} {
	return false

}

// Rule 82: cmd ::= createkw temp VIEW ifnotexists nm dbnm eidlist_opt AS select
func rule82(ruleNo int, p *Parser) interface{} {
	name := getString(getRHS(p, ruleNo, 5))
	schema := getString(getRHS(p, ruleNo, 6)) // dbnm - optional schema
	if schema != "" {
		name = name + "." + schema
	}
	sel := getSelectStmt(getRHS(p, ruleNo, 9))
	cols := getStringList(getRHS(p, ruleNo, 7))
	return &sql.CreateViewStmt{
		Name:        name,
		Columns:     cols,
		Select:      sel,
		Temporary:   getBool(getRHS(p, ruleNo, 2)), // temp nonterminal: TEMP/TEMPORARY
		IfNotExists: getBool(getRHS(p, ruleNo, 4)), // ifnotexists nonterminal
	}

}
