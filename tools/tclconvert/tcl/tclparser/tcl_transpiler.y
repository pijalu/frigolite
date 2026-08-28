%token_prefix TK_
%token_type {interface{}}
%default_type {interface{}}
%name TclTranspiler
%extra_argument {*TranspileResult result}

%token WORD SEPARATOR.

_START ::= command_list.
command_list ::= command_list command SEPARATOR.
command_list ::= command_list SEPARATOR.
command_list ::= command.
command_list ::= .

command ::= do_execsql_test_stmt.
command ::= do_catchsql_test_stmt.
command ::= execsql_stmt.

do_execsql_test_stmt ::= WORD WORD WORD WORD.
{
	name := $2.(string)
	sql := $3.(string)
	expect := $4.(string)
	result.emit("do_execsql_test(%q, func(t *testing.T) {\n", name)
	result.emit("  var _r *frigolite.Result\n")
	result.emit("  _r = db.ExecContext(context.Background(), %s)\n", sql)
	result.emit("  if _r.Error != nil { t.Errorf(...) }\n")
	result.emit("})\n")
}
do_execsql_test_stmt ::= WORD WORD WORD.
{
	name := $2.(string)
	sql := $3.(string)
	result.emit("do_execsql_test(%q, func(t *testing.T) {\n", name)
	result.emit("  var _r *frigolite.Result\n")
	result.emit("  _r = db.ExecContext(context.Background(), %s)\n", sql)
	result.emit("  if _r.Error != nil { t.Errorf(...) }\n")
	result.emit("})\n")
}

do_catchsql_test_stmt ::= WORD WORD WORD WORD.
{
	name := $2.(string)
	sql := $3.(string)
	expect := $4.(string)
	result.emit("do_catchsql_test(%q, %s, %s)\n", name, sql, expect)
}

execsql_stmt ::= WORD WORD.
{
	sql := $2.(string)
	result.emit("{\n  var _r *frigolite.Result\n")
	result.emit("  _r = db.ExecContext(context.Background(), %s)\n", sql)
	result.emit("  if _r.Error != nil { t.Errorf(\"exec error: %%v\\n  sql: %%s\", _r.Error, %s) }\n", sql)
	result.emit("}\n")
}
