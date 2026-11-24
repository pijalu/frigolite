package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// emitUpdateTriggerRows renders the firing UPDATE triggers' sub-program
// clusters (trigger.c's codeTriggerProgram) into the synthetic EXPLAIN
// stream. trigger7-2.x looks for the SELECT literal of each firing trigger
// in the flattened row stream, so the literal is carried as an OP_String8
// p4 value — the opcode C emits for a constant SELECT in a trigger body.
func (e *SelectEngine) emitUpdateTriggerRows(prog *explainProgram, s *sql.UpdateStmt) {
	triggers, err := e.ctx.Schema().FindTriggersForTable(s.Table)
	if err != nil {
		return
	}
	setCols := updateSetColumnNames(s)
	for _, trigger := range triggers {
		header, body := triggerHeaderBody(trigger.SQL)
		if !updateTriggerFires(header, setCols) {
			continue
		}
		for _, literal := range sqlStringLiterals(body) {
			prog.emit("String8", 0, 0, 0, literal, 0, "")
		}
	}
}

// updateSetColumnNames lists the columns assigned by this UPDATE (the
// UPDATE OF selectivity set — trigger.c compares the SET bitmask).
func updateSetColumnNames(s *sql.UpdateStmt) map[string]bool {
	cols := make(map[string]bool)
	for _, a := range s.Assignments {
		cols[strings.ToLower(a.Column)] = true
	}
	for _, c := range s.SetParenColumns {
		cols[strings.ToLower(c)] = true
	}
	return cols
}

// triggerHeaderBody splits a CREATE TRIGGER statement at BEGIN. The header
// is upper-cased for keyword sniffing; the body keeps the original literals.
func triggerHeaderBody(stmt string) (string, string) {
	upper := strings.ToUpper(stmt)
	i := strings.Index(upper, "BEGIN")
	if i < 0 {
		return upper, ""
	}
	return upper[:i], stmt[i+len("BEGIN"):]
}

// updateTriggerFires reports whether an UPDATE trigger's OF column list
// intersects the statement's SET columns. A plain UPDATE trigger has no OF
// list and always fires; a non-UPDATE trigger never does.
func updateTriggerFires(header string, setCols map[string]bool) bool {
	on := strings.Index(header, " ON ")
	if on < 0 {
		return false
	}
	header = header[:on]
	if !strings.Contains(header, "UPDATE") {
		return false
	}
	of := strings.Index(header, " OF ")
	if of < 0 {
		return true
	}
	for _, col := range strings.Split(header[of+len(" OF "):], ",") {
		col = strings.ToLower(strings.Trim(col, " \t\r\n\"'[]`"))
		if setCols[col] {
			return true
		}
	}
	return false
}

// sqlStringLiterals scans a trigger body for single-quoted SQL literals,
// honoring the doubled-quote escape. Opcodes for SELECT constants carry
// these values in their p4 column in C's EXPLAIN output.
func sqlStringLiterals(body string) []string {
	var out []string
	for i := 0; i < len(body); {
		if body[i] != '\'' {
			i++
			continue
		}
		i++
		var lit strings.Builder
		for i < len(body) {
			if body[i] == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					lit.WriteByte('\'')
					i += 2
					continue
				}
				i++
				break
			}
			lit.WriteByte(body[i])
			i++
		}
		out = append(out, lit.String())
	}
	return out
}
