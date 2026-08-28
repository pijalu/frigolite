// Package main implements the tcl2go tool.
//
// This file collects TCL variable/function metadata from parsed command
// trees (sqlite3 targets, set vars, incr-only vars, proc values).
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// collectSqlite3Targets recursively walks TCL commands and returns a set of
// variable names that are targets of sqlite3 commands (these are *frigolite.DB,
// not string, so must NOT be pre-declared as string).
func collectSqlite3Targets(cmds [][]tcl.RawWord) map[string]bool {
	result := make(map[string]bool)
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		if cmd[0].Text == "sqlite3" && len(cmd) >= 2 {
			// Dynamic targets ($con) hold connection NAMES (strings), not
			// handles — they must stay plain string variables.
			if !strings.HasPrefix(cmd[1].Text, "$") {
				gv := tclVarToGo(cmd[1].Text)
				if gv != "" {
					result[gv] = true
				}
			}
		}
		// `set VAR [sqlite3_open FILE]` is the legacy TCL form used by
		// tableapi.test and similar C-API suites.  Its assignment target is
		// also a database handle, despite the outer command being `set`.
		if cmd[0].Text == "set" && len(cmd) >= 2 {
			openExpr := false
			for _, word := range cmd[1:] {
				if strings.Contains(strings.TrimSpace(word.Text), "sqlite3_open") {
					openExpr = true
					break
				}
			}
			if openExpr {
				gv := tclVarToGo(cmd[1].Text)
				if gv != "" {
					result[gv] = true
				}
			}
		}
		collectSqlite3TargetsBodies(result, cmd)
	}
	return result
}

// collectSqlite3TargetsBodies recurses into braced sub-bodies of a command,
// merging any sqlite3 targets found.
func collectSqlite3TargetsBodies(result map[string]bool, cmd []tcl.RawWord) {
	for i := 1; i < len(cmd); i++ {
		if cmd[i].Braced && len(cmd[i].Text) > 10 {
			parsed := parseCommands(cmd[i].Text)
			if len(parsed) > 0 {
				for k, v := range collectSqlite3Targets(parsed) {
					result[k] = v
				}
			}
		}
	}
}

// collectSetVars recursively walks TCL commands and collects all variable
// names that are assigned via set, incr, foreach, or for-init commands.
func collectSetVars(cmds [][]tcl.RawWord) []string {
	c := &varCollector{}
	c.collect(cmds)
	return c.names
}

// collectArrayMapVars recursively walks TCL commands and collects array names
// that are assigned with a DYNAMIC key (`set arr($keyvar) V` where $keyvar is a
// runtime variable). Such arrays must be transpiled to Go maps (a literal-key
// `set arr(K) V` can use the arr_K variable form). Returns the base array
// names.
func collectArrayMapVars(cmds [][]tcl.RawWord) map[string]bool {
	result := make(map[string]bool)
	var walk func([][]tcl.RawWord)
	walk = func(cc [][]tcl.RawWord) {
		for _, cmd := range cc {
			if len(cmd) == 0 {
				continue
			}
			if cmd[0].Text == "set" && len(cmd) >= 2 {
				name := cmd[1].Text
				if idx := strings.Index(name, "("); idx > 0 && strings.HasSuffix(name, ")") {
					key := name[idx+1 : len(name)-1]
					if strings.HasPrefix(key, "$") {
						result[name[:idx]] = true
					}
				}
			}
			for i := 1; i < len(cmd); i++ {
				if cmd[i].Braced && len(cmd[i].Text) > 2 {
					if parsed := parseCommands(cmd[i].Text); len(parsed) > 0 {
						walk(parsed)
					}
				}
			}
		}
	}
	walk(cmds)
	return result
}

// varCollector accumulates variable names assigned by TCL commands, recursing
// into braced sub-bodies.
type varCollector struct {
	names []string
}

func (c *varCollector) collect(cmds [][]tcl.RawWord) {
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		c.collectCommand(cmd)
	}
}

// collectCommand dispatches one command to its variable collector.
func (c *varCollector) collectCommand(cmd []tcl.RawWord) {
	switch cmd[0].Text {
	case "set":
		c.collectSet(cmd)
	case "incr", "lappend", "append":
		c.collectIncr(cmd)
	case "foreach":
		c.collectForeach(cmd)
	case "for":
		c.collectFor(cmd)
	case "while":
		c.collectWhile(cmd)
	case "if":
		c.collectIf(cmd)
	case "do_test", "do_execsql_test", "do_catchsql_test", "do_eqp_test",
		"do_timed_execsql_test", "do_execsql2_test":
		c.collectDoTest(cmd)
	case "catch":
		c.collectCatch(cmd)
	case "db":
		c.collectDB(cmd)
	default:
		c.collectDefault(cmd)
	}
}

func (c *varCollector) collectSet(cmd []tcl.RawWord) {
	if len(cmd) >= 2 {
		c.names = append(c.names, cmd[1].Text)
	}
}

func (c *varCollector) collectIncr(cmd []tcl.RawWord) {
	if len(cmd) >= 2 {
		c.names = append(c.names, cmd[1].Text)
	}
}

func (c *varCollector) collectForeach(cmd []tcl.RawWord) {
	if len(cmd) >= 2 {
		// cmd[1] is the variable list (possibly braced with multiple vars)
		varNames := strings.Fields(cmd[1].Text)
		c.names = append(c.names, varNames...)
	}
	// The list elements (cmd[2]) may be braced scripts that a later `eval
	// $var` inlines (backup.test's `foreach zOpenScript {...} { eval
	// $zOpenScript }`). Collect set-vars from those scripts so the variables
	// are pre-declared at function scope (visible across the eval switch).
	if len(cmd) >= 3 {
		for _, v := range literalForeachList(cmd[2].Text) {
			c.collect(parseCommands(stripOuterBraces(v)))
		}
	}
	// Recurse into body (cmd[3])
	if len(cmd) >= 4 && cmd[3].Braced {
		c.collect(parseCommands(cmd[3].Text))
	}
}

func (c *varCollector) collectFor(cmd []tcl.RawWord) {
	// cmd[1] is init body, cmd[4] is loop body
	if len(cmd) >= 2 && cmd[1].Braced {
		c.collect(parseCommands(cmd[1].Text))
	}
	if len(cmd) >= 5 && cmd[4].Braced {
		c.collect(parseCommands(cmd[4].Text))
	}
	// Also process next (cmd[3])
	if len(cmd) >= 4 && cmd[3].Braced {
		c.collect(parseCommands(cmd[3].Text))
	}
}

func (c *varCollector) collectWhile(cmd []tcl.RawWord) {
	if len(cmd) >= 3 && cmd[2].Braced {
		c.collect(parseCommands(cmd[2].Text))
	}
}

func (c *varCollector) collectIf(cmd []tcl.RawWord) {
	// Walk if/elseif/else blocks
	for i := 1; i < len(cmd); i++ {
		if cmd[i].Braced && len(cmd[i].Text) > 0 {
			// Check if this looks like a body (not a condition)
			// Heuristic: bodies are after conditions and keywords
			parsed := parseCommands(cmd[i].Text)
			if parsed != nil {
				c.collect(parsed)
			}
		}
	}
}

func (c *varCollector) collectDoTest(cmd []tcl.RawWord) {
	if len(cmd) >= 3 && cmd[2].Braced {
		c.collect(parseCommands(cmd[2].Text))
	}
}

func (c *varCollector) collectCatch(cmd []tcl.RawWord) {
	if len(cmd) >= 2 && cmd[1].Braced {
		c.collect(parseCommands(cmd[1].Text))
	}
	if len(cmd) >= 3 {
		c.names = append(c.names, cmd[2].Text) // catch error variable
	}
}

func (c *varCollector) collectDB(cmd []tcl.RawWord) {
	// db transaction {body} — recurse
	if len(cmd) >= 3 && cmd[1].Text == "transaction" && cmd[2].Braced {
		c.collect(parseCommands(cmd[2].Text))
	}
}

func (c *varCollector) collectDefault(cmd []tcl.RawWord) {
	// Recognize TCL namespace helpers so `variable xyz 321` inside
	// `namespace eval ::ns { variable xyz 321 }` is collected.
	if cmd[0].Text == "namespace" && len(cmd) >= 4 && cmd[1].Text == "eval" && cmd[3].Braced {
		// Register the namespace name for later ::ns::var rewriting, and
		// collect vars inside the namespace body.
		nsName := ""
		if len(cmd) >= 3 {
			nsName = strings.TrimPrefix(strings.TrimSpace(cmd[2].Text), "::")
			nsName = strings.TrimSpace(nsName)
		}
		parsed := parseCommands(cmd[3].Text)
		// Qualify plain `variable NAME` inside the namespace body as NS::NAME
		// so they resolve to the same Go var as $NS::NAME references.
		if nsName != "" && len(parsed) > 0 {
			for i := range parsed {
				if len(parsed[i]) >= 2 && parsed[i][0].Text == "variable" && !strings.Contains(parsed[i][1].Text, "::") && !strings.Contains(parsed[i][1].Text, "$") {
					parsed[i][1].Text = nsName + "::" + parsed[i][1].Text
				}
			}
		}
		if len(parsed) > 0 {
			c.collect(parsed)
		}
		return
	}
	if cmd[0].Text == "variable" && len(cmd) >= 2 {
		c.names = append(c.names, cmd[1].Text)
		// variable NAME VALUE is an assignment — also capture the optional initial value
		return
	}
	// For any other command, try to find braced sub-bodies
	for i := 1; i < len(cmd); i++ {
		if cmd[i].Braced && len(cmd[i].Text) > 10 {
			// Heuristic: only recurse if the body contains TCL commands
			if strings.Contains(cmd[i].Text, "\n") || strings.Contains(cmd[i].Text, "set ") {
				parsed := parseCommands(cmd[i].Text)
				if len(parsed) > 0 {
					c.collect(parsed)
				}
			}
		}
	}
}

// collectRefVars scans raw TCL source text for all $var references and returns
// the variable names (without $). This catches variables that are referenced
// but never set (external TCL variables).
// collectIncrOnlyVars returns variables that appear only in `incr` (never in
// `set VAR value`). TCL treats an undefined var as 0 for incr, so these must
// be initialized to "0" instead of "" in the generated Go.
func collectIncrOnlyVars(cmds [][]tcl.RawWord) map[string]bool {
	c := &incrVarCollector{
		incrVars: make(map[string]bool),
		setVars:  make(map[string]bool),
	}
	c.walk(cmds)
	only := make(map[string]bool)
	for v := range c.incrVars {
		if !c.setVars[v] {
			only[v] = true
		}
	}
	return only
}

// incrVarCollector walks TCL commands, tracking which variables appear in
// `incr` (incrVars) and which are initialized by `set VAR value` (setVars).
type incrVarCollector struct {
	incrVars map[string]bool
	setVars  map[string]bool
}

func (c *incrVarCollector) walk(cs [][]tcl.RawWord) {
	for _, cmd := range cs {
		if len(cmd) == 0 {
			continue
		}
		switch cmd[0].Text {
		case "incr":
			if len(cmd) >= 2 {
				c.incrVars[cmd[1].Text] = true
			}
		case "set":
			// set VAR value — a value that is not a bare variable reference
			// initializes the var; mark it as NOT incr-only.
			if len(cmd) >= 3 {
				c.setVars[cmd[1].Text] = true
			}
		case "foreach", "for", "while", "if", "catch", "db":
			c.walkBraced(cmd)
		}
	}
}

// walkBraced recurses into every braced word of a control-flow command.
func (c *incrVarCollector) walkBraced(cmd []tcl.RawWord) {
	for i := 1; i < len(cmd); i++ {
		if cmd[i].Braced {
			c.walk(tcl.ParseCommands(cmd[i].Text))
		}
	}
}

// constantProcValue extracts a constant return value from a simple proc body
// like "return 1" or "{ return 1 }". It returns (value, true) for bodies
// whose only statement returns an integer constant; otherwise ("", false).
// SQLite's test suite uses such procs (e.g. `proc f {args} { return 1 }`)
// registered with `db func f f` as always-returning scalar SQL functions.
func constantProcValue(body string) string {
	body = strings.TrimSpace(body)
	// Strip one level of braces.
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	if !strings.HasPrefix(strings.ToLower(body), "return ") {
		return ""
	}
	val := strings.TrimSpace(body[len("return "):])
	// Only integer literals (optionally negative) are portable to Go int64.
	if _, err := strconv.ParseInt(val, 10, 64); err == nil {
		return val
	}
	return ""
}

// rangeListProcValue extracts a generated column-name list from a proc body
// like the vtabI.test helper:
//
//	proc all_col_list {} {
//	  set L [list]
//	  for {set i 1} {$i <= 100} {incr i} { lappend L "c$i" }
//	  set L
//	}
//
// The body builds "c1 c2 ... cN" by lappending a prefix plus the loop index
// into a list. Returns the resulting TCL list string (space-separated), or ""
// when the body is not a range-list build.
func rangeListProcValue(body string) string {
	body = trimBraceBlock(body)
	lines := strings.Split(body, "\n")
	if len(lines) != 3 {
		return ""
	}
	// set VAR [list]
	first := strings.Fields(strings.TrimSpace(lines[0]))
	if len(first) != 3 || first[0] != "set" || first[2] != "[list]" {
		return ""
	}
	varName := first[1]
	// set VAR  (returns the built list)
	last := strings.Fields(strings.TrimSpace(lines[2]))
	if len(last) != 2 || last[0] != "set" || last[1] != varName {
		return ""
	}
	idxVar, n, item, ok := rangeListForLoop(lines[1], varName)
	if !ok {
		return ""
	}
	prefix, ok := rangeListItem(item, varName, idxVar)
	if !ok {
		return ""
	}
	var parts []string
	for i := 1; i <= n; i++ {
		parts = append(parts, fmt.Sprintf("%s%d", prefix, i))
	}
	return strings.Join(parts, " ")
}

// rangeListForLoop parses the "for {set IDX 1} {$IDX <= N} {incr IDX}
// { lappend VAR ... }" middle line of a range-list proc body. Returns the
// loop index variable, the upper bound, the lappend body text, and whether
// the line matches the expected shape.
func rangeListForLoop(midLine, varName string) (string, int, string, bool) {
	mid := strings.TrimSpace(midLine)
	const forPrefix = "for "
	if !strings.HasPrefix(mid, forPrefix) {
		return "", 0, "", false
	}
	// Four braced groups: init, cond, incr, body.
	groups := strings.Split(mid[len(forPrefix):], "} {")
	if len(groups) != 4 {
		return "", 0, "", false
	}
	idxVar, ok := rangeListInit(groups[0])
	if !ok {
		return "", 0, "", false
	}
	n, ok := rangeListCond(groups[1], idxVar)
	if !ok {
		return "", 0, "", false
	}
	if !rangeListIncr(groups[2], idxVar) {
		return "", 0, "", false
	}
	item, ok := rangeListLappend(groups[3], varName)
	if !ok {
		return "", 0, "", false
	}
	return idxVar, n, item, true
}

// rangeListInit parses the for-loop initializer "set IDX 1".
func rangeListInit(group string) (string, bool) {
	init := strings.TrimSpace(strings.TrimPrefix(group, "{"))
	words := strings.Fields(init)
	if len(words) != 3 || words[0] != "set" || words[2] != "1" {
		return "", false
	}
	return words[1], true
}

// rangeListCond parses the for-loop condition "$IDX <= N", returning N.
func rangeListCond(group, idxVar string) (int, bool) {
	words := strings.Fields(strings.TrimSpace(group))
	if len(words) != 3 || words[0] != "$"+idxVar || words[1] != "<=" {
		return 0, false
	}
	n, err := strconv.Atoi(words[2])
	if err != nil || n < 1 || n > 10000 {
		return 0, false
	}
	return n, true
}

// rangeListIncr validates the for-loop increment "incr IDX".
func rangeListIncr(group, idxVar string) bool {
	words := strings.Fields(strings.TrimSpace(group))
	if len(words) < 2 || len(words) > 3 || words[0] != "incr" || words[1] != idxVar {
		return false
	}
	return true
}

// rangeListLappend parses the lappend body "lappend VAR ITEM" and returns
// the raw item token.
func rangeListLappend(group, varName string) (string, bool) {
	body := strings.TrimSpace(strings.TrimSuffix(group, "}"))
	words := strings.Fields(body)
	if len(words) != 3 || words[0] != "lappend" || words[1] != varName {
		return "", false
	}
	return words[2], true
}

// rangeListItem extracts the fixed prefix from a lappend item of the form
// "PREFIX$IDX": the item must end with $IDX. Returns the prefix (possibly
// empty for a bare "$IDX") and whether the item matches the expected shape.
func rangeListItem(item, varName, idxVar string) (string, bool) {
	item = strings.Trim(item, `"`)
	suffix := "$" + idxVar
	if !strings.HasSuffix(item, suffix) {
		return "", false
	}
	return strings.TrimSuffix(item, suffix), true
}

// trimBraceBlock strips one surrounding pair of braces from a TCL body when
// present (TCL renders a braced block as { ... }).
func trimBraceBlock(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	return body
}

// joinProcValue extracts the separator from a join proc body like
// "{ return [join $args -] }" (used by func8.test: `proc joinx {args}
// {return [join $args -]}`). Returns the separator, or "" when the body is
// not a join of $args.
func joinProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	body = strings.TrimSpace(strings.TrimPrefix(body, "return"))
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "[join $args ") || !strings.HasSuffix(body, "]") {
		return ""
	}
	sep := strings.TrimSpace(body[len("[join $args ") : len(body)-1])
	sep = strings.Trim(sep, "{}")
	return sep
}

// prefixProcValue extracts the prefix from a prefix proc body like
// "{ return \"window: $args\" }" (used by window6.test: `proc winproc
// {args} { return \"window: $args\" }`). The proc joins its args with a
// space and prepends the fixed prefix. Returns the prefix, or "" when the
// body is not a prefix-join of $args.
func prefixProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	body = strings.TrimSpace(strings.TrimPrefix(body, "return"))
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, `"`) || !strings.HasSuffix(body, `"`) {
		return ""
	}
	inner := body[1 : len(body)-1]
	if !strings.HasSuffix(inner, "$args") {
		return ""
	}
	prefix := strings.TrimSuffix(inner, "$args")
	if !strings.HasSuffix(prefix, ": ") && !strings.HasSuffix(prefix, " ") && prefix != "" {
		return ""
	}
	return prefix
}

// counterProcValue extracts the incremented variable name from a counter proc
// body like "{ incr ::udf }". It returns the Go variable name, or "" when the
// body is not a single incr of a namespace variable.
func counterProcValue(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	if !strings.HasPrefix(strings.ToLower(body), "incr ") {
		return ""
	}
	val := strings.TrimSpace(body[len("incr "):])
	if !strings.HasPrefix(val, "::") {
		return ""
	}
	goName := tclVarToGo(strings.TrimPrefix(val, "::"))
	if !isValidGoIdent(goName) {
		return ""
	}
	return goName
}

// IncrProcInfo describes a `proc NAME {args} { incr ::VAR [N]; return K }`
// body (vtabH's like()/glob()/regexp() counter functions).
type IncrProcInfo struct {
	GoVar  string
	Amount int
	Ret    int
}

// collectIncrRetFuncs finds proc definitions whose body increments a
// namespaced variable and returns a constant, mapping the proc name to the
// increment details so `db func OP -argcount N NAME` registrations can emit
// a faithful closure.
func collectIncrRetFuncs(cmds [][]tcl.RawWord) map[string]IncrProcInfo {
	result := make(map[string]IncrProcInfo)
	walkCommands(cmds, func(cmd []tcl.RawWord) {
		if cmd[0].Text != "proc" || len(cmd) < 4 {
			return
		}
		// cmd layout: proc NAME {params} {body}
		body := strings.TrimSpace(cmd[3].Text)
		if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
			body = strings.TrimSpace(body[1 : len(body)-1])
		}
		parts := strings.FieldsFunc(body, func(r rune) bool { return r == ';' || r == '\n' })
		if len(parts) != 2 {
			return
		}
		incr := strings.Fields(strings.TrimSpace(parts[0]))
		retPart := strings.TrimSpace(parts[1])
		if len(incr) < 2 || !strings.EqualFold(incr[0], "incr") {
			return
		}
		if !strings.HasPrefix(incr[1], "::") {
			return
		}
		goVar := tclVarToGo(strings.TrimPrefix(incr[1], "::"))
		if !isValidGoIdent(goVar) {
			return
		}
		amount := 1
		if len(incr) >= 3 {
			n, err := strconv.Atoi(incr[2])
			if err != nil {
				return
			}
			amount = n
		}
		retFields := strings.Fields(retPart)
		if len(retFields) != 2 || !strings.EqualFold(retFields[0], "return") {
			return
		}
		ret, err := strconv.Atoi(retFields[1])
		if err != nil {
			return
		}
		result[cmd[1].Text] = IncrProcInfo{GoVar: goVar, Amount: amount, Ret: ret}
	})
	return result
}

// dbFuncArgCount extracts the N of an `-argcount N` flag pair.
func dbFuncArgCount(rest []tcl.RawWord) (int, bool) {
	for i, a := range rest {
		arg := strings.TrimSpace(a.Text)
		if arg == "-argcount" && i+1 < len(rest) {
			if n, err := strconv.Atoi(strings.TrimSpace(rest[i+1].Text)); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}
