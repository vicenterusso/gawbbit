// Package server implements the collector's HTTP API with the routing,
// status codes, bodies and headers of the original FastAPI service.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/config"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/geoip"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/publisher"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/schema"
)

// The interactive docs FastAPI served, captured from the original service.
var (
	//go:embed static/openapi.json
	openAPIJSON []byte
	//go:embed static/docs.html
	docsHTML []byte
	//go:embed static/redoc.html
	redocHTML []byte
	//go:embed static/oauth2-redirect.html
	oauth2RedirectHTML []byte
)

// BatchPublisher publishes a batch of messages (see publisher.Publisher).
type BatchPublisher interface {
	PublishBatch(ctx context.Context, messages []publisher.Message) error
}

// CountryResolver resolves an IP's country (see geoip.Resolver).
type CountryResolver interface {
	LookupCountry(ip string) (geoip.Country, bool)
}

// Options configures a Server.
type Options struct {
	Settings  *config.Settings
	Log       *logx.Logger
	AccessLog io.Writer // uvicorn-style access lines; nil disables them
	Publisher BatchPublisher
	GeoIP     CountryResolver
	// ForwardedAllowIPs is uvicorn's FORWARDED_ALLOW_IPS (default "127.0.0.1").
	ForwardedAllowIPs string
	// Now and NewRequestID default to the wall clock and random UUIDv4s.
	Now          func() time.Time
	NewRequestID func() string
}

// Server is the collector HTTP handler.
type Server struct {
	opts    Options
	cors    *cors
	proxies *trustedProxies
}

// New builds a Server. CORS is enabled when CORS_ALLOW_ORIGINS lists any
// origin.
func New(opts Options) *Server {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewRequestID == nil {
		opts.NewRequestID = NewUUID4
	}
	s := &Server{opts: opts, proxies: newTrustedProxies(opts.ForwardedAllowIPs)}
	if origins := opts.Settings.CORSOriginsList(); len(origins) > 0 {
		s.cors = newCORS(CORSConfig{
			AllowOrigins:     origins,
			AllowMethods:     opts.Settings.CORSMethodsList(),
			AllowHeaders:     opts.Settings.CORSHeadersList(),
			AllowCredentials: opts.Settings.CORSAllowCredentials,
			ExposeHeaders:    []string{"Retry-After"},
			MaxAge:           opts.Settings.CORSMaxAgeSeconds,
		})
	}
	return s
}

// request carries per-request connection facts after proxy-header handling.
type request struct {
	clientHost string
	clientPort int
	scheme     string
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := s.connection(r)
	rec := &statusRecorder{ResponseWriter: w}
	defer s.accessLog(r, req, rec)
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			s.opts.Log.Error("Exception in ASGI application: %v", p)
			if rec.status == 0 {
				writeText(rec, http.StatusInternalServerError, "Internal Server Error")
			}
		}
	}()

	var out http.ResponseWriter = rec
	if s.cors != nil {
		var handled bool
		if out, handled = s.cors.handle(rec, r); handled {
			return
		}
	}
	s.route(out, r, req)
}

func (s *Server) connection(r *http.Request) request {
	host, portText, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	port, _ := strconv.Atoi(portText)
	req := request{clientHost: host, clientPort: port, scheme: "http"}
	if s.proxies.contains(host) {
		if values := r.Header.Values("X-Forwarded-Proto"); len(values) > 0 {
			proto := pytext.Strip(pytext.Latin1(values[len(values)-1]))
			switch proto {
			case "http", "https", "ws", "wss":
				req.scheme = proto
			}
		}
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			if h := s.proxies.clientHost(pytext.Latin1(values[len(values)-1])); h != "" {
				req.clientHost, req.clientPort = h, 0
			}
		}
	}
	return req
}

// statusPhrase is Python's http.HTTPStatus(code).phrase.
func statusPhrase(code int) string {
	switch code {
	case http.StatusRequestEntityTooLarge:
		return "Content Too Large"
	case http.StatusUnprocessableEntity:
		return "Unprocessable Content"
	}
	return http.StatusText(code)
}

func (s *Server) accessLog(r *http.Request, req request, rec *statusRecorder) {
	if s.opts.AccessLog == nil {
		return
	}
	target := quotePath(decodedPath(r), "/")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	line := fmt.Sprintf("INFO:     %s:%d - \"%s %s HTTP/%d.%d\" %d %s\n",
		req.clientHost, req.clientPort, r.Method, target, r.ProtoMajor, r.ProtoMinor, rec.status, statusPhrase(rec.status))
	_, _ = io.WriteString(s.opts.AccessLog, line)
}

// decodedPath is the percent-decoded request path as text, with invalid
// UTF-8 replaced like Python's urllib.parse.unquote.
func decodedPath(r *http.Request) string {
	p := r.URL.Path
	if utf8.ValidString(p) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		rn, size := utf8.DecodeRuneInString(p[i:])
		b.WriteRune(rn)
		i += size
	}
	return b.String()
}

// quotePath is Python's urllib.parse.quote(s, safe).
func quotePath(s, safe string) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~' || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0xF])
	}
	return b.String()
}

const (
	pathHealthz        = "/healthz"
	pathBatch          = "/v1/events:batch"
	pathOpenAPI        = "/openapi.json"
	pathDocs           = "/docs"
	pathOAuth2Redirect = "/docs/oauth2-redirect"
	pathRedoc          = "/redoc"
)

// routeMethods returns the allowed methods for a route path, or nil.
func routeMethods(path string) []string {
	switch path {
	case pathHealthz:
		return []string{http.MethodGet}
	case pathBatch:
		return []string{http.MethodPost}
	case pathOpenAPI, pathDocs, pathOAuth2Redirect, pathRedoc:
		return []string{http.MethodGet, http.MethodHead}
	}
	return nil
}

// routePath maps a request path to its route; Starlette matches route
// regexes with "$", which also accepts one trailing newline.
func routePath(path string) string {
	return strings.TrimSuffix(path, "\n")
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, req request) {
	path := decodedPath(r)
	route := routePath(path)
	methods := routeMethods(route)
	if methods == nil {
		s.notFound(w, r, req, path)
		return
	}
	allowed := false
	for _, m := range methods {
		allowed = allowed || r.Method == m
	}
	if !allowed {
		writeJSON(w, http.StatusMethodNotAllowed, []byte(`{"detail":"Method Not Allowed"}`), "Allow", strings.Join(methods, ", "))
		return
	}
	switch route {
	case pathHealthz:
		writeJSON(w, http.StatusOK, []byte(`{"status":"ok"}`))
	case pathBatch:
		s.ingest(w, r, req)
	case pathOpenAPI:
		writeJSON(w, http.StatusOK, openAPIJSON)
	case pathDocs:
		writeHTML(w, docsHTML)
	case pathOAuth2Redirect:
		writeHTML(w, oauth2RedirectHTML)
	case pathRedoc:
		writeHTML(w, redocHTML)
	}
}

// notFound applies Starlette's redirect_slashes before answering 404.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request, req request, path string) {
	if path != "/" {
		alt := path + "/"
		if strings.HasSuffix(path, "/") {
			alt = strings.TrimRight(path, "/")
		}
		if routeMethods(routePath(alt)) != nil {
			location := redirectURL(r, req, alt)
			w.Header().Set("Location", location)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, []byte(`{"detail":"Not Found"}`))
}

func redirectURL(r *http.Request, req request, path string) string {
	var url string
	if r.Host != "" {
		url = req.scheme + "://" + pytext.Latin1(r.Host) + path
	} else if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		host, portText, _ := net.SplitHostPort(local.String())
		port, _ := strconv.Atoi(portText)
		defaultPort := map[string]int{"http": 80, "https": 443, "ws": 80, "wss": 443}[req.scheme]
		if port == defaultPort {
			url = req.scheme + "://" + host + path
		} else {
			url = fmt.Sprintf("%s://%s:%d%s", req.scheme, host, port, path)
		}
	} else {
		url = path
	}
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	return quotePath(url, ":/%#?=@[]!$&'()*+,;")
}

func writeJSON(w http.ResponseWriter, status int, body []byte, headers ...string) {
	h := w.Header()
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

func writeDetail(w http.ResponseWriter, status int, detail string) {
	body := append([]byte(`{"detail":`), pyjson.AppendString(nil, detail)...)
	writeJSON(w, status, append(body, '}'))
}

// readBody reads the body chunk by chunk, stopping as soon as more than
// limit bytes arrived (tooLarge), like the original streaming check; the
// returned length is what had been received at that point.
func readBody(r io.Reader, limit int64) (body []byte, tooLarge bool, err error) {
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		body = append(body, buf[:n]...)
		if int64(len(body)) > limit {
			return body, true, nil
		}
		if err == io.EOF {
			return body, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

// maxDrainBytes bounds how much of an oversized body is read and discarded
// after the 413 response.
const maxDrainBytes = 64 << 20

// drainBody consumes the rest of an oversized body after the response was
// sent, as uvicorn did, so that clients still writing the body read the 413
// instead of a connection reset.
func drainBody(w http.ResponseWriter, body io.Reader) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
}

const (
	detailMissingKey  = "Missing X-API-Key"
	detailInvalidKey  = "Invalid API key"
	detailAppMismatch = "API key is not allowed for this app_id"
)

// authorize maps the API key to its app_id; it returns the HTTP status,
// detail and log reason of a rejection, or status 0.
func authorize(keys map[string]string, apiKey, appID string) (int, string, string) {
	if apiKey == "" {
		return http.StatusUnauthorized, detailMissingKey, "auth_missing_key"
	}
	allowed := keys[apiKey]
	if allowed == "" {
		return http.StatusUnauthorized, detailInvalidKey, "auth_invalid_key"
	}
	if allowed != appID {
		return http.StatusForbidden, detailAppMismatch, "auth_app_mismatch"
	}
	return 0, "", ""
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request, req request) {
	started := time.Now()
	cfg := s.opts.Settings
	log := s.opts.Log

	body, tooLarge, err := readBody(r.Body, cfg.MaxRequestBytes)
	if err != nil {
		// Malformed framing or a client gone away, answered like uvicorn.
		w.Header().Set("Connection", "close")
		writeText(w, http.StatusBadRequest, "Invalid HTTP request received.")
		return
	}
	if tooLarge {
		log.Warn("request_rejected reason=request_too_large request_bytes=%d", len(body))
		writeDetail(w, http.StatusRequestEntityTooLarge, "Request too large")
		drainBody(w, r.Body)
		return
	}

	events, verrs := schema.ParseBatch(body)
	if verrs != nil {
		log.Warn("request_rejected reason=validation_error request_bytes=%d", len(body))
		writeJSON(w, http.StatusUnprocessableEntity, ValidationErrorBody(verrs))
		return
	}
	if len(events) == 0 {
		log.Warn("request_rejected reason=empty_events request_bytes=%d", len(body))
		writeDetail(w, http.StatusBadRequest, "events cannot be empty")
		return
	}
	if int64(len(events)) > cfg.MaxEventsPerRequest {
		log.Warn("request_rejected reason=events_limit_exceeded events=%d max=%d", len(events), cfg.MaxEventsPerRequest)
		writeDetail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("events limit exceeded: %d", cfg.MaxEventsPerRequest))
		return
	}

	requestID := s.opts.NewRequestID()
	apiKey, _ := firstHeader(r, "X-API-Key")
	apiKey = pytext.Latin1(apiKey)
	for i := range events {
		if status, detail, reason := authorize(cfg.APIKeyMap, apiKey, events[i].AppID); status != 0 {
			log.Warn("request_rejected reason=%s events=%d", reason, len(events))
			writeDetail(w, status, detail)
			return
		}
	}

	meta := s.requestMeta(r, req)
	messages := make([]publisher.Message, len(events))
	apps := map[string]bool{}
	for i := range events {
		ev := &events[i]
		apps[ev.AppID] = true
		messages[i] = publisher.Message{
			Subject: SubjectForApp(cfg.NATSSubjectPrefix, ev.AppID),
			Payload: pyjson.Encode(s.enrich(ev, meta, requestID)),
			MsgID:   ev.AppID + ":" + ev.EventID,
		}
	}

	if err := s.opts.Publisher.PublishBatch(r.Context(), messages); err != nil {
		var batchErr *publisher.BatchPublishError
		published, failed, class := 0, len(messages), publisher.ErrorClass(err)
		if errors.As(err, &batchErr) {
			published, failed, class = batchErr.Published, batchErr.Failed, publisher.ErrorClass(batchErr.FirstError)
		}
		log.Error("publish_failed request_id=%s events=%d published_events=%d failed_events=%d error_class=%s",
			requestID, len(messages), published, failed, class)
		writeDetail(w, http.StatusServiceUnavailable, "Failed to publish events")
		return
	}

	log.Info("batch_accepted request_id=%s accepted_events=%d distinct_app_ids=%d request_bytes=%d publish_ms=%d",
		requestID, len(events), len(apps), len(body), time.Since(started).Milliseconds())
	resp := append([]byte(`{"request_id":`), pyjson.AppendString(nil, requestID)...)
	resp = append(resp, `,"accepted_events":`...)
	resp = strconv.AppendInt(resp, int64(len(events)), 10)
	writeJSON(w, http.StatusOK, append(resp, '}'))
}

// requestMeta holds the request facts used to enrich every event.
type requestMeta struct {
	userAgent *string
	clientIP  string
}

func (s *Server) requestMeta(r *http.Request, req request) requestMeta {
	var meta requestMeta
	if ua, ok := firstHeader(r, "User-Agent"); ok {
		ua = pytext.Latin1(ua)
		meta.userAgent = &ua
	}
	forwardedFor, _ := firstHeader(r, "X-Forwarded-For")
	realIP, _ := firstHeader(r, "X-Real-Ip")
	meta.clientIP = geoip.ClientIP(pytext.Latin1(forwardedFor), pytext.Latin1(realIP), req.clientHost)
	return meta
}

// enrich renders an event with server-side ingest metadata: receive time,
// request ID, user agent and salted IP hash always replace client values;
// the raw IP is kept only with STORE_RAW_IP; GeoIP overrides the country.
func (s *Server) enrich(ev *schema.Event, meta requestMeta, requestID string) pyjson.Value {
	cfg := s.opts.Settings
	now := pyjson.StringValue(isoformatUTC(s.opts.Now()))
	payload := ev.Dump()

	ingest := pyjson.NewObject(5)
	if current, _ := payload.Get("ingest"); current.Kind == pyjson.Object {
		for _, f := range current.Members {
			ingest.Set(f.Key, f.Value)
		}
	}
	ingest.Set("received_at", now)
	ingest.Set("request_id", pyjson.StringValue(requestID))
	ingest.Set("user_agent", pyjson.OptString(meta.userAgent))
	if meta.clientIP != "" {
		ingest.Set("ip_hash", pyjson.StringValue(HashIP(meta.clientIP, cfg.IPHashSalt)))
	} else {
		ingest.Set("ip_hash", pyjson.NullValue())
	}
	if meta.clientIP != "" && cfg.StoreRawIP {
		ingest.Set("ip", pyjson.StringValue(meta.clientIP))
	} else {
		ingest.Delete("ip")
	}

	if meta.clientIP != "" {
		if country, ok := s.opts.GeoIP.LookupCountry(meta.clientIP); ok {
			geo := pyjson.NewObject(5)
			if current, _ := payload.Get("geo"); current.Kind == pyjson.Object {
				for _, f := range current.Members {
					geo.Set(f.Key, f.Value)
				}
			}
			if country.Name != "" {
				geo.Set("country", pyjson.StringValue(country.Name))
			}
			if country.Code != "" {
				geo.Set("country_code", pyjson.StringValue(country.Code))
			}
			payload.Set("geo", geo.Value())
		}
	}

	payload.Set("ingest", ingest.Value())
	payload.Set("received_at", now)
	return payload.Value()
}

// isoformatUTC is Python's datetime.now(UTC).isoformat().
func isoformatUTC(t time.Time) string {
	t = t.UTC()
	if us := t.Nanosecond() / 1000; us != 0 {
		return t.Format("2006-01-02T15:04:05") + fmt.Sprintf(".%06d+00:00", us)
	}
	return t.Format("2006-01-02T15:04:05") + "+00:00"
}

// HashIP returns sha256("<salt>:<ip>") as hex.
func HashIP(ip, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + ip))
	return hex.EncodeToString(sum[:])
}

// SubjectForApp is "<prefix>.<app_id>" with the app_id stripped and spaces
// replaced by underscores.
func SubjectForApp(prefix, appID string) string {
	return prefix + "." + strings.ReplaceAll(pytext.Strip(appID), " ", "_")
}

// NewUUID4 returns a random RFC 4122 version 4 UUID string.
func NewUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
