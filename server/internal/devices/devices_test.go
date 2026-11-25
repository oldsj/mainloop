package devices_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
	"github.com/oldsj/mainloop/server/internal/devices"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

// pocketID is the app's Pocket ID sign-in at authTime, for the key whose thumbprint is nonce.
func pocketID(authTime time.Time, nonce string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "bearer",
		RegistrationOnly: true, DeviceClient: true, AuthTime: authTime, Nonce: nonce, TokenID: "jti-" + nonce,
	})
}

// signIn is a fresh sign-in made for d's key.
func signIn(d *authtest.Device) context.Context {
	return pocketID(time.Now(), thumb(d))
}

func thumb(d *authtest.Device) string {
	x, y := d.JWK()
	return devices.Thumbprint(x, y)
}

func asDevice(id string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindDevice, Name: id, Scope: auth.ScopeOwner, Via: "device"})
}

func registration(d *authtest.Device) api.RegisterDeviceRequestObject {
	x, y := d.JWK()
	return api.RegisterDeviceRequestObject{Body: &api.DeviceRegistration{
		Name: "  Owner's iPhone Duo ", Platform: api.DeviceRegistrationPlatformIos,
		RequestKey: api.DeviceKey{Kty: api.EC, Crv: api.P256, X: x, Y: y},
	}}
}

func register(t *testing.T, a *devices.DevicesAPI, d *authtest.Device) api.Device {
	t.Helper()
	res, err := a.RegisterDevice(signIn(d), registration(d))
	if err != nil {
		t.Fatal(err)
	}
	created, ok := res.(api.RegisterDevice201JSONResponse)
	if !ok {
		t.Fatalf("register = %#v", res)
	}
	return api.Device(created)
}

func TestRegisterListRevoke(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	phone := authtest.NewDevice(t, "dev_placeholder00000000000000")

	dev := register(t, a, phone)
	x, y := phone.JWK()
	if !auth.DeviceIDPattern.MatchString(dev.Id) || dev.Name != "Owner's iPhone Duo" || dev.Platform != api.DevicePlatformIos ||
		dev.KeyThumbprint != devices.Thumbprint(x, y) || dev.RevokedAt != nil || dev.LastSeenAt != nil || dev.Current {
		t.Fatalf("registered = %+v", dev)
	}

	// One registration per key, ever.
	if res, _ := a.RegisterDevice(signIn(phone), registration(phone)); !isType[api.RegisterDevice409JSONResponse](res) {
		t.Fatalf("same key again = %#v", res)
	}

	// The device sees itself as current; anyone else sees it listed.
	res, err := a.ListDevices(asDevice(dev.Id), api.ListDevicesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if list := res.(api.ListDevices200JSONResponse); len(list) != 1 || list[0].Id != dev.Id || !list[0].Current {
		t.Fatalf("list as the device = %+v", list)
	}
	cur, err := a.GetCurrentDevice(asDevice(dev.Id), api.GetCurrentDeviceRequestObject{})
	if err != nil || !api.Device(cur.(api.GetCurrentDevice200JSONResponse)).Current {
		t.Fatalf("current = %#v, %v", cur, err)
	}
	if cur, _ := a.GetCurrentDevice(signIn(phone), api.GetCurrentDeviceRequestObject{}); !isType[api.GetCurrentDevice404JSONResponse](cur) {
		t.Fatalf("current without a device = %#v", cur)
	}

	// The verifier reads it from the store, then not after revocation.
	store := &devices.Store{Pool: db.Pool, OwnerID: owner}
	verify := &auth.Devices{Store: store, MaxBody: 1 << 20, Audience: authtest.DeviceOrigin}
	signed := &authtest.Device{ID: dev.Id, Key: phone.Key}
	if _, err := verify.Verify(request("/attention"), signed.Token(t, "GET", "/attention", nil, nil)); err != nil {
		t.Fatalf("a registered device's token: %v", err)
	}
	rec, _ := store.Device(context.Background(), dev.Id)
	if rec.Revoked {
		t.Fatal("revoked before revocation")
	}
	after, _ := a.ListDevices(context.Background(), api.ListDevicesRequestObject{})
	if l := after.(api.ListDevices200JSONResponse); l[0].LastSeenAt == nil {
		t.Fatal("a verified request did not record last_seen_at")
	}

	// Another device may not revoke this one; it may revoke itself, and again.
	if res, _ := a.RevokeDevice(asDevice("dev_zzzzzzzzzzzzzzzzzzzzzzzzzz"), api.RevokeDeviceRequestObject{DeviceId: dev.Id}); !isType[api.RevokeDevice403JSONResponse](res) {
		t.Fatalf("another device revoking = %#v", res)
	}
	for range 2 {
		if res, err := a.RevokeDevice(asDevice(dev.Id), api.RevokeDeviceRequestObject{DeviceId: dev.Id}); err != nil || !isType[api.RevokeDevice204Response](res) {
			t.Fatalf("revoking itself = %#v, %v", res, err)
		}
	}
	if rec, _ := store.Device(context.Background(), dev.Id); !rec.Revoked {
		t.Fatal("not revoked")
	}
	if _, err := verify.Verify(request("/attention"), signed.Token(t, "GET", "/attention", nil, nil)); err == nil {
		t.Fatal("a revoked device's token verified")
	}
	if res, _ := a.RevokeDevice(context.Background(), api.RevokeDeviceRequestObject{DeviceId: "dev_00000000000000000000000000"}); !isType[api.RevokeDevice404JSONResponse](res) {
		t.Fatalf("unknown device = %#v", res)
	}

	// Another owner's API and store see none of it.
	other := &devices.DevicesAPI{Pool: db.Pool, OwnerID: "someone-else"}
	if res, _ := other.ListDevices(context.Background(), api.ListDevicesRequestObject{}); len(res.(api.ListDevices200JSONResponse)) != 0 {
		t.Fatal("another owner lists the device")
	}
	if _, err := (&devices.Store{Pool: db.Pool, OwnerID: "someone-else"}).Device(context.Background(), dev.Id); !errors.Is(err, auth.ErrUnknownDevice) {
		t.Fatalf("another owner's store: %v", err)
	}
}

func TestOnlyAFreshPocketIDSignInRegisters(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	phone := authtest.NewDevice(t, "dev_placeholder00000000000000")
	other := authtest.NewDevice(t, "dev_placeholder00000000000000")
	webBearer := auth.Principal{Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "bearer", AuthTime: time.Now(), Nonce: thumb(phone)}
	for name, ctx := range map[string]context.Context{
		"no principal (auth off)":      context.Background(),
		"stale sign-in":                pocketID(time.Now().Add(-11*time.Minute), thumb(phone)),
		"no auth_time":                 pocketID(time.Time{}, thumb(phone)),
		"auth_time in the future":      pocketID(time.Now().Add(5*time.Minute), thumb(phone)),
		"no nonce":                     pocketID(time.Now(), ""),
		"a sign-in made for other key": pocketID(time.Now(), thumb(other)),
		"the web app's audience":       auth.WithPrincipal(context.Background(), webBearer),
		"a device":                     asDevice("dev_aaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"the login cookie": auth.WithPrincipal(context.Background(), auth.Principal{
			Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "cookie", DeviceClient: true, AuthTime: time.Now(), Nonce: thumb(phone),
		}),
		"the operator ServiceAccount": auth.WithPrincipal(context.Background(), auth.Principal{
			Kind: auth.KindServiceAccount, Name: "mainloop/mainloop-operator", Scope: auth.ScopeOwner, Via: "bearer",
		}),
	} {
		if res, err := a.RegisterDevice(ctx, registration(phone)); err != nil || !isType[api.RegisterDevice403JSONResponse](res) {
			t.Errorf("%s: %#v, %v", name, res, err)
		}
	}
}

// A sign-in registers one key, once: its token id is recorded.
func TestASignInRegistersOnce(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	first, second := authtest.NewDevice(t, "dev_placeholder00000000000000"), authtest.NewDevice(t, "dev_placeholder00000000000000")
	register(t, a, first)
	// The same token id, as a replayed sign-in would carry, for a key whose thumbprint it names.
	again := auth.WithPrincipal(context.Background(), auth.Principal{
		Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "bearer",
		DeviceClient: true, AuthTime: time.Now(), Nonce: thumb(second), TokenID: "jti-" + thumb(first),
	})
	if res, err := a.RegisterDevice(again, registration(second)); err != nil || !isType[api.RegisterDevice409JSONResponse](res) {
		t.Fatalf("a reused sign-in: %#v, %v", res, err)
	}
}

// Every registration opens a Needs-you item. The owner's Revoke revokes the device; a device may
// not answer it, so a device registered by someone else can't keep itself.
func TestARegistrationAsksTheOwner(t *testing.T) {
	db := testdb.New(t)
	var revoked []string
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner, OnRevoke: func(id string) { revoked = append(revoked, id) }}
	svc := &attention.Service{Pool: db.Pool, OwnerID: owner}
	svc.OnAnswer(devices.RegistrationItemPrefix, a.AnswerRegistration)
	ctx := context.Background()

	dev := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	item := devices.RegistrationItemPrefix + dev.Id
	var title, kind string
	if err := db.Pool.QueryRow(ctx, `SELECT title, kind FROM attention_items WHERE owner_id = $1 AND source_id = $2 AND state = 'open'`, owner, item).Scan(&title, &kind); err != nil {
		t.Fatalf("no open Needs-you item for the registration: %v", err)
	}
	if !strings.Contains(title, "Owner's iPhone Duo") {
		t.Fatalf("item title %q", title)
	}

	if err := svc.Respond(asDevice(dev.Id), item, api.QueueItemResponse{Response: devices.AnswerKeep}); !errors.Is(err, attention.ErrForbidden) {
		t.Fatalf("the device keeping itself: %v", err)
	}
	if err := svc.Respond(ctx, item, api.QueueItemResponse{Response: "Maybe"}); !errors.Is(err, attention.ErrInvalid) {
		t.Fatalf("another answer: %v", err)
	}
	owner := auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "cookie"})
	if err := svc.Respond(owner, item, api.QueueItemResponse{Response: devices.AnswerRevoke}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := (&devices.Store{Pool: db.Pool, OwnerID: "owner-1"}).Device(ctx, dev.Id); !rec.Revoked {
		t.Fatal("Revoke did not revoke the device")
	}
	if len(revoked) != 1 || revoked[0] != dev.Id {
		t.Fatalf("OnRevoke calls = %v", revoked)
	}

	// Keep leaves the device as it is.
	kept := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	if err := svc.Respond(owner, devices.RegistrationItemPrefix+kept.Id, api.QueueItemResponse{Response: devices.AnswerKeep}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := (&devices.Store{Pool: db.Pool, OwnerID: "owner-1"}).Device(ctx, kept.Id); rec.Revoked {
		t.Fatal("Keep revoked the device")
	}
}

// A write token is recorded in the database, so it is good once across replicas and restarts.
func TestTheStoreRecordsUsedTokens(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	dev := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	store := &devices.Store{Pool: db.Pool, OwnerID: owner}
	ctx := context.Background()
	until := time.Now().Add(time.Minute)
	if fresh, err := store.UseToken(ctx, dev.Id, "jti-0123456789abcdef", until); err != nil || !fresh {
		t.Fatalf("first use: %v, %v", fresh, err)
	}
	if fresh, err := store.UseToken(ctx, dev.Id, "jti-0123456789abcdef", until); err != nil || fresh {
		t.Fatalf("second use: %v, %v", fresh, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE device_token_uses SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneTokenUses(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM device_token_uses`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after pruning: %d, %v", n, err)
	}
}

func TestRegistrationValidatesTheKey(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	phone := authtest.NewDevice(t, "dev_placeholder00000000000000")
	bad := registration(phone)
	bad.Body.RequestKey.Y = bad.Body.RequestKey.X // almost certainly not on the curve
	if _, err := a.RegisterDevice(signIn(phone), bad); !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("off-curve key: %v", err)
	}
	blank := registration(phone)
	blank.Body.Name = "   "
	if _, err := a.RegisterDevice(signIn(phone), blank); !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("blank name: %v", err)
	}
}

func TestARevokedDeviceStaysRevokedAndKeysNeverChange(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	dev := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	ctx := context.Background()
	other, _ := authtest.NewDevice(t, "dev_placeholder00000000000000").JWK()
	if _, err := db.Pool.Exec(ctx, `UPDATE owner_devices SET request_key_x = $2 WHERE id = $1`, dev.Id, other); err == nil {
		t.Fatal("a device's key was changed")
	}
	if _, err := a.RevokeDevice(ctx, api.RevokeDeviceRequestObject{DeviceId: dev.Id}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE owner_devices SET revoked_at = NULL WHERE id = $1`, dev.Id); err == nil {
		t.Fatal("a revoked device was brought back")
	}
}

func TestPublicKeyRefusesWhatIsNotAP256Point(t *testing.T) {
	phone := authtest.NewDevice(t, "dev_placeholder00000000000000")
	x, y := phone.JWK()
	if _, err := devices.PublicKey(x, y); err != nil {
		t.Fatal(err)
	}
	for name, xy := range map[string][2]string{
		"short":    {x[:42], y},
		"padded":   {x + "=", y},
		"swapped":  {y, x},
		"identity": {strings.Repeat("A", 43), strings.Repeat("A", 43)},
		// The last character carries 2 spare bits; another value of them is the same 32 bytes.
		"non-canonical": {x[:42] + string(nextSpare(x[42])), y},
	} {
		if _, err := devices.PublicKey(xy[0], xy[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// nextSpare is the base64url character with the same 4 data bits as c and other spare bits.
func nextSpare(c byte) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, c)
	return alphabet[(i&^3)|((i+1)&3)]
}

func request(target string) *http.Request {
	return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://mainloop.test"+target, nil)
}

func isType[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

func isStatus(err error, status int) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Status == status
}

func TestADeviceSetsAndClearsItsPushToken(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	first := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	second := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	token := strings.Repeat("0a", 32)
	set := func(ctx context.Context, tok string, env api.APNsEnvironment) (api.SetDevicePushResponseObject, error) {
		return a.SetDevicePush(ctx, api.SetDevicePushRequestObject{Body: &api.DevicePushRegistration{Token: tok, Environment: env}})
	}

	// Only a device sets a token, and only its own.
	if res, _ := set(signIn(authtest.NewDevice(t, "dev_placeholder00000000000000")), token, api.Production); !isType[api.SetDevicePush403JSONResponse](res) {
		t.Fatalf("set without a device = %#v", res)
	}
	if res, _ := set(asDevice("dev_zzzzzzzzzzzzzzzzzzzzzzzzzz"), token, api.Production); !isType[api.SetDevicePush404JSONResponse](res) {
		t.Fatalf("set by an unknown device = %#v", res)
	}
	for _, bad := range []string{"0A" + token[2:], token[:62], token + "zz"} {
		var ae *apierr.Error
		if _, err := set(asDevice(first.Id), bad, api.Production); !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
			t.Fatalf("token %q = %v", bad, err)
		}
	}

	res, err := set(asDevice(first.Id), token, api.Development)
	if err != nil {
		t.Fatal(err)
	}
	got := api.Device(res.(api.SetDevicePush200JSONResponse))
	if got.Push == nil || got.Push.Environment != api.Development || got.Push.InvalidAt != nil || !got.Current {
		t.Fatalf("after set = %+v", got.Push)
	}

	// A refused token is marked invalid; setting it again clears the mark.
	if _, err := db.Pool.Exec(context.Background(), `UPDATE device_push_tokens SET invalid_at = now() WHERE device_id = $1`, first.Id); err != nil {
		t.Fatal(err)
	}
	cur, _ := a.GetCurrentDevice(asDevice(first.Id), api.GetCurrentDeviceRequestObject{})
	if p := cur.(api.GetCurrentDevice200JSONResponse).Push; p == nil || p.InvalidAt == nil {
		t.Fatalf("current after refusal = %+v", p)
	}
	if res, _ := set(asDevice(first.Id), token, api.Production); res.(api.SetDevicePush200JSONResponse).Push.InvalidAt != nil {
		t.Fatal("setting the token again left it invalid")
	}

	// A token moves to the device that sets it last.
	if _, err := set(asDevice(second.Id), token, api.Production); err != nil {
		t.Fatal(err)
	}
	list, _ := a.ListDevices(context.Background(), api.ListDevicesRequestObject{})
	for _, d := range list.(api.ListDevices200JSONResponse) {
		if (d.Push != nil) != (d.Id == second.Id) {
			t.Fatalf("%s push = %+v", d.Id, d.Push)
		}
	}

	if res, _ := a.ClearDevicePush(context.Background(), api.ClearDevicePushRequestObject{}); !isType[api.ClearDevicePush403JSONResponse](res) {
		t.Fatalf("clear without a device = %#v", res)
	}
	for range 2 {
		if res, err := a.ClearDevicePush(asDevice(second.Id), api.ClearDevicePushRequestObject{}); err != nil || !isType[api.ClearDevicePush204Response](res) {
			t.Fatalf("clear = %#v, %v", res, err)
		}
	}
	cur, _ = a.GetCurrentDevice(asDevice(second.Id), api.GetCurrentDeviceRequestObject{})
	if p := cur.(api.GetCurrentDevice200JSONResponse).Push; p != nil {
		t.Fatalf("after clear = %+v", p)
	}

	// Signing out (revoking itself) takes the token with it.
	if _, err := set(asDevice(first.Id), token, api.Production); err != nil {
		t.Fatal(err)
	}
	if res, err := a.RevokeDevice(asDevice(first.Id), api.RevokeDeviceRequestObject{DeviceId: first.Id}); err != nil || !isType[api.RevokeDevice204Response](res) {
		t.Fatalf("revoke = %#v, %v", res, err)
	}
	list, _ = a.ListDevices(context.Background(), api.ListDevicesRequestObject{})
	for _, d := range list.(api.ListDevices200JSONResponse) {
		if d.Push != nil {
			t.Fatalf("%s still has a push token after revocation: %+v", d.Id, d.Push)
		}
	}
}

// Given a registration with a Touch ID approval key, the device is registered with both keys, the
// list shows the approval key's thumbprint and JWK, and the owner's registration item names it.
// The approval key must differ from the request key, be a P-256 point, and can't be added or
// changed later.
func TestARegistrationCarriesTheApprovalKey(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	phone := authtest.NewDevice(t, "dev_placeholder00000000000000")
	ax, ay := authtest.NewDevice(t, "dev_placeholder00000000000000").JWK()

	same := registration(phone)
	same.Body.ApprovalKey = &same.Body.RequestKey
	if _, err := a.RegisterDevice(signIn(phone), same); !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("approval key equal to the request key: %v", err)
	}
	offCurve := registration(phone)
	offCurve.Body.ApprovalKey = &api.DeviceKey{Kty: api.EC, Crv: api.P256, X: ax, Y: ax}
	if _, err := a.RegisterDevice(signIn(phone), offCurve); !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("off-curve approval key: %v", err)
	}

	req := registration(phone)
	req.Body.ApprovalKey = &api.DeviceKey{Kty: api.EC, Crv: api.P256, X: ax, Y: ay}
	res, err := a.RegisterDevice(signIn(phone), req)
	if err != nil {
		t.Fatal(err)
	}
	dev := api.Device(res.(api.RegisterDevice201JSONResponse))
	want := devices.Thumbprint(ax, ay)
	if dev.ApprovalKey == nil || dev.ApprovalKey.Thumbprint != want || dev.ApprovalKey.Jwk.X != ax || dev.ApprovalKey.Jwk.Y != ay {
		t.Fatalf("approval key = %+v", dev.ApprovalKey)
	}
	list, err := a.ListDevices(context.Background(), api.ListDevicesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if got := list.(api.ListDevices200JSONResponse)[0].ApprovalKey; got == nil || got.Thumbprint != want {
		t.Fatalf("listed approval key = %+v", got)
	}
	var content string
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT detail->>'content' FROM attention_items WHERE source_id = $1`, devices.RegistrationItemPrefix+dev.Id).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "Touch ID approval key "+want[:16]) {
		t.Fatalf("registration item: %s", content)
	}
	if _, err := db.Pool.Exec(context.Background(), `UPDATE owner_devices SET approval_key_x = NULL, approval_key_y = NULL, approval_key_thumbprint = NULL WHERE id = $1`, dev.Id); err == nil {
		t.Fatal("an approval key was removed")
	}

	// A device registered without one can't have one added.
	plain := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	if plain.ApprovalKey != nil {
		t.Fatalf("approval key = %+v", plain.ApprovalKey)
	}
	bx, by := authtest.NewDevice(t, "dev_placeholder00000000000000").JWK()
	if _, err := db.Pool.Exec(context.Background(), `UPDATE owner_devices SET approval_key_x = $2, approval_key_y = $3, approval_key_thumbprint = $4 WHERE id = $1`,
		plain.Id, bx, by, devices.Thumbprint(bx, by)); err == nil {
		t.Fatal("an approval key was added after registration")
	}
}

// Given a registered phone, when it signs a request for a web session, then it gets a token once,
// the store finds the session by the token's hash (never the token), and the session ends when it
// expires or the device is revoked. A web session, a sign-in or an unsigned caller can't start one.
func TestADeviceStartsAWebSession(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	dev := register(t, a, authtest.NewDevice(t, "dev_placeholder00000000000000"))
	store := &devices.Store{Pool: db.Pool, OwnerID: owner}
	ctx := context.Background()

	res, err := a.CreateDeviceWebSession(asDevice(dev.Id), api.CreateDeviceWebSessionRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := res.(api.CreateDeviceWebSession201JSONResponse)
	if !ok || s.CookieName != auth.DeviceSessionCookie || len(s.Token) != 43 {
		t.Fatalf("session = %#v", res)
	}
	if life := time.Until(s.ExpiresAt); life < auth.DeviceSessionLife-time.Minute || life > auth.DeviceSessionLife {
		t.Fatalf("expires in %s", life)
	}
	sum := sha256.Sum256([]byte(s.Token))
	if id, err := store.WebSession(ctx, sum[:]); err != nil || id != dev.Id {
		t.Fatalf("lookup: %q, %v", id, err)
	}
	var stored int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM device_web_sessions WHERE token_hash = $1`, []byte(s.Token)).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the token itself is stored: %d, %v", stored, err)
	}

	for name, c := range map[string]context.Context{
		"a web session": auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindDevice, Name: dev.Id, Scope: auth.ScopeOwner, Via: "session"}),
		"a sign-in":     signIn(authtest.NewDevice(t, "dev_placeholder11111111111111")),
		"no principal":  ctx,
	} {
		if res, err := a.CreateDeviceWebSession(c, api.CreateDeviceWebSessionRequestObject{}); err != nil || !isType[api.CreateDeviceWebSession403JSONResponse](res) {
			t.Fatalf("%s: %#v, %v", name, res, err)
		}
	}

	if _, err := db.Pool.Exec(ctx, `UPDATE device_web_sessions SET created_at = now() - interval '13 hours', expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WebSession(ctx, sum[:]); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("expired: %v", err)
	}
	if err := store.PruneTokenUses(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM device_web_sessions`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("after pruning: %d, %v", left, err)
	}
}
