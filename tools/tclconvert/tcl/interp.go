// SPDX-License-Identifier: GPL-3.0-or-later
// Package tcl implements a minimal TCL interpreter sufficient to parse and
// execute the subset of TCL used in SQLite test files (.test). It captures
// SQL statements emitted via db eval / execsql / catchsql / do_execsql_test /
// do_catchsql_test / do_test.
package tcl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Stmt represents a captured SQL statement with its expected result.
type Stmt struct {
	Type     string // "exec" or "query" or "catch" (exec expecting error)
	SQL      string
	Expected string // empty = no expected result check
	TestName string
}

// Interp is the TCL interpreter state.
type Interp struct {
	vars      map[string]string
	procs     map[string]*Proc
	stmts     []Stmt // captured SQL statements
	curTest   string // current test name (from do_test/do_execsql_test)
	depth     int    // call stack depth guard
	nullToken string // "db null TOKEN": rendering of SQL NULLs in results
}

// NullToken returns the token used to render SQL NULL in results
// (set via "db null TOKEN"); empty means the default.
func (i *Interp) NullToken() string { return i.nullToken }

// Proc is a user-defined TCL procedure.
type Proc struct {
	Name string
	Args []string
	Body string
}

// NewInterp creates a new TCL interpreter.
func NewInterp() *Interp {
	return &Interp{
		vars:  make(map[string]string),
		procs: make(map[string]*Proc),
	}
}

// Stmts returns the captured SQL statements.
func (i *Interp) Stmts() []Stmt { return i.stmts }

// Execute parses and executes TCL source code.
func (i *Interp) Execute(src string) error {
	return i.execScript(src, nil)
}

// evalWord evaluates a single word, performing variable ($var) and command
// ([cmd]) substitution if needed. Braced words are returned as-is.
func (i *Interp) evalWord(rw rawWord, localVars map[string]string) (string, error) {
	if rw.Braced {
		return rw.Text, nil
	}
	return i.substitute(rw.Text, localVars), nil
}

// substitute performs $var and [cmd] substitution in a string.
// $varname → variable value (or empty string if unset)
// ${varname} → variable value (braced name form)
// [cmd args] → result of executing command
// \n, \t, \\ → escape sequences
func (i *Interp) substitute(s string, localVars map[string]string) string {
	var result strings.Builder
	pos := 0
	for pos < len(s) {
		ch := s[pos]

		if ch == '\\' && pos+1 < len(s) {
			// Escape sequence
			next := s[pos+1]
			result.WriteByte(escapeChar(next))
			pos += 2
			continue
		}

		if ch == '$' {
			var val string
			var ok bool
			pos, val, ok = i.substituteVar(s, pos, localVars)
			if ok {
				result.WriteString(val)
			}
			continue
		}

		if ch == '[' {
			var val string
			pos, val = i.substituteCmd(s, pos, localVars)
			result.WriteString(val)
			continue
		}

		result.WriteByte(ch)
		pos++
	}
	return result.String()
}

// escapeChar maps a backslash escape sequence to its character.
func escapeChar(next byte) byte {
	switch next {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	default:
		return next
	}
}

// substituteVar handles a $ at s[pos-1], consuming the variable reference and
// returning the new position, the variable value (if set), and whether it was
// set. Supports $varname, ${varname}, and $var(array) forms.
func (i *Interp) substituteVar(s string, pos int, localVars map[string]string) (int, string, bool) {
	pos++
	if pos >= len(s) {
		// Trailing $ is a literal dollar sign.
		return pos, "$", true
	}

	if s[pos] == '{' {
		return i.substituteBracedVar(s, pos, localVars)
	}

	if unicode.IsLetter(rune(s[pos])) || s[pos] == '_' || s[pos] == ':' {
		return i.substituteNamedVar(s, pos, localVars)
	}

	// Not a variable reference — the $ is literal.
	return pos, "$", true
}

// substituteBracedVar handles the ${varname} form at s[pos] == '{'.
func (i *Interp) substituteBracedVar(s string, pos int, localVars map[string]string) (int, string, bool) {
	pos++
	start := pos
	for pos < len(s) && s[pos] != '}' {
		pos++
	}
	varName := s[start:pos]
	if pos < len(s) {
		pos++ // skip closing }
	}
	val, ok := i.getVar(varName, localVars)
	return pos, val, ok
}

// substituteNamedVar handles the $varname form (with optional array element)
// at a position where s[pos] starts a variable name.
func (i *Interp) substituteNamedVar(s string, pos int, localVars map[string]string) (int, string, bool) {
	varName, next := readVarName(s, pos)
	val, ok := i.getVar(varName, localVars)
	return next, val, ok
}

// readVarName scans a TCL variable name at s[pos] (letters/digits/_/: and an
// optional (array) element suffix), returning the name and the position after
// it.
func readVarName(s string, pos int) (string, int) {
	start := pos
	for pos < len(s) && (unicode.IsLetter(rune(s[pos])) || unicode.IsDigit(rune(s[pos])) || s[pos] == '_' || s[pos] == ':') {
		pos++
	}
	varName := s[start:pos]
	// Handle array element: $var(arr)
	if pos < len(s) && s[pos] == '(' {
		end := strings.IndexByte(s[pos:], ')')
		if end > 0 {
			varName += s[pos : pos+end+1]
			pos += end + 1
		}
	}
	return varName, pos
}

// substituteCmd handles a [ at s[pos], parsing balanced [...] and executing
// the inner command. Returns the new position and the command result.
func (i *Interp) substituteCmd(s string, pos int, localVars map[string]string) (int, string) {
	depth := 1
	start := pos + 1
	pos++
	for pos < len(s) && depth > 0 {
		if s[pos] == '\\' {
			pos += 2
			continue
		}
		if s[pos] == '[' {
			depth++
		} else if s[pos] == ']' {
			depth--
		}
		if depth > 0 {
			pos++
		}
	}
	cmdText := s[start:pos]
	if pos < len(s) {
		pos++ // skip closing ]
	}
	// Execute the command and capture the result (from i.vars[""])
	i.execScript(cmdText, localVars)
	return pos, i.vars[""]
}

// evalAllWords substitutes all words in a rawWord slice.
func (i *Interp) evalAllWords(words []rawWord, localVars map[string]string) ([]string, error) {
	result := make([]string, 0, len(words))
	for _, rw := range words {
		val, err := i.evalWord(rw, localVars)
		if err != nil {
			return nil, err
		}
		result = append(result, val)
	}
	return result, nil
}

// execScript executes a script (sequence of commands).
func (i *Interp) execScript(src string, localVars map[string]string) error {
	cmds := parseCommands(src)
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		// Skip comments (parser already handles most, but double-check)
		if len(cmd[0].Text) > 0 && cmd[0].Text[0] == '#' && !cmd[0].Braced && !cmd[0].Quoted {
			continue
		}
		err := i.execCommand(cmd, localVars)
		if err != nil {
			if ec, ok := err.(*ControlFlow); ok {
				if ec.Kind == "return" {
					return nil
				}
				return err // break/continue propagate up
			}
			return err
		}
	}
	return nil
}

// ControlFlow represents break/continue/return signals.
type ControlFlow struct {
	Kind   string // "break", "continue", "return"
	Result string
}

func (c *ControlFlow) Error() string { return c.Kind }

// execCommand executes a single command. Words are pre-parsed but NOT yet
// substituted. We need to handle {} braces specially: braced words should
// NOT be substituted at command-exec time (they are literal), but we need
// to substitute non-braced words.
func (i *Interp) execCommand(rawWords []rawWord, localVars map[string]string) error {
	// Substitute variables/commands in each word
	words := make([]string, 0, len(rawWords))
	for _, rw := range rawWords {
		val, err := i.evalWord(rw, localVars)
		if err != nil {
			return err
		}
		words = append(words, val)
	}
	if len(words) == 0 {
		return nil
	}

	return i.dispatchCommand(words[0], rawWords, words[1:], localVars)
}

// cmdSet implements the `set` command.
func (i *Interp) cmdSet(args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	name := args[0]
	if len(args) >= 2 {
		i.setVar(name, args[1], localVars)
	}
	val, _ := i.getVar(name, localVars)
	i.vars[""] = val
	return nil
}

// cmdIncr implements `incr varname [amount]`.
func (i *Interp) cmdIncr(args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	name := args[0]
	amount := 1
	if len(args) >= 2 {
		amount, _ = strconv.Atoi(args[1])
	}
	cur := 0
	if v, ok := i.getVar(name, localVars); ok {
		cur, _ = strconv.Atoi(v)
	}
	newVal := cur + amount
	i.setVar(name, strconv.Itoa(newVal), localVars)
	i.vars[""] = strconv.Itoa(newVal)
	return nil
}

// cmdFor implements `for {start} {cond} {next} {body}`.
func (i *Interp) cmdFor(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 5 {
		return nil
	}
	start := rawWords[1]
	cond := rawWords[2]
	next := rawWords[3]
	body := rawWords[4]

	// Execute start
	i.execIfBody(start, localVars)

	for iter := 0; iter < 50000; iter++ {
		ok, err := i.evalLoopCond(cond, localVars)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		// Execute body
		if body.Braced {
			brk, err := i.execLoopBody(body, localVars)
			if err != nil {
				return err
			}
			if brk {
				break
			}
		}

		// Execute next
		i.execIfBody(next, localVars)
	}
	return nil
}

// cmdForeach implements `foreach var list body` (and multi-var variants).
func (i *Interp) cmdForeach(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 4 {
		return nil
	}
	// Parse: foreach varspec listvar body
	// varspec can be a single var or {v1 v2}
	varSpecRaw := rawWords[1]
	listRaw := rawWords[2]
	body := rawWords[len(rawWords)-1]

	varSpec, err := i.evalWord(varSpecRaw, localVars)
	if err != nil {
		return err
	}
	vars := splitList(varSpec)

	listVal, err := i.evalWord(listRaw, localVars)
	if err != nil {
		return err
	}
	items := splitList(listVal)

	nvars := len(vars)
	idx := 0
	for idx+nvars <= len(items) {
		for j, v := range vars {
			i.setVar(v, items[idx+j], localVars)
		}
		idx += nvars
		if body.Braced {
			brk, err := i.execLoopBody(body, localVars)
			if err != nil {
				return err
			}
			if brk {
				break
			}
		}
	}
	return nil
}

// cmdWhile implements `while {cond} {body}`.
func (i *Interp) cmdWhile(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 3 {
		return nil
	}
	cond := rawWords[1]
	body := rawWords[2]

	for iter := 0; iter < 50000; iter++ {
		ok, err := i.evalLoopCond(cond, localVars)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if body.Braced {
			brk, err := i.execLoopBody(body, localVars)
			if err != nil {
				return err
			}
			if brk {
				break
			}
		}
	}
	return nil
}

// evalLoopCond evaluates a loop condition word and reports whether the loop
// should continue.
func (i *Interp) evalLoopCond(cond rawWord, localVars map[string]string) (bool, error) {
	condVal, err := i.evalWord(cond, localVars)
	if err != nil {
		return false, err
	}
	result, err := EvalExpr(condVal, i, localVars)
	if err != nil {
		return false, err
	}
	return isTrue(result), nil
}

// execLoopBody executes a loop body word, reporting whether a break was
// encountered. Continue falls through (returns false). Non-control-flow errors
// are returned.
func (i *Interp) execLoopBody(body rawWord, localVars map[string]string) (bool, error) {
	err := i.execScript(body.Text, localVars)
	if err == nil {
		return false, nil
	}
	ec, ok := err.(*ControlFlow)
	if !ok {
		return false, err
	}
	if ec.Kind == "break" {
		return true, nil
	}
	// continue — fall through to next iteration
	return false, nil
}

// cmdIf implements if/elseif/else.
func (i *Interp) cmdIf(rawWords []rawWord, localVars map[string]string) error {
	// if {cond} {body} [elseif {cond} {body}] [else {body}]
	// Also: if {cond} then {body} ...
	idx := 1
	for idx < len(rawWords) {
		condVal, next, err := i.evalIfCondition(rawWords, idx, localVars)
		if err != nil {
			return err
		}
		idx = next
		if idx >= len(rawWords) {
			break
		}

		// Evaluate condition
		result, err := EvalExpr(condVal, i, localVars)
		if err != nil {
			return err
		}

		if isTrue(result) {
			return i.execIfBody(rawWords[idx], localVars)
		}

		// Skip body and handle elseif/else chain
		idx = i.skipIfElse(rawWords, idx+1, localVars)
		if idx < 0 {
			return nil
		}
		if idx == 0 {
			break
		}
	}
	return nil
}

// skipIfElse advances past a false if-body, handling the elseif/else chain.
// Returns the index of the next condition to evaluate (positive), 0 when the
// if statement is complete, or -1 when an else-body was executed.
func (i *Interp) skipIfElse(rawWords []rawWord, idx int, localVars map[string]string) int {
	if idx >= len(rawWords) {
		return 0
	}
	kw, _ := i.evalWord(rawWords[idx], localVars)
	switch kw {
	case "elseif":
		return idx + 1
	case "else":
		idx++
		if idx < len(rawWords) && rawWords[idx].Braced {
			i.execScript(rawWords[idx].Text, localVars)
		}
		return -1
	}
	// In TCL, else is optional. If next word is a braced body, execute it.
	if rawWords[idx].Braced {
		i.execScript(rawWords[idx].Text, localVars)
		return -1
	}
	return 0
}

// evalIfCondition evaluates the condition word at rawWords[idx], skipping a
// "then" keyword after it. Returns the condition string and the index of the
// body word.
func (i *Interp) evalIfCondition(rawWords []rawWord, idx int, localVars map[string]string) (string, int, error) {
	condWord := rawWords[idx]
	condVal, err := i.evalWord(condWord, localVars)
	if err != nil {
		return "", 0, err
	}

	// Skip "then" keyword
	idx++
	if idx < len(rawWords) && !rawWords[idx].Braced {
		kw, _ := i.evalWord(rawWords[idx], localVars)
		if kw == "then" {
			idx++
		}
	}
	return condVal, idx, nil
}

// execIfBody executes the body word if it is braced.
func (i *Interp) execIfBody(bodyWord rawWord, localVars map[string]string) error {
	if bodyWord.Braced {
		return i.execScript(bodyWord.Text, localVars)
	}
	return nil
}

// cmdProc implements `proc name {args} {body}`.
func (i *Interp) cmdProc(rawWords []rawWord) error {
	if len(rawWords) < 4 {
		return nil
	}
	name, _ := i.evalWord(rawWords[1], nil)
	argsStr := ""
	if rawWords[2].Braced {
		argsStr = rawWords[2].Text
	} else {
		argsStr, _ = i.evalWord(rawWords[2], nil)
	}
	body := ""
	if rawWords[3].Braced {
		body = rawWords[3].Text
	} else {
		body, _ = i.evalWord(rawWords[3], nil)
	}
	argNames := splitList(argsStr)
	i.procs[name] = &Proc{Name: name, Args: argNames, Body: body}
	return nil
}

// callProc calls a user-defined procedure.
func (i *Interp) callProc(proc *Proc, args []string, callerVars map[string]string) error {
	i.depth++
	if i.depth > 100 {
		i.depth--
		return fmt.Errorf("proc call depth exceeded")
	}
	// Create a new local scope (procs get their own scope in TCL)
	localVars := make(map[string]string)
	for idx, argName := range proc.Args {
		if idx < len(args) {
			localVars[argName] = args[idx]
		}
	}
	err := i.execScript(proc.Body, localVars)
	i.depth--
	return err
}

// cmdLappend implements `lappend varname args...`.
func (i *Interp) cmdLappend(args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	name := args[0]
	cur, _ := i.getVar(name, localVars)

	// Fast path: when all appended elements are simple (no spaces, braces,
	// quotes, or semicolons), we can append directly without splitting/joining.
	// This avoids O(n²) behavior for large loops like lappend 70000 times.
	allSimple := true
	for _, item := range args[1:] {
		if needsBracing(item) {
			allSimple = false
			break
		}
	}

	if allSimple && len(args) >= 2 {
		// Simple append: no split/join needed
		if cur == "" {
			i.setVar(name, strings.Join(args[1:], " "), localVars)
			i.vars[""] = strings.Join(args[1:], " ")
		} else {
			result := cur + " " + strings.Join(args[1:], " ")
			i.setVar(name, result, localVars)
			i.vars[""] = result
		}
	} else {
		// Full path: handle elements that need bracing
		items := splitList(cur)
		items = append(items, args[1:]...)
		result := tclList(items)
		i.setVar(name, result, localVars)
		i.vars[""] = result
	}
	return nil
}

// cmdString implements basic string operations.
func (i *Interp) cmdString(args []string) error {
	if len(args) < 2 {
		return nil
	}
	sub := args[0]
	if fn, ok := stringHandlers[sub]; ok {
		return fn(i, args)
	}
	i.vars[""] = args[1]
	return nil
}

// cmdRegexp implements basic regexp matching.
func (i *Interp) cmdRegexp(args []string) error {
	if len(args) < 2 {
		i.vars[""] = "0"
		return nil
	}
	pattern := args[len(args)-2]
	str := args[len(args)-1]
	matched, err := regexp.MatchString(pattern, str)
	if err != nil || !matched {
		i.vars[""] = "0"
	} else {
		i.vars[""] = "1"
	}
	return nil
}

// cmdRegsub implements basic regsub.
func (i *Interp) cmdRegsub(args []string) error {
	if len(args) >= 3 {
		pattern := args[0]
		str := args[1]
		repl := args[2]
		re, err := regexp.Compile(pattern)
		if err == nil {
			result := re.ReplaceAllString(str, repl)
			i.vars[""] = result
			if len(args) >= 4 {
				i.setVar(args[3], result, nil)
			}
		}
	}
	return nil
}

// cmdSQL handles execsql/catchsql commands.
func (i *Interp) cmdSQL(rawWords []rawWord, args []string, sqlType string, localVars map[string]string) error {
	// execsql { SQL } [db] or execsql [subst { SQL }] [db]
	for _, rw := range rawWords[1:] {
		if rw.Braced && len(rw.Text) > 0 {
			sql := i.substitute(rw.Text, localVars)
			if strings.TrimSpace(sql) != "" {
				i.stmts = append(i.stmts, Stmt{
					Type:     sqlType,
					SQL:      sql,
					TestName: i.curTest,
				})
			}
			break
		}
		// Handle [subst { SQL }] form — the bracket parsing already resolved this
		if !rw.Braced && len(rw.Text) > 0 {
			val, _ := i.evalWord(rw, nil)
			if strings.TrimSpace(val) != "" && looksLikeSQL(val) {
				i.stmts = append(i.stmts, Stmt{
					Type:     sqlType,
					SQL:      val,
					TestName: i.curTest,
				})
			}
			break
		}
	}
	return nil
}

// cmdDB handles `db eval`, `db onecolumn`, `db transaction`, etc.
func (i *Interp) cmdDB(rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	sub := args[0]
	switch sub {
	case "eval":
		return i.dbEval(rawWords, args, localVars)
	case "onecolumn":
		if len(args) >= 2 {
			i.stmts = append(i.stmts, Stmt{
				Type:     "query",
				SQL:      args[1],
				TestName: i.curTest,
			})
		}
		return nil
	case "null":
		// "db null TOKEN" changes the TCL rendering of SQL NULLs
		// (tester.tcl); record it so expectations can be matched.
		if len(args) >= 2 {
			i.nullToken = args[1]
		}
		return nil
	case "transaction":
		// db transaction { ... } — execute the body
		if len(rawWords) >= 3 && rawWords[2].Braced {
			return i.execScript(rawWords[2].Text, localVars)
		}
		return nil
	case "intkey":
		// db intkey TABLE bool — no-op
		return nil
	}
	// close/on_disconnect/cache/... — no-op
	return nil
}

// dbEval implements `db eval { SQL } ?script?`.
func (i *Interp) dbEval(rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	// The SQL may be a braced word containing $var references.
	// Substitution applies ONLY to double-quoted words: TCL performs no
	// substitution inside braces, so bracketed JSON like [1,[2,3],4]
	// must survive verbatim.
	sql := args[1]
	if len(rawWords) >= 3 && !rawWords[2].Braced {
		// Re-substitute from the raw word to handle $var in quoted SQL
		sql = i.substitute(rawWords[2].Text, localVars)
	} else if len(rawWords) >= 3 && rawWords[2].Braced {
		// TCL's "db eval {SQL}" binds $name references inside the braced
		// SQL from TCL variables (sqlite3_bind_parameter equivalents).
		// Inline them as SQL literals; brackets stay untouched.
		sql = i.bindSQLParams(sql, localVars)
	}
	if strings.TrimSpace(sql) != "" {
		typ := "exec"
		up := strings.ToUpper(strings.TrimSpace(sql))
		if strings.HasPrefix(up, "SELECT") || strings.HasPrefix(up, "VALUES") || strings.HasPrefix(up, "WITH") {
			// Row-returning statement: capture as a query so the expected
			// result is compared against the rendered rows.
			typ = "query"
		}
		i.stmts = append(i.stmts, Stmt{
			Type:     typ,
			SQL:      sql,
			TestName: i.curTest,
		})
	}
	return nil
}

// bindSQLParams replaces $name / ${name} parameter references in braced SQL
// with SQL string literals of the variable values (TCL db-eval binding
// semantics). Unresolvable names are left verbatim.
func (i *Interp) bindSQLParams(sql string, localVars map[string]string) string {
	var b strings.Builder
	pos := 0
	for pos < len(sql) {
		ch := sql[pos]
		if ch != '$' {
			b.WriteByte(ch)
			pos++
			continue
		}
		if pos+1 < len(sql) && sql[pos+1] == '{' {
			end := strings.IndexByte(sql[pos+2:], '}')
			if end >= 0 {
				name := sql[pos+2 : pos+2+end]
				if v, ok := i.getVar(name, localVars); ok {
					b.WriteString(tclSQLLiteral(v))
					pos = pos + 3 + end
					continue
				}
			}
			b.WriteByte(ch)
			pos++
			continue
		}
		j := pos + 1
		for j < len(sql) && (unicode.IsLetter(rune(sql[j])) || unicode.IsDigit(rune(sql[j])) || sql[j] == '_' || sql[j] == ':') {
			j++
		}
		if j == pos+1 {
			b.WriteByte(ch)
			pos++
			continue
		}
		name := sql[pos+1 : j]
		if v, ok := i.getVar(name, localVars); ok {
			b.WriteString(tclSQLLiteral(v))
			pos = j
			continue
		}
		b.WriteString(sql[pos:j])
		pos = j
	}
	return b.String()
}

// tclSQLLiteral renders v as a SQL string literal.
func tclSQLLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}
func (i *Interp) cmdDoExecSQL(rawWords []rawWord, localVars map[string]string) error {
	words, _ := i.evalAllWords(rawWords[1:], localVars)
	// Skip optional -db flag
	idx := 0
	if idx < len(words) && words[idx] == "-db" {
		idx += 2
	}
	if idx >= len(words) {
		return nil
	}
	name := words[idx]
	sql := ""
	expected := ""
	idx++
	sqlRawIdx := idx
	if idx < len(words) {
		sql = words[idx]
		idx++
	}
	if idx < len(words) {
		expected = words[idx]
	}
	// A braced SQL word keeps TCL variables unexpanded; sqlite3 binds them
	// as named parameters from TCL scope. Inline resolvable ones as SQL
	// literals (brackets stay verbatim, preserving JSON arrays).
	if sqlRawIdx < len(rawWords) && rawWords[sqlRawIdx+1].Braced && sql != "" {
		sql = i.bindSQLParams(sql, localVars)
	}

	// Determine if it's a query or exec
	sqlType := "exec"
	lastStmt := lastStatement(sql)
	if isQueryStmt(lastStmt) {
		sqlType = "query"
	}

	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     sqlType,
		SQL:      sql,
		Expected: expected,
		TestName: name,
	})
	return nil
}

// cmdDoCatchSQL handles do_catchsql_test name { SQL } { expected_error }
func (i *Interp) cmdDoCatchSQL(rawWords []rawWord, localVars map[string]string) error {
	words, _ := i.evalAllWords(rawWords[1:], localVars)
	if len(words) < 2 {
		return nil
	}
	name := words[0]
	sql := words[1]
	expected := ""
	if len(words) >= 3 {
		expected = words[2]
	}
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     "catch",
		SQL:      sql,
		Expected: expected,
		TestName: name,
	})
	return nil
}

// cmdDoTest handles do_test name { body } { expected }
// The body may contain execsql, db eval, or other TCL code.
func (i *Interp) cmdDoTest(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 3 {
		return nil
	}
	nameWord := rawWords[1]
	name, _ := i.evalWord(nameWord, localVars)

	// Find the body (braced) and expected (braced)
	bodyWord := rawWords[2]
	expected := ""
	if len(rawWords) >= 4 && rawWords[3].Braced {
		expected = rawWords[3].Text
	}

	i.curTest = name

	// Execute the body — this captures SQL statements
	if bodyWord.Braced {
		i.execScript(bodyWord.Text, localVars)
	} else {
		body, _ := i.evalWord(bodyWord, localVars)
		i.execScript(body, localVars)
	}

	// If expected is non-empty, attach it to the last captured statement
	if expected != "" && len(i.stmts) > 0 {
		last := &i.stmts[len(i.stmts)-1]
		if last.TestName == name && last.Expected == "" {
			last.Expected = expected
		}
	}

	i.curTest = ""
	return nil
}

// cmdDoEQP handles do_eqp_test name { SQL } { expected }
func (i *Interp) cmdDoEQP(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 3 {
		return nil
	}
	name, _ := i.evalWord(rawWords[1], localVars)
	sql := ""
	if rawWords[2].Braced {
		sql = i.substitute(rawWords[2].Text, localVars)
	} else {
		sql, _ = i.evalWord(rawWords[2], localVars)
	}
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     "query",
		SQL:      "EXPLAIN QUERY PLAN " + sql,
		TestName: name,
	})
	return nil
}

// --- Variable management ---

func (i *Interp) setVar(name, val string, localVars map[string]string) {
	if localVars != nil {
		localVars[name] = val
	} else {
		i.vars[name] = val
	}
}

func (i *Interp) getVar(name string, localVars map[string]string) (string, bool) {
	// A "::name" reference targets the global namespace; foreach loop
	// variables are captured as locals, so try the bare name as well.
	if strings.HasPrefix(name, "::") {
		bare := name[2:]
		if v, ok := i.getVar(bare, localVars); ok {
			return v, true
		}
	}
	if localVars != nil {
		if v, ok := localVars[name]; ok {
			return v, true
		}
	}
	if v, ok := i.vars[name]; ok {
		return v, true
	}
	return "", false
}

// --- Helpers ---

// looksLikeSQL checks if a string looks like a SQL statement.
func looksLikeSQL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	upper := strings.ToUpper(s[:min(len(s), 20)])
	keywords := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP",
		"ALTER", "PRAGMA", "WITH", "REPLACE", "ATTACH", "DETACH", "BEGIN",
		"COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "ANALYZE", "REINDEX",
		"VACUUM", "EXPLAIN"}
	for _, kw := range keywords {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}

func lastStatement(sql string) string {
	stmts := strings.Split(sql, ";")
	for i := len(stmts) - 1; i >= 0; i-- {
		s := strings.TrimSpace(stripSQLComments(stmts[i]))
		if s != "" {
			return s
		}
	}
	return ""
}

// stripSQLComments removes SQL comments (/* ... */ and -- to end of line)
// outside string literals, so statement classification is not confused by
// braces or keywords inside comments (e.g. json101-11.2's trailing "*/ } */").
func stripSQLComments(sql string) string {
	var b strings.Builder
	inS, inD := false, false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case inS:
			b.WriteByte(c)
			if c == '\'' {
				inS = false
			}
		case inD:
			b.WriteByte(c)
			if c == '"' {
				inD = false
			}
		case c == '\'':
			inS = true
			b.WriteByte(c)
		case c == '"':
			inD = true
			b.WriteByte(c)
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isQueryStmt(stmt string) bool {
	stmt = strings.TrimSpace(stmt)
	upper := strings.ToUpper(stmt[:min(len(stmt), 10)])
	return strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "PRAGMA") ||
		strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "WITH")
}

func isTrue(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// TCL: true if it's a non-zero number
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f != 0
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
