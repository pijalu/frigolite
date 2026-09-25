// Package main implements the tcl2go tool.
//
// This file contains the [string ...] and TCL list command expression
// handlers ([list], [lindex], [llength], [lsearch], [lrange], [lreplace],
// [lsort], [split], [join], [concat]) plus the [binary encode/decode hex]
// codec.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// cmdExprStringSubs dispatches [string SUB ...] value substitution to the
// per-subcommand handler (the subcommand word is args[0]). Built lazily
// (see cmdExprStringSubsRef): handler bodies transitively reference cmdExpr
// through the string-expression builders, so a package-level literal would
// create an initialization cycle.
var (
	cmdExprStringSubsOnce sync.Once
	cmdExprStringSubs     map[string]cmdExprHandler
)

// cmdExprStringSubsRef returns the [string ...] subcommand dispatch table,
// building it on first use.
func cmdExprStringSubsRef() map[string]cmdExprHandler {
	cmdExprStringSubsOnce.Do(func() {
		cmdExprStringSubs = map[string]cmdExprHandler{
			"map":       (*transpiler).cmdExprStringMap,
			"length":    (*transpiler).cmdExprStringLength,
			"tolower":   (*transpiler).cmdExprStringToLower,
			"toupper":   (*transpiler).cmdExprStringToUpper,
			"trim":      (*transpiler).cmdExprStringTrimFrom,
			"trimleft":  (*transpiler).cmdExprStringTrimFrom,
			"trimright": (*transpiler).cmdExprStringTrimFrom,
			"match":     (*transpiler).cmdExprStringMatch,
			"range":     (*transpiler).cmdExprStringRange,
			"index":     (*transpiler).cmdExprStringIndex,
			"repeat":    (*transpiler).cmdExprStringRepeat,
			"replace":   (*transpiler).cmdExprStringReplace,
		}
	})
	return cmdExprStringSubs
}

// cmdExprString handles `[string ...]` subcommands (map, length, tolower,
// toupper, trim, range, repeat).
func (tp *transpiler) cmdExprString(cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `""`
	}
	if h, ok := cmdExprStringSubsRef()[args[0]]; ok {
		return h(tp, cmdName, cmdText, args)
	}
	str := strings.TrimSpace(cmdText[len("string "+args[0]):])
	return fmt.Sprintf("%q", str)
}

// cmdExprStringLength renders [string length STR].
func (tp *transpiler) cmdExprStringLength(cmdName, cmdText string, args []string) string {
	return tp.cmdExprStringUnary("length", args)
}

// cmdExprStringToLower renders [string tolower STR].
func (tp *transpiler) cmdExprStringToLower(cmdName, cmdText string, args []string) string {
	return tp.cmdExprStringUnary("tolower", args)
}

// cmdExprStringToUpper renders [string toupper STR].
func (tp *transpiler) cmdExprStringToUpper(cmdName, cmdText string, args []string) string {
	return tp.cmdExprStringUnary("toupper", args)
}

// cmdExprStringTrimFrom renders [string trim|trimleft|trimright STR ?chars?];
// the subcommand word (args[0]) selects the trim side.
func (tp *transpiler) cmdExprStringTrimFrom(cmdName, cmdText string, args []string) string {
	return tp.cmdExprStringTrim(args[0], args)
}

// cmdExprStringMatch renders [string match PATTERN STR] — TCL glob match; the
// result is a "1"/"0" string so callers wrapping it in tclBool(...) (condition
// path) or concatenating it still type-check.
func (tp *transpiler) cmdExprStringMatch(cmdName, cmdText string, args []string) string {
	if len(args) < 3 {
		return `""`
	}
	patternExpr := tp.buildStringExpr(args[1])
	strExpr := tp.buildStringExpr(args[2])
	return fmt.Sprintf("tclStringMatch01(%s, %s)", patternExpr, strExpr)
}

// cmdExprStringRange renders [string range STR START END].
func (tp *transpiler) cmdExprStringRange(cmdName, cmdText string, args []string) string {
	if len(args) < 4 {
		return `""`
	}
	strExpr := tp.buildStringExpr(args[1])
	startExpr := tp.buildStringExpr(args[2])
	endExpr := tp.buildStringExpr(args[3])
	return fmt.Sprintf("tclStringRange(%s, %s, %s)", strExpr, startExpr, endExpr)
}

// cmdExprStringIndex renders [string index STR IDX].
func (tp *transpiler) cmdExprStringIndex(cmdName, cmdText string, args []string) string {
	if len(args) < 3 {
		return `""`
	}
	strExpr := tp.buildStringExpr(args[1])
	idxExpr := tp.buildStringExpr(args[2])
	return fmt.Sprintf("tclStringIndex(%s, %s)", strExpr, idxExpr)
}

// cmdExprStringRepeat renders [string repeat STR N]. The count is rendered as
// a string by TCL; tclStringRepeat converts it at runtime (the expression
// context cannot emit a typed int).
func (tp *transpiler) cmdExprStringRepeat(cmdName, cmdText string, args []string) string {
	if len(args) < 3 {
		return `""`
	}
	strExpr := tp.buildStringExpr(args[1])
	nExpr := tp.buildStringExpr(args[2])
	return fmt.Sprintf("tclStringRepeat(%s, %s)", strExpr, nExpr)
}

// cmdExprStringReplace renders [string replace S FIRST LAST NEWSTR]
// (zipfile2 patches archive bytes).
func (tp *transpiler) cmdExprStringReplace(cmdName, cmdText string, args []string) string {
	if len(args) < 5 {
		return `""`
	}
	return fmt.Sprintf("tclStringReplace(%s, %s, %s, %s)",
		tp.buildStringExpr(args[1]), tp.buildStringExpr(args[2]),
		tp.buildStringExpr(args[3]), tp.buildStringExpr(args[4]))
}

// cmdExprStringUnary renders a unary [string OP STR] expression (length,
// tolower, toupper, trim). memdb1.test's [string length $::db1] reads the
// serialize image shadow (db1Blob), not the *frigolite.DB connection var.
func (tp *transpiler) cmdExprStringUnary(op string, args []string) string {
	if len(args) < 2 {
		return cmdExprStringUnaryDefault(op)
	}
	strExpr := tp.buildStringExpr(strings.Join(args[1:], " "))
	switch op {
	case "length":
		return fmt.Sprintf("strconv.Itoa(len(%s))", strExpr)
	case "tolower":
		return fmt.Sprintf("strings.ToLower(%s)", strExpr)
	case "toupper":
		return fmt.Sprintf("strings.ToUpper(%s)", strExpr)
	default:
		return fmt.Sprintf("strings.TrimSpace(%s)", strExpr)
	}
}

// cmdExprStringUnaryDefault returns the default result for a unary string
// expression with too few arguments.
func cmdExprStringUnaryDefault(op string) string {
	if op == "length" {
		return `"0"`
	}
	return `""`
}

// cmdExprStringTrim renders [string trim|trimleft|trimright STR ?chars?] —
// strip the given characters (default whitespace) from the start/end of STR.
// The charset is a TCL string of characters, each of which is trimmed (not a
// substring); Go's strings.Trim/TrimLeft/TrimRight match this behavior.
func (tp *transpiler) cmdExprStringTrim(op string, args []string) string {
	if len(args) < 2 {
		return cmdExprStringUnaryDefault(op)
	}
	strExpr := tp.buildStringExpr(args[1])
	charsExpr := `" \t\n\r\v\f"`
	if len(args) >= 3 {
		charsExpr = tp.buildStringExpr(args[2])
	}
	switch op {
	case "trim":
		return fmt.Sprintf("strings.Trim(%s, %s)", strExpr, charsExpr)
	case "trimleft":
		return fmt.Sprintf("strings.TrimLeft(%s, %s)", strExpr, charsExpr)
	default:
		return fmt.Sprintf("strings.TrimRight(%s, %s)", strExpr, charsExpr)
	}
}

// cmdExprStringMap handles `[string map {old new ...} $str]` →
// strings.ReplaceAll. The map is parsed from cmdText since braces aren't split
// properly by Fields. A `[string map [list old new] $str]` form (the map is
// itself a list command, often with a runtime $var replacement) is translated
// to runtime strings.ReplaceAll with the variable's Go value.
func (tp *transpiler) cmdExprStringMap(cmdName, cmdText string, args []string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(cmdText, "string map"))
	if os.Getenv("TCLDBG") != "" {
		fmt.Fprintf(os.Stderr, "SMAP rest=%q args=%q\n", rest, args)
	}
	if len(rest) < 2 {
		return `""`
	}
	// `string map [list OLD NEW] $str` — map is a list-command with the
	// replacement possibly a runtime $var. Emit strings.ReplaceAll with the
	// runtime values.
	if strings.HasPrefix(rest, "[list ") {
		return tp.cmdExprStringMapList(rest)
	}
	if rest[0] != '{' {
		return `""`
	}
	return tp.cmdExprStringMapBraced(rest)
}

// cmdExprStringMapList renders the `string map [list OLD NEW] $str` form.
func (tp *transpiler) cmdExprStringMapList(rest string) string {
	closeIdx := strings.Index(rest, "]")
	if closeIdx < 0 {
		return `""`
	}
	listContent := strings.TrimSpace(rest[5:closeIdx])
	strPart := strings.TrimSpace(rest[closeIdx+1:])
	// The SQL operand is usually a braced TCL word ({ ... }) whose outer
	// braces are list-delimiter syntax, not SQL content — strip one
	// balanced brace layer.
	if len(strPart) >= 2 && strPart[0] == '{' && strPart[len(strPart)-1] == '}' {
		strPart = strPart[1 : len(strPart)-1]
	}
	items := tclCmdWords(listContent)
	strExpr := tp.buildStringExpr(strPart)
	if len(items) >= 2 {
		oldExpr := tp.buildStringExpr(items[0])
		newExpr := tp.buildStringExpr(items[1])
		return fmt.Sprintf("strings.ReplaceAll(%s, %s, %s)", strExpr, oldExpr, newExpr)
	}
	return strExpr
}

// cmdExprStringMapBraced renders the `string map {OLD NEW} $str` form.
func (tp *transpiler) cmdExprStringMapBraced(rest string) string {
	// Find matching close brace for mapping
	depth := 0
	mapEnd := -1
	for i, c := range rest {
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
		}
		if depth == 0 {
			mapEnd = i
			break
		}
	}
	if mapEnd < 0 {
		return `""`
	}
	mapContent := rest[1:mapEnd]
	strPart := strings.TrimSpace(rest[mapEnd+1:])
	// Parse the map pairs with the TCL tokenizer so braced replacement values
	// (e.g. {"newname"} → "newname") keep their inner content without the
	// list-rendering braces (altertab2-3.$tn: string map {log_entry
	// {"newname"}} must emit "newname", not {"newname"}).
	items := tclCmdWords(mapContent)
	strExpr := tp.buildStringExpr(strPart)
	if len(items) >= 2 {
		return fmt.Sprintf("strings.ReplaceAll(%s, %q, %q)", strExpr, items[0], items[1])
	}
	return strExpr
}

// cmdExprBinary handles [binary encode hex S] / [binary decode hex S] value
// substitution: hex codec between byte strings and their lowercase text.
func (tp *transpiler) cmdExprBinary(cmdName, cmdText string, args []string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(cmdText, "binary"))
	fields := strings.Fields(rest)
	if len(fields) < 3 {
		return `""`
	}
	op := fields[0] + " " + fields[1]
	valWord := strings.TrimSpace(strings.TrimPrefix(rest, op))
	argExpr := tp.buildStringExpr(valWord)
	switch op {
	case "encode hex":
		return fmt.Sprintf("tclHexEncode(%s)", argExpr)
	case "decode hex":
		return fmt.Sprintf("string(tclHexDecode(%s))", argExpr)
	}
	return `""`
}

// listElem is one parsed [list ...] element: expr is the Go expression (or
// quoted literal), literal marks braced literal words, and text0 keeps the
// original literal text for unquote fallbacks.
type listElem struct {
	expr    string
	literal bool
	text0   string // original literal text when literal=true
}

// tclListElementRepr renders one list element in its TCL list string
// representation: values containing whitespace, braces, quotes, or the
// empty string are wrapped in ONE brace level so a downstream runtime
// tclListFlatten (which strips exactly one brace level per element)
// reproduces the original value — e.g. {"b":9} → {{"b":9}} → flatten →
// {"b":9}, and "" → {} → flatten → {} (matching flatten()'s NULL/empty
// cell rendering). Unbalanced braces cannot be braced; backslash-escape
// them instead (TCL braced words require balanced braces).
func tclListElementRepr(v string) string {
	if v == "" {
		return "{}"
	}
	needsBrace := strings.ContainsAny(v, " \t\n\r{}\"")
	depth := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				needsBrace = false // unbalanced: escape instead
			}
		}
	}
	if depth != 0 {
		needsBrace = false
	}
	if needsBrace {
		return "{" + v + "}"
	}
	// Unbalanced braces cannot be braced; escape the list-structural
	// characters instead (braces, quotes, whitespace). Brackets, $ and ;
	// are literal inside a list element and need no escaping.
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case ' ', '\t', '\n', '\r', '{', '}', '"':
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// cmdExprList handles `[list $var]` — constructs a TCL list. A single element
// renders as its value; multiple elements join with spaces (TCL list
// rendering).
func (tp *transpiler) cmdExprList(cmdName, cmdText string, args []string) string {
	// Re-parse the command words so each element's TCL quoting mode is
	// known: BRACED words are literals; their content must not undergo
	// $var or [cmd] substitution. Unbraced/quoted words substitute.
	raws := tcl.ParseCommands(strings.TrimSpace(cmdText))
	if len(raws) == 0 || len(raws[0]) < 1 {
		// Fallback: no parseable words — substitute every arg text.
		return tp.cmdExprListFallback(args)
	}
	clean := cmdExprListElems(tp, raws[0])
	if len(clean) == 0 {
		return `""`
	}
	if cmdExprListAllLiteral(clean) {
		return cmdExprListLiteralJoin(clean)
	}
	return cmdExprListMixedJoin(clean)
}

// cmdExprListFallback renders a [list ...] whose words did not re-parse:
// substitute every arg text and join with spaces.
func (tp *transpiler) cmdExprListFallback(args []string) string {
	if len(args) == 0 {
		return `""`
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = tp.buildStringExpr(a)
	}
	return strings.Join(parts, `+" "+`)
}

// cmdExprListElems parses the element words of a [list ...] command (words
// after the leading "list" word) into element descriptors.
func cmdExprListElems(tp *transpiler, words []tcl.RawWord) []listElem {
	var clean []listElem
	appendSubst := func(text string) {
		clean = append(clean, listElem{expr: tp.buildStringExpr(text)})
	}
	for i := 1; i < len(words); i++ { // words[0] is the "list" word
		w := words[i]
		// A lone backslash is a line-continuation remnant (backslash-newline
		// before `]`), not a list element — TCL folds it away.
		if !w.Braced && !w.Quoted && strings.TrimSpace(w.Text) == "\\" {
			continue
		}
		switch {
		case strings.HasPrefix(w.Text, "{*}"):
			// TCL `{*}` splice marker: value's elements join the list.
			appendSubst(strings.TrimPrefix(w.Text, "{*}"))
		case w.Text == "*":
			// Splice marker word followed by the spliced value; a
			// multi-line braced list value flattens to its space-joined
			// form (the shape flatten() produces).
			if i+1 < len(words) {
				i++
				spliced := words[i].Text
				if flat, ok := flattenBraceList(spliced); ok {
					spliced = flat
				}
				appendSubst(spliced)
			}
		case w.Braced:
			clean = append(clean, listElem{expr: strconv.Quote(w.Text), literal: true, text0: w.Text})
		case w.Quoted:
			appendSubst(tclUnescapeQuoted(w.Text))
		default:
			appendSubst(w.Text)
		}
	}
	return clean
}

// listElemText unquotes a literal element's Go expression back to its text,
// falling back to the original word when unquoting fails.
func listElemText(el listElem) string {
	u, err := strconv.Unquote(el.expr)
	if err != nil {
		u = el.text0
	}
	return u
}

// cmdExprListAllLiteral reports whether every parsed element is a braced
// literal.
func cmdExprListAllLiteral(clean []listElem) bool {
	for _, el := range clean {
		if !el.literal {
			return false
		}
	}
	return true
}

// cmdExprListLiteralJoin joins all-literal elements into one quoted TCL list
// string. Each element is emitted in TCL list representation so a runtime
// tclListFlatten round-trips braced/empty data elements exactly.
func cmdExprListLiteralJoin(clean []listElem) string {
	lits := make([]string, len(clean))
	for i, el := range clean {
		lits[i] = tclListElementRepr(listElemText(el))
	}
	return strconv.Quote(strings.Join(lits, " "))
}

// cmdExprListMixedJoin joins literal and substituted elements into a Go
// string concatenation.
func cmdExprListMixedJoin(clean []listElem) string {
	parts := make([]string, len(clean))
	for i, el := range clean {
		if el.literal {
			parts[i] = strconv.Quote(tclListElementRepr(listElemText(el)))
		} else {
			parts[i] = el.expr
		}
	}
	return strings.Join(parts, `+" "+`)
}

// cmdExprSplit handles `[split STR ?SEP?]` — TCL split as a value: the
// result is the TCL list string of parts (unionvtab 2.4.x:
// `set E [split $e .]`). With no SEP the split characters are the TCL
// whitespace default " \n\t\r"; an empty SEP splits into individual
// characters (see the runtime tclSplitString).
func (tp *transpiler) cmdExprSplit(cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `""`
	}
	strExpr := tp.buildStringExpr(args[0])
	sep := `" \n\t\r"`
	if len(args) >= 2 {
		sep = tp.buildStringExpr(args[1])
	}
	return fmt.Sprintf("tclSplitString(%s, %s)", strExpr, sep)
}

// cmdExprLIndex handles `[lindex $list $idx]`.
func (tp *transpiler) cmdExprLIndex(cmdName, cmdText string, args []string) string {
	if len(args) < 2 {
		return `""`
	}
	listExpr := tp.buildStringExpr(args[0])
	idxExpr := tp.buildStringExpr(args[1])
	return fmt.Sprintf("tclLIndex(%s, %s)", listExpr, idxExpr)
}

// cmdExprLLength handles `[llength $list]` — the list length as a string (TCL
// values are strings), so comparisons like {$i < [llength $::idxlist]} work.
func (tp *transpiler) cmdExprLLength(cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `"0"`
	}
	listExpr := tp.buildStringExpr(args[0])
	return fmt.Sprintf("strconv.Itoa(tclLLength(%s))", listExpr)
}

// cmdExprLSearch handles `[lsearch $list $value]` — index of value in the TCL
// list, or -1 when absent. Emits a runtime Go expression so conditions like
// {[lsearch $exprkw $kw]<0} resolve correctly (buildCmdNumericCond compares
// the Atoi-converted result).
func (tp *transpiler) cmdExprLSearch(cmdName, cmdText string, args []string) string {
	// Skip TCL lsearch flags (e.g. -exact, -glob, -regexp) before the list.
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	if len(args) < 2 {
		return `"-1"`
	}
	listExpr := tp.buildStringExpr(args[0])
	valueExpr := tp.buildStringExpr(args[1])
	return fmt.Sprintf("strconv.Itoa(tclLsearch(%s, %s))", listExpr, valueExpr)
}

// cmdExprLRange handles `[lrange $list start end]` — sublist as a TCL list
// string.
func (tp *transpiler) cmdExprLRange(cmdName, cmdText string, args []string) string {
	if len(args) < 3 {
		return `""`
	}
	listExpr := tp.buildStringExpr(args[0])
	startExpr := tp.buildStringExpr(args[1])
	endExpr := tp.buildStringExpr(args[2])
	return fmt.Sprintf("tclLRange(%s, %s, %s)", listExpr, startExpr, endExpr)
}

// cmdExprLReplace handles `[lreplace $list $first $last $repl...]` — returns
// the modified list as a TCL list string. TCL's lreplace replaces the range
// [first..last] (or just the single element at `first` when `last` is omitted
// but our callers always pass both) with the new elements. Used in test
// expressions like `set ::tbl_data [lreplace $::tbl_data $idx $idx]` to drop
// the deleted row's generated string from the reference list.
func (tp *transpiler) cmdExprLReplace(cmdName, cmdText string, args []string) string {
	if len(args) < 3 {
		return `""`
	}
	listExpr := tp.buildStringExpr(args[0])
	firstExpr := tp.buildStringExpr(args[1])
	// In TCL, a 2-arg form `lreplace $list $first` is "remove the element at
	// $first"; tests always pass 3+ args, so treat the third as `last`.
	lastExpr := tp.buildStringExpr(args[2])
	var replExprs []string
	for _, a := range args[3:] {
		replExprs = append(replExprs, tp.buildStringExpr(a))
	}
	if len(replExprs) == 0 {
		return fmt.Sprintf("tclLReplace(%s, %s, %s)", listExpr, firstExpr, lastExpr)
	}
	return fmt.Sprintf("tclLReplace(%s, %s, %s, %s)", listExpr, firstExpr, lastExpr, strings.Join(replExprs, ", "))
}

// lsortFlags parses lsort's leading switch flags: -integer (numeric
// compare), -increasing/-decreasing direction, -unique (dedup not needed by
// the corpus; treated as a plain sort). An unrecognized flag word is the
// list argument itself — the loop stops there and the caller renders
// tclSort over it (the original handler's default case). Flags precede the
// list argument.
func lsortFlags(args []string) (integer, desc bool, listArgs []string) {
	listArgs = args
	for len(listArgs) > 0 && strings.HasPrefix(listArgs[0], "-") {
		switch listArgs[0] {
		case "-integer":
			integer = true
		case "-decreasing":
			desc = true
		case "-increasing", "-ascii", "-real", "-nocase":
			integer = integer || listArgs[0] == "-real"
		case "-unique":
			// dedup not needed by the corpus; treat as plain sort
		default:
			return false, false, []string{listArgs[0]}
		}
		listArgs = listArgs[1:]
	}
	return integer, desc, listArgs
}

// lsortEmit renders the sorted-list expression for the parsed lsort flags.
func lsortEmit(integer, desc bool, listExpr string) string {
	switch {
	case integer && desc:
		return fmt.Sprintf("tclSortIntDesc(%s)", listExpr)
	case integer:
		return fmt.Sprintf("tclSortInt(%s)", listExpr)
	case desc:
		return fmt.Sprintf("tclSortDesc(%s)", listExpr)
	default:
		return fmt.Sprintf("tclSort(%s)", listExpr)
	}
}

// cmdExprLSort handles `[lsort $list]` — sorted list (default ascending).
func (tp *transpiler) cmdExprLSort(cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `""`
	}
	integer, desc, listArgs := lsortFlags(args)
	if len(listArgs) == 0 {
		return `""`
	}
	return lsortEmit(integer, desc, tp.buildStringExpr(listArgs[0]))
}

// cmdExprJoin handles `[join list sep]` — TCL list join. The list is a TCL
// variable built at Go runtime (e.g. by lappend), so emit
// strings.Join(tclSplitList).
func (tp *transpiler) cmdExprJoin(cmdName, cmdText string, args []string) string {
	if len(args) < 1 {
		return `""`
	}
	listExpr := tp.buildStringExpr(args[0])
	sep := `" "`
	if len(args) >= 2 {
		sep = tp.buildStringExpr(args[1])
	}
	return fmt.Sprintf("strings.Join(tclSplitList(%s), %s)", listExpr, sep)
}

// cmdExprConcat handles `[concat $a $b ...]` — TCL list concatenation. Each
// arg is rendered as a Go string expression (so $var and [cmd] refs are
// resolved), split via tclSplitList, and the elements are joined with a
// single space. Used by autovacuum.test 1.x's
//
//	[eval concat $delete_order]
//
// to flatten a list-of-lists into a single space-separated list before
// [lsort -integer] ingests it.
func (tp *transpiler) cmdExprConcat(cmdName, cmdText string, args []string) string {
	if len(args) == 0 {
		return `""`
	}
	exprs := make([]string, len(args))
	for i, a := range args {
		exprs[i] = tp.buildStringExpr(a)
	}
	return fmt.Sprintf("tclConcat(%s)", strings.Join(exprs, ", "))
}
