// Package devices registers the owner's phones and serves their keys to the request verifier
// (docs/specs/devices.md). A phone keeps a P-256 key in its Secure Enclave and signs every API
// request with it; only the public key is stored here.
package devices

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// DevicesAPI serves /devices.
type DevicesAPI struct {
	Pool    *pgxpool.Pool
	OwnerID string
	Now     func() time.Time
}

func (a *DevicesAPI) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Store reads devices for the request verifier. It implements auth.DeviceStore.
type Store struct {
	Pool    *pgxpool.Pool
	OwnerID string
}

var _ auth.DeviceStore = (*Store)(nil)

// Device implements auth.DeviceStore.
func (s *Store) Device(ctx context.Context, id string) (auth.DeviceRecord, error) {
	row, err := dbq.New(s.Pool).GetOwnerDevice(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.OwnerID != s.OwnerID) {
		return auth.DeviceRecord{}, auth.ErrUnknownDevice
	}
	if err != nil {
		return auth.DeviceRecord{}, err
	}
	key, err := PublicKey(row.RequestKeyX, row.RequestKeyY)
	if err != nil {
		return auth.DeviceRecord{}, fmt.Errorf("device %s: stored key: %w", id, err)
	}
	return auth.DeviceRecord{Key: key, Revoked: row.RevokedAt != nil}, nil
}

// Touch implements auth.DeviceStore.
func (s *Store) Touch(ctx context.Context, id string) error {
	return dbq.New(s.Pool).TouchOwnerDevice(ctx, id)
}

// PublicKey decodes JWK coordinates into a P-256 key, refusing a point that is not on the curve.
func PublicKey(x, y string) (*ecdsa.PublicKey, error) {
	xb, err1 := base64.RawURLEncoding.DecodeString(x)
	yb, err2 := base64.RawURLEncoding.DecodeString(y)
	if err1 != nil || err2 != nil || len(xb) != 32 || len(yb) != 32 {
		return nil, errors.New("coordinates must be 32 bytes of unpadded base64url each")
	}
	// ParseUncompressedPublicKey refuses a point that is not on the curve.
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, xb...), yb...))
	if err != nil {
		return nil, errors.New("not a P-256 point")
	}
	return key, nil
}

// Thumbprint is the RFC 7638 SHA-256 thumbprint of a P-256 JWK, unpadded base64url.
func Thumbprint(x, y string) string {
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newID returns dev_ and 26 random lowercase base32 characters (128 bits).
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dev_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func (a *DevicesAPI) ready() error {
	if a.Pool == nil {
		return apierr.Unavailable("devices need the database")
	}
	return nil
}

// current is the device that signed this request, if one did.
func current(ctx context.Context) string {
	if p, ok := auth.FromContext(ctx); ok && p.Kind == auth.KindDevice {
		return p.Name
	}
	return ""
}

func toAPI(r dbq.OwnerDevice, currentID string) api.Device {
	return api.Device{
		Id: r.ID, Name: r.Name, Platform: api.DevicePlatform(r.Platform), KeyThumbprint: r.KeyThumbprint,
		CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt, RevokedAt: r.RevokedAt, Current: r.ID == currentID,
	}
}

// ListDevices implements api.StrictServerInterface.
func (a *DevicesAPI) ListDevices(ctx context.Context, _ api.ListDevicesRequestObject) (api.ListDevicesResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	rows, err := dbq.New(a.Pool).ListOwnerDevices(ctx, a.OwnerID)
	if err != nil {
		return nil, err
	}
	cur := current(ctx)
	out := make(api.ListDevices200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAPI(r, cur))
	}
	return out, nil
}

// RegisterDevice implements api.StrictServerInterface. The auth layer has already required a
// recent Pocket ID bearer of the owner (auth.authorize); this checks it again, so the handler
// never registers a device for anything else, in observe mode (which refuses nothing) included.
// With auth off there is no principal, so no device can be registered.
func (a *DevicesAPI) RegisterDevice(ctx context.Context, r api.RegisterDeviceRequestObject) (api.RegisterDeviceResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind != auth.KindOwner || p.Via != "bearer" || p.IssuedAt.IsZero() || a.now().Sub(p.IssuedAt) > auth.RegistrationMaxAge {
		return api.RegisterDevice403JSONResponse{Detail: "Registering a device needs the owner's Pocket ID sign-in from the last 10 minutes"}, nil
	}
	body := r.Body
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return nil, apierr.Invalid("name must not be blank")
	}
	if _, err := PublicKey(body.RequestKey.X, body.RequestKey.Y); err != nil {
		return nil, apierr.Invalid("request_key: " + err.Error())
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	row, err := dbq.New(a.Pool).InsertOwnerDevice(ctx, dbq.InsertOwnerDeviceParams{
		ID: id, OwnerID: a.OwnerID, Name: name, Platform: string(body.Platform),
		RequestKeyX: body.RequestKey.X, RequestKeyY: body.RequestKey.Y,
		KeyThumbprint: Thumbprint(body.RequestKey.X, body.RequestKey.Y), RegisteredBy: p.Name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A key is registered once, ever: a phone that signs out makes a new key.
		return api.RegisterDevice409JSONResponse{Detail: "This request key is already registered"}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.RegisterDevice201JSONResponse(toAPI(row, "")), nil
}

// GetCurrentDevice implements api.StrictServerInterface.
func (a *DevicesAPI) GetCurrentDevice(ctx context.Context, _ api.GetCurrentDeviceRequestObject) (api.GetCurrentDeviceResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	id := current(ctx)
	if id == "" {
		return api.GetCurrentDevice404JSONResponse{Detail: "This request was not signed by a device"}, nil
	}
	row, err := dbq.New(a.Pool).GetOwnerDevice(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.OwnerID != a.OwnerID) {
		return api.GetCurrentDevice404JSONResponse{Detail: "Device not found"}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetCurrentDevice200JSONResponse(toAPI(row, id)), nil
}

// RevokeDevice implements api.StrictServerInterface. A device may revoke only itself.
func (a *DevicesAPI) RevokeDevice(ctx context.Context, r api.RevokeDeviceRequestObject) (api.RevokeDeviceResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if p, ok := auth.FromContext(ctx); ok && p.Kind == auth.KindDevice && p.Name != r.DeviceId {
		return api.RevokeDevice403JSONResponse{Detail: "A device may revoke only itself"}, nil
	}
	if !auth.DeviceIDPattern.MatchString(r.DeviceId) {
		return api.RevokeDevice404JSONResponse{Detail: "Device not found"}, nil
	}
	_, err := dbq.New(a.Pool).RevokeOwnerDevice(ctx, dbq.RevokeOwnerDeviceParams{ID: r.DeviceId, OwnerID: a.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.RevokeDevice404JSONResponse{Detail: "Device not found"}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.RevokeDevice204Response{}, nil
}
