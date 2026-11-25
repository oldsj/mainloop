package approval_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/approval"
	"github.com/oldsj/mainloop/server/internal/devices"
)

const (
	device   = "dev_0123456789abcdefghijklmnop"
	origin   = "https://mainloop.test"
	actionID = "pa_fixture0001"
	digest   = "f16d05ec6b29248d2c61adb1e9263f78e4f7bace1b955014a2d17872cfe4064d"
)

// TestATokenTheIPhoneAppSignedVerifies checks an approval token signed by the app's own code
// (ios/Packages/MainloopKit, ApprovalToken; regenerate with the command in its InteropFixture
// test, writeTheGoApprovalFixture).
func TestATokenTheIPhoneAppSignedVerifies(t *testing.T) {
	raw, err := os.ReadFile("testdata/swift-approval-token.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		DeviceID   string `json:"device_id"`
		X, Y       string
		Thumbprint string
		Audience   string
		ActionID   string `json:"action_id"`
		Digest     string
		IAT        int64
		Token      string
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	key, err := devices.PublicKey(f.X, f.Y)
	if err != nil {
		t.Fatal(err)
	}
	if got := devices.Thumbprint(f.X, f.Y); got != f.Thumbprint {
		t.Fatalf("thumbprint: server %q, app %q", got, f.Thumbprint)
	}
	pinned := approval.Pinned{f.DeviceID: key}
	c, err := approval.Parse(f.Token, pinned.Key, origin)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID != f.DeviceID || c.ActionID != f.ActionID || c.Digest != f.Digest || c.IssuedAt.Unix() != f.IAT {
		t.Fatalf("claims = %+v", c)
	}
	if err := c.Current(time.Unix(f.IAT+5, 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.Current(time.Unix(f.IAT+600, 0)); err == nil {
		t.Fatal("an expired token counted as current")
	}
	if _, err := approval.Parse(f.Token, pinned.Key, "https://other.test"); err == nil {
		t.Fatal("a token for another instance verified")
	}
}

func coords(k *ecdsa.PublicKey) (string, string) {
	b, _ := k.Bytes()
	return base64.RawURLEncoding.EncodeToString(b[1:33]), base64.RawURLEncoding.EncodeToString(b[33:])
}

// Given a token's form, Parse accepts only an ES256 approval token signed by the key for its kid,
// with sub equal to kid, a lifetime of at most 5 minutes, a jti, an action id and a hex digest.
func TestParseChecksTheTokensForm(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keys := approval.Pinned{device: &key.PublicKey}
	iat := time.Unix(1_790_000_000, 0)
	good := func() (jwt.MapClaims, map[string]any) {
		return jwt.MapClaims{
				"iss": "mainloop-device", "sub": device, "aud": origin, "iat": iat.Unix(), "exp": iat.Add(2 * time.Minute).Unix(),
				"jti": "a-jti-of-16-chars", "act": actionID, "dig": digest,
			}, map[string]any{
				"typ": approval.TokenType, "kid": device,
			}
	}
	sign := func(c jwt.MapClaims, h map[string]any) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, c)
		for k, v := range h {
			tok.Header[k] = v
		}
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := approval.Parse(sign(good()), keys.Key, origin); err != nil {
		t.Fatalf("a good token: %v", err)
	}
	bad := map[string]func(c jwt.MapClaims, h map[string]any){
		"a device request token": func(_ jwt.MapClaims, h map[string]any) { h["typ"] = "mainloop-device+jwt" },
		"an unpinned device":     func(c jwt.MapClaims, h map[string]any) { h["kid"] = "dev_zzzzzzzzzzzzzzzzzzzzzzzzzz"; c["sub"] = h["kid"] },
		"sub is not kid":         func(c jwt.MapClaims, _ map[string]any) { c["sub"] = "dev_zzzzzzzzzzzzzzzzzzzzzzzzzz" },
		"another issuer":         func(c jwt.MapClaims, _ map[string]any) { c["iss"] = "someone" },
		"another instance":       func(c jwt.MapClaims, _ map[string]any) { c["aud"] = "https://other.test" },
		"a long life":            func(c jwt.MapClaims, _ map[string]any) { c["exp"] = iat.Add(6 * time.Minute).Unix() },
		"no exp":                 func(c jwt.MapClaims, _ map[string]any) { delete(c, "exp") },
		"a short jti":            func(c jwt.MapClaims, _ map[string]any) { c["jti"] = "short" },
		"no action":              func(c jwt.MapClaims, _ map[string]any) { delete(c, "act") },
		"an upper-case digest":   func(c jwt.MapClaims, _ map[string]any) { c["dig"] = strings.ToUpper(digest) },
	}
	for name, mutate := range bad {
		c, h := good()
		mutate(c, h)
		if _, err := approval.Parse(sign(c, h), keys.Key, origin); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := approval.Parse(sign(good()), approval.Pinned{device: &other.PublicKey}.Key, origin); err == nil {
		t.Error("a token signed by another key verified")
	}
}

// Given MAINLOOP_PRIVILEGED_APPROVAL_KEYS, ParsePinned reads P-256 JWKs by device id and refuses
// anything else.
func TestParsePinned(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	x, y := coords(&key.PublicKey)
	entry := func(kid, kty, x string) string {
		return `{"kid":"` + kid + `","kty":"` + kty + `","crv":"P-256","x":"` + x + `","y":"` + y + `"}`
	}
	p, err := approval.ParsePinned("[" + entry(device, "EC", x) + "]")
	if err != nil || !p.Has(device, &key.PublicKey) {
		t.Fatalf("ParsePinned = %v, %v", p, err)
	}
	for _, raw := range []string{"", "[]"} {
		if p, err := approval.ParsePinned(raw); err != nil || p != nil {
			t.Errorf("%q = %v, %v; want nothing pinned", raw, p, err)
		}
	}
	for name, raw := range map[string]string{
		"not JSON":       "dev_x",
		"a bad kid":      "[" + entry("phone", "EC", x) + "]",
		"an RSA key":     "[" + entry(device, "RSA", x) + "]",
		"a bad point":    "[" + entry(device, "EC", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") + "]",
		"pinned twice":   "[" + entry(device, "EC", x) + "," + entry(device, "EC", x) + "]",
		"an extra field": `[{"kid":"` + device + `","kty":"EC","crv":"P-256","x":"` + x + `","y":"` + y + `","d":"secret"}]`,
	} {
		if _, err := approval.ParsePinned(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
