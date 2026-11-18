package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/config"
)

const allowedOrigin = "https://allowed.example"

// probe mirrors the original isolated ASGI probe: a route returning the
// requested status with Retry-After and X-Test-Header, behind the collector's
// CORS configuration (enabled or not).
func probe(t *testing.T, enabled bool, method, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	origins := ""
	if enabled {
		origins = allowedOrigin
	}
	settings, err := config.Load([]string{
		"CORS_ALLOW_ORIGINS=" + origins, "CORS_ALLOW_METHODS=POST,OPTIONS",
		"CORS_ALLOW_HEADERS=Content-Type,X-API-Key", "CORS_ALLOW_CREDENTIALS=false", "CORS_MAX_AGE_SECONDS=600",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Settings: settings})
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = rec
	if srv.cors != nil {
		var handled bool
		if w, handled = srv.cors.handle(rec, req); handled {
			return rec
		}
	}
	status, _ := strconv.Atoi(strings.TrimPrefix(path, "/test/"))
	w.Header().Set("Retry-After", "30")
	w.Header().Set("X-Test-Header", "unchanged")
	writeText(w, status, "test-response-"+strconv.Itoa(status))
	return rec
}

func tokens(v string) []string {
	out := []string{}
	for _, tok := range strings.Split(v, ",") {
		if tok = strings.ToLower(strings.TrimSpace(tok)); tok != "" {
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

func TestCORSConfigKeepsOptionsAndOnlyExposesRetryAfter(t *testing.T) {
	settings, _ := config.Load([]string{"CORS_ALLOW_ORIGINS=" + allowedOrigin}, "")
	c := New(Options{Settings: settings}).cors
	want := newCORS(CORSConfig{
		AllowOrigins: []string{allowedOrigin}, AllowMethods: []string{"POST", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "X-API-Key"}, MaxAge: 600, ExposeHeaders: []string{"Retry-After"},
	})
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
}

func TestAllowedErrorResponsesExposeOnlyRetryAfter(t *testing.T) {
	for _, status := range []int{429, 503} {
		rec := probe(t, true, http.MethodPost, "/test/"+strconv.Itoa(status), "Origin", allowedOrigin)
		h := rec.Header()
		if rec.Code != status || rec.Body.String() != "test-response-"+strconv.Itoa(status) ||
			h.Get("Retry-After") != "30" || h.Get("X-Test-Header") != "unchanged" ||
			h.Get("Access-Control-Allow-Origin") != allowedOrigin ||
			!reflect.DeepEqual(tokens(h.Get("Access-Control-Expose-Headers")), []string{"retry-after"}) {
			t.Errorf("%d: %d %v", status, rec.Code, h)
		}
	}
}

func TestDisallowedOriginGetsNoAllowOrigin(t *testing.T) {
	rec := probe(t, true, http.MethodPost, "/test/503", "Origin", "https://evil.example")
	h := rec.Header()
	if rec.Code != 503 || rec.Body.String() != "test-response-503" || h.Get("Retry-After") != "30" ||
		h.Get("Access-Control-Allow-Origin") != "" ||
		!reflect.DeepEqual(tokens(h.Get("Access-Control-Expose-Headers")), []string{"retry-after"}) {
		t.Errorf("%v", h)
	}
}

func TestNoOriginResponseOnlyGainsVary(t *testing.T) {
	enabled := probe(t, true, http.MethodPost, "/test/429")
	disabled := probe(t, false, http.MethodPost, "/test/429")
	if enabled.Code != 429 || enabled.Body.String() != disabled.Body.String() || enabled.Code != disabled.Code {
		t.Fatalf("%d %q", enabled.Code, enabled.Body.String())
	}
	for name, values := range disabled.Header() {
		if !reflect.DeepEqual(enabled.Header()[name], values) {
			t.Errorf("header %s changed", name)
		}
	}
	extra := []string{}
	for name := range enabled.Header() {
		if _, ok := disabled.Header()[name]; !ok {
			extra = append(extra, name)
		}
	}
	if !reflect.DeepEqual(extra, []string{"Vary"}) || !reflect.DeepEqual(tokens(enabled.Header().Get("Vary")), []string{"origin"}) {
		t.Errorf("extra headers %v vary %q", extra, enabled.Header().Get("Vary"))
	}
	if enabled.Header().Get("Access-Control-Allow-Origin") != "" || enabled.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Error("CORS access headers on a request without Origin")
	}
}

func TestPreflightResponse(t *testing.T) {
	rec := probe(t, true, http.MethodOptions, "/test/418", "Origin", allowedOrigin,
		"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type,x-api-key")
	h := rec.Header()
	if rec.Code != 200 || rec.Body.String() != "OK" || h.Get("Access-Control-Allow-Origin") != allowedOrigin ||
		!reflect.DeepEqual(tokens(h.Get("Access-Control-Allow-Methods")), []string{"options", "post"}) ||
		!reflect.DeepEqual(tokens(h.Get("Access-Control-Allow-Headers")),
			[]string{"accept", "accept-language", "content-language", "content-type", "x-api-key"}) ||
		h.Get("Access-Control-Max-Age") != "600" || h.Get("Retry-After") != "" || h.Get("Access-Control-Expose-Headers") != "" {
		t.Errorf("%d %q %v", rec.Code, rec.Body.String(), h)
	}
}

func TestCORSDisabledDoesNotInterceptPreflight(t *testing.T) {
	settings, _ := config.Load(nil, "")
	if New(Options{Settings: settings}).cors != nil {
		t.Fatal("CORS enabled without origins")
	}
	rec := probe(t, false, http.MethodOptions, "/test/418", "Origin", allowedOrigin,
		"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type,x-api-key")
	h := rec.Header()
	if rec.Code != 418 || rec.Body.String() != "test-response-418" || h.Get("Retry-After") != "30" ||
		h.Get("X-Test-Header") != "unchanged" || h.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("%d %v", rec.Code, h)
	}
}
