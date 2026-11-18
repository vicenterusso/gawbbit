// Package server wires the analytics tools into MCP streamable-HTTP
// endpoints: the backward-compatible main endpoint plus one isolated
// endpoint per runtime dataset.
package server

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/auth"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/datasets"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

// App is the complete HTTP application.
type App struct {
	main   *subApp
	mounts []mount
	log    *Logger
}

type mount struct {
	prefix string
	app    *subApp
}

// subApp is one FastMCP http_app: optional static-token middleware around a
// router with a single MCP route.
type subApp struct {
	path    string
	tokens  []auth.Label
	jwt     *auth.JWTVerifier
	handler http.Handler
}

// DefaultGatewayFactory queries ClickHouse over HTTP.
func DefaultGatewayFactory(userAgent string) GatewayFactory {
	return func(cfg *settings.Settings) clickhouse.Querier { return clickhouse.NewGateway(cfg, userAgent) }
}

func labels(tokens []settings.LabeledToken) []auth.Label {
	out := make([]auth.Label, len(tokens))
	for i, t := range tokens {
		out[i] = auth.Label{Label: t.Label, Token: t.Token}
	}
	return out
}

func datasetLabels(d *datasets.Dataset) []auth.Label {
	out := make([]auth.Label, len(d.BearerTokens))
	for i, t := range d.BearerTokens {
		out[i] = auth.Label{Label: t.Label, Token: t.Value}
	}
	return out
}

func actorResolver(tokens []auth.Label, authMode string) func(http.Header) string {
	return func(h http.Header) string {
		if token, ok := auth.ExtractBearer(h.Get("Authorization")); ok {
			if label, ok := auth.ResolveLabel(tokens, token); ok && actorLabelRe.MatchString(label) {
				return label
			}
		}
		switch authMode {
		case settings.AuthJWT:
			return "jwt"
		case settings.AuthNone:
			return "unauthenticated_dev"
		}
		return "unknown"
	}
}

// New mirrors create_app(): the main endpoint and isolated scoped endpoints.
func New(cfg *settings.Settings, reg *datasets.Registry, factory GatewayFactory, log *Logger) *App {
	mainTokens := labels(cfg.APIKeysByUser())
	authMode := cfg.AuthMode()
	mainEndpoint := newEndpoint(cfg.MCPName, buildTools(cfg, reg, factory, nil), "", authMode,
		actorResolver(mainTokens, authMode), log)
	app := &App{log: log, main: &subApp{path: cfg.MCPPath, tokens: mainTokens, handler: newTransport(mainEndpoint.server, mainEndpoint.sessions)}}
	if authMode == settings.AuthJWT {
		app.main.jwt = auth.NewJWTVerifier(cfg.JWTPublicKey, cfg.JWTJWKSURI, cfg.JWTIssuer, cfg.JWTAudience)
	}
	for _, d := range reg.Datasets {
		tokens := datasetLabels(d)
		ep := newEndpoint(cfg.MCPName+" dataset "+d.ID, buildTools(cfg, reg, factory, d), d.ID,
			settings.AuthStaticTokens, actorResolver(tokens, settings.AuthStaticTokens), log)
		app.mounts = append(app.mounts, mount{
			prefix: d.MountPrefix(cfg),
			app:    &subApp{path: cfg.MCPPath, tokens: tokens, handler: newTransport(ep.server, ep.sessions)},
		})
	}
	return app
}

// Endpoints reports how many MCP session managers the app runs.
func (a *App) Endpoints() int { return 1 + len(a.mounts) }

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		target := r.URL.RequestURI()
		a.log.Server(`%s - "%s %s HTTP/%d.%d" %d %s`, r.RemoteAddr, r.Method, target,
			r.ProtoMajor, r.ProtoMinor, rec.status, http.StatusText(rec.status))
	}()
	for _, m := range a.mounts {
		if strings.HasPrefix(r.URL.Path, m.prefix+"/") {
			m.app.serve(rec, r, r.URL.Path[len(m.prefix):])
			return
		}
	}
	a.main.serve(rec, r, r.URL.Path)
}

func (s *subApp) serve(w http.ResponseWriter, r *http.Request, path string) {
	if len(s.tokens) > 0 && r.Method != http.MethodOptions {
		token, _ := auth.ExtractBearer(r.Header.Get("Authorization"))
		if _, ok := auth.ResolveLabel(s.tokens, token); !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, `{"error":"Unauthorized"}`)
			return
		}
	}
	switch path {
	case s.path:
		if s.jwt != nil && !s.jwtAuthorized(w, r) {
			return
		}
		s.handler.ServeHTTP(w, r)
	case toggleSlash(s.path):
		redirectSlash(w, r)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	}
}

// invalidTokenDescription is FastMCP's enhanced RequireAuthMiddleware text.
const invalidTokenDescription = "Authentication failed. The provided bearer token is invalid, expired, or no longer " +
	"recognized by the server. To resolve: clear authentication tokens in your MCP client and reconnect. " +
	"Your client should automatically re-register and obtain new tokens."

// jwtAuthorized mirrors FastMCP's RequireAuthMiddleware: a missing
// Authorization header gets a bare challenge, an invalid one an error body.
func (s *subApp) jwtAuthorized(w http.ResponseWriter, r *http.Request) bool {
	values, present := r.Header["Authorization"]
	if !present {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.Header().Set("Content-Length", "0")
		w.Header()["Content-Type"] = nil
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	header := values[0]
	if len(header) >= 7 && strings.ToLower(header[:7]) == "bearer " && s.jwt.Verify(r.Context(), header[7:]) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="`+invalidTokenDescription+`"`)
	writeJSON(w, http.StatusUnauthorized, `{"error": "invalid_token", "error_description": "`+invalidTokenDescription+`"}`)
	return false
}

func toggleSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return strings.TrimSuffix(p, "/")
	}
	return p + "/"
}

// redirectSlash mirrors Starlette's redirect_slashes (307 to the other form).
func redirectSlash(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	target := scheme + "://" + r.Host + toggleSlash(r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	w.Header().Set("Location", target)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
