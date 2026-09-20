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
	vars         map[string]string
	procs        map[string]*Proc
	stmts        []Stmt          // captured SQL statements
	curTest      string          // current test name (from do_test/do_execsql_test)
	depth        int             // call stack depth guard
	catchDepth   int             // nesting level of `catch { ... }` blocks
	deletedFiles map[string]bool // files removed via forcedelete/file delete
	nullToken    string          // "db null TOKEN": rendering of SQL NULLs in results
}

// NullToken returns the token used to render SQL NULL in results
// (set via "db null TOKEN"); empty means the default.
func (i *Interp) NullToken() string { return i.nullToken }

// Proc is a user-defined TCL procedure.
type Proc struct {
	Name     string
	Args     []string
	Body     string
	Defaults map[string]string // optional-arg default values ({name default} specs)
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

// Execute parses and executes TCL source code. A top-level command that
// fails (bad expression, unsupported construct, user `error`) is skipped so
// the remainder of the file still converts; nested scripts keep their
// abort-on-error semantics so `catch` and control flow keep working.
func (i *Interp) Execute(src string) error {
	cmds := parseCommands(src)
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		if len(cmd[0].Text) > 0 && cmd[0].Text[0] == '#' && !cmd[0].Braced && !cmd[0].Quoted {
			continue
		}
		if err := i.execCommand(cmd, nil); err != nil {
			if ec, ok := err.(*ControlFlow); ok && ec.Kind == "return" {
				return nil
			}
			// Skip the offending command and continue with the next one.
		}
	}
	return nil
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

// substituteExprAtoms performs expr-context substitution: $var references
// bind as ATOMIC operands (expr substitutes variable values without
// re-tokenizing them), so a value containing spaces or braces is wrapped in
// a double-quoted string literal. Textual substitution would corrupt
// conditions like `$res == "0 {}"` when $res is the list
// "1 {FOREIGN KEY constraint failed}" (the braces would garble the parse).
func (i *Interp) substituteExprAtoms(s string, localVars map[string]string) string {
	var result strings.Builder
	pos := 0
	for pos < len(s) {
		ch := s[pos]
		switch {
		case ch == '\\' && pos+1 < len(s):
			result.WriteByte(escapeChar(s[pos+1]))
			pos += 2
		case ch == '$':
			var val string
			var ok bool
			pos, val, ok = i.substituteVar(s, pos, localVars)
			if ok {
				result.WriteString(exprAtom(val))
			}
		case ch == '[':
			var val string
			pos, val = i.substituteCmd(s, pos, localVars)
			result.WriteString(exprAtom(val))
		default:
			result.WriteByte(ch)
			pos++
		}
	}
	return result.String()
}

// exprAtom renders a substituted value as a single expr operand.
func exprAtom(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " \t\n\r{}\"") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}

// evalLoopCond evaluates a loop condition word and reports whether the loop
// should continue. An unparseable condition terminates the loop instead of
// aborting the whole file conversion.
func (i *Interp) evalLoopCond(cond rawWord, localVars map[string]string) (bool, error) {
	condVal, err := i.evalWord(cond, localVars)
	if err != nil {
		return false, err
	}
	result, err := EvalExpr(condVal, i, localVars)
	if err != nil {
		return false, nil
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

		// Evaluate condition. An unparseable condition is treated as false
		// (skip to the elseif/else chain) instead of aborting the whole
		// file conversion.
		result, err := EvalExpr(condVal, i, localVars)
		if err == nil && isTrue(result) {
			return i.execIfBody(rawWords[idx], localVars)
		}
		next, done := i.advancePastFalseBranch(rawWords, idx, localVars)
		if done {
			return nil
		}
		idx = next
	}
	return nil
}

// advancePastFalseBranch skips a false if-body and handles the elseif/else
// chain. It returns the index of the next condition and whether the whole if
// statement is finished.
func (i *Interp) advancePastFalseBranch(rawWords []rawWord, idx int, localVars map[string]string) (int, bool) {
	next := i.skipIfElse(rawWords, idx+1, localVars)
	return next, next <= 0
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

// execIfBody executes the body word. TCL body words need not be braced
// (e.g. `if {$tn<5} continue`) — an unbraced word is evaluated and executed
// as a script so control-flow commands like continue/break propagate.
func (i *Interp) execIfBody(bodyWord rawWord, localVars map[string]string) error {
	if bodyWord.Braced {
		return i.execScript(bodyWord.Text, localVars)
	}
	body, err := i.evalWord(bodyWord, localVars)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return i.execScript(body, localVars)
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
	proc := &Proc{Name: name, Args: argNames, Body: body}
	// TCL optional arguments use {name default} specs: parse them so calls
	// with fewer arguments bind the defaults instead of misnamed locals.
	for _, spec := range argNames {
		if name, def, ok := splitArgSpec(spec); ok {
			if proc.Defaults == nil {
				proc.Defaults = make(map[string]string)
			}
			proc.Defaults[name] = def
		}
	}
	i.procs[name] = proc
	return nil
}

// splitArgSpec parses a TCL argument spec, reporting (name, default, true)
// for the {name default} optional-argument form.
func splitArgSpec(spec string) (string, string, bool) {
	spec = strings.TrimSpace(spec)
	if !strings.ContainsAny(spec, " \t\n") {
		return "", "", false
	}
	items := splitList(spec)
	if len(items) != 2 {
		return "", "", false
	}
	return items[0], items[1], true
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
		// Optional-argument specs arrive as the {name default} element.
		specName, defaultVal, hasDefault := splitArgSpec(argName)
		if hasDefault {
			argName = specName
		}
		if idx < len(args) {
			localVars[argName] = args[idx]
		} else if hasDefault {
			localVars[argName] = defaultVal
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
