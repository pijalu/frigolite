package vtab

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// VocabSource supplies the vocabulary words and the edit-cost matrix backing
// an approximate_match virtual table (ext/misc/amatch.c).
type VocabSource interface {
	// VocabWords returns the words of the vocabulary column, in table order.
	VocabWords(table, wordCol string) ([]string, error)
	// CostRules returns the edit-cost rules of the cost table.
	CostRules(table string) ([]AmatchCostRule, error)
}

// AmatchCostRule is one row of the edit_distances table
// (iLang, cFrom, cTo, Cost).
type AmatchCostRule struct {
	Lang int64
	From string
	To   string
	Cost int64
}

// ApproximateMatchModule implements the approximate_match virtual table:
// given a vocabulary (any table/column) and a weighted edit-cost matrix, each
// scan computes every vocabulary word's edit distance from a MATCH target.
//
// Schema (amatch.c): word, distance, language, command HIDDEN, nword HIDDEN.
// A scan without a `word MATCH <target>` constraint yields no rows.
type ApproximateMatchModule struct {
	src VocabSource
}

// NewApproximateMatchModule builds the module over a vocabulary source.
func NewApproximateMatchModule(src VocabSource) *ApproximateMatchModule {
	return &ApproximateMatchModule{src: src}
}

const (
	amatchColWord     = 0
	amatchColDistance = 1
	amatchColLanguage = 2
	amatchColCommand  = 3
	amatchColNWord    = 4

	// amatchDefaultCost is applied to operations with no explicit rule
	// (amatch.c's default edit cost).
	amatchDefaultCost = 100
)

// amatchVTab is one configured instance. Configuration is deferred: the
// vocabulary and cost tables are read lazily at Open so CREATE VIRTUAL TABLE
// may precede the creation of those tables (amatch1 creates t4 against a
// not-yet-existing vtemp).
type amatchVTab struct {
	module    *ApproximateMatchModule
	vocabTab  string
	vocabWord string
	costsTab  string
	target    string // MATCH target; empty = unconstrained (no rows)
	hasTarget bool
}

// Create implements Module.
func (m *ApproximateMatchModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args)
}

// Connect implements Module.
func (m *ApproximateMatchModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args)
}

func (m *ApproximateMatchModule) connect(args []string) (VirtualTable, error) {
	v := &amatchVTab{module: m}
	for _, a := range args {
		eq := strings.Index(a, "=")
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(a[:eq]))
		val := strings.TrimSpace(a[eq+1:])
		val = strings.Trim(val, "'\"")
		switch key {
		case "vocabulary_table":
			v.vocabTab = val
		case "vocabulary_word":
			v.vocabWord = val
		case "edit_distances":
			v.costsTab = val
		}
	}
	return v, nil
}

// Columns implements ColumnInfo (amatch.c declared schema).
func (v *amatchVTab) Columns() []string {
	return []string{"word", "distance", "language", "command", "nword"}
}

// HiddenColumns reports command/nword as HIDDEN.
func (v *amatchVTab) HiddenColumns() map[int]bool {
	return map[int]bool{amatchColCommand: true, amatchColNWord: true}
}

// BestIndex accepts the default plan; MATCH/distance constraints are applied
// when the rows are generated.
func (v *amatchVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// SetMatchConstraint records the `word MATCH <target>` argument. The column
// must be "word" (amatch.c AMATCH_COL_WORD); other columns don't drive rows.
func (v *amatchVTab) SetMatchConstraint(column, target string) {
	if strings.EqualFold(column, "word") {
		v.target, v.hasTarget = target, true
	}
}

// Open loads the vocabulary and cost rules, computes each word's distance
// from the bound MATCH target, and yields the rows. Without a MATCH
// constraint the scan yields no rows (amatch.c requires one).
func (v *amatchVTab) Open() (Cursor, error) {
	if os.Getenv("AM_DBG") != "" {
		fmt.Fprintf(os.Stderr, "AMDBG open target=%q has=%v vocab=%q\n%s\n", v.target, v.hasTarget, v.vocabTab, debug.Stack())
	}
	c := &amatchCursor{}
	if !v.hasTarget || v.module == nil || v.module.src == nil {
		return c, nil
	}
	rules, err := v.module.src.CostRules(v.costsTab)
	if err != nil {
		return nil, err
	}
	if os.Getenv("AM_DBG") != "" {
		fmt.Fprintf(os.Stderr, "AMDBG rules=%d target=%q vocab=%q/%q\n", len(rules), v.target, v.vocabTab, v.vocabWord)
	}
	dp := newAmatchDP(rules, v.target)
	words, err := v.module.src.VocabWords(v.vocabTab, v.vocabWord)
	if err != nil {
		return nil, err
	}
	c.rows = make([]amatchRow, 0, len(words))
	for _, w := range words {
		d := dp.distance(w)
		if os.Getenv("AM_DBG") != "" && d <= 300 {
			fmt.Fprintf(os.Stderr, "AMDBG %q d=%d\n", w, d)
		}
		c.rows = append(c.rows, amatchRow{word: w, dist: d})
	}
	return c, nil
}

// amatchRow is one materialized approximate_match row.
type amatchRow struct {
	word string
	dist int64
}

// amatchCursor walks computed rows.
type amatchCursor struct {
	rows    []amatchRow
	idx     int
	started bool
	done    bool
}

// Next advances; the first call serves row 0.
func (c *amatchCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return len(c.rows) > 0
	}
	c.idx++
	if c.idx >= len(c.rows) {
		c.done = true
		return false
	}
	return true
}

// Column serves word/distance/language plus NULL command/nword.
func (c *amatchCursor) Column(idx int) (interface{}, error) {
	if c.idx >= len(c.rows) {
		return nil, fmt.Errorf("approximate_match: invalid column index %d", idx)
	}
	row := c.rows[c.idx]
	switch idx {
	case amatchColWord:
		return row.word, nil
	case amatchColDistance:
		return row.dist, nil
	case amatchColLanguage:
		return int64(0), nil
	case amatchColCommand, amatchColNWord:
		return nil, nil
	}
	return nil, fmt.Errorf("approximate_match: invalid column index %d", idx)
}

// Close implements Cursor.
func (c *amatchCursor) Close() error { return nil }
