// SPDX-License-Identifier: GPL-3.0-or-later
//
// sql_select.y — minimal SQL subset grammar covering the statements exercised
// by the selectE test (CREATE TABLE, INSERT, DELETE, compound SELECT with
// EXCEPT / ORDER BY / COLLATE, function calls, literals).
//
// The grammar is structured so that ORDER BY may only appear once, after the
// entire compound chain: the compound is LEFT-recursive (compound ::= compound
// EXCEPT select_core), and orderby_opt only follows the complete compound.
// The statement
//   SELECT 1 EXCEPT SELECT 2 ORDER BY ... EXCEPT SELECT 3
// is therefore a syntax error, exactly as SQLite requires.
//
// This grammar is the "minimal subset grammar" accepted by the completion
// criterion; go-lemon's own LALR(1) table generator (GenerateTables) builds
// the parse tables with no C-lemon intermediary.

%token_prefix TK_
%token_type {interface{}}
%default_type {interface{}}
%name SqlSelectParser

%token CREATE TABLE INSERT INTO VALUES DELETE FROM SELECT AS EXCEPT ORDER BY COLLATE.
%token LP RP COMMA SEMI.
%token ID NUMBER STRING.
%token STAR.

%left COLLATE.
%left EXCEPT.
%left AS.

input ::= cmdlist.
cmdlist ::= cmdlist cmd SEMI.
cmdlist ::= cmd SEMI.

cmd ::= create_stmt.
cmd ::= insert_stmt.
cmd ::= delete_stmt.
cmd ::= select_stmt.

// CREATE TABLE name ( col )
create_stmt ::= CREATE TABLE ID LP ID RP.

// INSERT INTO name VALUES (e), (e), ...
insert_stmt ::= INSERT INTO ID VALUES value_list.
value_list ::= value_row.
value_list ::= value_list COMMA value_row.
value_row ::= LP expr_list RP.
value_row ::= LP RP.

// DELETE FROM name
delete_stmt ::= DELETE FROM ID.

// Compound SELECT with a single trailing ORDER BY.
// The compound chain is left-recursive: select_core (EXCEPT select_core)*.
// ORDER BY may only appear after the whole compound, so ORDER BY between
// compound operators is a syntax error.
select_stmt ::= compound orderby_opt.

compound ::= select_core.
compound ::= compound EXCEPT select_core.

select_core ::= SELECT selcollist FROM ID.
select_core ::= SELECT selcollist.

selcollist ::= expr collate_opt.
selcollist ::= selcollist COMMA expr collate_opt.

orderby_opt ::= ORDER BY sortlist.
orderby_opt ::= .

sortlist ::= expr collate_opt.
sortlist ::= sortlist COMMA expr collate_opt.

collate_opt ::= COLLATE ID.
collate_opt ::= .

expr_list ::= expr.
expr_list ::= expr_list COMMA expr.

// Expressions: literals, identifiers, function calls, parenthesized.
expr ::= NUMBER.
expr ::= STRING.
expr ::= ID.
expr ::= STAR.
expr ::= ID LP expr_list RP.
expr ::= LP expr RP.
