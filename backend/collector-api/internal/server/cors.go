package server

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
)

// allMethods is what "*" expands to in CORS_ALLOW_METHODS.
var allMethods = []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "QUERY"}

var safelistedHeaders = []string{"Accept", "Accept-Language", "Content-Language", "Content-Type"}

const preflightVary = "Origin, Access-Control-Request-Method, Access-Control-Request-Headers, Access-Control-Request-Private-Network"

// CORSConfig mirrors the options the collector passed to Starlette's
// CORSMiddleware.
type CORSConfig struct {
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	AllowCredentials bool
	ExposeHeaders    []string
	MaxAge           int64
}

// cors reproduces Starlette 1.x CORSMiddleware behaviour.
type cors struct {
	origins          []string
	methods          []string
	allowHeaders     []string // lower-cased, sorted
	allowAllOrigins  bool
	allowAllHeaders  bool
	explicitOrigin   bool // preflight_explicit_allow_origin
	simpleHeaders    [][2]string
	preflightHeaders [][2]string
}

func newCORS(cfg CORSConfig) *cors {
	c := &cors{origins: cfg.AllowOrigins, methods: cfg.AllowMethods}
	if slices.Contains(cfg.AllowMethods, "*") {
		c.methods = allMethods
	}
	c.allowAllOrigins = slices.Contains(cfg.AllowOrigins, "*")
	c.allowAllHeaders = slices.Contains(cfg.AllowHeaders, "*")
	c.explicitOrigin = !c.allowAllOrigins || cfg.AllowCredentials

	if c.allowAllOrigins {
		c.simpleHeaders = append(c.simpleHeaders, [2]string{"Access-Control-Allow-Origin", "*"})
	}
	if cfg.AllowCredentials {
		c.simpleHeaders = append(c.simpleHeaders, [2]string{"Access-Control-Allow-Credentials", "true"})
	}
	if len(cfg.ExposeHeaders) > 0 {
		c.simpleHeaders = append(c.simpleHeaders, [2]string{"Access-Control-Expose-Headers", strings.Join(cfg.ExposeHeaders, ", ")})
	}

	c.preflightHeaders = [][2]string{{"Vary", preflightVary}}
	if !c.explicitOrigin {
		c.preflightHeaders = append(c.preflightHeaders, [2]string{"Access-Control-Allow-Origin", "*"})
	}
	c.preflightHeaders = append(c.preflightHeaders,
		[2]string{"Access-Control-Allow-Methods", strings.Join(c.methods, ", ")},
		[2]string{"Access-Control-Max-Age", strconv.FormatInt(cfg.MaxAge, 10)},
	)
	headerSet := map[string]bool{}
	for _, h := range append(append([]string{}, safelistedHeaders...), cfg.AllowHeaders...) {
		headerSet[h] = true
	}
	sorted := make([]string, 0, len(headerSet))
	for h := range headerSet {
		sorted = append(sorted, h)
	}
	sort.Strings(sorted)
	if !c.allowAllHeaders {
		c.preflightHeaders = append(c.preflightHeaders, [2]string{"Access-Control-Allow-Headers", strings.Join(sorted, ", ")})
	}
	if cfg.AllowCredentials {
		c.preflightHeaders = append(c.preflightHeaders, [2]string{"Access-Control-Allow-Credentials", "true"})
	}
	for _, h := range sorted {
		c.allowHeaders = append(c.allowHeaders, strings.ToLower(h))
	}
	return c
}

func (c *cors) isAllowedOrigin(origin string) bool {
	return c.allowAllOrigins || slices.Contains(c.origins, origin)
}

// firstHeader returns the first value of a request header and whether it
// was present at all.
func firstHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// handle serves a preflight request and returns true, or wraps w so that
// CORS headers are added to the eventual response and returns false.
func (c *cors) handle(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, bool) {
	rawOrigin, hasOrigin := firstHeader(r, "Origin")
	_, hasRequestMethod := firstHeader(r, "Access-Control-Request-Method")
	if hasOrigin && r.Method == http.MethodOptions && hasRequestMethod {
		c.preflight(w, r, rawOrigin)
		return w, true
	}
	return &corsWriter{ResponseWriter: w, cors: c, rawOrigin: rawOrigin, hasOrigin: hasOrigin}, false
}

func (c *cors) preflight(w http.ResponseWriter, r *http.Request, rawOrigin string) {
	origin := pytext.Latin1(rawOrigin)
	rawMethod, _ := firstHeader(r, "Access-Control-Request-Method")
	rawRequested, hasRequested := firstHeader(r, "Access-Control-Request-Headers")
	_, hasPrivateNetwork := firstHeader(r, "Access-Control-Request-Private-Network")

	h := w.Header()
	for _, kv := range c.preflightHeaders {
		h.Set(kv[0], kv[1])
	}
	var failures []string
	if c.isAllowedOrigin(origin) {
		if c.explicitOrigin {
			h.Set("Access-Control-Allow-Origin", rawOrigin)
		}
	} else {
		failures = append(failures, "origin")
	}
	if !slices.Contains(c.methods, pytext.Latin1(rawMethod)) {
		failures = append(failures, "method")
	}
	if hasRequested {
		if c.allowAllHeaders {
			h.Set("Access-Control-Allow-Headers", rawRequested)
		} else {
			for _, header := range strings.Split(strings.ToLower(pytext.Latin1(rawRequested)), ",") {
				if !slices.Contains(c.allowHeaders, pytext.Strip(header)) {
					failures = append(failures, "headers")
					break
				}
			}
		}
	}
	if hasPrivateNetwork {
		failures = append(failures, "private-network")
	}
	if len(failures) > 0 {
		writeText(w, http.StatusBadRequest, "Disallowed CORS "+strings.Join(failures, ", "))
		return
	}
	writeText(w, http.StatusOK, "OK")
}

// corsWriter adds simple-response CORS headers before the status line.
type corsWriter struct {
	http.ResponseWriter
	cors      *cors
	rawOrigin string
	hasOrigin bool
	wrote     bool
}

func (cw *corsWriter) WriteHeader(status int) {
	if !cw.wrote {
		cw.wrote = true
		c := cw.cors
		h := cw.Header()
		if cw.hasOrigin {
			for _, kv := range c.simpleHeaders {
				h.Set(kv[0], kv[1])
			}
		}
		addVary := func() {
			h.Set("Vary", strings.Join(append(h.Values("Vary"), "Origin"), ", "))
		}
		switch {
		case cw.hasOrigin && c.allowAllOrigins && c.explicitOrigin:
			h.Set("Access-Control-Allow-Origin", cw.rawOrigin)
			addVary()
		case cw.hasOrigin && !c.allowAllOrigins && c.isAllowedOrigin(pytext.Latin1(cw.rawOrigin)):
			h.Set("Access-Control-Allow-Origin", cw.rawOrigin)
			addVary()
		default:
			addVary()
		}
	}
	cw.ResponseWriter.WriteHeader(status)
}

func (cw *corsWriter) Write(b []byte) (int, error) {
	if !cw.wrote {
		cw.WriteHeader(http.StatusOK)
	}
	return cw.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer.
func (cw *corsWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
