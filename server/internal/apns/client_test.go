package apns_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/apns"
)

const (
	keyID  = "ABC123DEFG"
	teamID = "TEAM123456"
	topic  = "org.example.mainloop"
	token  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// fakeAPNs is Apple's push endpoint over HTTP/2 and TLS. It answers each request with the next
// queued answer, or 200.
type fakeAPNs struct {
	mu      sync.Mutex
	reqs    []seen
	answers []answer
}

type seen struct {
	proto   int
	path    string
	header  http.Header
	payload map[string]any
}

type answer struct {
	status int
	reason string
}

func (f *fakeAPNs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p map[string]any
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &p)
	f.reqs = append(f.reqs, seen{proto: r.ProtoMajor, path: r.URL.Path, header: r.Header.Clone(), payload: p})
	if len(f.answers) == 0 {
		w.Header().Set("apns-id", r.Header.Get("apns-id"))
		return
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	w.WriteHeader(a.status)
	_ = json.NewEncoder(w).Encode(map[string]string{"reason": a.reason})
}

func (f *fakeAPNs) queue(a ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, a...)
}

func (f *fakeAPNs) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func p8(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// fakeClient is a Client against a fake APNs, with a clock the test moves.
func fakeClient(t *testing.T) (*apns.Client, *fakeAPNs, *ecdsa.PrivateKey, *time.Time) {
	t.Helper()
	f := &fakeAPNs{}
	srv := httptest.NewUnstartedServer(f)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	key := newKey(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	c := &apns.Client{
		Creds: &apns.Credentials{KeyID: keyID, TeamID: teamID, Key: key},
		Topic: topic, HTTP: srv.Client(),
		Hosts: map[string]string{"production": srv.URL, "development": srv.URL},
		Now:   func() time.Time { return now },
	}
	return c, f, key, &now
}

func request() apns.Request {
	return apns.Request{
		Environment: "production", DeviceToken: token, ID: "6f1c0a52-3c3b-4c43-9a4e-2f4f0f7f9d11",
		CollapseID: "push_1", Expiration: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), Priority: 10,
		Payload: []byte(`{"aps":{"alert":{"title":"t","body":"b"}}}`),
	}
}

func TestSendSpeaksAPNs(t *testing.T) {
	c, f, key, now := fakeClient(t)
	if _, err := c.Send(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.proto != 2 || r.path != "/3/device/"+token {
		t.Fatalf("HTTP/%d %s", r.proto, r.path)
	}
	for h, want := range map[string]string{
		"apns-topic": topic, "apns-push-type": "alert", "apns-priority": "10", "apns-expiration": strconv.FormatInt(request().Expiration.Unix(), 10),
		"apns-id": "6f1c0a52-3c3b-4c43-9a4e-2f4f0f7f9d11", "apns-collapse-id": "push_1", "content-type": "application/json",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if r.payload["aps"] == nil {
		t.Errorf("payload = %v", r.payload)
	}

	// The provider token is ES256, names the key and the team, and is dated now.
	bearer, ok := strings.CutPrefix(r.header.Get("authorization"), "bearer ")
	if !ok {
		t.Fatalf("authorization = %q", r.header.Get("authorization"))
	}
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(bearer, claims, func(tk *jwt.Token) (any, error) {
		if tk.Method != jwt.SigningMethodES256 {
			return nil, errors.New("not ES256")
		}
		return &key.PublicKey, nil
	}, jwt.WithoutClaimsValidation())
	if err != nil || !tok.Valid {
		t.Fatalf("provider token: %v", err)
	}
	if tok.Header["kid"] != keyID || claims["iss"] != teamID || int64(claims["iat"].(float64)) != now.Unix() || len(claims) != 2 {
		t.Fatalf("provider token header %v claims %v", tok.Header, claims)
	}
}

func TestProviderTokenIsReusedThenReplaced(t *testing.T) {
	c, f, _, now := fakeClient(t)
	send := func() string {
		t.Helper()
		_, _ = c.Send(context.Background(), request())
		reqs := f.requests()
		return reqs[len(reqs)-1].header.Get("authorization")
	}
	first := send()
	*now = now.Add(39 * time.Minute)
	if send() != first {
		t.Fatal("replaced before 40 minutes")
	}
	*now = now.Add(time.Minute)
	second := send()
	if second == first {
		t.Fatal("not replaced at 40 minutes")
	}

	// APNs calls it expired: too young to replace (Apple refuses updates more often than every
	// 20 minutes), so it stays; once old enough, the next send signs a new one.
	*now = now.Add(10 * time.Minute)
	f.queue(answer{http.StatusForbidden, "ExpiredProviderToken"})
	if _, err := c.Send(context.Background(), request()); err == nil || errors.Is(err, apns.ErrRejected) || errors.Is(err, apns.ErrBadToken) {
		t.Fatalf("expired provider token = %v, want a retryable error", err)
	}
	if send() != second {
		t.Fatal("replaced a token younger than 20 minutes")
	}
	*now = now.Add(15 * time.Minute)
	f.queue(answer{http.StatusForbidden, "ExpiredProviderToken"})
	_, _ = c.Send(context.Background(), request())
	if send() == second {
		t.Fatal("kept an expired token older than 20 minutes")
	}
}

func TestRefusalsAreClassified(t *testing.T) {
	tests := []struct {
		answer answer
		want   error // nil: retryable
	}{
		{answer{http.StatusGone, "Unregistered"}, apns.ErrBadToken},
		{answer{http.StatusBadRequest, "BadDeviceToken"}, apns.ErrBadToken},
		{answer{http.StatusBadRequest, "DeviceTokenNotForTopic"}, apns.ErrBadToken},
		{answer{http.StatusBadRequest, "PayloadTooLarge"}, apns.ErrRejected},
		{answer{http.StatusBadRequest, "TopicDisallowed"}, apns.ErrRejected},
		{answer{http.StatusForbidden, "InvalidProviderToken"}, nil},
		{answer{http.StatusTooManyRequests, "TooManyRequests"}, nil},
		{answer{http.StatusServiceUnavailable, "ServiceUnavailable"}, nil},
		{answer{http.StatusInternalServerError, "InternalServerError"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.answer.reason, func(t *testing.T) {
			c, f, _, _ := fakeClient(t)
			f.queue(tt.answer)
			res, err := c.Send(context.Background(), request())
			if err == nil || res.Status != tt.answer.status || res.Reason != tt.answer.reason {
				t.Fatalf("Send = %+v, %v", res, err)
			}
			if tt.want == nil && (errors.Is(err, apns.ErrBadToken) || errors.Is(err, apns.ErrRejected)) {
				t.Fatalf("%v is final, want retryable", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("%v, want %v", err, tt.want)
			}
		})
	}
}

func TestEnvironmentPicksTheHost(t *testing.T) {
	c := &apns.Client{}
	if _, err := c.Send(context.Background(), apns.Request{Environment: "staging"}); !errors.Is(err, apns.ErrRejected) {
		t.Fatalf("unknown environment = %v", err)
	}
	if apns.ProductionHost != "https://api.push.apple.com" || apns.DevelopmentHost != "https://api.sandbox.push.apple.com" {
		t.Fatal("Apple's hosts changed")
	}
}

func TestLoadCredentials(t *testing.T) {
	key := newKey(t)
	write := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	good := map[string]string{"key.p8": string(p8(t, key)), "key-id": keyID + "\n", "team-id": " " + teamID}

	c, err := apns.LoadCredentials(write(t, good))
	if err != nil || c.KeyID != keyID || c.TeamID != teamID || !c.Key.Equal(key) {
		t.Fatalf("good = %+v, %v", c, err)
	}
	if _, err := apns.LoadCredentials(write(t, nil)); !errors.Is(err, apns.ErrNoCredentials) {
		t.Fatalf("empty dir = %v", err)
	}
	if _, err := apns.LoadCredentials(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, apns.ErrNoCredentials) {
		t.Fatalf("absent dir = %v", err)
	}

	rsa := "-----BEGIN PRIVATE KEY-----\nMIIBVAIBADANBgkqhkiG9w0BAQEFAASCAT4wggE6AgEAAkEA\n-----END PRIVATE KEY-----\n"
	bad := map[string]map[string]string{
		"partial":       {"key.p8": good["key.p8"], "key-id": keyID},
		"short key id":  {"key.p8": good["key.p8"], "key-id": "ABC", "team-id": teamID},
		"lower team id": {"key.p8": good["key.p8"], "key-id": keyID, "team-id": "team123456"},
		"not pem":       {"key.p8": "secret-looking-bytes", "key-id": keyID, "team-id": teamID},
		"not p-256":     {"key.p8": rsa, "key-id": keyID, "team-id": teamID},
	}
	for name, files := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := apns.LoadCredentials(write(t, files))
			if err == nil || errors.Is(err, apns.ErrNoCredentials) {
				t.Fatalf("= %v, want a configuration error", err)
			}
			if strings.Contains(err.Error(), "secret-looking") || strings.Contains(err.Error(), "MII") {
				t.Fatalf("error quotes the key: %v", err)
			}
		})
	}
}

// A transport error names neither the URL nor the device token in it.
func TestTransportErrorsDoNotCarryTheToken(t *testing.T) {
	c, _, _, _ := fakeClient(t)
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS()
	srv.Close() // nothing listens there now
	c.Hosts = map[string]string{"production": srv.URL}
	_, err := c.Send(context.Background(), request())
	if err == nil || errors.Is(err, apns.ErrRejected) || errors.Is(err, apns.ErrBadToken) {
		t.Fatalf("Send = %v, want a retryable transport error", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error carries the URL: %v", err)
	}
}
