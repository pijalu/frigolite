package vtab

import "fmt"

// EponymousOnlyModule is implemented by modules that are eponymous-only:
// SQLite registers them without an xCreate method, so CREATE VIRTUAL TABLE
// USING <module> fails with "no such module" while the module name is still
// usable directly in a FROM clause (series.c, generate_series).
type EponymousOnlyModule interface {
	Module
	// EponymousOnly reports whether the module rejects CREATE VIRTUAL TABLE.
	EponymousOnly() bool
}

// EponymousModule is implemented by modules registered with
// xCreate == xConnect (carray.c): besides supporting CREATE VIRTUAL TABLE,
// the module name is directly usable in a FROM clause as its implicit
// instance with hidden-column constraint bindings.
type EponymousModule interface {
	Module
	// Eponymous reports whether the module name is usable in FROM.
	Eponymous() bool
}

// ModuleIsEponymous reports whether a module's name is usable directly in a
// FROM clause: either eponymous-only (no xCreate) or fully eponymous
// (xCreate == xConnect).
func ModuleIsEponymous(module Module) bool {
	if eo, ok := module.(EponymousOnlyModule); ok && eo.EponymousOnly() {
		return true
	}
	if em, ok := module.(EponymousModule); ok && em.Eponymous() {
		return true
	}
	return false
}

// HiddenConstraintSetter is implemented by virtual-table instances that can
// absorb WHERE-clause equality bindings on their hidden columns before rows
// are generated. This mirrors SQLite's xBestIndex/xFilter argv handoff
// (e.g. FROM generate_series WHERE start=1 AND stop=9 AND step=2). The
// engine evaluates the constraint values itself and calls this method once
// per binding; unknown column names are reported as an error so the caller
// can fall back to plain filtering.
type HiddenConstraintSetter interface {
	SetHiddenConstraint(col string, val interface{}) error
}

// RowidCursor is implemented by cursors that expose a native rowid
// (xRowid parity). The materializer uses it to back the "rowid" column.
type RowidCursor interface {
	Rowid() int64
}

// InstanceValidator lets a virtual-table instance report missing or unusable
// hidden-constraint bindings before row generation begins (series.c's
// bStartSeen check raising 'first argument to "generate_series()" missing
// or unusable').
type InstanceValidator interface {
	ValidateInstance() error
}

// ValueRangeNarrower is implemented by virtual tables that can shrink their
// generated range from equality/range constraints on the "value" column
// (series.c xFilter iMin/iMax narrowing). min or max is nil when unbounded.
type ValueRangeNarrower interface {
	NarrowValueRange(min, max *int64)
}

// ValueConstraintExpander widens omitted START/STOP defaults to the full
// int64 range when the query constrains only the VALUE column (series.c
// xFilter: idxNum 0x05==0 with any 0x0380 bit widens START; 0x06==0 with any
// 0x3080 bit widens STOP). lowerSeen reports an equality or >=/> constraint;
// upperSeen an equality or <=/< constraint.
type ValueConstraintExpander interface {
	ExpandValueDefaults(lowerSeen, upperSeen bool)
}

// RowUpdater is implemented by updatable virtual-table instances (xUpdate
// parity). values slices hold one entry per declared column, unwrapped SQL
// values (nil = NULL).
// PrimaryKeyInfo is implemented by virtual tables whose declared schema
// carries PRIMARY KEY markers; PRAGMA table_info reports the pk column.
// ConnectModule is implemented by modules whose Connect carries
// connection-specific diagnostics; SELECT contexts prefer it over Create.
type ConnectModule interface {
	Module
}

type PrimaryKeyInfo interface {
	PrimaryKeyColumns() map[int]bool
}

// ExplicitNull marks a column whose UPDATE SET assigned a literal NULL —
// distinct from an unassigned column, which the core fills with the old
// value. Virtual tables can use it to implement xUpdate's NULL semantics
// (zipfile: SET data=NULL turns the entry into a directory).
type ExplicitNull struct{}

type RowUpdater interface {
	// UpdateRow applies the new column values to the row identified by
	// oldValues (xUpdate's argv[0]=old row / argv[2..]=new values form).
	UpdateRow(oldValues, newValues []interface{}) error
	// InsertRow inserts one row and returns its rowid. INSERT semantics are
	// module-defined (dbpage acts as REPLACE, truncating on NULL data).
	InsertRow(values []interface{}) (int64, error)
	// DeleteRow removes the row described by oldValues.
	DeleteRow(oldValues []interface{}) error
}

// DirectOnlyModule is implemented by modules registered with
// SQLITE_VTAB_DIRECTONLY: their virtual tables may only be used directly by
// top-level SQL — references from inside triggers or views raise "unsafe use
// of virtual table \"%s\"" regardless of PRAGMA trusted_schema.
type DirectOnlyModule interface {
	DirectOnly() bool
}

// ModuleIsDirectOnly reports whether a module forbids use inside trigger
// bodies and views.
func ModuleIsDirectOnly(module Module) bool {
	do, ok := module.(DirectOnlyModule)
	return ok && do.DirectOnly()
}

// setInt64 coerces a constraint value to int64 the way SQLite's
// sqlite3_value_int64 does for numeric strings, floats and integers.
func setInt64(val interface{}) (int64, error) {
	switch v := val.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case float64:
		return int64(v), nil
	case string:
		var out int64
		if _, err := fmt.Sscanf(v, "%d", &out); err != nil {
			return 0, fmt.Errorf("not an integer: %q", v)
		}
		return out, nil
	default:
		return 0, fmt.Errorf("unsupported constraint value type %T", val)
	}
}

// LimitPushdown is implemented by virtual-table modules whose instances can
// produce unbounded output (e.g. generate_series with only a START bound).
// For those, the core pushes a LIMIT into the scan even though residual
// WHERE clauses will still apply afterwards; every other module is
// materialized in full before filtering/sorting/limiting.
type LimitPushdown interface {
	NeedsLimitPushdown() bool
}

// MatchConstraintSetter is implemented by virtual-table instances whose rows
// are generated from a `column MATCH <target>` constraint (approximate_match).
type MatchConstraintSetter interface {
	SetMatchConstraint(column, target string)
}
