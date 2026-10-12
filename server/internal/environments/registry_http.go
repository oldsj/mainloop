package environments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	// perRequestTimeout bounds one HTTP request, including reading its body.
	perRequestTimeout = 20 * time.Second
	cdnHost           = "pkg-containers.githubusercontent.com"
	acceptManifests   = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
)

var challengeField = regexp.MustCompile(`(\w+)="([^"\r\n]*)"`)

// AnonymousRegistry is a [RegistryClient] for public images. It performs the anonymous bearer
// exchange on the allowlisted registry's own HTTPS origin and never follows a redirect. The one
// exception is the public GHCR config blob, which takes a single credential-free hop to GitHub's
// package CDN.
//
// It loads no credentials, ignores proxy environment variables and never logs a URL: the CDN
// Location is a signed URL.
type AnonymousRegistry struct {
	// Transport is the round tripper for every request. When nil each client gets its own
	// transport with no proxy and no transparent decompression. Tests inject a fake.
	Transport http.RoundTripper
	// ReadBudget caps one Read, covering the token exchange and the CDN hop. Zero means 30s.
	ReadBudget time.Duration
}

// client returns a client and a function that releases the connections of a transport the reader
// created. An injected Transport belongs to the caller and is left alone.
func (r *AnonymousRegistry) client() (*http.Client, func()) {
	rt, release := r.Transport, func() {}
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.DisableCompression = true
		rt, release = t, t.CloseIdleConnections
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, release
}

type fetched struct {
	status int
	header http.Header
	body   []byte
}

// Read implements [RegistryClient].
func (r *AnonymousRegistry) Read(ctx context.Context, registry, repository, kind, reference string) ([]byte, error) {
	budget := r.ReadBudget
	if budget == 0 {
		budget = 30 * time.Second
	}
	return withBudget(ctx, budget, "read", func(ctx context.Context) ([]byte, error) {
		return r.read(ctx, registry, repository, kind, reference)
	})
}

func (r *AnonymousRegistry) read(ctx context.Context, registry, repository, kind, reference string) ([]byte, error) {
	data, err := r.exchange(ctx, registry, repository, kind, reference)
	if err != nil {
		if ctx.Err() != nil {
			// A transport error would carry the request URL, signed on the CDN hop.
			return nil, context.Cause(ctx)
		}
		var re *RegistryError
		if errors.As(err, &re) {
			return nil, err
		}
		// Transport errors embed the request URL; the owner sees only this.
		return nil, regErr("Registry metadata unavailable or malformed")
	}
	return data, nil
}

func (r *AnonymousRegistry) exchange(ctx context.Context, registry, repository, kind, reference string) ([]byte, error) {
	client, release := r.client()
	defer release()
	target := "https://" + registry + "/v2/" + repository + "/" + kind + "/" + reference
	headers := http.Header{"Accept": {acceptManifests}}

	authenticated := false
	res, err := fetch(ctx, client, target, headers)
	if err != nil {
		return nil, err
	}
	if res.status == http.StatusUnauthorized {
		token, err := r.token(ctx, client, res.header.Get("Www-Authenticate"), registry, repository)
		if err != nil {
			return nil, err
		}
		headers.Set("Authorization", "Bearer "+token)
		if res, err = fetch(ctx, client, target, headers); err != nil {
			return nil, err
		}
		authenticated = true
	}
	if res.status == http.StatusTemporaryRedirect && authenticated && registry == "ghcr.io" && kind == "blobs" && digestPattern.MatchString(reference) {
		location, err := configCDNLocation(res.header.Get("Location"))
		if err != nil {
			return nil, err
		}
		// A fresh client with a fresh transport: nothing from the registry exchange (connection
		// state, cookies, headers) reaches the CDN.
		cdn, releaseCDN := r.client()
		defer releaseCDN()
		if res, err = fetch(ctx, cdn, location, nil); err != nil {
			return nil, err
		}
	}
	switch {
	case res.status == http.StatusUnauthorized, res.status == http.StatusForbidden, res.status == http.StatusNotFound:
		return nil, regErr("private images not supported yet (or image not found)")
	case res.status != http.StatusOK:
		return nil, regErr("Registry metadata request failed: HTTP %d", res.status)
	}
	return res.body, nil
}

// token performs the anonymous bearer exchange named by a Www-Authenticate challenge.
func (r *AnonymousRegistry) token(ctx context.Context, client *http.Client, challenge, registry, repository string) (string, error) {
	if len(challenge) < 7 || !strings.EqualFold(challenge[:7], "bearer ") {
		return "", regErr("private images not supported yet")
	}
	fields := map[string]string{}
	for _, m := range challengeField.FindAllStringSubmatch(challenge, -1) {
		fields[m[1]] = m[2]
	}
	realm, err := url.Parse(fields["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host != registry || realm.User != nil || strings.Contains(fields["realm"], "#") {
		return "", regErr("private images not supported yet: unsupported authentication origin")
	}
	service, ok := fields["service"]
	if !ok {
		service = registry
	}
	q := realm.Query()
	q.Set("service", service)
	q.Set("scope", "repository:"+repository+":pull")
	realm.RawQuery = q.Encode()

	res, err := fetch(ctx, client, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if res.status != http.StatusOK {
		return "", regErr("private images not supported yet")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(res.body, &body); err != nil || body == nil {
		return "", regErr("Registry metadata unavailable or malformed")
	}
	var token string
	for _, key := range []string{"token", "access_token"} {
		raw, ok := body[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", regErr("Registry metadata unavailable or malformed")
		}
		if token == "" {
			token = s
		}
	}
	if token == "" {
		return "", regErr("private images not supported yet")
	}
	return token, nil
}

// configCDNLocation accepts only an https URL on GitHub's package CDN, default port, without
// userinfo, fragment, whitespace or control characters. Its text never appears in an error.
func configCDNLocation(location string) (string, error) {
	denied := regErr("Unsupported registry config redirect")
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.Hostname() != cdnHost || (u.Port() != "" && u.Port() != "443") ||
		strings.HasSuffix(u.Host, ":") || u.User != nil || strings.Contains(authority(location), "@") || strings.Contains(location, "#") {
		return "", denied
	}
	if strings.ContainsFunc(location, func(c rune) bool { return unicode.IsSpace(c) || c < 32 || c == 127 }) {
		return "", denied
	}
	return location, nil
}

// fetch performs one GET with identity encoding. It reads the whole body, capped at
// [MaxMetadataBytes], and rejects any other Content-Encoding before reading it.
func fetch(ctx context.Context, client *http.Client, target string, headers http.Header) (fetched, error) {
	ctx, cancel := context.WithTimeout(ctx, perRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fetched{}, err
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return fetched{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc != "" && enc != "identity" {
		return fetched{}, regErr("Registry metadata requires identity Content-Encoding")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxMetadataBytes+1))
	if err != nil {
		return fetched{}, err
	}
	if len(body) > MaxMetadataBytes {
		return fetched{}, regErr("Registry metadata exceeds size limit")
	}
	return fetched{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// authority returns the text between "//" and the next path, query or fragment delimiter.
func authority(rawURL string) string {
	_, rest, ok := strings.Cut(rawURL, "//")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
