// SPDX-License-Identifier: GPL-3.0-or-later
//
// Quota VFS transpiler handlers. The SQLite quota VFS shim (src/test_quota.c)
// is a per-file-size cap that lives between the pager and the real VFS. It
// rejects writes that would push any file in a "quota group" (defined by a
// glob pattern) past the group's combined size limit, returning
// SQLITE_FULL (mapped to "database or disk is full" at the SQL layer).
//
// Frigolite has no VFS plug-in system, so the quota layer is emulated at
// the engine surface (a pure-Go state table keyed by glob pattern +
// file path). The transpiler maps each test_quota.c TCL command to a Go
// helper in the helpers template (helpersTemplatePart2):
//
//   sqlite3_quota_initialize VFS MAKEDEFAULT  → tclQuotaInitialize(VFS, MAKEDEFAULT)
//   sqlite3_quota_shutdown                     → tclQuotaShutdown()
//   sqlite3_quota_set PATTERN LIMIT SCRIPT     → tclQuotaSet(PATTERN, LIMIT, SCRIPT)
//   sqlite3_quota_remove FILENAME              → tclQuotaRemove(FILENAME)
//   sqlite3_quota_file FILENAME                → tclQuotaFile(FILENAME)
//   sqlite3_quota_dump                         → tclQuotaDump()
//   sqlite3_quota_glob PATTERN TEXT            → tclQuotaGlob(PATTERN, TEXT)
//   sqlite3_quota_dir PATTERN DIRECTORY        → tclQuotaDir(PATTERN, DIRECTORY)
//   sqlite3_quota_file_available HANDLE        → tclQuotaFileAvailable(HANDLE)
//   sqlite3_quota_file_size HANDLE             → tclQuotaFileSize(HANDLE)
//   sqlite3_quota_ferror HANDLE                → tclQuotaFerror(HANDLE)
//   sqlite3_quota_fopen FILENAME MODE          → tclQuotaFopen(FILENAME, MODE)
//   sqlite3_quota_fclose HANDLE                → tclQuotaFclose(HANDLE)
//   sqlite3_quota_fread HANDLE SIZE NELEM      → tclQuotaFread(HANDLE, SIZE, NELEM)
//   sqlite3_quota_fwrite HANDLE SIZE NELEM TXT → tclQuotaFwrite(HANDLE, SIZE, NELEM, TXT)
//   sqlite3_quota_fflush HANDLE ?HARDSYNC?     → tclQuotaFflush(HANDLE, HARDSYNC)
//   sqlite3_quota_fseek HANDLE OFFSET WHENCE   → tclQuotaFseek(HANDLE, OFFSET, WHENCE)
//   sqlite3_quota_rewind HANDLE                → tclQuotaRewind(HANDLE)
//   sqlite3_quota_ftell HANDLE                 → tclQuotaFTell(HANDLE)
//   sqlite3_quota_ftruncate HANDLE SIZE        → tclQuotaFtruncate(HANDLE, SIZE)
//
// Each handler emits a single line; the helpers are designed to mirror
// SQLite's quota semantics faithfully (src/test_quota.c + src/test_quota.h).
package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// quotaArg renders a RawWord as a Go string literal (with $var → Go ident)
// so the emitted helper call mirrors TCL's command argument substitution.
func quotaArg(tp *transpiler, w tcl.RawWord) string {
	return tp.goStringLiteral(w)
}

// quotaAtoi parses a decimal int string and returns int64. Mirrors
// helpersTemplatePart2::tclAtoi (the helper exists in the runtime package
// but the transpiler also needs an internal copy for quota limit folding).
func quotaAtoi(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// processSqlite3QuotaInitialize handles `sqlite3_quota_initialize VFS MAKEDEFAULT`.
// Returns SQLITE_OK (0). Mirrors src/test_quota.c::test_quota_initialize.
func (tp *transpiler) processSqlite3QuotaInitialize(args []tcl.RawWord) {
	vfs := ""
	makeDefault := "0"
	if len(args) >= 1 {
		vfs = quotaArg(tp, args[0])
	}
	if len(args) >= 2 {
		makeDefault = strconv.FormatInt(quotaAtoi(args[1].Text), 10)
	}
	tp.emitLine("_r = tclQuotaInitialize(%s, %s)", vfs, makeDefault)
}

// processSqlite3QuotaShutdown handles `sqlite3_quota_shutdown`. Returns SQLITE_OK
// when no connections are open, SQLITE_MISUSE otherwise. Mirrors
// src/test_quota.c::test_quota_shutdown.
func (tp *transpiler) processSqlite3QuotaShutdown(args []tcl.RawWord) {
	tp.emitLine("_r = tclQuotaShutdown()")
}

// processSqlite3QuotaSet handles `sqlite3_quota_set PATTERN LIMIT SCRIPT`.
// SCRIPT is a TCL proc name (or empty for the default no-op). Mirrors
// src/test_quota.c::test_quota_set.
func (tp *transpiler) processSqlite3QuotaSet(args []tcl.RawWord) {
	pattern := ""
	limit := "0"
	script := ""
	if len(args) >= 1 {
		pattern = quotaArg(tp, args[0])
	}
	if len(args) >= 2 {
		limit = strconv.FormatInt(quotaAtoi(args[1].Text), 10)
	}
	if len(args) >= 3 {
		script = quotaArg(tp, args[2])
	}
	tp.emitLine("_r = tclQuotaSet(%s, %s, %s)", pattern, limit, script)
}

// processSqlite3QuotaRemove handles `sqlite3_quota_remove FILENAME`.
// Returns SQLITE_OK. Mirrors src/test_quota.c::test_quota_remove.
func (tp *transpiler) processSqlite3QuotaRemove(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_remove", args)
		return
	}
	tp.emitLine("_r = tclQuotaRemove(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaFile handles `sqlite3_quota_file FILENAME`. Returns the
// current size of the quota group's file as a TCL integer string.
// Mirrors src/test_quota.c::test_quota_file.
func (tp *transpiler) processSqlite3QuotaFile(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_file", args)
		return
	}
	tp.emitLine("_r = tclQuotaFile(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaDump handles `sqlite3_quota_dump`. Returns a TCL list
// of "{pattern limit size}" triples. Mirrors src/test_quota.c::test_quota_dump.
func (tp *transpiler) processSqlite3QuotaDump(args []tcl.RawWord) {
	tp.emitLine("_r = tclQuotaDump()")
}

// processSqlite3QuotaGlob handles `sqlite3_quota_glob PATTERN TEXT`. Returns
// "1" when TEXT matches PATTERN, "0" otherwise. Mirrors
// src/test_quota.c::test_quota_glob (which delegates to quotaStrglob).
func (tp *transpiler) processSqlite3QuotaGlob(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_glob", args)
		return
	}
	tp.emitLine("_r = tclQuotaGlob(%s, %s)", quotaArg(tp, args[0]), quotaArg(tp, args[1]))
}

// processSqlite3QuotaDir handles `sqlite3_quota_dir PATTERN DIRECTORY`. Returns
// SQLITE_OK when the pattern is added to the directory scan, SQLITE_ERROR
// otherwise. Mirrors src/test_quota.c::test_quota_dir.
func (tp *transpiler) processSqlite3QuotaDir(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_dir", args)
		return
	}
	tp.emitLine("_r = tclQuotaDir(%s, %s)", quotaArg(tp, args[0]), quotaArg(tp, args[1]))
}

// processSqlite3QuotaFopen handles `sqlite3_quota_fopen FILENAME MODE`. Returns
// a TCL handle token for the open file or "" on error. Mirrors
// src/test_quota.c::test_quota_fopen.
func (tp *transpiler) processSqlite3QuotaFopen(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_fopen", args)
		return
	}
	tp.emitLine("_r = tclQuotaFopen(%s, %s)", quotaArg(tp, args[0]), quotaArg(tp, args[1]))
}

// processSqlite3QuotaFclose handles `sqlite3_quota_fclose HANDLE`. Mirrors
// src/test_quota.c::test_quota_fclose.
func (tp *transpiler) processSqlite3QuotaFclose(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_fclose", args)
		return
	}
	tp.emitLine("tclQuotaFclose(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaFread handles `sqlite3_quota_fread HANDLE SIZE NELEM`.
// Returns the read content as a TCL binary string. Mirrors
// src/test_quota.c::test_quota_fread.
func (tp *transpiler) processSqlite3QuotaFread(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_fread", args)
		return
	}
	tp.emitLine("_r = tclQuotaFread(%s, %s, %s)",
		quotaArg(tp, args[0]),
		strconv.FormatInt(quotaAtoi(args[1].Text), 10),
		strconv.FormatInt(quotaAtoi(args[2].Text), 10))
}

// processSqlite3QuotaFwrite handles `sqlite3_quota_fwrite HANDLE SIZE NELEM CONTENT`.
// Mirrors src/test_quota.c::test_quota_fwrite.
func (tp *transpiler) processSqlite3QuotaFwrite(args []tcl.RawWord) {
	if len(args) < 4 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_fwrite", args)
		return
	}
	tp.emitLine("tclQuotaFwrite(%s, %s, %s, %s)",
		quotaArg(tp, args[0]),
		strconv.FormatInt(quotaAtoi(args[1].Text), 10),
		strconv.FormatInt(quotaAtoi(args[2].Text), 10),
		quotaArg(tp, args[3]))
}

// processSqlite3QuotaFflush handles `sqlite3_quota_fflush HANDLE ?HARDSYNC?`.
// Mirrors src/test_quota.c::test_quota_fflush.
func (tp *transpiler) processSqlite3QuotaFflush(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_fflush", args)
		return
	}
	hardsync := "false"
	if len(args) >= 2 && strings.TrimSpace(args[1].Text) == "1" {
		hardsync = "true"
	}
	tp.emitLine("tclQuotaFflush(%s, %s)", quotaArg(tp, args[0]), hardsync)
}

// processSqlite3QuotaFseek handles `sqlite3_quota_fseek HANDLE OFFSET WHENCE`.
// Mirrors src/test_quota.c::test_quota_fseek.
func (tp *transpiler) processSqlite3QuotaFseek(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_fseek", args)
		return
	}
	tp.emitLine("tclQuotaFseek(%s, %s, %s)",
		quotaArg(tp, args[0]),
		quotaArg(tp, args[1]),
		quotaArg(tp, args[2]))
}

// processSqlite3QuotaRewind handles `sqlite3_quota_rewind HANDLE`. Mirrors
// src/test_quota.c::test_quota_rewind.
func (tp *transpiler) processSqlite3QuotaRewind(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_rewind", args)
		return
	}
	tp.emitLine("tclQuotaRewind(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaFTell handles `sqlite3_quota_ftell HANDLE`. Mirrors
// src/test_quota.c::test_quota_ftell.
func (tp *transpiler) processSqlite3QuotaFTell(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_ftell", args)
		return
	}
	tp.emitLine("_r = tclQuotaFTell(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaFtruncate handles `sqlite3_quota_ftruncate HANDLE SIZE`.
// Mirrors src/test_quota.c::test_quota_ftruncate.
func (tp *transpiler) processSqlite3QuotaFtruncate(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_ftruncate", args)
		return
	}
	tp.emitLine("tclQuotaFtruncate(%s, %s)", quotaArg(tp, args[0]),
		strconv.FormatInt(quotaAtoi(args[1].Text), 10))
}

// processSqlite3QuotaFileAvailable handles `sqlite3_quota_file_available HANDLE`.
// Mirrors src/test_quota.c::test_quota_file_available.
func (tp *transpiler) processSqlite3QuotaFileAvailable(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_file_available", args)
		return
	}
	tp.emitLine("_r = tclQuotaFileAvailable(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaFileSize handles `sqlite3_quota_file_size HANDLE`.
// Mirrors src/test_quota.c::test_quota_file_size.
func (tp *transpiler) processSqlite3QuotaFileSize(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_file_size", args)
		return
	}
	tp.emitLine("_r = tclQuotaFileSize(%s)", quotaArg(tp, args[0]))
}

// processSqlite3QuotaFerror handles `sqlite3_quota_ferror HANDLE`.
// Mirrors src/test_quota.c::test_quota_ferror.
func (tp *transpiler) processSqlite3QuotaFerror(args []tcl.RawWord) {
	if len(args) < 1 {
		tp.emitUnsupportedStmtCmd("sqlite3_quota_ferror", args)
		return
	}
	tp.emitLine("_r = tclQuotaFerror(%s)", quotaArg(tp, args[0]))
}