// Package clickhouse is a minimal ClickHouse HTTP client shared by the
// bounded control plane and the ingestion loader. It never uses proxy
// environment settings, never follows redirects, and never includes server
// response bodies in returned errors (they can contain SQL or credentials).
package clickhouse

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
)

// Endpoint is the validated loader identity and HTTP endpoint.
type Endpoint struct {
	URL      string
	User     string
	Password string
	Verify   bool
}

// ParseEndpoint validates the ClickHouse settings in the original order.
func ParseEndpoint(env config.Env) (Endpoint, error) {
	secure := env.Get("CLICKHOUSE_SECURE", "0")
	verify := env.Get("CLICKHOUSE_VERIFY", "1")
	if (secure != "0" && secure != "1") || (verify != "0" && verify != "1") {
		return Endpoint{}, config.Errorf("invalid ClickHouse TLS switches")
	}
	host := env.Get("CLICKHOUSE_HOST", "clickhouse")
	port := env.Get("CLICKHOUSE_PORT", "8123")
	if host == "" || strings.ContainsAny(host, `/@?#\`) || !validPort(port) {
		return Endpoint{}, config.Errorf("invalid ClickHouse HTTP endpoint")
	}
	scheme := "http"
	if secure == "1" {
		scheme = "https"
	}
	ep := Endpoint{URL: scheme + "://" + host + ":" + port, User: env.Get("CLICKHOUSE_DBT_USER", ""),
		Password: env.Get("CLICKHOUSE_DBT_PASSWORD", ""), Verify: verify == "1"}
	if ep.User == "" || ep.Password == "" {
		return Endpoint{}, config.Errorf("loader credentials required")
	}
	return ep, nil
}

func validPort(port string) bool {
	if port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

// ErrRequest is the only error surfaced for a failed request.
var ErrRequest = errors.New("ClickHouse request failed")

// Client sends single HTTP requests without retries.
type Client struct {
	Endpoint Endpoint
	http     *http.Client
}

// NewClient builds a client. connectTimeout bounds TCP/TLS setup; readTimeout
// bounds the wait for the (end-of-query) response.
func NewClient(ep Endpoint, connectTimeout, readTimeout time.Duration) *Client {
	dialer := &net.Dialer{Timeout: connectTimeout}
	transport := &http.Transport{
		Proxy:                 nil, // Never send credentials through proxy environment.
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: readTimeout,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !ep.Verify}, //nolint:gosec // explicit CLICKHOUSE_VERIFY=0
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
	}
	return &Client{Endpoint: ep, http: &http.Client{
		Transport: transport,
		// Do not follow HTTP redirects with credentials.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Do POSTs one statement and returns at most limit bytes of the body.
func (c *Client) Do(ctx context.Context, sql string, params url.Values, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Endpoint.URL+"/?"+params.Encode(), bytes.NewBufferString(sql))
	if err != nil {
		return nil, ErrRequest
	}
	request.Header.Set("X-ClickHouse-User", c.Endpoint.User)
	request.Header.Set("X-ClickHouse-Key", c.Endpoint.Password)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, ErrRequest
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, ErrRequest
	}
	return body, nil
}

// Rows decodes a FORMAT JSON response's data array.
func Rows(body []byte) ([]map[string]any, error) {
	var parsed struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data == nil {
		return nil, ErrRequest
	}
	return parsed.Data, nil
}

// Literal quotes a string as a ClickHouse SQL literal.
func Literal(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// UUID4 returns a random RFC 4122 version 4 UUID string.
func UUID4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// IsUUID reports a canonical lowercase UUID (str(uuid.UUID(v)) == v).
func IsUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i, c := range v {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
