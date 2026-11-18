package pyval

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// PydanticVersionURL is the documentation URL prefix pydantic 2.12 prints.
const PydanticVersionURL = "https://errors.pydantic.dev/2.12/v/"

// LineError is one entry of a pydantic ValidationError.
type LineError struct {
	Loc     []string // empty for model-level errors
	Type    string
	Message string
	Input   Value
}

// ValidationError renders like str(pydantic_core.ValidationError).
type ValidationError struct {
	Title  string
	Errors []LineError
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	n := len(e.Errors)
	plural := "s"
	if n == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "%d validation error%s for %s", n, plural, e.Title)
	for _, le := range e.Errors {
		b.WriteByte('\n')
		if len(le.Loc) > 0 {
			b.WriteString(strings.Join(le.Loc, "."))
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "  %s [type=%s, input_value=%s, input_type=%s]\n    For further information visit %s%s",
			le.Message, le.Type, TruncatedRepr(le.Input), TypeName(le.Input), PydanticVersionURL, le.Type)
	}
	return b.String()
}

// ValidateLaxInt applies pydantic's lax int coercion.
func ValidateLaxInt(v Value) (*big.Int, *LineError) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case *big.Int:
		return t, nil
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, &LineError{Type: "finite_number", Message: "Input should be a finite number", Input: v}
		}
		if t != math.Trunc(t) {
			return nil, &LineError{Type: "int_from_float", Message: "Input should be a valid integer, got a number with a fractional part", Input: v}
		}
		if t < -9223372036854775808 || t >= 9223372036854775808 {
			return nil, &LineError{Type: "int_parsing_size", Message: "Unable to parse input string as an integer, exceeded maximum size", Input: v}
		}
		return big.NewInt(int64(t)), nil
	case string:
		return parseIntString(t)
	}
	return nil, &LineError{Type: "int_type", Message: "Input should be a valid integer", Input: v}
}

func parseIntString(s string) (*big.Int, *LineError) {
	parsingErr := &LineError{Type: "int_parsing", Message: "Input should be a valid integer, unable to parse string as an integer", Input: s}
	t := Strip(s)
	if len(t) > 4300 {
		return nil, &LineError{Type: "int_parsing_size", Message: "Unable to parse input string as an integer, exceeded maximum size", Input: s}
	}
	// A decimal point followed only by zeros is accepted ("5.0", "5.00").
	if whole, frac, ok := strings.Cut(t, "."); ok {
		if frac == "" || strings.Trim(frac, "0") != "" {
			return nil, parsingErr
		}
		t = whole
	}
	sign := ""
	if strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") {
		sign, t = t[:1], t[1:]
	}
	if t == "" || t[0] == '_' || t[len(t)-1] == '_' || strings.Contains(t, "__") {
		return nil, parsingErr
	}
	digits := strings.ReplaceAll(t, "_", "")
	for _, c := range digits {
		if c < '0' || c > '9' {
			return nil, parsingErr
		}
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, parsingErr
	}
	if sign == "-" {
		n.Neg(n)
	}
	return n, nil
}

// ValidateLaxBool applies pydantic's lax bool coercion.
func ValidateLaxBool(v Value) (bool, *LineError) {
	parsing := &LineError{Type: "bool_parsing", Message: "Input should be a valid boolean, unable to interpret input", Input: v}
	switch t := v.(type) {
	case bool:
		return t, nil
	case *big.Int:
		switch {
		case t.Sign() == 0:
			return false, nil
		case t.IsInt64() && t.Int64() == 1:
			return true, nil
		}
		return false, parsing
	case float64:
		switch t {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return false, parsing
	}
	return false, &LineError{Type: "bool_type", Message: "Input should be a valid boolean", Input: v}
}
