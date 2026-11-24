package authtest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/auth"
)

// Device is a phone's request key, held in software for tests.
type Device struct {
	ID  string
	Key *ecdsa.PrivateKey
}

// NewDevice makes a device with a fresh P-256 key. id must match auth.DeviceIDPattern.
func NewDevice(t testing.TB, id string) *Device {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Device{ID: id, Key: k}
}

// JWK returns the public key's coordinates as unpadded base64url.
func (d *Device) JWK() (x, y string) {
	return base64.RawURLEncoding.EncodeToString(d.Key.X.FillBytes(make([]byte, 32))),
		base64.RawURLEncoding.EncodeToString(d.Key.Y.FillBytes(make([]byte, 32)))
}

// BodyHash is the bh claim for body.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Token signs a request token for method, target (path and query, as the API sees it) and body.
// mutate, when set, changes the claims or header before signing.
func (d *Device) Token(t testing.TB, method, target string, body []byte, mutate func(c jwt.MapClaims, h map[string]any)) string {
	t.Helper()
	now := time.Now()
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	c := jwt.MapClaims{
		"iss": auth.DeviceIssuer, "sub": d.ID, "aud": auth.DeviceAudience,
		"iat": now.Unix(), "exp": now.Add(60 * time.Second).Unix(), "jti": hex.EncodeToString(jti),
		"htm": method, "htu": target, "bh": BodyHash(body),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, c)
	tok.Header["typ"] = auth.DeviceTokenType
	tok.Header["kid"] = d.ID
	if mutate != nil {
		mutate(c, tok.Header)
	}
	s, err := tok.SignedString(d.Key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// DeviceStore is an in-memory auth.DeviceStore.
type DeviceStore struct {
	mu      sync.Mutex
	devices map[string]auth.DeviceRecord
	Touched map[string]int
}

// Add registers d.
func (s *DeviceStore) Add(d *Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devices == nil {
		s.devices = map[string]auth.DeviceRecord{}
	}
	s.devices[d.ID] = auth.DeviceRecord{Key: &d.Key.PublicKey}
}

// AddKey registers a device by its public key alone.
func (s *DeviceStore) AddKey(id string, key *ecdsa.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devices == nil {
		s.devices = map[string]auth.DeviceRecord{}
	}
	s.devices[id] = auth.DeviceRecord{Key: key}
}

// Revoke revokes id.
func (s *DeviceStore) Revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devices[id]
	r.Revoked = true
	s.devices[id] = r
}

// Device implements auth.DeviceStore.
func (s *DeviceStore) Device(_ context.Context, id string) (auth.DeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.devices[id]
	if !ok {
		return auth.DeviceRecord{}, auth.ErrUnknownDevice
	}
	return r, nil
}

// Touch implements auth.DeviceStore.
func (s *DeviceStore) Touch(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Touched == nil {
		s.Touched = map[string]int{}
	}
	s.Touched[id]++
	return nil
}
