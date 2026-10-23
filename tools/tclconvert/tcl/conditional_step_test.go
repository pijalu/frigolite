// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "testing"

// TestConditionalStepsCaptureBothBranches pins the runtime-conditional model:
//
//	set rc [catch {db eval "INSERT ..."}]
//	if {$rc} { ... } else { ... }
//
// The `catch` wraps a SQL statement, so its result is only known when the
// harness runs it. The converter must therefore capture BOTH branches and
// guard each with the condition that selects it — dropping the untaken
// branch loses the statements it would have executed (pager2's
// `ROLLBACK TO sp_$x`).
func TestConditionalStepsCaptureBothBranches(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`set rc [catch {db eval {INSERT INTO t1 VALUES(1)}}]
if {$rc} {
  db eval {INSERT INTO t2 VALUES(1)}
} else {
  db eval {INSERT INTO t3 VALUES(1)}
}`)
	if err != nil {
		t.Fatalf("catch/if: %v", err)
	}
	stmts := i.Stmts()
	if len(stmts) != 3 {
		t.Fatalf("captured %d statements, want 3 (the catch plus both branches): %#v", len(stmts), stmts)
	}
	catchStmt := stmts[0]
	if catchStmt.Type != "catch" {
		t.Errorf("the caught statement has type %q, want catch", catchStmt.Type)
	}
	if !catchStmt.CatchResult {
		t.Errorf("the caught statement must be marked as a catch result")
	}
	if catchStmt.Variable != "rc" {
		t.Errorf("the catch result variable = %q, want rc", catchStmt.Variable)
	}
	if stmts[1].Condition == "" {
		t.Errorf("the if-branch must carry its condition, got none")
	}
	if stmts[2].Condition == "" {
		t.Errorf("the else-branch must carry its condition, got none")
	}
	if stmts[1].Condition == stmts[2].Condition {
		t.Errorf("the branches must carry different conditions: %q", stmts[1].Condition)
	}
}

// TestConditionalStepsDoNotFireForStaticConditions pins that a condition the
// converter *can* decide keeps the old behaviour: only the branch that will
// actually run is captured (no condition annotation).
func TestConditionalStepsDoNotFireForStaticConditions(t *testing.T) {
	i := NewInterp()
	err := i.Execute(`if {1} {
  db eval {INSERT INTO t1 VALUES(1)}
} else {
  db eval {INSERT INTO t2 VALUES(1)}
}`)
	if err != nil {
		t.Fatalf("if: %v", err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 {
		t.Fatalf("captured %d statements, want 1 (only the taken branch)", len(stmts))
	}
	if stmts[0].Condition != "" {
		t.Errorf("a statically decided branch must not carry a condition, got %q", stmts[0].Condition)
	}
}
