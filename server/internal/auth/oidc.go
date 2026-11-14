package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

const (
	// keysMaxAge is how long a fetched key set is used before it is fetched again.
	keysMaxAge = time.Hour
	// keysRetryEvery bounds fetches: an unknown key id or an outage asks the issuer at most this often.
	keysRetryEvery = 30 * time.Second
	// leeway absorbs clock skew on exp and nbf.
	leeway = time.Minute
	// maxDocumentBytes caps a discovery or key-set document.
	maxDocumentBytes = 1 << 20
	fetchTimeout     = 5 * time.Second
)

// signingMethods are the algorithms an owner token may use: RSA only, which is what Pocket ID signs
// with. "none", HMAC and anything else are refused.
var signingMethods = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}

// OwnerClaims are the claims of a verified owner token.
type OwnerClaims struct {
	jwt.RegisteredClaims
	Groups []string `json:"groups,omitempty"`
}

// OIDC verifies tokens an OpenID Connect issuer signed, with the keys its discovery document names.
// Keys are cached; when the issuer cannot be reached the last good keys keep verifying.
type OIDC struct {
	Issuer   string
	Audience string
	Client   *http.Client
	Now      func() time.Time

	mu      sync.Mutex
	jwksURI string
	keys    map[string]jwk
	fetched time.Time
	tried   time.Time
}

type jwk struct {
	alg string
	key crypto.PublicKey
}

// Verify checks the token's signature, issuer, audience, expiry and subject.
func (o *OIDC) Verify(ctx context.Context, raw string) (*OwnerClaims, error) {
	var claims OwnerClaims
	parser := jwt.NewParser(jwt.WithValidMethods(signingMethods), jwt.WithoutClaimsValidation())
	_, err := parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, err := o.key(ctx, kid)
		if err != nil {
			return nil, err
		}
		if k.alg != "" && k.alg != t.Method.Alg() {
			return nil, fmt.Errorf("key %q is for %s, token uses %s", kid, k.alg, t.Method.Alg())
		}
		return k.key, nil
	})
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	now := o.now()
	switch {
	case claims.Issuer != o.Issuer:
		return nil, errors.New("token: wrong issuer")
	case !claims.VerifyAudience(o.Audience, true):
		return nil, errors.New("token: wrong audience")
	case claims.ExpiresAt == nil:
		return nil, errors.New("token: no expiry")
	case now.After(claims.ExpiresAt.Add(leeway)):
		return nil, errors.New("token: expired")
	case claims.NotBefore != nil && now.Add(leeway).Before(claims.NotBefore.Time):
		return nil, errors.New("token: not yet valid")
	case claims.Subject == "":
		return nil, errors.New("token: no subject")
	}
	return &claims, nil
}

func (o *OIDC) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// key returns the signing key for kid, fetching the key set when it is missing, stale or lacks kid,
// at most once per keysRetryEvery. A failed fetch keeps the keys already held.
func (o *OIDC) key(ctx context.Context, kid string) (jwk, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	k, found := o.lookup(kid)
	now := o.now()
	stale := o.keys == nil || now.Sub(o.fetched) > keysMaxAge
	if (stale || !found) && now.Sub(o.tried) >= keysRetryEvery {
		o.tried = now
		keys, err := o.fetch(ctx)
		if err == nil {
			o.keys, o.fetched = keys, now
			k, found = o.lookup(kid)
		} else if !found {
			return jwk{}, fmt.Errorf("signing keys unavailable: %w", err)
		}
	}
	if !found {
		return jwk{}, fmt.Errorf("unknown signing key %q", kid)
	}
	return k, nil
}

// lookup finds kid in the held keys. A token without a kid matches only a set of one key.
func (o *OIDC) lookup(kid string) (jwk, bool) {
	if kid == "" && len(o.keys) == 1 {
		for _, k := range o.keys {
			return k, true
		}
	}
	k, ok := o.keys[kid]
	return k, ok && kid != ""
}

// fetch reads the discovery document (once) and then the key set. It runs under o.mu, which only
// serializes verifiers of this process while a fetch is due.
func (o *OIDC) fetch(ctx context.Context) (map[string]jwk, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if o.jwksURI == "" {
		var doc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := o.getJSON(ctx, o.Issuer+"/.well-known/openid-configuration", &doc); err != nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		if doc.Issuer != o.Issuer {
			return nil, fmt.Errorf("discovery names issuer %q, want %q", doc.Issuer, o.Issuer)
		}
		if doc.JWKSURI == "" {
			return nil, errors.New("discovery has no jwks_uri")
		}
		o.jwksURI = doc.JWKSURI
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := o.getJSON(ctx, o.jwksURI, &set); err != nil {
		return nil, fmt.Errorf("key set: %w", err)
	}
	keys := map[string]jwk{}
	for _, raw := range set.Keys {
		kid, k, err := parseJWK(raw)
		if err != nil {
			continue // an unusable key (encryption, unknown type) is skipped, not fatal
		}
		keys[kid] = k
	}
	if len(keys) == 0 {
		return nil, errors.New("key set has no usable signing keys")
	}
	return keys, nil
}

func (o *OIDC) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDocumentBytes)).Decode(out)
}

// parseJWK reads one RSA signing key; other key types are skipped by the caller.
func parseJWK(raw []byte) (string, jwk, error) {
	var k struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return "", jwk{}, err
	}
	if k.Use != "" && k.Use != "sig" {
		return "", jwk{}, errors.New("not a signing key")
	}
	if k.Kty == "RSA" {
		n, err1 := b64int(k.N)
		e, err2 := b64int(k.E)
		if err1 != nil || err2 != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || n.BitLen() < 2048 {
			return "", jwk{}, errors.New("bad RSA key")
		}
		return k.Kid, jwk{alg: k.Alg, key: &rsa.PublicKey{N: n, E: int(e.Int64())}}, nil
	}
	return "", jwk{}, errors.New("unsupported key type")
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}
