package schema

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// This file ports the subset of speedate 0.17 (pydantic-core's date/time
// parser) used by a lax pydantic `datetime` field fed from JSON, with
// pydantic's settings: microsecond overflow truncated, timestamps in UTC
// (unix_timestamp_offset=0) and the seconds/milliseconds unit inferred.

var errParse = errors.New("datetime parse error")

const (
	msWatershed int64 = 20_000_000_000
	unix9999    int64 = 253_402_300_799
	unix0000    int64 = -62_167_219_200
)

// DateTime is a parsed datetime. Offset is nil for naive values.
type DateTime struct {
	Year, Month, Day     int
	Hour, Minute, Second int
	Microsecond          int
	Offset               *int // seconds east of UTC
}

// String renders the datetime like pydantic's JSON serializer.
func (d DateTime) String() string {
	var b strings.Builder
	b.Grow(32)
	pad(&b, d.Year, 4)
	b.WriteByte('-')
	pad(&b, d.Month, 2)
	b.WriteByte('-')
	pad(&b, d.Day, 2)
	b.WriteByte('T')
	pad(&b, d.Hour, 2)
	b.WriteByte(':')
	pad(&b, d.Minute, 2)
	b.WriteByte(':')
	pad(&b, d.Second, 2)
	if d.Microsecond != 0 {
		b.WriteByte('.')
		pad(&b, d.Microsecond, 6)
	}
	if d.Offset != nil {
		off := *d.Offset
		if off == 0 {
			b.WriteByte('Z')
		} else {
			if off < 0 {
				b.WriteByte('-')
			} else {
				b.WriteByte('+')
			}
			minutes := off / 60
			if minutes < 0 {
				minutes = -minutes
			}
			pad(&b, minutes/60, 2)
			b.WriteByte(':')
			pad(&b, minutes%60, 2)
		}
	}
	return b.String()
}

func pad(b *strings.Builder, n, width int) {
	s := strconv.Itoa(n)
	for i := len(s); i < width; i++ {
		b.WriteByte('0')
	}
	b.WriteString(s)
}

type date struct{ year, month, day int }

func digitAt(b []byte, i int) (int, bool) {
	if i < len(b) && b[i] >= '0' && b[i] <= '9' {
		return int(b[i] - '0'), true
	}
	return 0, false
}

func isLeap(year int) bool {
	if year%100 == 0 {
		return year%400 == 0
	}
	return year%4 == 0
}

// parseDatePartial is Date::parse_bytes_partial: a YYYY-MM-DD prefix.
func parseDatePartial(b []byte) (date, error) {
	if len(b) < 10 {
		return date{}, errParse
	}
	var d date
	for i := 0; i < 4; i++ {
		n, ok := digitAt(b, i)
		if !ok {
			return date{}, errParse
		}
		d.year = d.year*10 + n
	}
	if b[4] != '-' || b[7] != '-' {
		return date{}, errParse
	}
	m1, ok1 := digitAt(b, 5)
	m2, ok2 := digitAt(b, 6)
	d1, ok3 := digitAt(b, 8)
	d2, ok4 := digitAt(b, 9)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return date{}, errParse
	}
	d.month = m1*10 + m2
	d.day = d1*10 + d2
	var maxDays int
	switch d.month {
	case 1, 3, 5, 7, 8, 10, 12:
		maxDays = 31
	case 4, 6, 9, 11:
		maxDays = 30
	case 2:
		maxDays = 28
		if d.year%4 == 0 && (d.year%100 != 0 || d.year%400 == 0) {
			maxDays = 29
		}
	default:
		return date{}, errParse
	}
	if d.day < 1 || d.day > maxDays {
		return date{}, errParse
	}
	return d, nil
}

// parseTimeOffset is Time::parse_bytes_offset starting at offset.
func parseTimeOffset(b []byte, offset int) (DateTime, error) {
	var t DateTime
	if len(b)-offset < 5 {
		return t, errParse
	}
	h1, ok1 := digitAt(b, offset)
	h2, ok2 := digitAt(b, offset+1)
	if !ok1 || !ok2 || b[offset+2] != ':' {
		return t, errParse
	}
	mi1, ok3 := digitAt(b, offset+3)
	mi2, ok4 := digitAt(b, offset+4)
	if !ok3 || !ok4 {
		return t, errParse
	}
	t.Hour = h1*10 + h2
	t.Minute = mi1*10 + mi2
	if t.Hour > 23 || t.Minute > 59 {
		return t, errParse
	}
	length := 5
	if offset+5 < len(b) && b[offset+5] == ':' {
		s1, ok1 := digitAt(b, offset+6)
		s2, ok2 := digitAt(b, offset+7)
		if !ok1 || !ok2 {
			return t, errParse
		}
		t.Second = s1*10 + s2
		if t.Second > 59 {
			return t, errParse
		}
		length = 8
		if offset+8 < len(b) && (b[offset+8] == '.' || b[offset+8] == ',') {
			length = 9
			i := 0
			for {
				d, ok := digitAt(b, offset+length+i)
				if !ok {
					break
				}
				if i < 6 {
					t.Microsecond = t.Microsecond*10 + d
				}
				i++
			}
			if i == 0 {
				return t, errParse
			}
			for j := i; j < 6; j++ {
				t.Microsecond *= 10
			}
			length += i
		}
	}

	pos := offset + length
	if pos < len(b) {
		next := b[pos]
		pos++
		if next == 'Z' || next == 'z' {
			zero := 0
			t.Offset = &zero
		} else {
			sign := 1
			switch next {
			case '+':
			case '-':
				sign = -1
			case 0xE2: // U+2212 MINUS SIGN, encoded E2 88 92
				if pos+1 >= len(b) || b[pos] != 0x88 || b[pos+1] != 0x92 {
					return t, errParse
				}
				pos += 2
				sign = -1
			default:
				return t, errParse
			}
			th1, ok1 := digitAt(b, pos)
			th2, ok2 := digitAt(b, pos+1)
			if !ok1 || !ok2 {
				return t, errParse
			}
			var tm1 int
			switch {
			case pos+2 < len(b) && b[pos+2] == ':':
				pos += 3
				var ok bool
				if tm1, ok = digitAt(b, pos); !ok {
					return t, errParse
				}
			case pos+2 < len(b) && b[pos+2] >= '0' && b[pos+2] <= '9':
				pos += 2
				tm1 = int(b[pos] - '0')
			default:
				return t, errParse
			}
			tm2, ok := digitAt(b, pos+1)
			if !ok {
				return t, errParse
			}
			minuteSeconds := tm1*600 + tm2*60
			if minuteSeconds >= 3600 {
				return t, errParse
			}
			off := sign * (th1*36000 + th2*3600 + minuteSeconds)
			if off >= 86400 || off <= -86400 {
				return t, errParse
			}
			t.Offset = &off
			pos += 2
		}
	}
	if len(b) > pos {
		return t, errParse
	}
	return t, nil
}

// parseDateTimeRFC3339 is DateTime::parse_bytes_rfc3339_with_config.
func parseDateTimeRFC3339(b []byte) (DateTime, error) {
	d, err := parseDatePartial(b)
	if err != nil {
		return DateTime{}, err
	}
	if len(b) <= 10 {
		return DateTime{}, errParse
	}
	switch b[10] {
	case 'T', 't', ' ', '_':
	default:
		return DateTime{}, errParse
	}
	t, err := parseTimeOffset(b, 11)
	if err != nil {
		return DateTime{}, err
	}
	t.Year, t.Month, t.Day = d.year, d.month, d.day
	return t, nil
}

// intParseResult mirrors speedate's int_parse_bytes_internal outcome.
type intParseResult struct {
	value  int64
	ok     bool
	errDot bool // the first rejected byte was '.'
}

// intParse ports int_parse_bytes_internal, including its wrapping arithmetic.
func intParse(b []byte) intParseResult {
	if len(b) == 0 {
		return intParseResult{}
	}
	neg := false
	rest := b
	if len(b) >= 2 && b[0] == '-' {
		neg = true
		rest = b[1:]
	} else if len(b) >= 2 && b[0] == '+' {
		rest = b[1:]
	}
	first := rest[0]
	if first < '0' || first > '9' {
		return intParseResult{errDot: first == '.'}
	}
	value := int64(first - '0')
	for _, c := range rest[1:] {
		if c < '0' || c > '9' {
			return intParseResult{errDot: c == '.'}
		}
		value = value*10 + int64(c-'0') // wraps like Rust's wrapping_mul/add
		if value < 0 {
			return intParseResult{}
		}
	}
	if neg {
		value = -value
	}
	return intParseResult{value: value, ok: true}
}

// lexicalStandardFloat matches what lexical's STANDARD format accepts for
// the strings that reach it (an optional sign, digits around a '.', and an
// optional exponent).
var lexicalStandardFloat = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

func lexicalParseFloat(b []byte) (float64, bool) {
	if !lexicalStandardFloat.Match(b) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// saturatingInt64 mimics Rust's `f64 as i64`.
func saturatingInt64(f float64) int64 {
	switch {
	case math.IsNaN(f):
		return 0
	case f >= 9.223372036854775807e18:
		return math.MaxInt64
	case f <= -9.223372036854775808e18:
		return math.MinInt64
	}
	return int64(f)
}

// saturatingUint32 mimics Rust's `f64 as u32`.
func saturatingUint32(f float64) uint32 {
	switch {
	case math.IsNaN(f) || f <= 0:
		return 0
	case f >= math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(f)
}

// timestampWatershed infers seconds vs milliseconds.
func timestampWatershed(ts int64) (int64, uint32, error) {
	if ts == math.MinInt64 {
		return 0, 0, errParse
	}
	abs := ts
	if abs < 0 {
		abs = -abs
	}
	if abs <= msWatershed {
		return ts, 0, nil
	}
	seconds := ts / 1000
	micro := int32((ts % 1000) * 1000)
	if micro < 0 {
		seconds--
		micro += 1_000_000
	}
	return seconds, uint32(micro), nil
}

// dateFromTimestamp is Date::from_timestamp_calc.
func dateFromTimestamp(second int64) (date, int64, error) {
	if second < unix0000 || second > unix9999 {
		return date{}, 0, errParse
	}
	deltaDays := (second - unix0000) / 86_400
	deltaYears := deltaDays / 365
	ordinal := deltaDays%365 - interveningLeapYears(deltaYears) + 1
	year := int(deltaYears)
	leap := isLeap(year)
	for ordinal < 1 {
		year--
		leap = isLeap(year)
		if leap {
			ordinal += 366
		} else {
			ordinal += 365
		}
	}
	month, day := monthDay(int(ordinal), leap)
	rem := second % 86_400
	if rem < 0 {
		rem += 86_400
	}
	return date{year: year, month: month, day: day}, rem, nil
}

func interveningLeapYears(years int64) int64 {
	if years == 0 {
		return 0
	}
	return (years-1)/4 - (years-1)/100 + (years-1)/400 + 1
}

var cumulativeDays = [12]int{0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334}

func monthDay(ordinal int, leap bool) (int, int) {
	extra := 0
	if leap {
		extra = 1
	}
	for m := 11; m >= 1; m-- {
		start := cumulativeDays[m]
		if m >= 2 {
			start += extra
		}
		if ordinal > start {
			return m + 1, ordinal - start
		}
	}
	return 1, ordinal
}

// fromTimestamp is DateTime::from_timestamp_with_config with a UTC offset.
func fromTimestamp(ts int64, micro uint32) (DateTime, error) {
	second, extra, err := timestampWatershed(ts)
	if err != nil {
		return DateTime{}, err
	}
	total := uint64(micro) + uint64(extra)
	if total > math.MaxUint32 {
		return DateTime{}, errParse
	}
	if total >= 1_000_000 {
		add := int64(total / 1_000_000)
		if second > math.MaxInt64-add {
			return DateTime{}, errParse
		}
		second += add
		total %= 1_000_000
	}
	d, timeSecond, err := dateFromTimestamp(second)
	if err != nil {
		return DateTime{}, err
	}
	zero := 0
	return DateTime{
		Year: d.year, Month: d.month, Day: d.day,
		Hour: int(timeSecond / 3600), Minute: int(timeSecond % 3600 / 60), Second: int(timeSecond % 60),
		Microsecond: int(total),
		Offset:      &zero,
	}, nil
}

// fromFloat is DateTime::from_float_with_config with the inferred unit.
func fromFloat(ts float64) (DateTime, error) {
	frac := math.Abs(ts - math.Trunc(ts))
	var micro float64
	if math.Abs(ts) <= float64(msWatershed) {
		micro = frac * 1_000_000
	} else {
		micro = frac * 1_000
	}
	return fromTimestamp(saturatingInt64(math.Floor(ts)), saturatingUint32(math.Round(micro)))
}

// parseDateTimeString is DateTime::parse_bytes_with_config: RFC 3339 style
// text, falling back to a numeric unix timestamp.
func parseDateTimeString(b []byte) (DateTime, error) {
	dt, err := parseDateTimeRFC3339(b)
	if err == nil {
		return dt, nil
	}
	n := intParse(b)
	switch {
	case n.ok:
		return fromTimestamp(n.value, 0)
	case n.errDot:
		f, ok := lexicalParseFloat(b)
		if !ok {
			return DateTime{}, err
		}
		norm := f
		if math.Abs(f) > float64(msWatershed) {
			norm = f / 1000
		}
		seconds := saturatingInt64(math.Floor(norm))
		micro := saturatingUint32(math.Round((norm - float64(seconds)) * 1_000_000))
		return fromTimestamp(seconds, micro)
	}
	return DateTime{}, err
}

// parseDateString is Date::parse_bytes_with_config: YYYY-MM-DD, falling back
// to an integer timestamp that must be an exact midnight.
func parseDateString(b []byte) (date, error) {
	d, err := parseDatePartial(b)
	if err == nil {
		if len(b) > 10 {
			return date{}, errParse
		}
		return d, nil
	}
	n := intParse(b)
	if !n.ok {
		return date{}, err
	}
	seconds, micro, werr := timestampWatershed(n.value)
	if werr != nil {
		return date{}, werr
	}
	d, rem, cerr := dateFromTimestamp(seconds)
	if cerr != nil {
		return date{}, cerr
	}
	if rem != 0 || micro != 0 {
		return date{}, errParse
	}
	return d, nil
}
