package frigolite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
										// Check for regex patterns wrapped in /.../
										if strings.HasPrefix(want, "/") && strings.HasSuffix(want, "/") && len(want) > 2 {
											pattern := want[1 : len(want)-1]
											matched, err := regexp.MatchString(pattern, got)
											if err != nil || !matched {
												t.Errorf("result mismatch\n  got:  [%s]\n  want pattern: [%s]\n  sql: %s", got, pattern, step.SQL)
											}
										} else if got != want {
											// Normalize for cosmetic comparison: collapse whitespace, strip redundant parens
											if normalizeSQL(got) != normalizeSQL(want) {
												t.Errorf("result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
											}
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
				parts = append(parts, "")
			} else {
				parts = append(parts, formatSQLiteValue(val))
			}
		}
	}
	return strings.Join(parts, " ")
}

func cleanExpected(s string) string {
	s = strings.TrimSpace(s)
	// Check if the entire string is wrapped in a single pair of braces
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		depth := 0
		fullyBraced := true
		for i, ch := range s {
			switch ch {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 && i < len(s)-1 {
					fullyBraced = false
				}
			}
			if depth < 0 {
				break
			}
		}
		if fullyBraced && depth == 0 {
			s = s[1 : len(s)-1]
			return strings.TrimSpace(s)
		}
	}
	// Handle TCL lists with braced elements: {a} {b} {c} or 1 {error message}
	// Split by top-level whitespace, extracting braced content and preserving unbraced tokens
	var parts []string
	depth := 0
	tokenStart := 0
	for i, ch := range s {
		switch ch {
		case '{':
			if depth == 0 {
				// Add any unbraced text before this brace
				if tokenStart < i {
					token := strings.TrimSpace(s[tokenStart:i])
					if token != "" {
						parts = append(parts, token)
					}
				}
				tokenStart = i + 1
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				if tokenStart <= i {
					token := strings.TrimSpace(s[tokenStart:i])
					if token != "" {
						parts = append(parts, token)
					} else {
						// TCL {} represents an empty value (maps to SQL NULL)
						parts = append(parts, "NULL")
					}
				}
				tokenStart = i + 1
			}
		case ' ', '\n', '\r', '\t':
			if depth == 0 {
				if tokenStart < i {
					token := strings.TrimSpace(s[tokenStart:i])
					if token != "" {
						parts = append(parts, token)
					}
				}
				tokenStart = i + 1
			}
		}
	}
	// Handle remaining text after last brace
	if tokenStart < len(s) {
		token := strings.TrimSpace(s[tokenStart:])
		if token != "" && token != "}" {
			parts = append(parts, token)
		}
	}
	if len(parts) > 0 {
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return s
}

func splitExpect(expect string) []string {
	expect = strings.TrimSpace(expect)
	parts := strings.SplitN(expect, " ", 2)
	for i, p := range parts {
		parts[i] = strings.Trim(p, "{}")
	}
	return parts
}

// normalizeSQL normalizes SQL text for cosmetic comparison by collapsing whitespace
// and stripping certain formatting differences that don't affect semantics.
func normalizeSQL(s string) string {
	// Collapse all whitespace sequences to single spaces
	re := regexp.MustCompile(`\s+`)
	normalized := re.ReplaceAllString(s, " ")
	// Remove space before ( in CREATE TABLE/INDEX
	normalized = strings.ReplaceAll(normalized, "TABLE (", "TABLE(")
	normalized = strings.ReplaceAll(normalized, "TABLE  (", "TABLE(")
	// Normalize space around common operators
	normalized = strings.ReplaceAll(normalized, " = ", "=")
	normalized = strings.ReplaceAll(normalized, " > ", ">")
	normalized = strings.ReplaceAll(normalized, " >=", ">=")
	normalized = strings.ReplaceAll(normalized, " <=", "<=")
	normalized = strings.ReplaceAll(normalized, " <>", "<>")
	normalized = strings.ReplaceAll(normalized, " ,", ",")
	// Remove space after ( before non-space
	normalized = strings.ReplaceAll(normalized, "( ", "(")
	// Remove space before ) 
	normalized = strings.ReplaceAll(normalized, " )", ")")
	return strings.TrimSpace(normalized)
}
