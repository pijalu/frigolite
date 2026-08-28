// Package exec implements query execution.
//
// Rename utilities are re-exported from the shared internal/rename package
// for compatibility with existing callers within the exec package.
package exec

import (
	"github.com/pijalu/frigolite/internal/rename"
)

// RenameContext tracks the rename operation state.
// Deprecated: use rename.RenameContext instead.
type RenameContext = rename.RenameContext

// RenameRange represents a byte range in the original SQL text to replace.
// Deprecated: use rename.RenameRange instead.
type RenameRange = rename.RenameRange

// FindRenameTokens parses SQL text and returns all byte ranges that should be
// replaced when renaming a table or column.
// Deprecated: use rename.FindRenameTokens instead.
var FindRenameTokens = rename.FindRenameTokens

// ApplyRenames applies a set of byte-range replacements to a SQL text.
// Deprecated: use rename.ApplyRenames instead.
var ApplyRenames = rename.ApplyRenames
