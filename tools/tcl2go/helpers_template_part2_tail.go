package main

const helpersTemplatePart2Tail = `func tclStringIndex(s string, idx interface{}) string {
	n := tclIndex(idx, len(s))
	if n < 0 || n >= len(s) {
		return ""
	}
	return string(s[n])
}

// tclIsXdigit implements TCL's string-is-xdigit predicate: true when the
// string is a single hexadecimal digit (0-9, a-f, A-F). unhex.test uses it to
// build the expected filtered output of unhex().
func tclIsXdigit(s string) bool {
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func tclIndex(idx interface{}, length int) int {
	s := fmt.Sprintf("%%v", idx)
	if strings.HasPrefix(s, "end") {
		rest := s[3:]
		if rest == "" { return length - 1 }
		n, _ := strconv.Atoi(rest)
		return length - 1 + n
	}
	n, _ := strconv.Atoi(s)
	return n
}

// tclPrepareForOptimizeSQL builds the segdir-rewrite SQL used by the
// prepare_for_optimize proc in fts4opt.test: it collapses every segment in
// each level-group (level/1024) into a single level 1024*(level/1024)+32,
// recomputing idx from the number of following segments, and rewrites
// <tbl>_segdir in place via a temp table.
func tclPrepareForOptimizeSQL(tbl string) string {
	seg := tbl + "_segdir"
	return "BEGIN;\n" +
		"CREATE TEMP TABLE tmp_segdir(\n" +
		"  level, idx, start_block, leaves_end_block, end_block, root\n" +
		");\n" +
		"INSERT INTO temp.tmp_segdir\n" +
		"SELECT\n" +
		"1024*(o.level / 1024) + 32,\n" +
		"sum(o.level<i.level OR (o.level=i.level AND o.idx>i.idx)),\n" +
		"o.start_block, o.leaves_end_block, o.end_block, o.root\n" +
		"FROM " + seg + " o, " + seg + " i\n" +
		"WHERE (o.level / 1024) = (i.level / 1024)\n" +
		"GROUP BY o.level, o.idx;\n" +
		"DELETE FROM " + seg + ";\n" +
		"INSERT INTO " + seg + " SELECT * FROM temp.tmp_segdir;\n" +
		"DROP TABLE temp.tmp_segdir;\n" +
		"COMMIT;"
}

// tclQuoteIdent double-quotes an identifier when it is not a plain SQL
// identifier (contains spaces, quotes, or other special characters), so DROP
// TABLE etc. work for tables with unusual names (e.g. "t 1").
func tclQuoteIdent(name string) string {
	if name == "" {
		return name
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
	}
	return name
}

// tclFormat implements TCL's format command at runtime. The format string is
// a TCL printf-style format; each argument is passed as a string (TCL values
// are strings). TCL converts each argument to the type its conversion
// specifier requires — an integer conversion (%%d %%i %%o %%u %%x %%X %%c) parses
// the argument as an integer, a floating conversion (%%f %%e %%E %%g %%G) parses
// it as a double, and %%s uses it verbatim — unlike Go's fmt.Sprintf which
// type-errors on mismatches. Size modifiers (l, h, ll, hh) are stripped and
// %%i/%%u are mapped to %%d, both of which Go's fmt does not support.
func tclFormat(format string, args ...string) string {
	var goFmt strings.Builder
	var goArgs []interface{}
	ai := 0
	i := 0
	for i < len(format) {
		c := format[i]
		if c != '%%' {
			goFmt.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(format) && format[i+1] == '%%' {
			goFmt.WriteString("%%%%")
			i += 2
			continue
		}
		// Parse the conversion specifier: %%[flags][width][.prec][size]conv.
		j := i + 1
		for j < len(format) && strings.ContainsRune("-+ #0", rune(format[j])) {
			j++
		}
		for j < len(format) && (format[j] >= '0' && format[j] <= '9' || format[j] == '*') {
			j++
		}
		if j < len(format) && format[j] == '.' {
			j++
			for j < len(format) && (format[j] >= '0' && format[j] <= '9' || format[j] == '*') {
				j++
			}
		}
		for j < len(format) && strings.ContainsRune("hlL", rune(format[j])) {
			j++
		}
		if j >= len(format) {
			// Unterminated specifier: emit literally.
			goFmt.WriteString(format[i:])
			break
		}
		conv := format[j]
		spec := format[i : j+1]
		// Drop size modifiers (Go fmt has no l/h/ll/hh) and map %%i/%%u to %%d.
		spec = strings.Map(func(r rune) rune {
			if r == 'l' || r == 'h' || r == 'L' {
				return -1
			}
			return r
		}, spec)
		if conv == 'i' || conv == 'u' {
			spec = spec[:len(spec)-1] + "d"
		}
		j++
		if ai >= len(args) {
			goFmt.WriteString(spec)
			continue
		}
		arg := args[ai]
		ai++
		switch conv {
		case 'd', 'o', 'x', 'X', 'c', 'i', 'u':
			// TCL integer conversions accept numeric strings and real
			// values (truncated toward zero), like SQLite's printf.
			var n int64
			if f, err := strconv.ParseFloat(strings.TrimSpace(arg), 64); err == nil {
				n = int64(f)
			} else {
				n, _ = strconv.ParseInt(strings.TrimSpace(arg), 0, 64)
			}
			goArgs = append(goArgs, n)
		case 'f', 'e', 'E', 'g', 'G':
			f, _ := strconv.ParseFloat(strings.TrimSpace(arg), 64)
			goArgs = append(goArgs, f)
		default: // 's' and anything else: use the string verbatim.
			goArgs = append(goArgs, arg)
		}
		goFmt.WriteString(spec)
		i = j
	}
	return fmt.Sprintf(goFmt.String(), goArgs...)
}

// tclRand returns a deterministic pseudo-random float in [0,1), mirroring
// TCL's rand() builtin for tests that build self-consistent data with rand
// (cse.test). Uses Go's math/rand which is deterministic with the default
// seed; both the SQL-building and expected-answer generation call it in the
// same order, keeping the two sides consistent.
func tclRand() float64 {
	return rand.Float64()
}

// tclRandomUUID returns a pseudo-random 30-hex-char string (trans2.test's
// random_uuid proc: five hex values of int(rand()*16777216), seeded with
// srand(1) — deterministic sequence, self-consistent within the test).
func tclRandomUUID() string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < 5; i++ {
		n := rand.Intn(16777216)
		// Render n as zero-padded 6-hex-digit.
		var tmp [6]byte
		for j := 5; j >= 0; j-- {
			tmp[j] = hexdigits[n&0xf]
			n >>= 4
		}
		b.Write(tmp[:])
	}
	return b.String()
}

// tclScramble shuffles a TCL list into a random order (trans2.test's scramble
// proc: attach a random key to each element and sort by it).
func tclScramble(list string) string {
	items := tclSplitList(list)
	type keyed struct {
		key float64
		val string
	}
	ks := make([]keyed, len(items))
	for i, it := range items {
		ks[i] = keyed{key: rand.Float64(), val: it}
	}
	sort.SliceStable(ks, func(i, j int) bool { return ks[i].key < ks[j].key })
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.val
	}
	return tclList(out)
}

// tclMD5 returns the lowercase hex MD5 of a string (trans2.test's md5
// helper, registered by SQLite's test_config.c).
func tclMD5(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// tclHashByIndex computes trans2.test's hash1/hash2: sort the data records
// by field 0 (id) as integers, concatenate field idx (1 = u1, 3 = u2) of each
// record in that order, and return the MD5 of the concatenation.
func tclHashByIndex(data string, idx int) string {
	records := tclSplitList(data)
	sort.SliceStable(records, func(i, j int) bool {
		a := tclLIndex(records[i], "0")
		b := tclLIndex(records[j], "0")
		ai, _ := strconv.Atoi(a)
		bi, _ := strconv.Atoi(b)
		return ai < bi
	})
	var b strings.Builder
	for _, rec := range records {
		f := tclLIndex(rec, idx)
		b.WriteString(f)
	}
	return tclMD5(b.String())
}

// tclColumns generates e_createtable's columns-proc output: a
// comma-separated list of "c0, c1, ..., c(N-1)" (used by CREATE TABLE with
// many columns, exercising SQLITE_MAX_COLUMN). The argument may be an
// arithmetic expression (e.g. "2000+1"), evaluated like the TCL proc's
// [expr $n] would.
func tclColumns(n string) string {
	num, _ := strconv.Atoi(strings.TrimSpace(tclExpr(n)))
	if num < 0 {
		num = 0
	}
	parts := make([]string, 0, num)
	for i := 0; i < num; i++ {
		parts = append(parts, fmt.Sprintf("c%%d", i))
	}
	return strings.Join(parts, ", ")
}

// tclWordset implements fts3ab's wordset i proc: return the quoted
// space-joined list of {one two three four five} words whose bits are set in
// i (only the lower 5 bits are examined). For i=1 this is "'one'", for
// i=7 "'one two three'".
func tclWordset(v string) string {
	num, _ := strconv.Atoi(strings.TrimSpace(tclExpr(v)))
	words := []string{"one", "two", "three", "four", "five"}
	var parts []string
	for j, k := 0, 1; j < 5; j, k = j+1, k*2 {
		if k&num != 0 {
			parts = append(parts, words[j])
		}
	}
	return "'" + strings.Join(parts, " ") + "'"
}

// --- sqlite3_backup emulation helpers ---

// tclBackupInit starts a backup of srcSchema on src into dstSchema on dst
// (sqlite3_backup_init). On success it returns the *frigolite.Backup and a
// nil error; on failure the error message is recorded on the source
// connection for sqlite3_errmsg.
func tclBackupInit(dst *frigolite.DB, dstSchema string, src *frigolite.DB, srcSchema string) (*frigolite.Backup, error) {
	b, err := src.NewBackup(dst, dstSchema, srcSchema)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// tclBackupStep advances a backup by n pages and returns the SQLite result
// code string. The n argument is a TCL-style string ("200", "-1", "$var").
func tclBackupStep(b *frigolite.Backup, n string) string {
	if b == nil {
		return "SQLITE_ERROR"
	}
	ni, _ := strconv.Atoi(strings.TrimSpace(n))
	return b.Step(ni)
}

// tclBackupFinish completes a backup and returns the SQLite result code
// string.
func tclBackupFinish(b *frigolite.Backup) string {
	if b == nil {
		return "SQLITE_ERROR"
	}
	return b.Finish()
}

// tclSqlTail returns the SQL text after the first statement of a
// multi-statement body, matching sqlite3_prepare's TAIL out-parameter: for a
// single statement (or comment-only tail) it returns the tail text after the
// first ';' (including any trailing comment), and "" when the SQL has exactly
// one statement. The split is simple (no quote awareness), matching the
// transpiler's other SQL split helpers.
func tclSqlTail(sql string) string {
	idx := strings.Index(sql, ";")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(sql[idx+1:])
}

// tclListFlattenCollapse flattens a TCL list value and collapses ALL
// whitespace runs (including newlines) to single spaces, then trims. Used for
// set-var comparisons where the value may be SQL text / prepare TAIL content
// whose leading/trailing whitespace differs between the C-API tail pointer
// and the TCL braced expected value.
func tclListFlattenCollapse(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return strings.Join(strings.Fields(tclListFlatten(s)), " ")
}
var tclClosedConns = map[*frigolite.DB]bool{}

// tclCloseDB closes a connection and tracks successful closes.
func tclCloseDB(db *frigolite.DB) string {
	if db == nil { return "SQLITE_OK" }
	if err := db.Close(); err != nil { return "SQLITE_BUSY" }
	tclClosedConns[db] = true
	return "SQLITE_OK"
}

var tclPrepared = map[string]*frigolite.Stmt{}

func tclPrepareStep(db *frigolite.DB, sqlText, name string) {
	if db == nil { return }
	stmt, err := db.Prepare(sqlText)
	if err != nil { return }
	tclPrepared[name] = stmt
	_, _ = stmt.Step()
}

func tclErrMsg(db *frigolite.DB) string {
	if tclClosedConns[db] { return "bad parameter or other API misuse" }
	if db == nil { return "bad parameter or other API misuse" }
	// sqlite3_errmsg reflects only the most recent API call — no implicit
	// stepping of other prepared statements.
	return db.LastErr()
}

func tclStepPrepared(name string) {
	if stmt := tclPrepared[name]; stmt != nil { _, _ = stmt.Step() }
}

func tclResetPrepared(name string) {
	if stmt := tclPrepared[name]; stmt != nil { _ = stmt.Reset() }
}

func tclFinalizePrepared(name string) {
	if stmt := tclPrepared[name]; stmt != nil {
		_, _ = stmt.Step()
		// sqlite3_finalize re-reports a failed statement's error as the
		// connection's last error (vdbeapi.c sqlite3VdbeFinalize).
		_ = stmt.Finalize()
		delete(tclPrepared, name)
	}
}

// tclFinalizePreparedCode finalizes the named prepared statement and returns
// the sqlite3_finalize result code string: SQLITE_OK, or the code of the
// statement's most recent failed step re-reported by finalize (vdbeapi.c
// sqlite3VdbeFinalize). db is the connection the statement was prepared on
// (used only for error-code classification).
func tclFinalizePreparedCode(db *frigolite.DB, name string) string {
	stmt := tclPrepared[name]
	if stmt == nil {
		return "SQLITE_OK"
	}
	err := stmt.Finalize()
	delete(tclPrepared, name)
	if err != nil && db != nil {
		return db.ErrorCodeFor(err)
	}
	return "SQLITE_OK"
}

// tclStepPreparedCode steps the named prepared statement (sqlite3_step) and
// returns its result code string: SQLITE_DONE when the statement ran to
// completion, or the mapped SQLITE_* code on failure. When the statement has
// a materialized handle its step records the error on both the statement and
// the connection (so a following sqlite3_finalize re-reports it); otherwise
// (handle declared but never prepared at runtime) the SQL text runs directly
// on the connection.
func tclStepPreparedCode(db *frigolite.DB, name, sqlText string) string {
	if stmt := tclPrepared[name]; stmt != nil {
		r := stmt.Exec()
		if r.Error != nil {
			if db != nil {
				return db.ErrorCodeFor(r.Error)
			}
			return "SQLITE_ERROR"
		}
		return "SQLITE_DONE"
	}
	if db == nil {
		return "SQLITE_ERROR"
	}
	r := db.Exec(sqlText)
	if r.Error != nil {
		db.SetLastErr(r.Error.Error(), db.ErrorCodeFor(r.Error))
		return db.ErrorCodeFor(r.Error)
	}
	return "SQLITE_DONE"
}

// --- prepared-statement VM emulation (bind/bind2) ---
//
// These helpers model the sqlite3_prepare / sqlite3_bind_* / sqlite3_step
// cycle through frigolite's Stmt API, keeping the observable C-API semantics
// the TCL tests assert: prepare-time compile errors, typed binds (typeof
// preservation, embedded NULs), bind range/misuse errors on the connection,
// and step result codes.

// tclPrepareStmt prepares sqlText and stores the handle under name
// (sqlite3_prepare_v2). Returns the result code; a compile error is recorded
// on the connection for sqlite3_errmsg/sqlite3_errcode.
//
// Frigolite resolves column names lazily at execution, so a QUERY is run
// once here: prepare-time errors like "no such column" surface immediately
// (capi3-1.7/1.8.x), and the materialized rows feed subsequent sqlite3_step
// calls through the cursor.
func tclPrepareStmt(db *frigolite.DB, name, sqlText string, nByte int) string {
	if db == nil {
		return "SQLITE_MISUSE"
	}
	// sqlite3_prepare's nByte limits how much of the SQL is read.
	if nByte >= 0 && nByte < len(sqlText) {
		sqlText = sqlText[:nByte]
	}
	stmt, err := db.Prepare(sqlText)
	if err != nil {
		code := db.ErrorCodeFor(err)
		db.SetLastErr(err.Error(), code)
		return code
	}
	if old := tclPrepared[name]; old != nil {
		_ = old.Finalize()
	}
	tclPrepared[name] = stmt
	tclInvalidateStep(name)
	if tclIsQuerySQL(sqlText) {
		r := db.Query(sqlText)
		if r.Error != nil {
			code := db.ErrorCodeFor(r.Error)
			db.SetLastErr(r.Error.Error(), code)
			return code
		}
		tclLastStep[name] = &tclStepState{r: r, row: -1}
	}
	return "SQLITE_OK"
}

// tclIsQuerySQL reports whether the statement returns rows directly.
func tclIsQuerySQL(sqlText string) bool {
	up := strings.ToUpper(strings.TrimSpace(sqlText))
	return strings.HasPrefix(up, "SELECT") || strings.HasPrefix(up, "PRAGMA") ||
		strings.HasPrefix(up, "WITH") || strings.HasPrefix(up, "EXPLAIN")
}

// tclUnescapeOctal converts TCL backslash escapes (\000 octal, \n \t \r \\)
// in a braced literal to raw bytes so embedded NULs reach the engine.
func tclUnescapeOctal(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		c := s[i]
		switch {
		case c == 'n':
			b.WriteByte('\n')
		case c == 't':
			b.WriteByte('\t')
		case c == 'r':
			b.WriteByte('\r')
		case c == '\\':
			b.WriteByte('\\')
		case c >= '0' && c <= '7':
			v := int(c - '0')
			for k := 0; k < 2 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7'; k++ {
				i++
				v = v*8 + int(s[i]-'0')
			}
			b.WriteByte(byte(v))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// tclBindValue converts one bound value of kind ("int", "double", "text",
// "blob", "blob10") from its TCL literal text. nlen mirrors sqlite3_bind_text's
// length: <0 reads to the first NUL, otherwise exactly nlen bytes are taken.
// "blob10" is test1.c test_bind's fixed 10-byte static text "abc\0xyz\0pq".
func tclBindValue(kind, raw string, nlen int) interface{} {
	switch kind {
	case "null":
		return nil
	case "int":
		v, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		return v
	case "double":
		v, _ := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		return v
	case "blob10":
		return []byte("abc\x00xyz\x00pq")
	}
	data := tclUnescapeOctal(raw)
	if nlen < 0 {
		if z := strings.IndexByte(data, 0); z >= 0 {
			data = data[:z]
		}
	} else if nlen <= len(data) {
		data = data[:nlen]
	}
	if kind == "blob" {
		return []byte(data)
	}
	return data
}

// tclBindStmt binds idx to a typed literal on the named statement. Range and
// misuse failures are recorded as the connection's last error (vdbeapi.c).
func tclBindStmt(db *frigolite.DB, name string, idx int, kind, raw string, nlen int) string {
	stmt := tclPrepared[name]
	if stmt == nil {
		return "SQLITE_MISUSE"
	}
	err := stmt.Bind(idx, tclBindValue(kind, raw, nlen))
	if err != nil {
		code := "SQLITE_ERROR"
		if db != nil {
			code = db.ErrorCodeFor(err)
			db.SetLastErr(err.Error(), code)
		}
		return code
	}
	// A successful API call clears the connection's last error
	// (sqlite3_errmsg returns "not an error" afterwards).
	if db != nil {
		db.SetLastErr("", "")
	}
	tclInvalidateStep(name)
	return "SQLITE_OK"
}

// tclStepState tracks one statement's materialized step result and row
// cursor so successive sqlite3_step calls advance SQLITE_ROW → SQLITE_DONE,
// and a reset (or re-bind) restarts the program.
type tclStepState struct {
	r   *frigolite.Result
	row int
}

var tclLastStep = map[string]*tclStepState{}

func tclInvalidateStep(name string) {
	delete(tclLastStep, name)
}

// tclStepStmt steps the named prepared statement (sqlite3_step), returning
// the SQLite result code string. The first step after prepare/reset/re-bind
// runs the statement; further calls walk the result rows.
func tclStepStmt(db *frigolite.DB, name string) string {
	stmt := tclPrepared[name]
	if stmt == nil {
		return ""
	}
	cur := tclLastStep[name]
	if cur == nil {
		r := stmt.Exec()
		if r.Error != nil {
			code := "SQLITE_ERROR"
			if db != nil {
				code = db.ErrorCodeFor(r.Error)
				db.SetLastErr(r.Error.Error(), code)
			}
			return code
		}
		cur = &tclStepState{r: r, row: -1}
		tclLastStep[name] = cur
	}
	cur.row++
	if cur.row < len(cur.r.Rows) {
		return "SQLITE_ROW"
	}
	return "SQLITE_DONE"
}

// tclParamCountOf returns sqlite3_bind_parameter_count for the named stmt.
func tclParamCountOf(name string) int {
	if s := tclPrepared[name]; s != nil {
		return s.BindParameterCount()
	}
	return 0
}

// tclParamNameOf returns sqlite3_bind_parameter_name for the named stmt.
func tclParamNameOf(name string, idx int) string {
	if s := tclPrepared[name]; s != nil {
		return s.BindParameterName(idx)
	}
	return ""
}

// tclParamIndexOf returns sqlite3_bind_parameter_index for the named stmt.
func tclParamIndexOf(name, paramName string) int {
	if s := tclPrepared[name]; s != nil {
		return s.BindParameterIndex(paramName)
	}
	return 0
}

// tclColumnCount returns the column count of the named statement's last step.
func tclColumnCount(name string) int {
	if st := tclLastStep[name]; st != nil {
		return len(st.r.Columns)
	}
	return 0
}

// tclDataCount returns sqlite3_data_count: the number of columns with data
// available on the current row.
func tclDataCount(name string) int {
	st := tclLastStep[name]
	if st == nil || st.row < 0 || st.row >= len(st.r.Rows) {
		return 0
	}
	return len(st.r.Rows[st.row])
}

// tclColumnNameOf returns sqlite3_column_name for the named statement.
func tclColumnNameOf(name string, col int) string {
	st := tclLastStep[name]
	if st == nil || col < 0 || col >= len(st.r.Columns) {
		return ""
	}
	return st.r.Columns[col]
}

// tclColumnTextOf renders one cell of the current step row like
// sqlite3_column_text + TCL rendering ("" when unavailable).
func tclColumnTextOf(name string, col int) string {
	st := tclLastStep[name]
	if st == nil || st.row < 0 || st.row >= len(st.r.Rows) {
		return ""
	}
	row := st.r.Rows[st.row]
	if col < 0 || col >= len(row) || row[col] == nil {
		return ""
	}
	return tclRenderCell(row[col])
}

// tclColumnDoubleOf renders one cell of the current step row like
// sqlite3_column_double + TCL REAL rendering.
func tclColumnDoubleOf(name string, col int) string {
	st := tclLastStep[name]
	if st == nil || st.row < 0 || st.row >= len(st.r.Rows) {
		return ""
	}
	row := st.r.Rows[st.row]
	if col < 0 || col >= len(row) || row[col] == nil {
		return ""
	}
	if f, ok := row[col].(float64); ok {
		return tclRenderCell(f)
	}
	return tclRenderCell(row[col])
}

// tclResetStmtCode resets the named statement (sqlite3_reset), returning OK
// and restarting the step program.
func tclResetStmtCode(name string) string {
	if stmt := tclPrepared[name]; stmt != nil {
		_ = stmt.Reset()
	}
	tclInvalidateStep(name)
	return "SQLITE_OK"
}

// tclClearBindingsStmt clears the named statement's bound parameters
// (sqlite3_clear_bindings), restarting the step program like SQLite does not
// (bindings apply on the next run either way).
func tclClearBindingsStmt(name string) string {
	if stmt := tclPrepared[name]; stmt != nil {
		_ = stmt.ClearBindings()
	}
	return "SQLITE_OK"
}

// tclFinalizeStmt finalizes the named statement (sqlite3_finalize): the
// result re-reports the statement's most recent failed step (vdbeapi.c
// sqlite3VdbeFinalize); finalizing an unknown handle is SQLITE_OK.
func tclFinalizeStmt(db *frigolite.DB, name string) string {
	stmt := tclPrepared[name]
	delete(tclPrepared, name)
	tclInvalidateStep(name)
	if stmt == nil {
		return "SQLITE_OK"
	}
	err := stmt.Finalize()
	if err != nil && db != nil {
		code := db.ErrorCodeFor(err)
		db.SetLastErr(err.Error(), code)
		return code
	}
	return "SQLITE_OK"
}

// tclDBCksum computes the tester.tcl dbcksum value for a schema: the MD5 of
// the concatenated "type,name,tbl_name,sql" of every sqlite_master row plus
// the joined cell values of every table row. Both source and destination
// backups of the same database produce the same checksum, so comparing them
// verifies content equality.
func tclDBCksum(db *frigolite.DB, schemaName string) string {
	if db == nil {
		return ""
	}
	qual := ""
	if schemaName != "" && !strings.EqualFold(schemaName, "main") {
		qual = "\"" + strings.ReplaceAll(schemaName, "\"", "\"\"") + "\"."
	}
	var b strings.Builder
	mr := db.Query("SELECT type, name, tbl_name, sql FROM " + qual + "sqlite_master ORDER BY rowid")
	if mr.Error == nil {
		for _, row := range mr.Rows {
			for _, v := range row {
				b.WriteString(tclRenderCell(v))
			}
		}
	}
	// Table contents: for each table in the schema, join all cell values.
	tr := db.Query("SELECT name FROM " + qual + "sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%%' ORDER BY name")
	if tr.Error == nil {
		for _, row := range tr.Rows {
			tbl := fmt.Sprint(row[0])
			q := db.Query("SELECT * FROM " + qual + "\"" + strings.ReplaceAll(tbl, "\"", "\"\"") + "\" ORDER BY rowid")
			if q.Error == nil {
				for _, rrow := range q.Rows {
					for _, v := range rrow {
						b.WriteString(tclRenderCell(v))
					}
				}
			}
		}
	}
	return tclMD5(b.String())
}

// tclFileSize returns a file's size in bytes (0 when missing).
func tclFileSize(path string) int {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return int(fi.Size())
}

// tclReadFile returns the contents of a file (the TCL "read $fd" on a file
// channel whose path is held in the var). A missing/empty path yields "".
func tclReadFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// tclBlobResolve returns the open *frigolite.Blob whose channel-name string
// matches name (e.g. "incrblob_5"). Used when the transpiler cannot statically
// resolve a blob handle variable across body blocks.
func tclBlobResolve(name string, blobs ...*frigolite.Blob) *frigolite.Blob {
	for _, b := range blobs {
		_ = b
	}
	// The blobs are passed in order incrblob_1..incrblob_N; the channel name
	// "incrblob_K" maps to blobs[K-1].
	if strings.HasPrefix(name, "incrblob_") {
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "incrblob_")); err == nil && n >= 1 && n <= len(blobs) {
			return blobs[n-1]
		}
	}
	return nil
}

// blobReadAll reads the entire incremental-blob value from the channel
// cursor, advancing it to the end. On error it returns nil.
func blobReadAll(b *frigolite.Blob, pos int) []byte {
	if b == nil {
		return nil
	}
	data, err := b.Read(0, b.Bytes())
	if err != nil {
		return nil
	}
	if pos > len(data) {
		pos = len(data)
	}
	return data[pos:]
}

// tclBlobWritable reports whether a sqlite3_blob_open flags value requests
// read/write access (any non-zero value). The transpiler may pass an int
// literal or a string variable.
func tclBlobWritable(v interface{}) bool {
	switch x := v.(type) {
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return false
		}
		return n != 0
	case bool:
		return x
	}
	return false
}

// tclRowID converts a sqlite3_blob_open / db incrblob rowid argument (an int
// literal, int64, or string variable) to the int64 rowid.
func tclRowID(v interface{}) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// tclBlobInt converts a blob offset/count argument (int literal or string
// variable) to an int.
func tclBlobInt(v interface{}) int {
	return int(tclRowID(v))
}

// tclBlobBytes converts a value to its byte representation (for binary
// format aN/a* string-copy specs in the corruption tests).
func tclBlobBytes(v interface{}) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	default:
		return []byte(fmt.Sprintf("%%v", x))
	}
}

// tclBinToHex renders SQL blob bytes as uppercase hexadecimal, matching
// blob.test's binary scan/format helper.
func tclBinToHex(v interface{}) string {
	var b []byte
	switch x := v.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	default:
		b = []byte(tclStr(v))
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// tclHexDecode implements the test-harness blob() proc (binary decode hex):
// it converts a hex string (possibly with whitespace) to bytes
// (fts3corrupt4 builds modified segment roots through it).
func tclHexDecode(s string) []byte {
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if len(clean)%%2 != 0 {
		return nil
	}
	out := make([]byte, len(clean)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexNibble(clean[2*i])
		lo, ok2 := hexNibble(clean[2*i+1])
		if !ok1 || !ok2 {
			return nil
		}
		out[i] = hi<<4 | lo
	}
	return out
}

// tclMatchinfoDecode implements the fts3matchinfo test-harness mit() proc
// (binary scan $blob $scan(littleEndian) r): it decodes a matchinfo blob as
// little-endian 32-bit integers and returns them as a TCL list string
// ("1 1 1 2 2"). The TCL proc uses the platform byte order, which is
// little-endian on every supported platform (tcl_platform(byteOrder)).
func tclMatchinfoDecode(v interface{}) string {
	var blob []byte
	switch b := v.(type) {
	case []byte:
		blob = b
	case string:
		blob = []byte(b)
	default:
		return ""
	}
	var parts []string
	for i := 0; i+4 <= len(blob); i += 4 {
		u := uint32(blob[i]) | uint32(blob[i+1])<<8 | uint32(blob[i+2])<<16 | uint32(blob[i+3])<<24
		parts = append(parts, strconv.FormatInt(int64(int32(u)), 10))
	}
	return strings.Join(parts, " ")
}

// tclFts3Record implements the test-harness make_fts3record proc (src/
// test_hexio.c): it builds an FTS3 segment record blob from a heterogeneous
// argument list — each INTEGER argument is encoded with the FTS3 varint
// encoding, each STRING argument is appended as raw bytes (fts4record.test
// builds corrupted segment roots through it).
func tclFts3Record(args []interface{}) []byte {
	var out []byte
	for _, a := range args {
		if a == nil {
			continue
		}
		switch v := a.(type) {
		case int64:
			out = tclFts3PutVarint(out, uint64(v))
		case int:
			out = tclFts3PutVarint(out, uint64(v))
		case string:
			out = append(out, v...)
		case []byte:
			out = append(out, v...)
		default:
			s := fmt.Sprint(v)
			out = append(out, s...)
		}
	}
	return out
}

// tclFts3PutVarint writes v in the FTS3 varint encoding (sqlite3Fts3PutVarint,
// ext/fts3/fts3.c): the low 7 bits first with the high bit as a continuation
// marker; the final byte has the high bit clear. This is the encoding used by
// FTS segment nodes, doclists, and segdir root blobs.
func tclFts3PutVarint(buf []byte, v uint64) []byte {
	for {
		b := byte(v&0x7f) | 0x80
		v >>= 7
		if v == 0 {
			b &= 0x7f
			return append(buf, b)
		}
		buf = append(buf, b)
	}
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// blobReadN reads n bytes from an incremental blob at the channel cursor,
// advancing the cursor by the bytes read.
func blobReadN(b *frigolite.Blob, pos int, n int) []byte {
	if b == nil {
		return nil
	}
	data, err := b.Read(0, b.Bytes())
	if err != nil {
		return nil
	}
	if pos > len(data) {
		pos = len(data)
	}
	if n < 0 {
		n = 0
	}
	end := pos + n
	if end > len(data) {
		end = len(data)
	}
	return data[pos:end]
}

// blobSeek sets the byte cursor of an incremental-blob channel (the cursor is
// kept as a Go variable per channel; blobSeek just returns the new position).
func blobSeek(b *frigolite.Blob, offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

// blobSeekEnd sets the byte cursor relative to the end of the blob
// (TCL "seek $blob N end").
func blobSeekEnd(b *frigolite.Blob, offset int) int {
	if b == nil {
		return 0
	}
	size := b.Bytes()
	pos := size + offset
	if pos < 0 {
		pos = 0
	}
	if pos > size {
		pos = size
	}
	return pos
}

// blobPuts writes data at the channel cursor of an incremental-blob channel
// and advances the cursor. The -nonewline flag is accepted for compatibility;
// blob writes never add a newline.
func blobPuts(b *frigolite.Blob, pos int, data []byte, nonewline bool) int {
	if b == nil {
		return pos
	}
	if err := b.Write(pos, data, len(data)); err != nil {
		return pos
	}
	return pos + len(data)
}

// tclConnByName returns the open *frigolite.DB connection named by a TCL
// connection-name string ("db", "db2", ...) for execsql's runtime
// connection dispatch (foreach db {db db2} { execsql {...} $db }).
func tclConnByName(name string, db, db1, db2, db3, db4, db5, db6, db7, db8, db9 *frigolite.DB) *frigolite.DB {
	switch strings.TrimSpace(name) {
	case "db":
		return db
	case "db1":
		return db1
	case "db2":
		return db2
	case "db3":
		return db3
	case "db4":
		return db4
	case "db5":
		return db5
	case "db6":
		return db6
	case "db7":
		return db7
	case "db8":
		return db8
	case "db9":
		return db9
	}
	return db
}

// tclDBBackupRestore implements the TCL db backup / db restore methods:
// a convenience wrapper over sqlite3_backup that opens the target file, runs
// the backup to completion, and closes it. Errors are wrapped with the TCL
// binding's message prefix ("backup failed: ..." / "restore failed: ..." /
// "cannot open source database: ...").
func tclDBBackupRestore(db *frigolite.DB, kind, schemaName, file string) error {
	if kind == "backup" {

		// db backup [SCHEMA] FILE: back up db's SCHEMA (default main) into the
		// file's main schema. Open the file as the destination.
		d, err := frigolite.Open(file)
		if err != nil {
			return fmt.Errorf("backup failed: %%v", err)
		}

		b, berr := db.NewBackup(d, "main", schemaName)
		if berr != nil {
			d.Close()
			return fmt.Errorf("backup failed: %%v", berr)
		}
		rc := b.Step(-1)
		b.Finish()
		srcEmpty := false
		if check := db.Query("SELECT count(*) FROM sqlite_master"); check.Error == nil && len(check.Rows) == 1 && check.Rows[0][0] == int64(0) { srcEmpty = true }
		d.Close()
		if srcEmpty { _ = os.Truncate(db.FilePath(), 0) }
		if rc != "SQLITE_DONE" {
			if rc == "SQLITE_READONLY" {
				return fmt.Errorf("backup failed: attempt to write a readonly database")
			}
			return fmt.Errorf("backup failed: %%v", b.ErrMsg())
		}
		return nil
	}
	// db restore [SCHEMA] FILE: restore the file's main schema into db's
	// SCHEMA (default main). Open the file as the source.
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("cannot open source database: unable to open database file")
	}
	s, err := frigolite.Open(file)
	if err != nil {
		return fmt.Errorf("cannot open source database: %%v", err)
	}
	if db.LastErrCode() == "SQLITE_BUSY" {
		s.Close()
		return fmt.Errorf("restore failed: source database busy")
	}
	b, berr := s.NewBackup(db, schemaName, "main")
	if berr != nil {
		s.Close()
		return fmt.Errorf("restore failed: %%v", berr)
	}
	rc := b.Step(-1)
	b.Finish()
	s.Close()
	if rc == "SQLITE_BUSY" {
		return fmt.Errorf("restore failed: source database busy")
	}
	if rc != "SQLITE_DONE" {
		return fmt.Errorf("restore failed: %%v", b.ErrMsg())
	}
	return nil
}

// fts3ExprTest implements SQLite's test-only fts3_exprtest() SQL function
// (fts3_expr.c fts3ExprTest): parse the expression with the named tokenizer
// and return the expression tree dump used by fts3expr.test.
func fts3ExprTest(args []interface{}) (interface{}, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("Usage: fts3_exprtest(tokenizer, expr, col1, ...")
	}
	tok, _ := args[0].(string)
	expr, _ := args[1].(string)
	if tok != "simple" && tok != "unicode61" {
		return nil, fmt.Errorf("unknown tokenizer: %%s", tok)
	}
	node, err := fts.ParseMatchQuery(expr)
	if err != nil {
		return nil, fmt.Errorf("Error parsing expression")
	}
	return fts.ExprPrint(node), nil
}


// fts3SortBuildDatabase ports fts3sort.test's build_database proc: create the
// FTS4 table t1 (with an optional FTS4 parameter like order=asc/desc) and
// insert nRow six-token documents from a 10-word vocabulary. The suite
// compares frigolite against itself, so the token stream only needs to be
// deterministic, not TCL-rand identical.
func fts3SortBuildDatabase(db *frigolite.DB, nRow int, param string) {
	if param != "" {
		param = "," + param
	}
	db.Exec("DROP TABLE IF EXISTS t1")
	db.Exec("CREATE VIRTUAL TABLE t1 USING fts4(" + param + ")")
	vocab := []string{"aa", "ab", "ac", "ba", "bb", "bc", "ca", "cb", "cc", "da"}
	for i := 0; i < nRow; i++ {
		x := (i*7919 + 13) %% 1000000
		var doc []string
		for div := 1; div < 1000000; div *= 10 {
			doc = append(doc, vocab[(x/div)%%10])
		}
		db.Exec("INSERT INTO t1 VALUES('" + strings.Join(doc, " ") + "')")
	}
}


// tclChannelAppend appends text to a TCL write-mode file channel
// (set fd [open FILE wb] + puts $fd text + close $fd).
func tclChannelAppend(path, text string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(text)
	_ = f.Close()
}

// fileChannelSeek tracks the current byte position of each write-mode file
// channel so that 'seek FD OFFSET start' followed by 'puts -nonewline FD DATA'
// overwrites bytes at OFFSET instead of appending to EOF (corrupt2.test 1.4/1.5
// etc. rely on this to hex-corrupt a known offset).
var fileChannelSeek = map[string]int64{}

// tclChannelAppendAt writes text to a TCL file channel at a specific byte
// offset, extending the file with zero padding if the offset is past EOF.
// This mirrors TCL fconfigure -translation binary + seek FD OFFSET start
// + puts -nonewline FD DATA, the canonical pattern for hex-corrupting a
// database file at a known offset (corrupt*.test suites depend on this).
func tclChannelAppendAt(path, text string, offset int64) {
	data, err := os.ReadFile(path)
	if err != nil {
		tclChannelAppend(path, text)
		return
	}
	if offset < 0 {
		offset = 0
	}
	end := int(offset) + len(text)
	if len(data) < end {
		padded := make([]byte, end)
		copy(padded, data)
		data = padded
	}
	copy(data[int(offset):], text)
	_ = os.WriteFile(path, data, 0644)
}

// tclFileLen returns the current size of a file in bytes, or 0 if the file
// is missing. Used by 'seek FD OFFSET end' resolution.
func tclFileLen(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// tclSwapInt32Args ports rtreecheck.test's blob-surgery SQL functions onto
// one implementation (the "$data"-substitution form cannot express the
// 3-argument shapes):
//
//	swap_int32(blob, i0, i1)  — exchange the big-endian u32 words at i0/i1
//	set_int32(blob, idx, val) — store val as the big-endian u32 word at idx
//
// TCL binary scan I* reads BIG-endian unsigned 32-bit words indexed from
// byte 0; binary format I* writes them back. The mutated copy is returned so
// UPDATE ... SET data=swap_int32(data,...) round-trips.
func tclSwapInt32Args(args []interface{}, setForm bool) ([]byte, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("wrong # args")
	}
	var src []byte
	switch b := args[0].(type) {
	case []byte:
		src = b
	case string:
		src = []byte(b)
	default:
		return nil, fmt.Errorf("1st argument must be a blob")
	}
	out := append([]byte(nil), src...)
	toIndex := func(v interface{}) (int, bool) {
		switch n := v.(type) {
		case int64:
			return int(n), true
		case float64:
			return int(n), true
		case string:
			i, err := strconv.Atoi(n)
			return i, err == nil
		}
		return 0, false
	}
	word := func(i int) uint32 {
		return uint32(out[i*4])<<24 | uint32(out[i*4+1])<<16 | uint32(out[i*4+2])<<8 | uint32(out[i*4+3])
	}
	putWord := func(i int, v uint32) {
		out[i*4] = byte(v >> 24)
		out[i*4+1] = byte(v >> 16)
		out[i*4+2] = byte(v >> 8)
		out[i*4+3] = byte(v)
	}
	okIdx := func(i int) bool { return i >= 0 && i*4+4 <= len(out) }
	if setForm {
		idx, oki := toIndex(args[1])
		val, okv := toIndex(args[2])
		if !oki || !okv || !okIdx(idx) {
			return nil, fmt.Errorf("bad set_int32 arguments")
		}
		putWord(idx, uint32(val))
		return out, nil
	}
	i0, ok0 := toIndex(args[1])
	i1, ok1 := toIndex(args[2])
	if !ok0 || !ok1 || !okIdx(i0) || !okIdx(i1) {
		return nil, fmt.Errorf("word index out of range")
	}
	w0, w1 := word(i0), word(i1)
	putWord(i0, w1)
	putWord(i1, w0)
	return out, nil
}

// ---- user-proc registry (rtree4.test rand/randincr/scramble) ----

// tclUserProcs maps TCL proc names defined by a test to Go implementations
// registered at definition time. The runtime expression evaluator consults
// the registry for [name arg...] bracket substitutions it would otherwise
// leave as raw text.
var tclUserProcs = map[string]func(args []string) string{}

// registerTclUserProc installs one user-proc implementation; later
// definitions override earlier ones like TCL's proc redefinition.
func registerTclUserProc(name string, fn func(args []string) string) {
	tclUserProcs[name] = fn
}

// callTclUserProc invokes a registered user proc from transpiled set-RHS
// code. Unregistered names fall back to the raw TCL text so behavior matches
// previous (unevaluated) emission instead of panicking.
func callTclUserProc(name string, args ...string) string {
	if fn, ok := tclUserProcs[name]; ok {
		return fn(args)
	}
	return name + " " + strings.Join(args, " ")
}

// tclRtree4RandFloat mirrors rtree4.test's float-variant proc rand {X}:
// int((rand()-0.5)*1024.0*$X)/512.0 — a float in [-X, X) on the k/512 grid.
func tclRtree4RandFloat(xArg string) string {
	x, err := strconv.ParseFloat(strings.TrimSpace(xArg), 64)
	if err != nil {
		x = 0
	}
	v := float64(int((tclRand()-0.5)*1024.0*x)) / 512.0
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// tclRtree4RandInt mirrors rtree4.test's rtree_int_only variant of rand {X}:
// int((rand()-0.5)*2*$X) — an integer in (-X, X).
func tclRtree4RandInt(xArg string) string {
	x, err := strconv.Atoi(strings.TrimSpace(xArg))
	if err != nil {
		x = 0
	}
	return strconv.Itoa(int((tclRand()-0.5)*2*float64(x)))
}

// tclRtree4RandIncrFloat mirrors the float variant of randincr {X}: draw
// int(rand()*$X*32.0)/32.0 until strictly positive.
func tclRtree4RandIncrFloat(xArg string) string {
	x, err := strconv.ParseFloat(strings.TrimSpace(xArg), 64)
	if err != nil {
		x = 0
	}
	for i := 0; i < 10000; i++ {
		r := float64(int(tclRand()*x*32.0)) / 32.0
		if r > 0.0 {
			return strconv.FormatFloat(r, 'g', -1, 64)
		}
	}
	// Statistically unreachable; keep the loop total like TCL's while 1.
	return "0.03125"
}

// tclRtree4RandIncrInt mirrors the rtree_int_only randincr {X}: draw
// int(rand()*$X)+1 until > 0 (always true for X>0).
func tclRtree4RandIncrInt(xArg string) string {
	x, err := strconv.Atoi(strings.TrimSpace(xArg))
	if err != nil {
		x = 0
	}
	for i := 0; i < 10000; i++ {
		r := rand.Intn(x + 1)
		if r > 0 {
			return strconv.Itoa(r)
		}
	}
	return "1"
}

// tclUserScramble mirrors rtree4.test's scramble {inlist}: attach a random
// key to each element and sort by it (same shape as tclScramble but wired to
// this test's registry contract).
func tclUserScramble(list string) string {
	type kv struct {
		k float64
		v string
	}
	items := tclSplitList(list)
	ks := make([]kv, len(items))
	for i, it := range items {
		ks[i] = kv{k: tclRand(), v: it}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].k < ks[j].k })
	out := make([]string, len(ks))
	for i, e := range ks {
		out[i] = e.v
	}
	return strings.Join(out, " ")
}

// tclRemoveTimestamps reimplements the sqlite3_tcl remove_timestamps() test
// helper: it strips every extended-timestamp extra field (id 0x5455) from an
// in-memory ZIP archive and rebuilds the image. Local-file-header offsets
// recorded in the central directory shift when local extra data shrinks, so
// the whole image is re-emitted entry by entry with corrected offsets.
// Readers fall back to the DOS date/time fields, which encode the same
// second for even-numbered UTC timestamps. Input is returned unchanged when
// no EOCD signature is present.
func tclRemoveTimestamps(blob []byte) []byte {
	const utID = 0x5455
	le16 := func(b []byte, o int) int { return int(binary.LittleEndian.Uint16(b[o : o+2])) }
	le32 := func(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o : o+4]) }
	put16 := func(b []byte, o int, v uint16) { binary.LittleEndian.PutUint16(b[o:o+2], v) }
	put32 := func(b []byte, o int, v uint32) { binary.LittleEndian.PutUint32(b[o:o+4], v) }

	eocd := bytes.LastIndex(blob, []byte{0x50, 0x4b, 0x05, 0x06})
	if eocd < 0 || eocd+22 > len(blob) {
		return blob
	}
	n := le16(blob, eocd+10)
	cdOff := int(le32(blob, eocd+16))
	cdSize := int(le32(blob, eocd+12))
	if n == 0 || cdOff < 0 || cdOff+cdSize > len(blob) {
		return blob
	}

	var out []byte
	localStart := 0 // offset of the most recently emitted local header
	writeLocal := func(i int) bool {
		if i+30 > len(blob) {
			return false
		}
		localStart = len(out)
		lfh := append([]byte(nil), blob[i:i+30]...)
		nName, nExtra := le16(lfh, 26), le16(lfh, 28)
		nameEnd := i + 30 + nName + nExtra
		if nameEnd > len(blob) {
			return false
		}
		dataLen := le32(lfh, 18)
		if nameEnd+int(dataLen) > len(blob) {
			return false
		}
		extra := blob[i+30+nName : nameEnd]
		newExtra := stripExtraField(extra, utID)
		if len(newExtra) != len(extra) {
			put16(lfh, 28, uint16(len(newExtra)))
		}
		out = append(out, lfh...)
		out = append(out, blob[i+30:i+30+nName]...)
		out = append(out, newExtra...)
		out = append(out, blob[nameEnd:nameEnd+int(dataLen)]...)
		return true
	}

	var cd []byte
	rebuilt := 0
	for off := cdOff; rebuilt < n && off+46 <= cdOff+cdSize && off+46 <= len(blob); rebuilt++ {
		if le32(blob, off) != 0x02014b50 {
			return blob // unrecognised layout: leave the archive untouched
		}
		entry := append([]byte(nil), blob[off:off+46]...)
		nName, nExtra, nComment := le16(entry, 28), le16(entry, 30), le16(entry, 32)
		bodyEnd := off + 46 + nName + nExtra + nComment
		if bodyEnd > len(blob) {
			return blob
		}
		lho := int(le32(entry, 42))
		if !writeLocal(lho) {
			return blob
		}
		extra := blob[off+46+nName : off+46+nName+nExtra]
		newExtra := stripExtraField(extra, utID)
		if len(newExtra) != len(extra) {
			put16(entry, 30, uint16(len(newExtra)))
		}
		put32(entry, 42, uint32(localStart)) // shifted LFH offset
		cd = append(cd, entry...)
		cd = append(cd, blob[off+46:off+46+nName]...)
		cd = append(cd, newExtra...)
		cd = append(cd, blob[off+46+nName+nExtra:bodyEnd]...)
		off = bodyEnd
	}
	if rebuilt != n {
		return blob
	}
	out = append(out, cd...)
	patched := append([]byte(nil), blob[eocd:eocd+22]...)
	put32(patched, 12, uint32(len(cd)))
	// The new central directory starts right where the relocated local data
	// ends: total-so-far minus the directory itself.
	put32(patched, 16, uint32(len(out)-len(cd)))
	return append(out, patched...)
}

// tclFindAll implements the sqlite test-suite findall proc (zipfile2.test):
// every index where NEEDLE occurs in HAYSTACK, overlapping occurrences
// excluded, returned as a TCL list ("i j k"; empty list "" when none).
// Indexes count RUNES; callers pass ASCII hex archives.
func tclFindAll(needle, haystack string) string {
	var out []string
	for i := 0; i+utf8.RuneCountInString(needle) <= utf8.RuneCountInString(haystack); {
		idx := strings.Index(haystack[i:], needle)
		if idx < 0 {
			break
		}
		out = append(out, strconv.Itoa(utf8.RuneCountInString(haystack[:i+idx])))
		i += idx + utf8.RuneCountInString(needle)
	}
	return strings.Join(out, " ")
}

// stripExtraField removes all fields with the given 2-byte id from a ZIP
// "extra field" block (id 2 bytes + size 2 bytes + payload), preserving the
// remaining fields verbatim.
func stripExtraField(extra []byte, id uint16) []byte {
	var out []byte
	for j := 0; j+4 <= len(extra); {
		sz := int(binary.LittleEndian.Uint16(extra[j+2 : j+4]))
		if j+4+sz > len(extra) {
			break
		}
		fieldID := binary.LittleEndian.Uint16(extra[j : j+2])
		if fieldID != id {
			out = append(out, extra[j:j+4+sz]...)
		}
		j += 4 + sz
	}
	return out
}

// catBytes concatenates byte slices for the crafted-archive builders.
func catBytes(bs ...[]byte) []byte {
	out := make([]byte, 0, 64)
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

// tclHexEncode implements TCL [binary encode hex S]: lowercase hex text of
// the byte string.
func tclHexEncode(s string) string {
	return hex.EncodeToString([]byte(s))
}

// tclDbOne implements TCL [db one SQL]: run SQL and return the first column
// of the first row as a TCL string. Blob results convert to their raw bytes
// (never fmt's decimal slice rendering); NULL becomes "".
func tclDbOne(db *frigolite.DB, sql string) string {
	r := db.Query(sql)
	if r.Error != nil || len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
		return ""
	}
	switch v := r.Rows[0][0].(type) {
	case nil:
		return ""
	case []byte:
		return string(v)
	default:
		return fmt.Sprintf("%%v", v)
	}
}

// tclMakeCorruptFile reimplements zipfile2.test's make_corrupt_file proc: it
// writes a crafted archive whose central directory claims a 60000-byte entry
// name and a 60000-byte extra field, with the local file header at offset 200
// and the central directory at offset 1000.
func tclMakeCorruptFile(fname string) {
	u16 := func(v int) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, uint16(v)); return b }
	u32 := func(v int) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, uint32(v)); return b }
	const (
		centralDirOffset = 1000
		lfhOffset        = 200
		nFile            = 60000
		nExtra           = 60000
	)
	lfh := catBytes(
		u32(0x04034b50), u16(20), u16(0), u16(0), u16(0), u16(0),
		u32(0), u32(0), u32(0), u16(1), u16(0),
		[]byte{'A'},
	)
	cds := catBytes(
		u32(0x02014b50),
		u16(0),               // version made by
		u16(20),              // version needed
		u16(0),               // flags
		u16(0),               // method
		u16(0), u16(0),       // dos time/date
		u32(0),               // crc
		u32(0),               // compressed size
		u32(0),               // uncompressed size
		u16(nFile),           // file name length
		u16(nExtra),          // extra length
		u16(0),               // comment length
		u16(0),               // disk start
		u16(0),               // internal attrs
		u32(0),               // external attrs
		u32(lfhOffset),       // local header offset
	)
	payload := append(bytes.Repeat([]byte("B"), nFile), bytes.Repeat([]byte("C"), nExtra)...)
	cdSize := len(cds) + len(payload)
	eocd := catBytes(
		u32(0x06054b50), u16(0), u16(0), u16(1), u16(1),
		u32(cdSize), u32(centralDirOffset), u16(0),
	)
	buf := bytes.Repeat([]byte("X"), lfhOffset)
	buf = append(buf, lfh...)
	for len(buf) < centralDirOffset {
		buf = append(buf, 0)
	}
	buf = append(buf, cds...)
	buf = append(buf, payload...)
	buf = append(buf, eocd...)
	os.WriteFile(fname, buf, 0644)
}

// --- vtabH filesystem fixture procs (src/test_fs.c corpus) ---

// tclSortFiles implements vtabH.test's sort_files {names {nocase false}}:
// lsort of a TCL list; -nocase applies only when both the nocase argument is
// true AND the platform is windows (the upstream proc guards on
// $::tcl_platform(platform), which is "unix" for every testgen run).
func tclSortFiles(names, nocase string) string {
	items := tclSplitList(names)
	if (nocase == "true" || nocase == "1") && runtime.GOOS == "windows" {
		sort.Slice(items, func(i, j int) bool {
			return strings.ToLower(items[i]) < strings.ToLower(items[j])
		})
	} else {
		sort.Strings(items)
	}
	return strings.Join(items, " ")
}

// tclGlobFS expands one filesystem glob pattern the way TCL's
// [glob -nocomplain -- PAT] does on unix: absolute matches, directories
// included, unmatched patterns yield no entries.
func tclGlobFS(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	return matches
}

// tclListRootFiles implements the unix branch of list_root_files: every
// top-level entry of "/", leading slash stripped, sorted. The dot-tail skip
// is upstream's windows-branch "file tail" filter, adopted unconditionally:
// macOS mounts carry dot entries (/.file, /.vol, ...) that the engine's
// fstree CTE must skip ("name NOT LIKE '.%%'"), and leaving them in the
// baseline would make vtabH 3.1's LIMIT/BFS parity assert an impossible set.
func tclListRootFiles() string {
	var out []string
	for _, p := range tclGlobFS("/*") {
		name := strings.TrimPrefix(p, "/")
		if strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	return tclSortFiles(strings.Join(out, " "), "")
}

// tclFileIsDir reports whether path exists and is a directory.
func tclFileIsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// tclListFiles implements the unix branch of list_files: expand one glob
// pattern against the filesystem and return the sorted matches.
func tclListFiles(pattern string) string {
	var out []string
	out = append(out, tclGlobFS(pattern)...)
	return tclSortFiles(strings.Join(out, " "), "")
}

// tclContents implements vtabH.test's contents {pattern}: recursive listing
// concatenating each glob match followed by the expansion of "<match>/*"
// when the match is a directory.
func tclContents(pattern string) string {
	var out []string
	for _, f := range tclGlobFS(pattern) {
		out = append(out, f)
		if tclFileIsDir(f) {
			out = append(out, tclSplitList(tclContents(f+"/*"))...)
		}
	}
	return strings.Join(out, " ")
}
`
