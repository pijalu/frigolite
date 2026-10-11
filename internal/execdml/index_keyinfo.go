// Collation-aware index-key ordering: the KeyInfo plumbing that makes index
// b-trees order their entries by VALUE under the keys' collations (build.c
// sqlite3KeyInfoFromIndex semantics — every index carries one collation per
// key column, resolved from the key's explicit COLLATE or the column's
// declared collation), instead of raw payload byte order.

package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// IndexKeyInfo builds the btree.KeyInfo for an index from its stored CREATE
// INDEX statement and the table's column definitions: one collation + sort
// flag per key column (explicit COLLATE wins, then the column's declared
// collation; DESC keys carry KeyInfoOrderDesc). Returns nil when the key
// list cannot be parsed (the caller then keeps the default comparator).
// lookup resolves registered custom collations in the context's native
// signature (nil = built-ins only).
func IndexKeyInfo(indexSQL string, colDefs []sql.ColumnDef, lookup func(string) func(a, b string) int) *btree.KeyInfo {
	colText := indexColumnListText(indexSQL)
	if colText == "" {
		return nil
	}
	colls := IndexKeyCollations(indexSQL, colDefs)
	if len(colls) == 0 {
		return nil
	}
	flags := parseIndexKeySortFlags(colText)
	names := make([]string, len(colls))
	for i, c := range colls {
		names[i] = strings.ToUpper(c)
	}
	return btree.NewKeyInfo(len(names), names, flags)
}

// installIndexOrder equips an index b-tree with the collation-aware record
// comparator for indexSQL's keys (a no-op when no KeyInfo can be built).
func installIndexOrder(tree *btree.BTree, indexSQL string, colDefs []sql.ColumnDef, lookup func(string) func(a, b string) int) {
	ki := IndexKeyInfo(indexSQL, colDefs, lookup)
	if ki == nil {
		return
	}
	tree.SetIndexKeyInfo(ki, indexOrderLookup(lookup))
}

// ikiOrder is installIndexOrder's memoizing form: same installed comparator,
// but the KeyInfo of the last index write is reused. It is the per-index-cell
// write path's hot call — the R14 profile put the CREATE INDEX re-parse at
// ~20% of an INSERT's cumulated allocation work.
//
// The memo key is (schema stamp, DatabaseContext, indexName): RootPage alone
// cannot key it because an index name can be reused across ATTACHed schemas
// (backup-2.x copies a second database whose indexes share MAIN's names), so
// the owning context and the name are both part of the identity. The stamp
// bounds the lifetime: a DROP/CREATE of any index in any database retires the
// entry.
func (e *DMLExecutor) ikiOrder(tree *btree.BTree, def indexDef, colDefs []sql.ColumnDef, lookup func(string) func(a, b string) int) {
	stamp := e.databasesSchemaStamp()
	if e.ikiStamp == stamp && e.ikiCtx == def.Ctx && e.ikiName == def.Name {
		tree.SetIndexKeyInfo(e.ikiVal, indexOrderLookup(lookup))
		return
	}
	ki := IndexKeyInfo(def.SQL, colDefs, lookup)
	if ki == nil {
		return
	}
	tree.SetIndexKeyInfo(ki, indexOrderLookup(lookup))
	e.ikiStamp, e.ikiCtx, e.ikiName, e.ikiVal = stamp, def.Ctx, def.Name, ki
}

// indexOrderLookup adapts the collation registry to btree's CollationLookup
// (the adapter SetIndexKeyInfo installs alongside the comparator).
func indexOrderLookup(lookup func(string) func(a, b string) int) btree.CollationLookup {
	return func(name string) (util.CollationFunc, bool) {
		if lookup == nil {
			return nil, false
		}
		if fn := lookup(name); fn != nil {
			return util.CollationFunc(fn), true
		}
		return nil, false
	}
}

// collationLookup exposes the context's collation registry in the native
// lookup signature for the index comparator plumbing.
func (e *DMLExecutor) collationLookup() func(string) func(a, b string) int {
	return e.ctx.LookupCollation
}
