package frigolite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type TestStep struct {
	Type   string `json:"type"`
	SQL    string `json:"sql,omitempty"`
	Expect string `json:"expect,omitempty"`
}

type TestCase struct {
	Name  string     `json:"name"`
	Steps []TestStep `json:"steps"`
}

type TestFileData struct {
	File  string     `json:"file"`
	Name  string     `json:"name"`
	Tests []TestCase `json:"tests"`
}

func TestSQLiteSuite(t *testing.T) {
	pattern := os.Getenv("FRIGOLITE_TEST")
	files, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatalf("failed to list test data: %v", err)
	}
	if len(files) == 0 {
		t.Skip("no test data files found (run: python3 tools/convert_compat_json.py)")
		return
	}
	for _, fpath := range files {
		fpath := fpath
		base := strings.TrimSuffix(filepath.Base(fpath), ".json")
		if pattern != "" && !strings.Contains(base, pattern) {
			continue
		}
		t.Run(base, func(t *testing.T) {
			data, err := os.ReadFile(fpath)
			if err != nil {
				t.Fatalf("read %s: %v", fpath, err)
			}
			var td TestFileData
			if err := json.Unmarshal(data, &td); err != nil {
				t.Fatalf("parse %s: %v", fpath, err)
			}
			db := setupDB(t)
			defer db.Close()
			for _, tc := range td.Tests {
				if tc.Name == "__RESET_DB__" {
					db.Close()
					db = setupDB(t)
					continue
				}
				tc := tc
				t.Run(tc.Name, func(t *testing.T) {
					for _, step := range tc.Steps {
						switch step.Type {
						case "exec":
							res := db.Exec(step.SQL)
							if step.Expect != "" {
								expect := cleanExpected(step.Expect)
								if strings.HasPrefix(expect, "1 ") || expect == "1" {
									// catchsql: error expected
									if res.Error == nil {
										t.Errorf("expected error but got success\n  sql: %s", step.SQL)
										return
									}
									parts := splitExpect(expect)
									if len(parts) >= 2 && !strings.Contains(res.Error.Error(), parts[1]) {
										t.Errorf("error mismatch\n  got:  %v\n  want: %s\n  sql: %s", res.Error, parts[1], step.SQL)
										return
									}
								} else if strings.HasPrefix(expect, "0 ") || expect == "0" {
									if res.Error != nil {
										t.Errorf("exec error: %v\n  sql: %s", res.Error, step.SQL)
										return
									}
								} else if res.Error != nil {
									t.Errorf("exec error: %v\n  sql: %s", res.Error, step.SQL)
									return
								}
							} else if res.Error != nil {
								t.Errorf("exec error: %v\n  sql: %s", res.Error, step.SQL)
								return
							}
						case "query":
							res := db.Query(step.SQL)
							if res.Error != nil {
								t.Errorf("query error: %v\n  sql: %s", res.Error, step.SQL)
								return
							}
							if step.Expect != "" {
								got := flattenResult(res)
								want := cleanExpected(step.Expect)
								if got != want {
									t.Errorf("result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
								}
							}
						}
					}
				})
			}
		})
	}
}

func flattenResult(res *Result) string {
	var parts []string
	for _, row := range res.Rows {
		for _, val := range row {
			if val == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, fmt.Sprintf("%v", val))
			}
		}
	}
	return strings.Join(parts, " ")
}

func cleanExpected(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "{}")
	return strings.TrimSpace(s)
}

func splitExpect(expect string) []string {
	expect = strings.TrimSpace(expect)
	parts := strings.SplitN(expect, " ", 2)
	for i, p := range parts {
		parts[i] = strings.Trim(p, "{}")
	}
	return parts
}
