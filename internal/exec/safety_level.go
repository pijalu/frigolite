package exec

import (
	"strconv"
	"strings"
)

// parseSafetyLevel mirrors pragma.c getSafetyLevel:72 (dflt=1): a numeric
// value is taken verbatim; the recognized words map through the
// "onoffalseyestruextrafull" table; anything else is NORMAL (1). The
// caller applies the (v+1)&3 mask.
func parseSafetyLevel(value string) int64 {
	v := strings.TrimSpace(value)
	if n, err := strconv.Atoi(v); err == nil {
		return int64(n)
	}
	switch strings.ToLower(v) {
	case "on", "yes", "true":
		return 1
	case "no", "off", "false":
		return 0
	case "extra":
		return 3
	case "full":
		return 2
	}
	return 1
}
