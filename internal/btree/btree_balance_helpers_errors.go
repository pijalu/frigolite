// P8.INCRVACUUM phase 5.5: small btree-internal error helpers used
// across the btree.c port. They all live here so the rebalance /
// balance / copyNodeContent / rebuildPage / editPage / balance_quick
// implementations share a consistent error vocabulary.

package btree

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
)

// errBtreeNotInterior reports a misuse of an interior-only btree
// routine on a non-interior page. Mirrors the asserts at the top of
// the C functions (e.g. btree.c::copyNodeContent assumes
// pFrom->leaf == 0).
func errBtreeNotInterior(fn string, pgno uint32, pageType byte) error {
	return fmt.Errorf("btree: %s: page %d is not interior (type 0x%02x)", fn, pgno, pageType)
}

// errBtreeCorrupt reports a structural inconsistency in the btree
// page. Mirrors the SQLITE_CORRUPT paths in btree.c.
func errBtreeCorrupt(format string, args ...interface{}) error {
	return fmt.Errorf("btree: corrupt: "+format, args...)
}

// ensure interface compile-time check for the pager.Page
// (no-op; just keeps the import live so the helpers file does not
// drift away from the pager dependency).
var _ = (*pager.Page)(nil)
