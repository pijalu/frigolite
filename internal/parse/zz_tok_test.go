package parse

import (
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

func TestTokTrace(t *testing.T) {
	tz := sql.NewTokenizer("CREATE VIRTUAL TABLE temp.t1 USING csv(\n    data=\n'1,2\n5,6\n',\n    columns=2\n  )")
	for {
		tok := tz.Next()
		if tok.Type == sql.TokenEOF {
			break
		}
		t.Logf("type=%d val=%q pos=%d", tok.Type, tok.Value, tok.Pos)
		if tok.Type == 999 {
			break
		}
	}
}
