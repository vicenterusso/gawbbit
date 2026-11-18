// Package settings loads raw-writer configuration from the environment (and an optional
// ./.env file) with the same names, defaults, parsing rules and validation messages as
// the original pydantic-settings model.
package settings

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
)

// Settings is the validated configuration.
type Settings struct {
	Env      string
	LogLevel string

	NATSURL                 string
	NATSStream              string
	NATSSubjectPrefix       string
	NATSConsumer            string
	NATSFetchBatch          int
	NATSFetchTimeoutSeconds float64
	NATSAckWaitSeconds      int
	NATSMaxDeliver          int
	// Used only when raw-writer creates the durable consumer; an existing one is never changed.
	NATSMaxAckPending int

	RawStorageBackend string

	GCSRawBucket string
	GCSRawPrefix string

	S3EndpointURL    string
	S3Bucket         string
	S3Prefix         string
	S3AccessKey      string
	S3SecretKey      string
	S3Region         string
	S3ForcePathStyle bool
	S3UseSSL         bool
	S3VerifySSL      bool

	RawFlushIntervalSeconds int
	RawMaxEventsPerFile     int
	RawMaxBytesPerFile      int
	// Optional cap on buffered events across partitions; nil when unset.
	RawMaxBufferedEvents *int
}

// StorageBucket is the bucket of the selected backend.
func (s *Settings) StorageBucket() string {
	if s.RawStorageBackend == "gcs" {
		return s.GCSRawBucket
	}
	return s.S3Bucket
}

// StoragePrefix is the object prefix of the selected backend.
func (s *Settings) StoragePrefix() string {
	if s.RawStorageBackend == "gcs" {
		return s.GCSRawPrefix
	}
	return s.S3Prefix
}

const (
	msgInt   = "Input should be a valid integer, unable to parse string as an integer"
	msgFloat = "Input should be a valid number, unable to parse string as a number"
	msgBool  = "Input should be a valid boolean, unable to interpret input"
	msgGE1   = "Input should be greater than or equal to 1"
)

// Load reads the process environment, falling back to ./.env for unset names.
func Load() (*Settings, error) {
	values := map[string]string{}
	if dotenv, err := readDotenv(".env"); err == nil {
		for k, v := range dotenv {
			values[strings.ToLower(k)] = v
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			values[strings.ToLower(k)] = v
		}
	}
	return FromMap(values)
}

// FromMap builds settings from NAME=value pairs; names are case-insensitive.
func FromMap(env map[string]string) (*Settings, error) {
	lookup := map[string]string{}
	for k, v := range env {
		lookup[strings.ToLower(k)] = v
	}
	p := parser{env: lookup}
	s := &Settings{
		Env:                     p.str("ENV", "local"),
		LogLevel:                p.str("LOG_LEVEL", "INFO"),
		NATSURL:                 p.str("NATS_URL", "nats://localhost:4222"),
		NATSStream:              p.str("NATS_STREAM", "EVENTS"),
		NATSSubjectPrefix:       p.str("NATS_SUBJECT_PREFIX", "events"),
		NATSConsumer:            p.str("NATS_CONSUMER", "raw-writer"),
		NATSFetchBatch:          p.integer("NATS_FETCH_BATCH", 500, false),
		NATSFetchTimeoutSeconds: p.float("NATS_FETCH_TIMEOUT_SECONDS", 1.0),
		NATSAckWaitSeconds:      p.integer("NATS_ACK_WAIT_SECONDS", 120, false),
		NATSMaxDeliver:          p.integer("NATS_MAX_DELIVER", 10, false),
		NATSMaxAckPending:       p.integer("NATS_MAX_ACK_PENDING", 10000, true),
		RawStorageBackend:       p.str("RAW_STORAGE_BACKEND", "gcs"),
		GCSRawBucket:            p.str("GCS_RAW_BUCKET", ""),
		GCSRawPrefix:            p.str("GCS_RAW_PREFIX", "raw"),
		S3EndpointURL:           p.str("S3_ENDPOINT_URL", ""),
		S3Bucket:                p.str("S3_BUCKET", ""),
		S3Prefix:                p.str("S3_PREFIX", "raw"),
		S3AccessKey:             p.str("S3_ACCESS_KEY", ""),
		S3SecretKey:             p.str("S3_SECRET_KEY", ""),
		S3Region:                p.str("S3_REGION", "us-east-1"),
		S3ForcePathStyle:        p.boolean("S3_FORCE_PATH_STYLE", true),
		S3UseSSL:                p.boolean("S3_USE_SSL", true),
		S3VerifySSL:             p.boolean("S3_VERIFY_SSL", true),
		RawFlushIntervalSeconds: p.integer("RAW_FLUSH_INTERVAL_SECONDS", 60, false),
		RawMaxEventsPerFile:     p.integer("RAW_MAX_EVENTS_PER_FILE", 1000, false),
		RawMaxBytesPerFile:      p.integer("RAW_MAX_BYTES_PER_FILE", 4_194_304, false),
	}
	if raw, ok := p.env["raw_max_buffered_events"]; ok && pyval.Strip(raw) != "" {
		v := p.integer("RAW_MAX_BUFFERED_EVENTS", 0, true)
		s.RawMaxBufferedEvents = &v
	}
	if len(p.problems) == 0 {
		if err := s.validate(); err != nil {
			p.problems = append(p.problems, "Value error, "+err.Error())
		}
	}
	if len(p.problems) > 0 {
		// Report messages only: input values may hold S3 secrets.
		return nil, fmt.Errorf("Invalid raw-writer settings: %s", strings.Join(p.problems, "; "))
	}
	return s, nil
}

func (s *Settings) validate() error {
	if s.RawFlushIntervalSeconds >= s.NATSAckWaitSeconds {
		return fmt.Errorf("RAW_FLUSH_INTERVAL_SECONDS must be lower than NATS_ACK_WAIT_SECONDS "+
			"(got %d >= %d); otherwise buffered messages are redelivered before they are flushed",
			s.RawFlushIntervalSeconds, s.NATSAckWaitSeconds)
	}
	backend := strings.ToLower(pyval.Strip(s.RawStorageBackend))
	if backend != "gcs" && backend != "s3" {
		return errors.New("RAW_STORAGE_BACKEND must be one of: gcs, s3")
	}
	s.RawStorageBackend = backend
	s.GCSRawBucket = pyval.Strip(s.GCSRawBucket)
	s.GCSRawPrefix = orDefault(pyval.Strip(s.GCSRawPrefix), "raw")
	s.S3EndpointURL = pyval.Strip(s.S3EndpointURL)
	s.S3Bucket = pyval.Strip(s.S3Bucket)
	s.S3Prefix = orDefault(pyval.Strip(s.S3Prefix), "raw")
	s.S3AccessKey = pyval.Strip(s.S3AccessKey)
	s.S3SecretKey = pyval.Strip(s.S3SecretKey)

	if backend == "gcs" {
		if s.GCSRawBucket == "" {
			return errors.New("GCS_RAW_BUCKET is required when RAW_STORAGE_BACKEND=gcs")
		}
		return nil
	}
	var missing []string
	for _, req := range []struct{ name, value string }{
		{"S3_ENDPOINT_URL", s.S3EndpointURL},
		{"S3_BUCKET", s.S3Bucket},
		{"S3_ACCESS_KEY", s.S3AccessKey},
		{"S3_SECRET_KEY", s.S3SecretKey},
	} {
		if req.value == "" {
			missing = append(missing, req.name)
		}
	}
	if len(missing) > 0 {
		return errors.New("Missing required S3 settings when RAW_STORAGE_BACKEND=s3: " + strings.Join(missing, ", "))
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

type parser struct {
	env      map[string]string
	problems []string
}

func (p *parser) str(name, def string) string {
	if v, ok := p.env[strings.ToLower(name)]; ok {
		return v
	}
	return def
}

func (p *parser) integer(name string, def int, atLeastOne bool) int {
	raw, ok := p.env[strings.ToLower(name)]
	if !ok {
		return def
	}
	v, ok := parseInt(raw)
	if !ok {
		p.problems = append(p.problems, msgInt)
		return def
	}
	if atLeastOne && v < 1 {
		p.problems = append(p.problems, msgGE1)
		return def
	}
	return v
}

func (p *parser) float(name string, def float64) float64 {
	raw, ok := p.env[strings.ToLower(name)]
	if !ok {
		return def
	}
	v, ok := parseFloat(raw)
	if !ok {
		p.problems = append(p.problems, msgFloat)
		return def
	}
	return v
}

func (p *parser) boolean(name string, def bool) bool {
	raw, ok := p.env[strings.ToLower(name)]
	if !ok {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "on", "t", "true", "y", "yes":
		return true
	case "0", "off", "f", "false", "n", "no":
		return false
	}
	p.problems = append(p.problems, msgBool)
	return def
}

// validDigits accepts ASCII digits with single underscores between them.
func validDigits(s string) bool {
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// parseInt follows pydantic's lax str->int: surrounding whitespace, optional sign,
// underscores between digits and an all-zero fractional part ("5.0") are accepted.
// Values beyond the int64 range are rejected.
func parseInt(raw string) (int, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	sign := ""
	if s != "" && (s[0] == '+' || s[0] == '-') {
		sign, s = s[:1], s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac && (frac == "" || strings.Trim(frac, "0") != "") {
		return 0, false
	}
	if !validDigits(whole) {
		return 0, false
	}
	v, err := strconv.ParseInt(sign+strings.ReplaceAll(whole, "_", ""), 10, 64)
	if err != nil || v < math.MinInt || v > math.MaxInt {
		return 0, false
	}
	return int(v), true
}

// parseFloat follows pydantic's str->float: surrounding whitespace, inf/infinity/nan in
// any case, and underscores that are neither leading, trailing nor doubled.
func parseFloat(raw string) (float64, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 {
		return 0, false
	}
	switch strings.ToLower(body) {
	case "nan":
		return math.NaN(), true
	case "inf", "infinity":
	default:
		for _, c := range body {
			if !(c >= '0' && c <= '9') && !strings.ContainsRune(".eE+-", c) {
				return 0, false
			}
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return v, true
}

// readDotenv parses a minimal .env file: [export] KEY=VALUE lines, # comments, single or
// double quoted values.
func readDotenv(path string) (map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	out := map[string]string{}
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case len(value) >= 2 && value[0] == '\'' && strings.LastIndexByte(value, '\'') > 0:
			value = value[1:strings.LastIndexByte(value, '\'')]
		case len(value) >= 2 && value[0] == '"' && strings.LastIndexByte(value, '"') > 0:
			unquoted, err := strconv.Unquote(value[:strings.LastIndexByte(value, '"')+1])
			if err != nil {
				unquoted = value[1:strings.LastIndexByte(value, '"')]
			}
			value = unquoted
		default:
			if i := strings.Index(value, " #"); i >= 0 {
				value = strings.TrimSpace(value[:i])
			}
		}
		out[key] = value
	}
	return out, scanner.Err()
}
