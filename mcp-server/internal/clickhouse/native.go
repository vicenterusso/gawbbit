package clickhouse

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
)

// timeContext decides how DateTime values become Python datetimes.
type timeContext struct {
	// defaultTZ applies to DateTime columns without an explicit timezone;
	// nil means naive UTC.
	defaultTZ *time.Location
}

// location returns nil for naive (UTC) datetimes, like clickhouse-connect.
func (c timeContext) location(t *chType) (*time.Location, error) {
	if !t.hasTZ {
		return c.defaultTZ, nil
	}
	if strings.EqualFold(t.tz, "UTC") {
		return nil, nil
	}
	loc, err := time.LoadLocation(t.tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %s", t.tz)
	}
	return loc, nil
}

type nativeReader struct {
	r   *bufio.Reader
	ctx timeContext
}

var errTruncated = errors.New("ClickHouse returned a truncated Native response")

func (nr *nativeReader) bytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(nr.r, buf); err != nil {
		return nil, errTruncated
	}
	return buf, nil
}

func (nr *nativeReader) uvarint() (uint64, error) {
	v, err := binary.ReadUvarint(nr.r)
	if err != nil {
		return 0, errTruncated
	}
	return v, nil
}

func (nr *nativeReader) str() ([]byte, error) {
	n, err := nr.uvarint()
	if err != nil {
		return nil, err
	}
	if n > 1<<31 {
		return nil, errTruncated
	}
	return nr.bytes(int(n))
}

func (nr *nativeReader) u64() (uint64, error) {
	b, err := nr.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// decodeNative reads every block of a Native response into named rows.
func decodeNative(body io.Reader, ctx timeContext) ([]*pyval.Dict, error) {
	nr := &nativeReader{r: bufio.NewReaderSize(body, 64<<10), ctx: ctx}
	var rows []*pyval.Dict
	for {
		if _, err := nr.r.Peek(1); err == io.EOF {
			return rows, nil
		}
		// client_protocol_version=54405 adds an 8-byte BlockInfo header.
		if _, err := nr.bytes(8); err != nil {
			return nil, err
		}
		numCols, err := nr.uvarint()
		if err != nil {
			return nil, err
		}
		numRows, err := nr.uvarint()
		if err != nil {
			return nil, err
		}
		names := make([]string, numCols)
		columns := make([][]pyval.Value, numCols)
		for c := range names {
			name, err := nr.str()
			if err != nil {
				return nil, err
			}
			typeName, err := nr.str()
			if err != nil {
				return nil, err
			}
			names[c] = decodeName(name)
			t, err := parseType(string(typeName))
			if err != nil {
				return nil, err
			}
			if numRows == 0 {
				continue
			}
			if err := nr.prefix(t); err != nil {
				return nil, err
			}
			if columns[c], err = nr.column(t, int(numRows)); err != nil {
				return nil, err
			}
		}
		for r := 0; r < int(numRows); r++ {
			row := pyval.NewDict(len(names))
			for c, name := range names {
				row.Set(name, columns[c][r])
			}
			rows = append(rows, row)
		}
	}
}

func decodeName(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return hex.EncodeToString(b)
}

// prefix consumes serialization state prefixes (LowCardinality versions).
func (nr *nativeReader) prefix(t *chType) error {
	switch t.base {
	case "LowCardinality":
		_, err := nr.u64()
		return err
	case "Array", "Nullable", "Map", "Tuple", "Nested":
		for _, inner := range t.inner {
			if err := nr.prefix(inner); err != nil {
				return err
			}
		}
	}
	return nil
}

func (nr *nativeReader) column(t *chType, n int) ([]pyval.Value, error) {
	switch t.base {
	case "Nullable":
		mask, err := nr.bytes(n)
		if err != nil {
			return nil, err
		}
		values, err := nr.column(t.inner[0], n)
		if err != nil {
			return nil, err
		}
		for i, isNull := range mask {
			if isNull != 0 {
				values[i] = nil
			}
		}
		return values, nil
	case "LowCardinality":
		return nr.lowCardinality(t, n)
	case "Array", "Nested", "Map":
		return nr.arrayLike(t, n)
	case "Tuple":
		elems := make([][]pyval.Value, len(t.inner))
		for i, inner := range t.inner {
			var err error
			if elems[i], err = nr.column(inner, n); err != nil {
				return nil, err
			}
		}
		named := len(t.names) > 0 && t.names[0] != ""
		out := make([]pyval.Value, n)
		for r := range out {
			if named {
				d := pyval.NewDict(len(elems))
				for i := range elems {
					d.Set(t.names[i], elems[i][r])
				}
				out[r] = d
				continue
			}
			tuple := make(pyval.Tuple, len(elems))
			for i := range elems {
				tuple[i] = elems[i][r]
			}
			out[r] = tuple
		}
		return out, nil
	}
	return nr.scalar(t, n)
}

func (nr *nativeReader) arrayLike(t *chType, n int) ([]pyval.Value, error) {
	offsets := make([]uint64, n)
	for i := range offsets {
		v, err := nr.u64()
		if err != nil {
			return nil, err
		}
		offsets[i] = v
	}
	total := 0
	if n > 0 {
		total = int(offsets[n-1])
	}
	var items, keys []pyval.Value
	var err error
	switch t.base {
	case "Map":
		if keys, err = nr.column(t.inner[0], total); err != nil {
			return nil, err
		}
		if items, err = nr.column(t.inner[1], total); err != nil {
			return nil, err
		}
	case "Nested":
		tuple := &chType{full: "Tuple", base: "Tuple", inner: t.inner, names: t.names}
		if items, err = nr.column(tuple, total); err != nil {
			return nil, err
		}
	default:
		if items, err = nr.column(t.inner[0], total); err != nil {
			return nil, err
		}
	}
	out := make([]pyval.Value, n)
	start := 0
	for i, end := range offsets {
		if int(end) < start || int(end) > total {
			return nil, errTruncated
		}
		if t.base == "Map" {
			d := pyval.NewDict(int(end) - start)
			for j := start; j < int(end); j++ {
				setAnyKey(d, keys[j], items[j])
			}
			out[i] = d
		} else {
			out[i] = pyval.List(append([]pyval.Value(nil), items[start:end]...))
		}
		start = int(end)
	}
	return out, nil
}

// setAnyKey inserts with Python dict semantics for scalar keys.
func setAnyKey(d *pyval.Dict, key, value pyval.Value) {
	id := pyval.Repr(key)
	for i, k := range d.Keys {
		if pyval.Repr(k) == id {
			d.Values[i] = value
			return
		}
	}
	d.Keys = append(d.Keys, key)
	d.Values = append(d.Values, value)
}

func (nr *nativeReader) lowCardinality(t *chType, n int) ([]pyval.Value, error) {
	const (
		needUpdateDictionary = 1 << 10
		indexTypeMask        = 0xff
	)
	inner := t.inner[0]
	nullable := inner.base == "Nullable"
	keyType := inner
	if nullable {
		keyType = inner.inner[0]
	}
	out := make([]pyval.Value, 0, n)
	var dict []pyval.Value
	for len(out) < n {
		flags, err := nr.u64()
		if err != nil {
			return nil, err
		}
		if flags&needUpdateDictionary != 0 {
			count, err := nr.u64()
			if err != nil {
				return nil, err
			}
			if dict, err = nr.column(keyType, int(count)); err != nil {
				return nil, err
			}
		}
		count, err := nr.u64()
		if err != nil {
			return nil, err
		}
		width := 1 << (flags & indexTypeMask)
		raw, err := nr.bytes(int(count) * width)
		if err != nil {
			return nil, err
		}
		for i := 0; i < int(count); i++ {
			var idx uint64
			switch width {
			case 1:
				idx = uint64(raw[i])
			case 2:
				idx = uint64(binary.LittleEndian.Uint16(raw[i*2:]))
			case 4:
				idx = uint64(binary.LittleEndian.Uint32(raw[i*4:]))
			default:
				idx = binary.LittleEndian.Uint64(raw[i*8:])
			}
			if idx >= uint64(len(dict)) {
				return nil, errTruncated
			}
			if nullable && idx == 0 {
				out = append(out, nil)
			} else {
				out = append(out, dict[idx])
			}
		}
	}
	return out, nil
}

func fixedWidth(base string) int {
	switch base {
	case "Int8", "UInt8", "Bool", "Nothing":
		return 1
	case "Int16", "UInt16", "Date", "Enum16":
		return 2
	case "Int32", "UInt32", "Float32", "Date32", "DateTime", "IPv4":
		return 4
	case "Int64", "UInt64", "Float64", "DateTime64":
		return 8
	case "Int128", "UInt128", "UUID", "IPv6":
		return 16
	case "Int256", "UInt256":
		return 32
	case "Enum8":
		return 1
	}
	if strings.HasPrefix(base, "Interval") {
		return 8
	}
	return 0
}

func (nr *nativeReader) scalar(t *chType, n int) ([]pyval.Value, error) {
	out := make([]pyval.Value, n)
	switch t.base {
	case "String":
		for i := range out {
			b, err := nr.str()
			if err != nil {
				return nil, err
			}
			if utf8.Valid(b) {
				out[i] = string(b)
			} else {
				out[i] = hex.EncodeToString(b)
			}
		}
		return out, nil
	case "FixedString":
		for i := range out {
			b, err := nr.bytes(t.size)
			if err != nil {
				return nil, err
			}
			out[i] = pyval.Bytes(b)
		}
		return out, nil
	}
	width := fixedWidth(t.base)
	if t.base == "Decimal" || strings.HasPrefix(t.base, "Decimal") {
		width = t.size
	}
	if width == 0 {
		return nil, &unrecognizedTypeError{t.base, t.full}
	}
	raw, err := nr.bytes(width * n)
	if err != nil {
		return nil, err
	}
	var loc *time.Location
	if t.base == "DateTime" || t.base == "DateTime64" {
		if loc, err = nr.ctx.location(t); err != nil {
			return nil, err
		}
	}
	for i := range out {
		b := raw[i*width : (i+1)*width]
		if out[i], err = scalarValue(t, b, loc); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scalarValue(t *chType, b []byte, loc *time.Location) (pyval.Value, error) {
	le := binary.LittleEndian
	switch t.base {
	case "Int8":
		return big.NewInt(int64(int8(b[0]))), nil
	case "UInt8":
		return big.NewInt(int64(b[0])), nil
	case "Bool":
		return b[0] != 0, nil
	case "Nothing":
		return nil, nil
	case "Int16":
		return big.NewInt(int64(int16(le.Uint16(b)))), nil
	case "UInt16":
		return big.NewInt(int64(le.Uint16(b))), nil
	case "Int32":
		return big.NewInt(int64(int32(le.Uint32(b)))), nil
	case "UInt32":
		return big.NewInt(int64(le.Uint32(b))), nil
	case "Int64":
		return big.NewInt(int64(le.Uint64(b))), nil
	case "UInt64":
		return new(big.Int).SetUint64(le.Uint64(b)), nil
	case "Int128", "Int256":
		return signedLE(b), nil
	case "UInt128", "UInt256":
		return unsignedLE(b), nil
	case "Float32":
		return float64(math.Float32frombits(le.Uint32(b))), nil
	case "Float64":
		return math.Float64frombits(le.Uint64(b)), nil
	case "Date":
		return time.Unix(int64(le.Uint16(b))*86400, 0).UTC().Format("2006-01-02"), nil
	case "Date32":
		return time.Unix(int64(int32(le.Uint32(b)))*86400, 0).UTC().Format("2006-01-02"), nil
	case "DateTime":
		return pyDatetime(int64(le.Uint32(b)), 0, loc), nil
	case "DateTime64":
		ticks := int64(le.Uint64(b))
		prec := int64(math.Pow10(t.scale))
		seconds := floorDiv(ticks, prec)
		micros := floorDiv((ticks-seconds*prec)*1_000_000, prec)
		return pyDatetime(seconds, micros, loc), nil
	case "UUID":
		hi, lo := le.Uint64(b[:8]), le.Uint64(b[8:])
		s := fmt.Sprintf("%016x%016x", hi, lo)
		return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
	case "IPv4":
		v := le.Uint32(b)
		return fmt.Sprintf("%d.%d.%d.%d", v>>24, (v>>16)&0xff, (v>>8)&0xff, v&0xff), nil
	case "IPv6":
		return netip.AddrFrom16([16]byte(b)).String(), nil
	case "Enum8", "Enum16":
		var v int64
		if t.base == "Enum8" {
			v = int64(int8(b[0]))
		} else {
			v = int64(int16(le.Uint16(b)))
		}
		if name, ok := t.enumVals[v]; ok {
			return name, nil
		}
		return big.NewInt(v), nil
	}
	if strings.HasPrefix(t.base, "Decimal") {
		return pyval.Decimal(decimalString(signedLE(b), t.scale)), nil
	}
	if strings.HasPrefix(t.base, "Interval") {
		return big.NewInt(int64(le.Uint64(b))), nil
	}
	return nil, &unrecognizedTypeError{t.base, t.full}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func unsignedLE(b []byte) *big.Int {
	be := make([]byte, len(b))
	for i := range b {
		be[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(be)
}

func signedLE(b []byte) *big.Int {
	v := unsignedLE(b)
	if b[len(b)-1]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), uint(len(b)*8)))
	}
	return v
}

// pyDatetime renders datetime.isoformat() for a UTC epoch second count.
func pyDatetime(seconds, micros int64, loc *time.Location) string {
	utc := time.Unix(seconds, 0).UTC()
	offset := 0
	if loc != nil {
		_, raw := utc.In(loc).Zone()
		// pytz rounds historical (LMT) offsets to whole minutes.
		offset = int(floorDiv(int64(raw)+30, 60) * 60)
	}
	s := utc.Add(time.Duration(offset) * time.Second).Format("2006-01-02T15:04:05")
	if micros != 0 {
		s += fmt.Sprintf(".%06d", micros)
	}
	if loc == nil {
		return s
	}
	// pydantic serializes a zero UTC offset as "Z".
	if offset == 0 {
		return s + "Z"
	}
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	return s + fmt.Sprintf("%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
}

// decimalString renders decimal.Decimal(unscaled * 10**-scale) with str().
func decimalString(unscaled *big.Int, scale int) string {
	sign := ""
	if unscaled.Sign() < 0 {
		sign = "-"
	}
	digits := new(big.Int).Abs(unscaled).String()
	exp := -scale
	leftDigits := exp + len(digits)
	var dotPlace int
	if exp <= 0 && leftDigits > -6 {
		dotPlace = leftDigits
	} else {
		dotPlace = 1
	}
	var intPart, fracPart string
	switch {
	case dotPlace <= 0:
		intPart = "0"
		fracPart = "." + strings.Repeat("0", -dotPlace) + digits
	case dotPlace >= len(digits):
		intPart = digits + strings.Repeat("0", dotPlace-len(digits))
	default:
		intPart = digits[:dotPlace]
		fracPart = "." + digits[dotPlace:]
	}
	expPart := ""
	if leftDigits != dotPlace {
		expPart = fmt.Sprintf("E%+d", leftDigits-dotPlace)
	}
	return sign + intPart + fracPart + expPart
}
