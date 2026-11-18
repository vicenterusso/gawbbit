//go:build integration

// Differential parity suite for the MCP server. It seeds ClickHouse with
// testdata/parity/seed.sql, starts the server for every configuration in
// testdata/parity/configs and compares the initialize result, tools/list,
// tools/call results and raw HTTP responses with goldens recorded from the
// original Python (FastMCP) server.
//
//	MCP_PARITY_CLICKHOUSE=http://admin:adminpw@127.0.0.1:38123 \
//	  go test -tags integration ./internal/server -run TestMCPParity
//
// MCP_PARITY_RECORD="<python> -m rawbbit_mcp.server" (with PYTHONPATH set)
// re-records the goldens by launching that command on MCP_PARITY_RECORD_PORT.
package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/datasets"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

type parityCase struct {
	Kind    string            `json:"kind"` // init, call, http
	Path    string            `json:"path"`
	Token   string            `json:"token,omitempty"`
	Tool    string            `json:"tool,omitempty"`
	Args    json.RawMessage   `json:"args,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Divergent names a documented, deliberate deviation (see README "Go
	// rewrite notes"): the golden holds the Python output and Go must differ
	// in the documented way.
	Divergent string `json:"divergent,omitempty"`
}

type parityConfig struct {
	Env      map[string]string `json:"env"`
	Registry json.RawMessage   `json:"registry,omitempty"`
	Cases    []parityCase      `json:"cases"`
}

func chExec(t *testing.T, base *url.URL, sql string) {
	t.Helper()
	resp, err := http.Post(base.String(), "text/plain", strings.NewReader(sql))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("seed %q: %s", sql[:min(len(sql), 80)], body)
	}
}

func seed(t *testing.T, base *url.URL) {
	data, err := os.ReadFile(filepath.Join("testdata", "parity", "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(data), ";\n\n") {
		if stmt = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";")); stmt != "" {
			chExec(t, base, stmt)
		}
	}
}

// jwtTokens signs the bearer tokens used by the JWT configuration.
func jwtTokens(t *testing.T) (map[string]string, string) {
	t.Helper()
	key, other := newKey(t), newKey(t)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	sign := func(method jwt.SigningMethod, k any, claims jwt.MapClaims) string {
		s, err := jwt.NewWithClaims(method, claims).SignedString(k)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := func(extra jwt.MapClaims) jwt.MapClaims {
		c := jwt.MapClaims{"sub": "parity-client", "iss": "https://issuer.example", "aud": "rawbbit-mcp", "exp": 4102444800}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	return map[string]string{
		"valid":       sign(jwt.SigningMethodRS256, key, base(nil)),
		"aud_list":    sign(jwt.SigningMethodRS256, key, base(jwt.MapClaims{"aud": []string{"x", "rawbbit-mcp"}})),
		"no_exp":      sign(jwt.SigningMethodRS256, key, jwt.MapClaims{"iss": "https://issuer.example", "aud": "rawbbit-mcp"}),
		"expired":     sign(jwt.SigningMethodRS256, key, base(jwt.MapClaims{"exp": 1000000000})),
		"wrong_iss":   sign(jwt.SigningMethodRS256, key, base(jwt.MapClaims{"iss": "https://evil.example"})),
		"wrong_aud":   sign(jwt.SigningMethodRS256, key, base(jwt.MapClaims{"aud": "other"})),
		"hs256":       sign(jwt.SigningMethodHS256, []byte(pub), base(nil)),
		"other_key":   sign(jwt.SigningMethodRS256, other, base(nil)),
		"not_a_token": "not.a.token",
	}, pub
}

// newKey creates a throwaway signing key; results never contain token bytes,
// so the goldens do not depend on the key.
func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

var jwtRef = regexp.MustCompile(`@jwt:([a-z_]+)`)

// resolveToken replaces @jwt:<name> references with generated tokens.
func resolveToken(token string, jwts map[string]string) string {
	return jwtRef.ReplaceAllStringFunc(token, func(ref string) string { return jwts[ref[len("@jwt:"):]] })
}

func httpCase(t *testing.T, base string, c parityCase, jwts map[string]string) map[string]any {
	req, _ := http.NewRequest(c.Method, base+c.Path, strings.NewReader(c.Body))
	for k, v := range c.Headers {
		req.Header.Set(k, resolveToken(v, jwts))
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{"status": resp.StatusCode}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		out["body"] = string(body)
		out["content_type"] = ct
	}
	for _, h := range []string{"WWW-Authenticate", "Location"} {
		if v := resp.Header.Get(h); v != "" {
			out[strings.ToLower(h)] = strings.Replace(v, req.Host, "HOST", 1)
		}
	}
	return out
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func canonical(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	if m, ok := out.(map[string]any); ok {
		if m["isError"] == false {
			delete(m, "isError")
		}
	}
	return out
}

func runCases(t *testing.T, base string, cfg parityConfig, jwts map[string]string) []any {
	sessions := map[string]*mcp.ClientSession{}
	defer func() {
		for _, s := range sessions {
			_ = s.Close()
		}
	}()
	session := func(path, token string) (*mcp.ClientSession, error) {
		key := path + "\x00" + token
		if s, ok := sessions[key]; ok {
			return s, nil
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "parity", Version: "1"}, nil)
		s, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint: base + path, HTTPClient: &http.Client{Transport: bearerTransport{resolveToken(token, jwts)}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err == nil {
			sessions[key] = s
		}
		return s, err
	}
	var results []any
	for _, c := range cfg.Cases {
		switch c.Kind {
		case "http":
			results = append(results, httpCase(t, base, c, jwts))
		case "init":
			s, err := session(c.Path, c.Token)
			if err != nil {
				results = append(results, map[string]any{"connect_error": true})
				continue
			}
			tools, err := s.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			init := canonical(s.InitializeResult()).(map[string]any)
			results = append(results, map[string]any{"initialize": init, "tools": canonical(tools)})
		case "call":
			s, err := session(c.Path, c.Token)
			if err != nil {
				t.Fatalf("connect %s: %v", c.Path, err)
			}
			args := any(c.Args)
			if len(c.Args) == 0 {
				args = nil
			}
			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: c.Tool, Arguments: args})
			if err != nil {
				results = append(results, map[string]any{"rpc_error": err.Error()})
				continue
			}
			results = append(results, canonical(res))
		default:
			t.Fatalf("unknown case kind %q", c.Kind)
		}
	}
	return results
}

func freePortReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return true
	}
	return false
}

func startPython(t *testing.T, command []string, env map[string]string, port string) func() {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append([]string{}, os.Environ()...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "MCP_HOST=127.0.0.1", "MCP_PORT="+port)
	cmd.Dir = t.TempDir() // no stray .env
	logFile, _ := os.Create(filepath.Join(os.TempDir(), "mcp-parity-python.log"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + port
	for i := 0; i < 100 && !freePortReady(addr); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logFile.Close()
		for i := 0; i < 50 && freePortReady(addr); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func TestMCPParity(t *testing.T) {
	raw := os.Getenv("MCP_PARITY_CLICKHOUSE")
	if raw == "" {
		t.Skip("set MCP_PARITY_CLICKHOUSE=http://admin:password@host:port to run the parity suite")
	}
	chURL, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, chURL)
	jwts, publicKey := jwtTokens(t)
	record := strings.Fields(os.Getenv("MCP_PARITY_RECORD"))
	files, _ := filepath.Glob(filepath.Join("testdata", "parity", "configs", "*.json"))
	sort.Strings(files)
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var cfg parityConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for k, v := range cfg.Env {
				env[k] = strings.ReplaceAll(v, "@jwt_public_key", publicKey)
			}
			if len(cfg.Registry) > 0 {
				path := filepath.Join(t.TempDir(), "datasets.json")
				if err := os.WriteFile(path, cfg.Registry, 0o600); err != nil {
					t.Fatal(err)
				}
				env["MCP_DATASETS_FILE"] = path
			}
			golden := filepath.Join("testdata", "parity", "golden", name+".json")
			var base string
			if len(record) > 0 {
				port := os.Getenv("MCP_PARITY_RECORD_PORT")
				if port == "" {
					port = "38000"
				}
				stop := startPython(t, record, env, port)
				defer stop()
				base = "http://127.0.0.1:" + port
			} else {
				s, err := settings.FromValues(env)
				if err != nil {
					t.Fatal(err)
				}
				reg, err := datasets.Load(env["MCP_DATASETS_FILE"], s)
				if err != nil {
					t.Fatal(err)
				}
				log, _ := NewLogger(io.Discard, "INFO")
				srv := httptest.NewServer(New(s, reg, DefaultGatewayFactory("rawbbit-mcp-server/parity"), log))
				defer srv.Close()
				base = srv.URL
			}
			got := runCases(t, base, cfg, jwts)
			if len(record) > 0 {
				encoded, _ := json.MarshalIndent(got, "", " ")
				if err := os.WriteFile(golden, append(encoded, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantData, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden: %v", err)
			}
			var want []any
			if err := json.Unmarshal(wantData, &want); err != nil {
				t.Fatal(err)
			}
			if len(want) != len(got) {
				t.Fatalf("golden has %d results, got %d", len(want), len(got))
			}
			mismatches := 0
			for i := range want {
				c := cfg.Cases[i]
				g := canonical(got[i])
				if c.Divergent != "" {
					if err := checkDivergent(c.Divergent, want[i], g); err != nil {
						t.Errorf("case %d (%s %s %s): %v", i, c.Kind, c.Path, c.Tool, err)
					}
					continue
				}
				wj, _ := json.Marshal(want[i])
				gj, _ := json.Marshal(g)
				if string(wj) != string(gj) {
					mismatches++
					if mismatches <= 10 {
						t.Errorf("case %d (%s %s %s %s):\n python: %s\n     go: %s", i, c.Kind, c.Path, c.Tool, c.Args, wj, gj)
					}
				}
			}
			if mismatches > 0 {
				t.Errorf("%d of %d cases differ", mismatches, len(want))
			}
		})
	}
}

// checkDivergent verifies a documented deviation:
//   - readonly_settings: clickhouse-connect raised "Setting ... is unknown or
//     readonly" for readonly dataset users; Go drops those settings and succeeds.
//   - interval_width: clickhouse-connect decoded Interval columns as 4-byte
//     integers and misread the rest of the block; Go decodes them as Int64.
func checkDivergent(kind string, python, goResult any) error {
	pj, _ := json.Marshal(python)
	gj, _ := json.Marshal(goResult)
	marker := map[string]string{
		"readonly_settings": "is unknown or readonly",
		"interval_width":    "Unrecognized ClickHouse type base:  name: ",
	}[kind]
	if marker == "" || !strings.Contains(string(pj), marker) {
		return fmt.Errorf("python golden no longer shows the %s behavior: %s", kind, pj)
	}
	if string(pj) == string(gj) {
		return errors.New("go result unexpectedly equals the python result")
	}
	if kind == "readonly_settings" {
		if m, ok := goResult.(map[string]any); !ok || m["isError"] == true {
			return errors.New("go result is an error: " + string(gj))
		}
	}
	return nil
}
