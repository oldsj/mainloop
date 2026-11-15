package mcpserver

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

var bundleDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// dotfilesBundle serves an agent dotfiles bundle by digest (docs/specs/agent-dotfiles.md). The
// runtime fetches it through kagent's egress gateway, which adds the session's MCP credential: a
// session gets only a bundle one of its native Sessions, or its fork source, was pinned to. Any
// other digest is not found, whether or not it exists.
func (s *Server) dotfilesBundle(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	digest := r.PathValue("digest")
	if !bundleDigest.MatchString(digest) {
		httpx.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	ctx := r.Context()
	pinned, err := s.Queries.SessionPinnedToDigest(ctx, dbq.SessionPinnedToDigestParams{SessionID: actor.BindingID, Digest: &digest})
	if err != nil {
		s.dotfilesFailed(w, r, err)
		return
	}
	if !pinned {
		httpx.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	content, err := s.Queries.GetDotfilesBundle(ctx, digest)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "Not Found")
		return
	}
	if err != nil {
		s.dotfilesFailed(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("Content-Length", strconv.Itoa(len(content)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content) //nolint:gosec // a stored tar served as application/x-tar with nosniff; requests with an Origin are refused
}

func (s *Server) dotfilesFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.ErrorContext(r.Context(), "dotfiles bundle could not be read", "error", err)
	httpx.WriteError(w, http.StatusInternalServerError, "Internal Server Error")
}
