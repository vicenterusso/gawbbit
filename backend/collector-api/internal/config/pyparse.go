package config

import (
	"math"
	"strconv"
	"strings"
)

// The helpers below reproduce pydantic's lax parsing of string inputs
// (environment variables) into int and bool fields.

type intResult int

const (
	intOK intResult = iota
	intParsing
	intParsingSize
)

// parseStrictInt is jiter's NumberInt::try_from: an optional '-', no leading
// zeros, digits only. Values outside int64 saturate (they are limits or
// counts, where saturation keeps the behaviour of Python's unbounded ints).
func parseStrictInt(s string) (int64, intResult) {
	if len(s) > 4300 {
		return 0, intParsingSize
	}
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || (digits[0] == '0' && len(digits) > 1) {
		return 0, intParsing
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, intParsing
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil { // out of range: saturate
		if s[0] == '-' {
			return math.MinInt64, intOK
		}
		return math.MaxInt64, intOK
	}
	return n, intOK
}

// parsePyInt is pydantic-core's str_as_int.
func parsePyInt(s string) (int64, intResult) {
	n, res := parseStrictInt(s)
	if res != intParsing {
		return n, res
	}
	cleaned, ok := cleanIntStr(s)
	if !ok {
		return 0, intParsing
	}
	n, res = parseStrictInt(cleaned)
	if res != intOK {
		return 0, intParsing
	}
	return n, intOK
}

// cleanIntStr is pydantic-core's clean_int_str.
func cleanIntStr(s string) (string, bool) {
	lenBefore := len(s)
	s = strings.TrimSpace(s)
	if suffix, ok := strings.CutPrefix(s, "+"); ok {
		if strings.HasPrefix(suffix, "-") {
			return "", false
		}
		s = suffix
	}
	negative := false
	if suffix, ok := strings.CutPrefix(s, "-"); ok {
		if strings.HasPrefix(suffix, "-") || strings.HasPrefix(suffix, "+") {
			return "", false
		}
		negative = true
		s = suffix
	}
	s, ok := stripLeadingZeros(s)
	if !ok {
		return "", false
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		decimal := s[i+1:]
		if decimal != "" && strings.Trim(decimal, "0") == "" {
			s = s[:i]
		}
	}
	sign := ""
	if negative {
		sign = "-"
	}
	if stripped, ok := stripUnderscores(s); ok {
		return sign + stripped, true
	}
	if lenBefore == len(s) {
		return "", false
	}
	return sign + s, true
}

func stripLeadingZeros(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	switch c := s[0]; {
	case c == '0':
	case (c >= '1' && c <= '9') || c == '-':
		return s, true
	default:
		return "", false
	}
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '0' || c == '_':
		case (c >= '1' && c <= '9') || c == '-':
			return s[i:], true
		case c == '.':
			return s[i-1:], true
		default:
			return "", false
		}
	}
	return s[len(s)-1:], true
}

func stripUnderscores(s string) (string, bool) {
	if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || !strings.Contains(s, "_") || strings.Contains(s, "__") {
		return "", false
	}
	return strings.ReplaceAll(s, "_", ""), true
}

// parsePyBool is pydantic-core's str_as_bool (exact match, ASCII
// case-insensitive, no trimming).
func parsePyBool(s string) (bool, bool) {
	switch asciiLower(s) {
	case "0", "f", "n", "no", "off", "false":
		return false, true
	case "1", "t", "y", "on", "yes", "true":
		return true, true
	}
	return false, false
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
