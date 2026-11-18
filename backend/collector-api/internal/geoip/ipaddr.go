package geoip

import (
	"net/netip"
	"strings"
)

// ParseIP accepts exactly the strings Python 3.13's ipaddress.ip_address()
// accepts (IPv4 without leading zeros, IPv6 with optional "%scope" suffix and
// embedded IPv4) and returns the address with any scope dropped.
func ParseIP(s string) (netip.Addr, bool) {
	if strings.Contains(s, "/") {
		return netip.Addr{}, false
	}
	if v4, ok := parseIPv4(s); ok {
		return netip.AddrFrom4(v4), true
	}
	addr, scope, hasScope := strings.Cut(s, "%")
	if hasScope && (scope == "" || strings.Contains(scope, "%")) {
		return netip.Addr{}, false
	}
	v6, ok := parseIPv6(addr)
	if !ok {
		return netip.Addr{}, false
	}
	return netip.AddrFrom16(v6), true
}

func parseIPv4(s string) ([4]byte, bool) {
	var out [4]byte
	octets := strings.Split(s, ".")
	if s == "" || len(octets) != 4 {
		return out, false
	}
	for i, o := range octets {
		if o == "" || len(o) > 3 {
			return out, false
		}
		n := 0
		for j := 0; j < len(o); j++ {
			if o[j] < '0' || o[j] > '9' {
				return out, false
			}
			n = n*10 + int(o[j]-'0')
		}
		if o != "0" && o[0] == '0' {
			return out, false
		}
		if n > 255 {
			return out, false
		}
		out[i] = byte(n)
	}
	return out, true
}

const hextetCount = 8

func parseIPv6(s string) ([16]byte, bool) {
	var out [16]byte
	if s == "" || len([]rune(s)) > 45 {
		return out, false
	}
	maxParts := hextetCount + 1
	parts := strings.SplitN(s, ":", maxParts+1)
	if len(parts) < 3 {
		return out, false
	}
	if last := parts[len(parts)-1]; strings.Contains(last, ".") {
		v4, ok := parseIPv4(last)
		if !ok {
			return out, false
		}
		parts = parts[:len(parts)-1]
		parts = append(parts, hexWord(uint16(v4[0])<<8|uint16(v4[1])), hexWord(uint16(v4[2])<<8|uint16(v4[3])))
	}
	if len(parts) > maxParts {
		return out, false
	}
	skip := -1
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			if skip >= 0 {
				return out, false
			}
			skip = i
		}
	}
	var hi, lo, skipped int
	if skip >= 0 {
		hi = skip
		lo = len(parts) - skip - 1
		if parts[0] == "" {
			hi--
			if hi != 0 {
				return out, false
			}
		}
		if parts[len(parts)-1] == "" {
			lo--
			if lo != 0 {
				return out, false
			}
		}
		skipped = hextetCount - (hi + lo)
		if skipped < 1 {
			return out, false
		}
	} else {
		if len(parts) != hextetCount || parts[0] == "" || parts[len(parts)-1] == "" {
			return out, false
		}
		hi = len(parts)
	}
	words := make([]uint16, 0, hextetCount)
	for i := 0; i < hi; i++ {
		w, ok := parseHextet(parts[i])
		if !ok {
			return out, false
		}
		words = append(words, w)
	}
	for i := 0; i < skipped; i++ {
		words = append(words, 0)
	}
	for i := len(parts) - lo; i < len(parts); i++ {
		w, ok := parseHextet(parts[i])
		if !ok {
			return out, false
		}
		words = append(words, w)
	}
	for i, w := range words {
		out[2*i] = byte(w >> 8)
		out[2*i+1] = byte(w)
	}
	return out, true
}

func parseHextet(s string) (uint16, bool) {
	if s == "" || len(s) > 4 {
		return 0, false
	}
	var n uint16
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		n = n<<4 | uint16(d)
	}
	return n, true
}

func hexWord(w uint16) string {
	const digits = "0123456789abcdef"
	if w == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for w > 0 {
		i--
		buf[i] = digits[w&0xF]
		w >>= 4
	}
	return string(buf[i:])
}
