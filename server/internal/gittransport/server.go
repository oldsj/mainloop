package gittransport

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// DefaultMaxBody caps a request body, compressed and decompressed alike. Fetch requests are lists
// of wanted and known commits; the cap bounds a hostile one.
const DefaultMaxBody int64 = 256 << 20

// Capability is what a valid read bearer stands for.
type Capability struct {
	WorkspaceID  string
	CredentialID string
	Repository   string // "owner/name", lower-case
}

// ErrUnauthorized means the bearer is missing, malformed, unknown, revoked, or belongs to a
// workspace that has ended.
var ErrUnauthorized = errors.New("gittransport: unauthorized")

// Authenticator turns a read bearer into a [Capability].
type Authenticator struct {
	Q *dbq.Queries
}

// Authenticate checks an Authorization header value.
func (a *Authenticator) Authenticate(ctx context.Context, header string) (Capability, error) {
	token, err := agentauth.BearerToken(header)
	if err != nil {
		return Capability{}, ErrUnauthorized
	}
	row, err := a.Q.GetGitReadCapability(ctx, agentauth.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Capability{}, ErrUnauthorized
	}
	if err != nil {
		return Capability{}, fmt.Errorf("gittransport: read capability: %w", err)
	}
	return Capability{WorkspaceID: row.WorkspaceID, CredentialID: row.CredentialID, Repository: row.Repository}, nil
}

// Upstream is the GitHub side: an upload-pack request for one repository.
type Upstream interface {
	GitFetch(ctx context.Context, repo github.Repo, f github.GitFetch) (*github.GitResponse, error)
}

// Pinger reports whether the database answers.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server is the read listener's handler.
type Server struct {
	Auth interface {
		Authenticate(ctx context.Context, header string) (Capability, error)
	}
	Upstream Upstream
	Log      *slog.Logger
	// Ready is checked by /ready. Nil means always ready.
	Ready Pinger
	// AllowedHosts are the Host names served, besides loopback in dev.
	AllowedHosts map[string]bool
	Dev          bool
	// MaxBody caps request bodies; zero means [DefaultMaxBody].
	MaxBody int64
}

// Handler returns the listener's handler: /health and /ready, and the two upload-pack routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"healthy"}`)
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if s.Ready != nil {
			if err := s.Ready.Ping(r.Context()); err != nil {
				httpx.WriteError(w, http.StatusServiceUnavailable, "not ready")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})
	mux.HandleFunc("/", s.serve)
	return httpx.Chain(mux,
		httpx.RequestIDMiddleware,
		httpx.AccessLog(s.log()),
		httpx.Recover(s.log()),
		httpx.HostGuard(s.AllowedHosts, s.Dev, "/health", "/ready"),
	)
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) maxBody() int64 {
	if s.MaxBody > 0 {
		return s.MaxBody
	}
	return DefaultMaxBody
}

// A refusal is a short code and a status. The body is "<code> <request id>" so an operator can find
// the log line; nothing from upstream or from the request is echoed.
type refusal struct {
	code   string
	status int
}

func (r *refusal) Error() string { return r.code }

func deny(code string) *refusal        { return &refusal{code, http.StatusForbidden} }
func unavailable(code string) *refusal { return &refusal{code, http.StatusServiceUnavailable} }

var routePath = regexp.MustCompile(`^/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// Headers that name a route, a rewrite, an actor or a proxy hop. A client has no business sending
// them to the transport, and one that does is trying to reach something else.
var routingHeaders = map[string]bool{
	"Upgrade": true, "Proxy-Authorization": true, "X-Http-Method-Override": true,
	"X-Original-Url": true, "X-Rewrite-Url": true, "Forwarded": true,
}

var routingPrefixes = []string{"X-Forwarded-", "X-Actor", "X-Session"}

type request struct {
	repo      github.Repo
	discovery bool
	protocol  string
	gzip      bool
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	rq, err := s.parse(r)
	if err == nil {
		err = s.handle(w, r, rq)
	}
	if err == nil {
		return
	}
	var ref *refusal
	if !errors.As(err, &ref) {
		s.log().ErrorContext(r.Context(), "git transport failed", "request_id", httpx.RequestID(r.Context()), "error", err.Error())
		ref = unavailable("transport_unavailable")
	}
	s.log().InfoContext(r.Context(), "git transport refused", "request_id", httpx.RequestID(r.Context()), "code", ref.code)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(ref.status)
	_, _ = fmt.Fprintf(w, "%s %s\n", ref.code, httpx.RequestID(r.Context()))
}

// parse validates the route, the method and the headers. It authenticates nothing.
func (s *Server) parse(r *http.Request) (request, error) {
	path := r.URL.EscapedPath()
	m := routePath.FindStringSubmatch(path)
	if m == nil || strings.Contains(path, "%") || r.URL.RawPath != "" {
		return request{}, deny("path_invalid")
	}
	repo, err := github.ParseRepo(m[1] + "/" + m[2])
	if err != nil {
		return request{}, deny("path_invalid")
	}
	if m[3] == "git-receive-pack" || (m[3] == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack") {
		return request{}, deny("push_not_served")
	}
	rq := request{repo: repo, discovery: m[3] == "info/refs"}
	if rq.discovery {
		if r.Method != http.MethodGet || r.URL.RawQuery != "service=git-upload-pack" {
			return request{}, deny("route_denied")
		}
	} else if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		return request{}, deny("route_denied")
	}
	for k, vs := range r.Header {
		if len(vs) > 1 {
			return request{}, deny("duplicate_header")
		}
		if routingHeaders[k] {
			return request{}, deny("routing_header")
		}
		for _, p := range routingPrefixes {
			if strings.HasPrefix(k, p) {
				return request{}, deny("routing_header")
			}
		}
	}
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		rq.gzip = true
	default:
		return request{}, deny("http_compression")
	}
	if rq.discovery {
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			return request{}, deny("http_framing")
		}
	} else {
		if r.Header.Get("Content-Type") != "application/x-git-upload-pack-request" {
			return request{}, deny("content_type")
		}
		if r.ContentLength > s.maxBody() {
			return request{}, &refusal{"body_limit", http.StatusRequestEntityTooLarge}
		}
	}
	switch p := r.Header.Get("Git-Protocol"); p {
	case "", "version=0":
	case "version=1", "version=2":
		rq.protocol = p
	default:
		return request{}, deny("git_protocol")
	}
	return rq, nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, rq request) error {
	if r.Header.Get("Authorization") == "" {
		return &refusal{"authentication_required", http.StatusUnauthorized}
	}
	capability, err := s.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if errors.Is(err, ErrUnauthorized) {
		return &refusal{"authentication_required", http.StatusUnauthorized}
	}
	if err != nil {
		return err
	}
	// The repository in the URL is only a claim; the capability decides. A bearer for one
	// repository is useless on any other, whoever holds it.
	if !strings.EqualFold(capability.Repository, rq.repo.FullName()) {
		return deny("repository_denied")
	}
	enrolled, err := github.ParseRepo(capability.Repository)
	if err != nil {
		return deny("repository_denied")
	}
	fetch := github.GitFetch{Discovery: rq.discovery, Protocol: rq.protocol}
	var body *bodyReader
	if !rq.discovery {
		body, err = newBodyReader(r.Body, rq.gzip, s.maxBody())
		if err != nil {
			return err
		}
		fetch.Body = body
	}
	resp, err := s.Upstream.GitFetch(r.Context(), enrolled, fetch)
	if err != nil {
		if body != nil && body.err != nil {
			return bodyRefusal(body.err)
		}
		return upstreamRefusal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return stream(w, resp)
}

// stream sends the upstream answer. The first chunk is read before the status is written so a
// failure there is still a clean refusal; after that a failure can only cut the connection, which
// the client sees as a failed fetch rather than a truncated pack taken for a whole one.
func stream(w http.ResponseWriter, resp *github.GitResponse) error {
	buf := make([]byte, 32<<10)
	var (
		n   int
		err error
	)
	for n == 0 && err == nil {
		n, err = resp.Body.Read(buf)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return upstreamRefusal(err)
	}
	w.Header().Set("Content-Type", resp.ContentType)
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(buf[:n]); werr != nil || errors.Is(err, io.EOF) {
		return nil //nolint:nilerr // the client went away, or the answer was a single chunk
	}
	rc := http.NewResponseController(w)
	_ = rc.Flush() // streaming relies on flushes; an error here is a closed connection
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil //nolint:nilerr // the client went away
			}
			_ = rc.Flush()
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func upstreamRefusal(err error) *refusal {
	switch {
	case errors.Is(err, github.ErrNotInstalled):
		return unavailable("repository_unavailable")
	case errors.Is(err, github.ErrGitTokenReflected):
		return unavailable("upstream_invalid")
	case errors.Is(err, github.ErrGitRefused):
		return unavailable("upstream_refused")
	}
	return unavailable("upstream_unavailable")
}

var (
	errBodyLimit   = errors.New("request body too large")
	errBodyInvalid = errors.New("request body invalid")
)

func bodyRefusal(err error) *refusal {
	if errors.Is(err, errBodyLimit) {
		return &refusal{"body_limit", http.StatusRequestEntityTooLarge}
	}
	return &refusal{"body_invalid", http.StatusBadRequest}
}

// bodyReader reads the request body for upstream: capped, and gunzipped when the client sent it
// compressed. It remembers why a read failed, since the HTTP client reports only that it did.
type bodyReader struct {
	src io.Reader
	err error
}

func newBodyReader(body io.Reader, compressed bool, limit int64) (*bodyReader, error) {
	capped := &capReader{r: body, left: limit}
	if !compressed {
		return &bodyReader{src: capped}, nil
	}
	zr, err := gzip.NewReader(capped)
	if err != nil {
		return nil, bodyRefusal(classify(err))
	}
	// The inflated size is capped too: a few kilobytes of gzip can stand for gigabytes.
	return &bodyReader{src: &capReader{r: zr, left: limit}}, nil
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = classify(err)
		return n, b.err
	}
	return n, err
}

func classify(err error) error {
	var mbe *http.MaxBytesError
	if errors.Is(err, errBodyLimit) || errors.As(err, &mbe) {
		return errBodyLimit
	}
	return errBodyInvalid
}

// capReader fails with errBodyLimit once more than left bytes have been read.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errBodyLimit
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return max(n+int(c.left), 0), errBodyLimit
	}
	return n, err
}
