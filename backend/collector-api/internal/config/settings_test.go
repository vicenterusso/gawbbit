package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeoIPSettingsParseFromEnv(t *testing.T) {
	s, err := Load([]string{"GEOIP_ENABLED=1", "GEOIP_MMDB_PATH=/var/lib/geoip/dbip-country-lite.mmdb"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.GeoIPEnabled || s.GeoIPMMDBPath != "/var/lib/geoip/dbip-country-lite.mmdb" {
		t.Fatalf("%+v", s)
	}
}

func TestDefaults(t *testing.T) {
	s, err := Load(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Env != "local" || s.Port != 8080 || s.LogLevel != "INFO" || s.NATSURL != "nats://localhost:4222" ||
		s.NATSStream != "EVENTS" || s.NATSSubjectPrefix != "events" || s.NATSStreamMaxAgeSeconds != 604800 ||
		s.NATSDuplicateWindowSeconds != 600 || s.NATSMaxInflightPublishes != 512 || s.MaxEventsPerRequest != 500 ||
		s.MaxRequestBytes != 1048576 || s.IPHashSalt != "change-me" || s.StoreRawIP || s.GeoIPEnabled ||
		s.CORSAllowMethods != "POST,OPTIONS" || s.CORSAllowHeaders != "Content-Type,X-API-Key" ||
		s.CORSAllowCredentials || s.CORSMaxAgeSeconds != 600 || len(s.APIKeyMap) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDotenvFileIsReadAndEnvironmentWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nexport PORT=9001\nIP_HASH_SALT='single # kept'\nENV=\"dev\\nline\" # trailing\n" +
		"NATS_STREAM=S1 # comment\nNATS_SUBJECT_PREFIX=${NATS_STREAM}_x\nGEOIP_MMDB_PATH=${UNSET:-/d.mmdb}\nBARE\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load([]string{"nats_stream=FROM_ENV"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != 9001 || s.IPHashSalt != "single # kept" || s.Env != "dev\nline" || s.NATSStream != "FROM_ENV" ||
		s.NATSSubjectPrefix != "S1_x" || s.GeoIPMMDBPath != "/d.mmdb" {
		t.Fatalf("%+v", s)
	}
}

func TestValidationErrorText(t *testing.T) {
	_, err := Load([]string{"PORT=abc"}, "")
	want := "Invalid collector settings: 1 validation error for Settings\nPORT\n  Input should be a valid integer, unable to parse string as an integer [type=int_parsing, input_value='abc', input_type=str]\n    For further information visit https://errors.pydantic.dev/2.12/v/int_parsing"
	if err == nil || err.Error() != want {
		t.Fatalf("%v", err)
	}
}
