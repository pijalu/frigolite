// SPDX-License-Identifier: GPL-3.0-or-later
// Package tcl implements a minimal TCL interpreter sufficient to parse and
// execute the subset of TCL used in SQLite test files (.test). It captures
// SQL statements emitted via db eval / execsql / catchsql / do_execsql_test /
// do_catchsql_test / do_test.
package tcl

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ErrStatementBudget is returned once a file has captured more statements than
// the configured budget: runtime-terminated loops (`for {set i 1} {$stop == 0}`)
// otherwise unroll tens of thousands of iterations and produce enormous
// fixtures. The file is reported as a conversion error so the installed
// fixture stays in place.
var ErrStatementBudget = errors.New("statement budget exceeded")

// ErrConversionDeadline is returned when a file exceeds its conversion
// deadline. The interpreter checks it as it runs, so the conversion stops
// promptly instead of leaking a runaway goroutine.
var ErrConversionDeadline = errors.New("conversion deadline exceeded")

// Stmt represents a captured SQL statement with its expected result.
type Stmt struct {
	Type            string // exec, query, tclquery, query2, or catch (exec expecting error)
	SQL             string
	Variable        string            // Tcl global assigned from query_assign result
	ResultAll       bool              // query_assign keeps the whole row (Tcl execsql/db eval)
	ResultRows      bool              // the variable holds every row (a Tcl list of rows)
	StmtStatus      bool              // append the stmt-status counters ("scan N sort M") to the value
	Condition       string            // the step runs only when this Tcl expression is true
	CatchResult     bool              // the step stores its success code in Variable (a `catch` result)
	CatchMessage    string            // the variable the catch's error message is stored in
	AuthProc        string            // the Tcl authorizer proc name (a `db auth <proc>` step)
	ExpectedExpr    string            // the expectation is itself a runtime expression
	ListValues      []string          // the step builds a Tcl list from these values
	Expected        string            // empty = no expected result check
	ResultTransform string            // Tcl postprocessing applied to query result
	CallbackProcs   []Proc            // procedures visible to callbacks
	CallbackVars    map[string]string // Tcl globals visible to registered callbacks
	FunctionName    string            // Tcl db function registration marker
	CollationName   string            // custom collation registration/deletion name
	CollationProc   string            // Tcl comparator proc name
	CollationBody   string            // Tcl comparator proc body
	DestructorBody  string            // Tcl destructor script
	ModuleArgs      []string          // Tcl module registration arguments
	FunctionProc    string            // Tcl procedure name implementing registered function
	FunctionArgs    []string          // Tcl argument names
	FunctionBody    string            // Tcl procedure body
	MinArgs         int               // registered function minimum argument count
	MaxArgs         int               // registered function maximum argument count
	Deterministic   bool              // SQLITE_DETERMINISTIC
	Innocuous       bool              // SQLITE_INNOCUOUS
	DirectOnly      bool              // SQLITE_DIRECTONLY
	Database        string            // optional Tcl connection name (default db)
	TableName       string            // alter_table helper target
	FileFormat      int               // alter_table helper file-format value
	SchemaSQL       string            // alter_table replacement CREATE TABLE statement
	TestName        string
	GroupName       string // optional sequential harness grouping distinct from assertion name
	FileData        string // bytes written by a captured write_file operation
	FileTarget      string // destination path for copy_file markers
	AuxSQL          string // second query for runtime result comparisons (compare_query)
	FileOffset      int    // byte offset for hexio_write patches

	// Runtime loop: a Tcl `for` whose bounds are only known at run time
	// (the start comes from a query result). The harness evaluates
	// the bounds, binds LoopVar and runs Body once per iteration.
	LoopVar   string
	LoopStart string
	LoopCond  string
	LoopStep  int
	LoopBody  []Stmt

	// TransformProc names a Tcl procedure that post-processes the
	// statement's rendered result (fts3expr3.test's strip_phrase_data).
	TransformProc string
	TransformBody string
	TransformArgs []string

	// SortMarker marks a statement whose result is followed by the
	// testfixture's sort marker ("sort" or "nosort", collate4.test's
	// cksort helper).
	SortMarker bool

	// runtimeID identifies the runtime value of a captured query: the
	// commands that post-process that value (a sort) find the statement
	// through it. It is an internal representation and is not serialized.
	runtimeID int
}

// Interp is the TCL interpreter state.
type Interp struct {
	vars              map[string]string
	dynamicVars       map[string]bool
	trackedVars       map[string]bool // globals written by registered callbacks (runtime_var candidates)
	internalFunctions bool
	callbackCommands  map[string]func([]string) (string, error)
	currentLocalVars  map[string]string
	procs             map[string]*Proc
	stmts             []Stmt // captured SQL statements
	preparedSQL       map[string]string
	preparedSeq       int
	curTest           string // current test name (from do_test/do_execsql_test)
	depth             int    // call stack depth guard
	selectGroup       int
	catchDepth        int             // nesting level of `catch { ... }` blocks
	deletedFiles      map[string]bool // files removed via forcedelete/file delete
	fileWrites        map[string]*tclFileWrite
	nextFileHandle    int
	nullToken         string    // "db null TOKEN": rendering of SQL NULLs in results
	stmtBudget        int       // max captured statements; 0 = unlimited
	deadline          time.Time // zero = none
	runawayErr        error     // sticky budget/deadline error
	substErr          error     // sticky error from a command substitution
	randSeed          int64     // expr rand() state (see tclRand)
	randSeeded        bool
	runtimeID         int            // counter for the runtime value markers
	runtimeVars       map[string]int // variable name -> the runtime query id it holds
}

// TCL's random number generator (TclRand in tclBasic.c): a Park-Miller LCG
// with Schrage's method. atof1.test drives its whole loop from
// `expr srand(1)` + `rand()`, so a constant stub collapsed all 20,000
// iterations into one statement.
const (
	randIA   = 16807
	randIM   = 2147483647
	randIQ   = 127773
	randIR   = 2836
	randMask = 123459876
)

// tclRand reproduces TclRand(): advance the Park-Miller LCG and scale the
// seed by the double constant 1.0/RAND_IM (TCL multiplies by a precomputed
// rounded constant instead of dividing, so the exact bits match).
func (i *Interp) tclRand() float64 {
	if !i.randSeeded {
		// TclRand() seeds from the current time and thread ID; a converter
		// must stay deterministic, so use the value TCL picks when the
		// time-derived seed is 0 modulo the mask (1 <= seed <= 2^31-2).
		i.randSeeded = true
		i.randSeed = randMask
	}
	tmp := i.randSeed / randIQ
	i.randSeed = randIA*(i.randSeed-tmp*randIQ) - randIR*tmp
	if i.randSeed < 0 {
		i.randSeed += randIM
	}
	return float64(i.randSeed) * (1.0 / randIM)
}

// tclSrand reproduces TclSrand(): reset the seed, then generate (and
// discard) one value — TCL's srand() calls rand() internally, so the first
// rand() after srand(N) is the second LCG step from N.
func (i *Interp) tclSrand(seed int64) float64 {
	i.randSeeded = true
	i.randSeed = seed & 0x7FFFFFFF
	if i.randSeed == 0 || i.randSeed == 0x7FFFFFFF {
		i.randSeed ^= randMask
	}
	return i.tclRand()
}

// SetStatementBudget bounds how many statements a single file may capture.
// Once the budget is exhausted every further command fails with
// ErrStatementBudget, which unwinds the interpreter loops and keeps both the
// process memory and the emitted fixture bounded.
func (i *Interp) SetStatementBudget(n int) { i.stmtBudget = n }

// SetDeadline bounds the total execution time of a file. The interpreter
// checks the deadline between commands, so a runaway loop stops by itself
// (no abandoned goroutine keeps allocating after a timeout).
func (i *Interp) SetDeadline(t time.Time) { i.deadline = t }

// checkRunaway reports the sticky runaway error, tripping it when the
// statement budget or the deadline is exceeded.
func (i *Interp) checkRunaway() error {
	if i.runawayErr != nil {
		return i.runawayErr
	}
	if i.stmtBudget > 0 && len(i.stmts) >= i.stmtBudget {
		i.runawayErr = ErrStatementBudget
		return i.runawayErr
	}
	if !i.deadline.IsZero() && !time.Now().Before(i.deadline) {
		i.runawayErr = ErrConversionDeadline
		return i.runawayErr
	}
	return nil
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

type tclFileWrite struct {
	path string
	data strings.Builder
}

// NewInterp creates a new TCL interpreter.
func NewInterp() *Interp {
	return &Interp{
		// The SQLITE_MAX_* globals the testfixture exposes to the TCL
		// scripts; they mirror the C engine's compile-time defaults
		// (SQLITE_MAX_VARIABLE_NUMBER post-3.32 = 32766, which is also
		// this engine's default limit).
		vars: map[string]string{
			"SQLITE_MAX_COLUMN":          "2000",
			"SQLITE_MAX_LENGTH":          "1000000000",
			"SQLITE_MAX_ATTACHED":        "10",
			"SQLITE_MAX_VARIABLE_NUMBER": "32766",
		},
		dynamicVars:      make(map[string]bool),
		trackedVars:      make(map[string]bool),
		callbackCommands: make(map[string]func([]string) (string, error)),
		procs:            make(map[string]*Proc),
		fileWrites:       make(map[string]*tclFileWrite),
		preparedSQL:      make(map[string]string),
		runtimeVars:      make(map[string]int),
	}
}

// CallbackProcs returns a snapshot of procedures registered at the current source point.
func (i *Interp) CallbackProcs() []Proc {
	procs := make([]Proc, 0, len(i.procs))
	for _, proc := range i.procs {
		clone := *proc
		clone.Args = append([]string(nil), proc.Args...)
		procs = append(procs, clone)
	}
	return procs
}

// updateTclCallbackSnapshots fills only forward-declared callbacks on earlier module markers.
func (i *Interp) updateTclCallbackSnapshots() {
	procs := i.CallbackProcs()
	for idx := range i.stmts {
		if i.stmts[idx].Type != "register_tcl_module" {
			continue
		}
		known := make(map[string]bool, len(i.stmts[idx].CallbackProcs))
		for _, proc := range i.stmts[idx].CallbackProcs {
			known[proc.Name] = true
		}
		for _, proc := range procs {
			if !known[proc.Name] {
				i.stmts[idx].CallbackProcs = append(i.stmts[idx].CallbackProcs, proc)
			}
		}
	}
}

func (i *Interp) callbackVars() map[string]string {
	vars := make(map[string]string, len(i.vars))
	for key, value := range i.vars {
		vars[key] = value
	}
	return vars
}

// Stmts returns captured SQL statements.
func (i *Interp) Stmts() []Stmt { return i.stmts }
func (i *Interp) LoadProcedures(procs []Proc) {
	for idx := range procs {
		proc := procs[idx]
		proc.Args = append([]string(nil), proc.Args...)
		i.procs[proc.Name] = &proc
	}
}

// InvokeCommand runs a Tcl command with the given arguments and returns its
// result. The command may be a built-in ("lindex", "string", ...) or a user
// procedure. The arguments are used verbatim: no substitution is applied to
// them.
//
// The testfixture registers SQL functions that map onto Tcl *commands*
// ("db func lindex lindex"), so a harness that evaluates a function's proc
// needs this entry point.
func (i *Interp) InvokeCommand(name string, args ...string) (string, error) {
	i.vars[""] = ""
	words := make([]rawWord, 0, len(args)+1)
	words = append(words, rawWord{Text: name})
	for _, arg := range args {
		words = append(words, rawWord{Text: arg, Braced: true})
	}
	if err := i.execCommand(words, nil); err != nil {
		if flow, ok := err.(*ControlFlow); ok && flow.Kind == "return" {
			return flow.Result, nil
		}
		return i.vars[""], err
	}
	return i.vars[""], nil
}

// InvokeProcedure invokes a named Tcl procedure and returns its Tcl result.
func (i *Interp) InvokeProcedure(name string, args ...string) (string, error) {
	proc, ok := i.procs[name]
	if !ok {
		return "", fmt.Errorf("invalid command name %q", name)
	}
	i.vars[""] = ""
	err := i.callProc(proc, args, nil)
	if flow, ok := err.(*ControlFlow); ok && flow.Kind == "return" {
		return flow.Result, nil
	}
	return i.vars[""], err
}

// SetVariable sets a Tcl global variable for callback execution.
func (i *Interp) SetVariable(name, value string) { i.setVar(name, value, nil) }

// Variable returns the value visible to the currently executing callback scope.
func (i *Interp) Variable(name string) (string, bool) {
	return i.getVar(name, i.currentLocalVars)
}

// Substitute applies Tcl variable and command substitution in the active scope.
func (i *Interp) Substitute(value string) string {
	return i.substitute(value, i.currentLocalVars)
}

// BindSQLParams applies Tcl's db-command parameter substitution to SQL.
func (i *Interp) BindSQLParams(value string) string {
	return i.bindSQLParams(value, i.currentLocalVars)
}

// RegisterCallbackCommand exposes a harness-owned Tcl command during callbacks.
func (i *Interp) RegisterCallbackCommand(name string, fn func([]string) (string, error)) {
	i.callbackCommands[name] = fn
}

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
			if errors.Is(err, ErrStatementBudget) || errors.Is(err, ErrConversionDeadline) {
				// Runaway conversion: stop immediately (continuing would
				// keep unrolling the loop and allocating).
				return err
			}
			if ec, ok := err.(*ControlFlow); ok && ec.Kind == "return" {
				return nil
			}
			var unsupported *UnsupportedCommandError
			if errors.As(err, &unsupported) {
				// The command is not implemented: continuing would bake a
				// wrong value into the fixture. Fail the file instead.
				return err
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
func (i *Interp) substituteOptions(s string, localVars map[string]string, variables, commands, backslashes bool) string {
	var result strings.Builder
	for pos := 0; pos < len(s); {
		switch s[pos] {
		case '\\':
			if backslashes && pos+1 < len(s) {
				decoded, next := decodeBackslashEscape(s, pos)
				result.WriteString(decoded)
				pos = next
			} else {
				result.WriteByte(s[pos])
				pos++
			}
		case '$':
			if variables {
				var value string
				var ok bool
				pos, value, ok = i.substituteVar(s, pos, localVars)
				if ok {
					result.WriteString(value)
				}
			} else {
				result.WriteByte(s[pos])
				pos++
			}
		case '[':
			if commands {
				var value string
				pos, value = i.substituteCmd(s, pos, localVars)
				result.WriteString(value)
			} else {
				result.WriteByte(s[pos])
				pos++
			}
		default:
			result.WriteByte(s[pos])
			pos++
		}
	}
	return result.String()
}

func (i *Interp) substitute(s string, localVars map[string]string) string {
	var result strings.Builder
	pos := 0
	for pos < len(s) {
		ch := s[pos]

		if ch == '\\' && pos+1 < len(s) {
			decoded, next := decodeBackslashEscape(s, pos)
			result.WriteString(decoded)
			pos = next
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
func decodeBackslashEscape(s string, pos int) (string, int) {
	if pos+1 < len(s) && s[pos+1] == 'u' && pos+6 <= len(s) {
		code, err := strconv.ParseUint(s[pos+2:pos+6], 16, 16)
		if err == nil {
			return string(rune(code)), pos + 6
		}
	}
	return string(escapeChar(s[pos+1])), pos + 2
}

func escapeChar(next byte) byte {
	switch next {
	case 'a':
		return '\a'
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'v':
		return '\v'
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
	varName, next := i.readVarName(s, pos, localVars)
	if i.dynamicVars[strings.TrimPrefix(varName, "::")] {
		return next, "$" + varName, true
	}
	val, ok := i.getVar(varName, localVars)
	return next, val, ok
}

// readVarName scans a TCL variable name at s[pos] (letters/digits/_/: and an
// optional (array) element suffix), returning the name and the position after
// it.
func (i *Interp) readVarName(s string, pos int, localVars map[string]string) (string, int) {
	start := pos
	for pos < len(s) && (unicode.IsLetter(rune(s[pos])) || unicode.IsDigit(rune(s[pos])) || s[pos] == '_' || s[pos] == ':') {
		pos++
	}
	varName := s[start:pos]
	// Handle array element: $var(arr). Array indexes may contain nested
	// variable references, e.g. $cols($C(column)); locate the matching close
	// parenthesis instead of stopping at the inner variable's delimiter.
	if pos < len(s) && s[pos] == '(' {
		depth := 1
		end := pos + 1
		for end < len(s) && depth > 0 {
			switch s[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
			end++
		}
		if depth == 0 {
			index := i.substitute(s[pos+1:end-1], localVars)
			varName += "(" + index + ")"
			pos = end
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
	// Each bracket command starts with an empty result; commands that return
	// a value set it explicitly. Prevent a prior command's result leaking
	// through Tcl commands such as `db close` that return no value.
	i.vars[""] = ""
	// A failing command substitution must abort the conversion: swallowing
	// the error turns an unimplemented command into a silently wrong value
	// (round1: `string trimright` answered with its input).
	if err := i.execScript(cmdText, localVars); err != nil {
		if _, isControl := err.(*ControlFlow); !isControl {
			i.substErr = err
		}
	}
	return pos, i.vars[""]
}

// evalAllWords substitutes all words in a rawWord slice. A word carrying the
// {*} argument-expansion prefix is replaced by the elements of its evaluated
// value (TCL 8.5 semantics).
func (i *Interp) evalAllWords(words []rawWord, localVars map[string]string) ([]string, error) {
	result := make([]string, 0, len(words))
	for _, rw := range words {
		val, err := i.evalWord(rw, localVars)
		if err != nil {
			return nil, err
		}
		if rw.Expand {
			result = append(result, splitList(val)...)
			continue
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
					return ec
				}
				return err // break/continue propagate up
			}
			return err
		}
		if i.substErr != nil {
			err := i.substErr
			i.substErr = nil
			return err
		}
	}
	return nil
}

// UnsupportedCommandError marks a command the converter does not implement.
// It is fatal for the whole file: skipping the command would silently
// produce a wrong value (round1: an unknown `string` subcommand answered
// with its input, which produced 9,356 wrong expectations).
type UnsupportedCommandError struct {
	Cmd string
}

func (e *UnsupportedCommandError) Error() string {
	return "unsupported command: " + e.Cmd
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
	// Bound runaway loops: every command (including each loop iteration's
	// body) passes the statement budget / deadline check, so a loop that
	// never terminates stops here instead of accumulating statements forever.
	if err := i.checkRunaway(); err != nil {
		return err
	}
	// Substitute variables/commands in each word
	words := make([]string, 0, len(rawWords))
	for idx, rw := range rawWords {
		// A trailing `[db eval {...}]` / `[execsql {...}]` expected-result
		// argument of an assertion command is a runtime result comparison
		// (see runtimeCompareSQL); leave it unsubstituted so the query is not
		// executed a second time here.
		if idx > 0 && idx == len(rawWords)-1 && len(rawWords) >= 3 && isRuntimeCompareCommand(rawWords[0].Text) && rawWordIsRuntimeQuery(rw) {
			words = append(words, rw.Text)
			continue
		}
		val, err := i.evalWord(rw, localVars)
		if err != nil {
			return err
		}
		if rw.Expand {
			// TCL {*} argument expansion: splice the value's list elements.
			words = append(words, splitList(val)...)
			continue
		}
		words = append(words, val)
	}
	if len(words) == 0 {
		return nil
	}
	// A command substitution that failed aborts the enclosing command: the
	// value it would produce is wrong, and a wrong value in a fixture is a
	// silent failure.
	if i.substErr != nil {
		err := i.substErr
		i.substErr = nil
		return err
	}

	return i.dispatchCommand(words[0], rawWords, words[1:], localVars)
}

// isRuntimeCompareCommand reports whether cmd is a test-assertion command whose
// trailing word may be a bracketed runtime query used as the expected result.
func isRuntimeCompareCommand(cmd string) bool {
	switch cmd {
	case "do_test", "do_execsql_test", "do_execsql2_test", "do_timed_execsql_test":
		return true
	}
	return false
}

// rawWordIsRuntimeQuery reports whether the raw word is a bracketed
// `[db eval {...}]` or `[execsql {...}]` command substitution.
func rawWordIsRuntimeQuery(rw rawWord) bool {
	text := strings.TrimSpace(rw.Text)
	return strings.HasPrefix(text, "[db eval") || strings.HasPrefix(text, "[execsql")
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

	// A loop whose start value is only known at RUN TIME (the bound comes
	// from a query result) cannot be unrolled offline: capture it as a
	// runtime_loop step the harness executes (pager2's
	// `for {set k $now} {$k < $x} {incr k}` inserts one row per
	// iteration).
	if i.emitRuntimeLoop(start, cond, next, body, localVars) {
		return nil
	}

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

// foreachPair is one varList/list pair of a foreach command.
type foreachPair struct {
	vars  []string
	items []string
}

// cmdForeach implements `foreach varList list ?varList list ...? body`.
// Iteration follows TCL: the loop runs as many times as the longest list
// requires, and shorter lists contribute empty strings for their missing
// elements (rowvalue2's `foreach c $cList v $vList { ... }`).
func (i *Interp) cmdForeach(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 4 {
		return nil
	}
	// Parse: foreach varspec listvar ?varspec listvar ...? body
	// varspec can be a single var or {v1 v2}
	listRaw := rawWords[2]
	body := rawWords[len(rawWords)-1]
	if body.Braced && isDropAllTablesLoop(body.Text, listRaw.Text) {
		i.stmts = append(i.stmts, Stmt{Type: "drop_all_tables"})
		return nil
	}
	pairs, iterations, err := i.parseForeachPairs(rawWords, localVars)
	if err != nil {
		return err
	}
	for it := 0; it < iterations; it++ {
		i.assignForeachVars(pairs, it, localVars)
		if !body.Braced {
			continue
		}
		brk, err := i.execLoopBody(body, localVars)
		if err != nil {
			return err
		}
		if brk {
			break
		}
	}
	return nil
}

// parseForeachPairs collects the varList/list pairs of a foreach command
// (rawWords includes the command word; the final word is the body) and the
// iteration count TCL would use: enough iterations for the longest list.
func (i *Interp) parseForeachPairs(rawWords []rawWord, localVars map[string]string) ([]foreachPair, int, error) {
	nPairs := (len(rawWords) - 2) / 2
	pairs := make([]foreachPair, 0, nPairs)
	iterations := 0
	for p := 0; p < nPairs; p++ {
		varSpec, err := i.evalWord(rawWords[1+2*p], localVars)
		if err != nil {
			return nil, 0, err
		}
		listVal, err := i.evalWord(rawWords[2+2*p], localVars)
		if err != nil {
			return nil, 0, err
		}
		vars := splitList(varSpec)
		if len(vars) == 0 {
			continue
		}
		items := splitList(listVal)
		pairs = append(pairs, foreachPair{vars: vars, items: items})
		if n := (len(items) + len(vars) - 1) / len(vars); n > iterations {
			iterations = n
		}
	}
	return pairs, iterations, nil
}

// assignForeachVars binds every pair's variables for one iteration, filling
// values past a list's end with empty strings (TCL foreach semantics).
func (i *Interp) assignForeachVars(pairs []foreachPair, iteration int, localVars map[string]string) {
	for _, p := range pairs {
		for j, v := range p.vars {
			value := ""
			if k := iteration*len(p.vars) + j; k < len(p.items) {
				value = p.items[k]
			}
			i.setVar(v, value, localVars)
		}
	}
}

func isDropAllTablesLoop(body, listExpr string) bool {
	bodyLower := strings.ToLower(body)
	listLower := strings.ToLower(listExpr)
	return strings.Contains(listLower, "db eval") &&
		strings.Contains(listLower, "select name from sqlite_master where type = 'table'") &&
		strings.Contains(bodyLower, "db eval") &&
		strings.Contains(bodyLower, "drop table $")
}

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

// exprAtom renders a substituted value as a single expr operand. In Tcl a
// substituted value is an *atomic* operand: a numeric value keeps its numeric
// typing (bare literal), everything else is a quoted string. The value is
// never re-read as expr source, so a value like "'abc'" (the e_expr literals
// list keeps the quotes) compares as the string it is instead of failing the
// expression parse.
func exprAtom(v string) string {
	if v == "" {
		return `""`
	}
	if lit, ok := exprNumericLiteral(v); ok {
		return lit
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// exprNumericLiteral returns the literal form of a numeric value so that a
// substituted value keeps its Tcl typing: integers stay integers, everything
// else that parses as a number stays a double.
func exprNumericLiteral(v string) (string, bool) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", false
	}
	if i, err := strconv.ParseInt(t, 10, 64); err == nil {
		return strconv.FormatInt(i, 10), true
	}
	if _, err := strconv.ParseFloat(t, 64); err == nil {
		return t, true
	}
	return "", false
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

		// A condition that reads a runtime-captured value (the result of a
		// `catch` around a SQL statement, say) cannot be decided at
		// conversion time: BOTH branches must be captured, each carrying the
		// condition that guards it, so the harness picks the right one when
		// the statement actually runs. Dropping the untaken branch loses the
		// statements it would have executed (pager2's `ROLLBACK TO`).
		if i.exprReadsRuntimeVar(condVal) {
			// The condition mixes runtime values with compile-time ones
			// (pager2: `$now > $x`, where $now is captured from a query and
			// $x is a loop variable). The harness evaluates the condition
			// with its own evaluator, which knows only the runtime
			// variables - so bake the known ones in as literals and keep
			// the runtime ones as $name references.
			return i.execConditionalIf(rawWords, idx, i.bakeKnownExprVars(condVal, localVars), localVars)
		}
		// The testfixture's cksort helper (collate4.test) reports whether
		// the statement sorted by reading the test build's sort counter:
		//
		//   set ::sqlite_sort_count 0
		//   set data [execsql $sql]
		//   if {$::sqlite_sort_count} {set x sort} {set x nosort}
		//   lappend data $x
		//
		// The counter is only known at run time, so the statement that
		// ran carries the marker: the harness appends "sort" or "nosort"
		// to the rendered rows.
		if strings.Contains(condVal, "sqlite_sort_count") && len(i.stmts) > 0 {
			i.stmts[len(i.stmts)-1].SortMarker = true
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

// execConditionalIf captures both branches of an `if` whose condition reads a
// runtime value. The if-body's steps carry the condition itself; the
// else/elseif bodies carry the negated (or combined) condition.
func (i *Interp) execConditionalIf(rawWords []rawWord, idx int, cond string, localVars map[string]string) error {
	if idx < len(rawWords) {
		i.execConditionalBody(rawWords[idx], cond, localVars)
		idx++
	}
	negated := "!(" + cond + ")"
	for idx < len(rawWords) {
		kw, _ := i.evalWord(rawWords[idx], localVars)
		switch kw {
		case "elseif":
			idx++
			if idx >= len(rawWords) {
				return nil
			}
			cond2, next, err := i.evalIfCondition(rawWords, idx, localVars)
			if err != nil {
				return err
			}
			idx = next
			if i.exprReadsRuntimeVar(cond2) {
				cond2 = i.bakeKnownExprVars(cond2, localVars)
			}
			if !i.exprReadsRuntimeVar(cond2) {
				// The elseif condition is known at conversion time: keep
				// only the branch that will actually run.
				result, evalErr := EvalExpr(cond2, i, localVars)
				if evalErr == nil && isTrue(result) {
					i.execConditionalBody(rawWords[idx], negated, localVars)
					return nil
				}
				negated = negated + " && !(" + cond2 + ")"
				idx++
				continue
			}
			i.execConditionalBody(rawWords[idx], negated+" && ("+cond2+")", localVars)
			negated = negated + " && !(" + cond2 + ")"
			idx++
		case "else":
			idx++
			if idx < len(rawWords) && rawWords[idx].Braced {
				i.execConditionalBody(rawWords[idx], negated, localVars)
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

// execConditionalBody executes a branch body and stamps the steps it
// captured with the condition that guards them.
//
// The branch's *conversion-time* side effects must not leak: the branch that
// runs at run time is unknown, so executing both branches offline would apply
// both sets of variable changes (rtree2's `incr ii -1` in the branch that
// only runs on failure would make the loop unrolling never terminate). The
// local variables are therefore restored after the branch runs; the captured
// statements stay.
func (i *Interp) execConditionalBody(bodyWord rawWord, condition string, localVars map[string]string) {
	start := len(i.stmts)
	saved := make(map[string]string, len(localVars))
	for name, value := range localVars {
		saved[name] = value
	}
	savedGlobals := make(map[string]string, len(i.vars))
	for name, value := range i.vars {
		savedGlobals[name] = value
	}
	if bodyWord.Braced {
		i.execScript(bodyWord.Text, localVars)
	} else {
		body, err := i.evalWord(bodyWord, localVars)
		if err == nil {
			i.execScript(body, localVars)
		}
	}
	for name := range localVars {
		if _, existed := saved[name]; !existed {
			delete(localVars, name)
		}
	}
	for name, value := range saved {
		localVars[name] = value
	}
	for name := range i.vars {
		if _, existed := savedGlobals[name]; !existed {
			delete(i.vars, name)
		}
	}
	for name, value := range savedGlobals {
		i.vars[name] = value
	}
	for idx := start; idx < len(i.stmts); idx++ {
		// A nested conditional keeps the outer condition too: the step runs
		// only when BOTH hold. Overwriting would let an inner branch escape
		// its outer branch (pager2's `ROLLBACK TO` ran even when the outer
		// `if {$x == "x"}` did not apply).
		if existing := i.stmts[idx].Condition; existing != "" {
			i.stmts[idx].Condition = "(" + existing + ") && (" + condition + ")"
			continue
		}
		i.stmts[idx].Condition = condition
	}
}

// runtimeListValues renders the arguments of a Tcl `list` command as the
// values a runtime list step builds. It reports false when every argument is
// known at conversion time (the list is then an ordinary value).
func (i *Interp) runtimeListValues(words []rawWord, localVars map[string]string) ([]string, bool) {
	values := make([]string, 0, len(words))
	runtime := false
	for _, w := range words {
		text := w.Text
		if !w.Braced {
			if name, ok := runtimeVarRef(text); ok && i.dynamicVars[name] {
				values = append(values, "$"+name)
				runtime = true
				continue
			}
		}
		values = append(values, i.bakeKnownExprVars(text, localVars))
	}
	if !runtime {
		return nil, false
	}
	return values, true
}

// bracketedExprText extracts the expression text of a bracketed word that
// holds a command substitution like "[expr {$a - $b}]".
func bracketedExprText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[expr") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSpace(text[len("[expr") : len(text)-1])
	if len(inner) >= 2 && strings.HasPrefix(inner, "{") && strings.HasSuffix(inner, "}") {
		inner = strings.TrimSpace(inner[1 : len(inner)-1])
	}
	if inner == "" {
		return "", false
	}
	return inner, true
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
	i.updateTclCallbackSnapshots()
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
	if proc.Name == "set_file_format" && len(args) == 1 {
		format, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("set_file_format: invalid format %q", args[0])
		}
		i.stmts = append(i.stmts, Stmt{Type: "header_format", FileFormat: format, TestName: i.curTest})
		return nil
	}
	if proc.Name == "alter_table" && len(args) >= 2 {
		format := 2
		if len(args) >= 3 {
			if parsed, err := strconv.Atoi(args[2]); err == nil {
				format = parsed
			}
		}
		i.stmts = append(i.stmts, Stmt{Type: "alter_table", TableName: args[0], SchemaSQL: args[1], FileFormat: format, TestName: i.curTest})
		return nil
	}
	i.depth++
	if i.depth > 100 {
		i.depth--
		return fmt.Errorf("proc call depth exceeded")
	}
	// A procedure that receives a runtime query value post-processes the
	// rows the query returns (fts3expr3.test's strip_phrase_data). The
	// result is only known at run time, so the fixture records the
	// procedure and the harness applies it to the rendered rows.
	for _, arg := range args {
		if id, ok := runtimeValueID(arg); ok {
			if stmt := i.runtimeStatement(id); stmt != nil && stmt.TransformProc == "" {
				stmt.TransformProc = proc.Name
				stmt.TransformBody = proc.Body
				stmt.TransformArgs = append([]string(nil), proc.Args...)
			}
		}
	}
	// Create a new local scope (procs get their own scope in TCL)
	localVars := make(map[string]string)
	variadic := len(proc.Args) > 0 && proc.Args[len(proc.Args)-1] == "args"
	fixedArgs := len(proc.Args)
	if variadic {
		fixedArgs--
	}
	for idx, argName := range proc.Args[:fixedArgs] {
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
	if variadic {
		remaining := args[fixedArgs:]
		localVars["args"] = tclList(remaining)
	}
	previousLocalVars := i.currentLocalVars
	i.currentLocalVars = localVars
	err := i.execScript(proc.Body, localVars)
	i.currentLocalVars = previousLocalVars
	i.depth--
	if flow, ok := err.(*ControlFlow); ok && flow.Kind == "return" {
		i.vars[""] = flow.Result
		return nil
	}
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
	// An unknown subcommand must be an error, never a plausible answer:
	// returning the first argument silently produced wrong expectations
	// (round1: `string trimright` answered with its input, 9,356 leaves).
	return &UnsupportedCommandError{Cmd: "string " + sub}
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
func (i *Interp) cmdRegsub(args []string, localVars map[string]string) error {
	if len(args) >= 3 {
		pattern := args[0]
		str := args[1]
		repl := args[2]
		re, err := regexp.Compile(pattern)
		if err == nil {
			result := re.ReplaceAllString(str, repl)
			i.vars[""] = result
			if len(args) >= 4 {
				// The result is written back to the variable in the
				// current scope: fts3expr3.test's regsub writes a
				// procedure-local.
				i.setVar(args[3], result, localVars)
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
	if !strings.HasPrefix(name, "::") {
		if v, ok := i.vars["::"+name]; ok {
			return v, true
		}
	}
	return "", false
}

// emitRuntimeLoop captures a Tcl `for` loop whose start value is a runtime
// variable (one assigned from a query result) as a runtime_loop step: the
// harness evaluates the bounds and runs the body once per iteration. Reports
// whether the loop was captured.
func (i *Interp) emitRuntimeLoop(start, cond, next, body rawWord, localVars map[string]string) bool {
	// The loop variable: the start body must be a plain `set VAR VALUE`.
	startCmds := parseCommands(start.Text)
	if len(startCmds) != 1 || len(startCmds[0]) != 3 || startCmds[0][0].Text != "set" {
		return false
	}
	varName := strings.TrimSpace(startCmds[0][1].Text)
	if varName == "" {
		return false
	}
	value, ok := i.getVar(varName, localVars)
	if !ok || !strings.HasPrefix(value, "$") {
		return false
	}
	// Only a *dynamic* variable (assigned from a query result) makes the
	// bound runtime-dependent.
	name := strings.TrimPrefix(value, "$")
	if !i.dynamicVars[strings.TrimPrefix(name, "::")] {
		return false
	}
	// The condition keeps the loop variable (it is bound per iteration);
	// every other variable is substituted now.
	condVars := make(map[string]string, len(localVars))
	for k, v := range localVars {
		if k != varName {
			condVars[k] = v
		}
	}
	// The loop variable is bound per iteration at RUN time: mark it
	// dynamic so that references to it stay "$name" in the condition and
	// in the body (the harness binds it from the runtime variables).
	i.dynamicVars[varName] = true
	condText := i.substitute(cond.Text, condVars)
	// The increment: `incr VAR` or `incr VAR N`.
	step := 1
	nextCmds := parseCommands(next.Text)
	if len(nextCmds) == 1 && len(nextCmds[0]) >= 2 && nextCmds[0][0].Text == "incr" {
		if len(nextCmds[0]) >= 3 {
			if n, err := strconv.Atoi(strings.TrimSpace(nextCmds[0][2].Text)); err == nil {
				step = n
			}
		}
	} else {
		return false
	}
	mark := len(i.stmts)
	if _, err := i.execLoopBody(body, localVars); err != nil {
		return false
	}
	bodyStmts := append([]Stmt(nil), i.stmts[mark:]...)
	i.stmts = i.stmts[:mark]
	i.stmts = append(i.stmts, Stmt{
		Type:      "runtime_loop",
		LoopVar:   varName,
		LoopStart: value,
		LoopCond:  condText,
		LoopStep:  step,
		LoopBody:  bodyStmts,
		TestName:  i.curTest,
	})
	return true
}
