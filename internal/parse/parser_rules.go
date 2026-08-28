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
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// ruleHandlers dispatches each grammar rule to its action handler.
// Rules absent from the map fall through to handleRuleFallback (pass-through).
var ruleHandlers = map[int]ruleHandler{
	0:   rule0,
	1:   rule1,
	2:   rule2,
	3:   rule3,
	4:   rule4,
	8:   rule8,
	9:   rule9,
	13:  rule13,
	14:  rule14,
	15:  rule15,
	16:  rule16,
	17:  rule17,
	18:  rule18,
	19:  rule19,
	20:  rule20,
	21:  rule21,
	22:  rule22,
	23:  rule23,
	24:  rule24,
	25:  rule25,
	26:  rule26,
	27:  rule27,
	28:  rule28,
	29:  rule29,
	32:  rule32,
	33:  rule33,
	34:  rule34,
	35:  rule35,
	36:  rule36,
	37:  rule37,
	38:  rule38,
	39:  rule39,
	40:  rule40,
	41:  rule41,
	42:  rule42,
	43:  rule43,
	44:  rule44,
	45:  rule45,
	46:  rule46,
	47:  rule47,
	48:  rule48,
	49:  rule49,
	50:  rule50,
	51:  rule51,
	52:  rule52,
	53:  rule53,
	54:  rule54,
	55:  rule55,
	56:  rule56,
	57:  rule57,
	58:  rule58,
	59:  rule59,
	61:  rule61,
	62:  rule62,
	63:  rule63,
	64:  rule64,
	65:  rule65,
	66:  rule66,
	67:  rule67,
	68:  rule68,
	69:  rule69,
	70:  rule70,
	71:  rule71,
	72:  rule72,
	73:  rule73,
	74:  rule74,
	75:  rule75,
	76:  rule76,
	77:  rule77,
	78:  rule78,
	79:  rule79,
	80:  rule80,
	81:  rule81,
	82:  rule82,
	83:  rule83,
	84:  rule84,
	85:  rule85,
	86:  rule86,
	87:  rule87,
	88:  rule88,
	89:  rule89,
	90:  rule90,
	91:  rule91,
	92:  rule92,
	93:  rule93,
	94:  rule94,
	95:  rule95,
	96:  rule96,
	97:  rule97,
	98:  rule98,
	99:  rule99,
	100: rule100,
	102: rule102,
	103: rule103,
	104: rule104,
	105: rule105,
	106: rule106,
	107: rule107,
	108: rule108,
	109: rule109,
	110: rule110,
	111: rule111,
	112: rule112,
	113: rule113,
	114: rule114,
	115: rule115,
	116: rule116,
	117: rule117,
	118: rule118,
	119: rule119,
	121: rule121,
	122: rule122,
	123: rule123,
	124: rule124,
	125: rule125,
	126: rule126,
	127: rule127,
	128: rule128,
	129: rule129,
	130: rule130,
	131: rule131,
	132: rule132,
	133: rule133,
	134: rule134,
	135: rule135,
	136: rule136,
	137: rule137,
	138: rule138,
	139: rule139,
	140: rule140,
	141: rule141,
	142: rule142,
	143: rule143,
	144: rule144,
	145: rule145,
	146: rule146,
	147: rule147,
	148: rule148,
	149: rule149,
	150: rule150,
	151: rule151,
	152: rule152,
	153: rule153,
	154: rule154,
	155: rule155,
	156: rule156,
	157: rule157,
	158: rule158,
	159: rule159,
	160: rule160,
	162: rule162,
	164: rule164,
	165: rule165,
	166: rule166,
	167: rule167,
	168: rule168,
	169: rule169,
	170: rule170,
	171: rule171,
	172: rule172,
	173: rule173,
	174: rule174,
	175: rule175,
	176: rule176,
	177: rule177,
	178: rule178,
	179: rule179,
	180: rule180,
	181: rule181,
	182: rule182,
	183: rule183,
	184: rule184,
	185: rule185,
	186: rule186,
	187: rule187,
	188: rule188,
	189: rule189,
	190: rule190,
	191: rule191,
	192: rule192,
	193: rule193,
	194: rule194,
	196: rule196,
	197: rule197,
	198: rule198,
	199: rule199,
	200: rule200,
	201: rule201,
	202: rule202,
	203: rule203,
	204: rule204,
	205: rule205,
	206: rule206,
	207: rule207,
	208: rule208,
	209: rule209,
	210: rule210,
	211: rule211,
	212: rule212,
	213: rule213,
	214: rule214,
	215: rule215,
	216: rule216,
	217: rule217,
	220: rule220,
	221: rule221,
	222: rule222,
	223: rule223,
	224: rule224,
	225: rule225,
	226: rule226,
	227: rule227,
	228: rule228,
	229: rule229,
	230: rule230,
	231: rule231,
	232: rule232,
	233: rule233,
	234: rule234,
	235: rule235,
	236: rule236,
	237: rule237,
	238: rule238,
	239: rule239,
	242: rule242,
	243: rule243,
	244: rule244,
	245: rule245,
	248: rule248,
	249: rule249,
	253: rule253,
	254: rule254,
	255: rule255,
	256: rule256,
	257: rule255,
	259: rule259,
	260: rule260,
	261: rule261,
	270: rule270,
	271: rule271,
	274: rule274,
	275: rule275,
	276: rule276,
	277: rule277,
	278: rule278,
	279: rule279,
	280: rule280,
	281: rule281,
	282: rule282,
	283: rule283,
	284: rule284,
	285: rule285,
	288: rule288,
	289: rule289,
	290: rule290,
	291: rule291,
	292: rule292,
	293: rule293,
	294: rule294,
	295: rule295,
	296: rule296,
	297: rule297,
	298: rule298,
	299: rule299,
	300: rule300,
	301: rule301,
	302: rule302,
	303: rule303,
	304: rule304,
	305: rule305,
	306: rule306,
	309: rule309,
	310: rule310,
	311: rule311,
	314: rule314,
	315: rule315,
	316: rule316,
	317: rule317,
	318: rule318,
	319: rule319,
	320: rule320,
	321: rule321,
	322: rule322,
	323: rule323,
	324: rule324,
	325: rule325,
	326: rule326,
	327: rule327,
	328: rule328,
	329: rule329,
	330: rule330,
	331: rule331,
	332: rule332,
	333: rule333,
	334: rule334,
	335: rule335,
	336: rule336,
	337: rule337,
	338: rule338,
	339: rule339,
	340: rule340,
	341: rule341,
	342: rule342,
	343: rule343,
	344: rule344,
	345: rule345,
	346: rule346,
	348: rule348,
	349: rule349,
	350: rule350,
	351: rule351,
	352: rule352,
	353: rule353,
	354: rule354,
	355: rule355,
	359: rule359,
	360: rule360,
	361: rule361,
	362: rule362,
	363: rule363,
	364: rule364,
	365: rule365,
	366: rule366,
	369: rule369,
	370: rule370,
	371: rule371,
	372: rule372,
	373: rule373,
	374: rule374,
	375: rule375,
	376: rule376,
	377: rule377,
	379: rule379,
	380: rule380,
	381: rule381,
	383: rule383,
	385: rule385,
	386: rule386,
	387: rule387,
	389: rule389,
	395: rule395,
	403: rule403,
	404: rule404,
	405: rule405,
	409: rule409,
	410: rule410,
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

func rule0(ruleNo int, p *Parser) interface{} {
	return false // plain EXPLAIN (opcode dump)

}

// Rule 1: explain ::= EXPLAIN QUERY PLAN
func rule1(ruleNo int, p *Parser) interface{} {
	return true // EXPLAIN QUERY PLAN (plan output)

}

// Rule 2: cmdx ::= cmd
func rule2(ruleNo int, p *Parser) interface{} {
	return getRHS(p, ruleNo, 1)

}

// Rule 3: cmd ::= BEGIN transtype trans_opt
func rule3(ruleNo int, p *Parser) interface{} {
	// transtype is RHS element 2 (BEGIN is 1). Its Minor is the DEFERRED /
	// IMMEDIATE / EXCLUSIVE token (fallback passthrough) or nil for the empty
	// rule (parse.y L179-187: sqlite3BeginTransaction(pParse, Y) with
	// Y = TK_DEFERRED/TK_IMMEDIATE/TK_EXCLUSIVE).
	typ := strings.ToUpper(getString(getRHS(p, ruleNo, 2)))
	return &sql.BeginStmt{Type: typ}

}

// Rule 4: transtype ::= (empty)
func rule4(ruleNo int, p *Parser) interface{} {
	return nil

}

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
	cd.OnConflict = getString(getRHS(p, ruleNo, 3))
	return cd

}

// Rule 39: ccons ::= PRIMARY KEY sortorder onconf autoinc
func rule39(ruleNo int, p *Parser) interface{} {
	cd := sql.ColumnDef{PrimaryKey: true}
	cd.OnConflict = getString(getRHS(p, ruleNo, 4))
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
	cd.OnConflict = getString(getRHS(p, ruleNo, 2))
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
		OnConflict: getString(getRHS(p, ruleNo, 6)),
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
		OnConflict: getString(getRHS(p, ruleNo, 5)),
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
