package pushgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/github"
)

type Server struct {
	Service      *Service
	Log          *slog.Logger
	AllowedHosts map[string]bool
	Dev          bool
	Enabled      bool
	SpoolRoot    string
	Limits       Limits
}

var route = regexp.MustCompile(`^/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)\.git/(info/refs|git-receive-pack)$`)

// Handler owns its correlation IDs and refusal logging. It deliberately has no access logger:
// paths, queries, client-supplied IDs and exception messages must never enter refusal logs.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	w.Header().Set("x-mainloop-request-id", id)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	stage := "request"
	defer func() {
		if recover() != nil {
			s.refuse(w, r, id, "transport_unavailable", stage)
		}
	}()
	if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/ready") {
		if r.URL.Path == "/ready" && s.Enabled && s.Service != nil && s.Service.Pool != nil {
			if err := s.Service.Pool.Ping(r.Context()); err != nil {
				s.refuse(w, r, id, "transport_unavailable", "configuration")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"healthy"}`)
		return
	}
	if !s.Enabled {
		s.refuse(w, r, id, "git_transport_disabled", "listener_flags")
		return
	}
	if s.Service == nil || s.Service.Pool == nil || s.Service.Native == nil || s.Service.Upstream == nil {
		s.refuse(w, r, id, "transport_unavailable", "configuration")
		return
	}
	repo, discovery, err := s.parse(r)
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	l := s.Limits.defaults()
	ctx, cancel := context.WithTimeout(r.Context(), 3*l.Timeout)
	defer cancel()
	// Bound network body reads as well as time spent inside the handler.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(l.Timeout))
	stage = "authentication"
	c, err := s.Service.authenticate(ctx, r.Header.Get("Authorization"))
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	stage = "binding"
	a, err := s.Service.check(ctx, c, "")
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	if !strings.EqualFold(a.W.Repository, repo.FullName()) {
		s.refuse(w, r, id, "repository_mismatch", stage)
		return
	}
	if discovery {
		refs, err := s.Service.Upstream.Refs(ctx, repo)
		if err != nil {
			s.fail(w, r, id, "upstream_read", err)
			return
		}
		// Advertisement supplies no authority; a subsequent probe or push independently checks it.
		oid := refs["refs/heads/"+a.W.Branch]
		if oid == "" {
			oid = zeroOID
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = w.Write(advertisement(a.W.Branch, oid)) //nolint:gosec // validated Git pkt-lines, Git media type and nosniff; never HTML
		return
	}
	stage = "admission"
	claim, err := s.Service.admit(ctx, c, a, id, l)
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	defer s.Service.releaseAdmission(ctx, claim)
	scratch, err := reserveScratchReservation(l)
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	defer scratch.release()
	stage = "body"
	bctx, bcancel := context.WithTimeout(ctx, l.Timeout)
	defer bcancel()
	// Read only enough to distinguish the complete probe from a real body. This happens after
	// admission and never allocates a spool; the fifth byte excludes all extra-byte neighbours.
	prefix, err := io.ReadAll(io.LimitReader(&contextReader{bctx, r.Body}, 5))
	if err != nil {
		s.refuse(w, r, id, "http_disconnected", stage)
		return
	}
	if bytes.Equal(prefix, []byte("0000")) {
		if r.ContentLength >= 0 && r.ContentLength != 4 {
			s.refuse(w, r, id, "http_body_length", stage)
			return
		}
		stage = "probe_authority"
		current, err := s.Service.check(ctx, c, claim)
		if err != nil {
			s.fail(w, r, id, stage, err)
			return
		}
		if !sameAuthority(a, current) {
			s.refuse(w, r, id, "authority_changed", stage)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		return
	}
	sp, err := newSpool(bctx, s.SpoolRoot, io.MultiReader(bytes.NewReader(prefix), r.Body), l)
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	defer sp.close()
	sp.scratch = scratch
	if r.ContentLength >= 0 && sp.Size != r.ContentLength {
		s.refuse(w, r, id, "http_body_length", stage)
		return
	}
	stage = "commands"
	cmd, err := receiveCommands(sp.body, l.CommandBytes)
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	stage = "dispatch"
	data, err := s.Service.receive(ctx, c, a, cmd, sp, id, l)
	if err != nil {
		s.fail(w, r, id, stage, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	_, _ = w.Write(data) //nolint:gosec // locally synthesized Git report, Git media type and nosniff; never HTML
}

func (s *Server) parse(r *http.Request) (github.Repo, bool, error) {
	host := strings.ToLower(r.Host)
	if strings.Contains(host, ":") {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			return github.Repo{}, false, denial("host_denied")
		}
		host = h
	}
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if !s.AllowedHosts[host] && (!s.Dev || !local) {
		return github.Repo{}, false, denial("host_denied")
	}
	m := route.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil || r.URL.RawPath != "" {
		return github.Repo{}, false, denial("path_invalid")
	}
	repo, err := github.ParseRepo(m[1] + "/" + m[2])
	if err != nil || r.URL.Path != "/"+repo.Owner+"/"+repo.Name+".git/"+m[3] {
		return github.Repo{}, false, denial("path_invalid")
	}
	discovery := m[3] == "info/refs"
	if (discovery && (r.Method != http.MethodGet || r.URL.RawQuery != "service=git-receive-pack")) || (!discovery && (r.Method != http.MethodPost || r.URL.RawQuery != "")) {
		return repo, discovery, denial("route_denied")
	}
	for k, vs := range r.Header {
		if len(vs) != 1 {
			return repo, discovery, denial("duplicate_header")
		}
		k = strings.ToLower(k)
		for _, v := range vs {
			if strings.ContainsAny(v, "\r\n\x00") {
				return repo, discovery, denial("routing_header")
			}
		}
		switch k {
		case "forwarded", "upgrade", "proxy-authorization", "x-http-method-override", "x-original-url", "x-rewrite-url":
			return repo, discovery, denial("routing_header")
		}
		if strings.HasPrefix(k, "x-forwarded-") || strings.HasPrefix(k, "x-actor") || strings.HasPrefix(k, "x-session") {
			return repo, discovery, denial("routing_header")
		}
	}
	enc := r.Header.Get("Content-Encoding")
	if enc != "" && enc != "identity" {
		return repo, discovery, denial("http_compression")
	}
	if r.Header.Get("Git-Protocol") != "" && r.Header.Get("Git-Protocol") != "version=0" {
		return repo, discovery, denial("git_protocol")
	}
	if len(r.TransferEncoding) > 1 || (len(r.TransferEncoding) == 1 && r.TransferEncoding[0] != "chunked") || (len(r.TransferEncoding) > 0 && r.ContentLength >= 0) {
		return repo, discovery, denial("http_framing")
	}
	if discovery && (r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		return repo, discovery, denial("http_framing")
	}
	if !discovery && r.Header.Get("Content-Type") != "application/x-git-receive-pack-request" {
		return repo, discovery, denial("content_type")
	}
	if r.ContentLength > s.Limits.defaults().BodyBytes {
		return repo, discovery, denial("body_limit")
	}
	return repo, discovery, nil
}

// Closed vocabularies are defense in depth: even an injected dependency cannot log its error
// message as a code or stage. Responses and logs share the same safe classification.
var denialCodes = strings.Fields(`transport_unavailable git_transport_disabled host_denied path_invalid route_denied duplicate_header routing_header http_compression git_protocol http_framing content_type body_limit authentication_required writer_claim_lost grant_revoked credential_revoked stale_writer_generation binding_scope policy_mismatch metadata_unavailable repository_mismatch runtime_unattested runtime_changed authority_changed publication_unresolved admission_busy private_spool_required disk_spool_required disk_unavailable http_disconnected http_body_length pkt_header pkt_length pkt_truncated command_limit shallow_invalid single_ref_required capability_placement capability_duplicate unsupported_capability command_invalid invalid_branch default_branch protected_branch branch_mismatch ref_namespace deletion non_fast_forward body_identity_changed pack_checksum pack_header pack_limit pack_object_size pack_truncated pack_compression pack_expansion_limit pack_trailing_or_truncated delta_header remote_ref_changed default_objects_unavailable git_inventory validated_object_limit validation_timeout git_validation commit_required disk_limit response_limit upstream_advertisement upstream_object_format upstream_unavailable seed_framing upstream_http upstream_receipt upstream_reflection repository_unavailable publication_unknown`)
var denialStages = strings.Fields(`configuration listener_flags request authentication binding admission body probe_authority commands dispatch upstream_read`)

func closed(v string, set []string, fallback string) string {
	for _, s := range set {
		if v == s {
			return v
		}
	}
	return fallback
}
func (s *Server) fail(w http.ResponseWriter, r *http.Request, id, stage string, err error) {
	code := "transport_unavailable"
	var d denial
	if errors.As(err, &d) {
		code = string(d)
	}
	s.refuse(w, r, id, code, stage)
}
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, id, code, stage string) {
	code = closed(code, denialCodes, "transport_unavailable")
	stage = closed(stage, denialStages, "unknown")
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log.WarnContext(r.Context(), "git request refused", "request_id", id, "code", code, "stage", stage)
	status := http.StatusForbidden
	if code == "authentication_required" {
		status = http.StatusUnauthorized
	}
	if code == "body_limit" {
		status = http.StatusRequestEntityTooLarge
	}
	if code == "transport_unavailable" || code == "publication_unknown" || strings.HasPrefix(code, "upstream_") || code == "disk_unavailable" || code == "metadata_unavailable" {
		status = http.StatusServiceUnavailable
	}
	if code == "admission_busy" {
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "%s %s\n", code, id)
}
