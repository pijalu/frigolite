package main

import (
	"strings"
	"testing"

	"github.com/lmorg/readline/v4"
	"github.com/pijalu/frigolite"
)

func TestLastWord(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"cre", "cre"},
		{"SELECT * FROM ", "FROM"},
		{"SELECT * FROM t WHERE ", "WHERE"},
		{"INSERT INTO ", "INTO"},
		{"CREATE ", "CREATE"},
		{"SELECT cr", "cr"},
		{"sel", "sel"},
		{"CREATE TABLE ", "TABLE"},
	}
	for _, tt := range tests {
		got := lastWord(tt.input)
		if got != tt.want {
			t.Errorf("lastWord(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCompletionContext(t *testing.T) {
	tests := []struct {
		line string
		want completionCtx
	}{
		{"", ctxAny},
		{"S", ctxAny},
		{"SELECT", ctxColumn},
		{"SELECT ", ctxColumn},
		{"SELECT *", ctxAny},
		{"SELECT * FROM", ctxTable},
		{"SELECT * FROM ", ctxTable},
		{"SELECT * FROM t", ctxTable},
		{"SELECT * FROM t WHERE", ctxColumn},
		{"SELECT * FROM t WHERE ", ctxColumn},
		{"SELECT * FROM t WHERE a", ctxColumn},
		{"CREATE", ctxCreateType},
		{"CREATE ", ctxCreateType},
		{"CREATE T", ctxAny},
		{"CREATE TABLE", ctxTable},
		{"CREATE TABLE ", ctxTable},
		{"CREATE TABLE t", ctxAny},
		{"INSERT", ctxInto},
		{"INSERT ", ctxInto},
		{"INSERT INTO", ctxTable},
		{"INSERT INTO ", ctxTable},
		{"UPDATE", ctxTable},
		{"UPDATE ", ctxTable},
		{"DELETE", ctxAny},
		{"DROP", ctxDropType},
		{"DROP ", ctxDropType},
		{"DROP TABLE", ctxTable},
		{"ORDER BY", ctxColumn},
		{"ORDER BY ", ctxColumn},
		{"GROUP BY", ctxColumn},
		{"GROUP BY ", ctxColumn},
		{"WHERE", ctxColumn},
		{"WHERE ", ctxColumn},
		{"AND", ctxColumn},
		{"OR", ctxColumn},
	}
	for _, tt := range tests {
		got := completionContext(tt.line)
		if got != tt.want {
			t.Errorf("completionContext(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestFilterPrefix(t *testing.T) {
	items := []string{"SELECT", "SET", "FROM", "WHERE", "CREATE", "TABLE"}
	tests := []struct {
		prefix string
		want   []string
	}{
		{"", items},
		{"S", []string{"SELECT", "SET"}},
		{"SE", []string{"SELECT", "SET"}},
		{"SEL", []string{"SELECT"}},
		{"CR", []string{"CREATE"}},
		{"TAB", []string{"TABLE"}},
		{"X", nil},
	}
	for _, tt := range tests {
		got := filterPrefix(items, tt.prefix)
		if !stringSliceEqual(got, tt.want) {
			t.Errorf("filterPrefix(%q) = %v, want %v", tt.prefix, got, tt.want)
		}
	}
}

func TestBuildContextSuggestions(t *testing.T) {
	// Uses an in-memory DB with no tables.
	db := openTestDB(t)
	defer db.Close()

	tests := []struct {
		name   string
		ctx    completionCtx
		prefix string
		check  func(t *testing.T, got []string)
	}{
		{
			name:   "create type returns all options unfiltered",
			ctx:    ctxCreateType,
			prefix: "CREATE",
			check: func(t *testing.T, got []string) {
				if len(got) != 4 {
					t.Fatalf("len = %d, want 4", len(got))
				}
				want := []string{"TABLE", "INDEX", "VIEW", "TRIGGER"}
				for _, w := range want {
					found := false
					for _, g := range got {
						if g == w {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("missing %q in %v", w, got)
					}
				}
			},
		},
		{
			name:   "drop type returns all options unfiltered",
			ctx:    ctxDropType,
			prefix: "DROP",
			check: func(t *testing.T, got []string) {
				if len(got) != 3 {
					t.Fatalf("len = %d, want 3", len(got))
				}
				want := []string{"TABLE", "INDEX", "VIEW"}
				for _, w := range want {
					found := false
					for _, g := range got {
						if g == w {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("missing %q in %v", w, got)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildContextSuggestions(tt.ctx, tt.prefix, db)
			tt.check(t, got)
		})
	}
}

func TestNeedsAppendMode(t *testing.T) {
	tests := []struct {
		name   string
		ctx    completionCtx
		prefix string
		raw    []string
		want   bool
	}{
		{
			name:   "ctxAny never appends",
			ctx:    ctxAny,
			prefix: "CREATE",
			raw:    []string{"TABLE", "INDEX"},
			want:   false,
		},
		{
			name:   "empty raw never appends",
			ctx:    ctxCreateType,
			prefix: "CREATE",
			raw:    nil,
			want:   false,
		},
		{
			name:   "empty prefix never appends",
			ctx:    ctxCreateType,
			prefix: "",
			raw:    []string{"TABLE", "INDEX"},
			want:   false,
		},
		{
			name:   "prefix matches suggestion no append",
			ctx:    ctxCreateType,
			prefix: "TA",
			raw:    []string{"TABLE", "INDEX", "VIEW", "TRIGGER"},
			want:   false,
		},
		{
			name:   "prefix is keyword no match -> append",
			ctx:    ctxCreateType,
			prefix: "CREATE",
			raw:    []string{"TABLE", "INDEX", "VIEW", "TRIGGER"},
			want:   true,
		},
		{
			name:   "prefix is DROP no match -> append",
			ctx:    ctxDropType,
			prefix: "DROP",
			raw:    []string{"TABLE", "INDEX", "VIEW"},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsAppendMode(tt.ctx, tt.prefix, tt.raw)
			if got != tt.want {
				t.Errorf("needsAppendMode(%v, %q, %v) = %v, want %v",
					tt.ctx, tt.prefix, tt.raw, got, tt.want)
			}
		})
	}
}

func TestTabCompleteAppendMode(t *testing.T) {
	// Verify that for "CREATE" + Tab, the tab completer returns
	// suggestions in append mode: empty prefix, \x02-space-prefixed suggestions.
	db := openTestDB(t)
	defer db.Close()

	completer := tabCompleter(db)
	line := []rune("CREATE")
	result := completer(line, len(line), readline.DelayedTabContext{})

	if result == nil {
		t.Fatal("tabCompleter returned nil")
	}

	// Should be in append mode: empty prefix
	if result.Prefix != "" {
		t.Errorf("Prefix = %q, want '' (append mode)", result.Prefix)
	}

	// Suggestions should be space-prefixed with \x02
	if len(result.Suggestions) != 4 {
		t.Fatalf("got %d suggestions, want 4", len(result.Suggestions))
	}

	expected := []string{"\x02 TABLE", "\x02 INDEX", "\x02 VIEW", "\x02 TRIGGER"}
	for i, s := range result.Suggestions {
		if s != expected[i] {
			t.Errorf("suggestion[%d] = %q, want %q", i, s, expected[i])
		}
	}
}

func TestTabCompleteReplaceMode(t *testing.T) {
	// Verify that for "CR" + Tab, the tab completer returns
	// suggestions in replace mode: prefix = "CR", \x02-prefixed suggestions.
	db := openTestDB(t)
	defer db.Close()

	completer := tabCompleter(db)
	line := []rune("CR")
	result := completer(line, len(line), readline.DelayedTabContext{})

	if result == nil {
		t.Fatal("tabCompleter returned nil")
	}

	// Should be in replace mode: prefix = "CR"
	if result.Prefix != "CR" {
		t.Errorf("Prefix = %q, want 'CR'", result.Prefix)
	}

	// Should have at least "CREATE" in suggestions with \x02 prefix
	found := false
	for _, s := range result.Suggestions {
		if s == "\x02CREATE" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf(`expected "\x02CREATE" in suggestions, got %v`, result.Suggestions)
	}
}

func TestTabCompleteSentinel(t *testing.T) {
	// Verify that suggestions matching the prefix get \x02 sentinel
	// so readline replaces the prefix word with the full suggestion.
	prefix := "cre"
	raw := []string{"CREATE", "SELECT"}
	suggestions := make([]string, len(raw))
	for i, s := range raw {
		upper := strings.ToUpper(s)
		if prefix != "" && strings.HasPrefix(upper, strings.ToUpper(prefix)) {
			suggestions[i] = "\x02" + s
		} else {
			suggestions[i] = s
		}
	}

	if !strings.HasPrefix(suggestions[0], "\x02") {
		t.Errorf("suggestion[0] should start with \\x02")
	}
	if suggestions[0] != "\x02CREATE" {
		t.Errorf("suggestion[0] should be '\\x02CREATE', got %q", suggestions[0])
	}
	if strings.HasPrefix(suggestions[1], "\x02") {
		t.Errorf("suggestion[1] should NOT have \\x02 prefix")
	}
	if suggestions[1] != "SELECT" {
		t.Errorf("suggestion[1] should be 'SELECT', got %q", suggestions[1])
	}
}

func openTestDB(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("frigolite.Open(':memory:'): %v", err)
	}
	return db
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mockAcceptor implements lineAcceptor for testing without a real cliState.
type mockAcceptor struct {
	execSQLFn     func(string)
	dotHandlerFn  func(string) bool
}

func (m *mockAcceptor) execSQL(sql string) {
	if m.execSQLFn != nil {
		m.execSQLFn(sql)
	}
}

func (m *mockAcceptor) handleDotCommand(cmd string) bool {
	if m.dotHandlerFn != nil {
		return m.dotHandlerFn(cmd)
	}
	return true
}

// TestProcessLineAccumulation verifies that multi-line SQL pasting
// accumulates lines until a semicolon is found, then executes.
func TestProcessLineAccumulation(t *testing.T) {
	var executed []string
	acceptor := &mockAcceptor{
		execSQLFn: func(sql string) { executed = append(executed, sql) },
	}
	var buf strings.Builder

	// Line 1: partial SQL, no semicolon
	if !processLine(acceptor, &buf, "CREATE TABLE contacts (") {
		t.Fatal("processLine returned false")
	}
	if len(executed) != 0 {
		t.Fatal("SQL executed before semicolon")
	}
	if buf.Len() == 0 {
		t.Fatal("buffer should not be empty")
	}

	// Line 2: partial SQL, no semicolon
	if !processLine(acceptor, &buf, "  id INTEGER PRIMARY KEY") {
		t.Fatal("processLine returned false")
	}
	if len(executed) != 0 {
		t.Fatal("SQL executed before semicolon")
	}

	// Line 3: final line WITH semicolon
	if !processLine(acceptor, &buf, ");") {
		t.Fatal("processLine returned false")
	}
	if len(executed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(executed))
	}
	expected := "CREATE TABLE contacts ( id INTEGER PRIMARY KEY );"
	if executed[0] != expected {
		t.Errorf("executed SQL = %q, want %q", executed[0], expected)
	}
	if buf.Len() != 0 {
		t.Fatal("buffer should be empty after execution")
	}
}

// TestProcessLineDotCommand verifies that dot commands flush buffer.
func TestProcessLineDotCommand(t *testing.T) {
	var executed []string
	acceptor := &mockAcceptor{
		execSQLFn: func(sql string) { executed = append(executed, sql) },
	}
	var buf strings.Builder

	processLine(acceptor, &buf, "SELECT 1")
	if len(executed) != 0 {
		t.Fatal("SQL executed before dot command")
	}

	if !processLine(acceptor, &buf, ".help") {
		t.Fatal("processLine returned false for .help")
	}
	if len(executed) != 1 {
		t.Fatalf("expected 1 execution after dot command flush, got %d", len(executed))
	}
	if executed[0] != "SELECT 1" {
		t.Errorf("flushed SQL = %q, want 'SELECT 1'", executed[0])
	}
	if buf.Len() != 0 {
		t.Fatal("buffer should be empty after dot command flush")
	}
}

// TestProcessLineEmptyLine verifies empty lines are skipped.
func TestProcessLineEmptyLine(t *testing.T) {
	var called bool
	acceptor := &mockAcceptor{
		execSQLFn: func(sql string) { called = true },
	}
	var buf strings.Builder

	if !processLine(acceptor, &buf, "  ") {
		t.Fatal("processLine returned false for empty line")
	}
	if called {
		t.Fatal("empty line should not execute")
	}
	if buf.Len() != 0 {
		t.Fatal("empty line should not add to buffer")
	}
}

// TestProcessLineSemicolonInMiddle verifies handling when a line
// contains a semicolon in the middle.
func TestProcessLineSemicolonInMiddle(t *testing.T) {
	var executed []string
	acceptor := &mockAcceptor{
		execSQLFn: func(sql string) { executed = append(executed, sql) },
	}

	var buf strings.Builder
	processLine(acceptor, &buf, "SELECT 1; SELECT 2")

	if len(executed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(executed))
	}
	if executed[0] != "SELECT 1; SELECT 2" {
		t.Errorf("executed SQL = %q, want 'SELECT 1; SELECT 2'", executed[0])
	}
	if buf.Len() != 0 {
		t.Fatal("buffer should be empty after execution")
	}
}
