package exec

import (
	"fmt"
	"strings"
)

func stat4FormatCounts(counts []int) string {
	parts := make([]string, len(counts))
	for i, count := range counts {
		parts[i] = fmt.Sprint(count)
	}
	return strings.Join(parts, " ")
}
