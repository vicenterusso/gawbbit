package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestExtractBearerAndResolveLabel(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc": "abc", "bearer abc": "abc", "BEARER  abc  ": "abc", "Bearer": "", "Basic abc": "", "": "", "abc": "",
	} {
		got, ok := ExtractBearer(header)
		if got != want || ok != (want != "") {
			t.Errorf("ExtractBearer(%q) = %q, %v", header, got, ok)
		}
	}
	tokens := []Label{{"operator", "main-token"}, {"analyst", "analyst-token"}}
	if label, ok := ResolveLabel(tokens, "analyst-token"); !ok || label != "analyst" {
		t.Fatalf("label %q %v", label, ok)
	}
	if _, ok := ResolveLabel(tokens, "main"); ok {
		t.Fatal("prefix must not match")
	}
}

func sign(t *testing.T, key any, method jwt.SigningMethod, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTVerifier(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	v := NewJWTVerifier(pub, "", "iss", "aud")
	ctx := context.Background()
	good := jwt.MapClaims{"iss": "iss", "aud": "aud", "exp": 4102444800}
	cases := map[string]bool{
		sign(t, key, jwt.SigningMethodRS256, good, ""):                                                     true,
		sign(t, key, jwt.SigningMethodRS256, jwt.MapClaims{"iss": "iss", "aud": []string{"x", "aud"}}, ""): true,
		sign(t, key, jwt.SigningMethodRS256, jwt.MapClaims{"iss": "iss", "aud": "aud", "exp": 1000}, ""):   false,
		sign(t, key, jwt.SigningMethodRS256, jwt.MapClaims{"iss": "other", "aud": "aud"}, ""):              false,
		sign(t, key, jwt.SigningMethodRS256, jwt.MapClaims{"iss": "iss", "aud": "other"}, ""):              false,
		sign(t, key, jwt.SigningMethodRS256, jwt.MapClaims{"iss": "iss", "aud": "aud", "exp": "soon"}, ""): false,
		sign(t, []byte(pub), jwt.SigningMethodHS256, good, ""):                                             false,
		"not.a.token": false,
	}
	for token, want := range cases {
		if got := v.Verify(ctx, token); got != want {
			t.Errorf("Verify(%s...) = %v, want %v", token[:10], got, want)
		}
	}

	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(jwks) }))
	defer srv.Close()
	jv := NewJWTVerifier("", srv.URL, "", "")
	if !jv.Verify(ctx, sign(t, key, jwt.SigningMethodRS256, good, "k1")) {
		t.Fatal("JWKS token with kid rejected")
	}
	if !jv.Verify(ctx, sign(t, key, jwt.SigningMethodRS256, good, "")) {
		t.Fatal("JWKS token without kid rejected with a single key")
	}
	if jv.Verify(ctx, sign(t, key, jwt.SigningMethodRS256, good, "unknown")) {
		t.Fatal("unknown kid accepted")
	}
}
