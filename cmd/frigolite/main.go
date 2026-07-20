// Frigolite CLI shell - interactive SQL database tool with scripting support.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lmorg/readline/v4"
	"github.com/pijalu/frigolite"
)

const version = "0.1.0"

// ---- Output formatting ----

type cliState struct {
	db          *frigolite.DB
	formatter   Formatter
	showHeaders bool
	showTimer   bool
	showStats   bool
	echoSQL     bool
	separator   string
	outputFile  *os.File
}

// cliConfig holds parsed command-line flags.
type cliConfig struct {
	dbPath      string // database file path (default :memory:)
	sqlArg      string // SQL from -e/--sql flag or positional args
	fileArg     string // SQL script from -f/--file flag
	modeName    string // output mode from --mode flag
	showHelp    bool   // -h/--help
	showVersion bool   // -v/--version
}

func main() {
	cfg := parseFlags()

	if cfg.showHelp {
		printUsage()
		os.Exit(0)
	}
	if cfg.showVersion {
		fmt.Printf("frigolite %s\n", version)
		os.Exit(0)
	}

	db, err := frigolite.Open(cfg.dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	state := &cliState{
		db:          db,
		formatter:   LookupFormatter("list"),
		showHeaders: true,
		separator:   "|",
	}

	// Apply --mode flag if given
	if cfg.modeName != "" {
		f := LookupFormatter(cfg.modeName)
		if f != nil {
			state.formatter = f
		} else {
			fmt.Fprintf(os.Stderr, "Error: unknown mode %q\n", cfg.modeName)
			os.Exit(1)
		}
	}

	switch {
	case cfg.fileArg != "":
		runScriptFile(state, cfg.fileArg)
	case cfg.sqlArg != "":
		runBatch(state, cfg.sqlArg)
	case isPipedStdin():
		runPipe(state)
	default:
		runInteractive(state)
	}
	state.flushOutput()
}

func parseFlags() cliConfig {
	var cfg cliConfig
	cfg.dbPath = ":memory:"

	fs := flag.NewFlagSet("frigolite", flag.ContinueOnError)
	fs.BoolVar(&cfg.showHelp, "help", false, "Show detailed help and exit")
	fs.BoolVar(&cfg.showHelp, "h", false, "Show detailed help and exit (shorthand)")
	fs.StringVar(&cfg.sqlArg, "sql", "", "Execute SQL statement and exit")
	fs.StringVar(&cfg.sqlArg, "e", "", "Execute SQL statement and exit (shorthand)")
	fs.StringVar(&cfg.fileArg, "file", "", "Execute SQL from file and exit")
	fs.StringVar(&cfg.fileArg, "f", "", "Execute SQL from file and exit (shorthand)")
	fs.StringVar(&cfg.modeName, "mode", "", "Output mode: list, column, csv, tabs, line, markdown, html")
	fs.BoolVar(&cfg.showVersion, "version", false, "Show version and exit")
	fs.BoolVar(&cfg.showVersion, "V", false, "Show version and exit (shorthand)")

	// Suppress default usage output; we handle it ourselves.
	fs.Usage = func() {}

	// Pre-process args: extract flags and positional args separately,
	// so flags can appear anywhere (before or after the db path).
	// Then parse the re-ordered args.
	var flagsAndArgs []string
	var positional []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "--":
			// End-of-options marker: everything after is positional.
			positional = append(positional, os.Args[i+1:]...)
			i = len(os.Args)
		case strings.HasPrefix(arg, "-"):
			// Flag: add it and its value (if any) to flagsAndArgs.
			flagsAndArgs = append(flagsAndArgs, arg)
			// Check if next arg is a value (not a flag)
			if needsValue(arg) && i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				i++
				flagsAndArgs = append(flagsAndArgs, os.Args[i])
			}
		default:
			positional = append(positional, arg)
		}
	}

	// Parse flags first.
	err := fs.Parse(flagsAndArgs)
	if err != nil {
		os.Exit(1)
	}

	// Positional arguments (backward-compat):
	//   frigolite my.db              → open my.db
	//   frigolite my.db SELECT 1     → open my.db, execute "SELECT 1"
	if len(positional) > 0 {
		cfg.dbPath = positional[0]
	}
	if len(positional) > 1 && cfg.sqlArg == "" && cfg.fileArg == "" {
		// Only use positional SQL if no -e/--file was given.
		cfg.sqlArg = strings.Join(positional[1:], " ")
	}

	return cfg
}

// needsValue returns true if flag name needs a following value argument.
func needsValue(arg string) bool {
	switch {
	case arg == "-e", arg == "--sql", arg == "-f", arg == "--file", arg == "--mode":
		return true
	case strings.HasPrefix(arg, "-e=") || strings.HasPrefix(arg, "--sql="):
		return false
	case strings.HasPrefix(arg, "-f=") || strings.HasPrefix(arg, "--file="):
		return false
	case strings.HasPrefix(arg, "--mode="):
		return false
	default:
		return false
	}
}

func printUsage() {
	fmt.Print(`Usage: frigolite [OPTIONS] [DATABASE] [SQL...]

SQLite-compatible database CLI. Connects to DATABASE (default :memory:)
and executes SQL in one of three modes:

  1. SQL from arguments (via -e or positional) → executes and exits
  2. SQL from file (via -f) → reads file, executes, exits
  3. Interactive shell (default when stdin is a terminal)

Options:
  -e, --sql STRING    Execute SQL statement and exit
  -f, --file FILE     Read and execute SQL from file, then exit
  -h, --help          Show this help and exit
  -V, --version       Show version and exit
  --mode MODE         Output mode (list|column|csv|tabs|line|markdown|html)
  --                  End of options (following arguments are positional)

Positional arguments:
  DATABASE            Path to SQLite database file (default: ':memory:')
  SQL...              SQL statement(s) to execute (only when no -e given)

Interactive dot commands:
  .tables                  List all tables
  .schema                  Show all CREATE statements
  .dump                    Dump database contents
  .databases               Show current database path
  .headers on|off          Toggle column headers (default: on)
  .mode list|column|csv|tabs|line|markdown|html  Set output mode (default: list)
  .separator STRING        Set field separator (default: |)
  .output FILE             Redirect output to file
  .import FILE TABLE       Import CSV into table
  .timer on|off            Toggle query timing
  .echo on|off             Toggle SQL echoing before execution
  .stats on|off            Toggle row count display
  .print TEXT              Print literal text
  .exit, .quit             Exit the shell
  .help                    Show this help

Examples:
  frigolite                        Open :memory: interactively
  frigolite my.db                  Open my.db interactively
  frigolite my.db -e "SELECT 1"    Execute query on my.db
  frigolite -f script.sql          Run script on :memory:
  frigolite my.db -f script.sql    Run script on my.db
  frigolite my.db SELECT 1         Positional SQL (backward compat)
  echo "SELECT 1;" | frigolite     Pipe SQL (stdin mode)
`)
}

func isPipedStdin() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func runBatch(state *cliState, sql string) {
	for _, stmt := range splitStatements(sql) {
		if strings.TrimSpace(stmt) != "" {
			state.execSQL(stmt)
		}
	}
}

// splitStatements splits SQL text into individual statements by semicolon.
func splitStatements(sql string) []string {
	var stmts []string
	var buf strings.Builder
	for _, r := range sql {
		buf.WriteRune(r)
		if r == ';' {
			stmts = append(stmts, buf.String())
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		stmts = append(stmts, buf.String())
	}
	return stmts
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

// runScriptFile reads and executes SQL from a file.
func runScriptFile(state *cliState, path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
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
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", path, err)
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

	var buf strings.Builder
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
		if !processLine(state, &buf, line) {
			break
		}
	}
	if buf.Len() > 0 {
		state.execSQL(buf.String())
	}
}

// lineAcceptor is the interface processLine needs.
// Both *cliState and test mocks implement it.
type lineAcceptor interface {
	execSQL(sql string)
	handleDotCommand(cmdLine string) bool
}

// processLine handles one line from the readline loop.
// Returns false if the caller should break the loop.
func processLine(acceptor lineAcceptor, buf *strings.Builder, line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}
	if strings.HasPrefix(line, ".") {
		if buf.Len() > 0 {
			acceptor.execSQL(buf.String())
			buf.Reset()
		}
		return acceptor.handleDotCommand(line)
	}
	// Accumulate SQL until we see a semicolon.
	// This handles multiline pasting where readline returns
	// one line per Readline() call.
	if buf.Len() > 0 {
		buf.WriteString(" ")
	}
	buf.WriteString(line)
	if strings.Contains(line, ";") {
		acceptor.execSQL(buf.String())
		buf.Reset()
	}
	return true
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
	f := LookupFormatter(args[1])
	if f != nil {
		s.formatter = f
	} else {
		names := FormatterNames()
		s.output("Error: unknown mode %q (available: %s)\n", args[1], strings.Join(names, ", "))
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

	// Query returns empty columns for DDL (CREATE TABLE etc.) and errors for
	// DML (INSERT/UPDATE/DELETE). In both cases we fall through to Exec to
	// get proper change tracking and feedback. Successful queries (SELECT)
	// have non-empty columns and are printed directly.
	if res.Error != nil || len(res.Columns) == 0 {
		s.execExec(sql, res, elapsed)
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

// execExec runs sql via Exec and prints feedback.
func (s *cliState) execExec(sql string, firstRes *frigolite.Result, elapsed time.Duration) {
	res := firstRes
	if res.Error != nil {
		res = s.db.Exec(sql)
		if res.Error != nil {
			s.output("Error: %v\n", res.Error)
			return
		}
	}
	if s.showTimer {
		s.output("Run time: %v\n", elapsed)
	}
	if s.showStats || res.Changes > 0 {
		s.output("Rows affected: %d\n", res.Changes)
	} else {
		s.output("Done\n")
	}
}

func (s *cliState) printRows(cols []string, rows [][]interface{}) {
	if len(cols) == 0 || s.formatter == nil {
		return
	}
	opts := FormatOptions{
		ShowHeaders: s.showHeaders,
		Separator:   s.separator,
	}
	out := s.formatter.Format(cols, rows, opts)
	if out != "" {
		s.output("%s", out)
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

func needsAppendMode(ctx completionCtx, prefix string, raw []string) bool {
	if ctx == ctxAny || len(raw) == 0 || prefix == "" {
		return false
	}
	for _, s := range raw {
		if strings.HasPrefix(strings.ToUpper(s), strings.ToUpper(prefix)) {
			return false
		}
	}
	return true
}

func tabCompleter(db *frigolite.DB) func([]rune, int, readline.DelayedTabContext) *readline.TabCompleterReturnT {
	return func(line []rune, pos int, _ readline.DelayedTabContext) *readline.TabCompleterReturnT {
		lineStr := string(line[:pos])
		prefix := lastWord(lineStr)
		ctx := completionContext(lineStr)
		raw := buildContextSuggestions(ctx, prefix, db)

		appendMode := needsAppendMode(ctx, prefix, raw)

		suggestions := make([]string, len(raw))
		var p string

		if appendMode {
			// Append mode: empty prefix so readline doesn't delete anything;
			// \x02 prefix + leading space so the suggestion cleanly appends
			// after the keyword (e.g. "CREATE" + " TABLE" = "CREATE TABLE").
			p = ""
			for i, s := range raw {
				suggestions[i] = "\x02 " + s
			}
		} else {
			p = prefix
			for i, s := range raw {
				upper := strings.ToUpper(s)
				if prefix != "" && strings.HasPrefix(upper, strings.ToUpper(prefix)) {
					suggestions[i] = "\x02" + s
				} else {
					suggestions[i] = s
				}
			}
		}

		return &readline.TabCompleterReturnT{
			Prefix:      p,
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
		// Return all possible items after CREATE; prefix filtering is
		// handled by the tab completer's append/replace logic.
		return []string{"TABLE", "INDEX", "VIEW", "TRIGGER"}
	case ctxDropType:
		return []string{"TABLE", "INDEX", "VIEW"}
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
