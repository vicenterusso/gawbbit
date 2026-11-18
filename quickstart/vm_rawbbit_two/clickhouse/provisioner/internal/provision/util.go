// Package provision is the privileged ClickHouse dataset provisioner: it
// validates the protected registries, preflights ownership and isolation,
// and idempotently reconciles managed databases, tables, roles and users.
package provision

import (
	"math/big"
	"regexp"
	"strings"
	"unicode"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

var (
	identifierRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	datasetIDRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	sha256Re       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	clickhouseCode = regexp.MustCompile(`Code:` + ws + `*(\d+)`)
	endpointPathRe = regexp.MustCompile(`^/datasets/[a-z][a-z0-9_]{0,39}/[A-Za-z0-9_/-]+$`)
	tokenLabelRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// Error is a safe-to-report configuration, ownership, or ClickHouse error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func provisionErr(msg string) error { return &Error{Msg: msg} }

func identifier(v pyjson.Value, name string) (string, error) {
	s, ok := v.(string)
	if !ok || !identifierRe.MatchString(s) {
		return "", provisionErr(name + " must be a simple ClickHouse identifier")
	}
	return s, nil
}

func datasetID(v pyjson.Value) (string, error) {
	s, ok := v.(string)
	if !ok || !datasetIDRe.MatchString(s) {
		return "", provisionErr("dataset id must be a lowercase slug using letters, digits, and underscores")
	}
	if s == "default" {
		return "", provisionErr("dataset id 'default' is reserved for the original main dataset")
	}
	return s, nil
}

func requiredString(v pyjson.Value, name string) (string, error) {
	s, ok := v.(string)
	if !ok || pyjson.Strip(s) == "" {
		return "", provisionErr(name + " is required")
	}
	return s, nil
}

func rejectUnknownFields(d *pyjson.Dict, allowed map[string]bool, name string) error {
	for _, k := range d.Keys() {
		if !allowed[k] {
			return provisionErr(name + " contains unsupported fields")
		}
	}
	return nil
}

func set(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

func quoteIdentifier(value string) (string, error) {
	if _, err := identifier(value, "ClickHouse identifier"); err != nil {
		return "", err
	}
	return "`" + value + "`", nil
}

// quoteIdentifierUnchecked quotes a name that was already validated.
func quoteIdentifierUnchecked(value string) string { return "`" + value + "`" }

func quoteString(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// pyLower mirrors str.lower().
func pyLower(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 0x130 {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// positiveInt reports isinstance(v, int) and not bool and v >= minimum.
func intInRange(v pyjson.Value, lo, hi int64) (*big.Int, bool) {
	n, ok := pyjson.IsInt(v)
	if !ok || n.Cmp(big.NewInt(lo)) < 0 || n.Cmp(big.NewInt(hi)) > 0 {
		return nil, false
	}
	return n, true
}

func positiveInt(v pyjson.Value) (*big.Int, bool) {
	n, ok := pyjson.IsInt(v)
	if !ok || n.Sign() < 1 {
		return nil, false
	}
	return n, true
}

// hasStableUUID mirrors uuid.UUID(value).int != 0.
func hasStableUUID(v pyjson.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	s = strings.ReplaceAll(s, "urn:", "")
	s = strings.ReplaceAll(s, "uuid:", "")
	s = strings.ReplaceAll(strings.Trim(s, "{}"), "-", "")
	if len(s) != 32 {
		return false
	}
	nonZero := false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			if c != '0' {
				nonZero = true
			}
		default:
			return false
		}
	}
	return nonZero
}

func validEndpointPath(path, id string) bool {
	if path != pyjson.Strip(path) || !strings.HasPrefix(path, "/datasets/"+id+"/") ||
		strings.HasSuffix(path, "/") || strings.Contains(path, "//") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return endpointPathRe.MatchString(path)
}
