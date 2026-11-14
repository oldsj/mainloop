// Package authtest is a fake OpenID Connect issuer for tests: a discovery document, a key set and
// a signer, served over TLS from httptest.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// Issuer serves discovery and keys and signs tokens.
type Issuer struct {
	Server *httptest.Server
	// KeyFetches counts key-set requests.
	KeyFetches atomic.Int32

	mu   sync.Mutex
	kid  string
	key  *rsa.PrivateKey
	down bool
}

// NewIssuer starts an issuer with one RSA key; the server stops with the test.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	i := &Issuer{}
	i.Rotate(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if i.isDown() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": i.URL(), "jwks_uri": i.URL() + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		i.KeyFetches.Add(1)
		if i.isDown() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		i.mu.Lock()
		pub := i.key.PublicKey
		kid := i.kid
		i.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	i.Server = httptest.NewTLSServer(mux)
	t.Cleanup(i.Server.Close)
	return i
}

// URL is the issuer identifier.
func (i *Issuer) URL() string { return i.Server.URL }

// Client trusts the issuer's certificate.
func (i *Issuer) Client() *http.Client { return i.Server.Client() }

// Rotate replaces the signing key with a new one under a new key id.
func (i *Issuer) Rotate(t testing.TB) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.key = k
	i.kid = base64.RawURLEncoding.EncodeToString(k.N.Bytes()[:8])
}

// SetDown makes discovery and the key set answer 503.
func (i *Issuer) SetDown(down bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.down = down
}

func (i *Issuer) isDown() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.down
}

// Claims are a token's claims; Sign fills iss, exp and iat when they are absent.
type Claims = jwt.MapClaims

// Sign returns an RS256 token for claims, signed with the current key.
func (i *Issuer) Sign(t testing.TB, claims Claims) string {
	t.Helper()
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = i.URL()
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	if _, ok := claims["iat"]; !ok {
		claims["iat"] = time.Now().Unix()
	}
	i.mu.Lock()
	key, kid := i.key, i.kid
	i.mu.Unlock()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ServiceAccountToken returns an unsigned-looking projected token shape: the claims a kubelet puts
// in one. Only a TokenReview can say whether it is real, which is the point of the fake reviewer.
func ServiceAccountToken(t testing.TB, namespace, name, audience string, exp time.Time) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://kubernetes.test", "aud": []string{audience}, "exp": exp.Unix(),
		"sub":           "system:serviceaccount:" + namespace + ":" + name,
		"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]string{"name": name}},
	})
	s, err := tok.SignedString([]byte("not-verified-locally"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
