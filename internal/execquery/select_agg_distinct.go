package execquery

// Aggregate DISTINCT deduplication helpers (split from select_agg.go for
// file-size hygiene).

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/value"
)

// distinctKey builds a deduplication key for already-evaluated aggregate args.
// A zeroblob(N) argument keys as its expanded zero bytes so it deduplicates
// against an equal materialized blob (zeroblob-3.1: count(DISTINCT a) over
// x'00000000000000000000' and zeroblob(10) is 1) — same normalization as
// rowValueKey (select_setop.go).
func distinctKey(args []interface{}) string {
	var key string
	for _, a := range args {
		if a == nil {
			key += "\x00"
			continue
		}
		if z, ok := a.(value.ZeroBlob); ok {
			key += "b:" + string(z.Bytes()) + "\x00"
			continue
		}
		if b, ok := a.([]byte); ok {
			key += "b:" + string(b) + "\x00"
			continue
		}
		key += fmt.Sprintf("%v", a) + "\x00"
	}
	return key
}
