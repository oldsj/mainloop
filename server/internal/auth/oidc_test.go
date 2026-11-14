package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
)

func newOIDC(iss *authtest.Issuer) *auth.OIDC {
	return &auth.OIDC{Issuer: iss.URL(), Audience: "mainloop", Client: iss.Client()}
}

func TestOIDCVerifiesAnOwnerToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	o := newOIDC(iss)
	tok := iss.Sign(t, authtest.Claims{"sub": "owner-sub", "aud": "mainloop", "groups": []string{"admins"}})
	c, err := o.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "owner-sub" || len(c.Groups) != 1 || c.Groups[0] != "admins" {
		t.Fatalf("claims = %+v", c)
	}
}

func TestOIDCRefuses(t *testing.T) {
	iss := authtest.NewIssuer(t)
	other := authtest.NewIssuer(t)
	hour := time.Now().Add(time.Hour).Unix()
	cases := map[string]string{
		"wrong audience":       iss.Sign(t, authtest.Claims{"sub": "s", "aud": "house"}),
		"wrong issuer":         iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "iss": "https://elsewhere"}),
		"expired":              iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "exp": time.Now().Add(-2 * time.Minute).Unix()}),
		"no expiry":            iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "exp": nil}),
		"not yet valid":        iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "nbf": time.Now().Add(5 * time.Minute).Unix()}),
		"no subject":           iss.Sign(t, authtest.Claims{"aud": "mainloop"}),
		"another issuer's key": other.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "iss": iss.URL()}),
		"garbage":              "not.a.token",
	}
	// An HMAC token keyed with anything public must never pass as an RSA one (algorithm confusion).
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": iss.URL(), "sub": "s", "aud": "mainloop", "exp": hour})
	hs.Header["kid"] = "x"
	cases["HMAC algorithm"], _ = hs.SignedString([]byte("secret"))
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": iss.URL(), "sub": "s", "aud": "mainloop", "exp": hour})
	cases["none algorithm"], _ = none.SignedString(jwt.UnsafeAllowNoneSignatureType)

	o := newOIDC(iss)
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if c, err := o.Verify(context.Background(), tok); err == nil {
				t.Fatalf("accepted: %+v", c)
			}
		})
	}
}

func TestOIDCFollowsKeyRotationAndSurvivesAnOutage(t *testing.T) {
	iss := authtest.NewIssuer(t)
	now := time.Now()
	o := newOIDC(iss)
	o.Now = func() time.Time { return now }
	first := iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "exp": now.Add(24 * time.Hour).Unix()})
	if _, err := o.Verify(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	// The issuer goes down well after the keys went stale: the held keys still verify.
	iss.SetDown(true)
	now = now.Add(2 * time.Hour)
	if _, err := o.Verify(context.Background(), first); err != nil {
		t.Fatalf("held key during outage: %v", err)
	}

	// Back up with a new key: an unknown kid fetches again, but no more than once per retry period.
	iss.SetDown(false)
	iss.Rotate(t)
	second := iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop", "exp": now.Add(24 * time.Hour).Unix()})
	if _, err := o.Verify(context.Background(), second); err == nil {
		t.Fatal("refetched within the retry period")
	}
	now = now.Add(time.Minute)
	before := iss.KeyFetches.Load()
	if _, err := o.Verify(context.Background(), second); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if got := iss.KeyFetches.Load() - before; got != 1 {
		t.Fatalf("key fetches = %d, want 1", got)
	}
}

func TestOIDCUnreachableIssuerFailsClosed(t *testing.T) {
	iss := authtest.NewIssuer(t)
	iss.SetDown(true)
	tok := iss.Sign(t, authtest.Claims{"sub": "s", "aud": "mainloop"})
	_, err := newOIDC(iss).Verify(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "signing keys unavailable") {
		t.Fatalf("err = %v", err)
	}
}
