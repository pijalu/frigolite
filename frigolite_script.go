package frigolite

import (
	"strings"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/sql"
)

// Multi-statement script execution.
//
// A script submitted as one text (N statements joined by ';' — the speed1 /
// BenchmarkPerfInsert1 shape) is prepared and run statement by statement, which
// is sqlite3_exec's own loop: prepare from pzTail, run, prepare the next. The
// whole-script parser used to see one text whose normalized form is unique for
// every distinct literal sequence, so the per-statement template cache never
// applied and every statement paid the full parser (30 of 70 ms for 20 000
// point SELECTs, R13_RESEARCH.md §0b). Preparing each statement on its own puts
// the script on exactly the path a call-per-statement loop uses.
//
// The split is textual (the tokenizer's top-level semicolons), so a script
// whose statements are not separable that way must not run a prefix before the
// split is known to be sound: scriptSteps validates the fragments up front and
// declines the script otherwise (see scriptSteps).

// statementTexts returns the per-statement raw texts for the trace hooks
// (splitScriptStatements), short-circuiting the common single-statement form:
// when the batch contains no semicolon byte at all there is nothing to split
// (a string/blob literal containing one goes through the full tokenizer). A nil
// return tells stmtTextAt to use the batch text itself, skipping the tokenizer
// walk on every plain single-statement Exec/Query call.
func statementTexts(sqlStr string) []string {
	texts, _, ok := splitScriptStatements(sqlStr)
	if !ok {
		return nil
	}
	return texts
}

// traceStatementTexts returns the per-statement raw texts only when a
// trace/profile hook is registered. db.execPrepared ignores the text entirely
// while StmtHooksActive() is false, so a connection without hooks never pays
// the tokenizer walk over a batch script (the walk is one token per token of
// the whole text — for a 20 000-statement script it cost more than the
// per-statement prepare the script path saves).
func (db *DB) traceStatementTexts(sqlStr string) []string {
	if !db.engine.StmtHooksActive() {
		return nil
	}
	return statementTexts(sqlStr)
}

// stmtTextAt returns the raw source text for statement si, or "" when the
// prepared statement list is longer than the split texts. texts == nil means
// the batch had no semicolon anywhere, so the whole batch text is statement
// 0's text (and a multi-statement batch always split, so no later si exists).
func stmtTextAt(sqlStr string, texts []string, si int) string {
	if texts == nil {
		if si == 0 {
			return sqlStr
		}
		return ""
	}
	if si < len(texts) {
		return texts[si]
	}
	return ""
}

// splitScriptStatements cuts a SQL script into per-statement raw texts at
// top-level semicolons. The cut is token-aware (sqlite3_prepare's walk):
// semicolons inside string literals, blob literals, bracket identifiers, or
// comments never split, so each chunk is exactly the text of one statement
// including its trailing semicolon (sqlite3_stmt_sql semantics for the
// trace/profile hooks).
//
// trigger reports that the script carries a TRIGGER token. A CREATE TRIGGER
// body is the only statement SQLite has whose text contains a top-level
// semicolon, so it is the only script whose fragments are not statements on
// their own (see scriptSteps). ok=false reports a lexer error: the caller then
// takes the whole-script path, whose parser reports the canonical error
// message.
func splitScriptStatements(sqlStr string) (texts []string, trigger, ok bool) {
	if strings.IndexByte(sqlStr, ';') < 0 {
		return nil, false, true
	}
	if plain, trigger := plainScriptScan(sqlStr); plain {
		return splitPlainScript(sqlStr), trigger, true
	}
	return splitTokenizerScript(sqlStr)
}

// splitTokenizerScript is the general cut: the tokenizer walk that keeps
// semicolons inside string/blob literals, quoted identifiers and comments from
// splitting. ok=false reports a lexer error (the caller then parses the whole
// script so the canonical error surfaces).
func splitTokenizerScript(sqlStr string) (texts []string, trigger, ok bool) {
	tok := sql.NewTokenizer(sqlStr)
	start := 0
	for {
		t := tok.Next()
		switch t.Type {
		case sql.TokenEOF:
			if tail := sqlStr[start:]; strings.TrimSpace(tail) != "" {
				texts = append(texts, tail)
			}
			return texts, trigger, true
		case sql.TokenError, sql.TokenUnrecognized:
			return nil, false, false
		case sql.TokenSemicolon:
			text := sqlStr[start : t.Pos+len(t.Value)]
			texts = append(texts, strings.TrimLeft(text, " \t\n\r\v\f"))
			start = t.Pos + len(t.Value)
		case sql.TokenKeyword, sql.TokenIdentifier:
			// Cheap screen first: TRIGGER is seven bytes, and most tokens are
			// not even that long.
			if len(t.Value) == 7 && strings.EqualFold(t.Value, "TRIGGER") {
				trigger = true
			}
		}
	}
}

// plainScriptByte marks the bytes a script may consist of for the direct
// semicolon cut (splitPlainScript). Every one of them is a byte the tokenizer
// always classifies as itself — a real single-char token, a parameter or
// operator byte, an identifier/number byte, or whitespace — so a script made of
// them alone has no quoted token, comment, bracket identifier or unrecognized
// byte that could hide a semicolon or raise a lexer error. The absent bytes are
// exactly the ones that can: the quote chars (' " `), the bracket opener '[',
// the comment openers '-' and '/', and every byte the tokenizer's default case
// reports as "unrecognized token" ('#', '!', '{', '}', '^', '\', ...).
var plainScriptByte = func() [256]bool {
	var t [256]bool
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for c := 0x80; c < 256; c++ {
		t[c] = true // identifier bytes (isIdentStart accepts the high range)
	}
	for _, c := range []byte("_ \t\n\r\v\f;,().+*=<>|~%&?$@:") {
		t[c] = true
	}
	return t
}()

// plainScriptScan reports whether the script consists only of plain bytes (see
// plainScriptByte) and whether it carries the TRIGGER word. A plain script's
// semicolons are all top-level statement boundaries, so it can be cut on them
// directly — one byte pass instead of the tokenizer walk, which costs ~166 ns
// per statement of a 20 000-statement script (the split is on the per-statement
// hot path of every script Exec).
//
// The TRIGGER word cannot be hidden behind a quoted token in a plain script, so
// scanning for it is exact; a false negative here would split a CREATE TRIGGER
// body and is the one thing this fast path must not do.
func plainScriptScan(s string) (plain, trigger bool) {
	const word = "trigger"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !plainScriptByte[c] {
			return false, false
		}
		if (c == 't' || c == 'T') && i+len(word) <= len(s) && strings.EqualFold(s[i:i+len(word)], word) {
			trigger = true
		}
	}
	return true, trigger
}

// splitPlainScript cuts a plain script on its ';' bytes, producing exactly the
// chunks splitScriptStatements' tokenizer walk produces: each chunk carries its
// terminator, leading whitespace is trimmed, and a trailing chunk without a
// terminator is appended verbatim when it is not blank.
func splitPlainScript(s string) []string {
	texts := make([]string, 0, strings.Count(s, ";")+1)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ';' {
			continue
		}
		texts = append(texts, strings.TrimLeft(s[start:i+1], " \t\n\r\v\f"))
		start = i + 1
	}
	if tail := s[start:]; strings.TrimSpace(tail) != "" {
		texts = append(texts, tail)
	}
	return texts
}

// scriptStep is one top-level statement of a script the connection can run
// statement by statement (see scriptSteps).
type scriptStep struct {
	text string // canonical form the per-statement caches key on
	raw  string // source text as written (the trace hooks' text)
}

// canonicalStatementText reduces one raw statement chunk to the form the
// per-statement caches key on: surrounding whitespace and the trailing
// terminator are stripped so every statement of one shape normalizes to
// identical bytes (' ;' vs ';' and leading whitespace would otherwise miss the
// template another statement of the script stored). The parser re-appends the
// terminator (parse.ensureTrailingSemicolon) and trims the raw text it stores
// for sqlite_schema, so this is the text the whole-script parse worked on.
func canonicalStatementText(raw string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ";"))
}

// scriptSteps returns the top-level statements of a splittable script. ok=false
// reports that the script must go through the whole-script parser: fewer than
// two statements, a lexer error, a text over SQLITE_LIMIT_SQL_LENGTH, or a
// fragment that is not a statement on its own.
//
// A CREATE TRIGGER body is the only statement whose text carries top-level
// semicolons, so a script without a TRIGGER token splits into statements
// directly. A script with one validates every fragment BEFORE anything runs: an
// unsplittable script must fall back with nothing executed yet, because the
// statements already run could not be taken back.
func (db *DB) scriptSteps(sqlStr string) ([]scriptStep, bool) {
	if err := db.engine.CheckScriptLength(sqlStr); err != nil {
		return nil, false
	}
	texts, trigger, ok := splitScriptStatements(sqlStr)
	if !ok {
		return nil, false
	}
	steps := make([]scriptStep, 0, len(texts))
	for _, raw := range texts {
		text := canonicalStatementText(raw)
		if text == "" {
			continue // empty statement: SQLite treats it as a no-op
		}
		steps = append(steps, scriptStep{text: text, raw: raw})
	}
	if len(steps) < 2 {
		return nil, false
	}
	if trigger {
		for _, st := range steps {
			if _, err := db.engine.PrepareExec(st.text); err != nil {
				return nil, false
			}
		}
	}
	return steps, true
}

// execScript runs a splittable script one statement at a time: sqlite3_exec's
// loop over sqlite3_prepare (prepare from pzTail, run, prepare the next).
//
// Preparing and running a statement before the next prepare is what lets the
// per-statement template cache serve every statement of a script with the same
// slot-path clone a call-per-statement loop gets: the engine's live clone per
// (template, exec depth) is reused only after the statement that held it has
// finished (clone_scratch.go), which is exactly the order here.
func (db *DB) execScript(steps []scriptStep) *Result {
	// A whole-batch BEGIN EXCLUSIVE is a single-statement text (see Exec), so
	// it cannot reach this path.
	var lastResult *exec.Result
	for _, step := range steps {
		stmts, err := db.engine.PrepareExec(step.text)
		if err != nil {
			db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
			return &Result{Error: err}
		}
		for _, stmt := range stmts {
			res := db.execPrepared(stmt, step.raw)
			if res.Error != nil {
				db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
				return execResult(res)
			}
			lastResult = res
			if res.LastInsertRowID > 0 {
				db.lastRowID = res.LastInsertRowID
			}
		}
	}
	db.engine.SetLastErr("", "")
	if lastResult == nil {
		return &Result{}
	}
	return execResult(lastResult)
}

// runScript is execScript's Query counterpart: it concatenates the rows of
// every statement of the script (runSQLText's batch fold) while preparing and
// running each statement in turn.
func (db *DB) runScript(steps []scriptStep) *exec.Result {
	var allRows [][]interface{}
	var allColumns []string
	var last *exec.Result
	n := 0
	for _, step := range steps {
		stmts, err := db.engine.PrepareExec(step.text)
		if err != nil {
			db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
			return &exec.Result{Error: err}
		}
		for _, stmt := range stmts {
			res := db.execPrepared(stmt, step.raw)
			if res.Error != nil {
				db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
				return res
			}
			expandResultZeroBlobs(res)
			allRows, allColumns = foldQueryResult(allRows, allColumns, res, true)
			if res.LastInsertRowID > 0 {
				db.lastRowID = res.LastInsertRowID
			}
			last = res
			n++
		}
	}
	db.engine.SetLastErr("", "")
	if last == nil {
		return &exec.Result{}
	}
	if n == 1 {
		// One statement after all (comment-only fragments carry none):
		// runSQLText's single-statement pass-through semantics.
		if len(last.Rows) == 0 {
			last.Rows = nil
		}
		last.Columns = allColumns
		return last
	}
	out := *last
	out.Rows, out.Columns = allRows, allColumns
	return &out
}
