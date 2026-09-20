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
// and the TCL braced expected value. An empty value follows tclListFlatten's
// "{}" convention (TCL renders an empty list/element as {}), so the empty
// TAIL of a single-statement prepare (capi3-1.1) compares equal to the {}
// expected value — "" and "{}" must not normalize to different strings.
func tclListFlattenCollapse(s string) string {
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

// tclCatchStmtResult maps a stmt-API helper's returned code string to the
// TCL-visible result of "catch {sqlite3_* ...} var": SQLITE_OK (and empty)
// mean the call succeeded and left no interpreter result (bind/reset/
// finalize), while every other code — including SQLITE_ROW/SQLITE_DONE from
// step — is itself the result or error message.
func tclCatchStmtResult(code string) string {
	if code == "SQLITE_OK" || code == "" {
		return ""
	}
	return code
}

// tclPrepareCatchErr formats a failed sqlite3_prepare's TCL error the way
// the test1.c test_prepare wrapper does: "(<code>) <errmsg>"
// (sqllimits1-6.3 expects "(18) statement too long").
func tclPrepareCatchErr(db *frigolite.DB, code string) error {
	msg := "unknown error"
	if db != nil && db.LastErr() != "" {
		msg = db.LastErr()
	}
	return fmt.Errorf("(%%d) %%s", sqliteErrCodeNum(code), msg)
}

// sqliteErrCodeNum maps the primary SQLITE_* result-code names to their
// numeric values (sqlite3.h).
func sqliteErrCodeNum(code string) int {
	switch code {
	case "SQLITE_OK":
		return 0
	case "SQLITE_ERROR":
		return 1
	case "SQLITE_INTERNAL":
		return 2
	case "SQLITE_PERM":
		return 3
	case "SQLITE_ABORT":
		return 4
	case "SQLITE_BUSY":
		return 5
	case "SQLITE_LOCKED":
		return 6
	case "SQLITE_NOMEM":
		return 7
	case "SQLITE_READONLY":
		return 8
	case "SQLITE_INTERRUPT":
		return 9
	case "SQLITE_IOERR":
		return 10
	case "SQLITE_CORRUPT":
		return 11
	case "SQLITE_NOTFOUND":
		return 12
	case "SQLITE_FULL":
		return 13
	case "SQLITE_CANTOPEN":
		return 14
	case "SQLITE_PROTOCOL":
		return 15
	case "SQLITE_EMPTY":
		return 16
	case "SQLITE_SCHEMA":
		return 17
	case "SQLITE_TOOBIG":
		return 18
	case "SQLITE_CONSTRAINT":
		return 19
	case "SQLITE_MISMATCH":
		return 20
	case "SQLITE_MISUSE":
		return 21
	case "SQLITE_NOLFS":
		return 22
	case "SQLITE_AUTH":
		return 23
	case "SQLITE_FORMAT":
		return 24
	case "SQLITE_RANGE":
		return 25
	case "SQLITE_NOTADB":
		return 26
	case "SQLITE_NOTICE":
		return 27
	case "SQLITE_WARNING":
		return 28
	}
	return 1
}

func tclPrepareStep(db *frigolite.DB, sqlText, name string) {
	if db == nil { return }
	stmt, err := db.Prepare(sqlText)
	if err != nil { return }
	tclPrepared[name] = stmt
	// vdbeapi.c sqlite3_step: the first step materializes the rows, leaves
	// row 0 current so sqlite3_column_text reads after prepare+step see the
	// first row (rtree8-1.3.2), and — while rows remain — holds the
	// statement's prepared read lock so the database reports "in use" for a
	// concurrent backup destination (backup5-1.4).
	_, serr := stmt.Step()
	r := stmt.StepResult()
	if r == nil {
		r = &frigolite.Result{}
	}
	if serr != nil && r.Error == nil {
		r = &frigolite.Result{Error: serr}
	}
	tclLastStep[name] = &tclStepState{r: r, row: 0}
	if r.Error != nil && db != nil {
		db.SetLastErr(r.Error.Error(), db.ErrorCodeFor(r.Error))
	}
}

// tclNumberName ports the test-suite number_name proc (speed1p/speed2/
// speed3.test): converts an integer to English words ("one hundred twenty
// three"), used to build deterministic wide text payloads.
func tclNumberName(n int) string {
	ones := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen",
		"eighteen", "nineteen"}
	tens := []string{"", "ten", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	txt := ""
	if n >= 1000 {
		txt = tclNumberName(n/1000) + " thousand"
		n = n %% 1000
	}
	if n >= 100 {
		txt += " " + ones[n/100] + " hundred"
		n = n %% 100
	}
	if n >= 20 {
		txt += " " + tens[n/10]
		n = n %% 10
	}
	if n > 0 {
		txt += " " + ones[n]
	}
	txt = strings.TrimSpace(txt)
	if txt == "" {
		txt = "zero"
	}
	return txt
}

// tclStepEmulated runs one legacy-emulation step of the named statement and
// leaves the step state current for sqlite3_column_* reads (queries read
// rows; writes report only the error state — vdbeapi.c sqlite3_step).
func tclStepEmulated(db *frigolite.DB, name, sqlText string) {
	if db == nil {
		return
	}
	var r *frigolite.Result
	if tclIsQuerySQL(sqlText) {
		r = db.Query(sqlText)
	} else {
		r = db.Exec(sqlText)
	}
	tclLastStep[name] = &tclStepState{r: r, row: 0}
	if r.Error != nil {
		db.SetLastErr(r.Error.Error(), db.ErrorCodeFor(r.Error))
	}
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
	// prepare.c:766-774: an explicit nByte larger than
	// SQLITE_LIMIT_SQL_LENGTH fails SQLITE_TOOBIG "statement too long"
	// before any parsing; the message stays as the connection's last error
	// (sqlite3_errmsg — sqllimits1-6.3/6.4).
	if nByte >= 0 && nByte > db.Limit("SQLITE_LIMIT_SQL_LENGTH") {
		db.SetLastErr("statement too long", "SQLITE_TOOBIG")
		return "SQLITE_TOOBIG"
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
// tclCksum replicates tester.tcl's cksum proc: a fingerprint over
// sqlite_master (name/type/sql ordered by name), every table's full
// contents (tables ordered by name), and the default_synchronous /
// default_cache_size pragmas, rendered as "LEN-MD5".
func tclCksum(db *frigolite.DB) string {
	var txt []byte
	appendQuery := func(sql string) {
		r := db.Query(sql)
		if r.Error != nil {
			return
		}
		for _, row := range r.Rows {
			for _, v := range row {
				if v != nil {
					txt = append(txt, []byte(fmt.Sprint(v))...)
				}
			}
		}
		txt = append(txt, '\n')
	}
	appendQuery("SELECT name, type, sql FROM sqlite_master ORDER BY name")
	var tables []string
	if r := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"); r.Error == nil {
		for _, row := range r.Rows {
			if len(row) > 0 && row[0] != nil {
				tables = append(tables, fmt.Sprint(row[0]))
			}
		}
	}
	for _, tbl := range tables {
		appendQuery("SELECT * FROM " + quoteTableName(tbl))
	}
	for _, prag := range []string{"default_synchronous", "default_cache_size"} {
		txt = append(txt, []byte(prag+"-")...)
		appendQuery("PRAGMA " + prag)
	}
	h := md5.New()
	h.Write(txt)
	return fmt.Sprintf("%%d-%%s", len(txt), hex.EncodeToString(h.Sum(nil)))
}
` + helpersTemplatePart2TailMid
