package geoip

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
)

type fakeReader struct{ record any }

func (f fakeReader) Lookup(netip.Addr) (any, error) { return f.record, nil }
func (f fakeReader) Close() error                   { return nil }

func tempFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.mmdb")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolverWith(t *testing.T, record any) *Resolver {
	return NewResolver(true, tempFile(t), func(string) (Reader, error) { return fakeReader{record}, nil },
		logx.New(&bytes.Buffer{}, logx.LevelDebug))
}

func TestClientIPPrefersForwardedFor(t *testing.T) {
	if got := ClientIP("203.0.113.8, 10.0.0.1", "198.51.100.4", "172.16.0.2"); got != "203.0.113.8" {
		t.Fatal(got)
	}
}

func TestClientIPFallsBackToRealIPThenSocket(t *testing.T) {
	if got := ClientIP("unknown", "198.51.100.4", "172.16.0.2"); got != "198.51.100.4" {
		t.Fatal(got)
	}
	if got := ClientIP("", "", "172.16.0.2"); got != "172.16.0.2" {
		t.Fatal(got)
	}
}

func TestLookupCountryReturnsNormalizedPayload(t *testing.T) {
	r := resolverWith(t, map[string]any{"country": map[string]any{"name": "Finland", "iso_code": "fi"}})
	if c, ok := r.LookupCountry("203.0.113.8"); !ok || c != (Country{Name: "Finland", Code: "FI"}) {
		t.Fatal(c, ok)
	}
}

func TestLookupCountrySupportsFlatRecordShape(t *testing.T) {
	r := resolverWith(t, map[string]any{"country_name": "Finland", "country_code": "fi"})
	if c, ok := r.LookupCountry("203.0.113.8"); !ok || c != (Country{Name: "Finland", Code: "FI"}) {
		t.Fatal(c, ok)
	}
}

func TestLookupCountryFailsOpenWhenMMDBMissing(t *testing.T) {
	var logs bytes.Buffer
	r := NewResolver(true, "/definitely/missing.mmdb", nil, logx.New(&logs, logx.LevelDebug))
	if _, ok := r.LookupCountry("203.0.113.8"); ok {
		t.Fatal("expected no result")
	}
	if !strings.Contains(logs.String(), "geoip_unavailable reason=missing_mmdb path=/definitely/missing.mmdb") {
		t.Fatal(logs.String())
	}
}

func TestRecordShapes(t *testing.T) {
	for _, tc := range []struct {
		record any
		want   Country
		ok     bool
	}{
		{nil, Country{}, false},
		{"not a map", Country{}, false},
		{map[string]any{}, Country{}, false},
		{map[string]any{"country": map[string]any{"iso_code": " se "}}, Country{Code: "SE"}, true},
		{map[string]any{"country": map[string]any{"name": " "}, "country_name": "Sweden"}, Country{Name: "Sweden"}, true},
		{map[string]any{"country": "SE", "country_code": "se"}, Country{Code: "SE"}, true},
		{map[string]any{"country_name": uint64(7), "country_code": false}, Country{Name: "7"}, true},
	} {
		got, ok := countryFromRecord(tc.record)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%v: got %+v %v", tc.record, got, ok)
		}
	}
}

// TestMaxMindTestDatabase uses MaxMind's GeoIP2-Country test database. Its
// records carry country.iso_code and country.names but no country.name, so
// only the code is enriched, as with the original service.
func TestMaxMindTestDatabase(t *testing.T) {
	r := NewResolver(true, "testdata/GeoIP2-Country-Test.mmdb", nil, logx.New(&bytes.Buffer{}, logx.LevelDebug))
	defer r.Close()
	for ip, want := range map[string]Country{
		"81.2.69.160":        {Code: "GB"},
		"::ffff:81.2.69.160": {Code: "GB"},
		"89.160.20.112":      {Code: "SE"},
		"2001:218::1":        {Code: "JP"},
	} {
		if got, ok := r.LookupCountry(ip); !ok || got != want {
			t.Errorf("%s: %+v %v", ip, got, ok)
		}
	}
	if _, ok := r.LookupCountry("10.0.0.1"); ok {
		t.Error("private address resolved")
	}
}

func TestIPv6InIPv4DatabaseLogsLookupFailure(t *testing.T) {
	var logs bytes.Buffer
	r := NewResolver(true, "testdata/MaxMind-DB-test-ipv4-24.mmdb", nil, logx.New(&logs, logx.LevelDebug))
	defer r.Close()
	if _, ok := r.LookupCountry("2001:db8::1"); ok {
		t.Fatal("unexpected result")
	}
	if !strings.Contains(logs.String(), "geoip_lookup_failed error_class=ValueError") {
		t.Fatal(logs.String())
	}
}

func TestInvalidDatabaseFailsOpen(t *testing.T) {
	var logs bytes.Buffer
	path := tempFile(t)
	r := NewResolver(true, path, nil, logx.New(&logs, logx.LevelDebug))
	r.Warmup()
	if _, ok := r.LookupCountry("81.2.69.160"); ok {
		t.Fatal("unexpected result")
	}
	if !strings.Contains(logs.String(), "reason=reader_open_failed path="+path+" error_class=InvalidDatabaseError") {
		t.Fatal(logs.String())
	}
}

func TestDisabledResolver(t *testing.T) {
	var logs bytes.Buffer
	r := NewResolver(false, "testdata/GeoIP2-Country-Test.mmdb", nil, logx.New(&logs, logx.LevelDebug))
	r.Warmup()
	if _, ok := r.LookupCountry("81.2.69.160"); ok || !strings.Contains(logs.String(), "geoip_disabled reason=feature_flag_off") {
		t.Fatal(logs.String())
	}
	logs.Reset()
	r = NewResolver(true, "", nil, logx.New(&logs, logx.LevelDebug))
	r.Warmup()
	if !strings.Contains(logs.String(), "geoip_disabled reason=missing_path") {
		t.Fatal(logs.String())
	}
}
