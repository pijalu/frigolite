package vtab

import (
	"fmt"
	"strconv"
	"strings"
)

// BestIndexPlan runs Tcl xBestIndex when a fixture runtime is present.
func moduleCommandWords(argument string) []string {
	argument = strings.TrimSpace(argument)
	argument = strings.Trim(argument, `"'`)
	return tclWords(argument)
}

func (v *tclCmdVTab) BestIndexPlan(info *IndexInfo) error {
	if v.runtime == nil {
		return nil
	}
	result, err := v.runtime.BestIndex(v.procName, v.moduleArgs, info)
	if err != nil {
		return err
	}
	return applyTclBestIndexResult(result, info)
}

// tclCmdCursor is a cursor populated by Tcl xFilter's returned SQL query.
type tclCmdCursor struct {
	vtab   *tclCmdVTab
	rows   [][]interface{}
	rowids []int64
	index  int
}

func (v *tclCmdVTab) Open() (Cursor, error) {
	return &tclCmdCursor{vtab: v, index: -1}, nil
}

func (c *tclCmdCursor) FilterPlan(idxNum int, idxStr string, argv []interface{}) error {
	if c.vtab.runtime == nil {
		return nil
	}
	result, err := c.vtab.runtime.Filter(c.vtab.procName, c.vtab.moduleArgs, idxNum, idxStr, argv)
	if err != nil {
		return err
	}
	pairs := tclWords(result)
	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.EqualFold(pairs[i], "sql") {
			return fmt.Errorf("unexpected: %s", pairs[i])
		}
		c.rows, c.rowids, err = c.vtab.runtime.Query(pairs[i+1])
		if err != nil {
			return fmt.Errorf("unexpected: %w", err)
		}
	}
	c.index = -1
	return nil
}

func (c *tclCmdCursor) Next() bool {
	c.index++
	return c.index < len(c.rows)
}
func (c *tclCmdCursor) Column(idx int) (interface{}, error) {
	if c.index < 0 || c.index >= len(c.rows) || idx+1 >= len(c.rows[c.index]) {
		return nil, fmt.Errorf("no row")
	}
	return c.rows[c.index][idx+1], nil
}
func (c *tclCmdCursor) Rowid() int64 {
	if c.index >= 0 && c.index < len(c.rowids) {
		return c.rowids[c.index]
	}
	if c.index >= 0 && c.index < len(c.rows) && len(c.rows[c.index]) > 0 {
		if value, ok := c.rows[c.index][0].(int64); ok {
			return value
		}
	}
	return 0
}
func (c *tclCmdCursor) Close() error { return nil }

func applyTclBestIndexResult(result string, info *IndexInfo) error {
	words := tclWords(result)
	if len(words)%2 != 0 {
		return fmt.Errorf("malformed Tcl xBestIndex result")
	}
	for i := 0; i < len(words); i += 2 {
		if err := applyTclBestIndexPair(words[i], words[i+1], info); err != nil {
			return err
		}
	}
	return nil
}

func applyTclBestIndexPair(key, val string, info *IndexInfo) error {
	switch strings.ToLower(key) {
	case "orderby":
		info.OrderByConsumed = val == "1" || strings.EqualFold(val, "true")
	case "cost":
		if n, err := strconv.ParseFloat(val, 64); err == nil {
			info.EstimatedCost = n
		}
	case "rows":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			info.EstimatedRows = n
		}
	case "idxnum":
		if n, err := strconv.Atoi(val); err == nil {
			info.IdxNum = n
		}
	case "idxstr":
		info.IdxStr = val
	case "use", "omit":
		return setTclConstraintUsage(strings.ToLower(key), val, info)
	default:
		return fmt.Errorf("unexpected: %s", key)
	}
	return nil
}

func setTclConstraintUsage(key, value string, info *IndexInfo) error {
	idx, err := strconv.Atoi(value)
	if err != nil || idx < 0 || idx >= len(info.Usage) {
		return fmt.Errorf("invalid %s constraint index %q", key, value)
	}
	info.Usage[idx].ArgvIndex = nextTclArgvIndex(info.Usage)
	info.Usage[idx].Omit = key == "omit"
	return nil
}

func nextTclArgvIndex(usage []ConstraintUsage) int {
	n := 1
	for _, item := range usage {
		if item.ArgvIndex >= n {
			n = item.ArgvIndex + 1
		}
	}
	return n
}

// tclWords parses braced and unbraced words in callback Tcl lists.
func tclWords(text string) []string {
	var words []string
	for pos := 0; pos < len(text); {
		pos = skipTclSpaces(text, pos)
		if pos == len(text) {
			break
		}
		word, next := parseTclWord(text, pos)
		words = append(words, word)
		pos = next
	}
	return words
}

func skipTclSpaces(text string, pos int) int {
	for pos < len(text) && isTclSpace(text[pos]) {
		pos++
	}
	return pos
}

func isTclSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

func parseTclWord(text string, pos int) (string, int) {
	if text[pos] != '{' {
		return parseSimpleTclWord(text, pos)
	}
	return parseBracedTclWord(text, pos+1)
}

func parseSimpleTclWord(text string, pos int) (string, int) {
	start := pos
	for pos < len(text) && !isTclSpace(text[pos]) {
		pos++
	}
	return text[start:pos], pos
}

func parseBracedTclWord(text string, pos int) (string, int) {
	var word strings.Builder
	depth := 1
	for pos < len(text) && depth > 0 {
		switch text[pos] {
		case '{':
			depth++
			if depth > 1 {
				word.WriteByte(text[pos])
			}
		case '}':
			depth--
			if depth > 0 {
				word.WriteByte(text[pos])
			}
		default:
			word.WriteByte(text[pos])
		}
		pos++
	}
	return word.String(), pos
}
