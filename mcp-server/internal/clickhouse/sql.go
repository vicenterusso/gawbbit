// Package clickhouse builds the analytics SQL, guards free-form queries and
// talks to ClickHouse over HTTP like clickhouse-connect (Native format).
package clickhouse

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

// JSONColumns are the JSON string columns discover_json_keys accepts.
var JSONColumns = []string{
	"event_params_json",
	"user_properties_json",
	"traffic_source_json",
	"geo_json",
	"consent_json",
}

// IsJSONColumn reports whether name is one of JSONColumns.
func IsJSONColumn(name string) bool {
	for _, c := range JSONColumns {
		if c == name {
			return true
		}
	}
	return false
}

// SortedJSONColumns returns JSONColumns sorted like Python's sorted().
func SortedJSONColumns() []string {
	out := append([]string(nil), JSONColumns...)
	sort.Strings(out)
	return out
}

var readonlyPrefixes = []string{"select", "with", "show", "describe", "desc", "explain"}

// forbiddenWords mirrors FORBIDDEN_SQL under re.IGNORECASE. Go's case
// folding already matches U+017F for "s" and U+212A for "k" like Python;
// Python additionally matches U+0130 and U+0131 for "i".
var forbiddenWords = regexp.MustCompile("(?i)(" + strings.ReplaceAll(
	"insert|alter|create|drop|truncate|delete|update|optimize|system|grant|revoke|attach|detach|rename|kill",
	"i", "[i\u0130\u0131]") + ")")

// UnsafeQueryError reports a rejected free-form query.
type UnsafeQueryError struct{ Msg string }

func (e *UnsafeQueryError) Error() string { return e.Msg }

// QuoteString renders a ClickHouse string literal.
func QuoteString(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// CheckedLimit clamps a requested row limit into [1, maximum].
func CheckedLimit(limit *big.Int, maximum int64) *big.Int {
	if limit.Sign() < 1 {
		return big.NewInt(1)
	}
	max := big.NewInt(maximum)
	if limit.Cmp(max) > 0 {
		return max
	}
	return limit
}

// ValidateReadonlySQL accepts a single read-only statement and returns it
// without surrounding whitespace and trailing semicolons.
func ValidateReadonlySQL(sql string) (string, error) {
	cleaned := pyval.Strip(sql)
	if cleaned == "" {
		return "", &UnsafeQueryError{"SQL cannot be empty"}
	}
	if strings.Contains(strings.TrimRight(cleaned, ";"), ";") {
		return "", &UnsafeQueryError{"Only one statement is allowed"}
	}
	cleaned = pyval.Strip(strings.TrimRight(cleaned, ";"))
	lowered := pyval.Lower(cleaned)
	allowed := false
	for _, prefix := range readonlyPrefixes {
		if strings.HasPrefix(lowered, prefix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", &UnsafeQueryError{"Only read-only SELECT/WITH/SHOW/DESCRIBE/EXPLAIN queries are allowed"}
	}
	if containsForbiddenWord(cleaned) {
		return "", &UnsafeQueryError{"Query contains forbidden mutating or administrative SQL"}
	}
	return cleaned, nil
}

// containsForbiddenWord mirrors re.search(r"\b(...)\b", sql, re.IGNORECASE)
// with Python's Unicode-aware word boundaries.
func containsForbiddenWord(sql string) bool {
	for _, loc := range forbiddenAll(sql) {
		before, _ := utf8.DecodeLastRuneInString(sql[:loc[0]])
		after, _ := utf8.DecodeRuneInString(sql[loc[1]:])
		if (loc[0] == 0 || !pyval.IsWordChar(before)) && (loc[1] == len(sql) || !pyval.IsWordChar(after)) {
			return true
		}
	}
	return false
}

// forbiddenAll finds every (possibly overlapping) keyword occurrence.
func forbiddenAll(sql string) [][]int {
	var out [][]int
	for start := 0; start < len(sql); {
		loc := forbiddenWords.FindStringIndex(sql[start:])
		if loc == nil {
			break
		}
		out = append(out, []int{start + loc[0], start + loc[1]})
		_, size := utf8.DecodeRuneInString(sql[start+loc[0]:])
		start += loc[0] + size
	}
	return out
}

// BotFilterSQL is the optional bot user-agent exclusion clause.
func BotFilterSQL(cfg *settings.Settings, excludeBots bool) string {
	if !excludeBots {
		return ""
	}
	return " AND NOT match(lower(ifNull(ingest_user_agent, '')), " + QuoteString(pyval.Lower(cfg.BotUserAgentRegex)) + ")"
}

// OptionalFilters are the shared app/environment/event filters (nil = unset).
type OptionalFilters struct {
	AppID       *string
	Environment *string
	EventName   *string
}

// SQL renders the filters; empty strings are ignored like in Python.
func (f OptionalFilters) SQL() string {
	var clauses []string
	if f.AppID != nil && *f.AppID != "" {
		clauses = append(clauses, "app_id = "+QuoteString(*f.AppID))
	}
	if f.Environment != nil && *f.Environment != "" {
		clauses = append(clauses, "environment = "+QuoteString(*f.Environment))
	}
	if f.EventName != nil && *f.EventName != "" {
		clauses = append(clauses, "event_name = "+QuoteString(*f.EventName))
	}
	if len(clauses) == 0 {
		return ""
	}
	return " AND " + strings.Join(clauses, " AND ")
}

// FunnelParams are the calculate_funnel inputs after validation.
type FunnelParams struct {
	Steps       []string
	StartDate   string
	EndDate     string
	AppID       *string
	Environment *string
	WindowHours *big.Int
	ExcludeBots bool
}

// BuildFunnelSQL renders the ordered windowFunnel query.
func BuildFunnelSQL(cfg *settings.Settings, p FunnelParams) string {
	quoted := make([]string, len(p.Steps))
	conditions := make([]string, len(p.Steps))
	counts := make([]string, len(p.Steps))
	for i, step := range p.Steps {
		quoted[i] = QuoteString(step)
		conditions[i] = "event_name = " + QuoteString(step)
		counts[i] = fmt.Sprintf("countIf(funnel_step >= %d) AS step_%d_users", i+1, i+1)
	}
	hours := p.WindowHours
	if hours.Cmp(big.NewInt(1)) < 0 {
		hours = big.NewInt(1)
	}
	windowSeconds := new(big.Int).Mul(hours, big.NewInt(3600))
	filters := OptionalFilters{AppID: p.AppID, Environment: p.Environment}

	return `
    WITH per_actor AS
    (
      SELECT
        coalesce(nullIf(user_id, ''), user_pseudo_id) AS actor_id,
        windowFunnel(` + windowSeconds.String() + `)(
          toUnixTimestamp(event_time),
          ` + strings.Join(conditions, ",\n          ") + `
        ) AS funnel_step
      FROM ` + cfg.TableRef() + `
      WHERE event_date BETWEEN toDate(` + QuoteString(p.StartDate) + `) AND toDate(` + QuoteString(p.EndDate) + `)
        AND event_time IS NOT NULL
        AND event_name IN (` + strings.Join(quoted, ", ") + `)
      ` + filters.SQL() + `
      ` + BotFilterSQL(cfg, p.ExcludeBots) + `
      GROUP BY actor_id
    )
    SELECT
      ` + strings.Join(counts, ",\n      ") + `
    FROM per_actor
    `
}
