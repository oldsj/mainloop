package github_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

const (
	advert = "application/x-git-upload-pack-advertisement"
	result = "application/x-git-upload-pack-result"
)

// gitApp returns an App whose git host is handler, with the repository installed.
func gitApp(t *testing.T, handler http.HandlerFunc) (*github.App, *githubtest.Server) {
	t.Helper()
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "mainloop"})
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	app, err := github.NewApp(github.Options{
		AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL, GitBaseURL: up.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, fake
}

func TestGitFetchAuthenticatesWithAScopedToken(t *testing.T) {
	var gotPath, gotQuery, gotType, gotProto string
	var allowed bool
	var fake *githubtest.Server
	app, f := gitApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotProto = r.URL.Path, r.URL.RawQuery, r.Header.Get("Git-Protocol")
		_, pass, _ := r.BasicAuth()
		allowed = fake.TokenAllows(pass, "oldsj", "mainloop", "contents") && !fake.TokenAllows(pass, "oldsj", "other", "contents")
		gotType = r.Header.Get("Accept")
		w.Header().Set("Content-Type", advert)
		_, _ = io.WriteString(w, "001e# service=git-upload-pack\n")
	})
	fake = f
	resp, err := app.GitFetch(context.Background(), repo, github.GitFetch{Discovery: true, Protocol: "version=2"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "001e# service=git-upload-pack\n" || resp.ContentType != advert {
		t.Fatalf("got %q %q", body, resp.ContentType)
	}
	if gotPath != "/oldsj/mainloop.git/info/refs" || gotQuery != "service=git-upload-pack" || gotType != advert || gotProto != "version=2" || !allowed {
		t.Fatalf("upstream saw %q ?%q accept=%q proto=%q allowed=%v", gotPath, gotQuery, gotType, gotProto, allowed)
	}
	mints := fake.Mints()
	if len(mints) != 1 || mints[0].Permissions["contents"] != "read" || len(mints[0].Permissions) != 1 || len(mints[0].Repositories) != 1 {
		t.Fatalf("mints = %+v", mints)
	}
}

func TestGitFetchPostsTheBody(t *testing.T) {
	var got string
	var ct string
	app, _ := gitApp(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, ct = string(b), r.Header.Get("Content-Type")
		if r.Method != http.MethodPost || r.URL.Path != "/oldsj/mainloop.git/git-upload-pack" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", result)
		_, _ = io.WriteString(w, "PACK")
	})
	resp, err := app.GitFetch(context.Background(), repo, github.GitFetch{Body: strings.NewReader("wants")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got != "wants" || ct != "application/x-git-upload-pack-request" || string(b) != "PACK" {
		t.Fatalf("got %q %q %q", got, ct, b)
	}
}

func TestGitFetchRefusesOtherAnswers(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"error status": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", advert)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "secret upstream text")
		},
		"other media type": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>")
		},
		"compressed": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", advert)
			w.Header().Set("Content-Encoding", "gzip")
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		},
		"token in a header": func(w http.ResponseWriter, r *http.Request) {
			_, pass, _ := r.BasicAuth()
			w.Header().Set("Content-Type", advert)
			w.Header().Set("X-Debug", "token="+pass)
		},
	} {
		t.Run(name, func(t *testing.T) {
			app, _ := gitApp(t, h)
			resp, err := app.GitFetch(context.Background(), repo, github.GitFetch{Discovery: true})
			if !errors.Is(err, github.ErrGitRefused) || resp != nil {
				t.Fatalf("got %v, %v", resp, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("upstream text leaked: %v", err)
			}
		})
	}
}

func TestGitFetchCutsOffAReflectedToken(t *testing.T) {
	app, _ := gitApp(t, func(w http.ResponseWriter, r *http.Request) {
		_, pass, _ := r.BasicAuth()
		w.Header().Set("Content-Type", advert)
		// The token, split across two writes, behind innocent bytes.
		_, _ = io.WriteString(w, "0000")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "hello "+pass[:5])
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, pass[5:]+" world")
	})
	resp, err := app.GitFetch(context.Background(), repo, github.GitFetch{Discovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got strings.Builder
	_, err = io.Copy(&got, resp.Body)
	if !errors.Is(err, github.ErrGitTokenReflected) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(got.String(), "ghs_") {
		t.Fatalf("the token reached the caller: %q", got.String())
	}
}

func TestGitFetchRefusesAnUnsupportedProtocolAndMissingInstallation(t *testing.T) {
	app, fake := gitApp(t, func(http.ResponseWriter, *http.Request) { t.Error("upstream called") })
	if _, err := app.GitFetch(context.Background(), repo, github.GitFetch{Discovery: true, Protocol: "version=9"}); err == nil {
		t.Fatal("accepted version=9")
	}
	fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "gone", NotInstalled: true})
	_, err := app.GitFetch(context.Background(), github.Repo{Owner: "oldsj", Name: "gone"}, github.GitFetch{Discovery: true})
	if !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("err = %v", err)
	}
}
