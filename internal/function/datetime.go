package function

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pijalu/frigolite/internal/util"
)

// This file is a faithful port of SQLite's src/date.c (version 3.51.0) to
// pure Go. It implements the date/time SQL functions:
//
//	date(), time(), datetime(), julianday(), unixepoch(), strftime(),
//	timediff(), and the 'now' time-value with all modifiers:
//	+N units, start of month/year/day, weekday N, utc, localtime, subsec,
//	unixepoch, julianday, auto, and the +/-YYYY-MM-DD HH:MM:SS[.SSS] forms.
//
// SQLite uses UTC internally; the 'localtime'/'utc' modifiers convert via the
// platform localtime. The engine here computes from fixed inputs; clock
// dependent behavior ('now', localtime) is routed through hooks that tests
// may pin.

// dateTime mirrors the C DateTime structure. All dates are stored either as a
// julian day number (iJD, in milliseconds) or as broken-out Y/M/D h:m:s
// fields. iJD is the authoritative value once computed.
type dateTime struct {
	iJD      int64   // The julian day number times 86400000
	Y, M, D  int     // Year, month, and day
	h, m     int     // Hour and minutes
	tz       int     // Timezone offset in minutes
	s        float64 // Seconds
	validJD  bool    // True if iJD is valid
	validYMD bool    // True if Y,M,D are valid
	validHMS bool    // True if h,m,s are valid
	nFloor   int     // Days to implement "floor"
	rawS     bool    // Raw numeric value stored in s
	isError  bool    // An overflow has occurred
	useSubsec bool   // Display subsecond precision
	isUtc    bool    // Time is known to be UTC
	isLocal  bool    // Time is known to be localtime
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

// getDigits parses one or more integers from zDate according to zFormat.
// Each 4-character format spec is:
//
//	A: number of digits (2 or 4)
//	B: minimum value
//	C: maximum value class: a=12 b=14 c=24 d=31 e=59 f=9999
//	D: separator character, or 0 for the last number
//
// It returns the number of successful conversions.
func getDigits(zDate string, zFormat string, vals ...*int) int {
	// aMx[] translates the 3rd character of each format spec into a max size.
	aMx := [...]int{12, 14, 24, 31, 59, 14712}
	cnt := 0
	vi := 0
	pos := 0
	zf := 0
	for {
		if zf+3 > len(zFormat) {
			break
		}
		N := int(zFormat[zf] - '0')
		min := int(zFormat[zf+1] - '0')
		maxC := zFormat[zf+2]
		nextC := byte(0)
		if zf+3 < len(zFormat) {
			nextC = zFormat[zf+3]
		}
		max := aMx[maxC-'a']
		val := 0
		for k := 0; k < N; k++ {
			if pos >= len(zDate) || !isDigit(zDate[pos]) {
				goto endGetDigits
			}
			val = val*10 + int(zDate[pos]-'0')
			pos++
		}
		if val < min || val > max || (nextC != 0 && (pos >= len(zDate) || nextC != zDate[pos])) {
			goto endGetDigits
		}
		if vi < len(vals) {
			*vals[vi] = val
			vi++
		}
		pos++
		cnt++
		zf += 4
		if nextC == 0 {
			break
		}
	}
endGetDigits:
	return cnt
}

// parseTimezone parses a timezone extension on the end of a date-time:
// (+/-)HH:MM or "Z" (zulu). On success p.tz holds minutes of change and 0 is
// returned. A missing specifier is not an error.
func parseTimezone(zDate string, p *dateTime) bool {
	sgn := 0
	var nHr, nMn int
	for len(zDate) > 0 && isSpace(zDate[0]) {
		zDate = zDate[1:]
	}
	p.tz = 0
	if len(zDate) == 0 {
		return false
	}
	c := zDate[0]
	switch {
	case c == '-':
		sgn = -1
	case c == '+':
		sgn = +1
	case c == 'Z' || c == 'z':
		zDate = zDate[1:]
		p.isLocal = false
		p.isUtc = true
		goto zuluTime
	default:
		return true
	}
	zDate = zDate[1:]
	if getDigits(zDate, "20b:20e", &nHr, &nMn) != 2 {
		return true
	}
	zDate = zDate[5:]
	p.tz = sgn * (nMn + nHr*60)
	if p.tz == 0 {
		p.isLocal = false
		p.isUtc = true
	}
zuluTime:
	for len(zDate) > 0 && isSpace(zDate[0]) {
		zDate = zDate[1:]
	}
	return len(zDate) != 0
}

// parseHhMmSs parses times of the form HH:MM or HH:MM:SS or HH:MM:SS.FFFF.
// Returns true on a parsing error.
func parseHhMmSs(zDate string, p *dateTime) bool {
	var h, m, s int
	ms := 0.0
	if getDigits(zDate, "20c:20e", &h, &m) != 2 {
		return true
	}
	zDate = zDate[5:]
	if len(zDate) > 0 && zDate[0] == ':' {
		zDate = zDate[1:]
		if getDigits(zDate, "20e", &s) != 1 {
			return true
		}
		zDate = zDate[2:]
		if len(zDate) > 0 && zDate[0] == '.' && len(zDate) > 1 && isDigit(zDate[1]) {
			rScale := 1.0
			zDate = zDate[1:]
			for len(zDate) > 0 && isDigit(zDate[0]) {
				ms = ms*10.0 + float64(zDate[0]-'0')
				rScale *= 10.0
				zDate = zDate[1:]
			}
			ms /= rScale
			// Truncate to avoid problems with sub-millisecond rounding.
			if ms > 0.999 {
				ms = 0.999
			}
		}
	} else {
		s = 0
	}
	p.validJD = false
	p.rawS = false
	p.validHMS = true
	p.h = h
	p.m = m
	p.s = float64(s) + ms
	if parseTimezone(zDate, p) {
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

// parseYyyyMmDd parses dates of the form YYYY-MM-DD [HH:MM[:SS[.FFF]]].
// Returns true on a parse error.
func parseYyyyMmDd(zDate string, p *dateTime) bool {
	var Y, M, D int
	neg := false
	if len(zDate) > 0 && zDate[0] == '-' {
		zDate = zDate[1:]
		neg = true
	}
	if getDigits(zDate, "40f-21a-21d", &Y, &M, &D) != 3 {
		return true
	}
	zDate = zDate[10:]
	for len(zDate) > 0 && (isSpace(zDate[0]) || zDate[0] == 'T') {
		zDate = zDate[1:]
	}
	if !parseHhMmSs(zDate, p) {
		// We got the time
	} else if len(zDate) == 0 {
		p.validHMS = false
	} else {
		return true
	}
	p.validJD = false
	p.validYMD = true
	if neg {
		Y = -Y
	}
	p.Y = Y
	p.M = M
	p.D = D
	p.computeFloor()
	if p.tz != 0 {
		p.computeJD()
	}
	return false
}

// setRawDateNumber stores a numeric time-value. "r" might be a julian day
// number or unix seconds. If within julian-day range it is installed as such.
func (p *dateTime) setRawDateNumber(r float64) {
	p.s = r
	p.rawS = true
	if r >= 0.0 && r < 5373484.5 {
		p.iJD = int64(r*86400000.0 + 0.5)
		p.validJD = true
	}
}

// parseDateOrTime parses a time-value string. The returned error is non-nil
// only for real errors (local time unavailable, non-determinism); an
// unrecognized time-value is not an error — SQLite returns NULL for it.
func (p *dateTime) parseDateOrTime(zDate string, fnName string) (string, error) {
	if !parseYyyyMmDd(zDate, p) {
		return "", nil
	} else if !parseHhMmSs(zDate, p) {
		return "", nil
	} else if strings.EqualFold(zDate, "now") {
		if ctx := PureContext(); ctx != "" {
			return "", nonDeterministicError(fnName, ctx)
		}
		return "", p.setDateTimeToCurrent()
	} else if r, ok := atofOK(zDate); ok {
		p.setRawDateNumber(r)
		return "", nil
	} else if strings.EqualFold(zDate, "subsec") || strings.EqualFold(zDate, "subsecond") {
		if ctx := PureContext(); ctx != "" {
			return "", nonDeterministicError(fnName, ctx)
		}
		p.useSubsec = true
		return "", p.setDateTimeToCurrent()
	}
	return "", errUnrecognizedDate
}

// setDateTimeToCurrent sets the time to "now" from the clock hook.
func (p *dateTime) setDateTimeToCurrent() error {
	now := currentTime()
	p.iJD = now.UnixMilli() + 210866760000000
	p.validJD = true
	p.isUtc = true
	p.isLocal = false
	p.clearYMDHMSTZ()
	return nil
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

// aXformType mirrors the aXformType[] table in date.c: 'NNN units' transforms.
type xformType struct {
	nName  int
	zName  string
	rLimit float64
	rXform float64
}

var aXformType = []xformType{
	{6, "second", 4.6427e+14, 1.0},
	{6, "minute", 7.7379e+12, 60.0},
	{4, "hour", 1.2897e+11, 3600.0},
	{3, "day", 5373485.0, 86400.0},
	{5, "month", 176546.0, 2592000.0},
	{4, "year", 14713.0, 31536000.0},
}

// autoAdjustDate tries to figure out if the raw number p is a julian day
// number or a unix timestamp and sets p appropriately.
func (p *dateTime) autoAdjustDate() {
	if !p.rawS || p.validJD {
		p.rawS = false
	} else if p.s >= -210866760000 && p.s <= 253402300799 {
		// -4713-11-24 12:00:00 .. 9999-12-31 23:59:59 as unix seconds
		r := p.s*1000.0 + 210866760000000.0
		p.clearYMDHMSTZ()
		p.iJD = int64(r + 0.5)
		p.validJD = true
		p.rawS = false
	}
}

// errUnrecognizedDate marks a time-value string SQLite cannot parse; the
// date function returns NULL for it (not an error).
var errUnrecognizedDate = fmt.Errorf("unrecognized date/time")

// nonDeterministicError builds the "non-deterministic use of %s() in %s" error.
func nonDeterministicError(fnName, ctx string) error {
	switch ctx {
	case "check":
		return fmt.Errorf("non-deterministic use of %s() in a CHECK constraint", fnName)
	case "gencol":
		return fmt.Errorf("non-deterministic use of %s() in a generated column", fnName)
	default:
		return fmt.Errorf("non-deterministic use of %s() in an index", fnName)
	}
}

// parseModifier processes one modifier. It returns (ok, err); ok=false means
// an unrecognized modifier (which makes the whole function return NULL), err
// is non-nil only for real errors (localtime failure, non-determinism).
func (p *dateTime) parseModifier(z string, idx int, fnName string) (bool, error) {
	ok := false
	if len(z) == 0 {
		return false, nil
	}
	lc := lowerASCII(z[0])
	switch lc {
	case 'a':
		// auto
		if strings.EqualFold(z, "auto") {
			if idx > 1 {
				return false, nil // IMP: R-33611-57934
			}
			p.autoAdjustDate()
			ok = true
		}
	case 'c':
		// ceiling
		if strings.EqualFold(z, "ceiling") {
			p.computeJD()
			p.clearYMDHMSTZ()
			ok = true
			p.nFloor = 0
		}
	case 'f':
		// floor
		if strings.EqualFold(z, "floor") {
			p.computeJD()
			p.iJD -= int64(p.nFloor) * 86400000
			p.clearYMDHMSTZ()
			ok = true
		}
	case 'j':
		// julianday
		if strings.EqualFold(z, "julianday") {
			if idx > 1 {
				return false, nil // IMP: R-31176-64601
			}
			if p.validJD && p.rawS {
				ok = true
				p.rawS = false
			}
		}
	case 'l':
		// localtime
		if strings.EqualFold(z, "localtime") {
			if ctx := PureContext(); ctx != "" {
				return false, nonDeterministicError(fnName, ctx)
			}
			if p.isLocal {
				ok = true
			} else {
				if err := p.toLocaltime(); err != nil {
					return false, err
				}
				ok = true
			}
			p.isUtc = false
			p.isLocal = true
		}
	case 'u':
		// unixepoch / utc
		if strings.EqualFold(z, "unixepoch") && p.rawS {
			if idx > 1 {
				return false, nil // IMP: R-49255-55373
			}
			r := p.s*1000.0 + 210866760000000.0
			if r >= 0.0 && r < 464269060800000.0 {
				p.clearYMDHMSTZ()
				p.iJD = int64(r + 0.5)
				p.validJD = true
				p.rawS = false
				ok = true
			}
		} else if strings.EqualFold(z, "utc") {
			if ctx := PureContext(); ctx != "" {
				return false, nonDeterministicError(fnName, ctx)
			}
			if !p.isUtc {
				iOrigJD := p.iJD
				iGuess := p.iJD
				iErr := int64(0)
				cnt := 0
				p.computeJD()
				iGuess = p.iJD
				iOrigJD = iGuess
				for {
					new := dateTime{}
					iGuess -= iErr
					new.iJD = iGuess
					new.validJD = true
					if err := new.toLocaltime(); err != nil {
						return false, err
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
			}
			ok = true
		}
	case 'w':
		// weekday N
		if strings.HasPrefix(strings.ToLower(z), "weekday ") {
			var r float64
			rest := z[8:]
			if f, err := strconv.ParseFloat(rest, 64); err == nil {
				r = f
			} else {
				break
			}
			if r >= 0.0 && r < 7.0 && float64(int(r)) == r {
				p.computeYMDHMS()
				p.tz = 0
				p.validJD = false
				p.computeJD()
				n := int(r)
				Z := ((p.iJD + 129600000) / 86400000) % 7
				if Z > int64(n) {
					Z -= 7
				}
				p.iJD += (int64(n) - Z) * 86400000
				p.clearYMDHMSTZ()
				ok = true
			}
		}
	case 's':
		// start of TTTTT / subsec / subsecond
		if !strings.HasPrefix(strings.ToLower(z), "start of ") {
			if strings.EqualFold(z, "subsec") || strings.EqualFold(z, "subsecond") {
				p.useSubsec = true
				ok = true
			}
			break
		}
		if !p.validJD && !p.validYMD && !p.validHMS {
			break
		}
		rest := z[9:]
		p.computeYMD()
		p.validHMS = true
		p.h = 0
		p.m = 0
		p.s = 0.0
		p.rawS = false
		p.tz = 0
		p.validJD = false
		if strings.EqualFold(rest, "month") {
			p.D = 1
			ok = true
		} else if strings.EqualFold(rest, "year") {
			p.M = 1
			p.D = 1
			ok = true
		} else if strings.EqualFold(rest, "day") {
			ok = true
		}
	case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		ok, err := p.parseModifierNum(z, fnName)
		return ok, err
	}
	return ok, nil
}

// parseModifierNum handles the +/-NNN units, +/-YYYY-MM-DD, and +/-HH:MM:SS
// modifier forms (date.c case '+': in parseModifier).
func (p *dateTime) parseModifierNum(z string, fnName string) (bool, error) {
	ok := false
	z0 := z[0]
	var Y, M, D, h, m int
	var r float64

	// Find the end of the leading number (stopping at ':', whitespace, or a
	// '-' that begins a YYYY-MM-DD modifier).
	n := 1
	for n < len(z) && z[n] != ':' && !isSpace(z[n]) {
		if z[n] == '-' {
			if n == 5 && getDigits(z[1:], "40f", &Y) == 1 {
				break
			}
			if n == 6 && getDigits(z[1:], "50f", &Y) == 1 {
				break
			}
		}
		n++
	}
	if f, err := strconv.ParseFloat(z[:n], 64); err == nil {
		r = f
	} else {
		return false, nil
	}

	if n < len(z) && z[n] == '-' {
		// A modifier of the form (+|-)YYYY-MM-DD adds or subtracts the
		// specified number of years, months, and days.
		if z0 != '+' && z0 != '-' {
			return false, nil
		}
		if n == 5 {
			if getDigits(z[1:], "40f-20a-20d", &Y, &M, &D) != 3 {
				return false, nil
			}
		} else {
			if n != 6 {
				return false, nil
			}
			if getDigits(z[1:], "50f-20a-20d", &Y, &M, &D) != 3 {
				return false, nil
			}
			z = z[1:]
		}
		if M >= 12 {
			return false, nil // M range 0..11
		}
		if D >= 31 {
			return false, nil // D range 0..30
		}
		p.computeYMDHMS()
		p.validJD = false
		if z0 == '-' {
			p.Y -= Y
			p.M -= M
			D = -D
		} else {
			p.Y += Y
			p.M += M
		}
		x := 0
		if p.M > 0 {
			x = (p.M - 1) / 12
		} else {
			x = (p.M - 12) / 12
		}
		p.Y += x
		p.M -= x * 12
		p.computeFloor()
		p.computeJD()
		p.validHMS = false
		p.validYMD = false
		p.iJD += int64(D) * 86400000
		if n+11 >= len(z) {
			ok = true
			breakOut := true
			_ = breakOut
			return ok, nil
		}
		if isSpace(z[11]) && getDigits(z[12:], "20c:20e", &h, &m) == 2 {
			z = z[12:]
			n = 2
		} else {
			return false, nil
		}
	}

	// The +/-HH:MM:SS[.FFF] modifier form.
	if n < len(z) && z[n] == ':' {
		tx := dateTime{}
		z2 := z
		if len(z2) > 0 && !isDigit(z2[0]) {
			z2 = z2[1:]
		}
		if parseHhMmSs(z2, &tx) {
			return false, nil
		}
		tx.computeJD()
		tx.iJD -= 43200000
		day := tx.iJD / 86400000
		tx.iJD -= day * 86400000
		if z0 == '-' {
			tx.iJD = -tx.iJD
		}
		p.computeJD()
		p.clearYMDHMSTZ()
		p.iJD += tx.iJD
		return true, nil
	}

	// The 'NNN units' transform.
	z = z[n:]
	for len(z) > 0 && isSpace(z[0]) {
		z = z[1:]
	}
	n = len(z)
	if n < 3 || n > 10 {
		return false, nil
	}
	if lowerASCII(z[n-1]) == 's' {
		n--
	}
	p.computeJD()
	rRounder := 0.5
	if r < 0 {
		rRounder = -0.5
	}
	p.nFloor = 0
	for i := range aXformType {
		xf := &aXformType[i]
		if xf.nName == n && strings.EqualFold(xf.zName, z[:n]) && r > -xf.rLimit && r < xf.rLimit {
			switch i {
			case 4: // Special processing to add months
				p.computeYMDHMS()
				p.M += int(r)
				x := 0
				if p.M > 0 {
					x = (p.M - 1) / 12
				} else {
					x = (p.M - 12) / 12
				}
				p.Y += x
				p.M -= x * 12
				p.computeFloor()
				p.validJD = false
				r -= float64(int(r))
			case 5: // Special processing to add years
				y := int(r)
				p.computeYMDHMS()
				p.Y += y
				p.computeFloor()
				p.validJD = false
				r -= float64(int(r))
			}
			p.computeJD()
			p.iJD += int64(r*1000.0*xf.rXform + rRounder)
			ok = true
			break
		}
	}
	p.clearYMDHMSTZ()
	return ok, nil
}

// isDate processes the function arguments: argv[0] is a date-time stamp,
// argv[1:] are modifiers. Returns the parsed DateTime, an ok flag, and an
// error (nil error + ok=false means the result is NULL).
func isDate(fnName string, args []interface{}) (dateTime, bool, error) {
	var p dateTime
	if len(args) == 0 {
		if ctx := PureContext(); ctx != "" {
			return p, false, nonDeterministicError(fnName, ctx)
		}
		if err := p.setDateTimeToCurrent(); err != nil {
			return p, false, err
		}
	} else {
		arg0 := unwrapValue(args[0])
		if arg0 == nil {
			return p, false, nil // NULL input → NULL result
		}
		switch v := arg0.(type) {
		case int64:
			p.setRawDateNumber(float64(v))
		case float64:
			p.setRawDateNumber(v)
		case int:
			p.setRawDateNumber(float64(v))
		default:
			s := toString(arg0)
			if s == "" {
				return p, false, nil
			}
			if _, err := p.parseDateOrTime(s, fnName); err != nil {
				if err == errUnrecognizedDate {
					return p, false, nil // unrecognized input → NULL
				}
				return p, false, err
			}
		}
	}
	for i := 1; i < len(args); i++ {
		z := args[i]
		if z == nil {
			return p, false, nil // NULL modifier → NULL result
		}
		zv := unwrapValue(z)
		s, isStr := zv.(string)
		if !isStr {
			// Non-text modifiers are invalid → NULL (matches SQLite: the
			// modifier must be text).
			return p, false, nil
		}
		ok, err := p.parseModifier(s, i, fnName)
		if err != nil {
			return p, false, err
		}
		if !ok {
			return p, false, nil
		}
	}
	p.computeJD()
	if p.isError || !validJulianDay(p.iJD) {
		return p, false, nil
	}
	if len(args) == 1 && p.validYMD && p.D > 28 {
		// Make sure a YYYY-MM-DD is normalized: 2023-02-31 -> 2023-03-03
		p.validYMD = false
	}
	return p, true, nil
}

// unwrapValue peels ColumnValue wrappers from a value.
func unwrapValue(v interface{}) interface{} {
	return util.UnwrapColumnValue(v)
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
		switch cf {
		case 'd':
			sb.WriteString(pad2(p.D))
		case 'e':
			s := strconv.Itoa(p.D)
			if p.D < 10 {
				s = " " + s
			}
			sb.WriteString(s)
		case 'f': // Fractional seconds SS.SSS (non-standard)
			s := p.s
			if s > 59.999 {
				s = 59.999
			}
			sb.WriteString(fmt.Sprintf("%06.3f", s))
		case 'F':
			sb.WriteString(pad4(p.Y))
			sb.WriteByte('-')
			sb.WriteString(pad2(p.M))
			sb.WriteByte('-')
			sb.WriteString(pad2(p.D))
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
		case 'j': // Day of year: Jan01==1
			sb.WriteString(pad3(daysAfterJan01(p) + 1))
		case 'J': // Julian day number (non-standard)
			sb.WriteString(fmt.Sprintf("%.16g", float64(p.iJD)/86400000.0))
		case 'm':
			sb.WriteString(pad2(p.M))
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
		case 's':
			if p.useSubsec {
				sb.WriteString(fmt.Sprintf("%.3f", float64(p.iJD-210866760000000)/1000.0))
			} else {
				iS := p.iJD/1000 - 210866760000
				sb.WriteString(strconv.FormatInt(iS, 10))
			}
		case 'S':
			sb.WriteString(pad2(int(p.s)))
		case 'T':
			sb.WriteString(pad2(p.h))
			sb.WriteByte(':')
			sb.WriteString(pad2(p.m))
			sb.WriteByte(':')
			sb.WriteString(pad2(int(p.s)))
		case 'u':
			c := daysAfterSunday(p)
			if c == 0 {
				sb.WriteByte('7')
			} else {
				sb.WriteByte(byte('0' + c))
			}
		case 'w':
			sb.WriteByte(byte('0' + daysAfterSunday(p)))
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
		case 'Y':
			sb.WriteString(pad4(p.Y))
		case '%':
			sb.WriteByte('%')
		default:
			return "", false // unrecognized → NULL
		}
	}
	return sb.String(), true
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
		Y = d1.Y - d2.Y
		if Y != 0 {
			d2.Y = d1.Y
			d2.validJD = false
			d2.computeJD()
		}
		M = d1.M - d2.M
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
		d1.iJD -= d2.iJD
		d1.iJD += 148699540800000
	} else {
		sign = '-'
		Y = d2.Y - d1.Y
		if Y != 0 {
			d2.Y = d1.Y
			d2.validJD = false
			d2.computeJD()
		}
		M = d2.M - d1.M
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
		d1.iJD = d2.iJD - d1.iJD
		d1.iJD += 148699540800000
	}
	d1.clearYMDHMSTZ()
	d1.computeYMDHMS()
	return fmt.Sprintf("%c%04d-%02d-%02d %02d:%02d:%06.3f", sign, Y, M, d1.D-1, d1.h, d1.m, d1.s), nil
}

// atofOK parses a floating point number like SQLite sqlite3AtoF.
func atofOK(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// SQLite sqlite3AtoF accepts a leading sign, digits, and an optional
	// exponent. Go's ParseFloat accepts all that; reject hex/Inf/NaN forms.
	if strings.ContainsAny(s, "xX") {
		return 0, false
	}
	if strings.EqualFold(s, "inf") || strings.EqualFold(s, "infinity") || strings.EqualFold(s, "nan") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
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
