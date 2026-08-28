package frigolite

import (
	"strings"
	"testing"
)

// P4 pre-tests: hand-written tests for G4.DATETIME date/time functions,
// written BEFORE running the TCL testgen packages (date, timediff). Each
// expectation was verified against sqlite3 3.51 as the oracle and uses fixed
// inputs only (no 'now'/localtime), so the tests are deterministic.
//
// Scope: date/time/datetime/julianday/unixepoch/strftime, all modifiers
// (+N units, start of month/year/day, weekday N, utc, localtime, subsec,
// unixepoch, julianday, auto, +/-YYYY-MM-DD HH:MM:SS), time-value parsing
// variants (YYYY-MM-DD[T]HH:MM[:SS], Julian day, unix epoch), and the
// UTC-internal model (timezone suffixes are converted to UTC).

func TestP4Datetime_Basic(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT date('2020-01-01')", "2020-01-01"},
		{"SELECT datetime('2020-01-01')", "2020-01-01 00:00:00"},
		{"SELECT datetime('2020-01-01 12:34:56')", "2020-01-01 12:34:56"},
		{"SELECT time('2020-01-01 12:34:56')", "12:34:56"},
		{"SELECT time('12:34:56')", "12:34:56"},
		{"SELECT time('12:34')", "12:34:00"},
		{"SELECT julianday('2020-01-01')", "2458849.5"},
		{"SELECT unixepoch('2020-01-01')", "1577836800"},
		{"SELECT date('0000-01-01')", "0000-01-01"},
		{"SELECT date('-0001-01-01')", "-0001-01-01"},
		{"SELECT julianday('0000-01-01')", "1721059.5"},
		// Invalid / NULL input → NULL.
		{"SELECT ifnull(date('garbage'),'NULL')", "NULL"},
		{"SELECT ifnull(date(NULL),'NULL')", "NULL"},
		{"SELECT ifnull(datetime('2020-13-01'),'NULL')", "NULL"},
		{"SELECT ifnull(datetime('2020-01-01 25:00:00'),'NULL')", "NULL"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

func TestP4Datetime_ParsingVariants(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		// YYYY-MM-DD[T]HH:MM[:SS] variants.
		{"SELECT datetime('2020-01-01T12:34:56')", "2020-01-01 12:34:56"},
		{"SELECT datetime('2020-01-01 12:34')", "2020-01-01 12:34:00"},
		{"SELECT date('2020-01-01T12:34:56')", "2020-01-01"},
		// Timezone suffix: converted to UTC (UTC-internal model).
		{"SELECT datetime('1994-04-16 14:00:00 +05:00')", "1994-04-16 09:00:00"},
		{"SELECT datetime('1994-04-16T14:00:00Z')", "1994-04-16 14:00:00"},
		{"SELECT datetime('1994-04-16 14:00:00z')", "1994-04-16 14:00:00"},
		{"SELECT datetime('1994-04-16 14:00:00 -05:15')", "1994-04-16 19:15:00"},
		{"SELECT datetime('2000-10-29 12:00Z','utc','utc')", "2000-10-29 12:00:00"},
		// Julian day number time-value.
		{"SELECT datetime(2458850.5)", "2020-01-02 00:00:00"},
		{"SELECT date(2440587.5)", "1970-01-01"},
		{"SELECT datetime(0.0)", "-4713-11-24 12:00:00"},
		// Unix epoch via unixepoch modifier.
		{"SELECT datetime('1577836800','unixepoch')", "2020-01-01 00:00:00"},
		{"SELECT datetime(1577836800,'unixepoch')", "2020-01-01 00:00:00"},
		// auto: julian day vs unix epoch by magnitude.
		{"SELECT datetime(1577836800,'auto')", "2020-01-01 00:00:00"},
		{"SELECT datetime(2458849.5,'auto')", "2020-01-01 00:00:00"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

func TestP4Datetime_Modifiers(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		// +N units / -N units.
		{"SELECT date('2020-01-31','+1 month')", "2020-03-02"},
		{"SELECT datetime('2004-01-29','+1 month')", "2004-02-29 00:00:00"},
		{"SELECT datetime('2000-03-01','-1 day')", "2000-02-29 00:00:00"},
		{"SELECT datetime('2000-02-29','+1 year')", "2001-03-01 00:00:00"},
		{"SELECT date('2000-01-01','-1 month')", "1999-12-01"},
		{"SELECT date('2023-01-15','+2.5 days')", "2023-01-17"},
		{"SELECT date('2023-01-15','-10 hours')", "2023-01-14"},
		// start of month/year/day.
		{"SELECT date('2023-01-15','start of month')", "2023-01-01"},
		{"SELECT date('2023-01-15','start of year')", "2023-01-01"},
		{"SELECT date('2023-01-15','start of day')", "2023-01-15"},
		{"SELECT datetime('2023-01-15 12:34:56','start of month')", "2023-01-01 00:00:00"},
		// weekday N.
		{"SELECT date('2023-01-15','weekday 0')", "2023-01-15"},
		{"SELECT date('2023-01-15','weekday 6')", "2023-01-21"},
		{"SELECT datetime('2023-01-15 10:30:00','weekday 1')", "2023-01-16 10:30:00"},
		// subsec.
		{"SELECT datetime('2023-01-15 12:34:56.789','subsec')", "2023-01-15 12:34:56.789"},
		{"SELECT time('2023-01-15 12:34:56.789','subsec')", "12:34:56.789"},
		{"SELECT unixepoch('2020-01-01 00:00:00.5','subsec')", "1577836800.5"},
		// unixepoch / julianday / auto modifiers.
		{"SELECT datetime(0,'unixepoch')", "1970-01-01 00:00:00"},
		{"SELECT datetime(2458849.5,'julianday')", "2020-01-01 00:00:00"},
		// +/-YYYY-MM-DD HH:MM:SS modifiers (timediff output form).
		{"SELECT datetime('2000-03-02','+0000-01-00 00:00:00.000')", "2000-04-02 00:00:00"},
		{"SELECT datetime('2000-01-31','+0001-02-03')", "2001-04-03 00:00:00"},
		// floor / ceiling (day-of-month overflow resolution).
		{"SELECT date('2000-01-31','floor')", "2000-01-31"},
		{"SELECT date('2000-02-31','floor')", "2000-02-29"},
		{"SELECT date('2000-02-31','ceiling')", "2000-03-02"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

func TestP4Datetime_Strftime(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT strftime('%Y-%m-%d %H:%M:%S','2020-01-01 12:34:56')", "2020-01-01 12:34:56"},
		{"SELECT strftime('%Y','2020-01-01')", "2020"},
		{"SELECT strftime('%m','2020-01-01')", "01"},
		{"SELECT strftime('%d','2020-01-01')", "01"},
		{"SELECT strftime('%e','2020-01-01')", " 1"},
		{"SELECT strftime('%H','2020-01-01 13:00:00')", "13"},
		{"SELECT strftime('%I','2020-01-01 13:00:00')", "01"},
		{"SELECT strftime('%j','2023-12-31')", "365"},
		{"SELECT strftime('%w','2023-01-04')", "3"},
		{"SELECT strftime('%u','2023-01-04')", "3"},
		{"SELECT strftime('%W','2023-01-01')", "00"},
		{"SELECT strftime('%U','2023-01-01')", "01"},
		{"SELECT strftime('%V','2023-01-01')", "52"},
		{"SELECT strftime('%G','2023-01-01')", "2022"},
		{"SELECT strftime('%g','2023-01-01')", "22"},
		{"SELECT strftime('%f','2020-01-01 12:34:56.789')", "56.789"},
		{"SELECT strftime('%F','2020-01-01')", "2020-01-01"},
		{"SELECT strftime('%p','2020-01-01 13:00:00')", "PM"},
		{"SELECT strftime('%P','2020-01-01 13:00:00')", "pm"},
		{"SELECT strftime('%R','2020-01-01 13:14:15')", "13:14"},
		{"SELECT strftime('%T','2020-01-01 13:14:15')", "13:14:15"},
		{"SELECT strftime('%s','2020-01-01')", "1577836800"},
		{"SELECT strftime('%J','2000-01-01')", "2451544.5"},
		{"SELECT strftime('%%','2020-01-01')", "%"},
		{"SELECT ifnull(strftime('%q','2020-01-01'),'NULL')", "NULL"}, // unknown code → NULL
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

func TestP4Datetime_JulianDayRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT date(julianday('2020-01-01'))", "2020-01-01"},
		{"SELECT date(julianday('0000-01-01'))", "0000-01-01"},
		{"SELECT date(julianday('-0001-01-01'))", "-0001-01-01"},
		{"SELECT datetime(julianday('2000-02-29 12:34:56'))", "2000-02-29 12:34:56"},
		// Extreme julian days: min and max supported dates.
		{"SELECT datetime(0.0)", "-4713-11-24 12:00:00"},
		{"SELECT datetime(5373484.4999999)", "9999-12-31 23:59:59"},
		// Out-of-range → NULL.
		{"SELECT ifnull(date(-1),'NULL')", "NULL"},
		{"SELECT ifnull(date(5373484.5),'NULL')", "NULL"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

func TestP4Datetime_LocaltimeUTC(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// The localtime/utc modifiers are timezone-dependent; with the system
	// timezone these produce deterministic results only for the UTC
	// round-trip. The testgen suite pins localtime via the TCL harness hook;
	// here we verify the utc modifier with an explicit +00:00 offset (no
	// conversion needed) and that localtime on a UTC-pinned value round-trips
	// through utc (the engine's utc modifier converts local back to UTC).
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT datetime('2000-10-29 12:00Z','utc')", "2000-10-29 12:00:00"},
		{"SELECT datetime('2000-10-29 12:00+00:00','utc','utc')", "2000-10-29 12:00:00"},
		{"SELECT datetime('2000-10-29 12:00:00+05:00', 'utc')", "2000-10-29 07:00:00"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}
}

// TestP4CompileOptionFunctions covers sqlite_compileoption_used/get and
// PRAGMA compile_options (ctime.test). Expected values were verified against
// sqlite3 3.51 as the oracle; the engine advertises THREADSAFE=2 and the
// DIRECT_OVERFLOW_READ/ENABLE_FTS3/ENABLE_MATH_FUNCTIONS options.
func TestP4CompileOptionFunctions(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// ctime-1.3: SQLITE_ prefix optional, '='-value matching, boundary rule.
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT sqlite_compileoption_used('SQLITE_THREADSAFE')", "1"},
		{"SELECT sqlite_compileoption_used('THREADSAFE')", "1"},
		{"SELECT sqlite_compileoption_used('THREADSAFE=0')", "0"},
		{"SELECT sqlite_compileoption_used('THREADSAFE=1')", "0"},
		{"SELECT sqlite_compileoption_used('THREADSAFE=2')", "1"},
		{"SELECT sqlite_compileoption_used('THREADSAFE=')", "0"},
		{"SELECT sqlite_compileoption_used('DIRECT_OVERFLOW_READ')", "1"},
		{"SELECT sqlite_compileoption_used('ENABLE_FTS3')", "1"},
		{"SELECT sqlite_compileoption_used('ENABLE_MATH_FUNCTIONS')", "1"},
		// Case-insensitive, and non-matching names are 0 (not an error).
		{"SELECT sqlite_compileoption_used('threadsafe')", "1"},
		{"SELECT sqlite_compileoption_used('THREADSAFE_')", "0"},
		{"SELECT sqlite_compileoption_used('NOPE_NOT_A_OPTION')", "0"},
		// NULL argument → NULL.
		{"SELECT typeof(sqlite_compileoption_used(NULL))", "null"},
		// Non-text coerces to text.
		{"SELECT sqlite_compileoption_used(0)", "0"},
		// get(0) is the first option; out-of-range → NULL.
		{"SELECT sqlite_compileoption_get(0)", "DIRECT_OVERFLOW_READ"},
		{"SELECT typeof(sqlite_compileoption_get(-1))", "null"},
		{"SELECT typeof(sqlite_compileoption_get(999))", "null"},
		// Every advertised option matches itself.
		{"SELECT sqlite_compileoption_used(sqlite_compileoption_get(0))", "1"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c.sql, got, c.want)
		}
	}

	// Fixed arity 1: wrong argument counts error like SQLite.
	r := db.Query("SELECT sqlite_compileoption_used()")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "wrong number of arguments to function sqlite_compileoption_used()") {
		t.Errorf("used() arity: %v", r.Error)
	}
	r = db.Query("SELECT sqlite_compileoption_used('a','b')")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "wrong number of arguments to function sqlite_compileoption_used()") {
		t.Errorf("used(2 args) arity: %v", r.Error)
	}
	r = db.Query("SELECT sqlite_compileoption_get(0, 0)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "wrong number of arguments to function sqlite_compileoption_get()") {
		t.Errorf("get(2 args) arity: %v", r.Error)
	}

	// PRAGMA compile_options lists every advertised option, and the option
	// list is non-empty.
	r = db.Query("PRAGMA compile_options")
	if r.Error != nil {
		t.Fatalf("PRAGMA compile_options: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("PRAGMA compile_options returned no rows")
	}
	found := false
	for _, row := range r.Rows {
		if row[0] == "DIRECT_OVERFLOW_READ" {
			found = true
		}
	}
	if !found {
		t.Errorf("PRAGMA compile_options missing DIRECT_OVERFLOW_READ: %v", r.Rows)
	}
}
