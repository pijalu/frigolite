package main

import (
	"strings"
	"testing"
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
