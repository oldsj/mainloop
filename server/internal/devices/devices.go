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
	"crypto/subtle"
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
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/events"
)

// RegistrationItemPrefix starts the id of the Needs-you item each registration opens.
const RegistrationItemPrefix = "device_registered_"

// Answers to a registration's Needs-you item.
const (
	AnswerKeep   = "Keep"
	AnswerRevoke = "Revoke"
)

// DevicesAPI serves /devices.
type DevicesAPI struct {
	Pool    *pgxpool.Pool
	OwnerID string
	Now     func() time.Time
	// OnRevoke is told of each revocation, to end the device's open requests in this process
	// (auth.Devices.Revoked). Other processes notice within a few seconds on their own.
	OnRevoke func(id string)
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

// UseToken implements auth.DeviceStore.
func (s *Store) UseToken(ctx context.Context, id, jti string, until time.Time) (bool, error) {
	n, err := dbq.New(s.Pool).UseDeviceToken(ctx, dbq.UseDeviceTokenParams{DeviceID: id, Jti: jti, ExpiresAt: until})
	return n == 1, err
}

// PruneTokenUses implements auth.DeviceStore.
func (s *Store) PruneTokenUses(ctx context.Context) error {
	_, err := dbq.New(s.Pool).PruneDeviceTokenUses(ctx)
	return err
}

// PublicKey decodes JWK coordinates into a P-256 key, refusing a point that is not on the curve.
func PublicKey(x, y string) (*ecdsa.PublicKey, error) {
	// Strict: one encoding per key, so one key can't be registered again under another thumbprint.
	xb, err1 := base64.RawURLEncoding.Strict().DecodeString(x)
	yb, err2 := base64.RawURLEncoding.Strict().DecodeString(y)
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

// RegisterDevice implements api.StrictServerInterface. The auth layer has already required the
// owner's recent Pocket ID sign-in through the app's own client (auth.CanRegisterDevice); this
// checks it again, so the handler never registers a device for anything else, in observe mode
// (which refuses nothing) included. With auth off there is no principal, so nothing registers.
// The token's nonce must be the new key's thumbprint: a token registers the key it was minted for
// and, since a key registers once, only once. Every registration opens a Needs-you item.
func (a *DevicesAPI) RegisterDevice(ctx context.Context, r api.RegisterDeviceRequestObject) (api.RegisterDeviceResponseObject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	p, ok := auth.FromContext(ctx)
	if !ok {
		return api.RegisterDevice403JSONResponse{Detail: "Registering a device needs the owner's Pocket ID sign-in"}, nil
	}
	if err := auth.CanRegisterDevice(p, a.now()); err != nil {
		return api.RegisterDevice403JSONResponse{Detail: "Registering a device needs the owner's Pocket ID sign-in in the app, from the last 10 minutes"}, nil
	}
	body := r.Body
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return nil, apierr.Invalid("name must not be blank")
	}
	if _, err := PublicKey(body.RequestKey.X, body.RequestKey.Y); err != nil {
		return nil, apierr.Invalid("request_key: " + err.Error())
	}
	thumb := Thumbprint(body.RequestKey.X, body.RequestKey.Y)
	if subtle.ConstantTimeCompare([]byte(thumb), []byte(p.Nonce)) != 1 {
		return api.RegisterDevice403JSONResponse{Detail: "The sign-in was not made for this key"}, nil
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	var jti *string
	if p.TokenID != "" {
		jti = &p.TokenID
	}
	var row dbq.OwnerDevice
	err = events.InTxPublish(ctx, a.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		var err error
		row, err = dbq.New(tx).InsertOwnerDevice(ctx, dbq.InsertOwnerDeviceParams{
			ID: id, OwnerID: a.OwnerID, Name: name, Platform: string(body.Platform),
			RequestKeyX: body.RequestKey.X, RequestKeyY: body.RequestKey.Y, KeyThumbprint: thumb,
			RegisteredBy: p.Name, RegistrationJti: jti, RegistrationAuthTime: p.AuthTime,
		})
		if err != nil {
			return nil, err
		}
		return attention.Open(ctx, tx, a.OwnerID, registrationItem(row))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A key, and a sign-in, registers once, ever: a phone that signs out makes a new key.
		return api.RegisterDevice409JSONResponse{Detail: "This request key or sign-in is already registered"}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.RegisterDevice201JSONResponse(toAPI(row, "")), nil
}

// registrationItem tells the owner a device was registered, so one they didn't register is seen
// and revoked: the request key outlives the sign-in that registered it.
func registrationItem(r dbq.OwnerDevice) attention.NewQueueItem {
	content := fmt.Sprintf("A device named %q was registered at %s, with a Pocket ID sign-in of %s.\n\n"+
		"Device %s · key %s\n\n"+
		"If you didn't register it, answer Revoke: its key stops working at once. Keep closes this. "+
		"Answer from the web app: a device can't answer this.",
		r.Name, r.CreatedAt.UTC().Format(time.RFC1123), r.RegisteredBy, r.ID, r.KeyThumbprint[:16])
	return attention.NewQueueItem{
		ID: RegistrationItemPrefix + r.ID, ItemType: api.QueueItemTypeQuestion, Priority: api.QueueItemPriorityHigh,
		Title: "New device registered: " + r.Name, Content: content, Needed: "New device " + r.Name + ": keep or revoke?",
		Context: map[string]any{"device_id": r.ID}, Options: []string{AnswerKeep, AnswerRevoke},
	}
}

// AnswerRegistration is the attention hook for a registration's Needs-you item: Revoke revokes
// the device in the answer's transaction, Keep closes the item. Only the owner's Pocket ID sign-in
// answers it, never a device: a device registered by someone else could otherwise keep itself.
func (a *DevicesAPI) AnswerRegistration(ctx context.Context, tx pgx.Tx, sourceID, response string) error {
	if p, ok := auth.FromContext(ctx); ok && p.Kind == auth.KindDevice {
		return fmt.Errorf("%w: a device can't answer a device registration; answer from the web app", attention.ErrForbidden)
	}
	id := strings.TrimPrefix(sourceID, RegistrationItemPrefix)
	switch strings.TrimSpace(response) {
	case AnswerKeep:
		return nil
	case AnswerRevoke:
		_, err := dbq.New(tx).RevokeOwnerDevice(ctx, dbq.RevokeOwnerDeviceParams{ID: id, OwnerID: a.OwnerID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if a.OnRevoke != nil {
			a.OnRevoke(id)
		}
		return nil
	default:
		return fmt.Errorf("%w: answer %s or %s", attention.ErrInvalid, AnswerKeep, AnswerRevoke)
	}
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
	if a.OnRevoke != nil {
		a.OnRevoke(r.DeviceId)
	}
	return api.RevokeDevice204Response{}, nil
}
