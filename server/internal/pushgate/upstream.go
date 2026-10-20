package pushgate

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gitref"
)

// GitHubUpstream reuses G11a for read-only seeds. The receive port uses the same App libraries
// and endpoint allowlist, with an independent repository-scoped write token: contents:write,
// plus workflows:write when the push touches .github/workflows/. Tokens never leave this
// adapter; even successful raw receive replies are replaced by a local report.
type GitHubUpstream struct {
	read    *github.App
	options github.Options
	base    *url.URL
	gitBase *url.URL
	http    *http.Client
	mu      sync.Mutex
	writers map[string]*ghinstallation.Transport
}

func NewGitHubUpstream(o github.Options) (*GitHubUpstream, error) {
	read, err := github.NewApp(o)
	if err != nil {
		return nil, err
	}
	raw := o.BaseURL
	if raw == "" {
		raw = github.DefaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	raw = o.GitBaseURL
	if raw == "" {
		raw = github.DefaultGitBaseURL
	}
	gitBase, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	rt := o.Transport
	if rt == nil {
		rt = http.DefaultTransport.(*http.Transport).Clone()
	}
	if t, ok := rt.(*http.Transport); ok {
		t = t.Clone()
		t.Proxy = nil
		t.DisableCompression = true
		rt = t
	}
	return &GitHubUpstream{read: read, options: o, base: base, gitBase: gitBase, http: &http.Client{Transport: rt, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, writers: map[string]*ghinstallation.Transport{}}, nil
}

func (u *GitHubUpstream) Refs(ctx context.Context, repo github.Repo) (map[string]string, error) {
	resp, err := u.read.GitFetch(ctx, repo, github.GitFetch{Discovery: true})
	if err != nil {
		return nil, denial("upstream_unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readBounded(resp.Body, 256<<10)
	if err != nil {
		return nil, err
	}
	return advertisedRefs(data, "git-upload-pack")
}

func (u *GitHubUpstream) Seed(ctx context.Context, repo github.Repo, wants []string, out io.Writer) error {
	var body []byte
	for _, oid := range wants {
		if !oidPattern.MatchString(oid) || oid == zeroOID {
			return denial("seed_framing")
		}
		body = append(body, pkt([]byte("want "+oid+"\n"))...)
	}
	body = append(body, []byte("0000")...)
	body = append(body, pkt([]byte("done\n"))...)
	resp, err := u.read.GitFetch(ctx, repo, github.GitFetch{Body: bytes.NewReader(body)})
	if err != nil {
		return denial("upstream_unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	b, end, err := packet(resp.Body)
	if err != nil || end || string(b) != "NAK\n" {
		return denial("seed_framing")
	}
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return denial("upstream_unavailable")
	}
	return nil
}

func (u *GitHubUpstream) DefaultBranch(ctx context.Context, repo github.Repo) (string, error) {
	tr, err := u.tokenTransport(ctx, repo, github.Permissions{"metadata": github.Read})
	if err != nil {
		return "", denial("metadata_unavailable")
	}
	client, err := gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}), gh.WithURLs(ptr(u.base.String()), ptr(u.base.String())))
	if err != nil {
		return "", denial("metadata_unavailable")
	}
	r, _, err := client.Repositories.Get(ctx, repo.Owner, repo.Name)
	if err != nil || !strings.EqualFold(r.GetFullName(), repo.FullName()) || !gitref.Valid(r.GetDefaultBranch()) {
		return "", denial("metadata_unavailable")
	}
	return r.GetDefaultBranch(), nil
}

func (u *GitHubUpstream) tokenTransport(ctx context.Context, repo github.Repo, perms github.Permissions) (*ghinstallation.Transport, error) {
	// Lookup/mint runs without a database transaction. Serializing construction here only shares
	// the token cache; branch admission belongs to PostgreSQL.
	u.mu.Lock()
	defer u.mu.Unlock()
	name := strings.ToLower(repo.FullName()) + "|" + permKey(perms)
	if t := u.writers[name]; t != nil {
		return t, nil
	}
	id, err := github.ParseAppID(u.options.AppID)
	if err != nil {
		return nil, err
	}
	key, err := github.ParsePrivateKey(u.options.PrivateKey)
	if err != nil {
		return nil, err
	}
	next := u.options.Transport
	if next == nil {
		next = http.DefaultTransport.(*http.Transport).Clone()
	}
	allow := &github.Allowlist{Base: u.base, Scope: github.Scope{Repo: repo, Permissions: perms}, Next: apiTransport{next}}
	at := ghinstallation.NewAppsTransportFromPrivateKey(allow, id, key)
	at.BaseURL = strings.TrimSuffix(u.base.String(), "/")
	client, err := gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: at, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}), gh.WithURLs(ptr(u.base.String()), ptr(u.base.String())))
	if err != nil {
		return nil, denial("upstream_unavailable")
	}
	inst, _, err := client.Apps.GetRepositoryInstallation(ctx, repo.Owner, repo.Name)
	if err != nil || inst.GetID() <= 0 {
		return nil, denial("repository_unavailable")
	}
	allow.BindInstallation(inst.GetID())
	tr := ghinstallation.NewFromAppsTransport(at, inst.GetID())
	tr.BaseURL = at.BaseURL
	fields, err := github.PermissionFields(perms)
	if err != nil {
		return nil, err
	}
	tr.InstallationTokenOptions = &gh.InstallationTokenOptions{Repositories: []string{repo.Name}, Permissions: fields}
	u.writers[name] = tr
	return tr, nil
}

// permKey orders perms so an equal permission set shares one cached token transport regardless
// of map iteration order.
func permKey(p github.Permissions) string {
	names := make([]string, 0, len(p))
	for name, level := range p {
		names = append(names, name+"="+level)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
func ptr[T any](v T) *T { return &v }

// Push sends body as one git-receive-pack request with a repository-scoped contents:write
// installation token. workflows additionally requests workflows:write, the permission GitHub
// requires to create or modify a file under .github/workflows/; a push that does not touch that
// directory keeps the narrower contents-only scope.
func (u *GitHubUpstream) Push(ctx context.Context, repo github.Repo, body io.Reader, workflows bool) ([]byte, error) {
	perms := github.Permissions{"contents": github.Write}
	if workflows {
		perms["workflows"] = github.Write
	}
	tr, err := u.tokenTransport(ctx, repo, perms)
	if err != nil {
		return nil, err
	}
	token, err := tr.Token(ctx)
	if err != nil || token == "" {
		return nil, denial("upstream_unavailable")
	}
	target := u.gitBase.JoinPath(repo.Owner, repo.Name+".git", "git-receive-pack")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return nil, denial("upstream_unavailable")
	}
	req.SetBasicAuth("x-access-token", token)
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Accept", "application/x-git-receive-pack-result")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, denial("upstream_unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || len(resp.Header.Values("Content-Type")) != 1 || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-result" || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		return nil, denial("upstream_http")
	}
	data, err := readBounded(resp.Body, 256<<10)
	if err != nil {
		return nil, err
	}
	for _, form := range []string{token, base64.StdEncoding.EncodeToString([]byte(token)), base64.RawStdEncoding.EncodeToString([]byte(token)), base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)), base64.RawStdEncoding.EncodeToString([]byte("x-access-token:" + token))} {
		if bytes.Contains(data, []byte(form)) {
			return nil, denial("upstream_reflection")
		}
		for _, vs := range resp.Header {
			for _, v := range vs {
				if strings.Contains(v, form) {
					return nil, denial("upstream_reflection")
				}
			}
		}
	}
	return data, nil
}
func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, denial("upstream_unavailable")
	}
	if int64(len(b)) > maxBytes {
		return nil, denial("response_limit")
	}
	return b, nil
}

var _ Upstream = (*GitHubUpstream)(nil)

// All App API answers, including token mints, are capped before a JSON decoder sees them.
type apiTransport struct{ next http.RoundTripper }

func (t apiTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, denial("upstream_unavailable")
	}
	data, err := readBounded(resp.Body, 2<<20)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}
