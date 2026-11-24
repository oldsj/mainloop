package auth_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
	"github.com/oldsj/mainloop/server/internal/devices"
)

// TestATokenTheIPhoneAppSignedVerifies checks a token signed by the app's own code
// (ios/Packages/MainloopKit, DeviceToken; regenerate with the command in its InteropFixture test).
func TestATokenTheIPhoneAppSignedVerifies(t *testing.T) {
	raw, err := os.ReadFile("testdata/swift-device-token.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		DeviceID   string `json:"device_id"`
		X, Y       string
		Thumbprint string
		Audience   string
		Method     string
		Target     string
		Body       string
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
	// The app's sign-in nonce is its key's thumbprint; the server must compute the same one.
	if got := devices.Thumbprint(f.X, f.Y); got != f.Thumbprint {
		t.Fatalf("thumbprint: server %q, app %q", got, f.Thumbprint)
	}
	if f.Audience != authtest.DeviceOrigin {
		t.Fatalf("fixture signed for %q, want %q", f.Audience, authtest.DeviceOrigin)
	}
	verify := func(method, target, body string) (string, error) {
		store := &authtest.DeviceStore{}
		store.AddKey(f.DeviceID, key)
		d := &auth.Devices{Store: store, MaxBody: 1 << 20, Audience: authtest.DeviceOrigin, Now: func() time.Time { return time.Unix(f.IAT+5, 0) }}
		r := httptest.NewRequestWithContext(context.Background(), method, "https://mainloop.test"+target, strings.NewReader(body))
		return d.Verify(r, f.Token)
	}

	id, err := verify(f.Method, f.Target, f.Body)
	if err != nil || id != f.DeviceID {
		t.Fatalf("Verify = %q, %v; want %q", id, err, f.DeviceID)
	}
	for name, req := range map[string][3]string{
		"another body":   {f.Method, f.Target, `{"response":"Deny"}`},
		"another path":   {f.Method, "/queue/q-0002/respond?wait=1", f.Body},
		"another method": {"PUT", f.Target, f.Body},
	} {
		if _, err := verify(req[0], req[1], req[2]); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}
