// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"strings"
	"testing"
)

// TestReadFTS3VarintEmitsRuntimeExpr pins fts3cov 2.x: the blob argument is
// runtime ($root), so the nested string range must stay in the emitted
// runtime_expr instead of being evaluated at conversion time.
func TestReadFTS3VarintEmitsRuntimeExpr(t *testing.T) {
	i := NewInterp()
	i.dynamicVars["root"] = true
	if err := i.Execute(`read_fts3varint [string range $root 1 end] left_child`); err != nil {
		t.Fatal(err)
	}
	if len(i.stmts) != 1 {
		t.Fatalf("stmts = %#v, want 1", i.stmts)
	}
	s := i.stmts[0]
	if s.Type != "runtime_expr" || s.Variable != "left_child" ||
		strings.TrimSpace(s.SQL) != "read_fts3varint [string range $root 1 end] left_child" {
		t.Fatalf("stmt = %#v", s)
	}
}

// TestMallocCommonWrappers pins the subset of malloc_common.tcl that the JSON
// converter materializes: successful writes become exec steps and expected
// errors become catch steps with do_catchsql_test's "1 {msg}" form.
func TestMallocCommonWrappers(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`do_write_test mw sqlite_master {CREATE TABLE x(a)}`); err != nil {
		t.Fatal(err)
	}
	if err := i.Execute(`do_error_test me {CREATE VIRTUAL TABLE y USING fts3(a, tokenize unknown)} {unknown tokenizer: unknown}`); err != nil {
		t.Fatal(err)
	}
	if len(i.stmts) != 2 {
		t.Fatalf("stmts = %#v, want 2", i.stmts)
	}
	write := i.stmts[0]
	if write.Type != "exec" || write.SQL != "CREATE TABLE x(a)" || write.Expected != "" || write.TestName != "mw" {
		t.Errorf("write step = %#v", write)
	}
	fail := i.stmts[1]
	if fail.Type != "catch" || fail.SQL != "CREATE VIRTUAL TABLE y USING fts3(a, tokenize unknown)" ||
		fail.Expected != "1 {unknown tokenizer: unknown}" || fail.TestName != "me" {
		t.Errorf("error step = %#v", fail)
	}
}
