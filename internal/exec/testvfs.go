package exec

import (
	"github.com/pijalu/frigolite/internal/pager"
)

// testvfs glue: the harness installs a named fixture VFS on a connection
// (the test_vfs.c shell's observable surface, slice S1).

// InstallVFSHook wires the fixture's callback hook and devchar override
// onto the connection's main pager. op is the VFS method name ("xSync"),
// path the file the method ran on, details the method-specific trailing
// arguments (test_vfs.c's tvfsExecTcl shapes) — the fixture script
// dispatches on the file's tail (walvfs's xSync counts only test.db-wal
// syncs) and reads the details for the lock/map/write shapes
// (e_walckpt's "1 1 lock exclusive"). devcharBits < 0 is test_vfs.c's
// iDevchar=-1 passthrough state — the pager keeps the unix-VFS default
// mask (POWERSAFE_OVERWRITE) and the fixture never overrides it.
func (e *Engine) InstallVFSHook(fn func(op, path string, details []string) error, devcharBits int) {
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Pager != nil {
			ctx.Pager.SetVFSHook(fn)
			// The rollback-journal sidecar's xOpen/xClose/xDelete events go
			// through the same fixture VFS: C's testvfs intercepts every
			// method of every file it backs, and pager1-4.5.1's
			// copy_on_journal_delete faultsim_save fires on the journal's
			// xDelete. The journal hook's fire-and-forget signature wraps
			// the fixture's (a script answer never vetoes these events).
			pgr := ctx.Pager
			pgr.SetJournalFileOpHook(func(op, path string) {
				_ = fn(op, path, nil)
			})
			if devcharBits >= 0 {
				ctx.Pager.SetDeviceCharacteristics(devcharBits)
			}
			// A fixture opening AFTER the pragmas ran still needs the
			// stored synchronous level; with none stored the C default
			// applies (SQLITE_DEFAULT_SYNCHRONOUS=2, pragma numbering =
			// FULL — commit sync bits set).
			if lvl, ok := e.settings.synchronousLevels["MAIN"]; ok {
				ctx.Pager.SetWALSyncFlags(walSyncFlagsForLevel(lvl))
			} else {
				ctx.Pager.SetWALSyncFlags(pager.WALSyncFull)
			}
		}
	}
}

// PagerShmID renders the connection's shm identifier (test_vfs.c's pShmId)
// the fixture callbacks receive as their third argument.
func (e *Engine) PagerShmID() string {
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Pager != nil {
			return ctx.Pager.ShmID()
		}
	}
	return "0"
}

// PagerForDB resolves a database name ("main" or an attached name) to its
// pager — the vfs_set_readmark surface addresses a connection's wal-index
// through the named database.
func (e *Engine) PagerForDB(name string) *pager.Pager {
	ctx := e.GetDB(name)
	if ctx == nil {
		return nil
	}
	return ctx.Pager
}
