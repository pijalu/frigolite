package vtab

import (
	"strconv"
	"strings"
)

// EchoSource resolves the echo module's source-table facts for one database
// connection (test8.c resolves them through the same connection). The engine
// supplies the implementation; this package stays layer-clean by depending on
// plain strings only.
type EchoSource interface {
	// EchoColumnDefs returns the source table's declared column names and
	// their type strings in declaration order. ok is false when the table
	// does not exist (test8.c echoDeclareVtab finds no sqlite_schema row).
	EchoColumnDefs(srcTable string) (names []string, types []string, ok bool)
	// EchoLeadingIndex reports whether the source table has an index whose
	// left-most column is column index col (test8.c getIndexArray builds
	// echo_vtab.aIndex from PRAGMA index_list/index_info).
	EchoLeadingIndex(srcTable string, col int) bool
}

// SilentConstructorError marks a module-constructor failure that carries no
// error message of its own (test8.c echoConstructor returns SQLITE_ERROR with
// zErr==0 when the source table cannot be resolved). The core then reports
// "vtable constructor failed: <table>" (vtab.c vtabCallConstructor); an error
// WITH a message passes through verbatim.
type SilentConstructorError struct{ Err error }

// Error returns the wrapped message ("" when the constructor set no message).
func (e *SilentConstructorError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap exposes the wrapped error for errors.Is chains.
func (e *SilentConstructorError) Unwrap() error { return e.Err }

// SilentConstructorFailure builds a SilentConstructorError (test8.c
// echoDeclareVtab's bare SQLITE_ERROR when the source table has no schema
// row).
func SilentConstructorFailure() error {
	return &SilentConstructorError{}
}

// EchoModule is the echo virtual-table module (SQLite test-only module,
// src/test8.c): CREATE VIRTUAL TABLE t1 USING echo(treal) mirrors the source
// table treal — echoDeclareVtab declares the source table's CREATE statement
// as the vtab schema, echoCursor reads the source rows, and writes route to
// the source table. Row storage stays in the exec layer's source-table
// materializer; this module ports the constructor/xBestIndex OBSERVABLE
// contract:
//
//   - echo with zero arguments never declares a schema, so the core raises
//     "vtable constructor did not declare schema: t1" (vtab1-1.3.x);
//   - echo with a source argument naming no table fails with no message, so
//     the core raises "vtable constructor failed: t1" (vtab1-1.5.x);
//   - xBestIndex (echoBestIndex) treats every offered constraint on an
//     indexed column (or the rowid) as claimable via argvIndex+omit unless
//     the TCL variable echo_module_ignore_usable is set — claiming an
//     UNUSABLE constraint is exactly the contract violation the core reports
//     as "<name>.xBestIndex malfunction" (vtab6-11.4.x). The
//     echo_module_cost variable overrides the estimated cost.
type EchoModule struct {
	Src EchoSource
	// createName is the vtab's own name for the CREATE in flight
	// (sqlite3 xCreate's argv[2]); the pattern source form resolves
	// <name><suffix> during the constructor.
	createName string
}

// NewEchoModule builds the echo module over a connection-bound source
// resolver (register_echo_module registers one per connection in SQLite;
// the module is NOT part of RegisterDefaults).
func NewEchoModule(src EchoSource) *EchoModule { return &EchoModule{Src: src} }

// SetCreateName delivers the CREATE VIRTUAL TABLE target name before
// Module.Create runs (vtab.CreateNameSetter parity of argv[2]).
func (m *EchoModule) SetCreateName(name string) { m.createName = name }

// echoVTab is one echo virtual-table instance (test8.c echo_vtab).
type echoVTab struct {
	src       EchoSource // the connection's source resolver
	zSrc      string     // the source ("real") table name, dequoted
	srcSuffix string     // pattern form: the text after the leading '*'
	isPattern bool       // source name began with '*' (echoConstructor)
	cols      []string   // source column names (echo_vtab.aCol)
	types     []string   // source column declared types
	aIndex    []bool     // per-column leading-index flags (echo_vtab.aIndex)
}

// echoDequote ports test8.c dequoteString: strip one level of ' " ` or []
// quoting (with doubled-quote escapes). Unquoted input is returned as-is.
func echoDequote(s string) string {
	if s == "" {
		return s
	}
	quote := s[0]
	var close byte
	switch quote {
	case '\'', '"', '`':
		close = quote
	case '[':
		close = ']'
	default:
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] == close {
			if i+1 < len(s) && s[i+1] == close {
				b.WriteByte(close)
				i++
				continue
			}
			return b.String()
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (m *EchoModule) Create(args []string) (VirtualTable, error) { return m.construct(args) }

// Connect ports echoConnect: on an existing schema entry the constructor
// re-resolves the source table (xConnect runs when the schema is next
// required after a reopen).
func (m *EchoModule) Connect(args []string) (VirtualTable, error) { return m.construct(args) }

// construct ports test8.c echoConstructor + echoDeclareVtab. argv conventions
// mirror SQLite's azArg with argv[0..3] stripped: args[0] (when present) is
// the source table name. The vtab's OWN name (argv[2], needed only by the '*'
// pattern source form) arrives via BindSchema after Create.
func (m *EchoModule) construct(args []string) (VirtualTable, error) {
	v := &echoVTab{src: m.Src}
	if len(args) > 0 {
		src := echoDequote(args[0])
		// A leading '*' marks a pattern source: the real table is
		// <this-table-name><rest> (echoConstructor isPattern branch).
		if strings.HasPrefix(src, "*") {
			v.isPattern = true
			v.srcSuffix = src[1:]
			if m.createName != "" {
				v.zSrc = m.createName + v.srcSuffix
			}
		} else if src != "" {
			v.zSrc = src
		}
	}
	if v.zSrc == "" && !v.isPattern {
		// No source argument: echoDeclareVtab declares nothing, so the core
		// reports "vtable constructor did not declare schema: <name>"
		// (vtab1-1.3.x). The instance still exists (xCreate returned OK).
		return v, nil
	}
	if m.Src == nil {
		return nil, SilentConstructorFailure()
	}
	if v.isPattern && v.zSrc == "" {
		// The pattern source resolves as <this-name><suffix>; the vtab's own
		// name arrived too late for this constructor (no SetCreateName), so
		// the source resolution defers to BindSchema.
		return v, nil
	}
	if !v.bindSource() {
		// No schema row for the source table: bare SQLITE_ERROR, no message
		// → "vtable constructor failed: <name>" from the core (vtab1-1.5.x).
		return nil, SilentConstructorFailure()
	}
	return v, nil
}

// bindSource records the resolved source schema and the leading-index array
// (getColumnNames + getIndexArray in test8.c). ok is false when the source
// table does not exist.
func (v *echoVTab) bindSource() bool {
	names, types, ok := v.src.EchoColumnDefs(v.zSrc)
	if !ok {
		return false
	}
	v.cols = names
	v.types = types
	v.aIndex = make([]bool, len(names))
	for i := range names {
		v.aIndex[i] = v.src.EchoLeadingIndex(v.zSrc, i)
	}
	return true
}

// BindSchema implements SchemaBoundVTab: the engine hands the vtab its own
// name, which completes the deferred pattern-form source resolution
// (zTableName = zThis + suffix, echoConstructor).
func (v *echoVTab) BindSchema(dbName, tableName string) error {
	if !v.isPattern || v.zSrc != "" || v.src == nil {
		return nil
	}
	v.zSrc = tableName + v.srcSuffix
	if !v.bindSource() {
		return SilentConstructorFailure()
	}
	return nil
}

// Columns implements ColumnInfo: the declared schema mirrors the source table
// (sqlite3_declare_vtab parity). A no-argument constructor has no columns.
func (v *echoVTab) Columns() []string { return v.cols }

// ColumnTypes implements ColumnTypeInfo: the source table's declared types.
func (v *echoVTab) ColumnTypes() []string { return v.types }

// BestIndex implements the legacy byte contract (superseded for planning by
// BestIndexPlan).
func (v *echoVTab) BestIndex([]byte) ([]byte, error) { return nil, nil }

// BestIndexPlan ports test8.c echoBestIndex's observable decisions:
//
//   - the echo_module_ignore_usable TCL variable makes constraints with
//     usable==0 eligible for claiming (isIgnoreUsable) — claiming an unusable
//     constraint is the contract violation the core reports as
//     "<name>.xBestIndex malfunction" (vtab6-11.4.x);
//   - constraints on an indexed column or the rowid are claimed via
//     argvIndex + omit (echo_vtab.aIndex membership test);
//   - the echo_module_cost TCL variable overrides estimatedCost;
//   - a single ORDER BY term on an indexed column (or rowid) sets
//     orderByConsumed.
func (v *echoVTab) BestIndexPlan(ii *IndexInfo) error {
	v.applyCost(ii)
	v.claimConstraints(ii, TclVarGet("echo_module_ignore_usable", "") != "")
	v.consumeOrderBy(ii)
	return nil
}

// applyCost honors the echo_module_cost TCL variable as estimatedCost
// (echoBestIndex's useCost branch).
func (v *echoVTab) applyCost(ii *IndexInfo) {
	cost := TclVarGet("echo_module_cost", "")
	if cost == "" {
		return
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(cost), 64); err == nil {
		ii.EstimatedCost = f
	}
}

// claimConstraints ports echoBestIndex's constraint loop: a constraint on an
// indexed column (or the rowid) is claimed via argvIndex+omit; with
// isIgnoreUsable the usable==0 flag is ignored (the claim that makes the
// core report a malfunction).
func (v *echoVTab) claimConstraints(ii *IndexInfo, isIgnoreUsable bool) {
	nArg := 0
	for i := range ii.Constraints {
		c := &ii.Constraints[i]
		if !isIgnoreUsable && !c.Usable {
			continue
		}
		if !v.claimable(c.Column) {
			continue
		}
		nArg++
		ii.Usage[i].ArgvIndex = nArg
		ii.Usage[i].Omit = true
	}
}

// claimable reports whether the column (rowid for <0) is indexed
// (echo_vtab.aIndex membership, test8.c getIndexArray).
func (v *echoVTab) claimable(col int) bool {
	return col < 0 || (col < len(v.aIndex) && v.aIndex[col])
}

// consumeOrderBy ports echoBestIndex's orderByConsumed branch: a single
// ORDER BY term on an indexed column / rowid is consumed.
func (v *echoVTab) consumeOrderBy(ii *IndexInfo) {
	if len(ii.OrderBy) != 1 {
		return
	}
	if v.claimable(ii.OrderBy[0].Column) {
		ii.OrderByConsumed = true
	}
}

// Open returns a cursor that reads no rows: the echo module's row storage
// lives in the engine's source-table materializer, which never opens module
// cursors. Legacy storageless DML paths still see a valid instance.
func (v *echoVTab) Open() (Cursor, error) { return v, nil }

// Next implements Cursor (no rows).
func (v *echoVTab) Next() bool { return false }

// Column implements Cursor.
func (v *echoVTab) Column(int) (interface{}, error) { return nil, nil }

// Close implements Cursor.
func (v *echoVTab) Close() error { return nil }

// Compile-time interface conformance checks.
var (
	_ Module          = (*EchoModule)(nil)
	_ ColumnInfo      = (*echoVTab)(nil)
	_ ColumnTypeInfo  = (*echoVTab)(nil)
	_ PlanBestIndexer = (*echoVTab)(nil)
	_ SchemaBoundVTab = (*echoVTab)(nil)
	_ VirtualTable    = (*echoVTab)(nil)
	_ error           = (*SilentConstructorError)(nil)
)
