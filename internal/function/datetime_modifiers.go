package function

// This file is part of the faithful port of SQLite's src/date.c (version
// 3.51.0) to pure Go. It holds the parse front-end: converting the time-value
// argument (a YYYY-MM-DD HH:MM:SS string, a raw number, or 'now') and the
// modifier arguments into a dateTime value. The julian-day computation,
// formatting, and the SQL function entry points live in datetime_compute.go.
//
// Modifiers supported:
//
//	+N units, start of month/year/day, weekday N, utc, localtime, subsec,
//	unixepoch, julianday, auto, ceiling, floor, and the +/-YYYY-MM-DD
//	HH:MM:SS[.SSS] forms.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// parseDigitSpec parses one 4-character format spec from zFormat at zf,
// consuming digits from zDate at pos. Returns the parsed value, the separator
// character (0 for the last spec), and success with the position after the
// digits (before the separator).
func parseDigitSpec(zDate string, zFormat string, zf, pos int) (val int, nextC byte, ok bool, newPos int) {
	if zf+3 > len(zFormat) {
		return 0, 0, false, pos
	}
	N := int(zFormat[zf] - '0')
	min := int(zFormat[zf+1] - '0')
	maxC := zFormat[zf+2]
	nextC = 0
	if zf+3 < len(zFormat) {
		nextC = zFormat[zf+3]
	}
	// aMx[] translates the 3rd character of each format spec into a max size.
	aMx := [...]int{12, 14, 24, 31, 59, 14712}
	max := aMx[maxC-'a']
	v := 0
	for k := 0; k < N; k++ {
		if pos >= len(zDate) || !isDigit(zDate[pos]) {
			return 0, nextC, false, pos
		}
		v = v*10 + int(zDate[pos]-'0')
		pos++
	}
	if v < min || v > max || (nextC != 0 && (pos >= len(zDate) || nextC != zDate[pos])) {
		return 0, nextC, false, pos
	}
	return v, nextC, true, pos
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
	cnt := 0
	vi := 0
	pos := 0
	for zf := 0; zf+3 <= len(zFormat); zf += 4 {
		val, nextC, ok, newPos := parseDigitSpec(zDate, zFormat, zf, pos)
		if !ok {
			break
		}
		if vi < len(vals) {
			*vals[vi] = val
			vi++
		}
		pos = newPos + 1
		cnt++
		if nextC == 0 {
			break
		}
	}
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

// parseSubsecondFraction reads the ".FFFF" fraction after seconds into ms,
// returning the updated fractional-seconds value and the remaining string.
func parseSubsecondFraction(zDate string, ms float64) (float64, string) {
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
	return ms, zDate
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
		ms, zDate = parseSubsecondFraction(zDate, ms)
	} else {
		s = 0
	}
	p.validJD = false
	p.rawS = false
	p.validHMS = true
	p.h = h
	p.m = m
	p.s = float64(s) + ms
	return parseTimezone(zDate, p)
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

// lowerASCII lower-cases an ASCII byte (SQLite sqlite3UpperToLower).
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// parseModifier processes one modifier. It returns (ok, err); ok=false means
// an unrecognized modifier (which makes the whole function return NULL), err
// is non-nil only for real errors (localtime failure, non-determinism).
func (p *dateTime) parseModifier(z string, idx int, fnName string) (bool, error) {
	if len(z) == 0 {
		return false, nil
	}
	switch lowerASCII(z[0]) {
	case 'a':
		return p.modAuto(z, idx)
	case 'c':
		return p.modCeiling(z), nil
	case 'f':
		return p.modFloor(z), nil
	case 'j':
		return p.modJulianDay(z, idx), nil
	case 'l':
		return p.modLocaltime(z, fnName)
	case 'u':
		return p.modUnixEpochUtc(z, idx, fnName)
	case 'w':
		return p.parseWeekday(z), nil
	case 's':
		return p.parseStartOf(z), nil
	case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.parseModifierNum(z, fnName)
	}
	return false, nil
}

// modAuto handles the "auto" modifier (date.c case 'a').
func (p *dateTime) modAuto(z string, idx int) (bool, error) {
	if !strings.EqualFold(z, "auto") {
		return false, nil
	}
	if idx > 1 {
		return false, nil // IMP: R-33611-57934
	}
	p.autoAdjustDate()
	return true, nil
}

// modCeiling handles the "ceiling" modifier (date.c case 'c').
func (p *dateTime) modCeiling(z string) bool {
	if !strings.EqualFold(z, "ceiling") {
		return false
	}
	p.computeJD()
	p.clearYMDHMSTZ()
	p.nFloor = 0
	return true
}

// modFloor handles the "floor" modifier (date.c case 'f').
func (p *dateTime) modFloor(z string) bool {
	if !strings.EqualFold(z, "floor") {
		return false
	}
	p.computeJD()
	p.iJD -= int64(p.nFloor) * 86400000
	p.clearYMDHMSTZ()
	return true
}

// modJulianDay handles the "julianday" modifier (date.c case 'j').
func (p *dateTime) modJulianDay(z string, idx int) bool {
	if !strings.EqualFold(z, "julianday") {
		return false
	}
	if idx > 1 {
		return false // IMP: R-31176-64601
	}
	if p.validJD && p.rawS {
		p.rawS = false
		return true
	}
	return false
}

// modLocaltime handles the "localtime" modifier (date.c case 'l').
func (p *dateTime) modLocaltime(z, fnName string) (bool, error) {
	if !strings.EqualFold(z, "localtime") {
		return false, nil
	}
	if ctx := PureContext(); ctx != "" {
		return false, nonDeterministicError(fnName, ctx)
	}
	if !p.isLocal {
		if err := p.toLocaltime(); err != nil {
			return false, err
		}
	}
	p.isUtc = false
	p.isLocal = true
	return true, nil
}

// modUnixEpochUtc handles the "unixepoch" and "utc" modifiers (date.c
// case 'u').
func (p *dateTime) modUnixEpochUtc(z string, idx int, fnName string) (bool, error) {
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
			return true, nil
		}
		return false, nil
	}
	if !strings.EqualFold(z, "utc") {
		return false, nil
	}
	if ctx := PureContext(); ctx != "" {
		return false, nonDeterministicError(fnName, ctx)
	}
	if !p.isUtc {
		if err := p.toUtc(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// parseWeekday handles the "weekday N" modifier (date.c case 'w').
func (p *dateTime) parseWeekday(z string) bool {
	if !strings.HasPrefix(strings.ToLower(z), "weekday ") {
		return false
	}
	rest := z[8:]
	f, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return false
	}
	if f < 0.0 || f >= 7.0 || float64(int(f)) != f {
		return false
	}
	p.computeYMDHMS()
	p.tz = 0
	p.validJD = false
	p.computeJD()
	n := int(f)
	Z := ((p.iJD + 129600000) / 86400000) % 7
	if Z > int64(n) {
		Z -= 7
	}
	p.iJD += (int64(n) - Z) * 86400000
	p.clearYMDHMSTZ()
	return true
}

// parseStartOf handles the "start of TTTTT", "subsec", and "subsecond"
// modifiers (date.c case 's').
func (p *dateTime) parseStartOf(z string) bool {
	if !strings.HasPrefix(strings.ToLower(z), "start of ") {
		if strings.EqualFold(z, "subsec") || strings.EqualFold(z, "subsecond") {
			p.useSubsec = true
			return true
		}
		return false
	}
	if !p.validJD && !p.validYMD && !p.validHMS {
		return false
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
		return true
	}
	if strings.EqualFold(rest, "year") {
		p.M = 1
		p.D = 1
		return true
	}
	if strings.EqualFold(rest, "day") {
		return true
	}
	return false
}

// scanModifierNumber returns the length of the leading numeric token in z for
// a +/-NNN modifier, stopping at ':', whitespace, or a '-' that begins a
// YYYY-MM-DD modifier. Y receives the year digits when a date form is
// detected.
func scanModifierNumber(z string) (n int, Y int) {
	n = 1
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
	return n, Y
}

// parseModifierNum handles the +/-NNN units, +/-YYYY-MM-DD, and +/-HH:MM:SS
// modifier forms (date.c case '+': in parseModifier).
func (p *dateTime) parseModifierNum(z string, fnName string) (bool, error) {
	z0 := z[0]
	n, _ := scanModifierNumber(z)
	r, err := strconv.ParseFloat(z[:n], 64)
	if err != nil {
		return false, nil
	}
	if n < len(z) && z[n] == '-' {
		z2, n2, done, ok := p.addDateModifier(z, z0, n)
		if !ok {
			return false, nil
		}
		if done {
			return true, nil
		}
		z, n = z2, n2
	}
	if n < len(z) && z[n] == ':' {
		return p.addTimeModifier(z, z0)
	}
	return p.addUnitsModifier(z[n:], r), nil
}

// parseDateModifierDigits extracts the Y/M/D digits for a (+|-)YYYY-MM-DD
// modifier whose numeric prefix length is n. Returns ok=false on failure.
func parseDateModifierDigits(z string, n int) (Y, M, D int, ok bool) {
	if n == 5 {
		if getDigits(z[1:], "40f-20a-20d", &Y, &M, &D) != 3 {
			return 0, 0, 0, false
		}
		return Y, M, D, true
	}
	if n != 6 {
		return 0, 0, 0, false
	}
	if getDigits(z[1:], "50f-20a-20d", &Y, &M, &D) != 3 {
		return 0, 0, 0, false
	}
	return Y, M, D, true
}

// addDateModifier applies the (+|-)YYYY-MM-DD [HH:MM] modifier to p. It
// returns the (possibly re-based) remainder z and its numeric length n when a
// trailing HH:MM follows (done=false), or reports done when the modifier is
// complete. ok=false means the modifier is invalid.
func (p *dateTime) addDateModifier(z string, z0 byte, n int) (string, int, bool, bool) {
	if z0 != '+' && z0 != '-' {
		return "", 0, false, false
	}
	Y, M, D, ok := parseDateModifierDigits(z, n)
	if !ok {
		return "", 0, false, false
	}
	if n == 6 {
		z = z[1:]
	}
	if M >= 12 || D >= 31 {
		return "", 0, false, false // M range 0..11, D range 0..30
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
	p.normalizeMonthYear()
	p.computeFloor()
	p.computeJD()
	p.validHMS = false
	p.validYMD = false
	p.iJD += int64(D) * 86400000
	if n+11 >= len(z) {
		return "", 0, true, true
	}
	var h, m int
	if isSpace(z[11]) && getDigits(z[12:], "20c:20e", &h, &m) == 2 {
		return z[12:], 2, false, true
	}
	return "", 0, false, false
}

// normalizeMonthYear carries overflowing month values into years, mirroring
// the month normalization in date.c.
func (p *dateTime) normalizeMonthYear() {
	x := (p.M - 1) / 12
	if p.M <= 0 {
		x = (p.M - 12) / 12
	}
	p.Y += x
	p.M -= x * 12
}

// addMonths applies the fractional month part of an 'NNN months' modifier,
// returning the residual fractional months that become day adjustments.
func (p *dateTime) addMonths(r float64) float64 {
	p.computeYMDHMS()
	p.M += int(r)
	p.normalizeMonthYear()
	p.computeFloor()
	p.validJD = false
	return r - float64(int(r))
}

// addYears applies the whole-year part of an 'NNN years' modifier, returning
// the residual fractional years that become day adjustments.
func (p *dateTime) addYears(r float64) float64 {
	y := int(r)
	p.computeYMDHMS()
	p.Y += y
	p.computeFloor()
	p.validJD = false
	return r - float64(int(r))
}

// addTimeModifier applies the (+|-)HH:MM:SS[.FFF] modifier to p.
func (p *dateTime) addTimeModifier(z string, z0 byte) (bool, error) {
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

// xformMatches reports whether the aXformType entry xf matches the unit name
// z[:n] with the numeric value r within range.
func xformMatches(xf *xformType, n int, z string, r float64) bool {
	return xf.nName == n && strings.EqualFold(xf.zName, z[:n]) && r > -xf.rLimit && r < xf.rLimit
}

// applyXform applies the aXformType[] transform for entry i: the month/year
// special processing plus the iJD adjustment.
func (p *dateTime) applyXform(xf *xformType, i int, r, rRounder float64) {
	switch i {
	case 4: // Special processing to add months
		r = p.addMonths(r)
	case 5: // Special processing to add years
		r = p.addYears(r)
	}
	p.computeJD()
	p.iJD += int64(r*1000.0*xf.rXform + rRounder)
}

// addUnitsModifier applies the 'NNN units' transform (date.c aXformType[]).
func (p *dateTime) addUnitsModifier(z string, r float64) bool {
	for len(z) > 0 && isSpace(z[0]) {
		z = z[1:]
	}
	n := len(z)
	if n < 3 || n > 10 {
		return false
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
	ok := false
	for i := range aXformType {
		xf := &aXformType[i]
		if xformMatches(xf, n, z, r) {
			p.applyXform(xf, i, r, rRounder)
			ok = true
			break
		}
	}
	p.clearYMDHMSTZ()
	return ok
}

// parseDateValue parses the time-value argument (args[0], or the current time
// when no argument is given) into p. Returns ok=false for NULL results.
func parseDateValue(fnName string, args []interface{}, p *dateTime) (bool, error) {
	if len(args) == 0 {
		if ctx := PureContext(); ctx != "" {
			return false, nonDeterministicError(fnName, ctx)
		}
		if err := p.setDateTimeToCurrent(); err != nil {
			return false, err
		}
		return true, nil
	}
	arg0 := unwrapValue(args[0])
	if arg0 == nil {
		return false, nil // NULL input → NULL result
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
			return false, nil
		}
		if _, err := p.parseDateOrTime(s, fnName); err != nil {
			if err == errUnrecognizedDate {
				return false, nil // unrecognized input → NULL
			}
			return false, err
		}
	}
	return true, nil
}

// applyModifiers applies the modifier arguments argv[1:] to p. Returns
// ok=false (NULL result) for NULL or non-text modifiers.
func applyModifiers(p *dateTime, args []interface{}, fnName string) (bool, error) {
	for i := 1; i < len(args); i++ {
		z := args[i]
		if z == nil {
			return false, nil // NULL modifier → NULL result
		}
		zv := unwrapValue(z)
		s, isStr := zv.(string)
		if !isStr {
			// Non-text modifiers are invalid → NULL (matches SQLite: the
			// modifier must be text).
			return false, nil
		}
		ok, err := p.parseModifier(s, i, fnName)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// isDate processes the function arguments: argv[0] is a date-time stamp,
// argv[1:] are modifiers. Returns the parsed DateTime, an ok flag, and an
// error (nil error + ok=false means the result is NULL).
func isDate(fnName string, args []interface{}) (dateTime, bool, error) {
	var p dateTime
	ok, err := parseDateValue(fnName, args, &p)
	if err != nil || !ok {
		return p, ok, err
	}
	ok, err = applyModifiers(&p, args, fnName)
	if err != nil || !ok {
		return p, ok, err
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
