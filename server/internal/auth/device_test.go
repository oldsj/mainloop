package auth_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
)

const (
	phoneID = "dev_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherID = "dev_bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// body is what fixture.do sends with every request.
var body = []byte("{}")

var native = map[string]string{auth.ClientHeader: "ios"}

func TestDeviceTokens(t *testing.T) {
	f := newFixture(t, auth.ModeEnforce)
	phone := authtest.NewDevice(t, phoneID)
	f.devices.Add(phone)
	stranger := authtest.NewDevice(t, otherID) // never registered
	revoked := authtest.NewDevice(t, "dev_cccccccccccccccccccccccccc")
	f.devices.Add(revoked)
	f.devices.Revoke(revoked.ID)
	tok := func(method, target string, mutate func(jwt.MapClaims, map[string]any)) string {
		return phone.Token(t, method, target, body, mutate)
	}
	now := time.Now()

	cases := []struct {
		name string
		req  req
		want int // 0: passed as the device
	}{
		{"device reads", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", nil)}, 0},
		{"device reads with a query", req{method: "GET", path: "/sessions?limit=5", bearer: tok("GET", "/sessions?limit=5", nil)}, 0},
		{"device writes with the client header", req{method: "POST", path: "/sessions", bearer: tok("POST", "/sessions", nil), headers: native}, 0},
		{"device write without the client header", req{method: "POST", path: "/sessions", bearer: tok("POST", "/sessions", nil)}, 403},
		{"device write from a web origin", req{method: "POST", path: "/sessions", bearer: tok("POST", "/sessions", nil), headers: map[string]string{auth.ClientHeader: "ios", "Origin": "https://evil.test"}}, 403},
		{"unregistered device", req{method: "GET", path: "/attention", bearer: stranger.Token(t, "GET", "/attention", body, nil)}, 401},
		{"revoked device", req{method: "GET", path: "/attention", bearer: revoked.Token(t, "GET", "/attention", body, nil)}, 401},
		{"signed for another method", req{method: "DELETE", path: "/devices/" + phoneID, bearer: tok("GET", "/devices/"+phoneID, nil), headers: native}, 401},
		{"signed for another path", req{method: "GET", path: "/sessions", bearer: tok("GET", "/attention", nil)}, 401},
		{"signed for another query", req{method: "GET", path: "/sessions?limit=500", bearer: tok("GET", "/sessions?limit=5", nil)}, 401},
		{"signed for another body", req{method: "POST", path: "/sessions", bearer: tok("POST", "/sessions", func(c jwt.MapClaims, _ map[string]any) { c["bh"] = authtest.BodyHash([]byte(`{"x":1}`)) }), headers: native}, 401},
		{"wrong audience", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) { c["aud"] = "mainloop" })}, 401},
		{"signed for another instance", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) { c["aud"] = "https://other.test" })}, 401},
		{"sub is not kid", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) { c["sub"] = otherID })}, 401},
		{"kid names another device", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, h map[string]any) { h["kid"] = revoked.ID; c["sub"] = revoked.ID })}, 401},
		{"no typ", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(_ jwt.MapClaims, h map[string]any) { delete(h, "typ") })}, 401},
		{"lives over a minute", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) { c["exp"] = now.Add(10 * time.Minute).Unix() })}, 401},
		{"expired", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) {
			c["iat"], c["exp"] = now.Add(-3*time.Minute).Unix(), now.Add(-2*time.Minute).Unix()
		})}, 401},
		{"issued in the future", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) {
			c["iat"], c["exp"] = now.Add(5*time.Minute).Unix(), now.Add(6*time.Minute).Unix()
		})}, 401},
		{"no expiry", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) { delete(c, "exp") })}, 401},
		{"short jti", req{method: "GET", path: "/attention", bearer: tok("GET", "/attention", func(c jwt.MapClaims, _ map[string]any) { c["jti"] = "1" })}, 401},
		{"a device may not register a device", req{method: "POST", path: "/devices", bearer: tok("POST", "/devices", nil), headers: native}, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := f.do(c.req)
			if c.want == 0 {
				if !f.called || f.seen == nil || f.seen.Kind != auth.KindDevice || f.seen.Name != phoneID || f.seen.Scope != auth.ScopeOwner {
					t.Fatalf("status %d, principal %+v; logs:\n%s", w.Code, f.seen, f.logs)
				}
				return
			}
			if f.called || w.Code != c.want {
				t.Fatalf("status = %d (passed=%v), want %d; logs:\n%s", w.Code, f.called, c.want, f.logs)
			}
		})
	}
	if f.devices.Touched[phoneID] != 1 {
		t.Fatalf("last seen recorded %d times, want once a minute", f.devices.Touched[phoneID])
	}
}

func TestDeviceTokenIsGoodForOneRequest(t *testing.T) {
	f := newFixture(t, auth.ModeEnforce)
	phone := authtest.NewDevice(t, phoneID)
	f.devices.Add(phone)
	tok := phone.Token(t, "GET", "/attention", body, nil)
	if w := f.do(req{method: "GET", path: "/attention", bearer: tok}); !f.called {
		t.Fatalf("first use refused: %d", w.Code)
	}
	if w := f.do(req{method: "GET", path: "/attention", bearer: tok}); f.called || w.Code != 401 {
		t.Fatalf("replay: status %d, passed=%v", w.Code, f.called)
	}
}

func TestRevocationTakesEffectOnTheNextRequest(t *testing.T) {
	f := newFixture(t, auth.ModeEnforce)
	phone := authtest.NewDevice(t, phoneID)
	f.devices.Add(phone)
	if f.do(req{method: "GET", path: "/attention", bearer: phone.Token(t, "GET", "/attention", body, nil)}); !f.called {
		t.Fatal("refused before revocation")
	}
	f.devices.Revoke(phoneID)
	if w := f.do(req{method: "GET", path: "/attention", bearer: phone.Token(t, "GET", "/attention", body, nil)}); f.called || w.Code != 401 {
		t.Fatalf("after revocation: status %d, passed=%v", w.Code, f.called)
	}
}

func TestTheHandlerStillReadsTheBody(t *testing.T) {
	store := &authtest.DeviceStore{}
	phone := authtest.NewDevice(t, phoneID)
	store.Add(phone)
	d := &auth.Devices{Store: store, MaxBody: 64, Audience: authtest.DeviceOrigin}
	payload := []byte(`{"message":"hello"}`)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://mainloop.test/sessions/s1/message", strings.NewReader(string(payload)))
	if _, err := d.Verify(r, phone.Token(t, "POST", "/sessions/s1/message", payload, nil)); err != nil {
		t.Fatal(err)
	}
	got := new(bytes.Buffer)
	if _, err := got.ReadFrom(r.Body); err != nil || got.String() != string(payload) {
		t.Fatalf("body after verify = %q, %v", got, err)
	}

	big := strings.Repeat("x", 100)
	r = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://mainloop.test/sessions/s1/message", strings.NewReader(big))
	if _, err := d.Verify(r, phone.Token(t, "POST", "/sessions/s1/message", []byte(big), nil)); err == nil {
		t.Fatal("a body over the cap was verified")
	}
	got.Reset()
	if _, err := got.ReadFrom(r.Body); err != nil || got.String() != big {
		t.Fatalf("an over-cap body was not passed on intact (observe mode): %d bytes, %v", got.Len(), err)
	}
}

func TestRegistration(t *testing.T) {
	f := newFixture(t, auth.ModeEnforce)
	const nonce = "the-new-keys-thumbprint"
	app := func(authTime time.Time, extra authtest.Claims) string {
		c := authtest.Claims{"aud": "mainloop-ios", "auth_time": authTime.Unix(), "nonce": nonce, "jti": "sign-in-1"}
		for k, v := range extra {
			c[k] = v
		}
		return f.owner(t, c)
	}
	write := func(bearer, cookie string) req {
		return req{method: "POST", path: "/devices", bearer: bearer, cookie: cookie, headers: native}
	}

	if f.do(write(app(time.Now(), nil), "")); !f.called || !f.seen.RegistrationOnly || !f.seen.DeviceClient ||
		f.seen.Nonce != nonce || f.seen.TokenID != "sign-in-1" || f.seen.AuthTime.IsZero() {
		t.Fatalf("the app's fresh sign-in may register: passed=%v principal=%+v; logs:\n%s", f.called, f.seen, f.logs)
	}
	// A refresh renews iat but not auth_time: an old sign-in stays old.
	refreshed := app(time.Now().Add(-20*time.Minute), authtest.Claims{"iat": time.Now().Unix()})
	web := f.owner(t, authtest.Claims{"auth_time": time.Now().Unix(), "nonce": nonce})
	for name, r := range map[string]req{
		"stale sign-in, fresh token":  write(refreshed, ""),
		"no auth_time":                write(app(time.Now(), authtest.Claims{"auth_time": nil}), ""),
		"auth_time in the future":     write(app(time.Now().Add(10*time.Minute), nil), ""),
		"no nonce":                    write(app(time.Now(), authtest.Claims{"nonce": ""}), ""),
		"the web app's bearer":        write(web, ""),
		"login cookie":                write("", web),
		"operator ServiceAccount":     write(f.sa(t, "mainloop/mainloop-operator"), ""),
		"app token on another route":  {method: "GET", path: "/sessions", bearer: app(time.Now(), nil)},
		"app token writing elsewhere": {method: "POST", path: "/sessions", bearer: app(time.Now(), nil), headers: native},
	} {
		if w := f.do(r); f.called || w.Code != 403 {
			t.Errorf("%s: status %d, passed=%v", name, w.Code, f.called)
		}
	}
}

func TestRevocationEndsOpenRequests(t *testing.T) {
	store := &authtest.DeviceStore{}
	phone := authtest.NewDevice(t, phoneID)
	store.Add(phone)
	d := &auth.Devices{Store: store, MaxBody: 1 << 20, Audience: authtest.DeviceOrigin, RecheckEvery: 10 * time.Millisecond}

	// Revoked here: Revoked ends it at once.
	ctx, stop := d.Watch(context.Background(), phoneID)
	defer stop()
	d.Revoked(phoneID)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a request stayed open after its device was revoked here")
	}

	// Revoked by another process: the recheck ends it.
	other := authtest.NewDevice(t, otherID)
	store.Add(other)
	ctx2, stop2 := d.Watch(context.Background(), otherID)
	defer stop2()
	store.Revoke(otherID)
	select {
	case <-ctx2.Done():
	case <-time.After(time.Second):
		t.Fatal("a request stayed open after its device was revoked elsewhere")
	}
}

func TestAWriteTokenIsGoodOnceAcrossProcesses(t *testing.T) {
	store := &authtest.DeviceStore{}
	phone := authtest.NewDevice(t, phoneID)
	store.Add(phone)
	// Two verifiers share the store, as two replicas share the database.
	a := &auth.Devices{Store: store, MaxBody: 1 << 20, Audience: authtest.DeviceOrigin}
	b := &auth.Devices{Store: store, MaxBody: 1 << 20, Audience: authtest.DeviceOrigin}
	tok := phone.Token(t, "POST", "/sessions", body, nil)
	post := func() *http.Request {
		return httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://mainloop.test/sessions", bytes.NewReader(body))
	}
	if _, err := a.Verify(post(), tok); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(post(), tok); err == nil {
		t.Fatal("another replica accepted a write token already used")
	}
}

func TestTheAppsAudienceNeedsToBeConfigured(t *testing.T) {
	f := newFixture(t, auth.ModeEnforce)
	tok := f.owner(t, authtest.Claims{"aud": "mainloop-ios"})
	o := &auth.OIDC{Issuer: f.iss.URL(), Audience: "mainloop", Client: f.iss.Client()}
	if _, err := o.Verify(context.Background(), tok); err == nil {
		t.Fatal("a token for the app's client verified without MAINLOOP_AUTH_DEVICE_CLIENT_ID")
	}
}
