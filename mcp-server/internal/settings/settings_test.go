package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsParseStaticAPIKeysAndAuthMode(t *testing.T) {
	s, err := FromValues(map[string]string{"MCP_API_KEYS_JSON": `{"user1":"token-1","user2":"token-2"}`})
	if err != nil {
		t.Fatal(err)
	}
	keys := s.APIKeysByUser()
	if len(keys) != 2 || keys[0] != (LabeledToken{"user1", "token-1"}) || keys[1] != (LabeledToken{"user2", "token-2"}) {
		t.Fatalf("keys: %+v", keys)
	}
	if s.AuthMode() != AuthStaticTokens {
		t.Fatalf("auth mode %s", s.AuthMode())
	}
}

func TestSettingsUseJWTModeWithoutStaticTokens(t *testing.T) {
	s, err := FromValues(map[string]string{"MCP_JWT_PUBLIC_KEY": "public-key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.APIKeysByUser()) != 0 || s.AuthMode() != AuthJWT {
		t.Fatalf("unexpected %v %s", s.APIKeysByUser(), s.AuthMode())
	}
}

func TestSettingsFailClosedWithoutAuth(t *testing.T) {
	_, err := FromValues(map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "MCP authentication is required") {
		t.Fatalf("got %v", err)
	}
	want := "1 validation error for Settings\n  Value error, MCP authentication is required. Set MCP_API_KEYS_JSON or JWT settings, " +
		"or explicitly set MCP_ALLOW_UNAUTHENTICATED=1 for local-only development. [type=value_error, input_value={}, input_type=dict]\n" +
		"    For further information visit https://errors.pydantic.dev/2.12/v/value_error"
	if err.Error() != want {
		t.Fatalf("message:\n%s", err)
	}
}

func TestSettingsAllowExplicitUnauthenticatedMode(t *testing.T) {
	s, err := FromValues(map[string]string{"MCP_ALLOW_UNAUTHENTICATED": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if s.AuthMode() != AuthNone || !s.AllowUnauthenticated {
		t.Fatalf("unexpected %s %v", s.AuthMode(), s.AllowUnauthenticated)
	}
}

func TestSettingsRejectInvalidStaticAPIKeysJSON(t *testing.T) {
	for _, raw := range []string{"[]", `{"user1":"same","user2":"same"}`, `{"":"token"}`, `{"user1":""}`, "{", `{"a":1}`} {
		if _, err := FromValues(map[string]string{"MCP_API_KEYS_JSON": raw}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestSettingsDefaultsAndCoercion(t *testing.T) {
	s, err := FromValues(map[string]string{
		"mcp_allow_unauthenticated": "on", "MCP_PATH": " custom ", "MCP_MAX_QUERY_ROWS": " 1_000 ",
		"CLICKHOUSE_SECURE": "Yes", "CLICKHOUSE_DATABASE": " analytics ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.MCPPath != "/custom" || s.MaxQueryRows != 1000 || !s.ClickHouseSecure || s.ClickHouseDatabase != "analytics" ||
		s.MCPPort != 8000 || s.ClickHousePort != 8123 || s.ClickHouseHost != "host.docker.internal" || s.MaxSampleRows != 50 ||
		s.TableRef() != "analytics.events" || !s.ClickHouseVerify {
		t.Fatalf("unexpected settings %+v", s)
	}
	_, err = FromValues(map[string]string{"MCP_ALLOW_UNAUTHENTICATED": "1", "MCP_PORT": "abc", "CLICKHOUSE_TABLE": "bad-name"})
	want := "2 validation errors for Settings\nMCP_PORT\n  Input should be a valid integer, unable to parse string as an integer [type=int_parsing, input_value='abc', input_type=str]\n" +
		"    For further information visit https://errors.pydantic.dev/2.12/v/int_parsing\nCLICKHOUSE_TABLE\n" +
		"  Value error, ClickHouse database and table names may only contain letters, numbers, and underscores [type=value_error, input_value='bad-name', input_type=str]\n" +
		"    For further information visit https://errors.pydantic.dev/2.12/v/value_error"
	if err == nil || err.Error() != want {
		t.Fatalf("message:\n%v", err)
	}
}

func TestLoadReadsDotenvBelowEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# comment\nexport MCP_NAME='From file'\nMCP_PORT=9100 # inline\nMCP_API_KEYS_JSON={\"a\":\"b\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_PORT", "9200")
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.MCPName != "From file" || s.MCPPort != 9200 || s.AuthMode() != AuthStaticTokens {
		t.Fatalf("unexpected %+v", s)
	}
}
