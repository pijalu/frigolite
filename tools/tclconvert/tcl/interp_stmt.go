package tcl

import (
	"fmt"
	"strings"
)

func commandCacheFlush(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	database := ""
	if len(args) > 0 && args[0] != "db" {
		database = args[0]
	}
	i.stmts = append(i.stmts, Stmt{Type: "cacheflush", Database: database, TestName: i.curTest})
	i.vars[""] = "__CACHEFLUSH__"
	return nil
}

func commandPrepareStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	i.preparedSeq++
	handle := fmt.Sprintf("stmt_%d", i.preparedSeq)
	i.preparedSQL[handle] = args[1]
	database := args[0]
	if database == "db" {
		database = ""
	}
	i.stmts = append(i.stmts, Stmt{Type: "prepare_stmt", SQL: args[1], Variable: handle, Database: database, TestName: i.curTest})
	i.vars[""] = handle
	return nil
}

func commandStepStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	i.stmts = append(i.stmts, Stmt{Type: "stmt_step", Variable: handle, SQL: i.preparedSQL[handle], TestName: i.curTest})
	i.vars[""] = "__STEP_" + handle + "__"
	return nil
}

func commandResetStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	i.stmts = append(i.stmts, Stmt{Type: "stmt_reset", Variable: handle, SQL: i.preparedSQL[handle], TestName: i.curTest})
	i.vars[""] = "SQLITE_OK"
	return nil
}

func commandFinalizeStmt(i *Interp, _ []rawWord, args []string, _ map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	handle := strings.TrimPrefix(args[0], "$")
	i.stmts = append(i.stmts, Stmt{Type: "stmt_finalize", Variable: handle, SQL: i.preparedSQL[handle], TestName: i.curTest})
	delete(i.preparedSQL, handle)
	i.vars[""] = "SQLITE_OK"
	return nil
}
