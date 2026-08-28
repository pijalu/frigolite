package exec

import (
	"github.com/pijalu/frigolite/internal/execexpr"
)

// extractValue extracts the raw value and collation from a potentially
// collated value (delegates to execexpr).
func extractValue(v interface{}) (interface{}, string) {
	return execexpr.ExtractValue(v)
}

// isColumnValue reports whether v is a column value (delegates to execexpr).
func isColumnValue(v interface{}) bool {
	return execexpr.IsColumnValue(v)
}
