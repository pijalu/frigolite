package execexpr

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file implements the fts5 test-support auxiliary function family —
// fts5_aux.c's SQLITE_TEST-only fts5_test_* functions (the
// fts5_aux_test_functions of ext/fts5/test/fts5_common.tcl) and the ad-hoc
// create_function registrations of fts5aux.test (inst/colsize/totalsize/
// prevrowid/phrasequery/my_rowid/my_phrasesize/firstcol/fts5_hitcount).
// Each entry is one small evaluator method; evalFTS5TestFunc dispatches
// through fts5TestFuncDispatch.

// fts5TestArgs binds one test-support aux call: the lowercased function name,
// the raw argument expressions, and the current row, with the evaluated-
// argument helpers the C implementations share.
type fts5TestArgs struct {
	ev   *Evaluator
	name string // lowercased function name
	args []sql.Expr
	row  Row
}

// argInt evaluates argument i as an int (0 past the end, mirroring the C
// test functions' default-0 arguments).
func (a *fts5TestArgs) argInt(i int) (int, error) {
	if i >= len(a.args) {
		return 0, nil
	}
	v, err := a.ev.evalExpr(a.args[i], a.row)
	if err != nil {
		return 0, err
	}
	return int(ToIntValue(util.UnwrapColumnValue(v))), nil
}

// argText evaluates argument i as text ("" past the end).
func (a *fts5TestArgs) argText(i int) (string, error) {
	if i >= len(a.args) {
		return "", nil
	}
	v, err := a.ev.evalExpr(a.args[i], a.row)
	if err != nil {
		return "", err
	}
	return valueTextOf(util.UnwrapColumnValue(v)), nil
}

// fts5TestFuncImpl is one test-support function implementation: the scanned
// table, the statement's aux query, the bound arguments, and the row's docid.
type fts5TestFuncImpl func(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error)

// fts5TestFuncDispatch maps each test-support function to its implementation.
// Populated in init(): a literal map here would form an initialization cycle
// (the implementations reach back to evalFTS5TestFunc through the evaluator's
// dispatch chain).
var fts5TestFuncDispatch map[string]fts5TestFuncImpl

func init() {
	fts5TestFuncDispatch = map[string]fts5TestFuncImpl{
		"inst":                      (*Evaluator).fts5TestInst,
		"colsize":                   (*Evaluator).fts5TestColsize,
		"matchinfo":                 (*Evaluator).fts5TestMatchinfo,
		"totalsize":                 (*Evaluator).fts5TestTotalsize,
		"fts5_test_columnsize":      (*Evaluator).fts5TestColumnSizeList,
		"fts5_test_columntotalsize": (*Evaluator).fts5TestColumnTotalSizeList,
		"fts5_columntext":           (*Evaluator).fts5TestColumntext,
		"fts5_test_columntext":      (*Evaluator).fts5TestColumnTextList,
		"fts5_columnlocale":         (*Evaluator).fts5TestColumnLocale,
		"fts5_test_columnlocale":    (*Evaluator).fts5TestColumnLocale,
		"fts5_test_insttoken":       fts5TestNullResult,
		"fts5_test_poslist":         (*Evaluator).fts5TestPoslist,
		"fts5_test_poslist2":        (*Evaluator).fts5TestPoslist2,
		"fts5_test_collist":         (*Evaluator).fts5TestCollist,
		"fts5_collist":              (*Evaluator).fts5TestOneCollist,
		"fts5_test_tokenize":        (*Evaluator).fts5TestTokenizeList,
		"tokenize":                  (*Evaluator).fts5TestTokenize,
		"fts5_test_rowcount":        fts5TestRowCount,
		"fts5_test_rowid":           fts5TestRowid,
		"my_rowid":                  fts5TestRowid,
		"fts5_test_phrasecount":     fts5TestPhraseCount,
		"my_phrasesize":             (*Evaluator).fts5TestPhraseSize,
		"fts5_test_queryphrase":     (*Evaluator).fts5TestQueryPhraseList,
		"fts5_queryphrase":          (*Evaluator).fts5TestQueryPhraseHits,
		"fts5_hitcount":             (*Evaluator).fts5TestHitcount,
		"phrasequery":               (*Evaluator).fts5TestPhraseQuery,
		"prevrowid":                 (*Evaluator).fts5TestPrevrowid,
		"prevrowid1":                (*Evaluator).fts5TestPrevrowid,
		"firstcol":                  fts5TestFirstcol,
		"fts5_test_all":             fts5TestAll,
	}
}

// evalFTS5TestFunc evaluates one test-support auxiliary function (the
// fts5_aux_test_functions family and the fts5aux.test registrations).
func (ev *Evaluator) evalFTS5TestFunc(lower string, t5 *fts5.Table, aq *fts5.AuxQuery, args []sql.Expr, row Row) (interface{}, error) {
	impl, ok := fts5TestFuncDispatch[lower]
	if !ok {
		return nil, fmt.Errorf("unable to use function %s in the requested context", lower)
	}
	return impl(ev, t5, aq, &fts5TestArgs{ev: ev, name: lower, args: args, row: row}, ev.auxRowid(row))
}

// fts5TestInst is inst(): xInst(i) → "ip ic io".
func (ev *Evaluator) fts5TestInst(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	inst, err := aq.Inst(rowid, i)
	if err != nil {
		return nil, err
	}
	return fmt.Sprintf("%d %d %d", inst.Phrase, inst.Col, inst.Offset), nil
}

// fts5TestColsize is colsize(): xColumnSize(i).
func (ev *Evaluator) fts5TestColsize(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	return aq.ColumnSize(rowid, i)
}

// fts5TestMatchinfo is matchinfo(t, zArg) (fts5_test_mi.c).
func (ev *Evaluator) fts5TestMatchinfo(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	flag := ""
	if len(a.args) > 0 {
		v, err := a.ev.evalExpr(a.args[0], a.row)
		if err != nil {
			return nil, err
		}
		flag = valueTextOf(util.UnwrapColumnValue(v))
	}
	return aq.Matchinfo(rowid, flag)
}

// fts5TestTotalsize is totalsize(): xColumnTotalSize(i).
func (ev *Evaluator) fts5TestTotalsize(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	return aq.ColumnTotalSize(i)
}

// fts5TestColumnSizeList is fts5_test_columnsize: the per-column xColumnSize
// list.
func (ev *Evaluator) fts5TestColumnSizeList(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	out := make([]string, len(t5.ColumnNames()))
	for i := range out {
		n, err := aq.ColumnSize(rowid, i)
		if err != nil {
			return nil, err
		}
		out[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(out, " "), nil
}

// fts5TestColumnTotalSizeList is fts5_test_columntotalsize: the per-column
// xColumnTotalSize list.
func (ev *Evaluator) fts5TestColumnTotalSizeList(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	out := make([]string, len(t5.ColumnNames()))
	for i := range out {
		n, err := aq.ColumnTotalSize(i)
		if err != nil {
			return nil, err
		}
		out[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(out, " "), nil
}

// fts5TestColumntext is fts5_columntext: xColumnText(i).
func (ev *Evaluator) fts5TestColumntext(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	return aq.ColumnText(rowid, i)
}

// fts5TestColumnTextList is fts5_test_columntext: the per-column xColumnText
// TCL list.
func (ev *Evaluator) fts5TestColumnTextList(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	out := make([]string, len(t5.ColumnNames()))
	for i := range out {
		text, err := aq.ColumnText(rowid, i)
		if err != nil {
			return nil, err
		}
		out[i] = text
	}
	return renderTclList(out), nil
}

// fts5TestColumnLocale is xColumnLocale: no locale= support — a NULL result
// after the column argument's evaluation.
func (ev *Evaluator) fts5TestColumnLocale(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	if _, err := a.argInt(0); err != nil {
		return nil, err
	}
	return nil, nil
}

// fts5TestNullResult always returns NULL (xInstToken: no tokendata support).
func fts5TestNullResult(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	return nil, nil
}

// fts5TestPoslist is fts5_test_poslist: "ip.ic.io" per instance.
func (ev *Evaluator) fts5TestPoslist(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	insts := aq.RowInstances(rowid)
	out := make([]string, len(insts))
	for i, in := range insts {
		out[i] = fmt.Sprintf("%d.%d.%d", in.Phrase, in.Col, in.Offset)
	}
	return strings.Join(out, " "), nil
}

// fts5TestPoslist2 is fts5_test_poslist2: sorted "i.c.o" per instance.
func (ev *Evaluator) fts5TestPoslist2(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	insts := aq.RowInstances(rowid)
	sort.Slice(insts, func(x, y int) bool {
		p, q := insts[x], insts[y]
		if p.Phrase != q.Phrase {
			return p.Phrase < q.Phrase
		}
		if p.Col != q.Col {
			return p.Col < q.Col
		}
		return p.Offset < q.Offset
	})
	out := make([]string, len(insts))
	for i, in := range insts {
		out[i] = fmt.Sprintf("%d.%d.%d", in.Phrase, in.Col, in.Offset)
	}
	return strings.Join(out, " "), nil
}

// fts5TestCollist is fts5_test_collist: "i.c" per phrase column.
func (ev *Evaluator) fts5TestCollist(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	var out []string
	for i := range aq.PhraseCount() {
		cols, err := aq.PhraseCollist(rowid, i)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			out = append(out, fmt.Sprintf("%d.%d", i, c))
		}
	}
	return strings.Join(out, " "), nil
}

// fts5TestOneCollist is fts5_collist: distinct columns of one phrase.
func (ev *Evaluator) fts5TestOneCollist(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	cols, err := aq.PhraseCollist(rowid, i)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for j, c := range cols {
		out[j] = strconv.Itoa(c)
	}
	return strings.Join(out, " "), nil
}

// fts5TestTokenizeList is fts5_test_tokenize: the per-column token-term TCL
// list.
func (ev *Evaluator) fts5TestTokenizeList(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	out := make([]string, len(t5.ColumnNames()))
	for i := range out {
		text, err := aq.ColumnText(rowid, i)
		if err != nil {
			return nil, err
		}
		out[i] = renderTclList(aq.TokenizeText(text))
	}
	return strings.Join(out, " "), nil
}

// fts5TestTokenize is the fts5tokenizer.test 6.x aux proc: tokens of column 0.
func (ev *Evaluator) fts5TestTokenize(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	text, err := aq.ColumnText(rowid, 0)
	if err != nil {
		return nil, err
	}
	toks := aq.TokenizeText(text)
	// The TCL callback (test_token_cb) returns SQLITE_DONE once three
	// tokens are collected, so xTokenize stops there.
	if len(toks) > 3 {
		toks = toks[:3]
	}
	return renderTclList(toks), nil
}

// fts5TestRowCount is fts5_test_rowcount.
func fts5TestRowCount(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	return aq.RowCount(), nil
}

// fts5TestRowid is fts5_test_rowid / my_rowid.
func fts5TestRowid(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	return rowid, nil
}

// fts5TestPhraseCount is fts5_test_phrasecount.
func fts5TestPhraseCount(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	return int64(aq.PhraseCount()), nil
}

// fts5TestPhraseSize is my_phrasesize: xPhraseSize(i); out of range returns 0.
func (ev *Evaluator) fts5TestPhraseSize(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	return int64(aq.PhraseSize(i)), nil
}

// fts5TestQueryPhraseList is fts5_test_queryphrase: per-phrase per-column hit
// lists.
func (ev *Evaluator) fts5TestQueryPhraseList(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	var out []string
	for i := range aq.PhraseCount() {
		hits, err := aq.QueryPhraseColumnHits(i)
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(hits))
		for j, h := range hits {
			parts[j] = strconv.FormatInt(h, 10)
		}
		out = append(out, renderTclList(parts))
	}
	return strings.Join(out, " "), nil
}

// fts5TestQueryPhraseHits is fts5_queryphrase: per-column hit counts of one
// phrase.
func (ev *Evaluator) fts5TestQueryPhraseHits(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	i, err := a.argInt(0)
	if err != nil {
		return nil, err
	}
	hits, err := aq.QueryPhraseColumnHits(i)
	if err != nil {
		return nil, err
	}
	parts := make([]string, len(hits))
	for j, h := range hits {
		parts[j] = strconv.FormatInt(h, 10)
	}
	return strings.Join(parts, " "), nil
}

// fts5TestHitcount is fts5_hitcount: total instances of phrase 0
// (xQueryPhrase sum).
func (ev *Evaluator) fts5TestHitcount(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	n, err := aq.PhraseHitCount(0)
	if err != nil {
		return nil, err
	}
	return int64(n), nil
}

// fts5TestPhraseQuery is phrasequery: xQueryPhrase(1) with the TCL stop-code
// protocol.
func (ev *Evaluator) fts5TestPhraseQuery(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	code, err := a.argText(0)
	if err != nil {
		return nil, err
	}
	rowids, err := aq.QueryPhraseRowids(1, code, rowid)
	if err != nil {
		return nil, err
	}
	parts := make([]string, len(rowids))
	for i, r := range rowids {
		parts[i] = strconv.FormatInt(r, 10)
	}
	return strings.Join(parts, " "), nil
}

// fts5TestPrevrowid is prevrowid/prevrowid1: the xGetAuxdataInt/xSetAuxdataInt
// round trip (prevrowid1 adds one).
func (ev *Evaluator) fts5TestPrevrowid(t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	prev := aq.AuxDataInt(a.name)
	aq.AuxDataSetInt(a.name, rowid)
	if a.name == "prevrowid1" {
		return prev + 1, nil
	}
	return prev, nil
}

// fts5TestFirstcol is firstcol: xColumnText(0).
func fts5TestFirstcol(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	return aq.ColumnText(rowid, 0)
}

// fts5TestAll is fts5_test_all: the flattened test summary.
func fts5TestAll(ev *Evaluator, t5 *fts5.Table, aq *fts5.AuxQuery, a *fts5TestArgs, rowid int64) (interface{}, error) {
	return aq.TestAll(rowid)
}
