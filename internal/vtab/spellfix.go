package vtab

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SpellfixModule implements the spellfix1 fuzzy-search virtual table
// (ext/misc/spellfix.c): a vocabulary stored in a "%_vocab" shadow table with
// transliteration (k1) and phonetic-hash (k2) keys; `word MATCH ?` queries
// return the best-scoring candidates by edit distance.
type SpellfixModule struct {
	db Database
	// per-table persistent state (upstream lives on the sqlite3_vtab object,
	// which survives across statements): the edit_cost_table name and its
	// loaded configuration. Keyed by "schema.table".
	mu     sync.Mutex
	tables map[string]*spellfixShared
}

// spellfixShared is the state one virtual table keeps across statements
// (spellfix1_vtab's zCostTable/pConfig3). seeded marks that the CREATE-TIME
// edit_cost_table argument was applied: upstream creates the vtab object
// once per connection, so a command= reset is never undone by the argument;
// the engine re-binds instances per statement, so the seed must be recorded.
type spellfixShared struct {
	costTable string
	config3   *spellfixEditDist3Config
	seeded    bool
}

// NewSpellfixModule builds the module over the connection's Database handle.
func NewSpellfixModule(db Database) *SpellfixModule {
	return &SpellfixModule{db: db, tables: map[string]*spellfixShared{}}
}

// Eponymous implements EponymousModule: spellfix1 registers xCreate==xConnect.
func (m *SpellfixModule) Eponymous() bool { return true }

// spellfix column indices (SPELLFIX_COL_*).
const (
	spColWord       = 0
	spColRank       = 1
	spColDistance   = 2
	spColLangID     = 3
	spColScore      = 4
	spColMatchlen   = 5
	spColPhonehash  = 6
	spColTop        = 7
	spColScope      = 8
	spColSrchcnt    = 9
	spColSoundslike = 10
	spColCommand    = 11
	spNColumn       = 12
)

// Create implements Module (xCreate: shadow tables are created on BindSchema).
func (m *SpellfixModule) Create(args []string) (VirtualTable, error) {
	v, err := m.init(args)
	if err != nil {
		return nil, err
	}
	v.created = true
	return v, nil
}

// Connect implements Module (xConnect: attach to existing shadow tables).
func (m *SpellfixModule) Connect(args []string) (VirtualTable, error) {
	return m.init(args)
}

// init ports spellfix1Init's argument handling: only edit_cost_table= is
// recognized; anything else is "bad argument to spellfix1()".
func (m *SpellfixModule) init(args []string) (*spellfixVTab, error) {
	v := &spellfixVTab{db: m.db, mod: m}
	for _, a := range args {
		if strings.HasPrefix(a, "edit_cost_table=") && v.pendingCostTable == "" {
			v.pendingCostTable = spellfixDequote(strings.TrimPrefix(a, "edit_cost_table="))
			continue
		}
		return nil, fmt.Errorf("bad argument to spellfix1(): \"%s\"", a)
	}
	return v, nil
}

// BindSchema implements SchemaBoundVTab: bind the resolved identity and
// per-table shared state, and materialize the shadow vocabulary when missing
// (spellfix1Init's CREATE TABLE IF NOT EXISTS "%w"."%w_vocab" + langid/k2
// index; idempotent on every bind, mirroring the rtree module).
func (v *spellfixVTab) BindSchema(dbName, tableName string) error {
	v.schema = dbName
	v.name = tableName
	v.shared = v.mod.sharedFor(dbName, tableName)
	// A CREATE ... , edit_cost_table=X argument seeds the persistent state
	// once per connection (upstream applies it in xCreate only; a later
	// command= reset or edit_cost_table switch must survive re-binds).
	if !v.shared.seeded {
		if v.pendingCostTable != "" {
			v.shared.costTable = v.pendingCostTable
		}
		v.shared.seeded = true
	}
	v.pendingCostTable = ""
	if _, err := v.db.ExecSQL(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s(id INTEGER PRIMARY KEY, rank INT, langid INT, word TEXT, k1 TEXT, k2 TEXT)",
		spellfixIdent(tableName+"_vocab"))); err != nil {
		return err
	}
	_, err := v.db.ExecSQL(fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON %s(langid,k2)",
		spellfixIdent(tableName+"_vocab_index_langid_k2"),
		spellfixIdent(tableName+"_vocab")))
	return err
}

// spellfixIdent renders a quoted identifier ("name", doubled quotes).
func spellfixIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// spellfixDequote ports spellfix1Dequote: strip surrounding single or double
// quotes.
func spellfixDequote(z string) string {
	if len(z) >= 2 && (z[0] == '\'' || z[0] == '"') && z[len(z)-1] == z[0] {
		return z[1 : len(z)-1]
	}
	return z
}

// spellfixQuote renders a SQL string literal (%Q parity: quotes doubled).
func spellfixQuote(z string) string {
	return "'" + strings.ReplaceAll(z, "'", "''") + "'"
}

type spellfixVTab struct {
	mod    *SpellfixModule
	db     Database
	name   string // virtual table name (shadow prefix)
	schema string // schema name ("main")
	shared *spellfixShared
	// pendingCostTable carries the CREATE-TIME edit_cost_table= argument
	// into BindSchema (the schema name is not resolved at init time).
	pendingCostTable string
	created          bool
	consume          spellfixConstraints
}

// sharedFor returns the persistent per-table state (module-owned).
func (m *SpellfixModule) sharedFor(dbName, tableName string) *spellfixShared {
	key := spellfixSharedKey(dbName, tableName)
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.tables[key]; ok {
		return s
	}
	s := &spellfixShared{}
	m.tables[key] = s
	return s
}

// spellfixSharedKey is the module map key for one virtual table's state.
func spellfixSharedKey(dbName, tableName string) string {
	return strings.ToLower(dbName + "." + tableName)
}

// DropTable implements TableDropper (spellfix1Uninit's isDestroy arm): the
// "%w_vocab" shadow table is destroyed with the virtual table and the
// per-table cost-table state is freed.
func (m *SpellfixModule) DropTable(dbName, tableName string) error {
	m.mu.Lock()
	delete(m.tables, spellfixSharedKey(dbName, tableName))
	m.mu.Unlock()
	_, err := m.db.ExecSQL(fmt.Sprintf("DROP TABLE IF EXISTS %s.%s",
		spellfixIdent(dbName), spellfixIdent(tableName+"_vocab")))
	return err
}

// loadCostConfig loads the configured edit-cost table on demand
// (spellfix1FilterForMatch's lazy p->pConfig3 load).
func (s *spellfixShared) loadCostConfig(db Database) error {
	if s.costTable == "" || s.config3 != nil {
		return nil
	}
	cfg, err := spellfixEditDist3ConfigLoad(db, s.costTable)
	if err != nil {
		return err
	}
	s.config3 = cfg
	return nil
}

// Columns implements ColumnInfo (declare_vtab schema).
func (v *spellfixVTab) Columns() []string {
	return []string{"word", "rank", "distance", "langid", "score", "matchlen",
		"phonehash", "top", "scope", "srchcnt", "soundslike", "command"}
}

// HiddenColumns implements HiddenColumnInfo: phonehash..command.
func (v *spellfixVTab) HiddenColumns() map[int]bool {
	return map[int]bool{6: true, 7: true, 8: true, 9: true, 10: true, 11: true}
}

// BestIndex accepts the default plan; constraints bind through
// SpellfixConstraintSink (xBestIndex idxNum parity) via the engine hook.
func (v *spellfixVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// spellfixConstraintPlan mirrors the xBestIndex idxNum bit layout.
const (
	spPlanMatch  = 1
	spPlanLangid = 2
	spPlanTop    = 4
	spPlanScope  = 8
	spPlanDistLT = 16
	spPlanDistLE = 32
	spPlanRowid  = 64
)

// spellfixConstraints accumulates the bound WHERE constraints for one scan.
type spellfixConstraints struct {
	plan     int
	match    string
	langid   int
	top      int
	scope    int
	maxDist  int
	rowid    int64
	hasRowid bool
}

// PushSpellfixConstraint implements SpellfixConstraintSink (xBestIndex:
// the first usable constraint per kind wins; every consumed term omits).
func (v *spellfixVTab) PushSpellfixConstraint(col int, op string, value interface{}) bool {
	c := v.consume
	ok := false
	switch op {
	case "MATCH":
		ok = spellfixPushMatch(&c, col, value)
	case "=", "==":
		ok = spellfixPushEqual(&c, col, value)
	case "<", "<=":
		ok = spellfixPushDist(&c, col, op, value)
	}
	if !ok {
		return false
	}
	v.consume = c
	return true
}

// spellfixPushMatch binds word MATCH ? (once).
func spellfixPushMatch(c *spellfixConstraints, col int, value interface{}) bool {
	if c.plan&spPlanMatch != 0 || col != spColWord {
		return false
	}
	s, ok := value.(string)
	if !ok {
		return false
	}
	c.plan |= spPlanMatch
	c.match = s
	return true
}

// spellfixPushEqual binds langid/top/scope/rowid equality terms. A rowid=
// term is only consumed by the ROWID plan when no MATCH term exists; with
// MATCH it stays in the WHERE clause for the core to re-check
// (spellfix1BestIndex; rowid=10 AND word MATCH).
func spellfixPushEqual(c *spellfixConstraints, col int, value interface{}) bool {
	switch {
	case col == spColLangID && c.plan&spPlanLangid == 0:
		c.plan |= spPlanLangid
		c.langid = spellfixInt(value)
	case col == spColTop && c.plan&spPlanTop == 0:
		c.plan |= spPlanTop
		c.top = spellfixInt(value)
	case col == spColScope && c.plan&spPlanScope == 0:
		c.plan |= spPlanScope
		c.scope = spellfixInt(value)
	case col == -1 && c.plan&spPlanRowid == 0:
		return spellfixPushRowid(c, value)
	default:
		return false
	}
	return true
}

// spellfixPushRowid binds the rowid= term of the ROWID-only plan.
func spellfixPushRowid(c *spellfixConstraints, value interface{}) bool {
	if c.plan&spPlanMatch != 0 {
		return false
	}
	n, ok := value.(int64)
	if !ok {
		return false
	}
	c.plan |= spPlanRowid
	c.rowid = n
	c.hasRowid = true
	return true
}

// spellfixPushDist binds "distance < / <= $dist" (once).
func spellfixPushDist(c *spellfixConstraints, col int, op string, value interface{}) bool {
	if col != spColDistance || c.plan&(spPlanDistLT|spPlanDistLE) != 0 {
		return false
	}
	if op == "<" {
		c.plan |= spPlanDistLT
	} else {
		c.plan |= spPlanDistLE
	}
	c.maxDist = spellfixInt(value)
	return true
}

// spellfixInt coerces a bound SQL value to int (sqlite3_value_int parity).
func spellfixInt(value interface{}) int {
	switch n := value.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	case []byte:
		return len(n)
	}
	return 0
}

// spellfixRow is one MATCH result row (struct spellfix1_row).
type spellfixRow struct {
	word      string
	iRowid    int64
	iRank     int
	iDistance int
	iScore    int
	iMatchlen int
	zHash     string
}

// spellfixCursor scans either the MATCH candidate set or the full vocab.
type spellfixCursor struct {
	v        *spellfixVTab
	idxNum   int
	zPattern string // full MATCH pattern (trailing '*' kept for matchlen)
	iLang    int
	iTop     int
	iScope   int
	nSearch  int
	nAlloc   int // row capacity (spellfix1ResizeCursor)
	rows     []spellfixRow
	iRow     int
	// full-scan mode (idxNum 0/64): materialized vocab rows of
	// (word, rank, NULL, langid, id).
	fullScan [][]interface{}
}

// Open resolves the bound constraint plan and positions the cursor
// (spellfix1Open + spellfix1Filter). iRow starts at -1: the engine's first
// Next() moves to row 0 (zipCursor convention).
func (v *spellfixVTab) Open() (Cursor, error) {
	c := &spellfixCursor{v: v, idxNum: v.consume.plan, iTop: 20, iScope: 3, iRow: -1}
	plan := v.consume
	v.consume = spellfixConstraints{} // plan consumed by this scan
	if plan.plan&spPlanMatch != 0 {
		if err := c.filterForMatch(plan); err != nil {
			return nil, err
		}
	} else {
		if err := c.filterForFullScan(plan); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// vocabName is the shadow vocabulary table name.
func (v *spellfixVTab) vocabName() string {
	return spellfixIdent(v.name + "_vocab")
}

// filterForFullScan ports spellfix1FilterForFullScan: stream
// (word, rank, NULL, langid, id) over the shadow vocabulary.
func (c *spellfixCursor) filterForFullScan(plan spellfixConstraints) error {
	sqlStr := "SELECT word, rank, NULL, langid, id FROM " + c.v.vocabName()
	if plan.plan&spPlanRowid != 0 {
		sqlStr += fmt.Sprintf(" WHERE rowid=%d", plan.rowid)
	}
	rows, err := c.v.db.ExecSQL(sqlStr)
	if err != nil {
		return err
	}
	c.fullScan = rows
	return nil
}

// filterForMatch ports spellfix1FilterForMatch: transliterate the MATCH
// argument, restrict candidates to the phonetic-hash range, score each by
// edit distance and keep the best iLimit rows.
func (c *spellfixCursor) filterForMatch(plan spellfixConstraints) error {
	iLimit, iScope, iMaxDist, iLang := spellfixPlanLimits(plan)
	c.iLang = iLang

	zMatchThis := plan.match
	if zMatchThis == "" && plan.match != "" {
		return nil // NULL-ish match value: empty result
	}
	zPattern := transliterate(zMatchThis)
	c.zPattern = zPattern
	zHash1, zHash2, iScope := spellfixHashWindow(zPattern, iScope)
	c.iScope = iScope

	// Candidate vocabulary rows for the langid + k2 range. The C statement
	// walks the (langid,k2) index, i.e. candidates arrive in k2 order; the
	// order decides WHICH rows survive when scores tie (strict < replace).
	sqlStr := fmt.Sprintf("SELECT id, word, rank, coalesce(k1,word) FROM %s WHERE langid=%d AND k2>=%s AND k2<%s ORDER BY k2",
		c.v.vocabName(), iLang, spellfixQuote(zHash1), spellfixQuote(zHash2))
	vocab, err := c.v.db.ExecSQL(sqlStr)
	if err != nil {
		return err
	}
	// Lazy cost-table load (spellfix1FilterForMatch); when a configuration
	// is active the distance comes from editDist3Core instead of editdist1.
	if err := c.v.shared.loadCostConfig(c.v.db); err != nil {
		return err
	}
	var matchStr *spellfixFromString
	if c.v.shared.config3 != nil {
		matchStr = spellfixFromStringNew(spellfixEditDist3FindLang(c.v.shared.config3, iLang), zMatchThis)
	}

	// spellfix1ResizeCursor(iLimit): the array starts at iLimit slots.
	c.nAlloc = iLimit
	c.rows = c.rows[:0]
	c.scoreVocab(vocab, plan, matchStr, zPattern, zHash1, iMaxDist)
	if len(c.rows) > 0 {
		spellfixRowCompare(c.rows)
	}
	c.iTop = iLimit
	return nil
}

// spellfixPlanLimits resolves the iLimit/iScope/iMaxDist/iLang defaults
// from the bound plan bits (spellfix1FilterForMatch's parameter block).
func spellfixPlanLimits(plan spellfixConstraints) (iLimit, iScope, iMaxDist, iLang int) {
	iLimit = 20
	if plan.plan&spPlanTop != 0 {
		iLimit = plan.top
		if iLimit < 1 {
			iLimit = 1
		}
	}
	iScope = 3
	if plan.plan&spPlanScope != 0 {
		iScope = plan.scope
		if iScope < 1 {
			iScope = 1
		}
		if iScope > spellfixMxHash-2 {
			iScope = spellfixMxHash - 2
		}
	}
	iMaxDist = -1
	if plan.plan&(spPlanDistLT|spPlanDistLE) != 0 {
		iMaxDist = plan.maxDist
		if plan.plan&spPlanDistLT != 0 {
			iMaxDist--
		}
		if iMaxDist < 0 {
			iMaxDist = 0
		}
	}
	iLang = 0
	if plan.plan&spPlanLangid != 0 {
		iLang = plan.langid
	}
	return iLimit, iScope, iMaxDist, iLang
}

// spellfixHashWindow computes the phonetic-hash range [zHash1, zHash2)
// over the (possibly star-truncated) pattern and clamps iScope.
func spellfixHashWindow(zPattern string, iScope int) (zHash1, zHash2 string, scopeOut int) {
	nPattern := len(zPattern)
	if nPattern > 0 && zPattern[nPattern-1] == '*' {
		nPattern--
	}
	zClass := phoneticHash(zPattern[:nPattern])
	if len(zClass) > spellfixMxHash-2 {
		zClass = zClass[:spellfixMxHash-2]
	}
	nClass := len(zClass)
	if nClass <= iScope {
		if nClass > 2 {
			iScope = nClass - 1
		} else {
			iScope = nClass
		}
	}
	return zClass[:iScope], zClass[:iScope] + "Z", iScope
}

// scoreVocab is the Wagner scoring pass (spellfix1RunQuery): keep the best
// rows by score, replacing the worst slot when full.
func (c *spellfixCursor) scoreVocab(vocab [][]interface{}, plan spellfixConstraints, matchStr *spellfixFromString, zPattern, zHash1 string, iMaxDist int) {
	iWorst, idxWorst := 0, -1
	for _, vr := range vocab {
		if len(vr) < 4 {
			continue
		}
		iMatchlen := -1
		word, _ := vr[1].(string)
		k1, _ := vr[3].(string)
		rowid, _ := vr[0].(int64)
		rank := spellfixValueInt(vr[2])
		iDist := spellfixMatchDistance(c.v.shared.config3, matchStr, word, zPattern, k1, c.iLang, &iMatchlen)
		if iDist < 0 {
			continue
		}
		c.nSearch++
		// "distance < / <= $dist" gate; without a "top=?" bound the array
		// keeps growing (nAlloc*2 + 10) to hold every in-range row.
		if c.distGate(iDist, iMaxDist, plan.plan&spPlanTop != 0) {
			continue
		}
		row := spellfixRow{
			word: word, iRowid: rowid, iRank: rank,
			iDistance: iDist, iScore: spellfix1Score(iDist, rank), iMatchlen: iMatchlen,
			zHash: zHash1,
		}
		if !c.absorbRow(row, &iWorst, &idxWorst) {
			continue
		}
	}
}

// distGate enforces the iMaxDist bound (skip when over) and grows the
// row array when no top= bound caps it. It reports whether to skip.
func (c *spellfixCursor) distGate(iDist, iMaxDist int, hasTop bool) bool {
	if iMaxDist < 0 {
		return false
	}
	if iDist > iMaxDist {
		return true
	}
	if len(c.rows) >= c.nAlloc && !hasTop {
		c.nAlloc = c.nAlloc*2 + 10
	}
	return false
}

// absorbRow stores the row (append or worst-slot replacement) and
// refreshes the worst-slot scan when the array is full. ok is false when
// the row scored too high to keep.
func (c *spellfixCursor) absorbRow(row spellfixRow, pWorst, pIdxWorst *int) (ok bool) {
	switch {
	case len(c.rows) < c.nAlloc:
		c.rows = append(c.rows, row)
	case *pIdxWorst >= 0 && row.iScore < *pWorst:
		c.rows[*pIdxWorst] = row
	default:
		return false
	}
	if len(c.rows) == c.nAlloc {
		*pWorst, *pIdxWorst = spellfixFindWorst(c.rows)
	}
	return true
}

// spellfixMatchDistance measures one candidate word: editDist3Core when a
// cost configuration is active, else the fixed-cost editdist1.
func spellfixMatchDistance(config3 *spellfixEditDist3Config, matchStr *spellfixFromString, word, zPattern, k1 string, iLang int, pnMatch *int) int {
	if matchStr != nil {
		return spellfixEditDist3Core(matchStr, word, spellfixEditDist3FindLang(config3, iLang), pnMatch)
	}
	return editdist1(zPattern, k1, nil)
}

// spellfixFindWorst locates the highest-score slot (C's worst-slot scan).
func spellfixFindWorst(rows []spellfixRow) (iWorst, idx int) {
	iWorst, idx = rows[0].iScore, 0
	for i := 1; i < len(rows); i++ {
		if iWorst < rows[i].iScore {
			iWorst = rows[i].iScore
			idx = i
		}
	}
	return iWorst, idx
}

// spellfixValueInt coerces a stored SQL value to int.
func spellfixValueInt(v interface{}) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// Next implements Cursor.
func (c *spellfixCursor) Next() bool {
	if c.fullScan != nil {
		c.iRow++
		return c.iRow < len(c.fullScan)
	}
	c.iRow++
	return c.iRow < len(c.rows)
}

// Column implements Cursor (spellfix1Column).
func (c *spellfixCursor) Column(idx int) (interface{}, error) {
	if idx < 0 || idx >= spNColumn {
		return nil, fmt.Errorf("spellfix: invalid column index %d", idx)
	}
	if c.fullScan != nil {
		return c.columnFromFullScan(idx), nil
	}
	if c.iRow >= len(c.rows) {
		return nil, nil
	}
	r := c.rows[c.iRow]
	if f, ok := spellfixIntColumns[idx]; ok {
		return f(c, &r), nil
	}
	switch idx {
	case spColWord:
		return r.word, nil
	case spColPhonehash:
		return r.zHash, nil
	default:
		return nil, nil
	}
}

// spellfixIntColumns maps the int-valued output columns to their accessors
// (spellfix1Column's numeric arms; soundslike/command are always NULL).
var spellfixIntColumns = map[int]func(*spellfixCursor, *spellfixRow) int64{
	spColRank: func(_ *spellfixCursor, r *spellfixRow) int64 { return int64(r.iRank) },
	spColDistance: func(_ *spellfixCursor, r *spellfixRow) int64 {
		return int64(r.iDistance)
	},
	spColLangID: func(c *spellfixCursor, _ *spellfixRow) int64 { return int64(c.iLang) },
	spColScore:  func(_ *spellfixCursor, r *spellfixRow) int64 { return int64(r.iScore) },
	spColMatchlen: func(c *spellfixCursor, r *spellfixRow) int64 {
		return int64(c.matchlen(r))
	},
	spColTop:   func(c *spellfixCursor, _ *spellfixRow) int64 { return int64(c.iTop) },
	spColScope: func(c *spellfixCursor, _ *spellfixRow) int64 { return int64(c.iScope) },
	spColSrchcnt: func(c *spellfixCursor, _ *spellfixRow) int64 {
		return int64(c.nSearch)
	},
}

// columnFromFullScan reads one full-scan row (word, rank, NULL, langid,
// id): distance, score, matchlen and the hidden scan columns are NULL.
func (c *spellfixCursor) columnFromFullScan(idx int) interface{} {
	if idx <= spColLangID {
		return c.fullScan[c.iRow][idx]
	}
	return nil
}

// matchlen ports the SPELLFIX_COL_MATCHLEN logic: for prefix searches the
// matched length maps back from the transliteration; otherwise it is the
// word's character length.
func (c *spellfixCursor) matchlen(r *spellfixRow) int {
	if r.iMatchlen >= 0 {
		return r.iMatchlen
	}
	nPattern := len(c.zPattern)
	if nPattern > 0 && c.zPattern[nPattern-1] == '*' {
		zTranslit := transliterate(r.word)
		ml := 0
		res := editdist1(c.zPattern, zTranslit, &ml)
		if res < 0 {
			return 0
		}
		return translen_to_charlen(r.word, len(r.word), ml)
	}
	return utf8Charlen(r.word, len(r.word))
}

// Rowid implements RowidCursor.
func (c *spellfixCursor) Rowid() int64 {
	if c.fullScan != nil {
		if c.iRow < len(c.fullScan) {
			if id, ok := c.fullScan[c.iRow][4].(int64); ok {
				return id
			}
		}
		return 0
	}
	if c.iRow < len(c.rows) {
		return c.rows[c.iRow].iRowid
	}
	return 0
}

// Close implements Cursor.
func (c *spellfixCursor) Close() error { return nil }

// SpellfixConstraintSink is implemented by spellfix1 instances: WHERE
// constraints on the plan columns (word MATCH, langid/top/scope =,
// distance </<=, rowid =) are consumed by the scan (xBestIndex omit=1).
// col is the declared column index (-1 for rowid). Returns false when the
// constraint does not fit the module's plan set.
type SpellfixConstraintSink interface {
	PushSpellfixConstraint(col int, op string, value interface{}) bool
}

// spellfixColumnNames maps declared column names to their indices.
var spellfixColumnNames = map[string]int{
	"word": 0, "rank": 1, "distance": 2, "langid": 3, "score": 4,
	"matchlen": 5, "phonehash": 6, "top": 7, "scope": 8, "srchcnt": 9,
	"soundslike": 10, "command": 11,
}

// SpellfixColumnIndex maps a spellfix declared column name to its index;
// ok is false for other names.
func SpellfixColumnIndex(name string) (col int, ok bool) {
	col, ok = spellfixColumnNames[strings.ToLower(name)]
	if !ok {
		return -1, false
	}
	return col, true
}

// spellfixRowCompare orders rows by increasing iScore (spellfix1RowCompare).
func spellfixRowCompare(rows []spellfixRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].iScore < rows[j].iScore })
}
