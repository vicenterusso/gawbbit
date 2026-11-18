// Package geoip extracts the client IP from a request and resolves its
// country from a MaxMind-format database, failing open like the original.
package geoip

import (
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
)

// Reader looks up the decoded record for an address (nil when absent).
type Reader interface {
	Lookup(ip netip.Addr) (any, error)
	Close() error
}

// ReaderFactory opens a Reader for a database path.
type ReaderFactory func(path string) (Reader, error)

// Country is the enrichment applied to an event's geo context.
type Country struct {
	Name string // empty when unknown
	Code string // empty when unknown
}

// Resolver resolves countries; the database is opened lazily on first use.
type Resolver struct {
	enabled bool
	path    string
	factory ReaderFactory
	log     *logx.Logger

	once   sync.Once
	mu     sync.Mutex
	reader Reader
}

// NewResolver returns a resolver. A nil factory opens MaxMind DB files.
func NewResolver(enabled bool, path string, factory ReaderFactory, log *logx.Logger) *Resolver {
	if factory == nil {
		factory = openMMDB
	}
	return &Resolver{enabled: enabled, path: path, factory: factory, log: log}
}

func (r *Resolver) getReader() Reader {
	if !r.enabled {
		return nil
	}
	r.once.Do(func() {
		if r.path == "" {
			r.log.Warn("geoip_disabled reason=missing_path")
			return
		}
		info, err := os.Stat(r.path)
		if err != nil || !info.Mode().IsRegular() {
			r.log.Warn("geoip_unavailable reason=missing_mmdb path=%s", r.path)
			return
		}
		reader, err := r.factory(r.path)
		if err != nil {
			r.log.Warn("geoip_unavailable reason=reader_open_failed path=%s error_class=%s", r.path, openErrorClass(err))
			return
		}
		r.mu.Lock()
		r.reader = reader
		r.mu.Unlock()
		r.log.Info("geoip_ready path=%s", r.path)
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reader
}

// Warmup opens the database at startup when enabled.
func (r *Resolver) Warmup() {
	if r.enabled {
		r.getReader()
	} else {
		r.log.Info("geoip_disabled reason=feature_flag_off")
	}
}

// LookupCountry returns the country for ip, or false when unavailable.
func (r *Resolver) LookupCountry(ip string) (Country, bool) {
	if ip == "" {
		return Country{}, false
	}
	reader := r.getReader()
	if reader == nil {
		return Country{}, false
	}
	addr, ok := ParseIP(ip)
	if !ok {
		return Country{}, false
	}
	record, err := reader.Lookup(addr)
	if err != nil {
		r.log.Warn("geoip_lookup_failed error_class=%s", lookupErrorClass(err))
		return Country{}, false
	}
	return countryFromRecord(record)
}

// Close releases the database.
func (r *Resolver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader != nil {
		_ = r.reader.Close()
		r.reader = nil
	}
}

// countryFromRecord accepts GeoIP2/DB-IP records ({"country": {"name",
// "iso_code"}}) and flat ones ({"country_name", "country_code"}).
func countryFromRecord(record any) (Country, bool) {
	m, ok := record.(map[string]any)
	if !ok {
		return Country{}, false
	}
	var c Country
	if country, ok := m["country"].(map[string]any); ok {
		c.Name = pytext.Strip(pyStrOrEmpty(country["name"]))
		c.Code = strings.ToUpper(pytext.Strip(pyStrOrEmpty(country["iso_code"])))
	}
	if c.Name == "" {
		c.Name = pytext.Strip(pyStrOrEmpty(m["country_name"]))
	}
	if c.Code == "" {
		c.Code = strings.ToUpper(pytext.Strip(pyStrOrEmpty(m["country_code"])))
	}
	if c.Name == "" && c.Code == "" {
		return Country{}, false
	}
	return c, true
}

// pyStrOrEmpty is Python's str(value or "") for decoded MaxMind values.
func pyStrOrEmpty(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return ""
	case float64:
		if x == 0 {
			return ""
		}
		return pyjson.FloatRepr(x)
	case float32:
		if x == 0 {
			return ""
		}
		return pyjson.FloatRepr(float64(x))
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		if s := fmt.Sprint(x); s != "0" {
			return s
		}
		return ""
	case *big.Int:
		if x.Sign() == 0 {
			return ""
		}
		return x.String()
	case map[string]any:
		if len(x) == 0 {
			return ""
		}
	case []any:
		if len(x) == 0 {
			return ""
		}
	}
	// Containers and bytes never hold a country name in practice; Python
	// would use their repr, which is not reproduced here.
	return ""
}

func openErrorClass(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "PermissionError"
	}
	return "InvalidDatabaseError"
}

func lookupErrorClass(err error) string {
	var invalid maxminddb.InvalidDatabaseError
	if errors.As(err, &invalid) {
		return "InvalidDatabaseError"
	}
	return "ValueError"
}

type mmdbReader struct{ db *maxminddb.Reader }

func openMMDB(path string) (Reader, error) {
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return mmdbReader{db: db}, nil
}

func (m mmdbReader) Lookup(ip netip.Addr) (any, error) {
	res := m.db.Lookup(ip)
	if err := res.Err(); err != nil {
		return nil, err
	}
	if !res.Found() {
		return nil, nil
	}
	var record any
	if err := res.Decode(&record); err != nil {
		return nil, err
	}
	return record, nil
}

func (m mmdbReader) Close() error { return m.db.Close() }
