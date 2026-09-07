// UCL conformance: our recovered SQL must match the reference build's
// .recover output (testdata/recoverconformance), captured from the
// 3.51.0 reference binary (P8.RECOVER sub-plan, UCL tranche).

package recover

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

const fixtureDir = "../../testdata/recoverconformance"

func TestRecoverConformance(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(fixtureDir, "*.input.db"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".input.db")
		t.Run(name, func(t *testing.T) {
			wantSQL, err := os.ReadFile(filepath.Join(fixtureDir, name+".recover.sql"))
			if err != nil {
				t.Fatal(err)
			}
			pg, err := pager.OpenReadOnly(in, 1024)
			if err != nil {
				t.Fatal(err)
			}
			defer pg.Close()
			got, err := RecoverSQL(pg, Options{})
			if err != nil {
				t.Fatal(err)
			}
			// The harness strips the .dbconfig line before executing.
			want := strings.ReplaceAll(string(wantSQL), ".dbconfig defensive off\n", "")
			if got != want {
				gotLines := strings.Split(got, "\n")
				wantLines := strings.Split(want, "\n")
				for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
					g, w := "", ""
					if i < len(gotLines) {
						g = gotLines[i]
					}
					if i < len(wantLines) {
						w = wantLines[i]
					}
					if g != w {
						t.Fatalf("first divergence at line %d:\n  got:  %q\n  want: %q", i+1, g, w)
					}
				}
				t.Fatalf("recovered SQL differs (same length?)")
			}
			_ = fmt.Sprint()
		})
	}
}
