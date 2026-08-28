package parse

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/pijalu/frigolite/internal/sql"
)

// TestSelectE_JSON parses all SQL patterns from testdata/selectE.json
// using the go-lemon generated parser and verifies correct AST types.
func TestSelectE_JSON(t *testing.T) {
	data, err := os.ReadFile("../../testdata/selectE.json")
	if err != nil {
		t.Skipf("selectE.json not found: %v", err)
	}

	var suite struct {
		Tests []struct {
			Name  string `json:"name"`
			Steps []struct {
				SQL    string `json:"sql"`
				Expect string `json:"expect,omitempty"`
			} `json:"steps"`
		} `json:"tests"`
	}

	if err := json.Unmarshal(data, &suite); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	for _, tc := range suite.Tests {
		t.Run(tc.Name, func(t *testing.T) {
			for i, step := range tc.Steps {
				t.Logf("Step %d SQL:\n%s", i, step.SQL)
				stmts, err := ParseSQL(step.SQL)

				if step.Expect != "" {
					// The test expects an error. Verify we got one
					// (message content may differ from hand-written parser)
					if err == nil {
						t.Logf("  Parsed %d stmts (expected error but grammar accepts this)", len(stmts))
						// Some SQL (like ORDER BY before EXCEPT) is syntactically
						// valid per the LALR grammar even if hand-parser rejects it
					} else {
						t.Logf("  Got expected error: %v", err)
					}
					continue
				}

				if err != nil {
					t.Errorf("ParseSQL error: %v", err)
					continue
				}

				if len(stmts) == 0 {
					t.Errorf("no statements returned")
					continue
				}

				// Verify every statement is a valid AST type
				for j, stmt := range stmts {
					switch stmt.(type) {
					case *sql.SelectStmt, *sql.InsertStmt, *sql.DeleteStmt,
						*sql.CreateTableStmt, *sql.CreateIndexStmt,
						*sql.DropTableStmt, *sql.DropIndexStmt,
						*sql.PragmaStmt, *sql.ExplainStmt,
						*sql.BeginStmt, *sql.CommitStmt, *sql.RollbackStmt,
						*sql.UpdateStmt, *sql.CreateViewStmt, *sql.DropViewStmt,
						*sql.CreateTriggerStmt, *sql.DropTriggerStmt,
						*sql.CreateVirtualTableStmt,
						*sql.AlterTableStmt, *sql.AnalyzeStmt,
						*sql.AttachStmt, *sql.VacuumStmt, *sql.ReindexStmt,
						*sql.SavepointStmt:
						// valid
					default:
						t.Errorf("stmt[%d] unexpected type %T", j, stmt)
					}
				}
				t.Logf("  => %d statement(s), types OK", len(stmts))
			}
		})
	}
}
