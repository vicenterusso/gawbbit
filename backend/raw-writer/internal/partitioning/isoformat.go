package partitioning

import (
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
)

// This file ports CPython 3.13's C implementation of datetime.fromisoformat
// (Modules/_datetimemodule.c), which the original raw-writer used to read
// event_timestamp. It works on the UTF-8 bytes of the string followed by a NUL
// terminator, exactly like the C code, so quirks such as an embedded NUL ending
// the parse are kept.

// isoResult is a parsed datetime; offsetMicros is the UTC offset when hasTZ is set.
type isoResult struct {
	year, month, day             int
	hour, minute, second, micros int
	hasTZ                        bool
	offsetMicros                 int64
}

type cbuf []byte

// at reads like a NUL-terminated C string: bytes past the end read as NUL.
func (b cbuf) at(i int) byte {
	if i < 0 || i >= len(b) {
		return 0
	}
	return b[i]
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// parseDigits reads n ASCII digits at p; ok is false on a non-digit.
func (b cbuf) parseDigits(p int, v *int, n int) (int, bool) {
	for i := 0; i < n; i++ {
		c := b.at(p)
		p++
		if !isDigit(c) {
			return p, false
		}
		*v = *v*10 + int(c-'0')
	}
	return p, true
}

// fromISOFormat returns ok=false when fromisoformat raises ValueError.
func fromISOFormat(s string) (isoResult, bool) {
	runes := pyval.Runes(s)
	if len(runes) < 7 {
		return isoResult{}, false
	}
	// A surrogate is allowed only as the separator; CPython swaps the first one found at
	// code point 7, 8 or 10 for 'T'. Any other surrogate fails UTF-8 encoding.
	for _, pos := range []int{7, 8, 10} {
		if pos > len(runes) {
			break
		}
		if pos < len(runes) && pyval.IsSurrogate(runes[pos]) {
			runes[pos] = 'T'
			break
		}
	}
	raw := make([]byte, 0, len(s))
	for _, r := range runes {
		if pyval.IsSurrogate(r) {
			return isoResult{}, false
		}
		raw = pyval.AppendRune(raw, r)
	}
	b := cbuf(raw)
	n := len(b)

	sep, ok := findSeparator(b, n)
	if !ok {
		return isoResult{}, false
	}
	var res isoResult
	rv := parseISODate(b, sep, &res)
	if rv == 0 && n > sep {
		p := sep
		switch lead := b.at(p); {
		case lead&0x80 == 0:
			p++
		case lead&0xf0 == 0xe0:
			p += 3
		case lead&0xf0 == 0xf0:
			p += 4
		default:
			p += 2
		}
		remaining := n - p
		if remaining < 0 {
			return isoResult{}, false
		}
		rv = parseISOTime(b, p, remaining, &res)
	}
	if rv < 0 {
		return isoResult{}, false
	}
	if rv == 1 {
		res.hasTZ = true
		if res.offsetMicros <= -86_400_000_000 || res.offsetMicros >= 86_400_000_000 {
			return isoResult{}, false
		}
	}
	if !validDate(res.year, res.month, res.day) ||
		res.hour > 23 || res.minute > 59 || res.second > 59 || res.micros > 999_999 {
		return isoResult{}, false
	}
	return res, true
}

func findSeparator(b cbuf, n int) (int, bool) {
	if n == 7 {
		return 7, true
	}
	if b.at(4) == '-' {
		if b.at(5) == 'W' {
			if n < 8 {
				return 0, false
			}
			if n > 8 && b.at(8) == '-' {
				if n == 9 {
					return 0, false
				}
				if n > 10 && isDigit(b.at(10)) {
					return 8, true
				}
				return 10, true
			}
			return 8, true
		}
		return 10, true
	}
	if b.at(4) == 'W' {
		idx := 7
		for ; idx < n; idx++ {
			if !isDigit(b.at(idx)) {
				break
			}
		}
		if idx < 9 {
			return idx, true
		}
		if idx%2 == 0 {
			return 7, true
		}
		return 8, true
	}
	return 8, true
}

func parseISODate(b cbuf, length int, res *isoResult) int {
	p, ok := b.parseDigits(0, &res.year, 4)
	if !ok {
		return -1
	}
	usesSep := b.at(p) == '-'
	if usesSep {
		p++
	}
	if b.at(p) == 'W' {
		p++
		week, day := 0, 0
		if p, ok = b.parseDigits(p, &week, 2); !ok {
			return -3
		}
		if p < length {
			if usesSep {
				c := b.at(p)
				p++
				if c != '-' {
					return -2
				}
			}
			if _, ok = b.parseDigits(p, &day, 1); !ok {
				return -4
			}
		} else {
			day = 1
		}
		y, m, d, ok := isoToYMD(res.year, week, day)
		if !ok {
			return -5
		}
		res.year, res.month, res.day = y, m, d
		return 0
	}
	if p, ok = b.parseDigits(p, &res.month, 2); !ok {
		return -1
	}
	if usesSep {
		c := b.at(p)
		p++
		if c != '-' {
			return -2
		}
	}
	if _, ok = b.parseDigits(p, &res.day, 2); !ok {
		return -1
	}
	return 0
}

// parseHHMMSSFF parses [HH[:?MM[:?SS[{.,}ffffff]]]] between p and end; it returns
// a negative code on error, 1 when unparsed characters remain and 0 otherwise.
func parseHHMMSSFF(b cbuf, p, end int, h, m, s, us *int) int {
	*h, *m, *s, *us = 0, 0, 0, 0
	vals := []*int{h, m, s}
	hasSep := true
	for i := 0; i < 3; i++ {
		var ok bool
		if p, ok = b.parseDigits(p, vals[i], 2); !ok {
			return -3
		}
		c := b.at(p)
		p++
		if i == 0 {
			hasSep = c == ':'
		}
		if p >= end {
			if c != 0 {
				return 1
			}
			return 0
		} else if hasSep && c == ':' {
			continue
		} else if c == '.' || c == ',' {
			break
		} else if !hasSep {
			p--
		} else {
			return -4
		}
	}
	toParse := end - p
	if toParse >= 6 {
		toParse = 6
	}
	var ok bool
	if p, ok = b.parseDigits(p, us, toParse); !ok {
		return -3
	}
	if toParse > 0 && toParse < 6 {
		*us *= []int{100000, 10000, 1000, 100, 10}[toParse-1]
	}
	for isDigit(b.at(p)) {
		p++
	}
	if b.at(p) != 0 {
		return 1
	}
	return 0
}

func parseISOTime(b cbuf, start, length int, res *isoResult) int {
	end := start + length
	tz := start
	for {
		if c := b.at(tz); c == 'Z' || c == '+' || c == '-' {
			break
		}
		tz++
		if tz >= end {
			break
		}
	}
	rv := parseHHMMSSFF(b, start, tz, &res.hour, &res.minute, &res.second, &res.micros)
	if rv < 0 {
		return rv
	}
	if tz == end {
		if rv == 1 {
			return -5
		}
		return 0
	}
	if b.at(tz) == 'Z' {
		res.offsetMicros = 0
		if b.at(tz+1) != 0 {
			return -5
		}
		return 1
	}
	sign := int64(1)
	if b.at(tz) == '-' {
		sign = -1
	}
	var th, tm, ts, tus int
	rv = parseHHMMSSFF(b, tz+1, end, &th, &tm, &ts, &tus)
	seconds := sign * int64(th*3600+tm*60+ts)
	if seconds == 0 {
		// CPython returns UTC whenever the whole-second offset is zero, dropping the
		// fractional part.
		res.offsetMicros = 0
	} else {
		res.offsetMicros = seconds*1_000_000 + sign*int64(tus)
	}
	if rv != 0 {
		return -5
	}
	return 1
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func daysInMonth(y, m int) int {
	switch m {
	case 2:
		if isLeap(y) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

func validDate(y, m, d int) bool {
	return y >= 1 && y <= 9999 && m >= 1 && m <= 12 && d >= 1 && d <= daysInMonth(y, m)
}

// isoToYMD converts an ISO calendar date (date.fromisocalendar rules).
func isoToYMD(year, week, day int) (int, int, int, bool) {
	if year < 1 || year > 9999 {
		return 0, 0, 0, false
	}
	jan1 := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	if week <= 0 || week >= 53 {
		// Python weekday(): Monday=0; a 53-week ISO year starts on Thursday, or on
		// Wednesday in a leap year.
		first := (int(jan1.Weekday()) + 6) % 7
		if !(week == 53 && (first == 3 || (first == 2 && isLeap(year)))) {
			return 0, 0, 0, false
		}
	}
	if day <= 0 || day >= 8 {
		return 0, 0, 0, false
	}
	// Monday of ISO week 1: the week holding January 4th.
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	week1Monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
	t := week1Monday.AddDate(0, 0, (week-1)*7+day-1)
	return t.Year(), int(t.Month()), t.Day(), true
}
