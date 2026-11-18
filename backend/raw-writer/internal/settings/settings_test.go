package settings

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func s3(overrides map[string]string) map[string]string {
	env := map[string]string{
		"RAW_STORAGE_BACKEND": "s3",
		"S3_ENDPOINT_URL":     "http://seaweedfs:8333",
		"S3_BUCKET":           "events",
		"S3_ACCESS_KEY":       "key",
		"S3_SECRET_KEY":       "secret",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

func mustLoad(t *testing.T, env map[string]string) *Settings {
	t.Helper()
	s, err := FromMap(env)
	if err != nil {
		t.Fatalf("FromMap(%q): %v", env, err)
	}
	return s
}

func mustFail(t *testing.T, env map[string]string, want string) {
	t.Helper()
	_, err := FromMap(env)
	if err == nil {
		t.Fatalf("FromMap(%v) succeeded", env)
	}
	if want != "" && err.Error() != "Invalid raw-writer settings: "+want {
		t.Fatalf("error %q, want %q", err, want)
	}
}

func TestGCSBackendRequiresBucket(t *testing.T) {
	mustFail(t, map[string]string{"RAW_STORAGE_BACKEND": "gcs", "GCS_RAW_BUCKET": ""},
		"Value error, GCS_RAW_BUCKET is required when RAW_STORAGE_BACKEND=gcs")
}

func TestGCSBackendAllowsBucket(t *testing.T) {
	if s := mustLoad(t, map[string]string{"RAW_STORAGE_BACKEND": "gcs", "GCS_RAW_BUCKET": "bucket-a"}); s.RawStorageBackend != "gcs" {
		t.Fatal(s.RawStorageBackend)
	}
}

func TestBackendSelectorIsTrimmedAndLowercased(t *testing.T) {
	if s := mustLoad(t, s3(map[string]string{"RAW_STORAGE_BACKEND": " S3 "})); s.RawStorageBackend != "s3" {
		t.Fatal(s.RawStorageBackend)
	}
}

func TestGCSBackendRejectsWhitespaceBucket(t *testing.T) {
	mustFail(t, map[string]string{"RAW_STORAGE_BACKEND": "gcs", "GCS_RAW_BUCKET": "   "}, "")
}

func TestS3BackendRequiresCoreFields(t *testing.T) {
	mustFail(t, map[string]string{"RAW_STORAGE_BACKEND": "s3", "S3_BUCKET": "events"},
		"Value error, Missing required S3 settings when RAW_STORAGE_BACKEND=s3: S3_ENDPOINT_URL, S3_ACCESS_KEY, S3_SECRET_KEY")
}

func TestS3BackendAllowsRequiredFields(t *testing.T) {
	s := mustLoad(t, s3(nil))
	if s.RawStorageBackend != "s3" || s.S3Region != "us-east-1" {
		t.Fatalf("%+v", s)
	}
}

func TestS3BackendRejectsWhitespaceRequiredFields(t *testing.T) {
	mustFail(t, s3(map[string]string{"S3_ENDPOINT_URL": "   "}), "")
}

func TestFlushIntervalMustBeBelowAckWait(t *testing.T) {
	mustFail(t, s3(map[string]string{"RAW_FLUSH_INTERVAL_SECONDS": "300", "NATS_ACK_WAIT_SECONDS": "300"}),
		"Value error, RAW_FLUSH_INTERVAL_SECONDS must be lower than NATS_ACK_WAIT_SECONDS (got 300 >= 300); otherwise buffered messages are redelivered before they are flushed")
	mustFail(t, s3(map[string]string{"RAW_FLUSH_INTERVAL_SECONDS": "900", "NATS_ACK_WAIT_SECONDS": "120"}), "")
	// The timing check runs before the backend check, like the original validators.
	mustFail(t, map[string]string{"RAW_STORAGE_BACKEND": "x", "RAW_FLUSH_INTERVAL_SECONDS": "200"},
		"Value error, RAW_FLUSH_INTERVAL_SECONDS must be lower than NATS_ACK_WAIT_SECONDS (got 200 >= 120); otherwise buffered messages are redelivered before they are flushed")
}

func TestDefaultAndQuickstartTimingPass(t *testing.T) {
	if s := mustLoad(t, s3(nil)); s.RawFlushIntervalSeconds != 60 {
		t.Fatal(s.RawFlushIntervalSeconds)
	}
	if s := mustLoad(t, s3(map[string]string{"RAW_FLUSH_INTERVAL_SECONDS": "60", "NATS_ACK_WAIT_SECONDS": "300"})); s.NATSAckWaitSeconds != 300 {
		t.Fatal(s.NATSAckWaitSeconds)
	}
}

func TestMaxAckPendingDefaultAndEmptyBufferOverride(t *testing.T) {
	s := mustLoad(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": ""}))
	if s.NATSMaxAckPending != 10000 || s.RawMaxBufferedEvents != nil {
		t.Fatalf("%+v", s)
	}
	if s := mustLoad(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": "500"})); *s.RawMaxBufferedEvents != 500 {
		t.Fatal(*s.RawMaxBufferedEvents)
	}
}

func TestDefaults(t *testing.T) {
	s := mustLoad(t, map[string]string{"GCS_RAW_BUCKET": "b"})
	want := Settings{
		Env: "local", LogLevel: "INFO", NATSURL: "nats://localhost:4222", NATSStream: "EVENTS",
		NATSSubjectPrefix: "events", NATSConsumer: "raw-writer", NATSFetchBatch: 500,
		NATSFetchTimeoutSeconds: 1, NATSAckWaitSeconds: 120, NATSMaxDeliver: 10, NATSMaxAckPending: 10000,
		RawStorageBackend: "gcs", GCSRawBucket: "b", GCSRawPrefix: "raw", S3Prefix: "raw", S3Region: "us-east-1",
		S3ForcePathStyle: true, S3UseSSL: true, S3VerifySSL: true, RawFlushIntervalSeconds: 60,
		RawMaxEventsPerFile: 1000, RawMaxBytesPerFile: 4194304,
	}
	if *s != want {
		t.Fatalf("got %+v\nwant %+v", *s, want)
	}
}

// Parsing tables recorded from the original pydantic-settings model.
func TestIntegerParsing(t *testing.T) {
	ok := map[string]int{
		"5": 5, " 5 ": 5, "5.0": 5, "5.00": 5, "1_000": 1000, "+5": 5, "-5": -5, "05": 5, "\t5\n": 5,
		"-0": 0, "-5.0": -5, "1_000.0": 1000, "0_0": 0, " 5 ": 5, "　5": 5, "\x0b5\x0c": 5,
	}
	for in, want := range ok {
		if s := mustLoad(t, map[string]string{"GCS_RAW_BUCKET": "b", "NATS_FETCH_BATCH": in}); s.NATSFetchBatch != want {
			t.Errorf("%q -> %d, want %d", in, s.NATSFetchBatch, want)
		}
	}
	for _, in := range []string{"5.5", "1e3", "0x10", "", "abc", "٥", "5.", ".5e1", "Infinity", "inf", "5.10",
		"_1", "1_", "1__0", "1e0", "5\x1c", "5..0", "+-5", "- 5", ".0", "5.0_0", "5_.0", "+_5"} {
		mustFail(t, map[string]string{"GCS_RAW_BUCKET": "b", "NATS_FETCH_BATCH": in}, msgInt)
	}
}

func TestFloatParsing(t *testing.T) {
	ok := map[string]float64{
		"1": 1, "1.5": 1.5, " 2.5 ": 2.5, "inf": math.Inf(1), "-inf": math.Inf(-1), "1e3": 1000, "1_0": 10,
		"Infinity": math.Inf(1), "+1": 1, ".5": 0.5, "5.": 5, "1E2": 100, "infinity": math.Inf(1), "INF": math.Inf(1),
		"5.0_0": 5, "5_.0": 5, "+_5": 5, "1_0.5": 10.5, "1._5": 1.5, "1e1_0": 1e10, "1_e1": 10, "0001": 1,
		"1.5e+3": 1500, "1.5E-3": 0.0015, "-0.0": 0,
	}
	for in, want := range ok {
		if s := mustLoad(t, map[string]string{"GCS_RAW_BUCKET": "b", "NATS_FETCH_TIMEOUT_SECONDS": in}); s.NATSFetchTimeoutSeconds != want {
			t.Errorf("%q -> %v, want %v", in, s.NATSFetchTimeoutSeconds, want)
		}
	}
	for _, in := range []string{"nan", " nan", "nAn", "+nan", "-nan", " nan"} {
		if s := mustLoad(t, map[string]string{"GCS_RAW_BUCKET": "b", "NATS_FETCH_TIMEOUT_SECONDS": in}); !math.IsNaN(s.NATSFetchTimeoutSeconds) {
			t.Errorf("%q -> %v, want NaN", in, s.NATSFetchTimeoutSeconds)
		}
	}
	for _, in := range []string{"", "abc", "1__0", "0x1", "٥", "_1", "1_", "infinit", "1e", "e1", ".", "0x1p3", "1,5"} {
		mustFail(t, map[string]string{"GCS_RAW_BUCKET": "b", "NATS_FETCH_TIMEOUT_SECONDS": in}, msgFloat)
	}
}

func TestBoolParsing(t *testing.T) {
	ok := map[string]bool{
		"1": true, "0": false, "true": true, "FALSE": false, "on": true, "off": false, "t": true, "f": false,
		"y": true, "n": false, "no": false, "True": true, "ON": true, "yes": true,
	}
	for in, want := range ok {
		if s := mustLoad(t, s3(map[string]string{"S3_FORCE_PATH_STYLE": in})); s.S3ForcePathStyle != want {
			t.Errorf("%q -> %v", in, s.S3ForcePathStyle)
		}
	}
	for _, in := range []string{" yes ", "2", "", "1.0", "0.0", "TRUE "} {
		mustFail(t, s3(map[string]string{"S3_FORCE_PATH_STYLE": in}), msgBool)
	}
}

func TestBufferedEventsOverride(t *testing.T) {
	for in, want := range map[string]int{"5": 5, " 7 ": 7, "1.0": 1} {
		if s := mustLoad(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": in})); *s.RawMaxBufferedEvents != want {
			t.Errorf("%q -> %d", in, *s.RawMaxBufferedEvents)
		}
	}
	if s := mustLoad(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": "  "})); s.RawMaxBufferedEvents != nil {
		t.Error("whitespace should mean unset")
	}
	mustFail(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": "0"}), msgGE1)
	mustFail(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": "-1"}), msgGE1)
	mustFail(t, s3(map[string]string{"RAW_MAX_BUFFERED_EVENTS": "abc"}), msgInt)
	mustFail(t, s3(map[string]string{"NATS_MAX_ACK_PENDING": "0"}), msgGE1)
}

func TestFieldErrorsAreAllReported(t *testing.T) {
	mustFail(t, map[string]string{"NATS_FETCH_BATCH": "x", "NATS_MAX_DELIVER": "y", "RAW_MAX_BUFFERED_EVENTS": "0"},
		strings.Join([]string{msgInt, msgInt, msgGE1}, "; "))
}

func TestNamesAreCaseInsensitiveAndStringsTrimmed(t *testing.T) {
	s := mustLoad(t, map[string]string{"raw_storage_backend": "gcs", "gcs_raw_bucket": " b ", "GCS_RAW_PREFIX": "  ", "S3_PREFIX": " /x/ "})
	if s.GCSRawBucket != "b" || s.GCSRawPrefix != "raw" || s.S3Prefix != "/x/" {
		t.Fatalf("%+v", s)
	}
}

func TestSecretsAreNotEchoed(t *testing.T) {
	_, err := FromMap(s3(map[string]string{"S3_SECRET_KEY": "supersecret", "NATS_FETCH_BATCH": "supersecret"}))
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error %v", err)
	}
}

func TestLoadReadsDotenvBelowEnvironment(t *testing.T) {
	dir := t.TempDir()
	content := "# comment\nexport GCS_RAW_BUCKET='from-file'\nNATS_STREAM=\"FILE\"\nNATS_CONSUMER=file # note\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("NATS_STREAM", "ENVIRON")
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.GCSRawBucket != "from-file" || s.NATSStream != "ENVIRON" || s.NATSConsumer != "file" {
		t.Fatalf("%+v", s)
	}
}
