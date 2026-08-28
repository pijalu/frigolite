// Package execddl implements DDL execution (CREATE, DROP, ALTER, ATTACH,
// DETACH).
//
// The DDL executor owns the CREATE/DROP/ALTER statement family: table,
// index, view, trigger, and virtual-table creation, drop operations, column
// and table renames, ADD/DROP/ALTER column operations, and schema dependency
// analysis. It delegates engine capability access back to the execution
// engine via the DDLContext interface (Dependency Inversion).
package execddl
