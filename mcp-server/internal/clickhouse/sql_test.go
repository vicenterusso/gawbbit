package clickhouse

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

func TestQuoteStringEscapesSingleQuotes(t *testing.T) {
	if got := QuoteString("bob's app"); got != `'bob\'s app'` {
		t.Fatalf("got %s", got)
	}
	if got := QuoteString(`a\b`); got != `'a\\b'` {
		t.Fatalf("got %s", got)
	}
}

func TestCheckedLimitBoundsValues(t *testing.T) {
	for _, c := range [][3]int64{{0, 100, 1}, {50, 100, 50}, {500, 100, 100}, {-5, 100, 1}} {
		if got := CheckedLimit(big.NewInt(c[0]), c[1]); got.Int64() != c[2] {
			t.Fatalf("CheckedLimit(%d, %d) = %s", c[0], c[1], got)
		}
	}
	huge, _ := new(big.Int).SetString("99999999999999999999999", 10)
	if got := CheckedLimit(huge, 50); got.Int64() != 50 {
		t.Fatalf("huge limit not clamped: %s", got)
	}
}

func TestValidateReadonlySQLAllowsSelect(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1;":                  "SELECT 1",
		"  with 1 as a select a ;; ": "with 1 as a select a",
		"\x1cSHOW TABLES\u00a0":      "SHOW TABLES",
		"select 'déjà' as droplet":   "select 'déjà' as droplet",
		"SELECT 1 AS updated_at":     "SELECT 1 AS updated_at",
		"select 1 as éinsert":        "select 1 as éinsert",
	} {
		got, err := ValidateReadonlySQL(in)
		if err != nil || got != want {
			t.Fatalf("ValidateReadonlySQL(%q) = %q, %v", in, got, err)
		}
	}
}

func TestValidateReadonlySQLRejectsMutation(t *testing.T) {
	cases := map[string]string{
		"INSERT INTO analytics.events SELECT 1":       "Only read-only SELECT/WITH/SHOW/DESCRIBE/EXPLAIN queries are allowed",
		"SELECT 1; DROP TABLE analytics.events":       "Only one statement is allowed",
		"ALTER TABLE analytics.events DELETE WHERE 1": "Only read-only SELECT/WITH/SHOW/DESCRIBE/EXPLAIN queries are allowed",
		"   ":                              "SQL cannot be empty",
		"select 'drop' as x":               "Query contains forbidden mutating or administrative SQL",
		"SELECT * FROM system.tables":      "Query contains forbidden mutating or administrative SQL",
		"WITH x AS (SELECT 1) SELECT KILL": "Query contains forbidden mutating or administrative SQL",
	}
	for sql, msg := range cases {
		_, err := ValidateReadonlySQL(sql)
		var unsafe *UnsafeQueryError
		if !errors.As(err, &unsafe) || unsafe.Msg != msg {
			t.Fatalf("ValidateReadonlySQL(%q) = %v, want %q", sql, err, msg)
		}
	}
}

func TestBuildFunnelSQLUsesOrderedWindowFunnel(t *testing.T) {
	cfg, err := settings.FromValues(map[string]string{"MCP_API_KEYS_JSON": `{"test":"token"}`})
	if err != nil {
		t.Fatal(err)
	}
	app, env := "com.example.game", "prod"
	sql := BuildFunnelSQL(cfg, FunnelParams{
		Steps:       []string{"tutorial_started", "tutorial_completed", "purchase_completed"},
		StartDate:   "2026-06-01",
		EndDate:     "2026-06-07",
		AppID:       &app,
		Environment: &env,
		WindowHours: big.NewInt(24),
		ExcludeBots: true,
	})
	for _, want := range []string{
		"windowFunnel(86400)", "event_name = 'tutorial_started'", "event_name = 'tutorial_completed'",
		"event_name = 'purchase_completed'", "countIf(funnel_step >= 3) AS step_3_users",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("funnel SQL lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "minIf(") {
		t.Fatal("funnel SQL must not use minIf")
	}
}
