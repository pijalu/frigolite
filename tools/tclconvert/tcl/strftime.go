// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// commandStrftime implements the testfixture `strftime FORMAT UNIXTIMESTAMP`
// command (test1.c strftime_cmd): gmtime() plus the C library's strftime().
// date4.test compares SQLite's strftime() against this C-library reference for
// 24,859 timestamps; without the command the converter produced empty
// expectations (`{}` -> NULL) for every one of them.
func commandStrftime(i *Interp, rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) != 2 {
		return fmt.Errorf("wrong # args: should be \"strftime FORMAT UNIXTIMESTAMP\"")
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
	if err != nil {
		return fmt.Errorf("expected integer but got %q", args[1])
	}
	i.vars[""] = cStrftime(args[0], time.Unix(ts, 0).UTC())
	return nil
}

// cStrftime renders the strftime() conversions the corpus uses, following
// POSIX/C-library semantics on a UTC broken-down time. Unknown conversions are
// copied through verbatim, as glibc does.
func cStrftime(format string, t time.Time) string {
	var b strings.Builder
	for pos := 0; pos < len(format); pos++ {
		if format[pos] != '%' || pos+1 >= len(format) {
			b.WriteByte(format[pos])
			continue
		}
		pos++
		switch format[pos] {
		case '%':
			b.WriteByte('%')
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'c':
			b.WriteString(t.Format("Mon Jan  2 15:04:05 2006"))
		case 'C':
			fmt.Fprintf(&b, "%02d", t.Year()/100)
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'D':
			b.WriteString(t.Format("01/02/06"))
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'G':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%04d", y)
		case 'g':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", y%100)
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&b, "%02d", h)
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'k':
			fmt.Fprintf(&b, "%2d", t.Hour())
		case 'l':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&b, "%2d", h)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'n':
			b.WriteByte('\n')
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'P':
			if t.Hour() < 12 {
				b.WriteString("am")
			} else {
				b.WriteString("pm")
			}
		case 'r':
			b.WriteString(t.Format("03:04:05 PM"))
		case 'R':
			b.WriteString(t.Format("15:04"))
		case 's':
			fmt.Fprintf(&b, "%d", t.Unix())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 't':
			b.WriteByte('\t')
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'u':
			wd := int(t.Weekday())
			if wd == 0 {
				wd = 7
			}
			fmt.Fprintf(&b, "%d", wd)
		case 'U':
			fmt.Fprintf(&b, "%02d", (t.YearDay()+6-int(t.Weekday()))/7)
		case 'V':
			_, w := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", w)
		case 'w':
			fmt.Fprintf(&b, "%d", int(t.Weekday()))
		case 'W':
			wd := (int(t.Weekday()) + 6) % 7
			fmt.Fprintf(&b, "%02d", (t.YearDay()+6-wd)/7)
		case 'x':
			b.WriteString(t.Format("01/02/06"))
		case 'X':
			b.WriteString(t.Format("15:04:05"))
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'Y':
			fmt.Fprintf(&b, "%d", t.Year())
		case 'z':
			b.WriteString("+0000")
		case 'Z':
			b.WriteString("GMT")
		default:
			b.WriteByte('%')
			b.WriteByte(format[pos])
		}
	}
	return b.String()
}
