package main

const helpersTemplatePart1Tail = `func tclSplitList(s string) []string {
	var result []string
	pos := 0
	for pos < len(s) {
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
			pos++
		}
		// A backslash-newline is a TCL line continuation (semantically a
		// single space): consume it so list elements split correctly.
		if pos < len(s) && s[pos] == '\\' && pos+1 < len(s) && (s[pos+1] == '\n' || s[pos+1] == '\r') {
			pos++
			continue
		}
		if pos >= len(s) { break }
		switch s[pos] {
		case '{':
			depth := 1; start := pos + 1; pos++
			for pos < len(s) && depth > 0 {
				if s[pos] == '{' { depth++ }
				if s[pos] == '}' { depth-- }
				if depth > 0 { pos++ }
			}
			result = append(result, s[start:pos])
			if pos < len(s) { pos++ }
		case '"':
			start := pos + 1
			pos++
			for pos < len(s) && s[pos] != '"' {
				if s[pos] == '\\' && pos+1 < len(s) {
					pos += 2
					continue
				}
				pos++
			}
			result = append(result, tclUnescapeQuoted(s[start:pos]))
			if pos < len(s) { pos++ }
		default:
			start := pos
			for pos < len(s) && s[pos] != ' ' && s[pos] != '\t' && s[pos] != '\n' && s[pos] != '\r' { pos++ }
			result = append(result, s[start:pos])
		}
	}
	return result
}

// tclSplitString ports TCL [split str ?splitChars?] as a value: the result
// is the TCL list string of parts. An empty splitChars splits the string
// into individual characters; the whitespace default (emitted by the
// transpiler when splitChars is omitted) treats each of " \n\t\r" as a
// split point; otherwise every character in splitChars is a split point and
// consecutive split characters yield empty elements (Tcl_SplitObjCmd,
// generic/tclCmdMZ.c).
func tclSplitString(s string, sep string) string {
	if sep == "" {
		var chars []string
		for _, r := range s {
			chars = append(chars, string(r))
		}
		return tclList(chars)
	}
	splitSet := map[rune]bool{}
	for _, r := range sep {
		splitSet[r] = true
	}
	parts := []string{}
	cur := ""
	for _, r := range s {
		if splitSet[r] {
			parts = append(parts, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return tclList(append(parts, cur))
}

func tclNeedsBracing(s string) bool {
	if s == "" { return true }
	for _, c := range s {
		switch c { case ' ', '\t', '\n', '\r', '{', '}', '"', ';': return true }
	}
	return false
}

func tclLIndex(list string, idx interface{}) string {
	items := tclSplitList(list)
	var i int
	switch v := idx.(type) {
	case int:
		i = v
	case string:
		i, _ = strconv.Atoi(v)
	default:
		return ""
	}
	if i < 0 || i >= len(items) { return "" }
	return items[i]
}

func tclLLength(list string) int { return len(tclSplitList(list)) }

// tclLsearch returns the index of value in the TCL list, or -1 when absent
// (TCL lsearch semantics). Used for [lsearch $list $value] command
// substitutions in conditions, e.g. {[lsearch $exprkw $kw]<0}.
func tclLsearch(list string, value string) int {
	for i, item := range tclSplitList(list) {
		if item == value {
			return i
		}
	}
	return -1
}

func tclLRange(list string, start, end interface{}) string {
	items := tclSplitList(list)
	s, _ := strconv.Atoi(fmt.Sprintf("%%v", start))
	// "end" means the last element (TCL lrange semantics); a numeric end is
	// clamped to the list bounds.
	e := len(items) - 1
	if es, ok := end.(string); ok && es != "end" {
		e, _ = strconv.Atoi(es)
	}
	if s < 0 { s = 0 }
	if e < 0 || e >= len(items) { e = len(items) - 1 }
	if s > e || s >= len(items) { return "" }
	return tclList(items[s : e+1])
}

func tclLReplace(list string, first, count interface{}, args ...string) string {
	items := tclSplitList(list)
	f := toInt(first)
	c := toInt(count)
	// TCL lreplace: a negative first means "from end" (e.g. -1 == end).
	if f < 0 {
		f = len(items) + f
	}
	if f < 0 {
		f = 0
	}
	if f > len(items) {
		f = len(items)
	}
	// TCL lreplace: a negative count means "all remaining elements".
	if c < 0 {
		c = len(items) - f
	}
	end := f + c
	if end < f {
		end = f
	}
	if end > len(items) {
		end = len(items)
	}
	repl := args
	items = append(items[:f], append(repl, items[end:]...)...)
	return tclList(items)
}

// tclMakeStr implements the autovacuum.test / incrvacuum*.test 'make_str'
// helper: build a string of LEN characters by repeating CHAR (and the literal
// "." character as the unit — the proc body uses "string repeat CHAR. LEN"
// where the trailing "." is part of the unit, NOT a TCL syntax artifact). The
// result is truncated to exactly LEN characters.
//
//	tclMakeStr("abc", 8) -> "abcabcab"
//
// This matches TCL:
//	[string repeat "abc." 8] -> "abc.abc.abc.abc.abc.abc.abc.abc."
//	[string range ... 0 7]   -> "abcabcab"
func tclMakeStr(char string, length int) string {
	unit := char + "."
	var sb strings.Builder
	for sb.Len() < length {
		sb.WriteString(unit)
	}
	return sb.String()[:length]
}

// tclToInt parses a TCL value as an int (decimal or 0x...); used by the
// make_str / file_pages 2-arg special-func templates to convert a TCL-string
// length/size argument into a Go int at the call site.
func tclToInt(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// tclFilePages returns the page count of the test database by reading the
// in-header db size (offset 28) and dividing by the page size. The page size
// is the user-configured one (1024 in autovacuum.test, 4096 in
// incrvacuum*.test). autovacuum.test's 'file_pages' proc is hard-coded to 1024
// (its body literally is '[expr [file size test.db] / 1024]'); incrvacuum*.test
// uses 4096 implicitly via a similar proc. The transpiler wires both via
// tclFilePages(name) so the runtime helper picks the right denominator.
func tclFilePages(name string) int {
	size := tclFileSize(name)
	if size <= 0 {
		return 0
	}
	// autovacuum.test runs at page_size=1024; incrvacuum*.test at 4096.
	// Default to 1024 (the autovacuum.test convention) — the incrvacuum tests
	// register a different proc body (a 'file_pages' proc with / 4096) so
	// they get their own tclFilePages4096 path via a different template.
	ps := 1024
	return size / ps
}

// tclConcat implements TCL's 'concat' command on N string arguments. Each
// argument is a TCL list; the result is the space-joined concatenation of
// every element of every list. Used by autovacuum.test 1.x's
// '[eval concat $delete_order]' to flatten a list-of-lists into a single
// space-separated list before [lsort -integer] sorts it. Returns a flat
// list string (no brace-wrapping) so tclSplitList on the result yields the
// expected per-element tokens.
func tclConcat(args ...string) string {
	var out []string
	for _, a := range args {
		out = append(out, tclSplitList(a)...)
	}
	return strings.Join(out, " ")
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case int: return x
	case int64: return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}

func tclSort(list string) string {
	items := tclSplitList(list)
	sort.Strings(items)
	return tclList(items)
}

// tclSortInt implements TCL's lsort -integer: numeric ascending order.
func tclSortInt(list string) string {
	items := tclSplitList(list)
	ns := make([]int64, len(items))
	for i, it := range items {
		ns[i], _ = strconv.ParseInt(it, 10, 64)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i] < ns[j] })
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.FormatInt(n, 10)
	}
	return tclList(out)
}

// tclSortIntDesc implements TCL's lsort -integer -decreasing.
func tclSortIntDesc(list string) string {
	items := tclSplitList(list)
	ns := make([]int64, len(items))
	for i, it := range items {
		ns[i], _ = strconv.ParseInt(it, 10, 64)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i] > ns[j] })
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.FormatInt(n, 10)
	}
	return tclList(out)
}

// tclSortDesc implements TCL's lsort -decreasing (string compare).
func tclSortDesc(list string) string {
	items := tclSplitList(list)
	sort.Sort(sort.Reverse(sort.StringSlice(items)))
	return tclList(items)
}

// tclMakeExpr1 implements rowvalue2's make_expr1: (c1, c2) OP (v1, v2).
func tclMakeExpr1(cList, vList, op string) string {
	cs := tclSplitList(cList)
	vs := tclSplitList(vList)
	return "(" + strings.Join(cs, ", ") + ") " + op + " (" + strings.Join(vs, ", ") + ")"
}

// tclMakeExpr3 implements rowvalue2's make_expr3: a prefix of equalities plus
// one final OP comparison: (c0==v0 AND c1==v1 AND ... AND cN OP vN).
func tclMakeExpr3(cList, vList, op string) string {
	cs := tclSplitList(cList)
	vs := tclSplitList(vList)
	var parts []string
	for i := 0; i+1 < len(cs); i++ {
		parts = append(parts, cs[i]+" == "+vs[i])
	}
	parts = append(parts, cs[len(cs)-1]+" "+op+" "+vs[len(vs)-1])
	return "(" + strings.Join(parts, " AND ") + ")"
}

// tclMakeExpr2 implements rowvalue2's make_expr2: row-value comparison
// expanded per SQLite's lexicographic row-value semantics.
func tclMakeExpr2(cList, vList, op string) string {
	cs := tclSplitList(cList)
	vs := tclSplitList(vList)
	switch op {
	case "==", "IS":
		var parts []string
		for i := range cs {
			parts = append(parts, "("+cs[i]+" "+op+" "+vs[i]+")")
		}
		return strings.Join(parts, " AND ")
	case "<", ">":
		var parts []string
		for i := range cs {
			parts = append(parts, tclMakeExpr3(tclList(cs[:i+1]), tclList(vs[:i+1]), op))
		}
		return strings.Join(parts, " OR ")
	case "<=", ">=":
		o2 := op[:1]
		var parts []string
		for i := 0; i+1 < len(cs); i++ {
			parts = append(parts, tclMakeExpr3(tclList(cs[:i+1]), tclList(vs[:i+1]), o2))
		}
		parts = append(parts, tclMakeExpr3(cList, vList, op))
		return strings.Join(parts, " OR ")
	}
	return ""
}

func tclRegexp(pattern, str string) string {
	matched, _ := regexp.MatchString(pattern, str)
	if matched { return "1" }
	return "0"
}

func tclRegsub(pattern, str, replacement string) string {
	re, err := regexp.Compile(pattern)
	if err != nil { return str }
	return re.ReplaceAllString(str, replacement)
}

func tclRegsubAll(pattern, str, replacement string) string {
	return tclRegsub(pattern, str, replacement)
}

func tclStringMatch(pattern, str string) bool {
	// Convert TCL glob pattern to Go regexp
	goPattern := ""
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*': goPattern += ".*"
		case '?': goPattern += "."
		case '.', '+', '(', ')', '|', '^', '$': goPattern += "\\" + string(c)
		default: goPattern += string(c)
		}
	}
	matched, _ := regexp.MatchString("^"+goPattern+"$", str)
	return matched
}

// tclStringMatch01 is tclStringMatch rendered as a TCL boolean string
// ("1"/"0") for use in expression contexts ([string match ...] inside
// conditions or concatenations, where the command value is a string).
func tclStringMatch01(pattern, str string) string {
	if tclStringMatch(pattern, str) {
		return "1"
	}
	return "0"
}

func tclFileCopy(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil { return }
	os.WriteFile(dst, data, 0644)
}

// tclHexioWrite implements the test framework's hexio_write: decode hexStr
// and write the bytes to file at byte offset (the file is patched in place;
// corruption tests rely on the engine re-reading the patched header).
func tclHexioWrite(file string, offset int64, hexStr string) {
	var data []byte
	// The hexio_render_int{8,16,32} N form (a helper in sqlite/test/hexio.test)
	// formats an integer as big-endian bytes. Without parsing this form, the
	// corrupt* tests that do "tclHexioWrite $db $off hexio_render_int32 2"
	// silently no-op (hex.DecodeString rejects the literal and returns),
	// leaving the DB unpatched.
	if strings.HasPrefix(hexStr, "hexio_render_int") {
		parts := strings.Fields(hexStr)
		if len(parts) == 2 {
			n, _ := strconv.ParseInt(parts[1], 0, 64)
			switch parts[0] {
			case "hexio_render_int8":
				data = []byte{byte(n)}
			case "hexio_render_int16":
				data = []byte{byte(n >> 8), byte(n)}
			case "hexio_render_int32":
				data = []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
			default:
				return
			}
		}
	} else {
		var err error
		data, err = hex.DecodeString(hexStr)
		if err != nil {
			return
		}
	}
	if len(data) == 0 {
		return
	}
	f, err := os.OpenFile(file, os.O_RDWR, 0644)
	if err != nil { return }
	defer f.Close()
	f.WriteAt(data, offset)
}

func tclGlob(pattern string) string {
	matches, _ := filepath.Glob(pattern)
	return tclList(matches)
}
// tclExtractOffsets ports fts3offsets.test's extract(offsets, text) proc:
// given an offsets() flat 4-tuple list (phrase, column, start, length) and
// the document text, return the text with each matched span wrapped in
// parentheses, spans ordered by start byte (lsort -integer -index 0).
func tclExtractOffsets(offsets, text string) string {
	items := tclSplitList(offsets)
	type rng struct{ s, n int }
	var off []rng
	for i := 0; i+3 < len(items); i += 4 {
		s, _ := strconv.Atoi(items[i+2])
		n, _ := strconv.Atoi(items[i+3])
		off = append(off, rng{s, n})
	}
	sort.Slice(off, func(i, j int) bool { return off[i].s < off[j].s })
	var b strings.Builder
	iOff := 0
	for _, r := range off {
		if r.s > iOff && r.s <= len(text) {
			b.WriteString(text[iOff:r.s])
		}
		b.WriteByte('(')
		end := r.s + r.n
		if end > len(text) {
			end = len(text)
		}
		if r.s < end {
			b.WriteString(text[r.s:end])
		}
		b.WriteByte(')')
		if r.s+r.n > iOff {
			iOff = r.s + r.n
		}
	}
	if iOff < len(text) {
		b.WriteString(text[iOff:])
	}
	return b.String()
}


// tclArrayGetFlat returns the flattened key/value pairs of a dynamic-key
// array map (the [array get VAR] value), keys in sorted order.
func tclArrayGetFlat(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k, m[k])
	}
	return tclList(out)
}

// tclRowFlatPairs renders one result row as flattened column/value pairs
// (the [array get VAR] value inside a db-eval row callback).
func tclRowFlatPairs(cols []string, row []interface{}) string {
	var out []string
	for i, c := range cols {
		v := ""
		if i < len(row) && row[i] != nil {
			v = fmt.Sprint(row[i])
		}
		out = append(out, c, v)
	}
	return tclList(out)
}


`
