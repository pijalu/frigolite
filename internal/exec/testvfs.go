package exec

import (
	"github.com/pijalu/frigolite/internal/pager"
)

// testvfs glue: the harness installs a named fixture VFS on a connection
// (the test_vfs.c shell's observable surface, slice S1).

// InstallVFSHook wires the fixture's callback hook and devchar override
// onto the connection's main pager. op is the VFS method name ("xSync"),
// path the file the method ran on — the fixture script dispatches on the
// file's tail (walvfs's xSync counts only test.db-wal syncs).
func (e *Engine) InstallVFSHook(fn func(op, path string) error, devcharBits int) {
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Pager != nil {
			ctx.Pager.SetVFSHook(fn)
			ctx.Pager.SetDeviceCharacteristics(devcharBits)
			// A fixture opening AFTER the pragmas ran still needs the
			// stored synchronous level (the pager default is NORMAL).
			if lvl, ok := e.settings.synchronousLevels["MAIN"]; ok {
				ctx.Pager.SetWALSyncFlags(walSyncFlagsForLevel(lvl))
			} else {
				ctx.Pager.SetWALSyncFlags(pager.WALSyncNormal)
			}
		}
	}
}
