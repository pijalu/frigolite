package main

import (
	"fmt"
	"strings"
)

// FormatOptions carries rendering parameters for formatters.
type FormatOptions struct {
	ShowHeaders bool
	Separator   string
}

// Formatter is the interface for rendering query results.
// Implementations must be safe for concurrent use (no shared state).
// SOLID: Open for extension (add new formatters by implementing this interface),
// Closed for modification (no switch/if-else chains).
type Formatter interface {
	// Name returns the canonical name (e.g. "list", "csv", "markdown").
	Name() string

	// Aliases returns alternative names (e.g. "col" for "column").
	Aliases() []string

	// Format renders the result as a string. Called once per query.
	Format(cols []string, rows [][]interface{}, opts FormatOptions) string
}

// ---- Registry ----

var formatters = make(map[string]Formatter)

func registerFormatter(f Formatter) {
	formatters[f.Name()] = f
	for _, a := range f.Aliases() {
		formatters[a] = f
	}
}

// LookupFormatter returns the formatter registered under name (or alias).
// Returns nil when not found.
func LookupFormatter(name string) Formatter {
	return formatters[strings.ToLower(name)]
}

// FormatterNames returns the canonical names of all registered formatters.
func FormatterNames() []string {
	var names []string
	seen := make(map[string]bool)
	for _, f := range formatters {
		n := f.Name()
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	return names
}

// ---- Built-in formatters ----

// ---- list ----

type listFormatter struct{}

func (listFormatter) Name() string               { return "list" }
func (listFormatter) Aliases() []string           { return nil }
func (listFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	var b strings.Builder
	sep := " " + opts.Separator + " "
	if opts.ShowHeaders && len(rows) > 0 {
		b.WriteString(strings.Join(cols, sep))
		b.WriteString("\n")
	}
	for _, row := range rows {
		for i, v := range row {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(fmt.Sprintf("%v", v))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ---- csv ----

type csvFormatter struct{}

func (csvFormatter) Name() string               { return "csv" }
func (csvFormatter) Aliases() []string           { return nil }
func (csvFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	var b strings.Builder
	if opts.ShowHeaders {
		b.WriteString(escapeCSV(cols))
		b.WriteString("\n")
	}
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = escapeCSVValue(fmt.Sprintf("%v", v))
		}
		b.WriteString(strings.Join(parts, ","))
		b.WriteString("\n")
	}
	return b.String()
}

func escapeCSV(fields []string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = escapeCSVValue(f)
	}
	return strings.Join(parts, ",")
}

func escapeCSVValue(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// ---- tabs ----

type tabsFormatter struct{}

func (tabsFormatter) Name() string               { return "tabs" }
func (tabsFormatter) Aliases() []string           { return nil }
func (tabsFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	var b strings.Builder
	if opts.ShowHeaders {
		b.WriteString(strings.Join(cols, "\t"))
		b.WriteString("\n")
	}
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprintf("%v", v)
		}
		b.WriteString(strings.Join(parts, "\t"))
		b.WriteString("\n")
	}
	return b.String()
}

// ---- line ----

type lineFormatter struct{}

func (lineFormatter) Name() string               { return "line" }
func (lineFormatter) Aliases() []string           { return nil }
func (lineFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	var b strings.Builder
	for ri, row := range rows {
		if ri > 0 {
			b.WriteString("\n")
		}
		for i, v := range row {
			b.WriteString(cols[i])
			b.WriteString(" = ")
			b.WriteString(fmt.Sprintf("%v", v))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---- column (aligned) ----

type columnFormatter struct{}

func (columnFormatter) Name() string               { return "column" }
func (columnFormatter) Aliases() []string           { return []string{"col"} }
func (columnFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	widths := columnWidths(cols, rows)
	var b strings.Builder
	if opts.ShowHeaders {
		columnFormatHeader(&b, cols, widths)
	}
	for _, row := range rows {
		columnFormatRow(&b, row, widths)
	}
	return b.String()
}

func columnFormatHeader(b *strings.Builder, cols []string, widths []int) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(fmt.Sprintf("%-*s", widths[i], c))
	}
	b.WriteString("\n")
	for i := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(strings.Repeat("-", widths[i]))
	}
	b.WriteString("\n")
}

func columnFormatRow(b *strings.Builder, row []interface{}, widths []int) {
	for i, v := range row {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(fmt.Sprintf("%-*s", widths[i], fmt.Sprintf("%v", v)))
	}
	b.WriteString("\n")
}

func columnWidths(cols []string, rows [][]interface{}) []int {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	for _, row := range rows {
		for i, v := range row {
			w := len(fmt.Sprintf("%v", v))
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// ---- markdown ----

type markdownFormatter struct{}

func (markdownFormatter) Name() string               { return "markdown" }
func (markdownFormatter) Aliases() []string           { return []string{"md"} }
func (markdownFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	var b strings.Builder
	if len(cols) == 0 {
		return ""
	}

	widths := columnWidths(cols, rows)

	// Header row
	b.WriteString("|")
	for i, c := range cols {
		b.WriteString(" ")
		b.WriteString(fmt.Sprintf("%-*s", widths[i], c))
		b.WriteString(" |")
	}
	b.WriteString("\n")

	// Separator row
	b.WriteString("|")
	for _, w := range widths {
		b.WriteString(" ")
		b.WriteString(strings.Repeat("-", w))
		b.WriteString(" |")
	}
	b.WriteString("\n")

	// Data rows
	for _, row := range rows {
		b.WriteString("|")
		for i, v := range row {
			b.WriteString(" ")
			b.WriteString(fmt.Sprintf("%-*s", widths[i], fmt.Sprintf("%v", v)))
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}

	return b.String()
}

// ---- html ----

type htmlFormatter struct{}

func (htmlFormatter) Name() string               { return "html" }
func (htmlFormatter) Aliases() []string           { return nil }
func (htmlFormatter) Format(cols []string, rows [][]interface{}, opts FormatOptions) string {
	var b strings.Builder
	b.WriteString("<table>\n")

	// Header
	if opts.ShowHeaders {
		b.WriteString("  <thead>\n    <tr>")
		for _, c := range cols {
			b.WriteString("<th>")
			b.WriteString(htmlEscape(c))
			b.WriteString("</th>")
		}
		b.WriteString("</tr>\n  </thead>\n")
	}

	// Body
	b.WriteString("  <tbody>\n")
	for _, row := range rows {
		b.WriteString("    <tr>")
		for _, v := range row {
			b.WriteString("<td>")
			b.WriteString(htmlEscape(fmt.Sprintf("%v", v)))
			b.WriteString("</td>")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("  </tbody>\n")
	b.WriteString("</table>\n")

	return b.String()
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}

// ---- init ----

func init() {
	registerFormatter(listFormatter{})
	registerFormatter(csvFormatter{})
	registerFormatter(tabsFormatter{})
	registerFormatter(lineFormatter{})
	registerFormatter(columnFormatter{})
	registerFormatter(markdownFormatter{})
	registerFormatter(htmlFormatter{})
}
