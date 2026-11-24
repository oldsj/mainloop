package devices_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
	"github.com/oldsj/mainloop/server/internal/devices"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

func pocketID(iat time.Time) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "bearer", RegistrationOnly: true, IssuedAt: iat,
	})
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
	res, err := a.RegisterDevice(pocketID(time.Now()), registration(d))
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
	if res, _ := a.RegisterDevice(pocketID(time.Now()), registration(phone)); !isType[api.RegisterDevice409JSONResponse](res) {
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
	if cur, _ := a.GetCurrentDevice(pocketID(time.Now()), api.GetCurrentDeviceRequestObject{}); !isType[api.GetCurrentDevice404JSONResponse](cur) {
		t.Fatalf("current without a device = %#v", cur)
	}

	// The verifier reads it from the store, then not after revocation.
	store := &devices.Store{Pool: db.Pool, OwnerID: owner}
	verify := &auth.Devices{Store: store, MaxBody: 1 << 20}
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
	for name, ctx := range map[string]context.Context{
		"no principal (auth off)": context.Background(),
		"stale sign-in":           pocketID(time.Now().Add(-11 * time.Minute)),
		"no iat":                  pocketID(time.Time{}),
		"a device":                asDevice("dev_aaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"the login cookie": auth.WithPrincipal(context.Background(), auth.Principal{
			Kind: auth.KindOwner, Name: "owner-sub", Scope: auth.ScopeOwner, Via: "cookie", IssuedAt: time.Now(),
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

func TestRegistrationValidatesTheKey(t *testing.T) {
	db := testdb.New(t)
	a := &devices.DevicesAPI{Pool: db.Pool, OwnerID: owner}
	phone := authtest.NewDevice(t, "dev_placeholder00000000000000")
	bad := registration(phone)
	bad.Body.RequestKey.Y = bad.Body.RequestKey.X // almost certainly not on the curve
	if _, err := a.RegisterDevice(pocketID(time.Now()), bad); !isStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("off-curve key: %v", err)
	}
	blank := registration(phone)
	blank.Body.Name = "   "
	if _, err := a.RegisterDevice(pocketID(time.Now()), blank); !isStatus(err, http.StatusUnprocessableEntity) {
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
	} {
		if _, err := devices.PublicKey(xy[0], xy[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
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
