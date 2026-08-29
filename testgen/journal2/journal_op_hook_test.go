//go:build testgen
// +build testgen

// Non-generated helper for the journal2 testgen package.
//
// The TCL source uses testvfs to capture xOpen/xClose/xDelete events on the
// journal sidecar; frigolite does not have a full VFS plugin system, but
// exposes a process-wide hook (pager.SetDefaultJournalFileOpHook) that
// every Pager consults for journal-sidecar events. This file installs
// that hook (via init) so the generated test, which resets the package-
// level `oplog` variable between do_test blocks, captures every journal-
// sidecar event into oplog for the expected-list comparisons in
// Test_journal2. Using the default hook means the hook is observed for
// every connection the test opens (db / db2 / ...), not just one.
package journal2

import (
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
)

// journalOpHook appends every journal-sidecar event to the package-level
// `oplog` variable in the format the TCL test expects: "xOpen test.db-journal
// xClose test.db-journal xDelete test.db-journal". The hook only fires for
// the journal sidecar (the hook path is built so it ends in "-journal").
func journalOpHook(op, path string) {
	if !strings.HasSuffix(path, "-journal") {
		return
	}
	// Each event is appended as " OP PATH" with a leading space; the test
	// resets oplog to "" at the start of every do_test block, and the
	// assertion flattens whitespace via tclListFlatten (which strips the
	// leading space via strings.Fields).
	oplog += " " + op + " " + path
}

// init installs the journal-op hook as the process-wide default so every
// Pager in the test (db, db2, ...) observes journal-sidecar events.
func init() {
	pager.SetDefaultJournalFileOpHook(journalOpHook)
}