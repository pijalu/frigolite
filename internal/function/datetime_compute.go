package function

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is part of the faithful port of SQLite's src/date.c (version
// 3.51.0) to pure Go. It holds the computation back-end for the date/time SQL
// functions: the julian-day arithmetic, broken-out Y/M/D h:m:s conversion, the
// localtime/utc conversions, output formatting (date/time/datetime/strftime),
// and the SQL function entry points (date(), time(), datetime(), julianday(),
// unixepoch(), strftime(), timediff()).
//
// The parse front-end (time-value strings, modifiers) lives in
// datetime_modifiers.go. SQLite uses UTC internally; the 'localtime'/'utc'
// modifiers convert via the platform localtime. The engine here computes from
// fixed inputs; clock dependent behavior ('now', localtime) is routed through
// hooks that tests may pin.

// dateTime mirrors the C DateTime structure. All dates are stored either as a
// julian day number (iJD, in milliseconds) or as broken-out Y/M/D h:m:s
// fields. iJD is the authoritative value once computed.
type dateTime struct {
	iJD       int64   // The julian day number times 86400000
	Y, M, D   int     // Year, month, and day
	h, m      int     // Hour and minutes
	tz        int     // Timezone offset in minutes
	s         float64 // Seconds
	validJD   bool    // True if iJD is valid
	validYMD  bool    // True if Y,M,D are valid
	validHMS  bool    // True if h,m,s are valid
	nFloor    int     // Days to implement "floor"
	rawS      bool    // Raw numeric value stored in s
	isError   bool    // An overflow has occurred
	useSubsec bool    // Display subsecond precision
	isUtc     bool    // Time is known to be UTC
	isLocal   bool    // Time is known to be localtime
}

func (p *dateTime) clearYMDHMSTZ() {
	p.validYMD = false
	p.validHMS = false
	p.tz = 0
}

func (p *dateTime) datetimeError() {
	*p = dateTime{isError: true}
}

// isDigit reports whether c is an ASCII digit (SQLite sqlite3Isdigit).
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isSpace reports whether c is SQLite whitespace.
func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// computeJD converts YYYY-MM-DD HH:MM:SS to julian day (Meeus page 61).
func (p *dateTime) computeJD() {
	var Y, M, D, A, B, X1, X2 int
	if p.validJD {
		return
	}
	if p.validYMD {
		Y = p.Y
		M = p.M
		D = p.D
	} else {
		// If no YMD specified, assume 2000-Jan-01
		Y = 2000
		M = 1
		D = 1
	}
	if Y < -4713 || Y > 9999 || p.rawS {
		p.datetimeError()
		return
	}
	if M <= 2 {
		Y--
		M += 12
	}
	A = (Y + 4800) / 100
	B = 38 - A + (A / 4)
	X1 = 36525 * (Y + 4716) / 100
	X2 = 306001 * (M + 1) / 10000
	p.iJD = int64((float64(X1+X2+D+B) - 1524.5) * 86400000)
	p.validJD = true
	if p.validHMS {
		p.iJD += int64(p.h)*3600000 + int64(p.m)*60000 + int64(p.s*1000+0.5)
		if p.tz != 0 {
			p.iJD -= int64(p.tz) * 60000
			p.validYMD = false
			p.validHMS = false
			p.tz = 0
			p.isUtc = true
			p.isLocal = false
		}
	}
}

// computeFloor determines day-of-month overflow, setting nFloor to the number
// of days that would need to be subtracted to reach the end of the month.
func (p *dateTime) computeFloor() {
	if p.D <= 28 {
		p.nFloor = 0
	} else if (1<<p.M)&0x15aa != 0 {
		p.nFloor = 0
	} else if p.M != 2 {
		if p.D == 31 {
			p.nFloor = 1
		} else {
			p.nFloor = 0
		}
	} else if p.Y%4 != 0 || (p.Y%100 == 0 && p.Y%400 != 0) {
		p.nFloor = p.D - 28
	} else {
		p.nFloor = p.D - 29
	}
}

// int_464269060799999 is the max iJD value (9999-12-31 23:59:59.999).
const int464269060799999 = int64(464269060799999)

// validJulianDay reports whether iJD is within the supported range.
func validJulianDay(iJD int64) bool {
	return iJD >= 0 && iJD <= int464269060799999
}

// computeYMD computes Year, Month, Day from the julian day number.
func (p *dateTime) computeYMD() {
	if p.validYMD {
		return
	}
	if !p.validJD {
		p.Y = 2000
		p.M = 1
		p.D = 1
	} else if !validJulianDay(p.iJD) {
		p.datetimeError()
		return
	} else {
		Z := int((p.iJD + 43200000) / 86400000)
		alpha := int((float64(Z)+32044.75)/36524.25) - 52
		A := Z + 1 + alpha - (alpha+100)/4 + 25
		B := A + 1524
		C := int((float64(B) - 122.1) / 365.25)
		D := (36525 * (C & 32767)) / 100
		E := int((float64(B) - float64(D)) / 30.6001)
		X1 := int(30.6001 * float64(E))
		p.D = B - D - X1
		if E < 14 {
			p.M = E - 1
		} else {
			p.M = E - 13
		}
		if p.M > 2 {
			p.Y = C - 4716
		} else {
			p.Y = C - 4715
		}
	}
	p.validYMD = true
}

// computeHMS computes Hour, Minute, Seconds from the julian day number.
func (p *dateTime) computeHMS() {
	if p.validHMS {
		return
	}
	p.computeJD()
	dayMS := int((p.iJD + 43200000) % 86400000)
	p.s = float64(dayMS%60000) / 1000.0
	dayMin := dayMS / 60000
	p.m = dayMin % 60
	p.h = dayMin / 60
	p.rawS = false
	p.validHMS = true
}

// computeYMDHMS computes both YMD and HMS.
func (p *dateTime) computeYMDHMS() {
	p.computeYMD()
	p.computeHMS()
}

// toLocaltime assumes the input DateTime is UTC and moves it to its localtime
// equivalent, using the localtime hook (or the system local time zone).
func (p *dateTime) toLocaltime() error {
	p.computeJD()
	t := p.iJD/1000 - 210866760000 // unix seconds

	var tm time.Time
	if hook := getLocaltimeHook(); hook != nil {
		localSec, err := hook(t)
		if err != nil {
			return fmt.Errorf("local time unavailable")
		}
		tm = time.Unix(localSec, 0).UTC()
	} else {
		tm = time.Unix(t, 0).In(time.Local)
	}
	frac := float64(p.iJD%1000) * 0.001
	p.Y = tm.Year()
	p.M = int(tm.Month())
	p.D = tm.Day()
	p.h = tm.Hour()
	p.m = tm.Minute()
	p.s = float64(tm.Second()) + frac
	p.validYMD = true
	p.validHMS = true
	p.validJD = false
	p.rawS = false
	p.tz = 0
	p.isError = false
	return nil
}

// toUtc converts the stored local time to UTC by iteratively correcting the
// Julian day estimate (mirrors date.c computeJD-style utc handling).
func (p *dateTime) toUtc() error {
	var iErr int64
	cnt := 0
	p.computeJD()
	iGuess := p.iJD
	iOrigJD := iGuess
	for {
		new := dateTime{}
		iGuess -= iErr
		new.iJD = iGuess
		new.validJD = true
		if err := new.toLocaltime(); err != nil {
			return err
		}
		new.computeJD()
		iErr = new.iJD - iOrigJD
		if iErr == 0 || cnt >= 3 {
			break
		}
		cnt++
	}
	*p = dateTime{}
	p.iJD = iGuess
	p.validJD = true
	p.isUtc = true
	p.isLocal = false
	return nil
}

// --- Output formatting ---

// formatDate formats YYYY-MM-DD (with a leading '-' for negative years).
func formatDate(p *dateTime) string {
	Y := p.Y
	if Y < 0 {
		Y = -Y
	}
	var sb strings.Builder
	if p.Y < 0 {
		sb.WriteByte('-')
	}
	sb.WriteString(pad4(Y))
	sb.WriteByte('-')
	sb.WriteString(pad2(p.M))
	sb.WriteByte('-')
	sb.WriteString(pad2(p.D))
	return sb.String()
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func pad4(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

func pad3(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// formatDatetime formats YYYY-MM-DD HH:MM:SS (or with .SSS when useSubsec).
func formatDatetime(p *dateTime) string {
	Y := p.Y
	if Y < 0 {
		Y = -Y
	}
	var sb strings.Builder
	if p.Y < 0 {
		sb.WriteByte('-')
	}
	sb.WriteString(pad4(Y))
	sb.WriteByte('-')
	sb.WriteString(pad2(p.M))
	sb.WriteByte('-')
	sb.WriteString(pad2(p.D))
	sb.WriteByte(' ')
	sb.WriteString(pad2(p.h))
	sb.WriteByte(':')
	sb.WriteString(pad2(p.m))
	sb.WriteByte(':')
	if p.useSubsec {
		s := int(1000.0*p.s + 0.5)
		sb.WriteString(pad2(s / 1000))
		sb.WriteByte('.')
		sb.WriteString(pad3(s % 1000))
	} else {
		s := int(p.s)
		sb.WriteString(pad2(s))
	}
	return sb.String()
}

// formatTime formats HH:MM:SS (or with .SSS when useSubsec).
func formatTime(p *dateTime) string {
	var sb strings.Builder
	sb.WriteString(pad2(p.h))
	sb.WriteByte(':')
	sb.WriteString(pad2(p.m))
	sb.WriteByte(':')
	if p.useSubsec {
		s := int(1000.0*p.s + 0.5)
		sb.WriteString(pad2(s / 1000))
		sb.WriteByte('.')
		sb.WriteString(pad3(s % 1000))
	} else {
		s := int(p.s)
		sb.WriteString(pad2(s))
	}
	return sb.String()
}

// --- Scalar functions ---

// fnDATE implements date(TIMESTRING, MOD, ...) → YYYY-MM-DD.
func fnDATE(args []interface{}) (interface{}, error) {
	return dateFunc("date", args, dateOnly)
}

// fnDateNow returns date('now') — the statement-cached current date.
func FnDateNow() (interface{}, error) {
	return dateFunc("date", nil, dateOnly)
}

// fnTimeNow returns time('now') — the statement-cached current time.
func FnTimeNow() (interface{}, error) {
	return dateFunc("time", nil, timeOnly)
}

// fnDateTimeNow returns datetime('now') — the statement-cached current
// date and time.
func FnDateTimeNow() (interface{}, error) {
	return dateFunc("datetime", nil, dateTimeOnly)
}

// fnTIME implements time(TIMESTRING, MOD, ...) → HH:MM:SS.
func fnTIME(args []interface{}) (interface{}, error) {
	return dateFunc("time", args, timeOnly)
}

// fnDATETIME implements datetime(TIMESTRING, MOD, ...) → YYYY-MM-DD HH:MM:SS.
func fnDATETIME(args []interface{}) (interface{}, error) {
	return dateFunc("datetime", args, dateTimeOnly)
}

type dateOutput int

const (
	dateOnly dateOutput = iota
	timeOnly
	dateTimeOnly
)

func dateFunc(fnName string, args []interface{}, out dateOutput) (interface{}, error) {
	p, ok, err := isDate(fnName, args)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	switch out {
	case dateOnly:
		p.computeYMD()
		return formatDate(&p), nil
	case timeOnly:
		p.computeHMS()
		return formatTime(&p), nil
	default:
		p.computeYMDHMS()
		return formatDatetime(&p), nil
	}
}

// fnJULIANDAY implements julianday(TIMESTRING, MOD, ...) → real.
func fnJULIANDAY(args []interface{}) (interface{}, error) {
	p, ok, err := isDate("julianday", args)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	p.computeJD()
	return float64(p.iJD) / 86400000.0, nil
}

// fnUNIXEPOCH implements unixepoch(TIMESTRING, MOD, ...) → int or real.
func fnUNIXEPOCH(args []interface{}) (interface{}, error) {
	p, ok, err := isDate("unixepoch", args)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	p.computeJD()
	if p.useSubsec {
		return float64(p.iJD-210866760000000) / 1000.0, nil
	}
	return p.iJD/1000 - 210866760000, nil
}

// fnSTRFTIME implements strftime(FORMAT, TIMESTRING, MOD, ...) → text.
func fnSTRFTIME(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, nil
	}
	format := args[0]
	if format == nil {
		return nil, nil
	}
	zFmt := toString(unwrapValue(format))
	p, ok, err := isDate("strftime", args[1:])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	p.computeJD()
	p.computeYMDHMS()
	out, ok2 := strftimeFormat(zFmt, &p)
	if !ok2 {
		return nil, nil // unrecognized format code → NULL
	}
	return out, nil
}

// strftimeFormat applies SQLite's strftime format codes to p.
func strftimeFormat(zFmt string, p *dateTime) (string, bool) {
	var sb strings.Builder
	for i := 0; i < len(zFmt); i++ {
		if zFmt[i] != '%' {
			sb.WriteByte(zFmt[i])
			continue
		}
		i++
		if i >= len(zFmt) {
			return "", false
		}
		cf := zFmt[i]
		if !strftimeAppend(&sb, cf, p) {
			return "", false // unrecognized → NULL
		}
	}
	return sb.String(), true
}

// strftimeAppend renders one %-code. Returns false for an unrecognized code.
func strftimeAppend(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'd', 'e', 'F', 'j', 'm', 'Y':
		return strftimeDate(sb, cf, p)
	case 'H', 'k', 'I', 'l':
		return strftimeHour(sb, cf, p)
	case 'M', 'p', 'P', 'R', 'S', 'T':
		return strftimeClock(sb, cf, p)
	case 'G', 'g', 'U', 'V', 'W':
		return strftimeWeek(sb, cf, p)
	case 'f', 'J', 's':
		return strftimeNumeric(sb, cf, p)
	case 'u', 'w':
		return strftimeWeekday(sb, cf, p)
	case '%':
		sb.WriteByte('%')
		return true
	default:
		return false // unrecognized → NULL
	}
}

// strftimeDate renders the calendar-date codes %d %e %F %j %m %Y.
func strftimeDate(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'd':
		sb.WriteString(pad2(p.D))
	case 'e':
		s := strconv.Itoa(p.D)
		if p.D < 10 {
			s = " " + s
		}
		sb.WriteString(s)
	case 'F':
		sb.WriteString(pad4(p.Y))
		sb.WriteByte('-')
		sb.WriteString(pad2(p.M))
		sb.WriteByte('-')
		sb.WriteString(pad2(p.D))
	case 'j': // Day of year: Jan01==1
		sb.WriteString(pad3(daysAfterJan01(p) + 1))
	case 'm':
		sb.WriteString(pad2(p.M))
	case 'Y':
		sb.WriteString(pad4(p.Y))
	}
	return true
}

// strftimeHour renders the hour codes %H %k %I %l.
func strftimeHour(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'H':
		sb.WriteString(pad2(p.h))
	case 'k':
		s := strconv.Itoa(p.h)
		if p.h < 10 {
			s = " " + s
		}
		sb.WriteString(s)
	case 'I', 'l':
		h := p.h
		if h > 12 {
			h -= 12
		}
		if h == 0 {
			h = 12
		}
		if cf == 'I' {
			sb.WriteString(pad2(h))
		} else {
			s := strconv.Itoa(h)
			if h < 10 {
				s = " " + s
			}
			sb.WriteString(s)
		}
	}
	return true
}

// strftimeClock renders the clock codes %M %p %P %R %S %T.
func strftimeClock(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'M':
		sb.WriteString(pad2(p.m))
	case 'p':
		if p.h >= 12 {
			sb.WriteString("PM")
		} else {
			sb.WriteString("AM")
		}
	case 'P':
		if p.h >= 12 {
			sb.WriteString("pm")
		} else {
			sb.WriteString("am")
		}
	case 'R':
		sb.WriteString(pad2(p.h))
		sb.WriteByte(':')
		sb.WriteString(pad2(p.m))
	case 'S':
		sb.WriteString(pad2(int(p.s)))
	case 'T':
		sb.WriteString(pad2(p.h))
		sb.WriteByte(':')
		sb.WriteString(pad2(p.m))
		sb.WriteByte(':')
		sb.WriteString(pad2(int(p.s)))
	}
	return true
}

// strftimeWeek renders the ISO/week-number codes %G %g %U %V %W.
func strftimeWeek(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'G', 'g':
		y := *p
		y.iJD += int64(3-daysAfterMonday(p)) * 86400000
		y.validYMD = false
		y.computeYMD()
		if cf == 'g' {
			sb.WriteString(pad2(y.Y % 100))
		} else {
			sb.WriteString(pad4(y.Y))
		}
	case 'U':
		sb.WriteString(pad2((daysAfterJan01(p) - daysAfterSunday(p) + 7) / 7))
	case 'V':
		y := *p
		y.iJD += int64(3-daysAfterMonday(p)) * 86400000
		y.validYMD = false
		y.computeYMD()
		sb.WriteString(pad2(daysAfterJan01(&y)/7 + 1))
	case 'W':
		sb.WriteString(pad2((daysAfterJan01(p) - daysAfterMonday(p) + 7) / 7))
	}
	return true
}

// strftimeNumeric renders the numeric codes %f %J %s.
func strftimeNumeric(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'f': // Fractional seconds SS.SSS (non-standard)
		s := p.s
		if s > 59.999 {
			s = 59.999
		}
		sb.WriteString(fmt.Sprintf("%06.3f", s))
	case 'J': // Julian day number (non-standard)
		sb.WriteString(fmt.Sprintf("%.16g", float64(p.iJD)/86400000.0))
	case 's':
		if p.useSubsec {
			sb.WriteString(fmt.Sprintf("%.3f", float64(p.iJD-210866760000000)/1000.0))
		} else {
			iS := p.iJD/1000 - 210866760000
			sb.WriteString(strconv.FormatInt(iS, 10))
		}
	}
	return true
}

// strftimeWeekday renders the weekday codes %u (1-7) and %w (0-6).
func strftimeWeekday(sb *strings.Builder, cf byte, p *dateTime) bool {
	switch cf {
	case 'u':
		c := daysAfterSunday(p)
		if c == 0 {
			sb.WriteByte('7')
		} else {
			sb.WriteByte(byte('0' + c))
		}
	case 'w':
		sb.WriteByte(byte('0' + daysAfterSunday(p)))
	}
	return true
}

// daysAfterJan01 returns the zero-based day number for the current year
// (Jan01=0 ... Dec31=364 or 365).
func daysAfterJan01(p *dateTime) int {
	jan01 := *p
	jan01.validJD = false
	jan01.M = 1
	jan01.D = 1
	jan01.computeJD()
	return int((p.iJD - jan01.iJD + 43200000) / 86400000)
}

// daysAfterMonday returns 0=Monday ... 6=Sunday.
func daysAfterMonday(p *dateTime) int {
	return int((p.iJD+43200000)/86400000) % 7
}

// daysAfterSunday returns 0=Sunday ... 6=Saturday.
func daysAfterSunday(p *dateTime) int {
	return int((p.iJD+129600000)/86400000) % 7
}

// timediffForward computes the year/month difference for the d1 >= d2 case,
// adjusting d2's calendar so the residual iJD difference is within one month.
func timediffForward(d1, d2 *dateTime) (int, int) {
	Y := d1.Y - d2.Y
	if Y != 0 {
		d2.Y = d1.Y
		d2.validJD = false
		d2.computeJD()
	}
	M := d1.M - d2.M
	if M < 0 {
		Y--
		M += 12
	}
	if M != 0 {
		d2.M = d1.M
		d2.validJD = false
		d2.computeJD()
	}
	for d1.iJD < d2.iJD {
		M--
		if M < 0 {
			M = 11
			Y--
		}
		d2.M--
		if d2.M < 1 {
			d2.M = 12
			d2.Y--
		}
		d2.validJD = false
		d2.computeJD()
	}
	return Y, M
}

// timediffReverse computes the year/month difference for the d2 > d1 case,
// adjusting d2's calendar so the residual iJD difference is within one month.
func timediffReverse(d1, d2 *dateTime) (int, int) {
	Y := d2.Y - d1.Y
	if Y != 0 {
		d2.Y = d1.Y
		d2.validJD = false
		d2.computeJD()
	}
	M := d2.M - d1.M
	if M < 0 {
		Y--
		M += 12
	}
	if M != 0 {
		d2.M = d1.M
		d2.validJD = false
		d2.computeJD()
	}
	for d1.iJD > d2.iJD {
		M--
		if M < 0 {
			M = 11
			Y--
		}
		d2.M++
		if d2.M > 12 {
			d2.M = 1
			d2.Y++
		}
		d2.validJD = false
		d2.computeJD()
	}
	return Y, M
}

// fnTIMEDIFF implements timediff(DATE1, DATE2) → '+YYYY-MM-DD HH:MM:SS.SSS'.
func fnTIMEDIFF(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, nil
	}
	d1, ok1, err1 := isDate("timediff", args[0:1])
	if err1 != nil {
		return nil, err1
	}
	d2, ok2, err2 := isDate("timediff", args[1:2])
	if err2 != nil {
		return nil, err2
	}
	if !ok1 || !ok2 {
		return nil, nil
	}
	d1.computeYMDHMS()
	d2.computeYMDHMS()
	sign := byte('+')
	var Y, M int
	if d1.iJD >= d2.iJD {
		Y, M = timediffForward(&d1, &d2)
		d1.iJD -= d2.iJD
		d1.iJD += 148699540800000
	} else {
		sign = '-'
		Y, M = timediffReverse(&d1, &d2)
		d1.iJD = d2.iJD - d1.iJD
		d1.iJD += 148699540800000
	}
	d1.clearYMDHMSTZ()
	d1.computeYMDHMS()
	return fmt.Sprintf("%c%04d-%02d-%02d %02d:%02d:%06.3f", sign, Y, M, d1.D-1, d1.h, d1.m, d1.s), nil
}

// --- Clock and localtime hooks (test-pinnable) ---

var (
	nowMu       sync.RWMutex
	nowFunc     = time.Now
	stmtTimeMu  sync.RWMutex
	stmtTimeSet bool
	stmtTime    time.Time
	localtimeMu sync.RWMutex
	localtimeFn func(unixSec int64) (int64, error)
)

// SetNowFunc replaces the clock used by 'now' (nil restores the system
// clock). Tests use this to pin time-dependent behavior.
func SetNowFunc(fn func() time.Time) {
	nowMu.Lock()
	defer nowMu.Unlock()
	if fn == nil {
		nowFunc = time.Now
	} else {
		nowFunc = fn
	}
}

// SetStmtTime pins the 'now' value for the duration of one SQL statement,
// matching SQLite's sqlite3StmtCurrentTime (all 'now' calls within a single
// statement return the same instant, even when a user function sleeps in
// between). The engine calls this once at statement start and clears it with
// a zero value at the end.
func SetStmtTime(t time.Time) {
	stmtTimeMu.Lock()
	defer stmtTimeMu.Unlock()
	if t.IsZero() {
		stmtTimeSet = false
	} else {
		stmtTimeSet = true
		stmtTime = t
	}
}

func currentTime() time.Time {
	stmtTimeMu.RLock()
	cached := stmtTimeSet
	t := stmtTime
	stmtTimeMu.RUnlock()
	if cached {
		return t
	}
	return Now()
}

// Now returns the current wall-clock time, honoring a test-installed
// SetNowFunc override (nil restores the system clock).
func Now() time.Time {
	nowMu.RLock()
	defer nowMu.RUnlock()
	return nowFunc()
}

// SetLocaltimeHook installs a test hook used by the 'localtime'/'utc'
// modifiers. The hook receives a UTC unix timestamp and returns the local
// unix timestamp (same epoch) or an error ("local time unavailable").
// Passing nil restores the system local time zone.
func SetLocaltimeHook(fn func(unixSec int64) (int64, error)) {
	localtimeMu.Lock()
	defer localtimeMu.Unlock()
	localtimeFn = fn
}

func getLocaltimeHook() func(unixSec int64) (int64, error) {
	localtimeMu.RLock()
	defer localtimeMu.RUnlock()
	return localtimeFn
}

// --- Pure-context mechanism ---
//
// SQLite marks function calls in CHECK constraints, generated columns, and
// index expressions as OP_PureFunc: date/time functions that use
// non-deterministic inputs ('now', 'subsec', 'localtime', 'utc') then throw
// "non-deterministic use of %s() in %s". The exec engine wraps evaluation of
// such expressions with WithPureContext; date functions consult PureContext.

var (
	pureMu    sync.Mutex
	pureStack []string
)

// WithPureContext runs fn with the given pure-context label pushed on the
// stack. Labels: "check", "gencol", "index".
func WithPureContext(ctx string, fn func() error) error {
	pureMu.Lock()
	pureStack = append(pureStack, ctx)
	pureMu.Unlock()
	err := fn()
	pureMu.Lock()
	pureStack = pureStack[:len(pureStack)-1]
	pureMu.Unlock()
	return err
}

// PureContext returns the innermost active pure-context label, or "" if none.
func PureContext() string {
	pureMu.Lock()
	defer pureMu.Unlock()
	if len(pureStack) == 0 {
		return ""
	}
	return pureStack[len(pureStack)-1]
}
