package function

import (
	"fmt"
	"strings"
	"testing"
)

func TestP6QuotesParse(t *testing.T) {
	for n := 40; n < 60; n++ {
		str := "abcdef" + strings.Repeat("\"", n) + "uvwxyz"
		arr := jsonSerialize(&jsonNode{kind: jsonArray, arr: []*jsonNode{{kind: jsonString, str: str}}})
		node, err := parseJSON(arr)
		if err != nil {
			t.Errorf("n=%d parse failed: %v\ntext=%s", n, err, arr)
			return
		}
		comps, _ := parseJSONPath("$[0]")
		got, ok := jsonLookup(node, comps)
		if !ok || got.str != str {
			t.Errorf("n=%d lookup mismatch: ok=%v got=%q", n, ok, fmt.Sprint(got))
			return
		}
	}
}
