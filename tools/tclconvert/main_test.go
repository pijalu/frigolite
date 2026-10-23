// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

func TestCompressFixtureJSONIsDeterministicAndLossless(t *testing.T) {

	input := []byte(strings.Repeat(`{"sql":"INSERT INTO t VALUES(1)"}`+"\n", 1000))
	first, err := compressFixtureJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compressFixtureJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("gzip output is not deterministic")
	}
	if len(first) >= len(input) {
		t.Fatalf("compressed size %d >= input size %d", len(first), len(input))
	}
	reader, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatal("decompressed JSON differs from original")
	}
}

func TestConvertKeepsSelectTestCasesInOneOrderedHarnessCase(t *testing.T) {
	data := convertToJSON("e_createtable", []tcl.Stmt{
		{Type: "exec", SQL: "CREATE TABLE t1(a)", TestName: "e_createtable-3.2.3.1", GroupName: "select_tests_1"},
		{Type: "exec", SQL: "INSERT INTO t1 VALUES(1)", TestName: "e_createtable-3.2.3.1", GroupName: "select_tests_1"},
		{Type: "query", SQL: "SELECT a FROM t1", Expected: "1", TestName: "e_createtable-3.2.3.1", GroupName: "select_tests_1"},
		{Type: "exec", SQL: "INSERT INTO t1 VALUES(2)", TestName: "e_createtable-3.2.3.2", GroupName: "select_tests_1"},
		{Type: "query", SQL: "SELECT a FROM t1", Expected: "1 2", TestName: "e_createtable-3.2.3.2", GroupName: "select_tests_1"},
	}, "")
	if len(data.Tests) != 1 {
		t.Fatalf("converted helper call split into %d harness cases; want one sequential case", len(data.Tests))
	}
	if got := len(data.Tests[0].Steps); got != 5 {
		t.Fatalf("converted helper steps = %d, want 5 ordered steps: %#v", got, data.Tests[0].Steps)
	}
}

func TestAltercolRenameHelperKeepsTclListExpectationAndGrouping(t *testing.T) {
	src := `proc rename_test {tn old new schema} {
  for {set i 0} {$i < 2} {incr i} {
    drop_all_tables_and_views db
    set sorted [list]
    foreach sql $schema { execsql $sql; lappend sorted [string trim $sql] }
    set sorted [lsort $sorted]
    do_execsql_test $tn.$i.1 {SELECT sql FROM sqlite_master ORDER BY 1} $sorted
    do_execsql_test $tn.$i.2 "ALTER TABLE t1 RENAME $old TO $new"
    do_execsql_test $tn.$i.3 {SELECT sql FROM sqlite_master ORDER BY 1} [string map [list $old $new] $sorted]
  }
}
rename_test altercol-9.1 _x_ _xxx_ {{CREATE TABLE t1(a, _x_)} {CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN SELECT _x_ FROM t1; END}}`
	stmts, _, err := runInterpreter([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	data := convertToJSON("altercol", stmts, "")
	if len(data.Tests) != 10 {
		t.Fatalf("converted test groups = %d, want two setup/reset runs and six checks/renames: %#v", len(data.Tests), data.Tests)
	}
	if got := data.Tests[2].Steps[0].Expect; !strings.Contains(got, "CREATE TABLE t1(a, _x_)") || !strings.Contains(got, "CREATE TRIGGER tr AFTER INSERT") {
		t.Fatalf("initial Tcl list expectation = %q, want resolved schema list", got)
	}
	if got := data.Tests[4].Steps[0].Expect; !strings.Contains(got, "_xxx_") || strings.Contains(got, "_x_)") {
		t.Fatalf("mapped Tcl list expectation = %q, want old-to-new substitution", got)
	}
}

func TestConvertPreservesFunctionMarker(t *testing.T) {
	data := convertToJSON("trustschema1", []tcl.Stmt{{Type: "register_function", FunctionName: "f1", FunctionProc: "f1", FunctionArgs: []string{"x"}, FunctionBody: "return $x", MinArgs: 1, MaxArgs: 1, Deterministic: true, Innocuous: true, TestName: "trustschema1-1.100"}}, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 1 {
		t.Fatalf("function marker conversion = %#v", data.Tests)
	}
	step := data.Tests[0].Steps[0]
	if step.Type != "register_function" || step.FunctionName != "f1" || step.FunctionBody != "return $x" || !step.Deterministic || !step.Innocuous || step.DirectOnly {
		t.Fatalf("function marker = %#v", step)
	}
}

func TestConvertPreservesConnectionLifecycleMarkerOrder(t *testing.T) {
	data := convertToJSON("vtab1", []tcl.Stmt{
		{Type: "close_db", TestName: "vtab1-1.10"},
		{Type: "reopen_db", SQL: "test.db", TestName: "vtab1-1.10"},
		{Type: "register_echo_module", TestName: "vtab1-1.10"},
	}, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 3 {
		t.Fatalf("converted lifecycle steps = %#v, want 3 ordered steps", data.Tests)
	}
	steps := data.Tests[0].Steps
	if steps[0].Type != "close_db" || steps[1].Type != "reopen_db" || steps[1].SQL != "test.db" || steps[2].Type != "register_echo_module" {
		t.Fatalf("converted lifecycle steps = %#v, want close, reopen test.db, register", steps)
	}
}

func TestConvertPreservesNamedConnection(t *testing.T) {
	data := convertToJSON("vtab1", []tcl.Stmt{{Type: "exec", SQL: "INSERT INTO t VALUES(1)", Database: "db2", TestName: "vtab1-1.1"}}, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 1 || data.Tests[0].Steps[0].Database != "db2" {
		t.Fatalf("converted named connection = %#v, want db2", data.Tests)
	}
}

func TestConvertPreservesResultTransform(t *testing.T) {
	data := convertToJSON("func", []tcl.Stmt{{Type: "query", SQL: "SELECT round(1e500)", Expected: "inf", ResultTransform: "tolower", TestName: "func-4.39"}}, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 1 || data.Tests[0].Steps[0].ResultTransform != "tolower" {
		t.Fatalf("converted result transform = %#v, want tolower", data.Tests)
	}
}

func TestConvertPreservesTclCallbackRegistration(t *testing.T) {
	proc := tcl.Proc{Name: "vtab_command", Args: []string{"method", "args"}, Body: "return schema"}
	data := convertToJSON("bestindex1", []tcl.Stmt{{Type: "register_tcl_module", ModuleArgs: []string{"db"}, CallbackProcs: []tcl.Proc{proc}}}, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 1 {
		t.Fatalf("converted TCL module marker = %#v", data.Tests)
	}
	step := data.Tests[0].Steps[0]
	if len(step.CallbackProcs) != 1 || step.CallbackProcs[0].Name != proc.Name || step.CallbackProcs[0].Body != proc.Body || len(step.ModuleArgs) != 1 || step.ModuleArgs[0] != "db" {
		t.Fatalf("converted TCL callback = %#v", step)
	}
}

func TestConvertPreservesCustomCollationLifecycle(t *testing.T) {
	src := `set ::caseless_del 0
proc caseless_cmp {zLeft zRight} { string compare -nocase $zLeft $zRight }
do_test collation-1 { sqlite3_create_collation_v2 db CASELESS caseless_cmp {incr ::caseless_del}; set ::caseless_del } {0}
do_test collation-2 { sqlite_delete_collation db CASELESS; set ::caseless_del } {1}`
	stmts, _, err := runInterpreter([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	data := convertToJSON("collate7", stmts, "")
	if len(data.Tests) != 2 || len(data.Tests[0].Steps) != 2 || len(data.Tests[1].Steps) != 2 {
		t.Fatalf("collation lifecycle steps = %#v", data.Tests)
	}
	register, remove := data.Tests[0].Steps[0], data.Tests[1].Steps[0]
	if register.Type != "register_collation" || register.Database != "db" || register.CollationName != "CASELESS" || register.CollationProc != "caseless_cmp" || !strings.Contains(register.CollationBody, "string compare -nocase") || register.DestructorBody != "incr ::caseless_del" {
		t.Fatalf("registration marker = %#v", register)
	}
	if remove.Type != "delete_collation" || remove.Database != "db" || remove.CollationName != "CASELESS" {
		t.Fatalf("deletion marker = %#v", remove)
	}
}

func TestConvertPreservesDBCollationCallback(t *testing.T) {
	src := `proc reverse_sort {lhs rhs} { return [string compare $rhs $lhs] }
db collate "reverse sort" reverse_sort`
	stmts, _, err := runInterpreter([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	data := convertToJSON("collate9", stmts, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 1 {
		t.Fatalf("db collate marker = %#v", data.Tests)
	}
	step := data.Tests[0].Steps[0]
	if step.Type != "register_collation" || step.CollationName != "reverse sort" || step.CollationProc != "reverse_sort" || !strings.Contains(step.CollationBody, "$rhs $lhs") {
		t.Fatalf("db collation callback = %#v", step)
	}
}

func TestCompactRepeatedStepsRoundTripsSequence(t *testing.T) {
	steps := []TestStep{{Type: "exec", SQL: "BEGIN"}, {Type: "catch", SQL: "bad", CatchResult: true}, {Type: "catch", SQL: "bad", CatchResult: true}, {Type: "exec", SQL: "COMMIT"}}
	compact := compactRepeatedSteps(steps)
	if len(compact) != 3 || compact[1].RepeatCount != 2 || compact[1].RepeatBody[0].SQL != "bad" {
		t.Fatalf("compact steps = %#v", compact)
	}
	var expanded []TestStep
	for _, step := range compact {
		if step.Type == "repeat" {
			for i := 0; i < step.RepeatCount; i++ {
				expanded = append(expanded, step.RepeatBody...)
			}
		} else {
			expanded = append(expanded, step)
		}
	}
	if !reflect.DeepEqual(expanded, steps) {
		t.Fatalf("expanded = %#v, want %#v", expanded, steps)
	}
}

func TestConvertRetainsEchoRegistrationMarker(t *testing.T) {
	data := convertToJSON("vtab1", []tcl.Stmt{{Type: "register_echo_module"}}, "")
	if len(data.Tests) != 1 || len(data.Tests[0].Steps) != 1 || data.Tests[0].Steps[0].Type != "register_echo_module" {
		t.Fatalf("converted registration marker = %#v, want one register_echo_module step", data.Tests)
	}
}
