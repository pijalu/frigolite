// SPDX-License-Identifier: GPL-3.0-or-later

package tcl

import (
	"strings"
	"testing"
)

func TestTopLevelReturnStopsFixtureConversion(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`return

do_execsql_test alias-1 {SELECT sequence()} {1}`); err != nil {
		t.Fatal(err)
	}
	if stmts := i.Stmts(); len(stmts) != 0 {
		t.Fatalf("statements after top-level return = %#v, want none", stmts)
	}
}

// TestTclSQLLiteral pins the SQL rendering of bound TCL variable values: the
// real tcl interface binds $name/:name/@name parameters with the variable's
// TCL type (tclsqlite.c:1519/:1526/:1537), so numeric values must render as
// bare SQL literals and everything else as quoted text. See interp_sql.go's
// tclSQLLiteral for the rationale (recursive CTE `WHERE i<$nrow`).
func TestDoTestCapturesTclLowercaseResultTransform(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test func-4.39 { string tolower [db eval {SELECT round(1e500), round(-1e500);}] } {inf -inf}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "SELECT round(1e500), round(-1e500);" || stmts[0].ResultTransform != "tolower" {
		t.Fatalf("captured statement = %#v, want query tagged with Tcl tolower result transform", stmts)
	}
}

func TestDoTestCapturesLreplaceResultTransform(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test colname-5.1 { lreplace [db eval {SELECT x.* FROM sqlite_master X LIMIT 1;}] 3 3 x } {table tabc tabc x {CREATE TABLE tabc(a,b,c)}}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "SELECT x.* FROM sqlite_master X LIMIT 1;" || stmts[0].ResultTransform != "lreplace:3:3:x" {
		t.Fatalf("captured statement = %#v, want query tagged with lreplace transform", stmts)
	}
}

func TestDoExecSQLResolvesUnbracedLocalExpectation(t *testing.T) {
	i := NewInterp()
	src := `proc sample {} {
  set expected {CREATE TABLE t1(a)}
  do_execsql_test sample-1 {SELECT sql FROM sqlite_master} $expected
}
sample`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Expected != "CREATE TABLE t1(a)" {
		t.Fatalf("dynamic expectation = %#v, want resolved local value", stmts)
	}
}
func TestExecSQLAssignmentDoesNotClassifyTriggerBodyAsQuery(t *testing.T) {
	i := NewInterp()
	src := `do_test trigger-1 {
  execsql {CREATE TRIGGER AFTER INSERT ON t1 BEGIN SELECT _x_ FROM t1; END}
  set schema [execsql {SELECT sql FROM sqlite_master}]
} {}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) < 2 || stmts[0].Type != "exec" || stmts[1].Type != "query_assign" {
		t.Fatalf("trigger and SELECT assignment statements = %#v, want exec then query_assign", stmts)
	}
}

func TestLSearchGlobMatchReturnsIndexOrMinusOne(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set found [lsearch {Init IdxInsert VFilter} Idx*]; set missing [lsearch {Init VFilter} IdxInsert]`); err != nil {
		t.Fatal(err)
	}
	found, _ := i.Variable("found")
	missing, _ := i.Variable("missing")
	if found != "1" || missing != "-1" {
		t.Fatalf("lsearch results = (%q, %q), want (1, -1)", found, missing)
	}
}

func TestExecSQLResultAssignmentIsCapturedAsRuntimeStep(t *testing.T) {
	i := NewInterp()
	src := `do_test alter-6.2 {
  execsql {CREATE TABLE abc(a)}
  set ::oid [execsql {SELECT max(oid) FROM sqlite_master}]
  execsql "SELECT sql FROM sqlite_master WHERE oid = $::oid"
} {CREATE TABLE abc(a)}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 || stmts[0].SQL != "CREATE TABLE abc(a)" || stmts[1].Type != "query_assign" || stmts[1].SQL != "SELECT max(oid) FROM sqlite_master" || stmts[1].Variable != "oid" || stmts[2].Type != "exec" || stmts[2].SQL != "SELECT sql FROM sqlite_master WHERE oid = $::oid" {
		t.Fatalf("result assignment statements = %#v, want ordered CREATE, assignment query, dynamic SQL", stmts)
	}
}

func TestExecSQLDDLCommandSubstitutionCapturesRuntimeAssignment(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`proc create_vtab {tname} {
  set cmd [list vtab_command $tname {a b}]
  execsql "CREATE VIRTUAL TABLE $tname USING tcl('$cmd')"
  execsql "SELECT '$cmd'"
}
do_test sample-1 { create_vtab v1 } {}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "exec" || stmts[0].SQL != "CREATE VIRTUAL TABLE v1 USING tcl('vtab_command v1 {a b}')" || stmts[1].SQL != "SELECT 'vtab_command v1 {a b}'" {
		t.Fatalf("DDL command substitution steps = %#v, want executed DDL and preserved Tcl list value", stmts)
	}
}

func TestSQLite3InternalFunctionControlEmitsOrderedMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3_test_control SQLITE_TESTCTRL_INTERNAL_FUNCTIONS db
sqlite3_test_control SQLITE_TESTCTRL_INTERNAL_FUNCTIONS db`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "internal_functions" || stmts[1].Type != "internal_functions" || stmts[0].SQL != "enable" || stmts[1].SQL != "disable" {
		t.Fatalf("internal function control markers = %#v, want ordered enable/disable", stmts)
	}
}

func TestSQLite3NamedMemoryOpenEmitsOpenMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3 db2 :memory:`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "open_named_db" || stmts[0].SQL != ":memory:" || stmts[0].Database != "db2" {
		t.Fatalf("named in-memory open = %#v, want open_named_db marker", stmts)
	}
}

func TestNamedDBFunctionRegistrationEmitsDatabaseMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`proc myfunc {x} {expr $x < 10}
db2 func myfunc myfunc`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "register_function" || stmts[0].Database != "db2" || stmts[0].FunctionName != "myfunc" {
		t.Fatalf("named function marker = %#v", stmts)
	}
}

func TestCacheFlushEmitsRuntimeMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test cacheflush-1 { sqlite3_db_cacheflush db } {}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "cacheflush" {
		t.Fatalf("cacheflush events = %#v", stmts)
	}
}

func TestChanges2CAPIEventsAreCapturedInSourceOrder(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set ::stmt [sqlite3_prepare_v2 db {INSERT INTO log VALUES(changes() || ' changes')} -1 tail]
do_test 2.2 { list [sqlite3_step $::stmt] [sqlite3_reset $::stmt] } {SQLITE_DONE SQLITE_OK}
sqlite3_finalize $::stmt`); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range i.Stmts() {
		got = append(got, s.Type)
	}
	want := []string{"prepare_stmt", "stmt_step", "stmt_reset", "runtime_assert", "stmt_finalize"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestLegacySQLite3PrepareCapturesHandle(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set STMT [sqlite3_prepare db {SELECT x FROM t1} -1 TAIL]
do_test sample-1 { sqlite3_step $STMT } {SQLITE_ROW}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 || stmts[0].Type != "prepare_stmt" || stmts[0].Variable != "stmt_1" || stmts[1].Type != "stmt_step" || stmts[1].Variable != "stmt_1" || stmts[2].Type != "runtime_assert" {
		t.Fatalf("legacy prepare events = %#v", stmts)
	}
}

func TestForcecopyEmitsTypedMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`forcecopy test.db dq.db`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "copy_file" || stmts[0].SQL != "test.db" || stmts[0].FileTarget != "dq.db" {
		t.Fatalf("forcecopy statements = %#v, want copy_file test.db dq.db marker", stmts)
	}
}

// TestCopyFileEmitsFileCopyMarker pins the tester.tcl helper `copy_file FROM
// TO` (8_3_names.test:71-72 copies the live database and its 8.3 journal):
// the converted fixture must carry the raw file copy so the harness can
// reproduce the on-disk state the following connection opens.
func TestCopyFileEmitsFileCopyMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`copy_file test.db test2.db; copy_file test.db-journal test2.db-journal`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("copy_file statements = %#v, want two copy markers", stmts)
	}
	if stmts[0].Type != "copy_file" || stmts[0].SQL != "test.db" || stmts[0].FileTarget != "test2.db" {
		t.Fatalf("first copy marker = %#v", stmts[0])
	}
	if stmts[1].Type != "copy_file" || stmts[1].SQL != "test.db-journal" || stmts[1].FileTarget != "test2.db-journal" {
		t.Fatalf("second copy marker = %#v", stmts[1])
	}
}

// TestIfCapableAbsent8_3NamesStopsFile pins the guard at 8_3_names.test:20-23:
// the standard SQLite testfixture does not compile SQLITE_ENABLE_8_3_NAMES,
// so `ifcapable !8_3_names { finish_test; return }` must terminate conversion
// of the file (its journal/WAL name checks require that compile option).
func TestIfCapableAbsent8_3NamesStopsFile(t *testing.T) {
	i := NewInterp()
	src := `ifcapable !8_3_names {
  finish_test
  return
}
do_test x-1 { SELECT 1 } {1}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	if got := i.Stmts(); len(got) != 0 {
		t.Fatalf("statements = %#v, want none (file skipped by capability guard)", got)
	}
}

// TestIfCapablePresentWalKeepsFile pins the complementary case: capabilities
// the engine implements (wal) do not stop conversion.
func TestIfCapablePresentWalKeepsFile(t *testing.T) {
	i := NewInterp()
	src := `ifcapable wal {
  do_test y-1 { execsql {SELECT 2} } {2}
}
ifcapable !wal {
  finish_test
  return
}
do_test y-2 { execsql {SELECT 3} } {3}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].SQL != "SELECT 2" || stmts[1].SQL != "SELECT 3" {
		t.Fatalf("statements = %#v, want both wal-guarded tests converted", stmts)
	}
}

func TestFileWriteCommandsEmitTypedMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set fd [open test2.db w]
puts $fd "not a database"
close $fd`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "write_file" || stmts[0].SQL != "test2.db" || stmts[0].FileData != "not a database\n" {
		t.Fatalf("file write statements = %#v, want one write_file marker", stmts)
	}
}

func TestSQLite3ExecCapturesSQLAndDatabase(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test sample-1 { sqlite3_exec db {CREATE TABLE t11(%c6%c6)} } {0 {}}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "sqlite3_exec" || stmts[0].SQL != "CREATE TABLE t11(%c6%c6)" || stmts[0].Database != "" {
		t.Fatalf("sqlite3_exec statements = %#v, want captured SQL on main database", stmts)
	}
}

func TestStepsqlCapturesOrderedMainConnectionSQL(t *testing.T) {
	i := NewInterp()
	src := `do_test alter-1.7 { stepsql $DB {
  ALTER TABLE [-t1-] RENAME TO [*t1*];
  ALTER TABLE T2 RENAME TO [<t2>];
} } {}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "exec" || !strings.Contains(stmts[0].SQL, "ALTER TABLE T2 RENAME TO [<t2>]") {
		t.Fatalf("stepsql statements = %#v, want ordered SQL on default connection", stmts)
	}
}

func TestForeachBoundSQLParameterIsQuoted(t *testing.T) {
	i := NewInterp()
	src := `foreach {name sdx} {euler E460} {
  do_test func-20.1 { execsql {SELECT soundex($name)} } $sdx
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "SELECT soundex('euler')" || stmts[0].Expected != "E460" {
		t.Fatalf("captured loop statement = %#v, want quoted euler and E460", stmts)
	}
}

func TestDBFunctionCapturesFlagsBeforeSchemaSQL(t *testing.T) {
	i := NewInterp()
	src := `proc f1 {x} {return $x}
do_test trustschema1-1.100 {
  db function f1 -innocuous -deterministic f1
  db function f2 -deterministic f1
  db function f3 -directonly -deterministic f1
  db eval {CREATE TABLE t1(a, b AS (f1(a+1)), c AS (f2(a+2)));}
} {}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 4 || stmts[0].Type != "register_function" || stmts[0].FunctionName != "f1" || !stmts[0].Innocuous || stmts[1].FunctionName != "f2" || stmts[1].Innocuous || stmts[2].FunctionName != "f3" || !stmts[2].DirectOnly || stmts[3].SQL != "CREATE TABLE t1(a, b AS (f1(a+1)), c AS (f2(a+2)));" {
		t.Fatalf("captured function registration sequence = %#v", stmts)
	}
}

func TestDoTestCapturesGlobalVariableAssertion(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test alter2-8.3 { set ::val } {-123.0 real 5 text}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "runtime_var" || stmts[0].Variable != "val" || stmts[0].Expected != "-123.0 real 5 text" {
		t.Fatalf("global variable assertion = %#v", stmts)
	}
}

func TestDBFunctionCapturesSetCommandPrefix(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`db function set_val {set ::val}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "register_function" || stmts[0].FunctionProc != "set ::val" || stmts[0].FunctionArgs == nil || len(stmts[0].FunctionArgs) != 1 || stmts[0].FunctionArgs[0] != "value" || stmts[0].FunctionBody != "set ::val $value" || stmts[0].MinArgs != 1 || stmts[0].MaxArgs != 1 {
		t.Fatalf("set command-prefix registration = %#v", stmts)
	}
}

func TestDBFunctionCapturesVariadicGlobalCallback(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`proc trigfunc {args} { set ::TRIGGER $args }
db func trigfunc trigfunc`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "register_function" || stmts[0].FunctionName != "trigfunc" || stmts[0].FunctionArgs == nil || len(stmts[0].FunctionArgs) != 1 || stmts[0].FunctionArgs[0] != "args" || stmts[0].FunctionBody != " set ::TRIGGER $args " || stmts[0].MinArgs != 0 || stmts[0].MaxArgs != -1 {
		t.Fatalf("variadic callback registration = %#v", stmts)
	}
}

func TestDBFunctionCapturesFixedAndVariadicArity(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`proc callback {required {optional value} args} { set ::CALLBACK $args }
db func callback callback`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].MinArgs != 1 || stmts[0].MaxArgs != -1 || len(stmts[0].FunctionArgs) != 3 {
		t.Fatalf("fixed/variadic function registration = %#v", stmts)
	}
}

func TestDoSelectTestsErrorSwitchCapturesCatchExpectation(t *testing.T) {
	i := NewInterp()
	src := `proc do_createtable_tests {nm args} { uplevel do_select_tests [list e_createtable-$nm] $args }
do_createtable_tests 0.1.2 -error { near "%s": syntax error } {
  1 {CREATE TABLE t1(c1 one(number))} {number}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "catch" || stmts[0].SQL != "CREATE TABLE t1(c1 one(number))" || stmts[0].Expected != `1 {near "number": syntax error}` {
		t.Fatalf("error test statements = %#v, want one caught SQL statement with catchsql error text", stmts)
	}
}

func TestECreateTableExpandsSQLiteMaxColumnAndRuntimeLimit(t *testing.T) {
	i := NewInterp()
	src := `proc columns {n} { set res [list]; for {set j 0} {$j < $n} {incr j} { lappend res c$j }; join $res ", " }
do_test e_createtable-3.10.1 { columns $::SQLITE_MAX_COLUMN } 2000
sqlite3_limit db SQLITE_LIMIT_COLUMN 500`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "set_limit" || stmts[0].SQL != "SQLITE_LIMIT_COLUMN:500" {
		t.Fatalf("column-limit markers = %#v", stmts)
	}
}

func TestECreateTablePreservesClockAndTclFunctionSetup(t *testing.T) {
	i := NewInterp()
	src := `set sqlite_current_time 1000000000
proc nextint {} { incr ::nextint }
db func nextint nextint`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "set_current_time" || stmts[0].SQL != "1000000000" || stmts[1].Type != "register_function" || stmts[1].FunctionName != "nextint" || stmts[1].FunctionBody != " incr ::nextint " || stmts[1].MinArgs != 0 {
		t.Fatalf("clock/function setup markers = %#v", stmts)
	}
}

func TestDoSelectTestsPreservesTclQueryMode(t *testing.T) {
	i := NewInterp()
	src := `do_select_tests sample -tclquery {list [table_list]} {
  1 {CREATE TABLE t1(a)} {main t1 temp}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "exec" || stmts[1].Type != "tclquery" || stmts[1].SQL != "list [table_list]" {
		t.Fatalf("tclquery statements = %#v, want exec plus preserved Tcl query", stmts)
	}
}

func TestDoSelectTestsExpandsQueryCasesInOrder(t *testing.T) {
	i := NewInterp()
	src := `proc do_createtable_tests {nm args} { uplevel do_select_tests [list e_createtable-$nm] $args }
do_createtable_tests 2.3.1 -query {SELECT sql FROM sqlite_master ORDER BY rowid DESC LIMIT 1} {
  1 {CREATE TABLE x1 AS SELECT * FROM t6} {{CREATE TABLE x1(m,n)}}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	// Canonical TCL list serialization braces elements containing spaces
	// (the harness strips the wrapping braces before comparing).
	if len(stmts) != 2 || stmts[0].TestName != "e_createtable-2.3.1.1" || stmts[0].SQL != "CREATE TABLE x1 AS SELECT * FROM t6" || stmts[1].Type != "query" || stmts[1].SQL != "SELECT sql FROM sqlite_master ORDER BY rowid DESC LIMIT 1" || stmts[1].Expected != "{CREATE TABLE x1(m,n)}" {
		t.Fatalf("expanded test statements = %#v", stmts)
	}
}

// TestDoExecSQLExpandsListArgument pins TCL {*} argument expansion inside the
// expected-value command substitution used by e_update-1.8:
// `[list {*}$data]` must produce the elements of $data, not "*" plus $data.
func TestDoExecSQLExpandsListArgument(t *testing.T) {
	i := NewInterp()
	src := `set data {1 one 2 two 3 three 4 four}
do_execsql_test sample-1 {SELECT * FROM t3} [list {*}$data]`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Expected != "1 one 2 two 3 three 4 four" {
		t.Fatalf("expanded expectation = %#v, want {*} list elements", stmts)
	}
}

// TestDoExecSQLExpandsBracedListArgument pins the `[list {*}{...}]` form used
// by e_update-2.3.0: the braced block's elements are spliced through list and
// re-serialized canonically (whitespace collapsed, aux  t1 -> aux t1).
func TestDoExecSQLExpandsBracedListArgument(t *testing.T) {
	i := NewInterp()
	src := "do_execsql_test sample-2 {SELECT 'main', tbl_name FROM sqlite_master} [list {*}{\n    main t1\n    aux  t1\n}]"
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Expected != "main t1 aux t1" {
		t.Fatalf("braced expansion expectation = %#v, want canonical list", stmts)
	}
}

// TestDoSelectTestsCanonicalizesExpectedListElements pins that do_select_tests
// family expectations are serialized as canonical TCL lists: empty elements
// stay as {} instead of being lost to a plain space join
// (e_update-1.4: {{} {}  1 i  2 ii  3 iii}).
func TestDoSelectTestsCanonicalizesExpectedListElements(t *testing.T) {
	i := NewInterp()
	src := `proc do_update_tests {args} { uplevel do_select_tests $args }
do_update_tests e_update-1.4 -query {SELECT * FROM t1} {
  1 "UPDATE t1 SET b = 'burmese' WHERE a=5" {{} {}  1 i  2 ii  3 iii}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[1].Type != "query" || stmts[1].Expected != "{} {} 1 i 2 ii 3 iii" {
		t.Fatalf("canonical expectation = %#v, want empty elements preserved", stmts)
	}
}

// TestDoSelectTestsFormatsIntegerErrorTemplates pins that -errorformat
// templates with %d verbs substitute the integer argument values the way TCL
// `format` does (e_insert-1.2.1: "table a2 has %d columns but %d values ..."),
// instead of Go's raw fmt rendering "%!d(string=3)".
func TestDoSelectTestsFormatsIntegerErrorTemplates(t *testing.T) {
	i := NewInterp()
	src := `proc do_insert_tests {args} { uplevel do_select_tests $args }
do_insert_tests e_insert-1.2 -error {table %s has %d columns but %d values were supplied} {
  1 {INSERT INTO a2 VALUES(1)} {a2 3 1}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	want := "1 {table a2 has 3 columns but 1 values were supplied}"
	if len(stmts) != 1 || stmts[0].Type != "catch" || stmts[0].Expected != want {
		t.Fatalf("formatted error expectation = %#v, want %q", stmts, want)
	}
}

func TestTopLevelDBEvalCapturesSetupSQL(t *testing.T) {
	i := NewInterp()
	src := `db eval {
CREATE TABLE t1(a INT);
INSERT INTO t1 VALUES(1);
}
do_execsql_test joinB-1 { SELECT * FROM t1; } { 1 }`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].TestName != "" || stmts[0].Type != "exec" || !strings.Contains(stmts[0].SQL, "CREATE TABLE t1") || !strings.Contains(stmts[0].SQL, "INSERT INTO t1") || stmts[1].TestName != "joinB-1" {
		t.Fatalf("captured statements = %#v, want setup SQL followed by joinB-1", stmts)
	}
}

func TestIfCapableSkipsUnsupportedUTF16Body(t *testing.T) {
	i := NewInterp()
	src := `ifcapable utf16 {
 do_test attach2-2.3 { execsql {SELECT 1} } {1}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	if got := i.Stmts(); len(got) != 0 {
		t.Fatalf("captured statements = %#v, want unsupported UTF-16 body skipped", got)
	}
}

func TestIfCapableExecutesSupportedAuthBody(t *testing.T) {
	i := NewInterp()
	src := `ifcapable auth { do_execsql_test attach2-2.3 {SELECT 1} {1} }`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].TestName != "attach2-2.3" {
		t.Fatalf("captured statements = %#v, want supported auth body", got)
	}
}

// TestIfCapableExecutesElseBranchForAbsentCapability pins `ifcapable EXPR BODY
// else ELSEBODY` (triggerupfrom-4.1): with the capability absent the else body
// runs, not the true body picked as the last braced word.
func TestIfCapableExecutesElseBranchForAbsentCapability(t *testing.T) {
	i := NewInterp()
	src := `ifcapable utf16 {
  do_execsql_test sample-enabled {SELECT 1} {1}
} else {
  do_execsql_test sample-disabled {SELECT 2} {2}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].TestName != "sample-disabled" {
		t.Fatalf("captured statements = %#v, want else body for absent capability", got)
	}
}

// TestIfCapableExecutesTrueBranchSuppressingElse pins the enabled side of the
// same construct: the true body runs and the else body is not emitted.
func TestIfCapableExecutesTrueBranchSuppressingElse(t *testing.T) {
	i := NewInterp()
	src := `ifcapable hiddencolumns {
  do_execsql_test sample-enabled {SELECT 1} {1}
} else {
  do_execsql_test sample-disabled {SELECT 2} {2}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].TestName != "sample-enabled" {
		t.Fatalf("captured statements = %#v, want true body for present capability", got)
	}
}

func TestDBAuthCapturesAnalyzeDenial(t *testing.T) {
	i := NewInterp()
	src := `ifcapable auth {
	proc authproc {op args} {
			if {$op == "SQLITE_ANALYZE"} { return "SQLITE_DENY" }
			return "SQLITE_OK"
	}
	do_test analyze9-19.2 {
			reset_db
			db auth authproc
			execsql {CREATE TABLE t1(x,y); CREATE VIEW v1 AS SELECT * FROM t1;}
			catchsql ANALYZE
	} {1 {not authorized}}
	}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	found := false
	for _, stmt := range stmts {
		found = found || stmt.Type == "auth_analyze_deny"
	}
	if !found {
		t.Fatalf("captured statements = %#v, want ANALYZE authorizer marker", stmts)
	}
}

func TestTCLModuleRegistrationCapturesProcedures(t *testing.T) {
	interp := NewInterp()
	if err := interp.Execute(`proc t1_vtab {mode method args} {return $method}; register_tcl_module db`); err != nil {
		t.Fatal(err)
	}
	stmts := interp.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "register_tcl_module" {
		t.Fatalf("module marker = %#v", stmts)
	}
	marker := stmts[0]
	if len(marker.CallbackProcs) != 1 || marker.CallbackProcs[0].Name != "t1_vtab" || marker.CallbackProcs[0].Body != "return $method" || len(marker.CallbackProcs[0].Args) != 3 || len(marker.ModuleArgs) != 1 || marker.ModuleArgs[0] != "db" {
		t.Fatalf("captured Tcl callback marker = %#v", marker)
	}
}
func TestInvokeSerializedTclProcedure(t *testing.T) {
	interp := NewInterp()
	interp.LoadProcedures([]Proc{{Name: "callback", Args: []string{"method"}, Body: `return schema`}})
	got, err := interp.InvokeProcedure("callback", "xConnect")
	if err != nil || got != "schema" {
		t.Fatalf("callback result = %q, %v; want schema", got, err)
	}
}

func TestProcedureCollectsVariadicArgs(t *testing.T) {
	i := NewInterp()
	src := `proc fill {first args} { execsql {INSERT INTO t VALUES($args)} }; fill 1 -ncopy 10 -nval 19`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "INSERT INTO t VALUES('-ncopy 10 -nval 19')" {
		t.Fatalf("captured statements = %#v, want variadic args bound", stmts)
	}
}

func TestProcedureArraySetAndInfoExists(t *testing.T) {
	i := NewInterp()
	src := `proc fill {} { array set A {-ncopy 1}; if {[info exists A(-ncopy)]==0} { error missing }; execsql {INSERT INTO t VALUES($A(-ncopy))} }; fill`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "INSERT INTO t VALUES(1)" {
		t.Fatalf("captured statements = %#v, want one insert with value 1", stmts)
	}
}

func TestNestedArrayVariableSubstitution(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set C(column) 1; set cols(1) value; set result "$cols($C(column))"; execsql {INSERT INTO t VALUES($result)}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "INSERT INTO t VALUES('value')" {
		t.Fatalf("nested array substitution captured %#v, want value", stmts)
	}
}

func TestExprIntegerDivisionPreservesTclSemantics(t *testing.T) {
	got, err := EvalExpr("9 / 2", NewInterp(), nil)
	if err != nil || got != "4" {
		t.Fatalf("EvalExpr integer division = %q, %v; want 4", got, err)
	}
	got, err = EvalExpr("9.0 / 2", NewInterp(), nil)
	if err != nil || got != "4.5" {
		t.Fatalf("EvalExpr float division = %q, %v; want 4.5", got, err)
	}
}

func TestExecSQLBindsBracedSQLParameters(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`for {set i 1} {$i < 2} {incr i} { set a $i; execsql {INSERT INTO t VALUES($a)} }`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "INSERT INTO t VALUES(1)" {
		t.Fatalf("captured statements = %#v, want bound SQL value 1", stmts)
	}
}

func TestExecSQL2IsCapturedWithNamedRows(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test avtrans-6.1 { execsql2 {SELECT 1 AS a} } {a 1}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "query2" || stmts[0].Expected != "a 1" {
		t.Fatalf("captured statements = %#v, want query2 with expectation a 1", stmts)
	}
}

func TestDBNullvalueSetsNullToken(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`db nullvalue -`); err != nil {
		t.Fatal(err)
	}
	if got := i.NullToken(); got != "-" {
		t.Fatalf("NullToken() = %q, want %q", got, "-")
	}
}

func TestCommandSubstitutionClearsStaleResult(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set prior [set value stale]; set result [db close]`); err != nil {
		t.Fatal(err)
	}
	if got := i.vars["result"]; got != "" {
		t.Fatalf("result = %q, want empty for command with no return value", got)
	}
}

func TestForeachDropAllTablesLoopEmitsMarker(t *testing.T) {
	i := NewInterp()
	src := `foreach tab [db eval {SELECT name FROM sqlite_master WHERE type = 'table'}] {
	db eval "DROP TABLE $tab"
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "query" || stmts[1].Type != "drop_all_tables" {
		t.Fatalf("captured statements = %#v, want query followed by drop_all_tables marker", stmts)
	}
}

func TestSwitchSelectsMatchingAndDefaultBody(t *testing.T) {
	i := NewInterp()
	src := `for {set n 0} {$n < 3} {incr n} {
  switch $n {
    0 {set value alpha}
    1 {set value bravo}
    default {set value other}
  }
  db eval {INSERT INTO t VALUES($value)}
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 {
		t.Fatalf("captured statements = %#v, want 3 row inserts", stmts)
	}
	for n, want := range []string{"VALUES('alpha')", "VALUES('bravo')", "VALUES('other')"} {
		if !strings.Contains(stmts[n].SQL, want) {
			t.Errorf("statement %d = %q, want %q", n, stmts[n].SQL, want)
		}
	}
}

// TestDBEvalKeepsUnsetVariablesAsParameters pins the unset-variable rule:
// tclsqlite.c:1494 only binds a parameter when Tcl_GetVar2Ex finds the TCL
// variable, so an unset variable stays an *unbound parameter* (NULL at run
// time) — the SQL text must keep the reference, because parse-time checks
// such as "trigger cannot use variables" (triggerE-1.1.11) depend on it.
func TestDBEvalKeepsUnsetVariablesAsParameters(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`db eval {INSERT INTO t VALUES($unset)}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || !strings.Contains(stmts[0].SQL, "VALUES($unset)") {
		t.Fatalf("captured statements = %#v, want unset binding kept as $unset", stmts)
	}
}

// TestDoCatchsqlKeepsParameterInTriggerBody pins the triggerE regression: a
// deliberate parameter reference inside a trigger body must stay in the SQL
// text so CREATE TRIGGER fails with "trigger cannot use variables". Replacing
// the parameter with NULL would make the body valid and the test would pass
// when it must fail.
func TestDoCatchsqlKeepsParameterInTriggerBody(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_catchsql_test triggerE-1.1.11 {
  CREATE TRIGGER tr1 BEFORE INSERT ON t1 BEGIN
    INSERT INTO t1 SELECT max(b) OVER(ORDER BY $1) FROM t1; END
} {1 {trigger cannot use variables}}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || !strings.Contains(stmts[0].SQL, "$1") {
		t.Fatalf("captured statements = %#v, want the $1 parameter preserved", stmts)
	}
}

func TestDropAllTablesAndViewsCapturesMainCleanupMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`proc drop_all_tables_and_views {db} {db eval {SELECT name FROM sqlite_master}; db eval "DROP TABLE t1"}; drop_all_tables_and_views db`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "drop_main_tables" {
		t.Fatalf("captured statements = %#v, want one main-database cleanup marker", stmts)
	}
}

func TestDBCloseCapturesConnectionLifecycleMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`db close; sqlite3 db test.db`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "close_db" || stmts[1].Type != "reopen_db" || stmts[1].SQL != "test.db" {
		t.Fatalf("captured statements = %#v, want ordered close and existing-file reopen markers", stmts)
	}
}

func TestRegisterEchoModuleCapturesLifecycleMarker(t *testing.T) {
	i := NewInterp()
	src := `set testprefix vtab1
 do_test vtab1-1.2 {
  execsql {SELECT name FROM sqlite_master ORDER BY 1}
 } {}
 register_echo_module [sqlite3_connection_pointer db]`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[1].Type != "register_echo_module" {
		t.Fatalf("captured statements = %#v, want query followed by echo registration marker", stmts)
	}
}

func TestForcedeleteCapturesExplicitFileCleanup(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`forcedelete test.db2`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "delete_file" || stmts[0].SQL != "test.db2" {
		t.Fatalf("captured statements = %#v, want typed test.db2 removal marker", stmts)
	}
}

func TestSQLite3ExistingFileEmitsOrderedReopenMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`db close; sqlite3 db test.db`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 2 || got[0].Type != "close_db" || got[1].Type != "reopen_db" || got[1].SQL != "test.db" {
		t.Fatalf("markers = %#v, want ordered close_db then reopen_db(test.db)", got)
	}
}

func TestSQLite3EmptyFilenameOpensEmptyDatabase(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3 db ""`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "open_empty_db" {
		t.Fatalf("captured statements = %#v, want one open_empty_db marker", stmts)
	}
	// `:memory:` is the same replacement: a fresh empty main handle that
	// must not delete the lifecycle file (reset_db semantics differ).
	i = NewInterp()
	if err := i.Execute(`sqlite3 db :memory:`); err != nil {
		t.Fatal(err)
	}
	stmts = i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "open_empty_db" {
		t.Fatalf("`sqlite3 db :memory:` markers = %#v, want open_empty_db (not reset_db)", stmts)
	}
}

func TestExecSQLBindsOnlySQLiteParameterTokens(t *testing.T) {
	i := NewInterp()
	src := "set v1 value; execsql {SELECT '$v1', \"$v1\", `$v1`, [$v1], $v1 -- $v1\n/* $v1 */}"
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	want := "SELECT '$v1', \"$v1\", `$v1`, [$v1], 'value' -- $v1\n/* $v1 */"
	if len(got) != 1 || got[0].SQL != want {
		t.Fatalf("captured SQL = %#v, want quoted/comment tokens untouched and parameter bound", got)
	}
}

func TestExecSQLSubstitutesGlobalVariableByBareName(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set ::table_name T; execsql "CREATE TABLE ${table_name}(a)"`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].SQL != "CREATE TABLE T(a)" {
		t.Fatalf("captured global-variable SQL = %#v, want CREATE TABLE T(a)", got)
	}
}

func TestExecSQLSubstitutesGlobalVariableInQuotedSQL(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set ::table_name T; execsql "CREATE TABLE ${::table_name}(a)"`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].SQL != "CREATE TABLE T(a)" {
		t.Fatalf("captured global-variable SQL = %#v, want CREATE TABLE T(a)", got)
	}
}

func TestTclDoubleQuotedControlEscapeBindsAsControlByte(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set v1 "abc\fdef"; execsql {SELECT $v1 REGEXP '^abc\fdef$'}`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || !strings.Contains(got[0].SQL, "abc\fdef") {
		t.Fatalf("captured SQL = %#v, want bound input with form-feed byte", got)
	}
	if !strings.Contains(got[0].SQL, `REGEXP '^abc\fdef$'`) {
		t.Fatalf("captured SQL = %q, want regexp pattern escape preserved", got[0].SQL)
	}
}

func TestEchoIgnoreUsableSetIsCaptured(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set ::echo_module_ignore_usable 1`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].Type != "echo_ignore_usable" || got[0].SQL != "1" {
		t.Fatalf("captured echo setting = %#v, want echo_ignore_usable=1", got)
	}
}

func TestExecSQLPreservesNamedConnection(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`execsql {INSERT INTO t VALUES(1)} db2`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].Database != "db2" {
		t.Fatalf("captured SQL connection = %#v, want db2", got)
	}
}

func TestDoExecSQLPreservesSingleEmptyTextExpectedValue(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_execsql_test empty-text-1 {SELECT ''} {{}}`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 1 || got[0].Type != "emptytext" || got[0].SQL != "SELECT ''" {
		t.Fatalf("empty-text fixture marker = %#v, want typed emptytext query", got)
	}
}

func TestDoExecSQLPreservesDatabaseOption(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3 db2 test.db; do_execsql_test -db db2 test-1 {BEGIN} {}`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 2 || got[1].Type != "exec" || got[1].Database != "db2" || got[1].SQL != "BEGIN" {
		t.Fatalf("-db fixture marker = %#v", got)
	}
}

func TestNamedConnectionUnbracedEvalPreservesDatabase(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3 db2 test.db; db2 eval BEGIN`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 2 || got[1].Type != "exec" || got[1].SQL != "BEGIN" || got[1].Database != "db2" {
		t.Fatalf("unbraced named connection eval markers = %#v", got)
	}
}

func TestNamedConnectionEvalPreservesDatabase(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3 db2 test.db; db2 eval {CREATE TABLE t(x); INSERT INTO t VALUES(1)}; db2 close`); err != nil {
		t.Fatal(err)
	}
	got := i.Stmts()
	if len(got) != 3 || got[0].Type != "open_named_db" || got[1].Type != "exec" || got[1].Database != "db2" || got[2].Type != "close_named_db" || got[2].Database != "db2" {
		t.Fatalf("named connection eval markers = %#v", got)
	}
}

func TestSQLite3AttachedConnectionDoesNotResetMain(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`register_echo_module db; forcedelete test2.db; sqlite3 db2 test2.db`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range i.Stmts() {
		if stmt.Type == "reset_db" || stmt.Type == "reopen_db" {
			t.Fatalf("secondary connection markers = %#v; must not reset/reopen main connection", i.Stmts())
		}
	}
}

// TestSQLite3ReopenAfterForcedeleteURI pins the connection replacement at
// 8_3_names.test:97-99 (`forcedelete test.db; sqlite3 db
// file:./test.db?8_3_names=1`): the harness models it as reopening the
// fixture lifecycle file, so the URI is normalized to the bare basename
// (the 8_3_names=VFS query is a compile-time feature, not a file path).
func TestSQLite3ReopenAfterForcedeleteURI(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`forcedelete test.db; sqlite3 db file:./test.db?8_3_names=1`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 || stmts[0].Type != "delete_file" || stmts[1].Type != "reopen_db" || stmts[1].SQL != "test.db" {
		t.Fatalf("captured statements = %#v, want file deletion followed by reopen of test.db", stmts)
	}
}

func TestAppendUpdatesProcedureVariable(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`proc build {} {set order "ORDER BY "; append order "2 ASC,"; append order " 3 DESC"; set answer $order}; set result [build]`); err != nil {
		t.Fatal(err)
	}
	if got, want := i.vars["result"], "ORDER BY 2 ASC, 3 DESC"; got != want {
		t.Fatalf("append result = %q, want %q", got, want)
	}
}

func TestFormatZeroPaddedInteger(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set result [format "c%02d" 7]`); err != nil {
		t.Fatal(err)
	}
	if got, want := i.vars["result"], "c07"; got != want {
		t.Fatalf("format result = %q, want %q", got, want)
	}
}

func TestExecSQLBracedWordKeepsBracketedIdentifiers(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`execsql {SELECT * FROM [temp table]}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "SELECT * FROM [temp table]" {
		t.Fatalf("captured SQL = %#v, want bracketed identifier unchanged", stmts)
	}
}

func TestSubstDecodesUnicodeEscapes(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set x [subst {$\u00A2\u20AC}]`); err != nil {
		t.Fatal(err)
	}
	if got := i.vars["x"]; got != "$¢€" {
		t.Fatalf("subst Unicode escapes = %q, want %q", got, "$¢€")
	}
}

func TestSubstNoCommandsAppliesVariablesButKeepsBrackets(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set temp TEMP; set result [subst -nocommands {CREATE $temp TABLE [set ignored]}]`); err != nil {
		t.Fatal(err)
	}
	if got, want := i.vars["result"], "CREATE TEMP TABLE [set ignored]"; got != want {
		t.Fatalf("subst result = %q, want %q", got, want)
	}
}

func TestTclSQLLiteral(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"10", "10"},
		{"-5", "-5"},
		{"+3", "+3"},
		{"1.5", "1.5"},
		{".5", ".5"},
		{"5.", "5."},
		{"1e3", "1e3"},
		{"1E-3", "1E-3"},
		{"  10  ", "10"},
		{"", "''"},
		{"abc", "'abc'"},
		{"O'Brien", "'O''Brien'"},
		{"10abc", "'10abc'"},
		{"0x10", "'0x10'"},
		{"1.2.3", "'1.2.3'"},
		{"1e", "'1e'"},
		{"Inf", "'Inf'"},
		{"+", "'+'"},
		{"1 0", "'1 0'"},
	}
	for _, c := range cases {
		if got := tclSQLLiteral(c.in); got != c.want {
			t.Errorf("tclSQLLiteral(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDoExecSQLRuntimeComparisonCapturesCompareQuery pins the authoritative
// `do_execsql_test NAME { SQL1 } [db eval { SQL2 }]` shape (update2.test:38,
// analyze5.test:186-197): the expected value is the runtime result of the
// second query, so the converted fixture must carry both statements as a
// compare_query instead of evaluating SQL2 offline (which previously attached a
// stale Tcl value as the expectation and leaked a spurious query step).
func TestDoExecSQLRuntimeComparisonCapturesCompareQuery(t *testing.T) {
	i := NewInterp()
	src := `do_execsql_test update2-1.1.2 { SELECT * FROM t1; } [db eval { SELECT a, repeat(b, 100) FROM t2 }]`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 {
		t.Fatalf("runtime comparison statements = %#v, want one compare_query", stmts)
	}
	s := stmts[0]
	if s.Type != "compare_query" || strings.TrimSpace(s.SQL) != "SELECT * FROM t1;" || strings.TrimSpace(s.AuxSQL) != "SELECT a, repeat(b, 100) FROM t2" || s.Expected != "ok" {
		t.Fatalf("runtime comparison step = %#v, want compare_query with both queries and ok expectation", s)
	}
}

// TestDoTestRuntimeComparisonCapturesCompareQuery covers the do_test body form:
// the body's query result is compared against the runtime query.
func TestDoTestRuntimeComparisonCapturesCompareQuery(t *testing.T) {
	i := NewInterp()
	src := `do_test sample-1.1 { db eval {SELECT * FROM t1} } [db eval {SELECT a FROM t2 ORDER BY a}]`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 {
		t.Fatalf("runtime comparison statements = %#v, want one compare_query", stmts)
	}
	s := stmts[0]
	if s.Type != "compare_query" || strings.TrimSpace(s.SQL) != "SELECT * FROM t1" || strings.TrimSpace(s.AuxSQL) != "SELECT a FROM t2 ORDER BY a" || s.Expected != "ok" {
		t.Fatalf("runtime comparison step = %#v, want compare_query with both queries and ok expectation", s)
	}
}

// TestSqliteCurrentTimeRuntimeAssignment pins date.test:402, where the clock
// override is computed by a query rather than a literal.
func TestSqliteCurrentTimeRuntimeAssignment(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set sqlite_current_time [db eval {SELECT strftime('%s','2003-10-22 12:34:00')}]`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "query_assign" || stmts[0].Variable != "sqlite_current_time" || stmts[0].SQL != "SELECT strftime('%s','2003-10-22 12:34:00')" {
		t.Fatalf("clock assignment statements = %#v, want query_assign for sqlite_current_time", stmts)
	}
}

// TestHexioWriteCapturesFilePatch pins the legacy file-format helper used by
// date-14.x (../sqlite/test/date.test:528): a byte patch at an absolute file
// offset, with the payload often produced by `format %02x`.
func TestHexioWriteCapturesFilePatch(t *testing.T) {
	i := NewInterp()
	src := `hexio_write test.db 2040 4142ba32bffffff9
for {set i 0} {$i<=1} {incr i} {
  hexio_write test.db 2047 [format %02x $i]
}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 {
		t.Fatalf("hexio_write statements = %#v, want three patch markers", stmts)
	}
	if s := stmts[0]; s.Type != "hexio_write" || s.FileTarget != "test.db" || s.FileOffset != 2040 || s.FileData != "4142ba32bffffff9" {
		t.Fatalf("hexio_write target step = %#v", s)
	}
	if s := stmts[1]; s.Type != "hexio_write" || s.FileOffset != 2047 || s.FileData != "00" {
		t.Fatalf("hexio_write first loop step = %#v", s)
	}
	if s := stmts[2]; s.FileOffset != 2047 || s.FileData != "01" {
		t.Fatalf("hexio_write second loop step = %#v", s)
	}
}

// TestSQLite3HasCodecResult pins the `sqlite3 -has-codec` probe used by
// date.test:519 to guard the hexio block; the Tcl build reports 0 (no codec),
// so the guarded body must execute.
func TestSQLite3HasCodecResult(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`if {0==[sqlite3 -has-codec]} { set codec no } else { set codec yes }`); err != nil {
		t.Fatal(err)
	}
	if got, _ := i.Variable("codec"); got != "no" {
		t.Fatalf("codec probe branch = %q, want %q", got, "no")
	}
}

// TestDoTestFinalExprWithRuntimeVarEmitsRuntimeExpr pins date.test:545-548:
// the body's result is a Tcl `expr` over a value fetched by an earlier
// `db one` assignment. The expression cannot be evaluated offline, so the
// converted fixture must carry a runtime_expr assertion instead of silently
// parking the expectation on the query_assign step.
func TestDoTestFinalExprWithRuntimeVarEmitsRuntimeExpr(t *testing.T) {
	i := NewInterp()
	src := `do_test date-14.2.0 {
  set date [db one {SELECT datetime(x) FROM t1}]
  expr {$date eq "2008-06-12 00:00:00" || $date eq "2008-06-11 23:59:59"}
} {1}`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("statements = %#v, want query_assign then runtime_expr", stmts)
	}
	if stmts[0].Type != "query_assign" || stmts[0].Variable != "date" || stmts[0].Expected != "" {
		t.Fatalf("assignment step = %#v, want query_assign for date without expectation", stmts[0])
	}
	s := stmts[1]
	if s.Type != "runtime_expr" || strings.TrimSpace(s.SQL) != `$date eq "2008-06-12 00:00:00" || $date eq "2008-06-11 23:59:59"` || s.Expected != "1" {
		t.Fatalf("runtime expression step = %#v, want the source expr with expectation 1", s)
	}
}

// TestDoTestFinalExprWithoutRuntimeVarStaysOffline ensures the runtime_expr
// emission is scoped to expressions that read a runtime-captured variable;
// fully resolvable expressions keep the previous offline behavior.
func TestDoTestFinalExprWithoutRuntimeVarStaysOffline(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_test sample-9.9 { expr {1 + 2} } {3}`); err != nil {
		t.Fatal(err)
	}
	for _, s := range i.Stmts() {
		if s.Type == "runtime_expr" {
			t.Fatalf("unexpected runtime_expr for offline-resolvable expression: %#v", s)
		}
	}
}

// TestSQLiteCreateFunctionEmitsTestFuncsMarker pins the testfixture helper
// `sqlite3_create_function DB` (test1.c test_create_function): the converter
// must emit a register_test_funcs marker so the harness can install the
// built-in test SQL functions (ptrchng.test relies on pointer_change).
func TestSQLiteCreateFunctionEmitsTestFuncsMarker(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`sqlite3_create_function db`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].Type != "register_test_funcs" || stmts[0].Database != "" {
		t.Fatalf("test-funcs marker = %#v", stmts)
	}
	i2 := NewInterp()
	if err := i2.Execute(`sqlite3_create_function db2`); err != nil {
		t.Fatal(err)
	}
	stmts2 := i2.Stmts()
	if len(stmts2) != 1 || stmts2[0].Type != "register_test_funcs" || stmts2[0].Database != "db2" {
		t.Fatalf("named test-funcs marker = %#v", stmts2)
	}
}

// TestDBEvalBindsEscapedParamsInQuotedSQL pins atof1.test's pattern
// `set y [db eval "SELECT $xf=\$x"]`: TCL consumes the unescaped $xf during
// substitution, and the escaped \$x survives as a SQL parameter that
// testfixture binds from the TCL variable of the same name. The converter must
// inline the current value as an SQL literal; leaving $x in the fixture would
// execute the query with an unbound parameter.
func TestDBEvalBindsEscapedParamsInQuotedSQL(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set x -Inf
set y [db eval "SELECT -Inf=\$x"]`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "SELECT -Inf='-Inf'" {
		t.Fatalf("captured statement = %#v, want escaped $x bound as '-Inf'", stmts)
	}
}

// TestDBEvalBindsNumericEscapedParams keeps the numeric case bare: a value TCL
// would bind as a number (tclsqlite.c:1526 sqlite3_bind_double) must not become
// a quoted text literal.
func TestDBEvalBindsNumericEscapedParams(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set x 3.5
set y [db eval "SELECT 3.5=\$x"]`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || stmts[0].SQL != "SELECT 3.5=3.5" {
		t.Fatalf("captured statement = %#v, want escaped $x bound as 3.5", stmts)
	}
}

// TestDoCatchsqlBindsTCLVariables pins do_catchsql_test's parameter binding:
// testfixture's catchsql binds $name parameters from the caller's TCL scope,
// so the converter must inline compile-time-known loop variables instead of
// leaving them for the harness. rtree8-2.1.3.$i otherwise ran the same SQL
// "SELECT * FROM t1 WHERE id = $i" in all 50 iterations.
func TestDoCatchsqlBindsTCLVariables(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`for {set i 1} {$i <= 3} {incr i} {
  do_catchsql_test rtree8-2.1.3.$i {
    SELECT * FROM t1 WHERE id = $i
  } {1 {database disk image is malformed}}
}`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 {
		t.Fatalf("captured %d statements, want 3: %#v", len(stmts), stmts)
	}
	want := []string{
		"SELECT * FROM t1 WHERE id = 1",
		"SELECT * FROM t1 WHERE id = 2",
		"SELECT * FROM t1 WHERE id = 3",
	}
	for n, w := range want {
		if got := strings.TrimSpace(stmts[n].SQL); got != w {
			t.Errorf("statement %d SQL = %q, want %q", n, got, w)
		}
	}
}
