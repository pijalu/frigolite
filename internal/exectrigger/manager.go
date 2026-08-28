// Package exectrigger manages trigger execution state.
//
// The package owns the trigger state fields that previously lived on the
// Engine: the current trigger nesting depth (and its limit), the chain of
// tables in trigger programs, the NEW/OLD row values for trigger expression
// evaluation, and the per-table trigger-existence and trigger-validation
// caches. The Engine delegates trigger state access here; trigger firing
// itself lives in internal/execdml (SOLID-09/10).
package exectrigger

import (
	"github.com/pijalu/frigolite/internal/execquery"
)

// Row provides column value lookup for expression evaluation (alias of the
// query engine's row abstraction).
type Row = execquery.Row

// Manager owns trigger execution state that previously lived on the Engine.
type Manager struct {
	// depth is the current trigger execution nesting depth.
	depth int
	// depthLimit is SQLITE_LIMIT_TRIGGER_DEPTH (0 = use the DML executor's
	// SQLITE_MAX_TRIGGER_DEPTH default).
	depthLimit int
	// tables is the chain of tables currently in trigger programs.
	tables []string
	// newRow holds the new-row values for trigger execution (keyed as
	// "new.colname").
	newRow Row
	// oldRow holds the old-row values for trigger execution (keyed as
	// "old.colname").
	oldRow Row
	// hasTriggersCache caches trigger existence per table name.
	hasTriggersCache map[string]bool
	// validatedTriggers records triggers whose loaded-body schema refs were
	// validated.
	validatedTriggers map[string]bool
}

// New creates a TriggerManager with default state. The depth limit is left at
// 0 ("use the engine's SQLITE_MAX_TRIGGER_DEPTH default") so the DML executor's
// trigger-depth check applies its own maxTriggerDepth constant, matching the
// original Engine behavior.
func New() *Manager {
	return &Manager{
		hasTriggersCache: make(map[string]bool),
	}
}

// Depth returns the current trigger execution depth.
func (m *Manager) Depth() int { return m.depth }

// SetDepth sets the current trigger execution depth.
func (m *Manager) SetDepth(depth int) { m.depth = depth }

// DepthLimit returns the maximum trigger nesting depth (0 = default).
func (m *Manager) DepthLimit() int { return m.depthLimit }

// SetDepthLimit sets the maximum trigger nesting depth. A negative value
// queries the current limit without changing it.
func (m *Manager) SetDepthLimit(n int) int {
	if n >= 0 {
		m.depthLimit = n
	}
	return m.depthLimit
}

// Tables returns the chain of tables currently in trigger programs.
func (m *Manager) Tables() []string { return m.tables }

// SetTables sets the chain of tables in trigger programs.
func (m *Manager) SetTables(tables []string) { m.tables = tables }

// NewRow returns the new-row values for trigger program execution.
func (m *Manager) NewRow() Row { return m.newRow }

// SetNewRow sets the new-row values for trigger program execution.
func (m *Manager) SetNewRow(row Row) { m.newRow = row }

// OldRow returns the old-row values for trigger program execution.
func (m *Manager) OldRow() Row { return m.oldRow }

// SetOldRow sets the old-row values for trigger program execution.
func (m *Manager) SetOldRow(row Row) { m.oldRow = row }

// CachedTriggerFlag returns the cached trigger-existence flag for a table.
func (m *Manager) CachedTriggerFlag(tableName string) (bool, bool) {
	has, ok := m.hasTriggersCache[tableName]
	return has, ok
}

// SetCachedTriggerFlag caches the trigger-existence flag for a table.
func (m *Manager) SetCachedTriggerFlag(tableName string, has bool) {
	m.hasTriggersCache[tableName] = has
}

// ResetHasTriggersCache clears the cached trigger-existence flags.
func (m *Manager) ResetHasTriggersCache() {
	m.hasTriggersCache = make(map[string]bool)
}

// InitValidatedTriggers ensures the validated-trigger cache is non-nil.
func (m *Manager) InitValidatedTriggers() {
	if m.validatedTriggers == nil {
		m.validatedTriggers = make(map[string]bool)
	}
}

// IsTriggerValidated reports whether a trigger was already validated.
func (m *Manager) IsTriggerValidated(key string) bool {
	return m.validatedTriggers[key]
}

// MarkTriggerValidated records a trigger as validated.
func (m *Manager) MarkTriggerValidated(key string) {
	m.validatedTriggers[key] = true
}
