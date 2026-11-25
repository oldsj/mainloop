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
	raw, err := d.Key.PublicKey.Bytes() // 0x04 ‖ x ‖ y
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[1:33]), base64.RawURLEncoding.EncodeToString(raw[33:])
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
		"iss": auth.DeviceIssuer, "sub": d.ID, "aud": DeviceOrigin,
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
	uses    map[string]time.Time
	Touched map[string]int
	// Pruned counts PruneTokenUses calls.
	Pruned int
	// Sessions maps a web session token's SHA-256 (hex) to its device.
	Sessions map[string]string
}

// AddSession records a web session token for device id.
func (s *DeviceStore) AddSession(token, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Sessions == nil {
		s.Sessions = map[string]string{}
	}
	sum := sha256.Sum256([]byte(token))
	s.Sessions[hex.EncodeToString(sum[:])] = id
}

// WebSession implements auth.DeviceStore.
func (s *DeviceStore) WebSession(_ context.Context, tokenHash []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.Sessions[hex.EncodeToString(tokenHash)]
	if !ok {
		return "", auth.ErrUnknownSession
	}
	return id, nil
}

// DeviceOrigin is the instance origin device tokens are signed for in tests (Devices.Audience).
const DeviceOrigin = "https://mainloop.test"

// UseToken implements auth.DeviceStore: a write's token is good once, as in the database.
func (s *DeviceStore) UseToken(_ context.Context, id, jti string, until time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uses == nil {
		s.uses = map[string]time.Time{}
	}
	if _, used := s.uses[id+"|"+jti]; used {
		return false, nil
	}
	s.uses[id+"|"+jti] = until
	return true, nil
}

// PruneTokenUses implements auth.DeviceStore.
func (s *DeviceStore) PruneTokenUses(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Pruned++
	return nil
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
