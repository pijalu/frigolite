package vtab

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CSVModule implements the csv virtual table (ext/misc/csv.c): a read-only
// table whose rows come from inline CSV data or a CSV file. Arguments are
// key=value pairs:
//
//	data=<csv text>            inline CSV content
//	filename=<path>            CSV file to read
//	columns=N                  column count when no header/schema (c0..cN-1)
//	header[=bool]              first row names the columns
//	schema=<CREATE TABLE ...>  explicit column names/types
//
// Columns without an explicit schema are declared TEXT (csv.c appends
// " TEXT" to every generated column), so comparisons apply TEXT affinity.
type CSVModule struct{}

// csvVTab is one instance holding all parsed rows in memory.
type csvVTab struct {
	columns      []string
	types        []string
	rows         [][]interface{}
	withoutRowid bool
}

// WithoutRowid reports whether the schema= argument declared the table
// WITHOUT ROWID (such tables reject rowid references).
func (v *csvVTab) WithoutRowid() bool { return v.withoutRowid }

// Create implements Module.
func (m *CSVModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module.
func (m *CSVModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

func (m *CSVModule) connect(args []string) (VirtualTable, error) {
	params := csvParseArgs(args)
	data := params["data"]
	filename := params["filename"]
	if data != "" && filename != "" {
		return nil, fmt.Errorf("csv: must specify either filename= or data= but not both")
	}
	records, err := csvLoadRecords(data, filename)
	if err != nil {
		return nil, err
	}
	v, records, err := csvBuildColumns(params, records)
	if err != nil {
		return nil, err
	}
	csvAppendRows(v, records)
	return v, nil
}

// csvParseArgs splits the module argv into key=value parameters and bare
// flags (csv.c's parameter pass); SQL quoted values are dequoted one level.
func csvParseArgs(args []string) map[string]string {
	params := map[string]string{}
	flags := map[string]bool{}
	for _, a := range args {
		eq := strings.Index(a, "=")
		if eq < 0 {
			key := strings.ToLower(strings.TrimSpace(a))
			if key != "" {
				flags[key] = true
			}
			continue
		}
		key := strings.ToLower(strings.TrimSpace(a[:eq]))
		params[key] = csvDequoteValue(strings.TrimSpace(a[eq+1:]))
	}
	for k := range flags {
		if _, isParam := params[k]; !isParam {
			params[k] = "true" // bare flag: header, testflags w/o value, ...
		}
	}
	return params
}

// csvDequoteValue strips one level of SQL string-literal quoting (the quotes
// survive in the verbatim argv); ” inside '...' escapes.
func csvDequoteValue(val string) string {
	if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
		return strings.ReplaceAll(val[1:len(val)-1], "''", "'")
	}
	if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '`' && val[len(val)-1] == '`')) {
		return val[1 : len(val)-1]
	}
	return val
}

// csvLoadRecords reads the CSV records from data= or filename=
// (csv.c's fopen/Reader wiring).
func csvLoadRecords(data, filename string) ([][]string, error) {
	if data != "" {
		recs, err := parseCSVString(data)
		if err != nil {
			return nil, fmt.Errorf("csv: %w", err)
		}
		return recs, nil
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot open '%s' for reading", filename)
	}
	recs, perr := parseCSVString(string(content))
	if perr != nil {
		return nil, fmt.Errorf("csv: %w", perr)
	}
	return recs, nil
}

// csvBuildColumns derives the declared column list from the schema=, header
// or default c0..cN-1 naming (csv.c's schema pass); a header consumes the
// first record.
func csvBuildColumns(params map[string]string, records [][]string) (*csvVTab, [][]string, error) {
	header := false
	if hv, hvOK := params["header"]; hvOK {
		header = parseBoolParam(hv)
	}
	nCol, err := csvParseColumnCount(params)
	if err != nil {
		return nil, nil, err
	}
	v := &csvVTab{}
	switch {
	case params["schema"] != "":
		if serr := csvApplySchema(v, params["schema"]); serr != nil {
			return nil, nil, serr
		}
	case header:
		if len(records) == 0 {
			return nil, nil, fmt.Errorf("csv: empty input with header")
		}
		v.columns = quotedNames(records[0])
		v.types = textTypes(len(v.columns))
		records = records[1:]
	default:
		v.columns = csvDefaultNames(nCol, records)
		v.types = textTypes(len(v.columns))
	}
	return v, records, nil
}

// csvParseColumnCount resolves columns=N with csv.c's limit checks; -1 when
// absent.
func csvParseColumnCount(params map[string]string) (int, error) {
	v, ok := params["columns"]
	if !ok {
		return -1, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("csv: invalid columns=%q", v)
	}
	if n == 0 {
		return 0, fmt.Errorf("column= value must be positive")
	}
	// SQLite's default SQLITE_LIMIT_COLUMN is 2000 (csv.c column= check).
	if n > 2000 {
		return 0, fmt.Errorf("column= value too big, max %d", 2000)
	}
	return n, nil
}

// textTypes renders n declared "TEXT" types (csv.c appends " TEXT" to every
// generated column).
func textTypes(n int) []string {
	types := make([]string, n)
	for i := range types {
		types[i] = "TEXT"
	}
	return types
}

// csvApplySchema applies a declared schema and its WITHOUT ROWID marker.
// The schema argument is passed to sqlite3_declare_vtab verbatim (csv.c:
// only GENERATED declarations append " TEXT"), so the declared column
// types — not an all-TEXT assumption — drive comparison affinities.
func csvApplySchema(v *csvVTab, schema string) error {
	names, types, err := columnDefsFromSchema(schema)
	if err != nil {
		return err
	}
	v.columns = names
	v.types = types
	if strings.Contains(strings.ToUpper(schema), "WITHOUT ROWID") {
		v.withoutRowid = true
		if verr := validateWithoutRowidSchema(schema); verr != nil {
			return verr
		}
	}
	return nil
}

// csvDefaultNames renders the c0..cN-1 default column names; N falls back to
// the first record's width.
func csvDefaultNames(nCol int, records [][]string) []string {
	count := nCol
	if count < 0 && len(records) > 0 {
		count = len(records[0])
	}
	if count < 0 {
		count = 0
	}
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("c%d", i)
	}
	return names
}

// csvAppendRows normalizes row widths to the column count (missing trailing
// fields become NULL; extras are dropped) like csv.c's field accounting.
// Fields are kept as TEXT verbatim: csv.c yields every field via
// sqlite3_result_text (csvtabColumn), so numeric-looking predicates are
// answered by the DECLARED column affinity applied by the core
// (csv01-2.3: d BLOB holds '12' and d=12 matches nothing, while the
// TEXT-affinity default columns match numerics — affinity conversion —
// exactly as SQLite does).
func csvAppendRows(v *csvVTab, records [][]string) {
	for _, r := range records {
		row := make([]interface{}, len(v.columns))
		for i := 0; i < len(v.columns); i++ {
			if i < len(r) {
				row[i] = r[i]
			}
		}
		v.rows = append(v.rows, row)
	}
}

// parseBoolParam interprets header=true/false/1/0/yes/no and bare presence.
func parseBoolParam(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true", "1", "yes":
		return true
	}
	return false
}

// validateWithoutRowidSchema mirrors SQLite's WITHOUT ROWID requirements for
// a declared csv schema: the table must have a PRIMARY KEY, and a table-level
// PRIMARY KEY must name exactly one column (csv.c wraps declare_vtab failures
// as "bad schema: '<schema>' - <reason>").
func validateWithoutRowidSchema(schema string) error {
	upper := strings.ToUpper(schema)
	open := strings.Index(schema, "(")
	closeIdx := strings.LastIndex(schema, ")")
	if open < 0 || closeIdx <= open {
		return fmt.Errorf("bad schema: '%s' - not an error", schema)
	}
	body := schema[open+1 : closeIdx]
	name := ""
	if fields := strings.Fields(schema[:open]); len(fields) >= 3 {
		name = fields[2]
	}
	if !strings.Contains(upper, "PRIMARY KEY") {
		return fmt.Errorf("bad schema: '%s' - PRIMARY KEY missing on table %s", schema, name)
	}
	// A table-level PRIMARY KEY listing more than one column cannot serve as
	// the WITHOUT ROWID key here; declare_vtab fails with an empty message.
	if m := strings.ToUpper(body); strings.Contains(m, "PRIMARY KEY(") {
		pkStart := strings.Index(m, "PRIMARY KEY(") + len("PRIMARY KEY(")
		mBody := body[pkStart:]
		pkEnd := strings.Index(mBody, ")")
		if len(strings.Split(strings.TrimSpace(mBody[:pkEnd]), ",")) > 1 {
			return fmt.Errorf("bad schema: '%s' - not an error", schema)
		}
	}
	return nil
}

// parseCSVString parses inline CSV text (RFC-4180 quoting via encoding/csv).
func parseCSVString(data string) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(data))
	r.FieldsPerRecord = -1 // variable width; normalized later
	return r.ReadAll()
}

// quotedNames renders header names as quoted identifiers stripped of quotes.
func quotedNames(header []string) []string {
	out := make([]string, len(header))
	for i, h := range header {
		out[i] = strings.Trim(h, `"`)
	}
	return out
}

// constraintKeyword reports whether a term's leading keyword is a table-level
// constraint rather than a column.
func constraintKeyword(name string) bool {
	switch name {
	case "WITHOUT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
		return true
	}
	return false
}

// splitTopLevel splits body on top-level commas (paren depth 0).
func splitTopLevel(body string) []string {
	var parts []string
	depth := 0
	cur := strings.Builder{}
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(body[i])
	}
	return append(parts, cur.String())
}

// columnNamesFromSchema extracts column names from a "CREATE TABLE x(a,b,c)"
// style schema argument: the identifier before the first space of each
// top-level comma-separated term inside the outermost parentheses.
func columnNamesFromSchema(schema string) ([]string, error) {
	names, _, err := columnDefsFromSchema(schema)
	return names, err
}

// columnDefsFromSchema extracts column names and declared types from a
// "CREATE TABLE x(a INT, b TEXT)" style schema argument: the identifier
// before the first space of each top-level comma-separated term inside the
// outermost parentheses, with the remaining fields as the declared type
// ("" for a bare column — build.c sqlite3AddColumn gives an empty type
// BLOB affinity).
func columnDefsFromSchema(schema string) ([]string, []string, error) {
	open := strings.Index(schema, "(")
	close := strings.LastIndex(schema, ")")
	if open < 0 || close <= open {
		return nil, nil, fmt.Errorf("csv: invalid schema=%q", schema)
	}
	// Strip a trailing WITHOUT ROWID clause if the paren scan caught it.
	var names, types []string
	for _, p := range splitTopLevel(schema[open+1 : close]) {
		f := strings.Fields(strings.TrimSpace(p))
		if len(f) == 0 {
			continue
		}
		if constraintKeyword(strings.ToUpper(f[0])) {
			continue // table-level constraint tail, not a column
		}
		names = append(names, strings.Trim(f[0], `"`+"`"))
		types = append(types, strings.Join(f[1:], " "))
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("csv: schema=%q declares no columns", schema)
	}
	return names, types, nil
}

// Columns implements ColumnInfo.
func (v *csvVTab) Columns() []string { return v.columns }

// ColumnTypes implements ColumnTypeInfo: the schema= declaration's types
// (sqlite3_declare_vtab parity), or "TEXT" for generated columns — csv.c
// appends " TEXT" to every generated column, so comparisons apply TEXT
// affinity — WHERE c1=10 matches the stored '10' (csv01-1.0).
func (v *csvVTab) ColumnTypes() []string {
	if len(v.types) == len(v.columns) {
		return v.types
	}
	return textTypes(len(v.columns))
}

// BestIndex accepts the default full-scan plan; WHERE filtering happens at
// run time over the materialized rows.
func (v *csvVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open returns a cursor over the parsed rows.
func (v *csvVTab) Open() (Cursor, error) {
	return &csvCursor{rows: v.rows}, nil
}

// csvCursor walks the materialized rows.
type csvCursor struct {
	rows    [][]interface{}
	idx     int
	started bool
	done    bool
}

// Next advances; the first call serves row 0 (materializer convention).
func (c *csvCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return len(c.rows) > 0
	}
	c.idx++
	if c.idx >= len(c.rows) {
		c.done = true
		return false
	}
	return true
}

// Column returns field idx as TEXT (nil past the stored width = NULL).
// An out-of-range index errors so the materializer's column loop terminates.
func (c *csvCursor) Column(idx int) (interface{}, error) {
	if c.idx >= len(c.rows) || idx >= len(c.rows[c.idx]) {
		return nil, fmt.Errorf("csv: invalid column index %d", idx)
	}
	return c.rows[c.idx][idx], nil
}

// Close implements Cursor.
func (c *csvCursor) Close() error { return nil }
