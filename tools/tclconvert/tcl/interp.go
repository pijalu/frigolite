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
	vars   map[string]string
	procs  map[string]*Proc
	stmts  []Stmt // captured SQL statements
	curTest string // current test name (from do_test/do_execsql_test)
	depth  int    // call stack depth guard
	skip   bool   // skip SQL capture (inside unsupported blocks)
}

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
	if rw.braced {
		return rw.text, nil
	}
	return i.substitute(rw.text, localVars), nil
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
			switch next {
			case 'n':
				result.WriteByte('\n')
			case 't':
				result.WriteByte('\t')
			case 'r':
				result.WriteByte('\r')
			case '\\':
				result.WriteByte('\\')
			case '$':
				result.WriteByte('$')
			case '[':
				result.WriteByte('[')
			case ']':
				result.WriteByte(']')
			case '{':
				result.WriteByte('{')
			case '}':
				result.WriteByte('}')
			case '"':
				result.WriteByte('"')
			default:
				result.WriteByte(next)
			}
			pos += 2
			continue
		}

		if ch == '$' {
			// Variable substitution
			pos++
			if pos >= len(s) {
				result.WriteByte('$')
				break
			}

			if s[pos] == '{' {
				// ${varname} — braced variable name
				pos++
				start := pos
				for pos < len(s) && s[pos] != '}' {
					pos++
				}
				varName := s[start:pos]
				if pos < len(s) {
					pos++ // skip closing }
				}
				if val, ok := i.getVar(varName, localVars); ok {
					result.WriteString(val)
				}
			} else if unicode.IsLetter(rune(s[pos])) || s[pos] == '_' {
				// $varname — alphanumeric variable name
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
				if val, ok := i.getVar(varName, localVars); ok {
					result.WriteString(val)
				}
			} else {
				result.WriteByte('$')
			}
			continue
		}

		if ch == '[' {
			// Command substitution — parse balanced [...] and execute
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
			result.WriteString(i.vars[""])
			continue
		}

		result.WriteByte(ch)
		pos++
	}
	return result.String()
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
		if len(cmd[0].text) > 0 && cmd[0].text[0] == '#' && !cmd[0].braced && !cmd[0].quoted {
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

	cmd := words[0]
	args := words[1:]

	switch cmd {
	case "set":
		return i.cmdSet(args, localVars)
	case "unset":
		for _, a := range args {
			delete(i.vars, a)
			delete(localVars, a)
		}
		return nil
	case "incr":
		return i.cmdIncr(args, localVars)
	case "expr":
		// expr can take braced or unbraced args
		exprStr := strings.Join(args, " ")
		result, err := EvalExpr(exprStr, i, localVars)
		if err != nil {
			return err
		}
		// Store as last result (expr output goes to the command result)
		i.vars[""] = result
		return nil
	case "if":
		return i.cmdIf(rawWords, localVars)
	case "for":
		return i.cmdFor(rawWords, localVars)
	case "foreach":
		return i.cmdForeach(rawWords, localVars)
	case "while":
		return i.cmdWhile(rawWords, localVars)
	case "proc":
		return i.cmdProc(rawWords)
	case "return":
		val := ""
		if len(args) > 0 {
			val = args[0]
		}
		return &ControlFlow{Kind: "return", Result: val}
	case "break":
		return &ControlFlow{Kind: "break"}
	case "continue":
		return &ControlFlow{Kind: "continue"}
	case "list":
		i.vars[""] = tclList(args)
	case "lappend":
		return i.cmdLappend(args, localVars)
	case "llength":
		if len(args) > 0 {
			i.vars[""] = strconv.Itoa(tclLLength(args[0]))
		}
	case "lindex":
		if len(args) >= 2 {
			idx, _ := strconv.Atoi(args[1])
			i.vars[""] = tclLIndex(args[0], idx)
		}
	case "lrange":
		if len(args) >= 3 {
			start, _ := strconv.Atoi(args[1])
			end, _ := strconv.Atoi(args[2])
			i.vars[""] = tclLRange(args[0], start, end)
		}
	case "lsearch":
		i.vars[""] = "0" // simplified
	case "lsort":
		i.vars[""] = args[0] // simplified
	case "concat":
		i.vars[""] = strings.Join(args, " ")
	case "string":
		return i.cmdString(args)
	case "regexp":
		return i.cmdRegexp(args)
	case "regsub":
		return i.cmdRegsub(args)
	case "catch":
		// catch { body } var — execute body, ignore errors
		if len(rawWords) >= 2 {
			body := rawWords[1]
			if body.braced {
				err := i.execScript(body.text, localVars)
				if err != nil {
					i.vars[""] = "1"
					if len(args) >= 2 {
						i.setVar(args[1], err.Error(), localVars)
					}
				} else {
					i.vars[""] = "0"
				}
			}
		}
		return nil
	case "error":
		return fmt.Errorf("%s", strings.Join(args, " "))
	case "uplevel":
		// uplevel N { script } or uplevel #N { script }
		if len(rawWords) >= 2 {
			bodyIdx := 1
			if len(rawWords) >= 3 {
				bodyIdx = 2 // skip level arg
			}
			if rawWords[bodyIdx].braced {
				return i.execScript(rawWords[bodyIdx].text, localVars)
			}
		}
		return nil
	case "upvar":
		// upvar N other my — alias variable. Simplified: copy.
		if len(args) >= 2 {
			orig := args[len(args)-2]
			alias := args[len(args)-1]
			if v, ok := i.getVar(orig, localVars); ok {
				i.setVar(alias, v, localVars)
			}
		}
		return nil
	case "global":
		// global var — no-op (we use global vars map directly)
		return nil
	case "info":
		i.vars[""] = "" // simplified
		return nil
	case "namespace":
		return nil // no-op
	case "rename":
		return nil
	case "array":
		return nil
	case "foreach_kv":
		return nil
	case "foreach_u":
		return nil

	// SQL-related commands
	case "execsql":
		return i.cmdSQL(rawWords, args, "exec", localVars)
	case "catchsql":
		return i.cmdSQL(rawWords, args, "catch", localVars)
	case "db":
		return i.cmdDB(rawWords, args, localVars)
	case "do_execsql_test":
		return i.cmdDoExecSQL(rawWords, localVars)
	case "do_catchsql_test":
		return i.cmdDoCatchSQL(rawWords, localVars)
	case "do_test":
		return i.cmdDoTest(rawWords, localVars)
	case "do_eqp_test":
		return i.cmdDoEQP(rawWords, localVars)
	case "do_timed_execsql_test":
		return i.cmdDoExecSQL(rawWords, localVars)
	case "do_execsql2_test":
		return i.cmdDoExecSQL(rawWords, localVars)

	// Test infrastructure stubs
	case "finish_test", "test_finish", "exit":
		return nil
	case "puts", "output1", "output2", "output2_if_no_verbose":
		return nil
	case "fix_testname":
		return nil
	case "incr_ntest":
		return nil
	case "sqlite3_memdebug_settitle":
		return nil
	case "flush":
		return nil
	case "source":
		return nil // skip sourcing test infrastructure
	case "ifcapable":
		// ifcapable FEATURE { body } — we can't know capabilities, skip body
		return nil
	case "ifnotcapable":
		return nil

	default:
		// Check if it's a user-defined proc
		if proc, ok := i.procs[cmd]; ok {
			return i.callProc(proc, args, localVars)
		}
		// Unknown command — silently ignore (many TCL commands are not needed)
		return nil
	}
	return nil
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
	if start.braced {
		i.execScript(start.text, localVars)
	}

	maxIter := 1000000 // safety limit
	for iter := 0; iter < maxIter; iter++ {
		// Evaluate condition
		condVal, err := i.evalWord(cond, localVars)
		if err != nil {
			return err
		}
		result, err := EvalExpr(condVal, i, localVars)
		if err != nil {
			return err
		}
		if !isTrue(result) {
			break
		}

		// Execute body
		if body.braced {
			err := i.execScript(body.text, localVars)
			if err != nil {
				if ec, ok := err.(*ControlFlow); ok {
					if ec.Kind == "break" {
						break
					}
					if ec.Kind == "continue" {
						// fall through to next
					}
				} else {
					return err
				}
			}
		}

		// Execute next
		if next.braced {
			i.execScript(next.text, localVars)
		}
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
		if body.braced {
			err := i.execScript(body.text, localVars)
			if err != nil {
				if ec, ok := err.(*ControlFlow); ok {
					if ec.Kind == "break" {
						break
					}
				} else {
					return err
				}
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

	for iter := 0; iter < 1000000; iter++ {
		condVal, err := i.evalWord(cond, localVars)
		if err != nil {
			return err
		}
		result, err := EvalExpr(condVal, i, localVars)
		if err != nil {
			return err
		}
		if !isTrue(result) {
			break
		}
		if body.braced {
			err := i.execScript(body.text, localVars)
			if err != nil {
				if ec, ok := err.(*ControlFlow); ok {
					if ec.Kind == "break" {
						break
					}
				} else {
					return err
				}
			}
		}
	}
	return nil
}

// cmdIf implements if/elseif/else.
func (i *Interp) cmdIf(rawWords []rawWord, localVars map[string]string) error {
	// if {cond} {body} [elseif {cond} {body}] [else {body}]
	// Also: if {cond} then {body} ...
	idx := 1
	for idx < len(rawWords) {
		// Get condition
		if idx >= len(rawWords) {
			break
		}
		// Check for "else" keyword
		condWord := rawWords[idx]
		condVal, err := i.evalWord(condWord, localVars)
		if err != nil {
			return err
		}

		// Skip "then" keyword
		idx++
		if idx < len(rawWords) && !rawWords[idx].braced {
			kw, _ := i.evalWord(rawWords[idx], localVars)
			if kw == "then" {
				idx++
			}
		}

		if idx >= len(rawWords) {
			break
		}

		// Evaluate condition
		result, err := EvalExpr(condVal, i, localVars)
		if err != nil {
			return err
		}

		if isTrue(result) {
			// Execute body
			bodyWord := rawWords[idx]
			if bodyWord.braced {
				return i.execScript(bodyWord.text, localVars)
			}
			return nil
		}

		// Skip body
		idx++

		// Check for elseif/else
		if idx < len(rawWords) {
			kw, _ := i.evalWord(rawWords[idx], localVars)
			if kw == "elseif" {
				idx++
				continue
			}
			if kw == "else" {
				idx++
				if idx < len(rawWords) && rawWords[idx].braced {
					return i.execScript(rawWords[idx].text, localVars)
				}
				return nil
			}
			// In TCL, else is optional. If next word is a braced body, execute it.
			if idx < len(rawWords) && rawWords[idx].braced {
				return i.execScript(rawWords[idx].text, localVars)
			}
		}
		break
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
	if rawWords[2].braced {
		argsStr = rawWords[2].text
	} else {
		argsStr, _ = i.evalWord(rawWords[2], nil)
	}
	body := ""
	if rawWords[3].braced {
		body = rawWords[3].text
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
	items := splitList(cur)
	items = append(items, args[1:]...)
	i.setVar(name, tclList(items), localVars)
	i.vars[""] = tclList(items)
	return nil
}

// cmdString implements basic string operations.
func (i *Interp) cmdString(args []string) error {
	if len(args) < 2 {
		return nil
	}
	sub := args[0]
	str := args[1]
	switch sub {
	case "length":
		i.vars[""] = strconv.Itoa(len(str))
	case "tolower":
		i.vars[""] = strings.ToLower(str)
	case "toupper":
		i.vars[""] = strings.ToUpper(str)
	case "trim":
		i.vars[""] = strings.TrimSpace(str)
	case "range":
		if len(args) >= 4 {
			start, _ := strconv.Atoi(args[2])
			end, _ := strconv.Atoi(args[3])
			if end >= len(str) {
				end = len(str) - 1
			}
			if start < 0 {
				start = 0
			}
			if start > end {
				i.vars[""] = ""
			} else {
				i.vars[""] = str[start : end+1]
			}
		}
	case "compare":
		if len(args) >= 3 {
			i.vars[""] = strconv.Itoa(strings.Compare(str, args[2]))
		}
	case "equal":
		if len(args) >= 3 {
			if str == args[2] {
				i.vars[""] = "1"
			} else {
				i.vars[""] = "0"
			}
		}
	case "first":
		if len(args) >= 3 {
			i.vars[""] = strconv.Itoa(strings.Index(args[2], str))
		}
	case "map":
		// simplified: string map {from to} str
		i.vars[""] = str
	case "repeat":
		if len(args) >= 3 {
			n, _ := strconv.Atoi(args[2])
			i.vars[""] = strings.Repeat(str, n)
		}
	case "index":
		if len(args) >= 3 {
			idx, _ := strconv.Atoi(args[2])
			if idx >= 0 && idx < len(str) {
				i.vars[""] = string(str[idx])
			} else {
				i.vars[""] = ""
			}
		}
	default:
		i.vars[""] = str
	}
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
		if rw.braced && len(rw.text) > 0 {
			sql := i.substitute(rw.text, localVars)
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
		if !rw.braced && len(rw.text) > 0 {
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
		// db eval { SQL } ?script?
		if len(args) >= 2 {
			// The SQL may be a braced word containing $var references.
			// We need to substitute variables before capturing.
			sql := args[1]
			if len(rawWords) >= 3 {
				// Re-substitute from the raw word to handle $var in braced SQL
				sql = i.substitute(rawWords[2].text, localVars)
			}
			if strings.TrimSpace(sql) != "" {
				i.stmts = append(i.stmts, Stmt{
					Type:     "exec",
					SQL:      sql,
					TestName: i.curTest,
				})
			}
		}
	case "onecolumn":
		if len(args) >= 2 {
			i.stmts = append(i.stmts, Stmt{
				Type:     "query",
				SQL:      args[1],
				TestName: i.curTest,
			})
		}
	case "transaction":
		// db transaction { ... } — execute the body
		if len(rawWords) >= 3 && rawWords[2].braced {
			return i.execScript(rawWords[2].text, localVars)
		}
	case "close", "on_disconnect", "cache", "function", "collate",
		"create_function", "progress", "trace", "busy", "wal_hook",
		"commit_hook", "rollback_hook", "update_hook", "total_changes",
		"last_insert_rowid", "last_query_plan", "changes", "null_value",
		"status", "release_memory", "soft_heap_limit", "hard_heap_limit",
		"config", "deserialize", "serialize", "readonly", "exists",
		"complete", "interrupt", "db_filename", "errorcode", "errmsg",
		"authorizer", "enable_load_extension", "eval_dispatch":
		// no-op
	case "intkey":
		// db intkey TABLE bool — no-op
	}
	return nil
}

// cmdDoExecSQL handles do_execsql_test [-db db] name { SQL } { expected }
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
	if idx < len(words) {
		sql = words[idx]
		idx++
	}
	if idx < len(words) {
		expected = words[idx]
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
	if len(rawWords) >= 4 && rawWords[3].braced {
		expected = rawWords[3].text
	}

	i.curTest = name

	// Execute the body — this captures SQL statements
	if bodyWord.braced {
		i.execScript(bodyWord.text, localVars)
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
	if rawWords[2].braced {
		sql = i.substitute(rawWords[2].text, localVars)
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
		s := strings.TrimSpace(stmts[i])
		if s != "" {
			return s
		}
	}
	return ""
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
