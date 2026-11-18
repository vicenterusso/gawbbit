package clickhouse

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

// Querier runs a guarded read-only query and returns named rows.
type Querier interface {
	QueryRows(ctx context.Context, sql string) ([]*pyval.Dict, error)
}

// Gateway mirrors rawbbit_mcp.clickhouse.ClickHouseGateway: every call
// creates a fresh logical client (settings discovery, then the query).
type Gateway struct {
	cfg   *settings.Settings
	http  *http.Client
	url   string
	auth  string
	agent string
}

const (
	connectTimeout = 10 * time.Second
	readTimeout    = 300 * time.Second
	maxErrorSize   = 1024
	// progressIntervalMS is min(120000, max(10000, (300 - 5) * 1000)).
	progressIntervalMS = "120000"
	// nativeProtocolVersion enables LowCardinality, DateTime timezones and
	// BlockInfo in Native responses, as negotiated by clickhouse-connect.
	nativeProtocolVersion = "54405"
)

// NewGateway builds a gateway for one ClickHouse identity.
func NewGateway(cfg *settings.Settings, userAgent string) *Gateway {
	scheme := "http"
	if cfg.ClickHouseSecure {
		scheme = "https"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = readTimeout
	if cfg.ClickHouseSecure && !cfg.ClickHouseVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit CLICKHOUSE_VERIFY=0
	}
	credentials := cfg.ClickHouseUser + ":" + cfg.ClickHousePassword
	return &Gateway{
		cfg:   cfg,
		http:  &http.Client{Transport: transport},
		url:   fmt.Sprintf("%s://%s:%d", scheme, cfg.ClickHouseHost, cfg.ClickHousePort),
		auth:  "Basic " + base64.StdEncoding.EncodeToString([]byte(credentials)),
		agent: userAgent,
	}
}

// DatabaseError mirrors clickhouse_connect DatabaseError / OperationalError /
// ProgrammingError messages.
type DatabaseError struct{ Msg string }

func (e *DatabaseError) Error() string { return e.Msg }

type serverSetting struct {
	value    string
	readonly bool
}

type session struct {
	settings map[string]serverSetting
	serverTZ string
	params   url.Values
	compress bool
}

var bootstrapSettings = []string{
	"readonly", "max_execution_time", "max_result_rows", "result_overflow_mode",
	"enable_http_compression", "send_progress_in_http_headers", "http_headers_progress_interval_ms",
	"date_time_input_format", "allow_experimental_json_type", "cast_string_to_dynamic_use_inference",
}

var columnsOnlyRe = regexp.MustCompile(`(?i)LIMIT 0\s*$`)

// commentRe mirrors clickhouse_connect.driver.query.comment_re.
var commentRe = regexp.MustCompile(`(?ms)("[^"]*?"|'.*?')|(/\*.*?\*/|(--\s)[^\n]*$)`)

func removeSQLComments(sql string) string {
	return commentRe.ReplaceAllStringFunc(sql, func(m string) string {
		if strings.HasPrefix(m, `"`) || strings.HasPrefix(m, "'") {
			return m
		}
		return ""
	})
}

// QueryRows validates and runs sql, returning rows like
// dict(zip(result.column_names, row)).
func (g *Gateway) QueryRows(ctx context.Context, sql string) ([]*pyval.Dict, error) {
	checked, err := ValidateReadonlySQL(sql)
	if err != nil {
		return nil, err
	}
	s, err := g.open(ctx)
	if err != nil {
		return nil, err
	}
	params := cloneValues(s.params)
	params.Set("database", g.cfg.ClickHouseDatabase)
	params.Set("client_protocol_version", nativeProtocolVersion)
	for _, qs := range g.cfg.QuerySettings() {
		if s.shouldSend(qs.Name, qs.Value, s.params.Get(qs.Name)) {
			params.Set(qs.Name, qs.Value)
		}
	}
	finalQuery := strings.TrimRight(checked, ";")
	if columnsOnlyRe.MatchString(removeSQLComments(finalQuery)) {
		body, _, err := g.request(ctx, finalQuery+"\n FORMAT JSON", params, s.compress)
		if err != nil {
			return nil, err
		}
		if !json.Valid(body) {
			return nil, &DatabaseError{"ClickHouse returned an invalid JSON response"}
		}
		return []*pyval.Dict{}, nil
	}
	body, header, err := g.request(ctx, finalQuery+"\n FORMAT Native", params, s.compress)
	if err != nil {
		return nil, err
	}
	tctx, err := s.timeContext(header.Get("X-ClickHouse-Timezone"))
	if err != nil {
		return nil, err
	}
	return decodeNative(bytes.NewReader(body), tctx)
}

// open performs the per-client discovery clickhouse-connect does in
// get_client(): server timezone and the current user's setting values.
func (g *Gateway) open(ctx context.Context) (*session, error) {
	quoted := make([]string, len(bootstrapSettings))
	for i, name := range bootstrapSettings {
		quoted[i] = QuoteString(name)
	}
	query := "SELECT timezone() AS tz, name, value, readonly FROM system.settings WHERE name IN (" +
		strings.Join(quoted, ", ") + ") FORMAT JSONEachRow"
	params := url.Values{}
	params.Set("database", g.cfg.ClickHouseDatabase)
	body, _, err := g.request(ctx, query, params, false)
	if err != nil {
		return nil, err
	}
	s := &session{settings: map[string]serverSetting{}, params: url.Values{}}
	dec := json.NewDecoder(bytes.NewReader(body))
	for dec.More() {
		var row struct {
			TZ       string          `json:"tz"`
			Name     string          `json:"name"`
			Value    string          `json:"value"`
			Readonly json.RawMessage `json:"readonly"`
		}
		if err := dec.Decode(&row); err != nil {
			return nil, &DatabaseError{"ClickHouse returned an invalid settings response"}
		}
		s.serverTZ = row.TZ
		ro := strings.Trim(string(row.Readonly), `"`)
		s.settings[row.Name] = serverSetting{value: row.Value, readonly: ro == "1"}
	}

	// HttpClient.__init__ transport decisions.
	comp := s.status("enable_http_compression")
	s.compress = comp.isSet || comp.writable
	if !comp.isSet && comp.writable {
		s.params.Set("enable_http_compression", "1")
	}
	progress := s.status("send_progress_in_http_headers")
	if !progress.isSet && progress.writable {
		s.params.Set("send_progress_in_http_headers", "1")
	}
	if (progress.isSet || progress.writable) && s.status("http_headers_progress_interval_ms").writable {
		s.params.Set("http_headers_progress_interval_ms", progressIntervalMS)
	}
	s.params.Set("wait_end_of_query", "1")
	// _init_common_settings client settings.
	if s.status("date_time_input_format").writable && s.shouldSend("date_time_input_format", "best_effort", "") {
		s.params.Set("date_time_input_format", "best_effort")
	}
	if s.status("allow_experimental_json_type").isSet && s.status("cast_string_to_dynamic_use_inference").writable &&
		s.shouldSend("cast_string_to_dynamic_use_inference", "1", "") {
		s.params.Set("cast_string_to_dynamic_use_inference", "1")
	}
	// get_client(settings=query_settings) validates the client-level copy.
	for _, qs := range g.cfg.QuerySettings() {
		if s.shouldSend(qs.Name, qs.Value, "") {
			s.params.Set(qs.Name, qs.Value)
		}
	}
	return s, nil
}

type settingStatus struct{ isSet, writable bool }

func (s *session) status(name string) settingStatus {
	def, ok := s.settings[name]
	if !ok {
		return settingStatus{}
	}
	return settingStatus{isSet: def.value != "0", writable: !def.readonly}
}

// shouldSend mirrors BaseClient._validate_setting: settings that
// already have the requested value are not sent, and unknown or readonly
// settings are dropped. clickhouse-connect raised "Setting X is unknown or
// readonly" for the latter, which made every call fail for provisioned
// dataset users (their readonly=1 profile forbids changing settings) unless
// the profile matched the runtime limits exactly; the profile already
// enforces the dataset limits server-side, so the Go client drops them.
func (s *session) shouldSend(name, value, current string) bool {
	def, ok := s.settings[name]
	if !ok || def.readonly {
		return false
	}
	return def.value != value || (current != "" && current != def.value)
}

// utcNames are the zones clickhouse-connect normalizes to naive UTC.
var utcNames = map[string]bool{
	"UTC": true, "Etc/UTC": true, "UCT": true, "Etc/UCT": true, "GMT": true, "Etc/GMT": true,
	"GMT0": true, "GMT+0": true, "GMT-0": true, "Etc/GMT0": true, "Etc/GMT+0": true, "Etc/GMT-0": true,
	"Greenwich": true, "Etc/Greenwich": true, "Universal": true, "Etc/Universal": true,
	"Zulu": true, "Etc/Zulu": true,
}

func (s *session) timeContext(responseTZ string) (timeContext, error) {
	serverUTC := utcNames[s.serverTZ] || strings.EqualFold(s.serverTZ, "UTC")
	active := s.serverTZ
	if responseTZ != "" {
		if serverUTC && !strings.EqualFold(responseTZ, "UTC") || !serverUTC && responseTZ != s.serverTZ {
			active = responseTZ
		} else if serverUTC {
			active = "UTC"
		}
	} else if serverUTC {
		active = "UTC"
	}
	if strings.EqualFold(active, "UTC") || active == "" {
		return timeContext{}, nil
	}
	loc, err := time.LoadLocation(active)
	if err != nil {
		return timeContext{}, nil
	}
	return timeContext{defaultTZ: loc}, nil
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func (g *Gateway) request(ctx context.Context, query string, params url.Values, compress bool) ([]byte, http.Header, error) {
	endpoint := g.url + "/?" + params.Encode()
	var resp *http.Response
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(query))
		if err != nil {
			return nil, nil, &DatabaseError{fmt.Sprintf("Error %s executing HTTP request attempt %d (%s)", err, attempt, g.url)}
		}
		req.Header.Set("Authorization", g.auth)
		req.Header.Set("User-Agent", g.agent)
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		if !compress {
			// Go adds and transparently decodes gzip unless told otherwise.
			req.Header.Set("Accept-Encoding", "identity")
		}
		resp, err = g.http.Do(req)
		if err == nil {
			break
		}
		// clickhouse-connect retries a remotely closed keep-alive connection once.
		if attempt == 1 && isConnectionReset(err) {
			continue
		}
		return nil, nil, &DatabaseError{fmt.Sprintf("Error %s executing HTTP request attempt %d (%s)", err, attempt, g.url)}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Header.Get("X-ClickHouse-Exception-Code") == "" {
		if readErr != nil {
			return nil, nil, &DatabaseError{fmt.Sprintf("Error %s executing HTTP request attempt 1 (%s)", readErr, g.url)}
		}
		return body, resp.Header, nil
	}
	return nil, nil, &DatabaseError{g.errorMessage(resp, body)}
}

func isConnectionReset(err error) bool {
	return strings.Contains(err.Error(), "connection reset by peer")
}

// errorMessage mirrors HttpClient._error_handler with show_clickhouse_errors.
func (g *Gateway) errorMessage(resp *http.Response, body []byte) string {
	text := truncateRunes(backslashReplace(body), maxErrorSize)
	text = pyval.Strip(text)
	var msg string
	if code := resp.Header.Get("X-ClickHouse-Exception-Code"); code != "" {
		msg = "Received ClickHouse exception, code: " + code
	} else {
		msg = "HTTP driver received HTTP status " + strconv.Itoa(resp.StatusCode)
	}
	if text != "" {
		msg += ", server response: " + text
	}
	return msg + " (for url " + g.url + ")"
}

// backslashReplace mirrors bytes.decode(errors="backslashreplace").
func backslashReplace(b []byte) string {
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&sb, `\x%02x`, b[0])
			b = b[1:]
			continue
		}
		sb.WriteRune(r)
		b = b[size:]
	}
	return sb.String()
}

func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
