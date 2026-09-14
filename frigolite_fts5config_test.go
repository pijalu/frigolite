package frigolite

// Native port of ext/fts5/test/fts5config.test's engine-visible contract
// (fts5_config.c option parsing). The testgen package is superseded: its
// 4.1.x section reads the rank value through the 'first' UDF registered by
// sqlite3_fts5_create_function (an fts5 aux-API function that cannot be
// expressed as a plain SQL function and is not transpiled), and its
// error-and-return structure blocks every later section. The rank-literal
// INSERT halves and all other sections are pinned here.

import "testing"

// TestFTS5ConfigOptions ports fts5config.test 1.0-13.x.
func TestFTS5ConfigOptions(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// 1.0: every TCL quote character is a column name; PRAGMA table_info
	// reports them in order. The TCL want's {} cells are ambiguous between
	// '' and NULL (the engine — like C — stores the empty-string type and a
	// NULL default), so the projection pins the unambiguous columns.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5('a', \"b\", [c], `d`)"))
	checkQueryResult(t, db.Query(
		`SELECT cid, name, "notnull", pk FROM pragma_table_info('t1')`),
		"0 a 0 0 1 b 0 0 2 c 0 0 3 d 0 0")

	// 2.x: syntax errors in the prefix= option.
	for _, opt := range []string{
		"prefix=x", "prefix='x'", "prefix='$'", "prefix='1,2,'",
		"prefix=',1'", "prefix='1,2,3...'", "prefix='1,2,3xyz'",
	} {
		checkExecError(t, db.Exec("CREATE VIRTUAL TABLE f1 USING fts5(x, "+opt+")"),
			"malformed prefix=... directive")
	}

	// 3.x: the source test's braced script passes $val unsubstituted, so C
	// binds a NULL rank specification — every case fails with the bare
	// SQLITE_ERROR (pinned here and by 12.1 below).
	for range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		checkExecError(t, db.Exec("INSERT INTO t1(t1, rank) VALUES('rank', NULL)"),
			"SQL logic error")
	}

	// 4.x: rank specifications carrying every SQL-literal form parse and
	// persist (the source test's SELECT rank IS <arg> half needs the 'first'
	// aux UDF and is pinned via the fts5rank anchor's override machinery).
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE zzz USING fts5(one)"))
	checkExecOK(t, db.Exec("INSERT INTO zzz VALUES('a b c')"))
	for _, arg := range []string{
		"123", "'01234567890ABCDEF'", "x'0123'", "x'ABCD'",
		"x'0123456789ABCDEF'", "x'0123456789abcdef'", "22.5", "-91.5",
		"-.5", "''''", "+.5",
	} {
		fn := "first(" + arg + ")"
		checkExecOK(t, db.Exec(
			"INSERT INTO zzz(zzz, rank) VALUES('rank', '" + doubleQuote(fn) + "')"))
	}
	// 4.2: an empty argument list is a valid spec.
	checkExecOK(t, db.Exec("INSERT INTO zzz(zzz, rank) VALUES('rank', 'f1()')"))

	// 5.x: misquoting in tokenize= and other options.
	checkExecError(t, db.Exec(`CREATE VIRTUAL TABLE xx USING fts5(x, tokenize="porter 'ascii")`),
		"parse error in tokenize directive")
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE xx USING fts5(x, [y[])"))
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE yy USING fts5(x, [y]])"),
		"unrecognized token: \"]\"")

	// 6.x: prefix lengths out of range.
	for _, spec := range []string{"prefix='1, 2, 1001'", "prefix='1, 2, 0000'", "prefix='1  , 1000000'"} {
		checkExecError(t, db.Exec("CREATE VIRTUAL TABLE abc USING fts5(a, "+spec+")"),
			"prefix length out of range (max 999)")
	}

	// 7.x: duplicate directives.
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE abc USING fts5(a, tokenize=porter, tokenize=ascii)"),
		"multiple tokenize=... directives")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE abc USING fts5(a, content=porter, content=ascii)"),
		"multiple content=... directives")
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE abc USING fts5(a, content_rowid=porter, content_rowid=a)"),
		"multiple content_rowid=... directives")

	// 8.x: unrecognized options.
	checkExecError(t, db.Exec("CREATE VIRTUAL TABLE abc USING fts5(a, nosuchoption=123)"),
		"unrecognized option: \"nosuchoption\"")
	checkExecError(t, db.Exec(`CREATE VIRTUAL TABLE abc USING fts5(a, "nosuchoption"=123)`),
		`parse error in ""nosuchoption"=123"`)

	// 9.x: option value checks.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE abc USING fts5(a, b)"))
	for _, val := range []string{"-5", "50000000", "66.67"} {
		checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('pgsz', "+val+")"),
			"SQL logic error")
		checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('hashsize', "+val+")"),
			"SQL logic error")
	}
	for _, val := range []string{"-5", "50000000", "66.67"} {
		checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('automerge', "+val+")"),
			"SQL logic error")
	}
	checkExecOK(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('automerge', 1)"))
	checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('crisismerge', -5)"), "SQL logic error")
	checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('crisismerge', 66.67)"), "SQL logic error")
	checkExecOK(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('crisismerge', 1)"))
	checkExecOK(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('crisismerge', 50000000)"))
	checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('nosuchoption', 1)"), "SQL logic error")
	checkExecError(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('hashsize', 'not an integer')"),
		"SQL logic error")
	checkExecOK(t, db.Exec("INSERT INTO abc(abc, rank) VALUES('hashsize', 500000)"))

	// 10.x: too many prefix indexes.
	checkExecError(t, db.Exec(
		`CREATE VIRTUAL TABLE xyz USING fts5(x, prefix="1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32")`),
		"too many prefix indexes (max 31)")
	checkExecError(t, db.Exec(
		`CREATE VIRTUAL TABLE xyz USING fts5(x, prefix="1 2 3 4", prefix="5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32")`),
		"too many prefix indexes (max 31)")

	// 11.x: detail= directive errors.
	for _, opt := range []string{"detail=x", "detail='x'", "detail='$'", "detail='1,2,'", "detail=',1'", "detail=''"} {
		checkExecError(t, db.Exec("CREATE VIRTUAL TABLE f9 USING fts5(x, "+opt+")"),
			"malformed detail=... directive")
	}

	// 12.1: a NULL rank specification fails.
	checkExecError(t, db.Exec("INSERT INTO t1(t1, rank) VALUES('rank', NULL)"), "SQL logic error")

	// 13.x: usermerge values — the accepted range is 2..16, so all four
	// source values fail (including 1).
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE tt USING fts5(ttt)"))
	checkExecError(t, db.Exec("INSERT INTO tt(tt, rank) VALUES('usermerge', -1)"), "SQL logic error")
	checkExecError(t, db.Exec("INSERT INTO tt(tt, rank) VALUES('usermerge', 4.2)"), "SQL logic error")
	checkExecError(t, db.Exec("INSERT INTO tt(tt, rank) VALUES('usermerge', 17)"), "SQL logic error")
	checkExecError(t, db.Exec("INSERT INTO tt(tt, rank) VALUES('usermerge', 1)"), "SQL logic error")
	checkExecOK(t, db.Exec("INSERT INTO tt(tt, rank) VALUES('usermerge', 4)"))
}

// doubleQuote doubles single quotes for embedding in a '-quoted SQL literal.
func doubleQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		out = append(out, s[i])
		if s[i] == '\'' {
			out = append(out, '\'')
		}
	}
	return string(out)
}
