// Package approval checks approvals of privileged actions signed with a phone's Touch ID approval
// key (docs/specs/privileged-actions.md, "Signed approvals"). The phone signs a short token naming
// one action and its digest. serve-api checks it against the device's registered key when the
// answer arrives; serve-executor checks it again against keys pinned in its own configuration.
package approval

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/devices"
)

const (
	// TokenType is the approval token's typ header.
	TokenType = "mainloop-approval+jwt" //nolint:gosec // G101: a JWS typ header value, not a credential
	// MetadataKey is the answer's metadata key that carries the token.
	MetadataKey = "approval"
	// MaxLife is the longest an approval token may live (exp - iat).
	MaxLife = 5 * time.Minute
	// Skew absorbs the difference between the phone's clock and the server's.
	Skew = 30 * time.Second
	// MaxTokenBytes bounds a token before it is parsed.
	MaxTokenBytes = 4096
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Claims is what a verified approval token says.
type Claims struct {
	DeviceID  string
	ActionID  string
	Digest    string
	JTI       string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type claims struct {
	jwt.RegisteredClaims
	Action string `json:"act"`
	Digest string `json:"dig"`
}

// Keys returns a device's approval key, or an error when it has none to check with.
type Keys func(deviceID string) (*ecdsa.PublicKey, error)

// Parse checks the token's signature with the key Keys gives for its kid, and its form: the
// issuer, sub equal to kid, a lifetime of at most MaxLife, a jti of 16 to 128 characters, an
// action id and a 64-hex digest. With an audience it must be signed for that instance. It does
// not check the time against a clock; see Claims.Current.
func Parse(raw string, keys Keys, audience string) (Claims, error) {
	if len(raw) > MaxTokenBytes {
		return Claims{}, errors.New("approval token: too long")
	}
	var c claims
	var kid string
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"ES256"}), jwt.WithoutClaimsValidation())
	_, err := parser.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		if typ, _ := t.Header["typ"].(string); typ != TokenType {
			return nil, errors.New("not an approval token")
		}
		kid, _ = t.Header["kid"].(string)
		if !auth.DeviceIDPattern.MatchString(kid) {
			return nil, errors.New("malformed device id")
		}
		return keys(kid)
	})
	if err != nil {
		return Claims{}, fmt.Errorf("approval token: %w", err)
	}
	switch {
	case c.Issuer != auth.DeviceIssuer:
		return Claims{}, errors.New("approval token: wrong issuer")
	case c.Subject != kid:
		return Claims{}, errors.New("approval token: sub is not kid")
	case audience != "" && !c.VerifyAudience(audience, true):
		return Claims{}, errors.New("approval token: signed for another instance")
	case c.IssuedAt == nil || c.ExpiresAt == nil:
		return Claims{}, errors.New("approval token: iat and exp are required")
	case !c.ExpiresAt.After(c.IssuedAt.Time) || c.ExpiresAt.Sub(c.IssuedAt.Time) > MaxLife:
		return Claims{}, errors.New("approval token: lifetime over 5 minutes")
	case len(c.ID) < 16 || len(c.ID) > 128:
		return Claims{}, errors.New("approval token: jti must be 16 to 128 characters")
	case c.Action == "" || len(c.Action) > 128:
		return Claims{}, errors.New("approval token: no action id")
	case !digestPattern.MatchString(c.Digest):
		return Claims{}, errors.New("approval token: the digest must be 64 lowercase hex characters")
	}
	return Claims{
		DeviceID: kid, ActionID: c.Action, Digest: c.Digest, JTI: c.ID,
		IssuedAt: c.IssuedAt.Time, ExpiresAt: c.ExpiresAt.Time,
	}, nil
}

// Current refuses a token issued in the future or already expired, with Skew either way.
func (c Claims) Current(now time.Time) error {
	switch {
	case now.Add(Skew).Before(c.IssuedAt):
		return errors.New("approval token: issued in the future")
	case now.After(c.ExpiresAt.Add(Skew)):
		return errors.New("approval token: expired")
	}
	return nil
}

// Pinned is the approval keys the owner pinned in configuration, by device id.
type Pinned map[string]*ecdsa.PublicKey

// Key implements Keys over the pinned set.
func (p Pinned) Key(deviceID string) (*ecdsa.PublicKey, error) {
	if k, ok := p[deviceID]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("device %s has no pinned approval key", deviceID)
}

// Has reports whether key is the one pinned for deviceID.
func (p Pinned) Has(deviceID string, key *ecdsa.PublicKey) bool {
	k, ok := p[deviceID]
	return ok && key != nil && k.Equal(key)
}

// pinnedKey is one entry of MAINLOOP_PRIVILEGED_APPROVAL_KEYS: a device's public approval key as
// a JWK, with the device id as its kid.
type pinnedKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParsePinned reads the pinned keys: a JSON array of P-256 JWKs, each with the device id as its
// kid. An empty string or an empty array pins nothing.
func ParsePinned(raw string) (Pinned, error) {
	if raw == "" {
		return nil, nil
	}
	var keys []pinnedKey
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&keys); err != nil {
		return nil, fmt.Errorf("approval keys: %w", err)
	}
	out := Pinned{}
	for i, k := range keys {
		if !auth.DeviceIDPattern.MatchString(k.Kid) {
			return nil, fmt.Errorf("approval keys: entry %d: kid must be a device id", i)
		}
		if k.Kty != "EC" || k.Crv != "P-256" {
			return nil, fmt.Errorf("approval keys: entry %d: not a P-256 key", i)
		}
		key, err := devices.PublicKey(k.X, k.Y)
		if err != nil {
			return nil, fmt.Errorf("approval keys: entry %d: %w", i, err)
		}
		if _, dup := out[k.Kid]; dup {
			return nil, fmt.Errorf("approval keys: %s is pinned twice", k.Kid)
		}
		out[k.Kid] = key
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
