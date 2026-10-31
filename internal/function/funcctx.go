package function

// FuncContext is the per-invocation context handed to scalar functions
// registered with RegisterContext — the sqlite3_context stand-in. Its first
// capability is the sqlite3_get_auxdata()/sqlite3_set_auxdata() API: one
// auxiliary-data slot per (call site, argument index), living for the
// duration of the prepared statement's execution (C: Vdbe.pAuxData,
// vdbeapi.c sqlite3_get_auxdata/sqlite3_set_auxdata).
//
// The auxiliary storage itself is owned by the execution engine's evaluator
// (statement-scoped, destroyed with destructor callbacks when the statement
// boundary resets); FuncContext only adapts it to the per-call shape. The
// AuxStore port keeps this package below the evaluator in the layer stack
// (dependency inversion).
type FuncContext struct {
	// store is the statement-scoped auxdata table; wired per call by the
	// evaluator. Never nil for a context handed out by the dispatcher.
	store AuxStore
	// site identifies the call site (the *sql.FuncCall node in this
	// engine). C keys auxdata by the OP_Function instruction address; the
	// two sites of one function call in one statement therefore keep
	// distinct slots.
	site interface{}
	// newSlot mirrors C's soft `pCtx->isError = -1` flag: set when
	// SetAuxData created a NEW (site, arg) slot during this invocation. The
	// dispatcher runs the post-call release (drop non-constant-argument
	// slots at this site) exactly when C does: after the call, when the
	// soft flag is set or the call errored (vdbe.c OP_Function).
	newSlot bool
}

// AuxStore is the auxiliary-data storage port the evaluator implements for
// FuncContext. Site is the call-site identity; arg is the function-argument
// index (negative arg = the statement-global cache shared by all function
// sites — C's undocumented negative-iArg behavior, vdbeapi.c:1157).
type AuxStore interface {
	// GetAuxData returns the value stored for (site, arg) and whether a
	// slot exists.
	GetAuxData(site interface{}, arg int) (interface{}, bool)
	// SetAuxData stores v under (site, arg), running the previous value's
	// destructor when the slot already existed, and reports whether a NEW
	// slot was created (C's isError=-1 trigger).
	SetAuxData(site interface{}, arg int, v interface{}, destroy func(interface{})) bool
}

// NewFuncContext wires a per-call context to the statement-scoped auxdata
// table for one call site. The evaluator calls this once per function
// invocation; the site key must be stable for the call site within the
// statement.
func NewFuncContext(store AuxStore, site interface{}) *FuncContext {
	return &FuncContext{store: store, site: site}
}

// GetAuxData returns the auxiliary data previously stored for argument arg
// at this call site, or nil (sqlite3_get_auxdata). A negative arg reads the
// statement-global cache shared by every function site.
func (c *FuncContext) GetAuxData(arg int) interface{} {
	v, _ := c.store.GetAuxData(c.site, arg)
	return v
}

// SetAuxData stores v as the auxiliary data for argument arg at this call
// site, with an optional destructor (sqlite3_set_auxdata). The destructor
// runs when the value is replaced, when the statement boundary drops the
// slot, or when the post-call release discards a non-constant-argument
// slot. A negative arg writes the statement-global cache.
func (c *FuncContext) SetAuxData(arg int, v interface{}, destroy func(interface{})) {
	if c.store.SetAuxData(c.site, arg, v, destroy) {
		c.newSlot = true
	}
}

// AuxSlotCreated reports whether SetAuxData created a new (site, arg) slot
// during this invocation (C's isError==-1 soft flag): the dispatcher uses
// it to run the post-call release of non-constant-argument slots.
func (c *FuncContext) AuxSlotCreated() bool { return c.newSlot }

// Site returns the call-site identity this context is bound to (C's
// OP_Function instruction address equivalent — the *sql.FuncCall node in
// this engine). Two sites of the same function in one statement carry
// distinct auxdata.
func (c *FuncContext) Site() interface{} { return c.site }
