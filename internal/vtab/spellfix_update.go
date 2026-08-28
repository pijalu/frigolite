package vtab

import (
	"fmt"
	"strings"
)

// xUpdate implementation (spellfix1Update) for the spellfix1 module.
//
// The declared columns map to the shadow "%_vocab" table: word/rank/langid
// are stored, soundslike feeds k1, command carries meta-commands. Conflict
// resolution mirrors spellfix1GetConflict: the OR action flows into the
// shadow INSERT OR / UPDATE OR clause; a violated id uniqueness becomes the
// bare "constraint failed" error (xUpdate returns SQLITE_CONSTRAINT with no
// message, so the core renders its generic constraint text).
func (v *spellfixVTab) InsertRow(values []interface{}) (int64, error) {
	return v.insertRow(values, 0, false, "")
}

// InsertRowConflict implements ConflictAwareInserter.
func (v *spellfixVTab) InsertRowConflict(values []interface{}, resolve string) (int64, error) {
	return v.insertRow(values, 0, false, resolve)
}

// InsertRowWithRowid implements RowidConflictWriter (xUpdate argv[0]=NULL,
// argv[1]=explicit rowid).
func (v *spellfixVTab) InsertRowWithRowid(values []interface{}, rowid int64, resolve string) (int64, error) {
	return v.insertRow(values, rowid, true, resolve)
}

// UpdateRow implements RowUpdater (generic path: rowid unchanged).
func (v *spellfixVTab) UpdateRow(oldValues, newValues []interface{}) error {
	_, _, err := v.updateRow(oldValues, newValues, 0, 0, false, "")
	return err
}

// UpdateRowWithRowid implements RowidConflictWriter.
func (v *spellfixVTab) UpdateRowWithRowid(oldValues []interface{}, oldRowid int64, newValues []interface{}, newRowid int64, resolve string) (bool, bool, error) {
	return v.updateRow(oldValues, newValues, oldRowid, newRowid, true, resolve)
}

// DeleteRow implements RowUpdater (generic path).
func (v *spellfixVTab) DeleteRow(oldValues []interface{}) error {
	return v.deleteRow(0)
}

// DeleteRowWithRowid implements RowidConflictWriter.
func (v *spellfixVTab) DeleteRowWithRowid(oldValues []interface{}, rowid int64) error {
	return v.deleteRow(rowid)
}

// deleteRow ports the argc==1 xUpdate arm: DELETE FROM vocab WHERE id=?.
func (v *spellfixVTab) deleteRow(rowid int64) error {
	_, err := v.db.ExecSQL(fmt.Sprintf(
		"DELETE FROM %s WHERE id=%d", v.vocabName(), rowid))
	return err
}

// spellfixConflictMode maps an OR action to the shadow clause token
// (spellfix1GetConflict: FAIL behaves as ABORT).
func spellfixConflictMode(resolve string) string {
	switch strings.ToUpper(resolve) {
	case "ROLLBACK":
		return "ROLLBACK"
	case "IGNORE":
		return "IGNORE"
	case "ABORT", "FAIL":
		return "ABORT"
	case "REPLACE":
		return "REPLACE"
	}
	return ""
}

// spellfixModeClause renders the "OR x " prefix (empty when bare).
func spellfixModeClause(resolve string) string {
	mode := spellfixConflictMode(resolve)
	if mode == "" {
		return ""
	}
	return "OR " + mode + " "
}

// computeKeys derives (rank, langid, k1, k2) from the row values
// (spellfix1Update body): rank<1 becomes 1; k1 is the lowercased
// transliteration of soundslike (or word); k2 is the phonetic hash.
func (v *spellfixVTab) computeKeys(values []interface{}) (rank, langid int, k1Val, k2 interface{}, word string, err error) {
	word, _ = values[spColWord].(string)
	langid = spellfixValueInt(values[spColLangID])
	rank = spellfixValueInt(values[spColRank])
	if rank < 1 {
		rank = 1
	}
	zSoundslike, hasSoundslike := values[spColSoundslike].(string)
	var zK1 string
	if hasSoundslike && zSoundslike != "" {
		zK1 = strings.ToLower(transliterate(zSoundslike))
	} else {
		zK1 = strings.ToLower(transliterate(word))
	}
	zK2 := phoneticHash(zK1)
	// nullif(k1, word): k1 is NULL when identical to the stored word.
	k1Val = zK1
	if k1Val.(string) == word {
		k1Val = nil
	}
	k2 = zK2
	return rank, langid, k1Val, k2, word, nil
}

// insertRow ports the insert arm of spellfix1Update.
func (v *spellfixVTab) insertRow(values []interface{}, rowid int64, hasRowid bool, resolve string) (int64, error) {
	if len(values) <= spColWord || values[spColWord] == nil {
		// command column inserts (INSERT INTO t(command) VALUES('...')).
		cmd, _ := values[spColCommand].(string)
		if cmd == "" {
			return 0, fmt.Errorf("NOT NULL constraint failed: %s.word", v.name)
		}
		return 0, v.runCommand(cmd)
	}
	rank, langid, k1, k2, word, err := v.computeKeys(values)
	if err != nil {
		return 0, err
	}
	cols := fmt.Sprintf("%d,%d,%s,%s,%s", rank, langid,
		spellfixQuote(word), spellfixNullable(k1), spellfixNullable(k2))
	var sqlStr string
	if hasRowid {
		sqlStr = fmt.Sprintf("INSERT %sINTO %s(id,rank,langid,word,k1,k2) VALUES(%d,%s)",
			spellfixModeClause(resolve), v.vocabName(), rowid, cols)
	} else {
		sqlStr = fmt.Sprintf("INSERT INTO %s(rank,langid,word,k1,k2) VALUES(%s)",
			v.vocabName(), cols)
	}
	if _, err := v.db.ExecSQL(sqlStr); err != nil {
		return 0, fmt.Errorf("constraint failed")
	}
	if hasRowid {
		return rowid, nil
	}
	return v.lastInsertRowid()
}

// lastInsertRowid reads sqlite3_last_insert_rowid() (*pRowid assignment).
func (v *spellfixVTab) lastInsertRowid() (int64, error) {
	rows, err := v.db.ExecSQL("SELECT last_insert_rowid()")
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
		return 0, nil
	}
	if id, ok := rows[0][0].(int64); ok {
		return id, nil
	}
	return 0, nil
}

// updateRow ports the update arm of spellfix1Update (argv[0] and argv[1]
// both set): rewrite id/rank/langid/word/k1/k2 for the old id.
func (v *spellfixVTab) updateRow(oldValues, newValues []interface{}, oldRowid, newRowid int64, hasRowid bool, resolve string) (bool, bool, error) {
	if len(newValues) <= spColWord || newValues[spColWord] == nil {
		cmd, _ := newValues[spColCommand].(string)
		if cmd == "" {
			return false, false, fmt.Errorf("NOT NULL constraint failed: %s.word", v.name)
		}
		return false, false, v.runCommand(cmd)
	}
	rank, langid, k1, k2, word, err := v.computeKeys(newValues)
	if err != nil {
		return false, false, err
	}
	// Set id explicitly when a new rowid was supplied (re-key write).
	newID := oldRowid
	if hasRowid {
		newID = newRowid
	}
	sqlStr := fmt.Sprintf(
		"UPDATE %s%s SET id=%d, rank=%d, langid=%d, word=%s, k1=%s, k2=%s WHERE id=%d",
		spellfixModeClause(resolve), v.vocabName(), newID, rank, langid,
		spellfixQuote(word), spellfixNullable(k1), spellfixNullable(k2), oldRowid)
	if _, err := v.db.ExecSQL(sqlStr); err != nil {
		return false, false, fmt.Errorf("constraint failed")
	}
	return true, false, nil
}

// runCommand handles INSERT INTO t(command) VALUES('...') meta-commands
// (spellfix1Update's zCmd branch): "reset" and "edit_cost_table=<name>".
func (v *spellfixVTab) runCommand(cmd string) error {
	if cmd == "reset" {
		v.shared.config3 = nil
		return nil
	}
	if strings.HasPrefix(cmd, "edit_cost_table=") {
		v.shared.config3 = nil
		v.shared.costTable = spellfixDequote(strings.TrimPrefix(cmd, "edit_cost_table="))
		if v.shared.costTable == "" || strings.EqualFold(v.shared.costTable, "null") {
			v.shared.costTable = ""
		}
		return nil
	}
	return fmt.Errorf("unknown value for %s.command: \"%s\"", v.name, cmd)
}

// spellfixNullable renders a nullable SQL value literal (NULL or quoted).
func spellfixNullable(v interface{}) string {
	if v == nil {
		return "NULL"
	}
	if s, ok := v.(string); ok {
		return spellfixQuote(s)
	}
	return fmt.Sprintf("%v", v)
}
