// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"strconv"
	"strings"
)

// A runtime query value is not known at conversion time: the rows of a
// `db eval`/`execsql` only exist when the statement runs. The interpreter
// marks such a value so that the commands which post-process it (a sort, a
// list operation) can record what they do to the *rendered* rows instead of
// silently operating on an empty list.
//
// where7.test's count_steps_sort helper is the motivating case:
//
//	proc count_steps_sort {sql} {
//	  set r [lsort -integer [db eval $sql]]
//	  return "$r scan [db status step] sort [db status sort]"
//	}
//
// The expectation of the test is the *sorted* row list, so the fixture must
// record that the rows are sorted; the harness then sorts the rows it
// rendered before comparing them with the expectation.
//
// The marker never reaches an expectation or a SQL statement: the places
// that bake a value into one strip it (stripRuntimeValues).
const runtimeValuePrefix = "\x00frt:"

// runtimeValueMarker returns the marker of the value of a captured query.
// The id identifies the statement whose rows the value holds.
func runtimeValueMarker(id int) string {
	return runtimeValuePrefix + strconv.Itoa(id) + "\x00"
}

// runtimeValueID reports the id of the query whose value a string carries.
// ok is false when the string carries no marker.
func runtimeValueID(s string) (int, bool) {
	start := strings.Index(s, runtimeValuePrefix)
	if start < 0 {
		return 0, false
	}
	rest := s[start+len(runtimeValuePrefix):]
	end := strings.Index(rest, "\x00")
	if end < 0 {
		return 0, false
	}
	id, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return id, true
}

// stripRuntimeValues removes the runtime markers from a value. The markers
// are an internal representation: they must never appear in an expectation,
// in a SQL statement or in a rendered result.
func stripRuntimeValues(s string) string {
	if !strings.Contains(s, runtimeValuePrefix) {
		return s
	}
	var b strings.Builder
	for {
		start := strings.Index(s, runtimeValuePrefix)
		if start < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:start])
		rest := s[start+len(runtimeValuePrefix):]
		end := strings.Index(rest, "\x00")
		if end < 0 {
			s = rest
			break
		}
		s = rest[end+1:]
	}
	return b.String()
}

// markRuntimeValue tags a captured statement with a runtime value id and
// returns the marker. The id ties the value to the statement whose rows it
// holds, so a later command (a sort) can find the statement even when the
// statement list has been re-ordered (a runtime loop body moves its
// statements into a nested step).
func (i *Interp) markRuntimeValue(stmt *Stmt) string {
	i.runtimeID++
	stmt.runtimeID = i.runtimeID
	return runtimeValueMarker(i.runtimeID)
}

// StripRuntimeValues removes the runtime markers from a value. It is the
// boundary of the internal representation: a marker identifies a query whose
// rows exist only at run time, and it must never reach a fixture.
func StripRuntimeValues(s string) string {
	return stripRuntimeValues(s)
}

// runtimeValueStatement returns the statement whose rows a value holds, when
// the value is a runtime query value.
func (i *Interp) runtimeValueStatement(value string) *Stmt {
	id, ok := runtimeValueID(value)
	if !ok {
		return nil
	}
	return i.runtimeStatement(id)
}

// runtimeStatement returns the statement holding the rows of a runtime value.
func (i *Interp) runtimeStatement(id int) *Stmt {
	for idx := range i.stmts {
		if i.stmts[idx].runtimeID == id {
			return &i.stmts[idx]
		}
	}
	return nil
}
