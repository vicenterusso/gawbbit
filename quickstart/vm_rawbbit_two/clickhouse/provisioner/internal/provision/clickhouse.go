package provision

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

// Client is the ClickHouse metadata/DDL interface the provisioner uses.
type Client interface {
	Query(sql string) ([]*pyjson.Dict, error)
	Execute(sql string) error
}

// HTTPClient posts SQL to the loopback ClickHouse HTTP interface.
type HTTPClient struct {
	url           string
	authorization string
	http          *http.Client
}

// NewHTTPClient mirrors ClickHouseHTTP(AdminConnection).
func NewHTTPClient(c *AdminConnection) *HTTPClient {
	credentials := c.Username + ":" + c.Password
	return &HTTPClient{
		url:           fmt.Sprintf("http://%s:%d/", c.Host, c.Port),
		authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte(credentials)),
		http: &http.Client{
			Timeout: time.Duration(c.TimeoutSeconds) * time.Second,
			// urllib never follows redirects for POST bodies here.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{Proxy: nil, DisableCompression: true},
		},
	}
}

func (c *HTTPClient) request(sql string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, c.url, strings.NewReader(sql))
	if err != nil {
		return "", provisionErr("ClickHouse request failed or timed out")
	}
	req.Header.Set("Authorization", c.authorization)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", provisionErr("ClickHouse request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Never return ClickHouse's response body: it can include SQL text.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		code := "unknown"
		if m := clickhouseCode.FindStringSubmatch(ignoreInvalidUTF8(body)); m != nil {
			code = m[1]
		}
		return "", provisionErr("ClickHouse rejected a request (HTTP " + strconv.Itoa(resp.StatusCode) + ", error code " + code + ")")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", provisionErr("ClickHouse request failed or timed out")
	}
	return string(body), nil
}

func ignoreInvalidUTF8(b []byte) string {
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size > 1 {
			sb.WriteRune(r)
		}
		b = b[size:]
	}
	return sb.String()
}

// Query runs a metadata query with FORMAT JSONEachRow.
func (c *HTTPClient) Query(sql string) ([]*pyjson.Dict, error) {
	body, err := c.request(strings.TrimRight(strings.TrimRightFunc(sql, pyjson.IsSpace), ";") + " FORMAT JSONEachRow")
	if err != nil {
		return nil, err
	}
	return parseRows(body)
}

func parseRows(body string) ([]*pyjson.Dict, error) {
	if pyjson.Strip(body) == "" {
		return nil, nil
	}
	var rows []*pyjson.Dict
	for _, line := range pyjson.SplitLines(body) {
		if pyjson.Strip(line) == "" {
			continue
		}
		v, err := pyjson.Decode([]byte(line))
		if err != nil {
			return nil, provisionErr("ClickHouse returned an invalid metadata response")
		}
		row, ok := v.(*pyjson.Dict)
		if !ok {
			return nil, provisionErr("ClickHouse returned an invalid metadata response")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Execute runs a DDL/DCL statement.
func (c *HTTPClient) Execute(sql string) error {
	_, err := c.request(sql)
	return err
}

func restrictedClient(runtime RuntimeDataset, admin *AdminConnection) *HTTPClient {
	return NewHTTPClient(&AdminConnection{
		Host:           admin.Host,
		Port:           admin.Port,
		Username:       pyjson.Str(runtime.Lookup("clickhouse_user")),
		Password:       pyjson.Str(runtime.Lookup("clickhouse_password")),
		TimeoutSeconds: admin.TimeoutSeconds,
	})
}
