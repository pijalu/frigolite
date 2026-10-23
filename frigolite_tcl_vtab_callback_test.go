package frigolite

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

type harnessTclCallbackRuntime struct {
	interp *tcl.Interp
	db     *DB
	procs  map[string]callbackProc
}

func newHarnessTclCallbackRuntime(step TestStep, db *DB) *harnessTclCallbackRuntime {
	runtime := &harnessTclCallbackRuntime{interp: tcl.NewInterp(), db: db, procs: make(map[string]callbackProc)}
	procs := make([]tcl.Proc, 0, len(step.CallbackProcs))
	for _, proc := range step.CallbackProcs {
		runtime.procs[proc.Name] = proc
		procs = append(procs, tcl.Proc{Name: proc.Name, Args: proc.Args, Body: proc.Body})
	}
	runtime.interp.LoadProcedures(procs)
	runtime.interp.RegisterCallbackCommand("db", func(args []string) (string, error) {
		if len(args) != 2 || args[0] != "one" {
			return "", fmt.Errorf("unsupported Tcl db callback command: %v", args)
		}
		query := runtime.interp.BindSQLParams(args[1])
		result := db.Query(query)
		if result.Error != nil {
			return "", result.Error
		}
		if len(result.Rows) == 0 || len(result.Rows[0]) == 0 || result.Rows[0][0] == nil {
			return "", nil
		}
		return fmt.Sprint(result.Rows[0][0]), nil
	})
	for name, value := range step.CallbackVars {
		runtime.interp.SetVariable(name, value)
	}
	return runtime
}

func (r *harnessTclCallbackRuntime) Connect(proc string, moduleArgs []string) (string, error) {
	return r.invoke(proc, moduleArgs, "xConnect")
}

func (r *harnessTclCallbackRuntime) BestIndex(proc string, moduleArgs []string, info *vtab.IndexInfo) (string, error) {
	const handle = "__frigolite_bestindex_handle"
	r.interp.RegisterCallbackCommand(handle, func(args []string) (string, error) {
		if len(args) == 0 {
			return "", fmt.Errorf("missing bestindex handle subcommand")
		}
		switch args[0] {
		case "constraints":
			return tclList(r.constraintEntries(info)), nil
		case "orderby":
			return tclList(r.orderEntries(info)), nil
		case "mask":
			return strconv.FormatUint(info.ColsUsed, 10), nil
		case "distinct":
			return strconv.Itoa(info.Distinct), nil
		default:
			return "", fmt.Errorf("unknown bestindex subcommand %q", args[0])
		}
	})
	return r.invoke(proc, moduleArgs, "xBestIndex", handle)
}

func (r *harnessTclCallbackRuntime) Filter(proc string, moduleArgs []string, idxNum int, idxStr string, argv []interface{}) (string, error) {
	values := make([]string, len(argv))
	for i, value := range argv {
		values[i] = fmt.Sprint(value)
	}
	return r.invoke(proc, moduleArgs, "xFilter", strconv.Itoa(idxNum), idxStr, tclList(values))
}

func (r *harnessTclCallbackRuntime) HasProc(proc string) bool { _, ok := r.procs[proc]; return ok }

func (r *harnessTclCallbackRuntime) invoke(proc string, moduleArgs []string, args ...string) (string, error) {
	if _, ok := r.procs[proc]; !ok {
		return "", fmt.Errorf("invalid command name %q", proc)
	}
	callArgs := append([]string(nil), moduleArgs...)
	callArgs = append(callArgs, args...)
	return r.interp.InvokeProcedure(proc, callArgs...)
}

func (r *harnessTclCallbackRuntime) Query(sql string) ([][]interface{}, []int64, error) {
	result := r.db.Query(sql)
	if result.Error != nil {
		return nil, nil, result.Error
	}
	rowids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		if len(row) > 0 {
			if id, ok := row[0].(int64); ok {
				rowids[i] = id
			}
		}
	}
	return result.Rows, rowids, nil
}

func (r *harnessTclCallbackRuntime) constraintEntries(info *vtab.IndexInfo) []string {
	entries := make([]string, len(info.Constraints))
	for i, constraint := range info.Constraints {
		usable := "0"
		if constraint.Usable {
			usable = "1"
		}
		entries[i] = tclList([]string{"op", constraintOpName(constraint.Op), "column", strconv.Itoa(constraint.Column), "usable", usable})
	}
	return entries
}

func (r *harnessTclCallbackRuntime) orderEntries(info *vtab.IndexInfo) []string {
	entries := make([]string, len(info.OrderBy))
	for i, order := range info.OrderBy {
		desc := "0"
		if order.Desc {
			desc = "1"
		}
		entries[i] = tclList([]string{"column", strconv.Itoa(order.Column), "desc", desc})
	}
	return entries
}

func constraintOpName(op vtab.IndexConstraintOp) string {
	switch op {
	case vtab.IndexConstraintEq:
		return "eq"
	case vtab.IndexConstraintGt:
		return "gt"
	case vtab.IndexConstraintLe:
		return "le"
	case vtab.IndexConstraintLt:
		return "lt"
	case vtab.IndexConstraintGe:
		return "ge"
	case vtab.IndexConstraintMatch:
		return "match"
	case vtab.IndexConstraintLike:
		return "like"
	case vtab.IndexConstraintGlob:
		return "glob"
	case vtab.IndexConstraintRegexp:
		return "regexp"
	case vtab.IndexConstraintNe:
		return "ne"
	case vtab.IndexConstraintIsNot:
		return "isnot"
	case vtab.IndexConstraintIsNotNull:
		return "isnotnull"
	case vtab.IndexConstraintIsNull:
		return "isnull"
	case vtab.IndexConstraintIs:
		return "is"
	case vtab.IndexConstraintLimit:
		return "limit"
	case vtab.IndexConstraintOffset:
		return "offset"
	default:
		return strconv.Itoa(int(op))
	}
}

func tclList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		if value == "" {
			quoted[i] = "{}"
			continue
		}
		if strings.IndexFunc(value, func(r rune) bool { return strings.ContainsRune(" \t\n{}\\\"", r) }) < 0 {
			quoted[i] = value
			continue
		}
		quoted[i] = "{" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "}", "\\}") + "}"
	}
	return strings.Join(quoted, " ")
}

func TestTclCallbackVirtualTableJoinRows(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	step := TestStep{CallbackProcs: []callbackProc{{Name: "callback", Args: []string{"method", "args"}, Body: `switch -- $method {xConnect {return "CREATE TABLE t1(PrimaryKey,flagA,columnA)"} xBestIndex {return [list idxstr {SELECT rowid,* FROM t1}]} xFilter {return [list sql [lindex $args 1]]}}; return {}`}}}
	runtime := newHarnessTclCallbackRuntime(step, db)
	db.RegisterVtabModule("tcl", vtab.NewTclCommandModuleWithRuntime(runtime))
	if result := db.Exec("CREATE TABLE t1(i INTEGER PRIMARY KEY, flagA, columnA); INSERT INTO t1 VALUES(1,0,'A'); INSERT INTO t1 VALUES(2,0,'B'); CREATE VIRTUAL TABLE va USING tcl(callback); CREATE VIRTUAL TABLE vb USING tcl(callback);"); result.Error != nil {
		t.Fatal(result.Error)
	}
	for _, name := range []string{"va", "vb"} {
		rows := db.Query("SELECT * FROM " + name)
		if rows.Error != nil || len(rows.Rows) != 2 {
			t.Fatalf("standalone %s rows=%#v err=%v", name, rows.Rows, rows.Error)
		}
	}
	cartesian := db.Query("SELECT * FROM va a CROSS JOIN vb b")
	if cartesian.Error != nil || len(cartesian.Rows) != 4 {
		t.Fatalf("cartesian vtab rows = %#v, err=%v", cartesian.Rows, cartesian.Error)
	}
	noWhere := db.Query("SELECT * FROM va a CROSS JOIN vb b ON b.PrimaryKey+0=a.PrimaryKey")
	if noWhere.Error != nil || len(noWhere.Rows) != 2 {
		t.Fatalf("unfiltered vtab join rows = %#v, err=%v, va=%#v, vb=%#v, plan=%#v", noWhere.Rows, noWhere.Error, db.Query("SELECT * FROM va").Rows, db.Query("SELECT * FROM vb").Rows, db.Query("EXPLAIN QUERY PLAN SELECT * FROM va a CROSS JOIN vb b ON b.PrimaryKey=a.PrimaryKey").Rows)
	}
	plain := db.Query("SELECT * FROM t1 a CROSS JOIN t1 b ON b.i=a.i WHERE a.columnA IN ('A','B') AND a.flagA=0")
	if plain.Error != nil || len(plain.Rows) != 2 {
		t.Fatalf("plain join rows = %#v, err=%v", plain.Rows, plain.Error)
	}
	result := db.Query("SELECT * FROM va a CROSS JOIN vb b ON b.PrimaryKey=a.PrimaryKey WHERE a.ColumnA IN ('A','B') AND a.flagA=0")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("join rows = %#v, want 2", result.Rows)
	}
}

func TestHarnessTclCallbackRuntimeDBOne(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE t1(id int, value text)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	step := TestStep{CallbackProcs: []callbackProc{{Name: "vtab_command", Args: []string{"src", "method", "args"}, Body: `switch -- $method {xConnect {return [db one {SELECT sql FROM sqlite_master WHERE name=$src}]}}`}}}
	runtime := newHarnessTclCallbackRuntime(step, db)
	got, err := runtime.Connect("vtab_command", []string{"t1"})
	if err != nil || got != "CREATE TABLE t1(id int, value text)" {
		t.Fatalf("xConnect schema = %q, %v", got, err)
	}
}

func TestHarnessTclCallbackRuntimeXConnect(t *testing.T) {
	step := TestStep{CallbackProcs: []callbackProc{{Name: "callback", Args: []string{"method", "args"}, Body: `switch -- $method {xConnect {return "CREATE TABLE t1(a,b,c)"}}; return {}`}}}
	runtime := newHarnessTclCallbackRuntime(step, nil)
	got, err := runtime.Connect("callback", nil)
	if err != nil || got != "CREATE TABLE t1(a,b,c)" {
		t.Fatalf("xConnect = %q, %v", got, err)
	}
}

func TestTclCallbackVirtualTableOrBranchOrder(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtime := &orBranchCallbackRuntime{db: db}
	db.RegisterVtabModule("tcl", vtab.NewTclCommandModuleWithRuntime(runtime))
	result := db.Exec("CREATE TABLE ttt(a,b,c); INSERT INTO ttt VALUES(1,'two','three'); INSERT INTO ttt VALUES(2,'one','two'); INSERT INTO ttt VALUES(3,'three','one'); INSERT INTO ttt VALUES(4,'y','one'); INSERT INTO ttt VALUES(5,'x','two'); INSERT INTO ttt VALUES(6,'y','three'); CREATE VIRTUAL TABLE t1 USING tcl(callback);")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	rows := db.Query("SELECT rowid FROM t1 WHERE c='three' OR c LIKE 'o%'")
	if rows.Error != nil {
		t.Fatal(rows.Error)
	}
	if got, want := fmt.Sprint(rows.Rows), "[[1] [6] [3] [4]]"; got != want {
		t.Fatalf("OR branch rows = %s, want %s; bestindex=%d filter=%d", got, want, runtime.bestIndexes, runtime.filters)
	}
	if got := runtime.bestIndexes; got != 2 {
		t.Fatalf("xBestIndex calls = %d, want one per OR branch", got)
	}
	if got := runtime.filters; got != 2 {
		t.Fatalf("xFilter calls = %d, want one per OR branch", got)
	}
}

type orBranchCallbackRuntime struct {
	db          *DB
	bestIndexes int
	filters     int
	operator    vtab.IndexConstraintOp
}

func (r *orBranchCallbackRuntime) HasProc(proc string) bool { return proc == "callback" }
func (r *orBranchCallbackRuntime) Connect(string, []string) (string, error) {
	return "CREATE TABLE t1(a,b,c)", nil
}
func (r *orBranchCallbackRuntime) BestIndex(_ string, _ []string, info *vtab.IndexInfo) (string, error) {
	r.bestIndexes++
	for i, constraint := range info.Constraints {
		if constraint.Usable && constraint.Column == 2 && (constraint.Op == vtab.IndexConstraintEq || constraint.Op == vtab.IndexConstraintLike) {
			info.Usage[i] = vtab.ConstraintUsage{ArgvIndex: 1, Omit: true}
			r.operator = constraint.Op
			return "idxnum 1 idxstr branch cost 100 rows 10", nil
		}
	}
	return "cost 1000000 rows 1000000", nil
}
func (r *orBranchCallbackRuntime) Filter(_ string, _ []string, _ int, _ string, argv []interface{}) (string, error) {
	r.filters++
	if len(argv) == 0 {
		return "sql {SELECT rowid,* FROM ttt}", nil
	}
	op := "="
	if r.operator == vtab.IndexConstraintLike {
		op = "LIKE"
	}
	return fmt.Sprintf("sql {SELECT rowid,* FROM ttt WHERE c %s '%v'}", op, argv[0]), nil
}
func (r *orBranchCallbackRuntime) Query(query string) ([][]interface{}, []int64, error) {
	result := r.db.Query(query)
	if result.Error != nil {
		return nil, nil, result.Error
	}
	ids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		if len(row) > 0 {
			ids[i], _ = row[0].(int64)
		}
	}
	return result.Rows, ids, nil
}
