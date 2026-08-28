package main

import (
	"testing"

	"github.com/lmorg/readline/v4"
)

// TestUnicodeSetRPosClamp verifies that UnicodeT.Set() clamps rPos
// when the new value is shorter than the previous one.
//
// Regression test for crash:
//   readline.go:299 → vim_delete.go:68 → write.go:78 → write.go:176
//   panic: slice bounds out of range [:2] with capacity 0
//
// Triggered by typing "cr" + Tab + Enter: the tab completer replaces
// "cr" with an empty line, but Set() left rPos=2, causing
// Runes()[:RunePos()] on a 0-length slice to panic.
func TestUnicodeSetRPosClamp(t *testing.T) {
	rl := readline.NewInstance()
	if rl == nil {
		t.Fatal("NewInstance() returned nil")
	}

	// UnicodeT is an exported type from the readline package
	ut := new(readline.UnicodeT)

	// Step 1: Set line to "cr", cursor at position 2
	ut.Set(rl, []rune("cr"))
	ut.SetRunePos(2)
	if pos := ut.RunePos(); pos != 2 {
		t.Fatalf("after SetRunePos(2): RunePos() = %d, want 2", pos)
	}
	if n := ut.RuneLen(); n != 2 {
		t.Fatalf("after Set('cr'): RuneLen() = %d, want 2", n)
	}

	// Step 2: Set line to empty (simulates viDeleteByAdjustLogic
	// returning empty line when deleting "cr" with adjust=-2)
	ut.Set(rl, []rune{})
	if n := ut.RuneLen(); n != 0 {
		t.Fatalf("after Set(''): RuneLen() = %d, want 0", n)
	}

	// Without the fix, RunePos() would still be 2 and any operation
	// using Runes()[:RunePos()] would panic.
	if pos := ut.RunePos(); pos != 0 {
		t.Errorf("after Set(''): RunePos() = %d, want 0 (clamped)", pos)
	}

	// Step 3: Verify Runes() can be sliced at RunePos() without panic
	runes := ut.Runes()
	rp := ut.RunePos()
	if rp > len(runes) {
		t.Errorf("RunePos()=%d > len(Runes())=%d, would panic: slice bounds out of range", rp, len(runes))
	}
	// Safe slice that would have panicked before the fix
	_ = runes[:rp]
	t.Logf("OK: runes[:%d] on %d-length slice (no panic)", rp, len(runes))
}

// TestUnicodeSetRPosClampPartial verifies clamping works when
// the new line is shorter but not empty.
func TestUnicodeSetRPosClampPartial(t *testing.T) {
	rl := readline.NewInstance()
	ut := new(readline.UnicodeT)

	// Set line to "hello", cursor at position 5
	ut.Set(rl, []rune("hello"))
	ut.SetRunePos(5)
	if pos := ut.RunePos(); pos != 5 {
		t.Fatalf("after SetRunePos(5): RunePos() = %d, want 5", pos)
	}

	// Set to "hi" (shorter, cursor should clamp to 2)
	ut.Set(rl, []rune("hi"))
	if pos := ut.RunePos(); pos != 2 {
		t.Errorf("after Set('hi'): RunePos() = %d, want 2 (clamped to len)", pos)
	}

	// Verify no panic when slicing
	runes := ut.Runes()
	rp := ut.RunePos()
	if rp > len(runes) {
		t.Errorf("RunePos()=%d > len(Runes())=%d, would panic", rp, len(runes))
	}
	_ = runes[:rp]
	t.Logf("OK: partial clamp, runes[:%d] on %d-length slice", rp, len(runes))
}

// TestUnicodeSetRPosNoChange verifies that Set() doesn't change rPos
// when the new line is longer (no clamping needed).
func TestUnicodeSetRPosNoChange(t *testing.T) {
	rl := readline.NewInstance()
	ut := new(readline.UnicodeT)

	ut.Set(rl, []rune("hi"))
	ut.SetRunePos(2)

	// Set to longer line — rPos should stay at 2
	ut.Set(rl, []rune("hello"))
	if pos := ut.RunePos(); pos != 2 {
		t.Errorf("after Set('hello'): RunePos() = %d, want 2 (unchanged)", pos)
	}

	// Verify no panic
	runes := ut.Runes()
	rp := ut.RunePos()
	if rp > len(runes) {
		t.Errorf("RunePos()=%d > len(Runes())=%d, would panic", rp, len(runes))
	}
	_ = runes[:rp]
	t.Logf("OK: no clamping needed, runes[:%d] on %d-length slice", rp, len(runes))
}
