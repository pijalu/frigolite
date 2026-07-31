%token_prefix TK_
%token_type {string}
%default_type {string}
%name TclParser
%extra_argument {interface{} extraCtx}

%token IDENTIFIER STRING BRACED_STRING NUMBER VARIABLE.
%token DO_EXECSQL_TEST DO_CATCHSQL_TEST FOREACH FOR WHILE IF SET EXPR.
%token ELSE THEN ASSIGN MINUS.

input ::= command_list.
command_list ::= command_list command.
command_list ::= command.

command ::= do_execsql_test_stmt.
command ::= do_catchsql_test_stmt.
command ::= foreach_stmt.
command ::= if_stmt.
command ::= set_stmt.

do_execsql_test_stmt ::= DO_EXECSQL_TEST name body body_opt.
{
    if cb, ok := extraCtx.(*CodeBuffer); ok { cb.emit("do_execsql_test(%s, %s, %s)\n", $2, $3, $4); }
}

do_catchsql_test_stmt ::= DO_CATCHSQL_TEST name body body_opt.
{
    if cb, ok := extraCtx.(*CodeBuffer); ok { cb.emit("do_catchsql_test(%s, %s, %s)\n", $2, $3, $4); }
}

foreach_stmt ::= FOREACH name body body.
{
    if cb, ok := extraCtx.(*CodeBuffer); ok { cb.emit("foreach %s in %s do %s\n", $2, $3, $4); }
}

if_stmt ::= IF body body else_clause.
{
    if cb, ok := extraCtx.(*CodeBuffer); ok { cb.emit("if %s then %s else %s\n", $2, $3, $4); }
}
else_clause ::= ELSE body.
{
    $$ = $2;
}
else_clause ::= .
{
    $$ = "";
}

set_stmt ::= SET name body.
{
    if cb, ok := extraCtx.(*CodeBuffer); ok { cb.emit("set %s = %s\n", $2, $3); }
}

name ::= IDENTIFIER.
name ::= STRING.
name ::= BRACED_STRING.

body ::= BRACED_STRING.
body ::= STRING.
body ::= IDENTIFIER.

body_opt ::= body.
body_opt ::= .
