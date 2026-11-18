package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/config"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/geoip"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/publisher"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/schema"
)

type publisherStub struct {
	mu       sync.Mutex
	calls    int
	messages []publisher.Message
	err      error
}

func (p *publisherStub) PublishBatch(_ context.Context, messages []publisher.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.messages = messages
	return p.err
}

type resolverStub struct {
	country geoip.Country
	ok      bool
	calls   int
}

func (r *resolverStub) LookupCountry(string) (geoip.Country, bool) {
	r.calls++
	return r.country, r.ok
}

type harness struct {
	t      *testing.T
	srv    *Server
	pub    *publisherStub
	geo    *resolverStub
	logs   *bytes.Buffer
	config *config.Settings
}

func newHarness(t *testing.T, env ...string) *harness {
	t.Helper()
	settings, err := config.Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pub: &publisherStub{}, geo: &resolverStub{}, logs: &bytes.Buffer{}, config: settings}
	h.srv = New(Options{
		Settings:          settings,
		Log:               logx.New(h.logs, logx.LevelDebug),
		Publisher:         h.pub,
		GeoIP:             h.geo,
		ForwardedAllowIPs: "127.0.0.1",
	})
	return h
}

func (h *harness) post(body io.Reader, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events:batch", body)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

func requiredEvent() map[string]any {
	return map[string]any{
		"event_id":        "evt-asgi",
		"app_id":          "com.example.game",
		"event_name":      "level_completed",
		"event_timestamp": "2026-03-20T18:42:15.123Z",
	}
}

func requestBody(t *testing.T, events ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type detailItem map[string]any

func details(t *testing.T, rec *httptest.ResponseRecorder) []detailItem {
	t.Helper()
	var payload map[string][]detailItem
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("422 body %q: %v", rec.Body.String(), err)
	}
	if len(payload) != 1 {
		t.Fatalf("422 body has keys other than detail: %s", rec.Body.String())
	}
	for _, d := range payload["detail"] {
		if len(d) != 3 || d["type"] == nil || d["loc"] == nil || d["msg"] == nil {
			t.Fatalf("detail entry is not exactly type/loc/msg: %v", d)
		}
	}
	return payload["detail"]
}

func TestMalformedJSONReturns422WithoutBodyLogging(t *testing.T) {
	for _, body := range []string{"{", "not json", `{"events": [}`, "\xff", `{"events":[`} {
		t.Run(body, func(t *testing.T) {
			h := newHarness(t)
			rec := h.post(iotest.OneByteReader(strings.NewReader(body)))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d", rec.Code)
			}
			for _, d := range details(t, rec) {
				if d["msg"] != "Invalid JSON" {
					t.Errorf("msg %v", d["msg"])
				}
			}
			logs := h.logs.String()
			if !strings.Contains(logs, "WARNING") || strings.Contains(logs, "Traceback") {
				t.Errorf("logs: %s", logs)
			}
			if decoded := strings.ToValidUTF8(body, ""); decoded != "" && strings.Contains(logs, decoded) {
				t.Errorf("logs reflect the body: %s", logs)
			}
		})
	}
}

func TestSchema422NeverReflectsBodyOrPrivateFields(t *testing.T) {
	const sentinel = "private-validation-sentinel-7d38"
	ev := requiredEvent()
	delete(ev, "event_id")
	ev["user"] = map[string]any{"user_id": sentinel}
	h := newHarness(t)
	// No API key: if authentication ran first the status would be 401.
	rec := h.post(bytes.NewReader(requestBody(t, ev)), "X-API-Key", "not-used")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	text := rec.Body.String()
	if strings.Contains(text, sentinel) || strings.Contains(h.logs.String(), sentinel) {
		t.Fatal("sentinel reflected")
	}
	d := details(t, rec)
	if d[0]["type"] != "missing" {
		t.Errorf("type %v", d[0]["type"])
	}
	for _, field := range []string{`"input"`, `"ctx"`, `"url"`} {
		if strings.Contains(text, field) {
			t.Errorf("body contains %s", field)
		}
	}
	if h.pub.calls != 0 {
		t.Error("published")
	}
}

func TestInvalidTypeAndTimestampMessagesDoNotReflectValues(t *testing.T) {
	const sentinel = "private-invalid-value-sentinel-945e"
	badType := requiredEvent()
	badType["app_id"] = map[string]any{"user_value": sentinel}
	badTimestamp := requiredEvent()
	badTimestamp["event_timestamp"] = sentinel
	for _, ev := range []map[string]any{badType, badTimestamp} {
		h := newHarness(t)
		rec := h.post(bytes.NewReader(requestBody(t, ev)))
		if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), sentinel) {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		details(t, rec)
	}
}

func TestValidationErrorCountIsBoundedWithTruncationMarker(t *testing.T) {
	for _, tc := range []struct {
		events, want int
		truncated    bool
	}{{1, 4, false}, {5, 20, false}, {6, 20, true}} {
		t.Run(fmt.Sprint(tc.events), func(t *testing.T) {
			events := make([]map[string]any, tc.events)
			for i := range events {
				events[i] = map[string]any{}
			}
			rec := newHarness(t).post(bytes.NewReader(requestBody(t, events...)))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d", rec.Code)
			}
			d := details(t, rec)
			if len(d) != tc.want {
				t.Fatalf("got %d errors", len(d))
			}
			rest := d
			if tc.truncated {
				last := d[len(d)-1]
				if last["type"] != "errors_truncated" || last["msg"] != "Additional validation errors omitted" ||
					len(last["loc"].([]any)) != 0 {
					t.Errorf("marker %v", last)
				}
				rest = d[:len(d)-1]
			}
			for _, e := range rest {
				if e["type"] != "missing" {
					t.Errorf("type %v", e["type"])
				}
			}
		})
	}
}

func TestValidationPrecedenceAndUnrelatedStatuses(t *testing.T) {
	h := newHarness(t, "MAX_REQUEST_BYTES=4")
	if rec := h.post(strings.NewReader(`{"events": [`)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", rec.Code)
	}
	h = newHarness(t, "MAX_REQUEST_BYTES=1")
	if rec := h.post(iotest.OneByteReader(strings.NewReader("{"))); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("at limit: %d", rec.Code)
	}
	h = newHarness(t, "MAX_REQUEST_BYTES=0")
	if rec := h.post(iotest.OneByteReader(strings.NewReader("{"))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over limit: %d", rec.Code)
	}
	h = newHarness(t)
	rec := h.post(strings.NewReader(`{"events":[]}`))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"detail":"events cannot be empty"}` {
		t.Errorf("empty: %d %s", rec.Code, rec.Body.String())
	}
	h = newHarness(t, "MAX_EVENTS_PER_REQUEST=1")
	rec = h.post(bytes.NewReader(requestBody(t, requiredEvent(), requiredEvent())))
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != `{"detail":"events limit exceeded: 1"}` {
		t.Errorf("too many: %d %s", rec.Code, rec.Body.String())
	}
	h = newHarness(t)
	if rec := h.post(strings.NewReader("{")); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("malformed before auth: %d", rec.Code)
	}
}

func TestValidRequestAuthAndPublishFailureStatuses(t *testing.T) {
	keys := `COLLECTOR_API_KEYS_JSON={"valid-key":"com.example.game"}`
	h := newHarness(t, keys)
	rec := h.post(bytes.NewReader(requestBody(t, requiredEvent())), "X-API-Key", "valid-key")
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp["accepted_events"] != float64(1) || h.pub.calls != 1 {
		t.Fatalf("success: %d %s calls=%d", rec.Code, rec.Body.String(), h.pub.calls)
	}

	h = newHarness(t)
	if rec := h.post(bytes.NewReader(requestBody(t, requiredEvent()))); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthorized: %d", rec.Code)
	}

	h = newHarness(t, keys)
	h.pub.err = &publisher.BatchPublishError{Failed: 1, FirstError: errors.New("publish failed")}
	rec = h.post(bytes.NewReader(requestBody(t, requiredEvent())), "X-API-Key", "valid-key")
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"detail":"Failed to publish events"}` {
		t.Errorf("publish failure: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthorization(t *testing.T) {
	keys := map[string]string{"k1": "app", "k2": "other", "k3": ""}
	for _, tc := range []struct {
		key, app string
		status   int
		detail   string
	}{
		{"", "app", 401, "Missing X-API-Key"},
		{"nope", "app", 401, "Invalid API key"},
		{"k3", "", 401, "Invalid API key"},
		{"k2", "app", 403, "API key is not allowed for this app_id"},
		{"k1", "app", 0, ""},
	} {
		status, detail, _ := authorize(keys, tc.key, tc.app)
		if status != tc.status || detail != tc.detail {
			t.Errorf("%+v: got %d %q", tc, status, detail)
		}
	}
}

// --- enrichment

func eventFrom(t *testing.T, overrides map[string]any) *schema.Event {
	t.Helper()
	ev := map[string]any{
		"event_id": "evt-1", "app_id": "com.example.game", "event_name": "level_completed",
		"event_timestamp": "2026-03-20T18:42:15.123Z",
	}
	for k, v := range overrides {
		ev[k] = v
	}
	events, errs := schema.ParseBatch(requestBody(t, ev))
	if errs != nil {
		t.Fatalf("invalid test event: %v", errs)
	}
	return &events[0]
}

func metaFor(h *harness, remote string, headers ...string) requestMeta {
	req := httptest.NewRequest(http.MethodPost, "/v1/events:batch", nil)
	req.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	return h.srv.requestMeta(req, h.srv.connection(req))
}

var isoUTC = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{6})?\+00:00$`)

func enriched(t *testing.T, h *harness, ev *schema.Event, meta requestMeta, requestID string) map[string]any {
	t.Helper()
	before := time.Now().UTC().Truncate(time.Microsecond)
	out := pyjson.Encode(h.srv.enrich(ev, meta, requestID))
	after := time.Now().UTC()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	ingest := m["ingest"].(map[string]any)
	top, nested := m["received_at"].(string), ingest["received_at"].(string)
	if top != nested || !isoUTC.MatchString(top) {
		t.Fatalf("received_at %q vs %q", top, nested)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.999999-07:00", top)
	if err != nil || parsed.Before(before) || parsed.After(after) {
		t.Fatalf("received_at %q not fresh (%v)", top, err)
	}
	return m
}

func TestOmittedNullEmptyAndAllNullIngestGetServerMetadata(t *testing.T) {
	ip := "203.0.113.8"
	for name, overrides := range map[string]map[string]any{
		"omitted":  {},
		"null":     {"ingest": nil},
		"empty":    {"ingest": map[string]any{}},
		"all-null": {"ingest": map[string]any{"user_agent": nil, "ip_hash": nil, "ip": nil, "received_at": nil, "request_id": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "STORE_RAW_IP=0", "IP_HASH_SALT=test-salt")
			meta := metaFor(h, "10.0.0.1:1", "User-Agent", "collector-test/1.0", "X-Forwarded-For", ip)
			m := enriched(t, h, eventFrom(t, overrides), meta, "batch-req-1")
			ingest := m["ingest"].(map[string]any)
			if ingest["request_id"] != "batch-req-1" || ingest["user_agent"] != "collector-test/1.0" ||
				ingest["ip_hash"] != HashIP(ip, "test-salt") {
				t.Errorf("ingest %v", ingest)
			}
			if _, ok := ingest["ip"]; ok {
				t.Error("ip present")
			}
		})
	}
}

func TestSpoofedMetadataIsOverwrittenAndOtherDataPreserved(t *testing.T) {
	ip := "203.0.113.19"
	spoofed := "2001-01-01T00:00:00+00:00"
	h := newHarness(t, "STORE_RAW_IP=1", "IP_HASH_SALT=test-salt")
	h.geo.country, h.geo.ok = geoip.Country{Name: "Finland", Code: "FI"}, true
	ev := eventFrom(t, map[string]any{
		"received_at": spoofed,
		"ingest": map[string]any{"user_agent": "spoofed-agent", "ip_hash": "spoofed-hash", "ip": "198.51.100.44",
			"received_at": spoofed, "request_id": "spoofed-request"},
		"geo":          map[string]any{"country": "Wrong", "country_code": "WR", "city": "Oulu"},
		"event_params": map[string]any{"level": 17},
	})
	meta := metaFor(h, "10.0.0.1:1", "User-Agent", "real-agent/2.0", "X-Forwarded-For", ip)
	m := enriched(t, h, ev, meta, "server-request")
	ingest := m["ingest"].(map[string]any)
	if ingest["request_id"] != "server-request" || ingest["user_agent"] != "real-agent/2.0" ||
		ingest["ip_hash"] != HashIP(ip, "test-salt") || ingest["ip"] != ip || m["received_at"] == spoofed {
		t.Errorf("ingest %v", ingest)
	}
	wantGeo := map[string]any{"country": "Finland", "country_code": "FI", "city": "Oulu", "region": nil, "timezone": nil}
	if fmt.Sprint(m["geo"]) != fmt.Sprint(wantGeo) {
		t.Errorf("geo %v", m["geo"])
	}
	if fmt.Sprint(m["event_params"]) != fmt.Sprint(map[string]any{"level": float64(17)}) {
		t.Errorf("event_params %v", m["event_params"])
	}
}

func TestRawIPSettingControlsWhetherIPIsPublished(t *testing.T) {
	ip := "203.0.113.22"
	for _, store := range []string{"1", "0"} {
		h := newHarness(t, "STORE_RAW_IP="+store, "IP_HASH_SALT=raw-ip-test")
		m := enriched(t, h, eventFrom(t, map[string]any{"ingest": map[string]any{"ip": "198.51.100.1"}}),
			metaFor(h, "10.0.0.1:1", "X-Forwarded-For", ip), "request-1")
		ingest := m["ingest"].(map[string]any)
		got, present := ingest["ip"]
		if (store == "1") != present || (present && got != ip) {
			t.Errorf("store=%s ingest %v", store, ingest)
		}
		if ingest["ip_hash"] != HashIP(ip, "raw-ip-test") {
			t.Errorf("hash %v", ingest["ip_hash"])
		}
	}
}

func TestIPHashIsDerivedFromValidRequestIP(t *testing.T) {
	ip := "203.0.113.23"
	for _, store := range []string{"1", "0"} {
		h := newHarness(t, "STORE_RAW_IP="+store, "IP_HASH_SALT=hash-test")
		m := enriched(t, h, eventFrom(t, map[string]any{"ingest": map[string]any{"ip_hash": "spoofed-hash"}}),
			metaFor(h, "10.0.0.1:1", "X-Forwarded-For", ip), "request-1")
		if m["ingest"].(map[string]any)["ip_hash"] != HashIP(ip, "hash-test") {
			t.Errorf("store=%s: %v", store, m["ingest"])
		}
	}
}

func TestMissingIPDoesNotFallBackToBodyIPMetadata(t *testing.T) {
	h := newHarness(t, "STORE_RAW_IP=1", "IP_HASH_SALT=test-salt")
	h.geo.country, h.geo.ok = geoip.Country{Name: "Finland"}, true
	ev := eventFrom(t, map[string]any{"ingest": map[string]any{"ip_hash": "spoofed-hash", "ip": "198.51.100.20"}})
	m := enriched(t, h, ev, metaFor(h, "not-an-ip:1"), "server-request")
	ingest := m["ingest"].(map[string]any)
	if ingest["ip_hash"] != nil {
		t.Errorf("ip_hash %v", ingest["ip_hash"])
	}
	if _, ok := ingest["ip"]; ok {
		t.Error("ip present")
	}
	if m["geo"] != nil || h.geo.calls != 0 {
		t.Errorf("geo %v calls=%d", m["geo"], h.geo.calls)
	}
}

func TestMissingUserAgentOverwritesSpoofedValue(t *testing.T) {
	h := newHarness(t)
	m := enriched(t, h, eventFrom(t, map[string]any{"ingest": map[string]any{"user_agent": "spoofed-agent"}}),
		metaFor(h, "10.0.0.1:1"), "server-request")
	if ua, ok := m["ingest"].(map[string]any)["user_agent"]; !ok || ua != nil {
		t.Errorf("user_agent %v", ua)
	}
}

func TestBatchResponseIDIsUsedForEveryEnrichedEvent(t *testing.T) {
	ip := "203.0.113.28"
	spoofed := "2001-01-01T00:00:00+00:00"
	h := newHarness(t, `COLLECTOR_API_KEYS_JSON={"test-key":"com.example.game"}`, "STORE_RAW_IP=1")
	var events []map[string]any
	for i := 0; i < 2; i++ {
		events = append(events, map[string]any{
			"event_id": fmt.Sprintf("evt-%d", i), "app_id": "com.example.game", "event_name": "level_completed",
			"event_timestamp": "2026-03-20T18:42:15.123Z", "received_at": spoofed,
			"ingest": map[string]any{"request_id": "client-request", "received_at": spoofed, "user_agent": "client-agent",
				"ip_hash": "client-hash", "ip": "198.51.100.10"},
		})
	}
	rec := h.post(bytes.NewReader(requestBody(t, events...)), "X-API-Key", "test-key",
		"User-Agent", "batch-agent/1.0", "X-Forwarded-For", ip)
	var resp struct {
		RequestID      string `json:"request_id"`
		AcceptedEvents int    `json:"accepted_events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.AcceptedEvents != 2 || h.pub.calls != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(h.pub.messages) != 2 {
		t.Fatalf("messages %d", len(h.pub.messages))
	}
	for _, msg := range h.pub.messages {
		var m map[string]any
		if err := json.Unmarshal(msg.Payload, &m); err != nil {
			t.Fatal(err)
		}
		ingest := m["ingest"].(map[string]any)
		if ingest["request_id"] != resp.RequestID || ingest["user_agent"] != "batch-agent/1.0" ||
			ingest["ip"] != ip || m["received_at"] == spoofed || m["received_at"] != ingest["received_at"] {
			t.Errorf("message %s", msg.Payload)
		}
	}
}

func TestInvalidIngestMetadataTypesReturn422WithoutPublishing(t *testing.T) {
	for name, overrides := range map[string]map[string]any{
		"ingest is not an object":    {"ingest": []any{"invalid"}},
		"request ID is not a string": {"ingest": map[string]any{"request_id": 123}},
		"receipt time is invalid":    {"received_at": "not-a-timestamp"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, `COLLECTOR_API_KEYS_JSON={"test-key":"com.example.game"}`)
			ev := requiredEvent()
			for k, v := range overrides {
				ev[k] = v
			}
			rec := h.post(bytes.NewReader(requestBody(t, ev)), "X-API-Key", "test-key")
			if rec.Code != http.StatusUnprocessableEntity || h.pub.calls != 0 {
				t.Errorf("%d calls=%d", rec.Code, h.pub.calls)
			}
		})
	}
}

func TestGeoIPOverridesCountryFieldsAndPreservesOtherGeoFields(t *testing.T) {
	h := newHarness(t)
	h.geo.country, h.geo.ok = geoip.Country{Name: "Finland", Code: "FI"}, true
	ev := eventFrom(t, map[string]any{"geo": map[string]any{"country": "Wrong", "country_code": "WR", "city": "Oulu"}})
	m := enriched(t, h, ev, metaFor(h, "10.0.0.1:1", "X-Forwarded-For", "203.0.113.8"), "req-1")
	geo := m["geo"].(map[string]any)
	if geo["country"] != "Finland" || geo["country_code"] != "FI" || geo["city"] != "Oulu" {
		t.Errorf("geo %v", geo)
	}
}

func TestMessageIDsAndSubjects(t *testing.T) {
	h := newHarness(t, `COLLECTOR_API_KEYS_JSON={"k1":"com.example.game"}`)
	var events []map[string]any
	for i := 0; i < 5; i++ {
		ev := requiredEvent()
		ev["event_id"] = fmt.Sprintf("evt-%d", i)
		events = append(events, ev)
	}
	if rec := h.post(bytes.NewReader(requestBody(t, events...)), "X-API-Key", "k1"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	for i, m := range h.pub.messages {
		if m.MsgID != fmt.Sprintf("com.example.game:evt-%d", i) || m.Subject != "events.com.example.game" {
			t.Errorf("message %d: %s %s", i, m.Subject, m.MsgID)
		}
	}
}

func TestPublishFailureLogsCounts(t *testing.T) {
	h := newHarness(t, `COLLECTOR_API_KEYS_JSON={"k1":"com.example.game"}`)
	h.pub.err = &publisher.BatchPublishError{Published: 4, Failed: 1, FirstError: context.DeadlineExceeded}
	if rec := h.post(bytes.NewReader(requestBody(t, requiredEvent())), "X-API-Key", "k1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(h.logs.String(), "published_events=4 failed_events=1 error_class=TimeoutError") {
		t.Errorf("logs: %s", h.logs.String())
	}
}

// --- utils

func TestHashIPIsStable(t *testing.T) {
	if HashIP("1.2.3.4", "salt") != HashIP("1.2.3.4", "salt") {
		t.Fatal("unstable")
	}
}

func TestSubjectForApp(t *testing.T) {
	if got := SubjectForApp("events", "com.example.app"); got != "events.com.example.app" {
		t.Fatal(got)
	}
}

func TestConcurrentRequests(t *testing.T) {
	h := newHarness(t, `COLLECTOR_API_KEYS_JSON={"k1":"com.example.game"}`)
	resolver := geoip.NewResolver(true, "../geoip/testdata/GeoIP2-Country-Test.mmdb", nil, logx.New(io.Discard, 0))
	defer resolver.Close()
	h.srv.opts.GeoIP = resolver
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := requiredEvent()
			ev["event_id"] = fmt.Sprintf("evt-%d", i)
			req := httptest.NewRequest(http.MethodPost, "/v1/events:batch", bytes.NewReader(requestBody(t, ev)))
			req.Header.Set("X-API-Key", "k1")
			req.Header.Set("X-Forwarded-For", "81.2.69.160")
			rec := httptest.NewRecorder()
			h.srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status %d", rec.Code)
			}
		}(i)
	}
	wg.Wait()
	if h.pub.calls != 64 {
		t.Errorf("calls %d", h.pub.calls)
	}
}
