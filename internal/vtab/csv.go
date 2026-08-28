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
		val := strings.TrimSpace(a[eq+1:])
		// SQL string-literal values keep their quotes in the verbatim argv:
		// strip one level and undo '' escaping.
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			val = strings.ReplaceAll(val[1:len(val)-1], "''", "'")
		} else if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '`' && val[len(val)-1] == '`')) {
			val = val[1 : len(val)-1]
		}
		params[key] = val
	}
	for k := range flags {
		if _, isParam := params[k]; !isParam {
			params[k] = "true" // bare flag: header, testflags w/o value, ...
		}
	}

	data := params["data"]
	filename := params["filename"]
	if data != "" && filename != "" {
		return nil, fmt.Errorf("csv: must specify either filename= or data= but not both")
	}

	var records [][]string
	if data != "" {
		recs, err := parseCSVString(data)
		if err != nil {
			return nil, fmt.Errorf("csv: %w", err)
		}
		records = recs
	} else {
		content, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("cannot open '%s' for reading", filename)
		}
		recs, perr := parseCSVString(string(content))
		if perr != nil {
			return nil, fmt.Errorf("csv: %w", perr)
		}
		records = recs
	}

	header := false
	if hv, hvOK := params["header"]; hvOK {
		header = parseBoolParam(hv)
	}
	nCol := -1
	if v, ok := params["columns"]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("csv: invalid columns=%q", v)
		}
		if n == 0 {
			return nil, fmt.Errorf("column= value must be positive")
		}
		// SQLite's default SQLITE_LIMIT_COLUMN is 2000 (csv.c column= check).
		if n > 2000 {
			return nil, fmt.Errorf("column= value too big, max %d", 2000)
		}
		nCol = n
	}

	v := &csvVTab{}
	if params["schema"] != "" {
		names, err := columnNamesFromSchema(params["schema"])
		if err != nil {
			return nil, err
		}
		v.columns = names
		if strings.Contains(strings.ToUpper(params["schema"]), "WITHOUT ROWID") {
			v.withoutRowid = true
			if verr := validateWithoutRowidSchema(params["schema"]); verr != nil {
				return nil, verr
			}
		}
	} else if header {
		if len(records) == 0 {
			return nil, fmt.Errorf("csv: empty input with header")
		}
		v.columns = quotedNames(records[0])
		records = records[1:]
	} else {
		count := nCol
		if count < 0 && len(records) > 0 {
			count = len(records[0])
		}
		if count < 0 {
			count = 0
		}
		v.columns = make([]string, count)
		for i := range v.columns {
			v.columns[i] = fmt.Sprintf("c%d", i)
		}
	}

	// Normalize row widths to the column count (missing trailing fields
	// become NULL; extras are dropped) like csv.c's field accounting.
	// Fields that parse as numbers are stored as numbers: the engine does
	// not apply column affinity when filtering materialized virtual-table
	// rows, so keeping them as TEXT would break numeric predicates that
	// SQLite answers correctly via the declared TEXT affinity.
	for _, r := range records {
		row := make([]interface{}, len(v.columns))
		for i := 0; i < len(v.columns); i++ {
			if i < len(r) {
				row[i] = coerceCSVField(r[i])
			}
		}
		v.rows = append(v.rows, row)
	}
	return v, nil
}

// coerceCSVField converts a CSV field to int64/float64 when it is exactly a
// number, else keeps the text.
func coerceCSVField(f string) interface{} {
	if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
		return n
	}
	if fl, err := strconv.ParseFloat(strings.TrimSpace(f), 64); err == nil {
		return fl
	}
	return f
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

// columnNamesFromSchema extracts column names from a "CREATE TABLE x(a,b,c)"
// style schema argument: the identifier before the first space of each
// top-level comma-separated term inside the outermost parentheses.
func columnNamesFromSchema(schema string) ([]string, error) {
	open := strings.Index(schema, "(")
	close := strings.LastIndex(schema, ")")
	if open < 0 || close <= open {
		return nil, fmt.Errorf("csv: invalid schema=%q", schema)
	}
	body := schema[open+1 : close]
	// Strip a trailing WITHOUT ROWID clause if the paren scan caught it.
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
	parts = append(parts, cur.String())
	var names []string
	for _, p := range parts {
		f := strings.Fields(strings.TrimSpace(p))
		if len(f) == 0 {
			continue
		}
		name := strings.ToUpper(f[0])
		if name == "WITHOUT" || name == "PRIMARY" || name == "UNIQUE" || name == "CHECK" || name == "FOREIGN" || name == "CONSTRAINT" {
			continue // table-level constraint tail, not a column
		}
		names = append(names, strings.Trim(f[0], `"`+"`"))
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("csv: schema=%q declares no columns", schema)
	}
	return names, nil
}

// Columns implements ColumnInfo.
func (v *csvVTab) Columns() []string { return v.columns }

// ColumnTypes implements ColumnTypeInfo: every csv column is declared TEXT
// (csv.c appends " TEXT" to generated columns), so comparisons apply TEXT
// affinity — WHERE c1=10 matches the stored '10'.
func (v *csvVTab) ColumnTypes() []string {
	types := make([]string, len(v.columns))
	for i := range types {
		types[i] = "TEXT"
	}
	return types
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
