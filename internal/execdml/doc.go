// Package execdml implements DML (INSERT/UPDATE/DELETE) execution.
//
// This package owns the write-path execution family: INSERT (VALUES, SELECT,
// DEFAULT, ON CONFLICT/upsert, RETURNING), UPDATE (with FROM, ORDER BY,
// LIMIT, triggers), DELETE (bulk, RETURNING, INSTEAD OF-trigger views),
// plus the shared OR-clause, RETURNING, rowid, and trigger-firing helpers
// those statements use.
//
// The DMLExecutor depends on a minimal DMLContext capability interface (the
// Engine in internal/exec implements it) rather than on the concrete engine
// type: Dependency Inversion. The statement families (InsertExecutor,
// UpdateExecutor, DeleteExecutor) are composed by DMLExecutor, isolating each
// concern (Single Responsibility). The Engine delegates INSERT/UPDATE/DELETE
// statements to the DMLExecutor's Insert/Update/Delete entry points.
package execdml
