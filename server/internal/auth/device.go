package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// Device request tokens (docs/specs/devices.md). A registered phone signs every request with the
// P-256 key in its Secure Enclave: a short JWT bound to the request's method, path and body, good
// for one request. Only the public key is held here, so nothing the server stores lets anyone sign.
const (
	DeviceIssuer    = "mainloop-device"
	DeviceAudience  = "mainloop-api"
	DeviceTokenType = "mainloop-device+jwt"
	// deviceMaxLife is the longest a device token may live (exp - iat).
	deviceMaxLife = 60 * time.Second
	// deviceSkew absorbs the difference between the phone's clock and the server's.
	deviceSkew = 30 * time.Second
	// deviceTouchEvery limits last_seen_at writes to one a minute per device.
	deviceTouchEvery = time.Minute
	// deviceReplayCap bounds the replay cache. Past it a token is refused rather than remembered
	// imperfectly: at one request a second per device this is hours of headroom.
	deviceReplayCap = 100_000
)

// DeviceIDPattern is the form of a device id (owner_devices.id).
var DeviceIDPattern = regexp.MustCompile(`^dev_[a-z0-9]{26}$`)

// ErrUnknownDevice is returned by a DeviceStore for an id it has no device for.
var ErrUnknownDevice = errors.New("unknown device")

// DeviceRecord is what the verifier needs about a registered device.
type DeviceRecord struct {
	Key     *ecdsa.PublicKey
	Revoked bool
}

// DeviceStore reads the owner's registered devices. It is read on every device request, so a
// revocation takes effect on the next one.
type DeviceStore interface {
	// Device returns the owner's device id, or ErrUnknownDevice.
	Device(ctx context.Context, id string) (DeviceRecord, error)
	// Touch records that the device made a verified request.
	Touch(ctx context.Context, id string) error
}

// Devices verifies device request tokens.
type Devices struct {
	Store DeviceStore
	// MaxBody is the largest body a device request may carry; its hash is checked before the
	// handler reads it. It matches the API's body cap.
	MaxBody int64
	Now     func() time.Time

	mu      sync.Mutex
	used    map[string]time.Time // kid|jti → when the entry may be forgotten
	touched map[string]time.Time // device id → last Touch
}

// deviceClaims are a device token's claims.
type deviceClaims struct {
	jwt.RegisteredClaims
	Method   string `json:"htm"`
	Target   string `json:"htu"`
	BodyHash string `json:"bh"`
}

// Verify checks a device token against the request it came with and returns the device id. On
// success the request's body has been read and replaced with an identical one.
func (d *Devices) Verify(r *http.Request, raw string) (string, error) {
	var claims deviceClaims
	var kid string
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"ES256"}), jwt.WithoutClaimsValidation())
	_, err := parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if typ, _ := t.Header["typ"].(string); typ != DeviceTokenType {
			return nil, errors.New("not a device token")
		}
		kid, _ = t.Header["kid"].(string)
		if !DeviceIDPattern.MatchString(kid) {
			return nil, errors.New("malformed device id")
		}
		rec, err := d.Store.Device(r.Context(), kid)
		if err != nil {
			return nil, err
		}
		if rec.Revoked {
			return nil, errors.New("device revoked")
		}
		return rec.Key, nil
	})
	if err != nil {
		return "", fmt.Errorf("device token: %w", err)
	}
	now := d.now()
	switch {
	case claims.Issuer != DeviceIssuer:
		return "", errors.New("device token: wrong issuer")
	case claims.Subject != kid:
		return "", errors.New("device token: sub is not kid")
	case !claims.VerifyAudience(DeviceAudience, true):
		return "", errors.New("device token: wrong audience")
	case claims.IssuedAt == nil || claims.ExpiresAt == nil:
		return "", errors.New("device token: iat and exp are required")
	case !claims.ExpiresAt.After(claims.IssuedAt.Time) || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > deviceMaxLife:
		return "", errors.New("device token: lifetime over 60 seconds")
	case now.Add(deviceSkew).Before(claims.IssuedAt.Time):
		return "", errors.New("device token: issued in the future")
	case now.After(claims.ExpiresAt.Add(deviceSkew)):
		return "", errors.New("device token: expired")
	case len(claims.ID) < 16 || len(claims.ID) > 128:
		return "", errors.New("device token: jti must be 16 to 128 characters")
	case claims.Method != r.Method:
		return "", errors.New("device token: signed for another method")
	case claims.Target != r.URL.RequestURI():
		return "", errors.New("device token: signed for another path")
	}
	sum, err := d.hashBody(r)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(claims.BodyHash), []byte(sum)) != 1 {
		return "", errors.New("device token: signed for another body")
	}
	if err := d.use(kid+"|"+claims.ID, claims.ExpiresAt.Add(deviceSkew), now); err != nil {
		return "", err
	}
	d.touch(r.Context(), kid, now)
	return kid, nil
}

// hashBody reads the whole body (up to MaxBody), puts an identical body back, and returns the
// unpadded base64url SHA-256 of it.
func (d *Devices) hashBody(r *http.Request) (string, error) {
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		orig := r.Body
		b, err := io.ReadAll(io.LimitReader(orig, d.MaxBody+1))
		// Whatever happens, the handler (in observe mode) still gets the body as sent.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), orig), orig}
		if err != nil {
			return "", fmt.Errorf("device token: reading the body: %w", err)
		}
		if int64(len(b)) > d.MaxBody {
			return "", errors.New("device token: body too large to verify")
		}
		body = b
	}
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// use records a token id until it would have expired anyway. A second use is a replay. The cache
// is per process; with several replicas a replay could reach another one within the token's
// minute, for the same method, path and body only.
func (d *Devices) use(key string, until, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.used == nil {
		d.used = map[string]time.Time{}
	}
	if _, seen := d.used[key]; seen {
		return errors.New("device token: already used")
	}
	if len(d.used) >= deviceReplayCap {
		for k, exp := range d.used {
			if now.After(exp) {
				delete(d.used, k)
			}
		}
		if len(d.used) >= deviceReplayCap {
			return errors.New("device token: replay cache full")
		}
	}
	d.used[key] = until
	if len(d.used)%1024 == 0 {
		for k, exp := range d.used {
			if now.After(exp) {
				delete(d.used, k)
			}
		}
	}
	return nil
}

// touch updates last_seen_at at most once a minute per device. A failure only loses the stamp.
func (d *Devices) touch(ctx context.Context, id string, now time.Time) {
	d.mu.Lock()
	if d.touched == nil {
		d.touched = map[string]time.Time{}
	}
	last, ok := d.touched[id]
	due := !ok || now.Sub(last) >= deviceTouchEvery
	if due {
		d.touched[id] = now
	}
	d.mu.Unlock()
	if due {
		_ = d.Store.Touch(ctx, id)
	}
}

func (d *Devices) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
