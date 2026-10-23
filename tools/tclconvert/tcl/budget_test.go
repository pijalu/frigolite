// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestStatementBudgetStopsRunawayLoops pins the guard that keeps runtime-
// terminated loops (backup_ioerr.test's `for {set i 1} {$stop == 0}`) from
// unrolling into multi-million-statement fixtures. The budget error must
// unwind the loop and stick, so even a loop wrapped in `catch` cannot keep
// allocating.
func TestStatementBudgetStopsRunawayLoops(t *testing.T) {
	i := NewInterp()
	i.SetStatementBudget(50)
	err := i.Execute(`set i 0
while {1} {
  incr i
  db eval {SELECT 1}
}`)
	if !errors.Is(err, ErrStatementBudget) {
		t.Fatalf("Execute error = %v, want ErrStatementBudget", err)
	}
	if got := len(i.Stmts()); got < 50 {
		t.Fatalf("captured %d statements, want at least the budget", got)
	}
}

// TestStatementBudgetSticksThroughCatch ensures the runaway error is reported
// again on the next command even when a TCL catch swallows it, so a
// `while {1} { catch { ... } }` loop still terminates.
func TestStatementBudgetSticksThroughCatch(t *testing.T) {
	i := NewInterp()
	i.SetStatementBudget(20)
	err := i.Execute(`while {1} {
  catch { db eval {SELECT 1} }
}`)
	if !errors.Is(err, ErrStatementBudget) {
		t.Fatalf("Execute error = %v, want ErrStatementBudget", err)
	}
}

// TestConversionDeadlineStopsExecution pins the cooperative timeout: the
// interpreter checks the deadline between commands instead of leaving an
// abandoned goroutine running (which kept appending statements forever).
func TestConversionDeadlineStopsExecution(t *testing.T) {
	i := NewInterp()
	i.SetDeadline(time.Now().Add(-time.Second))
	err := i.Execute(`set x [db eval {SELECT 1}]`)
	if !errors.Is(err, ErrConversionDeadline) {
		t.Fatalf("Execute error = %v, want ErrConversionDeadline", err)
	}
}

// TestStatementBudgetDoesNotAffectSmallFiles keeps the normal conversion path
// untouched: a file below the budget returns its statements and no error.
func TestStatementBudgetDoesNotAffectSmallFiles(t *testing.T) {
	i := NewInterp()
	i.SetStatementBudget(1000)
	if err := i.Execute(`db eval {CREATE TABLE t1(a)}`); err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 || !strings.Contains(stmts[0].SQL, "CREATE TABLE t1") {
		t.Fatalf("statements = %#v, want the CREATE TABLE statement", stmts)
	}
}
