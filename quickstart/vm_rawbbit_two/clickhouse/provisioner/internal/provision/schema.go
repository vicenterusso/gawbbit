package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strings"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

// ws matches what Python's Unicode-aware \s matches.
const ws = `[\t\n\v\f\r \x1c-\x1f\x85\p{Z}]`

var (
	canonicalTableRe = regexp.MustCompile(`(?is)CREATE` + ws + `+TABLE` + ws + `+IF` + ws + `+NOT` + ws + `+EXISTS` + ws +
		`+analytics\.events` + ws + `*\((.*?)\)` + ws + `*ENGINE` + ws + `*=` + ws + `*([A-Za-z0-9_]+)` + ws +
		`*PARTITION` + ws + `+BY` + ws + `*(.*?)` + ws + `*ORDER` + ws + `+BY` + ws + `*(.*?);`)
	createPrefixRe = regexp.MustCompile(`(?i)CREATE` + ws + `+TABLE` + ws + `+IF` + ws + `+NOT` + ws + `+EXISTS` + ws + `+analytics\.events`)
	columnRe       = regexp.MustCompile(`(?s)^` + ws + "*`?([A-Za-z_][A-Za-z0-9_]*)`?" + ws + `+(.+?)` + ws + `*$`)
	wsRunRe        = regexp.MustCompile(ws + `+`)
)

// Column is one (name, type) pair of the canonical events table.
type Column struct {
	Name string
	Type string
}

// CanonicalSchema is the parsed canonical analytics.events DDL.
type CanonicalSchema struct {
	CreateTableSQL string
	Columns        []Column
	Engine         string
	PartitionKey   string
	SortingKey     string
	Fingerprint    string
}

func splitTopLevel(value string) ([]string, error) {
	var parts []string
	runes := []rune(value)
	start, depth := 0, 0
	var quote rune
	escaped := false
	for i, c := range runes {
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, provisionErr("canonical events schema has invalid parentheses")
			}
		case ',':
			if depth == 0 {
				parts = append(parts, pyjson.Strip(string(runes[start:i])))
				start = i + 1
			}
		}
	}
	if depth != 0 || quote != 0 {
		return nil, provisionErr("canonical events schema has invalid SQL structure")
	}
	if tail := pyjson.Strip(string(runes[start:])); tail != "" {
		parts = append(parts, tail)
	}
	return parts, nil
}

// normalizeExpression mirrors _normalize_expression.
func normalizeExpression(value string) string {
	normalized := strings.TrimRight(pyLower(wsRunRe.ReplaceAllString(value, "")), ";")
	if strings.HasPrefix(normalized, "(") && strings.HasSuffix(normalized, ")") {
		depth := 0
		wraps := true
		runes := []rune(normalized)
		for i, c := range runes {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && i != len(runes)-1 {
					wraps = false
				}
			}
			if !wraps {
				break
			}
		}
		if wraps && depth == 0 {
			normalized = string(runes[1 : len(runes)-1])
		}
	}
	return normalized
}

// ParseCanonicalSchema mirrors parse_canonical_schema.
func ParseCanonicalSchema(path string) (*CanonicalSchema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, provisionErr("canonical events schema file is missing or unreadable")
	}
	source := string(data)
	loc := canonicalTableRe.FindStringSubmatchIndex(source)
	if loc == nil {
		return nil, provisionErr("canonical events schema does not contain the expected analytics.events MergeTree DDL")
	}
	group := func(i int) string { return source[loc[2*i]:loc[2*i+1]] }
	columnBlock, engine, partitionKey, sortingKey := group(1), group(2), group(3), group(4)
	definitions, err := splitTopLevel(columnBlock)
	if err != nil {
		return nil, err
	}
	var columns []Column
	seen := map[string]bool{}
	for _, def := range definitions {
		m := columnRe.FindStringSubmatch(def)
		if m == nil {
			return nil, provisionErr("canonical events schema contains an unsupported column declaration")
		}
		columns = append(columns, Column{Name: m[1], Type: wsRunRe.ReplaceAllString(pyjson.Strip(m[2]), " ")})
		seen[m[1]] = true
	}
	if len(columns) == 0 || len(seen) != len(columns) {
		return nil, provisionErr("canonical events schema must contain unique columns")
	}
	if pyLower(engine) != "mergetree" {
		return nil, provisionErr("canonical events schema must use MergeTree")
	}
	partition := normalizeExpression(partitionKey)
	sorting := normalizeExpression(sortingKey)
	cols := make(pyjson.List, len(columns))
	for i, c := range columns {
		cols[i] = pyjson.List{c.Name, c.Type}
	}
	payload := pyjson.NewDict()
	payload.Set("columns", cols)
	payload.Set("engine", pyLower(engine))
	payload.Set("partition_key", partition)
	payload.Set("sorting_key", sorting)
	sum := sha256.Sum256([]byte(pyjson.Dumps(payload)))
	return &CanonicalSchema{
		CreateTableSQL: strings.TrimRight(pyjson.Strip(source[loc[0]:loc[1]]), ";"),
		Columns:        columns,
		Engine:         engine,
		PartitionKey:   partition,
		SortingKey:     sorting,
		Fingerprint:    hex.EncodeToString(sum[:]),
	}, nil
}

// bindCreateTable mirrors the re.sub(count=1) that retargets the DDL.
func (s *CanonicalSchema) bindCreateTable(database, table string) string {
	loc := createPrefixRe.FindStringIndex(s.CreateTableSQL)
	if loc == nil {
		return s.CreateTableSQL
	}
	return s.CreateTableSQL[:loc[0]] + "CREATE TABLE IF NOT EXISTS " + quoteIdentifierUnchecked(database) + "." +
		quoteIdentifierUnchecked(table) + s.CreateTableSQL[loc[1]:]
}
