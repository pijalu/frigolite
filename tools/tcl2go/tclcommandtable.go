// Package main implements the tcl2go tool.
//
// This file holds the TCL command dispatch table (command name -> emitter)
// and the small named handlers that back the table's branching entries.
package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// tclCmdHandler emits Go code for one TCL command. args excludes the command
// name word.
type tclCmdHandler func(tp *transpiler, args []tcl.RawWord)

// tclCommandHandlers maps TCL command names to their Go emitters. Keeping the
// dispatch in data (rather than a giant switch) keeps processCommand's
// complexity low; each handler has a single responsibility. It is built lazily
// via tclHandlers() because the handler bodies reference processCommand (via
// processCommands), which would create a package-level initialization cycle.
var (
	tclCommandHandlersOnce sync.Once
	tclCommandHandlers     map[string]tclCmdHandler
)

// tclHandlers returns the command dispatch table, building it on first use.
func tclHandlers() map[string]tclCmdHandler {
	tclCommandHandlersOnce.Do(func() {
		tclCommandHandlers = buildTclCommandHandlers()
	})
	return tclCommandHandlers
}

func buildTclCommandHandlers() map[string]tclCmdHandler {
	return map[string]tclCmdHandler{
		// SQL test commands
		"do_execsql_test":       (*transpiler).processDoExecSQLTest,
		"do_timed_execsql_test": (*transpiler).processDoExecSQLTest,
		"do_execsql2_test":      (*transpiler).processDoExecSQLTest,
		"do_catchsql_test":      (*transpiler).processDoCatchSQLTest,
		"do_test":               (*transpiler).processDoTest,
		"do_eqp_test":           (*transpiler).processDoEQPTest,
		"do_changes_test":       (*transpiler).processDoChangesTest,
		"do_tc_test":            (*transpiler).processDoTCtest,
		"do_preupdate_test":     (*transpiler).processDoPreupdateTest,

		// do_select_tests and its wrapper procs (do_createtable_tests,
		// do_delete_tests, do_insert_tests, do_update_tests, do_reindex_tests).
		// Each wrapper prefixes the test name with its file name, matching the
		// TCL `uplevel do_select_tests [list PREFIX-$name] $args` call.
		"do_select_tests":      func(tp *transpiler, args []tcl.RawWord) { tp.processDoSelectTests("", args) },
		"do_createtable_tests": func(tp *transpiler, args []tcl.RawWord) { tp.processDoSelectTests("e_createtable-", args) },
		"do_delete_tests":      func(tp *transpiler, args []tcl.RawWord) { tp.processDoSelectTests("e_delete-", args) },
		"do_insert_tests":      func(tp *transpiler, args []tcl.RawWord) { tp.processDoSelectTests("e_insert-", args) },
		"do_update_tests":      func(tp *transpiler, args []tcl.RawWord) { tp.processDoSelectTests("e_update-", args) },
		"do_reindex_tests":     func(tp *transpiler, args []tcl.RawWord) { tp.processDoSelectTests("e_reindex-", args) },

		// SQL execution
		"execsql":        func(tp *transpiler, args []tcl.RawWord) { tp.processExecSQL(args, "exec") },
		"execsql_intout": func(tp *transpiler, args []tcl.RawWord) { tp.processExecSQL(args, "exec") },
		"execsql2":       func(tp *transpiler, args []tcl.RawWord) { tp.processExecSQL(args, "exec") },
		// execsqlS (fkey2.test / without_rowid3.test): plain execsql whose
		// result is the search/found count CONCATENATED with the rows — the
		// count half is a C-internal VDBE statistic the engine does not
		// expose, so the SQL runs for its side effects and the comparison is
		// dropped (the count expectation is unassertable).
		"execsqlS": func(tp *transpiler, args []tcl.RawWord) { tp.processExecSQL(args, "exec") },
		"stepsql":  (*transpiler).processStepsql,
		"sql":      (*transpiler).processSQLVar,
		"catchsql": func(tp *transpiler, args []tcl.RawWord) { tp.processExecSQL(args, "catch") },
		"db":       (*transpiler).processDB,
		"count":    (*transpiler).processCount,
		"cksort":   (*transpiler).processCksort,
		"integrity_check": func(tp *transpiler, args []tcl.RawWord) {
			tp.emitLine("_res = db.Exec(\"PRAGMA integrity_check\")")
			tp.emitLine("if _res.Error != nil { t.Errorf(\"integrity check: %%v\", _res.Error) }")
		},
		"capture_pragma": (*transpiler).processCapturePragma,
		// sqlite3_create_aggregate $DB — the TCL harness fixture registering
		// the x_count test aggregate (src/test1.c t1CountStep/
		// t1CountFinalize: counts non-null first args; a step input of 40 or
		// 41 errors; a final count of 42 errors). aggerror.test / func.test
		// / misuse.test use it.
		"sqlite3_create_aggregate": (*transpiler).processSqlite3CreateAggregate,

		// recover.test: .recover harness procs (in-process RecoverSQL port;
		// ext/misc/recover.c sqlite3recover semantics, no CLI subprocess).
		"recover_with_opts": (*transpiler).processRecoverWithOpts,
		"do_recover_test":   (*transpiler).processDoRecoverTest,
		"compare_result":    (*transpiler).processCompareResult,
		"compare_dbs":       (*transpiler).processCompareDBs,
		"test_find_cli":     (*transpiler).processTestFindCli,

		// Control flow
		"foreach": (*transpiler).processForeach,
		// fts5_common.tcl meta-loop: run the body once per detail mode after
		// reset_db (fts5simple3 2.x/4.x; untranspiled, its per-mode reset_db
		// is lost and later CREATEs collide with pre-loop tables).
		"foreach_detail_mode": (*transpiler).processForEachDetailMode,
		"for":                 (*transpiler).processForCommand,
		"while":               (*transpiler).processWhile,
		"if":                  (*transpiler).processIf,
		"set":                 (*transpiler).processSet,
		"incr":                (*transpiler).processIncr,
		"expr":                (*transpiler).processExpr,
		"catch":               (*transpiler).processCatch,
		"return":              func(tp *transpiler, args []tcl.RawWord) { tp.processReturn(args) },
		"break":               func(tp *transpiler, args []tcl.RawWord) { tp.emitLine("break") },
		"continue":            func(tp *transpiler, args []tcl.RawWord) { tp.emitContinue() },
		"time":                (*transpiler).processTime,
		"eval":                (*transpiler).processScriptEval,
		"subst":               (*transpiler).processSubst,
		"proc":                (*transpiler).processProc,
		"unset":               (*transpiler).processUnset,

		// autovacuum.test / incrvacuum*.test file_pages proc — returns
		// the page count of test.db (1024-byte page size; the engine's
		// pager reports NumPages in pages, so divide file size by the
		// page size). The generated code assigns the result to _r so
		// do_test compares the value to the expected page count.
		"file_pages": func(tp *transpiler, args []tcl.RawWord) {
			_rExpr := `strconv.Itoa(tclFilePages("test.db"))`
			tp.emitLine("_r = %s // file_pages result", _rExpr)
		},

		// drop_all_indexes (tester.tcl proc, {{db db}} default): drop every
		// explicitly created index so a loop body's CREATE INDEX re-runs from
		// the same schema (rowvalue3/rowvalue4 index-permutation loops).
		"drop_all_indexes": (*transpiler).processDropAllIndexes,

		// String / list operations
		"append":   (*transpiler).processStringAppend,
		"lappend":  (*transpiler).processListAppend,
		"list":     (*transpiler).processList,
		"close":    (*transpiler).processClose,
		"string":   (*transpiler).processStringCmd,
		"concat":   (*transpiler).processConcat,
		"lindex":   func(tp *transpiler, args []tcl.RawWord) { tp.processListOp("lindex", args) },
		"lrange":   func(tp *transpiler, args []tcl.RawWord) { tp.processListOp("lrange", args) },
		"llength":  func(tp *transpiler, args []tcl.RawWord) { tp.processListOp("llength", args) },
		"lsort":    func(tp *transpiler, args []tcl.RawWord) { tp.processListOp("lsort", args) },
		"lreplace": func(tp *transpiler, args []tcl.RawWord) { tp.processListOp("lreplace", args) },
		"lsearch":  func(tp *transpiler, args []tcl.RawWord) { tp.processListOp("lsearch", args) },
		"regexp":   (*transpiler).processRegexp,
		"regsub":   (*transpiler).processRegsub,
		"error":    (*transpiler).processError,
		"glob":     (*transpiler).processGlob,
		"split":    (*transpiler).processSplit,
		"join":     (*transpiler).processJoin,

		// sqlite3 C API
		"sqlite3":              (*transpiler).processSqlite3,
		"sqlite3_exec":         (*transpiler).processSqlite3Exec,
		"sqlite3_test_control": (*transpiler).processSqlite3TestControl,
		// `sqlite3_test_control_pending_byte 0x0010000` — the C-defined
		// TCL command in src/test2.c::testPendingByte. Updates the global
		// pending byte (tester.tcl:102 calls it on harness init). The
		// transpiler emits an assignment to the Go shadow variable.
		"sqlite3_test_control_pending_byte": (*transpiler).processSqlite3TestControlPendingByte,
		"sqlite3_limit":                     (*transpiler).processSqlite3Limit,
		"sqlite3_db_config":                 (*transpiler).processDBConfig,
		"optimization_control":              (*transpiler).processOptimizationControl,
		"dbconfig_maindbname_icecube":       (*transpiler).processDBConfigMainDBNameIcecube,
		"sqlite3_create_collation_v2":       (*transpiler).processCreateCollation,
		"sqlite_delete_collation":           (*transpiler).processDeleteCollation,
		"sqlite3_backup":                    (*transpiler).processSqlite3Backup,
		"sqlite3_errmsg":                    (*transpiler).processSqlite3Errmsg,
		"sqlite3_errcode":                   (*transpiler).processSqlite3Errcode,
		"sqlite3_close":                     (*transpiler).processSqlite3Close,
		"sqlite3_interrupt":                 (*transpiler).processSqlite3Interrupt,
		"sqlite3_is_interrupted":            (*transpiler).processSqlite3IsInterrupted,
		"sqlite3_stmt_status":               (*transpiler).processSqlite3StmtStatus,
		"sqlite3_autovacuum_pages":          (*transpiler).processSqlite3AutovacuumPages,
		"dbcksum":                           (*transpiler).processDBCksum,
		"file_control_data_version":         (*transpiler).processFileControlDataVersion,
		"sqlite3_prepare":                   processPrepareCmd("sqlite3_prepare"),
		"sqlite3_prepare_v2":                processPrepareCmd("sqlite3_prepare_v2"),
		"sqlite3_bind_double":               func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_double", args) },
		"sqlite3_bind_int":                  func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_int", args) },
		"sqlite3_bind_int64":                func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_int64", args) },
		"sqlite3_bind_text":                 func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_text", args) },
		"sqlite3_bind_text16":               func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_text16", args) },
		"sqlite3_bind_null":                 func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_null", args) },
		"sqlite3_bind_blob":                 func(tp *transpiler, args []tcl.RawWord) { tp.processBind("sqlite3_bind_blob", args) },
		"sqlite_bind":                       (*transpiler).processLegacyBind,
		"sqlite3_transfer_bindings":         (*transpiler).processTransferBindings,
		"sqlite3_step":                      (*transpiler).processStep,
		// sqlite_step is bind.test's TCL wrapper proc over sqlite3_step
		// (stmt N VALS COLS): the rc result matters; the upvar'd lists are
		// not asserted by the suite.
		"sqlite_step": (*transpiler).processSqliteStepTCL,

		// intarray test-only C-API (src/test_intarray.c), emulated so the
		// intarray virtual table can be created and populated by the harness.
		"sqlite3_intarray_create": (*transpiler).processIntarrayCreate,
		"sqlite3_intarray_bind":   (*transpiler).processIntarrayBind,
		"sqlite3_reset":           (*transpiler).processReset,
		"sqlite3_finalize":        (*transpiler).processFinalize,
		"sqlite3_clear_bindings":  (*transpiler).processClearBindings,
		"sqlite3_create_function": (*transpiler).processCreateFunction,

		// quota VFS (src/test_quota.c). Each command returns its result via
		// the runtime helper of the same name (defined in helpersTemplatePart2).
		"sqlite3_quota_initialize":     (*transpiler).processSqlite3QuotaInitialize,
		"sqlite3_quota_shutdown":       (*transpiler).processSqlite3QuotaShutdown,
		"sqlite3_quota_set":            (*transpiler).processSqlite3QuotaSet,
		"sqlite3_quota_remove":         (*transpiler).processSqlite3QuotaRemove,
		"sqlite3_quota_file":           (*transpiler).processSqlite3QuotaFile,
		"sqlite3_quota_dump":           (*transpiler).processSqlite3QuotaDump,
		"sqlite3_quota_glob":           (*transpiler).processSqlite3QuotaGlob,
		"sqlite3_quota_dir":            (*transpiler).processSqlite3QuotaDir,
		"sqlite3_quota_fopen":          (*transpiler).processSqlite3QuotaFopen,
		"sqlite3_quota_fclose":         (*transpiler).processSqlite3QuotaFclose,
		"sqlite3_quota_fread":          (*transpiler).processSqlite3QuotaFread,
		"sqlite3_quota_fwrite":         (*transpiler).processSqlite3QuotaFwrite,
		"sqlite3_quota_fflush":         (*transpiler).processSqlite3QuotaFflush,
		"sqlite3_quota_fseek":          (*transpiler).processSqlite3QuotaFseek,
		"sqlite3_quota_rewind":         (*transpiler).processSqlite3QuotaRewind,
		"sqlite3_quota_ftell":          (*transpiler).processSqlite3QuotaFTell,
		"sqlite3_quota_ftruncate":      (*transpiler).processSqlite3QuotaFtruncate,
		"sqlite3_quota_file_available": (*transpiler).processSqlite3QuotaFileAvailable,
		"sqlite3_quota_file_size":      (*transpiler).processSqlite3QuotaFileSize,
		"sqlite3_quota_file_truesize":  (*transpiler).processSqlite3QuotaFileTrueSize,
		"sqlite3_quota_ferror":         (*transpiler).processSqlite3QuotaFerror,
		"file_control_vfsname":         (*transpiler).processFileControlVfsName,
		"file_control_reservebytes":    (*transpiler).processFileControlReserveBytes,

		// Prepared-statement metadata queries (value-producing statements).
		// Only active for files using the runtime Stmt VM emulation; other
		// files keep their historical unsupported-command comments.
		"sqlite3_bind_parameter_count": makeStmtMetadataHandler("sqlite3_bind_parameter_count"),
		"sqlite3_bind_parameter_name":  makeStmtMetadataHandler("sqlite3_bind_parameter_name"),
		"sqlite3_bind_parameter_index": makeStmtMetadataHandler("sqlite3_bind_parameter_index"),
		"sqlite3_column_count":         makeStmtMetadataHandler("sqlite3_column_count"),
		"sqlite3_data_count":           makeStmtMetadataHandler("sqlite3_data_count"),
		"sqlite3_column_name":          makeStmtMetadataHandler("sqlite3_column_name"),
		"sqlite3_column_text":          makeStmtMetadataHandler("sqlite3_column_text"),
		"sqlite3_column_int":           makeStmtMetadataHandler("sqlite3_column_int"),
		"sqlite3_column_double":        makeStmtMetadataHandler("sqlite3_column_double"),
		// fts3sort.test's build_database proc: FTS4 table + deterministic docs.
		"build_database": (*transpiler).processBuildDatabase,

		// Multi-process locking emulation (lock2/lock4/...): a testfixture is
		// a persistent second connection on the same file. The transpiler
		// emulates it as a persistent in-process connection keyed in
		// tclFixtureDBs (see processfixture.go).
		"testfixture":        (*transpiler).processTestfixture,
		"launch_testfixture": (*transpiler).processLaunchTestfixture,

		// Incremental blob I/O (sqlite3_blob_*)
		"sqlite3_blob_open":     (*transpiler).processSqlite3BlobOpen,
		"sqlite3_blob_bytes":    (*transpiler).processSqlite3BlobBytes,
		"sqlite3_blob_read":     (*transpiler).processSqlite3BlobRead,
		"sqlite3_blob_write":    (*transpiler).processSqlite3BlobWrite,
		"sqlite3_blob_close":    (*transpiler).processSqlite3BlobClose,
		"sqlite3_blob_reopen":   (*transpiler).processSqlite3BlobReopen,
		"blob_write_test":       (*transpiler).processBlobWriteTest,
		"blob_write_error_test": (*transpiler).processBlobWriteErrorTest,
		"create_t1":             (*transpiler).processCreateT1,
		"populate_t1":           (*transpiler).processPopulateT1,

		// e_fts3.test wrapper procs (ddl_test/write_test/read_test/error_test)
		// thin aliases over do_write_test/do_read_test/do_error_test. The
		// procs are defined locally in the TCL source with bodies
		// `uplevel [list do_write_test e_fts3-$tn sqlite_master $ddl]` etc.,
		// so inline the wrapped operation here (no OOM mode).
		"ddl_test":   (*transpiler).processFTSDDLTest,
		"write_test": (*transpiler).processFTSWriteTest,
		"read_test":  (*transpiler).processFTSReadTest,
		"error_test": (*transpiler).processFTSErrorTest,

		// Files and db lifecycle
		"forcedelete":           (*transpiler).processFileDelete,
		"delete_file":           (*transpiler).processDeleteFile,
		"forcecopy":             (*transpiler).processFileCopy,
		"copy_file":             (*transpiler).processFileCopy,
		"file":                  (*transpiler).processFileCmd,
		"reset_db":              func(tp *transpiler, args []tcl.RawWord) { tp.processResetDB() },
		"db_save":               func(tp *transpiler, args []tcl.RawWord) { tp.processDBSave() },
		"db_save_and_close":     func(tp *transpiler, args []tcl.RawWord) { tp.processDBSaveAndClose() },
		"db_restore_and_reopen": func(tp *transpiler, args []tcl.RawWord) { tp.processDBRestoreAndReopen() },
		"db_restore":            func(tp *transpiler, args []tcl.RawWord) { tp.processDBRestore() },
		"db_delete_and_reopen":  func(tp *transpiler, args []tcl.RawWord) { tp.processDBDeleteAndReopen() },
		// faultsim harness aliases (ext/*.test fault-injection framework):
		// reset/save/restore operate on the same test.db* files.
		"faultsim_save_and_close":     func(tp *transpiler, args []tcl.RawWord) { tp.processDBSaveAndClose() },
		"faultsim_restore_and_reopen": func(tp *transpiler, args []tcl.RawWord) { tp.processDBRestoreAndReopen() },
		"faultsim_delete_and_reopen":  func(tp *transpiler, args []tcl.RawWord) { tp.processDBDeleteAndReopen() },
		"puts":                        (*transpiler).processPuts,

		// FTS test data loader: fills table t1(docid, words) with the text of
		// the Book of Genesis (source $testdir/genesis.tcl defines the
		// fts_kjv_genesis proc; the transpiler inlines its INSERTs).
		"fts_kjv_genesis": (*transpiler).processFTSKJVGenesis,

		// FTS test data loaders (source $testdir/fts3_common.tcl): build the
		// sample FTS tables t1/t2 with synthetic text. The transpiler emits
		// package-level helpers (fts3BuildDB1/fts3BuildDB2).
		"fts3_build_db_1":         (*transpiler).processFTS3BuildDB1,
		"fts3_build_db_2":         (*transpiler).processFTS3BuildDB2,
		"build_multilingual_db_1": (*transpiler).processBuildMultilingualDB1,
		"build_multilingual_db_2": (*transpiler).processBuildMultilingualDB2,
		"build_multilingual_db_3": (*transpiler).processBuildMultilingualDB3,

		// Capability guards
		"ifcapable":    (*transpiler).processIfcapable,
		"ifnotcapable": (*transpiler).processIfnotcapable,

		// Test infrastructure (no-op or comment emitters)
		"source": noopTclCommand, "finish_test": noopTclCommand, "test_finish": noopTclCommand,
		"exit": noopTclCommand, "flush": noopTclCommand, "fix_testname": noopTclCommand,
		"incr_ntest": noopTclCommand, "sqlite3_memdebug_settitle": noopTclCommand,
		"namespace": (*transpiler).processNamespace, "rename": noopTclCommand, "array": (*transpiler).processArray,
		"foreach_kv": noopTclCommand, "foreach_u": noopTclCommand, "global": noopTclCommand,
		"uplevel": noopTclCommand, "upvar": noopTclCommand, "info": (*transpiler).processInfoCommand,
		"vwait": noopTclCommand, "after": noopTclCommand, "update": noopTclCommand,
		"breakpoint":        noopTclCommand,
		"queryplan":         (*transpiler).processQueryPlan,
		"sqlite3_exec_hex":  (*transpiler).processExecHex,
		"optimization":      func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("optimization", args) },
		"uses":              func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("uses", args) },
		"xferopt":           func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("xferopt", args) },
		"xfer":              func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("xfer", args) },
		"switch":            func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("switch", args) },
		"do_sp_test":        func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("do_sp_test", args) },
		"do_select_test":    func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("do_select_test", args) },
		"record":            func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("record", args) },
		"tcl_platform":      func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("tcl_platform", args) },
		"binary":            (*transpiler).processBinaryCommand,
		"read":              (*transpiler).processRead,
		"seek":              (*transpiler).processSeek,
		"open":              (*transpiler).processOpen,
		"fconfigure":        (*transpiler).processFConfigure,
		"hexio_write":       (*transpiler).processHexioWrite,
		"hexio_read":        (*transpiler).processHexioRead,
		"hexio_get_int":     (*transpiler).processHexioGetInt,
		"chan":              (*transpiler).processChanSubcommand,
		"sqlite3_normalize": func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("sqlite3_normalize", args) },
		"verify_db":         func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("verify_db", args) },
		"do_aggregate_test": func(tp *transpiler, args []tcl.RawWord) { tp.processInfraComment("do_aggregate_test", args) },
		"test_expr":         func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("test_expr", args) },
		"test_expr2":        func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("test_expr2", args) },
		"test_realnum_expr": func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("test_realnum_expr", args) },
		"test_boolean_expr": func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("test_boolean_expr", args) },
		"do_realnum_test":   func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("do_realnum_test", args) },
		"do_like_test":      func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("do_like_test", args) },
		"do_test_withfunc":  func(tp *transpiler, args []tcl.RawWord) { tp.processExprTest("do_test_withfunc", args) },
		"drop_all_tables":   func(tp *transpiler, args []tcl.RawWord) { tp.processDropAllTables() },
	}
}

// noopTclCommand emits nothing for TCL infrastructure commands that have no Go
// equivalent (source, finish_test, namespace, etc.).
func noopTclCommand(tp *transpiler, args []tcl.RawWord) {}

// processPrepareCmd builds the sqlite3_prepare[_v2] handler: inside a catch
// block with a full argument list the prepare is emulated; standalone
// prepares stay unsupported.
func processPrepareCmd(cmdName string) tclCmdHandler {
	return func(tp *transpiler, args []tcl.RawWord) {
		if tp.catchMode && len(args) >= 4 {
			tp.emitPrepareInCatch(args)
			return
		}
		tp.emitLine("// %s (standalone prepare; not emulated)", cmdName)
	}
}

// makeStmtMetadataHandler builds the handler for a prepared-statement
// metadata query command: the Stmt-VM result emitter when enabled, the
// unsupported-command comment otherwise.
func makeStmtMetadataHandler(cmdName string) tclCmdHandler {
	return func(tp *transpiler, args []tcl.RawWord) {
		if !stmtVMEnabled() {
			tp.emitUnsupportedStmtCmd(cmdName, args)
			return
		}
		tp.emitLine("%s", stmtMetadataResultExpr(cmdName, tp, args))
	}
}

// stmtMetadataResultExpr renders one prepared-statement metadata command's
// result assignment line.
func stmtMetadataResultExpr(cmdName string, tp *transpiler, args []tcl.RawWord) string {
	switch cmdName {
	case "sqlite3_bind_parameter_count":
		return fmt.Sprintf("_r = strconv.Itoa(tclParamCountOf(%q))", stmtVarArg(args))
	case "sqlite3_bind_parameter_name":
		return fmt.Sprintf("_r = tclParamNameOf(%q, %s)", stmtVarArg(args), tp.intArgExpr(argAt(args, 1).Text))
	case "sqlite3_bind_parameter_index":
		return fmt.Sprintf("_r = strconv.Itoa(tclParamIndexOf(%q, %s))", stmtVarArg(args), tp.buildStringExpr(argAt(args, 1).Text))
	case "sqlite3_column_count":
		return fmt.Sprintf("_r = strconv.Itoa(tclColumnCount(%q))", stmtVarArg(args))
	case "sqlite3_data_count":
		return fmt.Sprintf("_r = strconv.Itoa(tclDataCount(%q))", stmtVarArg(args))
	case "sqlite3_column_name":
		return fmt.Sprintf("_r = tclColumnNameOf(%q, %s)", stmtVarArg(args), tp.intArgExpr(argAt(args, 1).Text))
	case "sqlite3_column_text", "sqlite3_column_int":
		return fmt.Sprintf("_r = tclColumnTextOf(%q, %s)", stmtVarArg(args), tp.intArgExpr(argAt(args, 1).Text))
	case "sqlite3_column_double":
		return fmt.Sprintf("_r = tclColumnDoubleOf(%q, %s)", stmtVarArg(args), tp.intArgExpr(argAt(args, 1).Text))
	}
	return ""
}

// processDropAllIndexes emits the drop_all_indexes call (tester.tcl proc,
// {{db db}} default).
func (tp *transpiler) processDropAllIndexes(args []tcl.RawWord) {
	dbName := "db"
	if len(args) >= 1 {
		dbName = args[0].Text
	}
	tp.emitLine("tclDropAllIndexes(%s)", tp.dbArgGo(dbName))
}

// processBuildDatabase emits fts3sort.test's build_database proc: an FTS4
// table filled with deterministic documents.
func (tp *transpiler) processBuildDatabase(args []tcl.RawWord) {
	nRowExpr := "1000"
	paramExpr := `""`
	if len(args) > 0 {
		nRowExpr = tp.intArgExpr(argAt(args, 0).Text)
	}
	if len(args) > 1 {
		paramExpr = tp.buildStringExpr(argAt(args, 1).Text)
	}
	tp.emitLine("fts3SortBuildDatabase(db, %s, %s)", nRowExpr, paramExpr)
}

// connNameFromPointerArg extracts the connection name from a
// register_echo_module argument: either a bare connection name ("db") or a
// sqlite3_connection_pointer command substitution ("[sqlite3_connection_pointer
// db2]"). Returns "" when no name can be extracted.
func connNameFromPointerArg(arg string) string {
	arg = strings.TrimSpace(arg)
	arg = strings.TrimPrefix(arg, "[")
	arg = strings.TrimSuffix(arg, "]")
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if last == "" || strings.ContainsAny(last, "$[]") {
		return ""
	}
	return last
}
