package pushgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve-git builds the gate without a credential service: it never enrolls, and the path that needs
// the service must refuse instead of reaching a nil.
func TestEnrollWithoutCredentialsOrUpstreamIsUnavailable(t *testing.T) {
	type upstream struct{ Upstream }
	for name, svc := range map[string]*Service{
		"no credentials": NewService(nil, nil, upstream{}, nil),
		"no upstream":    NewService(nil, nil, nil, nil),
	} {
		if _, err := svc.Enroll(context.Background(), "ws"); !errors.Is(err, ErrEnrollUnavailable) {
			t.Errorf("%s: Enroll = %v, want ErrEnrollUnavailable", name, err)
		}
	}
}

// The push listener deliberately has no access log: it never logs a path or adopts a client's id.
// Its one log line per refusal carries the id it generated, and the response returns the same one.
func TestRefusalLogCarriesTheResponseRequestID(t *testing.T) {
	var logs bytes.Buffer
	srv := &Server{Log: slog.New(slog.NewJSONHandler(&logs, nil)), Dev: true}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", nil)
	r.Header.Set("X-Request-Id", "client-chosen")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &rec); err != nil {
		t.Fatalf("log %q: %v", logs.String(), err)
	}
	id, _ := rec["request_id"].(string)
	if id == "" || id == "client-chosen" || id != w.Header().Get("x-mainloop-request-id") {
		t.Fatalf("log request_id = %q, response header = %q", id, w.Header().Get("x-mainloop-request-id"))
	}
}
