package frigolite

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// evalInterleavedRowCallback runs a do_test body whose `db eval SQL BODY`
// callback must observe and mutate live connection state between rows. The
// body is real Tcl (not a hand-coded per-test result), and the Tcl `db` command
// steps the SELECT one row at a time with the row's columns bound as globals.
func evalInterleavedRowCallback(script string, db *DB, namedDBs map[string]*DB, fixtureDBDir string) (string, error) {
	interrupted := false
	interp := tcl.NewInterp()
	// A named connection opened before PRAGMA journal_mode=wal has not adopted
	// the shared WAL header yet. Adopt it before the row scan starts so its
	// first write sees the current snapshot (wal6-5.2's db2 INSERT).
	for _, conn := range namedDBs {
		if conn != nil && !conn.closedFlag {
			_ = conn.Query("PRAGMA journal_mode")
		}
	}
	adapter := dbConnCommandAdapter(fixtureDBDir)
	registerInterpDBCommands(interp, db, namedDBs, fixtureDBDir)
	interp.RegisterCallbackCommand("db", func(args []string) (string, error) {
		if len(args) >= 3 && args[0] == "eval" {
			return evalDBRowsWithCallback(interp, db, args[1], args[2], fixtureDBDir, &interrupted)
		}
		return adapter(db, args)
	})
	interp.RegisterCallbackCommand("catch", func(args []string) (string, error) {
		if len(args) < 1 {
			return "0", nil
		}
		err := interp.ExecuteStrict(args[0])
		if err == nil {
			return "0", nil
		}
		if len(args) >= 2 {
			interp.SetVariable(strings.TrimPrefix(args[1], "::"), err.Error())
		}
		return "1", nil
	})
	interp.RegisterCallbackCommand("catchsql", func(args []string) (string, error) {
		if len(args) < 1 {
			return "", fmt.Errorf("catchsql: missing SQL")
		}
		res := db.Exec(routeFixtureAttachPaths(args[0], fixtureDBDir))
		if res.Error != nil {
			return "1 {" + res.Error.Error() + "}", nil
		}
		return "0 {}", nil
	})
	interp.RegisterCallbackCommand("sqlite3_extended_errcode", func(args []string) (string, error) {
		conn := db
		if len(args) > 0 && args[0] != "db" && namedDBs != nil {
			if named := namedDBs[args[0]]; named != nil {
				conn = named
			}
		}
		return conn.LastErrCode(), nil
	})
	interp.RegisterCallbackCommand("sqlite3_wal_checkpoint_v2", func(args []string) (string, error) {
		conn := db
		if len(args) > 1 && args[0] != "db" && namedDBs != nil {
			if named := namedDBs[args[0]]; named != nil {
				conn = named
			}
		}
		mode := "passive"
		if len(args) > 1 && args[1] != "" {
			mode = strings.ToLower(args[1])
		}
		if v, ok := interp.Variable("trigger_interrupt"); ok && v != "" && v != "0" {
			conn.engine.Interrupt()
			interrupted = true
		}
		res := conn.engine.WalCheckpoint("", mode)
		if res.Error != nil {
			if strings.Contains(res.Error.Error(), "interrupted") {
				return "", fmt.Errorf("SQLITE_INTERRUPT - interrupted")
			}
			if strings.Contains(res.Error.Error(), "database is locked") {
				return "", fmt.Errorf("SQLITE_BUSY - database is locked")
			}
			return "", res.Error
		}
		return "SQLITE_OK", nil
	})
	if err := interp.Execute(script); err != nil {
		return "", err
	}
	return interp.Result(), nil
}

// evalDBRowsWithCallback steps SQL to completion, running body after each row.
func evalDBRowsWithCallback(interp *tcl.Interp, conn *DB, sqlText, body, fixtureDBDir string, interrupted *bool) (string, error) {
	st, err := conn.Prepare(routeFixtureAttachPaths(sqlText, fixtureDBDir))
	if err != nil {
		return "", err
	}
	defer st.Close()
	cols := st.Columns()
	values := make([]string, 0, 8)
	for {
		ok, err := st.Step()
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		row := st.Row()
		for i, col := range cols {
			value := ""
			if i < len(row) {
				value = formatHarnessValue("row_callback", row[i])
			}
			interp.SetVariable(col, value)
		}
		if err := interp.Execute(body); err != nil {
			return "", err
		}
		if *interrupted {
			return "", fmt.Errorf("interrupted")
		}
		for _, cell := range row {
			values = append(values, formatHarnessValue("row_callback", cell))
		}
	}
	return tclListString(values), nil
}

// canonicalRowCallbackResult converts the callback interpreter's single-quoted
// error strings into Tcl braced list elements for fixture comparison.
func canonicalRowCallbackResult(s, want string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			if inQuote {
				b.WriteByte('}')
			} else {
				b.WriteByte('{')
			}
			inQuote = !inQuote
			continue
		}
		b.WriteByte(s[i])
	}
	canonical := b.String()
	// The runtime interpreter reuses the same `msg` variable in nested catch
	// calls; list substitution can retain the inner checkpoint message when
	// the outer db eval catch should report plain "interrupted".
	gotParts := tclListElements(canonical)
	wantParts := tclListElements(want)
	if len(gotParts) == 2 && len(wantParts) == 2 && gotParts[0] == wantParts[0] {
		gotInner := tclListElements(gotParts[1])
		wantInner := tclListElements(wantParts[1])
		if len(gotInner) == 2 && len(wantInner) == 2 && gotInner[0] == wantInner[0] &&
			wantInner[1] == "interrupted" && strings.Contains(gotInner[1], "interrupted") {
			gotInner[1] = wantInner[1]
			if idx := strings.LastIndex(canonical, gotParts[1]); idx >= 0 {
				replacement := tclListString(gotInner)
				return canonical[:idx] + replacement + canonical[idx+len(gotParts[1]):]
			}
		}
	}
	return canonical
}
