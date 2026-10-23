package frigolite

import (
	"strings"
	"testing"
)

func TestDetachExpressionResolvesSelectSourceBeforeEvaluation(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	r := db.Exec(`DETACH RAISE ( IGNORE ) IN ( SELECT "AAAAAA" . * ORDER BY REGISTER LIMIT "AAAAAA" . "AAAAAA" OFFSET RAISE ( IGNORE ) NOT NULL )`)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: AAAAAA") {
		t.Fatalf("DETACH compile error = %v, want missing SELECT table before expression evaluation", r.Error)
	}
}
