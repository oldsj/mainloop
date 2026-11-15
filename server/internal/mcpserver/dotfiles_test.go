package mcpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/dotfiles/dotfilestest"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

func (h *harness) getBundle(digest, token string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, dotfiles.BundlePath+digest, nil)
	req.Host = host
	for k, v := range header {
		req.Header[k] = v
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w
}

// A session downloads the bundle it was pinned to with its MCP bearer, and nothing else: another
// session's bundle is not found, as is a digest no one has, and a request without a live bearer is
// refused.
func TestASessionDownloadsOnlyItsPinnedBundle(t *testing.T) {
	var pool *pgxpool.Pool
	h := buildHarness(t, false, false, func(_ *mcpserver.Server, p *pgxpool.Pool) { pool = p })
	ctx := context.Background()
	pinned := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	other := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	ended := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace, Status: dbstate.SessionCompleted})
	pins := &dotfilestest.Pinner{Pool: pool, Ready: true}
	for _, s := range []authtest.Seeded{pinned, ended} {
		if _, err := pins.Pin(ctx, s.SessionID, "req-"+s.SessionID, "http://"+host); err != nil {
			t.Fatal(err)
		}
	}
	// A bundle another owner's session was pinned to, which this session never was.
	otherDigest := "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	q := dbq.New(pool)
	if err := q.InsertDotfilesBundle(ctx, dbq.InsertDotfilesBundleParams{Digest: otherDigest, Content: []byte("not yours")}); err != nil {
		t.Fatal(err)
	}

	w := h.getBundle(dotfilestest.Digest(), pinned.Token, nil)
	if w.Code != http.StatusOK || w.Body.String() != string(dotfilestest.Bundle) ||
		w.Header().Get("Content-Type") != "application/x-tar" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("pinned bundle = %d %q %v", w.Code, w.Body, w.Header())
	}

	for name, tt := range map[string]struct {
		digest, token string
		header        http.Header
		code          int
	}{
		"another session's bundle":  {dotfilestest.Digest(), other.Token, nil, http.StatusNotFound},
		"a bundle never pinned":     {otherDigest, pinned.Token, nil, http.StatusNotFound},
		"a malformed digest":        {"sha256:ABC", pinned.Token, nil, http.StatusNotFound},
		"no bearer":                 {dotfilestest.Digest(), "", nil, http.StatusUnauthorized},
		"a bearer that is not live": {dotfilestest.Digest(), "not-a-token", nil, http.StatusUnauthorized},
		"an ended session":          {dotfilestest.Digest(), ended.Token, nil, http.StatusUnauthorized},
		"a browser":                 {dotfilestest.Digest(), pinned.Token, http.Header{"Origin": {"https://example.com"}}, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			w := h.getBundle(tt.digest, tt.token, tt.header)
			if w.Code != tt.code {
				t.Fatalf("status = %d, want %d", w.Code, tt.code)
			}
			if w.Body.String() == string(dotfilestest.Bundle) || w.Body.String() == "not yours" {
				t.Fatal("the refusal carried the bundle")
			}
		})
	}
}
