package github_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

var repo = github.Repo{Owner: "oldsj", Name: "mainloop"}

func newApp(t *testing.T, fake *githubtest.Server) *github.App {
	t.Helper()
	app, err := github.NewApp(github.Options{
		AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func seeded(t *testing.T) (*githubtest.Server, *github.App) {
	t.Helper()
	fake := githubtest.New(t)
	desc := "the loop"
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fake.AddRepo(githubtest.Repository{
		Owner: "oldsj", Name: "mainloop", Description: &desc, AvatarURL: "https://avatars.example/o.png", Issues: 4,
		PullRequests: []githubtest.PullRequest{
			{Number: 3, Title: "third", Author: "o", HeadRef: "feature/x", CreatedAt: now, UpdatedAt: now},
			{Number: 2, Title: "second", Author: "o", HeadRef: "mainloop/y", CreatedAt: now, UpdatedAt: now},
			{Number: 1, Title: "first", Author: "o", HeadRef: "fix", CreatedAt: now, UpdatedAt: now},
		},
		Commits: []githubtest.Commit{
			{SHA: "abcdef0123456789", Message: "subject\n\nbody", Author: "A", Date: now},
			{SHA: "1234567890abcdef", Message: "older", Author: "B", Date: now},
		},
	})
	return fake, newApp(t, fake)
}

func TestMetadataCountsPullRequestsAndIssuesSeparately(t *testing.T) {
	_, app := seeded(t)
	m, err := app.Metadata(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if m.OpenPRs != 3 || m.OpenIssues != 4 {
		t.Fatalf("prs %d issues %d", m.OpenPRs, m.OpenIssues)
	}
	if m.DefaultBranch != "main" || m.Repo != repo || m.HTMLURL != "https://github.com/oldsj/mainloop" ||
		m.Description == nil || *m.Description != "the loop" || m.AvatarURL == nil {
		t.Fatalf("%+v", m)
	}
}

func TestMetadataWithNoPullRequests(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "mainloop", Issues: 2})
	m, err := newApp(t, fake).Metadata(context.Background(), repo)
	if err != nil || m.OpenPRs != 0 || m.OpenIssues != 2 {
		t.Fatalf("%+v %v", m, err)
	}
}

// A reply without a default branch is reported as such, never filled in with a guess.
func TestMetadataDoesNotInventADefaultBranch(t *testing.T) {
	fake, app := seeded(t)
	fake.Update("oldsj", "mainloop", func(r *githubtest.Repository) { r.DefaultBranch = "" })
	m, err := app.Metadata(context.Background(), repo)
	if err != nil || m.DefaultBranch != "" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestPullRequestsAndCommits(t *testing.T) {
	_, app := seeded(t)
	prs, err := app.OpenPullRequests(context.Background(), repo, 2)
	if err != nil || len(prs) != 2 || prs[0].Number != 3 || !prs[0].IsMainloop || !prs[1].IsMainloop {
		t.Fatalf("%+v %v", prs, err)
	}
	all, _ := app.OpenPullRequests(context.Background(), repo, 50)
	if len(all) != 3 || all[2].IsMainloop {
		t.Fatalf("%+v", all)
	}
	cs, err := app.RecentCommits(context.Background(), repo, "", 5)
	if err != nil || len(cs) != 2 || cs[0].SHA != "abcdef0" || cs[0].Message != "subject" || cs[0].Author != "A" {
		t.Fatalf("%+v %v", cs, err)
	}
}

// Each use asks for one repository and exactly the permissions it needs.
func TestTokensAreScopedToOneRepoWithMinimalPermissions(t *testing.T) {
	fake, app := seeded(t)
	ctx := context.Background()
	if _, err := app.Metadata(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := app.OpenPullRequests(ctx, repo, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RecentCommits(ctx, repo, "", 5); err != nil {
		t.Fatal(err)
	}
	mints := fake.Mints()
	if len(mints) != 3 {
		t.Fatalf("%d mints", len(mints))
	}
	want := []map[string]string{
		{"metadata": "read", "pull_requests": "read"},
		{"pull_requests": "read"},
		{"contents": "read"},
	}
	for i, m := range mints {
		if len(m.Repositories) != 1 || m.Repositories[0] != "mainloop" || fmt.Sprint(m.Permissions) != fmt.Sprint(want[i]) {
			t.Errorf("mint %d: %+v", i, m)
		}
	}
}

func TestTokensAreCachedUntilNearExpiry(t *testing.T) {
	fake, app := seeded(t)
	ctx := context.Background()
	for range 4 {
		if _, err := app.Metadata(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fake.Mints()); n != 1 {
		t.Fatalf("%d mints for 4 uses, want 1", n)
	}
	installs := 0
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r.Path, "/installation") {
			installs++
		}
	}
	if installs != 1 {
		t.Fatalf("%d installation lookups, want 1", installs)
	}
}

func TestTokensNearExpiryAreRefreshed(t *testing.T) {
	fake, app := seeded(t)
	// Leave several seconds before the one-minute margin, including the fake's second rounding.
	fake.TokenTTL = 65 * time.Second
	ctx := context.Background()
	for range 2 {
		if _, err := app.Metadata(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fake.Mints()); n != 1 {
		t.Fatalf("%d mints before the margin, want 1", n)
	}
	time.Sleep(5100 * time.Millisecond)
	if _, err := app.Metadata(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Mints()); n != 2 {
		t.Fatalf("%d mints after the margin, want 2", n)
	}
}

// A token answer without a usable token or a lasting expiry is refused with a typed error and
// is not cached: the next use mints again.
func TestUnusableTokenAnswersAreRefused(t *testing.T) {
	past := func(token string, _ time.Time) map[string]any {
		return map[string]any{"token": token, "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	}
	for name, resp := range map[string]func(string, time.Time) map[string]any{
		"empty token": func(_ string, e time.Time) map[string]any {
			return map[string]any{"token": "", "expires_at": e.Format(time.RFC3339)}
		},
		"missing token": func(_ string, e time.Time) map[string]any {
			return map[string]any{"expires_at": e.Format(time.RFC3339)}
		},
		"space in token": func(tok string, e time.Time) map[string]any {
			return map[string]any{"token": tok + " x", "expires_at": e.Format(time.RFC3339)}
		},
		"newline in token": func(tok string, e time.Time) map[string]any {
			return map[string]any{"token": tok + "\n", "expires_at": e.Format(time.RFC3339)}
		},
		"missing expiry": func(tok string, _ time.Time) map[string]any { return map[string]any{"token": tok} },
		"bad expiry": func(tok string, _ time.Time) map[string]any {
			return map[string]any{"token": tok, "expires_at": "tomorrow"}
		},
		"past expiry": past,
		"inside margin": func(tok string, _ time.Time) map[string]any {
			return map[string]any{"token": tok, "expires_at": time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)}
		},
		"non-string token": func(_ string, e time.Time) map[string]any {
			return map[string]any{"token": 5, "expires_at": e.Format(time.RFC3339)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, app := seeded(t)
			fake.MintResponse = resp
			ctx := context.Background()
			for range 2 {
				_, err := app.Metadata(ctx, repo)
				if !errors.Is(err, github.ErrInvalidToken) || strings.Contains(err.Error(), "ghs_fake_") {
					t.Fatalf("got %v", err)
				}
			}
			if n := len(fake.Mints()); n != 2 {
				t.Fatalf("%d mints for 2 refused answers, want 2 (nothing cached)", n)
			}
			fake.MintResponse = nil
			if _, err := app.Metadata(ctx, repo); err != nil {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}

// A network failure on a data call is "unavailable", not a token problem.
func TestTransportFailureOnDataCallIsUnavailable(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "mainloop"})
	flaky := &flaky{next: http.DefaultTransport}
	app, err := github.NewApp(github.Options{
		AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL, Transport: flaky,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := app.Metadata(ctx, repo); err != nil { // token now cached
		t.Fatal(err)
	}
	flaky.down.Store(true)
	_, err = app.Metadata(ctx, repo)
	var ae *github.AuthError
	if !errors.Is(err, github.ErrUnavailable) || errors.As(err, &ae) || strings.Contains(err.Error(), "refused") {
		t.Fatalf("got %v", err)
	}
}

type flaky struct {
	next http.RoundTripper
	down atomic.Bool
}

func (f *flaky) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.down.Load() {
		return nil, errors.New("dial tcp: connection refused")
	}
	return f.next.RoundTrip(r)
}

// 16 callers arriving together share one installation lookup and one token.
func TestConcurrentFirstUseLooksUpAndMintsOnce(t *testing.T) {
	fake, app := seeded(t)
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Go(func() { _, errs[i] = app.Metadata(context.Background(), repo) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	lookups := 0
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r.Path, "/installation") {
			lookups++
		}
	}
	if lookups != 1 || len(fake.Mints()) != 1 {
		t.Fatalf("%d lookups and %d mints, want 1 and 1", lookups, len(fake.Mints()))
	}
}

// Scopes of one repository (different permissions) still share the installation lookup.
func TestScopesOfOneRepositoryShareTheInstallationLookup(t *testing.T) {
	fake, app := seeded(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { _, _ = app.Metadata(ctx, repo) })
		wg.Go(func() { _, _ = app.OpenPullRequests(ctx, repo, 5) })
		wg.Go(func() { _, _ = app.RecentCommits(ctx, repo, "", 5) })
	}
	wg.Wait()
	lookups := 0
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r.Path, "/installation") {
			lookups++
		}
	}
	if lookups != 1 || len(fake.Mints()) != 3 {
		t.Fatalf("%d lookups and %d mints, want 1 and 3", lookups, len(fake.Mints()))
	}
}

// A caller that gives up stops waiting, but the shared work finishes for the others.
func TestCancelledCallerDoesNotFailTheOthers(t *testing.T) {
	fake, app := seeded(t)
	gone, cancel := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	fake.Before("GET", "/repos/oldsj/mainloop/installation", func() {
		close(started)
		<-release
	})
	result := make(chan error, 1)
	go func() {
		_, err := app.Metadata(gone, repo)
		result <- err
	}()
	<-started
	cancel()
	defer cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("got %v", err)
	}
	close(release)
	if _, err := app.Metadata(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count("GET", "/repos/oldsj/mainloop/installation"); n != 1 {
		t.Fatalf("%d installation lookups, want the shared lookup reused", n)
	}
}

func TestNotInstalled(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "mainloop", NotInstalled: true})
	app := newApp(t, fake)
	if _, err := app.Metadata(context.Background(), repo); !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("got %v", err)
	}
	if _, err := app.Metadata(context.Background(), github.Repo{Owner: "nobody", Name: "nothing"}); !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("unknown repo: %v", err)
	}
}

func TestUpstreamFailuresAreTypedAndOpaque(t *testing.T) {
	fake, app := seeded(t)
	ctx := context.Background()
	// Warm the token so the failure is on the data call.
	if _, err := app.Metadata(ctx, repo); err != nil {
		t.Fatal(err)
	}
	for status, check := range map[int]func(error) bool{
		500: func(err error) bool { var s *github.StatusError; return errors.As(err, &s) && s.Status == 500 },
		403: func(err error) bool { var s *github.StatusError; return errors.As(err, &s) && s.Status == 403 },
		404: func(err error) bool { return errors.Is(err, github.ErrNotFound) },
	} {
		fake.Fail("GET", "/repos/oldsj/mainloop", status)
		_, err := app.Metadata(ctx, repo)
		if err == nil || !check(err) || strings.Contains(err.Error(), githubtest.SecretMarker) {
			t.Errorf("status %d: %v", status, err)
		}
	}
	fake.Fail("GET", "/repos/oldsj/mainloop", 0)
	if _, err := app.Metadata(ctx, repo); err != nil {
		t.Fatal(err)
	}
}

func TestMintFailureIsAuthError(t *testing.T) {
	fake, app := seeded(t)
	fake.Fail("POST", fmt.Sprintf("/app/installations/%d/access_tokens", installID(t, fake, app)), 500)
	_, err := app.Metadata(context.Background(), repo)
	var ae *github.AuthError
	if !errors.As(err, &ae) || ae.Status != 500 || strings.Contains(err.Error(), githubtest.SecretMarker) {
		t.Fatalf("%v", err)
	}
}

func installID(t *testing.T, fake *githubtest.Server, app *github.App) int64 {
	t.Helper()
	if _, err := app.OpenPullRequests(context.Background(), repo, 1); err != nil {
		t.Fatal(err)
	}
	return fake.Mints()[0].InstallationID
}

func TestWrongKeyIsRefusedByGitHub(t *testing.T) {
	fake, _ := seeded(t)
	other, err := github.NewApp(github.Options{AppID: "999", PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Metadata(context.Background(), repo)
	var se *github.StatusError
	if err == nil || !errors.As(err, &se) || se.Status != 401 {
		t.Fatalf("got %v", err)
	}
}

// A redirect is never followed, so a hostile Location cannot steer a token elsewhere.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits int
	other := newCountingServer(t, &hits)
	fake, app := seeded(t)
	_ = fake
	fake.Redirect("/repos/oldsj/mainloop", other)
	_, err := app.Metadata(context.Background(), repo)
	if err == nil {
		t.Fatal("redirect followed")
	}
	if hits != 0 {
		t.Fatalf("%d requests reached the redirect target", hits)
	}
}

func newCountingServer(t *testing.T, hits *int) string {
	t.Helper()
	srv := newHTTPServer(t, func(http.ResponseWriter, *http.Request) { *hits++ })
	return srv
}

func TestCredentialsAreNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	_, app := seeded(t)
	_, _ = app.Metadata(context.Background(), repo)
	out := fmt.Sprintf("%v %+v %#v %s", app, app, app, buf.String())
	for _, secret := range []string{githubtest.PrivateKey(), "ghs_fake_", "BEGIN"} {
		if strings.Contains(out, secret) {
			t.Fatalf("output contains %q", secret)
		}
	}
}

func TestNewAppRejectsBadSettings(t *testing.T) {
	good := githubtest.PrivateKey()
	for name, o := range map[string]github.Options{
		"no id":         {PrivateKey: good},
		"bad key":       {AppID: "1", PrivateKey: "nope"},
		"bad base":      {AppID: "1", PrivateKey: good, BaseURL: "ftp://x/"},
		"base userinfo": {AppID: "1", PrivateKey: good, BaseURL: "https://u:p@x/"},
	} {
		if _, err := github.NewApp(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
