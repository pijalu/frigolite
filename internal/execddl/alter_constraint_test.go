package execddl

import "testing"

func TestDropColumnConstraintAfterLineComment(t *testing.T) {
	input := "CREATE TABLE t1(a, b, c -- comment\n  CONSTRAINT abc NOT NULL\n  )"
	got := removeConstraintFromSQL(input, "abc")
	want := "CREATE TABLE t1(a, b, c)"
	if got != want {
		t.Fatalf("removeConstraintFromSQL() = %q, want %q", got, want)
	}
}

func TestDropColumnConstraintPreservesGeneratedColumn(t *testing.T) {
	input := "CREATE TABLE t1(a, b, c CONSTRAINT abc AS (b+1))"
	got := removeConstraintFromSQL(input, "abc")
	want := "CREATE TABLE t1(a, b, c AS (b+1))"
	if got != want {
		t.Fatalf("removeConstraintFromSQL() = %q, want %q", got, want)
	}
}

func TestDropColumnConstraintPreservesStoredGeneratedColumn(t *testing.T) {
	input := "CREATE TABLE t1(a, b, c CONSTRAINT abc GENERATED ALWAYS AS (b+1) STORED)"
	got := removeConstraintFromSQL(input, "abc")
	want := "CREATE TABLE t1(a, b, c GENERATED ALWAYS AS (b+1) STORED)"
	if got != want {
		t.Fatalf("removeConstraintFromSQL() = %q, want %q", got, want)
	}
}
