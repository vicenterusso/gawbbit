// Package auth implements the bearer-token checks of the MCP endpoints:
// static API keys and FastMCP-compatible RS256 JWT verification.
package auth

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
)

// Label is one static token and its audit label.
type Label struct {
	Label string
	Token string
}

// ExtractBearer mirrors _extract_bearer_token: "<scheme> <token>" with a
// case-insensitive "bearer" scheme and a stripped, non-empty token.
func ExtractBearer(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	scheme, token, _ := strings.Cut(header, " ")
	if strings.ToLower(scheme) != "bearer" {
		return "", false
	}
	token = pyval.Strip(token)
	return token, token != ""
}

// ResolveLabel returns the label of the first matching token, comparing in
// constant time like hmac.compare_digest.
func ResolveLabel(tokens []Label, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(t.Token), []byte(token)) == 1 {
			return t.Label, true
		}
	}
	return "", false
}

const (
	jwksCacheTTL        = time.Hour
	jwksRefreshInterval = 30 * time.Second
)

// JWTVerifier mirrors fastmcp.server.auth.JWTVerifier with algorithm RS256.
type JWTVerifier struct {
	publicKey string
	jwksURI   string
	issuer    string
	audience  string
	client    *http.Client

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	cachedAt    time.Time
	lastAttempt time.Time
}

// NewJWTVerifier configures a verifier from a PEM public key or a JWKS URI.
func NewJWTVerifier(publicKey, jwksURI, issuer, audience string) *JWTVerifier {
	return &JWTVerifier{
		publicKey: publicKey,
		jwksURI:   jwksURI,
		issuer:    issuer,
		audience:  audience,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Verify reports whether the token is acceptable (signature, exp, iss, aud).
func (v *JWTVerifier) Verify(ctx context.Context, token string) bool {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, hasCrit := t.Header["crit"]; hasCrit {
			return nil, errors.New("unsupported critical header")
		}
		return v.key(ctx, t)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithoutClaimsValidation())
	if err != nil || !parsed.Valid {
		return false
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if exp, present := claims["exp"]; present && exp != nil {
		n, isNum := exp.(float64)
		if !isNum || n < now {
			return false
		}
	}
	if v.issuer != "" {
		if iss, _ := claims["iss"].(string); claims["iss"] == nil || iss != v.issuer {
			return false
		}
	}
	if v.audience != "" && !audienceMatches(claims["aud"], v.audience) {
		return false
	}
	return true
}

func audienceMatches(aud any, expected string) bool {
	switch a := aud.(type) {
	case string:
		return a == expected
	case []any:
		for _, item := range a {
			if s, ok := item.(string); ok && s == expected {
				return true
			}
		}
	}
	return false
}

func (v *JWTVerifier) key(ctx context.Context, t *jwt.Token) (any, error) {
	if v.publicKey != "" {
		return parseRSAPublicKey(v.publicKey)
	}
	kid, _ := t.Header["kid"].(string)
	return v.jwksKey(ctx, kid)
}

func parseRSAPublicKey(value string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, errors.New("invalid PEM public key")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := pub.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("public key is not RSA")
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		if rsaKey, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
}

func (v *JWTVerifier) lookup(kid string) (*rsa.PublicKey, bool) {
	if kid != "" {
		k, ok := v.keys[kid]
		return k, ok
	}
	if len(v.keys) == 1 {
		for _, k := range v.keys {
			return k, true
		}
	}
	return nil, false
}

func (v *JWTVerifier) jwksKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if now.Sub(v.cachedAt) < jwksCacheTTL {
		if k, ok := v.lookup(kid); ok {
			return k, nil
		}
	}
	if !v.lastAttempt.IsZero() && now.Sub(v.lastAttempt) < jwksRefreshInterval {
		return nil, errors.New("no matching fresh JWKS key; refresh available shortly")
	}
	v.lastAttempt = now
	keys, err := v.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	v.keys, v.cachedAt = keys, time.Now()
	if k, ok := v.lookup(kid); ok {
		return k, nil
	}
	return nil, fmt.Errorf("no usable JWKS key for kid %q", kid)
}

func (v *JWTVerifier) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("JWKS fetch failed with HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, raw := range doc.Keys {
		var jwk struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		}
		if json.Unmarshal(raw, &jwk) != nil || jwk.Kty != "RSA" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(strings.TrimRight(jwk.N, "="))
		e, errE := base64.RawURLEncoding.DecodeString(strings.TrimRight(jwk.E, "="))
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		id := jwk.Kid
		if id == "" {
			id = "_default"
		}
		keys[id] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	return keys, nil
}
