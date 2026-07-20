// Frigolite CLI shell - interactive SQL database tool with scripting support.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lmorg/readline/v4"
	"github.com/pijalu/frigolite"
)

// ---- Output mode ----

type outputMode int

const (
	modeList   outputMode = iota // default: columns | values
	modeColumn                   // aligned columns
	modeCSV                      // comma-separated
	modeTabs                     // tab-separated
	modeLine                     // one value per line: col = value
)

type cliState struct {
	db         *frigolite.DB
	mode       outputMode
	showHeaders bool
	showTimer  bool
	showStats  bool
	echoSQL    bool
	separator  string
	outputFile *os.File
}

func main() {
	path, sqlFromArg := parseArgs()
	db, err := frigolite.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	state := &cliState{
		db:          db,
		mode:        modeList,
		showHeaders: true,
		separator:   "|",
	}

	switch {
	case sqlFromArg != "":
		runBatch(state, sqlFromArg)
	case isPipedStdin():
		runPipe(state)
	default:
		runInteractive(state)
	}
	state.flushOutput()
}

func parseArgs() (string, string) {
	path := ":memory:"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if len(os.Args) > 2 {
		return path, strings.Join(os.Args[2:], " ")
	}
	return path, ""
}

func isPipedStdin() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func runBatch(state *cliState, sql string) {
	state.execSQL(sql)
}

func runPipe(state *cliState) {
	scanner := bufio.NewScanner(os.Stdin)
	var buf strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), ".") {
			if buf.Len() > 0 {
				state.execSQL(buf.String())
				buf.Reset()
			}
			state.handleDotCommand(strings.TrimSpace(line))
		} else {
			buf.WriteString(line)
			buf.WriteString(" ")
			if strings.Contains(line, ";") {
				state.execSQL(buf.String())
				buf.Reset()
			}
		}
	}
	if buf.Len() > 0 {
		state.execSQL(buf.String())
	}
}

func runInteractive(state *cliState) {
	fmt.Fprintf(os.Stderr, "Frigolite CLI - connected to %s\n", state.db.Path())
	fmt.Fprintln(os.Stderr, "Enter SQL statements (.exit or Ctrl+D to quit, .help for help)")

	rl := readline.NewInstance()
	rl.SetPrompt("sql> ")
	rl.MinTabItemLength = 1
	rl.MaxTabCompleterRows = 20
	rl.TabCompleter = tabCompleter(state.db)
	rl.History = new(historyFile)

	for {
		line, err := rl.Readline()
		if err != nil {
			if errors.Is(err, readline.ErrCtrlC) || errors.Is(err, readline.ErrEOF) {
				fmt.Fprintln(os.Stderr)
				break
			}
			fmt.Fprintf(os.Stderr, "Read error: %v\n", err)
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ".") {
			if !state.handleDotCommand(line) {
				break
			}
		} else {
			state.execSQL(line)
		}
	}
}

func (s *cliState) output(format string, args ...interface{}) {
	if s.outputFile != nil {
		fmt.Fprintf(s.outputFile, format, args...)
	} else {
		fmt.Printf(format, args...)
	}
}

func (s *cliState) flushOutput() {
	if s.outputFile != nil {
		s.outputFile.Close()
		s.outputFile = nil
	}
}

func (s *cliState) handleDotCommand(cmdLine string) bool {
	args := strings.Fields(strings.ToLower(strings.TrimSpace(cmdLine)))
	if len(args) == 0 {
		return true
	}

	switch args[0] {
	case ".exit", ".quit":
		return false
	case ".help":
		s.printHelp()
	case ".tables", ".schema":
		s.handleSchemaQuery(args[0])
	case ".dump":
		s.db.DumpAll()
	case ".databases":
		s.output("database: %s\n", s.db.Path())
	case ".headers":
		s.handleHeaders(args)
	case ".mode":
		s.handleMode(args)
	case ".separator":
		s.handleSeparator(args)
	case ".output":
		s.handleOutput(args)
	case ".import":
		s.handleImport(args)
	case ".timer", ".echo", ".stats":
		s.handleToggleCmd(args)
	case ".print":
		s.output("%s\n", strings.Join(args[1:], " "))
	default:
		s.output("Error: unknown command: %s\n", args[0])
	}
	return true
}

func (s *cliState) handleSchemaQuery(cmd string) {
	switch cmd {
	case ".tables":
		s.execSQL("SELECT name FROM sqlite_schema WHERE type='table'")
	case ".schema":
		s.execSQL("SELECT sql FROM sqlite_schema")
	}
}

func (s *cliState) handleToggleCmd(args []string) {
	switch args[0] {
	case ".timer":
		s.handleToggle("timer", &s.showTimer, args)
	case ".echo":
		s.handleToggle("echo", &s.echoSQL, args)
	case ".stats":
		s.handleToggle("stats", &s.showStats, args)
	}
}

func (s *cliState) handleHeaders(args []string) {
	if len(args) > 1 {
		s.showHeaders = args[1] == "on"
	}
	s.output("headers %v\n", map[bool]string{true: "on", false: "off"}[s.showHeaders])
}

func (s *cliState) handleMode(args []string) {
	if len(args) < 2 {
		return
	}
	switch args[1] {
	case "list":
		s.mode = modeList
	case "column", "col":
		s.mode = modeColumn
	case "csv":
		s.mode = modeCSV
	case "tabs":
		s.mode = modeTabs
	case "line":
		s.mode = modeLine
	default:
		s.output("Error: unknown mode %s (list|column|csv|tabs|line)\n", args[1])
	}
}

func (s *cliState) handleSeparator(args []string) {
	if len(args) > 1 {
		s.separator = args[1]
	}
}

func (s *cliState) handleOutput(args []string) {
	if len(args) > 1 {
		f, err := os.Create(args[1])
		if err != nil {
			s.output("Error: %v\n", err)
		} else {
			s.outputFile = f
		}
	} else {
		s.flushOutput()
	}
}

func (s *cliState) handleImport(args []string) {
	if len(args) < 3 {
		s.output("Usage: .import FILE TABLE\n")
		return
	}
	s.importCSV(args[1], args[2])
}

func (s *cliState) handleToggle(name string, flag *bool, args []string) {
	if len(args) > 1 {
		*flag = args[1] == "on"
	}
	s.output("%s %v\n", name, map[bool]string{true: "on", false: "off"}[*flag])
}

func (s *cliState) execSQL(sql string) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return
	}

	if s.echoSQL {
		s.output("%s\n", sql)
	}

	start := time.Now()
	res := s.db.Query(sql)
	elapsed := time.Since(start)

	if res.Error != nil {
		res = s.db.Exec(sql)
		if res.Error != nil {
			s.output("Error: %v\n", res.Error)
			return
		}
		if s.showTimer {
			s.output("Run time: %v\n", elapsed)
		}
		if s.showStats {
			s.output("Rows affected: %d\n", res.Changes)
		} else if res.Changes > 0 {
			s.output("Rows affected: %d\n", res.Changes)
		}
		return
	}

	if s.showTimer {
		s.output("Run time: %v\n", elapsed)
	}

	s.printRows(res.Columns, res.Rows)

	if s.showStats {
		s.output("(%d rows)\n", len(res.Rows))
	}
}

func (s *cliState) printRows(cols []string, rows [][]interface{}) {
	if len(cols) == 0 {
		return
	}

	switch s.mode {
	case modeColumn:
		s.printColumnMode(cols, rows)
	case modeCSV:
		s.printCSVMode(cols, rows)
	case modeTabs:
		s.printTabsMode(cols, rows)
	case modeLine:
		s.printLineMode(cols, rows)
	default:
		s.printListMode(cols, rows)
	}
}

func (s *cliState) printListMode(cols []string, rows [][]interface{}) {
	if s.showHeaders && len(rows) > 0 {
		s.output("%s\n", strings.Join(cols, " "+s.separator+" "))
	}
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprintf("%v", v)
		}
		s.output("%s\n", strings.Join(parts, " "+s.separator+" "))
	}
}

func (s *cliState) printColumnMode(cols []string, rows [][]interface{}) {
	widths := computeColumnWidths(cols, rows)
	s.printColumnHeader(cols, widths)
	for _, row := range rows {
		s.printColumnRow(cols, row, widths)
	}
}

func computeColumnWidths(cols []string, rows [][]interface{}) []int {
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

func (s *cliState) printColumnHeader(cols []string, widths []int) {
	if !s.showHeaders {
		return
	}
	for i, c := range cols {
		if i > 0 {
			s.output("  ")
		}
		s.output("%-*s", widths[i], c)
	}
	s.output("\n")
	for i := range cols {
		if i > 0 {
			s.output("  ")
		}
		s.output("%s", strings.Repeat("-", widths[i]))
	}
	s.output("\n")
}

func (s *cliState) printColumnRow(cols []string, row []interface{}, widths []int) {
	for i, v := range row {
		if i > 0 {
			s.output("  ")
		}
		s.output("%-*s", widths[i], fmt.Sprintf("%v", v))
	}
	s.output("\n")
}

func (s *cliState) printCSVMode(cols []string, rows [][]interface{}) {
	if s.showHeaders {
		s.output("%s\n", strings.Join(cols, ","))
	}
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			str := fmt.Sprintf("%v", v)
			if strings.ContainsAny(str, ",\"\n") {
				str = `"` + strings.ReplaceAll(str, `"`, `""`) + `"`
			}
			parts[i] = str
		}
		s.output("%s\n", strings.Join(parts, ","))
	}
}

func (s *cliState) printTabsMode(cols []string, rows [][]interface{}) {
	if s.showHeaders {
		s.output("%s\n", strings.Join(cols, "\t"))
	}
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprintf("%v", v)
		}
		s.output("%s\n", strings.Join(parts, "\t"))
	}
}

func (s *cliState) printLineMode(cols []string, rows [][]interface{}) {
	for ri, row := range rows {
		if ri > 0 {
			s.output("\n")
		}
		for i, v := range row {
			s.output("%s = %v\n", cols[i], v)
		}
	}
}

func (s *cliState) importCSV(filename, table string) {
	f, err := os.Open(filename)
	if err != nil {
		s.output("Error: %v\n", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var count int
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Simple CSV parsing (no quoted fields)
		fields := strings.Split(line, ",")
		vals := make([]string, len(fields))
		for i, f := range fields {
			f = strings.TrimSpace(f)
			if f == "" || f == "NULL" {
				vals[i] = "NULL"
			} else {
				vals[i] = "'" + strings.ReplaceAll(f, "'", "''") + "'"
			}
		}
		sql := fmt.Sprintf("INSERT INTO %s VALUES (%s)", table, strings.Join(vals, ","))
		res := s.db.Exec(sql)
		if res.Error != nil {
			s.output("Error at line %d: %v\n", count+1, res.Error)
			return
		}
		count++
	}
	s.output("Imported %d rows into %s\n", count, table)
}

func (s *cliState) printHelp() {
	s.output(`
Commands:
  .tables                  List all tables
  .schema                  Show CREATE TABLE statements
  .dump                    Dump database contents
  .databases               List databases
  .headers on|off          Show column headers
  .mode list|column|csv|tabs|line  Set output mode
  .separator STRING        Set separator (default |)
  .output FILENAME         Redirect output to file
  .import FILE TABLE       Import CSV into table
  .timer on|off            Show query execution time
  .echo on|off             Echo SQL before execution
  .stats on|off            Show row count for queries
  .print TEXT              Print literal text
  .exit or .quit           Exit
  .help                    Show this help

SQL:
  Any valid SQL statement ending with ;
  Tab completion available for SQL keywords
`)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- Tab completer (same as before) ----

var sqlKeywords = []string{
	"SELECT", "FROM", "WHERE", "INSERT", "INTO", "VALUES",
	"UPDATE", "SET", "DELETE", "CREATE", "TABLE", "INDEX",
	"DROP", "ALTER", "BEGIN", "COMMIT", "ROLLBACK",
	"AND", "OR", "NOT", "NULL", "IS", "IN", "BETWEEN", "LIKE",
	"ORDER", "BY", "GROUP", "HAVING", "LIMIT", "OFFSET",
	"ASC", "DESC", "DISTINCT", "AS", "ON", "JOIN", "LEFT",
	"RIGHT", "INNER", "OUTER", "FULL", "CROSS", "NATURAL",
	"PRIMARY", "KEY", "FOREIGN", "REFERENCES", "UNIQUE",
	"CHECK", "DEFAULT", "CONSTRAINT", "INDEX", "VIEW",
	"TRIGGER", "IF", "EXISTS", "TEMP", "TEMPORARY",
	"INTEGER", "TEXT", "REAL", "BLOB", "NUMERIC",
	"BOOLEAN", "DATE", "DATETIME", "VARCHAR", "CHAR",
}

var sqlFunctions = []string{
	"COUNT", "SUM", "AVG", "MIN", "MAX",
	"UPPER", "LOWER", "LENGTH", "TRIM", "SUBSTR",
	"IFNULL", "COALESCE", "ROUND", "ABS", "TYPEOF", "TOTAL",
}

func lastWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimRight(fields[len(fields)-1], ",;()")
}

func tabCompleter(db *frigolite.DB) func([]rune, int, readline.DelayedTabContext) *readline.TabCompleterReturnT {
	return func(line []rune, pos int, _ readline.DelayedTabContext) *readline.TabCompleterReturnT {
		lineStr := string(line[:pos])
		prefix := lastWord(lineStr)
		ctx := completionContext(lineStr)
		raw := buildContextSuggestions(ctx, prefix, db)

		// Return full suggestion words for display.
		// When a suggestion matches the typed prefix, prefix it with \x02
		// so readline replaces the prefix word with the suggestion.
		suggestions := make([]string, len(raw))
		for i, s := range raw {
			upper := strings.ToUpper(s)
			if prefix != "" && strings.HasPrefix(upper, strings.ToUpper(prefix)) {
				suggestions[i] = "\x02" + s
			} else {
				suggestions[i] = s
			}
		}

		return &readline.TabCompleterReturnT{
			Prefix:      prefix,
			Suggestions: suggestions,
		}
	}
}

type completionCtx int

const (
	ctxAny           completionCtx = iota
	ctxTable
	ctxColumn
	ctxCreateType
	ctxDropType
	ctxInto
	ctxSet
	ctxValues
)

func completionContext(line string) completionCtx {
	upper := strings.ToUpper(strings.TrimSpace(line))
	tokens := strings.Fields(upper)
	if len(tokens) == 0 {
		return ctxAny
	}
	for i := len(tokens) - 1; i >= 0; i-- {
		ctx := tokenToContext(tokens, i)
		if ctx != ctxAny || isKnownKeyword(tokens[i]) {
			return ctx
		}
	}
	return ctxAny
}

func isKnownKeyword(s string) bool {
	switch s {
	case "FROM", "JOIN", "INTO", "CREATE", "DROP", "TABLE", "INDEX",
		"VIEW", "TRIGGER", "SET", "WHERE", "AND", "OR", "ON", "SELECT",
		"ORDER", "GROUP", "BY", "VALUES", "UPDATE", "DELETE", "INSERT",
		"ALTER", "PRIMARY", "KEY", "NOT", "NULL", "DEFAULT", "UNIQUE",
		"CHECK", "FOREIGN", "REFERENCES", "BETWEEN", "LIKE", "IN", "IS",
		"ASC", "DESC", "DISTINCT", "AS", "LIMIT", "OFFSET", "HAVING":
		return true
	}
	return false
}

func tokenToContext(tokens []string, i int) completionCtx {
	switch tokens[i] {
	case "FROM", "JOIN", "INTO":
		return ctxTable
	case "CREATE":
		return lastTokenCtx(i, len(tokens), ctxCreateType)
	case "DROP":
		return lastTokenCtx(i, len(tokens), ctxDropType)
	case "TABLE", "INDEX", "VIEW", "TRIGGER":
		if i > 0 && isCreateOrDrop(tokens[i-1]) {
			return lastTokenCtx(i, len(tokens), ctxTable)
		}
	case "SET", "SELECT", "ORDER", "GROUP":
		return lastTokenCtx(i, len(tokens), ctxColumn)
	case "WHERE", "AND", "OR", "ON":
		return ctxColumn
	case "BY":
		if i > 0 && isOrderOrGroup(tokens[i-1]) {
			return ctxColumn
		}
	case "VALUES":
		return ctxValues
	case "UPDATE":
		return lastTokenCtx(i, len(tokens), ctxTable)
	case "INSERT":
		return lastTokenCtx(i, len(tokens), ctxInto)
	}
	return ctxAny
}

func isCreateOrDrop(s string) bool { return s == "CREATE" || s == "DROP" }
func isOrderOrGroup(s string) bool { return s == "ORDER" || s == "GROUP" }

func lastTokenCtx(i, n int, ctx completionCtx) completionCtx {
	if i == n-1 {
		return ctx
	}
	return ctxAny
}

func buildContextSuggestions(ctx completionCtx, prefix string, db *frigolite.DB) []string {
	tables := getTableNames(db)
	columns := getColumnNames(db)
	upper := strings.ToUpper(prefix)

	switch ctx {
	case ctxTable:
		return filterPrefix(tables, upper)
	case ctxColumn:
		all := append([]string{}, columns...)
		all = append(all, "*")
		all = append(all, sqlFunctions...)
		return filterPrefix(all, upper)
	case ctxCreateType:
		return filterPrefix([]string{"TABLE", "INDEX", "VIEW", "TRIGGER"}, upper)
	case ctxDropType:
		return filterPrefix([]string{"TABLE", "INDEX", "VIEW"}, upper)
	case ctxInto:
		if len(prefix) == 0 || strings.HasPrefix("INTO", upper) {
			return []string{"INTO"}
		}
		return nil
	case ctxSet:
		return filterPrefix(columns, upper)
	case ctxValues:
		return nil
	default:
		all := append([]string{}, sqlKeywords...)
		all = append(all, tables...)
		all = append(all, columns...)
		return filterPrefix(all, upper)
	}
}

func filterPrefix(items []string, upperPrefix string) []string {
	if upperPrefix == "" {
		return items
	}
	var result []string
	for _, item := range items {
		if strings.HasPrefix(strings.ToUpper(item), upperPrefix) {
			result = append(result, item)
		}
	}
	return result
}

func getTableNames(db *frigolite.DB) []string {
	res := db.Query("SELECT name FROM sqlite_schema WHERE type='table'")
	if res.Error != nil {
		return nil
	}
	var tables []string
	for _, row := range res.Rows {
		tables = append(tables, fmt.Sprintf("%v", row[0]))
	}
	return tables
}

func getColumnNames(db *frigolite.DB) []string {
	res := db.Query("SELECT name FROM sqlite_schema WHERE type='table'")
	if res.Error != nil {
		return nil
	}
	var cols []string
	for _, row := range res.Rows {
		tableName := fmt.Sprintf("%v", row[0])
		cols = append(cols, tableName+".*")
		colRes := db.Query("PRAGMA table_info(" + tableName + ")")
		if colRes.Error == nil {
			for _, r := range colRes.Rows {
				if len(r) > 1 {
					cols = append(cols, fmt.Sprintf("%v", r[1]))
				}
			}
		}
	}
	return cols
}

// ---- History ----

type historyFile struct {
	items []string
}

func (h *historyFile) Write(line string) (int, error) {
	h.items = append(h.items, line)
	return len(h.items), nil
}

func (h *historyFile) GetLine(pos int) (string, error) {
	if pos < 0 || pos >= len(h.items) {
		return "", fmt.Errorf("history: position %d out of range", pos)
	}
	return h.items[pos], nil
}

func (h *historyFile) Len() int {
	return len(h.items)
}

func (h *historyFile) Dump() any {
	return h.items
}
